package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/ptyx"
)

// The node's half of an interactive session.
//
// `srun` and `srun --pty` differ in what has to be relayed, and the
// difference is worth keeping rather than papering over:
//
//   - Without a terminal, only output flows. The job runs exactly as a batch
//     job does, writing to its output file, and this tails that file. The
//     output is still archived afterwards, so `shome cat` works on it later,
//     and nothing about enforcement changes.
//   - With a terminal, both directions flow and there is no output file,
//     because a terminal has no separate stdout and stderr to capture.
//
// Either way the job goes through the ordinary launch path -- same sandbox,
// same QoS clamp, same memory polling -- because a second launch path is a
// second place for enforcement to be left out.

// ControllerAPI forwards a job's cluster-API requests to the controller.
//
// Implemented by *Client; an interface so a job's link to the cluster can
// be tested without one, and so a node with no controller connection is
// simply a nil here rather than a special case in the launch path.
type ControllerAPI interface {
	ProxyAPI(w http.ResponseWriter, r *http.Request)
}

// StreamRelay is what the agent needs from its controller connection to
// carry an interactive session. Implemented by *Client; an interface so the
// relay can be tested without a controller.
type StreamRelay interface {
	// StreamOut sends a session's output up, reading from r until it ends.
	StreamOut(ctx context.Context, stream int64, r io.Reader) error
	// StreamIn returns the user's keystrokes as a stream.
	StreamIn(ctx context.Context, stream int64) (io.ReadCloser, error)
	// StreamSize waits for a window size newer than the one the node has
	// already applied, and reports it with the version that names it.
	StreamSize(ctx context.Context, stream int64, since int64) (job.Winsize, int64, error)
	// StreamExit reports the process's final status.
	StreamExit(ctx context.Context, stream int64, code int) error
}

// tailInterval is how often a job's output file is re-checked while it runs.
//
// Short enough that output feels live, long enough that a chatty job does not
// make the agent spin. A file that is being appended to gives no readiness
// signal, so there is nothing to wait on but the clock.
const tailInterval = 150 * time.Millisecond

// relayOutput tails a running job's output file and streams it to the user.
func (a *Agent) relayOutput(ctx context.Context, rel StreamRelay, stream, id int64,
	outPath string, done <-chan struct{}) {

	pr, pw := io.Pipe()
	go func() {
		if err := rel.StreamOut(ctx, stream, pr); err != nil {
			a.Log.Debug("interactive output relay ended", "job", id, "err", err)
		}
	}()
	defer pw.Close()

	var f *os.File
	// The file appears when the job starts; a short wait avoids reporting
	// nothing for a job that is a moment behind.
	for i := 0; i < 40 && f == nil; i++ {
		if h, err := os.Open(outPath); err == nil {
			f = h
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	if f == nil {
		fmt.Fprintf(pw, "shome: no output from job %d\n", id)
		return
	}
	defer f.Close()

	buf := make([]byte, 32<<10)
	finished := false
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if _, werr := pw.Write(buf[:n]); werr != nil {
				return
			}
			continue // more may be waiting
		}
		if err != nil && err != io.EOF {
			return
		}
		// At the end of the file. If the job has finished, one more read has
		// already happened above, so everything written is sent.
		if finished {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-done:
			// Read once more before stopping, or the last lines a job wrote
			// as it exited would never be shown.
			finished = true
		case <-time.After(tailInterval):
		}
	}
}

// resizeRetry is how long the agent waits before asking for the window size
// again after a failed request.
//
// Long enough that a controller which is down or restarting is not hammered
// by every interactive job at once, short enough that a session whose window
// changed during the outage is put right rather than left wrong.
const resizeRetry = 2 * time.Second

// relayTTY connects a job's terminal to the user's, in both directions.
//
// done is closed when the job has finished, which is what stops the
// window-size loop; the other two directions stop on their own when the
// terminal closes.
func (a *Agent) relayTTY(ctx context.Context, rel StreamRelay, stream, id int64,
	p *ptyx.PTY, done <-chan struct{}) {
	// Output up. The request body is the terminal, so it ends when the
	// terminal closes -- which is when the job and everything holding its
	// terminal have exited.
	go func() {
		if err := rel.StreamOut(ctx, stream, p.Master); err != nil {
			a.Log.Debug("interactive tty output ended", "job", id, "err", err)
		}
	}()
	// Keystrokes down.
	go func() {
		in, err := rel.StreamIn(ctx, stream)
		if err != nil {
			a.Log.Debug("no interactive input for this session", "job", id, "err", err)
			return
		}
		defer in.Close()
		io.Copy(p.Master, in)
	}()
	// The window size, which changes on its own schedule and so has its own
	// channel. Without this the job's terminal keeps whatever size it was
	// created with: line editing wraps at the wrong column and full-screen
	// programs draw to the wrong shape for the rest of the session.
	go a.followSize(ctx, rel, stream, id, p, done)
}

