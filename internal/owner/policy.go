// Package owner implements the machine owner's control over what shome may use.
//
// The plan's first design principle is that owner sovereignty is
// non-negotiable: any conflict between cluster throughput and the owner's
// experience resolves toward the owner, automatically and without asking. A
// cluster admin can drain a node, but cannot override the policy its owner set.
//
// The decision logic here is a pure function of policy plus observed signals,
// so it is testable without a Mac, a battery, or a busy afternoon.
package owner

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Action is what the agent should do with cluster work right now.
type Action string

const (
	// Run: cluster work proceeds normally.
	Run Action = "run"
	// Throttle: keep running but yield aggressively to the owner.
	Throttle Action = "throttle"
	// Suspend: stop the work but keep it resident, ready to continue.
	Suspend Action = "suspend"
	// Drain: accept no new work; let what is running finish.
	Drain Action = "drain"
)

// severity orders actions so the most protective one wins when several
// triggers fire at once.
func severity(a Action) int {
	switch a {
	case Run:
		return 0
	case Drain:
		return 1
	case Throttle:
		return 2
	case Suspend:
		return 3
	}
	return 0
}

// Policy is the owner's contract with the cluster, read from node.yaml.
// omitempty throughout: this file is the owner's interface, and a command
// that rewrote it with every unset field spelled out as null would turn a
// short, readable policy into a wall of noise after one edit.
type Policy struct {
	Contribute   Contribute   `yaml:"contribute,omitempty"`
	Availability Availability `yaml:"availability,omitempty"`
	Yield        Yield        `yaml:"yield,omitempty"`
}

// Contribute caps what shome may ever use, regardless of what is idle.
type Contribute struct {
	// MaxCores of 0 means "unset"; Fraction applies when it is.
	MaxCores  int     `yaml:"max_cores,omitempty"`
	MaxMemGB  float64 `yaml:"max_mem_gb,omitempty"`
	MaxDiskGB float64 `yaml:"max_disk_gb,omitempty"`
	// MaxInodes bounds the number of files and directories shome may create.
	//
	// Separate from MaxDiskGB because they run out independently: a job
	// writing a million empty files uses no meaningful space and can still
	// make a filesystem unusable, and a cap in gigabytes says nothing about
	// it. 0 means unset.
	MaxInodes int64 `yaml:"max_inodes,omitempty"`
	// MinFreeDiskGB is a floor on what shome leaves alone, regardless of its
	// own usage.
	//
	// A contribution cap alone does not protect the machine: shome could be
	// well inside its 200 GB while something else has taken the disk to
	// nothing, and the next job still fails for want of space. This is the
	// owner saying "stop before the disk does".
	MinFreeDiskGB float64 `yaml:"min_free_disk_gb,omitempty"`
	// GPU is "exclusive", "shared" or "never".
	GPU string `yaml:"gpu,omitempty"`
}

// Availability restricts when the node participates at all.
type Availability struct {
	// Schedule entries look like "Mon-Fri 22:00-08:00" or "Sat-Sun *".
	Schedule []string `yaml:"schedule,omitempty"`
	// Require lists conditions that must hold: ac_power, screen_locked.
	Require []string `yaml:"require,omitempty"`
}

// Yield describes reactive throttling.
type Yield struct {
	OnUserInput      *Trigger `yaml:"on_user_input,omitempty"`
	OnBattery        *Trigger `yaml:"on_battery,omitempty"`
	OnThermal        *Trigger `yaml:"on_thermal,omitempty"`
	OnMemoryPressure *Trigger `yaml:"on_memory_pressure,omitempty"`
	OnAppRunning     *Trigger `yaml:"on_app_running,omitempty"`
	ResumeAfterIdle  string   `yaml:"resume_after_idle,omitempty"`
}

// Trigger is one reactive rule.
type Trigger struct {
	Action string   `yaml:"action,omitempty"` // throttle | suspend | drain
	Within string   `yaml:"within,omitempty"` // for on_user_input: "30s"
	Above  string   `yaml:"above,omitempty"`  // for thermal/memory: a level name
	Apps   []string `yaml:"apps,omitempty"`   // for on_app_running
}

// Signals is what the node currently observes about its owner's use.
type Signals struct {
	Now time.Time
	// UserIdle is time since the last keyboard or mouse input. A large value
	// means the owner is away.
	UserIdle     time.Duration
	OnBattery    bool
	ScreenLocked bool
	// Thermal and MemoryPressure use the platform's level names, normalised
	// to: nominal < fair < serious < critical.
	Thermal        string
	MemoryPressure string
	RunningApps    []string
}

// Decision is the outcome, with the reason that produced it.
type Decision struct {
	Action Action
	Reason string
}

