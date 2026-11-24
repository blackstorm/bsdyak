package sys

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInterfaceNameLengthInvariant(t *testing.T) {
	fake := NewFakeDriver(t.TempDir())
	nm := NewNetManager(fake)

	if err := nm.EnsureBridge(context.Background(), "ykb_1", "10.88.1.1/24"); err != nil {
		t.Fatalf("EnsureBridge: %v", err)
	}
	if err := ValidateIfName("ykb_1"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateIfName("too_long_interface_name"); err == nil {
		t.Fatal("expected error for interface name > 15 chars")
	}

	hostIf, jailIf, err := nm.CreateEpair(context.Background(), "ykb_1", "shop:api:r1")
	if err != nil {
		t.Fatalf("CreateEpair: %v", err)
	}
	if len(hostIf) > MaxIfNameLen || len(jailIf) > MaxIfNameLen {
		t.Fatalf("epair exceeds IFNAMSIZ: %s (%d), %s (%d)", hostIf, len(hostIf), jailIf, len(jailIf))
	}
}

func TestPFAnchorNeverModifiesHostPFConf(t *testing.T) {
	fake := NewFakeDriver(t.TempDir())
	nm := NewNetManager(fake)

	if err := nm.ApplyPFAnchor(context.Background(), "shop", "10.88.1.0/24", "vtnet0", []string{"10.88.2.0/24"}); err != nil {
		t.Fatalf("ApplyPFAnchor: %v", err)
	}
	anchor := fake.PFAnchors["yak/shop"]
	if !strings.Contains(anchor, "nat on vtnet0 from 10.88.1.0/24") {
		t.Fatalf("missing NAT rule: %s", anchor)
	}
	if !strings.Contains(anchor, "block drop quick from 10.88.1.0/24 to 10.88.2.0/24") {
		t.Fatalf("missing isolation rule: %s", anchor)
	}
}

func TestZFSCapacityGuard(t *testing.T) {
	fake := NewFakeDriver(t.TempDir())
	zfs := NewZFS(fake, "zroot/yak")

	fake.PoolCapacity = 50
	if err := zfs.CheckCapacityGuard(context.Background()); err != nil {
		t.Fatalf("unexpected error at 50%%: %v", err)
	}

	fake.PoolCapacity = 86
	if err := zfs.CheckCapacityGuard(context.Background()); !errors.Is(err, ErrPoolCapacity) {
		t.Fatalf("expected ErrPoolCapacity at 86%%, got %v", err)
	}
}

func TestJailParamWhitelist(t *testing.T) {
	if err := ValidateJailParams(map[string]string{"sysvshm": "new", "sysvsem": "new"}); err != nil {
		t.Fatalf("valid sysv params rejected: %v", err)
	}
	if err := ValidateJailParams(map[string]string{"allow.sysvipc": "1"}); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("unsafe allow.sysvipc should be rejected, got %v", err)
	}
}

func TestInitHostIdempotent(t *testing.T) {
	tmp := t.TempDir()
	fake := NewFakeDriver(tmp)

	opts := InitOptions{
		ConfigPath: filepath.Join(tmp, "etc/yak.conf"),
		RCPath:     filepath.Join(tmp, "rc.d/yakd"),
		ServerCfg: ServerConfig{
			ZFSRoot:    "zroot/yak",
			Egress:     "vtnet0",
			StateDir:   filepath.Join(tmp, "db"),
			RunDir:     filepath.Join(tmp, "run"),
			SocketPath: filepath.Join(tmp, "run/yakd.sock"),
		},
	}

	if err := InitHost(context.Background(), fake, opts); err != nil {
		t.Fatalf("first InitHost: %v", err)
	}
	if err := InitHost(context.Background(), fake, opts); err != nil {
		t.Fatalf("second InitHost should be idempotent: %v", err)
	}
	if !fake.Groups["yak"] {
		t.Fatal("expected yak group to be created")
	}
	if _, ok := fake.Datasets["zroot/yak/projects"]; !ok {
		t.Fatal("expected zroot/yak/projects dataset")
	}
}

func TestOSRunnerRunStreamDoesNotHangOnOpenStdin(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()

	var stdout, stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- OSRunner{}.RunStream(context.Background(), pr, &stdout, &stderr, "sh", "-c", "echo fast")
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunStream failed: %v", err)
		}
		if strings.TrimSpace(stdout.String()) != "fast" {
			t.Fatalf("expected 'fast', got %q", stdout.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunStream deadlocked waiting on open stdin pipe")
	}
}
