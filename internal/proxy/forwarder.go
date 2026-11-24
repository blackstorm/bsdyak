package proxy

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Forwarder provides an ultra-thin protocol-agnostic L4 TCP/Unix forwarder
// with atomic backend switching and graceful draining of active connections.
type Forwarder struct {
	key      string
	listen   string
	unixPath string

	target   atomic.Pointer[string]
	listener net.Listener

	mu          sync.Mutex
	closed      bool
	activeConns map[net.Conn]struct{}
	wg          sync.WaitGroup
}

func StartForwarder(key, listen, unixPath, initialTarget string) (*Forwarder, error) {
	if listen == "" && unixPath == "" {
		return nil, fmt.Errorf("expose requires either listen or unix socket")
	}

	var ln net.Listener
	var err error
	if unixPath != "" {
		if err := os.MkdirAll(filepath.Dir(unixPath), 0o755); err != nil {
			return nil, fmt.Errorf("mkdir unix socket parent: %w", err)
		}
		_ = os.Remove(unixPath)
		ln, err = net.Listen("unix", unixPath)
		if err != nil {
			return nil, fmt.Errorf("listen unix %s: %w", unixPath, err)
		}
		_ = os.Chmod(unixPath, 0o660)
	} else {
		ln, err = net.Listen("tcp", listen)
		if err != nil {
			return nil, fmt.Errorf("listen tcp %s: %w", listen, err)
		}
	}

	f := &Forwarder{
		key:         key,
		listen:      listen,
		unixPath:    unixPath,
		listener:    ln,
		activeConns: make(map[net.Conn]struct{}),
	}
	t := initialTarget
	f.target.Store(&t)

	f.wg.Add(1)
	go f.serve()
	return f, nil
}

func (f *Forwarder) Addr() string {
	return f.listener.Addr().String()
}

func (f *Forwarder) Target() string {
	p := f.target.Load()
	if p == nil {
		return ""
	}
	return *p
}

// Switch atomically updates the backend target for new connections.
// Existing connections remain pinned to their dialed backend until closed.
func (f *Forwarder) Switch(newTarget string) {
	t := newTarget
	f.target.Store(&t)
}

func (f *Forwarder) serve() {
	defer f.wg.Done()
	for {
		clientConn, err := f.listener.Accept()
		if err != nil {
			f.mu.Lock()
			closed := f.closed
			f.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}

		f.wg.Add(1)
		go f.handleConn(clientConn)
	}
}

func (f *Forwarder) handleConn(clientConn net.Conn) {
	defer f.wg.Done()

	targetPtr := f.target.Load()
	if targetPtr == nil || *targetPtr == "" {
		_ = clientConn.Close()
		return
	}

	upstreamConn, err := net.DialTimeout("tcp", *targetPtr, 5*time.Second)
	if err != nil {
		_ = clientConn.Close()
		return
	}

	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		_ = clientConn.Close()
		_ = upstreamConn.Close()
		return
	}
	f.activeConns[clientConn] = struct{}{}
	f.activeConns[upstreamConn] = struct{}{}
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		delete(f.activeConns, clientConn)
		delete(f.activeConns, upstreamConn)
		f.mu.Unlock()
		_ = clientConn.Close()
		_ = upstreamConn.Close()
	}()

	var copyWG sync.WaitGroup
	copyWG.Add(2)

	pipe := func(dst, src net.Conn) {
		defer copyWG.Done()
		_, _ = io.Copy(dst, src)
		closeWrite(dst)
	}

	go pipe(upstreamConn, clientConn)
	go pipe(clientConn, upstreamConn)
	copyWG.Wait()
}

func closeWrite(c net.Conn) {
	type closeWriter interface {
		CloseWrite() error
	}
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}

// Stop closes the listener immediately and waits up to drainTimeout for active
// connections to finish naturally before force-closing remaining sockets.
func (f *Forwarder) Stop(drainTimeout time.Duration) error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	f.mu.Unlock()

	err := f.listener.Close()
	if f.unixPath != "" {
		_ = os.Remove(f.unixPath)
	}

	done := make(chan struct{})
	go func() {
		f.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(drainTimeout):
		f.mu.Lock()
		for c := range f.activeConns {
			_ = c.Close()
		}
		f.mu.Unlock()
		<-done
	}
	return err
}

// ExposeManager tracks all L4 expose forwarders across projects in yakd.
type ExposeManager struct {
	mu         sync.Mutex
	forwarders map[string]*Forwarder
}

func NewExposeManager() *ExposeManager {
	return &ExposeManager{
		forwarders: make(map[string]*Forwarder),
	}
}

// Ensure creates or updates a forwarder for key ("<project>/<service>").
// If listen/unix matches the existing forwarder, it swaps the backend IP atomically
// with zero interruption to the listening socket.
func (m *ExposeManager) Ensure(key, listen, unixPath, target string, drainTimeout time.Duration) (string, error) {
	m.mu.Lock()
	existing := m.forwarders[key]
	if existing != nil && existing.listen == listen && existing.unixPath == unixPath {
		existing.Switch(target)
		addr := existing.Addr()
		m.mu.Unlock()
		return addr, nil
	}
	if existing != nil {
		delete(m.forwarders, key)
	}
	m.mu.Unlock()

	if existing != nil {
		_ = existing.Stop(drainTimeout)
	}

	fwd, err := StartForwarder(key, listen, unixPath, target)
	if err != nil {
		return "", err
	}

	m.mu.Lock()
	m.forwarders[key] = fwd
	m.mu.Unlock()
	return fwd.Addr(), nil
}

func (m *ExposeManager) Remove(key string, drainTimeout time.Duration) error {
	m.mu.Lock()
	existing := m.forwarders[key]
	delete(m.forwarders, key)
	m.mu.Unlock()

	if existing == nil {
		return nil
	}
	return existing.Stop(drainTimeout)
}

func (m *ExposeManager) StopAll(drainTimeout time.Duration) {
	m.mu.Lock()
	list := make([]*Forwarder, 0, len(m.forwarders))
	for k, f := range m.forwarders {
		list = append(list, f)
		delete(m.forwarders, k)
	}
	m.mu.Unlock()

	for _, f := range list {
		_ = f.Stop(drainTimeout)
	}
}
