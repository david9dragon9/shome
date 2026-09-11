package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/davidwu/shome/internal/agent"
	"github.com/davidwu/shome/internal/platform"
	"github.com/davidwu/shome/internal/tui"
)

// The owner's frame.
//
// Ordered by what an owner actually wants to know, in order: is anything
// running on my machine, is it hurting, what is it costing me, and can I stop
// it. Cluster-wide figures are deliberately absent -- that is `shome top`.

func (u *monitorUI) render(w, h int, interactive bool) string {
	if w < 40 {
		w = 40
	}
	if u.err != nil {
		return u.renderNoStatus(w)
	}
	var lines []string
	add := func(s string) { lines = append(lines, s) }

	lines = append(lines, u.renderHeader(w)...)
	add("")
	lines = append(lines, u.renderUtilisation(w)...)
	add("")
	lines = append(lines, u.renderContribution(w)...)
	add("")

	remaining := h - len(lines) - 2
	if remaining < 4 {
		remaining = 4
	}
	lines = append(lines, u.renderJobs(w, remaining)...)

	if u.showHelp {
		lines = u.renderHelp(w)
	}
	body := clip(lines, h-1)
	for i, l := range body {
		body[i] = stripTrailing(l, w)
	}
	return strings.Join(body, "\n") + "\n" + stripTrailing(u.renderFooter(w), w)
}

// renderNoStatus explains the one failure a reader will actually hit: shome
// is not running here, or has never run.
func (u *monitorUI) renderNoStatus(w int) string {
	var b strings.Builder
	b.WriteString(tui.C(tui.Bold, "shome monitor") + "\n\n")
	b.WriteString("No local status to read.\n\n")
	b.WriteString("  " + tui.C(tui.Grey, "looked in "+agent.StatusFile(u.root)) + "\n\n")
	b.WriteString("This machine may not be running shome, or may never have joined a\n")
	b.WriteString("cluster. Check with:\n\n")
	b.WriteString("    shome status\n\n")
	b.WriteString("Start it with 'shome up', or join a cluster with 'shome join'.\n")
	return b.String()
}

func (u *monitorUI) renderHeader(w int) []string {
	st := u.st
	title := tui.C(tui.Bold, "this machine") + tui.C(tui.Grey, " in the cluster")
	name := tui.C(tui.BrightCyan, st.Node)

	// The owner's disposition, which is the single most important line here:
	// it is the answer to "is my machine mine right now".
	var disp string
	switch {
	case st.Paused:
		disp = tui.C(tui.BrightYellow, "PAUSED")
		if st.PauseReason != "" {
			disp += tui.C(tui.Grey, " — "+st.PauseReason)
		}
	case st.DiskRefusal != "":
		disp = tui.C(tui.BrightYellow, "NOT TAKING WORK") + tui.C(tui.Grey, " — "+st.DiskRefusal)
	case st.OwnerAction == "suspend" || st.OwnerAction == "drain":
		disp = tui.C(tui.BrightYellow, strings.ToUpper(st.OwnerAction))
		if st.OwnerReason != "" {
			disp += tui.C(tui.Grey, " — "+st.OwnerReason)
		}
	case st.OwnerAction == "throttle":
		disp = tui.C(tui.BrightYellow, "THROTTLED") + tui.C(tui.Grey, " — "+st.OwnerReason)
	default:
		disp = tui.C(tui.BrightGreen, "CONTRIBUTING")
		if st.OwnerReason != "" {
			disp += tui.C(tui.Grey, " — "+st.OwnerReason)
		}
	}

	link := tui.C(tui.BrightGreen, "connected")
	if !st.Connected {
		link = tui.C(tui.BrightRed, "cannot reach the controller")
		if st.LastError != "" {
			link += tui.C(tui.Grey, " ("+tui.Truncate(st.LastError, 40)+")")
		}
	}
	if st.Controller != "" {
		link = tui.C(tui.Grey, st.Controller+" — ") + link
	}

	clock := st.At.Format("15:04:05")
	if u.stale() {
		clock = tui.C(tui.BrightRed, "stale, last written "+clock)
	} else if u.paused {
		clock = tui.C(tui.BrightYellow, "frozen "+clock)
	} else {
		clock = tui.C(tui.Grey, clock)
	}
	left := title + "   " + name
	line1 := left + strings.Repeat(" ", maxInt(1, w-tui.VisibleLen(left)-tui.VisibleLen(clock))) + clock

	out := []string{line1, "  " + disp, "  " + link}
	if u.stale() {
		out = append(out, "", "  "+tui.C(tui.BrightRed,
			"These figures are not current. The shome agent has stopped writing them."))
	}
	return out
}

