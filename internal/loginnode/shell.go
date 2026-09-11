package loginnode

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/davidwu/shome/internal/maccontainer"
	"github.com/davidwu/shome/internal/platform"
	"github.com/davidwu/shome/internal/ptyx"
	"github.com/davidwu/shome/internal/userenv"
)

// The interactive session: a real shell, in a sandbox, on the login node.
//
// # What changed and why
//
// This used to be a prompt loop that matched the first word of each line
// against a list of shome verbs. That is enough to submit a job and read its
// output, and not enough to prepare one -- no editor, no interpreter, no way
// to install a dependency. The complaint that prompted this was exactly that:
// a logged-in user could not run or install anything.
//
// So a session now gets a shell. What keeps that safe is not a list of
// allowed commands -- that approach cannot survive `python -c` -- but the
// sandbox around it, which is the same machinery that contains batch jobs:
//
//   - The only writable place is the account's own directory, which is also
//     the directory `shome fs` manages, so anything built there is inside the
//     account's disk limit and can be copied to another machine.
//   - The machine owner's home is denied.
//   - Every other account's directory is denied.
//   - shome's own state is denied, which covers the admin token and the
//     certificate authority's private key.
//   - Other processes cannot be inspected or signalled.
//
// Two things are deliberately allowed that a batch job does not get. Outbound
// network, because installing packages is the point. And a bridge to the
// control socket, so the shome CLI works -- carrying a credential scoped to
// this one account, which expires with the session.

// SessionTTL bounds a session credential.
//
// Long enough not to interrupt an afternoon's work, short enough that a
// leaked token is not a standing grant. The credential is also deleted when
// the session closes, so this only matters for a session that ends abruptly.
const SessionTTL = 12 * time.Hour

// shellSession is one interactive shell.
type shellSession struct {
	srv    *Server
	user   string
	keyFP  string
	id     string
	ch     ssh.Channel
	pty    *ptyx.PTY
	bridge *bridge
	tokens func()
	// isolation is the container this session runs in, when it runs in one.
	isolation string
}

