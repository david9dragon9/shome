// Package job defines shome's core domain types.
//
// These are deliberately free of platform and storage concerns: the scheduler
// is a pure function over these values, which is what makes it testable without
// daemons or a clock.
package job

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// State is a job's lifecycle position. Names mirror Slurm's so that squeue
// output is familiar.
type State string

const (
	Pending   State = "PENDING"
	Running   State = "RUNNING"
	Completed State = "COMPLETED"
	Failed    State = "FAILED"
	Cancelled State = "CANCELLED"
	Timeout   State = "TIMEOUT"
	OOM       State = "OUT_OF_MEMORY"
)

// Terminal reports whether no further transitions are possible.
func (s State) Terminal() bool {
	switch s {
	case Completed, Failed, Cancelled, Timeout, OOM:
		return true
	}
	return false
}

// Limits are the resources a job requested. Enforcement fidelity varies by
// platform; see platform.Backend.Enforce and the ErrAdvisory contract.
type Limits struct {
	CPUs     int           // cores requested (admission control; advisory on macOS)
	MemBytes int64         // hard-ish: kernel cap on leaf exec + polled tree kill
	Walltime time.Duration // 0 means unlimited
	GPUs     int           // exclusive whole-GPU allocation on Apple Silicon
	Network  bool          // deny-by-default; opt-in

	// MaxProcs caps the processes and threads the job's tree may have alive.
	// Set from the submitter's QoS limits rather than requested, and enforced
	// by the same polling that enforces memory. 0 means unlimited.
	MaxProcs int
}

// Spec is everything needed to run a job, as submitted.
type Spec struct {
	Name   string
	User   string // shome-internal identity, not an OS uid (see docs/design-notes.md)
	Script string // original path, kept for display and error messages

	// ScriptBody is the script's contents, captured at submit time.
	//
	// The job runs a copy written into its own scratch, never the original
	// path. Two reasons, both of which bite immediately otherwise: the
	// sandbox denies the submitter's home directory, so a script living there
	// cannot even be read by the job that is supposed to run it; and on a
	// remote node the path may not exist at all, since a home cluster has no
	// shared filesystem. Slurm does the same thing for the same reasons.
	ScriptBody []byte
	Args       []string
	Env        map[string]string
	// Workdir is where the submitter ran the command, recorded for
	// SHOME_SUBMIT_DIR. It is not where the job runs: a cluster has no
	// shared filesystem, so that path usually does not exist on the node.
	Workdir string

	// Chdir is where in the account's storage the job should run, relative
	// to the top of it. Empty means the top, which is where a login session
	// starts. Slurm's --chdir, with the one difference that it has to be
	// relative: an absolute path on the submitter's machine means nothing
	// on the node that ends up running the job.
	Chdir  string
	Limits Limits

	// ArrayTaskID is this task's index within a job array, or -1 for an
	// ordinary job. Exposed to the job as SHOME_ARRAY_TASK_ID.
	ArrayTaskID int

	// Dependency is a Slurm-style expression, e.g. "afterok:12:13".
	Dependency string

	// Aggregate request: totals across however many nodes it takes. Set by
	// --total-* and --nodes=auto. When any of these is non-zero the scheduler
	// solves placement instead of using the per-node Limits.
	TotalCPUs     int
	TotalMemBytes int64
	TotalGPUMem   int64
	TotalGPUs     int
	MaxNodes      int

	// Requeue allows shome to re-run this job from the start if the node it
	// was on disappears.
	//
	// Defaults to FALSE, unlike Slurm. A job that has already done part of its
	// work and then loses its node cannot be safely restarted unless it is
	// idempotent: re-running "process these 1000 records" after 300 were
	// written produces 1300. Silently doing that is worse than failing, so the
	// user has to say the job is safe to repeat.
	Requeue bool

	// NodeList pins the job to specific machines by name. Blunter than
	// --constraint, but it is what you want when testing one particular
	// machine rather than a class of them.
	NodeList []string

	// Constraint restricts which nodes may run this job, by capability rather
	// than by name -- see sched.ParseConstraint.
	Constraint string

	// Fabric pins the coordination backend; empty means shome chooses.
	Fabric string
	// Model marks the job as model-parallel inference over these weights,
	// which is what makes a sharding fabric applicable.
	Model string

	// StageIn is set when the submitter uploaded an input archive; the agent
	// unpacks it into the job's scratch before launching.
	StageIn bool
	// StageOut lists paths, relative to scratch, to return to the controller
	// when the job finishes.
	StageOut []string

	// Interactive means the submitter is waiting at a terminal: the job's
	// output goes to them live over a relayed stream rather than being
	// archived when it finishes, and its input comes from them.
	//
	// The relay exists because compute nodes dial the controller and cannot
	// be dialled -- see the stream relay in internal/ctl.
	Interactive bool

	// PTY asks for a pseudo-terminal on the node, for `srun --pty bash`.
	// Without one a shell prints no prompt and full-screen programs do not
	// work; with one, output is not separable into stdout and stderr, which
	// is why it is a choice rather than the default.
	PTY bool

	// TTYSize is how big the submitter's terminal was when they typed the
	// command, so the job's terminal starts the right size rather than at
	// a default nobody's window matches. Changes after that arrive over the
	// session's own channel; see the stream relay in internal/ctl.
	TTYSize Winsize

	// Stream is the relay this job's terminal is attached to. Set by the
	// controller at submission, not by the submitter.
	Stream int64
}

