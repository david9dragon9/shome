package owner

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Sensor reads the machine's current owner-activity signals.
// Sensor reads the machine's live state.
//
// Live reports whether it actually observes anything. A stub that always says
// "owner absent" is not merely less accurate -- it makes every reactive yield
// rule silently dead, which is worse than refusing to accept them, so callers
// must be able to tell the difference and say so.
type Sensor interface {
	Read() Signals
	Live() bool
}

// PolicyFile is the owner-editable policy, relative to the node's state dir.
const PolicyFile = "node.yaml"

// PauseFile marks a manual pause.
//
// A file rather than a command to the controller, deliberately: the owner must
// be able to reclaim their machine when the controller is unreachable, and the
// pause has to survive a reboot. A pause that depends on the cluster being
// healthy is not a pause.
const PauseFile = "paused"

// Manager tracks the current owner decision for a node.
type Manager struct {
	root   string
	sensor Sensor

	mu       sync.Mutex
	policy   *Policy
	loadedAt time.Time
	current  Decision
	// invalid is set when node.yaml exists but does not parse. Held as state
	// rather than folded into `current`, because Evaluate recomputes `current`
	// on every tick and would otherwise overwrite the fail-closed decision.
	invalid string
}

func NewManager(root string, sensor Sensor) *Manager {
	return &Manager{root: root, sensor: sensor,
		current: Decision{Run, "no policy set"}}
}

func (m *Manager) policyPath() string { return filepath.Join(m.root, PolicyFile) }
func (m *Manager) pausePath() string  { return filepath.Join(m.root, PauseFile) }

// Pause records a manual pause with a reason.
func (m *Manager) Pause(reason string) error {
	if reason == "" {
		reason = "paused by the machine owner"
	}
	// Create the state directory if it is missing. Pausing is the owner's
	// veto over their own machine and must not fail on a technicality --
	// least of all "that directory does not exist yet", which is exactly the
	// state a machine is in before it has ever run anything.
	if err := os.MkdirAll(m.root, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", m.root, err)
	}
	return os.WriteFile(m.pausePath(), []byte(reason+"\n"), 0o644)
}

