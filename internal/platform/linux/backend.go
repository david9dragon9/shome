//go:build linux

package linux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/platform"
)

// Backend is the Linux node backend.
type Backend struct {
	Root      string
	OwnerHome string
	Env       Environment

	// StateRoot is the whole installation's directory, which on a controller
	// is the parent of Root. Disk accounting uses it because a contribution
	// cap is a cap on everything shome occupies -- the database, archived
	// output, staged data and user storage as well as scratch -- and
	// measuring only this agent's corner would under-report it several-fold.
	StateRoot string

	procs procRegistry
}

// stateRoot is the installation root, falling back to this agent's own.
func (b *Backend) stateRoot() string {
	if b.StateRoot != "" {
		return b.StateRoot
	}
	return b.Root
}

func New(root, ownerHome string) *Backend {
	if ownerHome == "" {
		ownerHome = os.Getenv("HOME")
	}
	return &Backend{Root: root, OwnerHome: ownerHome, Env: Detect(),
		procs: procRegistry{m: map[int64]*proc{}}}
}

func (b *Backend) jobsRoot() string { return filepath.Join(b.Root, "jobs") }

// shomeCgroup is the parent cgroup shome creates; per-job cgroups nest inside,
// so a single removal cleans up everything.
func (b *Backend) shomeCgroup() string {
	return filepath.Join(b.Env.CgroupRoot, "shome.slice")
}

func (b *Backend) jobCgroup(id int64) string {
	return filepath.Join(b.shomeCgroup(), fmt.Sprintf("job-%d", id))
}

func (b *Backend) Inventory(ctx context.Context) (platform.Capabilities, error) {
	c := platform.Capabilities{
		OS:       "linux",
		Arch:     runtime.GOARCH,
		CPUs:     runtime.NumCPU(),
		MemBytes: totalMemory(),
	}
	c.OSVersion = kernelVersion()

	// Enforcement fidelity follows directly from what was detected.
	switch {
	case b.Env.Cgroup == CgroupV2 && b.Env.CanDelegate:
		c.MemLimit, c.CPULimit = platform.Hard, platform.Hard
	case b.Env.Cgroup == CgroupV1:
		c.MemLimit, c.CPULimit = platform.Polled, platform.Advisory
	default:
		c.MemLimit, c.CPULimit = platform.Polled, platform.None
	}

	if gpus := b.Env.NvidiaGPUs; len(gpus) > 0 {
		c.GPUs = len(gpus)
		c.GPUKind = "cuda"
		c.GPUName = gpus[0].Name
		for _, g := range gpus {
			c.GPUMemBytes += g.MemBytes
			if g.MemBytes > c.GPUMaxAllocBytes {
				c.GPUMaxAllocBytes = g.MemBytes
			}
		}
	}
	c.Tier, c.Lost = b.Env.Tier()
	return c, nil
}

func totalMemory() int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			return 0
		}
		kb, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb << 10
	}
	return 0
}

func kernelVersion() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (b *Backend) Prepare(ctx context.Context, spec job.Spec, id int64) (*platform.Sandbox, error) {
	scratch := filepath.Join(b.jobsRoot(), fmt.Sprintf("job-%d", id))
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return nil, fmt.Errorf("create scratch: %w", err)
	}
	if err := b.createCgroup(id, spec.Limits); err != nil {
		// Not fatal: the polled monitor still applies, and Inventory already
		// told the controller this node's limits are advisory.
		_ = err
	}
	// Where the scratch appears from inside. A bubblewrap namespace remaps
	// it to a fixed, uninformative path; without bubblewrap there is no
	// namespace and no remapping -- and no job either, since a node that
	// cannot confine work drains itself.
	scratchIn := ""
	if b.Env.HasBwrap {
		scratchIn = JobScratchPath
	}
	return &platform.Sandbox{
		JobID:      id,
		ScratchDir: scratch,
		ScratchIn:  scratchIn,
		Env: map[string]string{
			"SHOME_JOB_ID":   strconv.FormatInt(id, 10),
			"SHOME_SCRATCH":  scratch,
			"TMPDIR":         scratch,
			"HOME":           scratch,
			"SLURM_JOB_ID":   strconv.FormatInt(id, 10),
			"SLURM_JOB_NAME": spec.Name,
		},
	}, nil
}

