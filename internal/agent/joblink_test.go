package agent

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/davidwu/shome/internal/platform"
)

// recordingAPI stands in for the controller connection, and records what a
// job's request looked like by the time it got there.
type recordingAPI struct {
	gotPath string
	gotAuth string
}

func (r *recordingAPI) ProxyAPI(w http.ResponseWriter, req *http.Request) {
	r.gotPath, r.gotAuth = req.URL.Path, req.Header.Get("Authorization")
	io.WriteString(w, `{"name":"alice"}`)
}

// A job that shares this kernel reaches the cluster over a socket in its own
// scratch directory -- no network permission involved, which is the whole
// reason it is a socket.
func TestJobLinkOverASocketInTheScratch(t *testing.T) {
	// Not t.TempDir(): its path is long enough on macOS to exceed what a
	// unix socket path may be, which is the one thing this cannot test
	// around.
	scratch := shortTempDir(t)
	api := &recordingAPI{}
	a := &Agent{Log: slog.New(slog.DiscardHandler), API: api}
	sb := &platform.Sandbox{ScratchDir: scratch, ScratchIn: "/scratch"}

	link, err := a.openJobLink(context.Background(), sb, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if link == nil {
		t.Fatal("no link was made for a job that can have one")
	}
	defer link.Close()

	// What the job is told, and where that really is.
	if got := link.env["SHOME_ROOT"]; got != "/scratch/.shome" {
		t.Errorf("job told SHOME_ROOT=%q, want the scratch as it appears inside", got)
	}
	sock := filepath.Join(scratch, ".shome", "shome.sock")
	if fi, err := os.Stat(sock); err != nil {
		t.Fatalf("no socket for the job to use: %v", err)
	} else if fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s is not a socket", sock)
	}

	// A request over it arrives at the controller with the job's own
	// credential and the path it asked for.
	cl := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	req, _ := http.NewRequest("GET", "http://shome/whoami", nil)
	req.Header.Set("Authorization", "Bearer shs-alice.secret")
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("a job could not reach the cluster over its link: %v", err)
	}
	defer resp.Body.Close()
	if api.gotPath != "/whoami" {
		t.Errorf("controller saw path %q", api.gotPath)
	}
	if api.gotAuth != "Bearer shs-alice.secret" {
		t.Errorf("the job's credential did not survive the hop: %q", api.gotAuth)
	}

	// Closing it takes the socket with it, so nothing is left listening for
	// a job that has ended.
	link.Close()
	if _, err := os.Stat(sock); err == nil {
		t.Error("the socket outlived the job")
	}
}

// shortTempDir is a temporary directory whose path leaves room for a socket
// name. See maxUnixPath.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "shome-link-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// A scratch path too long for a socket is reported as such, rather than as
// the kernel's "invalid argument".
func TestJobLinkRefusesAPathTooLongForASocket(t *testing.T) {
	long := filepath.Join(shortTempDir(t), strings.Repeat("d", 100))
	a := &Agent{Log: slog.New(slog.DiscardHandler), API: &recordingAPI{}}
	_, err := a.openJobLink(context.Background(),
		&platform.Sandbox{ScratchDir: long}, false, false)
	if err == nil {
		t.Fatal("a path over the OS limit was accepted")
	}
	if !strings.Contains(err.Error(), "OS limit") {
		t.Errorf("error does not explain the limit: %v", err)
	}
}

// Without a controller connection there is nothing to forward to, and a job
// gets no link rather than a broken one.
func TestNoJobLinkWithoutAController(t *testing.T) {
	a := &Agent{Log: slog.New(slog.DiscardHandler)}
	link, err := a.openJobLink(context.Background(), &platform.Sandbox{ScratchDir: t.TempDir()}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if link != nil {
		link.Close()
		t.Error("a link was made with nothing on the other end")
	}
}
