//go:build darwin

package darwin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/maccontainer"
	"github.com/davidwu/shome/internal/platform"
	"github.com/davidwu/shome/internal/userenv"
	"github.com/davidwu/shome/internal/userfs"
)

// Backend is the macOS node backend. It creates no OS accounts (see docs/design-notes.md), so it
// needs no privilege and its teardown is a directory removal.
type Backend struct {
	Root      string // this agent's root; job scratch lives under Root/jobs
	OwnerHome string // denied to every job

	// StateRoot is the whole installation's directory, which on a controller
	// is the parent of Root. Disk accounting uses it because a contribution
	// cap is a cap on everything shome occupies -- the database, archived
	// output, staged data and user storage as well as scratch -- and
	// measuring only this agent's corner would under-report it several-fold.
	StateRoot string

	// Image is the container image sessions and jobs run in. Empty uses the
	// default.
	Image string

	// ctr is Apple's container runtime, detected once. A Seatbelt profile
	// cannot hide the host's filesystem -- it can refuse access but not move
	// a path -- so anything that must not see the machine runs in a Linux
	// container instead. See internal/maccontainer.
	ctrOnce sync.Once
	ctr     *maccontainer.Runtime
	ctrWhy  string

	// rt is the same runtime without the shared-mount check, for callers
	// that only need to ask it questions. See runtimeOnly.
	rtOnce sync.Once
	rt     *maccontainer.Runtime
	rtWhy  string
}

// container returns the runtime, detecting it once.
// runtimeOnly finds the container runtime without checking that a host
// directory mounts through it.
//
// The check in container() starts a probe container and creates the
// directory it probes, which is right before running work in one and wrong
// for merely asking what is running: the tidy-up sweep mounts nothing, and
// calling the full version from `shome nuke` recreated the state directory
// that had just been removed -- so nuke reported its own residue.
func (b *Backend) runtimeOnly() (*maccontainer.Runtime, string) {
	b.rtOnce.Do(func() {
		b.rt, b.rtWhy = maccontainer.Detect(context.Background(), "", b.Image)
	})
	return b.rt, b.rtWhy
}

func (b *Backend) container() (*maccontainer.Runtime, string) {
	b.ctrOnce.Do(func() {
		b.ctr, b.ctrWhy = maccontainer.Detect(context.Background(), "", b.Image)
		if b.ctr == nil {
			return
		}
		// Some host paths mount without sharing: writes appear inside and
		// never reach the host. Checked once, here, because the alternative
		// is a session that looks perfect and loses everything on exit.
		dir := filepath.Join(b.stateRoot(), "users")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			b.ctr, b.ctrWhy = nil, err.Error()
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := maccontainer.VerifyShared(ctx, *b.ctr, dir); err != nil {
			b.ctr, b.ctrWhy = nil, err.Error()
			return
		}

		// Prefer the image shome built, which has the session software in
		// it -- git, an editor, ssh. Falling back to the bare base image
		// when it has not been built yet leaves sessions working with less
		// in them, which beats refusing to start one.
		if b.Image != "" {
			return
		}
		extra, err := userenv.ExtraPackages(b.stateRoot())
		if err != nil {
			return
		}
		pkgs, err := userenv.Packages(extra)
		if err != nil {
			return
		}
		if tag := maccontainer.ImageTag(maccontainer.DefaultImage, pkgs); b.ctr.HasImage(ctx, tag) {
			b.ctr.Image = tag
		}
	})
	return b.ctr, b.ctrWhy
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
		ownerHome, _ = os.UserHomeDir()
	}
	return &Backend{Root: root, OwnerHome: ownerHome}
}

func (b *Backend) jobsRoot() string { return filepath.Join(b.Root, "jobs") }

// Tool locations differ across macOS versions (taskpolicy is in /usr/sbin, not
// /usr/bin). Resolve once and degrade loudly rather than failing every launch.
var (
	taskpolicyPath  = lookTool("taskpolicy", "/usr/sbin/taskpolicy", "/usr/bin/taskpolicy")
	sandboxExecPath = lookTool("sandbox-exec", "/usr/bin/sandbox-exec")
)

