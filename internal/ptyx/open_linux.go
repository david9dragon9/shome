//go:build linux

package ptyx

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// openMaster opens /dev/ptmx and returns the master together with the path of
// its slave.
func openMaster() (*os.File, string, error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open /dev/ptmx: %w", err)
	}
	fd := int(m.Fd())
	// Unlock before asking for the number: an unlocked pty is what the slave
	// side can be opened on.
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		m.Close()
		return nil, "", fmt.Errorf("unlock pty: %w", err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		m.Close()
		return nil, "", fmt.Errorf("get pty number: %w", err)
	}
	return m, fmt.Sprintf("/dev/pts/%d", n), nil
}
