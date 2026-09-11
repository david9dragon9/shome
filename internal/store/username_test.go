package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// An account's files live in a directory named after it, so the account name
// is a path component. The validation that keeps it one has no other
// enforcement behind it: nothing downstream re-checks, because everything
// downstream trusts that an account exists only if it was created here.
func TestAccountNamesThatWouldEscapeAPathAreRefused(t *testing.T) {
	st := newStore(t)
	for _, name := range []string{
		"", " ", ".", "..", "../boss", "a/b", `a\b`, "a\x00b",
		"alice bob", "alice.bob", "alice:bob", "alice*", "~root", "$HOME",
		"a%2fb", "a\nb", "a\tb", "café", strings.Repeat("a", 33),
	} {
		if _, err := st.CreateUser(t.Context(), name, RoleUser, 0, time.Now()); err == nil {
			t.Errorf("created an account named %q", name)
		}
	}
	// A bare "-" is a legal path component and is accepted. It is a poor name
	// -- every CLI reads it as a flag -- but that is a usability matter, and
	// this test is about what could escape a directory.
}

// And the names people actually use are accepted, or the validation would be
// a different bug.
func TestOrdinaryAccountNamesAreAccepted(t *testing.T) {
	st := newStore(t)
	for _, name := range []string{
		"alice", "Bob", "ops-team", "svc_runner", "u1", "a", strings.Repeat("a", 32),
	} {
		if _, err := st.CreateUser(t.Context(), name, RoleUser, 0, time.Now()); err != nil {
			t.Errorf("%q was refused: %v", name, err)
		}
	}
}

