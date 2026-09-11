// Package qos defines the limits a cluster places on what one account may use.
//
// Two layers: a default that applies to everybody, and per-account overrides
// that replace individual fields of it. That split is what makes the common
// case manageable -- an admin sets sensible defaults once, and only says
// something about the person who needs to differ.
//
// Every limit is a hard refusal, not a hint. A job that would exceed one is
// rejected at submission when that is knowable, and held out of scheduling
// when it depends on what is currently running. Nothing is silently truncated:
// asking for more than you may have is an error with a number in it, because
// a limit that quietly reshapes your request is worse than one that says no.
package qos

import (
	"fmt"
	"strings"
	"time"
)

// Limits is a sparse set of limits. A nil field means "not specified here" --
// at the default layer that means unlimited, and at the account layer it means
// inherit the default.
//
// Pointers rather than zero-as-unset because zero is a meaningful limit: an
// account with MaxRunningJobs of 0 is suspended from running anything, which
// is a thing an admin may genuinely want and must not be confused with having
// said nothing.
type Limits struct {
	// Queue shape.
	MaxSubmittedJobs *int `json:"max_submitted_jobs,omitempty" yaml:"max_submitted_jobs,omitempty"`
	MaxRunningJobs   *int `json:"max_running_jobs,omitempty" yaml:"max_running_jobs,omitempty"`

	// Concurrent resource use, summed over everything the account has running.
	MaxRunningCPUs  *int   `json:"max_running_cpus,omitempty" yaml:"max_running_cpus,omitempty"`
	MaxRunningGPUs  *int   `json:"max_running_gpus,omitempty" yaml:"max_running_gpus,omitempty"`
	MaxRunningMemMB *int64 `json:"max_running_mem_mb,omitempty" yaml:"max_running_mem_mb,omitempty"`

	// Per-job ceilings, checked at submission.
	MaxCPUsPerJob  *int    `json:"max_cpus_per_job,omitempty" yaml:"max_cpus_per_job,omitempty"`
	MaxGPUsPerJob  *int    `json:"max_gpus_per_job,omitempty" yaml:"max_gpus_per_job,omitempty"`
	MaxMemMBPerJob *int64  `json:"max_mem_mb_per_job,omitempty" yaml:"max_mem_mb_per_job,omitempty"`
	MaxWalltime    *string `json:"max_walltime,omitempty" yaml:"max_walltime,omitempty"`
	MaxProcsPerJob *int    `json:"max_procs_per_job,omitempty" yaml:"max_procs_per_job,omitempty"`

	// Persistent storage.
	MaxDiskMB *int64 `json:"max_disk_mb,omitempty" yaml:"max_disk_mb,omitempty"`
}

// Unlimited is the resolved value of a limit nobody set.
//
// Negative rather than zero, because zero is a limit somebody may genuinely
// want: an account that may run jobs but touch no accelerators is expressed by
// max-running-gpus 0, and conflating that with "no limit" would silently grant
// the opposite of what was asked.
const Unlimited = -1

// Effective is a fully resolved set of limits. Unlimited (-1) means no limit;
// zero means none allowed.
type Effective struct {
	MaxSubmittedJobs int
	MaxRunningJobs   int
	MaxRunningCPUs   int
	MaxRunningGPUs   int
	MaxRunningMemMB  int64
	MaxCPUsPerJob    int
	MaxGPUsPerJob    int
	MaxMemMBPerJob   int64
	MaxWalltime      time.Duration
	MaxProcsPerJob   int
	MaxDiskMB        int64
}

