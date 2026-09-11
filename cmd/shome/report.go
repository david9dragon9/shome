package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/davidwu/shome/internal/ctl"
	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/pipeline"
	"github.com/davidwu/shome/internal/userenv"
	"github.com/davidwu/shome/internal/xfer"
)

// sacct reports finished jobs. Unlike squeue it defaults to history, and
// includes the accounting columns you want after the fact: exit code, peak
// memory, and why the job ended.
func sacct(args []string) error {
	user := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-u", "--user":
			if i+1 < len(args) {
				i++
				user = args[i]
			}
		case "--me":
			user = currentUser()
		}
	}
	q := "/jobs"
	if user != "" {
		q += "?user=" + user
	}
	var jobs []ctl.JobView
	if err := call("GET", q, nil, &jobs); err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "JOBID\tNAME\tUSER\tSTATE\tEXIT\tELAPSED\tREQ MEM\tPEAK MEM\tREASON")
	n := 0
	for _, j := range jobs {
		if j.State == "PENDING" || j.State == "RUNNING" {
			continue
		}
		n++
		peak := "-"
		if j.PeakMiB > 0 {
			peak = fmt.Sprintf("%d MiB", j.PeakMiB)
		}
		req := "-"
		if j.MemMiB > 0 {
			req = fmt.Sprintf("%d MiB", j.MemMiB)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			j.Label, j.Name, j.User, j.State, j.ExitCode, j.Elapsed, req, peak, j.Reason)
	}
	if n == 0 {
		fmt.Fprintln(w, "(no completed jobs)")
	}
	return w.Flush()
}

// scontrol implements the read-only subset that gets used: `scontrol show job`.
func scontrol(args []string) error {
	if len(args) < 2 || args[0] != "show" {
		return fmt.Errorf("usage: scontrol show job JOBID")
	}
	switch args[1] {
	case "job":
		if len(args) < 3 {
			return fmt.Errorf("usage: scontrol show job JOBID")
		}
		id, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil {
			return fmt.Errorf("invalid job id %q", args[2])
		}
		var j ctl.JobView
		if err := call("GET", "/job/"+strconv.FormatInt(id, 10), nil, &j); err != nil {
			return err
		}
		p := func(k, v string) {
			if v != "" {
				fmt.Printf("  %-14s %s\n", k+":", v)
			}
		}
		fmt.Printf("Job %s\n", j.Label)
		p("Name", j.Name)
		p("User", j.User)
		p("State", j.State)
		p("Reason", j.Reason)
		p("Node", j.Node)
		p("Submitted", j.SubmitAt)
		p("Elapsed", j.Elapsed)
		p("TimeLimit", j.TimeLim)
		p("CPUs", strconv.Itoa(j.CPUs))
		p("MemReq", fmt.Sprintf("%d MiB", j.MemMiB))
		p("MemPeak", fmt.Sprintf("%d MiB", j.PeakMiB))
		p("GPUs", strconv.Itoa(j.GPUs))
		p("ExitCode", strconv.Itoa(j.ExitCode))
		// The priority breakdown, for a pending job on a cluster that orders
		// by it. "Why is this behind that one" needs the factors, not just
		// the score -- a single opaque number explains nothing.
		if j.Scored {
			var pr struct {
				Scored  bool    `json:"scored"`
				Explain string  `json:"explain"`
				Fair    float64 `json:"fair"`
				Age     float64 `json:"age"`
				Size    float64 `json:"size"`
			}
			if err := call("GET", fmt.Sprintf("/job/%d/priority", j.ID), nil, &pr); err == nil && pr.Scored {
				p("Priority", fmt.Sprintf("%.4f", j.Priority))
				p("  FairShare", fmt.Sprintf("%.4f", pr.Fair))
				p("  Age", fmt.Sprintf("%.4f", pr.Age))
				p("  Size", fmt.Sprintf("%.4f", pr.Size))
			}
		}
		p("Scratch", j.Scratch)
		if j.Scratch != "" {
			p("StdOut", j.Scratch+"/slurm-"+strconv.FormatInt(j.ID, 10)+".out")
		}
		return nil
	case "node", "nodes":
		return sinfo(nil)
	default:
		return fmt.Errorf("scontrol show %s is not supported yet", args[1])
	}
}

