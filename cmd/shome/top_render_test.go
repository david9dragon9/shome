package main

import (
	"errors"
	"os"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/davidwu/shome/internal/agent"
	"github.com/davidwu/shome/internal/ctl"
	"github.com/davidwu/shome/internal/platform"
	"github.com/davidwu/shome/internal/tui"
)

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

func visible(s string) int { return len([]rune(ansi.ReplaceAllString(s, ""))) }

func sampleDash() *ctl.Dashboard {
	now := time.Now()
	tel := platform.UnknownTelemetry(now)
	tel.CPUPercent, tel.MemUsedBytes, tel.MemTotalBytes = 62.5, 6<<30, 16<<30
	tel.Thermal, tel.OnBattery, tel.UserIdleSec = "serious", true, 3

	var hist []ctl.Sample
	for i := 0; i < 40; i++ {
		hist = append(hist, ctl.Sample{At: now, CPU: float64(i % 100), MemPct: 40, GPU: -1})
	}
	return &ctl.Dashboard{
		At: now,
		Totals: ctl.ClusterTotals{
			Nodes: 3, NodesUp: 2, NodesDown: 1, CPUs: 20, UsedCPUs: 12,
			MemMiB: 32768, UsedMemMiB: 8192, GPUs: 2, GPUMemMiB: 24000,
			CPUPercent: 62.5, MemPercent: 37.5, Reporting: 2, Running: 2, Pending: 1,
		},
		Nodes: []ctl.NodeDash{
			{NodeInfo: ctl.NodeInfo{Name: "a-very-long-machine-name", State: "UP", Tier: "full",
				CPUs: 10, UsedCPUs: 6, MemMiB: 16384, UsedMemMiB: 4096, GPUs: 1},
				Telemetry: tel, HasTelem: true, History: hist, GPUKind: "metal", RunningJobs: 2},
			{NodeInfo: ctl.NodeInfo{Name: "gone", State: "DOWN", Tier: "limited",
				Reason: "missed heartbeats and then some more text to overflow"}},
		},
		Jobs: []ctl.JobView{
			{ID: 1, Label: "1", Name: "a-long-job-name-here", User: "ada", State: "RUNNING",
				Elapsed: "00:01:02", CPUs: 4, MemMiB: 8192, LiveMiB: 4096, Node: "a-very-long-machine-name"},
			{ID: 2, Label: "2_10", Name: "sweep", User: "someone-else", State: "PENDING",
				Elapsed: "00:00:00", CPUs: 1, MemMiB: 512, LiveMiB: -1,
				Reason: "waiting for resources on a busy cluster", Held: true},
		},
		Recent: []ctl.JobView{
			{ID: 0, Label: "0", Name: "old", State: "FAILED", Elapsed: "00:00:01",
				Reason: "exit code 1 with quite a long explanatory message attached"},
		},
		Alerts: []string{"gone is down: missed heartbeats", "a-very-long-machine-name is on battery"},
	}
}

// The layout must never emit a line wider than the terminal. One over-wide
// line wraps and shears everything below it, and it only happens in a narrow
// window -- which is not where anyone is looking when they add a section.
func TestRenderNeverExceedsWidth(t *testing.T) {
	ui := &topUI{dash: sampleDash(), interval: time.Second}
	for _, noColor := range []bool{true, false} {
		tui.NoColor = noColor
		for _, w := range []int{40, 47, 60, 80, 100, 132, 200} {
			for _, h := range []int{8, 12, 24, 40, 60} {
				for _, help := range []bool{false, true} {
					ui.showHelp = help
					for i, line := range strings.Split(ui.render(w, h, true), "\n") {
						if got := visible(line); got > w {
							t.Fatalf("w=%d h=%d help=%v color=%v: line %d is %d cells:\n%q",
								w, h, help, !noColor, i, got, ansi.ReplaceAllString(line, ""))
						}
					}
				}
			}
		}
	}
	tui.NoColor = true
}

// And never more lines than the window has, or the top of the frame scrolls
// away and the header is never visible.
func TestRenderRespectsHeight(t *testing.T) {
	tui.NoColor = true
	ui := &topUI{dash: sampleDash(), interval: time.Second}
	for _, h := range []int{4, 8, 12, 24, 50} {
		lines := strings.Split(strings.TrimRight(ui.render(100, h, true), "\n"), "\n")
		if len(lines) > h {
			t.Errorf("h=%d produced %d lines", h, len(lines))
		}
	}
}

func TestRenderShowsTheEssentials(t *testing.T) {
	tui.NoColor = true
	ui := &topUI{dash: sampleDash(), interval: time.Second}
	out := ui.render(160, 50, true)
	for _, want := range []string{
		"a-very-long-machine-name", // node name, in full at a wide width
		"DOWN",                     // a dead node is not hidden
		"RUNNING",
		"battery", // owner signal
		"thermal", // owner signal
		"2/3 nodes up",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered frame is missing %q", want)
		}
	}
}

