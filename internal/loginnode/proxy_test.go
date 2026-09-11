package loginnode

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A session in a container cannot use a unix socket in its own directory:
// that directory arrives over virtiofs, which exposes the socket's inode and
// then fails the connect with "operation not supported". This is the
// replacement, and the properties that make it acceptable.
func TestTCPBridgeCarriesRequestsAndClosesWithTheSession(t *testing.T) {
	// Something to proxy to, standing in for the controller's socket.
	target := shortSocketPath(t)
	ln, err := net.Listen("unix", target)
	if err != nil {
		t.Fatalf("listen on %s: %v", target, err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(io.Discard, io.LimitReader(c, 1))
				c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
			}()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	br, err := newTCPBridge(ctx, "127.0.0.1", target)
	if err != nil {
		t.Fatal(err)
	}

	// The session is told a URL, not a path -- a path is what did not work.
	ep := br.Endpoint()
	if !strings.HasPrefix(ep, "http://127.0.0.1:") {
		t.Fatalf("endpoint = %q, want a URL on the bound address", ep)
	}
	if br.Root() != "" && strings.Contains(ep, ".sock") {
		t.Error("the endpoint names a socket path")
	}

	// It carries bytes.
	host := strings.TrimPrefix(ep, "http://")
	c, err := net.DialTimeout("tcp", host, 3*time.Second)
	if err != nil {
		t.Fatalf("cannot reach the bridge: %v", err)
	}
	c.Write([]byte("x"))
	buf := make([]byte, 12)
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, _ := c.Read(buf)
	c.Close()
	if n == 0 || !strings.Contains(string(buf[:n]), "HTTP") {
		t.Errorf("nothing came back through the bridge: %q", buf[:n])
	}

	// And it goes when the session does: a listener left behind would be a
	// way into the controller for as long as the machine stayed up.
	br.Close()
	if _, err := net.DialTimeout("tcp", host, time.Second); err == nil {
		t.Error("the bridge is still accepting connections after Close")
	}
}

// Bound to one address rather than everything: the host end of the container
// network is a host-only bridge, so this is reachable from a container here
// and from nowhere else.
func TestTCPBridgeBindsOnlyWhereItIsTold(t *testing.T) {
	// No listener needed: the bridge dials its target per connection, so
	// binding is observable without anything on the other end.
	br, err := newTCPBridge(context.Background(), "127.0.0.1", shortSocketPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer br.Close()
	if !strings.HasPrefix(br.Endpoint(), "http://127.0.0.1:") {
		t.Errorf("bound somewhere other than asked: %s", br.Endpoint())
	}
	// A port of zero would mean it never actually bound.
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(br.Endpoint(), "http://"))
	if port == "0" || port == "" {
		t.Errorf("no port in %q", br.Endpoint())
	}
}

// shortSocketPath returns a path a unix socket can actually be bound to.
//
// AF_UNIX paths are limited to about 104 bytes, and a test temp directory is
// longer than that on macOS -- which made both of these tests skip rather
// than run, which is worse than not having them.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "shome-b")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}
