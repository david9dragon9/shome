package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/davidwu/shome/internal/ctl"
	"github.com/davidwu/shome/internal/job"
)

// `srun` runs something on the cluster now and watches it.
//
// The difference from sbatch is who waits. sbatch hands the job to the
// scheduler and returns; srun stays attached, shows the output as it appears,
// and exits with the job's status -- so it composes with a shell the way any
// other command does.
//
// `--pty` goes further and gives the job a terminal, which is what makes
// `srun --pty bash` a shell on a compute node rather than a shell whose
// output happens to be piped.
//
// Both work by relaying bytes through the controller, because compute nodes
// dial out and cannot be dialled. See internal/ctl/stream.go.

func srunUsage() {
	fmt.Print(`shome srun - run something on the cluster now, and watch it

  srun COMMAND [ARGS...]        run it, streaming the output
  srun --pty bash               an interactive shell on a compute node
  srun -N2 COMMAND              across two machines

Takes the same resource options as sbatch:

  srun --cpus 4 --mem 8G python train.py
  srun --gres gpu:1 --pty bash
  srun --nodelist mini hostname

The difference from sbatch: srun waits, shows output as it happens, and exits
with the command's own status. sbatch returns a job id immediately.

With --pty the job gets a terminal, so a shell prints a prompt and full-screen
programs work. Without it, stdout and stderr stay separable and the output is
archived as usual, so 'shome cat JOBID' works afterwards.

Ctrl-C cancels the job.
`)
}

func srun(args []string) error {
	if len(args) == 0 {
		srunUsage()
		return nil
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		srunUsage()
		return nil
	}

	spec := job.Spec{User: currentUser(), Limits: job.Limits{CPUs: 1}}
	spec.Interactive = true

	// --pty is srun's own flag; everything else is shared with sbatch, so it
	// goes through the same parser and the same directives precedence.
	var pty bool
	var passthrough []string
	var command []string
	seenSep := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case seenSep:
			command = append(command, a)
		case a == "--pty":
			pty = true
		case a == "--":
			seenSep = true
		case strings.HasPrefix(a, "-"):
			passthrough = append(passthrough, a)
			// An option that takes a value takes the next argument with it;
			// otherwise the value would be mistaken for the command.
			if needsValue(a) && i+1 < len(args) {
				i++
				passthrough = append(passthrough, args[i])
			}
		default:
			// The first bare word starts the command; everything after it
			// belongs to the command, not to srun.
			command = append(command, args[i:]...)
			i = len(args)
		}
	}
	var array string
	var stageIn []string
	if _, err := parseOpts(passthrough, &spec, &array, &stageIn); err != nil {
		return err
	}
	if array != "" {
		return fmt.Errorf("--array is for sbatch: srun waits for one command, " +
			"and an array has many")
	}
	if len(command) == 0 {
		if pty {
			// The obvious intent, and what Slurm users type.
			command = []string{"bash"}
		} else {
			srunUsage()
			return fmt.Errorf("srun needs a command")
		}
	}

	spec.PTY = pty
	if pty {
		// How big the terminal is right now, so the job's own starts that
		// size instead of a default the window does not match -- which is
		// what makes a typed line wrap back over itself at column 80.
		// Changes after this are sent as they happen; see srunResize.
		spec.TTYSize = localWinsize()
	}
	spec.Args = command
	// Args are executed directly rather than through a script, so there is
	// nothing to materialise. The name is for listings.
	spec.Name = command[0]
	spec.Script = strings.Join(command, " ")
	if spec.Workdir == "" {
		spec.Workdir, _ = os.Getwd()
	}

	var v ctl.JobView
	if err := call("POST", "/submit", ctl.SubmitRequest{Spec: spec}, &v); err != nil {
		return err
	}
	if v.Stream == 0 {
		return fmt.Errorf("the controller did not open a session for job %d", v.ID)
	}

	// Cancel the job if the user gives up, rather than leaving it running
	// with nobody watching.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	go func() {
		<-sig
		fmt.Fprintf(os.Stderr, "\nsrun: cancelling job %d...\n", v.ID)
		call("POST", fmt.Sprintf("/job/%d/cancel", v.ID), nil, nil)
	}()

	if !srunQuiet() {
		fmt.Fprintf(os.Stderr, "srun: job %d queued\n", v.ID)
	}
	if v.Note != "" {
		fmt.Fprintf(os.Stderr, "srun: note: %s\n", wrapText(v.Note, 74))
	}
	// Attach to the session straight away rather than waiting for the job to
	// start. A short command can finish before a poll would notice it began,
	// and the relay holds its output until somebody reads -- so attaching
	// first is both simpler and race-free. Waiting first meant `srun
	// hostname` printed nothing at all: by the time it looked, the job was
	// already done.
	//
	// Progress is reported concurrently, so a job that waits for resources
	// still says why.
	progressDone := make(chan struct{})
	go srunProgress(v.ID, progressDone)
	defer close(progressDone)

	if pty {
		return srunPTY(v)
	}
	return srunStream(v)
}

