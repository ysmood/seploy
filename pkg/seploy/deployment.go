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
	"strconv"
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

// registryImage is the published registry image bundled with a heartbeat
// watchdog. See pkg/seploy/registry/ for the Dockerfile.
const registryImage = "ysmood/seploy-registry:v0.0.8"

// registryHeartbeatPeriod is how often seploy refreshes the heartbeat
// file. It must be well below the container-side timeout (1s).
const registryHeartbeatPeriod = 300 * time.Millisecond

// registryHeartbeatMount is where the heartbeat file lives inside the
// container. Matches the default in registry/entrypoint.sh.
const registryHeartbeatMount = "/tmp/seploy-heartbeat"

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

// Deploy runs the deployment. The temporary registry container reaps
// itself via its heartbeat watchdog when seploy stops refreshing the
// heartbeat file — which happens automatically on process exit (clean,
// SIGINT, SIGKILL, crash) or when ctx is cancelled.
func (d *Deployment) Deploy(ctx context.Context) error {
	lg.Info(ctx, "Start deployment", "image",
		d.ImageTag, "host", d.host(), "time", time.Now().Format(time.RFC3339))

	if err := d.hasDangerousOptions(); err != nil {
		return err
	}

	registry, err := d.startRegistry(ctx)
	if err != nil {
		return fmt.Errorf("failed to start registry: %w", err)
	}

	if err := d.deployWithRegistry(ctx, registry); err != nil {
		return err
	}

	lg.Info(ctx, "Deployment done", "image", d.ImageTag, "host", d.host())

	return nil
}

func (d *Deployment) deployWithRegistry(ctx context.Context, registry string) error {
	if err := d.waitRegistryReady(ctx, registry); err != nil {
		return fmt.Errorf("failed to wait registry ready: %w", err)
	}

	if err := d.registerImage(ctx, registry); err != nil {
		return fmt.Errorf("failed to deploy image: %w", err)
	}

	if err := d.deploy(ctx, registry); err != nil {
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

// startRegistry launches the temporary registry container and kicks off
// the heartbeat goroutine. No stop function is returned: the container's
// watchdog tears it down on its own once heartbeats stop, so cleanup
// happens uniformly whether seploy exits cleanly, crashes, or is killed.
// On error paths after `docker run`, the seeded-but-never-refreshed
// heartbeat file ages out and the watchdog reaps the container.
func (d *Deployment) startRegistry(ctx context.Context) (string, error) {
	lg.Info(ctx, "Start temporary docker registry")

	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate name: %w", err)
	}
	name := "seploy-" + hex.EncodeToString(b)

	heartbeatPath, err := createHeartbeatFile()
	if err != nil {
		return "", err
	}

	runCmd := exec.Command("docker", "run", "-d", "--name", name, "--rm",
		"-p", "127.0.0.1:0:5000",
		"-v", "seploy-registry:/var/lib/registry",
		"-v", heartbeatPath+":"+registryHeartbeatMount,
		registryImage)
	if err := runCmd.Run(); err != nil {
		_ = os.Remove(heartbeatPath)
		return "", fmt.Errorf("failed to start registry: %w", err)
	}

	buf := bytes.NewBuffer(nil)
	portCmd := exec.Command("docker", "port", name)
	portCmd.Stdout = buf
	if err := portCmd.Run(); err != nil {
		return "", fmt.Errorf("failed to get registry port: %w", err)
	}

	ms := regexp.MustCompile(`127.0.0.1:\d+`).FindStringSubmatch(buf.String())
	if ms == nil {
		return "", fmt.Errorf("failed to get registry addr: %s", buf.String())
	}

	go sendHeartbeats(ctx, heartbeatPath)

	return ms[0], nil
}

// createHeartbeatFile creates a host-side file seeded with an initial
// heartbeat value so the container's watchdog has something to compare
// against as soon as the bind mount is in place.
func createHeartbeatFile() (string, error) {
	f, err := os.CreateTemp("", "seploy-heartbeat-*")
	if err != nil {
		return "", fmt.Errorf("failed to create heartbeat file: %w", err)
	}
	_, err = f.WriteString(heartbeatValue())
	_ = f.Close()
	if err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("failed to write heartbeat file: %w", err)
	}
	return f.Name(), nil
}

// sendHeartbeats periodically overwrites the heartbeat file with a value
// that differs from the previous tick. The container's watchdog kills
// the registry when two consecutive reads see the same content. Stops
// when ctx is cancelled.
func sendHeartbeats(ctx context.Context, path string) {
	tick := func() {
		_ = os.WriteFile(path, []byte(heartbeatValue()), 0o644)
	}
	tick()

	t := time.NewTicker(registryHeartbeatPeriod)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// heartbeatValue returns a value guaranteed to differ across consecutive
// ticks at sub-second rates (nanosecond-resolution wall clock).
func heartbeatValue() string {
	return strconv.FormatInt(time.Now().UnixNano(), 10)
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