// shimNames are the Slurm commands shome answers to.
//
// Deliberately not a second copy: the sandboxed login sessions in
// internal/userenv need the same set, and keeping two lists is how one of
// them ends up missing a command.
var shimNames = userenv.ShimNames

// shims installs symlinks so existing habits and scripts work unchanged.
func shims(args []string) error {
	if len(args) < 2 || args[0] != "install" {
		return fmt.Errorf("usage: shome shims install DIR   (e.g. ~/bin or /usr/local/bin)")
	}
	dir := args[1]
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	for _, n := range shimNames {
		link := dir + string(os.PathSeparator) + n
		if existing, err := os.Readlink(link); err == nil {
			if existing == self {
				fmt.Printf("  ok       %s\n", link)
				continue
			}
			// Never clobber a real Slurm install or someone else's tool.
			return fmt.Errorf("%s already points at %s; remove it first", link, existing)
		}
		if _, err := os.Lstat(link); err == nil {
			return fmt.Errorf("%s already exists and is not a symlink; refusing to overwrite", link)
		}
		if err := os.Symlink(self, link); err != nil {
			return fmt.Errorf("link %s: %w", link, err)
		}
		fmt.Printf("  created  %s\n", link)
	}
	if !strings.Contains(os.Getenv("PATH"), dir) {
		fmt.Printf("\nnote: %s is not on your PATH\n", dir)
	}
	return nil
}

// catOutput prints a finished job's captured output.
//
// Output is fetched from the controller rather than the node: on a multi-node
// cluster the user has no reason to know, or be able to reach, whichever
// machine happened to run the job.
func catOutput(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: shome cat JOBID")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid job id %q", args[0])
	}
	q := "/job/" + strconv.FormatInt(id, 10) + "/output"
	for i := 1; i < len(args); i++ {
		if args[i] == "--rank" && i+1 < len(args) {
			q += "?rank=" + args[i+1]
		}
	}
	body, err := callRaw("GET", q)
	if err != nil {
		return err
	}
	os.Stdout.Write(body)
	return nil
}

// uploadStageIn packs the requested paths and sends them to the controller.
//
// The paths are relative to the directory the job was submitted from, and
// they keep those relative names inside the job -- that is what makes
// `--stage-in data.csv` then `open("data.csv")` work.
func uploadStageIn(jobID int64, paths []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := xfer.Tar(&buf, cwd, paths); err != nil {
		return err
	}
	return postRaw(fmt.Sprintf("/job/%d/stagein", jobID), &buf)
}

// checkStageIn rejects paths that cannot be staged, before anything is
// submitted.
//
// Before, rather than after: a mistyped input used to leave a held job
// behind for somebody to notice and cancel, and held jobs count against the
// account's queue limit.
func checkStageIn(paths []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	for _, p := range paths {
		// An absolute path has no relative name to land under, and joining
		// it to the submit directory quietly produced a different path
		// entirely: --stage-in /data/set.csv looked in ./data/set.csv, so it
		// either failed with a confusing message or staged the wrong file.
		if filepath.IsAbs(p) {
			return fmt.Errorf("--stage-in %s is an absolute path, and these are "+
				"relative to the directory you submit from -- so there is no name "+
				"for it to arrive under.\n\nRun from %s and pass %q, or copy it in "+
				"with 'shome fs cp'",
				p, filepath.Dir(p), filepath.Base(p))
		}
		full := filepath.Join(cwd, p)
		if !strings.HasPrefix(full, filepath.Clean(cwd)+string(filepath.Separator)) {
			return fmt.Errorf("--stage-in %s reaches outside the directory you "+
				"submit from, so it has no name to arrive under.\n\nRun from a "+
				"directory that contains it, or copy it in with 'shome fs cp'", p)
		}
		if _, err := os.Stat(full); err != nil {
			return fmt.Errorf("stage-in path %s (%s): %w", p, full, err)
		}
	}
	return nil
}

