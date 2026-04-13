package seploy

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/ysmood/glog/pkg/lg"
)

//go:embed deploy.sh
var scriptDeploy string

//go:embed register-image.sh
var scriptRegisterImage string

//go:embed ensure-docker.sh
var scriptEnsureDocker string

type Deployment struct {
	NotService bool
	Follow     bool
	EnvFiles   []string

	ImageTag string

	// SSHTarget is the SSH connection target in the form "user@host[:port]".
	SSHTarget     string
	SSHPrivateKey []byte

	DockerRunOptions  []string
	DockerRunCommands []string
	DockerRunVolumes  []string
}

func (d *Deployment) Deploy(ctx context.Context) error {
	lg.Info(ctx, "Start deployment", "image",
		d.ImageTag, "host", d.host(), "time", time.Now().Format(time.RFC3339))

	if err := d.hasDangerousOptions(); err != nil {
		return err
	}

	registry, stop, err := d.startRegistry(ctx)
	if err != nil {
		return fmt.Errorf("failed to start registry: %w", err)
	}

	defer func() {
		if stop != nil {
			stop()
		}
	}()

	err = d.waitRegistryReady(ctx, registry)
	if err != nil {
		return fmt.Errorf("failed to wait registry ready: %w", err)
	}

	err = d.registerImage(ctx, registry)
	if err != nil {
		return fmt.Errorf("failed to deploy image: %w", err)
	}

	err = d.deploy(ctx, registry)
	if err != nil {
		return fmt.Errorf("failed to run container: %w", err)
	}

	return nil
}

func (d *Deployment) registerImage(ctx context.Context, registry string) error {
	lg.Info(ctx, "Register image", "image", d.ImageTag, "host", d.host())

	return execScript(scriptRegisterImage, map[string]string{
		"tag":          d.ImageTag,
		"registry_tag": registry + "/" + d.ImageTag,
	})
}

func (d *Deployment) deploy(ctx context.Context, registry string) error {
	client, err := d.connectSSH()
	if err != nil {
		return fmt.Errorf("failed to ssh to host: %w", err)
	}
	defer func() { _ = client.Close() }()

	if err := d.sshExec(client, scriptEnsureDocker); err != nil {
		return fmt.Errorf("ssh failed to ensure docker: %w", err)
	}

	srcRegistry, err := d.proxyRegistry(ctx, client, registry)
	if err != nil {
		return fmt.Errorf("ssh failed to proxy registry: %w", err)
	}

	lg.Info(ctx, "Deploy image", "image", d.ImageTag, "host", d.host())

	name := imageName(d.ImageTag)

	repo, ref := d.getRepoInfo()

	service := "-d --restart unless-stopped"
	if d.NotService {
		if d.Follow {
			service = "--rm"
		} else {
			service = "-d --rm"
		}
	}

	env, err := d.getEnvFile()
	if err != nil {
		return fmt.Errorf("failed to get env file: %w", err)
	}

	env = bytes.Join([][]byte{[]byte("SEPLOY_REPO_REF=" + ref), env}, []byte("\n"))

	volumes := []string{}
	for _, v := range d.DockerRunVolumes {
		volumes = append(volumes, "-v", name+"-"+v)
	}

	noNetworkOptions := true
	for _, v := range d.DockerRunOptions {
		if strings.HasPrefix(v, "--network") {
			noNetworkOptions = false
			break
		}
	}
	if noNetworkOptions {
		d.DockerRunOptions = append(d.DockerRunOptions, "--network", "seploy")
	}

	script, err := renderTpl(scriptDeploy, map[string]string{
		"name":        escapeArgs(name),
		"tag":         escapeArgs(d.ImageTag),
		"registryTag": escapeArgs(srcRegistry + "/" + d.ImageTag),
		"service":     service,
		"volumes":     escapeArgs(volumes...),
		"options":     escapeArgs(d.DockerRunOptions...),
		"commands":    escapeArgs(d.DockerRunCommands...),
		"host":        escapeArgs("SEPLOY_HOST=" + d.host()),
		"hostLabel":   escapeArgs("seploy.host=" + d.host()),
		"repoLabel":   escapeArgs("seploy.repo=" + repo),
		"refLabel":    escapeArgs("seploy.repo.ref=" + ref),
		"env":         base64.StdEncoding.EncodeToString(env),
		"notService":  fmt.Sprint(d.NotService),
	})
	if err != nil {
		return fmt.Errorf("failed to render deploy script: %w", err)
	}

	err = d.sshExec(client, script)
	if err != nil {
		return fmt.Errorf("ssh failed to run script: %w", err)
	}

	lg.Info(ctx, "Deployment done", "image", d.ImageTag, "host", d.host())

	return nil
}

