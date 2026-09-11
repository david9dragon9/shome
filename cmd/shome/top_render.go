package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/davidwu/shome/internal/ctl"
	"github.com/davidwu/shome/internal/tui"
)

// Layout. The dashboard is built as a list of lines and truncated to the
// terminal height, rather than positioned absolutely: an absolutely-placed
// layout has to be re-derived every time a section changes size, and gets it
// wrong on a small window in ways that corrupt the display.

// render produces the whole frame. interactive controls whether selection
// highlighting and the key hints appear.
func (u *topUI) render(w, h int, interactive bool) string {
	if w < 40 {
		w = 40
	}
	var lines []string
	add := func(s string) { lines = append(lines, s) }

	if u.dash == nil {
		if u.err != nil {
			return tui.C(tui.BrightRed, "cannot reach the controller: "+u.err.Error()) +
				"\n\nIs it running?  shome status\n"
		}
		return "connecting...\n"
	}
	d := u.dash

	lines = append(lines, u.renderHeader(d, w)...)
	add("")
	lines = append(lines, u.renderNodes(d, w, interactive)...)

	if len(d.Alerts) > 0 {
		add("")
		lines = append(lines, u.renderAlerts(d, w)...)
	}

	add("")
	// Give the queue whatever vertical space is left, so a tall window shows
	// more jobs rather than more blank space.
	remaining := h - len(lines) - 2
	if remaining < 4 {
		remaining = 4
	}
	lines = append(lines, u.renderJobs(d, w, remaining, interactive)...)

	if u.showHelp {
		lines = u.renderHelp(w)
	}

	// One width clamp over every line, rather than truncating at each call
	// site. Any section that forgets would otherwise wrap and shear the whole
	// layout below it, and that failure only shows up in a narrow window --
	// which is exactly where nobody is looking when they write the section.
	body := clip(lines, h-1)
	for i, l := range body {
		body[i] = stripTrailing(l, w)
	}
	return strings.Join(body, "\n") + "\n" + stripTrailing(u.renderFooter(w), w)
}

// clip trims to n lines and pads escape-safe.
func clip(lines []string, n int) []string {
	if n < 1 {
		n = 1
	}
	if len(lines) > n {
		out := append([]string(nil), lines[:n-1]...)
		return append(out, tui.C(tui.Grey, fmt.Sprintf("  … %d more line(s); enlarge the window", len(lines)-n+1)))
	}
	return lines
}

func (u *topUI) renderHeader(d *ctl.Dashboard, w int) []string {
	t := d.Totals
	title := tui.C(tui.Bold, "shome") + tui.C(tui.Grey, " cluster dashboard")
	status := fmt.Sprintf("%d/%d nodes up", t.NodesUp, t.Nodes)
	if t.NodesDrain > 0 {
		status += tui.C(tui.BrightYellow, fmt.Sprintf("  %d draining", t.NodesDrain))
	}
	if t.NodesDown > 0 {
		status += tui.C(tui.BrightRed, fmt.Sprintf("  %d down", t.NodesDown))
	}
	clock := d.At.Format("15:04:05")
	if u.paused {
		clock = tui.C(tui.BrightYellow, "PAUSED "+clock)
	}
	left := title + "   " + status
	line1 := left + strings.Repeat(" ", maxInt(1, w-tui.VisibleLen(left)-tui.VisibleLen(clock))) + tui.C(tui.Grey, clock)

	// Two rows of cluster-wide gauges: measured load on the left, scheduler
	// allocation on the right. Both matter and they are routinely different --
	// a fully allocated cluster can be idle, and a busy machine can have
	// nothing allocated because its owner is using it.
	gw := clampInt((w-46)/2, 10, 40)
	cpuAlloc := pctOf(int64(t.UsedCPUs), int64(t.CPUs))
	memAlloc := pctOf(t.UsedMemMiB, t.MemMiB)

	line2 := fmt.Sprintf("  %s %s %s   %s %s %s",
		tui.C(tui.Grey, "cpu  used"), tui.Gauge(t.CPUPercent, gw, tui.LoadColor(t.CPUPercent)), tui.Pct(t.CPUPercent),
		tui.C(tui.Grey, "alloc"), tui.Gauge(cpuAlloc, gw, tui.LoadColor(cpuAlloc)),
		fmt.Sprintf("%d/%d", t.UsedCPUs, t.CPUs))
	line3 := fmt.Sprintf("  %s %s %s   %s %s %s",
		tui.C(tui.Grey, "mem  used"), tui.Gauge(t.MemPercent, gw, tui.LoadColor(t.MemPercent)), tui.Pct(t.MemPercent),
		tui.C(tui.Grey, "alloc"), tui.Gauge(memAlloc, gw, tui.LoadColor(memAlloc)),
		fmt.Sprintf("%s/%s", tui.MiB(t.UsedMemMiB), tui.MiB(t.MemMiB)))

	jobs := fmt.Sprintf("  %s %s running   %s pending   %s held   %s gpu(s), %s gpu mem",
		tui.C(tui.Grey, "jobs"),
		tui.C(tui.BrightGreen, fmt.Sprint(t.Running)),
		tui.C(tui.BrightYellow, fmt.Sprint(t.Pending)),
		tui.C(tui.Grey, fmt.Sprint(t.Held)),
		tui.C(tui.BrightCyan, fmt.Sprint(t.GPUs)),
		tui.MiB(t.GPUMemMiB))

	out := []string{line1, line2, line3, jobs}
	if t.Reporting < t.NodesUp {
		out = append(out, tui.C(tui.Grey, fmt.Sprintf(
			"  note: %d of %d live node(s) are reporting utilisation; the rest show allocation only",
			t.Reporting, t.NodesUp)))
	}
	return out
}

