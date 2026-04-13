package seploy

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"text/template"
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
		docker rm -f `+name+` > /dev/null 2>&1
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
	stdout := newPrefixedWriter(os.Stdout, "bash | ")
	stderr := newPrefixedWriter(os.Stderr, "bash ! ")

	defer stdout.Flush()
	defer stderr.Flush()

	return execWithIO(nil, stdout, stderr, script, vars)
}

func execWithIO(input io.Reader, stdout, stderr io.Writer, script string, vars map[string]string) error {
	cmd := exec.Command("bash", "-c", script)

	env := []string{}
	for k, v := range vars {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	cmd.Env = append(append([]string{}, env...), os.Environ()...)
	cmd.Stdin = input
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	return cmd.Run()
}

const prefixedWriterMaxBuf = 64 * 1024

type prefixedWriter struct {
	w      io.Writer
	prefix []byte
	buf    []byte
}

func newPrefixedWriter(w io.Writer, prefix string) *prefixedWriter {
	return &prefixedWriter{w: w, prefix: []byte(prefix)}
}

func (pw *prefixedWriter) Write(p []byte) (int, error) {
	pw.buf = append(pw.buf, p...)

	for {
		i := bytes.IndexByte(pw.buf, '\n')
		if i < 0 {
			if len(pw.buf) >= prefixedWriterMaxBuf {
				if err := pw.emit(pw.buf); err != nil {
					return 0, err
				}
				pw.buf = pw.buf[:0]
			}
			break
		}

		if err := pw.emit(pw.buf[:i]); err != nil {
			return 0, err
		}

		pw.buf = pw.buf[i+1:]
	}

	// Reclaim capacity once the backing array grows large but is mostly empty.
	if cap(pw.buf) > prefixedWriterMaxBuf && len(pw.buf) < cap(pw.buf)/4 {
		pw.buf = append([]byte(nil), pw.buf...)
	}

	return len(p), nil
}

func (pw *prefixedWriter) emit(line []byte) error {
	if _, err := pw.w.Write(pw.prefix); err != nil {
		return err
	}
	if _, err := pw.w.Write(line); err != nil {
		return err
	}
	if _, err := pw.w.Write([]byte{'\n'}); err != nil {
		return err
	}
	return nil
}

// Flush writes any buffered bytes that never got a trailing newline.
func (pw *prefixedWriter) Flush() {
	if len(pw.buf) == 0 {
		return
	}
	if err := pw.emit(pw.buf); err != nil {
		slog.Error("failed to flush prefixed writer", "error", err)
		return
	}
	pw.buf = nil
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
