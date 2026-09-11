// Package linux implements the Linux node backend.
//
// Linux diverges from macOS deliberately (see docs/design-notes.md): real OS users are cleanly
// reversible here (`userdel` works), and cgroups v2 plus namespaces and
// Landlock give a genuine kernel boundary rather than a policy one. The
// NodeBackend interface exists precisely to absorb that difference.
//
// The types and tier calculation live here, untagged, so the grading logic --
// which is pure and is what decides what shome promises a user -- can be
// tested on any platform rather than only on Linux.
package linux

// CgroupMode is which cgroup hierarchy the kernel exposes.
type CgroupMode string

const (
	CgroupV2     CgroupMode = "v2"     // unified: real memory.max and cpu.max
	CgroupV1     CgroupMode = "v1"     // legacy: usable but awkward; Crostini is here
	CgroupAbsent CgroupMode = "absent" // no cgroups we can write to
)

// Environment is what a Linux node can actually do, probed at startup.
//
// Every field is a capability shome must not assume: a ChromeOS Crostini
// container, an unprivileged user session and a bare-metal root install differ
// enormously, and a node that silently over-promises is worse than one that
// declines to join. Degrade loudly.
type Environment struct {
	Cgroup        CgroupMode
	CgroupRoot    string
	CanDelegate   bool // may we create and write our own cgroup?
	IsRoot        bool
	CanCreateUser bool
	HasLandlock   bool
	HasSeccomp    bool
	HasBwrap      bool
	BwrapPath     string
	// Container names the environment when this is not bare metal --
	// "crostini", "docker", "lxc", "wsl". Kept because the limitations differ
	// and the operator needs to know which they have hit.
	Container  string
	NvidiaGPUs []NvidiaGPU
}

// NvidiaGPU is one CUDA device.
type NvidiaGPU struct {
	Index    int
	Name     string
	MemBytes int64
	UUID     string
}
