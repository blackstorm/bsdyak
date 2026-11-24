package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"text/tabwriter"

	"github.com/blackstorm/bsdyak/internal/protocol"
	"github.com/spf13/cobra"
)

func newPsCmd(opts *GlobalOpts) *cobra.Command {
	var (
		asJSON bool
		ipOnly bool
	)

	cmd := &cobra.Command{
		Use:   "ps [service]",
		Short: "Print service state, active release, jail IP, and exposed endpoints",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, client, err := loadAndConnect(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer client.Close()

			var resp protocol.PsResponse
			if err := client.DoJSON(cmd.Context(), http.MethodGet, protocol.RoutePs(cfg.Project.Name), nil, &resp); err != nil {
				return err
			}

			if len(args) == 1 {
				target := args[0]
				var filtered []protocol.ServiceStatus
				for _, s := range resp.Services {
					if s.Service == target {
						filtered = append(filtered, s)
					}
				}
				resp.Services = filtered
			}

			if ipOnly {
				if len(args) != 1 {
					return fmt.Errorf("--ip requires specifying a single service name (e.g. `yak ps db --ip`)")
				}
				if len(resp.Services) == 0 || resp.Services[0].IP == "" {
					return fmt.Errorf("service %q has no active IP", args[0])
				}
				fmt.Fprintln(cmd.OutOrStdout(), resp.Services[0].IP)
				return nil
			}

			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(resp)
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "SERVICE\tSTATE\tRELEASE\tIP\tEXPOSE\tJAIL")
			for _, s := range resp.Services {
				exp := s.ExposeListen
				if exp == "" {
					exp = s.ExposeUnix
				}
				if exp == "" {
					exp = "-"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", s.Service, s.State, s.Release, s.IP, exp, s.JailName)
			}
			return tw.Flush()
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Output machine-readable JSON")
	cmd.Flags().BoolVar(&ipOnly, "ip", false, "Print only the active jail IP for the named service")
	return cmd
}
