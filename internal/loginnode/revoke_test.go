package loginnode

import (
	"context"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// Checking a key only at login would mean "signed out" really meant "cannot
// sign in again", leaving the session somebody was worried about still
// running. These cover both ways a live session is supposed to die.

// The fast path: revoking through shome closes the session at once.
func TestRevocationClosesALiveSession(t *testing.T) {
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
	if _, err := sess.Output("whoami"); err != nil {
		t.Fatalf("session was not working to begin with: %v", err)
	}
	sess.Close()

	closed := make(chan struct{})
	go func() { cl.Wait(); close(closed) }()

	// What the controller's hook calls.
	if n := f.srv.CloseSessionsFor("alice", ssh.FingerprintSHA256(signer.PublicKey())); n != 1 {
		t.Fatalf("closed %d sessions, want 1", n)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the connection stayed open after its key was revoked")
	}
}

// Revoking one person's access must not disturb anybody else's session.
func TestRevocationOnlyClosesTheRightSessions(t *testing.T) {
	f := newFixture(t)
	aliceKey := f.addUser(t, "alice")
	bobKey := f.addUser(t, "bob")

	alice, err := f.dial("alice", aliceKey)
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()
	bob, err := f.dial("bob", bobKey)
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()

	f.srv.CloseSessionsFor("alice", "")

	// Bob's session must still work.
	sess, err := bob.NewSession()
	if err != nil {
		t.Fatalf("bob's session died when alice was revoked: %v", err)
	}
	defer sess.Close()
	if _, err := sess.Output("whoami"); err != nil {
		t.Errorf("bob cannot run commands: %v", err)
	}
}

// The slow path, for when the hook could not have fired: the database changed
// underneath, or a restart lost the registry. A session must still notice.
func TestSessionNoticesRevocationWithoutTheHook(t *testing.T) {
	f := newFixture(t)
	signer := f.addUser(t, "alice")
	cl, err := f.dial("alice", signer)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	closed := make(chan struct{})
	go func() { cl.Wait(); close(closed) }()

	// Delete the key directly, as another process would -- no hook involved.
	if err := f.st.DeleteUserKey(t.Context(),
		ssh.FingerprintSHA256(signer.PublicKey()), time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(4 * recheckInterval):
		t.Fatalf("session survived revocation for more than %s", 4*recheckInterval)
	}
}

// And the command someone types immediately after being signed out must fail,
// without waiting for the timer.
func TestRevokedSessionCannotRunOneMoreCommand(t *testing.T) {
	f := newFixture(t)
	signer := f.addUser(t, "alice")
	cl, err := f.dial("alice", signer)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	// Hold a session open, then revoke without the hook so the timer has not
	// necessarily fired yet.
	sess, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := f.st.DeleteUserKey(t.Context(),
		ssh.FingerprintSHA256(signer.PublicKey()), time.Now()); err != nil {
		t.Fatal(err)
	}

	var stderr strings.Builder
	sess.Stderr = &stderr
	err = sess.Run("whoami")
	if err == nil {
		t.Fatal("a revoked session ran another command")
	}
	if !strings.Contains(stderr.String(), "signed out") {
		t.Errorf("unhelpful refusal: %q", stderr.String())
	}
	if cmd, _ := f.runner.last(); cmd != "" {
		t.Errorf("the command reached the runner anyway: %q", cmd)
	}
}

// Suspending an account must also end its sessions, or quarantine only stops
// the next login.
func TestSuspensionEndsALiveSession(t *testing.T) {
	f := newFixture(t)
	signer := f.addUser(t, "alice")
	cl, err := f.dial("alice", signer)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	closed := make(chan struct{})
	go func() { cl.Wait(); close(closed) }()

	if err := f.st.SetUserDisabled(t.Context(), "alice", true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(4 * recheckInterval):
		t.Fatal("a suspended account kept its session")
	}
}

func TestStillAuthorized(t *testing.T) {
	f := newFixture(t)
	signer := f.addUser(t, "alice")
	fp := ssh.FingerprintSHA256(signer.PublicKey())
	ctx := context.Background()

	if err := f.srv.stillAuthorized(ctx, "alice", fp); err != nil {
		t.Errorf("a valid session was rejected: %v", err)
	}
	if err := f.srv.stillAuthorized(ctx, "alice", "SHA256:gone"); err == nil {
		t.Error("an unregistered key passed")
	}
	if err := f.srv.stillAuthorized(ctx, "nobody", fp); err == nil {
		t.Error("a nonexistent account passed")
	}
	// A certificate session has no registered key; its own expiry bounds it.
	if err := f.srv.stillAuthorized(ctx, "alice", ""); err != nil {
		t.Errorf("a certificate session was rejected: %v", err)
	}
}

// Why a session ended has to be the right reason, because this string is what
// the person at the prompt is shown and the only thing they have to act on.
//
// A quarantined account told "your account no longer exists" goes to their
// admin with the wrong question, and an admin told an account is missing when
// it is merely suspended goes looking for a deletion that never happened.
// Both refusals end the session either way, so nothing but the message
// distinguishes them.
func TestASessionIsToldWhyItEnded(t *testing.T) {
	f := newFixture(t)
	signer := f.addUser(t, "alice")
	fp := ssh.FingerprintSHA256(signer.PublicKey())
	ctx := context.Background()

	if err := f.srv.stillAuthorized(ctx, "alice", fp); err != nil {
		t.Fatalf("setup: a valid session was rejected: %v", err)
	}

	// Suspended: still there, not allowed in.
	if err := f.st.SetUserDisabled(ctx, "alice", true); err != nil {
		t.Fatal(err)
	}
	err := f.srv.stillAuthorized(ctx, "alice", fp)
	if err == nil {
		t.Fatal("a suspended account's session was allowed to continue")
	}
	if !strings.Contains(err.Error(), "suspend") {
		t.Errorf("a suspended account is told %q; it should say it was suspended", err)
	}
	if strings.Contains(err.Error(), "no longer exists") {
		t.Errorf("a suspended account is told it does not exist: %q", err)
	}

	// Lifted: back in, without reconnecting.
	if err := f.st.SetUserDisabled(ctx, "alice", false); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.stillAuthorized(ctx, "alice", fp); err != nil {
		t.Errorf("lifting the suspension left the session refused: %v", err)
	}

	// Gone: a different answer, and the one that used to cover both.
	err = f.srv.stillAuthorized(ctx, "nobody", fp)
	if err == nil {
		t.Fatal("a nonexistent account's session was allowed to continue")
	}
	if !strings.Contains(err.Error(), "no longer exists") {
		t.Errorf("a missing account is told %q", err)
	}
}

// Minting an enrollment code is an admin operation, so a refusal names the
// reason. Spending one is not -- see the test below.
func TestMintingACodeForASuspendedAccountSaysWhy(t *testing.T) {
	f := newFixture(t)
	f.addUser(t, "alice")
	ctx := context.Background()
	if err := f.st.SetUserDisabled(ctx, "alice", true); err != nil {
		t.Fatal(err)
	}
	_, err := f.st.CreateEnrollCode(ctx, "alice", time.Now())
	if err == nil {
		t.Fatal("a code was minted for a suspended account")
	}
	if !strings.Contains(err.Error(), "suspend") {
		t.Errorf("the refusal says %q; an admin needs to know it is the quarantine", err)
	}
	// An account that really is missing is a different answer.
	if _, err := f.st.CreateEnrollCode(ctx, "nobody", time.Now()); err == nil {
		t.Error("a code was minted for a nonexistent account")
	} else if strings.Contains(err.Error(), "suspend") {
		t.Errorf("a missing account is reported as suspended: %q", err)
	}
}

// Spending a code is the one lookup that must NOT say why it failed. The code
// is offered by somebody not yet authenticated as anyone, so "that account is
// suspended" would confirm an account name to a stranger holding a guess.
func TestSpendingACodeNeverSaysWhichAccountExists(t *testing.T) {
	f := newFixture(t)
	f.addUser(t, "alice")
	ctx := context.Background()
	code, err := f.st.CreateEnrollCode(ctx, "alice", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetUserDisabled(ctx, "alice", true); err != nil {
		t.Fatal(err)
	}

	suspended := f.spendErr(t, code)
	bogus := f.spendErr(t, "not-a-real-code")
	if suspended == "" {
		t.Fatal("a suspended account's code was still spendable")
	}
	if suspended != bogus {
		t.Errorf("a suspended account's code is refused differently from a bogus one:\n"+
			"  suspended: %s\n  bogus:     %s\n"+
			"whoever offered it is not yet authenticated as anybody, so the two "+
			"must be one answer", suspended, bogus)
	}
	if strings.Contains(suspended, "alice") || strings.Contains(suspended, "suspend") {
		t.Errorf("the refusal leaks the account's state to an unauthenticated caller: %s", suspended)
	}
}

// spendErr returns the message from trying to spend a code, or "" on success.
func (f *fixture) spendErr(t *testing.T, code string) string {
	t.Helper()
	if _, err := f.st.RedeemEnrollCode(context.Background(), code, time.Now()); err != nil {
		return err.Error()
	}
	return ""
}
