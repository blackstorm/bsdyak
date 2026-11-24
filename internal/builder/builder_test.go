package builder

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blackstorm/bsdyak/internal/protocol"
	"github.com/blackstorm/bsdyak/internal/sys"
)

func TestLayerHashDeterminism(t *testing.T) {
	h1 := LayerHash("14.2-RELEASE", []string{"vips", "ffmpeg", "ca_root_nss"}, []string{"pw useradd -n app"})
	h2 := LayerHash("14.2-RELEASE", []string{"ca_root_nss", "ffmpeg", "vips"}, []string{"pw useradd -n app"})
	if h1 != h2 {
		t.Fatalf("package order should not change layer hash: %s vs %s", h1, h2)
	}
	h3 := LayerHash("14.2-RELEASE", []string{"ca_root_nss", "ffmpeg"}, []string{"pw useradd -n app"})
	if h1 == h3 {
		t.Fatalf("different package lists should yield distinct hashes")
	}
}

func TestLayerCacheHitSkipsPkgInstall(t *testing.T) {
	fake := sys.NewFakeDriver(t.TempDir())
	zfs := sys.NewZFS(fake, "zroot/yak")
	sb := NewServerBuilder(fake, zfs)

	var log bytes.Buffer
	snap1, hit1, err := sb.EnsureLayer(context.Background(), "14.2-RELEASE", []string{"ca_root_nss", "ffmpeg"}, []string{"pw useradd app"}, &log)
	if err != nil {
		t.Fatalf("first EnsureLayer: %v", err)
	}
	if hit1 {
		t.Fatal("expected cache miss on first call")
	}

	cmdCountAfterFirst := len(fake.Commands)
	snap2, hit2, err := sb.EnsureLayer(context.Background(), "14.2-RELEASE", []string{"ffmpeg", "ca_root_nss"}, []string{"pw useradd app"}, &log)
	if err != nil {
		t.Fatalf("second EnsureLayer: %v", err)
	}
	if !hit2 || snap1 != snap2 {
		t.Fatalf("expected cache hit returning %s, got hit=%v snap=%s", snap1, hit2, snap2)
	}

	for _, cmd := range fake.Commands[cmdCountAfterFirst:] {
		if strings.HasPrefix(cmd, "pkg ") || strings.Contains(cmd, " pkg ") {
			t.Fatalf("pkg should not run on ZFS layer cache hit: %s", cmd)
		}
	}
}

func TestLocalStageExecution(t *testing.T) {
	ws := t.TempDir()
	spec := protocol.DeploySpec{
		Project: "demo",
		FreeBSD: "14.2-RELEASE",
		Images: map[string]protocol.ImageSpec{
			"app": {
				Name: "app",
				Stages: []protocol.StageSpec{
					{
						Name:  "build",
						On:    "local",
						Steps: []string{"mkdir -p dist && echo built > dist/app.bin"},
					},
				},
				Runtime: protocol.RuntimeSpec{
					Copy: []protocol.CopySpec{
						{FromStage: "build", Src: "./dist/app.bin", Dst: "/app/app"},
					},
				},
			},
		},
	}

	var log bytes.Buffer
	files, err := RunLocalStages(context.Background(), ws, spec, &log)
	if err != nil {
		t.Fatalf("RunLocalStages: %v", err)
	}
	if len(files) != 1 || files[0] != "dist/app.bin" {
		t.Fatalf("expected [dist/app.bin], got %v", files)
	}
	data, err := os.ReadFile(filepath.Join(ws, "dist/app.bin"))
	if err != nil || strings.TrimSpace(string(data)) != "built" {
		t.Fatalf("unexpected artifact content: %q %v", string(data), err)
	}
}