func (u *monitorUI) renderUtilisation(w int) []string {
	t := u.st.Telemetry
	out := []string{sectionTitle("THIS MACHINE", w)}
	gw := clampInt(w-46, 10, 46)

	row := func(label string, pct float64, detail string) string {
		return fmt.Sprintf("  %-8s %s %s  %s",
			tui.C(tui.Grey, label), tui.Gauge(pct, gw, tui.LoadColor(pct)),
			tui.Pct(pct), tui.C(tui.Grey, detail))
	}
	out = append(out, row("cpu", t.CPUPercent, loadDetail(t)))
	out = append(out, row("memory", t.MemPercent(),
		fmt.Sprintf("%s of %s", tui.Bytes(t.MemUsedBytes), tui.Bytes(t.MemTotalBytes))))

	if t.KnownDisk() {
		out = append(out, row("disk", t.DiskPercent(),
			fmt.Sprintf("%s free; shome is using %s",
				tui.Bytes(t.DiskFreeBytes), tui.Bytes(t.ShomeDiskBytes))))
	}
	if t.KnownInodes() && t.InodesTotal > 0 {
		usedPct := 100 * float64(t.InodesTotal-t.InodesFree) / float64(t.InodesTotal)
		out = append(out, row("files", usedPct,
			fmt.Sprintf("shome holds %s of %s used",
				countShort(t.ShomeInodes), countShort(t.InodesTotal-t.InodesFree))))
	}
	if t.KnownNet() {
		out = append(out, fmt.Sprintf("  %-8s %s  %s",
			tui.C(tui.Grey, "network"),
			tui.C(tui.BrightCyan, fmt.Sprintf("%s/s in, %s/s out",
				tui.Bytes(int64(t.NetRxBytesPerSec)), tui.Bytes(int64(t.NetTxBytesPerSec)))),
			tui.C(tui.Grey, "whole machine, not just shome")))
	}

	// Owner-facing conditions, which are why work may be suspended.
	var flags []string
	if t.OnBattery {
		flags = append(flags, tui.C(tui.BrightYellow, "on battery"))
	}
	if t.Thermal == "serious" || t.Thermal == "critical" {
		flags = append(flags, tui.C(tui.BrightRed, "thermal "+t.Thermal))
	}
	if t.MemoryPressure == "warn" || t.MemoryPressure == "critical" {
		flags = append(flags, tui.C(tui.BrightYellow, "memory pressure "+t.MemoryPressure))
	}
	if t.UserIdleSec >= 0 && t.UserIdleSec < 60 {
		flags = append(flags, tui.C(tui.BrightCyan, "you are at the keyboard"))
	}
	if len(flags) > 0 {
		out = append(out, "  "+tui.C(tui.Grey, "state    ")+strings.Join(flags, "  "))
	}
	return out
}

func loadDetail(t platform.Telemetry) string {
	if t.LoadAvg1 < 0 {
		return ""
	}
	d := fmt.Sprintf("load %.2f", t.LoadAvg1)
	if t.Uptime > 0 {
		d += fmt.Sprintf(", up %s", shortDur(t.Uptime))
	}
	return d
}

// renderContribution shows the gap the owner's policy makes: what the machine
// has, against what the cluster is allowed to touch.
func (u *monitorUI) renderContribution(w int) []string {
	st := u.st
	out := []string{sectionTitle("WHAT THE CLUSTER MAY USE", w)}
	line := func(label string, advertised, total string, capped bool) string {
		note := ""
		if capped {
			note = tui.C(tui.BrightYellow, "  (your limit)")
		}
		return fmt.Sprintf("  %-8s %s of %s%s", tui.C(tui.Grey, label),
			tui.C(tui.Bold, advertised), tui.C(tui.Grey, total), note)
	}
	out = append(out, line("cores", fmt.Sprint(st.AdvertisedCPUs), fmt.Sprint(st.TotalCPUs),
		st.TotalCPUs > 0 && st.AdvertisedCPUs < st.TotalCPUs))
	out = append(out, line("memory", tui.MiB(st.AdvertisedMemMiB), tui.MiB(st.TotalMemMiB),
		st.TotalMemMiB > 0 && st.AdvertisedMemMiB < st.TotalMemMiB))
	if st.TotalGPUs > 0 || st.AdvertisedGPUs > 0 {
		out = append(out, line("gpu", fmt.Sprint(st.AdvertisedGPUs), fmt.Sprint(st.TotalGPUs),
			st.AdvertisedGPUs < st.TotalGPUs))
	}
	if st.Tier != "" {
		t := st.Tier
		if t != "full" {
			t = tui.C(tui.BrightYellow, t)
		}
		out = append(out, fmt.Sprintf("  %-8s %s", tui.C(tui.Grey, "tier"), t))
	}
	for _, l := range st.Limitations {
		out = append(out, "  "+tui.C(tui.Grey, "· "+tui.Truncate(l, maxInt(10, w-6))))
	}
	out = append(out, "", "  "+tui.C(tui.Grey, "change these with: shome config"))
	return out
}

