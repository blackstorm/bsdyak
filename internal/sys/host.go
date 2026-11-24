package sys

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	DefaultConfigPath = "/usr/local/etc/yak.conf"
	DefaultRCPath     = "/usr/local/etc/rc.d/yakd"
	DefaultSocketPath = "/var/run/yakd.sock"
	DefaultStateDir   = "/var/db/yak"
	DefaultRunDir     = "/var/run/yak"
	DefaultZFSRoot    = "zroot/yak"
)

type ServerConfig struct {
	ZFSRoot    string `json:"zfs_root"`
	Egress     string `json:"egress_iface"`
	StateDir   string `json:"state_dir"`
	RunDir     string `json:"run_dir"`
	SocketPath string `json:"socket_path"`
}

func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		ZFSRoot:    DefaultZFSRoot,
		Egress:     "vtnet0",
		StateDir:   DefaultStateDir,
		RunDir:     DefaultRunDir,
		SocketPath: DefaultSocketPath,
	}
}

func LoadServerConfig(path string) (ServerConfig, error) {
	cfg := DefaultServerConfig()
	if path == "" {
		path = DefaultConfigPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.ZFSRoot == "" {
		cfg.ZFSRoot = DefaultZFSRoot
	}
	if cfg.StateDir == "" {
		cfg.StateDir = DefaultStateDir
	}
	if cfg.RunDir == "" {
		cfg.RunDir = DefaultRunDir
	}
	if cfg.SocketPath == "" {
		cfg.SocketPath = DefaultSocketPath
	}
	return cfg, nil
}

const yakdRCScript = `#!/bin/sh
#
# PROVIDE: yakd
# REQUIRE: FILESYSTEMS NETWORKING pf
# KEYWORD: shutdown

. /etc/rc.subr

name="yakd"
rcvar="yakd_enable"
load_rc_config $name

: ${yakd_enable:="NO"}
: ${yakd_config:="/usr/local/etc/yak.conf"}

pidfile="/var/run/${name}.pid"
procname="/usr/local/bin/yak"
command="/usr/sbin/daemon"
command_args="-f -P ${pidfile} -r -o /var/log/yakd.log -t yakd -T yakd ${procname} server run --config ${yakd_config}"
extra_commands="status"

run_rc_command "$1"
`

type InitOptions struct {
	ConfigPath string
	RCPath     string
	ServerCfg  ServerConfig
}

// InitHost idempotently provisions Yak-owned datasets, directories, group, config, and rc.d script.
// Host-wide configuration files (/etc/pf.conf, /etc/rc.conf, /boot/loader.conf) are never modified.
func InitHost(ctx context.Context, runner Runner, opts InitOptions) error {
	cfg := opts.ServerCfg
	if cfg.ZFSRoot == "" {
		cfg.ZFSRoot = DefaultZFSRoot
	}
	if cfg.StateDir == "" {
		cfg.StateDir = DefaultStateDir
	}
	if cfg.RunDir == "" {
		cfg.RunDir = DefaultRunDir
	}
	if cfg.SocketPath == "" {
		cfg.SocketPath = DefaultSocketPath
	}
	if opts.ConfigPath == "" {
		opts.ConfigPath = DefaultConfigPath
	}
	if opts.RCPath == "" {
		opts.RCPath = DefaultRCPath
	}

	// 1. Ensure yak group exists for non-root socket access.
	if _, err := runner.Run(ctx, "pw", "groupshow", "yak"); err != nil {
		if _, gErr := runner.Run(ctx, "pw", "groupadd", "yak"); gErr != nil {
			return fmt.Errorf("create yak group: %w", gErr)
		}
	}

	// 2. Ensure directories exist with secure permissions.
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.RunDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(opts.ConfigPath), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(opts.RCPath), 0o755); err != nil {
		return err
	}

	// 3. Provision ZFS root dataset subtree (explicit mountpoint=/yak because FreeBSD zroot defaults to mountpoint=none).
	zfs := NewZFS(runner, cfg.ZFSRoot)
	if err := zfs.EnsureDataset(ctx, cfg.ZFSRoot, map[string]string{"mountpoint": "/yak"}); err != nil {
		return fmt.Errorf("ensure dataset %s: %w", cfg.ZFSRoot, err)
	}
	for _, sub := range []string{"/bases", "/layers", "/cache", "/projects"} {
		if err := zfs.EnsureDataset(ctx, cfg.ZFSRoot+sub, nil); err != nil {
			return fmt.Errorf("ensure dataset %s%s: %w", cfg.ZFSRoot, sub, err)
		}
	}

	// 4. Write Yak-owned server config & rc.d script atomically so re-running init is idempotent.
	rawCfg, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(opts.ConfigPath, append(rawCfg, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", opts.ConfigPath, err)
	}
	if err := writeFileAtomic(opts.RCPath, []byte(yakdRCScript), 0o555); err != nil {
		return fmt.Errorf("write %s: %w", opts.RCPath, err)
	}
	return nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

type CheckItem struct {
	Name    string
	OK      bool
	Detail  string
	Suggest string
}

// CheckHost inspects host readiness without altering /etc/pf.conf, /etc/rc.conf, or /boot/loader.conf.
func CheckHost(ctx context.Context, runner Runner, cfg ServerConfig) []CheckItem {
	var checks []CheckItem

	// 1. FreeBSD version
	if res, err := runner.Run(ctx, "freebsd-version", "-k"); err == nil {
		ver := strings.TrimSpace(res.Stdout)
		ok := strings.HasPrefix(ver, "14.") || strings.HasPrefix(ver, "15.")
		checks = append(checks, CheckItem{
			Name:    "FreeBSD Kernel",
			OK:      ok,
			Detail:  ver,
			Suggest: "Upgrade host to FreeBSD 14.x or newer",
		})
	} else {
		checks = append(checks, CheckItem{Name: "FreeBSD Kernel", OK: false, Detail: err.Error(), Suggest: "Run on FreeBSD 14.x"})
	}

	// 2. ZFS root
	zfs := NewZFS(runner, cfg.ZFSRoot)
	if zfs.DatasetExists(ctx, cfg.ZFSRoot) {
		pct, err := zfs.PoolCapacityPercent(ctx)
		if err == nil {
			checks = append(checks, CheckItem{
				Name:    "ZFS Root Dataset",
				OK:      pct < MaxPoolCapacityPercent,
				Detail:  fmt.Sprintf("%s (pool %d%% used)", cfg.ZFSRoot, pct),
				Suggest: "Ensure pool usage stays below 85%",
			})
		} else {
			checks = append(checks, CheckItem{Name: "ZFS Root Dataset", OK: true, Detail: cfg.ZFSRoot})
		}
	} else {
		checks = append(checks, CheckItem{
			Name:    "ZFS Root Dataset",
			OK:      false,
			Detail:  cfg.ZFSRoot + " missing",
			Suggest: "Run `sudo yak server init`",
		})
	}

	// 3. yak group
	if _, err := runner.Run(ctx, "pw", "groupshow", "yak"); err == nil {
		checks = append(checks, CheckItem{Name: "System Group (yak)", OK: true, Detail: "group yak exists"})
	} else {
		checks = append(checks, CheckItem{
			Name:    "System Group (yak)",
			OK:      false,
			Detail:  "group yak missing",
			Suggest: "Run `sudo yak server init`",
		})
	}

	// 4. IP forwarding
	if res, err := runner.Run(ctx, "sysctl", "-n", "net.inet.ip.forwarding"); err == nil && strings.TrimSpace(res.Stdout) == "1" {
		checks = append(checks, CheckItem{Name: "IPv4 Forwarding", OK: true, Detail: "net.inet.ip.forwarding=1"})
	} else {
		checks = append(checks, CheckItem{
			Name:    "IPv4 Forwarding",
			OK:      false,
			Detail:  "net.inet.ip.forwarding != 1",
			Suggest: `sysrc gateway_enable="YES" && sysctl net.inet.ip.forwarding=1`,
		})
	}

	// 5. pf anchors inside /etc/pf.conf
	pfData, err := os.ReadFile("/etc/pf.conf")
	pfText := string(pfData)
	hasNatAnchor := err == nil && strings.Contains(pfText, `nat-anchor "yak/*"`)
	hasRuleAnchor := err == nil && strings.Contains(pfText, `anchor "yak/*"`)
	checks = append(checks, CheckItem{
		Name:    "PF Anchors (/etc/pf.conf)",
		OK:      hasNatAnchor && hasRuleAnchor,
		Detail:  fmt.Sprintf("nat-anchor=%v anchor=%v", hasNatAnchor, hasRuleAnchor),
		Suggest: "Add to /etc/pf.conf (translation section before rules):\n  nat-anchor \"yak/*\"\n  anchor \"yak/*\"\nThen run: service pf reload",
	})

	// 6. Optional rctl
	rctlOK := NewRctlManager(runner).IsEnabled(ctx)
	checks = append(checks, CheckItem{
		Name:    "Resource Limits (rctl, optional)",
		OK:      rctlOK,
		Detail:  fmt.Sprintf("kern.racct.enable=%v", rctlOK),
		Suggest: `Optional: add kern.racct.enable="1" to /boot/loader.conf and reboot`,
	})

	return checks
}
