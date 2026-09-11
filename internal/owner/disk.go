package owner

import "fmt"

// The owner's disk and inode caps.
//
// Enforced by admission control, the same shape that already works for cores
// and memory: a node over its cap stops accepting new work and says why. That
// is the honest mechanism, because shome cannot bound how much a job already
// running chooses to write -- and pretending otherwise is what the previous
// state of this did. `max_disk_gb` was parsed and then read by nothing, so an
// owner who set it believed in a limit that did not exist. That is worse than
// having no setting at all: it is the one place the project claimed something
// it was not doing.

// DiskState is what the node currently measures about its storage.
type DiskState struct {
	// ShomeBytes and ShomeInodes are what shome's own tree occupies -- what a
	// contribution cap is a cap on.
	ShomeBytes  int64
	ShomeInodes int64
	// FreeBytes and FreeInodes are what the filesystem has left, whoever used
	// it. What actually runs out.
	FreeBytes  int64
	FreeInodes int64
	// Known is false when the platform could not measure, in which case no
	// disk rule can fire -- a limit that cannot be measured must not be
	// enforced by guessing.
	Known bool
}

// CheckDisk reports why new work should be refused, or "" to accept it.
func (p *Policy) CheckDisk(st DiskState) string {
	if p == nil || !st.Known {
		return ""
	}
	c := p.Contribute

	if c.MaxDiskGB > 0 {
		cap := int64(c.MaxDiskGB * float64(1<<30))
		if st.ShomeBytes >= cap {
			return fmt.Sprintf("shome is using %s of its %s disk allowance",
				gib(st.ShomeBytes), gib(cap))
		}
	}
	if c.MaxInodes > 0 && st.ShomeInodes >= c.MaxInodes {
		return fmt.Sprintf("shome holds %d files, at its limit of %d",
			st.ShomeInodes, c.MaxInodes)
	}
	if c.MinFreeDiskGB > 0 {
		floor := int64(c.MinFreeDiskGB * float64(1<<30))
		if st.FreeBytes >= 0 && st.FreeBytes < floor {
			return fmt.Sprintf("only %s free on this disk; the owner asked to keep %s",
				gib(st.FreeBytes), gib(floor))
		}
	}
	// A filesystem out of inodes cannot create a scratch directory, so there
	// is nothing useful to accept even without the owner saying so.
	if st.FreeInodes >= 0 && st.FreeInodes < 1000 && st.FreeInodes > 0 {
		return fmt.Sprintf("this filesystem has only %d inodes left", st.FreeInodes)
	}
	return ""
}

// DescribeDisk renders the disk position for `shome status`.
func (p *Policy) DescribeDisk(st DiskState) string {
	if !st.Known {
		return "disk: not measured on this platform"
	}
	out := fmt.Sprintf("disk: shome is using %s", gib(st.ShomeBytes))
	if p != nil && p.Contribute.MaxDiskGB > 0 {
		out += fmt.Sprintf(" of %s allowed", gib(int64(p.Contribute.MaxDiskGB*float64(1<<30))))
	}
	out += fmt.Sprintf("; %s free on the volume", gib(st.FreeBytes))
	if p != nil && p.Contribute.MinFreeDiskGB > 0 {
		out += fmt.Sprintf(" (keeping %s)", gib(int64(p.Contribute.MinFreeDiskGB*float64(1<<30))))
	}
	return out
}

func gib(b int64) string {
	if b < 0 {
		return "unknown"
	}
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
