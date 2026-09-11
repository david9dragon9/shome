package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/davidwu/shome/internal/platform"
)

// How a running job reaches the cluster.
//
// A job is in a sandbox with no route to the controller: the control socket
// is inside the installation root, which every job profile denies, and on
// most machines the controller is not even here. So the agent listens
// somewhere the job can reach and forwards what it hears over the
// connection it already has to the controller. See internal/ctl/jobapi.go
// for the controller's half and for why the job's own token, not this
// node's certificate, decides what the answer is.
//
// Two kinds of listener, for one reason:
//
//   - A unix socket in the job's scratch directory, for a job that shares
//     this kernel -- a GPU job on a Mac, any job on Linux. The scratch is
//     the job's own writable directory and the sandbox already permits
//     binding and connecting there, so this needs no new permission and
//     works whether or not the job was given network access.
//   - A TCP listener on the host's end of the container network, for a job
//     in a container. Its view of the scratch comes over virtiofs, which
//     exposes a socket's inode and then refuses the connection: the socket
//     is visible, looks correct, and does not work. The login node found
//     this first; see internal/loginnode/proxy.go.
//
// The TCP one is reachable by anything else on that host-only network, and
// is not a way in: every request over it still needs a credential, and the
// only one that works is the job's own.

// ErrJobNeedsNetwork is returned for a contained job that was given no
// network and therefore cannot be given a route to the cluster.
//
// Worth a distinct error because it is the one case a person can act on:
// the same job with --network, or `srun --pty`, has the commands. Deciding
// to hand a network-less job a host-only network instead would be shome
// quietly overriding what the submitter asked for, and the guarantee that
// a job without --network can reach nothing is worth more than squeue is.
var ErrJobNeedsNetwork = errors.New("a job in a container reaches the cluster " +
	"over the network, and this one was submitted without --network")

// jobLink is one job's route to the cluster API.
type jobLink struct {
	ln  net.Listener
	srv *http.Server
	// dir is the directory holding the socket, on this machine. Empty for a
	// TCP link.
	dir string
	// env is what the job is told: either SHOME_ROOT, naming a directory
	// with a socket in it, or SHOME_ENDPOINT, naming a URL. The CLI already
	// understands both, because a login session is given the same choice.
	env map[string]string
}

// maxUnixPath is the longest a unix socket path may be. 104 on macOS, 108
// on Linux; the smaller one applies everywhere so that a path which works
// on one machine of a cluster works on all of them.
const maxUnixPath = 104

// jobLinkDir is the directory inside a job's scratch that holds its socket.
//
// Named like the installation directory the CLI expects, because that is
// what it is being told this is: SHOME_ROOT points here and the CLI looks
// for shome.sock inside it. Nothing else is in it.
const jobLinkDir = ".shome"

// openJobLink starts a job's route to the cluster, if one can be made.
//
// Returns nil, with no error, when this node has no controller connection
// to forward to -- an agent running standalone, where there is nothing to
// ask -- and an error when a link was wanted and could not be made. Neither
// is fatal to the job: it loses the cluster commands and runs anyway.
func (a *Agent) openJobLink(ctx context.Context, sb *platform.Sandbox, mapped, network bool) (*jobLink, error) {
	if a.API == nil {
		return nil, nil
	}
	handler := http.HandlerFunc(a.API.ProxyAPI)

	// A container: no unix socket, so the host's end of the container
	// network.
	if mapped && a.Backend != nil {
		if addr := a.Backend.SessionHostAddr(ctx); addr != "" {
			if !network {
				// `--network none` leaves the container with no interface
				// at all, so there is no address to listen on that this job
				// could dial. Not an error: the job asked for no network
				// and got exactly that. See ErrJobNeedsNetwork for why this
				// is reported to the job rather than only logged.
				return nil, ErrJobNeedsNetwork
			}
			ln, err := net.Listen("tcp", net.JoinHostPort(addr, "0"))
			if err != nil {
				return nil, fmt.Errorf("listen for the job's cluster access: %w", err)
			}
			l := &jobLink{ln: ln, env: map[string]string{
				"SHOME_ENDPOINT": "http://" + ln.Addr().String(),
			}}
			l.serve(handler)
			return l, nil
		}
	}

	// Everything else: a socket in the job's own scratch.
	dir := filepath.Join(sb.ScratchDir, jobLinkDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "shome.sock")
	// A unix socket path is bounded by the OS at a little over a hundred
	// bytes, and a job's scratch is several directories deep inside the
	// installation root. Over the limit, bind fails with "invalid
	// argument", which says nothing about paths -- so say it here. The
	// control socket makes the same check for the same reason.
	if len(path) >= maxUnixPath {
		return nil, fmt.Errorf("this job's socket path is %d bytes, over the "+
			"%d-byte OS limit: %s\n  a shorter SHOME_ROOT would fit",
			len(path), maxUnixPath, path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen for the job's cluster access: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	// Where that directory appears from inside, which for a mount namespace
	// is not where it is.
	inside := dir
	if sb.ScratchIn != "" {
		inside = sb.ScratchIn + "/" + jobLinkDir
	}
	l := &jobLink{ln: ln, dir: dir, env: map[string]string{"SHOME_ROOT": inside}}
	l.serve(handler)
	return l, nil
}

// serve starts the link's server.
func (l *jobLink) serve(h http.Handler) {
	l.srv = &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go l.srv.Serve(l.ln)
}

// Close stops the link. Called when the job ends: the credential it carried
// is being revoked at the same time, and a listener for a job that is over
// is a door to nowhere.
func (l *jobLink) Close() {
	if l == nil {
		return
	}
	if l.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		l.srv.Shutdown(ctx)
	}
	if l.dir != "" {
		// The socket goes with the scratch directory anyway; removing it
		// here means a job that outlived its link does not find a dead one.
		os.Remove(filepath.Join(l.dir, "shome.sock"))
	}
}
