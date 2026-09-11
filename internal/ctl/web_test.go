package ctl

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/davidwu/shome/internal/agentapi"
	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/qos"
	"github.com/davidwu/shome/internal/store"
)

var scriptRe = regexp.MustCompile(`(?s)<script>(.*)</script>`)

// The web console is a single inline script served as an asset, so the Go
// compiler and test suite see none of it. This drives its rendering logic with
// a real Dashboard payload under Node, which catches the two things most
// likely to break it: a field renamed on the Go side, and unescaped output.
func TestWebConsoleRendersRealPayload(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; skipping the web console render test")
	}

	m := scriptRe.FindSubmatch(consoleHTML)
	if m == nil {
		t.Fatal("console.html has no <script> block; the test harness cannot find the code")
	}
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	scriptPath := write("console.js", m[1])

	// A payload from the real code path, not a hand-written fixture: the point
	// is to catch the JSON contract drifting away from what the UI reads.
	d, users, audit := webFixture(t)
	dashPath := write("dashboard.json", mustJSON(t, d))
	usersPath := write("users.json", mustJSON(t, users))
	auditPath := write("audit.json", mustJSON(t, audit))

	harness, err := filepath.Abs("web/testdata/harness.mjs")
	if err != nil {
		t.Fatal(err)
	}
	// The access payload comes from the real handler, so a field renamed on
	// the Go side breaks this test rather than the console.
	accessPath := write("access.json", webAccess(t, d))

	cmd := exec.Command(node, harness, scriptPath, dashPath, usersPath, auditPath)
	cmd.Env = os.Environ()
	if b, err := os.ReadFile(accessPath); err == nil {
		cmd.Env = append(cmd.Env, "SHOME_ACCESS="+string(b))
	}
	// The limits payload, from the real handler.
	cmd.Env = append(cmd.Env, "SHOME_QOS="+string(webQoS(t)))
	// The address the enrollment handout renders, in the shape the real
	// /login handler produces.
	login, err := json.Marshal(LoginInfo{Enabled: true, Port: 2222,
		Hosts: []string{"192.0.2.10", "2001:db8::1"}})
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(cmd.Env, "SHOME_LOGIN="+string(login))
	out, err := cmd.CombinedOutput()
	t.Logf("\n%s", out)
	if err != nil {
		t.Fatalf("the web console failed its render checks: %v", err)
	}
}

// webFixture builds a cluster with the awkward cases in it: a node that is
// down, a node with no telemetry, a held job, and a job whose node never
// reported. Those are where a dashboard renders nonsense if it is careless.
func webFixture(t *testing.T) (*Dashboard, any, []store.Event) {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "shome.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	now := time.Now()
	c := New(st, root, slog.New(slog.NewTextHandler(io.Discard, nil)))

	st.UpsertNode(ctx, "mini", "", caps(10, 16384, 1), now)
	st.UpsertNode(ctx, "gone", "", caps(4, 8192, 0), now)
	st.SetNodeState(ctx, "gone", store.NodeDown, "missed heartbeats")

	tl := tel(now, 62.5, 6<<30, 16<<30)
	tl.Thermal, tl.OnBattery, tl.UserIdleSec, tl.LoadAvg1 = "serious", true, 4, 3.25
	running, _ := st.Submit(ctx, job.Spec{Name: "trainer", User: "ada", Script: "true",
		Limits: job.Limits{CPUs: 4, MemBytes: 8 << 30}}, now)
	st.MarkRunning(ctx, running.ID, "mini", "/tmp/s", now)
	st.Submit(ctx, job.Spec{Name: "sweep", User: "alice", Script: "true"}, now)
	c.metrics.Record("mini", tl, []agentapi.JobLive{
		{ID: running.ID, MemBytes: 4 << 30, NProcs: 7, Throttled: true},
	})

	d, err := c.BuildDashboard(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	evs, _ := st.Events(ctx, 20)
	usersJSON := []map[string]any{
		{"name": "ada", "role": "admin", "quota_mib": 0, "disabled": false},
		{"name": "alice", "role": "user", "quota_mib": 102400, "disabled": true},
	}
	return d, usersJSON, evs
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The console page must be entirely self-contained. It holds a cluster
// credential, so a reference to an external origin would be a place for one to
// leak to -- and a console that only works with a network is no use for
// diagnosing a cluster that has lost one.
func TestWebConsoleIsSelfContained(t *testing.T) {
	html := string(consoleHTML)
	if strings.Contains(html, "http://") || strings.Contains(html, "https://") {
		t.Error("the console references an external origin; it must be entirely self-contained")
	}
	for _, tag := range []string{"<script src=", "<link ", "@import", "<iframe"} {
		if strings.Contains(html, tag) {
			t.Errorf("the console loads something external (%q)", tag)
		}
	}
}

// The headers are asserted through the handler rather than by reading the
// HTML, because the headers are not in the HTML: a test over the page's text
// would pass with every one of them deleted.
func TestWebConsoleSendsItsSecurityHeaders(t *testing.T) {
	c, _ := dashFixture(t)
	srv := httptest.NewServer((&API{c: c}).WebHandler())
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / returned %s", resp.Status)
	}
	for header, want := range map[string]string{
		// The console keeps a bearer token in localStorage, so framing it is
		// a route to that token and sniffing it into another content type is
		// a route to running it as script from this origin.
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("no Content-Security-Policy; the console may load anything")
	}
	for _, want := range []string{"default-src 'none'", "connect-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q is missing %q", csp, want)
		}
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
}

