// Package agent is the shome node agent: it executes and supervises jobs on
// one machine and reports to the controller.
//
// Supervision lives here rather than in the controller because it must survive
// the controller being unreachable. A home controller will be closed, slept and
// restarted; a job's memory limit must still be enforced while that happens.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/davidwu/shome/internal/agentapi"
	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/owner"
	"github.com/davidwu/shome/internal/platform"
	"github.com/davidwu/shome/internal/ptyx"
	"github.com/davidwu/shome/internal/userenv"
	"github.com/davidwu/shome/internal/userfs"
)

// exitKilled is the shell's exit status for a process killed by SIGKILL.
//
// What a kernel-enforced memory limit looks like from outside: the job is
// killed at the boundary rather than reported over it, so there is no usage
// figure to quote back -- only the limit it hit.
const exitKilled = 137

// Tick bounds how far a job can exceed its memory limit before being killed.
// macOS has no kernel cap that survives execve (see docs/design-notes.md), so this loop is the
// enforcement; an aggressive allocator was measured at ~130 MiB/100ms.
const Tick = 100 * time.Millisecond

// TermGrace is the pause between SIGTERM and SIGKILL.
const TermGrace = 10 * time.Second

type exit struct {
	code int
	err  error
}

type running struct {
	spec    job.Spec
	id      int64
	sandbox *platform.Sandbox
	handle  *platform.Handle
	started time.Time
	peak    int64
	killing bool
	exited  chan exit

	// Latest sample, for reporting. Distinct from peak, which exists for
	// enforcement: a dashboard showing the high-water mark of a job that has
	// since freed its memory tells you about the past, not the present.
	curMem    int64
	curProcs  int
	throttled bool
}

// Agent supervises this node's jobs.
// ownerState tracks how the owner policy is currently applied, so transitions
// are acted on once rather than re-applied every tick.
type ownerState struct {
	action owner.Action
	reason string
}

type Agent struct {
	// Owner enforces the machine owner's policy. Nil disables the feature.
	Owner *owner.Manager
	own   ownerState

	Backend platform.Backend
	Node    string
	Log     *slog.Logger

	// Root is this agent's state directory, and Files is the per-user
	// storage under it.
	Root  string
	Files *userfs.Store

	// cluster and label are what the controller says this cluster and this
	// machine are called, refreshed every heartbeat. Used for the prompt in
	// an interactive job: the agent knows its certificate identity but not
	// the name a person gave it.
	cluster, label string

	// Relay carries interactive sessions to the controller. Nil on a node
	// with no controller connection, in which case an interactive job runs
	// but nobody sees it -- which the scheduler prevents by not assigning
	// one.
	Relay StreamRelay

	// API forwards a job's cluster-API requests to the controller, so
	// `squeue` works from inside a job. Nil on a node with no controller
	// connection, where there is nothing to ask.
	API ControllerAPI

	mu   sync.Mutex
	run  map[int64]*running
	done []agentapi.JobStatus // terminal reports awaiting delivery
	// finished remembers what a completed job still needs: its captured
	// output and, for stage-out, its scratch dir and the paths to return.
	// Kept until the controller acks, so nothing is lost to a restart mid-upload.
	finished map[int64]*finishedJob

	// completed counts jobs finished since startup, so an idle machine can
	// still show its owner that it has been doing something.
	completed int

	// storageCache holds the last file-index measurement, which is the most
	// expensive thing the heartbeat does.
	storageCache []agentapi.UserStorage
	storageAt    time.Time
	// storageStart is when the walk behind storageCache began, which is what
	// the controller needs to know how far the report can speak for.
	storageStart time.Time
	// storageSent records that storageCache has been handed to the
	// controller, so an unchanged listing is not re-sent on every heartbeat.
	storageSent bool
	// storageFP fingerprints the last report actually sent, so a
	// re-measurement that found nothing different is not sent either.
	// storageFPSet distinguishes "the fingerprint is zero" from "nothing has
	// been sent yet".
	storageFP    uint64
	storageFPSet bool
	// storageOK records that storageCache is a real measurement rather than
	// the zero value, so an empty cluster is distinguishable from one that
	// has not been looked at yet.
	storageOK bool
	transfers []agentapi.TransferResult

	// tools caches where shome's own commands are for each kind of
	// environment this machine runs -- see hostTools.
	toolsMu    sync.Mutex
	toolsCache map[bool]toolsAnswer

	nowFn func() time.Time
}

// toolsAnswer is a resolved tool directory, or the reason there is none.
type toolsAnswer struct {
	dir string
	err error
}

// New creates an agent rooted at the directory it may use on this machine.
//
// The root is a parameter rather than something set afterwards because an
// agent holds cluster users' files: an unset root once meant a silent
// fallback to a *relative* "users" directory, which would have written other
// people's files into whatever directory the process happened to start in.
func New(be platform.Backend, node, root string, log *slog.Logger) *Agent {
	return &Agent{Backend: be, Node: node, Log: log, Root: root, Files: userfs.New(root),
		run: map[int64]*running{}, finished: map[int64]*finishedJob{}, nowFn: time.Now}
}

