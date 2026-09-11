package ownerweb

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/davidwu/shome/internal/agent"
	"github.com/davidwu/shome/internal/platform"
)

func fixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	srv, ln, err := Listen(root, "127.0.0.1:0", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return root, "http://" + ln.Addr().String()
}

// The page has no login, because it is for whoever is sitting at the machine.
// That is only defensible while it cannot be reached from anywhere else, so
// binding it to the network must fail rather than warn.
func TestRefusesNonLoopback(t *testing.T) {
	// ":0" and "0.0.0.0:0" are the same instruction to net.Listen -- every
	// interface -- and the bare form is the one a hand-edited config is most
	// likely to contain, so it must be refused by the same check rather than
	// slipping through as an empty host part.
	for _, addr := range []string{
		"0.0.0.0:0", "10.0.0.5:0", "[::]:0", ":0", ":7821",
		"192.0.2.10:7821", "example.invalid:7821",
	} {
		if _, _, err := Listen(t.TempDir(), addr, slog.New(slog.DiscardHandler)); err == nil {
			t.Errorf("%s was accepted; the page would be exposed to the network", addr)
		} else if !strings.Contains(err.Error(), "loopback") {
			t.Errorf("%s refused for the wrong reason: %v", addr, err)
		}
	}
	for _, addr := range []string{"127.0.0.1:0", "localhost:0"} {
		srv, ln, err := Listen(t.TempDir(), addr, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Errorf("%s was refused: %v", addr, err)
			continue
		}
		srv.Close()
		_ = ln
	}
}

// A machine where shome has never run is the ordinary case, and deserves an
// explanation rather than a 500.
func TestMissingStatusIsExplained(t *testing.T) {
	_, base := fixture(t)
	resp, err := http.Get(base + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("returned %d", resp.StatusCode)
	}
	var out struct {
		Available bool   `json:"available"`
		Reason    string `json:"reason"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Available {
		t.Error("reported status as available with no file")
	}
	if !strings.Contains(out.Reason, "not be running") {
		t.Errorf("unhelpful reason: %q", out.Reason)
	}
}

func TestServesPublishedStatus(t *testing.T) {
	root, base := fixture(t)
	tel := platform.UnknownTelemetry(time.Now())
	tel.CPUPercent = 42
	st := agent.LocalStatus{
		At: time.Now(), Node: "mini", Connected: true, OwnerAction: "run",
		AdvertisedCPUs: 6, TotalCPUs: 10, Telemetry: tel,
		Jobs: []agent.LocalJob{{ID: 7, Name: "train", User: "alice", CPUs: 2}},
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	if err := os.WriteFile(filepath.Join(root, "node-status.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(base + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Available bool              `json:"available"`
		Status    agent.LocalStatus `json:"status"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if !out.Available || out.Status.Node != "mini" {
		t.Fatalf("got %+v", out)
	}
	if len(out.Status.Jobs) != 1 || out.Status.Jobs[0].Name != "train" {
		t.Errorf("jobs not carried through: %+v", out.Status.Jobs)
	}
	// The gap the owner's policy makes must survive the round trip, since it
	// is the point of the contribution panel.
	if out.Status.AdvertisedCPUs != 6 || out.Status.TotalCPUs != 10 {
		t.Errorf("advertised/total = %d/%d, want 6/10",
			out.Status.AdvertisedCPUs, out.Status.TotalCPUs)
	}
}

// Pause and resume are the owner's veto and need no cluster involvement, so
// they must work against nothing but a state directory.
func TestPauseAndResume(t *testing.T) {
	root, base := fixture(t)
	resp, err := http.Post(base+"/api/pause?reason=lunch", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("pause returned %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(root, "paused")); err != nil {
		t.Fatalf("pause left no marker: %v", err)
	}

	resp, err = http.Post(base+"/api/resume", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if _, err := os.Stat(filepath.Join(root, "paused")); err == nil {
		t.Error("resume left the pause marker in place")
	}
}

func TestPageIsSelfContained(t *testing.T) {
	_, base := fixture(t)
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "shome") {
		t.Error("the page does not look like the monitor")
	}
	// No external origins: the page must work on a machine with no internet,
	// which is a normal state for a node whose network is the problem.
	for _, bad := range []string{"http://", "https://"} {
		if strings.Contains(string(body), bad) {
			t.Errorf("the page references an external origin (%s)", bad)
		}
	}
	if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q", got)
	}
}

// The refusal has to be the thing that stops the bind, not a message printed
// beside one: an address with no host part reaches net.Listen as "every
// interface", so a check that accepted it would publish the pause control with
// no login on it. Proved by trying to reach the listener off loopback.
func TestOffLoopbackAddressesNeverGetAListener(t *testing.T) {
	for _, addr := range []string{":0", "0.0.0.0:0", "[::]:0"} {
		srv, ln, err := Listen(t.TempDir(), addr, slog.New(slog.DiscardHandler))
		if err == nil {
			if ln != nil {
				t.Errorf("%s produced a listener on %s", addr, ln.Addr())
				ln.Close()
			}
			if srv != nil {
				srv.Close()
			}
			continue
		}
		if srv != nil || ln != nil {
			t.Errorf("%s was refused but still handed back a server or listener", addr)
		}
	}
}

// A hostname is refused rather than resolved. Whether a name points at this
// machine is not something this check can decide, and "localhost" is the only
// name that is loopback by definition.
func TestOnlyLoopbackLiteralsAndLocalhostAreAccepted(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "localhost", "[::1]"} {
		srv, ln, err := Listen(t.TempDir(), host+":0", slog.New(slog.DiscardHandler))
		if err != nil {
			t.Errorf("%s was refused: %v", host, err)
			continue
		}
		ln.Close()
		srv.Close()
	}
	for _, host := range []string{"example.invalid", "localhost.localdomain", "0.0.0.0"} {
		if _, _, err := Listen(t.TempDir(), host+":0", slog.New(slog.DiscardHandler)); err == nil {
			t.Errorf("%s was accepted; only loopback literals and localhost are", host)
		}
	}
	// The policy itself, for addresses this machine may not be able to bind:
	// the whole 127/8 range is loopback, and an empty host is not.
	for host, want := range map[string]bool{
		"127.0.0.1": true, "127.0.0.2": true, "::1": true, "localhost": true,
		"": false, "0.0.0.0": false, "::": false, "192.0.2.10": false,
		"example.invalid": false,
	} {
		if got := isLoopback(host); got != want {
			t.Errorf("isLoopback(%q) = %v, want %v", host, got, want)
		}
	}
}
