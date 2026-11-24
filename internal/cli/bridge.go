package cli

import (
	"os"

	"github.com/blackstorm/bsdyak/internal/sys"
	"github.com/blackstorm/bsdyak/internal/transport"
	"github.com/spf13/cobra"
)

func newBridgeCmd() *cobra.Command {
	var (
		allowProject string
		socketPath   string
	)

	cmd := &cobra.Command{
		Use:    "bridge",
		Short:  "Internal SSH stdio bridge to /var/run/yakd.sock",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return transport.RunBridge(cmd.Context(), socketPath, allowProject, os.Stdin, os.Stdout)
		},
	}

	cmd.Flags().StringVar(&allowProject, "allow-project", "", "Restrict bridge access to a single project")
	cmd.Flags().StringVar(&socketPath, "socket", sys.DefaultSocketPath, "Path to yakd Unix socket")
	return cmd
}
