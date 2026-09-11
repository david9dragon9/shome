package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/davidwu/shome/internal/platform"
)

// A job runs where a login session starts: the account's storage. It used to
// run in its scratch directory, which is deleted when the job ends -- so a
// script that wrote ./results.csv, which is most scripts, produced a file
// nobody could ever find.
func TestAJobRunsInTheAccountsStorage(t *testing.T) {
	home := t.TempDir()
	dir, in, err := jobWorkDir(home, "/home/alice@mini", "")
	if err != nil {
		t.Fatal(err)
	}
	if dir != home {
		t.Errorf("a job runs in %q, want the account's storage %q", dir, home)
	}
	if in != "/home/alice@mini" {
		t.Errorf("inside, the job runs in %q, want the mapped storage", in)
	}
}

// --chdir picks a subdirectory, and creates it: a job that fails because a
// directory it named does not exist yet is a job that fails for no reason.
func TestChdirPicksASubdirectoryAndCreatesIt(t *testing.T) {
	home := t.TempDir()
	dir, in, err := jobWorkDir(home, "/home/alice@mini", "runs/2026-09")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "runs", "2026-09"); dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
	// The path inside is built with forward slashes whatever the host uses,
	// because it names a place in a Linux container.
	if in != "/home/alice@mini/runs/2026-09" {
		t.Errorf("inside = %q", in)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Errorf("the working directory was not created: %v", err)
	}
}

// The value comes from whoever submitted the job, and the working directory
// is writable -- so a .. in it would be a way to write outside the one tree
// the account is allowed to write to.
func TestChdirCannotClimbOutOfTheAccountsStorage(t *testing.T) {
	home := t.TempDir()
	for _, bad := range []string{
		"..", "../elsewhere", "a/../../elsewhere", "../../../../etc",
	} {
		dir, _, err := jobWorkDir(home, "/home/alice@mini", bad)
		if err == nil {
			t.Errorf("--chdir %q was accepted, giving the job %q", bad, dir)
		}
	}
	// One that only looks like it climbs out is fine: it stays inside.
	dir, _, err := jobWorkDir(home, "/home/alice@mini", "a/../b")
	if err != nil {
		t.Fatalf("--chdir a/../b was refused: %v", err)
	}
	if want := filepath.Join(home, "b"); dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
}

// Output lands beside the job's other files, named the way Slurm names it,
// so `ls` after a job finishes shows it where a person would look.
func TestOutputGoesToTheWorkingDirectory(t *testing.T) {
	sb := &platform.Sandbox{JobID: 84, ScratchDir: "/scratch/job-84",
		WorkDir: "/storage/alice"}
	if got := JobOutputPath(sb); got != "/storage/alice/slurm-84.out" {
		t.Errorf("output at %q, want it in the account's storage", got)
	}
	// A job with no account has only its scratch, and must still capture
	// output somewhere rather than failing to start.
	bare := &platform.Sandbox{JobID: 9, ScratchDir: "/scratch/job-9"}
	if got := JobOutputPath(bare); !strings.HasPrefix(got, "/scratch/job-9/") {
		t.Errorf("a job with no storage writes output to %q", got)
	}
}
