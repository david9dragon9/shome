//go:build unix

package platform

import "golang.org/x/sys/unix"

// statfs asks the kernel what the filesystem has left.
//
// Blocks are multiplied by the filesystem's own block size rather than an
// assumed 4 KiB: APFS and ext4 disagree, and getting it wrong misreports free
// space by whole factors.
func statfs(path string) (FSStat, error) {
	var s unix.Statfs_t
	if err := unix.Statfs(path, &s); err != nil {
		return FSStat{}, err
	}
	bs := int64(s.Bsize)
	return FSStat{
		TotalBytes: int64(s.Blocks) * bs,
		// Bavail, not Bfree: the difference is the reserve only root may use,
		// and reporting it as available would promise space a job cannot have.
		FreeBytes:   int64(s.Bavail) * bs,
		TotalInodes: int64(s.Files),
		FreeInodes:  int64(s.Ffree),
	}, nil
}