// An empty cluster must say something useful rather than rendering a bare
// frame that looks broken.
func TestRenderEmptyCluster(t *testing.T) {
	tui.NoColor = true
	ui := &topUI{dash: &ctl.Dashboard{At: time.Now()}, interval: time.Second}
	out := ui.render(80, 24, true)
	if !strings.Contains(out, "shome invite") {
		t.Errorf("an empty cluster should point at 'shome invite':\n%s", out)
	}
}

func TestRenderBeforeFirstFetch(t *testing.T) {
	tui.NoColor = true
	ui := &topUI{interval: time.Second}
	if out := ui.render(80, 24, true); !strings.Contains(out, "connecting") {
		t.Errorf("got %q", out)
	}
}

func TestFilterMatchesAcrossFields(t *testing.T) {
	ui := &topUI{dash: sampleDash()}
	for _, tc := range []struct {
		filter string
		want   int
	}{
		{"", 2}, {"sweep", 1}, {"ada", 1}, {"RUNNING", 1},
		{"a-very-long", 1}, {"2_10", 1}, {"nothing-matches-this", 0},
	} {
		ui.filter = tc.filter
		if got := len(ui.visibleJobs(ui.dash)); got != tc.want {
			t.Errorf("filter %q matched %d jobs, want %d", tc.filter, got, tc.want)
		}
	}
}

// Selection must stay inside the list even as the filter shrinks it, or the
// next keystroke acts on a job that is not on screen.
func TestSelectionStaysInBounds(t *testing.T) {
	ui := &topUI{dash: sampleDash(), focus: paneJobs}
	for i := 0; i < 10; i++ {
		ui.move(1)
	}
	if _, ok := ui.selectedJob(); !ok {
		t.Fatal("selection ran off the end of the list")
	}
	if ui.jobSel != 1 {
		t.Errorf("jobSel = %d, want 1 (clamped to the last job)", ui.jobSel)
	}
	for i := 0; i < 10; i++ {
		ui.move(-1)
	}
	if ui.jobSel != 0 {
		t.Errorf("jobSel = %d after moving up repeatedly, want 0", ui.jobSel)
	}
	ui.focus = paneNodes
	for i := 0; i < 10; i++ {
		ui.move(1)
	}
	if _, ok := ui.selectedNode(); !ok {
		t.Fatal("node selection ran off the end")
	}
}

func TestSelectedJobEmptyQueue(t *testing.T) {
	ui := &topUI{dash: &ctl.Dashboard{}, focus: paneJobs}
	if _, ok := ui.selectedJob(); ok {
		t.Error("an empty queue reported a selected job")
	}
	if _, ok := ui.selectedNode(); ok {
		t.Error("an empty cluster reported a selected node")
	}
}

func TestClampInterval(t *testing.T) {
	if got := clampInterval(time.Millisecond); got != minInterval {
		t.Errorf("got %v, want the floor %v", got, minInterval)
	}
	if got := clampInterval(time.Hour); got != maxInterval {
		t.Errorf("got %v, want the ceiling %v", got, maxInterval)
	}
	if got := clampInterval(3 * time.Second); got != 3*time.Second {
		t.Errorf("a reasonable interval was altered to %v", got)
	}
}

// A failed join on macOS should name the cause people never guess. Local
// Network permission produces an instant connection failure from a terminal
// whose internet works, which reads as a firewall or routing fault.
func TestJoinHintNamesLocalNetworkPermission(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the hint is macOS-specific")
	}
	for _, msg := range []string{
		"dial tcp 10.0.0.5:7817: connect: connection refused",
		"no route to host",
		"i/o timeout",
		"connect: host is unreachable",
	} {
		if h := joinHint(errors.New(msg)); !strings.Contains(h, "Local Network") {
			t.Errorf("no hint for %q", msg)
		}
	}
	// Not every failure is a network failure; a bad token must not be blamed
	// on a permission the user has probably already granted.
	if h := joinHint(errors.New("join refused: token has expired")); h != "" {
		t.Errorf("hinted at Local Network for a token error:\n%s", h)
	}
}

