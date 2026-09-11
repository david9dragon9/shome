package linux

import "fmt"

// Tier grades what a node can actually enforce.
//
// The plan's rule is "degrade loudly": a node that cannot enforce memory
// limits still joins, but says so, and the controller records the limit as
// advisory rather than implying a guarantee it cannot keep.
func (e Environment) Tier() (tier string, lost []string) {
	tier = "full"
	demote := func(to, why string) {
		if to == "limited" || tier == "full" {
			tier = to
		}
		lost = append(lost, why)
	}

	switch e.Cgroup {
	case CgroupV2:
		if !e.CanDelegate {
			demote("standard",
				"cgroup v2 present but not delegated to this user: memory limits fall back to polling")
		}
	case CgroupV1:
		// Crostini is the common case here.
		demote("standard",
			"only cgroup v1 is available: memory limits are best-effort, CPU shares are relative not absolute")
	case CgroupAbsent:
		demote("limited",
			"no writable cgroup hierarchy: memory is enforced by polling only, CPU not at all")
	}

	// Per-user OS accounts are NOT implemented on Linux yet: the backend never
	// creates a user or switches uid, so every job runs as whoever runs the
	// agent, whatever `useradd` availability suggests. Stating this
	// unconditionally is the only honest option -- gating it on CanCreateUser
	// would imply that running as root buys a separation it does not.
	lost = append(lost,
		"all jobs run as the agent's user: per-user OS accounts are not implemented, "+
			"so jobs are isolated from each other by the sandbox alone")
	if e.CanCreateUser {
		lost = append(lost,
			"this node could create OS users (running as root) but shome does not use that yet")
	}
	// Bubblewrap is the ONLY filesystem isolation this backend implements.
	//
	// Landlock is detected and reported, but not yet enforced: applying it
	// requires restricting the child between fork and exec, which Go cannot do
	// without a re-exec helper. Until that exists, a node without bubblewrap
	// has no filesystem isolation whatsoever and must say so -- claiming
	// "Landlock only" would advertise protection that does not exist.
	if !e.HasBwrap {
		demote("limited",
			"bubblewrap not installed: NO filesystem isolation between jobs "+
				"or from the machine's owner (install it: apt install bubblewrap)")
		if e.HasLandlock {
			lost = append(lost,
				"Landlock is available but shome does not enforce it yet; it does not "+
					"substitute for bubblewrap")
		}
	} else if !e.HasLandlock {
		lost = append(lost,
			"Landlock unavailable: relying on bubblewrap mount namespaces alone")
	}
	if !e.HasSeccomp {
		lost = append(lost, "seccomp unavailable: no syscall filtering")
	}
	if e.Container == "crostini" {
		lost = append(lost,
			"running under ChromeOS Crostini: GPU access is mediated, hardware is restricted, "+
				"and the container may be suspended by ChromeOS without warning")
	}
	return tier, lost
}

// Summary renders the environment for `shome doctor`.
func (e Environment) Summary() string {
	s := fmt.Sprintf("cgroup=%s", e.Cgroup)
	if e.Cgroup != CgroupAbsent {
		s += fmt.Sprintf(" (delegated=%v)", e.CanDelegate)
	}
	s += fmt.Sprintf(" root=%v users=%v landlock=%v seccomp=%v bwrap=%v",
		e.IsRoot, e.CanCreateUser, e.HasLandlock, e.HasSeccomp, e.HasBwrap)
	if e.Container != "" {
		s += " container=" + e.Container
	}
	if n := len(e.NvidiaGPUs); n > 0 {
		s += fmt.Sprintf(" cuda=%d", n)
	}
	return s
}
