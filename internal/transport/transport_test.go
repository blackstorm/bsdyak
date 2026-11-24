package transport

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blackstorm/bsdyak/internal/protocol"
)

func TestBridgeYamuxAndAllowProject(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "yakd.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer ln.Close()

	mux := http.NewServeMux()
	mux.HandleFunc(protocol.RouteHealth, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","version":"test"}`))
	})
	mux.HandleFunc(protocol.RoutePs("shop"), func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"project":"shop","services":[]}`))
	})
	mux.HandleFunc(protocol.RoutePs("other"), func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"project":"other","services":[]}`))
	})

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	cConn, sConn := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bridgeDone := make(chan error, 1)
	go func() {
		bridgeDone <- RunBridge(ctx, sock, "shop", sConn, sConn)
	}()

	client, err := NewClientFromConn(cConn, cConn.Close)
	if err != nil {
		t.Fatalf("NewClientFromConn: %v", err)
	}
	defer client.Close()

	var health protocol.HealthResponse
	if err := client.DoJSON(ctx, http.MethodGet, protocol.RouteHealth, nil, &health); err != nil {
		t.Fatalf("health failed: %v", err)
	}
	if health.Status != "ok" {
		t.Fatalf("unexpected health: %+v", health)
	}

	var ps protocol.PsResponse
	if err := client.DoJSON(ctx, http.MethodGet, protocol.RoutePs("shop"), nil, &ps); err != nil {
		t.Fatalf("allowed project failed: %v", err)
	}

	// Cross-project request must be blocked by bridge --allow-project=shop.
	err = client.DoJSON(ctx, http.MethodGet, protocol.RoutePs("other"), nil, &ps)
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("expected HTTP 403 for restricted project, got %v", err)
	}

	// Close client session first so net.Pipe does not block yamux GoAway frame.
	_ = client.Close()
	cancel()
	select {
	case <-bridgeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("bridge did not exit cleanly")
	}
}

func TestIncrementalSyncAndIgnore(t *testing.T) {
	src := t.TempDir()
	_ = os.WriteFile(filepath.Join(src, ".gitignore"), []byte("dist/\n*.log\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(src, ".git"), 0o755)
	_ = os.WriteFile(filepath.Join(src, ".git/HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(src, "dist"), 0o755)
	_ = os.WriteFile(filepath.Join(src, "dist/app"), []byte("ignored"), 0o755)
	_ = os.WriteFile(filepath.Join(src, "debug.log"), []byte("ignored"), 0o644)
	_ = os.MkdirAll(filepath.Join(src, "cmd/api"), 0o755)
	_ = os.WriteFile(filepath.Join(src, "cmd/api/main.go"), []byte("package main\n"), 0o644)
	_ = os.WriteFile(filepath.Join(src, "Yakfile"), []byte("project \"demo\" {}\n"), 0o644)

	manifest, err := BuildSyncManifest(src)
	if err != nil {
		t.Fatalf("BuildSyncManifest: %v", err)
	}
	if _, ok := manifest["cmd/api/main.go"]; !ok {
		t.Fatalf("expected cmd/api/main.go in manifest: %+v", manifest)
	}
	if _, ok := manifest[".git/HEAD"]; ok {
		t.Fatalf(".git must be ignored")
	}
	if _, ok := manifest["dist/app"]; ok {
		t.Fatalf("dist/ must be ignored by .gitignore")
	}
	if _, ok := manifest["debug.log"]; ok {
		t.Fatalf("*.log must be ignored by .gitignore")
	}

	var buf bytes.Buffer
	if err := PackZstdTar(src, []string{"cmd/api/main.go", "Yakfile"}, &buf); err != nil {
		t.Fatalf("PackZstdTar: %v", err)
	}

	dst := t.TempDir()
	extracted, err := UnpackZstdTar(&buf, dst)
	if err != nil {
		t.Fatalf("UnpackZstdTar: %v", err)
	}
	if len(extracted) != 2 {
		t.Fatalf("expected 2 extracted files, got %v", extracted)
	}
	got, err := os.ReadFile(filepath.Join(dst, "cmd/api/main.go"))
	if err != nil || string(got) != "package main\n" {
		t.Fatalf("unexpected extracted content: %q %v", string(got), err)
	}
}
