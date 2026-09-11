package loginnode

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/davidwu/shome/internal/sshca"
	"github.com/davidwu/shome/internal/store"
)

// These tests drive the login node with a real SSH client over a real socket.
// The point is not the happy path -- it is that everything else is refused,
// and that can only be trusted if a genuine client actually tries.

// fakeRunner records what it was asked to run without executing anything.
type fakeRunner struct {
	mu    sync.Mutex
	calls []string
	users []string
}

func (f *fakeRunner) Allowed(verb string) bool {
	return verb == "squeue" || verb == "whoami" || verb == "sbatch"
}
func (f *fakeRunner) Help() string    { return "available commands:\n  squeue\n  whoami\n" }
func (f *fakeRunner) Verbs() []string { return []string{"sbatch", "squeue", "whoami"} }
func (f *fakeRunner) Run(ctx context.Context, user string, args []string, stdin []byte) ([]byte, []byte, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(args, " "))
	f.users = append(f.users, user)
	return []byte("ran " + strings.Join(args, " ") + " as " + user + "\n"), nil, 0
}
func (f *fakeRunner) last() (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return "", ""
	}
	return f.calls[len(f.calls)-1], f.users[len(f.users)-1]
}

type fixture struct {
	srv    *Server
	st     *store.Store
	runner *fakeRunner
	addr   string
	ca     *sshca.CA
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeHostKey(t, filepath.Join(root, "ssh", "login_host_key"))

	st, err := store.Open(filepath.Join(root, "shome.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	ca, err := sshca.LoadOrCreate(filepath.Join(root, "ssh"), "user")
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	srv, err := Listen(Config{
		Addr: "127.0.0.1:0", Root: root, Store: st, UserCA: ca,
		Runner: runner, Log: slog.New(slog.DiscardHandler), Cluster: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.Serve(ctx)
	t.Cleanup(func() { srv.Close() })

	return &fixture{srv: srv, st: st, runner: runner, addr: srv.Addr(), ca: ca}
}

func writeHostKey(t *testing.T, path string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(b), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newKey returns a fresh client key pair.
func newKey(t *testing.T) (ssh.Signer, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer, string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
}

// addUser creates an account with an authorised key.
func (f *fixture) addUser(t *testing.T, name string) ssh.Signer {
	t.Helper()
	if _, err := f.st.CreateUser(t.Context(), name, store.RoleUser, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	signer, pub := newKey(t)
	fp := ssh.FingerprintSHA256(signer.PublicKey())
	if err := f.st.AddUserKey(t.Context(), name, fp, pub, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	return signer
}

// dial connects as user with signer.
func (f *fixture) dial(user string, signer ssh.Signer) (*ssh.Client, error) {
	return ssh.Dial("tcp", f.addr, &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
}

func TestRegisteredKeyLogsIn(t *testing.T) {
	f := newFixture(t)
	signer := f.addUser(t, "alice")

	cl, err := f.dial("alice", signer)
	if err != nil {
		t.Fatalf("alice could not log in with her authorised key: %v", err)
	}
	defer cl.Close()

	sess, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	out, err := sess.Output("whoami")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "as alice") {
		t.Errorf("command did not run as alice: %q", out)
	}
	cmd, user := f.runner.last()
	if cmd != "whoami" || user != "alice" {
		t.Errorf("runner saw (%q, %q)", cmd, user)
	}
}

// The SSH username must match the account the key belongs to. Otherwise one
// person logs in under another's name while authenticating as themselves, and
// every audit entry after that is a lie.
func TestKeyCannotBeUsedForAnotherAccount(t *testing.T) {
	f := newFixture(t)
	aliceKey := f.addUser(t, "alice")
	f.addUser(t, "bob")

	if cl, err := f.dial("bob", aliceKey); err == nil {
		cl.Close()
		t.Fatal("alice's key logged in as bob")
	}
}

func TestUnknownKeyRejected(t *testing.T) {
	f := newFixture(t)
	f.addUser(t, "alice")
	stranger, _ := newKey(t)
	if cl, err := f.dial("alice", stranger); err == nil {
		cl.Close()
		t.Fatal("an unregistered key was accepted")
	}
}

func TestUnknownAccountRejected(t *testing.T) {
	f := newFixture(t)
	signer, _ := newKey(t)
	if cl, err := f.dial("nobody", signer); err == nil {
		cl.Close()
		t.Fatal("a nonexistent account was accepted")
	}
}

// Quarantine must lock somebody out of SSH, not only out of the API. Checking
// the account only when the key was registered would leave a suspended user
// with working access.
func TestSuspendedAccountCannotLogIn(t *testing.T) {
	f := newFixture(t)
	signer := f.addUser(t, "alice")
	if cl, err := f.dial("alice", signer); err != nil {
		t.Fatalf("setup: %v", err)
	} else {
		cl.Close()
	}
	if err := f.st.SetUserDisabled(t.Context(), "alice", true); err != nil {
		t.Fatal(err)
	}
	if cl, err := f.dial("alice", signer); err == nil {
		cl.Close()
		t.Fatal("a suspended account logged in")
	}
}

// Deleting an account must revoke its keys. Otherwise recreating the name
// later silently re-authorises whoever still holds the old key.
func TestDeletedAccountKeysAreRevoked(t *testing.T) {
	f := newFixture(t)
	signer := f.addUser(t, "alice")
	if err := f.st.DeleteUser(t.Context(), "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.CreateUser(t.Context(), "alice", store.RoleUser, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if cl, err := f.dial("alice", signer); err == nil {
		cl.Close()
		t.Fatal("a key survived the deletion of its account")
	}
}

// Port forwarding is how a login node becomes a router into the cluster's
// internals. The channel type must be refused outright.
func TestPortForwardingRefused(t *testing.T) {
	f := newFixture(t)
	signer := f.addUser(t, "alice")
	cl, err := f.dial("alice", signer)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	if conn, err := cl.Dial("tcp", "127.0.0.1:22"); err == nil {
		conn.Close()
		t.Fatal("a direct-tcpip channel was accepted")
	}
	if _, err := cl.Listen("tcp", "127.0.0.1:0"); err == nil {
		t.Fatal("a remote forward listener was accepted")
	}
}

// "subsystem" is how SFTP arrives. Accepting it would hand out a file browser.
func TestSubsystemRefused(t *testing.T) {
	f := newFixture(t)
	signer := f.addUser(t, "alice")
	cl, err := f.dial("alice", signer)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	sess, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.RequestSubsystem("sftp"); err == nil {
		t.Fatal("the sftp subsystem was accepted")
	}
}

func TestDisallowedVerbRefused(t *testing.T) {
	f := newFixture(t)
	signer := f.addUser(t, "alice")
	cl, err := f.dial("alice", signer)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	// This fixture has no sandbox, which is the case that matters here: a
	// machine that cannot isolate a command must refuse to run it rather
	// than run it unconfined. On a machine that can, these same commands
	// run inside the sandbox -- see the platform tests for what that
	// contains.
	for _, cmd := range []string{"/bin/sh", "admin user list", "nuke --confirm", "token"} {
		sess, err := cl.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		var stderr strings.Builder
		sess.Stderr = &stderr
		err = sess.Run(cmd)
		sess.Close()
		if err == nil {
			t.Errorf("%q was accepted on a machine with no sandbox", cmd)
			continue
		}
		// The refusal has to say what the caller can do instead, since
		// otherwise the only signal is a non-zero exit.
		if !strings.Contains(stderr.String(), "sandbox") ||
			!strings.Contains(stderr.String(), "squeue") {
			t.Errorf("%q refused without explanation: %q", cmd, stderr.String())
		}
	}
	if cmd, _ := f.runner.last(); cmd != "" {
		t.Errorf("a refused command still reached the runner: %q", cmd)
	}
}

// Shell metacharacters are no longer refused: a session is a real shell now,
// and `cd data && uv run x.py` is a reasonable thing to type. What keeps that
// safe is the sandbox, not a parser -- an allow-list of commands cannot
// survive `python -c` in any case.
//
// So on a machine with no sandbox they are refused along with everything
// else, and the allow-listed verbs still go straight to the runner without
// being parsed as a shell command.
func TestAllowedVerbsBypassTheShell(t *testing.T) {
	f := newFixture(t)
	signer := f.addUser(t, "alice")
	cl, err := f.dial("alice", signer)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	sess, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	out, err := sess.Output("squeue")
	sess.Close()
	if err != nil {
		t.Fatalf("an allow-listed verb was refused: %v", err)
	}
	if !strings.Contains(string(out), "ran squeue") {
		t.Errorf("squeue did not reach the runner: %q", out)
	}

	// A chained command is not handed to the runner as a verb: it is a shell
	// command, and this machine cannot sandbox one.
	sess2, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	sess2.Stderr = &stderr
	err = sess2.Run("squeue; id")
	sess2.Close()
	if err == nil {
		t.Fatal("a chained command ran on a machine with no sandbox")
	}
	if !strings.Contains(stderr.String(), "sandbox") {
		t.Errorf("unhelpful refusal: %q", stderr.String())
	}
	if got, _ := f.runner.last(); got == "squeue; id" {
		t.Error("a shell command was passed to the runner as a verb")
	}
}

// A certificate from this cluster's CA is the other way in.
func TestCertificateFromClusterCAAccepted(t *testing.T) {
	f := newFixture(t)
	if _, err := f.st.CreateUser(t.Context(), "carol", store.RoleUser, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	signer, pub := newKey(t)
	certPEM, err := f.ca.SignUserKey([]byte(pub), "carol", []string{"carol"}, time.Hour, true)
	if err != nil {
		t.Fatal(err)
	}
	certSigner := certSignerFrom(t, certPEM, signer)

	cl, err := f.dial("carol", certSigner)
	if err != nil {
		t.Fatalf("a certificate from this cluster's CA was rejected: %v", err)
	}
	cl.Close()
}

// A certificate signed by some other CA must not be accepted just because it
// names a principal this cluster recognises.
func TestCertificateFromForeignCARejected(t *testing.T) {
	f := newFixture(t)
	if _, err := f.st.CreateUser(t.Context(), "carol", store.RoleUser, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	other, err := sshca.LoadOrCreate(t.TempDir(), "user")
	if err != nil {
		t.Fatal(err)
	}
	signer, pub := newKey(t)
	certPEM, err := other.SignUserKey([]byte(pub), "carol", []string{"carol"}, time.Hour, true)
	if err != nil {
		t.Fatal(err)
	}
	if cl, err := f.dial("carol", certSignerFrom(t, certPEM, signer)); err == nil {
		cl.Close()
		t.Fatal("a certificate from a foreign CA was accepted")
	}
}

func TestExpiredCertificateRejected(t *testing.T) {
	f := newFixture(t)
	if _, err := f.st.CreateUser(t.Context(), "carol", store.RoleUser, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	signer, pub := newKey(t)
	// Negative validity: already expired when issued.
	certPEM, err := f.ca.SignUserKey([]byte(pub), "carol", []string{"carol"}, -time.Hour, true)
	if err != nil {
		t.Fatal(err)
	}
	if cl, err := f.dial("carol", certSignerFrom(t, certPEM, signer)); err == nil {
		cl.Close()
		t.Fatal("an expired certificate was accepted")
	}
}

func certSignerFrom(t *testing.T, certPEM []byte, signer ssh.Signer) ssh.Signer {
	t.Helper()
	pub, _, _, _, err := ssh.ParseAuthorizedKey(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	cert, ok := pub.(*ssh.Certificate)
	if !ok {
		t.Fatal("not a certificate")
	}
	cs, err := ssh.NewCertSigner(cert, signer)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

// A command's output must be bounded, so one listing cannot exhaust memory.
func TestOutputIsCapped(t *testing.T) {
	var w strings.Builder
	writeCapped(&w, make([]byte, MaxOutputBytes+1024))
	if w.Len() > MaxOutputBytes+128 {
		t.Errorf("wrote %d bytes, expected a cap near %d", w.Len(), MaxOutputBytes)
	}
	if !strings.Contains(w.String(), "truncated") {
		t.Error("truncation was silent")
	}
}

func TestExecPayloadDecoding(t *testing.T) {
	// A malformed payload must not panic or read out of bounds.
	for _, p := range [][]byte{
		nil, {}, {0, 0, 0}, {0, 0, 0, 99, 'a'}, {255, 255, 255, 255},
	} {
		if got := payloadString(p); got != "" {
			t.Errorf("payloadString(%v) = %q, want empty", p, got)
		}
	}
	good := append([]byte{0, 0, 0, 6}, []byte("squeue")...)
	if got := payloadString(good); got != "squeue" {
		t.Errorf("payloadString = %q", got)
	}
}

var _ = io.Discard
var _ = fmt.Sprint

// A client that asks for a PTY puts its own terminal in raw mode and stops
// echoing, and sends carriage returns rather than newlines. A session that
// reads the channel as newline-delimited lines shows nothing when the user
// types and never sees a command -- which reads as a completely dead session.
func TestInteractiveSessionWithPTYEchoesAndRuns(t *testing.T) {
	f := newFixture(t)
	signer := f.addUser(t, "alice")
	cl, err := f.dial("alice", signer)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	sess, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.RequestPty("xterm", 40, 100, ssh.TerminalModes{ssh.ECHO: 0}); err != nil {
		t.Fatal(err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var out lockedBuf
	sess.Stdout = &out
	sess.Stderr = &out
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}

	// Carriage return, as a real terminal sends.
	io.WriteString(stdin, "whoami\r")
	waitFor(t, &out, "as alice")
	io.WriteString(stdin, "exit\r")
	sess.Wait()

	got := out.String()
	// The command must be echoed back, or the user sees nothing as they type.
	if !strings.Contains(got, "whoami") {
		t.Errorf("input was not echoed:\n%s", got)
	}
	// And output must use CRLF, or it stair-steps across the screen.
	if strings.Contains(got, "as alice\n") && !strings.Contains(got, "as alice\r\n") {
		t.Errorf("output is not CRLF terminated:\n%q", got)
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func waitFor(t *testing.T, buf *lockedBuf, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("never saw %q in:\n%s", want, buf.String())
}
