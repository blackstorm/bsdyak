package sys

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// AllowedJailParams enumerates the only jail parameters accepted from Yakfile.
// Arbitrarily permissive options (such as legacy allow.sysvipc=1) are rejected.
var AllowedJailParams = map[string]map[string]bool{
	"sysvshm":           {"new": true, "disable": true, "inherit": true},
	"sysvsem":           {"new": true, "disable": true, "inherit": true},
	"sysvmsg":           {"new": true, "disable": true, "inherit": true},
	"allow.raw_sockets": {"0": true, "1": true, "true": true, "false": true},
	"allow.mlock":       {"0": true, "1": true, "true": true, "false": true},
}

func ValidateJailParams(params map[string]string) error {
	for k, v := range params {
		allowedVals, ok := AllowedJailParams[k]
		if !ok {
			return fmt.Errorf("%w: unsupported jail option %q", ErrInvalidParam, k)
		}
		if !allowedVals[v] {
			return fmt.Errorf("%w: invalid value %q for jail option %q", ErrInvalidParam, v, k)
		}
	}
	return nil
}

type JailMount struct {
	HostPath   string
	TargetPath string
	ReadOnly   bool
	Owner      string // optional "uid:gid" or "user:group"
}

type JailStartOpts struct {
	Name        string
	RootPath    string
	Hostname    string
	EpairJail   string
	IPCIDR      string
	GatewayIP   string
	DNSServer   string
	Params      map[string]string
	Mounts      []JailMount
	Env         map[string]string
	Secrets     map[string]string // injected strictly into volatile tmpfs, never ZFS
	User        string
	Workdir     string
	InitCmd     string
	Cmd         string
	HostLogPath string
}

type JailManager struct {
	runner Runner
}

func NewJailManager(runner Runner) *JailManager {
	return &JailManager{runner: runner}
}