// srunProgress reports why a job is not running yet, until it is.
//
// A command that appears to hang with no explanation is the most common
// complaint about batch systems, so the reason the scheduler gives is
// surfaced rather than left in squeue.
func srunProgress(id int64, done <-chan struct{}) {
	said := ""
	for {
		select {
		case <-done:
			return
		case <-time.After(400 * time.Millisecond):
		}
		var j ctl.JobView
		if err := call("GET", fmt.Sprintf("/job/%d", id), nil, &j); err != nil {
			return
		}
		switch job.State(j.State) {
		case job.Running:
			if !srunQuiet() {
				fmt.Fprintf(os.Stderr, "srun: job %d running on %s\n", id, j.Node)
			}
			return
		}
		if job.State(j.State).Terminal() {
			return
		}
		if j.Reason != "" && j.Reason != said {
			fmt.Fprintf(os.Stderr, "srun: %s\n", j.Reason)
			said = j.Reason
		}
	}
}

// needsValue reports whether an option consumes the following argument.
//
// Without this, `srun --cpus 4 python x.py` would treat "4" as the command.
// Options written as --cpus=4 carry their value already.
func needsValue(opt string) bool {
	if strings.Contains(opt, "=") {
		return false
	}
	switch strings.TrimLeft(opt, "-") {
	case "cpus", "c", "mem", "gres", "time", "t", "nodes", "N", "nodelist", "w",
		"constraint", "C", "name", "J", "fabric", "model", "dependency", "d",
		"total-cpus", "total-mem", "total-gpu-mem", "total-gpus", "procs",
		"stage-in", "stage-out", "chdir", "D":
		return true
	}
	return false
}

func srunQuiet() bool { return os.Getenv("SHOME_SRUN_QUIET") == "1" }