// runShell runs a command, or an interactive shell, inside a sandbox.
//
// A terminal is allocated only when the client asked for one. That is not a
// detail: with a pty attached, the shell's stdin is the terminal, so a piped
// `cat job.sh | ssh cluster sbatch` has its script echoed back rather than
// read -- which is exactly how the login node's most useful idiom breaks.
func (s *Server) runShell(ctx context.Context, user, keyFP string, ch ssh.Channel,
	reqs <-chan *ssh.Request, initial *winsize, argv []string, wantPTY bool) int {

	if s.cfg.Backend == nil {
		// No isolation available on this machine. An unconfined shell on
		// somebody's computer is not a thing to hand out because the
		// alternative is inconvenient, so:
		if len(argv) > 0 {
			// A one-shot command is refused outright. Dropping into a prompt
			// here instead would leave `ssh host 'some command'` sitting at
			// an interactive prompt the caller never asked for and cannot
			// see, which reads as a hang.
			fmt.Fprint(ch.Stderr(), toCRLF(
				"shome: this machine cannot run arbitrary commands safely "+
					"(no sandbox available).\n"+
					"Available here: "+strings.Join(s.cfg.Runner.Verbs(), ", ")+"\n"+
					"Anything else belongs in 'sbatch' or 'srun'.\n"))
			return 127
		}
		// An interactive login falls back to the restricted prompt, which is
		// still useful: submit jobs, read output, manage files.
		s.cfg.Log.Warn("no platform backend; serving the restricted prompt",
			"user", user)
		s.interactive(ctx, user, keyFP, ch, true)
		return 0
	}
	home := s.homeFor(user)
	// Keep the managed tools current. Cheap when nothing changed, and it
	// means a fresh install works on first login without an admin step for
	// everything except uv itself.
	if self, err := os.Executable(); err == nil {
		if err := userenv.EnsureManaged(s.cfg.Root, self); err != nil {
			s.cfg.Log.Warn("could not prepare the managed session tools", "err", err)
		}
	}

	sess := &shellSession{srv: s, user: user, keyFP: keyFP, ch: ch,
		id: fmt.Sprintf("login-%s-%d", user, time.Now().UnixNano())}
	defer sess.cleanup()

	// Where the account's directory will appear from inside the sandbox.
	// Asked before the environment is built, because HOME, TMPDIR and PATH
	// all have to name paths that exist in there -- on Linux those are not
	// the host's paths at all.
	cluster, machine := s.names()
	tools := userenv.ToolDir(s.cfg.Root)
	layout := s.cfg.Backend.Layout(platform.Placement{
		Home: home, Tools: tools, User: user, Machine: machine,
	})
	// After the layout, because the per-slot directories to create depend
	// on which environment this session will run in.
	if err := userenv.EnsureHomeSlot(home, layout.Slot); err != nil {
		fmt.Fprint(ch.Stderr(), toCRLF("shome: "+err.Error()+"\n"))
		return 1
	}
	// Which binaries to expose depends on what the session runs rather than
	// on what this machine is: a Linux container on a Mac cannot execute
	// the host's own. See userenv.HostTools, which a job asks the same
	// question of.
	if dir, warn, err := userenv.HostTools(s.cfg.Root, layout.Mapped); err == nil {
		tools = dir
		if warn != "" {
			s.cfg.Log.Warn("stale session tools", "detail", warn)
		}
	} else {
		s.cfg.Log.Warn("no Linux shome for sessions; the cluster CLI will be "+
			"missing inside them", "err", err)
		tools = ""
	}

	env, err := sess.environment(ctx, home, layout, cluster, machine)
	if err != nil {
		fmt.Fprint(ch.Stderr(), toCRLF("shome: "+err.Error()+"\n"))
		return 1
	}

	if len(argv) == 0 {
		if layout.Mapped {
			// The startup file has to be named by its path inside, where it
			// travels with the Linux tools. Naming the host's copy leaves
			// bash on its own defaults -- a "root@<container id>" prompt
			// and none of the session's own settings.
			argv = userenv.ShellArgvAt(layout.Tools)
		} else {
			argv = userenv.ShellArgv(s.cfg.Root)
			for k, v := range userenv.LoginShellEnv(s.cfg.Root) {
				env[k] = v
			}
		}
	}
	// The terminal is allocated before the sandbox, not after, because the
	// policy has to name it: terminal control is denied by default, and
	// without an allowance for this specific pty readline cannot turn the
	// terminal's echo off -- so every command the user types is echoed once
	// by the tty driver and again by readline, and appears twice.
	var p *ptyx.PTY
	if wantPTY {
		var err error
		p, err = ptyx.Open()
		if err != nil {
			fmt.Fprint(ch.Stderr(), toCRLF("shome: cannot allocate a terminal: "+err.Error()+"\n"))
			return 1
		}
		sess.pty = p
	}
	spec := platform.ShellSpec{
		User: user, Home: home,
		ReadOnly:  userenv.ReadOnlyPaths(s.cfg.Root),
		AllowNet:  true,
		Argv:      argv,
		Env:       env,
		TTY:       wantPTY,
		HomeAs:    layout.Home,
		ToolsAs:   layout.Tools,
		ToolsHost: tools,
		// Named so it can be stopped when the session ends. A container
		// outlives the process that started it, so without this every login
		// would leave a virtual machine running -- and an unnamed one could
		// not even be swept up later, being indistinguishable from anything
		// else on the machine.
		Name: sess.isolationName(),
	}
	sess.isolation = spec.Name
	if p != nil {
		spec.TTYPath = p.Slave.Name()
	}
	cmd, err := s.cfg.Backend.ShellCommand(ctx, spec)
	if err != nil {
		fmt.Fprint(ch.Stderr(), toCRLF("shome: "+err.Error()+"\n"))
		return 1
	}

	if !wantPTY {
		// Pipes, so stdin is the client's stdin and output is not echoed.
		code := sess.runPiped(ctx, cmd, reqs)
		s.cfg.Log.Info("sandboxed command", "user", user, "session", sess.id, "exit", code)
		return code
	}

	if initial != nil {
		p.Resize(uint16(initial.Rows), uint16(initial.Cols), uint16(initial.Width), uint16(initial.Height))
	} else {
		// A sane default rather than 0x0, which makes full-screen programs
		// draw into a zero-sized window and appear to hang.
		p.Resize(24, 80, 0, 0)
	}
	p.Attach(cmd)

	if err := cmd.Start(); err != nil {
		fmt.Fprint(ch.Stderr(), toCRLF("shome: cannot start a shell: "+err.Error()+"\n"))
		return 1
	}
	// Must happen after Start and before reading: while the parent holds the
	// child's terminal open, reads never reach end-of-file and the session
	// hangs after the shell exits.
	p.CloseSlave()

	s.cfg.Log.Info("interactive session started", "user", user, "session", sess.id)
	code := sess.pump(ctx, cmd, reqs)
	s.cfg.Log.Info("interactive session ended", "user", user, "session", sess.id, "exit", code)
	return code
}

