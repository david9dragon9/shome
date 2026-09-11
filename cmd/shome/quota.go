package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/davidwu/shome/internal/ctl"
	"github.com/davidwu/shome/internal/job"
)

// `squota` answers "what am I using, and what am I allowed?".
//
// The pieces were spread across three commands -- `shome admin qos show` for
// the limits, `shome fs du` for storage, `sshare` for standing -- and two of
// them read as administrative. This is the one a user runs.

func quotaUsage() {
	fmt.Print(`squota - what you are using, and what you are allowed

  squota             your own usage
  squota USER        somebody else's (admin)

Shows compute in use against your ceilings, storage on each machine against
your cluster-wide limit, and where you stand in the queue.

  unlimited   no ceiling on this
  none        you may not use this at all

Related:
  shome fs du     storage in more detail, file counts per machine
  sshare          fair-share standing, and how the queue is ordered
  sinfo           what the cluster has in total

Also reachable as 'shome quota'.
`)
}

func quota(args []string) error {
	who := ""
	for _, a := range args {
		switch a {
		case "help", "-h", "--help":
			quotaUsage()
			return nil
		default:
			if strings.HasPrefix(a, "-") {
				return fmt.Errorf("unknown option %q for squota", a)
			}
			who = a
		}
	}
	q := "/quota"
	if who != "" {
		q += "?user=" + urlEscape(who)
	}
	var v ctl.QuotaView
	if err := call("GET", q, nil, &v); err != nil {
		return err
	}

	fmt.Printf("usage for %s\n\n", v.User)

	// A tabwriter per section, rather than one with blank filler rows:
	// filler rows are padded to the table width and leave trailing
	// whitespace on screen, and the sections have different columns anyway.
	section := func(header string, rows [][]string) {
		if len(rows) == 0 {
			return
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, header)
		for _, r := range rows {
			fmt.Fprintln(w, strings.Join(r, "\t"))
		}
		w.Flush()
		fmt.Println()
	}

	var now [][]string
	for _, l := range v.Compute {
		now = append(now, []string{l.Name, quotaAmount(l.Used, l.Unit),
			quotaLimit(l.Limit, l.Unit), quotaBar(l)})
	}
	section("RIGHT NOW\tUSED\tLIMIT\t", now)

	// Storage is per machine because a file lives on one machine; the same
	// path on two machines is two files and costs twice.
	store := [][]string{}
	for _, n := range v.Storage.Nodes {
		stale := ""
		if n.Stale {
			stale = "(last reported a while ago)"
		}
		store = append(store, []string{"  " + n.Node, bytesShort(n.Bytes), "", stale})
	}
	if len(store) == 0 {
		store = append(store, []string{"  (nothing stored yet)", "", "", ""})
	}
	total := ctl.QuotaLine{Used: v.Storage.TotalBytes, Limit: v.Storage.LimitBytes, Unit: "bytes"}
	store = append(store, []string{"total, all machines", bytesShort(v.Storage.TotalBytes),
		quotaLimit(v.Storage.LimitBytes, "bytes"), quotaBar(total)})
	section("STORAGE\tUSED\tLIMIT\t", store)

	var perJob [][]string
	for _, l := range v.PerJob {
		perJob = append(perJob, []string{l.Name, quotaLimit(l.Limit, l.Unit)})
	}
	section("PER JOB\tLIMIT", perJob)

	if v.Share != nil {
		fmt.Printf("fair-share factor %.2f of 1.00\n", v.Share.Factor)
	}
	if v.PriorityHint != "" {
		fmt.Printf("%s\n", wrapText(v.PriorityHint, 76))
	}
	if v.StorageOld {
		fmt.Printf("\nSome machines have not reported recently, so their figures may lag.\n")
	}
	return nil
}

// quotaAmount renders a measured value in its unit.
func quotaAmount(v int64, unit string) string {
	switch unit {
	case "MiB":
		return bytesShort(v << 20)
	case "bytes":
		return bytesShort(v)
	case "seconds":
		return job.FormatDuration(time.Duration(v) * time.Second)
	}
	return fmt.Sprintf("%d", v)
}

// quotaLimit renders a ceiling, keeping "unlimited" and "none" distinct.
//
// Zero is not the same as no limit: zero means the account may not use this
// at all, and collapsing the two would tell somebody they had unlimited GPUs
// when they had been denied every one.
func quotaLimit(v int64, unit string) string {
	switch {
	case v < 0:
		return "unlimited"
	case v == 0:
		return "none"
	}
	return quotaAmount(v, unit)
}

// quotaBar draws how full an allowance is.
//
// Only where there is a limit to be full of: a bar against "unlimited" would
// be either always empty or meaningless.
func quotaBar(l ctl.QuotaLine) string {
	p := l.Percent()
	if p < 0 {
		return ""
	}
	const width = 20
	n := int(p / 100 * width)
	if n > width {
		n = width
	}
	if n < 0 {
		n = 0
	}
	return fmt.Sprintf("[%s%s] %3.0f%%",
		strings.Repeat("#", n), strings.Repeat(".", width-n), p)
}

// wrapText breaks a sentence at word boundaries so it reads in a terminal.
func wrapText(s string, width int) string {
	var out strings.Builder
	line := 0
	for i, word := range strings.Fields(s) {
		if line > 0 && line+1+len(word) > width {
			out.WriteString("\n")
			line = 0
		} else if i > 0 {
			out.WriteString(" ")
			line++
		}
		out.WriteString(word)
		line += len(word)
	}
	return out.String()
}