// Defaults are what a cluster starts with when nobody has said otherwise.
//
// Chosen to be generous enough that a household never notices them, and tight
// enough that one runaway submission loop cannot fill the queue or the disk.
// They exist so that "no configuration" still means "bounded", which is the
// safer default for a cluster made of other people's computers.
func Defaults() Limits {
	return Limits{
		MaxSubmittedJobs: ptr(200),
		MaxRunningJobs:   ptr(32),
		MaxRunningCPUs:   ptr(64),
		MaxRunningGPUs:   ptr(4),
		MaxRunningMemMB:  ptr64(256 << 10), // 256 GiB
		// Per-job ceilings are there to catch a typo -- --mem 500G when you
		// meant 500M -- not to size the cluster. Set generously, because an
		// aggregate request spans machines and is the feature shome exists
		// for: a ceiling tight enough to be a tuning knob would refuse
		// legitimate work on a cluster of any size. What actually bounds an
		// account is the running totals below, and the cluster's own.
		MaxCPUsPerJob:  ptr(256),
		MaxGPUsPerJob:  ptr(16),
		MaxMemMBPerJob: ptr64(1 << 20), // 1 TiB
		MaxWalltime:    ptrs("168h"),   // one week
		MaxProcsPerJob: ptr(4096),
		MaxDiskMB:      ptr64(100 << 10), // 100 GiB
	}
}

func ptr(v int) *int        { return &v }
func ptr64(v int64) *int64  { return &v }
func ptrs(v string) *string { return &v }

// Resolve layers an account's overrides over the cluster defaults.
func Resolve(def, user Limits) Effective {
	pick := func(a, b *int) int {
		if b != nil {
			return *b
		}
		if a != nil {
			return *a
		}
		return Unlimited
	}
	pick64 := func(a, b *int64) int64 {
		if b != nil {
			return *b
		}
		if a != nil {
			return *a
		}
		return Unlimited
	}
	picks := func(a, b *string) string {
		if b != nil {
			return *b
		}
		if a != nil {
			return *a
		}
		return ""
	}
	e := Effective{
		MaxSubmittedJobs: pick(def.MaxSubmittedJobs, user.MaxSubmittedJobs),
		MaxRunningJobs:   pick(def.MaxRunningJobs, user.MaxRunningJobs),
		MaxRunningCPUs:   pick(def.MaxRunningCPUs, user.MaxRunningCPUs),
		MaxRunningGPUs:   pick(def.MaxRunningGPUs, user.MaxRunningGPUs),
		MaxRunningMemMB:  pick64(def.MaxRunningMemMB, user.MaxRunningMemMB),
		MaxCPUsPerJob:    pick(def.MaxCPUsPerJob, user.MaxCPUsPerJob),
		MaxGPUsPerJob:    pick(def.MaxGPUsPerJob, user.MaxGPUsPerJob),
		MaxMemMBPerJob:   pick64(def.MaxMemMBPerJob, user.MaxMemMBPerJob),
		MaxProcsPerJob:   pick(def.MaxProcsPerJob, user.MaxProcsPerJob),
		MaxDiskMB:        pick64(def.MaxDiskMB, user.MaxDiskMB),
	}
	if w := picks(def.MaxWalltime, user.MaxWalltime); w != "" {
		if d, err := time.ParseDuration(w); err == nil {
			e.MaxWalltime = d
		}
	}
	return e
}

// Field describes one limit for display and for setting by name, so the CLI,
// the file and the console all agree on what the limits are called without
// three lists to keep in step.
type Field struct {
	Name  string
	Help  string
	Unit  string // "", "MB", "duration"
	Get   func(Effective) string
	Set   func(*Limits, string) error
	Clear func(*Limits)
}

