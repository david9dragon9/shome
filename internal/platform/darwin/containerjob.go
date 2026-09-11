//go:build darwin

package darwin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/maccontainer"
	"github.com/davidwu/shome/internal/platform"
)

// Running a job inside a Linux container on a Mac.
//
// # Which jobs
//
// Everything that does not ask for a GPU. A container is a virtual machine
// and has no Metal, so a GPU job has to run natively -- that is the one case
// where a job still sees the host's filesystem layout, and it is the price of
// the thing Mac nodes exist for.
//
// # What it buys
//
// The same isolation an account gets on a Linux node: the job's scratch
// directory is the only thing mounted, and the host is not merely forbidden
// but absent, so a job cannot tell whether a path on the host exists.
//
// It also makes the memory limit real. Native macOS has no per-process memory
// cap, so shome polls the process tree and kills it after the fact, with a
// window of about a tenth of a second (see docs/design-notes.md). A container gets a hard limit
// from the virtual machine, and the kernel kills the job at the boundary.

// ContainerScratch is where a job's working directory appears inside.
const ContainerScratch = "/scratch"

// Timeouts on talking to the container runtime.
//
// Every one of these calls shells out to a process that manages virtual
// machines, and a wedged runtime must not become a wedged agent. The
// supervisor samples memory through `container stats` on every tick, so one
// call that never returned would stop memory enforcement for every job on
// the machine; the startup sweep calls `container list`, so one there would
// stop the node joining the cluster at all -- with no diagnostic, because
// nothing had failed yet.
//
// Sampling gets the shorter one: it happens constantly, another sample is
// along in a moment, and a missed one costs only a gap in the record. The
// control operations get longer, because giving up on stopping a container
// leaves a virtual machine running.
const (
	sampleTimeout  = 5 * time.Second
	controlTimeout = 30 * time.Second
)

// MinContainerMemMB is the smallest memory limit the runtime accepts.
//
// Below it, `container run` refuses to start at all -- so a job asking for
// less than this would not run a weaker limit, it would fail outright with a
// message about the runtime rather than about the job. Clamping means a job
// that asked for 100 MiB is held to 200 instead: a looser cap than requested,
// bounded and small, and the scheduler still accounts the figure the job
// asked for.
const MinContainerMemMB = 200

// containerMemMB is the limit to hand the runtime for a job.
//
// A job with no memory limit must not silently get the runtime's default,
// which is a single gigabyte. Native jobs on the same machine may use all of
// it, so a containerised one that quietly stopped at 1 GiB would be a
// difference nobody asked for and nobody would see until something died.
func containerMemMB(spec job.Spec, machineBytes int64) int64 {
	if spec.Limits.MemBytes > 0 {
		// Deliberately above what the job asked for.
		//
		// The virtual machine's limit is a hard kill with no explanation:
		// the job dies with SIGKILL, and if its script backgrounded the
		// offending child and used a bare `wait` -- which returns zero
		// whatever the child did -- the job reports success. shome's own
		// poller reports OOM properly, naming the limit and the usage, but
		// only if it gets to see the job over the line first.
		//
		// So the hard limit sits above the requested one and acts as a
		// backstop for a spike too fast to poll, while the ordinary case is
		// caught and explained by shome.
		mb := spec.Limits.MemBytes >> 20
		headroom := mb / 4
		if headroom < 64 {
			headroom = 64
		}
		mb += headroom
		if mb < MinContainerMemMB {
			return MinContainerMemMB
		}
		return mb
	}
	// No limit asked for: everything the machine has, less a little for the
	// host. The owner's own contribution cap is applied by the scheduler
	// before a job is ever placed here.
	if machineBytes <= 0 {
		return 0
	}
	mb := (machineBytes >> 20) - 1024
	if mb < MinContainerMemMB {
		return MinContainerMemMB
	}
	return mb
}

// ContainerPrefix marks every container shome starts, so leftovers can be
// found and cleaned up.
const ContainerPrefix = "shome-job-"

// containerName is the container id for a job, so it can be stopped later.
//
// Killing the `container run` process does not stop the container -- it keeps
// running with nobody attached -- so the lifecycle has to be managed by name.
//
// The job id alone is not unique: ids are numbered per controller database,
// so a rebuilt cluster starts again at 1, two clusters on one machine collide
// immediately, and a container left behind by a crash blocks the id when it
// comes round again. The random suffix makes the name unique to this run; the
// job id is in it so a person reading `container list` can tell what it is.
// The installation's tag comes first, so a machine running two shome
// installations -- a second SHOME_ROOT, which is what testing one looks like
// -- can tell its own containers from the other's. Without it the tidy-up
// sweep stopped whatever it found, including another installation's running
// jobs.
func containerName(tag string, id int64) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Only reachable if the system entropy source fails, and a
		// timestamp still separates two runs of the same id.
		return fmt.Sprintf("%s%s-%d-%d", ContainerPrefix, tag, id, time.Now().UnixNano())
	}
	return fmt.Sprintf("%s%s-%d-%s", ContainerPrefix, tag, id, hex.EncodeToString(b[:]))
}

