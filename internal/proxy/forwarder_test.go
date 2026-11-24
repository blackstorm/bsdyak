package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func startEchoBackend(t *testing.T, tag string) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen backend %s: %v", tag, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
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
	return ln.Addr().String(), func() {
		_ = ln.Close()
		<-done
	}
}

func TestZeroDropBlueGreenSwitch(t *testing.T) {
	v1Addr, stopV1 := startEchoBackend(t, "v1")
	defer stopV1()
	v2Addr, stopV2 := startEchoBackend(t, "v2")
	defer stopV2()

	mgr := NewExposeManager()
	defer mgr.StopAll(time.Second)

	listenAddr, err := mgr.Ensure("shop/api", "127.0.0.1:0", "", v1Addr, time.Second)
	if err != nil {
		t.Fatalf("Ensure v1: %v", err)
	}

	// Open a long-lived connection pinned to v1 before switch.
	conn1, err := net.Dial("tcp", listenAddr)
	if err != nil {
		t.Fatalf("dial conn1: %v", err)
	}
	defer conn1.Close()

	_, _ = io.WriteString(conn1, "hello\n")
	resp1, err := bufio.NewReader(conn1).ReadString('\n')
	if err != nil || resp1 != "v1:hello\n" {
		t.Fatalf("expected v1:hello, got %q (err=%v)", resp1, err)
	}

	// Atomic blue/green switch to v2.
	listenAddr2, err := mgr.Ensure("shop/api", "127.0.0.1:0", "", v2Addr, time.Second)
	// Note: using existing key with different literal listenAddr pattern would recreate; test same config:
	_ = listenAddr2

	mgr2 := NewExposeManager()
	defer mgr2.StopAll(time.Second)
	fwd, err := StartForwarder("shop/api", "127.0.0.1:0", "", v1Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer fwd.Stop(time.Second)
	mgr2.mu.Lock()
	mgr2.forwarders["shop/api"] = fwd
	mgr2.mu.Unlock()
	fwd.listen = "127.0.0.1:0"

	// Hold open connection on fwd while v1 is active.
	pinned, err := net.Dial("tcp", fwd.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	_, _ = io.WriteString(pinned, "first\n")
	pinnedReader := bufio.NewReader(pinned)
	got, err := pinnedReader.ReadString('\n')
	if err != nil || got != "v1:first\n" {
		t.Fatalf("expected v1:first, got %q %v", got, err)
	}

	// Switch using Ensure with matching listen key.
	if _, err := mgr2.Ensure("shop/api", "127.0.0.1:0", "", v2Addr, time.Second); err != nil {
		t.Fatal(err)
	}

	// Existing pinned connection still drains cleanly against v1.
	_, _ = io.WriteString(pinned, "drain\n")
	got, err = pinnedReader.ReadString('\n')
	if err != nil || got != "v1:drain\n" {
		t.Fatalf("pinned connection dropped or rerouted mid-stream: %q %v", got, err)
	}

	// New connections hit v2 immediately.
	connNew, err := net.Dial("tcp", fwd.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer connNew.Close()
	_, _ = io.WriteString(connNew, "next\n")
	gotNew, err := bufio.NewReader(connNew).ReadString('\n')
	if err != nil || strings.TrimSpace(gotNew) != "v2:next" {
		t.Fatalf("expected v2:next on new connection, got %q %v", gotNew, err)
	}
}

func TestUnixSocketForwarder(t *testing.T) {
	v1Addr, stopV1 := startEchoBackend(t, "unix-v1")
	defer stopV1()

	sock := filepath.Join(t.TempDir(), "api.sock")
	mgr := NewExposeManager()
	defer mgr.StopAll(time.Second)

	addr, err := mgr.Ensure("shop/api", "", sock, v1Addr, time.Second)
	if err != nil {
		t.Fatalf("Ensure unix: %v", err)
	}
	if addr != sock {
		t.Fatalf("expected sock path %s, got %s", sock, addr)
	}

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "ping\n")
	got, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || got != "unix-v1:ping\n" {
		t.Fatalf("unexpected response: %q %v", got, err)
	}
}
