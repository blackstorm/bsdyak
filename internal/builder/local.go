package builder

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/blackstorm/bsdyak/internal/protocol"
	"github.com/blackstorm/bsdyak/internal/transport"
)

// RunLocalStages runs any stage with `on = "local"` in workspaceDir and returns
// relative paths of produced artifacts needed by later runtime/server stages.
func RunLocalStages(ctx context.Context, workspaceDir string, spec protocol.DeploySpec, logOut io.Writer) ([]string, error) {
	localStageNames := make(map[string]bool)
	var artifactGlobs []string

	for _, img := range sortedImages(spec.Images) {
		for _, st := range img.Stages {
			if st.On != "local" {
				continue
			}
			localStageNames[img.Name+":"+st.Name] = true

			workdir := workspaceDir
			if st.Workdir != "" {
				workdir = filepath.Join(workspaceDir, filepath.FromSlash(strings.TrimPrefix(st.Workdir, "/")))
			}
			env := os.Environ()
			for k, v := range st.Env {
				env = append(env, fmt.Sprintf("%s=%s", k, v))
			}

			for _, step := range st.Steps {
				if logOut != nil {
					_, _ = fmt.Fprintf(logOut, "[local %s/%s] %s\n", img.Name, st.Name, step)
				}
				cmd := exec.CommandContext(ctx, "sh", "-ec", step)
				cmd.Dir = workdir
				cmd.Env = env
				if logOut != nil {
					cmd.Stdout = logOut
					cmd.Stderr = logOut
				}
				if err := cmd.Run(); err != nil {
					return nil, fmt.Errorf("local stage %s.%s step %q: %w", img.Name, st.Name, step, err)
				}
			}
		}

		// Collect any copy specs whose source comes from a local stage.
		for _, cp := range img.Runtime.Copy {
			if localStageNames[img.Name+":"+cp.FromStage] || cp.FromStage == "" {
				artifactGlobs = append(artifactGlobs, cp.Src)
			}
		}
		for _, st := range img.Stages {
			if st.On != "local" {
				for _, cp := range st.Copy {
					if localStageNames[img.Name+":"+cp.FromStage] {
						artifactGlobs = append(artifactGlobs, cp.Src)
					}
				}
			}
		}
	}

	if len(localStageNames) == 0 {
		return nil, nil
	}

	seen := make(map[string]bool)
	var relFiles []string
	for _, g := range artifactGlobs {
		clean := strings.TrimPrefix(g, "./")
		matches, err := filepath.Glob(filepath.Join(workspaceDir, filepath.FromSlash(clean)))
		if err != nil {
			return nil, fmt.Errorf("glob local artifact %q: %w", g, err)
		}
		for _, m := range matches {
			info, err := os.Stat(m)
			if err != nil || info.IsDir() {
				continue
			}
			rel, err := filepath.Rel(workspaceDir, m)
			if err != nil {
				continue
			}
			relSlash := filepath.ToSlash(rel)
			if !seen[relSlash] {
				seen[relSlash] = true
				relFiles = append(relFiles, relSlash)
			}
		}
	}
	sort.Strings(relFiles)
	return relFiles, nil
}

// PackLocalArtifacts packs local stage artifacts into a zstd tar stream.
func PackLocalArtifacts(workspaceDir string, relFiles []string, out io.Writer) error {
	return transport.PackZstdTar(workspaceDir, relFiles, out)
}

func sortedImages(m map[string]protocol.ImageSpec) []protocol.ImageSpec {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]protocol.ImageSpec, 0, len(names))
	for _, n := range names {
		out = append(out, m[n])
	}
	return out
}
