// Package platform abstracts per-OS job isolation and resource enforcement.
//
// The backends deliberately diverge. On macOS, creating OS accounts is not
// reversible without Recovery Mode (see docs/design-notes.md), so isolation is a per-job Seatbelt
// profile under a single uid. On Linux, real users plus cgroups v2, namespaces
// and Landlock give a genuine kernel boundary and are cleanly reversible.
// Absorbing that difference is the whole point of this interface.
package platform

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"

	"github.com/davidwu/shome/internal/job"
)

// ErrAdvisory is returned by Enforce when a backend accepted a limit but cannot
// hard-enforce it. The controller records the limit as advisory and surfaces it
// in sinfo rather than implying a guarantee it cannot keep.
//
// This exists because macOS has no cgroups: CPU is admission control plus a QoS
// clamp, never a quota (see docs/design-notes.md).
var ErrAdvisory = errors.New("limit accepted but only advisory on this platform")

// Enforcement describes how faithfully a single limit is applied.
type Enforcement string

const (
	Hard     Enforcement = "hard"     // kernel-enforced
	Polled   Enforcement = "polled"   // supervisor-enforced, with a detection window
	Advisory Enforcement = "advisory" // scheduling hint only
	None     Enforcement = "none"
)

// Capabilities is what a node advertises. The scheduler matches against this
// rather than against platform names, so heterogeneity stays the default case.
type Capabilities struct {
	OS        string
	OSVersion string
	Arch      string
	CPUs      int
	MemBytes  int64

	// GPUMemBytes is the GPU-usable budget. On Apple Silicon this is
	// recommendedMaxWorkingSetSize, which is ~74% of installed RAM -- NOT the
	// installed total (see docs/design-notes.md).
	GPUs        int
	GPUKind     string // "metal" | "cuda" | "rocm" | ""
	GPUName     string
	GPUMemBytes int64 // schedulable GPU memory (Metal: recommendedMaxWorkingSetSize)
	// GPUMaxAllocBytes is the largest single allocation the device permits.
	// On Apple Silicon this is well below GPUMemBytes (measured 9093 vs 12124
	// MiB on an M5), so a workload needing one huge buffer can fail even when
	// total GPU memory looks sufficient.
	GPUMaxAllocBytes int64

	MemLimit Enforcement
	CPULimit Enforcement

	// Tier is "full", "standard" or "limited" -- what the node earned at
	// preflight. A node that cannot enforce joins and says so.
	Tier string
	Lost []string // capabilities probed for and not available
}

// Sandbox is a prepared, isolated environment for one job.
// Placement is what the caller wants mapped.
type Placement struct {
	// Home is the account's directory on this machine.
	Home string
	// Tools is shome's managed tool directory, exposed read-only.
	Tools string
	// User and Machine name the account and the machine, for building a
	// path that tells somebody which machine's files they are looking at.
	// A cluster has no shared filesystem, so that distinction matters.
	User, Machine string

	// GPUs is how many accelerators the work needs.
	//
	// Part of the question because it can decide the answer: Metal does not
	// exist inside a virtual machine, so a GPU job on a Mac runs natively
	// and sees the host's own paths, where the same job without a GPU runs
	// in a container and sees paths shome chose. Asking for a layout
	// without saying this got the container's answer for a job that was
	// about to run outside one.
	GPUs int
}

// Layout is where those directories appear from inside.
type Layout struct {
	Home  string
	Tools string
	// Mapped is false when the platform cannot remap paths, so the caller
	// knows the account will see the host's own layout.
	Mapped bool

	// Slot names the operating system and architecture of the environment
	// the work will run in, as "os-arch".
	//
	// Not necessarily this machine's: a job on an Apple silicon Mac runs in
	// a Linux virtual machine unless it asked for a GPU, so the same node
	// offers "linux-arm64" for most work and "darwin-arm64" for the rest.
	// An account's home is one directory shared by both, so anything in it
	// that holds compiled code -- a virtualenv, an installed tool, a
	// downloaded interpreter -- has to be kept per slot or the second
	// environment finds the first one's binaries and cannot run them. See
	// userenv.Session.
	Slot string

	// Software are the machine's own command directories, when the isolation
	// leaves them visible.
	//
	// Empty for a container or a mount namespace, which carry their own
	// software and show nothing of the host's. Non-empty only where the
	// process runs on the machine's real filesystem -- a GPU job on a Mac --
	// and is therefore the one case where the base software has to be found
	// rather than installed. The backend answers this because the backend is
	// what decides how much of the machine a job can see.
	Software []string
}

