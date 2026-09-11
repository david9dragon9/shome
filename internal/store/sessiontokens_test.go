package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func tokStore(t *testing.T) (*Store, context.Context, time.Time) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	if _, err := s.CreateUser(ctx, "alice", RoleUser, 0, now); err != nil {
		t.Fatal(err)
	}
	return s, ctx, now
}

func TestSessionTokenResolvesToItsAccount(t *testing.T) {
	s, ctx, now := tokStore(t)
	tok, err := s.CreateSessionToken(ctx, "alice", "sess-1", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.UserBySessionToken(ctx, tok, now)
	if err != nil {
		t.Fatal(err)
	}
	if u.Name != "alice" || u.Role != RoleUser {
		t.Errorf("resolved to %+v, want alice as a plain user", u)
	}
	// It must not also work as an account token, or the two kinds would be
	// interchangeable and the distinction pointless.
	if _, err := s.UserByToken(ctx, tok); err == nil {
		t.Error("a session token was accepted as an account token")
	}
}

// Expiry is checked on read, not only by a sweep: a sweep that has not run
// must not leave a credential working.
func TestSessionTokenExpires(t *testing.T) {
	s, ctx, now := tokStore(t)
	tok, _ := s.CreateSessionToken(ctx, "alice", "sess-1", time.Hour, now)

	if _, err := s.UserBySessionToken(ctx, tok, now.Add(59*time.Minute)); err != nil {
		t.Errorf("rejected before expiry: %v", err)
	}
	if _, err := s.UserBySessionToken(ctx, tok, now.Add(time.Hour+time.Second)); err == nil {
		t.Error("an expired session token still works")
	}
	// Even though the row is still there.
	if n, _ := s.CountSessionTokens(ctx, now); n != 1 {
		t.Errorf("live count at issue time = %d, want 1", n)
	}
	if n, _ := s.CountSessionTokens(ctx, now.Add(2*time.Hour)); n != 0 {
		t.Errorf("live count after expiry = %d, want 0", n)
	}
}

// The reason this exists: signing out of a public computer must take effect
// now, not whenever the credential would have expired.
func TestRevokingAnAccountKillsItsSessionCredentials(t *testing.T) {
	s, ctx, now := tokStore(t)
	if _, err := s.CreateUser(ctx, "bob", RoleUser, 0, now); err != nil {
		t.Fatal(err)
	}
	a1, _ := s.CreateSessionToken(ctx, "alice", "sess-1", time.Hour, now)
	a2, _ := s.CreateSessionToken(ctx, "alice", "sess-2", time.Hour, now)
	b1, _ := s.CreateSessionToken(ctx, "bob", "sess-3", time.Hour, now)

	if err := s.DeleteSessionTokensFor(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	for i, tok := range []string{a1, a2} {
		if _, err := s.UserBySessionToken(ctx, tok, now); err == nil {
			t.Errorf("alice's token %d still works after revocation", i+1)
		}
	}
	if _, err := s.UserBySessionToken(ctx, b1, now); err != nil {
		t.Errorf("revoking alice broke bob's session: %v", err)
	}
}

func TestDeletingOneSessionLeavesTheOthers(t *testing.T) {
	s, ctx, now := tokStore(t)
	a1, _ := s.CreateSessionToken(ctx, "alice", "sess-1", time.Hour, now)
	a2, _ := s.CreateSessionToken(ctx, "alice", "sess-2", time.Hour, now)

	if err := s.DeleteSessionTokens(ctx, "sess-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserBySessionToken(ctx, a1, now); err == nil {
		t.Error("the closed session's token still works")
	}
	if _, err := s.UserBySessionToken(ctx, a2, now); err != nil {
		t.Errorf("closing one session broke another: %v", err)
	}
}

// An account disabled mid-session must stop working at once, which means the
// account is looked up freshly rather than cached alongside the token.
func TestDisablingAnAccountIsSeenByALiveToken(t *testing.T) {
	s, ctx, now := tokStore(t)
	tok, _ := s.CreateSessionToken(ctx, "alice", "sess-1", time.Hour, now)

	if err := s.SetUserDisabled(ctx, "alice", true); err != nil {
		t.Fatal(err)
	}
	u, err := s.UserBySessionToken(ctx, tok, now)
	if err != nil {
		return // refusing outright is also correct
	}
	if !u.Disabled {
		t.Error("a live session token reported a disabled account as enabled")
	}
}

func TestSweepRemovesOnlyExpired(t *testing.T) {
	s, ctx, now := tokStore(t)
	s.CreateSessionToken(ctx, "alice", "old", time.Minute, now)
	live, _ := s.CreateSessionToken(ctx, "alice", "new", 4*time.Hour, now)

	n, err := s.SweepSessionTokens(ctx, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("swept %d rows, want 1", n)
	}
	if _, err := s.UserBySessionToken(ctx, live, now.Add(time.Hour)); err != nil {
		t.Errorf("the sweep took a live token: %v", err)
	}
}

func TestSessionTokenRequiresItsFields(t *testing.T) {
	s, ctx, now := tokStore(t)
	for _, c := range []struct {
		user, sess string
		ttl        time.Duration
	}{
		{"", "s", time.Hour}, {"alice", "", time.Hour}, {"alice", "s", 0}, {"alice", "s", -time.Hour},
	} {
		if _, err := s.CreateSessionToken(ctx, c.user, c.sess, c.ttl, now); err == nil {
			t.Errorf("accepted user=%q session=%q ttl=%v", c.user, c.sess, c.ttl)
		}
	}
}

// An unknown token must not resolve, and must not be distinguishable from an
// expired one by its error type.
func TestUnknownSessionToken(t *testing.T) {
	s, ctx, now := tokStore(t)
	if _, err := s.UserBySessionToken(ctx, "shs-alice.nonsense", now); err == nil {
		t.Error("an invented session token was accepted")
	}
}
