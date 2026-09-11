package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/davidwu/shome/internal/userenv"
)

// Every Slurm name shome installs a shim for must actually be dispatched.
//
// This is the invariant that was briefly broken: `sshare` was added to the
// CLI's own list and to the shim installer but not to the copy in
// internal/userenv, so it worked on a workstation and was "command not
// found" inside a login session. There is one list now, and this checks that
// every name in it reaches a real command.
func TestEveryShimNameDispatches(t *testing.T) {
	if len(userenv.ShimNames) == 0 {
		t.Fatal("no shim names at all")
	}
	dir := t.TempDir()
	for _, name := range userenv.ShimNames {
		// dispatch() reads os.Args[0], which is how a shim symlink tells
		// shome which command it was invoked as.
		saved := os.Args
		os.Args = []string{filepath.Join(dir, name)}
		got, _ := dispatch()
		os.Args = saved
		if got != name {
			t.Errorf("invoked as %q, dispatch() returned %q -- the shim would "+
				"not run the command it is named after", name, got)
		}
	}
}