// ShellSpec describes an interactive session's isolation.
type ShellSpec struct {
	// User is the account, for diagnostics and the profile's comments.
	User string

	// Home is the account's persistent directory on this machine, and the
	// only place the session may write. It becomes $HOME and the working
	// directory.
	Home string

	// ReadOnly are extra trees the session may read but not write -- the
	// directory shome keeps managed tools like uv in.
	ReadOnly []string

	// Writable are extra read-write trees, for a scheduled session that has
	// a job scratch dir as well as a home.
	Writable []string

	// AllowNet permits outbound network.
	//
	// True for a login shell, because a package manager that cannot reach an
	// index is not a package manager. This is the one deliberate widening
	// relative to a batch job, and it is the reason the session's writable
	// set is kept to exactly one directory.
	AllowNet bool

	// Argv is the command to run. Empty means the platform's default shell.
	Argv []string

	// Env is added to a minimal base environment. Nothing is inherited from
	// the caller's environment.
	Env map[string]string

	// Name identifies this session's isolation so it can be stopped when the
	// session ends.
	//
	// Needed because a container outlives the process that started it: an
	// unnamed one is indistinguishable from anything else on the machine, so
	// shome could neither stop it nor safely sweep it up later. Every login
	// would leave a virtual machine running.
	Name string

	// TTY says the session will be attached to a pseudo-terminal, so the
	// shell should be started as an interactive login shell.
	TTY bool

	// HomeAs is where Home should appear from inside, when the platform can
	// remap. Empty means the real path.
	HomeAs string
	// ToolsAs is likewise for the read-only tool directory, and ToolsHost
	// the directory to expose there. They are separate from ReadOnly because
	// a container on macOS needs Linux binaries, which live somewhere else
	// entirely from the host's own tool directory.
	ToolsAs   string
	ToolsHost string

	// TTYPath is that terminal's device path, when the caller has already
	// allocated it. The sandbox has to name the terminal to permit terminal
	// control on it, so the pty must exist before the policy is written.
	TTYPath string
}