func (a *Agent) now() time.Time { return a.nowFn() }

// Supervise runs the local enforcement loop until ctx is cancelled. It is
// independent of controller connectivity by design.
func (a *Agent) Supervise(ctx context.Context) {
	t := time.NewTicker(Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// Also called by the daemon, synchronously, on the way down.
			// Here as well because a context can be cancelled without the
			// process exiting -- a test, or a re-launched agent -- and the
			// second call finds nothing to do.
			a.ReleaseIsolation()
			return
		case <-t.C:
			a.checkAll(ctx)
		}
	}
}

// ReleaseIsolation stops the isolation of everything still running here,
// when this agent is asked to stop.
//
// A container outlives the process that started it, so an agent that shut
// down cleanly while a job was running left a virtual machine holding a
// gigabyte of memory behind -- and `shome nuke`, which removes the state
// directory the sweep would have found it from, left it there for good.
//
// The jobs themselves are not reported as anything: they are neither
// finished nor failed, and the machine is going away. The controller works
// out what happened when this agent comes back and does not claim them, or
// when it never comes back at all. See Controller.reclaimUnclaimed.
//
// A fresh context, because the one that has just been cancelled cannot be
// used to stop anything.
func (a *Agent) ReleaseIsolation() {
	ids := a.RunningIDs()
	if len(ids) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), IsolationStopWait)
	defer cancel()
	if stopped := a.Backend.SweepOrphanIsolation(ctx, nil); len(stopped) > 0 {
		a.Log.Info("stopped isolation for jobs that were running at shutdown",
			"count", len(stopped), "names", stopped)
	}
}

// IsolationStopWait bounds the shutdown sweep. Long enough for a virtual
// machine to be torn down, short enough that a stop does not look like a
// hang: the daemon's own shutdown grace is twenty seconds.
const IsolationStopWait = 15 * time.Second

func (a *Agent) checkAll(ctx context.Context) {
	a.mu.Lock()
	ids := make([]int64, 0, len(a.run))
	for id := range a.run {
		ids = append(ids, id)
	}
	a.mu.Unlock()

	for _, id := range ids {
		a.mu.Lock()
		r := a.run[id]
		a.mu.Unlock()
		if r == nil {
			continue
		}

		if u, err := a.Backend.Sample(ctx, r.handle); err == nil {
			if u.MemBytes > r.peak {
				r.peak = u.MemBytes
			}
			a.mu.Lock()
			r.curMem, r.curProcs = u.MemBytes, u.NProcs
			a.mu.Unlock()
			// Process count, enforced the same way memory is: by polling and
			// killing. A fork bomb is the case this exists for, and it is the
			// one failure that takes a machine down hard enough that its owner
			// notices -- which on a cluster of other people's computers is the
			// thing that ends the arrangement.
			if lim := r.spec.Limits.MaxProcs; lim > 0 && u.NProcs > lim && !r.killing {
				r.killing = true
				a.Log.Warn("job exceeded its process limit", "job", id,
					"procs", u.NProcs, "limit", lim)
				a.Backend.Signal(ctx, r.handle, "KILL")
				a.report(r, job.Failed, -1, fmt.Sprintf(
					"exceeded the process limit (%d processes, limit %d)", u.NProcs, lim))
				continue
			}
			if lim := r.spec.Limits.MemBytes; lim > 0 && u.MemBytes > lim && !r.killing {
				r.killing = true
				a.Log.Warn("job exceeded memory limit", "job", id,
					"used_mib", u.MemBytes>>20, "limit_mib", lim>>20)
				a.Backend.Signal(ctx, r.handle, "KILL")
				a.report(r, job.OOM, -1, fmt.Sprintf(
					"exceeded memory limit (%d MiB used, %d MiB requested)", u.MemBytes>>20, lim>>20))
				continue
			}
		}

		if w := r.spec.Limits.Walltime; w > 0 && !r.killing && a.now().Sub(r.started) > w {
			r.killing = true
			a.Backend.Signal(ctx, r.handle, "TERM")
			go func(h *platform.Handle) {
				time.Sleep(TermGrace)
				a.Backend.Signal(context.Background(), h, "KILL")
			}(r.handle)
			a.report(r, job.Timeout, -1, "exceeded time limit of "+job.FormatDuration(w))
			continue
		}

		select {
		case e := <-r.exited:
			st, reason := job.Completed, ""
			code := e.code
			// What the kernel recorded, which outranks the exit status:
			// a job whose real work was killed for memory can still exit
			// zero, and does whenever the script backgrounded that work and
			// waited for it with a bare `wait`.
			after, haveAfter := a.Backend.Aftermath(ctx, r.sandbox)
			if haveAfter && after.PeakBytes > r.peak {
				// The kernel's high-water mark, not a poll that happened to
				// land near the top.
				r.peak = after.PeakBytes
			}
			switch {
			case e.err != nil:
				st, code, reason = job.Failed, -1, "wait failed: "+e.err.Error()
			case haveAfter && after.OOMKills > 0:
				st, reason = job.OOM, oomReason(after, r.spec.Limits.MemBytes)
			case code == exitKilled && r.spec.Limits.MemBytes > 0:
				// Killed by SIGKILL with a memory limit in force. Where the
				// limit is enforced by the kernel rather than by shome's own
				// polling -- a containerised job, or a cgroup -- that is what
				// hitting it looks like, and reporting "exit code 137" would
				// send somebody looking for a bug in their program.
				st, reason = job.OOM, fmt.Sprintf(
					"exceeded its memory limit (%d MiB requested)",
					r.spec.Limits.MemBytes>>20)
			case code != 0:
				st, reason = job.Failed, fmt.Sprintf("exit code %d", code)
			}
			a.report(r, st, code, reason)
		default:
		}
	}
}

