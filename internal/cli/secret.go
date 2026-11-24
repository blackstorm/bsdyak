package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/blackstorm/bsdyak/internal/protocol"
	"github.com/spf13/cobra"
)

func newSecretCmd(opts *GlobalOpts) *cobra.Command {
	secretCmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage project secrets stored on the FreeBSD host",
	}

	setCmd := &cobra.Command{
		Use:   "set <KEY> [VALUE]",
		Short: "Set a secret value (reads stdin when VALUE is omitted)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			key := args[0]
			var val string
			if len(args) == 2 {
				val = args[1]
			} else {
				raw, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return err
				}
				val = strings.TrimRight(string(raw), "\r\n")
			}

			cfg, _, client, err := loadAndConnect(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer client.Close()

			if err := client.DoJSON(cmd.Context(), http.MethodPost, protocol.RouteSecrets(cfg.Project.Name), protocol.SecretSetRequest{
				Key:   key,
				Value: val,
			}, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Secret %q updated for project %q.\n", key, cfg.Project.Name)
			return nil
		},
	}

	unsetCmd := &cobra.Command{
		Use:   "unset <KEY>",
		Short: "Remove a secret key from the host store",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, client, err := loadAndConnect(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer client.Close()

			path := fmt.Sprintf("%s?key=%s", protocol.RouteSecrets(cfg.Project.Name), url.QueryEscape(args[0]))
			if err := client.DoJSON(cmd.Context(), http.MethodDelete, path, nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Secret %q removed.\n", args[0])
			return nil
		},
	}

	lsCmd := &cobra.Command{
		Use:   "ls",
		Short: "List configured secret keys (values are never printed)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _, client, err := loadAndConnect(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer client.Close()

			var resp protocol.SecretListResponse
			if err := client.DoJSON(cmd.Context(), http.MethodGet, protocol.RouteSecrets(cfg.Project.Name), nil, &resp); err != nil {
				return err
			}
			for _, k := range resp.Keys {
				fmt.Fprintln(cmd.OutOrStdout(), k)
			}
			return nil
		},
	}

	secretCmd.AddCommand(setCmd, unsetCmd, lsCmd)
	return secretCmd
}
