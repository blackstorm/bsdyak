package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/blackstorm/bsdyak/internal/protocol"
	"github.com/spf13/cobra"
)

func newExecCmd(opts *GlobalOpts) *cobra.Command {
	var (
		stdinFlag bool
		ttyFlag   bool
	)

	cmd := &cobra.Command{
		Use:   "exec <service> -- <cmd...>",
		Short: "Execute a command inside a running service jail",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc := args[0]
			remoteCmd := args[1:]

			cfg, _, client, err := loadAndConnect(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer client.Close()

			shouldSendStdin := stdinFlag || ttyFlag || isPipedStdin(cmd.InOrStdin())

			conn, br, err := client.OpenExecStream(cmd.Context(), protocol.RouteExec(cfg.Project.Name), protocol.ExecRequest{
				Service: svc,
				Command: remoteCmd,
				TTY:     ttyFlag,
				Stdin:   shouldSendStdin,
			})
			if err != nil {
				return fmt.Errorf("exec %s: %w", svc, err)
			}
			defer conn.Close()

			if shouldSendStdin {
				go func() {
					_, _ = io.Copy(conn, cmd.InOrStdin())
				}()
			}

			_, err = io.Copy(cmd.OutOrStdout(), br)
			return err
		},
	}

	cmd.Flags().BoolVarP(&stdinFlag, "stdin", "i", false, "Pass stdin to the remote process")
	cmd.Flags().BoolVarP(&ttyFlag, "tty", "t", false, "Request interactive terminal session")
	return cmd
}

func isPipedStdin(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return true
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) == 0
}