// finishedJob is what remains to be shipped after a job exits.
type finishedJob struct {
	outPath  string
	workDir  string
	scratch  string
	stageOut []string
	// sandbox is what Prepare created, kept so cleanup can undo precisely
	// that -- including the generated sandbox profile, which lives outside
	// the scratch directory on some platforms.
	sandbox *platform.Sandbox
}

// Finished returns the post-run artefacts for a job, if this node ran it.
//
// workDir rather than the scratch directory, because stage-out collects what
// the job produced and a job produces it where it ran.
func (a *Agent) Finished(id int64) (outPath, workDir string, stageOut []string, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f, ok := a.finished[id]
	if !ok {
		return "", "", nil, false
	}
	return f.outPath, f.workDir, f.stageOut, true
}

// report queues a terminal status for the next heartbeat and stops tracking.
//
// Queued rather than sent directly: the controller may be unreachable, and a
// finished job's outcome must not be lost because of that.
func (a *Agent) report(r *running, st job.State, code int, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.completed++
	delete(a.run, r.id)
	a.done = append(a.done, agentapi.JobStatus{
		ID: r.id, State: st, ExitCode: code, PeakMem: r.peak, Reason: reason,
	})
	a.Log.Info("job finished", "job", r.id, "state", st, "exit", code,
		"peak_mib", r.peak>>20, "reason", reason)
}

// oomReason explains an OOM the kernel counted after the fact.
//
// It names the peak, because that is the number a person needs in order to
// pick a bigger figure, and says where it came from: the job may well have
// reported success, and being told it ran out of memory anyway is
// surprising enough to be worth explaining in the same breath.
func oomReason(a platform.Aftermath, limit int64) string {
	killed := plural(a.OOMKills, "process", "processes")
	switch {
	case a.PeakBytes > 0 && limit > 0:
		return fmt.Sprintf(
			"exceeded memory limit: the kernel killed %s (peak %d MiB, %d MiB requested)",
			killed, a.PeakBytes>>20, limit>>20)
	case limit > 0:
		return fmt.Sprintf("exceeded memory limit: the kernel killed %s (%d MiB requested)",
			killed, limit>>20)
	default:
		return fmt.Sprintf("out of memory: the kernel killed %s", killed)
	}
}

// plural renders a count with the right noun.
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// GangInfo is the rank wiring for one task of a multi-node job.
type GangInfo struct {
	Rank       int
	NNodes     int
	NodeList   []string
	MasterAddr string
	MasterPort int

	// Fabric plan for this rank.
	Fabric    string
	Role      string // "user" or "worker"
	FabricEnv map[string]string
	Command   []string   // replaces the user script for worker ranks
	Pre       [][]string // run before the main command
	Post      [][]string // run after, always
}

// shellQuote renders an argument safe for the generated wrapper.
//
// The wrapper is a shell script, and fabric commands include addresses and
// paths that must not be re-interpreted. Single quotes with an escape for
// embedded quotes is the only form that is safe for arbitrary content.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func quoteAll(args []string) string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		out = append(out, shellQuote(a))
	}
	return strings.Join(out, " ")
}

// writeFabricWrapper builds the script a rank actually executes.
//
// Generating a wrapper keeps the platform Backend interface unchanged: it
// still receives one script path to run inside the sandbox. Pre and post steps
// fall out naturally, and post runs via a trap so a failed or killed job still
// tears its scaffolding down -- otherwise a crashed run leaves a Ray cluster
// or an rpc-server alive on someone's machine.
func writeFabricWrapper(scratch, userScript string, g *GangInfo) (string, error) {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# generated by shome -- fabric: " + g.Fabric + "\n")
	if len(g.Post) > 0 {
		b.WriteString("shome_teardown() {\n")
		for _, cmd := range g.Post {
			// Teardown must not itself abort the trap.
			b.WriteString("  " + quoteAll(cmd) + " >/dev/null 2>&1 || true\n")
		}
		b.WriteString("}\n")
		b.WriteString("trap shome_teardown EXIT INT TERM\n")
	}
	for _, cmd := range g.Pre {
		b.WriteString(quoteAll(cmd) + " || exit $?\n")
	}
	if len(g.Command) > 0 {
		b.WriteString("exec " + quoteAll(g.Command) + "\n")
	} else {
		b.WriteString("exec " + shellQuote(userScript) + "\n")
	}
	path := filepath.Join(scratch, ".shome-fabric.sh")
	if err := os.WriteFile(path, []byte(b.String()), 0o700); err != nil {
		return "", err
	}
	return path, nil
}