// The owner's monitor is a separate frame from the admin dashboard, and has
// the same obligation: never a line wider than the terminal, or it wraps and
// shears everything below it.
func sampleStatus() agent.LocalStatus {
	tel := platform.UnknownTelemetry(time.Now())
	tel.CPUPercent, tel.LoadAvg1 = 62.5, 3.25
	tel.MemUsedBytes, tel.MemTotalBytes = 11<<30, 16<<30
	tel.DiskTotalBytes, tel.DiskFreeBytes = 900<<30, 700<<30
	tel.ShomeDiskBytes, tel.ShomeInodes = 3<<30, 12000
	tel.InodesTotal, tel.InodesFree = 5_000_000, 4_000_000
	tel.NetRxBytesPerSec, tel.NetTxBytesPerSec = 1<<20, 512<<10
	tel.OnBattery, tel.Thermal, tel.UserIdleSec = true, "serious", 3
	tel.Uptime = 50 * time.Hour
	return agent.LocalStatus{
		At: time.Now(), Node: "a-very-long-machine-name", Connected: true,
		OwnerAction: "throttle", OwnerReason: "you are using the machine",
		AdvertisedCPUs: 6, AdvertisedMemMiB: 10240, AdvertisedGPUs: 0,
		TotalCPUs: 10, TotalMemMiB: 16384, TotalGPUs: 1,
		Tier: "standard", Limitations: []string{"no hard CPU quota; admission control only"},
		Telemetry: tel, Completed: 12,
		Jobs: []agent.LocalJob{
			{ID: 41, Name: "a-long-job-name", User: "someone-else", CPUs: 4,
				MemMiB: 8192, LiveMiB: 4096, Procs: 17, ElapsedSec: 3725, Throttled: true},
			{ID: 42, Name: "sweep", User: "ada", CPUs: 1, MemMiB: 512, LiveMiB: 96,
				Procs: 2, ElapsedSec: 12},
		},
	}
}

func TestMonitorRenderNeverExceedsWidth(t *testing.T) {
	ui := &monitorUI{st: sampleStatus(), interval: time.Second, root: t.TempDir()}
	for _, noColor := range []bool{true, false} {
		tui.NoColor = noColor
		for _, w := range []int{40, 60, 80, 100, 160} {
			for _, h := range []int{8, 14, 24, 40} {
				for _, help := range []bool{false, true} {
					ui.showHelp = help
					for i, line := range strings.Split(ui.render(w, h, true), "\n") {
						if got := visible(line); got > w {
							t.Fatalf("w=%d h=%d help=%v: line %d is %d cells:\n%q",
								w, h, help, i, got, ansi.ReplaceAllString(line, ""))
						}
					}
				}
			}
		}
	}
	tui.NoColor = true
}

func TestMonitorShowsWhatTheOwnerNeeds(t *testing.T) {
	tui.NoColor = true
	ui := &monitorUI{st: sampleStatus(), interval: time.Second, root: t.TempDir()}
	out := ui.render(150, 40, true)
	for _, want := range []string{
		"a-very-long-machine-name",
		"THROTTLED", // their disposition, the headline
		"6 of 10",   // the gap their policy makes
		"(your limit)",
		"on battery",
		"thermal serious",
		"someone-else", // whose work is on their machine
		"throttled for you",
		"shome config", // how to change it
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the owner's view is missing %q", want)
		}
	}
	// Cluster-wide figures belong to `shome top`, not here.
	if strings.Contains(out, "nodes up") {
		t.Error("the owner's view is showing cluster-wide state")
	}
}

// A paused machine must say so unmistakably, and offer the way back.
func TestMonitorShowsPaused(t *testing.T) {
	tui.NoColor = true
	st := sampleStatus()
	st.Paused, st.PauseReason = true, "stepping away"
	ui := &monitorUI{st: st, interval: time.Second, root: t.TempDir()}
	out := ui.render(100, 30, true)
	if !strings.Contains(out, "PAUSED") || !strings.Contains(out, "stepping away") {
		t.Errorf("a paused machine did not say so:\n%s", out)
	}
	if !strings.Contains(out, "r resume") {
		t.Error("no way back offered")
	}
}

// Stale figures must be called out. An agent that has stopped writing is
// exactly when believing the last numbers would be worst.
func TestMonitorFlagsStaleStatus(t *testing.T) {
	tui.NoColor = true
	st := sampleStatus()
	st.At = time.Now().Add(-10 * time.Minute)
	ui := &monitorUI{st: st, interval: time.Second, root: t.TempDir()}
	out := ui.render(100, 30, true)
	if !strings.Contains(out, "not current") && !strings.Contains(out, "stale") {
		t.Errorf("stale figures were shown as current:\n%s", out)
	}
}

// A disconnected node still has an owner, and must still show them what is
// happening on their machine.
func TestMonitorWorksDisconnected(t *testing.T) {
	tui.NoColor = true
	st := sampleStatus()
	st.Connected, st.LastError = false, "dial tcp 10.0.0.5:7817: connect: no route to host"
	ui := &monitorUI{st: st, interval: time.Second, root: t.TempDir()}
	out := ui.render(120, 30, true)
	if !strings.Contains(out, "cannot reach the controller") {
		t.Error("a disconnected agent did not say so")
	}
	// And the local figures are still there, because they are local.
	if !strings.Contains(out, "memory") {
		t.Error("local utilisation was hidden because the cluster was unreachable")
	}
}

// With no status file at all, explain rather than render an empty frame.
func TestMonitorNoStatusFile(t *testing.T) {
	tui.NoColor = true
	ui := &monitorUI{err: os.ErrNotExist, interval: time.Second, root: "/nowhere"}
	out := ui.render(80, 24, false)
	for _, want := range []string{"No local status", "shome status", "shome up"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q from:\n%s", want, out)
		}
	}
}
