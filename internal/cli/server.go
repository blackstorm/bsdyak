package cli

import (
	"fmt"
	"os/signal"
	"syscall"

	"github.com/blackstorm/bsdyak/internal/daemon"
	"github.com/blackstorm/bsdyak/internal/sys"
	"github.com/spf13/cobra"
)

func newServerCmd() *cobra.Command {
	serverCmd := &cobra.Command{
		Use:   "server",
		Short: "Manage the yakd host daemon on a FreeBSD 14.x server",
	}

	var (
		zfsRoot    string
		egress     string
		configPath string
		rcPath     string
	)

	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Idempotently initialize Yak ZFS datasets, directories, yak group, and rc.d script",
		RunE: func(cmd *cobra.Command, _ []string) error {
			sc := sys.DefaultServerConfig()
			sc.ZFSRoot = zfsRoot
			sc.Egress = egress
			if err := sys.InitHost(cmd.Context(), sys.OSRunner{}, sys.InitOptions{
				ConfigPath: configPath,
				RCPath:     rcPath,
				ServerCfg:  sc,
			}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Initialized Yak server:\n  ZFS root: %s\n  Config:   %s\n  rc.d:     %s\nNext step: run `sudo yak server check`\n",
				sc.ZFSRoot, configPath, rcPath)
			return nil
		},
	}
	initCmd.Flags().StringVar(&zfsRoot, "zfs-root", sys.DefaultZFSRoot, "Parent ZFS dataset for Yak")
	initCmd.Flags().StringVar(&egress, "egress", "vtnet0", "Host external interface used for jail NAT")
	initCmd.Flags().StringVar(&configPath, "config", sys.DefaultConfigPath, "Path to write yak.conf")
	initCmd.Flags().StringVar(&rcPath, "rc-path", sys.DefaultRCPath, "Path to write rc.d service script")

	checkCmd := &cobra.Command{
		Use:   "check",
		Short: "Check FreeBSD host readiness without modifying system config files",
		RunE: func(cmd *cobra.Command, _ []string) error {
			sc, err := sys.LoadServerConfig(configPath)
			if err != nil {
				return err
			}
			checks := sys.CheckHost(cmd.Context(), sys.OSRunner{}, sc)
			allOK := true
			for _, c := range checks {
				badge := "OK"
				if !c.OK {
					badge = "WARN"
					if c.Name != "Resource Limits (rctl, optional)" {
						allOK = false
					}
				}
				fmt.Fprintf(cmd.OutOrStdout(), "[%s] %s: %s\n", badge, c.Name, c.Detail)
				if !c.OK && c.Suggest != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "      -> %s\n", c.Suggest)
				}
			}
			if !allOK {
				return fmt.Errorf("host check found missing required items")
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Host is ready. Enable and start daemon: service yakd enable && service yakd start")
			return nil
		},
	}
	checkCmd.Flags().StringVar(&configPath, "config", sys.DefaultConfigPath, "Path to yak.conf")

	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Run the yakd daemon (typically invoked by rc.d)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			sc, err := sys.LoadServerConfig(configPath)
			if err != nil {
				return err
			}
			srv, err := daemon.NewServer(sc, sys.OSRunner{}, "")
			if err != nil {
				return err
			}
			defer srv.Close()

			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()
			return srv.ListenAndServe(ctx)
		},
	}
	runCmd.Flags().StringVar(&configPath, "config", sys.DefaultConfigPath, "Path to yak.conf")

	serverCmd.AddCommand(initCmd, checkCmd, runCmd)
	return serverCmd
}