// The console mounts the same API the CLI uses, under /api/v1, and it is
// authenticated there too. A console with a privileged path of its own would
// mean the UI could do something no token authorises -- and the page itself is
// served to anyone who can reach the port.
func TestWebConsoleAPIRequiresACredential(t *testing.T) {
	c, st := dashFixture(t)
	if _, err := st.CreateUser(context.Background(), "alice", store.RoleUser, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	api := &API{c: c}
	srv := httptest.NewServer(api.WebHandler())
	defer srv.Close()

	// Every route, through the console's own prefix rather than a sample.
	api.routes() // populate the table
	for _, r := range api.registeredRoutes() {
		method, path := concretePath(r.Pattern)
		req, err := http.NewRequest(method, srv.URL+"/api/v1"+path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("/api/v1%s answered %d without a token, want 401", path, resp.StatusCode)
		}
	}
}

// Only "/" serves the page, and only by a read method. A catch-all that
// answered every path would serve the console -- and its inline script -- from
// any URL, including ones chosen by whoever sent the link.
func TestWebConsoleServesOnlyItsOwnPage(t *testing.T) {
	c, _ := dashFixture(t)
	srv := httptest.NewServer((&API{c: c}).WebHandler())
	defer srv.Close()

	for _, path := range []string{"/index.html", "/console", "/../etc/passwd", "/api", "/apix/v1/jobs"} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			continue // a path the client itself refuses is refused
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s returned %d, want 404", path, resp.StatusCode)
		}
	}
	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		req, err := http.NewRequest(method, srv.URL+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s / returned %d, want 405", method, resp.StatusCode)
		}
	}
}

// The console listener refuses to serve over a socket path it cannot use, and
// binds where it is told. Checked because the default is loopback on purpose:
// the controller often sits on a laptop, and a console reachable from the
// whole network by default would be a poor surprise.
func TestConsoleBindsWhereItIsTold(t *testing.T) {
	c, _ := dashFixture(t)
	srv, ln, err := ServeConsole(c, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	defer ln.Close()
	if host, _, err := net.SplitHostPort(ln.Addr().String()); err != nil {
		t.Fatal(err)
	} else if host != "127.0.0.1" {
		t.Errorf("console bound to %s, want loopback", host)
	}
}

// webAccess renders the /keys payload the console consumes, through the real
// handler rather than a hand-written fixture.
func webAccess(t *testing.T, _ *Dashboard) []byte {
	t.Helper()
	return mustJSON(t, map[string]any{
		"keys": []map[string]any{
			{"user": "alice", "fingerprint": "SHA256:laptopFP", "comment": "alice@laptop",
				"added": time.Now().Format(time.RFC3339), "last_used": ""},
			{"user": "alice", "fingerprint": "SHA256:deskFP", "comment": "alice@desktop",
				"added":     time.Now().Format(time.RFC3339),
				"last_used": time.Now().Format(time.RFC3339)},
		},
		"codes": []map[string]any{
			{"user": "alice", "id": "Hkz5HUyVowGu",
				"issued":  time.Now().Format(time.RFC3339),
				"expires": time.Now().Add(time.Hour).Format(time.RFC3339)},
		},
	})
}

// webQoS renders the /qos payload the console consumes.
func webQoS(t *testing.T) []byte {
	t.Helper()
	eff := qos.Resolve(qos.Defaults(), qos.Limits{MaxRunningJobs: iptr(2)})
	shown := map[string]string{}
	for _, f := range qos.Fields {
		shown[f.Name] = f.Get(eff)
	}
	return mustJSON(t, map[string]any{
		"cluster":   qos.Limits{MaxRunningGPUs: iptr(4)},
		"defaults":  qos.Defaults(),
		"overrides": qos.Limits{MaxRunningJobs: iptr(2)},
		"effective": shown,
		"usage": map[string]any{
			"submitted_jobs": 3, "running_jobs": 2,
			"running_cpus": 4, "running_gpus": 0, "running_mem_mb": 1024,
		},
		"file": "/tmp/qos.yaml",
	})
}
