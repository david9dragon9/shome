package loginnode

import (
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/davidwu/shome/internal/store"
)

// enrollDial connects offering signer's key and answering every
// keyboard-interactive prompt with code -- which is what a stock ssh client
// does when its key is refused and the user types the code.
func (f *fixture) enrollDial(user string, signer ssh.Signer, code string) (*ssh.Client, error) {
	return ssh.Dial("tcp", f.addr, &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
			ssh.KeyboardInteractive(func(name, instruction string, qs []string, echos []bool) ([]string, error) {
				out := make([]string, len(qs))
				for i := range qs {
					out[i] = code
				}
				return out, nil
			}),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
}

func (f *fixture) mkUser(t *testing.T, name string) {
	t.Helper()
	if _, err := f.st.CreateUser(t.Context(), name, store.RoleUser, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// The whole point: somebody with an account and a code can log in from a stock
// ssh client, having exchanged no key with the admin beforehand.
func TestEnrolRegistersTheOfferedKey(t *testing.T) {
	f := newFixture(t)
	f.mkUser(t, "bob")
	code, err := f.st.CreateEnrollCode(t.Context(), "bob", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := newKey(t)

	cl, err := f.enrollDial("bob", signer, code)
	if err != nil {
		t.Fatalf("enrollment failed: %v", err)
	}
	cl.Close()

	keys, err := f.st.UserKeys(t.Context(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("registered %d keys, want 1", len(keys))
	}
	if keys[0].Fingerprint != ssh.FingerprintSHA256(signer.PublicKey()) {
		t.Errorf("registered the wrong key: %s", keys[0].Fingerprint)
	}

	// And from now on, plain key auth with no code.
	cl2, err := f.dial("bob", signer)
	if err != nil {
		t.Fatalf("key auth after enrollment failed: %v", err)
	}
	cl2.Close()
}

// A code must buy exactly one key. Otherwise a code shared or leaked is a
// standing invitation rather than a single handover.
func TestEnrolCodeIsSingleUse(t *testing.T) {
	f := newFixture(t)
	f.mkUser(t, "bob")
	code, _ := f.st.CreateEnrollCode(t.Context(), "bob", time.Now())

	first, _ := newKey(t)
	cl, err := f.enrollDial("bob", first, code)
	if err != nil {
		t.Fatalf("first enrollment failed: %v", err)
	}
	cl.Close()

	second, _ := newKey(t)
	if cl, err := f.enrollDial("bob", second, code); err == nil {
		cl.Close()
		t.Fatal("the same code enrolled a second key")
	}
	if keys, _ := f.st.UserKeys(t.Context(), "bob"); len(keys) != 1 {
		t.Errorf("bob has %d keys, want 1", len(keys))
	}
}

func TestEnrolRejectsBadCode(t *testing.T) {
	f := newFixture(t)
	f.mkUser(t, "bob")
	signer, _ := newKey(t)
	if cl, err := f.enrollDial("bob", signer, "not-a-real-code"); err == nil {
		cl.Close()
		t.Fatal("an invented code was accepted")
	}
	if keys, _ := f.st.UserKeys(t.Context(), "bob"); len(keys) != 0 {
		t.Error("a failed enrollment registered a key anyway")
	}
}

// A code names one account. Without this check, a code for a low-privilege
// account would let its holder take a session as anybody they can name.
func TestEnrolCodeIsBoundToItsAccount(t *testing.T) {
	f := newFixture(t)
	f.mkUser(t, "bob")
	f.mkUser(t, "root-ish")
	code, _ := f.st.CreateEnrollCode(t.Context(), "bob", time.Now())

	signer, _ := newKey(t)
	if cl, err := f.enrollDial("root-ish", signer, code); err == nil {
		cl.Close()
		t.Fatal("bob's code logged in as another account")
	}
	if keys, _ := f.st.UserKeys(t.Context(), "root-ish"); len(keys) != 0 {
		t.Error("a key was registered against the wrong account")
	}
}

func TestEnrolCodeExpires(t *testing.T) {
	f := newFixture(t)
	f.mkUser(t, "bob")
	code, _ := f.st.CreateEnrollCode(t.Context(), "bob", time.Now())
	// Redeem in the future, past the TTL.
	if _, err := f.st.RedeemEnrollCode(t.Context(), code, time.Now().Add(store.EnrollTTL+time.Minute)); err == nil {
		t.Fatal("an expired code was redeemed")
	}
}

// Access must always rest on a key. A client with a code but no key gets a
// clear explanation, and the code is not consumed.
func TestEnrolWithoutAKeyIsRefused(t *testing.T) {
	f := newFixture(t)
	f.mkUser(t, "bob")
	code, _ := f.st.CreateEnrollCode(t.Context(), "bob", time.Now())

	var sawAdvice bool
	_, err := ssh.Dial("tcp", f.addr, &ssh.ClientConfig{
		User: "bob",
		Auth: []ssh.AuthMethod{
			ssh.KeyboardInteractive(func(name, instruction string, qs []string, echos []bool) ([]string, error) {
				if strings.Contains(instruction, "ssh-keygen") {
					sawAdvice = true
				}
				out := make([]string, len(qs))
				for i := range qs {
					out[i] = code
				}
				return out, nil
			}),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err == nil {
		t.Fatal("a session was granted to a client with no key")
	}
	if !sawAdvice {
		t.Error("the client was not told how to fix it")
	}
	// The code must survive, so a fixable mistake does not burn it.
	if _, err := f.st.RedeemEnrollCode(t.Context(), code, time.Now()); err != nil {
		t.Errorf("a keyless attempt consumed the code: %v", err)
	}
}

// A suspended account cannot be enrolled into, or quarantine would be
// reversible by anyone holding an old code.
func TestEnrolRefusedForSuspendedAccount(t *testing.T) {
	f := newFixture(t)
	f.mkUser(t, "bob")
	code, _ := f.st.CreateEnrollCode(t.Context(), "bob", time.Now())
	if err := f.st.SetUserDisabled(t.Context(), "bob", true); err != nil {
		t.Fatal(err)
	}
	signer, _ := newKey(t)
	if cl, err := f.enrollDial("bob", signer, code); err == nil {
		cl.Close()
		t.Fatal("a suspended account was enrolled")
	}
}

// Codes cannot be minted for an account that does not exist: the admin would
// hand out something that can never work.
func TestEnrolCodeNeedsAnAccount(t *testing.T) {
	f := newFixture(t)
	if _, err := f.st.CreateEnrollCode(t.Context(), "ghost", time.Now()); err == nil {
		t.Fatal("minted a code for a nonexistent account")
	}
}

// The map of offered keys is fed by unauthenticated connections, so it must
// not grow without bound.
func TestOfferedKeysExpire(t *testing.T) {
	o := newOfferedKeys()
	signer, _ := newKey(t)
	o.put("sess", signer.PublicKey())
	if _, ok := o.peek("sess"); !ok {
		t.Fatal("a fresh offer was not retrievable")
	}
	// Reading it does not consume it: ssh allows several attempts at the
	// code within one connection, and mistyping it once used to produce
	// "your client did not offer a key -- make one with ssh-keygen" on the
	// retry, which was both wrong and expensive advice.
	if _, ok := o.peek("sess"); !ok {
		t.Error("an offer was consumed by being read, so a retyped code cannot work")
	}
	o.forget("sess")
	if _, ok := o.peek("sess"); ok {
		t.Error("an offer survived being forgotten")
	}
	o.put("old", signer.PublicKey())
	o.mu.Lock()
	e := o.keys["old"]
	e.at = time.Now().Add(-2 * offerTTL)
	o.keys["old"] = e
	o.mu.Unlock()
	if _, ok := o.peek("old"); ok {
		t.Error("a stale offer was still accepted")
	}
}

// A person routinely has more than one computer. Adding the second must not
// need an admin: whoever is already authenticated as the account could
// register a key anyway, so a code they issue themselves grants nothing new.
func TestSecondMachineEnrolsAlongsideTheFirst(t *testing.T) {
	f := newFixture(t)
	laptop := f.addUser(t, "alice")

	// A code alice could have obtained from her existing session.
	code, err := f.st.CreateEnrollCode(t.Context(), "alice", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	desktop, _ := newKey(t)
	cl, err := f.enrollDial("alice", desktop, code)
	if err != nil {
		t.Fatalf("second machine could not enroll: %v", err)
	}
	cl.Close()

	// Both machines work, and enrolling one did not displace the other.
	for name, signer := range map[string]ssh.Signer{"laptop": laptop, "desktop": desktop} {
		c, err := f.dial("alice", signer)
		if err != nil {
			t.Errorf("%s cannot log in: %v", name, err)
			continue
		}
		c.Close()
	}
	keys, _ := f.st.UserKeys(t.Context(), "alice")
	if len(keys) != 2 {
		t.Errorf("alice has %d keys, want 2", len(keys))
	}
}

// Revoking one machine must not lock out the others.
func TestRevokingOneMachineLeavesOthers(t *testing.T) {
	f := newFixture(t)
	laptop := f.addUser(t, "alice")
	code, _ := f.st.CreateEnrollCode(t.Context(), "alice", time.Now())
	desktop, _ := newKey(t)
	if cl, err := f.enrollDial("alice", desktop, code); err != nil {
		t.Fatal(err)
	} else {
		cl.Close()
	}

	if err := f.st.DeleteUserKey(t.Context(), ssh.FingerprintSHA256(desktop.PublicKey()), time.Now()); err != nil {
		t.Fatal(err)
	}
	if cl, err := f.dial("alice", desktop); err == nil {
		cl.Close()
		t.Error("the revoked machine still logs in")
	}
	cl, err := f.dial("alice", laptop)
	if err != nil {
		t.Fatalf("revoking one machine locked out another: %v", err)
	}
	cl.Close()
}

// The code must not be echoed. On the public computer this feature exists for,
// a visible code sits in the scrollback of a terminal the next person opens.
func TestEnrolCodePromptIsHidden(t *testing.T) {
	f := newFixture(t)
	f.mkUser(t, "bob")
	code, _ := f.st.CreateEnrollCode(t.Context(), "bob", time.Now())
	signer, _ := newKey(t)

	var echoed []bool
	cl, err := ssh.Dial("tcp", f.addr, &ssh.ClientConfig{
		User: "bob",
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
			ssh.KeyboardInteractive(func(name, instruction string, qs []string, echos []bool) ([]string, error) {
				echoed = append(echoed, echos...)
				out := make([]string, len(qs))
				for i := range qs {
					out[i] = code
				}
				return out, nil
			}),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	cl.Close()
	if len(echoed) == 0 {
		t.Fatal("no prompt was issued")
	}
	for i, e := range echoed {
		if e {
			t.Errorf("prompt %d asks the client to echo the code", i)
		}
	}
}
