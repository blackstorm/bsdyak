package sys

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type PkgManager struct {
	runner Runner
	zfs    *ZFS
}

func NewPkgManager(runner Runner, zfs *ZFS) *PkgManager {
	return &PkgManager{runner: runner, zfs: zfs}
}

// BaseTarballURL returns the official FreeBSD base.txz URL for release & architecture.
func BaseTarballURL(release, goarch string) string {
	arch := "amd64/amd64"
	if goarch == "arm64" {
		arch = "arm64/aarch64"
	}
	return fmt.Sprintf("https://download.freebsd.org/releases/%s/%s/base.txz", arch, release)
}

// EnsureBase ensures the base release ZFS dataset (<root>/bases/<release>@base) exists.
func (p *PkgManager) EnsureBase(ctx context.Context, release string) (string, error) {
	dataset := fmt.Sprintf("%s/bases/%s", p.zfs.Root(), release)
	snap := dataset + "@base"
	if p.zfs.SnapshotExists(ctx, snap) {
		return snap, nil
	}

	if err := p.zfs.EnsureDataset(ctx, dataset, nil); err != nil {
		return "", err
	}
	mp, err := p.zfs.Mountpoint(ctx, dataset)
	if err != nil {
		return "", err
	}

	tarPath := filepath.Join(os.TempDir(), fmt.Sprintf("freebsd-%s-base.txz", release))
	if _, err := os.Stat(tarPath); err != nil {
		url := BaseTarballURL(release, runtime.GOARCH)
		if _, err := p.runner.Run(ctx, "fetch", "-o", tarPath, url); err != nil {
			return "", fmt.Errorf("download FreeBSD base %s: %w", release, err)
		}
	}

	if _, err := p.runner.Run(ctx, "tar", "-xf", tarPath, "-C", mp); err != nil {
		return "", fmt.Errorf("extract base.txz into %s: %w", mp, err)
	}
	// Seed host resolv.conf so pkg works inside layers.
	_ = p.copyResolvConf(mp)

	return p.zfs.Snapshot(ctx, dataset, "base")
}

func (p *PkgManager) copyResolvConf(rootPath string) error {
	data, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return err
	}
	etcDir := filepath.Join(rootPath, "etc")
	if err := os.MkdirAll(etcDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(etcDir, "resolv.conf"), data, 0o644)
}

// InstallPackages installs FreeBSD binary packages into a chrooted/cloned rootfs.
func (p *PkgManager) InstallPackages(ctx context.Context, rootPath string, packages []string) error {
	if len(packages) == 0 {
		return nil
	}
	_ = p.copyResolvConf(rootPath)
	args := append([]string{"ASSUME_ALWAYS_YES=yes", "pkg", "-r", rootPath, "install", "-y"}, packages...)
	_, err := p.runner.Run(ctx, "env", args...)
	if err != nil && (strings.Contains(err.Error(), "not bootstrapped") || strings.Contains(err.Error(), "pkg: ")) {
		if _, bErr := p.runner.Run(ctx, "env", "ASSUME_ALWAYS_YES=yes", "pkg", "bootstrap", "-y"); bErr != nil {
			return fmt.Errorf("bootstrap pkg: %w", bErr)
		}
		_, _ = p.runner.Run(ctx, "env", "ASSUME_ALWAYS_YES=yes", "pkg", "-r", rootPath, "update")
		_, err = p.runner.Run(ctx, "env", args...)
	}
	return err
}
