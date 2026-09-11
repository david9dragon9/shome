// Package pipeline turns a declarative DAG into ordinary shome jobs.
//
// Deliberately a thin layer: stages become jobs and edges become the
// dependencies the scheduler already understands. A pipeline runner that
// tracked its own state would duplicate the scheduler and drift from it --
// this way a pipeline stage is inspectable with squeue like anything else.
package pipeline

import (
	"fmt"
	"sort"
	"strings"

	"github.com/davidwu/shome/internal/job"
	"gopkg.in/yaml.v3"
)

// Spec is a parsed pipeline file.
type Spec struct {
	Name   string  `yaml:"name"`
	Stages []Stage `yaml:"stages"`
}

// Stage is one node of the DAG.
type Stage struct {
	Name   string   `yaml:"name"`
	Script string   `yaml:"script"`
	After  []string `yaml:"after"`

	CPUs   int    `yaml:"cpus"`
	Mem    string `yaml:"mem"`
	GPUs   int    `yaml:"gpus"`
	Time   string `yaml:"time"`
	Fabric string `yaml:"fabric"`
	Model  string `yaml:"model"`

	TotalCPUs   int    `yaml:"total_cpus"`
	TotalMem    string `yaml:"total_mem"`
	TotalGPUMem string `yaml:"total_gpu_mem"`
	Nodes       string `yaml:"nodes"`

	// Inputs and Outputs name paths relative to the stage's working
	// directory. shome uses them only for staging; it does not try to infer
	// the DAG from them, because a stage that reads a file another stage
	// happens to write is not necessarily ordered after it.
	Inputs  []string `yaml:"inputs"`
	Outputs []string `yaml:"outputs"`
}

// Parse reads a pipeline definition and validates its shape.
func Parse(data []byte) (*Spec, error) {
	var s Spec
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("invalid pipeline file: %w", err)
	}
	if len(s.Stages) == 0 {
		return nil, fmt.Errorf("pipeline has no stages")
	}
	seen := map[string]bool{}
	for i, st := range s.Stages {
		if st.Name == "" {
			return nil, fmt.Errorf("stage %d has no name", i+1)
		}
		if seen[st.Name] {
			return nil, fmt.Errorf("duplicate stage name %q", st.Name)
		}
		seen[st.Name] = true
		if st.Script == "" {
			return nil, fmt.Errorf("stage %q has no script", st.Name)
		}
	}
	for _, st := range s.Stages {
		for _, dep := range st.After {
			if !seen[dep] {
				return nil, fmt.Errorf("stage %q depends on unknown stage %q", st.Name, dep)
			}
			if dep == st.Name {
				return nil, fmt.Errorf("stage %q depends on itself", st.Name)
			}
		}
	}
	if cyc := findCycle(s.Stages); cyc != "" {
		return nil, fmt.Errorf("pipeline has a dependency cycle: %s", cyc)
	}
	return &s, nil
}

// findCycle returns a human-readable cycle, or "" if the graph is acyclic.
//
// Worth catching here rather than at submission: a cycle would otherwise
// become a set of jobs that all sit PENDING forever waiting on each other,
// which looks like a scheduler bug rather than a typo in the pipeline.
func findCycle(stages []Stage) string {
	deps := map[string][]string{}
	for _, s := range stages {
		deps[s.Name] = s.After
	}
	const (
		white = 0
		grey  = 1
		black = 2
	)
	colour := map[string]int{}
	var path []string
	var visit func(string) string
	visit = func(n string) string {
		colour[n] = grey
		path = append(path, n)
		for _, d := range deps[n] {
			switch colour[d] {
			case grey:
				// Found a back edge: report from the first occurrence.
				for i, p := range path {
					if p == d {
						return strings.Join(append(path[i:], d), " -> ")
					}
				}
				return d + " -> " + d
			case white:
				if c := visit(d); c != "" {
					return c
				}
			}
		}
		path = path[:len(path)-1]
		colour[n] = black
		return ""
	}
	names := make([]string, 0, len(deps))
	for n := range deps {
		names = append(names, n)
	}
	sort.Strings(names) // deterministic error messages
	for _, n := range names {
		if colour[n] == white {
			if c := visit(n); c != "" {
				return c
			}
		}
	}
	return ""
}

// Order returns stages in a valid submission order (dependencies first).
func (s *Spec) Order() ([]Stage, error) {
	byName := map[string]Stage{}
	for _, st := range s.Stages {
		byName[st.Name] = st
	}
	var out []Stage
	done := map[string]bool{}
	// Repeatedly take any stage whose dependencies are already placed. The
	// input order is preserved among ready stages so the result is stable.
	for len(out) < len(s.Stages) {
		progress := false
		for _, st := range s.Stages {
			if done[st.Name] {
				continue
			}
			ready := true
			for _, d := range st.After {
				if !done[d] {
					ready = false
					break
				}
			}
			if !ready {
				continue
			}
			out = append(out, st)
			done[st.Name] = true
			progress = true
		}
		if !progress {
			return nil, fmt.Errorf("pipeline cannot be ordered; check for a dependency cycle")
		}
	}
	return out, nil
}

// ToSpec converts a stage into a job spec. parseMem and parseTime are injected
// so this package need not depend on the CLI's flag parsing.
func (st Stage) ToSpec(parseMem func(string) (int64, error)) (job.Spec, error) {
	spec := job.Spec{
		Name:        st.Name,
		Script:      st.Script,
		Fabric:      st.Fabric,
		Model:       st.Model,
		ArrayTaskID: -1,
		StageOut:    st.Outputs,
		Limits:      job.Limits{CPUs: max(st.CPUs, 1), GPUs: st.GPUs},
	}
	for _, pair := range []struct {
		in  string
		out *int64
	}{{st.Mem, &spec.Limits.MemBytes}, {st.TotalMem, &spec.TotalMemBytes}, {st.TotalGPUMem, &spec.TotalGPUMem}} {
		if pair.in == "" {
			continue
		}
		v, err := parseMem(pair.in)
		if err != nil {
			return spec, fmt.Errorf("stage %q: %w", st.Name, err)
		}
		*pair.out = v
	}
	if st.Time != "" {
		d, err := job.ParseWalltime(st.Time)
		if err != nil {
			return spec, fmt.Errorf("stage %q: %w", st.Name, err)
		}
		spec.Limits.Walltime = d
	}
	spec.TotalCPUs = st.TotalCPUs
	if st.Nodes != "" && st.Nodes != "auto" {
		var n int
		if _, err := fmt.Sscanf(st.Nodes, "%d", &n); err != nil || n < 1 {
			return spec, fmt.Errorf("stage %q: nodes must be a positive integer or 'auto'", st.Name)
		}
		spec.MaxNodes = n
	}
	return spec, nil
}

// DependencyExpr builds the --dependency string for a stage, given the job ids
// already submitted for its predecessors.
//
// afterok, not afterany: a pipeline stage should not run on the wreckage of a
// failed predecessor.
func DependencyExpr(after []string, ids map[string]int64) string {
	if len(after) == 0 {
		return ""
	}
	parts := make([]string, 0, len(after))
	for _, d := range after {
		if id, ok := ids[d]; ok {
			parts = append(parts, fmt.Sprintf("%d", id))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "afterok:" + strings.Join(parts, ":")
}
