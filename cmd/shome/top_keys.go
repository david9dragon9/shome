package main

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/davidwu/shome/internal/ctl"
)

// Key handling and the actions behind it.
//
// Every destructive action goes through confirm(): the dashboard puts the
// selection under whichever row the cursor happens to be on, and a single
// keystroke that cancels somebody's twelve-hour job with no confirmation is
// the kind of interface people stop trusting after using once.
//
// Actions call the same HTTP endpoints the CLI does, so nothing is reachable
// here that is not equally scriptable -- the console is a convenience over a
// complete API, never a privileged path of its own.

func (u *topUI) handleKey(b byte) {
	u.mu.Lock()
	defer u.mu.Unlock()

	// Modal states consume input first.
	if u.confirm != "" {
		u.handleConfirmKey(b)
		return
	}
	if u.filterOn {
		u.handleFilterKey(b)
		return
	}

	switch b {
	case 'q', 3: // 3 is Ctrl-C, which raw mode still delivers as a keystroke
		u.quit = true
	case '?':
		u.showHelp = !u.showHelp
	case '\t':
		if u.focus == paneNodes {
			u.focus = paneJobs
		} else {
			u.focus = paneNodes
		}
	case 'j':
		u.move(1)
	case 'k':
		// 'k' is both "move down in vi" and "kill" here. Kill wins in the
		// queue pane because that is the action people came for; arrows still
		// move in either pane.
		if u.focus == paneJobs {
			u.killSelected()
		} else {
			u.move(-1)
		}
	case 'p':
		u.paused = !u.paused
		if u.paused {
			u.note("paused; press p to resume")
		} else {
			u.note("resumed")
		}
	case '+', '=':
		u.interval = clampInterval(u.interval - 500_000_000)
		u.note("refresh every %s", u.interval)
	case '-', '_':
		u.interval = clampInterval(u.interval + 500_000_000)
		u.note("refresh every %s", u.interval)
	case '1':
		u.mu.Unlock()
		u.fetchInto()
		u.mu.Lock()
	case '/':
		u.filterOn = true
	case 'h':
		u.holdOrRelease()
	case 'r':
		u.requeueSelected()
	case 'd':
		u.drainSelected()
	case 'u':
		u.resumeSelected()
	case 'f':
		u.forgetSelected()
	case 'D':
		u.drainAll()
	}
}

func (u *topUI) handleConfirmKey(b byte) {
	switch b {
	case 'y', 'Y':
		fn := u.confirmFunc
		u.confirm, u.confirmFunc = "", nil
		if fn == nil {
			return
		}
		// Released around the call: an action is a network round trip, and
		// holding the lock would freeze redrawing until it returns.
		u.mu.Unlock()
		err := fn()
		u.mu.Lock()
		if err != nil {
			u.note("failed: %s", err.Error())
		} else {
			u.note("done")
			u.mu.Unlock()
			u.fetchInto()
			u.mu.Lock()
		}
	default:
		u.confirm, u.confirmFunc = "", nil
		u.note("cancelled")
	}
}

func (u *topUI) handleFilterKey(b byte) {
	switch b {
	case '\r', '\n':
		u.filterOn = false
	case 27: // Esc
		u.filterOn, u.filter = false, ""
	case 127, 8: // Backspace
		if u.filter != "" {
			u.filter = u.filter[:len(u.filter)-1]
		}
	default:
		if b >= 32 && b < 127 {
			u.filter += string(b)
		}
	}
	// The selection is an index into a list the filter just changed.
	u.jobSel = 0
}

func (u *topUI) move(delta int) {
	if u.dash == nil {
		return
	}
	if u.focus == paneNodes {
		u.nodeSel = clampInt(u.nodeSel+delta, 0, maxInt(0, len(u.dash.Nodes)-1))
		return
	}
	u.jobSel = clampInt(u.jobSel+delta, 0, maxInt(0, len(u.visibleJobs(u.dash))-1))
}

// selectedJob returns the highlighted job, if the queue pane has one.
func (u *topUI) selectedJob() (ctl.JobView, bool) {
	if u.dash == nil {
		return ctl.JobView{}, false
	}
	jobs := u.visibleJobs(u.dash)
	if u.jobSel < 0 || u.jobSel >= len(jobs) {
		return ctl.JobView{}, false
	}
	return jobs[u.jobSel], true
}

