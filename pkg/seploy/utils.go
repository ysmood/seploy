package seploy

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"text/template"

	"github.com/ysmood/glog/pkg/lg"
)

// List prints running containers and volumes on the target host.
func (d *Deployment) List() error {
	_, _ = os.Stdout.WriteString("\n#### Host: " + d.host() + "\n\n")

	//nolint: dupword
	err := d.sshExecWithOutput(`
		docker ps -a
		echo
		echo "## Volumes:"
		echo
		docker volume ls
	`, os.Stdout, os.Stderr)
	if err != nil {
		return fmt.Errorf("ssh failed to list containers: %w", err)
	}

	return nil
}

func (d *Deployment) RemoveContainer() error {
	name := imageName(d.ImageTag)

	err := d.sshExecWithOutput(`
		docker stop `+name+`
		docker rm -f `+name+` > /dev/null 2>&1 || true
	`, os.Stdout, os.Stderr)
	if err != nil {
		return fmt.Errorf("ssh failed to run script: %w", err)
	}

	return nil
}

func (d *Deployment) RemoveVolume(volume string) error {
	return d.sshExecWithOutput(`
		docker volume rm `+volume+`
	`, os.Stdout, os.Stderr)
}

func renderTpl(tpl string, data interface{}) (string, error) {
	template, err := template.New("").Parse(tpl)
	if err != nil {
		return "", fmt.Errorf("failed to parse template: %w", err)
	}

	var b strings.Builder

	err = template.Execute(&b, data)
	if err != nil {
		return "", fmt.Errorf("failed to execute template: %w", err)
	}

	return b.String(), nil
}

func escapeArgs(args ...string) string {
	list := []string{}

	for _, arg := range args {
		list = append(list, escapeShellString(arg))
	}

	return strings.Join(list, " ")
}

func imageName(imageTag string) string {
	return regexp.MustCompile(`:.+$`).ReplaceAllString(imageTag, "")
}

func execScript(script string, vars map[string]string) error {
	const shell = "bash"
	id := scriptHash(script, vars)

	lg.Info(context.Background(), "exec", "shell", shell, "id", id)
	defer lg.Info(context.Background(), "exec done", "shell", shell, "id", id)

	cmd := exec.Command(shell, "-c", script)

	env := []string{}
	for k, v := range vars {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	cmd.Env = append(append([]string{}, env...), os.Environ()...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func scriptHash(script string, vars map[string]string) string {
	varsData, _ := json.Marshal(vars)
	h := sha1.New()
	h.Write([]byte(script))
	h.Write(varsData)
	return fmt.Sprintf("%x", sha1.Sum(nil))
}

func escapeShellString(input string) string {
	var escaped strings.Builder

	for _, char := range input {
		if char == '\'' {
			escaped.WriteRune('\\')
		}

		escaped.WriteRune(char)
	}

	return fmt.Sprintf("'%s'", escaped.String())
}

var (
	regProto     = regexp.MustCompile(`^.*?://`)
	regUserPass  = regexp.MustCompile(`^.*?@`)
	regGitSuffix = regexp.MustCompile(`\.git$`)
)

// Example:
//
//	git@github.com:ysmood/deploy.git -> github.com/ysmood/deploy
//	https://github.com/ysmood/deploy.git -> github.com/ysmood/deploy
//	http://user:pass@github.com/ysmood/deploy.git -> github.com/ysmood/deploy
func normalizeRepoURL(u string) string {
	u = regProto.ReplaceAllString(u, "")
	u = regUserPass.ReplaceAllString(u, "")
	u = regGitSuffix.ReplaceAllString(u, "")
	u = strings.Replace(u, ":", "/", 1)

	return u
}
