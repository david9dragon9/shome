package pipeline

import (
	"strings"
	"testing"

	"github.com/davidwu/shome/internal/ctl"
)

const good = `
name: analysis
stages:
  - name: embed
    script: embed.sh
    gpus: 1
    mem: 4G
    outputs: [vectors]
  - name: index
    script: index.sh
    after: [embed]
    cpus: 8
    inputs: [vectors]
    outputs: [index]
  - name: evaluate
    script: eval.sh
    after: [index]
    total_gpu_mem: 20G
    nodes: auto
`

func TestParseAndOrder(t *testing.T) {
	s, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "analysis" || len(s.Stages) != 3 {
		t.Fatalf("parsed %+v", s)
	}
	ordered, err := s.Order()
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, st := range ordered {
		pos[st.Name] = i
	}
	if pos["embed"] > pos["index"] || pos["index"] > pos["evaluate"] {
		t.Errorf("bad order: %v", names(ordered))
	}
}

func names(ss []Stage) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.Name)
	}
	return out
}

func TestRejectsCycle(t *testing.T) {
	// A cycle would otherwise become jobs that sit PENDING forever waiting on
	// each other, which reads as a scheduler bug rather than a typo.
	_, err := Parse([]byte(`
stages:
  - {name: a, script: a.sh, after: [c]}
  - {name: b, script: b.sh, after: [a]}
  - {name: c, script: c.sh, after: [b]}
`))
	if err == nil {
		t.Fatal("cycle accepted")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error should name the problem: %v", err)
	}
	// And it should show the cycle, not just assert one exists.
	if !strings.Contains(err.Error(), "->") {
		t.Errorf("error should show the cycle path: %v", err)
	}
}

func TestRejectsSelfDependency(t *testing.T) {
	_, err := Parse([]byte("stages:\n  - {name: a, script: a.sh, after: [a]}\n"))
	if err == nil || !strings.Contains(err.Error(), "itself") {
		t.Errorf("expected a self-dependency error, got %v", err)
	}
}

func TestRejectsUnknownDependency(t *testing.T) {
	_, err := Parse([]byte("stages:\n  - {name: a, script: a.sh, after: [ghost]}\n"))
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error should name the missing stage, got %v", err)
	}
}

func TestRejectsMalformed(t *testing.T) {
	for name, in := range map[string]string{
		"no stages":     "name: x\n",
		"no stage name": "stages:\n  - {script: a.sh}\n",
		"no script":     "stages:\n  - {name: a}\n",
		"duplicate":     "stages:\n  - {name: a, script: a.sh}\n  - {name: a, script: b.sh}\n",
	} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDiamondDAGOrders(t *testing.T) {
	s, err := Parse([]byte(`
stages:
  - {name: d, script: d.sh, after: [b, c]}
  - {name: b, script: b.sh, after: [a]}
  - {name: c, script: c.sh, after: [a]}
  - {name: a, script: a.sh}
`))
	if err != nil {
		t.Fatal(err)
	}
	ordered, err := s.Order()
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, st := range ordered {
		pos[st.Name] = i
	}
	if pos["a"] > pos["b"] || pos["a"] > pos["c"] || pos["b"] > pos["d"] || pos["c"] > pos["d"] {
		t.Errorf("diamond mis-ordered: %v", names(ordered))
	}
}

func TestToSpecMapsResources(t *testing.T) {
	s, _ := Parse([]byte(good))
	byName := map[string]Stage{}
	for _, st := range s.Stages {
		byName[st.Name] = st
	}
	embed, err := byName["embed"].ToSpec(ctl.ParseMem)
	if err != nil {
		t.Fatal(err)
	}
	if embed.Limits.GPUs != 1 || embed.Limits.MemBytes != 4<<30 {
		t.Errorf("embed limits = %+v", embed.Limits)
	}
	if len(embed.StageOut) != 1 || embed.StageOut[0] != "vectors" {
		t.Errorf("outputs not mapped to stage-out: %v", embed.StageOut)
	}
	eval, err := byName["evaluate"].ToSpec(ctl.ParseMem)
	if err != nil {
		t.Fatal(err)
	}
	if eval.TotalGPUMem != 20<<30 {
		t.Errorf("aggregate GPU memory = %d", eval.TotalGPUMem)
	}
	if eval.MaxNodes != 0 {
		t.Errorf("nodes: auto should leave the solver free, got %d", eval.MaxNodes)
	}
}

func TestDependencyExprUsesAfterOK(t *testing.T) {
	ids := map[string]int64{"a": 7, "b": 9}
	got := DependencyExpr([]string{"a", "b"}, ids)
	// afterok, not afterany: a stage must not run on a failed predecessor.
	if got != "afterok:7:9" {
		t.Errorf("DependencyExpr = %q, want afterok:7:9", got)
	}
	if DependencyExpr(nil, ids) != "" {
		t.Error("no dependencies should yield an empty expression")
	}
}