// gangEnv renders the rank wiring as environment variables.
//
// Both native and SLURM_-aliased names are set, so scripts written for a real
// cluster run here unmodified -- which is the whole point of matching Slurm's
// surface rather than inventing a new one.
func gangEnv(g *GangInfo) map[string]string {
	if g == nil {
		return nil
	}
	list := strings.Join(g.NodeList, ",")
	env := map[string]string{
		"SHOME_NNODES":       strconv.Itoa(g.NNodes),
		"SHOME_NODEID":       strconv.Itoa(g.Rank),
		"SHOME_PROCID":       strconv.Itoa(g.Rank),
		"SHOME_NODELIST":     list,
		"SHOME_MASTER_ADDR":  g.MasterAddr,
		"SHOME_MASTER_PORT":  strconv.Itoa(g.MasterPort),
		"SLURM_NNODES":       strconv.Itoa(g.NNodes),
		"SLURM_NODEID":       strconv.Itoa(g.Rank),
		"SLURM_PROCID":       strconv.Itoa(g.Rank),
		"SLURM_JOB_NODELIST": list,
		"SLURM_NTASKS":       strconv.Itoa(g.NNodes),
		// torchrun and most PyTorch launchers read these two directly.
		"MASTER_ADDR": g.MasterAddr,
		"MASTER_PORT": strconv.Itoa(g.MasterPort),
	}
	return env
}

