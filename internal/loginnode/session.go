package loginnode

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// Session handling.
//
// The containment story here is subtractive: the only channel type accepted is
// "session", and within it the only requests honoured are "exec", "shell" and
// "pty-req". Everything else -- direct-tcpip, forwarded-tcpip, subsystem
// (which is how SFTP arrives), x11-req, and arbitrary env -- is rejected
// because it is never handled. There is no code path that opens a socket, no
// code path that reads a caller-supplied file path, and no code path that runs
// a shell. A hardened sshd_config achieves the same by listing each feature
// and turning it off; here the feature does not exist to turn off.

func (s *Server) handle(ctx context.Context, nConn net.Conn) {
	defer nConn.Close()

	// Bound the unauthenticated phase: a connection that never finishes the
	// handshake must not hold a slot indefinitely.
	nConn.SetDeadline(time.Now().Add(AuthTimeout))
	conn, chans, reqs, err := ssh.NewServerConn(nConn, s.sshCfg)
	if err != nil {
		return // already logged by AuthLogCallback
	}
	defer conn.Close()
	nConn.SetDeadline(time.Time{})

	user := userOf(conn.Permissions)
	if user == "" {
		return
	}
	keyFP := keyFingerprintOf(conn.Permissions)
	s.sessions.Add(1)
	defer s.sessions.Add(-1)

	sessCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Register the connection so its access can be withdrawn from outside,
	// and close the whole thing when that happens -- cancelling the context
	// alone would leave a client sitting on an open socket.
	notify := func(msg string) { conn.SendRequest("shome-revoked", false, []byte(msg)) }
	id := s.live.add(&liveSession{user: user, keyFP: keyFP, cancel: cancel, notify: notify})
	defer s.live.remove(id)
	go s.watchAuthorization(sessCtx, cancel, user, keyFP, notify)
	go func() {
		<-sessCtx.Done()
		conn.Close()
	}()

	// Global requests are all refused: keepalives are harmless but
	// tcpip-forward is a listening socket, and there is no reason to sort
	// them when none are wanted.
	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			// This is where port forwarding dies.
			newChan.Reject(ssh.Prohibited,
				"this login node only accepts session channels")
			continue
		}
		ch, chReqs, err := newChan.Accept()
		if err != nil {
			return
		}
		go s.serveSession(sessCtx, user, keyFP, ch, chReqs)
	}
}

// serveSession handles one session channel: at most one command, or one
// interactive loop.
func (s *Server) serveSession(ctx context.Context, user, keyFP string, ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	ctx, cancel := context.WithTimeout(ctx, IdleTimeout)
	defer cancel()

	wantsPTY := false
	var size *winsize
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			// A terminal is now genuinely allocated: a session gets a real
			// shell, and a shell without a terminal prints no prompt and
			// does no line editing.
			wantsPTY = true
			_, size = parsePTYReq(req.Payload)
			req.Reply(true, nil)

		case "env":
			// Silently ignored. Honouring caller-supplied environment on a
			// gateway means letting them set things like SHOME_ACT_AS -- or,
			// now that sessions are sandboxed, SHOME_TOKEN.
			req.Reply(true, nil)

		case "exec":
			cmdline := payloadString(req.Payload)
			req.Reply(true, nil)
			code := s.runExec(ctx, user, keyFP, cmdline, ch, reqs, wantsPTY, size)
			sendExit(ch, code)
			return

		case "shell":
			req.Reply(true, nil)
			code := s.runShell(ctx, user, keyFP, ch, reqs, size, nil, true)
			sendExit(ch, code)
			return

		case "window-change":
			req.Reply(true, nil)

		default:
			// "subsystem" lands here, which is how SFTP is refused.
			req.Reply(false, nil)
		}
	}
}

// runExec handles `ssh host COMMAND`.
//
// A shome verb is answered directly, which keeps `ssh cluster squeue` fast
// and scriptable and means a client that only wants one command pays for no
// sandbox. Anything else is run inside the session sandbox, because now that
// accounts have a real environment, `ssh cluster 'uv run x.py'` should work
// for the same reason typing it at the prompt does.
func (s *Server) runExec(ctx context.Context, user, keyFP, cmdline string,
	ch ssh.Channel, reqs <-chan *ssh.Request, pty bool, size *winsize) int {

	args, err := splitCommand(cmdline)
	if err == nil && len(args) > 0 && s.cfg.Runner.Allowed(args[0]) {
		return s.runOne(ctx, user, keyFP, cmdline, ch, ch.Stderr(), ch)
	}
	if err := s.stillAuthorized(ctx, user, keyFP); err != nil {
		fmt.Fprintf(ch.Stderr(), "shome: %v. Disconnecting.\n", err)
		return 1
	}
	// Through a shell, not an argument vector: someone typing
	// `ssh cluster 'cd data && uv run x.py'` means the shell operators, and
	// the sandbox rather than a parser is what makes that safe to allow.
	// -c, not -lc. A login shell reads /etc/profile, which the sandbox does
	// not expose -- so it printed a permission error before every command --
	// and which runs path_helper, whose whole job is to rewrite PATH. shome
	// sets PATH deliberately, with the account's own tools first; letting
	// the system reorder it would put shome's managed uv behind whatever
	// else is installed.
	return s.runShell(ctx, user, keyFP, ch, reqs, size,
		[]string{shellForExec(), "-c", cmdline}, pty)
}