type Sandbox struct {
	JobID       int64
	ScratchDir  string // job-private, read-write
	ProfilePath string // generated policy file, empty if the platform has none
	Env         map[string]string

	// UserHome is the account's persistent storage on this machine, and
	// UserHomeIn where it should appear from inside.
	//
	// A job's scratch is deleted when it finishes, so a job that could only
	// write there could not keep anything, and nothing an account did in one
	// job would be visible in the next. Storage is per account and per
	// machine -- the same thing `shome fs` manages -- so a job gets it too,
	// as $HOME, with the scratch as its working directory.
	UserHome, UserHomeIn string

	// WorkDir is where the job runs, and WorkDirIn where that is from
	// inside.
	//
	// The account's own storage, not the scratch directory. A job used to
	// start in its scratch, which is deleted when it ends -- so a script
	// that wrote ./results.csv, which is most scripts, produced a file that
	// existed only for the length of the run and was invisible to
	// `shome fs`. Making $HOME persistent did not fix that on its own,
	// because nothing tells a relative path to go there.
	//
	// This is also what Slurm does: a job runs in the directory it was
	// submitted from, which on a real cluster is shared storage under the
	// user's home. A home cluster has no shared filesystem, so the account's
	// storage on this machine is the closest true equivalent -- and it is
	// the same directory a login session starts in, which makes the rule
	// easy to state: a job begins where you would.
	//
	// The scratch directory still exists, still gets cleaned up, and is
	// still named by $SHOME_SCRATCH and $TMPDIR. It is for what a job does
	// not need afterwards.
	//
	// Empty when the job has no account storage -- one submitted by the
	// machine's owner outside any account. Each backend then falls back to
	// its own scratch path, which is the only writable place there is.
	WorkDir, WorkDirIn string

	// ScratchIn is where the scratch directory appears from inside, when
	// that is not where it is. Empty means the two are the same.
	//
	// Set by the backend that will run the job, because only it knows
	// whether this job is contained and where it puts things. What needs
	// the answer is anything the agent places in the scratch and then has
	// to name to the job -- see the job's link to the cluster API in
	// internal/agent/joblink.go.
	ScratchIn string

	// ToolsHost is shome's own commands on this machine, and ToolsIn where
	// the job should find them.
	//
	// A job's PATH names the tool directory whether or not anything is
	// there -- it is built from the same Layout a session's is -- so a job
	// in a container had a PATH entry pointing at nothing, and `squeue`
	// inside one reported "not found". The commands belong there for the
	// same reason a session has them: a job that cannot ask the cluster
	// anything cannot check on its own array, its own quota, or what it is
	// waiting behind.
	//
	// Read-only, always: these are shome's binaries, not the job's.
	// Empty when this machine has nothing to offer, which is not fatal.
	ToolsHost, ToolsIn string

	// TTY, when set, becomes the job's terminal instead of its output file.
	//
	// This is what `srun --pty` needs, and doing it here rather than in a
	// separate launch path is deliberate: an interactive job then gets
	// exactly the same isolation, QoS clamp, process group and memory
	// polling as a batch job. A parallel launch path would be a second place
	// for enforcement to be forgotten.
	TTY *os.File
}

// Handle refers to a launched job's process tree.
type Handle struct {
	JobID int64
	PID   int
	PGID  int
}

// Usage is a point-in-time resource sample of a job's whole process tree.
type Usage struct {
	At       time.Time
	MemBytes int64
	NProcs   int
}

// Aftermath is what the isolation can say about a job once it has exited,
// beyond the exit status of its first process.
//
// The exit status is not enough, and the gap it leaves is not obscure. A
// script that backgrounds work and calls a bare `wait` reports zero whatever
// happened to the child -- that is what `wait` with no arguments does -- so a
// job whose real work was killed for using too much memory finished
// "successfully". Sampling does not reliably catch it either: the kill can
// happen between two polls, and a doubling allocation crosses the limit in
// microseconds.
//
// Where the kernel enforced the limit, the kernel also counted it. Asking
// afterwards is how any cgroup-based scheduler reports OUT_OF_MEMORY, and it
// is exact rather than sampled.
type Aftermath struct {
	// OOMKills is how many processes the kernel killed for memory. Any
	// number above zero means the limit was reached, whoever it happened to
	// and whatever the script's exit status says.
	OOMKills int

	// PeakBytes is the highest memory the job's whole tree ever held, as
	// recorded by the kernel rather than sampled. Zero when unknown.
	PeakBytes int64
}

