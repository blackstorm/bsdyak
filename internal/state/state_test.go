package state

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestProjectLockContention(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	var active atomic.Int32
	var maxActive atomic.Int32
	var wg sync.WaitGroup

	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock, err := s.LockProject(ctx, "shop")
			if err != nil {
				t.Errorf("LockProject: %v", err)
				return
			}
			cur := active.Add(1)
			if cur > maxActive.Load() {
				maxActive.Store(cur)
			}
			time.Sleep(10 * time.Millisecond)
			active.Add(-1)
			unlock()
		}()
	}
	wg.Wait()

	if maxActive.Load() != 1 {
		t.Fatalf("expected exclusive lock (maxActive=1), got %d", maxActive.Load())
	}
}

func TestSubnetAndIPAllocation(t *testing.T) {
	s := newTestStore(t)

	p1, err := s.EnsureProject("shop", "14.2-RELEASE")
	if err != nil {
		t.Fatalf("EnsureProject shop: %v", err)
	}
	p2, err := s.EnsureProject("blog", "14.2-RELEASE")
	if err != nil {
		t.Fatalf("EnsureProject blog: %v", err)
	}

	if p1.SubnetCIDR == p2.SubnetCIDR {
		t.Fatalf("expected distinct subnets, both got %s", p1.SubnetCIDR)
	}
	if p1.GatewayIP != "10.88.1.1" || p2.GatewayIP != "10.88.2.1" {
		t.Fatalf("unexpected gateway IPs: %s, %s", p1.GatewayIP, p2.GatewayIP)
	}

	ip1, oct1, err := s.AllocateIP("shop")
	if err != nil {
		t.Fatalf("AllocateIP 1: %v", err)
	}
	ip2, oct2, err := s.AllocateIP("shop")
	if err != nil {
		t.Fatalf("AllocateIP 2: %v", err)
	}
	if ip1 == ip2 || oct1 == oct2 {
		t.Fatalf("expected distinct IPs, got %s and %s", ip1, ip2)
	}
}

func TestReleasesAndSecrets(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.EnsureProject("shop", "14.2-RELEASE"); err != nil {
		t.Fatal(err)
	}

	rev1, num1, err := s.NextRevision("shop")
	if err != nil || rev1 != "r1" || num1 != 1 {
		t.Fatalf("unexpected rev1: %s %d %v", rev1, num1, err)
	}
	if err := s.SaveRelease(ReleaseRecord{
		Project:  "shop",
		Service:  "api",
		Revision: rev1,
		RevNum:   num1,
		Dataset:  "zroot/yak/projects/shop/releases/api-r1",
		Snapshot: "zroot/yak/projects/shop/releases/api-r1@immutable",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetRelease("shop", "api", "r1")
	if err != nil || got == nil || got.Dataset == "" {
		t.Fatalf("GetRelease failed: %+v %v", got, err)
	}

	if err := s.SetSecret("shop", "db_pass", "secret123"); err != nil {
		t.Fatal(err)
	}
	keys, err := s.ListSecrets("shop")
	if err != nil || len(keys) != 1 || keys[0] != "db_pass" {
		t.Fatalf("unexpected secret keys: %v %v", keys, err)
	}
	vals, err := s.ResolveSecrets("shop", []string{"db_pass"})
	if err != nil || vals["db_pass"] != "secret123" {
		t.Fatalf("ResolveSecrets failed: %v %v", vals, err)
	}
	if _, err := s.ResolveSecrets("shop", []string{"db_pass", "missing_key"}); err == nil {
		t.Fatalf("expected error on missing secret")
	}
	if err := s.UnsetSecret("shop", "db_pass"); err != nil {
		t.Fatal(err)
	}
}
