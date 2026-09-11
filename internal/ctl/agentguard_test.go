package ctl

import (
	"crypto/tls"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/davidwu/shome/internal/pki"
)

// The agent listener accepts connections with no client certificate, because
// join -- the endpoint where a machine gets its certificate -- could not
// otherwise happen. So accepting a connection proves nothing about who is on
// it, and any endpoint that forgot to check would be reachable by anything
// that can route to the port.
//
// That is a claim about every endpoint, spread over four files, held together
// by discipline. These tests assert it over the recorded route table so that
// a new endpoint registered without the check fails here.

// agentPlaceholder matches a wildcard segment in a ServeMux pattern.
var agentPlaceholder = regexp.MustCompile(`\{[^}]*\}`)

// agentFixture starts the real mTLS listener, with a real CA, and returns
// clients holding a node certificate and holding none.
func agentFixture(t *testing.T) (*AgentServer, string, *http.Client, *http.Client) {
	t.Helper()
	c, _ := dashFixture(t)
	ca, err := pki.LoadOrCreateCA(filepath.Join(t.TempDir(), "ca"))
	if err != nil {
		t.Fatal(err)
	}
	srvCert, srvKey, err := ca.Issue("shome-controller", []string{"127.0.0.1"}, false)
	if err != nil {
		t.Fatal(err)
	}
	// ServerTLSJoinable is the production configuration: it verifies a
	// certificate when one is offered and accepts a connection without one.
	tlsCfg, err := ca.ServerTLSJoinable(srvCert, srvKey)
	if err != nil {
		t.Fatal(err)
	}

	s := &AgentServer{c: c, ca: ca, api: (&API{c: c}).routes()}
	srv := httptest.NewUnstartedServer(s.routes())
	srv.TLS = tlsCfg
	// A refused handshake is the expected result in several tests below, and
	// the server logs each one to stderr. Discarded so that a passing run is
	// quiet: scary-looking x509 errors in green output teach people to stop
	// reading it.
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)

	nodeCert, nodeKey, err := ca.Issue("node-a", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	withCert, err := pki.ClientTLS(ca.CertPEM(), nodeCert, nodeKey, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	// No client certificate at all: the shape an outsider on the network has.
	noCert := &tls.Config{RootCAs: ca.Pool(), ServerName: "127.0.0.1"}

	mk := func(cfg *tls.Config) *http.Client {
		return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
	}
	return s, srv.URL, mk(withCert), mk(noCert)
}

func agentRequest(t *testing.T, cli *http.Client, base, pattern string) int {
	t.Helper()
	method, path := "GET", pattern
	if m, rest, ok := strings.Cut(pattern, " "); ok {
		method, path = m, rest
	}
	path = agentPlaceholder.ReplaceAllString(path, "1")
	req, err := http.NewRequest(method, base+path, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// The table has to be populated, or every sweep below passes vacuously.
func TestEveryAgentRouteIsRecorded(t *testing.T) {
	s, _, _, _ := agentFixture(t)
	routes := s.registeredAgentRoutes()
	if len(routes) < 10 {
		t.Fatalf("only %d agent routes recorded; the table is not being populated", len(routes))
	}
	open := 0
	for _, r := range routes {
		if !r.NeedsPeer {
			open++
			if !strings.Contains(r.Pattern, "/join") {
				t.Errorf("%q is registered without requiring a node certificate", r.Pattern)
			}
		}
	}
	if open != 1 {
		t.Errorf("%d routes need no certificate; exactly one (join) should", open)
	}
}

// Nothing but join answers a caller with no client certificate. This is the
// test that would have caught an endpoint added without the check.
func TestNoAgentRouteAnswersWithoutANodeCertificate(t *testing.T) {
	s, base, _, noCert := agentFixture(t)
	for _, r := range s.registeredAgentRoutes() {
		if !r.NeedsPeer {
			continue
		}
		code := agentRequest(t, noCert, base, r.Pattern)
		if code != http.StatusUnauthorized {
			t.Errorf("%s answered %d with no client certificate, want 401", r.Pattern, code)
		}
	}
}

// A certificate this CA did not issue is no certificate. The join token is
// what mints one, so a cluster that accepted a foreign CA's certificate would
// have no membership control at all.
func TestACertificateFromAnotherCAIsRefused(t *testing.T) {
	s, base, _, _ := agentFixture(t)
	other, err := pki.LoadOrCreateCA(filepath.Join(t.TempDir(), "other-ca"))
	if err != nil {
		t.Fatal(err)
	}
	cert, key, err := other.Issue("node-a", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	// Trust this cluster's CA as a server, but present the other one's
	// certificate as a client -- an impostor claiming to be node-a.
	cfg, err := pki.ClientTLS(s.ca.CertPEM(), cert, key, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	cli := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}

	for _, r := range s.registeredAgentRoutes() {
		if !r.NeedsPeer {
			continue
		}
		method, path := "GET", r.Pattern
		if m, rest, ok := strings.Cut(r.Pattern, " "); ok {
			method, path = m, rest
		}
		path = agentPlaceholder.ReplaceAllString(path, "1")
		req, err := http.NewRequest(method, base+path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := cli.Do(req)
		if err != nil {
			continue // the handshake itself was refused, which is the point
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s answered %d to a foreign CA's certificate, want a refusal",
				r.Pattern, resp.StatusCode)
		}
	}
}

// A node with a valid certificate still does not get past the guard to
// somebody else's work: every per-job and per-session route checks that the
// job or stream is assigned to the calling node, and answers 403 when it is
// not. Without that, any machine in the cluster could attach to any session
// or collect any job's staged inputs.
func TestAValidNodeCannotReachWorkItWasNotGiven(t *testing.T) {
	s, base, withCert, _ := agentFixture(t)
	// node-a holds a real certificate, and no job or stream is assigned to
	// it, so every route scoped to one must refuse rather than serve.
	scoped := 0
	for _, r := range s.registeredAgentRoutes() {
		if !strings.Contains(r.Pattern, "{id}") {
			continue
		}
		scoped++
		code := agentRequest(t, withCert, base, r.Pattern)
		if code == http.StatusOK {
			t.Errorf("%s served a node that was not given that work (200)", r.Pattern)
		}
		if code == http.StatusUnauthorized {
			t.Errorf("%s refused a valid node certificate (401); the check should be "+
				"about which work is its own, not whether it is a node", r.Pattern)
		}
	}
	if scoped < 5 {
		t.Errorf("only %d per-id routes were checked; the sweep is not finding them", scoped)
	}
}

// The job API reached through a node is authenticated twice. The node's
// certificate gets the request there; it does not answer it. A node
// forwarding for a job cannot use its own membership to ask the cluster
// anything.
func TestTheJobAPIThroughANodeStillNeedsTheJobsToken(t *testing.T) {
	_, base, withCert, noCert := agentFixture(t)
	if code := agentRequest(t, noCert, base, "GET /agent/api/whoami"); code != http.StatusUnauthorized {
		t.Errorf("the job API answered %d with no node certificate, want 401", code)
	}
	// With a node certificate but no job token: through the first gate,
	// refused at the second.
	if code := agentRequest(t, withCert, base, "GET /agent/api/whoami"); code != http.StatusUnauthorized {
		t.Errorf("the job API answered %d to a node with no job token, want 401 -- "+
			"a node's own identity must not be an identity to answer as", code)
	}
}
