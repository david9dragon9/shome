//go:build darwin

package darwin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/davidwu/shome/internal/maccontainer"
	"github.com/davidwu/shome/internal/platform"
)

// The kernel's own record of an OOM kill, read back.
//
// This is the case a job's exit status cannot express: the script
// backgrounded its work, the kernel killed the child for memory, and `wait`
// with no arguments returned zero -- so the job "succeeded". The cgroup
// counted the kill regardless, and that is what this reads.
func TestCgroupNotesReportAnOOMTheExitStatusHid(t *testing.T) {
	got, ok := parseCgroupNotes("oom_kill 1\npeak 276824064\n")
	if !ok {
		t.Fatal("well-formed notes were not read")
	}
	if got.OOMKills != 1 {
		t.Errorf("OOMKills = %d, want 1", got.OOMKills)
	}
	if got.PeakBytes != 276824064 {
		t.Errorf("PeakBytes = %d, want 276824064", got.PeakBytes)
	}

	// A job that behaved: the numbers are there and say nothing happened.
	quiet, ok := parseCgroupNotes("oom_kill 0\npeak 6905856\n")
	if !ok || quiet.OOMKills != 0 || quiet.PeakBytes != 6905856 {
		t.Errorf("parseCgroupNotes(quiet) = %+v, %v", quiet, ok)
	}

	// Nothing usable must report nothing rather than zeroes, so the caller
	// falls back to the exit status instead of believing a job used no
	// memory and was never killed.
	for _, s := range []string{"", "\n", "garbage", "oom_kill\npeak\n", "oom_kill x\npeak y\n"} {
		if _, ok := parseCgroupNotes(s); ok {
			t.Errorf("parseCgroupNotes(%q) claimed to have an answer", s)
		}
	}
}

// The runner records into the job's scratch, which outlives the container.
func TestAftermathReadsWhatTheRunnerLeft(t *testing.T) {
	scratch := t.TempDir()
	b := &Backend{}
	sb := &platform.Sandbox{JobID: 7, ScratchDir: scratch}

	// Before the job has run there is nothing to say.
	if _, ok := b.Aftermath(t.Context(), sb); ok {
		t.Error("an empty scratch produced an answer")
	}
	if _, err := writeContainerRunner(scratch); err != nil {
		t.Fatal(err)
	}
	// The runner is a shell script the container runs, so it must be
	// executable and must pass the job's exit status through unchanged.
	fi, err := os.Stat(filepath.Join(scratch, containerRunnerName))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("the runner is not executable (mode %v)", fi.Mode())
	}
	if !strings.Contains(containerRunnerScript, "exit $rc") {
		t.Error("the runner does not pass the job's exit status through")
	}

	if err := os.WriteFile(filepath.Join(scratch, cgroupNotesFile),
		[]byte("oom_kill 2\npeak 1048576\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok := b.Aftermath(t.Context(), sb)
	if !ok || got.OOMKills != 2 || got.PeakBytes != 1<<20 {
		t.Errorf("Aftermath = %+v, %v", got, ok)
	}
}

// The runner really does run the job and record the numbers.
//
// Run here as a plain shell script rather than in a container: the script is
// the part that has to be right, and a container needs a runtime this test
// cannot assume.
func TestContainerRunnerRunsTheJobAndRecords(t *testing.T) {
	scratch := t.TempDir()
	if _, err := writeContainerRunner(scratch); err != nil {
		t.Fatal(err)
	}
	runner := filepath.Join(scratch, containerRunnerName)
	cmd := exec.Command("/bin/sh", runner, "/bin/sh", "-c", "echo ran; exit 5")
	cmd.Dir = scratch
	cmd.Env = append(os.Environ(), "SHOME_SCRATCH="+scratch)
	b, err := cmd.CombinedOutput()
	out := string(b)
	if err == nil {
		t.Error("the job's non-zero exit status was swallowed")
	}
	if !strings.Contains(out, "ran") {
		t.Errorf("the job did not run: %q", out)
	}
	// The cgroup files do not exist on a Mac, so the notes say zero rather
	// than nothing -- the runner must not fail the job over that.
	notes, err := os.ReadFile(filepath.Join(scratch, cgroupNotesFile))
	if err != nil {
		t.Fatalf("the runner left no notes: %v", err)
	}
	if !strings.Contains(string(notes), "oom_kill 0") {
		t.Errorf("notes = %q", notes)
	}
}

// Two shome installations on one machine share the container runtime's
// namespace, and neither may stop the other's work.
//
// A controller restarting in one state directory swept whatever it found,
// which stopped a job a second installation was running: the job died with
// exit 137 and nothing said why. Names now carry the installation, and the
// sweep matches on it.
func TestContainerNamesDistinguishInstallations(t *testing.T) {
	a := maccontainer.InstallTag("/tmp/shome-one")
	b := maccontainer.InstallTag("/tmp/shome-two")
	if a == b {
		t.Fatalf("two state directories produced the same tag %q", a)
	}
	if a != maccontainer.InstallTag("/tmp/shome-one/") {
		t.Error("the same directory, spelled differently, produced different tags")
	}
	mine := containerName(a, 7)
	theirs := containerName(b, 7)
	if mine == theirs {
		t.Fatal("the same job id in two installations produced the same name")
	}
	prefix := ContainerPrefix + a + "-"
	if id, ok := jobIDFromContainer(mine, prefix); !ok || id != 7 {
		t.Errorf("jobIDFromContainer(%q) = %d, %v; want 7", mine, id, ok)
	}
	// The other installation's name must not parse as ours, or the sweep
	// would match it to one of our live job ids and either spare or stop it
	// for the wrong reason.
	if _, ok := jobIDFromContainer(theirs, prefix); ok {
		t.Errorf("another installation's container %q parsed as ours", theirs)
	}
}

// A container from before names carried a tag is still swept: whatever made
// it is gone, and it is holding a virtual machine's memory.
func TestUntaggedContainersAreStillReclaimed(t *testing.T) {
	tag := maccontainer.InstallTag("/tmp/shome-one")
	cases := []struct {
		name   string
		tagged bool
	}{
		{containerName(tag, 3), true},
		{SessionPrefix + tag + "-login-alice-1", true},
		{ContainerPrefix + "12-9fbc1a2d", false},    // the old job format
		{SessionPrefix + "login-alice-1757", false}, // the old session format
		{ContainerPrefix + "abcdefg-1-x", false},    // seven chars: not a tag
		{ContainerPrefix + "abcde-1-x", false},      // five chars: not a tag
		{ContainerPrefix + "abcdez-1-x", false},     // not hex
	}
	for _, c := range cases {
		if got := hasInstallTag(c.name); got != c.tagged {
			t.Errorf("hasInstallTag(%q) = %v, want %v", c.name, got, c.tagged)
		}
	}
}