func (jm *JailManager) IsRunning(ctx context.Context, name string) (bool, bool) {
	res, err := jm.runner.Run(ctx, "jls", "-j", name, "-h", "name", "dying")
	if err != nil {
		return false, false
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if len(lines) < 2 {
		return false, false
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) >= 2 && fields[1] == "1" {
		return false, true
	}
	return true, false
}

func (jm *JailManager) MountNullFS(ctx context.Context, hostSrc, jailDst string, readOnly bool) error {
	if err := os.MkdirAll(hostSrc, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(jailDst, 0o755); err != nil {
		return err
	}
	args := []string{"-t", "nullfs"}
	if readOnly {
		args = append(args, "-o", "ro")
	}
	args = append(args, hostSrc, jailDst)
	_, err := jm.runner.Run(ctx, "mount", args...)
	return err
}

func (jm *JailManager) MountTmpFS(ctx context.Context, targetDir string) error {
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		return err
	}
	_, err := jm.runner.Run(ctx, "mount", "-t", "tmpfs", "-o", "mode=0700,size=2m", "tmpfs", targetDir)
	return err
}

func (jm *JailManager) Unmount(ctx context.Context, targetDir string) error {
	_, err := jm.runner.Run(ctx, "umount", targetDir)
	if err != nil {
		lower := strings.ToLower(err.Error())
		if strings.Contains(lower, "busy") || strings.Contains(lower, "ebusy") {
			return fmt.Errorf("%w: umount %s: %v", ErrJailDying, targetDir, err)
		}
		if strings.Contains(lower, "not currently mounted") || strings.Contains(lower, "unknown") {
			return nil
		}
	}
	return err
}

func (jm *JailManager) Start(ctx context.Context, opts JailStartOpts) ([]string, error) {
	if err := ValidateJailParams(opts.Params); err != nil {
		return nil, err
	}

	var mounted []string
	rollbackMounts := func() {
		for i := len(mounted) - 1; i >= 0; i-- {
			_ = jm.Unmount(context.Background(), mounted[i])
		}
	}

	// 1. Mount volatile tmpfs for runtime secrets & bootstrap script (never enters ZFS snapshots).
	runDir := filepath.Join(opts.RootPath, ".yak-run")
	if err := jm.MountTmpFS(ctx, runDir); err != nil {
		return nil, fmt.Errorf("mount tmpfs secrets dir: %w", err)
	}
	mounted = append(mounted, runDir)

	if err := jm.writeRuntimeFiles(opts, runDir); err != nil {
		rollbackMounts()
		return nil, err
	}

	// 2. Write resolv.conf pointing to project's embedded DNS server inside tmpfs or /etc.
	if opts.DNSServer != "" {
		etcDir := filepath.Join(opts.RootPath, "etc")
		_ = os.MkdirAll(etcDir, 0o755)
		_ = os.WriteFile(filepath.Join(etcDir, "resolv.conf"), []byte("nameserver "+opts.DNSServer+"\n"), 0o644)
	}

	// 3. Mount storage volumes via nullfs and enforce declared ownership if needed.
	for _, m := range opts.Mounts {
		target := filepath.Join(opts.RootPath, strings.TrimPrefix(m.TargetPath, "/"))
		if err := jm.MountNullFS(ctx, m.HostPath, target, m.ReadOnly); err != nil {
			rollbackMounts()
			return nil, fmt.Errorf("mount %s -> %s: %w", m.HostPath, target, err)
		}
		mounted = append(mounted, target)
		if m.Owner != "" {
			if _, err := jm.runner.Run(ctx, "chroot", opts.RootPath, "chown", "-R", m.Owner, m.TargetPath); err != nil {
				// Fallback if chroot chown isn't applicable on host-side UID/GID; try host chown for numeric uid:gid
				if strings.Contains(m.Owner, ":") {
					if _, err2 := jm.runner.Run(ctx, "chown", "-R", m.Owner, m.HostPath); err2 != nil {
						rollbackMounts()
						return nil, fmt.Errorf("chown %s on %s: %w", m.Owner, m.HostPath, err)
					}
				} else {
					rollbackMounts()
					return nil, fmt.Errorf("chown %s on %s: %w", m.Owner, m.TargetPath, err)
				}
			}
		}
	}

	// 3b. Mount devfs so jail processes have /dev/null, /dev/urandom, and /dev/zero.
	devDir := filepath.Join(opts.RootPath, "dev")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		rollbackMounts()
		return nil, fmt.Errorf("mkdir dev dir: %w", err)
	}
	if _, err := jm.runner.Run(ctx, "mount", "-t", "devfs", "devfs", devDir); err != nil {
		rollbackMounts()
		return nil, fmt.Errorf("mount devfs %s: %w", devDir, err)
	}
	mounted = append(mounted, devDir)

	// 4. Create persistent VNET jail.
	jailArgs := []string{
		"-c",
		"name=" + opts.Name,
		"path=" + opts.RootPath,
		"host.hostname=" + opts.Hostname,
		"vnet",
		"vnet.interface=" + opts.EpairJail,
		"persist",
	}
	for _, k := range sortedKeys(opts.Params) {
		jailArgs = append(jailArgs, fmt.Sprintf("%s=%s", k, opts.Params[k]))
	}
	if _, err := jm.runner.Run(ctx, "jail", jailArgs...); err != nil {
		rollbackMounts()
		return nil, fmt.Errorf("create jail %s: %w", opts.Name, err)
	}

	// 5. Configure VNET jail networking.
	if _, err := jm.runner.Run(ctx, "jexec", opts.Name, "ifconfig", "lo0", "inet", "127.0.0.1/8", "up"); err != nil {
		_ = jm.Stop(context.Background(), opts.Name, mounted)
		return nil, fmt.Errorf("configure lo0 in %s: %w", opts.Name, err)
	}
	if _, err := jm.runner.Run(ctx, "jexec", opts.Name, "ifconfig", opts.EpairJail, "inet", opts.IPCIDR, "up"); err != nil {
		_ = jm.Stop(context.Background(), opts.Name, mounted)
		return nil, fmt.Errorf("configure %s in %s: %w", opts.EpairJail, opts.Name, err)
	}
	if opts.GatewayIP != "" {
		if _, err := jm.runner.Run(ctx, "jexec", opts.Name, "route", "add", "default", opts.GatewayIP); err != nil {
			_ = jm.Stop(context.Background(), opts.Name, mounted)
			return nil, fmt.Errorf("set default route in %s: %w", opts.Name, err)
		}
	}

	// 6. Optional init_cmd (runs synchronously once before starting main daemon).
	if strings.TrimSpace(opts.InitCmd) != "" {
		if _, err := jm.Exec(ctx, opts.Name, []string{"/bin/sh", "-ec", ". /.yak-run/env; " + opts.InitCmd}); err != nil {
			_ = jm.Stop(context.Background(), opts.Name, mounted)
			return nil, fmt.Errorf("init_cmd in %s: %w", opts.Name, err)
		}
	}

	// 7. Launch supervised workload under FreeBSD daemon(8) with auto-restart (-r).
	if strings.TrimSpace(opts.Cmd) != "" {
		if _, err := jm.runner.Run(ctx, "jexec", opts.Name,
			"/usr/sbin/daemon", "-f", "-r", "-P", "/.yak-run/svc.pid", "-o", "/.yak-run/svc.log",
			"/bin/sh", "/.yak-run/start.sh"); err != nil {
			_ = jm.Stop(context.Background(), opts.Name, mounted)
			return nil, fmt.Errorf("start daemon in %s: %w", opts.Name, err)
		}
	}

	return mounted, nil
}