// Launch starts a job on this node. stageIn, if non-nil, is called with the
// job's working directory once it exists and before the job starts, to
// populate its inputs.
// gang is nil for ordinary single-node jobs.
//
// token is the job's credential for talking to the cluster, issued by the
// controller with the launch instruction. A parameter rather than a field
// of the spec because it is not part of what the job is: it is never
// stored, never shown, and outlives nothing.
func (a *Agent) Launch(ctx context.Context, id int64, spec job.Spec, token string, gang *GangInfo, stageIn func(workDir string) error) error {
	a.mu.Lock()
	if _, dup := a.run[id]; dup {
		a.mu.Unlock()
		return nil // already running; a duplicated action is not an error
	}
	a.mu.Unlock()

	sb, err := a.Backend.Prepare(ctx, spec, id)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	// The account's own storage, so work survives the job. The scratch
	// directory is removed when the job ends; without this a job could
	// write nothing that the next one, or `shome fs`, would ever see.
	// Whether this job's paths are remapped, which decides how it can be
	// given a route to the cluster: a socket in its scratch, or a listener
	// on the container network. Answered by the layout below.
	mapped := false
	if spec.User != "" {
		cluster, machine := a.Names()
		home := filepath.Join(a.Root, "users", spec.User)
		// Where things appear from inside, which the platform decides:
		// a container or a namespace can put them anywhere, and a policy
		// sandbox has to leave them where they are. The GPU count is
		// part of the question because it can change the answer.
		//
		// Asked before the directories are created, because which
		// environment this is decides which of the account's per-slot
		// directories the job will use.
		lay := a.Backend.Layout(platform.Placement{
			Home:  home,
			Tools: userenv.ToolDir(a.Root),
			User:  spec.User, Machine: machine,
			GPUs: spec.Limits.GPUs,
		})
		mapped = lay.Mapped
		if err := userenv.EnsureHomeSlot(home, lay.Slot); err != nil {
			a.Log.Warn("could not prepare the account's storage for a job",
				"job", id, "user", spec.User, "err", err)
		} else {
			sb.UserHome = home
			sb.UserHomeIn = lay.Home
			// Where the job actually runs. The account's storage, so a
			// script writing ./results.csv writes it somewhere that still
			// exists tomorrow; a subdirectory of it when the job asked for
			// one with --chdir.
			work, workIn, err := jobWorkDir(home, lay.Home, spec.Chdir)
			if err != nil {
				a.Backend.Cleanup(ctx, sb)
				return err
			}
			sb.WorkDir, sb.WorkDirIn = work, workIn
			// The same environment a login session gets: PATH reaching the
			// base software and the account's own installs, and every cache
			// redirected under $HOME so nothing lands outside the account's
			// storage where `shome fs` cannot see it and the disk limit
			// does not count it.
			//
			// A job used to inherit the agent's environment instead, which
			// is the machine owner's: their PATH, naming directories the
			// sandbox denies. Set here rather than in a backend so a job
			// gets the same variables on every platform.
			// shome's own commands, where the job's PATH already says
			// they are. Without this the entry names an empty path and
			// `squeue` inside a job reports "not found".
			if dir, err := a.hostTools(lay.Mapped); err != nil {
				a.Log.Warn("the cluster commands will be missing inside this job",
					"job", id, "err", err)
			} else {
				sb.ToolsHost, sb.ToolsIn = dir, lay.Tools
			}
			for k, v := range (userenv.Session{
				Home: lay.Home, Tools: lay.Tools, Software: lay.Software,
				Slot: lay.Slot,
				User: spec.User, Cluster: cluster, Host: machine,
			}).Env() {
				// Never over what the sandbox already decided: the job's
				// scratch and temp directory are its own.
				if _, ok := sb.Env[k]; !ok {
					sb.Env[k] = v
				}
			}
		}
	}

	// Inputs land where the job runs, which is also where stage-out
	// collects from -- so `--stage-in data.csv` then `open("data.csv")`
	// works, and the two directions are symmetric.
	if stageIn != nil {
		dir := sb.WorkDir
		if dir == "" {
			dir = sb.ScratchDir
		}
		if err := stageIn(dir); err != nil {
			a.Backend.Cleanup(ctx, sb)
			return fmt.Errorf("stage-in: %w", err)
		}
	}
	for k, v := range gangEnv(gang) {
		sb.Env[k] = v
	}
	if gang != nil {
		for k, v := range gang.FabricEnv {
			sb.Env[k] = v
		}
		// A worker rank, a pre/post step or an MLX hostfile all mean this rank
		// runs something other than the bare user script.
		if len(gang.Command) > 0 || len(gang.Pre) > 0 || len(gang.Post) > 0 {
			wrapper, werr := writeFabricWrapper(sb.ScratchDir, spec.Script, gang)
			if werr != nil {
				a.Backend.Cleanup(ctx, sb)
				return fmt.Errorf("write fabric wrapper: %w", werr)
			}
			spec.Script = wrapper
		}
		if hf := sb.Env["SHOME_MLX_HOSTFILE"]; hf != "" {
			// Materialise the hostfile mlx.launch expects, inside the job's
			// scratch so it is removed with the job.
			real := filepath.Join(sb.ScratchDir, "hostfile.json")
			hosts := strings.Split(sb.Env["SHOME_MLX_HOSTS"], ",")
			if err := os.WriteFile(real, []byte(mlxHostfileJSON(hosts)), 0o600); err != nil {
				a.Log.Warn("write mlx hostfile", "err", err)
			}
			sb.Env["SHOME_MLX_HOSTFILE"] = real
		}
	}
	// Where the submitter was standing, for scripts that use it. Not where
	// the job runs: a cluster has no shared filesystem, so that directory
	// usually does not exist on this machine. Both spellings, so an
	// unmodified Slurm script finds the one it expects.
	if spec.Workdir != "" {
		sb.Env["SHOME_SUBMIT_DIR"] = spec.Workdir
		sb.Env["SLURM_SUBMIT_DIR"] = spec.Workdir
	}

	// The job's own route to the cluster, and the credential to use over
	// it. Both are dropped when the job ends. A job without them still
	// runs; it just cannot ask the cluster anything.
	var closeLink func()
	if token != "" {
		link, err := a.openJobLink(ctx, sb, mapped, spec.Limits.Network)
		switch {
		case errors.Is(err, ErrJobNeedsNetwork):
			// Told to the job rather than only logged: without this the
			// cluster commands are present, look installed, and fail with
			// "cannot reach shomectld (is it running?)" -- which points at
			// the controller when the answer is one flag on the submission.
			sb.Env["SHOME_API_UNAVAILABLE"] = err.Error() +
				".\n  Resubmit with --network, or use 'srun --pty', which has it." +
				"\n  A GPU job needs neither: it runs natively and reaches the " +
				"cluster over a socket."
		case err != nil:
			a.Log.Warn("no cluster access inside this job", "job", id, "err", err)
		case link != nil:
			sb.Env["SHOME_TOKEN"] = token
			for k, v := range link.env {
				sb.Env[k] = v
			}
			closeLink = link.Close
		}
	}

	// A bare interactive shell gets the cluster's startup file, so a shell
	// on a compute node is the same shell as one on the login node. See
	// userenv.InteractiveShellArgv for what is and is not rewritten.
	if spec.PTY {
		spec.Args = userenv.InteractiveShellArgv(spec.Args, sb.ToolsIn)
	}

	// An interactive job gets a terminal before it starts, if it asked for
	// one, and its output is relayed to whoever is waiting. The launch
	// itself is the same call either way -- the sandbox simply carries a
	// terminal -- so isolation and enforcement cannot diverge.
	if spec.Interactive {
		for k, v := range a.interactiveEnv(spec) {
			sb.Env[k] = v
		}
	}
	var tty *ptyx.PTY
	if spec.Interactive && spec.PTY {
		tty, err = ptyx.Open()
		if err != nil {
			a.Backend.Cleanup(ctx, sb)
			return fmt.Errorf("allocate a terminal: %w", err)
		}
		// The submitter's own window, so the first prompt is already the
		// right width; a default only when they had no terminal to measure.
		// Later changes arrive over the session's size channel, see
		// followSize.
		ws := spec.TTYSize
		if !ws.Known() {
			ws = job.DefaultWinsize
		}
		tty.Resize(ws.Rows, ws.Cols, ws.XPixel, ws.YPixel)
		sb.TTY = tty.Slave
	}
	h, err := a.Backend.Launch(ctx, sb, spec)
	if err != nil {
		if tty != nil {
			tty.Close()
		}
		if closeLink != nil {
			closeLink()
		}
		a.Backend.Cleanup(ctx, sb)
		return fmt.Errorf("launch: %w", err)
	}
	r := &running{spec: spec, id: id, sandbox: sb, handle: h,
		started: a.now(), exited: make(chan exit, 1)}
	// finished is published to whoever is watching; exitCode is written
	// before the close, so a reader that has seen the close sees the value.
	// Reading the code off r.exited instead would race the heartbeat path,
	// which drains that channel -- whichever got there first would win and
	// the user's shell could report the wrong status.
	finished := make(chan struct{})
	var exitCode int
	go func() {
		code, err := a.Backend.Wait(context.Background(), h)
		exitCode = code
		close(finished)
		r.exited <- exit{code: code, err: err}
	}()
	// The job's route to the cluster lasts exactly as long as the job. The
	// credential it carried is revoked by the controller on the same
	// grounds; see ctl.reapJobTokens.
	if closeLink != nil {
		go func() { <-finished; closeLink() }()
	}
	if spec.Interactive && a.Relay != nil {
		if tty != nil {
			// Ours to release now the job holds its own copy, or reads on
			// the master would never end.
			tty.CloseSlave()
			a.relayTTY(ctx, a.Relay, spec.Stream, id, tty, finished)
			go func() { <-finished; tty.Close() }()
		} else {
			go a.relayOutput(ctx, a.Relay, spec.Stream, id,
				JobOutputPath(sb), finished)
		}
		// The status goes up separately: it is not known until wait returns,
		// and the user's shell needs it as its own exit code.
		go func() {
			<-finished
			c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			a.Relay.StreamExit(c, spec.Stream, exitCode)
		}()
	}
	a.mu.Lock()
	a.run[id] = r
	a.finished[id] = &finishedJob{
		outPath:  JobOutputPath(sb),
		workDir:  sb.WorkDir,
		scratch:  sb.ScratchDir,
		stageOut: spec.StageOut,
		sandbox:  sb,
	}
	a.mu.Unlock()
	a.Log.Info("job started", "job", id, "pid", h.PID, "scratch", sb.ScratchDir)
	return nil
}