func (u *topUI) renderNodes(d *ctl.Dashboard, w int, interactive bool) []string {
	out := []string{sectionTitle("NODES", w)}
	if len(d.Nodes) == 0 {
		return append(out, tui.C(tui.Grey, "  no nodes. add one with: shome invite"))
	}
	// Column widths chosen from the data so names are never truncated harder
	// than they have to be.
	nameW := 4
	for _, n := range d.Nodes {
		if l := len(n.Name); l > nameW {
			nameW = l
		}
	}
	nameW = clampInt(nameW, 6, 18)
	gw := clampInt((w-nameW-62)/2, 8, 24)
	sparkW := clampInt(gw, 8, 20)

	out = append(out, tui.C(tui.Grey, fmt.Sprintf("  %-*s %-6s %-*s %5s %-*s %5s %-*s %s",
		nameW, "NODE", "STATE", gw, "CPU", "", sparkW, "TREND", "", gw, "MEM", "JOBS  NOTE")))

	for i, n := range d.Nodes {
		sel := interactive && u.focus == paneNodes && i == u.nodeSel
		out = append(out, u.renderNodeRow(n, nameW, gw, sparkW, w, sel))
	}
	return out
}

func (u *topUI) renderNodeRow(n ctl.NodeDash, nameW, gw, sparkW, w int, selected bool) string {
	cpu, memPct := tui.Unknown, tui.Unknown
	if n.HasTelem && !n.Stale {
		cpu = n.Telemetry.CPUPercent
		memPct = n.Telemetry.MemPercent()
	}
	var hist []float64
	for _, s := range n.History {
		hist = append(hist, s.CPU)
	}

	state := tui.C(tui.StateColor(n.State), fmt.Sprintf("%-6s", n.State))
	row := fmt.Sprintf("  %-*s %s %s %s %s %s %s  %s",
		nameW, tui.Truncate(n.Name, nameW),
		state,
		tui.Gauge(cpu, gw, tui.LoadColor(cpu)), tui.Pct(cpu),
		tui.Sparkline(hist, sparkW, tui.Grey),
		tui.Gauge(memPct, gw, tui.LoadColor(memPct)), tui.Pct(memPct),
		u.nodeNote(n))
	if selected {
		return tui.C(tui.Reverse, tui.Pad(stripTrailing(row, w), w))
	}
	return row
}

// nodeNote is the short right-hand annotation: job count plus whatever the
// reader most needs to know about this machine right now.
func (u *topUI) nodeNote(n ctl.NodeDash) string {
	parts := []string{fmt.Sprintf("%2d", n.RunningJobs)}
	if n.Tier != "" && n.Tier != "full" {
		parts = append(parts, tui.C(tui.BrightYellow, n.Tier))
	}
	if n.HasTelem && !n.Stale {
		t := n.Telemetry
		if t.OnBattery {
			parts = append(parts, tui.C(tui.BrightYellow, "battery"))
		}
		switch t.Thermal {
		case "serious", "critical":
			parts = append(parts, tui.C(tui.BrightRed, "thermal:"+t.Thermal))
		}
		if t.MemoryPressure == "critical" {
			parts = append(parts, tui.C(tui.BrightRed, "mempress"))
		}
		// Someone is at the keyboard. The single most important thing to see
		// on a cluster made of other people's computers.
		if t.UserIdleSec >= 0 && t.UserIdleSec < 60 {
			parts = append(parts, tui.C(tui.BrightCyan, "owner active"))
		}
	}
	if n.Stale && n.State == "UP" {
		parts = append(parts, tui.C(tui.Grey, "no telemetry"))
	}
	if n.GPUKind != "" {
		parts = append(parts, tui.C(tui.Grey, n.GPUKind))
	}
	if n.Reason != "" {
		parts = append(parts, tui.C(tui.Grey, n.Reason))
	}
	return strings.Join(parts, "  ")
}

