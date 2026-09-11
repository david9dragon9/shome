package ctl

import (
	"fmt"
	"strconv"
	"strings"
)

// DepType is a Slurm dependency kind.
type DepType string

const (
	DepAfterOK    DepType = "afterok"    // dependency must have COMPLETED
	DepAfterAny   DepType = "afterany"   // dependency must have finished, any outcome
	DepAfterNotOK DepType = "afternotok" // dependency must have failed
)

// Dep is one parsed dependency edge.
type Dep struct {
	Type  DepType
	JobID int64
}

// ParseDependency parses Slurm's --dependency syntax, e.g.
// "afterok:12:13,afterany:14".
//
// Only the "after*" family is supported. Unsupported kinds are rejected at
// parse time rather than silently ignored -- a dependency that is quietly
// dropped would run a job before its input exists.
func ParseDependency(s string) ([]Dep, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []Dep
	for _, clause := range strings.Split(s, ",") {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}
		parts := strings.Split(clause, ":")
		if len(parts) < 2 {
			return nil, fmt.Errorf("dependency %q needs the form type:jobid", clause)
		}
		var t DepType
		switch strings.ToLower(parts[0]) {
		case "afterok":
			t = DepAfterOK
		case "afterany":
			t = DepAfterAny
		case "afternotok":
			t = DepAfterNotOK
		default:
			return nil, fmt.Errorf("unsupported dependency type %q (supported: afterok, afterany, afternotok)", parts[0])
		}
		for _, idStr := range parts[1:] {
			id, err := strconv.ParseInt(strings.TrimSpace(idStr), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("bad job id %q in dependency", idStr)
			}
			out = append(out, Dep{Type: t, JobID: id})
		}
	}
	return out, nil
}
