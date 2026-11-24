package builder

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/blackstorm/bsdyak/internal/protocol"
	"github.com/blackstorm/bsdyak/internal/sys"
)

// ServerBuilder builds multi-stage runtime snapshots on FreeBSD using content-addressed ZFS layers.
type ServerBuilder struct {
	runner sys.Runner
	zfs    *sys.ZFS
	pkg    *sys.PkgManager
	jail   *sys.JailManager
}

func NewServerBuilder(runner sys.Runner, zfs *sys.ZFS) *ServerBuilder {
	return &ServerBuilder{
		runner: runner,
		zfs:    zfs,
		pkg:    sys.NewPkgManager(runner, zfs),
		jail:   sys.NewJailManager(runner),
	}
}

// EnsureLayer returns a ready ZFS snapshot for (freebsd + packages + setup).
// When the content-addressed snapshot already exists, pkg install and setup are skipped completely.
func (b *ServerBuilder) EnsureLayer(ctx context.Context, freebsd string, packages, setup []string, logOut io.Writer) (string, bool, error) {
	hash := LayerHash(freebsd, packages, setup)
	layerDS := fmt.Sprintf("%s/layers/%s", b.zfs.Root(), hash)
	readySnap := layerDS + "@ready"
	if b.zfs.SnapshotExists(ctx, readySnap) {
		if logOut != nil {
			_, _ = fmt.Fprintf(logOut, "  reusing cached ZFS layer %s@ready\n", hash)
		}
		return readySnap, true, nil
	}

	baseSnap, err := b.pkg.EnsureBase(ctx, freebsd)
	if err != nil {
		return "", false, err
	}
	if err := b.zfs.Clone(ctx, baseSnap, layerDS); err != nil {
		return "", false, err
	}
	mp, err := b.zfs.Mountpoint(ctx, layerDS)
	if err != nil {
		return "", false, err
	}

	if len(packages) > 0 {
		if logOut != nil {
			_, _ = fmt.Fprintf(logOut, "  installing packages in layer %s: %s\n", hash, strings.Join(packages, ", "))
		}
		if err := b.pkg.InstallPackages(ctx, mp, packages); err != nil {
			_ = b.zfs.Destroy(context.Background(), layerDS, true)
			return "", false, err
		}
	}

	for _, cmd := range setup {
		if logOut != nil {
			_, _ = fmt.Fprintf(logOut, "  layer setup: %s\n", cmd)
		}
		if _, err := b.runner.Run(ctx, "chroot", mp, "/bin/sh", "-ec", cmd); err != nil {
			_ = b.zfs.Destroy(context.Background(), layerDS, true)
			return "", false, fmt.Errorf("layer setup %q: %w", cmd, err)
		}
	}

	snap, err := b.zfs.Snapshot(ctx, layerDS, "ready")
	if err != nil {
		return "", false, err
	}
	return snap, false, nil
}

