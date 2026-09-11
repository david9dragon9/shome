package owner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func at(day time.Weekday, hh, mm int) time.Time {
	// 2026-08-16 is a Sunday; add days to land on the weekday we want.
	base := time.Date(2026, 8, 16, hh, mm, 0, 0, time.UTC)
	return base.AddDate(0, 0, int(day))
}

func mustPolicy(t *testing.T, y string) *Policy {
	t.Helper()
	p, err := ParsePolicy([]byte(y))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	return p
}

// ---------- schedule ----------

func TestOvernightWindowWraps(t *testing.T) {
	w, err := parseWindow("Mon-Fri 22:00-08:00")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		when time.Time
		want bool
		note string
	}{
		{at(time.Monday, 23, 0), true, "Monday night, inside"},
		{at(time.Tuesday, 2, 0), true, "early Tuesday, still Monday's window"},
		{at(time.Tuesday, 9, 0), false, "Tuesday morning, after it ends"},
		{at(time.Monday, 12, 0), false, "Monday midday, before it starts"},
		{at(time.Saturday, 23, 0), false, "Saturday is not in Mon-Fri"},
		{at(time.Saturday, 2, 0), true, "early Saturday belongs to Friday's window"},
	}
	for _, c := range cases {
		if got := w.contains(c.when); got != c.want {
			t.Errorf("%s: contains(%s) = %v, want %v", c.note, c.when.Format("Mon 15:04"), got, c.want)
		}
	}
}

func TestAllDayAndWildcardDays(t *testing.T) {
	w, _ := parseWindow("Sat-Sun *")
	if !w.contains(at(time.Saturday, 3, 0)) || !w.contains(at(time.Sunday, 20, 0)) {
		t.Error("weekend all-day window should always match at weekends")
	}
	if w.contains(at(time.Wednesday, 12, 0)) {
		t.Error("weekend window matched a Wednesday")
	}
	any, _ := parseWindow("* 09:00-17:00")
	if !any.contains(at(time.Wednesday, 10, 0)) || any.contains(at(time.Wednesday, 20, 0)) {
		t.Error("wildcard day with a time range misbehaved")
	}
}

func TestBadScheduleRejected(t *testing.T) {
	for _, bad := range []string{"Mon", "Funday 10:00-11:00", "Mon 25:00-26:00", "Mon 10:00", "Mon x-y"} {
		if _, err := parseWindow(bad); err == nil {
			t.Errorf("accepted bad window %q", bad)
		}
	}
}

// ---------- decisions ----------

func TestNoPolicyRuns(t *testing.T) {
	if d := Decide(nil, Signals{}); d.Action != Run {
		t.Errorf("no policy should allow work, got %s", d.Action)
	}
}

func TestOwnerActivityThrottles(t *testing.T) {
	p := mustPolicy(t, `
yield:
  on_user_input: {within: 30s, action: throttle}
`)
	busy := Decide(p, Signals{Now: at(time.Monday, 14, 0), UserIdle: 2 * time.Second})
	if busy.Action != Throttle {
		t.Errorf("owner typing should throttle, got %s (%s)", busy.Action, busy.Reason)
	}
	if !strings.Contains(busy.Reason, "owner active") {
		t.Errorf("reason should say why: %q", busy.Reason)
	}
	away := Decide(p, Signals{Now: at(time.Monday, 14, 0), UserIdle: 10 * time.Minute})
	if away.Action != Run {
		t.Errorf("idle owner should allow full speed, got %s", away.Action)
	}
}

func TestBatteryAndAppTriggers(t *testing.T) {
	p := mustPolicy(t, `
yield:
  on_battery: {action: suspend}
  on_app_running: {apps: [Xcode, "Final Cut Pro"], action: suspend}
`)
	if d := Decide(p, Signals{OnBattery: true}); d.Action != Suspend {
		t.Errorf("battery should suspend, got %s", d.Action)
	}
	d := Decide(p, Signals{RunningApps: []string{"Safari", "Xcode"}})
	if d.Action != Suspend || !strings.Contains(d.Reason, "Xcode") {
		t.Errorf("Xcode should suspend and be named, got %s (%s)", d.Action, d.Reason)
	}
	// Matching must be case-insensitive; process lists vary in capitalisation.
	if d := Decide(p, Signals{RunningApps: []string{"final cut pro"}}); d.Action != Suspend {
		t.Error("app matching should be case-insensitive")
	}
	if d := Decide(p, Signals{RunningApps: []string{"Safari"}}); d.Action != Run {
		t.Errorf("an unlisted app should not suspend, got %s", d.Action)
	}
}

func TestPressureThresholds(t *testing.T) {
	p := mustPolicy(t, `
yield:
  on_thermal: {above: serious, action: throttle}
  on_memory_pressure: {above: warn, action: suspend}
`)
	if d := Decide(p, Signals{Thermal: "fair"}); d.Action != Run {
		t.Errorf("fair thermal is below the threshold, got %s", d.Action)
	}
	if d := Decide(p, Signals{Thermal: "serious"}); d.Action != Throttle {
		t.Errorf("serious thermal should throttle, got %s", d.Action)
	}
	if d := Decide(p, Signals{Thermal: "critical"}); d.Action != Throttle {
		t.Errorf("critical is above serious and should also fire, got %s", d.Action)
	}
	if d := Decide(p, Signals{MemoryPressure: "critical"}); d.Action != Suspend {
		t.Errorf("critical memory pressure should suspend, got %s", d.Action)
	}
}