// jobPrefix is what every container this installation starts for a job is
// named with.
func (b *Backend) jobPrefix() string {
	return ContainerPrefix + maccontainer.InstallTag(b.stateRoot()) + "-"
}

// sessionPrefix is the same for login sessions. See loginnode.isolationName,
// which builds the names this matches.
func (b *Backend) sessionPrefix() string {
	return SessionPrefix + maccontainer.InstallTag(b.stateRoot()) + "-"
}

// useContainer reports whether this job should run in a container.
func (b *Backend) useContainer(spec job.Spec) bool {
	if spec.Limits.GPUs > 0 {
		return false // Metal does not exist inside a virtual machine
	}
	rt, _ := b.container()
	return rt != nil
}

// launchContainer starts a job inside a container.
func (b *Backend) launchContainer(ctx context.Context, sb *platform.Sandbox,
	spec job.Spec) (*platform.Handle, error) {

	rt, why := b.container()
	if rt == nil {
		return nil, fmt.Errorf("%s", why)
	}
	// Only when there is a script to place: an argument vector runs
	// directly, and demanding a script for one is how `srun /bin/echo hi`
	// started failing with "job has neither a script body nor a path".
	inside := ""
	if len(spec.Args) == 0 {
		scriptPath, err := materialiseScript(sb, spec)
		if err != nil {
			return nil, err
		}
		// The script lives in the scratch directory, which is mounted, so
		// its path inside follows from where that lands.
		inside = ContainerScratch + "/" + filepath.Base(scriptPath)
	}

	env := map[string]string{}
	for k, v := range sb.Env {
		env[k] = v
	}
	for k, v := range spec.Env {
		env[k] = v
	}
	// The scratch variables have to name the path the job will actually see.
	env["SHOME_SCRATCH"] = ContainerScratch
	env["TMPDIR"] = ContainerScratch
	if spec.ArrayTaskID >= 0 {
		env["SHOME_ARRAY_TASK_ID"] = strconv.Itoa(spec.ArrayTaskID)
		env["SLURM_ARRAY_TASK_ID"] = strconv.Itoa(spec.ArrayTaskID)
	}

	argv := spec.Args
	if len(argv) == 0 {
		argv = []string{"/bin/sh", inside}
	}
	// Wrapped so that what the kernel did to the job is recorded before the
	// container goes away with the answer. See containerRunnerScript.
	//
	// An error here means the scratch directory cannot be written, which the
	// job itself could not survive either -- the script it is about to run
	// lives there.
	wrapper, err := writeContainerRunner(sb.ScratchDir)
	if err != nil {
		return nil, err
	}
	argv = append([]string{"/bin/sh", ContainerScratch + "/" + wrapper}, argv...)
	// Generated once: recomputing it would produce a different name and
	// leave the container unstoppable.
	name := containerName(maccontainer.InstallTag(b.stateRoot()), sb.JobID)
	// $HOME is the account's own storage, which persists; the working
	// directory is the job's scratch, which does not. A job that wants to
	// keep something writes it under $HOME and finds it there next time,
	// and in `shome fs`.
	if sb.UserHome != "" {
		env["HOME"] = sb.UserHomeIn
		env["SHOME_HOME"] = sb.UserHomeIn
	}
	cs := maccontainer.Spec{
		Home: sb.ScratchDir, HomeIn: ContainerScratch,
		// The job runs in the account's storage, which extraMounts places
		// inside. The scratch is mounted too and is still $SHOME_SCRATCH,
		// but it is no longer where the job stands.
		Workdir: sb.WorkDirIn,
		Extra:   extraMounts(sb),
		User:    spec.User, Env: env, Argv: argv,
		Network: spec.Limits.Network,
		Name:    name,
		CPUs:    spec.Limits.CPUs,
		MemMB:   containerMemMB(spec, sysctlInt("hw.memsize")),
		// srun --pty needs a terminal inside the container as well as one on
		// the host. Without the first, the shell reads end-of-file
		// immediately and the session is over before it starts.
		TTY: sb.TTY != nil,
	}
	cmd := rt.Command(ctx, cs)

	var out *os.File
	if sb.TTY != nil {
		// The host terminal is the launcher's stdio, and the launcher relays
		// it to the container's own. Session leader for the same reason the
		// native path is one: Ctrl-C must reach the job, not the agent.
		cmd.Stdin, cmd.Stdout, cmd.Stderr = sb.TTY, sb.TTY, sb.TTY
		// Session leader, with this terminal in charge. Not for the reason
		// the native path is one -- the job's shell has its own controlling
		// terminal inside the container, allocated by --tty -- but because
		// a window resize is delivered as SIGWINCH to the foreground
		// process group of the terminal that changed, and a launcher with
		// no controlling terminal is in no such group. It never hears, so
		// it never resizes the terminal inside, and the session stays the
		// size it started at however the user's window moves: a typed line
		// wraps back over itself at the old width, and full-screen programs
		// draw to the wrong shape.
		//
		// Setsid supersedes Setpgid, and setting both is rejected.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		outPath := filepath.Join(workDir(sb),
			"slurm-"+strconv.FormatInt(sb.JobID, 10)+".out")
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
		return nil, fmt.Errorf("launch in a container: %w", err)
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		pgid = cmd.Process.Pid
	}
	h := &platform.Handle{JobID: sb.JobID, PID: cmd.Process.Pid, PGID: pgid}
	pr := &proc{cmd: cmd, out: out, done: make(chan struct{}), container: name}
	procs.put(h.JobID, pr)
	go pr.reap()
	return h, nil
}

