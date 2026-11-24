package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/blackstorm/bsdyak/internal/config"
	"github.com/blackstorm/bsdyak/internal/transport"
	"github.com/blackstorm/bsdyak/internal/version"
	"github.com/spf13/cobra"
)

type GlobalOpts struct {
	File   string
	Target string
}

func NewRootCmd() *cobra.Command {
	opts := &GlobalOpts{
		File: "Yakfile",
	}

	root := &cobra.Command{
		Use:           "yak",
		Short:         "Declarative, zero-magic FreeBSD Jail & ZFS deployment over SSH",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVarP(&opts.File, "file", "F", "Yakfile", "Path to Yakfile")
	root.PersistentFlags().StringVarP(&opts.Target, "target", "T", "", "Target host block from Yakfile")

	root.AddCommand(
		newDeployCmd(opts),
		newDownCmd(opts),
		newPsCmd(opts),
		newLogsCmd(opts),
		newExecCmd(opts),
		newSecretCmd(opts),
		newServerCmd(),
		newBridgeCmd(),
		newVersionCmd(),
	)

	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print yak version information",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), version.String())
		},
	}
}

func loadAndConnect(ctx context.Context, opts *GlobalOpts) (*config.Config, string, *transport.Client, error) {
	cfg, err := config.ParseFile(opts.File, opts.Target)
	if err != nil {
		return nil, "", nil, err
	}
	if cfg.SelectedTarget == nil || cfg.SelectedTarget.Host == "" {
		return nil, "", nil, fmt.Errorf("no target selected; define a target block in %s or pass -T <target>", opts.File)
	}
	absFile, err := filepath.Abs(opts.File)
	if err != nil {
		return nil, "", nil, err
	}
	client, err := transport.DialSSH(ctx, cfg.SelectedTarget.Host)
	if err != nil {
		return nil, "", nil, err
	}
	return cfg, filepath.Dir(absFile), client, nil
}
