package transport

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/blackstorm/bsdyak/internal/protocol"
	"github.com/hashicorp/yamux"
)

type stdioConn struct {
	r io.Reader
	w io.WriteCloser
}

func (s *stdioConn) Read(p []byte) (int, error)  { return s.r.Read(p) }
func (s *stdioConn) Write(p []byte) (int, error) { return s.w.Write(p) }
func (s *stdioConn) Close() error                { return s.w.Close() }

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) {
	return b.r.Read(p)
}

type noopWriteCloser struct {
	io.Writer
}

func (noopWriteCloser) Close() error { return nil }

// RunBridge runs the server-side `yak bridge` stdio <-> unix socket forwarder over yamux.
// When allowProject is non-empty, only /v1/health and /v1/projects/<allowProject>/... are permitted.
func RunBridge(ctx context.Context, socketPath, allowProject string, stdin io.Reader, stdout io.Writer) error {
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard

	rwc := &stdioConn{r: stdin, w: noopWriteCloser{stdout}}
	session, err := yamux.Server(rwc, cfg)
	if err != nil {
		return fmt.Errorf("start yamux server: %w", err)
	}
	defer session.Close()

	go func() {
		<-ctx.Done()
		_ = session.Close()
	}()

	for {
		stream, err := session.AcceptStream()
		if err != nil {
			if ctx.Err() != nil || err == io.EOF {
				return nil
			}
			return nil
		}
		go handleBridgeStream(ctx, stream, socketPath, allowProject)
	}
}

func handleBridgeStream(ctx context.Context, stream net.Conn, socketPath, allowProject string) {
	defer stream.Close()

	br := bufio.NewReader(stream)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	defer req.Body.Close()

	if allowProject != "" && req.URL.Path != protocol.RouteHealth {
		proj, pErr := protocol.ParseProjectFromPath(req.URL.Path)
		if pErr != nil || proj != allowProject {
			writeHTTPError(stream, http.StatusForbidden,
				fmt.Sprintf("bridge restricted to project %q (requested %q)", allowProject, req.URL.Path))
			return
		}
	}

	upstream, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		writeHTTPError(stream, http.StatusBadGateway,
			fmt.Sprintf("connect to yakd socket %s: %v (is yakd running?)", socketPath, err))
		return
	}
	defer upstream.Close()

	isUpgrade := strings.EqualFold(req.Header.Get("Connection"), "Upgrade") || req.Header.Get("Upgrade") != ""
	if !isUpgrade {
		// One yamux stream per HTTP request simplifies scoping & streaming without keep-alive ambiguity.
		req.Close = true
		req.Header.Set("Connection", "close")
	}

	if isUpgrade {
		// Write upgrade request headers then pipe bidirectional bytes (for yak exec).
		if err := req.Write(upstream); err != nil {
			return
		}
		wrapped := &bufferedConn{Conn: stream, r: br}
		go func() {
			_, _ = io.Copy(upstream, wrapped)
			closeWrite(upstream)
		}()
		// Wait until remote command finishes writing to upstream; defer closes stream & upstream.
		_, _ = io.Copy(stream, upstream)
		return
	}

	// Standard HTTP request / SSE stream.
	// Note: do NOT call CloseWrite() on upstream after req.Write(upstream),
	// because Go's net/http server treats an early EOF as client cancellation
	// and aborts r.Context() before streaming handlers can finish.
	if err := req.Write(upstream); err != nil {
		return
	}

	doneCopy := make(chan struct{})
	go func() {
		defer close(doneCopy)
		_, _ = io.Copy(stream, upstream)
	}()

	// Propagate client disconnect (e.g. Ctrl-C during `yak logs -f`) to upstream.
	go func() {
		_, _ = io.Copy(io.Discard, br)
		_ = upstream.Close()
	}()

	<-doneCopy
}

func writeHTTPError(w io.Writer, code int, msg string) {
	body := msg + "\n"
	_, _ = fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		code, http.StatusText(code), len(body), body)
}

func closeWrite(c net.Conn) {
	type closeWriter interface {
		CloseWrite() error
	}
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}
