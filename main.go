package main

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"

	"github.com/urfave/cli/v3"
	"github.com/ysmood/glog"
	"github.com/ysmood/glog/pkg/lg"
	"github.com/ysmood/seploy/pkg/seploy"
)

var version = ""

func main() {
	ctx := context.Background()

	glog.SetupDefaultSlog()

	app := &cli.Command{
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
				Name:    "target",
				Aliases: []string{"t"},
				Usage:   "SSH target (e.g. admin@host)",
			},
			&cli.StringFlag{
				Name:    "private-key",
				Aliases: []string{"p"},
				Usage:   "Path to SSH private key file",
			},
		},
		Commands: []*cli.Command{
			{
				Name:         "up",
				Usage:        "Deploy a container to a host",
				StopOnNthArg: func() *int { n := 1; return &n }(),
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
						Usage:   "dotenv files to set environment variables for the container",
					},
					&cli.StringSliceFlag{
						Name:    "volume",
						Aliases: []string{"v"},
						Usage:   `Bind mount a name scoped volume, such "seploy -t admin@stg up -v data:/data my-app" will become "-v my-app-data:/data"`,
					},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					target := requireTarget(ctx, c)
					if c.NArg() < 1 {
						lg.Error(ctx, "IMAGE_TAG arg is required")
						err := cli.ShowSubcommandHelp(c)
						if err != nil {
							lg.Error(ctx, "Failed to show help", "err", err)
						}
						os.Exit(1)
					}

					d, err := newDeployment(ctx, target, c.String("private-key"))
					if err != nil {
						lg.Error(ctx, err.Error())
						os.Exit(1)
					}
					d.ImageTag = c.Args().Get(0)
					d.NotService = c.Bool("not-service")
					d.Follow = c.Bool("follow")
					d.EnvFiles = c.StringSlice("env-file")
					d.DockerRunVolumes = c.StringSlice("volume")

					d.DockerRunOptions, d.DockerRunCommands = parseDockerRunArgs(c.Args().Slice()[1:], os.Args)

					if err := d.Deploy(ctx); err != nil {
						lg.Error(ctx, "Failed to run container", "err", err)
						os.Exit(1)
					}

					return nil
				},
			},
			{
				Name:      "remove",
				Usage:     "Remove a container from a host",
				UsageText: `seploy -t SSH_TARGET remove IMAGE_TAG`,
				Action: func(ctx context.Context, c *cli.Command) error {
					target := requireTarget(ctx, c)
					if c.NArg() < 1 {
						lg.Error(ctx, "IMAGE_TAG arg is required")
						err := cli.ShowSubcommandHelp(c)
						if err != nil {
							lg.Error(ctx, "Failed to show help", "err", err)
						}
						os.Exit(1)
					}

					d, err := newDeployment(ctx, target, c.String("private-key"))
					if err != nil {
						lg.Error(ctx, err.Error())
						os.Exit(1)
					}
					d.ImageTag = c.Args().Get(0)
					err = d.RemoveContainer()
					if err != nil {
						lg.Error(ctx, "Failed to remove container", "err", err)
						os.Exit(1)
					}

					return nil
				},
			},
			{
				Name:      "remove-volume",
				Usage:     "Remove a volume on a host",
				UsageText: `seploy -t SSH_TARGET remove-volume VOLUME_NAME`,
				Action: func(ctx context.Context, c *cli.Command) error {
					target := requireTarget(ctx, c)
					if c.NArg() < 1 {
						lg.Error(ctx, "VOLUME_NAME arg is required")
						err := cli.ShowSubcommandHelp(c)
						if err != nil {
							lg.Error(ctx, "Failed to show help", "err", err)
						}
						os.Exit(1)
					}

					volumeName := c.Args().Get(0)
					d, err := newDeployment(ctx, target, c.String("private-key"))
					if err != nil {
						lg.Error(ctx, err.Error())
						os.Exit(1)
					}
					err = d.RemoveVolume(volumeName)
					if err != nil {
						lg.Error(ctx, "Failed to remove volume", "err", err)
						os.Exit(1)
					}

					return nil
				},
			},
			{
				Name:      "list",
				Usage:     "List container resources",
				UsageText: `seploy -t SSH_TARGET list`,
				Action: func(ctx context.Context, c *cli.Command) error {
					d, err := newDeployment(ctx, c.String("target"), c.String("private-key"))
					if err != nil {
						lg.Error(ctx, err.Error())
						os.Exit(1)
					}
					err = d.List()
					if err != nil {
						lg.Error(ctx, "Failed to list resources", "err", err)
						os.Exit(1)
					}

					return nil
				},
			},
		},
	}

	err := app.Run(ctx, os.Args)
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

func requireTarget(ctx context.Context, c *cli.Command) string {
	target := c.String("target")
	if target == "" {
		lg.Error(ctx, "SSH target is required (-t SSH_TARGET)")
		err := cli.ShowSubcommandHelp(c)
		if err != nil {
			lg.Error(ctx, "Failed to show help", "err", err)
		}
		os.Exit(1)
	}
	return target
}

// parseDockerRunArgs splits args into docker-run options and commands.
// urfave/cli/v3 strips "--" from c.Args(), so rawArgs (os.Args) is scanned
// to locate the terminator.
func parseDockerRunArgs(args, rawArgs []string) ([]string, []string) {
	var commands []string
	for i, a := range rawArgs {
		if a == "--" {
			commands = rawArgs[i+1:]
			break
		}
	}
	options := args[:len(args)-len(commands)]
	return options, commands
}
