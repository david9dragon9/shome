//go:build darwin

package main

import (
	"os"
	"os/exec"
)

// doctorPlatform runs the macOS-specific checks.
func doctorPlatform(check func(name string, status rune, detail, fix string)) {
	for _, t := range []struct{ name, path string }{
		{"sandbox-exec", "/usr/bin/sandbox-exec"},
		{"taskpolicy", "/usr/sbin/taskpolicy"},
	} {
		if _, err := os.Stat(t.path); err == nil {
			check(t.name, 'p', t.path, "")
		} else {
			check(t.name, 'f', "missing at "+t.path,
				"without it this node loses isolation or QoS control and will join at a lower tier")
		}
	}

	// Full Disk Access. Root is NOT sufficient on modern macOS: TCC binds the
	// process, so a root `rm` without FDA cannot manage another account's
	// Library. Detected by attempting a read that only FDA permits.
	if _, err := os.ReadFile("/Library/Application Support/com.apple.TCC/TCC.db"); err == nil {
		check("full disk access", 'p', "granted", "")
	} else {
		check("full disk access", 'w', "not granted to this process",
			"only needed if shome must manage per-user home directories; "+
				"System Settings > Privacy & Security > Full Disk Access")
	}

	// Metal is what makes a Mac worth scheduling; say so if it is absent.
	if out, err := exec.Command("/usr/sbin/system_profiler", "SPDisplaysDataType").Output(); err == nil && len(out) > 0 {
		check("metal gpu", 'p', "detected", "")
	} else {
		check("metal gpu", 'w', "could not query the GPU",
			"the node will still join as CPU-only")
	}
}