func lookTool(name string, candidates ...string) string {
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

func sysctlInt(name string) int64 {
	out, err := exec.Command("/usr/sbin/sysctl", "-n", name).Output()
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return v
}

func (b *Backend) Inventory(ctx context.Context) (platform.Capabilities, error) {
	c := platform.Capabilities{
		OS:       "darwin",
		Arch:     runtime.GOARCH,
		CPUs:     runtime.NumCPU(),
		MemBytes: sysctlInt("hw.memsize"),

		// Memory is polled, not kernel-hard: `taskpolicy -m` is not inherited
		// and does not survive execve (see docs/design-notes.md).
		MemLimit: platform.Polled,
		// No cgroups: CPU is admission control plus an inherited QoS clamp,
		// which measurably protects the owner but is not a quota.
		CPULimit: platform.Advisory,
		Tier:     "full",
	}
	if info, ok := DetectMetal(); ok {
		c.GPUs = 1 // Apple Silicon has no MPS/MIG equivalent: the GPU is allocated whole.
		c.GPUKind = "metal"
		c.GPUName = info.Name
		c.GPUMemBytes = info.RecommendedWorkingSet
		c.GPUMaxAllocBytes = info.MaxBufferLength
	}
	if sandboxExecPath == "" {
		c.Tier = "limited"
		c.Lost = append(c.Lost, "sandbox-exec unavailable: NO filesystem isolation")
	}
	if taskpolicyPath == "" {
		if c.Tier == "full" {
			c.Tier = "standard"
		}
		c.CPULimit = platform.None
		c.Lost = append(c.Lost, "taskpolicy unavailable: no QoS clamp, jobs will compete with the owner")
	}
	c.Lost = append(c.Lost,
		"memory enforced by polling (~100ms window), not a kernel cap",
		"no hard CPU quota; QoS clamp + admission control only")
	return c, nil
}

func (b *Backend) Prepare(ctx context.Context, spec job.Spec, id int64) (*platform.Sandbox, error) {
	scratch := filepath.Join(b.jobsRoot(), fmt.Sprintf("job-%d", id))
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return nil, fmt.Errorf("create scratch: %w", err)
	}
	// Isolate jobs from each other by denying the shared jobs root; the job's
	// own scratch is carved back out inside GenerateProfile, which emits the
	// re-allow AFTER the denies and with matching operations. See the comments
	// there -- the ordering and the operation match are both load-bearing.
	prof, err := GenerateProfile(ProfileConfig{
		ScratchDir: scratch,
		OwnerHome:  b.OwnerHome,
		// The whole installation, not just the jobs root. Everything shome
		// keeps lives under here -- the admin token, the mTLS CA private key,
		// the job database, every account's files -- and the profile's broad
		// (allow file-read*) means anything not explicitly denied is
		// readable. Denying only the jobs root left a job able to read
		// admin.token and act as a cluster administrator, which is a complete
		// escape from a boundary the rest of this file works hard to build.
		//
		// Denying the root rather than naming the sensitive files also means
		// a file added here later is covered without anyone remembering to.
		DenyPaths: []string{b.stateRoot(), b.jobsRoot()},
		// The account's own storage, so a GPU job -- which cannot run in a
		// container and so takes this path -- can still keep its work.
		WritePaths: userHomePaths(spec, b.stateRoot()),
		// macOS writes its own per-user state into $HOME whether the job
		// asked for it or not; the account's storage is not the place for
		// it. See machineLocalPaths.
		NoWritePaths: machineLocalPaths(userHomePaths(spec, b.stateRoot())),
		// shome's managed commands, and the machine's own software.
		//
		// A GPU job runs natively, so there is no image to give it git or
		// an editor and no mount to place shome's tools. Both have to be
		// reached where they already are. See userenv.MachineSoftware for
		// what this opens and why it is confined to the native path.
		ReadPaths: readableTools(b.stateRoot()),
		AllowGPU:  spec.Limits.GPUs > 0,
		AllowNet:  spec.Limits.Network,
	})
	if err != nil {
		return nil, err
	}
	profPath := filepath.Join(scratch, ".shome-profile.sb")
	if err := os.WriteFile(profPath, []byte(prof), 0o600); err != nil {
		return nil, fmt.Errorf("write profile: %w", err)
	}
	// Where the scratch will appear from inside, which depends on whether
	// this job is contained -- decided by the same rule Launch uses, so the
	// two cannot disagree.
	scratchIn := ""
	if b.useContainer(spec) {
		scratchIn = ContainerScratch
	}
	return &platform.Sandbox{
		JobID:       id,
		ScratchDir:  scratch,
		ScratchIn:   scratchIn,
		ProfilePath: profPath,
		Env: map[string]string{
			"SHOME_JOB_ID":   strconv.FormatInt(id, 10),
			"SHOME_SCRATCH":  scratch,
			"TMPDIR":         scratch,
			"SLURM_JOB_ID":   strconv.FormatInt(id, 10), // compatibility alias
			"SLURM_JOB_NAME": spec.Name,
		},
	}, nil
}