// fetchResults downloads and unpacks a job's staged-out files.
func fetchResults(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: shome fetch JOBID [DEST]")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid job id %q", args[0])
	}
	dest := "."
	if len(args) > 1 {
		dest = args[1]
	}
	body, err := callRaw("GET", fmt.Sprintf("/job/%d/stageout", id))
	if err != nil {
		return err
	}
	if err := xfer.Untar(bytes.NewReader(body), dest); err != nil {
		return err
	}
	abs, _ := filepath.Abs(dest)
	fmt.Printf("results for job %d written to %s\n", id, abs)
	return nil
}

// storage manages the caller's persistent files on the controller.
func storage(args []string) error {
	if len(args) == 0 {
		fmt.Print(`shome storage - your files on the controller machine

  ls [PATH]           list files
  put LOCAL [REMOTE]  upload a file
  get REMOTE [LOCAL]  download a file
  rm PATH             delete a file or directory
  du                  show usage against your limit

These are the same files as 'shome fs ls <controller>:'. Use 'shome fs' to
reach any other machine, to copy between machines, or to see your total.
`)
		return nil
	}
	switch args[0] {
	case "ls":
		path := ""
		if len(args) > 1 {
			path = args[1]
		}
		var es []ctl.Entry
		if err := call("GET", "/storage?path="+urlEscape(path), nil, &es); err != nil {
			return err
		}
		if len(es) == 0 {
			fmt.Println("(empty)")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, e := range es {
			kind, size := "", fmt.Sprintf("%d", e.Size)
			if e.IsDir {
				kind, size = "/", "-"
			}
			fmt.Fprintf(w, "%s%s\t%s\t%s\n", e.Name, kind, size, e.MTime[:19])
		}
		return w.Flush()

	case "put":
		if len(args) < 2 {
			return fmt.Errorf("usage: shome storage put LOCAL [REMOTE]")
		}
		local := args[1]
		remote := filepath.Base(local)
		if len(args) > 2 {
			remote = args[2]
		}
		f, err := os.Open(local)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := postRaw("/storage?path="+urlEscape(remote), f); err != nil {
			return err
		}
		fmt.Printf("uploaded %s -> %s\n", local, remote)
		return nil

	case "get":
		if len(args) < 2 {
			return fmt.Errorf("usage: shome storage get REMOTE [LOCAL]")
		}
		remote := args[1]
		local := filepath.Base(remote)
		if len(args) > 2 {
			local = args[2]
		}
		body, err := callRaw("GET", "/storage/file?path="+urlEscape(remote))
		if err != nil {
			return err
		}
		if err := os.WriteFile(local, body, 0o600); err != nil {
			return err
		}
		fmt.Printf("downloaded %s -> %s (%d bytes)\n", remote, local, len(body))
		return nil

	case "rm":
		if len(args) < 2 {
			return fmt.Errorf("usage: shome storage rm PATH")
		}
		if err := call("DELETE", "/storage?path="+urlEscape(args[1]), nil, nil); err != nil {
			return err
		}
		fmt.Printf("removed %s\n", args[1])
		return nil

	case "du":
		// The cluster-wide total, because that is what the limit is against.
		// This used to report only what was on the controller, which under
		// the per-machine storage model is one machine's share of the answer.
		var u ctl.FSUsage
		if err := call("GET", "/fs/usage", nil, &u); err != nil {
			return err
		}
		if u.LimitBytes < 0 {
			fmt.Printf("%s used across %d machine(s), no limit set\n",
				bytesShort(u.TotalBytes), len(u.Nodes))
			return nil
		}
		pct := 0.0
		if u.LimitBytes > 0 {
			pct = 100 * float64(u.TotalBytes) / float64(u.LimitBytes)
		}
		fmt.Printf("%s used of %s (%.0f%%), across %d machine(s)\n",
			bytesShort(u.TotalBytes), bytesShort(u.LimitBytes), pct, len(u.Nodes))
		fmt.Printf("per machine: shome fs du\n")
		return nil
	}
	return fmt.Errorf("unknown: shome storage %s", args[0])
}

// planCmd answers "can this run, and on which machines" without queueing anything.
func planCmd(args []string) error {
	var spec job.Spec
	var array string
	var stageIn []string
	if _, err := parseOpts(args, &spec, &array, &stageIn); err != nil {
		return err
	}
	var res ctl.PlanResult
	if err := call("POST", "/plan", spec, &res); err != nil {
		return err
	}
	if !res.Feasible {
		fmt.Printf("NOT SCHEDULABLE\n  %s\n", res.Why)
		return fmt.Errorf("request cannot be satisfied")
	}
	fmt.Printf("SCHEDULABLE\n  %s\n\n", res.Why)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  NODE\tCPUS\tMEM MiB\tGPUS\tGPU MiB")
	for _, s := range res.Shares {
		fmt.Fprintf(w, "  %s\t%d\t%d\t%d\t%d\n", s.Node, s.CPUs, s.MemMiB, s.GPUs, s.GPUMiB)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if res.Fabric != "" {
		fmt.Printf("\nfabric: %s\n", res.Fabric)
		for _, r := range res.FabricWhy {
			fmt.Printf("  - %s\n", r)
		}
		if len(res.FabricOptions) > 0 {
			fmt.Println("  considered:")
			for _, o := range res.FabricOptions {
				fmt.Printf("    %s\n", o)
			}
		}
	}
	return nil
}

// runPipeline submits a declarative DAG as a set of dependent jobs.
//
// Submits in topological order so each stage can name the real job ids of its
// predecessors. If a later stage fails validation, earlier ones are already
// queued -- so the whole file is validated up front and the ids are reported,
// letting the user cancel cleanly rather than hunt for orphans.
func runPipeline(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: shome pipeline run FILE.yaml")
	}
	path := args[0]
	if path == "run" {
		if len(args) < 2 {
			return fmt.Errorf("usage: shome pipeline run FILE.yaml")
		}
		path = args[1]
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	spec, err := pipeline.Parse(data)
	if err != nil {
		return err
	}
	ordered, err := spec.Order()
	if err != nil {
		return err
	}
	base := filepath.Dir(path)

	// Validate every stage before submitting any of them.
	specs := make([]job.Spec, 0, len(ordered))
	for _, st := range ordered {
		js, err := st.ToSpec(ctl.ParseMem)
		if err != nil {
			return err
		}
		if !filepath.IsAbs(js.Script) {
			js.Script = filepath.Join(base, js.Script)
		}
		body, rerr := os.ReadFile(js.Script)
		if rerr != nil {
			return fmt.Errorf("stage %q: script %s: %w", st.Name, js.Script, rerr)
		}
		js.ScriptBody = body
		specs = append(specs, js)
	}

	ids := map[string]int64{}
	var submitted []int64
	for i, st := range ordered {
		js := specs[i]
		js.Dependency = pipeline.DependencyExpr(st.After, ids)
		var v ctl.JobView
		if err := call("POST", "/submit", ctl.SubmitRequest{Spec: js}, &v); err != nil {
			return fmt.Errorf("stage %q: %w (already submitted: %v)", st.Name, err, submitted)
		}
		ids[st.Name] = v.ID
		submitted = append(submitted, v.ID)
		dep := js.Dependency
		if dep == "" {
			dep = "-"
		}
		fmt.Printf("  %-16s job %-5d depends on %s\n", st.Name, v.ID, dep)
	}
	fmt.Printf("\nsubmitted %d stages of %q\n", len(submitted), spec.Name)
	return nil
}
