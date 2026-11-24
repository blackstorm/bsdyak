package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blackstorm/bsdyak/internal/config"
	"github.com/blackstorm/bsdyak/internal/protocol"
	"github.com/blackstorm/bsdyak/internal/sys"
	"github.com/blackstorm/bsdyak/internal/transport"
)

func startEchoServer(t *testing.T, tag string) (string, int, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				r := bufio.NewReader(conn)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					_, _ = fmt.Fprintf(conn, "%s:%s", tag, line)
				}
			}(c)
		}
	}()
	return "127.0.0.1", port, func() { _ = ln.Close() }
}

func TestFullDeployBlueGreenRollbackAndGCIntegration(t *testing.T) {
	tmp := t.TempDir()
	fake := sys.NewFakeDriver(tmp)
	cfg := sys.ServerConfig{
		ZFSRoot:    "zroot/yak",
		Egress:     "vtnet0",
		StateDir:   filepath.Join(tmp, "db"),
		RunDir:     filepath.Join(tmp, "run"),
		SocketPath: filepath.Join(tmp, "yakd.sock"),
	}

	srv, err := NewServer(cfg, fake, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	// Point HTTP/TCP health probe cleanly to success unless overridden.
	healthy := true
	srv.Reconciler().HealthProbe = func(_ context.Context, jailName, _ string, _ protocol.ServiceSpec) error {
		if !healthy {
			return fmt.Errorf("simulated healthcheck failure on %s", jailName)
		}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.ListenAndServe(ctx) }()

	// Wait for Unix socket readiness.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(cfg.SocketPath); err == nil {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	// Connect client over net.Pipe -> yamux -> RunBridge -> yakd.sock.
	cConn, sConn := net.Pipe()
	go func() { _ = transport.RunBridge(ctx, cfg.SocketPath, "", sConn, sConn) }()
	client, err := transport.NewClientFromConn(cConn, cConn.Close)
	if err != nil {
		t.Fatalf("NewClientFromConn: %v", err)
	}
	defer client.Close()

	// Parse real multi-service example Yakfile, adjusting api expose to 127.0.0.1:0 for test isolation.
	parsed, err := config.ParseFile(filepath.Join("..", "..", "examples", "go-postgres-redis", "Yakfile"), "")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	spec := parsed.ToDeploySpec()
	apiSvc := spec.Services["api"]
	apiSvc.Expose = &protocol.ExposeSpec{Listen: "127.0.0.1:0", To: 8080}
	apiSvc.Healthcheck.Interval = "50ms"
	apiSvc.Healthcheck.Timeout = "200ms"
	spec.Services["api"] = apiSvc

	// 1. Set required secrets via API.
	for k, v := range map[string]string{"postgres_password": "pg-secret", "jwt_secret": "jwt-secret"} {
		if err := client.DoJSON(ctx, http.MethodPost, protocol.RouteSecrets("shop"), protocol.SecretSetRequest{Key: k, Value: v}, nil); err != nil {
			t.Fatalf("set secret %s: %v", k, err)
		}
	}

	// 2. Dry-run deploy must return plan without mutating jails.
	events := runDeploy(t, ctx, client, protocol.DeployRequest{Spec: spec, DryRun: true})
	if len(events) == 0 || events[0].Type != "plan" {
		t.Fatalf("expected plan event first, got %+v", events)
	}
	if len(fake.Jails) != 0 {
		t.Fatalf("dry-run must not start jails, got %d", len(fake.Jails))
	}

	// 3. Real first deploy (v1): starts db, cache, api, worker.
	events = runDeploy(t, ctx, client, protocol.DeployRequest{Spec: spec})
	assertDeploySucceeded(t, events)

	var ps protocol.PsResponse
	if err := client.DoJSON(ctx, http.MethodGet, protocol.RoutePs("shop"), nil, &ps); err != nil {
		t.Fatalf("ps failed: %v", err)
	}
	if len(ps.Services) != 4 {
		t.Fatalf("expected 4 running services, got %+v", ps.Services)
	}

	// Check endpoints.json was written for host nginx/caddy integration.
	if _, err := os.Stat(filepath.Join(cfg.RunDir, "projects/shop/endpoints.json")); err != nil {
		t.Fatalf("expected endpoints.json to be written: %v", err)
	}

	// Verify secrets never entered ZFS dataset files outside the volatile tmpfs `.yak-run` mount.
	zfsTree := filepath.Join(tmp, "zfs")
	_ = filepath.Walk(zfsTree, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() && info.Name() == ".yak-run" {
			return filepath.SkipDir
		}
		if err == nil && !info.IsDir() {
			b, _ := os.ReadFile(p)
			if strings.Contains(string(b), "pg-secret") || strings.Contains(string(b), "jwt-secret") {
				t.Fatalf("secret leaked onto ZFS file %s", p)
			}
		}
		return nil
	})

	// 4. Mark v1 api jail to enter `dying` state on first stop, then deploy v2 (blue/green).
	var apiV1Jail, apiV1Rel string
	for _, s := range ps.Services {
		if s.Service == "api" {
			apiV1Jail = s.JailName
			apiV1Rel = s.Release
		}
	}
	fake.DyingOnce[apiV1Jail] = true

	// Change api cmd to trigger v2 build/deploy while leaving db & cache unchanged.
	apiSvc = spec.Services["api"]
	apiSvc.Cmd = "/app/api --v2"
	spec.Services["api"] = apiSvc
	events = runDeploy(t, ctx, client, protocol.DeployRequest{Spec: spec})
	assertDeploySucceeded(t, events)

	if err := client.DoJSON(ctx, http.MethodGet, protocol.RoutePs("shop"), nil, &ps); err != nil {
		t.Fatal(err)
	}
	var apiV2Rel string
	for _, s := range ps.Services {
		if s.Service == "api" {
			apiV2Rel = s.Release
		}
	}
	if apiV2Rel == apiV1Rel {
		t.Fatalf("expected api release to advance from %s", apiV1Rel)
	}

	// Verify async GC queue holds the dying jail and clears it cleanly once dying clears.
	tasks, err := srv.Store().ListGCTasks()
	if err != nil || len(tasks) == 0 {
		t.Fatalf("expected dying jail in GC queue, got %+v %v", tasks, err)
	}
	if j := fake.Jails[apiV1Jail]; j != nil {
		j.Dying = false
		delete(fake.Jails, apiV1Jail)
	}
	cleaned, err := srv.GC().SweepOnce(ctx)
	if err != nil || cleaned == 0 {
		t.Fatalf("expected GC sweep to clean dying jail, cleaned=%d err=%v", cleaned, err)
	}

	// 5. Failed healthcheck on v3 aborts cleanly and keeps v2 active!
	healthy = false
	apiSvc.Cmd = "/app/api --v3-broken"
	spec.Services["api"] = apiSvc
	events = runDeploy(t, ctx, client, protocol.DeployRequest{Spec: spec})
	lastEv := events[len(events)-1]
	if lastEv.Type != "error" {
		t.Fatalf("expected deploy error on failed healthcheck, got %+v", lastEv)
	}
	if err := client.DoJSON(ctx, http.MethodGet, protocol.RoutePs("shop"), nil, &ps); err != nil {
		t.Fatal(err)
	}
	for _, s := range ps.Services {
		if s.Service == "api" && s.Release != apiV2Rel {
			t.Fatalf("failed deploy must keep %s live, got %s", apiV2Rel, s.Release)
		}
	}

	// 6. Rollback api back to v1 release (`apiV1Rel`).
	healthy = true
	events = runDeploy(t, ctx, client, protocol.DeployRequest{
		Spec:     spec,
		Services: []string{"api"},
		Release:  apiV1Rel,
	})
	assertDeploySucceeded(t, events)
	if err := client.DoJSON(ctx, http.MethodGet, protocol.RoutePs("shop"), nil, &ps); err != nil {
		t.Fatal(err)
	}
	for _, s := range ps.Services {
		if s.Service == "api" && s.Release != apiV1Rel {
			t.Fatalf("expected rollback release %s, got %s", apiV1Rel, s.Release)
		}
	}

	// 7. Test logs & exec stream over bridged yamux.
	logStream, err := client.DoStream(ctx, http.MethodGet, protocol.RouteLogs("shop")+"?service=api&n=10", nil)
	if err != nil {
		t.Fatalf("logs stream: %v", err)
	}
	logBytes, _ := io.ReadAll(logStream)
	_ = logStream.Close()
	if !strings.Contains(string(logBytes), "started") {
		t.Fatalf("expected service log output, got %q", string(logBytes))
	}

	execConn, execReader, err := client.OpenExecStream(ctx, protocol.RouteExec("shop"), protocol.ExecRequest{
		Service: "api",
		Command: []string{"echo", "ok"},
	})
	if err != nil {
		t.Fatalf("OpenExecStream: %v", err)
	}
	execOut, _ := io.ReadAll(execReader)
	_ = execConn.Close()
	if strings.TrimSpace(string(execOut)) != "ok" {
		t.Fatalf("expected exec output 'ok', got %q", string(execOut))
	}

	// 8. Down tears down all active services.
	var downResp protocol.DownResponse
	if err := client.DoJSON(ctx, http.MethodPost, protocol.RouteDown("shop"), protocol.DownRequest{}, &downResp); err != nil {
		t.Fatalf("down failed: %v", err)
	}
	if len(downResp.Stopped) != 4 {
		t.Fatalf("expected 4 stopped services, got %v", downResp.Stopped)
	}
}

func runDeploy(t *testing.T, ctx context.Context, c *transport.Client, req protocol.DeployRequest) []protocol.DeployEvent {
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
			t.Fatalf("decode deploy event %q: %v", sc.Text(), err)
		}
		events = append(events, ev)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan deploy stream: %v", err)
	}
	return events
}

func assertDeploySucceeded(t *testing.T, events []protocol.DeployEvent) {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("no deploy events received")
	}
	last := events[len(events)-1]
	if last.Type != "done" {
		t.Fatalf("expected deploy to finish with done, got %+v", last)
	}
}