// hostTools is where shome's commands are for one kind of environment,
// resolved once.
//
// The answer is a property of this machine and this build, not of the job:
// it changes when shome is upgraded, which means the agent has restarted.
// Resolving it per launch meant copying or hash-comparing a twenty-megabyte
// binary every time a job started, on the path between a job being assigned
// and it running.
//
// Keyed by whether the environment remaps paths, because that is what
// decides the answer: a container on a Mac needs the cross-built Linux set,
// everything else the managed one.
func (a *Agent) hostTools(mapped bool) (string, error) {
	a.toolsMu.Lock()
	defer a.toolsMu.Unlock()
	if got, ok := a.toolsCache[mapped]; ok {
		return got.dir, got.err
	}
	dir, warn, err := userenv.HostTools(a.Root, mapped)
	if warn != "" {
		a.Log.Warn("stale job tools", "detail", warn)
	}
	if a.toolsCache == nil {
		a.toolsCache = map[bool]toolsAnswer{}
	}
	a.toolsCache[mapped] = toolsAnswer{dir: dir, err: err}
	return dir, err
}

// Kill terminates a job.
func (a *Agent) Kill(ctx context.Context, id int64) error {
	a.mu.Lock()
	r := a.run[id]
	a.mu.Unlock()
	if r == nil {
		return nil
	}
	r.killing = true
	if err := a.Backend.Signal(ctx, r.handle, "TERM"); err != nil {
		return err
	}
	go func(h *platform.Handle) {
		time.Sleep(2 * time.Second)
		a.Backend.Signal(context.Background(), h, "KILL")
	}(r.handle)
	a.report(r, job.Cancelled, -1, "cancelled")
	return nil
}

// Statuses returns running-job reports plus any queued terminal reports,
// clearing the latter only once they have been handed over.
func (a *Agent) Statuses() []agentapi.JobStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]agentapi.JobStatus, 0, len(a.run)+len(a.done))
	for id, r := range a.run {
		out = append(out, agentapi.JobStatus{ID: id, State: job.Running, PeakMem: r.peak})
	}
	out = append(out, a.done...)
	return out
}

// ClaimedJobs is every job this agent is answerable for: running now, or
// finished with a result the controller has not acknowledged.
//
// The complete set, deliberately. It is what lets the controller notice a
// job that has ceased to exist -- an agent that restarted has an empty one,
// and the jobs its records still show as running there are gone.
func (a *Agent) ClaimedJobs() []int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]int64, 0, len(a.run)+len(a.done))
	for id := range a.run {
		out = append(out, id)
	}
	for _, d := range a.done {
		out = append(out, d.ID)
	}
	return out
}