// Backend is the per-OS implementation of job isolation and enforcement.
type Backend interface {
	Inventory(context.Context) (Capabilities, error)

	// Prepare creates the job's scratch dir and isolation policy.
	Prepare(context.Context, job.Spec, int64) (*Sandbox, error)

	// Launch starts the job inside the sandbox.
	Launch(context.Context, *Sandbox, job.Spec) (*Handle, error)

	// Sample reports current usage of the whole process tree. This is the
	// enforcement mechanism where the kernel offers no hard cap (see docs/design-notes.md).
	Sample(context.Context, *Handle) (Usage, error)

	// Throttle demotes (or restores) a running job's scheduling priority.
	//
	// Distinct from Suspend: the job keeps making progress, it just stops
	// competing with the owner. On macOS this is measurably enough to hold the
	// owner's p99 at idle levels under full load (see docs/design-notes.md).
	Throttle(context.Context, *Handle, bool) error

	// Signal delivers sig to the entire process tree.
	Signal(context.Context, *Handle, string) error

	// Wait blocks until the tree exits, returning its exit code.
	Wait(context.Context, *Handle) (int, error)

	// Cleanup removes everything Prepare created. Must be idempotent, and must
	// leave zero residue -- the reversibility promise depends on it.
	Cleanup(context.Context, *Sandbox) error

	// Layout reports where an account's directories appear from inside the
	// sandbox.
	//
	// A mount namespace can put a directory anywhere, so on Linux an
	// account's storage appears at a path that says nothing about the host:
	// no /home/dave/..., no installation path, nothing above it to walk up
	// into. A policy-based sandbox cannot remap anything, so on macOS the
	// real path is what the account sees.
	//
	// The caller needs this before it builds the session's environment --
	// HOME, TMPDIR and PATH all have to name the paths that will exist
	// inside, not the ones on the host.
	Layout(Placement) Layout

	// IsolationFault returns a non-empty reason when this machine cannot
	// confine work to its own directory.
	//
	// Every platform has a way to be missing its sandbox -- no bubblewrap on
	// Linux, no sandbox-exec on macOS -- and in both cases the code used to
	// carry on and launch the job unconfined, which is the one outcome a
	// cluster must never produce silently. A node reporting a fault drains
	// itself rather than refusing each launch: refusing at launch livelocks,
	// because the controller marks the job running, the agent says no, and
	// the pair cycle forever.
	IsolationFault() string

	// ShellCommand wraps argv in this platform's isolation for an
	// interactive session rooted at one account's persistent directory.
	//
	// Distinct from Prepare/Launch, which build a throwaway sandbox around a
	// job and tear it down afterwards. A login session is the opposite: the
	// directory outlives the session, because that is where the account's
	// files and its uv environments live.
	//
	// The returned command is not started. Callers attach a pty or pipes
	// themselves, since an interactive session and a one-shot command want
	// different plumbing.
	ShellCommand(context.Context, ShellSpec) (*exec.Cmd, error)

	// SessionHostAddr is the address a session reaches this machine on, when
	// a unix socket is not usable from inside its isolation.
	//
	// Empty means the socket is fine, which it is wherever isolation shares
	// the host's filesystem semantics. A container does not: the account's
	// directory arrives over virtiofs, which exposes a unix socket's inode
	// but refuses to connect through it -- so the socket is visible, looks
	// correct, and does not work.
	SessionHostAddr(ctx context.Context) string

	// StopIsolation ends isolation started for a session, by name.
	//
	// A no-op where isolation dies with its launcher.
	StopIsolation(ctx context.Context, name string) error

	// SweepOrphanIsolation stops isolation left running by jobs that are no
	// longer live, and reports what it stopped.
	//
	// A sibling of OrphanScratch and for the same reason -- the tidy path
	// needs the agent alive, so a crash or a kill leaves residue nobody
	// comes back for. Where the residue is a directory it merely takes
	// space; where it is a virtual machine it holds memory and CPUs
	// indefinitely.
	//
	// Nothing to do where isolation dies with its launcher, which is the
	// case for a bubblewrap namespace.
	SweepOrphanIsolation(ctx context.Context, live []int64) []string

	// Aftermath reports what the kernel recorded about a job that has
	// exited: whether anything in it was killed for memory, and how much it
	// really held.
	//
	// Best effort by design. A backend that has no kernel accounting to read
	// returns false, and the caller falls back to the exit status and its
	// own samples -- which is what it had before, not a failure.
	Aftermath(ctx context.Context, sb *Sandbox) (Aftermath, bool)

	// OrphanScratch lists job working directories that no live job owns,
	// given the ids that are still running.
	//
	// The tidy path needs the agent alive to run it, so a crash or a kill
	// leaves directories nobody will come back for. This is how they are
	// found again, rather than accumulating until somebody notices their disk
	// is full.
	OrphanScratch(ctx context.Context, live []int64) ([]string, error)
}