func (u *monitorUI) renderJobs(w, maxLines int) []string {
	jobs := u.st.Jobs
	out := []string{sectionTitle(fmt.Sprintf("RUNNING HERE (%d)", len(jobs)), w)}
	if len(jobs) == 0 {
		msg := "  nothing is running on this machine"
		if u.st.Completed > 0 {
			msg += fmt.Sprintf(" (%d job(s) finished since shome started)", u.st.Completed)
		}
		return append(out, tui.C(tui.Grey, msg))
	}
	nameW := clampInt(w/6, 8, 20)
	userW := clampInt(w/10, 6, 12)
	out = append(out, tui.C(tui.Grey, fmt.Sprintf("  %-7s %-*s %-*s %8s %5s %6s %11s  %s",
		"JOB", nameW, "NAME", userW, "WHOSE", "TIME", "CPUS", "PROCS", "MEMORY", "")))

	shown := jobs
	if len(shown) > maxLines-2 {
		shown = shown[:maxInt(1, maxLines-3)]
	}
	for _, j := range shown {
		mem := tui.MiB(j.LiveMiB)
		if j.MemMiB > 0 {
			mem += "/" + tui.MiB(j.MemMiB)
		}
		note := ""
		if j.Throttled {
			note = tui.C(tui.BrightYellow, "throttled for you")
		}
		out = append(out, fmt.Sprintf("  %-7d %-*s %-*s %8s %5d %6d %11s  %s",
			j.ID, nameW, tui.Truncate(j.Name, nameW), userW, tui.Truncate(j.User, userW),
			shortDur(time.Duration(j.ElapsedSec)*time.Second), j.CPUs, j.Procs, mem, note))
	}
	if len(jobs) > len(shown) {
		out = append(out, tui.C(tui.Grey, fmt.Sprintf("  … and %d more", len(jobs)-len(shown))))
	}
	return out
}

func (u *monitorUI) renderFooter(w int) string {
	if u.confirm != "" {
		return tui.C(tui.Reverse+tui.BrightYellow, tui.Pad("  "+u.confirm+"   [y/N] ", w))
	}
	if u.message != "" && time.Since(u.messageAt) < 4*time.Second {
		return tui.C(tui.Reverse, tui.Pad("  "+u.message, w))
	}
	keys := "  p pause   r resume   f refresh   space freeze   + - rate   ? help   q quit"
	if u.st.Paused {
		keys = "  " + tui.C(tui.BrightYellow, "PAUSED") +
			"   r resume   f refresh   space freeze   ? help   q quit"
	}
	return tui.C(tui.Grey, tui.Truncate(keys, w))
}

func (u *monitorUI) renderHelp(w int) []string {
	rows := [][2]string{
		{"p", "stop contributing now; running jobs are suspended, not killed"},
		{"r", "contribute again"},
		{"f", "refresh immediately"},
		{"space", "freeze the display"},
		{"+ -", "faster or slower refresh"},
		{"q", "quit"},
		{"", ""},
		{"why local?", "this reads a file the agent writes here, so it works"},
		{"", "even when the controller is unreachable"},
		{"", ""},
		{"settings", "shome config           what you give the cluster"},
		{"", "shome status           the same figures, once"},
		{"", "shome top              the whole cluster (admin view)"},
	}
	out := []string{sectionTitle("KEYS", w), ""}
	for _, r := range rows {
		if r[0] == "" && r[1] == "" {
			out = append(out, "")
			continue
		}
		out = append(out, fmt.Sprintf("    %s  %s",
			tui.C(tui.BrightCyan, fmt.Sprintf("%-10s", r[0])), tui.C(tui.Grey, r[1])))
	}
	out = append(out, "", tui.C(tui.Grey,
		"  Pausing needs no permission from anyone. A cluster admin cannot undo it."))
	return out
}

func countShort(n int64) string {
	if n < 0 {
		return "?"
	}
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func shortDur(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}
