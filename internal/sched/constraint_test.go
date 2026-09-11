package sched

import "testing"

var (
	macNode = NodeFeatures{OS: "darwin", Arch: "arm64", GPUKind: "metal", Tier: "full",
		CPUs: 10, MemBytes: 16 << 30, GPUs: 1, GPUMem: 12 << 30}
	cudaNode = NodeFeatures{OS: "linux", Arch: "amd64", GPUKind: "cuda", Tier: "full",
		CPUs: 16, MemBytes: 64 << 30, GPUs: 1, GPUMem: 24 << 30}
	limitedLinux = NodeFeatures{OS: "linux", Arch: "arm64", Tier: "limited",
		CPUs: 4, MemBytes: 8 << 30}
)

func match(t *testing.T, expr string, n NodeFeatures) bool {
	t.Helper()
	c, err := ParseConstraint(expr)
	if err != nil {
		t.Fatalf("ParseConstraint(%q): %v", expr, err)
	}
	return c.Matches(n)
}

func TestFeatureTags(t *testing.T) {
	cases := []struct {
		expr string
		node NodeFeatures
		want bool
	}{
		{"metal", macNode, true},
		{"metal", cudaNode, false},
		{"cuda", cudaNode, true},
		{"apple_silicon", macNode, true},
		{"apple_silicon", limitedLinux, false},
		{"linux", cudaNode, true},
		{"arm64", limitedLinux, true},
		{"gpu", limitedLinux, false},
		{"gpu", macNode, true},
		{"limited", limitedLinux, true},
	}
	for _, c := range cases {
		if got := match(t, c.expr, c.node); got != c.want {
			t.Errorf("%q against %s/%s = %v, want %v", c.expr, c.node.OS, c.node.Arch, got, c.want)
		}
	}
}

func TestNumericComparisons(t *testing.T) {
	cases := []struct {
		expr string
		node NodeFeatures
		want bool
	}{
		{"gpu_mem>=24G", cudaNode, true},
		{"gpu_mem>=24G", macNode, false},
		{"gpu_mem>=12G", macNode, true},
		{"mem>=32G", cudaNode, true},
		{"mem>=32G", macNode, false},
		{"cpus>=8", macNode, true},
		{"cpus>=8", limitedLinux, false},
		{"gpus>0", macNode, true},
		{"gpus>0", limitedLinux, false},
	}
	for _, c := range cases {
		if got := match(t, c.expr, c.node); got != c.want {
			t.Errorf("%q against %s = %v, want %v", c.expr, c.node.OS, got, c.want)
		}
	}
}

func TestAndOr(t *testing.T) {
	// The motivating case: "a Mac GPU with at least 12 GiB, or any CUDA card
	// with at least 24" -- expressible without naming a machine.
	expr := "metal&gpu_mem>=12G|cuda&gpu_mem>=24G"
	if !match(t, expr, macNode) {
		t.Error("mac should satisfy the metal alternative")
	}
	if !match(t, expr, cudaNode) {
		t.Error("cuda box should satisfy the cuda alternative")
	}
	if match(t, expr, limitedLinux) {
		t.Error("a node with no GPU should not match")
	}
	// AND must actually require both halves.
	if match(t, "metal&gpu_mem>=24G", macNode) {
		t.Error("mac has only 12 GiB of GPU memory; the AND should fail")
	}
}

func TestArchEqualsIsAFeature(t *testing.T) {
	// arch=arm64 and a bare arm64 should mean the same thing.
	if !match(t, "arch=arm64", macNode) {
		t.Error("arch=arm64 should match an Apple Silicon node")
	}
	if match(t, "arch=amd64", macNode) {
		t.Error("arch=amd64 should not match arm64")
	}
}

func TestEmptyConstraintMatchesEverything(t *testing.T) {
	c, err := ParseConstraint("")
	if err != nil {
		t.Fatal(err)
	}
	if c != nil && !c.Matches(limitedLinux) {
		t.Error("an empty constraint should place no restriction")
	}
	var nilC *Constraint
	if !nilC.Matches(limitedLinux) {
		t.Error("a nil constraint should match")
	}
}

func TestBadConstraintsRejected(t *testing.T) {
	for _, bad := range []string{"mem>=", "mem>=abc", "cpus>=1&", "hostname>=3"} {
		if _, err := ParseConstraint(bad); err == nil {
			t.Errorf("accepted invalid constraint %q", bad)
		}
	}
}

func TestExtraTags(t *testing.T) {
	n := macNode
	n.Extra = []string{"thunderbolt5", "quiet"}
	if !match(t, "thunderbolt5", n) {
		t.Error("operator-defined tag should match")
	}
	if match(t, "thunderbolt5", macNode) {
		t.Error("node without the tag should not match")
	}
}
