//go:build darwin

package darwin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The generated profile allows the job's own scratch but then denies the
// shared jobs root that CONTAINS it. Since SBPL is last-match-wins, this test
// exists to pin down which rule actually governs -- a job that cannot write its
// own scratch would be badly broken, and stdout-only jobs would never reveal it.
func TestJobCanWriteOwnScratchButNotOthers(t *testing.T) {
	root := t.TempDir()
	// Resolve: Seatbelt matches real paths, and t.TempDir() is under a symlink.
	root, _ = filepath.EvalSymlinks(root)
	jobs := filepath.Join(root, "jobs")
	mine := filepath.Join(jobs, "job-1")
	other := filepath.Join(jobs, "job-2")
	for _, d := range []string{mine, other} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	const secret = "other-job-private-payload"
	os.WriteFile(filepath.Join(other, "secret.txt"), []byte(secret), 0o600)

	home := t.TempDir()
	home, _ = filepath.EvalSymlinks(home)
	os.WriteFile(filepath.Join(home, "ownerfile"), []byte("owner-data"), 0o600)

	prof, err := GenerateProfile(ProfileConfig{
		ScratchDir: mine, OwnerHome: home, DenyPaths: []string{jobs},
	})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "p.sb")
	if err := os.WriteFile(p, []byte(prof), 0o600); err != nil {
		t.Fatal(err)
	}

	sb := func(args ...string) (string, bool) {
		out, err := exec.Command("sandbox-exec", append([]string{"-f", p}, args...)...).CombinedOutput()
		return string(out), err == nil
	}

	t.Run("can write its own scratch", func(t *testing.T) {
		out, ok := sb("/usr/bin/touch", filepath.Join(mine, "x.txt"))
		if !ok {
			t.Fatalf("job cannot write its own scratch dir: %s\n\nprofile:\n%s", out, prof)
		}
	})
	t.Run("cannot read another job's files", func(t *testing.T) {
		out, _ := sb("/bin/cat", filepath.Join(other, "secret.txt"))
		if strings.Contains(out, secret) {
			t.Error("job read another job's private file")
		}
	})
	t.Run("cannot write into another job's dir", func(t *testing.T) {
		sb("/usr/bin/touch", filepath.Join(other, "evil.txt"))
		if _, err := os.Stat(filepath.Join(other, "evil.txt")); err == nil {
			t.Error("job wrote into another job's directory")
		}
	})
	t.Run("cannot read the owner's home", func(t *testing.T) {
		out, _ := sb("/bin/cat", filepath.Join(home, "ownerfile"))
		if strings.Contains(out, "owner-data") {
			t.Error("job read the owner's file")
		}
	})
}
