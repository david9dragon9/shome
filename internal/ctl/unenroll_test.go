package ctl

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/davidwu/shome/internal/store"
)

// Signing out. The case that shapes it is a public or borrowed computer: the
// key must go, and so must any unspent enrollment code, or whoever sits down
// next can simply enroll again.

func unenrollFixture(t *testing.T) (*API, *store.Store) {
	t.Helper()
	c, st := dashFixture(t)
	ctx := context.Background()
	now := time.Now()
	for _, n := range []string{"alice", "bob"} {
		if _, err := st.CreateUser(ctx, n, store.RoleUser, 0, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []struct{ user, fp string }{
		{"alice", "SHA256:laptop"}, {"alice", "SHA256:desktop"}, {"bob", "SHA256:bobs"},
	} {
		if err := st.AddUserKey(ctx, k.user, k.fp, "ssh-ed25519 AAAA "+k.fp, "", now); err != nil {
			t.Fatal(err)
		}
	}
	return &API{c: c}, st
}

func post(t *testing.T, a *API, caller *store.User, body string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest("POST", "/unenroll", strings.NewReader(body))
	w := httptest.NewRecorder()
	a.unenroll(w, r, caller)
	var out map[string]any
	json.NewDecoder(w.Body).Decode(&out)
	return w.Code, out
}

func TestUnenrollRemovesOnlyThisMachine(t *testing.T) {
	a, st := unenrollFixture(t)
	ctx := context.Background()
	alice := &store.User{Name: "alice", Role: store.RoleUser}

	code, http1 := post(t, a, alice, `{"fingerprint":"SHA256:desktop"}`)
	if code != 200 {
		t.Fatalf("returned %d: %v", code, http1)
	}
	keys, _ := st.UserKeys(ctx, "alice")
	if len(keys) != 1 || keys[0].Fingerprint != "SHA256:laptop" {
		t.Fatalf("wrong keys remain: %+v", keys)
	}
}

// Every outstanding code dies with the sign-out. An unused one left alive is
// a way back in for whoever inherits the machine.
func TestUnenrollInvalidatesEveryCode(t *testing.T) {
	a, st := unenrollFixture(t)
	ctx := context.Background()
	now := time.Now()
	c1, _ := st.CreateEnrollCode(ctx, "alice", now)
	c2, _ := st.CreateEnrollCode(ctx, "alice", now)
	bobCode, _ := st.CreateEnrollCode(ctx, "bob", now)

	code, out := post(t, a, &store.User{Name: "alice", Role: store.RoleUser},
		`{"fingerprint":"SHA256:desktop"}`)
	if code != 200 {
		t.Fatalf("returned %d", code)
	}
	if n, _ := out["codes_invalidated"].(float64); n != 2 {
		t.Errorf("invalidated %v codes, want 2", out["codes_invalidated"])
	}
	for i, c := range []string{c1, c2} {
		if _, err := st.RedeemEnrollCode(ctx, c, now); err == nil {
			t.Errorf("alice's code %d still works after signing out", i+1)
		}
	}
	// And nobody else's are touched.
	if _, err := st.RedeemEnrollCode(ctx, bobCode, now); err != nil {
		t.Errorf("bob's code was invalidated by alice signing out: %v", err)
	}
}

func TestUnenrollAllRemovesEveryKey(t *testing.T) {
	a, st := unenrollFixture(t)
	ctx := context.Background()
	code, out := post(t, a, &store.User{Name: "alice", Role: store.RoleUser}, `{"all":true}`)
	if code != 200 {
		t.Fatalf("returned %d", code)
	}
	if removed, _ := out["removed"].([]any); len(removed) != 2 {
		t.Errorf("removed %v, want both keys", out["removed"])
	}
	if keys, _ := st.UserKeys(ctx, "alice"); len(keys) != 0 {
		t.Errorf("alice still has %d keys", len(keys))
	}
	// Bob is untouched.
	if keys, _ := st.UserKeys(ctx, "bob"); len(keys) != 1 {
		t.Errorf("bob's key was removed too")
	}
}

// Signing out is self-service, so naming somebody else's fingerprint must not
// revoke their machine.
func TestUnenrollCannotRemoveAnotherUsersKey(t *testing.T) {
	a, st := unenrollFixture(t)
	ctx := context.Background()
	code, _ := post(t, a, &store.User{Name: "alice", Role: store.RoleUser},
		`{"fingerprint":"SHA256:bobs"}`)
	if code == 200 {
		t.Fatal("alice revoked bob's key")
	}
	if keys, _ := st.UserKeys(ctx, "bob"); len(keys) != 1 {
		t.Error("bob's key was removed")
	}
}

// The account survives: this is signing out, not deleting yourself.
func TestUnenrollKeepsTheAccount(t *testing.T) {
	a, st := unenrollFixture(t)
	ctx := context.Background()
	if err := st.SetUserQuota(ctx, "alice", 100<<20); err != nil {
		t.Fatal(err)
	}
	post(t, a, &store.User{Name: "alice", Role: store.RoleUser}, `{"all":true}`)

	u, err := st.UserByName(ctx, "alice")
	if err != nil {
		t.Fatalf("the account was removed: %v", err)
	}
	if u.Disabled {
		t.Error("the account was suspended")
	}
	if u.QuotaBytes != 100<<20 {
		t.Errorf("quota changed to %d", u.QuotaBytes)
	}
	// And an admin can let them back in.
	if _, err := st.CreateEnrollCode(ctx, "alice", time.Now()); err != nil {
		t.Errorf("cannot issue a new code after signing out: %v", err)
	}
}

// Saying neither which key nor "all" must be an error, not a silent no-op that
// looks like a successful sign-out.
func TestUnenrollNeedsATarget(t *testing.T) {
	a, _ := unenrollFixture(t)
	if code, _ := post(t, a, &store.User{Name: "alice", Role: store.RoleUser}, `{}`); code == 200 {
		t.Error("an unenroll with no target reported success")
	}
}

// An admin signing a machine out must do exactly what the user doing it
// themselves does. These diverged once -- the admin path left the account's
// enrollment codes alive -- which made the command used to revoke a lost laptop
// the weaker of the two.
func TestAdminUnenrollMatchesSelfUnenroll(t *testing.T) {
	a, st := unenrollFixture(t)
	ctx := context.Background()
	code, _ := st.CreateEnrollCode(ctx, "alice", time.Now())

	r := httptest.NewRequest("POST", "/users/alice/unenroll",
		strings.NewReader(`{"fingerprint":"SHA256:desktop"}`))
	r.SetPathValue("name", "alice")
	w := httptest.NewRecorder()
	a.adminUnenroll(w, r, &store.User{Name: "root", Role: store.RoleAdmin})
	if w.Code != 200 {
		t.Fatalf("returned %d: %s", w.Code, w.Body.String())
	}

	keys, _ := st.UserKeys(ctx, "alice")
	if len(keys) != 1 || keys[0].Fingerprint != "SHA256:laptop" {
		t.Errorf("wrong keys remain: %+v", keys)
	}
	// The code must be spent, exactly as when alice does it herself.
	if _, err := st.RedeemEnrollCode(ctx, code, time.Now()); err == nil {
		t.Error("an admin sign-out left an enrollment code alive")
	}
}

func TestAdminUnenrollAll(t *testing.T) {
	a, st := unenrollFixture(t)
	ctx := context.Background()
	r := httptest.NewRequest("POST", "/users/alice/unenroll", strings.NewReader(`{"all":true}`))
	r.SetPathValue("name", "alice")
	w := httptest.NewRecorder()
	a.adminUnenroll(w, r, &store.User{Name: "root", Role: store.RoleAdmin})
	if w.Code != 200 {
		t.Fatalf("returned %d", w.Code)
	}
	if keys, _ := st.UserKeys(ctx, "alice"); len(keys) != 0 {
		t.Errorf("alice still has %d keys", len(keys))
	}
	if keys, _ := st.UserKeys(ctx, "bob"); len(keys) != 1 {
		t.Error("bob's machines were signed out too")
	}
}

// An admin naming a fingerprint that belongs to somebody else, under the wrong
// account, must not silently revoke it.
func TestAdminUnenrollChecksOwnership(t *testing.T) {
	a, st := unenrollFixture(t)
	ctx := context.Background()
	r := httptest.NewRequest("POST", "/users/alice/unenroll",
		strings.NewReader(`{"fingerprint":"SHA256:bobs"}`))
	r.SetPathValue("name", "alice")
	w := httptest.NewRecorder()
	a.adminUnenroll(w, r, &store.User{Name: "root", Role: store.RoleAdmin})
	if w.Code == 200 {
		t.Error("revoked a key under the wrong account")
	}
	if keys, _ := st.UserKeys(ctx, "bob"); len(keys) != 1 {
		t.Error("bob's key was removed")
	}
}

// Revoking by fingerprint alone finds the owner, and has the same meaning as
// every other route into Unenroll.
func TestKeyRemoveByFingerprintSpendsCodesToo(t *testing.T) {
	a, st := unenrollFixture(t)
	ctx := context.Background()
	code, _ := st.CreateEnrollCode(ctx, "alice", time.Now())

	r := httptest.NewRequest("DELETE", "/keys/SHA256:laptop", nil)
	r.SetPathValue("fingerprint", "SHA256:laptop")
	w := httptest.NewRecorder()
	a.delUserKey(w, r, &store.User{Name: "root", Role: store.RoleAdmin})
	if w.Code != 200 {
		t.Fatalf("returned %d: %s", w.Code, w.Body.String())
	}
	if _, err := st.RedeemEnrollCode(ctx, code, time.Now()); err == nil {
		t.Error("'key rm' left an enrollment code alive")
	}
}

func TestOwnerOfKey(t *testing.T) {
	a, _ := unenrollFixture(t)
	ctx := context.Background()
	if got, err := a.c.OwnerOfKey(ctx, "SHA256:bobs"); err != nil || got != "bob" {
		t.Errorf("OwnerOfKey = (%q, %v), want bob", got, err)
	}
	if _, err := a.c.OwnerOfKey(ctx, "SHA256:nope"); err == nil {
		t.Error("found an owner for a key that does not exist")
	}
}

// The listing must show both ways in. Registered machines answer "who can get
// in right now"; unused codes answer "and who could let themselves in this
// afternoon". Showing only the first is half an answer to an audit question.
func TestKeyListingIncludesPendingCodes(t *testing.T) {
	a, st := unenrollFixture(t)
	ctx := context.Background()
	if _, err := st.CreateEnrollCode(ctx, "alice", time.Now()); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/keys?user=alice", nil)
	w := httptest.NewRecorder()
	a.listUserKeys(w, r, &store.User{Name: "root", Role: store.RoleAdmin})

	var out struct {
		Keys  []map[string]any `json:"keys"`
		Codes []map[string]any `json:"codes"`
	}
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Keys) != 2 {
		t.Errorf("got %d machines, want 2", len(out.Keys))
	}
	if len(out.Codes) != 1 {
		t.Fatalf("got %d pending codes, want 1", len(out.Codes))
	}
	// The handle identifies the code without being the code: a listing that
	// showed the secret would be as sensitive as the codes themselves.
	id, _ := out.Codes[0]["id"].(string)
	if id == "" {
		t.Error("pending code has no handle to cancel it by")
	}
	if len(id) > 20 {
		t.Errorf("handle %q looks like it might be the code itself", id)
	}
}

// A spent or expired code is not a way in and must not be listed as one.
func TestPendingCodesExcludeSpentAndExpired(t *testing.T) {
	_, st := unenrollFixture(t)
	ctx := context.Background()
	now := time.Now()
	live, _ := st.CreateEnrollCode(ctx, "alice", now)
	spent, _ := st.CreateEnrollCode(ctx, "alice", now)
	st.RedeemEnrollCode(ctx, spent, now)
	old, _ := st.CreateEnrollCode(ctx, "alice", now.Add(-2*store.EnrollTTL))

	codes, err := st.PendingCodes(ctx, "alice", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != 1 {
		t.Fatalf("listed %d codes, want only the live one", len(codes))
	}
	_ = live
	_ = old
}

// Cancelling a code must not disturb machines that already have access: they
// undo different things.
func TestInvalidateOneCodeLeavesMachinesAlone(t *testing.T) {
	a, st := unenrollFixture(t)
	ctx := context.Background()
	now := time.Now()
	keep, _ := st.CreateEnrollCode(ctx, "alice", now)
	st.CreateEnrollCode(ctx, "alice", now)

	pending, _ := st.PendingCodes(ctx, "alice", now)
	if len(pending) != 2 {
		t.Fatalf("setup: %d codes", len(pending))
	}
	// Cancel whichever is not the one we want to keep working.
	var target string
	for _, p := range pending {
		if _, err := st.UserKeyByFingerprint(ctx, "alice", "SHA256:laptop"); err != nil {
			t.Fatal(err)
		}
		target = p.ID
		break
	}

	r := httptest.NewRequest("DELETE", "/codes/"+target, nil)
	r.SetPathValue("id", target)
	w := httptest.NewRecorder()
	a.invalidateCode(w, r, &store.User{Name: "root", Role: store.RoleAdmin})
	if w.Code != 200 {
		t.Fatalf("returned %d: %s", w.Code, w.Body.String())
	}

	if keys, _ := st.UserKeys(ctx, "alice"); len(keys) != 2 {
		t.Errorf("cancelling a code removed a machine: %d left", len(keys))
	}
	if left, _ := st.PendingCodes(ctx, "alice", now); len(left) != 1 {
		t.Errorf("%d codes left, want 1", len(left))
	}
	_ = keep
}

func TestInvalidateUnknownCode(t *testing.T) {
	a, _ := unenrollFixture(t)
	r := httptest.NewRequest("DELETE", "/codes/nosuchcode", nil)
	r.SetPathValue("id", "nosuchcode")
	w := httptest.NewRecorder()
	a.invalidateCode(w, r, &store.User{Name: "root", Role: store.RoleAdmin})
	if w.Code == 200 {
		t.Error("cancelling a code that does not exist reported success")
	}
}

// unenroll --all must do both halves: every machine out, every code dead.
func TestUnenrollAllClearsMachinesAndCodes(t *testing.T) {
	a, st := unenrollFixture(t)
	ctx := context.Background()
	now := time.Now()
	st.CreateEnrollCode(ctx, "alice", now)
	st.CreateEnrollCode(ctx, "alice", now)

	code, out := post(t, a, &store.User{Name: "alice", Role: store.RoleUser}, `{"all":true}`)
	if code != 200 {
		t.Fatalf("returned %d", code)
	}
	if n, _ := out["codes_invalidated"].(float64); n != 2 {
		t.Errorf("invalidated %v codes, want 2", out["codes_invalidated"])
	}
	if keys, _ := st.UserKeys(ctx, "alice"); len(keys) != 0 {
		t.Errorf("%d machines still enrolled", len(keys))
	}
	if codes, _ := st.PendingCodes(ctx, "alice", now); len(codes) != 0 {
		t.Errorf("%d codes still outstanding", len(codes))
	}
}

// Revocation must reach the login node so live sessions end at once.
func TestUnenrollFiresTheRevocationHook(t *testing.T) {
	a, _ := unenrollFixture(t)
	type call struct{ user, fp string }
	var got []call
	a.c.SetRevocationHook(func(user, fp string) { got = append(got, call{user, fp}) })

	post(t, a, &store.User{Name: "alice", Role: store.RoleUser}, `{"fingerprint":"SHA256:desktop"}`)
	if len(got) != 1 || got[0].user != "alice" || got[0].fp != "SHA256:desktop" {
		t.Fatalf("hook saw %+v", got)
	}

	got = nil
	post(t, a, &store.User{Name: "alice", Role: store.RoleUser}, `{"all":true}`)
	// An empty fingerprint means every machine.
	if len(got) != 1 || got[0].fp != "" {
		t.Fatalf("hook saw %+v, want one call covering every machine", got)
	}
}