// appendTTYRules adds terminal permissions to an already-written profile.
func appendTTYRules(profilePath, ttyPath string) error {
	f, err := os.OpenFile(profilePath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("\n" + TTYRules(ttyPath))
	return err
}

// materialiseScript writes the job's script into its own scratch and returns
// the path to run.
//
// The original path is never executed: it may sit inside the owner's home,
// which this job's sandbox denies, and on a remote node it may not exist. The
// copy lives beside the job's other files and is removed with them.
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
	return path, nil
}

func (b *Backend) Launch(ctx context.Context, sb *platform.Sandbox, spec job.Spec) (*platform.Handle, error) {
	// taskpolicy applies the QoS clamp, which IS inherited across fork+exec
	// (see docs/design-notes.md) and holds the owner's p99 at idle levels under full load.
	//
	// Deliberately NOT passing `taskpolicy -m`: the memory ledger is lost on
	// the next execve, so wrapping sandbox-exec with it would enforce nothing
	// while looking like it did. Memory is enforced by Sample() polling.
	// A job with no GPU runs in a container, where the host is absent rather
	// than merely forbidden and the memory limit is enforced by the virtual
	// machine instead of by polling. A GPU job cannot: Metal does not exist
	// inside one.
	if b.useContainer(spec) {
		return b.launchContainer(ctx, sb, spec)
	}

	var argv []string
	if taskpolicyPath != "" {
		argv = append(argv, taskpolicyPath, "-c", "background")
	}
	if sb.ProfilePath != "" && sandboxExecPath != "" {
		argv = append(argv, sandboxExecPath, "-f", sb.ProfilePath)
	}
	if len(spec.Args) > 0 {
		// An argument vector runs directly; there is no script to place.
		argv = append(argv, spec.Args...)
	} else {
		scriptPath, err := materialiseScript(sb, spec)
		if err != nil {
			return nil, err
		}
		argv = append(argv, "/bin/sh", scriptPath)
	}
	// Array task index, in both the native and Slurm-aliased spellings so
	// unmodified Slurm scripts work.
	if spec.ArrayTaskID >= 0 {
		sb.Env["SHOME_ARRAY_TASK_ID"] = strconv.Itoa(spec.ArrayTaskID)
		sb.Env["SLURM_ARRAY_TASK_ID"] = strconv.Itoa(spec.ArrayTaskID)
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = workDir(sb)
	// Own process group: lets us signal and account for the whole tree even
	// after intermediate parents exit and children reparent to launchd.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// The agent's own environment, then the job's. The agent runs as the
	// machine's owner, so its HOME and USER name that person -- a job
	// inheriting them looks like it is running as the owner and writes into
	// a directory the sandbox denies. Both are replaced with the account's.
	env := os.Environ()
	if sb.UserHome != "" {
		// UserHomeIn, not UserHome: a natively-run job sees the host's own
		// path and the two are the same, but reading the mapped one keeps
		// this the same statement the container path makes.
		home := sb.UserHomeIn
		if home == "" {
			home = sb.UserHome
		}
		env = append(env, "HOME="+home, "SHOME_HOME="+home)
		if spec.User != "" {
			env = append(env, "USER="+spec.User, "LOGNAME="+spec.User)
		}
	}
	for k, v := range sb.Env {
		env = append(env, k+"="+v)
	}
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env

	var out *os.File
	if sb.TTY != nil {
		// The profile was generated by Prepare, before this terminal
		// existed, so the allowance for it is appended now. SBPL is
		// last-match-wins and nothing above mentions file-ioctl, so this is
		// the only rule governing it.
		if sb.ProfilePath != "" {
			if err := appendTTYRules(sb.ProfilePath, sb.TTY.Name()); err != nil {
				return nil, fmt.Errorf("permit terminal control: %w", err)
			}
		}
		// An interactive job: its terminal is all three streams, and it
		// becomes the session leader so job control and Ctrl-C work.
		cmd.Stdin, cmd.Stdout, cmd.Stderr = sb.TTY, sb.TTY, sb.TTY
		cmd.SysProcAttr.Setsid = true
		cmd.SysProcAttr.Setctty = true
		// Setsid supersedes Setpgid, and setting both is rejected.
		cmd.SysProcAttr.Setpgid = false
	} else {
		outPath := filepath.Join(workDir(sb), "slurm-"+strconv.FormatInt(sb.JobID, 10)+".out")
		var err error
		out, err = os.Create(outPath)
		if err != nil {
			return nil, fmt.Errorf("create output file: %w", err)
		}
		cmd.Stdout, cmd.Stderr = out, out
	}

	if err := cmd.Start(); err != nil {
		if out != nil {
			out.Close()
		}
		return nil, fmt.Errorf("launch: %w", err)
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		pgid = cmd.Process.Pid
	}
	h := &platform.Handle{JobID: sb.JobID, PID: cmd.Process.Pid, PGID: pgid}
	pr := &proc{cmd: cmd, out: out, done: make(chan struct{})}
	procs.put(h.JobID, pr)
	go pr.reap()
	return h, nil
}

func (b *Backend) Sample(ctx context.Context, h *platform.Handle) (platform.Usage, error) {
	// A containerised job is not visible in the host process tree: the only
	// host process is the launcher, and the job runs inside a virtual
	// machine. Polling the tree would report a few megabytes for a job using
	// gigabytes, so its peak memory would be recorded as nothing.
	if p := procs.get(h.JobID); p != nil && p.container != "" {
		rt, _ := b.container()
		if rt == nil {
			return platform.Usage{}, nil
		}
		st, err := rt.Usage(ctx, p.container)
		if err != nil {
			// A container that has just exited has no stats, which is not a
			// failure worth propagating into the polling loop.
			return platform.Usage{}, nil
		}
		return platform.Usage{At: time.Now(), MemBytes: st.MemoryUsageBytes,
			NProcs: st.NumProcesses}, nil
	}
	if h == nil {
		return platform.Usage{}, fmt.Errorf("nil handle")
	}
	// A containerised job's usage is inside a virtual machine, where the
	// host's process tree cannot see it: polling would report the
	// launcher's few megabytes.
	if p := procs.get(h.JobID); p != nil && p.container != "" {
		return sampleContainer(ctx, p.container)
	}
	bytes, n, err := TreeFootprint(h.PGID)
	if err != nil {
		return platform.Usage{}, err
	}
	return platform.Usage{At: time.Now(), MemBytes: bytes, NProcs: n}, nil
}

// Throttle demotes or restores a running job's priority.
//
// `taskpolicy -b -p` works on already-running processes (unlike `-m -p`, which
// is a silent no-op), so a job can be demoted after the fact rather than only
// suspended or killed.
func (b *Backend) Throttle(ctx context.Context, h *platform.Handle, on bool) error {
	if h == nil {
		return fmt.Errorf("nil handle")
	}
	if taskpolicyPath == "" {
		return platform.ErrAdvisory
	}
	flag := "-B" // restore
	if on {
		flag = "-b" // background
	}
	pids, err := TreePIDs(h.PGID)
	if err != nil {
		return err
	}
	var firstErr error
	for _, pid := range pids {
		cmd := exec.CommandContext(ctx, taskpolicyPath, flag, "-p", strconv.Itoa(pid))
		if err := cmd.Run(); err != nil && firstErr == nil {
			// A rank that exited between listing and signalling is normal.
			firstErr = err
		}
	}
	return firstErr
}

func (b *Backend) Signal(ctx context.Context, h *platform.Handle, sig string) error {
	if h == nil {
		return fmt.Errorf("nil handle")
	}
	var s syscall.Signal
	switch strings.ToUpper(strings.TrimPrefix(sig, "SIG")) {
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
	// A containerised job is not stopped by signalling the process that
	// started it: the container keeps running, holding its memory and CPUs,
	// with nobody attached. Stop it by name and let the launcher exit.
	if p := procs.get(h.JobID); p != nil && p.container != "" {
		if s == syscall.SIGKILL || s == syscall.SIGTERM {
			if err := stopContainer(ctx, p.container); err != nil {
				return err
			}
		}
		// Suspend and resume have no container equivalent here; signalling
		// the launcher would stop the wrong thing, so say so rather than
		// silently doing nothing.
		if s == syscall.SIGSTOP || s == syscall.SIGCONT {
			return fmt.Errorf("cannot suspend a containerised job")
		}
		return nil
	}
	// Negative pid signals the whole process group.
	if err := syscall.Kill(-h.PGID, s); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

func (b *Backend) Wait(ctx context.Context, h *platform.Handle) (int, error) {
	if h == nil {
		return -1, fmt.Errorf("nil handle")
	}
	pr := procs.get(h.JobID)
	if pr == nil {
		return -1, fmt.Errorf("no process record for job %d", h.JobID)
	}
	select {
	case <-pr.done:
		return pr.code, nil
	case <-ctx.Done():
		return -1, ctx.Err()
	}
}

// Cleanup removes the job's scratch tree and verifies it is gone.
//
// It verifies rather than assumes: an early teardown script printed
// "removed" immediately after seven EPERM failures, and only a residue check
// caught it. Cleanup that reports success without checking is worse than none,
// because it stops you looking.
func (b *Backend) Cleanup(ctx context.Context, sb *platform.Sandbox) error {
	if sb == nil || sb.ScratchDir == "" {
		return nil
	}
	// Guard: only ever remove inside our own jobs root.
	if !strings.HasPrefix(filepath.Clean(sb.ScratchDir), filepath.Clean(b.jobsRoot())+string(os.PathSeparator)) {
		return fmt.Errorf("refusing to remove %q: outside %q", sb.ScratchDir, b.jobsRoot())
	}
	if err := os.RemoveAll(sb.ScratchDir); err != nil {
		return fmt.Errorf("remove scratch: %w", err)
	}
	if _, err := os.Stat(sb.ScratchDir); !os.IsNotExist(err) {
		return fmt.Errorf("scratch %q still present after removal", sb.ScratchDir)
	}
	// A container outlives the process that started it, so removing the
	// scratch directory is not the whole of "leave no residue": the virtual
	// machine would keep running, holding its memory and CPUs, with its
	// working directory already deleted underneath it.
	if p := procs.get(sb.JobID); p != nil && p.container != "" {
		if err := stopContainer(ctx, p.container); err != nil {
			// Already gone is the ordinary case -- --rm removes it when the
			// job exits cleanly -- so this is a warning's worth, not an
			// error that fails the cleanup and leaves the scratch behind.
			_ = err
		}
	}
	procs.del(sb.JobID)
	return nil
}

// SweepOrphanIsolation stops containers left behind by jobs that are no
// longer running.
//
// The same problem as orphaned scratch directories, and worse: a directory
// merely takes space, while a container holds a virtual machine's memory and
// CPUs indefinitely. They accumulate whenever the agent dies without
// finishing a job -- a crash, a kill, a machine that slept -- because the
// runtime does not stop a container when its launcher goes away.
func (b *Backend) SweepOrphanIsolation(ctx context.Context, live []int64) []string {
	rt, _ := b.runtimeOnly()
	if rt == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, rt.Bin, "list", "--format", "json").Output()
	if err != nil {
		return nil
	}
	var rows []struct {
		Configuration struct {
			ID    string `json:"id"`
			Image struct {
				Reference string `json:"reference"`
			} `json:"image"`
		} `json:"configuration"`
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil
	}
	liveSet := map[int64]bool{}
	for _, id := range live {
		liveSet[id] = true
	}
	liveSessions := b.liveSessions()
	// Collected first, stopped together: see stopAll.
	var doomed []string
	for _, r := range rows {
		name := r.ID
		if name == "" {
			name = r.Configuration.ID
		}
		if !strings.HasPrefix(name, maccontainer.Prefix) {
			// Not named by shome. Almost always somebody else's work, and
			// left alone -- except when it is running an image shome built,
			// which nothing else has a reason to run.
			//
			// That case exists because a container started without a name
			// gets a UUID from the runtime, and shome once had a path that
			// did: its own test suite, which left a virtual machine behind
			// on every run. The name cannot identify those; the image can,
			// and it is the only handle left for reclaiming them.
			if !isShomeImage(r.Configuration.Image.Reference) {
				continue
			}
			b.log("stopping an unnamed container running a shome image", name)
			doomed = append(doomed, name)
			continue
		}
		isJob := strings.HasPrefix(name, b.jobPrefix())
		isSession := strings.HasPrefix(name, b.sessionPrefix())
		if !isJob && !isSession {
			// Named by shome, but not by this installation. Either another
			// installation on this machine -- whose running jobs are no more
			// ours to stop than anybody else's, and stopping them killed a
			// job one was running -- or a container from before names
			// carried an installation tag at all.
			//
			// The untagged ones are swept: they can only have been left by a
			// shome that is no longer running, since the version that made
			// them has been replaced by this one, and a leaked container
			// holds a virtual machine's memory until something stops it.
			if hasInstallTag(name) {
				continue
			}
			b.log("stopping a container from an older shome (no installation tag)", name)
		}
		if isJob {
			if id, ok := jobIDFromContainer(name, b.jobPrefix()); ok && liveSet[id] {
				continue // still running
			}
		}
		if isSession && liveSessions[name] {
			continue
		}
		doomed = append(doomed, name)
	}
	return stopAll(ctx, doomed)
}

// stopAll stops containers together rather than one after another.
//
// Each stop waits out a shutdown grace period, so a queue of them added up:
// twenty-two leaked containers took nearly two minutes, and the sweep runs
// before the agent starts heartbeating, so the machine was absent from the
// cluster for all of it. Nothing about them is ordered, so they go at once.
func stopAll(ctx context.Context, names []string) []string {
	if len(names) == 0 {
		return nil
	}
	var mu sync.Mutex
	var stopped []string
	var wg sync.WaitGroup
	// Bounded, because each stop is a virtual machine being torn down and
	// the runtime is not helped by being asked for all of them at once.
	sem := make(chan struct{}, 8)
	for _, name := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := stopContainer(ctx, name); err != nil {
				return
			}
			mu.Lock()
			stopped = append(stopped, name)
			mu.Unlock()
		}(name)
	}
	wg.Wait()
	sort.Strings(stopped)
	return stopped
}

