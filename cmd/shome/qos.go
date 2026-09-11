package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/davidwu/shome/internal/qos"
)

// The QoS command surface.
//
// One set of flag names for everything -- the cluster defaults, an account's
// overrides, and the file on disk -- taken from qos.Fields so the three cannot
// drift apart. Adding a limit means adding it there and nowhere else.

func qosUsage() {
	fmt.Print(`shome admin qos - limits on what one account may use

  qos                          what applies to you
  qos show cluster             totals across everybody, and what is in use
  qos show default             what each account gets
  qos show NAME                what applies to one account, and its usage

  qos set cluster --LIMIT V    change the cluster-wide totals
  qos set default --LIMIT V    change what every account gets
  qos set NAME --LIMIT V       change one account, overriding the default
  qos clear NAME [--LIMIT]     drop an override, back to the default
  qos file                     where the limits are stored, for editing

Two separate things. "cluster" is a total across everybody: how much of this
cluster is handed out at once, whoever is asking. "default" is what each
account gets. Per-account limits multiply -- ten accounts each allowed four
accelerators is forty -- so the cluster total is what actually bounds it.

limits:
`)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, f := range qos.Fields {
		unit := ""
		if f.Unit == "MB" {
			unit = " (e.g. 8G, 512M)"
		} else if f.Unit == "duration" {
			unit = " (e.g. 2h, 30m)"
		}
		fmt.Fprintf(w, "  --%s\t%s%s\n", f.Name, f.Help, unit)
	}
	w.Flush()
	fmt.Print(`
Any limit may be set to 'unlimited'. Cluster defaults apply to everybody;
an account's own value replaces just that one limit for them.
`)
}

func adminQoS(args []string) error {
	if len(args) == 0 {
		return qosShow("")
	}
	switch args[0] {
	case "show":
		name := ""
		if len(args) > 1 {
			name = args[1]
		}
		return qosShow(name)
	case "cluster":
		return qosShow("cluster")
	case "file":
		var v struct {
			File string `json:"file"`
		}
		if err := call("GET", "/qos", nil, &v); err != nil {
			return err
		}
		fmt.Println(v.File)
		fmt.Println("\nEdit it directly if you prefer; changes apply within seconds.")
		return nil
	case "set":
		return qosSet(args[1:])
	case "clear":
		return qosClear(args[1:])
	case "help", "-h", "--help":
		qosUsage()
		return nil
	}
	qosUsage()
	return fmt.Errorf("unknown: shome admin qos %s", args[0])
}

// qosShow prints what applies, what was overridden, and what is being used.
func qosShow(name string) error {
	q := "/qos"
	if name != "" {
		q += "?user=" + urlEscape(name)
	}
	var v struct {
		Cluster   map[string]any    `json:"cluster"`
		Defaults  map[string]any    `json:"defaults"`
		Overrides map[string]any    `json:"overrides"`
		Effective map[string]string `json:"effective"`
		Usage     map[string]any    `json:"usage"`
		File      string            `json:"file"`
	}
	if name == "default" {
		// The per-account layer on its own, with nothing resolved over it.
		var raw struct {
			Defaults qos.Limits `json:"defaults"`
			File     string     `json:"file"`
		}
		if err := call("GET", "/qos", nil, &raw); err != nil {
			return err
		}
		eff := qos.Resolve(raw.Defaults, qos.Limits{})
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "LIMIT\tVALUE")
		for _, f := range qos.Fields {
			fmt.Fprintf(w, "--%s\t%s\n", f.Name, f.Get(eff))
		}
		w.Flush()
		fmt.Printf("\nlimits file: %s\n", raw.File)
		return nil
	}
	if err := call("GET", q, nil, &v); err != nil {
		return err
	}
	switch name {
	case "cluster":
		fmt.Printf("cluster totals \u2014 across everybody, whoever is asking\n\n")
	case "default":
		fmt.Printf("what each account gets unless overridden\n\n")
	case "":
		fmt.Printf("limits in force for you\n\n")
	default:
		fmt.Printf("limits in force for %s\n\n", name)
	}

	// Usage next to the limit it counts against, because "8" means nothing
	// without knowing you are at 7.
	using := map[string]string{}
	if v.Usage != nil {
		g := func(k string) string {
			if f, ok := v.Usage[k].(float64); ok {
				return fmt.Sprintf("%.0f", f)
			}
			return ""
		}
		using["max-submitted-jobs"] = g("submitted_jobs")
		using["max-running-jobs"] = g("running_jobs")
		using["max-running-cpus"] = g("running_cpus")
		using["max-running-gpus"] = g("running_gpus")
		using["max-running-mem"] = g("running_mem_mb")
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "LIMIT\tVALUE\tIN USE\tSOURCE")
	for _, f := range qos.Fields {
		src := "per-account default"
		if name == "cluster" {
			src = "cluster-wide"
			if v.Cluster == nil || v.Cluster[jsonKeyFor(f.Name)] == nil {
				src = "not set (unlimited)"
			}
		} else if v.Overrides != nil {
			if _, ok := v.Overrides[jsonKeyFor(f.Name)]; ok {
				src = "set for this account"
			}
		}
		use := using[f.Name]
		if use == "" {
			use = "-"
		} else if f.Unit == "MB" {
			use += " MiB"
		}
		fmt.Fprintf(w, "--%s\t%s\t%s\t%s\n", f.Name, v.Effective[f.Name], use, src)
	}
	w.Flush()
	fmt.Printf("\nlimits file: %s\n", v.File)
	return nil
}