// LiveJobs reports current resource use for everything running here.
//
// Reported separately from Statuses because the two answer different
// questions: Statuses is the authoritative record of what happened to a job,
// which the controller must not lose, while this is a snapshot that is
// worthless a few seconds later and can be dropped freely.
func (a *Agent) LiveJobs() []agentapi.JobLive {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	out := make([]agentapi.JobLive, 0, len(a.run))
	for id, r := range a.run {
		out = append(out, agentapi.JobLive{
			ID:         id,
			MemBytes:   r.curMem,
			PeakBytes:  r.peak,
			NProcs:     r.curProcs,
			CPUPercent: platform.Unknown,
			Throttled:  r.throttled,
			ElapsedSec: now.Sub(r.started).Seconds(),
		})
	}
	return out
}

// ForgetFinished removes a job's working files once its results have been
// shipped, and drops the record of it.
//
// This is where a job's disk is actually reclaimed. Cleanup used to be called
// only when launching failed, so every job that ran left its scratch behind
// for good -- a slow leak that nothing reported and that grew with use, on
// machines lent out by people who would eventually notice it as "shome filled
// my disk".
//
// It happens here rather than when the job exits because the scratch is what
// stage-out reads from: removing it earlier would delete the results on the
// way to fetching them.
func (a *Agent) ForgetFinished(id int64) {
	a.mu.Lock()
	f := a.finished[id]
	delete(a.finished, id)
	a.mu.Unlock()

	if f == nil || f.sandbox == nil {
		return
	}
	if err := a.Backend.Cleanup(context.Background(), f.sandbox); err != nil {
		// Worth saying out loud: the alternative is a disk that fills for a
		// reason nobody can see.
		a.Log.Warn("could not remove job working files", "job", id,
			"scratch", f.scratch, "err", err)
		return
	}
	a.Log.Info("removed job working files", "job", id, "scratch", f.scratch)
}

// SweepOrphanScratch removes working directories left by jobs this agent no
// longer knows about.
//
// Needed because the tidy path depends on the agent being alive to run it: a
// crash, a kill, or a controller that never acks leaves a directory nobody
// will ever come back for. Called at startup, when the set of live jobs is
// known to be empty.
func (a *Agent) SweepOrphanScratch(ctx context.Context) int {
	live := a.RunningIDs()
	// Isolation that outlives its launcher, which on a Mac is a container
	// still holding a virtual machine's memory. Swept first and
	// unconditionally: it is left behind by the same events as an orphaned
	// directory but not always alongside one, and putting it after the
	// early return below meant it never ran on a machine whose directories
	// happened to be clean.
	if stopped := a.Backend.SweepOrphanIsolation(ctx, live); len(stopped) > 0 {
		a.Log.Info("stopped containers left by a previous run",
			"count", len(stopped), "names", stopped)
	}
	dirs, err := a.Backend.OrphanScratch(ctx, live)
	if err != nil || len(dirs) == 0 {
		return 0
	}
	n := 0
	for _, d := range dirs {
		if err := os.RemoveAll(d); err != nil {
			a.Log.Warn("could not remove orphaned scratch", "dir", d, "err", err)
			continue
		}
		n++
	}
	if n > 0 {
		a.Log.Info("removed orphaned job directories from a previous run", "count", n)
	}
	return n
}

// AckDone drops terminal reports the controller has confirmed. Until then they
// are resent on every heartbeat, so a dropped connection cannot lose a result.
func (a *Agent) AckDone(ids []int64) {
	if len(ids) == 0 {
		return
	}
	acked := make(map[int64]bool, len(ids))
	for _, id := range ids {
		acked[id] = true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	kept := a.done[:0]
	for _, d := range a.done {
		if !acked[d.ID] {
			kept = append(kept, d)
		}
	}
	a.done = kept
}

// RunningIDs lists jobs this node currently has.
func (a *Agent) RunningIDs() []int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	ids := make([]int64, 0, len(a.run))
	for id := range a.run {
		ids = append(ids, id)
	}
	return ids
}