// isShomeImage reports whether an image reference is one shome runs
// sessions on.
//
// Both of them: the image shome builds for this machine, and the uv image it
// falls back to when it has not been able to build one. A container running
// either, with no name to identify it, can only have come from shome.
func isShomeImage(ref string) bool {
	return strings.HasPrefix(ref, maccontainer.ImageRepo+":") ||
		ref == maccontainer.DefaultImage ||
		strings.HasPrefix(ref, maccontainer.DefaultImage+"@")
}

// hasInstallTag reports whether a shome container name carries an
// installation tag, and so belongs to a shome that may still be running.
//
// The tag is six hex characters followed by a separator, immediately after
// the job or session prefix. A name without one was made by a version that
// did not tag them.
func hasInstallTag(name string) bool {
	for _, p := range []string{ContainerPrefix, SessionPrefix} {
		if !strings.HasPrefix(name, p) {
			continue
		}
		rest := strings.TrimPrefix(name, p)
		if len(rest) < 7 || rest[6] != '-' {
			return false
		}
		for _, r := range rest[:6] {
			if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
				return false
			}
		}
		return true
	}
	return false
}

// log reports a sweep decision. The backend has no logger of its own, so
// this goes to the standard one, which the daemon points at its log file.
func (b *Backend) log(msg, name string) {
	slog.Info(msg, "container", name)
}