// ParsePolicy reads node.yaml.
func ParsePolicy(data []byte) (*Policy, error) {
	var p Policy
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("invalid policy file: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Validate rejects a policy that cannot be honoured, rather than silently
// ignoring the parts that make no sense.
func (p *Policy) Validate() error {
	for _, s := range p.Availability.Schedule {
		if _, err := parseWindow(s); err != nil {
			return fmt.Errorf("availability schedule %q: %w", s, err)
		}
	}
	for _, r := range p.Availability.Require {
		switch r {
		case "ac_power", "screen_locked":
		default:
			return fmt.Errorf("unknown availability requirement %q "+
				"(known: ac_power, screen_locked)", r)
		}
	}
	for name, t := range p.triggers() {
		if t == nil {
			continue
		}
		switch t.Action {
		case "", "throttle", "suspend", "drain":
		default:
			return fmt.Errorf("yield.%s: unknown action %q (known: throttle, suspend, drain)",
				name, t.Action)
		}
		if t.Within != "" {
			if _, err := time.ParseDuration(t.Within); err != nil {
				return fmt.Errorf("yield.%s: bad duration %q", name, t.Within)
			}
		}
		if t.Above != "" && levelRank(t.Above) < 0 {
			return fmt.Errorf("yield.%s: unknown level %q "+
				"(known: nominal, fair, serious, critical)", name, t.Above)
		}
	}
	if p.Yield.ResumeAfterIdle != "" {
		if _, err := time.ParseDuration(p.Yield.ResumeAfterIdle); err != nil {
			return fmt.Errorf("yield.resume_after_idle: bad duration %q", p.Yield.ResumeAfterIdle)
		}
	}
	switch p.Contribute.GPU {
	case "", "exclusive", "shared", "never":
	default:
		return fmt.Errorf("contribute.gpu: unknown mode %q (known: exclusive, shared, never)",
			p.Contribute.GPU)
	}
	return nil
}

func (p *Policy) triggers() map[string]*Trigger {
	return map[string]*Trigger{
		"on_user_input":      p.Yield.OnUserInput,
		"on_battery":         p.Yield.OnBattery,
		"on_thermal":         p.Yield.OnThermal,
		"on_memory_pressure": p.Yield.OnMemoryPressure,
		"on_app_running":     p.Yield.OnAppRunning,
	}
}

func action(t *Trigger, dflt Action) Action {
	if t == nil || t.Action == "" {
		return dflt
	}
	return Action(t.Action)
}

// levelRank orders platform pressure levels; -1 means unrecognised.
func levelRank(s string) int {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "nominal", "normal", "":
		return 0
	case "fair", "warn", "warning":
		return 1
	case "serious", "critical-ish":
		return 2
	case "critical":
		return 3
	}
	return -1
}

// Decide resolves policy plus signals into a single action.
//
// The most protective applicable action wins. Being over-cautious costs the
// cluster some throughput; being under-cautious costs the owner their machine,
// and that is the asymmetry the whole feature exists to respect.
func Decide(p *Policy, s Signals) Decision {
	if p == nil {
		return Decision{Run, "no policy set"}
	}
	best := Decision{Run, "policy allows cluster work"}
	consider := func(a Action, reason string) {
		if severity(a) > severity(best.Action) {
			best = Decision{a, reason}
		}
	}

	// Availability first: outside its window the node simply does not
	// participate, whatever else is true.
	if len(p.Availability.Schedule) > 0 && !inAnyWindow(p.Availability.Schedule, s.Now) {
		consider(Suspend, "outside the availability schedule")
	}
	for _, r := range p.Availability.Require {
		switch r {
		case "ac_power":
			if s.OnBattery {
				consider(Suspend, "policy requires AC power and the machine is on battery")
			}
		case "screen_locked":
			if !s.ScreenLocked {
				consider(Suspend, "policy requires the screen to be locked and it is not")
			}
		}
	}

	if t := p.Yield.OnBattery; t != nil && s.OnBattery {
		consider(action(t, Suspend), "running on battery")
	}
	if t := p.Yield.OnUserInput; t != nil {
		within := 30 * time.Second
		if t.Within != "" {
			if d, err := time.ParseDuration(t.Within); err == nil {
				within = d
			}
		}
		if s.UserIdle < within {
			consider(action(t, Throttle),
				fmt.Sprintf("owner active (input %s ago)", s.UserIdle.Round(time.Second)))
		}
	}
	if t := p.Yield.OnThermal; t != nil && t.Above != "" {
		if levelRank(s.Thermal) >= levelRank(t.Above) && levelRank(s.Thermal) > 0 {
			consider(action(t, Throttle), "thermal pressure is "+s.Thermal)
		}
	}
	if t := p.Yield.OnMemoryPressure; t != nil && t.Above != "" {
		if levelRank(s.MemoryPressure) >= levelRank(t.Above) && levelRank(s.MemoryPressure) > 0 {
			consider(action(t, Suspend), "memory pressure is "+s.MemoryPressure)
		}
	}
	if t := p.Yield.OnAppRunning; t != nil && len(t.Apps) > 0 {
		for _, want := range t.Apps {
			for _, got := range s.RunningApps {
				if strings.EqualFold(strings.TrimSpace(want), strings.TrimSpace(got)) {
					consider(action(t, Suspend), "owner is running "+got)
				}
			}
		}
	}
	return best
}
