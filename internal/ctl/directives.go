package ctl

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/davidwu/shome/internal/job"
)

// ParseMem accepts Slurm's --mem forms: a bare number is MEGABYTES (Slurm's
// default unit, which surprises people), with K/M/G/T suffixes also allowed.
func ParseMem(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	mult := int64(1 << 20) // bare number => MiB, matching Slurm
	last := s[len(s)-1]
	if last < '0' || last > '9' {
		switch last {
		case 'K', 'k':
			mult = 1 << 10
		case 'M', 'm':
			mult = 1 << 20
		case 'G', 'g':
			mult = 1 << 30
		case 'T', 't':
			mult = 1 << 40
		default:
			return 0, fmt.Errorf("unknown memory suffix %q", string(last))
		}
		s = s[:len(s)-1]
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("bad memory value %q", s)
	}
	if v < 0 {
		return 0, fmt.Errorf("memory must not be negative")
	}
	return int64(v * float64(mult)), nil
}

// ParseGres handles --gres=gpu:2 and the bare --gres=gpu form.
func ParseGres(s string) (gpus int, err error) {
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		f := strings.Split(part, ":")
		if f[0] != "gpu" {
			return 0, fmt.Errorf("unsupported gres %q (only gpu is supported)", f[0])
		}
		n := 1
		if len(f) > 1 {
			if n, err = strconv.Atoi(f[len(f)-1]); err != nil {
				return 0, fmt.Errorf("bad gres count in %q", part)
			}
		}
		gpus += n
	}
	return gpus, nil
}

// ScriptDirectives extracts leading #SBATCH / #SHOME lines from a batch script.
//
// Only the contiguous comment header is scanned, matching Slurm: directives
// after the first real command are ignored, so a #SBATCH inside a heredoc later
// in the script cannot silently change the job's limits.
func ScriptDirectives(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readDirectives(f)
}

// ParseDirectives is ScriptDirectives for a script already in memory, which is
// how one arrives from a login node: the submitter has no filesystem in common
// with the cluster, so the script comes over the wire rather than as a path.
func ParseDirectives(body []byte) []string {
	out, _ := readDirectives(bytes.NewReader(body))
	return out
}

func readDirectives(r io.Reader) ([]string, error) {
	var out []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#!") {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			break // first non-comment line ends the directive block
		}
		up := strings.ToUpper(line)
		for _, tag := range []string{"#SBATCH", "#SHOME"} {
			if strings.HasPrefix(up, tag) {
				rest := strings.TrimSpace(line[len(tag):])
				if rest != "" {
					out = append(out, strings.Fields(rest)...)
				}
			}
		}
	}
	return out, sc.Err()
}

// ApplyLimit sets one limit from a flag name and value.
func ApplyLimit(l *job.Limits, name, val string) error {
	switch name {
	case "c", "cpus-per-task", "cpus":
		n, err := strconv.Atoi(val)
		if err != nil || n < 1 {
			return fmt.Errorf("--%s needs a positive integer, got %q", name, val)
		}
		l.CPUs = n
	case "mem":
		b, err := ParseMem(val)
		if err != nil {
			return err
		}
		l.MemBytes = b
	case "t", "time":
		d, err := job.ParseWalltime(val)
		if err != nil {
			return err
		}
		l.Walltime = d
	case "gres":
		g, err := ParseGres(val)
		if err != nil {
			return err
		}
		l.GPUs = g
	case "gpus", "gpus-per-node":
		n, err := strconv.Atoi(val)
		if err != nil || n < 0 {
			return fmt.Errorf("--%s needs a non-negative integer", name)
		}
		l.GPUs = n
	default:
		return fmt.Errorf("unsupported option --%s", name)
	}
	return nil
}

// ParseArray expands a Slurm --array specification into task indices.
//
// Accepts: "0-9", "1,3,5", "0-9:2" (step), and combinations. A trailing "%N"
// throttle is parsed and returned separately.
func ParseArray(s string) (tasks []int, throttle int, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, 0, nil
	}
	if i := strings.Index(s, "%"); i >= 0 {
		t, e := strconv.Atoi(strings.TrimSpace(s[i+1:]))
		if e != nil || t < 1 {
			return nil, 0, fmt.Errorf("bad array throttle in %q", s)
		}
		throttle = t
		s = s[:i]
	}
	seen := map[int]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		step := 1
		if i := strings.Index(part, ":"); i >= 0 {
			step, err = strconv.Atoi(part[i+1:])
			if err != nil || step < 1 {
				return nil, 0, fmt.Errorf("bad array step in %q", part)
			}
			part = part[:i]
		}
		lo, hi := 0, 0
		if i := strings.Index(part, "-"); i >= 0 {
			if lo, err = strconv.Atoi(part[:i]); err != nil {
				return nil, 0, fmt.Errorf("bad array range %q", part)
			}
			if hi, err = strconv.Atoi(part[i+1:]); err != nil {
				return nil, 0, fmt.Errorf("bad array range %q", part)
			}
		} else {
			if lo, err = strconv.Atoi(part); err != nil {
				return nil, 0, fmt.Errorf("bad array index %q", part)
			}
			hi = lo
		}
		if hi < lo {
			return nil, 0, fmt.Errorf("array range %q counts down", part)
		}
		// Guard against --array=0-100000000 turning into an OOM at submit.
		if (hi-lo)/step > maxArrayTasks {
			return nil, 0, fmt.Errorf("array %q expands past the %d-task limit", part, maxArrayTasks)
		}
		for i := lo; i <= hi; i += step {
			if !seen[i] {
				seen[i] = true
				tasks = append(tasks, i)
			}
		}
	}
	sort.Ints(tasks)
	if len(tasks) > maxArrayTasks {
		return nil, 0, fmt.Errorf("array expands to %d tasks, over the %d limit", len(tasks), maxArrayTasks)
	}
	return tasks, throttle, nil
}

// maxArrayTasks bounds a single submission. A home cluster has a handful of
// nodes; a million-task array is a typo, not a workload.
const maxArrayTasks = 10000