// jsonKeyFor maps a flag name to the JSON field it is stored under, so "was
// this overridden?" can be answered without a second table to maintain.
func jsonKeyFor(flag string) string {
	k := strings.ReplaceAll(flag, "-", "_")
	switch k {
	case "max_running_mem":
		return "max_running_mem_mb"
	case "max_mem_per_job":
		return "max_mem_mb_per_job"
	case "max_disk":
		return "max_disk_mb"
	}
	return k
}

// parseLimitFlags reads --limit value pairs into a sparse Limits.
func parseLimitFlags(args []string) (qos.Limits, error) {
	var l qos.Limits
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			return l, fmt.Errorf("expected a --limit flag, got %q", a)
		}
		name, val, inline := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if !inline {
			if i+1 >= len(args) {
				return l, fmt.Errorf("--%s needs a value", name)
			}
			i++
			val = args[i]
		}
		f, ok := qos.FieldByName(name)
		if !ok {
			return l, fmt.Errorf("unknown limit --%s\n\nSee: shome admin qos help", name)
		}
		if err := f.Set(&l, val); err != nil {
			return l, fmt.Errorf("--%s: %w", name, err)
		}
	}
	return l, nil
}

func qosSet(args []string) error {
	// A leading non-flag word names an account; without one this is the
	// cluster default.
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if len(args) == 0 {
		return fmt.Errorf("nothing to set. Say what you are changing:\n" +
			"  shome admin qos set cluster --max-running-gpus 6   totals across everybody\n" +
			"  shome admin qos set default --max-running-jobs 8   what each account gets\n" +
			"  shome admin qos set alice   --max-running-gpus 1   one account")
	}
	// Naming neither target used to mean "the defaults", which was ambiguous
	// once there were two cluster-level layers. Ask rather than guess.
	if name == "" {
		return fmt.Errorf("say which limits to change:\n" +
			"  shome admin qos set cluster --...   totals across everybody\n" +
			"  shome admin qos set default --...   what each account gets\n" +
			"  shome admin qos set NAME --...      one account")
	}

	if name == "cluster" || name == "default" {
		// Merge into what is already set rather than replacing it, so
		// changing one limit does not silently drop the rest.
		var cur struct {
			Cluster  qos.Limits `json:"cluster"`
			Defaults qos.Limits `json:"defaults"`
		}
		if err := call("GET", "/qos", nil, &cur); err != nil {
			return err
		}
		add, err := parseLimitFlags(args)
		if err != nil {
			return err
		}
		path, base := "/qos", cur.Defaults
		if name == "cluster" {
			path, base = "/qos/cluster", cur.Cluster
		}
		if err := call("POST", path, mergeLimits(base, add), nil); err != nil {
			return err
		}
		fmt.Printf("%s limits updated\n\n", name)
		return qosShow(name)
	}

	var cur struct {
		Overrides qos.Limits `json:"overrides"`
	}
	if err := call("GET", "/qos?user="+urlEscape(name), nil, &cur); err != nil {
		return err
	}
	add, err := parseLimitFlags(args)
	if err != nil {
		return err
	}
	if err := call("POST", "/users/"+urlEscape(name)+"/qos", mergeLimits(cur.Overrides, add), nil); err != nil {
		return err
	}
	fmt.Printf("limits updated for %s\n\n", name)
	return qosShow(name)
}

func qosClear(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: shome admin qos clear NAME [--LIMIT ...]\n" +
			"With no --limit, every override is dropped and the account returns to the defaults.")
	}
	name, rest := args[0], args[1:]
	if len(rest) == 0 {
		if err := call("POST", "/users/"+urlEscape(name)+"/qos", qos.Limits{}, nil); err != nil {
			return err
		}
		fmt.Printf("%s is back on the cluster defaults\n\n", name)
		return qosShow(name)
	}
	var cur struct {
		Overrides qos.Limits `json:"overrides"`
	}
	if err := call("GET", "/qos?user="+urlEscape(name), nil, &cur); err != nil {
		return err
	}
	l := cur.Overrides
	for _, a := range rest {
		f, ok := qos.FieldByName(strings.TrimPrefix(a, "--"))
		if !ok {
			return fmt.Errorf("unknown limit %s", a)
		}
		f.Clear(&l)
	}
	if err := call("POST", "/users/"+urlEscape(name)+"/qos", l, nil); err != nil {
		return err
	}
	fmt.Printf("limits updated for %s\n\n", name)
	return qosShow(name)
}

// mergeLimits layers b over a, field by field.
func mergeLimits(a, b qos.Limits) qos.Limits {
	if b.MaxSubmittedJobs != nil {
		a.MaxSubmittedJobs = b.MaxSubmittedJobs
	}
	if b.MaxRunningJobs != nil {
		a.MaxRunningJobs = b.MaxRunningJobs
	}
	if b.MaxRunningCPUs != nil {
		a.MaxRunningCPUs = b.MaxRunningCPUs
	}
	if b.MaxRunningGPUs != nil {
		a.MaxRunningGPUs = b.MaxRunningGPUs
	}
	if b.MaxRunningMemMB != nil {
		a.MaxRunningMemMB = b.MaxRunningMemMB
	}
	if b.MaxCPUsPerJob != nil {
		a.MaxCPUsPerJob = b.MaxCPUsPerJob
	}
	if b.MaxGPUsPerJob != nil {
		a.MaxGPUsPerJob = b.MaxGPUsPerJob
	}
	if b.MaxMemMBPerJob != nil {
		a.MaxMemMBPerJob = b.MaxMemMBPerJob
	}
	if b.MaxWalltime != nil {
		a.MaxWalltime = b.MaxWalltime
	}
	if b.MaxProcsPerJob != nil {
		a.MaxProcsPerJob = b.MaxProcsPerJob
	}
	if b.MaxDiskMB != nil {
		a.MaxDiskMB = b.MaxDiskMB
	}
	return a
}

var _ = sort.Strings
