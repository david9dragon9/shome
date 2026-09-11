package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/davidwu/shome/internal/ctl"
	"github.com/davidwu/shome/internal/daemon"
	"github.com/davidwu/shome/internal/owner"
	"github.com/davidwu/shome/internal/platform"
	"github.com/davidwu/shome/internal/tui"
)

// ownerMgr operates on this machine's local state directory.
//
// Deliberately local and controller-free: the owner must be able to reclaim
// their machine when the cluster is unreachable, so pause and resume never
// make a network call.
func ownerMgr() *owner.Manager {
	return owner.NewManager(ctl.DefaultRoot(), owner.DarwinSensor{})
}

func ownerPause(args []string) error {
	reason := strings.Join(args, " ")
	m := ownerMgr()
	if err := m.Pause(reason); err != nil {
		return fmt.Errorf("could not pause: %w", err)
	}
	fmt.Println("paused: this machine will stop running cluster work")
	fmt.Println("  running jobs are suspended, not killed; they resume where they left off")
	fmt.Println("  undo with: shome resume")
	return nil
}

func ownerResume(args []string) error {
	m := ownerMgr()
	if err := m.Resume(); err != nil {
		return fmt.Errorf("could not resume: %w", err)
	}
	// Lifting the pause does not mean work starts: a policy rule may still be
	// holding the node. Saying "this machine will contribute again" and then
	// silently not contributing is the kind of thing that makes people
	// distrust the whole mechanism, so check and say which rule applies.
	if d := m.Evaluate(); d.Action != owner.Run {
		fmt.Println("pause lifted, but your policy is still holding this machine:")
		fmt.Printf("  %s - %s\n", d.Action, d.Reason)
		fmt.Println("  it will contribute as soon as that no longer applies")
		return nil
	}
	fmt.Println("resumed: this machine is contributing again")
	return nil
}

func ownerStatus(args []string) error {
	m := ownerMgr()
	defer warnDeadTriggers(m)
	d := m.Evaluate()

	// Lead with whether shome is even running here. It is the first thing
	// anyone wants to know, and previously the only way to find out was to go
	// looking for the process.
	root := ctl.DefaultRoot()
	saved, hasSaved := daemon.LoadConfig(root)
	pid, running := daemon.Status(root)
	switch {
	case running && hasSaved && saved.Role == daemon.RoleController:
		fmt.Printf("shome: running as the controller, node %q (pid %d)\n", saved.Node, pid)
	case running && hasSaved:
		fmt.Printf("shome: running as node %q, connected to %s (pid %d)\n",
			saved.Node, saved.Controller, pid)
	case running:
		fmt.Printf("shome: running (pid %d)\n", pid)
	case hasSaved:
		fmt.Printf("shome: not running. This machine is set up as a %s -- start it with: shome up\n", saved.Role)
	default:
		fmt.Printf("shome: not running, and this machine is not part of a cluster.\n")
		fmt.Printf("       start one here with 'shome up', or join one with 'shome join'.\n")
	}
	fmt.Printf("state: %s\n\n", root)

	fmt.Printf("this machine: %s\n", m.Describe())

	s := owner.DarwinSensor{}.Read()
	fmt.Printf("\nsignals\n")
	fmt.Printf("  last input      %s ago\n", s.UserIdle.Round(1e9))
	fmt.Printf("  power           %s\n", map[bool]string{true: "battery", false: "AC"}[s.OnBattery])
	fmt.Printf("  screen          %s\n", map[bool]string{true: "locked", false: "unlocked"}[s.ScreenLocked])
	fmt.Printf("  thermal         %s\n", s.Thermal)
	fmt.Printf("  memory pressure %s\n", s.MemoryPressure)

	// Disk is reported here because it is the one contribution limit whose
	// position an owner cannot otherwise see: cores and memory show up in
	// sinfo as advertised capacity, while disk only manifests as work being
	// refused.
	if t, ok := localTelemetry(); ok {
		fmt.Printf("  %s\n", m.DescribeDisk(ownerDiskState(t)))
		if t.KnownInodes() {
			fmt.Printf("  files          shome holds %s; %s free on the volume\n",
				count(t.ShomeInodes), count(t.InodesFree))
		}
		if t.KnownNet() {
			fmt.Printf("  network        %s/s in, %s/s out\n",
				bytesShort(int64(t.NetRxBytesPerSec)), bytesShort(int64(t.NetTxBytesPerSec)))
		}
		if why := m.CheckDisk(ownerDiskState(t)); why != "" {
			fmt.Printf("\n  %s\n", tui.C(tui.BrightYellow,
				"not accepting new work: "+why))
		}
	}

	p := m.Policy()
	path := filepath.Join(ctl.DefaultRoot(), owner.PolicyFile)
	if p == nil {
		fmt.Printf("\nno policy at %s -- shome may use this machine freely.\n", path)
		fmt.Println("write one with: shome contribute --example > " + path)
		return nil
	}
	fmt.Printf("\npolicy (%s)\n", path)
	if p.Contribute.MaxCores > 0 {
		fmt.Printf("  cores      at most %d\n", p.Contribute.MaxCores)
	}
	if p.Contribute.MaxMemGB > 0 {
		fmt.Printf("  memory     at most %.0f GiB\n", p.Contribute.MaxMemGB)
	}
	if p.Contribute.GPU != "" {
		fmt.Printf("  gpu        %s\n", p.Contribute.GPU)
	}
	for _, w := range p.Availability.Schedule {
		fmt.Printf("  available  %s\n", w)
	}
	for _, r := range p.Availability.Require {
		fmt.Printf("  requires   %s\n", r)
	}
	_ = d
	return nil
}