// Fields is the authoritative list of limits.
var Fields = []Field{
	{"max-submitted-jobs", "queued plus running jobs at once", "",
		func(e Effective) string { return itoa(e.MaxSubmittedJobs) },
		func(l *Limits, v string) error { return setInt(&l.MaxSubmittedJobs, v) },
		func(l *Limits) { l.MaxSubmittedJobs = nil }},
	{"max-running-jobs", "jobs running at once", "",
		func(e Effective) string { return itoa(e.MaxRunningJobs) },
		func(l *Limits, v string) error { return setInt(&l.MaxRunningJobs, v) },
		func(l *Limits) { l.MaxRunningJobs = nil }},
	{"max-running-cpus", "CPU cores in use at once", "",
		func(e Effective) string { return itoa(e.MaxRunningCPUs) },
		func(l *Limits, v string) error { return setInt(&l.MaxRunningCPUs, v) },
		func(l *Limits) { l.MaxRunningCPUs = nil }},
	{"max-running-gpus", "accelerators in use at once", "",
		func(e Effective) string { return itoa(e.MaxRunningGPUs) },
		func(l *Limits, v string) error { return setInt(&l.MaxRunningGPUs, v) },
		func(l *Limits) { l.MaxRunningGPUs = nil }},
	{"max-running-mem", "memory allocated at once", "MB",
		func(e Effective) string { return mb(e.MaxRunningMemMB) },
		func(l *Limits, v string) error { return setSize(&l.MaxRunningMemMB, v) },
		func(l *Limits) { l.MaxRunningMemMB = nil }},
	{"max-cpus-per-job", "CPU cores one job may ask for", "",
		func(e Effective) string { return itoa(e.MaxCPUsPerJob) },
		func(l *Limits, v string) error { return setInt(&l.MaxCPUsPerJob, v) },
		func(l *Limits) { l.MaxCPUsPerJob = nil }},
	{"max-gpus-per-job", "accelerators one job may ask for", "",
		func(e Effective) string { return itoa(e.MaxGPUsPerJob) },
		func(l *Limits, v string) error { return setInt(&l.MaxGPUsPerJob, v) },
		func(l *Limits) { l.MaxGPUsPerJob = nil }},
	{"max-mem-per-job", "memory one job may ask for", "MB",
		func(e Effective) string { return mb(e.MaxMemMBPerJob) },
		func(l *Limits, v string) error { return setSize(&l.MaxMemMBPerJob, v) },
		func(l *Limits) { l.MaxMemMBPerJob = nil }},
	{"max-walltime", "how long one job may run", "duration",
		func(e Effective) string {
			if e.MaxWalltime == 0 {
				return "unlimited"
			}
			return e.MaxWalltime.String()
		},
		func(l *Limits, v string) error {
			if _, err := time.ParseDuration(v); err != nil {
				return fmt.Errorf("invalid duration %q (try 2h, 30m, 168h)", v)
			}
			l.MaxWalltime = &v
			return nil
		},
		func(l *Limits) { l.MaxWalltime = nil }},
	{"max-procs-per-job", "processes and threads one job may spawn", "",
		func(e Effective) string { return itoa(e.MaxProcsPerJob) },
		func(l *Limits, v string) error { return setInt(&l.MaxProcsPerJob, v) },
		func(l *Limits) { l.MaxProcsPerJob = nil }},
	{"max-disk", "persistent storage", "MB",
		func(e Effective) string { return mb(e.MaxDiskMB) },
		func(l *Limits, v string) error { return setSize(&l.MaxDiskMB, v) },
		func(l *Limits) { l.MaxDiskMB = nil }},
}

