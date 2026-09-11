//go:build linux

package main

import (
	"fmt"

	"github.com/davidwu/shome/internal/platform/linux"
)

// doctorPlatform runs the Linux-specific checks, reusing the same detection
// the node backend uses so doctor cannot disagree with reality.
func doctorPlatform(check func(name string, status rune, detail, fix string)) {
	e := linux.Detect()

	switch e.Cgroup {
	case linux.CgroupV2:
		if e.CanDelegate {
			check("cgroup v2", 'p', "delegated at "+e.CgroupRoot, "")
		} else {
			check("cgroup v2", 'w', "present but not delegated",
				"run as root, or enable systemd delegation for this user; "+
					"memory limits fall back to polling")
		}
	case linux.CgroupV1:
		check("cgroups", 'w', "only v1 available",
			"memory limits are best-effort here; common under ChromeOS Crostini")
	default:
		check("cgroups", 'f', "no writable hierarchy",
			"memory is enforced by polling only and CPU not at all")
	}

	if e.HasBwrap {
		check("bubblewrap", 'p', e.BwrapPath, "")
	} else {
		check("bubblewrap", 'w', "not installed",
			"install bubblewrap for mount-namespace isolation (apt install bubblewrap)")
	}
	if e.HasLandlock {
		check("landlock", 'p', "available", "")
	} else {
		check("landlock", 'w', "unavailable",
			"needs Linux 5.13+ with Landlock enabled; bubblewrap alone will be used")
	}
	if e.HasSeccomp {
		check("seccomp", 'p', "available", "")
	} else {
		check("seccomp", 'w', "unavailable", "no syscall filtering on this node")
	}
	if e.CanCreateUser {
		check("per-user accounts", 'p', "can create OS users", "")
	} else {
		check("per-user accounts", 'w', "cannot create OS users",
			"all jobs will share one identity, isolated by sandbox only")
	}
	if n := len(e.NvidiaGPUs); n > 0 {
		check("cuda", 'p', fmt.Sprintf("%d device(s), first: %s", n, e.NvidiaGPUs[0].Name), "")
	} else {
		check("cuda", 'w', "no NVIDIA devices found", "the node will join as CPU-only")
	}
	if e.Container != "" {
		check("container", 'w', "running under "+e.Container,
			"hardware access is mediated here; expect a reduced tier")
	}

	tier, lost := e.Tier()
	check("node tier", map[bool]rune{true: 'p', false: 'w'}[tier == "full"], tier, "")
	for _, l := range lost {
		fmt.Printf("         - %s\n", l)
	}
}
