package ctl

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/store"
)

// Impersonation is the sharpest edge in the control plane: one header turns a
// request into somebody else's. It exists because the login shell runs on the
// controller host as the machine's own account and has to act for whichever
// principal SSH authenticated -- so the credential presented is never the
// user's own.
//
// That makes it a privilege-escalation path by construction, and everything
// below is a property that keeps it from being one in practice.

const actAsHeader = "X-Shome-Act-As"

func actAsFixture(t *testing.T) (*API, *httptest.Server, map[string]string) {
	t.Helper()
	c, st := dashFixture(t)
	ctx := context.Background()
	tokens := map[string]string{}
	for name, role := range map[string]store.Role{
		"boss": store.RoleAdmin, "ops": store.RoleOperator,
		"alice": store.RoleUser, "bob": store.RoleUser,
	} {
		tok, err := st.CreateUser(ctx, name, role, 0, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		tokens[name] = tok
	}
	api := &API{c: c}
	srv := httptest.NewServer(api.routes())
	t.Cleanup(srv.Close)
	return api, srv, tokens
}

// whoAmI asks the API who it thinks is calling, which is the identity every
// other handler acts on.
func whoAmI(t *testing.T, srv *httptest.Server, token, actAs string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("GET", srv.URL+"/whoami", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if actAs != "" {
		req.Header.Set(actAsHeader, actAs)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		User string `json:"user"`
		Role string `json:"role"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body.User
}

// The header is admin-only. If any account could set it, the whole identity
// model would be advisory: submitting work as somebody else, reading their
// jobs and spending their quota would all be one header away.
func TestOnlyAnAdminMayActAsAnotherAccount(t *testing.T) {
	_, srv, tokens := actAsFixture(t)
	for _, caller := range []string{"alice", "ops"} {
		code, who := whoAmI(t, srv, tokens[caller], "bob")
		if code != http.StatusForbidden {
			t.Errorf("%s acting as bob returned %d (as %q), want 403", caller, code, who)
		}
	}
	code, who := whoAmI(t, srv, tokens["boss"], "bob")
	if code != http.StatusOK || who != "bob" {
		t.Errorf("an admin acting as bob returned %d as %q, want 200 as bob", code, who)
	}
}

// An operator is explicitly not enough. Operators exist so that day-to-day
// cluster babysitting does not need the credential that can mint accounts,
// and impersonation would hand back exactly that: acting as an admin account
// is acting as an admin.
func TestAnOperatorCannotActAsAnAdmin(t *testing.T) {
	_, srv, tokens := actAsFixture(t)
	if code, who := whoAmI(t, srv, tokens["ops"], "boss"); code != http.StatusForbidden {
		t.Errorf("an operator acting as an admin returned %d as %q, want 403", code, who)
	}
}

// Nor may a plain account use the header to escalate to an admin, which is the
// attack the role check is actually guarding.
func TestAPlainAccountCannotActAsAnAdmin(t *testing.T) {
	_, srv, tokens := actAsFixture(t)
	if code, who := whoAmI(t, srv, tokens["alice"], "boss"); code != http.StatusForbidden {
		t.Errorf("alice acting as an admin returned %d as %q, want 403", code, who)
	}
}

// Acting as somebody does not keep the admin's own rank. The point is to
// become that account, so the request must be refused everything that account
// would be refused -- otherwise a login session, which reaches the API this
// way, would be administering the cluster.
func TestActingAsAPlainAccountDropsAdminRank(t *testing.T) {
	_, srv, tokens := actAsFixture(t)
	req, err := http.NewRequest("GET", srv.URL+"/users", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tokens["boss"])
	req.Header.Set(actAsHeader, "alice")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET /users while acting as a plain account returned %d, want 403 -- "+
			"impersonation must not carry the admin's own rank", resp.StatusCode)
	}
}

// A suspended account cannot be acted as. Quarantine has to hold against a
// session that was opened before it, which is precisely the session somebody
// reaches for quarantine about; checking only at login would leave it running.
func TestCannotActAsASuspendedAccount(t *testing.T) {
	api, srv, tokens := actAsFixture(t)
	ctx := context.Background()
	if code, _ := whoAmI(t, srv, tokens["boss"], "alice"); code != http.StatusOK {
		t.Fatalf("setup: acting as alice was refused (%d)", code)
	}
	if err := api.c.Store().SetUserDisabled(ctx, "alice", true); err != nil {
		t.Fatal(err)
	}
	// 403 and not 400: a suspended account is a decision somebody made and can
	// lift, where a name that resolves to nothing is a typo. Reporting the
	// first as a bad request sends an admin looking for a misspelling.
	code, who := whoAmI(t, srv, tokens["boss"], "alice")
	if code != http.StatusForbidden {
		t.Errorf("acting as suspended account alice returned %d as %q, want 403", code, who)
	}
}

// An unknown target is refused rather than silently ignored. Falling back to
// the admin's own identity would turn a typo into work submitted, and billed,
// to the wrong account.
func TestActingAsAnUnknownAccountIsRefused(t *testing.T) {
	_, srv, tokens := actAsFixture(t)
	code, who := whoAmI(t, srv, tokens["boss"], "nobody-here")
	if code != http.StatusBadRequest {
		t.Errorf("acting as a nonexistent account returned %d as %q, want 400 -- "+
			"and never 200", code, who)
	}
}

// Whitespace and empty values mean "no impersonation", not "impersonate the
// account whose name is the empty string". A header trimmed to nothing has to
// leave the caller as themselves.
func TestABlankActAsHeaderIsNotImpersonation(t *testing.T) {
	_, srv, tokens := actAsFixture(t)
	// A newline is not a legal header value at all and the client refuses to
	// send it, which is its own kind of refusal; the values here are the ones
	// that do reach the server.
	for _, v := range []string{"", " ", "\t", "  "} {
		code, who := whoAmI(t, srv, tokens["boss"], v)
		if code != http.StatusOK || who != "boss" {
			t.Errorf("act-as %q returned %d as %q, want 200 as boss", v, code, who)
		}
	}
}

// A name that is not a name is refused, not resolved. Account names are path
// components -- an account's files live in a directory named after it -- so a
// value like "../boss" must not reach a lookup that might normalise it.
func TestActAsRefusesNamesThatAreNotAccounts(t *testing.T) {
	_, srv, tokens := actAsFixture(t)
	for _, v := range []string{"../boss", "boss/", "alice bob", "alice\x00", "%2e%2e/boss", "*"} {
		req, err := http.NewRequest("GET", srv.URL+"/whoami", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tokens["boss"])
		// Set directly: a value with a control character is not valid in a
		// header and http.Header.Set would panic on some of these, so the
		// map is written to and the server left to reject what arrives.
		req.Header[actAsHeader] = []string{v}
		resp, err := srv.Client().Do(req)
		if err != nil {
			// A malformed header the client itself refuses to send is also a
			// refusal, and a fine outcome.
			continue
		}
		var body struct {
			User string `json:"user"`
		}
		json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("act-as %q was accepted, answering as %q", v, body.User)
		}
	}
}

// Identity comes from the header, not the body, and the two disagreeing must
// resolve to the header. Submitting work is the case that matters: a job has
// to be recorded against, and charged to, the account it was run for.
func TestWorkSubmittedWhileActingAsSomebodyBelongsToThem(t *testing.T) {
	_, srv, tokens := actAsFixture(t)
	// The body names boss as the owner, which is the claim that must lose.
	body, err := json.Marshal(SubmitRequest{Spec: job.Spec{
		Name: "j", User: "boss", Script: "/bin/true",
		ArrayTaskID: -1, Limits: job.Limits{CPUs: 1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", srv.URL+"/submit", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tokens["boss"])
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(actAsHeader, "alice")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit while acting as alice: %s", resp.Status)
	}
	var v JobView
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if v.User != "alice" {
		t.Errorf("job recorded against %q; it was submitted for alice, and the "+
			"body claiming otherwise must not win", v.User)
	}
}

// Every use is recorded. Impersonation is the one operation whose audit entry
// cannot be reconstructed from anything else -- the request looks exactly like
// the target's own -- so it is logged where the admin who holds the token can
// be identified afterwards.
func TestImpersonationIsLogged(t *testing.T) {
	c, st := dashFixture(t)
	ctx := context.Background()
	for name, role := range map[string]store.Role{
		"boss": store.RoleAdmin, "alice": store.RoleUser,
	} {
		if _, err := st.CreateUser(ctx, name, role, 0, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	var logged strings.Builder
	c.log = slog.New(slog.NewTextHandler(&logged, nil))

	api := &API{c: c}
	srv := httptest.NewServer(api.routes())
	defer srv.Close()

	admin, err := st.ResetUserToken(ctx, "boss")
	if err != nil {
		t.Fatal(err)
	}
	if code, who := whoAmI(t, srv, admin, "alice"); code != http.StatusOK || who != "alice" {
		t.Fatalf("acting as alice returned %d as %q", code, who)
	}
	out := logged.String()
	for _, want := range []string{"acting as", "boss", "alice"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log does not mention %q:\n%s", want, out)
		}
	}
}