func (u *topUI) renderAlerts(d *ctl.Dashboard, w int) []string {
	out := []string{sectionTitle("ATTENTION", w)}
	for _, a := range d.Alerts {
		if len(out) > 6 {
			out = append(out, tui.C(tui.Grey, fmt.Sprintf("  … and %d more", len(d.Alerts)-5)))
			break
		}
		out = append(out, "  "+tui.C(tui.BrightYellow, "▲ ")+tui.Truncate(a, w-4))
	}
	return out
}

func (u *topUI) renderJobs(d *ctl.Dashboard, w, maxLines int, interactive bool) []string {
	jobs := u.visibleJobs(d)
	title := fmt.Sprintf("QUEUE (%d)", len(jobs))
	if u.filter != "" {
		title += "  filter: " + u.filter
	}
	out := []string{sectionTitle(title, w)}

	if len(jobs) == 0 {
		msg := "  nothing queued or running"
		if u.filter != "" {
			msg = "  nothing matches " + u.filter
		}
		out = append(out, tui.C(tui.Grey, msg))
		return append(out, u.renderRecent(d, w, maxLines-len(out)-1)...)
	}

	nameW := clampInt(w/6, 8, 22)
	userW := clampInt(w/10, 6, 12)
	nodeW := clampInt(w/8, 6, 16)
	out = append(out, tui.C(tui.Grey, fmt.Sprintf("  %-7s %-*s %-*s %-10s %8s %6s %9s %-*s %s",
		"JOB", nameW, "NAME", userW, "USER", "STATE", "TIME", "CPUS", "MEM", nodeW, "NODE", "WHY")))

	shown := jobs
	if len(shown) > maxLines-2 {
		shown = shown[:maxInt(1, maxLines-3)]
	}
	for i, j := range shown {
		sel := interactive && u.focus == paneJobs && i == u.jobSel
		out = append(out, u.renderJobRow(j, nameW, userW, nodeW, w, sel))
	}
	if len(jobs) > len(shown) {
		out = append(out, tui.C(tui.Grey, fmt.Sprintf("  … and %d more", len(jobs)-len(shown))))
	}
	return append(out, u.renderRecent(d, w, maxLines-len(out)-1)...)
}

func (u *topUI) renderJobRow(j ctl.JobView, nameW, userW, nodeW, w int, selected bool) string {
	// Memory as live/requested, because that comparison is the whole story of
	// whether a job asked for the right amount.
	mem := tui.MiB(j.MemMiB)
	if j.LiveMiB >= 0 {
		mem = tui.MiB(j.LiveMiB) + "/" + tui.MiB(j.MemMiB)
	}
	why := j.Reason
	if j.Throttled {
		why = "throttled for owner; " + why
	}
	state := j.State
	if j.Held {
		state = "HELD"
	}
	row := fmt.Sprintf("  %-7s %-*s %-*s %s %8s %6d %9s %-*s %s",
		j.Label,
		nameW, tui.Truncate(j.Name, nameW),
		userW, tui.Truncate(j.User, userW),
		tui.C(tui.StateColor(state), fmt.Sprintf("%-10s", state)),
		j.Elapsed, j.CPUs, mem,
		nodeW, tui.Truncate(j.Node, nodeW),
		tui.C(tui.Grey, why))
	if selected {
		return tui.C(tui.Reverse, tui.Pad(stripTrailing(row, w), w))
	}
	return row
}

func (u *topUI) renderRecent(d *ctl.Dashboard, w, maxLines int) []string {
	if maxLines < 3 || len(d.Recent) == 0 {
		return nil
	}
	out := []string{"", sectionTitle("RECENTLY FINISHED", w)}
	for i, j := range d.Recent {
		if i >= maxLines-2 {
			break
		}
		out = append(out, fmt.Sprintf("  %-7s %-22s %s %8s  %s",
			j.Label, tui.Truncate(j.Name, 22),
			tui.C(tui.StateColor(j.State), fmt.Sprintf("%-13s", j.State)),
			j.Elapsed, tui.C(tui.Grey, tui.Truncate(j.Reason, maxInt(0, w-56)))))
	}
	return out
}

