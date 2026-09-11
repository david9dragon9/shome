package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/davidwu/shome/internal/ctl"
	"github.com/davidwu/shome/internal/job"
)

// serve manages resident services: allocations shome keeps alive.
func serve(args []string) error {
	if len(args) == 0 {
		fmt.Print(`shome serve - long-running services on the cluster

  create NAME SCRIPT [job opts]  keep SCRIPT running (accepts sbatch options)
  list                           show services and their backing jobs
  stop NAME                      stop it, keep the definition
  start NAME                     start it again
  delete NAME                    stop and forget it

A service is an ordinary scheduled job that shome restarts when it exits, so
it obeys the same owner policy, limits and preemption as any other work.
`)
		return nil
	}
	switch args[0] {
	case "create":
		return serveCreate(args[1:])
	case "list", "ls":
		return serveList()
	case "stop", "start":
		if len(args) < 2 {
			return fmt.Errorf("usage: shome serve %s NAME", args[0])
		}
		if err := call("POST", "/services/"+args[1]+"/"+args[0], nil, nil); err != nil {
			return err
		}
		fmt.Printf("%s: %s\n", args[1], map[string]string{
			"stop": "stopped", "start": "starting",
		}[args[0]])
		return nil
	case "delete", "rm":
		if len(args) < 2 {
			return fmt.Errorf("usage: shome serve delete NAME")
		}
		if err := call("DELETE", "/services/"+args[1], nil, nil); err != nil {
			return err
		}
		fmt.Printf("%s: deleted\n", args[1])
		return nil
	}
	return fmt.Errorf("unknown: shome serve %s", args[0])
}

func serveCreate(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: shome serve create NAME SCRIPT [options]")
	}
	name, script := args[0], args[1]

	// Reuse sbatch's option parsing so a service takes exactly the same
	// resource flags as a batch job -- including --total-* and --constraint.
	var spec job.Spec
	var array string
	var stageIn []string
	spec.Limits.CPUs = 1
	rest, err := parseOpts(args[2:], &spec, &array, &stageIn)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return fmt.Errorf("unexpected argument %q", rest[0])
	}
	abs, err := filepath.Abs(script)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err != nil {
		return fmt.Errorf("script %s: %w", script, err)
	}
	spec.Script = abs
	body, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	spec.ScriptBody = body
	if spec.Workdir == "" {
		spec.Workdir, _ = os.Getwd()
	}

	idle := ""
	for i := 2; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--idle-timeout=") {
			idle = strings.TrimPrefix(args[i], "--idle-timeout=")
		}
	}
	if err := call("POST", "/services", map[string]any{
		"name": name, "spec": spec, "idle_timeout": idle,
	}, nil); err != nil {
		return err
	}
	fmt.Printf("service %q created; it will start on the next scheduling pass\n", name)
	fmt.Println("  watch it with: shome serve list")
	return nil
}

func serveList() error {
	var svcs []ctl.ServiceView
	if err := call("GET", "/services", nil, &svcs); err != nil {
		return err
	}
	if len(svcs) == 0 {
		fmt.Println("(no services)")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tOWNER\tDESIRED\tJOB\tSTATE\tNODE\tUPTIME\tRESTARTS")
	for _, s := range svcs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%d\n",
			s.Name, s.Owner, s.Desired, s.JobID, s.JobState, s.Node, s.Uptime, s.Restarts)
	}
	return w.Flush()
}
