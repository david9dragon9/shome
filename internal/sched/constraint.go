package sched

import (
	"fmt"
	"strconv"
	"strings"
)

// Constraint matches nodes by what they can do rather than what they are
// called.
//
// A heterogeneous cluster makes node names useless as a targeting mechanism --
// "run this on the machine with 24 GiB of CUDA memory" has to be expressible
// without knowing which box that is today. Slurm's --constraint is a flat set
// of feature tags; shome extends it with numeric comparisons because
// accelerator memory is the dimension that actually decides placement.
//
// Grammar (deliberately small):
//
//	expr    := term ('|' term)*        // alternatives
//	term    := factor ('&' factor)*    // all must hold
//	factor  := feature | comparison
//	feature := ident                   // metal, cuda, linux, darwin, arm64 ...
//	compare := ident op value          // mem>=32G, gpu_mem>=24G, cpus>=8
//	op      := '>=' | '<=' | '>' | '<' | '='
type Constraint struct {
	raw  string
	alts [][]predicate // OR of ANDs
}

type predicate struct {
	key string
	op  string
	val int64
	// feature is true for a bare tag with no comparison.
	feature bool
}

// ParseConstraint compiles a constraint expression.
func ParseConstraint(s string) (*Constraint, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	c := &Constraint{raw: s}
	for _, alt := range strings.Split(s, "|") {
		var group []predicate
		for _, f := range strings.Split(alt, "&") {
			p, err := parsePredicate(strings.TrimSpace(f))
			if err != nil {
				return nil, err
			}
			group = append(group, p)
		}
		if len(group) == 0 {
			return nil, fmt.Errorf("empty alternative in constraint %q", s)
		}
		c.alts = append(c.alts, group)
	}
	return c, nil
}

var ops = []string{">=", "<=", ">", "<", "="}

func parsePredicate(s string) (predicate, error) {
	if s == "" {
		return predicate{}, fmt.Errorf("empty term in constraint")
	}
	for _, op := range ops {
		i := strings.Index(s, op)
		if i <= 0 {
			continue
		}
		key := strings.TrimSpace(s[:i])
		rest := strings.TrimSpace(s[i+len(op):])
		if !numericKey(key) {
			// A string comparison like arch=arm64 becomes a feature tag, so
			// "arch=arm64" and a bare "arm64" mean the same thing.
			if op == "=" {
				return predicate{key: strings.ToLower(rest), feature: true}, nil
			}
			return predicate{}, fmt.Errorf("constraint %q: %q is not a numeric attribute", s, key)
		}
		v, err := parseSize(rest)
		if err != nil {
			return predicate{}, fmt.Errorf("constraint %q: %w", s, err)
		}
		return predicate{key: key, op: op, val: v}, nil
	}
	return predicate{key: strings.ToLower(s), feature: true}, nil
}

func numericKey(k string) bool {
	switch strings.ToLower(k) {
	case "mem", "memory", "gpu_mem", "gpumem", "cpus", "gpus":
		return true
	}
	return false
}

// parseSize accepts plain integers and K/M/G/T suffixes.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("missing value")
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'K', 'k':
		mult, s = 1<<10, s[:len(s)-1]
	case 'M', 'm':
		mult, s = 1<<20, s[:len(s)-1]
	case 'G', 'g':
		mult, s = 1<<30, s[:len(s)-1]
	case 'T', 't':
		mult, s = 1<<40, s[:len(s)-1]
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("bad value %q", s)
	}
	return int64(v * float64(mult)), nil
}

// NodeFeatures is what a node offers a constraint to match against.
type NodeFeatures struct {
	OS       string
	Arch     string
	GPUKind  string
	Tier     string
	CPUs     int
	MemBytes int64
	GPUs     int
	GPUMem   int64
	Extra    []string // operator-defined tags
}

// Matches reports whether the node satisfies the constraint.
func (c *Constraint) Matches(n NodeFeatures) bool {
	if c == nil {
		return true
	}
	for _, group := range c.alts {
		all := true
		for _, p := range group {
			if !p.matches(n) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func (p predicate) matches(n NodeFeatures) bool {
	if p.feature {
		for _, tag := range n.tags() {
			if tag == p.key {
				return true
			}
		}
		return false
	}
	var have int64
	switch strings.ToLower(p.key) {
	case "mem", "memory":
		have = n.MemBytes
	case "gpu_mem", "gpumem":
		have = n.GPUMem
	case "cpus":
		have = int64(n.CPUs)
	case "gpus":
		have = int64(n.GPUs)
	}
	switch p.op {
	case ">=":
		return have >= p.val
	case "<=":
		return have <= p.val
	case ">":
		return have > p.val
	case "<":
		return have < p.val
	case "=":
		return have == p.val
	}
	return false
}

func (n NodeFeatures) tags() []string {
	out := []string{
		strings.ToLower(n.OS),
		strings.ToLower(n.Arch),
		strings.ToLower(n.Tier),
	}
	if n.GPUKind != "" {
		out = append(out, strings.ToLower(n.GPUKind))
	}
	if n.GPUs > 0 {
		out = append(out, "gpu")
	}
	// "apple_silicon" is worth a tag of its own: it is the distinction people
	// actually reason about, and darwin+arm64 is clumsy to write.
	if strings.EqualFold(n.OS, "darwin") && strings.EqualFold(n.Arch, "arm64") {
		out = append(out, "apple_silicon")
	}
	for _, e := range n.Extra {
		out = append(out, strings.ToLower(e))
	}
	return out
}

func (c *Constraint) String() string {
	if c == nil {
		return ""
	}
	return c.raw
}