func (u *topUI) renderFooter(w int) string {
	// Anything needing a decision takes over the footer: a prompt buried under
	// key hints is a prompt nobody answers.
	if u.confirm != "" {
		return tui.C(tui.Reverse+tui.BrightYellow, tui.Pad("  "+u.confirm+"   [y/N] ", w))
	}
	if u.filterOn {
		return tui.C(tui.Reverse, tui.Pad("  filter: "+u.filter+"▌  (Enter to apply, Esc to clear)", w))
	}
	if u.message != "" && time.Since(u.messageAt) < 4*time.Second {
		return tui.C(tui.Reverse, tui.Pad("  "+u.message, w))
	}
	if u.err != nil {
		return tui.C(tui.BrightRed, tui.Truncate("  controller unreachable: "+u.err.Error(), w))
	}

	keys := "  tab panes   ↑↓ select   k kill   h hold/release   r requeue   d drain   u resume   / filter   ? help   q quit"
	if u.focus == paneNodes {
		keys = "  tab panes   ↑↓ select   d drain   u resume   f forget   D drain-all   / filter   ? help   q quit"
	}
	return tui.C(tui.Grey, tui.Truncate(keys, w))
}

func (u *topUI) renderHelp(w int) []string {
	rows := [][2]string{
		{"tab", "switch between the node and queue panes"},
		{"↑ ↓ / j k", "move the selection"},
		{"", ""},
		{"in the QUEUE pane", ""},
		{"k", "cancel the selected job"},
		{"h", "hold a pending job, or release a held one"},
		{"r", "requeue: kill it and put it back in the queue"},
		{"", ""},
		{"in the NODES pane", ""},
		{"d", "drain: stop new work; running jobs continue"},
		{"u", "resume a drained node"},
		{"f", "forget a node that has left for good"},
		{"D", "drain every node (break glass)"},
		{"", ""},
		{"anywhere", ""},
		{"/", "filter by name, user, node or state"},
		{"p", "pause refreshing"},
		{"+ -", "faster or slower refresh"},
		{"1", "refresh once, now"},
		{"?", "close this help"},
		{"q", "quit"},
	}
	out := []string{sectionTitle("KEYS", w), ""}
	for _, r := range rows {
		if r[0] == "" && r[1] == "" {
			out = append(out, "")
			continue
		}
		if r[1] == "" {
			out = append(out, "  "+tui.C(tui.Bold, r[0]))
			continue
		}
		out = append(out, fmt.Sprintf("    %s  %s",
			tui.C(tui.BrightCyan, fmt.Sprintf("%-12s", r[0])), tui.C(tui.Grey, r[1])))
	}
	out = append(out, "", tui.C(tui.Grey,
		"  Every action here is also a command: shome admin --help"))
	return out
}

// visibleJobs applies the current filter.
func (u *topUI) visibleJobs(d *ctl.Dashboard) []ctl.JobView {
	if u.filter == "" {
		return d.Jobs
	}
	q := strings.ToLower(u.filter)
	var out []ctl.JobView
	for _, j := range d.Jobs {
		hay := strings.ToLower(j.Name + " " + j.User + " " + j.Node + " " + j.State + " " + j.Label)
		if strings.Contains(hay, q) {
			out = append(out, j)
		}
	}
	return out
}

func sectionTitle(s string, w int) string {
	bar := strings.Repeat("─", maxInt(0, w-len(s)-4))
	return tui.C(tui.Bold, " "+s+" ") + tui.C(tui.Grey, bar)
}

// stripTrailing cuts a row to w visible cells, preserving colour codes.
func stripTrailing(s string, w int) string {
	if tui.VisibleLen(s) <= w {
		return s
	}
	var b strings.Builder
	n, inEsc := 0, false
	for _, r := range s {
		switch {
		case inEsc:
			b.WriteRune(r)
			if r == 'm' {
				inEsc = false
			}
		case r == '\033':
			inEsc = true
			b.WriteRune(r)
		default:
			if n >= w {
				return b.String()
			}
			b.WriteRune(r)
			n++
		}
	}
	return b.String()
}

func pctOf(used, total int64) float64 {
	if total <= 0 {
		return tui.Unknown
	}
	return 100 * float64(used) / float64(total)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