func (jm *JailManager) writeRuntimeFiles(opts JailStartOpts, runDir string) error {
	merged := make(map[string]string, len(opts.Env)+len(opts.Secrets))
	for k, v := range opts.Env {
		merged[k] = v
	}
	for k, v := range opts.Secrets {
		merged[k] = v
	}

	var envLines []string
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		envLines = append(envLines, fmt.Sprintf("export %s=%s", k, shellQuote(merged[k])))
	}

	envPath := filepath.Join(runDir, "env")
	if err := writeFileAtomic(envPath, []byte(strings.Join(envLines, "\n")+"\n"), 0o400); err != nil {
		return fmt.Errorf("write tmpfs env: %w", err)
	}

	workdir := opts.Workdir
	if workdir == "" {
		workdir = "/"
	}
	cmdLine := opts.Cmd
	var script string
	if opts.User != "" && opts.User != "root" {
		script = fmt.Sprintf("#!/bin/sh\nset -e\n. /.yak-run/env\ncd %s\nexec su -m %s -c %s\n",
			shellQuote(workdir), shellQuote(opts.User), shellQuote(cmdLine))
	} else {
		script = fmt.Sprintf("#!/bin/sh\nset -e\n. /.yak-run/env\ncd %s\nexec %s\n",
			shellQuote(workdir), cmdLine)
	}
	startPath := filepath.Join(runDir, "start.sh")
	if err := writeFileAtomic(startPath, []byte(script), 0o500); err != nil {
		return fmt.Errorf("write start.sh: %w", err)
	}
	return nil
}

func (jm *JailManager) Exec(ctx context.Context, jailName string, command []string) (CmdResult, error) {
	args := append([]string{jailName}, command...)
	return jm.runner.Run(ctx, "jexec", args...)
}

func (jm *JailManager) Stop(ctx context.Context, jailName string, mounts []string) error {
	// Attempt graceful signal to supervised process inside jail first.
	_, _ = jm.runner.Run(ctx, "jexec", jailName, "/bin/sh", "-c", "test -f /.yak-run/svc.pid && kill -TERM $(cat /.yak-run/svc.pid) || true")
	_, _ = jm.runner.Run(ctx, "jail", "-r", jailName)

	// Verify whether jail transitioned to dying state (TIME_WAIT sockets).
	running, dying := jm.IsRunning(ctx, jailName)
	if running || dying {
		return fmt.Errorf("%w: jail %s still terminating", ErrJailDying, jailName)
	}

	// Unmount in reverse order (nullfs volumes + tmpfs).
	for i := len(mounts) - 1; i >= 0; i-- {
		if err := jm.Unmount(ctx, mounts[i]); err != nil {
			return err
		}
	}
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
