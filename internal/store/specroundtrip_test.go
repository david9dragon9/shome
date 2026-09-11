package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/davidwu/shome/internal/job"
)

// A job's spec is stored column by column rather than as a blob, so a field
// added to job.Spec is silently dropped until someone remembers to add it to
// the INSERT, the column list and the scan.
//
// That has now happened twice: --max-procs was computed and discarded, so the
// fork-bomb limit did nothing; and srun's interactive fields were discarded,
// so the agent never learned to relay a session. Both were found by noticing
// the feature did not work, which is an expensive way to find them.
//
// This test walks job.Spec by reflection, so a new field fails here instead.
// The point is the loop, not the values: adding a field to Spec and not to
// the store is now a failing test rather than a silent bug.
func TestEverySpecFieldSurvivesTheDatabase(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)

	// Every field set to something distinctive, so a dropped one reads as a
	// zero value rather than accidentally matching.
	in := job.Spec{
		Name:       "the-name",
		User:       "alice",
		Script:     "/path/to/script.sh",
		ScriptBody: []byte("#!/bin/sh\necho hi\n"),
		Args:       []string{"arg-one", "arg-two"},
		Env:        map[string]string{"KEY": "value"},
		Workdir:    "/some/workdir",
		Chdir:      "experiments/run-3",
		Limits: job.Limits{
			CPUs: 7, MemBytes: 12345678, GPUs: 3,
			Walltime: 91 * time.Minute, Network: true, MaxProcs: 4321,
		},
		ArrayTaskID:   -1,
		Dependency:    "afterok:11",
		TotalCPUs:     64,
		TotalMemBytes: 400 << 30,
		TotalGPUMem:   132 << 30,
		TotalGPUs:     9,
		MaxNodes:      5,
		Requeue:       true,
		NodeList:      []string{"mini", "gpu-box"},
		Constraint:    "metal&unified_mem>=32",
		Fabric:        "mlx-ring",
		Model:         "Qwen3-235B",
		StageIn:       true,
		StageOut:      []string{"out/", "result.csv"},
		Interactive:   true,
		PTY:           true,
		TTYSize:       job.Winsize{Rows: 50, Cols: 203, XPixel: 1624, YPixel: 800},
		Stream:        4242,
	}

	// Guard against the test itself going stale: if a field is left at its
	// zero value above, it cannot detect a drop.
	rv := reflect.ValueOf(in)
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if f.Name == "ArrayTaskID" {
			continue // -1 is its meaningful "not an array" value
		}
		if rv.Field(i).IsZero() {
			t.Errorf("this test does not set Spec.%s, so it cannot notice "+
				"that field being dropped by the store", f.Name)
		}
	}

	j, err := s.Submit(ctx, in, now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Compared field by field, so a failure names what was lost.
	gv := reflect.ValueOf(got.Spec)
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		want, have := rv.Field(i).Interface(), gv.Field(i).Interface()
		if !reflect.DeepEqual(want, have) {
			t.Errorf("Spec.%s did not survive: stored %#v, read back %#v\n"+
				"    Add it to the INSERT, the `cols` list and scanJob in store.go.",
				name, want, have)
		}
	}
}
