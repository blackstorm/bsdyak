package sys

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// FakeDriver provides an in-memory stateful FreeBSD ZFS/Jail/VNET/PF simulator
// for comprehensive Linux/CI unit and integration tests.
type FakeDriver struct {
	mu            sync.Mutex
	RootDir       string
	Commands      []string
	Datasets      map[string]map[string]string
	Snapshots     map[string]bool
	PoolCapacity  int
	Bridges       map[string]string
	Epairs        map[string]string
	PFAnchors     map[string]string
	Jails         map[string]*FakeJail
	DyingOnce     map[string]bool // if set, first jail -r leaves jail dying to test async GC
	Groups        map[string]bool
	HealthFailFor map[string]bool
	EpairSeq      int
	BaseMocked    bool
}

type FakeJail struct {
	Name   string
	Root   string
	IP     string
	Dying  bool
	Logs   string
	Mounts []string
}

func NewFakeDriver(rootDir string) *FakeDriver {
	return &FakeDriver{
		RootDir:       rootDir,
		Datasets:      make(map[string]map[string]string),
		Snapshots:     make(map[string]bool),
		PoolCapacity:  25,
		Bridges:       make(map[string]string),
		Epairs:        make(map[string]string),
		PFAnchors:     make(map[string]string),
		Jails:         make(map[string]*FakeJail),
		DyingOnce:     make(map[string]bool),
		Groups:        make(map[string]bool),
		HealthFailFor: make(map[string]bool),
		BaseMocked:    true,
	}
}

func (f *FakeDriver) datasetDir(ds string) string {
	return filepath.Join(f.RootDir, "zfs", ds)
}

func (f *FakeDriver) Run(ctx context.Context, name string, args ...string) (CmdResult, error) {
	return f.RunWithInput(ctx, nil, name, args...)
}

func (f *FakeDriver) RunStream(ctx context.Context, _ io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	res, err := f.RunWithInput(ctx, nil, name, args...)
	if stdout != nil && res.Stdout != "" {
		_, _ = io.WriteString(stdout, res.Stdout)
	}
	if stderr != nil && res.Stderr != "" {
		_, _ = io.WriteString(stderr, res.Stderr)
	}
	return err
}

func (f *FakeDriver) RunWithInput(ctx context.Context, stdin io.Reader, name string, args ...string) (CmdResult, error) {
	var inBytes []byte
	if stdin != nil {
		inBytes, _ = io.ReadAll(stdin)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.Commands = append(f.Commands, name+" "+strings.Join(args, " "))

	// Unwrap `env KEY=VAL ... <cmd> <args...>` just like FreeBSD /usr/bin/env.
	if name == "env" {
		i := 0
		for i < len(args) && strings.Contains(args[i], "=") {
			i++
		}
		if i < len(args) {
			name = args[i]
			args = args[i+1:]
		}
	}

	switch name {
	case "freebsd-version":
		return CmdResult{Stdout: "14.2-RELEASE-p1\n"}, nil
	case "sysctl":
		if len(args) >= 2 && args[1] == "net.inet.ip.forwarding" {
			return CmdResult{Stdout: "1\n"}, nil
		}
		if len(args) >= 2 && args[1] == "kern.racct.enable" {
			return CmdResult{Stdout: "1\n"}, nil
		}
		return CmdResult{Stdout: "0\n"}, nil
	case "rctl":
		return CmdResult{}, nil
	case "pw":
		if len(args) >= 2 && args[0] == "groupshow" {
			if f.Groups[args[1]] {
				return CmdResult{Stdout: args[1] + ":*:1001:\n"}, nil
			}
			return CmdResult{}, fmt.Errorf("pw: unknown group `%s`", args[1])
		}
		if len(args) >= 2 && args[0] == "groupadd" {
			f.Groups[args[1]] = true
			return CmdResult{}, nil
		}
		return CmdResult{}, nil
	case "zpool":
		return CmdResult{Stdout: fmt.Sprintf("%d%%\n", f.PoolCapacity)}, nil
	case "zfs":
		return f.handleZFS(args)
	case "ifconfig":
		return f.handleIfconfig(args)
	case "pfctl":
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "-a" {
				f.PFAnchors[args[i+1]] = string(inBytes)
			}
		}
		return CmdResult{}, nil
	case "pkg":
		return CmdResult{Stdout: "installed\n"}, nil
	case "fetch", "tar":
		return CmdResult{}, nil
	case "mount":
		return CmdResult{}, nil
	case "umount":
		if len(args) > 0 {
			for _, j := range f.Jails {
				if j.Dying && strings.HasPrefix(args[len(args)-1], j.Root) {
					return CmdResult{}, fmt.Errorf("umount: %s: Device busy", args[len(args)-1])
				}
			}
		}
		return CmdResult{}, nil
	case "chroot", "chown":
		return CmdResult{}, nil
	case "jail":
		return f.handleJail(args)
	case "jls":
		return f.handleJls(args)
	case "jexec":
		return f.handleJexec(args)
	}
	return CmdResult{}, nil
}

