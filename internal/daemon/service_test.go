package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestWriteAndClearPID(t *testing.T) {
	root := t.TempDir()
	if _, running := Status(root); running {
		t.Fatal("empty root should not look like a running daemon")
	}
	if err := WritePID(root); err != nil {
		t.Fatal(err)
	}
	pid, running := Status(root)
	if !running || pid != os.Getpid() {
		t.Fatalf("Status = (%d, %v), want (%d, true)", pid, running, os.Getpid())
	}
	ClearPID(root)
	if _, running := Status(root); running {
		t.Fatal("pid file should be gone after ClearPID")
	}
}

// A pid file left by a crash must not make the next start think a daemon is
// up. Getting this wrong means a machine that never comes back after a hard
// reboot, with no error explaining why.
func TestStalePIDFileIsNotRunning(t *testing.T) {
	root := t.TempDir()
	// A process that has certainly exited: spawn one and wait for it.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skipf("cannot spawn a helper process: %v", err)
	}
	dead := cmd.Process.Pid
	if err := os.WriteFile(PIDPath(root), []byte(strconv.Itoa(dead)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, running := Status(root); running {
		t.Fatalf("pid %d has exited but Status says it is running", dead)
	}
}

// ClearPID must not remove a pid file belonging to a different, live daemon --
// otherwise a stray second process cleaning up on exit would make the real one
// invisible to `shome down`.
func TestClearPIDLeavesAnotherProcessAlone(t *testing.T) {
	root := t.TempDir()
	other := os.Getpid() + 1
	if err := os.WriteFile(PIDPath(root), []byte(strconv.Itoa(other)), 0o600); err != nil {
		t.Fatal(err)
	}
	ClearPID(root)
	if _, err := os.Stat(PIDPath(root)); err != nil {
		t.Fatal("ClearPID removed a pid file that names another process")
	}
}

func TestStopWhenNotRunning(t *testing.T) {
	root := t.TempDir()
	stopped, err := Stop(root, time.Second)
	if err != nil {
		t.Fatalf("Stop on an idle root: %v", err)
	}
	if stopped {
		t.Fatal("Stop reported stopping something that was not running")
	}
}

func TestStopSignalsAndWaits(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn a helper process: %v", err)
	}
	defer cmd.Process.Kill()
	if err := os.WriteFile(PIDPath(root), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()

	stopped, err := Stop(root, 10*time.Second)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !stopped {
		t.Fatal("Stop did not report stopping a live process")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process was not actually terminated")
	}
	if _, err := os.Stat(PIDPath(root)); err == nil {
		t.Fatal("pid file should be removed after a successful stop")
	}
}

func TestSaveAndLoadConfig(t *testing.T) {
	root := t.TempDir()
	if _, ok := LoadConfig(root); ok {
		t.Fatal("empty root should have no saved config")
	}
	want := SavedConfig{Role: RoleAgent, Node: "mini", Controller: "10.0.0.5:7817", ServerName: "10.0.0.5"}
	SaveConfig(root, want)
	got, ok := LoadConfig(root)
	if !ok {
		t.Fatal("config did not round-trip")
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestLoadConfigRejectsGarbage(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "node.json"), []byte("{not json"), 0o600)
	if _, ok := LoadConfig(root); ok {
		t.Fatal("a corrupt config file should read as absent, not as a valid role")
	}
}

// The bootstrap service sits one port above the agent listener. If this ever
// disagrees with what `shome invite` prints, joining silently cannot connect.
func TestBootstrapAddr(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"0.0.0.0:7817", "0.0.0.0:7818"},
		{"127.0.0.1:9000", "127.0.0.1:9001"},
		{"[::]:7817", "[::]:7818"},
		{"nonsense", "0.0.0.0:7818"},
	} {
		if got := BootstrapAddr(tc.in); got != tc.want {
			t.Errorf("BootstrapAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHostPart(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"10.0.0.5:7817", "10.0.0.5"},
		{"mini.local:7817", "mini.local"},
		{"10.0.0.5", "10.0.0.5"},
		{"[fe80::1]:7817", "fe80::1"},
	} {
		if got := HostPart(tc.in); got != tc.want {
			t.Errorf("HostPart(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A node name becomes a path component in scratch directories, so it must not
// carry a DNS suffix picked up from the machine's hostname.
func TestHostnameIsShort(t *testing.T) {
	if h := Hostname(); h == "" {
		t.Fatal("Hostname returned empty")
	} else if filepath.Base(h) != h {
		t.Fatalf("Hostname %q is not usable as a path component", h)
	}
}

func TestCertHostsCoversLoopbackAndExtras(t *testing.T) {
	got := CertHosts("vpn.example.test, 100.64.0.1")
	want := map[string]bool{"localhost": false, "127.0.0.1": false,
		"vpn.example.test": false, "100.64.0.1": false}
	for _, h := range got {
		if _, ok := want[h]; ok {
			want[h] = true
		}
	}
	for h, found := range want {
		if !found {
			t.Errorf("certificate hosts missing %q; got %v", h, got)
		}
	}
	// Duplicates would be harmless but signal a bug in the dedupe.
	seen := map[string]bool{}
	for _, h := range got {
		if seen[h] {
			t.Errorf("duplicate certificate host %q", h)
		}
		seen[h] = true
	}
}
