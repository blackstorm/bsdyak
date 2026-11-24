package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/blackstorm/bsdyak/internal/protocol"
	"github.com/blackstorm/bsdyak/internal/proxy"
	"github.com/blackstorm/bsdyak/internal/state"
	"github.com/blackstorm/bsdyak/internal/sys"
	"github.com/blackstorm/bsdyak/internal/transport"
	"github.com/blackstorm/bsdyak/internal/version"
)

type Server struct {
	cfg        sys.ServerConfig
	store      *state.Store
	runner     sys.Runner
	expose     *proxy.ExposeManager
	logs       *LogManager
	gc         *GCWorker
	reconciler *Reconciler
	httpSrv    *http.Server
	listener   net.Listener
}

func NewServer(cfg sys.ServerConfig, runner sys.Runner, dnsOverride string) (*Server, error) {
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.RunDir, 0o755); err != nil {
		return nil, err
	}
	store, err := state.Open(filepath.Join(cfg.StateDir, "state.db"))
	if err != nil {
		return nil, err
	}

	zfs := sys.NewZFS(runner, cfg.ZFSRoot)
	expose := proxy.NewExposeManager()
	logs := NewLogManager(filepath.Join(cfg.StateDir, "logs"))
	gc := NewGCWorker(store, runner, zfs)
	rec := NewReconciler(cfg, store, runner, expose, logs, gc)
	rec.DNSBindAddrOverride = dnsOverride

	s := &Server{
		cfg:        cfg,
		store:      store,
		runner:     runner,
		expose:     expose,
		logs:       logs,
		gc:         gc,
		reconciler: rec,
	}
	s.httpSrv = &http.Server{Handler: s.routes()}
	return s, nil
}

func (s *Server) Reconciler() *Reconciler { return s.reconciler }
func (s *Server) Store() *state.Store     { return s.store }
func (s *Server) Logs() *LogManager       { return s.logs }
func (s *Server) GC() *GCWorker           { return s.gc }

// ListenAndServe starts yakd on the configured Unix socket (0660, chgrp yak when present)
// and recovers previously active project networks/DNS/forwarders.
func (s *Server) ListenAndServe(ctx context.Context) error {
	sockDir := filepath.Dir(s.cfg.SocketPath)
	if err := os.MkdirAll(sockDir, 0o755); err != nil {
		return err
	}
	_ = os.Remove(s.cfg.SocketPath)

	ln, err := net.Listen("unix", s.cfg.SocketPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.SocketPath, err)
	}
	s.listener = ln
	_ = os.Chmod(s.cfg.SocketPath, 0o660)
	if grp, gErr := user.LookupGroup("yak"); gErr == nil {
		if gid, pErr := strconv.Atoi(grp.Gid); pErr == nil {
			_ = os.Chown(s.cfg.SocketPath, 0, gid)
		}
	}

	_ = s.reconciler.RecoverOnBoot(ctx)

	gcCtx, cancelGC := context.WithCancel(ctx)
	go s.gc.RunLoop(gcCtx, 5*time.Second)

	go func() {
		<-ctx.Done()
		cancelGC()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.httpSrv.Shutdown(shutdownCtx)
	}()

	err = s.httpSrv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Close() error {
	s.expose.StopAll(2 * time.Second)
	s.reconciler.Close()
	if s.listener != nil {
		_ = s.listener.Close()
	}
	return s.store.Close()
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(protocol.RouteHealth, s.handleHealth)
	mux.HandleFunc("/v1/projects/", s.handleProjectDispatch)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, protocol.HealthResponse{
		Status:  "ok",
		Version: version.Version,
	})
}

func (s *Server) handleProjectDispatch(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/projects/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) < 2 || parts[0] == "" {
		http.Error(w, "invalid project path", http.StatusNotFound)
		return
	}
	project, err := url.PathUnescape(parts[0])
	if err != nil {
		http.Error(w, "invalid project name", http.StatusBadRequest)
		return
	}
	sub := "/" + parts[1]

	switch sub {
	case "/sync/manifest":
		s.handleSyncManifest(w, r, project)
	case "/sync/upload":
		s.handleSyncUpload(w, r, project)
	case "/sync/artifacts":
		s.handleSyncArtifacts(w, r, project)
	case "/deploy":
		s.handleDeploy(w, r, project)
	case "/down":
		s.handleDown(w, r, project)
	case "/ps":
		s.handlePs(w, r, project)
	case "/logs":
		s.handleLogs(w, r, project)
	case "/exec":
		s.handleExec(w, r, project)
	case "/secrets":
		s.handleSecrets(w, r, project)
	default:
		http.Error(w, "unknown endpoint", http.StatusNotFound)
	}
}