// followSize keeps a job's terminal the size of the user's window.
//
// A loop rather than a single request because the size changes whenever the
// user drags their window, and each long poll returns one change. It ends
// with the session: the context is cancelled, or the relay reports the
// stream is gone.
func (a *Agent) followSize(ctx context.Context, rel StreamRelay, stream, id int64,
	p *ptyx.PTY, done <-chan struct{}) {

	// A poll that is already in flight when the job ends would otherwise
	// hold a request open on the controller for a session that no longer
	// exists, so the job finishing cancels it.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-done:
			cancel()
		case <-ctx.Done():
		}
	}()

	var ver int64
	for {
		// The wall clock, not the agent's: this measures how long a request
		// to the controller actually took, which a test clock would not.
		began := time.Now()
		ws, next, err := rel.StreamSize(ctx, stream, ver)
		if ctx.Err() != nil {
			return
		}
		switch {
		case err != nil:
			a.Log.Debug("no terminal size for this session", "job", id, "err", err)
		case next > ver:
			ver = next
			if ws.Known() {
				if err := p.Resize(ws.Rows, ws.Cols, ws.XPixel, ws.YPixel); err != nil {
					a.Log.Debug("could not resize a session's terminal", "job", id, "err", err)
				}
			}
			continue
		}
		// Either an error, or a poll that came back with nothing new. The
		// second is the ordinary case for a window nobody is resizing and
		// costs nothing to repeat -- unless it returned at once, which is
		// what a session the controller has already forgotten looks like.
		// Waiting before asking again is what keeps that from becoming a
		// loop that spins until the job ends.
		if time.Since(began) >= resizeRetry {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(resizeRetry):
		}
	}
}

// StartInteractive launches a job with a terminal attached and relays it.
//
// The terminal is created here rather than inside the backend so that the
// backend keeps one launch path: it takes a terminal if it is given one.
func (a *Agent) StartInteractive(ctx context.Context, rel StreamRelay, id int64,
	stream int64, pty bool, launch func(tty *os.File) error, outPath string,
	done <-chan struct{}) error {

	if !pty {
		if err := launch(nil); err != nil {
			return err
		}
		go a.relayOutput(ctx, rel, stream, id, outPath, done)
		return nil
	}

	p, err := ptyx.Open()
	if err != nil {
		return fmt.Errorf("allocate a terminal: %w", err)
	}
	p.Resize(job.DefaultWinsize.Rows, job.DefaultWinsize.Cols, 0, 0)
	if err := launch(p.Slave); err != nil {
		p.Close()
		return err
	}
	// The agent's copy of the job's terminal goes now that the job holds it,
	// so reads on the master end when the job does.
	p.CloseSlave()
	a.relayTTY(ctx, rel, stream, id, p, done)
	go func() {
		<-done
		p.Close()
	}()
	return nil
}

// SetNames records what the controller says this cluster and machine are
// called, for an interactive job's prompt.
func (a *Agent) SetNames(cluster, label string) {
	a.mu.Lock()
	a.cluster, a.label = cluster, label
	a.mu.Unlock()
}

// Names returns the cluster and machine names, with sensible fallbacks for a
// node that has not heard from its controller yet.
func (a *Agent) Names() (cluster, label string) {
	a.mu.Lock()
	cluster, label = a.cluster, a.label
	a.mu.Unlock()
	if cluster == "" {
		cluster = "shome"
	}
	if label == "" {
		label = a.Node
	}
	return cluster, label
}

// interactiveEnv is what an interactive job gets beyond an ordinary one.
//
// The prompt is set here rather than by sourcing a startup file, because a
// job's sandbox denies the installation root where that file lives -- and
// widening a *job's* sandbox to reach shome's own directory to make a prompt
// prettier would be the wrong trade. A PS1 string needs no file.
func (a *Agent) interactiveEnv(spec job.Spec) map[string]string {
	cluster, label := a.Names()
	env := map[string]string{
		"SHOME_CLUSTER": cluster,
		"SHOME_HOST":    label,
		"SHOME_USER":    spec.User,
		"TERM":          "xterm-256color",
	}
	if spec.PTY {
		// user@machine.cluster, matching the login node's prompt, so it is
		// always clear which machine a session is on.
		//
		// Deliberately free of bash's \w and \[ \] escapes: /bin/sh is dash
		// on most Linux distributions and shows those literally, which turns
		// the prompt into line noise on exactly the machines most likely to
		// be compute nodes. ${PWD##*/} is POSIX and works everywhere.
		env["PS1"] = fmt.Sprintf("%s@%s.%s:${PWD##*/}$ ", spec.User, label, cluster)
		env["SHOME_INTERACTIVE"] = "1"
	}
	return env
}

// IsolationDrain reports whether this machine should refuse cluster work
// because it cannot confine it.
//
// A drain rather than a launch refusal: refusing at launch livelocks, because
// the controller marks the job running, the agent says no, the controller
// requeues it, and the pair cycle on every heartbeat. The disk rule learned
// the same lesson, and this uses the same mechanism.
//
// Unlike the owner's own yield rules, this one is not the owner's to
// override. Those are about what they are willing to lend; this is about
// whether other people's code can be contained at all.
func (a *Agent) IsolationDrain() (action, reason string) {
	if a.Backend == nil {
		return "", ""
	}
	if why := a.Backend.IsolationFault(); why != "" {
		return "drain", why
	}
	return "", ""
}
