package ctl

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/davidwu/shome/internal/store"
)

func bootstrapFixture(t *testing.T) (*bootstrap, *store.Store, string) {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "shome.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tok, err := st.CreateJoinToken(t.Context(), JoinTokenTTL, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return &bootstrap{st: st, root: root, log: slog.New(slog.DiscardHandler)}, st, tok
}

func serve(b *bootstrap, method, target string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /j/{token}/install", b.install)
	mux.HandleFunc("GET /j/{token}/bin/{platform}", b.binary)
	req := httptest.NewRequest(method, target, nil)
	req.Host = "10.0.0.5:7818"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// The token in the URL is the only thing standing between this service and
// anyone on the network. Every route must check it.
func TestBootstrapRejectsBadToken(t *testing.T) {
	b, _, _ := bootstrapFixture(t)
	for _, path := range []string{
		"/j/not-a-real-token/install",
		"/j/not-a-real-token/bin/linux-amd64",
	} {
		if w := serve(b, "GET", path); w.Code != http.StatusForbidden {
			t.Errorf("%s returned %d, want 403", path, w.Code)
		}
	}
}

func TestBootstrapRejectsUsedToken(t *testing.T) {
	b, st, tok := bootstrapFixture(t)
	if err := st.RedeemJoinToken(t.Context(), tok, "mini", time.Now()); err != nil {
		t.Fatal(err)
	}
	if w := serve(b, "GET", "/j/"+tok+"/install"); w.Code != http.StatusForbidden {
		t.Errorf("a redeemed token still served the install script (%d)", w.Code)
	}
}

// Fetching the script must NOT consume the token: the join that follows is a
// separate request and needs it to still work.
func TestInstallScriptDoesNotConsumeToken(t *testing.T) {
	b, st, tok := bootstrapFixture(t)
	if w := serve(b, "GET", "/j/"+tok+"/install"); w.Code != http.StatusOK {
		t.Fatalf("install script returned %d", w.Code)
	}
	if err := st.RedeemJoinToken(t.Context(), tok, "mini", time.Now()); err != nil {
		t.Fatalf("token was spent by fetching the script: %v", err)
	}
}

func TestInstallScriptContents(t *testing.T) {
	b, _, tok := bootstrapFixture(t)
	w := serve(b, "GET", "/j/"+tok+"/install")
	if w.Code != http.StatusOK {
		t.Fatalf("returned %d", w.Code)
	}
	body := w.Body.String()
	// The controller address must be the agent port, one below the bootstrap
	// port the client reached us on -- not the bootstrap port itself.
	if !strings.Contains(body, "controller='10.0.0.5:7817'") {
		t.Errorf("script does not point at the agent port:\n%s", body)
	}
	if !strings.Contains(body, tok) {
		t.Error("script does not carry the join token")
	}
	for _, want := range []string{"uname -m", "shome join", "chmod +x"} {
		if !strings.Contains(body, want) {
			t.Errorf("script is missing %q", want)
		}
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/x-shellscript") {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestBinaryServedFromDist(t *testing.T) {
	b, _, tok := bootstrapFixture(t)
	dir := filepath.Join(b.root, "dist", "linux-amd64")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "shome"), []byte("ELF-ish"), 0o755)

	w := serve(b, "GET", "/j/"+tok+"/bin/linux-amd64")
	if w.Code != http.StatusOK {
		t.Fatalf("returned %d: %s", w.Code, w.Body.String())
	}
	got, _ := io.ReadAll(w.Body)
	if string(got) != "ELF-ish" {
		t.Errorf("served %q", got)
	}
}

func TestMissingBinaryExplainsItself(t *testing.T) {
	b, _, tok := bootstrapFixture(t)
	w := serve(b, "GET", "/j/"+tok+"/bin/linux-riscv64")
	if w.Code != http.StatusNotFound {
		t.Fatalf("returned %d, want 404", w.Code)
	}
	// A bare 404 sends the user hunting; the body should say what to do.
	if !strings.Contains(w.Body.String(), "install.sh") {
		t.Errorf("unhelpful error: %s", w.Body.String())
	}
}

// A platform name becomes a path segment. Traversal here would serve arbitrary
// files off the controller to anyone holding a join token.
func TestBinaryPathRejectsTraversal(t *testing.T) {
	b, _, _ := bootstrapFixture(t)
	for _, bad := range []string{"../../etc/passwd", "linux/../../..", ".", "", "a.b"} {
		if _, err := b.binaryPath(bad); err == nil {
			t.Errorf("binaryPath(%q) was accepted", bad)
		}
	}
}

func TestControllerAddrFromHost(t *testing.T) {
	b, _, _ := bootstrapFixture(t)
	for _, tc := range []struct{ host, want string }{
		{"10.0.0.5:7818", "10.0.0.5:7817"},
		{"mini.local:9001", "mini.local:9000"},
		{"10.0.0.5", "10.0.0.5"},
	} {
		req := httptest.NewRequest("GET", "/", nil)
		req.Host = tc.host
		if got := b.controllerAddr(req); got != tc.want {
			t.Errorf("controllerAddr(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}