// srunArchived prints a job's stored output, as a fallback for a session that
// produced nothing live -- a controller restart between submit and attach,
// say. Retried briefly because a node uploads output just after the job ends.
func srunArchived(id int64) bool {
	for i := 0; i < 10; i++ {
		if body, err := callRaw("GET", fmt.Sprintf("/job/%d/output", id)); err == nil {
			os.Stdout.Write(body)
			return len(body) > 0
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// srunStream shows a running job's output until it finishes.
func srunStream(v ctl.JobView) error {
	body, err := openStream(fmt.Sprintf("/stream/%d/out", v.Stream))
	if err != nil {
		// The session is gone -- the controller restarted, most likely. The
		// output is still archived, so show that rather than nothing.
		srunArchived(v.ID)
		return srunFinish(v)
	}
	defer body.Close()
	n, _ := io.Copy(os.Stdout, body)
	if n == 0 {
		// Nothing came through live. Worth one look at the archive before
		// reporting silence: swallowing this is what made `srun hostname`
		// print nothing.
		srunArchived(v.ID)
	}
	return srunFinish(v)
}

// localWinsize measures the terminal srun is attached to.
//
// A zero size means there is nothing to measure -- output is a pipe, or the
// ioctl failed -- and the far end uses its default rather than a window of
// no size, which full-screen programs draw into and appear to hang.
func localWinsize() job.Winsize {
	ws, err := unix.IoctlGetWinsize(int(os.Stdin.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return job.Winsize{}
	}
	return job.Winsize{Rows: ws.Row, Cols: ws.Col, XPixel: ws.Xpixel, YPixel: ws.Ypixel}
}

// srunResize reports the terminal's size to the session, now and whenever it
// changes.
//
// SIGWINCH is how a terminal says the window moved; without acting on it a
// session started in one shape stays that shape forever, and everything the
// remote shell draws is off by the difference. The first report is sent
// unconditionally because the window may have changed between submitting the
// job and it starting -- a job that waited in the queue often has.
func srunResize(stream int64, done <-chan struct{}) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	defer signal.Stop(ch)
	last := job.Winsize{}
	for {
		if ws := localWinsize(); ws.Known() && ws != last {
			// Best effort: a session whose window size cannot be reported
			// is still a usable session, and the next change tries again.
			if err := call("POST", fmt.Sprintf("/stream/%d/resize", stream), ws, nil); err == nil {
				last = ws
			}
		}
		select {
		case <-ch:
		case <-done:
			return
		}
	}
}

// srunPTY runs an interactive session on a compute node.
func srunPTY(v ctl.JobView) error {
	// Raw mode, so keystrokes reach the far end unbuffered and the remote
	// shell does its own line editing -- exactly what ssh does. Without it
	// the local terminal would echo and buffer lines, and Ctrl-C would kill
	// srun rather than reaching the job.
	var restore func()
	if term.IsTerminal(int(os.Stdin.Fd())) {
		st, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err == nil {
			restore = func() { term.Restore(int(os.Stdin.Fd()), st) }
			defer restore()
		}
	}

	out, err := openStream(fmt.Sprintf("/stream/%d/out", v.Stream))
	if err != nil {
		return err
	}
	defer out.Close()

	// The window's size, in its own channel: it changes on the user's
	// schedule rather than the session's, and the relay carries keystrokes
	// without interpreting them, so it cannot carry this.
	resizeDone := make(chan struct{})
	go srunResize(v.Stream, resizeDone)
	defer close(resizeDone)

	// Keystrokes up, in its own request: the two directions are independent
	// streams, so neither has to wait for the other.
	go func() {
		postStream(fmt.Sprintf("/stream/%d/in", v.Stream), os.Stdin)
	}()

	io.Copy(os.Stdout, out)
	if restore != nil {
		restore()
		restore = nil
	}
	return srunFinish(v)
}

// srunFinish reports the job's exit status as srun's own.
func srunFinish(v ctl.JobView) error {
	// The status arrives separately from the output, and may land a moment
	// later: the node reports it after wait() returns.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var st struct {
			Exit     int  `json:"exit"`
			Finished bool `json:"finished"`
		}
		if err := call("GET", fmt.Sprintf("/stream/%d", v.Stream), nil, &st); err == nil && st.Finished {
			if st.Exit == 0 {
				return nil
			}
			return srunExit{code: st.Exit}
		}
		// Fall back to the job record, which is durable: a controller restart
		// loses the in-memory session but not the job's result.
		var j ctl.JobView
		if err := call("GET", fmt.Sprintf("/job/%d", v.ID), nil, &j); err == nil &&
			job.State(j.State).Terminal() {
			if job.State(j.State) == job.Completed && j.ExitCode == 0 {
				return nil
			}
			return srunExit{code: exitOrOne(j.ExitCode), state: j.State, reason: j.Reason}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil
}

func exitOrOne(code int) int {
	if code == 0 {
		return 1
	}
	return code
}

// srunExit carries a command's exit status out to main, so srun exits with
// the status of what it ran -- which is what makes it usable in a script.
type srunExit struct {
	code   int
	state  string
	reason string
}

func (e srunExit) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "exited with status %d", e.code)
	if e.state != "" && e.state != string(job.Failed) && e.state != string(job.Completed) {
		fmt.Fprintf(&b, " (%s)", e.state)
	}
	// The scheduler's reason is only added when it says something the status
	// does not: "exit code 42" after "exited with status 42" is noise.
	if r := e.reason; r != "" && !strings.Contains(r, fmt.Sprint(e.code)) {
		fmt.Fprintf(&b, ": %s", r)
	}
	return b.String()
}

// ExitCode lets main exit with the command's status rather than 1.
func (e srunExit) ExitCode() int { return e.code }

// asExitCode reports the status a failed command wants shome to exit with.
func asExitCode(err error) (int, bool) {
	var e srunExit
	if errors.As(err, &e) {
		return e.code, true
	}
	return 0, false
}