// jobIDFromContainer reads the job id back out of a container name, given
// the prefix this installation uses.
func jobIDFromContainer(name, prefix string) (int64, bool) {
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}
	rest := strings.TrimPrefix(name, prefix)
	if i := strings.IndexByte(rest, '-'); i >= 0 {
		rest = rest[:i]
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	return id, err == nil
}

var _ platform.Backend = (*Backend)(nil)

// OrphanScratch lists job directories under the jobs root that no live job
// owns.
//
// Matched by directory name rather than by anything recorded elsewhere, so it
// works after a crash that lost every other trace of the job -- which is
// exactly when it is needed.
func (b *Backend) OrphanScratch(ctx context.Context, live []int64) ([]string, error) {
	return platform.OrphanScratchIn(b.jobsRoot(), live)
}

// IsolationFault reports whether this machine can confine a job.
//
// sandbox-exec is the only filesystem isolation this backend has. Launch used
// to include it only when the tool was found and otherwise run the job with
// no profile at all -- unconfined, with no indication -- which is exactly the
// failure a cluster must not have quietly.
func (b *Backend) IsolationFault() string {
	if _, why := b.container(); why != "" {
		return why
	}
	if sandboxExecPath == "" {
		return "sandbox-exec not found: this machine cannot confine a GPU job to " +
			"its own directory, so it will not accept cluster work"
	}
	return ""
}