// mlxHostfileJSON renders the host list mlx.launch reads.
func mlxHostfileJSON(hosts []string) string {
	var b strings.Builder
	b.WriteString("[\n")
	for i, h := range hosts {
		fmt.Fprintf(&b, "  {\"ssh\": %q}", h)
		if i < len(hosts)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("]\n")
	return b.String()
}

// ApplyOwnerPolicy evaluates the owner's policy and applies it to running work.
//
// Called every tick. Only transitions do anything: re-issuing SIGSTOP to an
// already-suspended tree each second would be wasteful and would spam the log
// the owner reads to understand what their machine is doing.
func (a *Agent) ApplyOwnerPolicy(ctx context.Context) owner.Decision {
	if a.Owner == nil {
		return owner.Decision{Action: owner.Run, Reason: "owner policy disabled"}
	}
	d := a.Owner.Evaluate()
	if d.Action == a.own.action {
		return d
	}
	prev := a.own.action
	a.own = ownerState{d.Action, d.Reason}
	a.Log.Info("owner policy changed", "from", prev, "to", d.Action, "reason", d.Reason)

	a.mu.Lock()
	handles := make([]*platform.Handle, 0, len(a.run))
	for _, r := range a.run {
		handles = append(handles, r.handle)
	}
	a.mu.Unlock()

	for _, h := range handles {
		switch d.Action {
		case owner.Suspend:
			if err := a.Backend.Signal(ctx, h, "STOP"); err != nil {
				a.Log.Warn("suspend for owner policy", "err", err)
			}
		case owner.Throttle:
			// Continue first: the previous state may have been suspended, and
			// a throttled job is still supposed to make progress.
			a.Backend.Signal(ctx, h, "CONT")
			if err := a.Backend.Throttle(ctx, h, true); err != nil && err != platform.ErrAdvisory {
				a.Log.Warn("throttle for owner policy", "err", err)
			}
		case owner.Run, owner.Drain:
			a.Backend.Signal(ctx, h, "CONT")
			if err := a.Backend.Throttle(ctx, h, false); err != nil && err != platform.ErrAdvisory {
				a.Log.Warn("restore priority", "err", err)
			}
		}
	}
	// Record it so the dashboard can show why a job is running slowly, which
	// is otherwise invisible and reads as the job being stuck.
	a.mu.Lock()
	for _, r := range a.run {
		r.throttled = d.Action == owner.Throttle
	}
	a.mu.Unlock()
	return d
}

// CappedCaps reduces advertised capacity to what the owner permits.
//
// Applied to the advertisement rather than enforced per job: if the cluster
// never sees capacity the owner did not offer, the scheduler will not place
// work that would overrun it, and no enforcement is needed after the fact.
func (a *Agent) CappedCaps(c platform.Capabilities) platform.Capabilities {
	if a.Owner == nil {
		return c
	}
	c.CPUs, c.MemBytes, c.GPUs = a.Owner.CapCapacity(c.CPUs, c.MemBytes, c.GPUs)
	if c.GPUs == 0 {
		c.GPUMemBytes = 0
	}
	return c
}

// ReportRefused tells the controller this node will not run a job after all.
//
// Reported as a terminal state rather than silently dropped: the controller
// needs to requeue it, and the user deserves to see why their job moved.
func (a *Agent) ReportRefused(id int64, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.done = append(a.done, agentapi.JobStatus{
		ID: id, State: job.Failed, ExitCode: -1,
		Reason: reason, Refused: true,
	})
}

// diskState converts the latest reading into what the owner's rules need.
func diskState(t platform.Telemetry) owner.DiskState {
	return owner.DiskState{
		ShomeBytes:  t.ShomeDiskBytes,
		ShomeInodes: t.ShomeInodes,
		FreeBytes:   t.DiskFreeBytes,
		FreeInodes:  t.InodesFree,
		Known:       t.KnownDisk(),
	}
}

// DiskRefusal reports why this node should not take new work on disk grounds.
//
// Separate from the owner's yield rules because it is not about the owner
// being present: a full disk refuses work whether they are at the keyboard or
// asleep, and it clears on its own when space is freed rather than after an
// idle timer.
func (a *Agent) DiskRefusal(ctx context.Context) string {
	if a.Owner == nil {
		return ""
	}
	return a.Owner.CheckDisk(diskState(a.Telemetry(ctx)))
}

// OwnerAllowsNewWork reports whether the node should accept new jobs.
func (a *Agent) OwnerAllowsNewWork() bool {
	if a.Owner == nil {
		return true
	}
	switch a.Owner.Current().Action {
	case owner.Run, owner.Throttle:
		return true
	}
	return false
}

// Telemetry samples what this machine is doing, overlaying the owner-facing
// signals the yield policy already reads.
//
// The overlay is why this lives on the agent rather than in the backend: the
// sensor is what decides whether jobs get throttled, so taking the dashboard's
// numbers from the same place removes any chance of the display saying the
// owner is away while the policy engine has just suspended everything.
func (a *Agent) Telemetry(ctx context.Context) platform.Telemetry {
	t := platform.Sample(ctx, a.Backend, a.now())
	if a.Owner == nil || !a.Owner.SensorLive() {
		return t
	}
	s := a.Owner.Signals()
	t.Thermal = s.Thermal
	t.MemoryPressure = s.MemoryPressure
	t.OnBattery = s.OnBattery
	t.UserIdleSec = s.UserIdle.Seconds()
	return t
}

// JobOutputPath is where a job's captured output is written.
//
// Beside the job's other files, in the account's storage, and named the way
// Slurm names it -- so `ls` after a job finishes shows slurm-84.out where a
// person would look for it. It used to land in the scratch directory, which
// is deleted when the job ends, and was only ever readable through shome's
// own copy on the controller.
func JobOutputPath(sb *platform.Sandbox) string {
	dir := sb.WorkDir
	if dir == "" {
		dir = sb.ScratchDir
	}
	return filepath.Join(dir, fmt.Sprintf("slurm-%d.out", sb.JobID))
}

// jobWorkDir resolves --chdir against the account's storage.
func jobWorkDir(home, homeIn, chdir string) (dir, in string, err error) {
	clean, err := job.CleanChdir(chdir)
	if err != nil {
		return "", "", err
	}
	if clean == "" {
		return home, homeIn, nil
	}
	dir = filepath.Join(home, clean)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("create the job's working directory: %w", err)
	}
	return dir, path.Join(homeIn, clean), nil
}
