package loginnode

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// First-connection enrollment.
//
// The problem this solves: requiring an admin to collect somebody's public key
// before they can log in is a step that does not exist in ordinary ssh use,
// and it has to happen over some other channel before anything works. Here the
// admin hands over a one-time code, and the person's first `ssh` registers
// their own key:
//
//	$ ssh -p 2222 alice@cluster
//	Enrollment code: ....
//	Registered SHA256:... for alice.
//	alice>
//
// The mechanics rely on the order a client tries authentication methods: it
// offers its public keys first, and only falls back to keyboard-interactive
// when those are refused. So by the time the code arrives, the key it should
// authorise is already known -- recorded during the failed public-key attempt
// and matched up by SSH session id, which is fixed for the life of a
// connection.
//
// What a code can do is deliberately narrow: register one key for one named
// account, once, within the hour. It is not a credential for anything else,
// which is why it is not the account's API token.

// offeredKeys remembers the public key a connection presented but that no
// account recognised, so enrollment can authorise the right one.
type offeredKeys struct {
	mu   sync.Mutex
	keys map[string]offered
}

type offered struct {
	key ssh.PublicKey
	at  time.Time
}

// offerTTL bounds how long an unmatched key is remembered. This map is fed by
// unauthenticated connections, so it must not be a place to accumulate memory.
const offerTTL = 2 * time.Minute

func newOfferedKeys() *offeredKeys { return &offeredKeys{keys: map[string]offered{}} }

func (o *offeredKeys) put(sessionID string, key ssh.PublicKey) {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := time.Now()
	for k, v := range o.keys {
		if now.Sub(v.at) > offerTTL {
			delete(o.keys, k)
		}
	}
	// Cheap ceiling in case something is hammering the port faster than
	// entries expire.
	if len(o.keys) > 4*MaxSessions {
		return
	}
	o.keys[sessionID] = offered{key: key, at: now}
}

// peek returns the key a connection offered, leaving it remembered.
//
// Left rather than taken, because ssh allows several attempts at
// keyboard-interactive within one connection: consuming it on the first
// attempt meant that mistyping the code once produced "your client did not
// offer one -- create one with ssh-keygen" on the retry. The key was fine,
// the advice was wrong, and it sent people off to make a second key they did
// not need.
func (o *offeredKeys) peek(sessionID string) (ssh.PublicKey, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	v, ok := o.keys[sessionID]
	if !ok || time.Since(v.at) > offerTTL {
		return nil, false
	}
	return v.key, true
}

// forget drops a remembered key, once it has been registered or the
// connection is over with it.
func (o *offeredKeys) forget(sessionID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.keys, sessionID)
}

func (s *Server) rememberOffered(conn ssh.ConnMetadata, key ssh.PublicKey) {
	if s.offered == nil {
		return
	}
	s.offered.put(sessionKey(conn), key)
}

// sessionKey identifies a connection across its authentication attempts. The
// SSH session id is fixed after key exchange, so it ties the key a client
// offered to the code it later types.
func sessionKey(conn ssh.ConnMetadata) string {
	return base64.RawStdEncoding.EncodeToString(conn.SessionID())
}

// authEnroll handles the keyboard-interactive fallback: prompt for a code,
// register the key the client already offered, and let them in.
func (s *Server) authEnroll(conn ssh.ConnMetadata, challenge ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	requested := conn.User()
	key, haveKey := s.offered.peek(sessionKey(conn))
	if !haveKey {
		// Nothing to authorise. Refusing here rather than after the code is
		// spent means a mistyped setup does not burn a single-use code, and
		// keeps the invariant that access always rests on a key.
		challenge(requested, "This cluster authenticates with SSH keys, and your client did not\r\n"+
			"offer one. Create one and try again:\r\n\r\n    ssh-keygen -t ed25519\r\n\r\n",
			nil, nil)
		return nil, fmt.Errorf("client offered no public key to enroll")
	}

	answers, err := challenge(requested,
		"No key of yours is registered with this cluster yet.\r\n"+
			"Enter the enrollment code your administrator gave you.\r\n\r\n",
		// echo off. A code typed in the clear is readable over a shoulder, and
		// on the public computer this feature exists for it would be left
		// sitting in the scrollback of a terminal the next person opens.
		[]string{"Enrollment code: "}, []bool{false})
	if err != nil {
		return nil, err
	}
	if len(answers) != 1 {
		return nil, fmt.Errorf("no code given")
	}
	code := strings.TrimSpace(answers[0])
	if code == "" {
		return nil, fmt.Errorf("no code given")
	}

	user, err := s.cfg.Store.RedeemEnrollCode(ctx, code, time.Now())
	if err != nil {
		s.cfg.Log.Warn("enrollment refused", "user", requested,
			"remote", conn.RemoteAddr().String(), "err", err)
		return nil, err
	}
	// The code names the account. Logging in under a different name would let
	// somebody with a code for one account take a session as another.
	if user != requested {
		return nil, fmt.Errorf("that code is for a different account")
	}

	fp := ssh.FingerprintSHA256(key)
	pub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	if err := s.cfg.Store.AddUserKey(ctx, user, fp, pub, "enrolled over ssh", time.Now()); err != nil {
		return nil, fmt.Errorf("could not register your key: %w", err)
	}
	// Registered, so it need not be remembered any longer.
	s.offered.forget(sessionKey(conn))
	challenge(requested, fmt.Sprintf(
		"\r\nRegistered %s for %s.\r\nFuture logins will use this key directly.\r\n\r\n",
		fp, user), nil, nil)

	u, err := s.cfg.Store.UserByName(ctx, user)
	if err != nil {
		return nil, err
	}
	s.cfg.Log.Info("account enrolled", "user", user, "fingerprint", fp,
		"remote", conn.RemoteAddr().String())
	return permissionsFor(u, fp), nil
}
