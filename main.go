package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/urfave/cli/v2"
	"github.com/ysmood/glog"
	"github.com/ysmood/glog/pkg/lg"
	"github.com/ysmood/seploy/pkg/seploy"
)

var version = ""

func main() {
	ctx := context.Background()

	glog.SetupDefaultSlog()

	app := &cli.App{
		Name:  "seploy",
		Usage: `Securely deploy containers to remote hosts`,
		Version: func() string {
			if version != "" {
				return version
			}

			if info, ok := debug.ReadBuildInfo(); ok {
				return info.Main.Version
			}

			return "dev"
		}(),
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "target",
				Aliases:  []string{"t"},
				Required: true,
				Usage:    "SSH target (e.g. admin@host)",
			},
			&cli.StringFlag{
				Name:    "private-key",
				Aliases: []string{"p"},
				Usage:   "Path to SSH private key file",
			},
		},
		Commands: []*cli.Command{
			{
				Name:  "up",
				Usage: "Deploy a container to a host",
				UsageText: `seploy -t SSH_TARGET up [OPTION...] IMAGE_TAG [DOCKER_RUN_ARG...] [-- [DOCKER_RUN_COMMAND...]]

EXAMPLES:
   seploy -t admin@stg up nginx
   seploy -p private/key/path -t admin@stg up nginx
   seploy -t admin@stg up nginx -p 80:80 --memory 500MB -v nginx:/var/cache/nginx
   seploy -t admin@prd up my-service -- /serve
   seploy -t admin@dev up -not-service -follow job
`,
				Flags: []cli.Flag{
					&cli.BoolFlag{
						Name:    "not-service",
						Aliases: []string{"n"},
						Usage:   "Do not run as a service",
					},
					&cli.BoolFlag{
						Name:    "follow",
						Aliases: []string{"f"},
						Usage:   "Follow the logs of the container",
					},
					&cli.StringSliceFlag{
						Name:    "env-file",
						Aliases: []string{"e"},
						Usage:   "dotenv file to set environment variables for the container",
					},
					&cli.StringSliceFlag{
						Name:    "volume",
						Aliases: []string{"v"},
						Usage:   "Bind mount a name scoped volume, such `seploy -t admin@stg up -v data:/data my-app` will become `-v my-app-data:/data`",
					},
				},
				Action: func(c *cli.Context) error {
					target := requireTarget(c)
					if c.NArg() < 1 {
						lg.Error(c.Context, "IMAGE_TAG arg is required")
						err := cli.ShowSubcommandHelp(c)
						if err != nil {
							lg.Error(c.Context, "Failed to show help", "err", err)
						}
						os.Exit(1)
					}

					d, err := newDeployment(c.Context, target, c.String("private-key"))
					if err != nil {
						lg.Error(c.Context, err.Error())
						os.Exit(1)
					}
					d.ImageTag = c.Args().Get(0)
					d.NotService = c.Bool("not-service")
					d.Follow = c.Bool("follow")
					d.EnvFiles = c.StringSlice("env-file")
					d.DockerRunVolumes = c.StringSlice("volume")

					d.DockerRunOptions, d.DockerRunCommands = parseDockerRunArgs(c.Args().Slice()[1:])

					closer := func() {}
					sigCh := make(chan os.Signal, 1)
					signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
					defer signal.Stop(sigCh)
					go func() {
						sig, ok := <-sigCh
						if !ok {
							return
						}
						lg.Info(c.Context, "Received signal, cleaning up", "signal", sig)
						closer()
						os.Exit(1)
					}()

					close, err := d.Deploy(c.Context)
					closer = close
					defer close()
					if err != nil {
						lg.Error(c.Context, "Failed to run container", "err", err)
						os.Exit(1)
					}

					return nil
				},
			},
			{
				Name:      "remove",
				Usage:     "Remove a container from a host",
				UsageText: `seploy -t SSH_TARGET remove IMAGE_TAG`,
				Action: func(c *cli.Context) error {
					target := requireTarget(c)
					if c.NArg() < 1 {
						lg.Error(c.Context, "IMAGE_TAG arg is required")
						err := cli.ShowSubcommandHelp(c)
						if err != nil {
							lg.Error(c.Context, "Failed to show help", "err", err)
						}
						os.Exit(1)
					}

					d, err := newDeployment(c.Context, target, c.String("private-key"))
					if err != nil {
						lg.Error(c.Context, err.Error())
						os.Exit(1)
					}
					d.ImageTag = c.Args().Get(0)
					err = d.RemoveContainer()
					if err != nil {
						lg.Error(c.Context, "Failed to remove container", "err", err)
						os.Exit(1)
					}

					return nil
				},
			},
			{
				Name:      "remove-volume",
				Usage:     "Remove a volume on a host",
				UsageText: `seploy -t SSH_TARGET remove-volume VOLUME_NAME`,
				Action: func(c *cli.Context) error {
					target := requireTarget(c)
					if c.NArg() < 1 {
						lg.Error(c.Context, "VOLUME_NAME arg is required")
						err := cli.ShowSubcommandHelp(c)
						if err != nil {
							lg.Error(c.Context, "Failed to show help", "err", err)
						}
						os.Exit(1)
					}

					volumeName := c.Args().Get(0)
					d, err := newDeployment(c.Context, target, c.String("private-key"))
					if err != nil {
						lg.Error(c.Context, err.Error())
						os.Exit(1)
					}
					err = d.RemoveVolume(volumeName)
					if err != nil {
						lg.Error(c.Context, "Failed to remove volume", "err", err)
						os.Exit(1)
					}

					return nil
				},
			},
			{
				Name:      "list",
				Usage:     "List container resources",
				UsageText: `seploy -t SSH_TARGET list`,
				Action: func(c *cli.Context) error {
					d, err := newDeployment(c.Context, c.String("target"), c.String("private-key"))
					if err != nil {
						lg.Error(c.Context, err.Error())
						os.Exit(1)
					}
					err = d.List()
					if err != nil {
						lg.Error(c.Context, "Failed to list resources", "err", err)
						os.Exit(1)
					}

					return nil
				},
			},
		},
	}

	err := app.RunContext(ctx, os.Args)
	if err != nil {
		lg.Error(ctx, "Failed to run app", "err", err)
		os.Exit(1)
	}
}

func newDeployment(ctx context.Context, sshTarget, privateKeyPath string) (*seploy.Deployment, error) {
	d := &seploy.Deployment{SSHTarget: sshTarget}

	if privateKeyPath == "" {
		lg.Info(ctx, "No private key provided, falling back to ssh-agent")
		return d, nil
	}

	key, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read private key: %w", err)
	}
	d.SSHPrivateKey = key

	return d, nil
}

func requireTarget(c *cli.Context) string {
	target := c.String("target")
	if target == "" {
		lg.Error(c.Context, "SSH target is required (-t SSH_TARGET)")
		err := cli.ShowSubcommandHelp(c)
		if err != nil {
			lg.Error(c.Context, "Failed to show help", "err", err)
		}
		os.Exit(1)
	}
	return target
}

func parseDockerRunArgs(args []string) ([]string, []string) {
	var dockerRunArgs []string
	var commands []string

	for i, arg := range args {
		if arg == "--" {
			if i+1 < len(args) {
				commands = args[i+1:]
			}
			break
		}
		dockerRunArgs = append(dockerRunArgs, arg)
	}

	return dockerRunArgs, commands
}