// Resume clears a manual pause.
func (m *Manager) Resume() error {
	err := os.Remove(m.pausePath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Paused reports whether a manual pause is in force, and why.
func (m *Manager) Paused() (bool, string) {
	b, err := os.ReadFile(m.pausePath())
	if err != nil {
		return false, ""
	}
	reason := "paused by the machine owner"
	if s := string(b); len(s) > 0 {
		reason = trimLine(s)
	}
	return true, reason
}

func trimLine(s string) string {
	for i, c := range s {
		if c == '\n' || c == '\r' {
			return s[:i]
		}
	}
	return s
}

// reloadPolicy re-reads node.yaml when it changes.
//
// Reloaded rather than read once at startup: the owner edits this file to
// reclaim their machine, and making them restart a daemon to be heard would
// undermine the point.
func (m *Manager) reloadPolicy() {
	fi, err := os.Stat(m.policyPath())
	if err != nil {
		m.mu.Lock()
		m.policy, m.invalid = nil, ""
		m.mu.Unlock()
		return
	}
	m.mu.Lock()
	unchanged := !m.loadedAt.IsZero() && fi.ModTime().Equal(m.loadedAt)
	m.mu.Unlock()
	if unchanged {
		return
	}
	b, err := os.ReadFile(m.policyPath())
	if err != nil {
		return
	}
	p, err := ParsePolicy(b)
	if err != nil {
		// Fail closed. A typo must not silently hand the machine to the
		// cluster, and the owner needs to be told which line is wrong.
		m.mu.Lock()
		m.invalid, m.loadedAt = err.Error(), fi.ModTime()
		m.mu.Unlock()
		return
	}
	m.mu.Lock()
	m.policy, m.loadedAt, m.invalid = p, fi.ModTime(), ""
	m.mu.Unlock()
}

// Evaluate recomputes the current decision.
func (m *Manager) Evaluate() Decision {
	m.reloadPolicy()
	if paused, reason := m.Paused(); paused {
		d := Decision{Suspend, reason}
		m.mu.Lock()
		m.current = d
		m.mu.Unlock()
		return d
	}
	m.mu.Lock()
	p, invalid := m.policy, m.invalid
	m.mu.Unlock()

	if invalid != "" {
		d := Decision{Suspend, "policy file is invalid, refusing to run cluster work: " + invalid}
		m.mu.Lock()
		m.current = d
		m.mu.Unlock()
		return d
	}

	d := Decide(p, m.sensor.Read())
	m.mu.Lock()
	m.current = d
	m.mu.Unlock()
	return d
}

// Current returns the last computed decision without re-reading anything.
func (m *Manager) Current() Decision {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// Policy returns the loaded policy, or nil.
func (m *Manager) Policy() *Policy {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.policy
}

// CapCapacity reduces advertised resources to what the owner permits.
//
// Applied to what the node advertises rather than enforced per job: if shome
// never claims more than its share exists, the scheduler will not place work
// that would overrun it in the first place.
func (m *Manager) CapCapacity(cpus int, memBytes int64, gpus int) (int, int64, int) {
	m.mu.Lock()
	p := m.policy
	m.mu.Unlock()
	if p == nil {
		return cpus, memBytes, gpus
	}
	if p.Contribute.MaxCores > 0 && p.Contribute.MaxCores < cpus {
		cpus = p.Contribute.MaxCores
	}
	if p.Contribute.MaxMemGB > 0 {
		if capped := int64(p.Contribute.MaxMemGB * float64(1<<30)); capped < memBytes {
			memBytes = capped
		}
	}
	if p.Contribute.GPU == "never" {
		gpus = 0
	}
	return cpus, memBytes, gpus
}

// Describe renders the current state for `shome status`.
func (m *Manager) Describe() string {
	d := m.Current()
	if paused, reason := m.Paused(); paused {
		return fmt.Sprintf("PAUSED - %s", reason)
	}
	switch d.Action {
	case Run:
		return "contributing - " + d.Reason
	default:
		return fmt.Sprintf("%s - %s", d.Action, d.Reason)
	}
}

// DeadTriggers lists reactive yield rules that cannot fire on this machine,
// because its sensor does not observe the signals they depend on.
//
// Silently accepting a rule that can never run is the worst option: someone
// writes "suspend on battery" on a laptop, believes their machine is
// protected, and it never is.
func (m *Manager) DeadTriggers() []string {
	if m.sensor == nil || m.sensor.Live() {
		return nil
	}
	// Read from disk rather than the cache: callers reach for this to explain
	// a policy the user has just written, which may not have been loaded yet.
	m.reloadPolicy()
	p := m.Policy()
	if p == nil {
		return nil
	}
	var dead []string
	for name, t := range p.triggers() {
		if t != nil {
			dead = append(dead, name)
		}
	}
	sort.Strings(dead)
	return dead
}

// SensorLive reports whether this machine observes live owner activity.
func (m *Manager) SensorLive() bool { return m.sensor != nil && m.sensor.Live() }

// Signals returns the current raw sensor reading.
//
// Exposed so the dashboard reports the same numbers the yield policy acts on.
// Reading the hardware separately would let the display claim the owner is
// away at the moment the policy suspends every job for owner activity, and
// that contradiction is exactly what erodes trust in a machine you lent out.
func (m *Manager) Signals() Signals {
	if m.sensor == nil {
		return Signals{Now: time.Now()}
	}
	return m.sensor.Read()
}

// CheckDisk reports why new work should be refused on disk grounds, or "" to
// accept it. Consulted alongside the other yield rules.
func (m *Manager) CheckDisk(st DiskState) string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	p := m.policy
	m.mu.Unlock()
	return p.CheckDisk(st)
}

// DescribeDisk renders the disk position for `shome status`.
func (m *Manager) DescribeDisk(st DiskState) string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	p := m.policy
	m.mu.Unlock()
	return p.DescribeDisk(st)
}
