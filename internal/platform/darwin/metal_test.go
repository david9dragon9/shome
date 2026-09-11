//go:build darwin && cgo

package darwin

import "testing"

func TestDetectMetal(t *testing.T) {
	info, ok := DetectMetal()
	if !ok {
		t.Skip("no Metal device on this machine (valid: it is then a CPU-only node)")
	}
	t.Logf("GPU: %s, working set %d MiB, max buffer %d MiB, unified=%v",
		info.Name, info.RecommendedWorkingSet>>20, info.MaxBufferLength>>20, info.UnifiedMemory)

	if info.Name == "" {
		t.Error("device name is empty")
	}
	if info.RecommendedWorkingSet <= 0 {
		t.Error("recommended working set must be positive; it is the scheduling dimension")
	}
	// The whole point of using this value: on Apple Silicon it is materially
	// below installed RAM, so scheduling on installed RAM would over-commit.
	if info.UnifiedMemory {
		total := sysctlInt("hw.memsize")
		if info.RecommendedWorkingSet >= total {
			t.Errorf("working set %d >= installed %d; expected a GPU budget below total RAM",
				info.RecommendedWorkingSet, total)
		}
	}
}