// The core guarantee: when several triggers fire, the owner is protected most.
func TestMostProtectiveActionWins(t *testing.T) {
	p := mustPolicy(t, `
yield:
  on_user_input: {within: 30s, action: throttle}
  on_battery: {action: suspend}
`)
	d := Decide(p, Signals{UserIdle: time.Second, OnBattery: true})
	if d.Action != Suspend {
		t.Errorf("throttle+suspend should resolve to suspend, got %s", d.Action)
	}
}

func TestAvailabilityWindowGatesEverything(t *testing.T) {
	p := mustPolicy(t, `
availability:
  schedule: ["Mon-Fri 22:00-08:00"]
`)
	if d := Decide(p, Signals{Now: at(time.Monday, 23, 30)}); d.Action != Run {
		t.Errorf("inside the window should run, got %s (%s)", d.Action, d.Reason)
	}
	d := Decide(p, Signals{Now: at(time.Monday, 14, 0)})
	if d.Action != Suspend {
		t.Errorf("outside the window should suspend, got %s", d.Action)
	}
	if !strings.Contains(d.Reason, "schedule") {
		t.Errorf("reason should mention the schedule: %q", d.Reason)
	}
}

func TestRequireConditions(t *testing.T) {
	p := mustPolicy(t, `
availability:
  require: [ac_power, screen_locked]
`)
	ok := Decide(p, Signals{OnBattery: false, ScreenLocked: true})
	if ok.Action != Run {
		t.Errorf("both conditions met should run, got %s (%s)", ok.Action, ok.Reason)
	}
	if d := Decide(p, Signals{OnBattery: true, ScreenLocked: true}); d.Action != Suspend {
		t.Errorf("on battery should suspend when ac_power required, got %s", d.Action)
	}
	if d := Decide(p, Signals{ScreenLocked: false}); d.Action != Suspend {
		t.Errorf("unlocked screen should suspend when screen_locked required, got %s", d.Action)
	}
}

// ---------- validation ----------

func TestPolicyValidationRejectsNonsense(t *testing.T) {
	for name, y := range map[string]string{
		"bad action":   "yield:\n  on_battery: {action: explode}\n",
		"bad duration": "yield:\n  on_user_input: {within: soon}\n",
		"bad level":    "yield:\n  on_thermal: {above: toasty}\n",
		"bad schedule": "availability:\n  schedule: [\"Someday 10:00-11:00\"]\n",
		"bad require":  "availability:\n  require: [moon_phase]\n",
		"bad gpu mode": "contribute:\n  gpu: sometimes\n",
		"bad resume":   "yield:\n  resume_after_idle: later\n",
	} {
		if _, err := ParsePolicy([]byte(y)); err == nil {
			t.Errorf("%s: accepted invalid policy", name)
		}
	}
}

func TestValidPolicyParses(t *testing.T) {
	p := mustPolicy(t, `
contribute:
  max_cores: 6
  max_mem_gb: 10
  max_disk_gb: 200
  gpu: exclusive
availability:
  schedule: ["Mon-Fri 22:00-08:00", "Sat-Sun *"]
  require: [ac_power]
yield:
  on_user_input: {within: 30s, action: throttle}
  on_battery: {action: suspend}
  on_thermal: {above: serious, action: throttle}
  on_app_running: {apps: [Xcode], action: suspend}
  resume_after_idle: 5m
`)
	if p.Contribute.MaxCores != 6 || p.Contribute.MaxMemGB != 10 {
		t.Errorf("contribute mis-parsed: %+v", p.Contribute)
	}
	if len(p.Availability.Schedule) != 2 {
		t.Errorf("schedule mis-parsed: %v", p.Availability.Schedule)
	}
	if p.Yield.OnUserInput == nil || p.Yield.OnUserInput.Within != "30s" {
		t.Errorf("yield mis-parsed: %+v", p.Yield.OnUserInput)
	}
}

// ---------- manager ----------

type fakeSensor struct{ s Signals }

func (f fakeSensor) Read() Signals { return f.s }

func (f fakeSensor) Live() bool { return true }

func TestPauseSurvivesAndOverridesEverything(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, fakeSensor{Signals{UserIdle: time.Hour}})
	if d := m.Evaluate(); d.Action != Run {
		t.Fatalf("expected Run with no policy, got %s", d.Action)
	}
	if err := m.Pause("taking my laptop out"); err != nil {
		t.Fatal(err)
	}
	d := m.Evaluate()
	if d.Action != Suspend {
		t.Errorf("pause should suspend, got %s", d.Action)
	}
	if !strings.Contains(d.Reason, "taking my laptop out") {
		t.Errorf("reason should carry the owner's words: %q", d.Reason)
	}
	// A new Manager must see the pause: it has to survive a daemon restart.
	if paused, _ := NewManager(dir, fakeSensor{}).Paused(); !paused {
		t.Error("pause did not survive a restart")
	}
	if err := m.Resume(); err != nil {
		t.Fatal(err)
	}
	if d := m.Evaluate(); d.Action != Run {
		t.Errorf("after resume expected Run, got %s", d.Action)
	}
	// Resume must be idempotent; an owner may run it twice.
	if err := m.Resume(); err != nil {
		t.Errorf("second resume errored: %v", err)
	}
}

