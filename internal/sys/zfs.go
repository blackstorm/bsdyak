package sys

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const MaxPoolCapacityPercent = 85

type ZFS struct {
	runner Runner
	root   string
}

func NewZFS(runner Runner, root string) *ZFS {
	return &ZFS{runner: runner, root: strings.TrimSuffix(root, "/")}
}

func (z *ZFS) Root() string {
	return z.root
}

func (z *ZFS) DatasetExists(ctx context.Context, dataset string) bool {
	_, err := z.runner.Run(ctx, "zfs", "list", "-H", "-o", "name", dataset)
	return err == nil
}

func (z *ZFS) SnapshotExists(ctx context.Context, snap string) bool {
	_, err := z.runner.Run(ctx, "zfs", "list", "-H", "-t", "snapshot", "-o", "name", snap)
	return err == nil
}

func (z *ZFS) EnsureDataset(ctx context.Context, dataset string, props map[string]string) error {
	if !z.DatasetExists(ctx, dataset) {
		args := []string{"create", "-p"}
		for _, k := range sortedKeys(props) {
			args = append(args, "-o", fmt.Sprintf("%s=%s", k, props[k]))
		}
		args = append(args, dataset)
		if _, err := z.runner.Run(ctx, "zfs", args...); err != nil {
			return err
		}
		return nil
	}
	return z.SetProperties(ctx, dataset, props)
}

func (z *ZFS) SetProperties(ctx context.Context, dataset string, props map[string]string) error {
	for _, k := range sortedKeys(props) {
		if _, err := z.runner.Run(ctx, "zfs", "set", fmt.Sprintf("%s=%s", k, props[k]), dataset); err != nil {
			return err
		}
	}
	return nil
}

func (z *ZFS) SetQuota(ctx context.Context, dataset, size string) error {
	if strings.TrimSpace(size) == "" {
		return nil
	}
	_, err := z.runner.Run(ctx, "zfs", "set", "quota="+size, dataset)
	return err
}

func (z *ZFS) Snapshot(ctx context.Context, dataset, snapName string) (string, error) {
	full := dataset + "@" + snapName
	if z.SnapshotExists(ctx, full) {
		return full, nil
	}
	if _, err := z.runner.Run(ctx, "zfs", "snapshot", full); err != nil {
		return "", err
	}
	return full, nil
}

func (z *ZFS) Clone(ctx context.Context, snapshot, targetDataset string) error {
	if z.DatasetExists(ctx, targetDataset) {
		return nil
	}
	_, err := z.runner.Run(ctx, "zfs", "clone", "-p", snapshot, targetDataset)
	return err
}

func (z *ZFS) Destroy(ctx context.Context, target string, recursive bool) error {
	args := []string{"destroy"}
	if recursive {
		args = append(args, "-r")
	}
	args = append(args, target)
	_, err := z.runner.Run(ctx, "zfs", args...)
	if err != nil && (strings.Contains(err.Error(), "dataset is busy") || strings.Contains(err.Error(), "EBUSY")) {
		return fmt.Errorf("%w: %v", ErrJailDying, err)
	}
	return err
}

func (z *ZFS) Mountpoint(ctx context.Context, dataset string) (string, error) {
	res, err := z.runner.Run(ctx, "zfs", "get", "-H", "-o", "value", "mountpoint", dataset)
	if err != nil {
		return "", err
	}
	mp := strings.TrimSpace(res.Stdout)
	if mp == "" || mp == "none" || mp == "-" {
		return "", fmt.Errorf("dataset %s has no mountpoint (%q)", dataset, mp)
	}
	return mp, nil
}

func (z *ZFS) ListSnapshots(ctx context.Context, dataset string) ([]string, error) {
	res, err := z.runner.Run(ctx, "zfs", "list", "-H", "-t", "snapshot", "-o", "name", "-s", "creation", dataset)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

func (z *ZFS) PoolName() string {
	parts := strings.SplitN(z.root, "/", 2)
	return parts[0]
}

func (z *ZFS) PoolCapacityPercent(ctx context.Context) (int, error) {
	pool := z.PoolName()
	res, err := z.runner.Run(ctx, "zpool", "list", "-H", "-o", "capacity", pool)
	if err != nil {
		return 0, err
	}
	raw := strings.TrimSpace(res.Stdout)
	raw = strings.TrimSuffix(raw, "%")
	val, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("parse zpool capacity %q: %w", res.Stdout, err)
	}
	return val, nil
}

// CheckCapacityGuard enforces the ZFS 85% pool usage safeguard to preserve CoW performance.
func (z *ZFS) CheckCapacityGuard(ctx context.Context) error {
	pct, err := z.PoolCapacityPercent(ctx)
	if err != nil {
		return err
	}
	if pct >= MaxPoolCapacityPercent {
		return fmt.Errorf("%w: pool %s is at %d%% (limit %d%%); free space or prune old snapshots before deploying",
			ErrPoolCapacity, z.PoolName(), pct, MaxPoolCapacityPercent)
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
