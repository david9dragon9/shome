package ctl

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/davidwu/shome/internal/store"
)

// These tests assert over the *whole* route table rather than a chosen
// handful, because "every route is authenticated" is the kind of claim that
// stays true until somebody adds the ninety-first route. A sample cannot
// catch that; an exhaustive sweep catches it the moment it is written.

// placeholder matches a wildcard segment in a ServeMux pattern: {id},
// {name}, {fingerprint...}.
var placeholder = regexp.MustCompile(`\{[^}]*\}`)

// guardFixture builds a controller with one account of each role, plus a
// request-issuing helper.
func guardFixture(t *testing.T) (*API, *httptest.Server, map[store.Role]string) {
	t.Helper()
	c, st := dashFixture(t)
	ctx := context.Background()
	tokens := map[store.Role]string{}
	for name, role := range map[string]store.Role{
		"boss": store.RoleAdmin, "ops": store.RoleOperator, "plain": store.RoleUser,
	} {
		tok, err := st.CreateUser(ctx, name, role, 0, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		tokens[role] = tok
	}
	api := &API{c: c}
	srv := httptest.NewServer(api.routes())
	t.Cleanup(srv.Close)
	return api, srv, tokens
}

// concretePath turns a registered pattern into a URL that will reach the
// guard. The value substituted for a wildcard does not matter: the role check
// runs before any handler, so whether the entity exists is irrelevant to what
// is being tested here.
func concretePath(pattern string) (method, path string) {
	method, path = "GET", pattern
	if m, rest, ok := strings.Cut(pattern, " "); ok {
		method, path = m, rest
	}
	return method, placeholder.ReplaceAllString(path, "1")
}

// requestBody is request() when the answer's text matters and not just its
// status, which is the case wherever a refusal is meant to be actionable.
func requestBody(t *testing.T, srv *httptest.Server, token, method, path string) string {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func request(t *testing.T, srv *httptest.Server, token, method, path string) int {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// The route table must be complete. A registration that bypassed reg() would
// serve a route this test never sees, so the count is asserted to be in the
// right order of magnitude: a table that has collapsed to a handful means the
// recording broke, and the tests below would then all pass vacuously.
func TestEveryRouteIsRecorded(t *testing.T) {
	api, _, _ := guardFixture(t)
	routes := api.registeredRoutes()
	if len(routes) < 50 {
		t.Fatalf("only %d routes recorded; the route table is not being "+
			"populated, so the guard sweeps below prove nothing", len(routes))
	}
	seen := map[string]bool{}
	for _, r := range routes {
		if seen[r.Pattern] {
			t.Errorf("%q is registered twice", r.Pattern)
		}
		seen[r.Pattern] = true
		if r.MinRole == "" {
			t.Errorf("%q has no required role", r.Pattern)
		}
		if !strings.Contains(r.Pattern, " ") {
			t.Errorf("%q names no method; it would answer every verb", r.Pattern)
		}
	}
}

// No route answers an unauthenticated caller. This is the whole reason the
// route table is recorded: the claim is about every route, so the test has to
// be too.
func TestNoRouteAnswersWithoutACredential(t *testing.T) {
	api, srv, _ := guardFixture(t)
	for _, r := range api.registeredRoutes() {
		method, path := concretePath(r.Pattern)
		if code := request(t, srv, "", method, path); code != http.StatusUnauthorized {
			t.Errorf("%s answered %d without a token, want 401", r.Pattern, code)
		}
	}
}

// Nor a caller holding a credential that is not a credential. Checked over
// every route as well, because a handler that read an identity from the
// request rather than the token would not be caught by the no-token sweep.
func TestNoRouteAnswersAnInvalidCredential(t *testing.T) {
	api, srv, tokens := guardFixture(t)
	// Shapes worth trying: rubbish, a token belonging to no account, and a
	// real token with a character changed.
	admin := tokens[store.RoleAdmin]
	tampered := admin[:len(admin)-1] + map[bool]string{true: "A", false: "B"}[strings.HasSuffix(admin, "B")]
	for _, bad := range []string{"not-a-token", "plain.", tampered} {
		for _, r := range api.registeredRoutes() {
			method, path := concretePath(r.Pattern)
			if code := request(t, srv, bad, method, path); code != http.StatusUnauthorized {
				t.Errorf("%s answered %d for token %q, want 401", r.Pattern, code, bad)
			}
		}
	}
}

// An "Authorization" header that is not a bearer token is no credential.
// Checked because a lenient parser here would accept the header shapes a
// browser or proxy adds on its own.
func TestOnlyABearerTokenAuthenticates(t *testing.T) {
	_, srv, tokens := guardFixture(t)
	admin := tokens[store.RoleAdmin]
	for _, header := range []string{
		admin,              // the token with no scheme
		"bearer " + admin,  // lowercase scheme
		"Bearer  " + admin, // the token is trimmed, so this one is fine
		"Basic " + admin,   // another scheme
		"Bearer",           // scheme alone
		"Bearer ",          // scheme and nothing
		"Token " + admin,   // a plausible-looking wrong scheme
		"Bearer " + admin + "x",
	} {
		req, err := http.NewRequest("GET", srv.URL+"/whoami", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", header)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		// "Bearer  <tok>" is the one accepted form here: the parser trims the
		// value, which is deliberate and harmless. Everything else must fail.
		want := http.StatusUnauthorized
		if header == "Bearer  "+admin {
			want = http.StatusOK
		}
		if resp.StatusCode != want {
			t.Errorf("Authorization: %q returned %d, want %d", header, resp.StatusCode, want)
		}
	}
}

// A plain account is refused every route above its rank, and an operator is
// refused every admin route. Swept over the table so that a new administrative
// route registered with the wrong guard fails here rather than in production.
func TestRoutesAboveTheCallersRankAreRefused(t *testing.T) {
	api, srv, tokens := guardFixture(t)
	for _, r := range api.registeredRoutes() {
		method, path := concretePath(r.Pattern)
		for _, caller := range []struct {
			role store.Role
			name string
		}{
			{store.RoleUser, "a plain account"},
			{store.RoleOperator, "an operator"},
		} {
			code := request(t, srv, tokens[caller.role], method, path)
			allowed := caller.role.AtLeast(r.MinRole)
			switch {
			case !allowed && code != http.StatusForbidden:
				t.Errorf("%s: %s got %d, want 403 (route needs %s)",
					r.Pattern, caller.name, code, r.MinRole)
			case allowed && code == http.StatusForbidden:
				// Cross-user access answers 404 by design and account
				// suspension answers 403, but neither applies to a fresh
				// account acting on itself -- so a 403 here means the route
				// demands more than its table entry says.
				t.Errorf("%s: %s got 403 though the route only needs %s",
					r.Pattern, caller.name, r.MinRole)
			}
		}
	}
}

// Every route an admin may reach, an admin does reach -- meaning it gets past
// the guard, whatever the handler then makes of the request. A route that
// answered 401 or 403 to an admin would be unreachable by anybody.
func TestAdminReachesEveryRoute(t *testing.T) {
	api, srv, tokens := guardFixture(t)
	for _, r := range api.registeredRoutes() {
		method, path := concretePath(r.Pattern)
		code := request(t, srv, tokens[store.RoleAdmin], method, path)
		if code == http.StatusUnauthorized || code == http.StatusForbidden {
			t.Errorf("%s refused an admin with %d; no account could reach it", r.Pattern, code)
		}
	}
}

// A suspended account is refused everywhere, on a credential it already
// holds. Quarantine is the break-glass lever for a runaway account, so it has
// to invalidate the token in somebody's hands rather than only stop the next
// login.
//
// Refused with 403 and a message that says so, on every route. Not 401: the
// caller has presented a genuine credential, so "invalid API token" would
// send somebody who has just been quarantined -- or the admin working out why
// their jobs stopped -- looking for a token that was never the problem.
func TestASuspendedAccountIsRefusedEveryRoute(t *testing.T) {
	api, srv, tokens := guardFixture(t)
	ctx := context.Background()
	tok := tokens[store.RoleUser]
	// It works first, or the sweep below would pass on a token that was never
	// good for anything.
	if code := request(t, srv, tok, "GET", "/whoami"); code != http.StatusOK {
		t.Fatalf("the account was refused before being suspended: %d", code)
	}
	if err := api.c.Store().SetUserDisabled(ctx, "plain", true); err != nil {
		t.Fatal(err)
	}
	for _, r := range api.registeredRoutes() {
		method, path := concretePath(r.Pattern)
		if code := request(t, srv, tok, method, path); code != http.StatusForbidden {
			t.Errorf("%s answered a suspended account %d, want 403", r.Pattern, code)
		}
	}
	// The message has to name the reason, or the status alone is the same
	// answer as "you lack the role for this".
	if body := requestBody(t, srv, tok, "GET", "/whoami"); !strings.Contains(body, "suspend") {
		t.Errorf("the refusal does not say the account is suspended: %s", body)
	}
	// And it works again when the suspension is lifted: quarantine is meant to
	// be reversible, so this must not have consumed the token.
	if err := api.c.Store().SetUserDisabled(ctx, "plain", false); err != nil {
		t.Fatal(err)
	}
	if code := request(t, srv, tok, "GET", "/whoami"); code != http.StatusOK {
		t.Errorf("lifting the suspension left the account locked out: %d", code)
	}
}

// The other refusals stay merged, and that is the actual security property:
// an unknown token, a malformed one and an expired session credential are all
// "this is not a credential". Telling them apart would say which guesses were
// closer to a real one.
func TestCredentialsThatAreNotCredentialsGetOneAnswer(t *testing.T) {
	api, srv, _ := guardFixture(t)
	ctx := context.Background()
	expired, err := api.c.Store().CreateSessionToken(ctx, "plain", "sess-x",
		time.Minute, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var bodies []string
	for _, tok := range []string{"not-a-token", "plain.wrong", expired, "x"} {
		code := request(t, srv, tok, "GET", "/whoami")
		if code != http.StatusUnauthorized {
			t.Errorf("token %q returned %d, want 401", tok, code)
		}
		bodies = append(bodies, requestBody(t, srv, tok, "GET", "/whoami"))
	}
	for i, b := range bodies {
		if b != bodies[0] {
			t.Errorf("refusal %d differs from the first:\n  %s\n  %s", i, bodies[0], b)
		}
	}
}

// A suspended account's session credentials go too. A login session holds a
// short-lived token of its own, so refusing only the account's API token would
// leave an open session working for up to its lifetime -- which is exactly the
// session a quarantine is aimed at.
func TestSuspensionRefusesASessionCredentialToo(t *testing.T) {
	api, srv, _ := guardFixture(t)
	ctx := context.Background()
	tok, err := api.c.Store().CreateSessionToken(ctx, "plain", "sess-1",
		time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if code := request(t, srv, tok, "GET", "/whoami"); code != http.StatusOK {
		t.Fatalf("a fresh session credential was refused: %d", code)
	}
	if err := api.c.Store().SetUserDisabled(ctx, "plain", true); err != nil {
		t.Fatal(err)
	}
	// 403, the same as the account's own token: the credential is genuine and
	// the account is suspended, which is one situation however it was reached.
	if code := request(t, srv, tok, "GET", "/whoami"); code != http.StatusForbidden {
		t.Errorf("a suspended account's session credential returned %d, want 403", code)
	}
}

// An expired session credential is no credential. The expiry is what bounds
// the damage of one leaking, so it has to be checked on use rather than only
// swept in the background.
func TestAnExpiredSessionCredentialIsRefused(t *testing.T) {
	api, srv, _ := guardFixture(t)
	ctx := context.Background()
	tok, err := api.c.Store().CreateSessionToken(ctx, "plain", "sess-2",
		time.Minute, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if code := request(t, srv, tok, "GET", "/whoami"); code != http.StatusUnauthorized {
		t.Errorf("an expired session credential returned %d, want 401", code)
	}
}

// The sweeps above check that the guard agrees with the route table. They
// cannot catch the table itself being wrong: lower a route's declared role and
// the expectation drops with it. So the sensitive routes are pinned here by
// what they *do*, independently of what the table claims.
//
// Expressed as patterns rather than a hand-list, so a route added later is
// covered the moment it is named rather than the moment somebody remembers to
// add it below.
func TestSensitiveRoutesCannotBeDowngraded(t *testing.T) {
	api, _, _ := guardFixture(t)

	// mustBe returns the least role a route matching this shape may accept,
	// and "" when nothing here constrains it.
	mustBe := func(pattern string) store.Role {
		method, path, _ := strings.Cut(pattern, " ")
		writing := method == "POST" || method == "DELETE" || method == "PUT"
		switch {
		// Identity and entitlement: who exists, and what they may have.
		// Reading /users is an inventory of accounts; writing is minting one.
		case strings.HasPrefix(path, "/users"), path == "/token":
			return store.RoleAdmin
		case writing && strings.HasPrefix(path, "/qos"):
			return store.RoleAdmin
		case writing && strings.HasPrefix(path, "/priority"):
			return store.RoleAdmin
		// Who can log in. An admin-only route, because a key is access.
		case writing && strings.HasPrefix(path, "/keys"),
			writing && strings.HasPrefix(path, "/codes"):
			return store.RoleAdmin
		// What the cluster and its machines are called, and whether a machine
		// is a member at all.
		case writing && strings.HasPrefix(path, "/cluster"):
			return store.RoleAdmin
		case path == "/shutdown",
			strings.HasSuffix(path, "/stop") && strings.HasPrefix(path, "/node"),
			strings.HasSuffix(path, "/remove") && strings.HasPrefix(path, "/node"):
			return store.RoleAdmin
		// Acting on running work, and reading the record of it. Not identity,
		// so an operator is enough -- but never a plain account.
		case strings.HasPrefix(path, "/audit"),
			strings.HasPrefix(path, "/emergency"),
			strings.HasSuffix(path, "/drain"), strings.HasSuffix(path, "/resume"):
			return store.RoleOperator
		}
		return ""
	}

	checked := 0
	for _, r := range api.registeredRoutes() {
		want := mustBe(r.Pattern)
		if want == "" {
			continue
		}
		checked++
		if !r.MinRole.AtLeast(want) {
			t.Errorf("%s requires %s; it must require at least %s",
				r.Pattern, r.MinRole, want)
		}
	}
	// A guard against the patterns above silently matching nothing, which
	// would make this test pass while checking no route at all.
	if checked < 15 {
		t.Errorf("only %d routes matched a sensitive pattern; the patterns have "+
			"drifted away from the route names", checked)
	}
}

// A plain account may reach the routes that are about its own work, or the
// cluster would be unusable by anyone who is not an admin. Pinned for the
// same reason as above: an over-tightened guard is a different bug, not a
// safer one, and the table cannot catch it either.
func TestEverydayRoutesStayOpenToAPlainAccount(t *testing.T) {
	api, _, _ := guardFixture(t)
	want := map[string]bool{
		"POST /submit": true, "GET /jobs": true, "GET /job/{id}": true,
		"POST /cancel/{id}": true, "GET /whoami": true, "GET /quota": true,
		"GET /fs": true, "GET /node": true, "POST /plan": true,
		"GET /job/{id}/output": true, "GET /share": true, "GET /qos": true,
	}
	seen := map[string]bool{}
	for _, r := range api.registeredRoutes() {
		if !want[r.Pattern] {
			continue
		}
		seen[r.Pattern] = true
		if r.MinRole != roleAny {
			t.Errorf("%s requires %s; a plain account must be able to use it",
				r.Pattern, r.MinRole)
		}
	}
	for p := range want {
		if !seen[p] {
			t.Errorf("%s is no longer registered; this test is checking a route "+
				"that does not exist", p)
		}
	}
}