// Winsize is a terminal's dimensions.
//
// Rows and columns are what line editing and full-screen programs need. The
// pixel dimensions are carried too because terminals that draw images ask
// for them, and a zero there is a terminal saying it does not know -- which
// is different from shome never having asked.
type Winsize struct {
	Rows   uint16 `json:"rows"`
	Cols   uint16 `json:"cols"`
	XPixel uint16 `json:"xpixel,omitempty"`
	YPixel uint16 `json:"ypixel,omitempty"`
}

// DefaultWinsize is what a terminal gets when nobody could say how big the
// submitter's window is -- a pipe on the other end, or an old client that
// does not send one. A sane default rather than 0x0, which makes
// full-screen programs draw into a zero-sized window and appear to hang.
var DefaultWinsize = Winsize{Rows: 24, Cols: 80}

// Known reports whether a size was actually measured.
//
// A zero size is not a small window, it is no answer: applying it makes
// full-screen programs draw into nothing and appear to hang, so every
// consumer of this treats it as "use the default instead".
func (w Winsize) Known() bool { return w.Rows > 0 && w.Cols > 0 }

// Job is a Spec plus its scheduling and execution state.
type Job struct {
	ID        int64
	Spec      Spec
	State     State
	Reason    string // why PENDING, or why it ended; surfaced by squeue
	Node      string
	ExitCode  int
	PeakMem   int64
	SubmitAt  time.Time
	StartAt   time.Time
	EndAt     time.Time
	ScratchOf string // absolute path to this job's scratch dir

	// Held is set by `scontrol hold`; a held job stays PENDING but is not a
	// scheduling candidate.
	Held bool

	// ArrayJobID is the id of the array's first task; 0 for ordinary jobs.
	// Slurm displays array tasks as "<arrayjobid>_<taskid>".
	ArrayJobID int64
}

// Label renders the job id the way Slurm does, so array tasks are
// distinguishable at a glance.
func (j *Job) Label() string {
	if j.ArrayJobID != 0 {
		return fmt.Sprintf("%d_%d", j.ArrayJobID, j.Spec.ArrayTaskID)
	}
	return fmt.Sprintf("%d", j.ID)
}

// Elapsed is the job's runtime so far, or its total if finished.
func (j *Job) Elapsed(now time.Time) time.Duration {
	if j.StartAt.IsZero() {
		return 0
	}
	if j.EndAt.IsZero() {
		return now.Sub(j.StartAt)
	}
	return j.EndAt.Sub(j.StartAt)
}

// FormatDuration renders a duration Slurm-style (D-HH:MM:SS / HH:MM:SS).
func FormatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	days := int(d.Hours()) / 24
	h := int(d.Hours()) % 24
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if days > 0 {
		return fmt.Sprintf("%d-%02d:%02d:%02d", days, h, m, s)
	}
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

// ParseWalltime accepts the Slurm --time forms: MM, MM:SS, HH:MM:SS,
// D-HH, D-HH:MM and D-HH:MM:SS.
func ParseWalltime(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	days := 0
	if i := strings.Index(s, "-"); i >= 0 {
		if _, err := fmt.Sscanf(s[:i], "%d", &days); err != nil {
			return 0, fmt.Errorf("bad days in %q", s)
		}
		s = s[i+1:]
	}
	parts := strings.Split(s, ":")
	var h, m, sec int
	var err error
	switch len(parts) {
	case 1:
		if days > 0 {
			_, err = fmt.Sscanf(parts[0], "%d", &h) // D-HH
		} else {
			_, err = fmt.Sscanf(parts[0], "%d", &m) // MM
		}
	case 2:
		if days > 0 {
			_, err = fmt.Sscanf(s, "%d:%d", &h, &m) // D-HH:MM
		} else {
			_, err = fmt.Sscanf(s, "%d:%d", &m, &sec) // MM:SS
		}
	case 3:
		_, err = fmt.Sscanf(s, "%d:%d:%d", &h, &m, &sec)
	default:
		return 0, fmt.Errorf("bad time format %q", s)
	}
	if err != nil {
		return 0, fmt.Errorf("bad time format %q: %w", s, err)
	}
	return time.Duration(days)*24*time.Hour +
		time.Duration(h)*time.Hour +
		time.Duration(m)*time.Minute +
		time.Duration(sec)*time.Second, nil
}

// CleanChdir validates --chdir and returns it as a clean relative path.
//
// One rule, checked twice on purpose. At submit, so a mistake is a message
// rather than a job that queues, gets scheduled, and only then fails. At the
// agent, because the agent must not trust a spec it was handed: the working
// directory is writable, so a path that climbs out of the account's storage
// would be a way to write outside the one tree the account may write to.
func CleanChdir(chdir string) (string, error) {
	if strings.TrimSpace(chdir) == "" {
		return "", nil
	}
	// Cleaned before the check, not after joining to the account's storage:
	// rooting it first would collapse "../elsewhere" into "elsewhere" and
	// run the job somewhere the submitter did not name, which is a worse
	// answer than refusing.
	clean := path.Clean(filepath.ToSlash(chdir))
	if clean == "." {
		return "", nil
	}
	if path.IsAbs(clean) {
		return "", fmt.Errorf("--chdir %q is absolute. A cluster has no shared "+
			"filesystem, so a path on your machine means nothing on the node "+
			"that runs the job; give one relative to your storage, which is "+
			"where the job starts", chdir)
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("--chdir %q does not stay inside your storage, "+
			"which is where a job runs; give a path relative to it", chdir)
	}
	return clean, nil
}
