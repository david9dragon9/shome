package loginnode

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
)

// A per-session bridge to the controller's control socket.
//
// # Why this exists
//
// The shome CLI talks to the controller over a unix socket in the
// installation root. A session's sandbox denies that root outright -- it
// holds the admin token, the certificate authority's private key and every
// other account's files -- so a session cannot reach the socket directly, and
// widening the sandbox to expose the directory it lives in would defeat the
// point.
//
// So the login node listens on a second socket inside the session's own home
// directory and copies bytes to the real one. The session can reach it, the
// sandbox stays shut, and the bridge carries no authority of its own: every
// request over it still needs the session's own credential, which is scoped
// to one account and expires.
//
// The bridge is a byte copier and deliberately understands nothing about
// HTTP. A proxy that parsed and rewrote requests would be a place where
// authorisation could be got wrong; this one cannot grant anything, because
// it cannot add a header.
type bridge struct {
	ln     net.Listener
	path   string
	target string
	// endpoint is the URL a session should use, when the bridge is a TCP
	// listener rather than a socket in the account's own directory.
	endpoint string

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

// bridgeDir is where a session's control socket lives, inside its own home.
func bridgeDir(home string) string { return filepath.Join(home, ".shome") }

// newTCPBridge starts a bridge reachable over the network at addr.
//
// For a session in a container, where a unix socket cannot be used: the
// account's directory reaches the container over virtiofs, which exposes a
// socket's inode but refuses the connect with "operation not supported". The
// socket is visible, looks correct, and does not work.
//
// Bound to one address -- the host end of the container network, which is a
// host-only bridge -- rather than to everything, so it is reachable from a
// container on this machine and from nowhere else. Every request over it
// still needs the session's own credential; the bridge grants nothing.
func newTCPBridge(ctx context.Context, addr, target string) (*bridge, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(addr, "0"))
	if err != nil {
		return nil, err
	}
	b := &bridge{ln: ln, target: target, endpoint: "http://" + ln.Addr().String()}
	b.wg.Add(1)
	go b.serve(ctx)
	return b, nil
}

// newBridge starts a bridge from a socket in the session's home to target.
func newBridge(ctx context.Context, home, target string) (*bridge, error) {
	dir := bridgeDir(home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "shome.sock")
	// A socket left behind by a session that was killed rather than closed
	// would make Listen fail with "address already in use". Nothing else can
	// legitimately hold this path: it is inside one account's home and named
	// per session.
	os.Remove(path)
	if len(path) > 103 {
		// The same limit the controller checks. Worth its own message here,
		// because the cause is the account name plus the installation path
		// rather than anything the user did.
		return nil, fmt.Errorf("this account's home path is too long for a control "+
			"socket (%d bytes, limit 103): %s", len(path), path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// Owner-only. The session runs as the same uid as the daemon, so this is
	// not the security boundary -- the token is -- but a socket readable by
	// anything on the machine is not worth leaving lying around.
	os.Chmod(path, 0o600)

	b := &bridge{ln: ln, path: path, target: target}
	b.wg.Add(1)
	go b.serve(ctx)
	return b, nil
}

func (b *bridge) serve(ctx context.Context) {
	defer b.wg.Done()
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			return
		}
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.pipe(ctx, conn)
		}()
	}
}

func (b *bridge) pipe(ctx context.Context, from net.Conn) {
	defer from.Close()
	to, err := net.Dial("unix", b.target)
	if err != nil {
		return
	}
	defer to.Close()

	// Closed when the session ends, so a client holding a connection open
	// does not keep the bridge alive after logout.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			from.Close()
			to.Close()
		case <-done:
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(to, from); closeWrite(to) }()
	go func() { defer wg.Done(); io.Copy(from, to); closeWrite(from) }()
	wg.Wait()
}

// closeWrite half-closes, so the other side sees the end of a request body
// rather than waiting for a full close.
func closeWrite(c net.Conn) {
	if u, ok := c.(*net.UnixConn); ok {
		u.CloseWrite()
	}
}

// Path is the socket the session should talk to.
func (b *bridge) Path() string { return b.path }

// Root is what SHOME_ROOT should be inside the session, so the CLI finds the
// bridge instead of the real installation.
func (b *bridge) Root() string { return filepath.Dir(b.path) }

// Endpoint is the URL to reach this bridge on, empty for a socket bridge.
func (b *bridge) Endpoint() string { return b.endpoint }

// Close stops the bridge and removes its socket.
func (b *bridge) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	b.mu.Unlock()

	b.ln.Close()
	b.wg.Wait()
	// Removed rather than left: a stale socket in the account's home is
	// clutter that `shome fs ls` would show them and that a later session
	// would have to clean up anyway. A TCP bridge has no path.
	if b.path != "" {
		os.Remove(b.path)
	}
}