func (s *Server) handleSyncManifest(w http.ResponseWriter, r *http.Request, project string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req protocol.SyncManifestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	srcDir := s.reconciler.SourceDir(project)
	_ = os.MkdirAll(srcDir, 0o755)
	existing, err := transport.BuildSyncManifest(srcDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var missing, removed []string
	for path, meta := range req.Files {
		ex, ok := existing[path]
		if !ok || ex.SHA256 != meta.SHA256 || ex.Mode != meta.Mode {
			missing = append(missing, path)
		}
	}
	for path := range existing {
		if _, ok := req.Files[path]; !ok {
			removed = append(removed, path)
			_ = os.Remove(filepath.Join(srcDir, filepath.FromSlash(path)))
		}
	}
	sort.Strings(missing)
	sort.Strings(removed)
	writeJSON(w, http.StatusOK, protocol.SyncManifestResponse{
		Missing: missing,
		Removed: removed,
	})
}

func (s *Server) handleSyncUpload(w http.ResponseWriter, r *http.Request, project string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	srcDir := s.reconciler.SourceDir(project)
	extracted, err := transport.UnpackZstdTar(r.Body, srcDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"uploaded": len(extracted)})
}

func (s *Server) handleSyncArtifacts(w http.ResponseWriter, r *http.Request, project string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	artDir := s.reconciler.ArtifactsDir(project)
	_ = os.RemoveAll(artDir)
	extracted, err := transport.UnpackZstdTar(r.Body, artDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": len(extracted)})
}

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request, project string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req protocol.DeployRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Spec.Project == "" {
		req.Spec.Project = project
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	emit := func(ev protocol.DeployEvent) {
		b, _ := json.Marshal(ev)
		_, _ = w.Write(append(b, '\n'))
		if flusher != nil {
			flusher.Flush()
		}
	}

	if err := s.reconciler.Deploy(r.Context(), req, emit); err != nil {
		emit(protocol.DeployEvent{
			Type:      "error",
			Timestamp: time.Now().UTC(),
			Error:     err.Error(),
		})
	}
}

func (s *Server) handleDown(w http.ResponseWriter, r *http.Request, project string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req protocol.DownRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	stopped, err := s.reconciler.Down(r.Context(), project, req.Services)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, protocol.DownResponse{Stopped: stopped})
}

func (s *Server) handlePs(w http.ResponseWriter, r *http.Request, project string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ps, err := s.store.GetProject(project)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp := protocol.PsResponse{Project: project}
	if ps != nil {
		resp.Subnet = ps.SubnetCIDR
		jm := sys.NewJailManager(s.runner)
		for _, name := range sortedActiveNames(ps.ActiveServices) {
			svc := ps.ActiveServices[name]
			stateStr := "running"
			running, dying := jm.IsRunning(r.Context(), svc.JailName)
			if !running {
				if dying {
					stateStr = "dying"
				} else {
					stateStr = "stopped"
				}
			}
			resp.Services = append(resp.Services, protocol.ServiceStatus{
				Project:      project,
				Service:      svc.Service,
				JailName:     svc.JailName,
				Release:      svc.Release,
				State:        stateStr,
				Strategy:     svc.Strategy,
				IP:           svc.IP,
				Port:         svc.Port,
				ExposeListen: svc.ExposeListen,
				ExposeUnix:   svc.ExposeUnix,
				UpdatedAt:    svc.UpdatedAt,
			})
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request, project string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	svc := r.URL.Query().Get("service")
	if svc == "" {
		http.Error(w, "missing service query parameter", http.StatusBadRequest)
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 {
		n = 100
	}
	follow := r.URL.Query().Get("follow") == "true"

	lines, err := s.logs.Tail(project, svc, n)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	for _, l := range lines {
		_, _ = io.WriteString(w, l+"\n")
	}
	if flusher != nil {
		flusher.Flush()
	}

	if !follow {
		return
	}

	ch, cancel := s.logs.Subscribe(project, svc)
	defer cancel()
	for {
		select {
		case <-r.Context().Done():
			return
		case line, ok := <-ch:
			if !ok {
				return
			}
			_, _ = io.WriteString(w, line)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request, project string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req protocol.ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Service == "" || len(req.Command) == 0 {
		http.Error(w, "service and command are required", http.StatusBadRequest)
		return
	}

	ps, err := s.store.GetProject(project)
	if err != nil || ps == nil {
		http.Error(w, fmt.Sprintf("project %q not found", project), http.StatusNotFound)
		return
	}
	active, ok := ps.ActiveServices[req.Service]
	if !ok {
		http.Error(w, fmt.Sprintf("service %q is not running in project %q", req.Service, project), http.StatusNotFound)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "streaming exec requires connection hijacking", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: yak-exec\r\n\r\n")
	_ = rw.Flush()

	var stdinReader io.Reader
	if req.Stdin || req.TTY {
		stdinReader = rw
	}
	args := append([]string{active.JailName}, req.Command...)
	_ = s.runner.RunStream(r.Context(), stdinReader, conn, conn, "jexec", args...)
}

func (s *Server) handleSecrets(w http.ResponseWriter, r *http.Request, project string) {
	switch r.Method {
	case http.MethodGet:
		keys, err := s.store.ListSecrets(project)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, protocol.SecretListResponse{Keys: keys})
	case http.MethodPost:
		var req protocol.SecretSetRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.store.SetSecret(project, req.Key, req.Value); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		key := r.URL.Query().Get("key")
		if key == "" {
			http.Error(w, "missing key", http.StatusBadRequest)
			return
		}
		if err := s.store.UnsetSecret(project, key); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