// createCgroup makes the job's cgroup and writes its limits.
//
// cgroup v2 only. v1's per-controller hierarchies would need a different path
// for each limit, and the environments that only have v1 (Crostini) have
// already been told their limits are best-effort.
func (b *Backend) createCgroup(id int64, lim job.Limits) error {
	if b.Env.Cgroup != CgroupV2 || !b.Env.CanDelegate {
		return fmt.Errorf("no delegated cgroup v2")
	}
	dir := b.jobCgroup(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if lim.MemBytes > 0 {
		// memory.max is a hard wall: the kernel OOM-kills the cgroup rather
		// than letting it exceed this. Unlike macOS, no polling is required.
		if err := os.WriteFile(filepath.Join(dir, "memory.max"),
			[]byte(strconv.FormatInt(lim.MemBytes, 10)), 0o644); err != nil {
			return err
		}
		// Disable swap for the job so memory.max means what it says.
		os.WriteFile(filepath.Join(dir, "memory.swap.max"), []byte("0"), 0o644)
	}
	if lim.CPUs > 0 {
		// cpu.max is "<quota> <period>": N cores means N * period per period.
		const period = 100000
		quota := lim.CPUs * period
		if err := os.WriteFile(filepath.Join(dir, "cpu.max"),
			[]byte(fmt.Sprintf("%d %d", quota, period)), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// sandboxArgv wraps a command in the strongest isolation this node offers.
//
// bubblewrap is preferred: a mount namespace means the job cannot even name
// paths outside its own tree, which is a stronger statement than a policy that
// denies access to them. Landlock is the fallback where bwrap is absent.
// toolsMount places shome's own commands inside, read-only, where the job's
// PATH already names them. Nothing to mount when this machine has no
// managed tool directory; a job without the cluster commands still runs.
func toolsMount(sb *platform.Sandbox) []Mount {
	if sb.ToolsHost == "" || sb.ToolsIn == "" {
		return nil
	}
	return []Mount{{Host: sb.ToolsHost, In: sb.ToolsIn}}
}

func (b *Backend) sandboxArgv(sb *platform.Sandbox, spec job.Spec, inner []string) []string {
	if !b.Env.HasBwrap {
		// Should be unreachable: a node with no bubblewrap drains itself via
		// IsolationFault and is never offered work. Returning inner here
		// would run the job unconfined, so refuse instead -- a command that
		// cannot start is recoverable, a job reading the owner's home is not.
		return []string{"/bin/false"}
	}
	// The job's scratch appears at a fixed, uninformative path. Where it
	// actually lives on this machine is not the job's business, and a job
	// that prints its working directory should not be publishing the
	// installation layout.
	return SandboxArgv(SandboxOpts{
		BwrapPath: b.Env.BwrapPath,
		Masks:     MaskPaths(PathExists),
		Mounts: append(append([]Mount{
			{Host: sb.ScratchDir, In: JobScratchPath, Write: true, Required: true},
		}, userHomeMount(sb)...), toolsMount(sb)...),
		// The job runs in the account's storage, which userHomeMount places
		// inside, so a relative path it writes still exists next time. The
		// scratch is mounted as well and is still $SHOME_SCRATCH.
		Chdir: chdirInside(sb),
		// $HOME is the account's own storage where it has one, so work
		// survives a job whose scratch is deleted when it ends.
		Home:    homeInside(sb),
		Network: spec.Limits.Network,
		GPU:     spec.Limits.GPUs > 0,
	}, inner)
}

// materialiseScript writes the job's script into its own scratch and returns
// the path to run it by, as the job will see it.
//
// The original path is never executed: it may sit inside the owner's home,
// which this job's sandbox denies, and on a remote node it may not exist. The
// copy lives beside the job's other files and is removed with them.
//
// Written to one path and returned as another, deliberately: see
// scriptPathIn.
func materialiseScript(sb *platform.Sandbox, spec job.Spec) (string, error) {
	body := spec.ScriptBody
	if len(body) == 0 {
		// A caller that passed a path rather than the contents. Read it here,
		// where we are still the agent and outside the sandbox, because the
		// job will not be able to: the sandbox is an allowlist and the
		// original path -- someone's home, a temp directory -- is not on it.
		//
		// This used to return the path unchanged and worked only because the
		// profile granted a broad read over the filesystem. It does not any
		// more, so the copy is no longer optional.
		if spec.Script == "" {
			return "", fmt.Errorf("job has neither a script body nor a path")
		}
		b, err := os.ReadFile(spec.Script)
		if err != nil {
			return "", fmt.Errorf("read job script %s: %w", spec.Script, err)
		}
		body = b
	}
	path := filepath.Join(sb.ScratchDir, "job-script")
	if err := os.WriteFile(path, body, 0o700); err != nil {
		return "", fmt.Errorf("write job script: %w", err)
	}
	// The path the job will run it by, which is not the path it was
	// written to. The scratch directory is bound into the namespace at a
	// fixed place, and the host's own path for it is not in there at all:
	// naming it made bubblewrap report
	//
	//	execvp /tmp/shome/jobs/job-129/job-script: No such file or directory
	//
	// for a file that had just been written successfully. It looked like a
	// script that failed to materialise, and was a script nobody could see.
	return scriptPathIn(sb), nil
}

func (b *Backend) Launch(ctx context.Context, sb *platform.Sandbox, spec job.Spec) (*platform.Handle, error) {
	// An interactive job's terminal replaces its output file; see
	// platform.Sandbox.TTY for why this shares the batch launch path.
	var out *os.File
	if sb.TTY == nil {
		outPath := filepath.Join(workDir(sb), fmt.Sprintf("slurm-%d.out", sb.JobID))
		var err error
		out, err = os.Create(outPath)
		if err != nil {
			return nil, fmt.Errorf("create output: %w", err)
		}
	}

	inner, err := JobArgv(spec.Args, func() (string, error) {
		return materialiseScript(sb, spec)
	})
	if err != nil {
		if out != nil {
			out.Close()
		}
		return nil, err
	}
	argv := b.sandboxArgv(sb, spec, inner)

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = workDir(sb)
	cmd.Env = envSlice(sb.Env, spec.Env)
	if sb.TTY != nil {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = sb.TTY, sb.TTY, sb.TTY
		// Setsid instead of Setpgid: it makes the job a session leader with
		// this terminal in charge, so Ctrl-C reaches the job rather than the
		// agent. Setting both is rejected.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	} else {
		cmd.Stdout, cmd.Stderr = out, out
		// Own process group, so the whole tree can be signalled and accounted.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}

	if err := cmd.Start(); err != nil {
		if out != nil {
			out.Close()
		}
		return nil, fmt.Errorf("start: %w", err)
	}
	// Move the job into its cgroup immediately after start. Doing it here
	// rather than pre-fork keeps shome's own process out of the job's limits.
	if b.Env.Cgroup == CgroupV2 && b.Env.CanDelegate {
		procsFile := filepath.Join(b.jobCgroup(sb.JobID), "cgroup.procs")
		os.WriteFile(procsFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	}

	h := &platform.Handle{JobID: sb.JobID, PID: cmd.Process.Pid, PGID: cmd.Process.Pid}
	b.procs.start(sb.JobID, cmd, out)
	return h, nil
}

func envSlice(base, extra map[string]string) []string {
	merged := map[string]string{
		"PATH": "/usr/local/bin:/usr/bin:/bin",
	}
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range extra {
		merged[k] = v
	}
	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	return out
}

// Sample reports the job's memory use.
//
// Prefers the cgroup's own accounting, which is exact and cheap; falls back to
// walking /proc when there is no cgroup.
func (b *Backend) Sample(ctx context.Context, h *platform.Handle) (platform.Usage, error) {
	u := platform.Usage{At: time.Now()}
	if b.Env.Cgroup == CgroupV2 && b.Env.CanDelegate {
		if v, err := readInt(filepath.Join(b.jobCgroup(h.JobID), "memory.current")); err == nil {
			u.MemBytes = v
			u.NProcs = countCgroupProcs(filepath.Join(b.jobCgroup(h.JobID), "cgroup.procs"))
			return u, nil
		}
	}
	bytes, n, err := procTreeFootprint(h.PGID)
	u.MemBytes, u.NProcs = bytes, n
	return u, err
}

func readInt(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

func countCgroupProcs(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

func (b *Backend) Throttle(ctx context.Context, h *platform.Handle, on bool) error {
	if h == nil {
		return fmt.Errorf("nil handle")
	}
	// cpu.weight is the cgroup v2 analogue of a nice value: 1 is the lowest
	// share, 100 the default. Unlike a hard quota it lets the job use idle
	// capacity, which is what "throttle, do not stop" should mean.
	if b.Env.Cgroup == CgroupV2 && b.Env.CanDelegate {
		w := "100"
		if on {
			w = "1"
		}
		if err := os.WriteFile(filepath.Join(b.jobCgroup(h.JobID), "cpu.weight"),
			[]byte(w), 0o644); err == nil {
			return nil
		}
	}
	// Fall back to renice on the process group.
	pri := 0
	if on {
		pri = 19
	}
	return syscall.Setpriority(syscall.PRIO_PGRP, h.PGID, pri)
}

func (b *Backend) Signal(ctx context.Context, h *platform.Handle, sig string) error {
	if h == nil {
		return fmt.Errorf("nil handle")
	}
	var s syscall.Signal
	switch strings.ToUpper(sig) {
	case "TERM":
		s = syscall.SIGTERM
	case "KILL":
		s = syscall.SIGKILL
	case "STOP":
		s = syscall.SIGSTOP
	case "CONT":
		s = syscall.SIGCONT
	default:
		return fmt.Errorf("unsupported signal %q", sig)
	}
	// Negative pid signals the whole process group.
	if err := syscall.Kill(-h.PGID, s); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

func (b *Backend) Wait(ctx context.Context, h *platform.Handle) (int, error) {
	return b.procs.wait(ctx, h.JobID)
}

// Cleanup removes everything Prepare created, and verifies it.
//
// Verifies rather than assumes: an early teardown reported success straight
// after seven permission failures, and only a residue check caught it.
func (b *Backend) Cleanup(ctx context.Context, sb *platform.Sandbox) error {
	if sb == nil || sb.ScratchDir == "" {
		return nil
	}
	if !strings.HasPrefix(filepath.Clean(sb.ScratchDir),
		filepath.Clean(b.jobsRoot())+string(os.PathSeparator)) {
		return fmt.Errorf("refusing to remove %q: outside %q", sb.ScratchDir, b.jobsRoot())
	}
	// Kill anything left in the cgroup before removing it; a non-empty cgroup
	// cannot be rmdir'd.
	if b.Env.Cgroup == CgroupV2 && b.Env.CanDelegate {
		dir := b.jobCgroup(sb.JobID)
		os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0o644)
		for i := 0; i < 20; i++ {
			if err := os.Remove(dir); err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if err := os.RemoveAll(sb.ScratchDir); err != nil {
		return err
	}
	if _, err := os.Stat(sb.ScratchDir); !os.IsNotExist(err) {
		return fmt.Errorf("scratch %q still present after removal", sb.ScratchDir)
	}
	return nil
}

// Aftermath has nothing to report on Linux yet.
//
// The kernel's memory accounting lives in a cgroup, and shome does not put a
// job in one of its own here: creating a cgroup needs delegation this agent
// may not have, and the job's processes therefore sit in whatever cgroup the
// agent was started in -- shared with the agent and with everything else on
// the machine. Reading its oom_kill counter would report kills that had
// nothing to do with this job, which is worse than reporting nothing.
//
// So memory on Linux is enforced by polling, as Capabilities says
// (MemLimit: Polled), and the caller falls back to the exit status. When
// shome grows its own cgroup per job -- which is also what would make the
// limit hard rather than polled -- this is where the answer comes from.
func (b *Backend) Aftermath(ctx context.Context, sb *platform.Sandbox) (platform.Aftermath, bool) {
	return platform.Aftermath{}, false
}

// OrphanScratch lists job directories that no live job owns. See the darwin
// implementation for why they can exist at all.
func (b *Backend) OrphanScratch(ctx context.Context, live []int64) ([]string, error) {
	return platform.OrphanScratchIn(b.jobsRoot(), live)
}

// IsolationFault reports whether this machine can confine a job.
//
// bubblewrap is the only filesystem isolation this backend has. Landlock is
// detected but not enforced, so it is not a substitute. Without bwrap the
// job path used to run the command with no wrapper at all -- able to read
// the machine owner's home and every other account's files -- while the tier
// report said so in text that nothing acted on.
func (b *Backend) IsolationFault() string {
	if !b.Env.HasBwrap {
		return "bubblewrap not installed: this machine cannot confine a job to " +
			"its own directory, so it will not accept cluster work " +
			"(install it: apt install bubblewrap)"
	}
	return ""
}

// SweepOrphanIsolation has nothing to do on Linux.
//
// A bubblewrap namespace is a process tree, started with --die-with-parent,
// so it goes when its launcher does. There is no equivalent of a container
// that outlives the command that started it.
func (b *Backend) SweepOrphanIsolation(ctx context.Context, live []int64) []string {
	return nil
}

// userHomeMount places the account's persistent storage inside a job.
func userHomeMount(sb *platform.Sandbox) []Mount {
	if sb.UserHome == "" || sb.UserHomeIn == "" {
		return nil
	}
	return []Mount{{Host: sb.UserHome, In: sb.UserHomeIn, Write: true}}
}

// homeInside is what $HOME becomes: the account's own storage where it has
// one, and otherwise the scratch directory.
func homeInside(sb *platform.Sandbox) string {
	if sb.UserHomeIn != "" {
		return sb.UserHomeIn
	}
	return JobScratchPath
}

// workDir is where the job runs on this machine: the account's storage, or
// the scratch directory for a job that has no account.
func workDir(sb *platform.Sandbox) string {
	if sb.WorkDir != "" {
		return sb.WorkDir
	}
	return sb.ScratchDir
}

// chdirInside is that directory as the namespace sees it.
func chdirInside(sb *platform.Sandbox) string {
	if sb.WorkDirIn != "" {
		return sb.WorkDirIn
	}
	return JobScratchPath
}