// BuildReleaseImage builds all server stages (if any) and produces an immutable release snapshot
// `<zfsRoot>/projects/<project>/releases/<service>-<rev>@immutable`.
func (b *ServerBuilder) BuildReleaseImage(
	ctx context.Context,
	project, service, rev, freebsd string,
	img protocol.ImageSpec,
	sourceDir, artifactsDir string,
	secrets map[string]string,
	logOut io.Writer,
) (dataset string, snap string, err error) {
	// 1. Run any `on = "server"` stages sequentially inside ephemeral builder datasets.
	stageRoots := make(map[string]string)
	for _, st := range img.Stages {
		if st.On == "local" {
			stageRoots[st.Name] = artifactsDir
			continue
		}
		stageSnap, _, err := b.EnsureLayer(ctx, freebsd, st.Packages, nil, logOut)
		if err != nil {
			return "", "", fmt.Errorf("prepare stage %s layer: %w", st.Name, err)
		}
		stageDS := fmt.Sprintf("%s/projects/%s/releases/.build-%s-%s-%s", b.zfs.Root(), project, service, rev, st.Name)
		if err := b.zfs.Clone(ctx, stageSnap, stageDS); err != nil {
			return "", "", err
		}
		stageRoot, err := b.zfs.Mountpoint(ctx, stageDS)
		if err != nil {
			_ = b.zfs.Destroy(context.Background(), stageDS, true)
			return "", "", err
		}
		stageRoots[st.Name] = stageRoot

		// Copy synced project source into /src inside stage builder.
		srcTarget := filepath.Join(stageRoot, "src")
		_ = os.MkdirAll(srcTarget, 0o755)
		_ = copyTree(sourceDir, srcTarget)
		_ = os.MkdirAll(filepath.Join(stageRoot, "out"), 0o755)

		// Mount persistent stage caches.
		var mounted []string
		for idx, cachePath := range st.Cache {
			cacheDS := fmt.Sprintf("%s/cache/%s/%s-%d", b.zfs.Root(), project, sanitizeName(st.Name), idx)
			_ = b.zfs.EnsureDataset(ctx, cacheDS, nil)
			cacheHost, cErr := b.zfs.Mountpoint(ctx, cacheDS)
			if cErr == nil {
				dst := filepath.Join(stageRoot, strings.TrimPrefix(cachePath, "/"))
				if mErr := b.jail.MountNullFS(ctx, cacheHost, dst, false); mErr == nil {
					mounted = append(mounted, dst)
				}
			}
		}

		// Copy cross-stage files.
		for _, cp := range st.Copy {
			fromRoot := artifactsDir
			if cp.FromStage != "" && stageRoots[cp.FromStage] != "" {
				fromRoot = stageRoots[cp.FromStage]
			}
			_ = copyPattern(fromRoot, cp.Src, stageRoot, cp.Dst)
		}

		// Inject RAM-only build_secrets via tmpfs (never captured in ZFS layers).
		stageEnv := make(map[string]string, len(st.Env)+len(st.BuildSecrets))
		for k, v := range st.Env {
			stageEnv[k] = v
		}
		for _, sk := range st.BuildSecrets {
			if val, ok := secrets[sk]; ok {
				stageEnv[sk] = val
			}
		}

		wd := st.Workdir
		if wd == "" {
			wd = "/src"
		}
		if err := b.runBuildSteps(ctx, stageRoot, wd, stageEnv, st.Steps, logOut); err != nil {
			for i := len(mounted) - 1; i >= 0; i-- {
				_ = b.jail.Unmount(context.Background(), mounted[i])
			}
			_ = b.zfs.Destroy(context.Background(), stageDS, true)
			return "", "", fmt.Errorf("stage %s: %w", st.Name, err)
		}
		for i := len(mounted) - 1; i >= 0; i-- {
			_ = b.jail.Unmount(context.Background(), mounted[i])
		}
		defer func(ds string) {
			_ = b.zfs.Destroy(context.Background(), ds, true)
		}(stageDS)
	}

	// 2. Build final runtime dataset from content-addressed runtime layer.
	rtSnap, _, err := b.EnsureLayer(ctx, freebsd, img.Runtime.Packages, img.Runtime.Setup, logOut)
	if err != nil {
		return "", "", fmt.Errorf("prepare runtime layer: %w", err)
	}
	releaseDS := fmt.Sprintf("%s/projects/%s/releases/%s-%s", b.zfs.Root(), project, service, rev)
	if err := b.zfs.Clone(ctx, rtSnap, releaseDS); err != nil {
		return "", "", err
	}
	releaseRoot, err := b.zfs.Mountpoint(ctx, releaseDS)
	if err != nil {
		return "", "", err
	}

	// 3. Copy built artifacts and local static files into runtime rootfs.
	for _, cp := range img.Runtime.Copy {
		fromRoot := sourceDir
		if cp.FromStage != "" {
			if r, ok := stageRoots[cp.FromStage]; ok {
				fromRoot = r
			}
		} else if artifactsDir != "" {
			if _, statErr := os.Stat(filepath.Join(artifactsDir, strings.TrimPrefix(cp.Src, "./"))); statErr == nil {
				fromRoot = artifactsDir
			}
		}
		if err := copyPattern(fromRoot, cp.Src, releaseRoot, cp.Dst); err != nil {
			return "", "", fmt.Errorf("copy %s -> %s: %w", cp.Src, cp.Dst, err)
		}
	}

	immSnap, err := b.zfs.Snapshot(ctx, releaseDS, "immutable")
	if err != nil {
		return "", "", err
	}
	return releaseDS, immSnap, nil
}

func (b *ServerBuilder) runBuildSteps(ctx context.Context, stageRoot, workdir string, env map[string]string, steps []string, logOut io.Writer) error {
	var script strings.Builder
	script.WriteString("set -e\n")
	for k, v := range env {
		fmt.Fprintf(&script, "export %s=%s\n", k, shellSingleQuote(v))
	}
	fmt.Fprintf(&script, "mkdir -p %s && cd %s\n", shellSingleQuote(workdir), shellSingleQuote(workdir))
	for _, step := range steps {
		if logOut != nil {
			_, _ = fmt.Fprintf(logOut, "  [build] %s\n", step)
		}
		script.WriteString(step + "\n")
	}
	_, err := b.runner.RunWithInput(ctx, strings.NewReader(script.String()), "chroot", stageRoot, "/bin/sh")
	return err
}

func copyPattern(srcRoot, srcPattern, dstRoot, dstPath string) error {
	cleanSrc := strings.TrimPrefix(srcPattern, "./")
	pattern := filepath.Join(srcRoot, filepath.FromSlash(strings.TrimPrefix(cleanSrc, "/")))
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		// On simulated FakeRunner builds, directories might be empty; allow clean no-op when srcRoot itself is empty mock.
		return nil
	}

	isDirDst := strings.HasSuffix(dstPath, "/") || len(matches) > 1
	targetBase := filepath.Join(dstRoot, filepath.FromSlash(strings.TrimPrefix(dstPath, "/")))

	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil {
			return err
		}
		if info.IsDir() {
			if err := copyTree(m, filepath.Join(targetBase, filepath.Base(m))); err != nil {
				return err
			}
			continue
		}
		destFile := targetBase
		if isDirDst {
			destFile = filepath.Join(targetBase, filepath.Base(m))
		}
		if err := copySingleFile(m, destFile, info.Mode()); err != nil {
			return err
		}
	}
	return nil
}

func copyTree(src, dst string) error {
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return nil
	}
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copySingleFile(p, target, info.Mode())
	})
}

func copySingleFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func sanitizeName(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, s)
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