func (u *topUI) selectedNode() (ctl.NodeDash, bool) {
	if u.dash == nil || u.nodeSel < 0 || u.nodeSel >= len(u.dash.Nodes) {
		return ctl.NodeDash{}, false
	}
	return u.dash.Nodes[u.nodeSel], true
}

// confirm arms a yes/no prompt in the footer.
func (u *topUI) confirmAction(prompt string, fn func() error) {
	u.confirm, u.confirmFunc = prompt, fn
}

func post(path string) error { return call("POST", path, nil, nil) }

func (u *topUI) killSelected() {
	j, ok := u.selectedJob()
	if !ok {
		u.note("no job selected")
		return
	}
	u.confirmAction(fmt.Sprintf("Cancel job %s (%s), owned by %s?", j.Label, j.Name, j.User),
		func() error { return post("/cancel/" + fmt.Sprint(j.ID)) })
}

// holdOrRelease is one key because they are opposites and the current state
// tells us unambiguously which one is meant.
func (u *topUI) holdOrRelease() {
	if u.focus != paneJobs {
		u.note("hold applies to jobs; press tab")
		return
	}
	j, ok := u.selectedJob()
	if !ok {
		u.note("no job selected")
		return
	}
	if j.Held {
		u.runNow(fmt.Sprintf("released job %s", j.Label),
			func() error { return post(fmt.Sprintf("/job/%d/release", j.ID)) })
		return
	}
	if j.State != "PENDING" {
		u.note("only a pending job can be held; job %s is %s", j.Label, j.State)
		return
	}
	u.runNow(fmt.Sprintf("held job %s", j.Label),
		func() error { return post(fmt.Sprintf("/job/%d/hold", j.ID)) })
}

func (u *topUI) requeueSelected() {
	if u.focus != paneJobs {
		u.resumeSelected()
		return
	}
	j, ok := u.selectedJob()
	if !ok {
		u.note("no job selected")
		return
	}
	u.confirmAction(fmt.Sprintf("Requeue job %s? It is killed and starts again from the beginning.", j.Label),
		func() error { return post(fmt.Sprintf("/job/%d/requeue", j.ID)) })
}

func (u *topUI) drainSelected() {
	n, ok := u.selectedNode()
	if !ok {
		u.note("no node selected; press tab to reach the node pane")
		return
	}
	u.confirmAction(fmt.Sprintf("Drain %s? Running jobs continue; no new work is placed there.",
		n.Name), func() error {
		return post("/node/" + url.PathEscape(n.Name) + "/drain?reason=" + url.QueryEscape("drained from the dashboard"))
	})
}

func (u *topUI) resumeSelected() {
	n, ok := u.selectedNode()
	if !ok {
		u.note("no node selected; press tab to reach the node pane")
		return
	}
	u.runNow(fmt.Sprintf("resumed %s", n.Name),
		func() error { return post("/node/" + url.PathEscape(n.Name) + "/resume") })
}

func (u *topUI) forgetSelected() {
	n, ok := u.selectedNode()
	if !ok {
		u.note("no node selected; press tab to reach the node pane")
		return
	}
	u.confirmAction(fmt.Sprintf("Forget %s entirely? Use this only for a machine that has left for good.",
		n.Name), func() error { return post("/node/" + url.PathEscape(n.Name) + "/remove") })
}

func (u *topUI) drainAll() {
	u.confirmAction("Drain EVERY node? The whole cluster stops accepting new work.",
		func() error {
			return post("/emergency/drain-all?reason=" + url.QueryEscape("drained from the dashboard"))
		})
}

// runNow performs a non-destructive action immediately, without a prompt.
func (u *topUI) runNow(success string, fn func() error) {
	u.mu.Unlock()
	err := fn()
	u.mu.Lock()
	if err != nil {
		u.note("failed: %s", strings.TrimSpace(err.Error()))
		return
	}
	u.note("%s", success)
	u.mu.Unlock()
	u.fetchInto()
	u.mu.Lock()
}