// userHomePaths is the account's persistent storage, if it has an account.
//
// Prepare runs before the agent fills in Sandbox.UserHome, so the path is
// derived here from the same rule the agent uses: one directory per account,
// beside the others, under the installation root.
func userHomePaths(spec job.Spec, stateRoot string) []string {
	if spec.User == "" || stateRoot == "" {
		return nil
	}
	if err := userfs.ValidUser(spec.User); err != nil {
		return nil
	}
	return []string{filepath.Join(stateRoot, "users", spec.User)}
}

// machineLocalPaths are the corners of an account's storage that macOS
// would fill with state belonging to this machine.
//
// A job that runs natively -- a GPU job, since Metal does not exist inside a
// virtual machine -- has HOME pointing at the account's own directory, and
// the frameworks it links take that as licence to create ~/Library. Apple's
// python does it for a bytecode cache the moment a script is imported, and
// it is not alone.
//
// That directory is wrong three times over: it is machine-local state in the
// one tree that is meant to be portable, it is counted against the account's
// disk quota, and it appears in `shome fs ls` beside the account's own work
// as though the account had put it there. So the writable tree has this
// corner cut out of it. What lands there is a cache, and a cache that cannot
// be written is a cache that is not used -- which is the correct outcome for
// something that could not have travelled with the account anyway.
//
// Denied for writing rather than for reading: a home that already collected
// one before this rule existed stays listable, so the account can see it and
// remove it from the login node.
func machineLocalPaths(homes []string) []string {
	out := make([]string, 0, len(homes))
	for _, h := range homes {
		if h == "" {
			continue
		}
		out = append(out, filepath.Join(h, "Library"))
	}
	return out
}

// readableTools are the read-only trees a natively-run job needs on top of
// the OS's own: shome's managed commands, and wherever this machine keeps
// the base software.
//
// Only the native path uses these -- a containerised job has the image and
// never reads the profile -- but they are put in every profile because
// whether a job is contained is decided at launch, not here.
func readableTools(stateRoot string) []string {
	trees := userenv.MachineSoftware()
	out := make([]string, 0, len(trees)+1)
	out = append(out, userenv.ReadOnlyPaths(stateRoot)...)
	return append(out, userenv.SoftwareRoots(trees)...)
}

// workDir is where the job runs on this machine: the account's storage, or
// the scratch directory for a job that has no account.
func workDir(sb *platform.Sandbox) string {
	if sb.WorkDir != "" {
		return sb.WorkDir
	}
	return sb.ScratchDir
}