// runPiped runs a one-shot command with the channel as its standard streams.
func (sess *shellSession) runPiped(ctx context.Context, cmd *exec.Cmd,
	reqs <-chan *ssh.Request) int {

	// Its own process group, so the whole tree can be signalled if the
	// client disconnects -- a `uv pip install` that outlives the connection
	// would otherwise keep running in the background.
	cmd.SysProcAttr = groupAttr()
	cmd.Stdout = sess.ch
	cmd.Stderr = sess.ch.Stderr()
	in, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintf(sess.ch.Stderr(), "shome: %v\n", err)
		return 1
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(sess.ch.Stderr(), "shome: cannot run that: %v\n", err)
		return 1
	}
	go func() {
		// Closed when the client is done sending, so a command reading
		// stdin sees end-of-file rather than hanging.
		io.Copy(in, sess.ch)
		in.Close()
	}()
	go func() {
		for req := range reqs {
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return exitCode(err)
	case <-ctx.Done():
		if cmd.Process != nil {
			killGroup(cmd.Process.Pid)
		}
		<-done
		return 1
	}
}

// environment builds the session's variables, including its credential and
// the bridge that credential is used over.
func (sess *shellSession) environment(ctx context.Context, home string,
	layout platform.Layout, cluster, machine string) (map[string]string, error) {

	s := sess.srv
	// Built from the paths that will exist inside the sandbox. Using the
	// host's paths here would set HOME and PATH to directories the session
	// cannot see, on any platform that remaps them.
	env := userenv.Session{
		Home: layout.Home, Tools: layout.Tools, Software: layout.Software,
		Slot: layout.Slot,
		User: sess.user, Cluster: cluster, Host: machine,
	}.Env()

	// A credential for this session only, scoped to this account. Not the
	// admin token the login node holds, and not the account's own token,
	// which is stored hashed and cannot be handed out.
	if s.cfg.Store != nil {
		tok, err := s.cfg.Store.CreateSessionToken(ctx, sess.user, sess.id, SessionTTL, time.Now())
		if err != nil {
			return nil, fmt.Errorf("could not issue a session credential: %w", err)
		}
		env["SHOME_TOKEN"] = tok
		sess.tokens = func() {
			// Best effort, and on a fresh context: the session's own is
			// already cancelled by the time this runs.
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			s.cfg.Store.DeleteSessionTokens(c, sess.id)
		}
	}

	// The bridge to the control socket, so the CLI works from inside the
	// sandbox. SHOME_ROOT points at the bridge's directory rather than the
	// real installation, which the sandbox denies.
	sock := s.cfg.ControlSocket
	if sock == "" {
		sock = filepath.Join(s.cfg.Root, "shome.sock")
	}
	if _, err := os.Stat(sock); err == nil {
		// Where a unix socket cannot be reached from inside the session --
		// a container, whose view of the account's directory comes over
		// virtiofs -- the bridge listens on the host's address on the
		// container network instead, and the session is told a URL.
		var br *bridge
		var err error
		if addr := s.cfg.Backend.SessionHostAddr(ctx); addr != "" {
			br, err = newTCPBridge(ctx, addr, sock)
		} else {
			br, err = newBridge(ctx, home, sock)
		}
		if err != nil {
			// Not fatal: a shell without the cluster CLI is still a usable
			// shell, and saying so beats refusing to log in.
			s.cfg.Log.Warn("no control bridge for this session", "user", sess.user, "err", err)
		} else {
			sess.bridge = br
			if ep := br.Endpoint(); ep != "" {
				env["SHOME_ENDPOINT"] = ep
			} else {
				// The bridge lives inside the account's own directory, so
				// its path inside follows wherever that directory was
				// mapped to.
				env["SHOME_ROOT"] = bridgeRootIn(br.Root(), home, layout.Home)
			}
		}
	}
	// Marks the session so the CLI can refuse verbs that manage a local
	// daemon -- 'up', 'down', 'nuke' -- which inside a session would act on
	// the bridge directory rather than a real installation.
	env["SHOME_SESSION"] = "1"
	return env, nil
}