func (f *FakeDriver) handleZFS(args []string) (CmdResult, error) {
	if len(args) == 0 {
		return CmdResult{}, fmt.Errorf("missing zfs subcommand")
	}
	switch args[0] {
	case "list":
		isSnap := false
		for _, a := range args {
			if a == "-t" {
				isSnap = true
			}
		}
		target := args[len(args)-1]
		if isSnap && strings.Contains(target, "@") {
			if f.Snapshots[target] {
				return CmdResult{Stdout: target + "\n"}, nil
			}
			return CmdResult{}, fmt.Errorf("dataset does not exist")
		}
		if isSnap {
			var lines []string
			prefix := target + "@"
			for s := range f.Snapshots {
				if strings.HasPrefix(s, prefix) {
					lines = append(lines, s)
				}
			}
			return CmdResult{Stdout: strings.Join(lines, "\n")}, nil
		}
		if _, ok := f.Datasets[target]; ok {
			return CmdResult{Stdout: target + "\n"}, nil
		}
		return CmdResult{}, fmt.Errorf("dataset %s does not exist", target)
	case "create":
		ds := args[len(args)-1]
		if _, ok := f.Datasets[ds]; !ok {
			f.Datasets[ds] = map[string]string{}
		}
		_ = os.MkdirAll(f.datasetDir(ds), 0o755)
		return CmdResult{}, nil
	case "set":
		kv := strings.SplitN(args[1], "=", 2)
		ds := args[2]
		if f.Datasets[ds] == nil {
			f.Datasets[ds] = map[string]string{}
		}
		f.Datasets[ds][kv[0]] = kv[1]
		return CmdResult{}, nil
	case "get":
		ds := args[len(args)-1]
		dir := f.datasetDir(ds)
		_ = os.MkdirAll(dir, 0o755)
		return CmdResult{Stdout: dir + "\n"}, nil
	case "snapshot":
		snap := args[len(args)-1]
		f.Snapshots[snap] = true
		return CmdResult{}, nil
	case "clone":
		snap := args[len(args)-2]
		ds := args[len(args)-1]
		if !f.Snapshots[snap] {
			return CmdResult{}, fmt.Errorf("snapshot %s does not exist", snap)
		}
		f.Datasets[ds] = map[string]string{"origin": snap}
		_ = os.MkdirAll(f.datasetDir(ds), 0o755)
		return CmdResult{}, nil
	case "destroy":
		target := args[len(args)-1]
		for _, j := range f.Jails {
			if j.Dying && strings.Contains(j.Root, target) {
				return CmdResult{}, fmt.Errorf("cannot destroy '%s': dataset is busy", target)
			}
		}
		delete(f.Datasets, target)
		delete(f.Snapshots, target)
		return CmdResult{}, nil
	}
	return CmdResult{}, nil
}

func (f *FakeDriver) handleIfconfig(args []string) (CmdResult, error) {
	if len(args) == 1 {
		if _, ok := f.Bridges[args[0]]; ok {
			return CmdResult{Stdout: args[0] + "\n"}, nil
		}
		return CmdResult{}, fmt.Errorf("interface %s does not exist", args[0])
	}
	if len(args) >= 2 && args[0] == "bridge" && args[1] == "create" {
		name := "bridge0"
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "name" {
				name = args[i+1]
			}
		}
		f.Bridges[name] = ""
		return CmdResult{Stdout: name + "\n"}, nil
	}
	if len(args) >= 2 && args[0] == "epair" && args[1] == "create" {
		id := f.EpairSeq
		f.EpairSeq++
		return CmdResult{Stdout: fmt.Sprintf("epair%da\n", id)}, nil
	}
	if len(args) >= 3 && args[1] == "name" {
		f.Epairs[args[2]] = args[0]
	}
	if len(args) >= 2 && args[1] == "destroy" {
		delete(f.Epairs, args[0])
	}
	return CmdResult{}, nil
}

func (f *FakeDriver) handleJail(args []string) (CmdResult, error) {
	if len(args) >= 2 && args[0] == "-r" {
		name := args[1]
		j, ok := f.Jails[name]
		if !ok {
			return CmdResult{}, nil
		}
		if f.DyingOnce[name] {
			delete(f.DyingOnce, name)
			j.Dying = true
			return CmdResult{}, nil
		}
		delete(f.Jails, name)
		return CmdResult{}, nil
	}
	var name, root string
	for _, a := range args {
		if strings.HasPrefix(a, "name=") {
			name = strings.TrimPrefix(a, "name=")
		} else if strings.HasPrefix(a, "path=") {
			root = strings.TrimPrefix(a, "path=")
		}
	}
	if name != "" {
		f.Jails[name] = &FakeJail{Name: name, Root: root, Logs: "started " + name + "\n"}
	}
	return CmdResult{}, nil
}

func (f *FakeDriver) handleJls(args []string) (CmdResult, error) {
	var name string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-j" {
			name = args[i+1]
		}
	}
	j, ok := f.Jails[name]
	if !ok {
		return CmdResult{}, fmt.Errorf("jail %q not found", name)
	}
	dying := "0"
	if j.Dying {
		dying = "1"
	}
	return CmdResult{Stdout: fmt.Sprintf("name dying\n%s %s\n", j.Name, dying)}, nil
}

func (f *FakeDriver) handleJexec(args []string) (CmdResult, error) {
	if len(args) == 0 {
		return CmdResult{}, nil
	}
	jailName := args[0]
	rest := strings.Join(args[1:], " ")
	if strings.Contains(rest, "fail-health") || f.HealthFailFor[jailName] {
		return CmdResult{}, fmt.Errorf("healthcheck failed")
	}
	return CmdResult{Stdout: "ok\n"}, nil
}