// stopContainer ends a job's container.
//
// Necessary because signalling the `container run` process does not: the
// container outlives it, still holding its memory and CPUs, and the next
// thing to notice would be a machine that has quietly filled up with the
// remains of cancelled jobs.
func stopContainer(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "container", "stop", "--time", "5", name).
		CombinedOutput()
	if err != nil {
		// Already gone is the ordinary case, not a problem: a job or session
		// that exited cleanly has had its container removed by --rm before
		// anyone asks it to stop. Reporting that would put a warning in the
		// log on every clean logout.
		if strings.Contains(string(out), "notFound") ||
			strings.Contains(string(out), "not found") {
			return nil
		}
		return fmt.Errorf("stop container %s: %w (%s)", name, err, firstLine(out))
	}
	return nil
}

func firstLine(b []byte) string {
	for i, c := range b {
		if c == '\n' {
			return string(b[:i])
		}
	}
	return string(b)
}

// The container runner: the job's command with a note taken afterwards.
//
// A containerised job's memory limit is enforced by the virtual machine's
// kernel, which kills the offending process and records that it did. The
// recording is inside the container, in its cgroup, and the container is
// destroyed when the job ends -- so it has to be read out before then, and
// the only thing still running in there at that point is the job itself.
//
// Hence a wrapper rather than a question asked afterwards. It runs the job,
// then copies two numbers out of the cgroup into the job's scratch
// directory, which is a mount of a host directory and therefore still there
// when the container is not.
//
// The wrapper's own exit status is the job's, unchanged. Nothing here can
// fail the job: every read is guarded, because a job must not be lost over
// its own accounting.
const containerRunnerName = ".shome-runner"

// cgroupNotesFile is where the runner leaves what it read.
const cgroupNotesFile = ".shome-cgroup"

const containerRunnerScript = `#!/bin/sh
# shome container runner (generated -- do not edit)
#
# Runs the job, then records what the kernel did to it. See containerjob.go.
"$@"
rc=$?
{
  # cgroup v2. oom_kill counts processes the kernel killed for memory, which
  # is the only reliable signal when the one that died was a background child
  # whose status a bare 'wait' reported as zero. memory.peak is the true high
  # water mark, where polling from outside sees only what it happened to
  # catch.
  kills=$(awk '/^oom_kill /{print $2}' /sys/fs/cgroup/memory.events 2>/dev/null)
  peak=$(cat /sys/fs/cgroup/memory.peak 2>/dev/null)
  printf 'oom_kill %s\npeak %s\n' "${kills:-0}" "${peak:-0}"
} > "$SHOME_SCRATCH/` + cgroupNotesFile + `" 2>/dev/null
exit $rc
`

// writeContainerRunner places the runner in the job's scratch and returns
// its name there.
func writeContainerRunner(scratch string) (string, error) {
	path := filepath.Join(scratch, containerRunnerName)
	if err := os.WriteFile(path, []byte(containerRunnerScript), 0o700); err != nil {
		return "", err
	}
	return containerRunnerName, nil
}