func TestInvalidPolicyFailsClosed(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, PolicyFile), []byte("yield:\n  on_battery: {action: explode}\n"), 0o644)
	m := NewManager(dir, fakeSensor{})
	d := m.Evaluate()
	// A typo must not silently hand the machine to the cluster.
	if d.Action != Suspend {
		t.Errorf("invalid policy should fail closed, got %s (%s)", d.Action, d.Reason)
	}
	if !strings.Contains(d.Reason, "invalid") {
		t.Errorf("reason should say the file is invalid: %q", d.Reason)
	}
}

func TestCapCapacity(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, PolicyFile),
		[]byte("contribute:\n  max_cores: 6\n  max_mem_gb: 10\n  gpu: never\n"), 0o644)
	m := NewManager(dir, fakeSensor{})
	m.Evaluate()
	cpus, mem, gpus := m.CapCapacity(10, 16<<30, 1)
	if cpus != 6 {
		t.Errorf("cpus = %d, want 6", cpus)
	}
	if mem != 10<<30 {
		t.Errorf("mem = %d GiB, want 10", mem>>30)
	}
	if gpus != 0 {
		t.Errorf("gpus = %d; policy says never", gpus)
	}
	// Caps must never inflate what the machine actually has.
	cpus, mem, _ = m.CapCapacity(4, 8<<30, 0)
	if cpus != 4 || mem != 8<<30 {
		t.Errorf("cap raised capacity above reality: %d cpus, %d GiB", cpus, mem>>30)
	}
}

func TestPolicyReloadsOnEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, PolicyFile)
	os.WriteFile(path, []byte("contribute:\n  max_cores: 8\n"), 0o644)
	m := NewManager(dir, fakeSensor{})
	m.Evaluate()
	if c, _, _ := m.CapCapacity(10, 0, 0); c != 8 {
		t.Fatalf("initial cap = %d, want 8", c)
	}
	// The owner edits the file to reclaim their machine; no restart required.
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(path, []byte("contribute:\n  max_cores: 2\n"), 0o644)
	os.Chtimes(path, time.Now(), time.Now().Add(time.Second))
	m.Evaluate()
	if c, _, _ := m.CapCapacity(10, 0, 0); c != 2 {
		t.Errorf("cap after edit = %d, want 2", c)
	}
}

// A rule that cannot fire must be reported, not silently accepted. Someone
// writing "suspend on battery" on a laptop should not be left believing their
// machine is protected when the platform reports nothing.
type deadSensor struct{}

func (deadSensor) Read() Signals { return Signals{Now: time.Now(), UserIdle: 24 * time.Hour} }
func (deadSensor) Live() bool    { return false }

func TestDeadTriggersReportedWhenSensorIsBlind(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, PolicyFile), []byte(`
contribute:
  max_cores: 2
yield:
  on_battery: {action: suspend}
  on_user_input: {within: 30s, action: throttle}
`), 0o644)

	blind := NewManager(dir, deadSensor{})
	dead := blind.DeadTriggers()
	if len(dead) != 2 {
		t.Errorf("expected both rules reported as dead, got %v", dead)
	}
	if blind.SensorLive() {
		t.Error("SensorLive should be false for a stub sensor")
	}

	// With a real sensor the same policy is fine.
	live := NewManager(dir, fakeSensor{})
	if d := live.DeadTriggers(); len(d) != 0 {
		t.Errorf("a live sensor should report no dead rules, got %v", d)
	}
}

func TestNoPolicyMeansNoDeadTriggers(t *testing.T) {
	m := NewManager(t.TempDir(), deadSensor{})
	if d := m.DeadTriggers(); len(d) != 0 {
		t.Errorf("no policy should yield no warnings, got %v", d)
	}
}

// Pausing is the owner's veto over their own machine. It must work even on a
// machine that has never run anything, where the state directory does not yet
// exist -- failing there would deny someone control of their own hardware for
// a reason that has nothing to do with them.
func TestPauseWorksBeforeTheStateDirExists(t *testing.T) {
	root := filepath.Join(t.TempDir(), "never-used", "nested")
	m := NewManager(root, fakeSensor{})
	if err := m.Pause("mine"); err != nil {
		t.Fatalf("pause failed on a fresh machine: %v", err)
	}
	paused, reason := m.Paused()
	if !paused || reason != "mine" {
		t.Errorf("paused=%v reason=%q, want true/mine", paused, reason)
	}
	if err := m.Resume(); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if p, _ := m.Paused(); p {
		t.Error("still paused after resume")
	}
}
