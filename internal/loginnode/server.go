// Package loginnode is the cluster's front door: an SSH server that lets a
// shome account submit and manage jobs from another computer, and do nothing
// else.
//
// It is an embedded SSH server rather than a configured system sshd, for three
// reasons that all point the same way:
//
//   - Authentication is the point. A shome account is not an OS account, so
//     the system sshd can only be told about it indirectly -- through a CA and
//     a certificate principal read back out of an environment variable. An
//     embedded server can just ask the user database, which is what the admin
//     actually manages.
//   - Containment is by construction. There is no shell to escape to, no
//     subsystem to request, no forwarding to disable, because none of it is
//     implemented. Compare a hardened sshd_config, where every one of those is
//     a line you must remember to write, and a missing line is a hole.
//   - Reversibility. Nothing outside shome's own directory is touched: no
//     system sshd config, no host keys in /etc, nothing to put back.
//
// The login node deliberately runs no jobs. It holds a connection, checks an
// identity, and forwards a handful of commands to the controller.
package loginnode

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/davidwu/shome/internal/platform"
	"github.com/davidwu/shome/internal/sshca"
	"github.com/davidwu/shome/internal/store"
)

// Limits keep a login node cheap and hard to abuse. It is a gateway, not a
// compute node: anything expensive happening here is a bug or an attack.
const (
	// MaxSessions caps concurrent logins. A home cluster has a handful of
	// users; a flood past this is not legitimate traffic.
	MaxSessions = 32

	// AuthTimeout bounds how long an unauthenticated connection may sit
	// holding resources.
	AuthTimeout = 30 * time.Second

	// IdleTimeout closes a session nobody is using. Interactive sessions on a
	// login node are typically seconds of typing separated by long silences,
	// so this is generous.
	IdleTimeout = 30 * time.Minute

	// MaxCommandBytes bounds a single command line.
	MaxCommandBytes = 64 << 10

	// MaxOutputBytes bounds what one command may write back, so a runaway
	// listing cannot be used to exhaust memory or saturate the link.
	MaxOutputBytes = 8 << 20
)

// Runner executes one allow-listed command on behalf of an authenticated user.
//
// An interface so the SSH layer can be tested without a controller, and so the
// execution strategy stays replaceable: today it runs the shome CLI with the
// caller's identity, and nothing here depends on that.
type Runner interface {
	Run(ctx context.Context, user string, args []string, stdin []byte) (stdout, stderr []byte, code int)
	// Allowed reports whether a verb may be run at all, so the server can
	// refuse before allocating anything.
	Allowed(verb string) bool
	// Help is what an interactive session prints on connect.
	Help() string
	// Verbs lists what is allowed, for the message shown on a machine that
	// cannot sandbox anything else.
	Verbs() []string
}

// KeyAwareRunner is a Runner that also wants the fingerprint of the key the
// session authenticated with. Separate so the basic interface stays small and
// a test runner need not care.
type KeyAwareRunner interface {
	RunAs(ctx context.Context, user, keyFP string, args []string, stdin []byte) (stdout, stderr []byte, code int)
}

// Config configures the login node.
type Config struct {
	Addr    string // listen address, e.g. 0.0.0.0:2222
	Root    string // shome state directory, for host key material
	Store   *store.Store
	UserCA  *sshca.CA // optional: also accept certificates it signed
	Runner  Runner
	Log     *slog.Logger
	Cluster string // fallback name, when ClusterName is not set

	// ClusterName reads the cluster's current name, so renaming it changes
	// the banner and every session's prompt without a restart.
	ClusterName func() string

	// NodeLabel is what this machine is called, for the prompt. "login"
	// where it is not set, since that is what this machine is doing.
	NodeLabel string

	// Backend provides the isolation an interactive session runs inside.
	// Without it, sessions are refused rather than run unconfined.
	Backend platform.Backend

	// ControlSocket is the controller's client API socket, bridged into each
	// session so the shome CLI works from inside the sandbox.
	ControlSocket string
}

// Server is a running login node.
type Server struct {
	cfg      Config
	ln       net.Listener
	sshCfg   *ssh.ServerConfig
	sessions atomic.Int64
	wg       sync.WaitGroup
	closed   atomic.Bool

	// offered holds public keys presented by connections no account
	// recognised, so first-connection enrollment can authorise the right one.
	offered *offeredKeys

	// live tracks open sessions so access can be withdrawn from one that is
	// already connected.
	live *live
}

// Listen prepares the login node without accepting connections yet.
func Listen(cfg Config) (*Server, error) {
	if cfg.Store == nil || cfg.Runner == nil {
		return nil, fmt.Errorf("login node needs a store and a runner")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	signer, err := hostSigner(cfg.Root)
	if err != nil {
		return nil, err
	}

	s := &Server{cfg: cfg, offered: newOfferedKeys(), live: newLive()}
	s.sshCfg = &ssh.ServerConfig{
		// Public keys are the only standing credential. Keyboard-interactive
		// exists solely as the enrollment path: it registers the key a client
		// already offered, in exchange for a single-use code, and grants
		// nothing on its own. There is deliberately no password method -- a
		// password prompt on a cluster whose accounts have API tokens is an
		// invitation to type the token into it.
		PublicKeyCallback:           s.authPublicKey,
		KeyboardInteractiveCallback: s.authEnroll,
		AuthLogCallback: func(conn ssh.ConnMetadata, method string, err error) {
			if err != nil {
				cfg.Log.Info("login refused", "user", conn.User(),
					"method", method, "remote", conn.RemoteAddr().String(), "err", err)
			}
		},
		ServerVersion: "SSH-2.0-shome",
		MaxAuthTries:  3,
	}
	s.sshCfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("login node listener: %w", err)
	}
	s.ln = ln
	return s, nil
}

// Addr is where the login node is listening.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Serve accepts connections until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		s.Close()
	}()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if s.closed.Load() {
				s.wg.Wait()
				return nil
			}
			return err
		}
		// Refuse rather than queue: a login node that stops answering under
		// load is worse than one that says no.
		if s.sessions.Load() >= MaxSessions {
			s.cfg.Log.Warn("login refused: too many sessions",
				"remote", conn.RemoteAddr().String(), "limit", MaxSessions)
			conn.Close()
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(ctx, conn)
		}()
	}
}

// Close stops accepting and waits for open sessions to finish.
func (s *Server) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	return s.ln.Close()
}

// hostSigner loads the login node's host key, generating one if needed, and
// presents its certificate when there is one so clients never see a
// trust-on-first-use prompt.
func hostSigner(root string) (ssh.Signer, error) {
	dir := filepath.Join(root, "ssh")
	keyPath := filepath.Join(dir, "login_host_key")
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("login node host key: %w "+
			"(the controller generates this at startup)", err)
	}
	signer, err := ssh.ParsePrivateKey(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("login node host key: %w", err)
	}
	certPEM, err := os.ReadFile(keyPath + "-cert.pub")
	if err != nil {
		return signer, nil // usable, just without a host certificate
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey(certPEM)
	if err != nil {
		return signer, nil
	}
	cert, ok := pub.(*ssh.Certificate)
	if !ok {
		return signer, nil
	}
	certSigner, err := ssh.NewCertSigner(cert, signer)
	if err != nil {
		return signer, nil
	}
	return certSigner, nil
}