// shellForExec is the shell a one-shot command runs under.
func shellForExec() string {
	if fi, err := os.Stat("/bin/bash"); err == nil && !fi.IsDir() {
		return "/bin/bash"
	}
	return "/bin/sh"
}

// runOne executes a single command line as user.
func (s *Server) runOne(ctx context.Context, user, keyFP, cmdline string, stdout, stderr io.Writer, stdin io.Reader) int {
	args, err := splitCommand(cmdline)
	if err != nil {
		fmt.Fprintf(stderr, "shome: %v\n", err)
		return 2
	}
	if len(args) == 0 {
		return 0
	}
	// Revalidate before doing anything. The timer will catch a revoked
	// session within seconds, but "within seconds" is not good enough for the
	// command someone types immediately after being signed out.
	if err := s.stillAuthorized(ctx, user, keyFP); err != nil {
		fmt.Fprintf(stderr, "shome: %v. Disconnecting.\n", err)
		return 1
	}

	verb := args[0]
	if !s.cfg.Runner.Allowed(verb) {
		fmt.Fprintf(stderr, "shome: %q is not available from the login node.\n", verb)
		fmt.Fprint(stderr, s.cfg.Runner.Help())
		return 127
	}

	// Some verbs read a script from stdin. Bounded, because a login node must
	// not be a place to upload arbitrary volume.
	var input []byte
	if stdin != nil && needsStdin(verb) {
		input, _ = io.ReadAll(io.LimitReader(stdin, MaxCommandBytes))
	}

	// Pass the session's key through when the runner can use it, so a command
	// can act on this machine rather than all of them.
	var out, errOut []byte
	var code int
	if ka, ok := s.cfg.Runner.(KeyAwareRunner); ok {
		out, errOut, code = ka.RunAs(ctx, user, keyFP, args, input)
	} else {
		out, errOut, code = s.cfg.Runner.Run(ctx, user, args, input)
	}
	writeCapped(stdout, out)
	writeCapped(stderr, errOut)
	s.cfg.Log.Info("login command", "user", user, "verb", verb,
		"args", len(args)-1, "exit", code)
	return code
}

// interactive runs a small prompt loop.
//
// The fallback for a machine that cannot isolate a session -- no Seatbelt, no
// bubblewrap -- where handing out a real shell would mean handing out
// unconfined access to somebody's computer. Sessions on a machine that *can*
// isolate get runShell instead.
//
// Not a shell: it reads a line, matches the first word against the allow-list,
// and runs it. There is no expansion, no pipes, no redirection, no history
// file, and no way to reach anything not on the list.
//
// The line reading is golang.org/x/term rather than a bufio.Scanner, and that
// is not a detail. Once a client asks for a PTY it puts its own terminal in
// raw mode and stops echoing, expecting the server to do it -- so a scanner
// reading the channel produces a session where typing shows nothing at all.
// Worse, Enter arrives as a carriage return, which a scanner splitting on
// newlines never sees, so the session appears completely dead. x/term does the
// echoing, the line editing and the CR handling that a terminal expects.
func (s *Server) interactive(ctx context.Context, user, keyFP string, ch ssh.Channel, pty bool) {
	t := term.NewTerminal(ch, "")
	t.SetPrompt(user + "> ")

	fmt.Fprintf(t, "shome %s\r\n", s.cfg.Cluster)
	fmt.Fprintf(t, "signed in as %s\r\n\r\n", user)
	writeCRLF(t, s.cfg.Runner.Help())
	fmt.Fprint(t, "\r\n")

	for {
		if ctx.Err() != nil {
			fmt.Fprint(t, "session timed out\r\n")
			return
		}
		line, err := t.ReadLine()
		if err != nil {
			return // client disconnected, or Ctrl-D
		}
		line = strings.TrimSpace(line)
		switch line {
		case "":
			continue
		case "exit", "quit", "logout":
			fmt.Fprint(t, "bye\r\n")
			return
		}
		// stdin is the terminal here, so a command cannot also read from it.
		s.runOne(ctx, user, keyFP, line, crlfWriter{t}, crlfWriter{t}, nil)
	}
}

// crlfWriter turns the bare newlines of ordinary command output into the CRLF
// a terminal expects, so output does not stair-step down the screen.
type crlfWriter struct{ w io.Writer }

func (c crlfWriter) Write(p []byte) (int, error) {
	if _, err := c.w.Write([]byte(toCRLF(string(p)))); err != nil {
		return 0, err
	}
	return len(p), nil
}

func writeCRLF(w io.Writer, s string) { io.WriteString(w, toCRLF(s)) }

func toCRLF(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

func writeCapped(w io.Writer, b []byte) {
	if len(b) > MaxOutputBytes {
		b = append(b[:MaxOutputBytes:MaxOutputBytes],
			[]byte("\n... output truncated at 8 MiB\n")...)
	}
	w.Write(b)
}

// needsStdin reports whether a verb reads piped input. Only sbatch does, for
// `cat job.sh | ssh cluster sbatch`.
func needsStdin(verb string) bool { return verb == "sbatch" }

// payloadString decodes an SSH string-prefixed payload.
func payloadString(p []byte) string {
	if len(p) < 4 {
		return ""
	}
	n := binary.BigEndian.Uint32(p)
	if int(n) > len(p)-4 {
		return ""
	}
	return string(p[4 : 4+n])
}

func sendExit(ch ssh.Channel, code int) {
	var payload = struct{ Status uint32 }{uint32(code)}
	ch.SendRequest("exit-status", false, ssh.Marshal(&payload))
}