// A name is claimed once. Two accounts sharing one would share a storage
// directory, so the refusal is about files as much as about identity.
func TestAnAccountNameIsClaimedOnce(t *testing.T) {
	st := newStore(t)
	if _, err := st.CreateUser(t.Context(), "alice", RoleUser, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(t.Context(), "alice", RoleAdmin, 0, time.Now()); err == nil {
		t.Fatal("a second account took a name already in use")
	}
	// And the first account's role is untouched by the attempt.
	u, err := st.UserByName(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != RoleUser {
		t.Errorf("alice is now %s; a refused creation changed an existing account", u.Role)
	}
}

// Two accounts never share a token, and a token is long enough not to be
// guessed. It is the cluster's only credential for an account, so its entropy
// is the whole of that account's security.
func TestEveryAccountTokenIsUniqueAndLong(t *testing.T) {
	st := newStore(t)
	seen := map[string]bool{}
	for _, name := range []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8"} {
		tok, err := st.CreateUser(t.Context(), name, RoleUser, 0, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if seen[tok] {
			t.Fatalf("two accounts were issued the same token")
		}
		seen[tok] = true
		// Prefixed with the account name, then at least 32 random bytes in
		// base64 -- enough that guessing is not a strategy.
		if !strings.HasPrefix(tok, name+".") {
			t.Errorf("token %q is not scoped to its account", tok)
		}
		if secret := strings.TrimPrefix(tok, name+"."); len(secret) < 40 {
			t.Errorf("the random part of %q is only %d characters", tok, len(secret))
		}
	}
}

// The token is stored hashed, never in the clear. The database is a file on
// somebody's disk, and a readable one would hand over every account at once.
func TestTokensAreNotStoredInTheClear(t *testing.T) {
	st := newStore(t)
	tok, err := st.CreateUser(t.Context(), "alice", RoleUser, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := st.db.QueryRowContext(t.Context(),
		`SELECT token_hash FROM users WHERE name=?`, "alice").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == tok || strings.Contains(stored, strings.TrimPrefix(tok, "alice.")) {
		t.Error("the account's token is recoverable from the database")
	}
	if _, err := st.UserByToken(t.Context(), tok); err != nil {
		t.Fatalf("the token does not authenticate: %v", err)
	}
}

// Resetting a token invalidates the old one at once. A reset that left the
// previous token working would be a reset in name only, which is the whole
// reason to reach for one.
func TestResettingATokenRetiresThePreviousOne(t *testing.T) {
	st := newStore(t)
	old, err := st.CreateUser(t.Context(), "alice", RoleUser, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := st.ResetUserToken(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if fresh == old {
		t.Fatal("the reset returned the same token")
	}
	if _, err := st.UserByToken(t.Context(), old); err == nil {
		t.Error("the previous token still authenticates")
	}
	if _, err := st.UserByToken(t.Context(), fresh); err != nil {
		t.Errorf("the new token does not authenticate: %v", err)
	}
}

// Deleting an account retires its token too. Otherwise a deleted account's
// credential would keep working until something else happened to notice.
func TestDeletingAnAccountRetiresItsToken(t *testing.T) {
	st := newStore(t)
	tok, err := st.CreateUser(t.Context(), "alice", RoleUser, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteUser(t.Context(), "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UserByToken(t.Context(), tok); err == nil {
		t.Error("a deleted account's token still authenticates")
	}
}

// Every account lookup distinguishes "suspended" from "not a credential",
// because the HTTP layer answers them differently: a genuine credential whose
// account is suspended is a 403 with a reason, and anything else is a 401.
// A lookup that stopped saying which would silently merge the two again.
func TestEveryLookupSaysWhenAnAccountIsMerelySuspended(t *testing.T) {
	st := newStore(t)
	ctx := t.Context()
	tok, err := st.CreateUser(ctx, "alice", RoleUser, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddUserKey(ctx, "alice", "SHA256:fp", "ssh-ed25519 AAAA", "alice@laptop", time.Now()); err != nil {
		t.Fatal(err)
	}
	sess, err := st.CreateSessionToken(ctx, "alice", "s1", time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetUserDisabled(ctx, "alice", true); err != nil {
		t.Fatal(err)
	}

	for name, lookup := range map[string]func() error{
		"UserByToken":          func() error { _, e := st.UserByToken(ctx, tok); return e },
		"UserByName":           func() error { _, e := st.UserByName(ctx, "alice"); return e },
		"UserBySessionToken":   func() error { _, e := st.UserBySessionToken(ctx, sess, time.Now()); return e },
		"UserByKeyFingerprint": func() error { _, e := st.UserByKeyFingerprint(ctx, "SHA256:fp"); return e },
	} {
		err := lookup()
		if err == nil {
			t.Errorf("%s admitted a suspended account", name)
			continue
		}
		if !errors.Is(err, ErrAccountDisabled) {
			t.Errorf("%s refused with %v, which does not match ErrAccountDisabled -- "+
				"the caller cannot tell this from an unknown credential", name, err)
		}
	}

	// And a credential that is genuinely unknown must NOT match it, or the
	// distinction would be worthless in the other direction.
	for name, lookup := range map[string]func() error{
		"UserByToken":          func() error { _, e := st.UserByToken(ctx, "nope"); return e },
		"UserByName":           func() error { _, e := st.UserByName(ctx, "nobody"); return e },
		"UserBySessionToken":   func() error { _, e := st.UserBySessionToken(ctx, "nope", time.Now()); return e },
		"UserByKeyFingerprint": func() error { _, e := st.UserByKeyFingerprint(ctx, "SHA256:nope"); return e },
	} {
		err := lookup()
		if err == nil {
			t.Errorf("%s admitted an unknown credential", name)
			continue
		}
		if errors.Is(err, ErrAccountDisabled) {
			t.Errorf("%s reported an unknown credential as a suspended account: %v", name, err)
		}
	}

	// An expired session credential is not a suspended account either: the
	// account may be in perfectly good standing and the token simply old.
	if err := st.SetUserDisabled(ctx, "alice", false); err != nil {
		t.Fatal(err)
	}
	old, err := st.CreateSessionToken(ctx, "alice", "s2", time.Minute, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UserBySessionToken(ctx, old, time.Now()); err == nil {
		t.Error("an expired session credential was accepted")
	} else if errors.Is(err, ErrAccountDisabled) {
		t.Errorf("an expired credential was reported as a suspended account: %v", err)
	}

	// The suspension lifted, the account's own token works again.
	if _, err := st.UserByToken(ctx, tok); err != nil {
		t.Errorf("lifting the suspension left the token refused: %v", err)
	}
}