// pump moves bytes between the SSH channel and the terminal until the shell
// exits or the session is revoked.
func (sess *shellSession) pump(ctx context.Context, cmd *exec.Cmd, reqs <-chan *ssh.Request) int {
	p := sess.pty
	var once sync.Once
	stop := func() {
		once.Do(func() {
			// Signal the whole group: the shell may have children, and
			// killing only the shell leaves them attached to a terminal
			// nobody is reading.
			if cmd.Process != nil {
				killGroup(cmd.Process.Pid)
			}
		})
	}

	go func() {
		<-ctx.Done()
		stop()
	}()

	// Terminal requests continue to arrive during the session; window-change
	// is the one that matters, and ignoring it leaves full-screen programs
	// drawing at the wrong size after the user resizes their window.
	go func() {
		for req := range reqs {
			switch req.Type {
			case "window-change":
				if w := parseWinsize(req.Payload); w != nil {
					p.Resize(uint16(w.Rows), uint16(w.Cols), uint16(w.Width), uint16(w.Height))
				}
			case "signal":
				// The client asking us to signal the shell. Honoured only
				// for interrupt-like signals; a session should not be a way
				// to send arbitrary signals into the machine.
				req.Reply(true, nil)
			default:
				if req.WantReply {
					req.Reply(false, nil)
				}
			}
		}
	}()

	// Client to terminal. When the client closes its side the copy ends, and
	// that is all it means: the shell reads end-of-file from its terminal
	// and exits by itself, the way a shell does.
	//
	// Killing the process group here instead -- which is what this did at
	// first -- makes `ssh -t host <<EOF` fail outright: ssh sends the input
	// and closes immediately, so the shell was signalled before it had
	// finished printing its prompt, and the session produced nothing but
	// the terminal's echo of what had been typed.
	go io.Copy(p.Master, sess.ch)

	// Terminal to client. This is the one that decides when we are done:
	// end-of-file here means the shell's terminal has closed, which happens
	// when the shell and everything holding the terminal have exited.
	io.Copy(sess.ch, p.Master)

	// Only now, and only for stragglers: something still holding the
	// terminal after the shell has gone, or a client that vanished.
	stop()
	return exitCode(cmd.Wait())
}

// cleanup releases the session's terminal, bridge and credential.
func (sess *shellSession) cleanup() {
	if sess.pty != nil {
		sess.pty.Close()
	}
	if sess.bridge != nil {
		sess.bridge.Close()
	}
	if sess.tokens != nil {
		sess.tokens()
	}
	if sess.isolation != "" {
		// On a fresh context: the session's own is already cancelled, and a
		// container left running would hold its memory indefinitely.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := sess.srv.cfg.Backend.StopIsolation(ctx, sess.isolation); err != nil {
			sess.srv.cfg.Log.Warn("could not stop the session's container",
				"name", sess.isolation, "err", err)
		}
	}
}

// isolationName is a container id for this session.
//
// Derived from the session id, which is already unique per login, with the
// characters a container id cannot carry removed, and prefixed with this
// installation's tag so that a machine running two of them can tell whose
// container is whose. The tidy-up sweep matches on that prefix; see
// maccontainer.InstallTag.
func (sess *shellSession) isolationName() string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		}
		return '-'
	}, sess.id)
	return "shome-session-" + maccontainer.InstallTag(sess.srv.cfg.Root) + "-" + clean
}

// names reports what this cluster and machine are called.
func (s *Server) names() (cluster, machine string) {
	cluster = "shome"
	if s.cfg.ClusterName != nil {
		cluster = s.cfg.ClusterName()
	} else if s.cfg.Cluster != "" {
		cluster = s.cfg.Cluster
	}
	machine = s.cfg.NodeLabel
	if machine == "" {
		machine = "login"
	}
	return cluster, machine
}

// bridgeRootIn translates a path under the account's home to where it appears
// inside the sandbox.
func bridgeRootIn(path, home, homeIn string) string {
	if homeIn == "" || homeIn == home || !strings.HasPrefix(path, home) {
		return path
	}
	return homeIn + strings.TrimPrefix(path, home)
}

// homeFor is the account's directory on this machine -- the same one
// `shome fs` lists and accounts for.
func (s *Server) homeFor(user string) string {
	return filepath.Join(s.cfg.Root, "users", user)
}

// winsize is a terminal's dimensions as SSH reports them.
type winsize struct{ Cols, Rows, Width, Height uint32 }

// parseWinsize reads a pty-req or window-change payload.
//
// Both start with the same four 32-bit values; pty-req has a terminal name
// before them, which the caller strips.
func parseWinsize(p []byte) *winsize {
	if len(p) < 16 {
		return nil
	}
	return &winsize{
		Cols:   binary.BigEndian.Uint32(p[0:4]),
		Rows:   binary.BigEndian.Uint32(p[4:8]),
		Width:  binary.BigEndian.Uint32(p[8:12]),
		Height: binary.BigEndian.Uint32(p[12:16]),
	}
}

// parsePTYReq reads a pty-req payload: a terminal name, then the dimensions.
func parsePTYReq(p []byte) (term string, w *winsize) {
	if len(p) < 4 {
		return "", nil
	}
	n := binary.BigEndian.Uint32(p[0:4])
	if int(n) > len(p)-4 {
		return "", nil
	}
	term = string(p[4 : 4+n])
	return term, parseWinsize(p[4+n:])
}

// exitCode turns a wait error into a shell exit status.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errorsAs(err, &ee) {
		return ee.ExitCode()
	}
	return 1
}

// shellQuoteArgs renders an argument vector for a log line.
func shellQuoteArgs(argv []string) string { return strings.Join(argv, " ") }
