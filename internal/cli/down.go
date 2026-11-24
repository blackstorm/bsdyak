package cli

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/blackstorm/bsdyak/internal/protocol"
	"github.com/spf13/cobra"
)

func newDownCmd(opts *GlobalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "down [service...]",
		Short: "Stop and remove service jails while preserving storage ZFS datasets",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, client, err := loadAndConnect(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer client.Close()

			var resp protocol.DownResponse
			if err := client.DoJSON(cmd.Context(), http.MethodPost, protocol.RouteDown(cfg.Project.Name), protocol.DownRequest{
				Services: args,
			}, &resp); err != nil {
				return err
			}
			if len(resp.Stopped) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No running services stopped.")
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Stopped: %s\n", strings.Join(resp.Stopped, ", "))
			return nil
		},
	}
}