const examplePolicy = `# shome node policy -- this machine's contract with the cluster.
#
# You own this file. A cluster admin can drain your node, but cannot override
# what you set here. Edits take effect within a few seconds; no restart needed.

contribute:
  max_cores: 6          # never use more than 6 of this machine's cores
  max_mem_gb: 10
  max_disk_gb: 200      # shome's own files; enforced by refusing new work
  max_inodes: 500000    # files and directories; runs out separately from space
  min_free_disk_gb: 20  # stop before the disk does, whatever is using it
  gpu: exclusive        # exclusive | shared | never

availability:
  # When the machine may participate at all. Overnight ranges wrap midnight.
  schedule: ["Mon-Fri 22:00-08:00", "Sat-Sun *"]
  require: [ac_power]   # ac_power | screen_locked

yield:
  # Reactive rules. When several apply, the most protective one wins.
  on_user_input:      {within: 30s, action: throttle}
  on_battery:         {action: suspend}
  on_thermal:         {above: serious, action: throttle}
  on_memory_pressure: {above: warn, action: suspend}
  on_app_running:     {apps: [Xcode, "Final Cut Pro"], action: suspend}
  resume_after_idle:  5m
`

func ownerContribute(args []string) error {
	for _, a := range args {
		if a == "--example" {
			fmt.Print(examplePolicy)
			return nil
		}
	}
	path := filepath.Join(ctl.DefaultRoot(), owner.PolicyFile)
	if _, err := os.Stat(path); err == nil {
		fmt.Printf("policy already exists at %s\n", path)
		fmt.Println("edit it directly, or see a fresh example with: shome contribute --example")
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(examplePolicy), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote a starter policy to %s\n", path)
	fmt.Println("edit it to taste; changes are picked up automatically")
	return nil
}

// warnDeadTriggers says plainly when a policy asks for something this machine
// cannot detect, rather than letting the rule sit there looking effective.
func warnDeadTriggers(m *owner.Manager) {
	dead := m.DeadTriggers()
	if len(dead) == 0 {
		return
	}
	fmt.Printf("\nWARNING: these yield rules cannot fire on this machine:\n")
	for _, d := range dead {
		fmt.Printf("  %s\n", d)
	}
	fmt.Printf("shome does not read live power, input or pressure signals on %s yet.\n", runtime.GOOS)
	fmt.Printf("Contribution caps, availability schedules and `shome pause` all still work.\n")
}

// localTelemetry reads this machine's own metrics directly, without going
// through the controller.
//
// The owner commands must work when the cluster does not: `shome status` on a
// machine whose controller is unreachable is exactly when somebody wants to
// know what shome is doing to their disk.
func localTelemetry() (platform.Telemetry, bool) {
	// The installation root, which is what a contribution cap bounds: the
	// database, archived output and user storage count as shome's footprint
	// just as much as job scratch does.
	root := ctl.DefaultRoot()
	be, err := newLocalBackend(root)
	if err != nil {
		return platform.Telemetry{}, false
	}
	// Twice, briefly apart. CPU and throughput are rates, so a single reading
	// has nothing to compare against and reports them as unmeasured -- which
	// in a one-shot command means never showing them at all.
	ctx := context.Background()
	platform.Sample(ctx, be, time.Now())
	time.Sleep(250 * time.Millisecond)
	t := platform.Sample(ctx, be, time.Now())
	return t, t.KnownDisk()
}

func ownerDiskState(t platform.Telemetry) owner.DiskState {
	return owner.DiskState{
		ShomeBytes:  t.ShomeDiskBytes,
		ShomeInodes: t.ShomeInodes,
		FreeBytes:   t.DiskFreeBytes,
		FreeInodes:  t.InodesFree,
		Known:       t.KnownDisk(),
	}
}

func count(n int64) string {
	if n < 0 {
		return "unknown"
	}
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	default:
		return fmt.Sprint(n)
	}
}

func bytesShort(b int64) string {
	if b < 0 {
		return "-"
	}
	// Up to terabytes: disk limits are set in gigabytes, and stopping at MB
	// turned a 100 GiB allowance into "102400.0 MB" for anyone reading it.
	switch {
	case b >= 1<<40:
		return fmt.Sprintf("%.1f TB", float64(b)/(1<<40))
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