// FieldByName looks up a limit by its flag name.
func FieldByName(name string) (Field, bool) {
	name = strings.TrimPrefix(name, "--")
	for _, f := range Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

func itoa(v int) string {
	switch {
	case v < 0:
		return "unlimited"
	case v == 0:
		return "0 (none allowed)"
	}
	return fmt.Sprint(v)
}

func mb(v int64) string {
	switch {
	case v < 0:
		return "unlimited"
	case v == 0:
		return "0 (none allowed)"
	}
	switch {
	case v >= 1<<20:
		return fmt.Sprintf("%.1fT", float64(v)/(1<<20))
	case v >= 1024:
		return fmt.Sprintf("%.0fG", float64(v)/1024)
	default:
		return fmt.Sprintf("%dM", v)
	}
}

func setInt(dst **int, v string) error {
	n, err := parseCount(v)
	if err != nil {
		return err
	}
	*dst = &n
	return nil
}

func setSize(dst **int64, v string) error {
	mb, err := ParseSizeMB(v)
	if err != nil {
		return err
	}
	*dst = &mb
	return nil
}

func parseCount(v string) (int, error) {
	v = strings.TrimSpace(v)
	if v == "unlimited" {
		return Unlimited, nil
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n < 0 {
		return 0, fmt.Errorf("expected a whole number or 'unlimited', got %q", v)
	}
	return n, nil
}

// ParseSizeMB reads a size like 8G, 512M, 2T into mebibytes.
func ParseSizeMB(v string) (int64, error) {
	v = strings.TrimSpace(v)
	if v == "unlimited" {
		return Unlimited, nil
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(v, "T"), strings.HasSuffix(v, "t"):
		mult, v = 1<<20, v[:len(v)-1]
	case strings.HasSuffix(v, "G"), strings.HasSuffix(v, "g"):
		mult, v = 1024, v[:len(v)-1]
	case strings.HasSuffix(v, "M"), strings.HasSuffix(v, "m"):
		mult, v = 1, v[:len(v)-1]
	}
	var n float64
	if _, err := fmt.Sscanf(strings.TrimSpace(v), "%g", &n); err != nil || n < 0 {
		return 0, fmt.Errorf("expected a size like 8G, 512M or 'unlimited', got %q", v)
	}
	return int64(n * float64(mult)), nil
}

// Config is the two layers an admin sets, which answer different questions.
//
//   - Cluster is a total across everybody: how much of this cluster may be in
//     use at once, whoever is using it. It exists because per-account limits
//     multiply -- ten accounts each allowed four accelerators is forty
//     accelerators, which is not a bound on anything if the cluster has six.
//   - PerUser is what each account gets unless overridden for one.
//
// They are separate settings rather than one, because tightening what an
// individual may take and tightening what the cluster may give away are
// different decisions that happen for different reasons.
type Config struct {
	Cluster Limits `json:"cluster" yaml:"cluster"`
	PerUser Limits `json:"per_user" yaml:"per_user"`
}

// DefaultConfig is a cluster nobody has configured.
//
// The cluster totals are deliberately absent -- unlimited -- because shome
// cannot know how big the cluster is, and a made-up ceiling would either be
// meaningless or would quietly refuse work on a machine that could run it. The
// per-account limits are what stop one person monopolising it in the meantime.
func DefaultConfig() Config {
	return Config{PerUser: Defaults()}
}

// ResolveFor gives the limits applying to one account: the per-account default
// with that account's overrides on top.
func (c Config) ResolveFor(user Limits) Effective {
	return Resolve(c.PerUser, user)
}

// ClusterEffective is the aggregate ceiling.
func (c Config) ClusterEffective() Effective {
	return Resolve(Limits{}, c.Cluster)
}

// LooksLegacy reports whether a parsed file used the old flat shape, where the
// limits sat at the top level and meant per-account defaults.
//
// Detected rather than versioned: the flat form predates the split, and
// silently reading it as an empty config would drop every limit an admin had
// set -- turning an upgrade into an unbounded cluster.
func (c Config) LooksLegacy(raw map[string]any) bool {
	if len(raw) == 0 {
		return false
	}
	if _, ok := raw["cluster"]; ok {
		return false
	}
	if _, ok := raw["per_user"]; ok {
		return false
	}
	for _, f := range Fields {
		if _, ok := raw[strings.ReplaceAll(f.Name, "-", "_")]; ok {
			return true
		}
	}
	// Field names differ slightly from flag names for the sized limits.
	for _, k := range []string{"max_running_mem_mb", "max_mem_mb_per_job", "max_disk_mb"} {
		if _, ok := raw[k]; ok {
			return true
		}
	}
	return false
}
