package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"text/tabwriter"

	"github.com/blackstorm/bsdyak/internal/builder"
	"github.com/blackstorm/bsdyak/internal/config"
	"github.com/blackstorm/bsdyak/internal/protocol"
	"github.com/blackstorm/bsdyak/internal/transport"
	"github.com/spf13/cobra"
)

func newDeployCmd(opts *GlobalOpts) *cobra.Command {
	var (
		dryRun        bool
		localValidate bool
		release       string
	)

	cmd := &cobra.Command{
		Use:   "deploy [service...]",
		Short: "Reconcile services declared in Yakfile on the target FreeBSD host",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.ParseFile(opts.File, opts.Target)
			if err != nil {
				return err
			}
			spec := cfg.ToDeploySpec()

			if localValidate {
				fmt.Fprintf(cmd.OutOrStdout(), "Yakfile valid: project=%s freebsd=%s services=%v\n",
					spec.Project, spec.FreeBSD, spec.ServiceOrder)
				if dryRun {
					tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
					fmt.Fprintln(tw, "KIND\tTARGET\tACTION\tDETAIL")
					for _, st := range spec.Storage {
						fmt.Fprintf(tw, "storage\t%s\tensured\t%s\n", st.Name, st.Size)
					}
					for _, svcName := range spec.ServiceOrder {
						svc := spec.Services[svcName]
						fmt.Fprintf(tw, "service\t%s\treconcile\tstrategy=%s image=%s\n", svc.Name, svc.Strategy, svc.Image)
					}
					_ = tw.Flush()
				}
				return nil
			}

			if cfg.SelectedTarget == nil || cfg.SelectedTarget.Host == "" {
				return fmt.Errorf("no target selected; define a target block in %s or pass -T <target>", opts.File)
			}

			absFile, err := filepath.Abs(opts.File)
			if err != nil {
				return err
			}
			workspaceDir := filepath.Dir(absFile)
			ctx := cmd.Context()

			// Run local build stages before opening SSH if applying a new deploy.
			var localArtifacts []string
			if !dryRun && release == "" {
				localArtifacts, err = builder.RunLocalStages(ctx, workspaceDir, spec, cmd.ErrOrStderr())
				if err != nil {
					return err
				}
			}

			client, err := transport.DialSSH(ctx, cfg.SelectedTarget.Host)
			if err != nil {
				return err
			}
			defer client.Close()

			if !dryRun && release == "" {
				if err := syncWorkspace(ctx, client, spec.Project, workspaceDir); err != nil {
					return fmt.Errorf("sync workspace: %w", err)
				}
				if len(localArtifacts) > 0 {
					var artBuf bytes.Buffer
					if err := builder.PackLocalArtifacts(workspaceDir, localArtifacts, &artBuf); err != nil {
						return fmt.Errorf("pack local artifacts: %w", err)
					}
					if err := client.PostStream(ctx, protocol.RouteSyncArtifacts(spec.Project), "application/zstd", &artBuf, nil, nil); err != nil {
						return fmt.Errorf("upload local artifacts: %w", err)
					}
				}
			}

			stream, err := client.DoStream(ctx, http.MethodPost, protocol.RouteDeploy(spec.Project), protocol.DeployRequest{
				Spec:     spec,
				Services: args,
				DryRun:   dryRun,
				Release:  release,
			})
			if err != nil {
				return err
			}
			defer stream.Close()

			sc := bufio.NewScanner(stream)
			sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
			for sc.Scan() {
				var ev protocol.DeployEvent
				if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
					continue
				}
				switch ev.Type {
				case "plan":
					tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
					fmt.Fprintln(tw, "KIND\tTARGET\tACTION\tDETAIL")
					for _, p := range ev.Plan {
						fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.Kind, p.Target, p.Action, p.Detail)
					}
					_ = tw.Flush()
				case "step":
					fmt.Fprintf(cmd.OutOrStdout(), "==> %s\n", ev.Message)
				case "log":
					if ev.Service != "" {
						fmt.Fprintf(cmd.OutOrStdout(), "  [%s] %s\n", ev.Service, ev.Message)
					} else {
						fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", ev.Message)
					}
				case "done":
					fmt.Fprintf(cmd.OutOrStdout(), "Done: %s\n", ev.Message)
				case "error":
					return fmt.Errorf("%s", ev.Error)
				}
			}
			return sc.Err()
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview planned changes without modifying the host")
	cmd.Flags().BoolVar(&localValidate, "local-validate", false, "Validate Yakfile locally without connecting over SSH")
	cmd.Flags().StringVar(&release, "release", "", "Roll back specified service(s) to an existing release (e.g. r41)")
	return cmd
}

func syncWorkspace(ctx context.Context, client *transport.Client, project, workspaceDir string) error {
	manifest, err := transport.BuildSyncManifest(workspaceDir)
	if err != nil {
		return err
	}
	var resp protocol.SyncManifestResponse
	if err := client.DoJSON(ctx, http.MethodPost, protocol.RouteSyncManifest(project), protocol.SyncManifestRequest{
		Files: manifest,
	}, &resp); err != nil {
		return err
	}
	if len(resp.Missing) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := transport.PackZstdTar(workspaceDir, resp.Missing, &buf); err != nil {
		return err
	}
	return client.PostStream(ctx, protocol.RouteSyncUpload(project), "application/zstd", &buf, nil, nil)
}
