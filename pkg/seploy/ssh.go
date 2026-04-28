package seploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/ysmood/glog/pkg/lg"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func (d *Deployment) sshExec(client *ssh.Client, script string) error {
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create ssh session: %w", err)
	}
	defer func() { _ = session.Close() }()

	session.Stdin = bytes.NewBufferString(script)
	session.Stdout = newPrefixedWriter(os.Stdout, d.host()+" | ")
	session.Stderr = newPrefixedWriter(os.Stderr, d.host()+" ! ")

	return session.Run("sudo bash -s")
}

func (d *Deployment) sshExecWithOutput(script string, stdout, stderr io.Writer) error {
	client, err := d.connectSSH()
	if err != nil {
		return fmt.Errorf("failed to connect to ssh: %w", err)
	}
	defer func() { _ = client.Close() }()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create ssh session: %w", err)
	}
	defer func() { _ = session.Close() }()

	session.Stdin = bytes.NewBufferString(script)
	session.Stdout = stdout
	session.Stderr = stderr

	return session.Run("sudo bash -s")
}

func (d *Deployment) connectSSH() (*ssh.Client, error) {
	user, host, port, err := parseSSHTarget(d.SSHTarget)
	if err != nil {
		return nil, err
	}

	auths := []ssh.AuthMethod{}

	if len(d.SSHPrivateKey) > 0 {
		signer, err := ssh.ParsePrivateKey(d.SSHPrivateKey)
		if err != nil {
			return nil, fmt.Errorf("failed to parse private key: %w", err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}

	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		conn, err := net.Dial("unix", sock)
		if err == nil {
			agentClient := agent.NewClient(conn)
			auths = append(auths, ssh.PublicKeysCallback(agentClient.Signers))
		}
	}

	if len(auths) == 0 {
		return nil, errors.New("no auth methods available")
	}

	config := &ssh.ClientConfig{
		User:            user,
		Auth:            auths,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		// Forward server banner messages (e.g. Tailscale SSH check-mode
		// login URLs) instead of silently dropping them.
		BannerCallback: func(message string) error {
			_, _ = newPrefixedWriter(os.Stderr, host+" ! ").Write([]byte(message))
			return nil
		},
	}

	addr := net.JoinHostPort(host, port)

	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return nil, fmt.Errorf("failed to establish ssh connection: %w", err)
	}

	return client, nil
}

// host returns the host portion of SSHTarget, or SSHTarget itself if parsing fails.
func (d *Deployment) host() string {
	_, h, _, err := parseSSHTarget(d.SSHTarget)
	if err != nil {
		return d.SSHTarget
	}
	return h
}

// parseSSHTarget parses "user@host[:port]" and returns user, host and port
// (defaulting to "22" when omitted).
func parseSSHTarget(s string) (user, host, port string, err error) {
	at := strings.Index(s, "@")
	if at <= 0 || at == len(s)-1 {
		return "", "", "", fmt.Errorf("invalid SSHTarget %q: expected user@host[:port]", s)
	}

	user = s[:at]
	rest := s[at+1:]

	if h, p, splitErr := net.SplitHostPort(rest); splitErr == nil {
		return user, h, p, nil
	}

	return user, rest, "22", nil
}

func (d *Deployment) proxyRegistry(ctx context.Context, client *ssh.Client, destRegistry string) (string, error) {
	registryProxy, err := client.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("failed to set up reverse port forwarding: %w", err)
	}

	srcRegistry := registryProxy.Addr().(*net.TCPAddr).String() //nolint: forcetypeassert

	lg.Info(ctx, "Proxy registry", "src", srcRegistry, "dest", destRegistry)

	go func() {
		for {
			src, err := registryProxy.Accept()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return
				}

				lg.Error(ctx, "Failed to accept registry connection", "err", err)

				return
			}

			go func() {
				defer func() { _ = src.Close() }()

				dest, err := net.Dial("tcp", destRegistry)
				if err != nil {
					lg.Error(ctx, "Failed to dial registry", "err", err)
					return
				}
				defer func() { _ = dest.Close() }()

				proxyCopy(ctx, src, dest)
			}()
		}
	}()

	return srcRegistry, nil
}

// proxyCopy shuttles bytes in both directions between a and b, returning when
// both directions are done. Errors from a peer closing the connection are
// expected and not logged.
func proxyCopy(ctx context.Context, a, b net.Conn) {
	done := make(chan struct{})

	go func() {
		defer close(done)

		if _, err := io.Copy(b, a); err != nil && !isClosedConnErr(err) {
			lg.Error(ctx, "Failed to copy from src to registry", "err", err)
		}

		if c, ok := b.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
	}()

	if _, err := io.Copy(a, b); err != nil && !isClosedConnErr(err) {
		lg.Error(ctx, "Failed to copy from registry to dest", "err", err)
	}

	if c, ok := a.(interface{ CloseWrite() error }); ok {
		_ = c.CloseWrite()
	}

	<-done
}

func isClosedConnErr(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF)
}
