package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blackstorm/bsdyak/internal/builder"
	"github.com/blackstorm/bsdyak/internal/config"
	"github.com/blackstorm/bsdyak/internal/daemon"
	"github.com/blackstorm/bsdyak/internal/protocol"
	"github.com/blackstorm/bsdyak/internal/sys"
	"github.com/blackstorm/bsdyak/internal/transport"
)

type harness struct {
	srv    *daemon.Server
	fake   *sys.FakeDriver
	client *transport.Client
	cancel context.CancelFunc
}

func startHarness(t *testing.T, allowProject string) *harness {
	t.Helper()
	tmp := t.TempDir()
	fake := sys.NewFakeDriver(tmp)
	cfg := sys.ServerConfig{
		ZFSRoot:    "zroot/yak",
		Egress:     "vtnet0",
		StateDir:   filepath.Join(tmp, "db"),
		RunDir:     filepath.Join(tmp, "run"),
		SocketPath: filepath.Join(tmp, "yakd.sock"),
	}

	srv, err := daemon.NewServer(cfg, fake, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	srv.Reconciler().HealthProbe = func(_ context.Context, _ string, _ string, _ protocol.ServiceSpec) error {
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.ListenAndServe(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(cfg.SocketPath); err == nil {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	cConn, sConn := net.Pipe()
	go func() { _ = transport.RunBridge(ctx, cfg.SocketPath, allowProject, sConn, sConn) }()

	client, err := transport.NewClientFromConn(cConn, cConn.Close)
	if err != nil {
		t.Fatalf("NewClientFromConn: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	return &harness{
		srv:    srv,
		fake:   fake,
		client: client,
		cancel: cancel,
	}
}

func runDeployEvents(t *testing.T, ctx context.Context, c *transport.Client, req protocol.DeployRequest) []protocol.DeployEvent {
	t.Helper()
	stream, err := c.DoStream(ctx, http.MethodPost, protocol.RouteDeploy(req.Spec.Project), req)
	if err != nil {
		t.Fatalf("DoStream deploy: %v", err)
	}
	defer stream.Close()

	var events []protocol.DeployEvent
	sc := bufio.NewScanner(stream)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var ev protocol.DeployEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("decode deploy event: %v", err)
		}
		events = append(events, ev)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan deploy stream: %v", err)
	}
	if len(events) == 0 || events[len(events)-1].Type != "done" {
		t.Fatalf("deploy did not complete cleanly: %+v", events)
	}
	return events
}

func TestMultiServiceStackLifecycle(t *testing.T) {
	ctx := context.Background()
	h := startHarness(t, "shop")

	parsed, err := config.ParseFile(filepath.Join("..", "..", "examples", "go-postgres-redis", "Yakfile"), "")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	spec := parsed.ToDeploySpec()
	api := spec.Services["api"]
	api.Expose = &protocol.ExposeSpec{Listen: "127.0.0.1:0", To: 8080}
	api.Healthcheck.Interval = "20ms"
	api.Healthcheck.Timeout = "200ms"
	spec.Services["api"] = api

	// 1. Bridge enforces --allow-project=shop.
	var otherPs protocol.PsResponse
	if err := h.client.DoJSON(ctx, http.MethodGet, protocol.RoutePs("other"), nil, &otherPs); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("expected 403 for cross-project request, got %v", err)
	}

	// 2. Inject secrets and deploy entire stack in topological order.
	for k, v := range map[string]string{"postgres_password": "pg1", "jwt_secret": "jwt1"} {
		if err := h.client.DoJSON(ctx, http.MethodPost, protocol.RouteSecrets("shop"), protocol.SecretSetRequest{Key: k, Value: v}, nil); err != nil {
			t.Fatalf("set secret %s: %v", k, err)
		}
	}
	runDeployEvents(t, ctx, h.client, protocol.DeployRequest{Spec: spec})

	var ps1 protocol.PsResponse
	if err := h.client.DoJSON(ctx, http.MethodGet, protocol.RoutePs("shop"), nil, &ps1); err != nil {
		t.Fatalf("ps: %v", err)
	}
	if len(ps1.Services) != 4 {
		t.Fatalf("expected 4 services, got %d", len(ps1.Services))
	}

	releasesBefore := make(map[string]string)
	for _, s := range ps1.Services {
		releasesBefore[s.Service] = s.Release
		if !strings.HasPrefix(s.IP, "10.88.") {
			t.Fatalf("unexpected jail IP for %s: %s", s.Service, s.IP)
		}
	}

	// 3. Roll out new api cmd; verify db & cache stay untouched (same release, no restart).
	api = spec.Services["api"]
	api.Cmd = "/app/api --v2"
	spec.Services["api"] = api
	runDeployEvents(t, ctx, h.client, protocol.DeployRequest{Spec: spec})

	var ps2 protocol.PsResponse
	if err := h.client.DoJSON(ctx, http.MethodGet, protocol.RoutePs("shop"), nil, &ps2); err != nil {
		t.Fatal(err)
	}
	for _, s := range ps2.Services {
		switch s.Service {
		case "db", "cache":
			if s.Release != releasesBefore[s.Service] {
				t.Fatalf("%s unexpectedly restarted: %s -> %s", s.Service, releasesBefore[s.Service], s.Release)
			}
		case "api":
			if s.Release == releasesBefore["api"] {
				t.Fatalf("api release should advance from %s", releasesBefore["api"])
			}
		}
	}

	// 4. Rollback api to initial release and clean down.
	runDeployEvents(t, ctx, h.client, protocol.DeployRequest{
		Spec:     spec,
		Services: []string{"api"},
		Release:  releasesBefore["api"],
	})
	var downResp protocol.DownResponse
	if err := h.client.DoJSON(ctx, http.MethodPost, protocol.RouteDown("shop"), protocol.DownRequest{}, &downResp); err != nil {
		t.Fatal(err)
	}
	if len(downResp.Stopped) != 4 {
		t.Fatalf("expected 4 stopped services, got %v", downResp.Stopped)
	}
}

func TestLocalBuildStageAndSync(t *testing.T) {
	ctx := context.Background()
	h := startHarness(t, "")

	ws := t.TempDir()
	_ = os.WriteFile(filepath.Join(ws, ".gitignore"), []byte("*.tmp\nignored_dir/\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(ws, "ignored_dir"), 0o755)
	_ = os.WriteFile(filepath.Join(ws, "ignored_dir/skip.txt"), []byte("nope"), 0o644)
	_ = os.WriteFile(filepath.Join(ws, "junk.tmp"), []byte("nope"), 0o644)
	_ = os.WriteFile(filepath.Join(ws, "Yakfile"), []byte(fmt.Sprintf(`
project "localapp" {
  freebsd = "14.5-RELEASE"
}

image "app" {
  stage "build" {
    on    = "local"
    steps = ["mkdir -p dist && echo local-bin > dist/srv"]
  }
  runtime {
    copy {
      from = stage.build
      src  = "./dist/srv"
      dst  = "/app/srv"
    }
  }
}

service "web" {
  image = image.app
  cmd   = "/app/srv"
}
`)), 0o644)

	cfg, err := config.ParseFile(filepath.Join(ws, "Yakfile"), "")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	spec := cfg.ToDeploySpec()

	var buildLog bytes.Buffer
	artifacts, err := builder.RunLocalStages(ctx, ws, spec, &buildLog)
	if err != nil {
		t.Fatalf("RunLocalStages: %v", err)
	}
	if len(artifacts) != 1 || artifacts[0] != "dist/srv" {
		t.Fatalf("unexpected local artifacts: %v", artifacts)
	}

	manifest, err := transport.BuildSyncManifest(ws)
	if err != nil {
		t.Fatalf("BuildSyncManifest: %v", err)
	}
	if _, ok := manifest["junk.tmp"]; ok {
		t.Fatal("expected junk.tmp to be ignored")
	}
	if _, ok := manifest["ignored_dir/skip.txt"]; ok {
		t.Fatal("expected ignored_dir/skip.txt to be ignored")
	}

	var syncResp protocol.SyncManifestResponse
	if err := h.client.DoJSON(ctx, http.MethodPost, protocol.RouteSyncManifest("localapp"), protocol.SyncManifestRequest{Files: manifest}, &syncResp); err != nil {
		t.Fatalf("SyncManifest: %v", err)
	}
	var srcBuf bytes.Buffer
	if err := transport.PackZstdTar(ws, syncResp.Missing, &srcBuf); err != nil {
		t.Fatal(err)
	}
	if err := h.client.PostStream(ctx, protocol.RouteSyncUpload("localapp"), "application/zstd", &srcBuf, nil, nil); err != nil {
		t.Fatal(err)
	}

	var artBuf bytes.Buffer
	if err := builder.PackLocalArtifacts(ws, artifacts, &artBuf); err != nil {
		t.Fatal(err)
	}
	if err := h.client.PostStream(ctx, protocol.RouteSyncArtifacts("localapp"), "application/zstd", &artBuf, nil, nil); err != nil {
		t.Fatal(err)
	}

	artFile := filepath.Join(h.srv.Reconciler().ArtifactsDir("localapp"), "dist/srv")
	data, err := os.ReadFile(artFile)
	if err != nil || strings.TrimSpace(string(data)) != "local-bin" {
		t.Fatalf("expected unpacked local artifact on server, got %q err=%v", string(data), err)
	}

	runDeployEvents(t, ctx, h.client, protocol.DeployRequest{Spec: spec})
}
