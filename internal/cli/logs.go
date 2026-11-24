package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/blackstorm/bsdyak/internal/protocol"
	"github.com/spf13/cobra"
)

func newLogsCmd(opts *GlobalOpts) *cobra.Command {
	var (
		follow bool
		lines  int
	)

	cmd := &cobra.Command{
		Use:   "logs <service>",
		Short: "Stream or tail service logs to stdout",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, client, err := loadAndConnect(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer client.Close()

			q := url.Values{}
			q.Set("service", args[0])
			q.Set("n", strconv.Itoa(lines))
			q.Set("follow", strconv.FormatBool(follow))

			path := fmt.Sprintf("%s?%s", protocol.RouteLogs(cfg.Project.Name), q.Encode())
			stream, err := client.DoStream(cmd.Context(), http.MethodGet, path, nil)
			if err != nil {
				return err
			}
			defer stream.Close()

			_, err = io.Copy(cmd.OutOrStdout(), stream)
			return err
		},
	}

	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow log output")
	cmd.Flags().IntVarP(&lines, "lines", "n", 100, "Number of recent log lines to show")
	return cmd
}