// Aftermath reads what the runner recorded.
//
// Absent notes are not an error: a native job has no runner, a job killed
// before it started has nothing to say, and a job whose container died
// outright never got to write them. In every one of those cases the caller
// should fall back to the exit status rather than assume anything.
func (b *Backend) Aftermath(ctx context.Context, sb *platform.Sandbox) (platform.Aftermath, bool) {
	if sb == nil || sb.ScratchDir == "" {
		return platform.Aftermath{}, false
	}
	data, err := os.ReadFile(filepath.Join(sb.ScratchDir, cgroupNotesFile))
	if err != nil {
		return platform.Aftermath{}, false
	}
	return parseCgroupNotes(string(data))
}

// parseCgroupNotes reads the runner's two numbers.
func parseCgroupNotes(s string) (platform.Aftermath, bool) {
	var out platform.Aftermath
	any := false
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		n, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "oom_kill":
			out.OOMKills, any = int(n), true
		case "peak":
			out.PeakBytes, any = n, true
		}
	}
	return out, any
}

// containerStats is what the runtime reports about a running container.
type containerStats struct {
	ID          string `json:"id"`
	MemoryUsage int64  `json:"memoryUsageBytes"`
	NumProcs    int    `json:"numProcesses"`
}

// sampleContainer reads a containerised job's real usage.
//
// Polling the host process tree measures the launcher, which sits at a few
// megabytes while the job inside uses gigabytes -- so peak memory in `sacct`
// would be nonsense and the polled memory killer would never fire. The
// runtime knows the real figure; ask it.
//
// The memory limit is enforced by the virtual machine regardless, so this is
// for accounting rather than for enforcement. That is why a failure here is
// reported rather than swallowed: a wrong number is worse than none.
func sampleContainer(ctx context.Context, name string) (platform.Usage, error) {
	ctx, cancel := context.WithTimeout(ctx, sampleTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "container", "stats",
		"--format", "json", "--no-stream", name).Output()
	if err != nil {
		return platform.Usage{}, fmt.Errorf("container stats %s: %w", name, err)
	}
	var rows []containerStats
	if err := json.Unmarshal(out, &rows); err != nil {
		return platform.Usage{}, fmt.Errorf("container stats %s: %w", name, err)
	}
	for _, r := range rows {
		if r.ID == name || len(rows) == 1 {
			return platform.Usage{At: time.Now(),
				MemBytes: r.MemoryUsage, NProcs: r.NumProcs}, nil
		}
	}
	return platform.Usage{}, fmt.Errorf("container stats %s: not reported", name)
}

// SessionPrefix marks a container started for a login session.
const SessionPrefix = "shome-session-"

// StopIsolation ends a session's container.
func (b *Backend) StopIsolation(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	noteSession(name, false)
	return stopContainer(ctx, name)
}

// sessions tracks the containers of sessions currently running, so a sweep
// does not stop one that is in use.
var sessions = struct {
	mu sync.Mutex
	m  map[string]bool
}{m: map[string]bool{}}

// noteSession records a session's container as live.
func noteSession(name string, live bool) {
	if name == "" {
		return
	}
	sessions.mu.Lock()
	if live {
		sessions.m[name] = true
	} else {
		delete(sessions.m, name)
	}
	sessions.mu.Unlock()
}

// liveSessions is the set a sweep must leave alone.
func (b *Backend) liveSessions() map[string]bool {
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	out := make(map[string]bool, len(sessions.m))
	for k := range sessions.m {
		out[k] = true
	}
	return out
}

// SessionHostAddr is the host's address on the container network.
//
// A session in a container cannot use the unix socket in its own directory:
// that directory arrives over virtiofs, which exposes the socket's inode and
// then fails the connect with "operation not supported".
func (b *Backend) SessionHostAddr(ctx context.Context) string {
	rt, _ := b.container()
	if rt == nil {
		return ""
	}
	addr, err := rt.Gateway(ctx)
	if err != nil {
		return ""
	}
	return addr
}

// extraMounts are directories a job needs beyond its scratch.
func extraMounts(sb *platform.Sandbox) []maccontainer.Mount {
	var out []maccontainer.Mount
	if sb.UserHome != "" {
		out = append(out, maccontainer.Mount{
			Host: sb.UserHome, In: sb.UserHomeIn, Write: true,
		})
	}
	// shome's own commands, read-only, where the job's PATH says they are.
	// The Linux build: this container cannot execute the host's.
	if sb.ToolsHost != "" && sb.ToolsIn != "" {
		out = append(out, maccontainer.Mount{Host: sb.ToolsHost, In: sb.ToolsIn})
	}
	return out
}