func (d *Deployment) getEnvFile() ([]byte, error) {
	content := [][]byte{}
	for _, f := range d.EnvFiles {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("failed to read env file: %w", err)
		}

		content = append(content, b)
	}

	return bytes.Join(content, []byte("\n")), nil
}

func (d *Deployment) getRepoInfo() (string, string) {
	buf := bytes.NewBuffer(nil)

	err := execWithIO(nil, buf, buf, "echo $(git config --get remote.origin.url) $(git rev-parse HEAD)", nil)
	if err != nil {
		return err.Error(), err.Error()
	}

	info := strings.Split(strings.TrimSpace(buf.String()), " ")
	if len(info) != 2 {
		msg := fmt.Sprintf("invalid git info: %s", buf.String())
		return msg, msg
	}

	return normalizeRepoURL(info[0]), info[1]
}

// check if has dangerous docker run options.
func (d *Deployment) hasDangerousOptions() error {
	blackList := []string{
		"--privileged",
		"--mount",
	}

	for _, opt := range d.DockerRunOptions {
		for _, black := range blackList {
			if opt == black {
				return fmt.Errorf("docker run not allowed option detected: %s", opt)
			}
		}
	}

	return nil
}

func (d *Deployment) startRegistry(ctx context.Context) (string, func(), error) {
	lg.Info(ctx, "Start temporary docker registry")

	var name string

	{
		b := make([]byte, 8)

		_, err := rand.Read(b)
		if err != nil {
			return "", nil, fmt.Errorf("failed to generate name: %w", err)
		}

		name = "seploy-" + hex.EncodeToString(b)
	}

	{
		cmd := exec.Command("docker", "run", "-d", "--name", name, "--rm",
			"-p", "127.0.0.1:0:5000", "-v", "seploy-registry:/var/lib/registry", "registry")

		err := cmd.Run()
		if err != nil {
			return "", nil, fmt.Errorf("failed to start registry: %w", err)
		}
	}

	buf := bytes.NewBuffer(nil)
	cmd := exec.Command("docker", "port", name)
	cmd.Stdout = buf

	err := cmd.Run()
	if err != nil {
		return "", nil, fmt.Errorf("failed to get registry port: %w", err)
	}

	ms := regexp.MustCompile(`127.0.0.1:\d+`).FindStringSubmatch(buf.String())
	if ms == nil {
		return "", nil, fmt.Errorf("failed to get registry addr: %s", buf.String())
	}

	return ms[0], func() {
		err = execScript("docker stop "+name, nil)
		if err != nil {
			lg.Error(ctx, "Failed to stop registry", "name", name, "err", err)
		}
	}, nil
}

func (d *Deployment) waitRegistryReady(ctx context.Context, u string) error {
	u = "http://" + u

	for range 10 {
		lg.Info(ctx, "Wait registry ready...", "url", u)

		res, err := http.Get(u) //nolint: noctx
		defer func() {
			if res != nil {
				_ = res.Body.Close()
			}
		}()

		if err == nil && res.StatusCode == http.StatusOK {
			lg.Info(ctx, "Registry is ready", "url", u)
			return nil
		}

		lg.Info(ctx, "Registry not ready", "url", u, "err", err)

		time.Sleep(time.Second)
	}

	return fmt.Errorf("max retry reached")
}
