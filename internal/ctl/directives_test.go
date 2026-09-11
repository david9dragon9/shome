package ctl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/davidwu/shome/internal/job"
)

func TestParseMemDefaultsToMegabytesLikeSlurm(t *testing.T) {
	cases := map[string]int64{
		"1024":  1024 << 20, // bare number is MB in Slurm, not bytes
		"4G":    4 << 30,
		"512M":  512 << 20,
		"2048K": 2048 << 10,
		"1.5G":  int64(1.5 * float64(1<<30)),
		"":      0,
	}
	for in, want := range cases {
		got, err := ParseMem(in)
		if err != nil {
			t.Errorf("ParseMem(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseMem(%q) = %d, want %d", in, got, want)
		}
	}
	if _, err := ParseMem("10X"); err == nil {
		t.Error("expected error for unknown suffix")
	}
}

func TestParseWalltimeSlurmForms(t *testing.T) {
	cases := map[string]time.Duration{
		"30":         30 * time.Minute,
		"1:30":       time.Minute + 30*time.Second,
		"02:30:00":   2*time.Hour + 30*time.Minute,
		"1-00":       24 * time.Hour,
		"1-12:30:00": 36*time.Hour + 30*time.Minute,
		"":           0,
	}
	for in, want := range cases {
		got, err := job.ParseWalltime(in)
		if err != nil {
			t.Errorf("ParseWalltime(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseWalltime(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseGres(t *testing.T) {
	for in, want := range map[string]int{"gpu": 1, "gpu:2": 2, "gpu:metal:3": 3} {
		got, err := ParseGres(in)
		if err != nil || got != want {
			t.Errorf("ParseGres(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := ParseGres("fpga:1"); err == nil {
		t.Error("expected error for unsupported gres type")
	}
}

func TestScriptDirectives(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "j.sh")
	os.WriteFile(p, []byte(`#!/bin/bash
#SBATCH --job-name=test
#SBATCH -c 4
#SHOME --mem=2G

echo hello
#SBATCH --time=99:00:00
`), 0o644)
	got, err := ScriptDirectives(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--job-name=test", "-c", "4", "--mem=2G"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	// The directive after the first real command must be ignored, or a
	// #SBATCH inside a heredoc could silently rewrite the job's limits.
	for _, g := range got {
		if g == "--time=99:00:00" {
			t.Error("directive after first command should be ignored")
		}
	}
}

func TestApplyLimit(t *testing.T) {
	var l job.Limits
	for _, kv := range [][2]string{{"c", "8"}, {"mem", "4G"}, {"time", "1:00:00"}, {"gres", "gpu:2"}} {
		if err := ApplyLimit(&l, kv[0], kv[1]); err != nil {
			t.Fatalf("%v: %v", kv, err)
		}
	}
	if l.CPUs != 8 || l.MemBytes != 4<<30 || l.Walltime != time.Hour || l.GPUs != 2 {
		t.Errorf("bad limits: %+v", l)
	}
	if err := ApplyLimit(&l, "c", "0"); err == nil {
		t.Error("zero CPUs should be rejected")
	}
}

func TestServeRejectsOverlongSocketPath(t *testing.T) {
	long := "/tmp/" + strings.Repeat("x", 120) + "/s.sock"
	_, _, err := Serve(nil, long)
	if err == nil {
		t.Fatal("expected an error for an over-long socket path")
	}
	// bind(2) reports only "invalid argument"; the message must explain the
	// actual cause and the fix.
	if !strings.Contains(err.Error(), "SHOME_ROOT") {
		t.Errorf("error should tell the user how to fix it, got: %v", err)
	}
}

func TestParseArray(t *testing.T) {
	cases := []struct {
		in    string
		tasks []int
		thr   int
	}{
		{"0-4", []int{0, 1, 2, 3, 4}, 0},
		{"1,3,5", []int{1, 3, 5}, 0},
		{"0-9:3", []int{0, 3, 6, 9}, 0},
		{"1-3,7", []int{1, 2, 3, 7}, 0},
		{"0-9%2", []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, 2},
		{"2,2,2", []int{2}, 0}, // duplicates collapse
	}
	for _, c := range cases {
		got, thr, err := ParseArray(c.in)
		if err != nil {
			t.Errorf("ParseArray(%q): %v", c.in, err)
			continue
		}
		if thr != c.thr || len(got) != len(c.tasks) {
			t.Errorf("ParseArray(%q) = %v thr=%d, want %v thr=%d", c.in, got, thr, c.tasks, c.thr)
			continue
		}
		for i := range got {
			if got[i] != c.tasks[i] {
				t.Errorf("ParseArray(%q) = %v, want %v", c.in, got, c.tasks)
				break
			}
		}
	}
	for _, bad := range []string{"5-1", "0-99999999", "abc", "0-9%0"} {
		if _, _, err := ParseArray(bad); err == nil {
			t.Errorf("ParseArray(%q) should have failed", bad)
		}
	}
}
