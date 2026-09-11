package linux

import (
	"fmt"
	"strings"
	"testing"

	"github.com/davidwu/shome/internal/platform"
)

func joined(args []string) string { return strings.Join(args, " ") }

// boundAs reports where a host path appears inside, or "" if it is absent.
//
// bubblewrap builds a fresh mount namespace, so a path that is not bound does
// not exist inside it -- which is why reading the argv is a real check and
// not a proxy for one.
func boundAs(args []string, host string) string {
	for i, a := range args {
		switch a {
		case "--bind", "--ro-bind", "--bind-try", "--ro-bind-try", "--dev-bind-try":
			if i+2 < len(args) && args[i+1] == host {
				return args[i+2]
			}
		}
	}
	return ""
}

func masked(args []string, path string) bool {
	for i, a := range args {
		if a == "--tmpfs" && i+1 < len(args) && args[i+1] == path {
			return true
		}
	}
	return false
}

func sessionArgs(host, user, machine string) []string {
	return SandboxArgv(SandboxOpts{
		BwrapPath: "/usr/bin/bwrap",
		Mounts: []Mount{
			{Host: host, In: HomePath(user, machine), Write: true, Required: true},
			{Host: "/srv/shome/env/bin", In: ToolsPath},
		},
		Chdir: HomePath(user, machine),
		Home:  HomePath(user, machine),
		// A machine that has every path worth hiding, so the policy is
		// what is under test rather than this machine's /etc.
		Masks: MaskPaths(func(string) bool { return true }),
	}, []string{"/bin/bash", "-i"})
}

// The property a user asked for: from inside, the account's own folder is the
// only place, and nothing names where it really lives.
func TestHostPathsAreNotVisibleInside(t *testing.T) {
	const host = "/Users/Shared/shome-dave/users/alice"
	args := sessionArgs(host, "alice", "mini")

	where := boundAs(args, host)
	if where != "/home/alice" {
		t.Fatalf("the account's directory appears at %q, want /home/alice", where)
	}
	// Nothing in the command line tells the session where its files live, or
	// anything about the installation -- except as the source of a bind,
	// which the session cannot read back.
	inside := []string{}
	for i, a := range args {
		if a == "--chdir" || a == "--setenv" {
			if a == "--setenv" && i+2 < len(args) {
				inside = append(inside, args[i+2])
			} else if i+1 < len(args) {
				inside = append(inside, args[i+1])
			}
		}
	}
	for _, v := range inside {
		if strings.Contains(v, "/Users/Shared") || strings.Contains(v, "shome-dave") {
			t.Errorf("a host path leaks into the session: %q", v)
		}
	}
}

// The machine's name is in the folder name on purpose: a cluster has no shared
// filesystem, so the same account's files differ per machine.
func TestHomePathIsTheSameOnEveryMachine(t *testing.T) {
	// Deliberate: an account's installed software records this path, so a
	// name that differed per machine broke every environment that was
	// copied to another one. Which machine you are on is said by the
	// prompt and by $SHOME_HOST instead.
	if got := HomePath("alice", "mini"); got != "/home/alice" {
		t.Errorf("HomePath = %q, want /home/alice", got)
	}
	if HomePath("alice", "mini") != HomePath("alice", "gpu-box") {
		t.Error("the same account's directory must have the same name on every machine")
	}
	if got := HomePath("alice", ""); got != "/home/alice" {
		t.Errorf("HomePath with no machine = %q", got)
	}
}

// Nothing that belongs to a person is inside the namespace.
func TestNothingPersonalIsBound(t *testing.T) {
	args := sessionArgs("/srv/shome/users/alice", "alice", "mini")
	for _, p := range []string{
		"/home", "/root", "/opt", "/srv", "/mnt", "/media", "/var", "/tmp",
		"/Users", "/Applications",
	} {
		if boundAs(args, p) != "" {
			t.Errorf("%s is bound into the sandbox; it holds user data", p)
		}
	}
	if boundAs(args, "/usr") == "" {
		t.Error("/usr is not bound, so nothing would run")
	}
	if !masked(args, "/usr/local") {
		t.Error("/usr/local is not masked; it names what the owner installed")
	}
	for _, p := range []string{"/etc/ssh", "/etc/NetworkManager", "/etc/wpa_supplicant"} {
		if !masked(args, p) {
			t.Errorf("%s is not masked; it holds credentials", p)
		}
	}
}

// One writable directory, and the session starts in it.
func TestOnlyTheAccountDirectoryIsWritable(t *testing.T) {
	args := sessionArgs("/srv/shome/users/alice", "alice", "mini")
	var writable []string
	for i, a := range args {
		if (a == "--bind" || a == "--bind-try") && i+2 < len(args) {
			writable = append(writable, args[i+2])
		}
	}
	if len(writable) != 1 || writable[0] != "/home/alice" {
		t.Errorf("writable set = %v, want only the account's directory", writable)
	}
	j := joined(args)
	if !strings.Contains(j, "--chdir /home/alice") {
		t.Error("the session does not start in its own directory")
	}
	if !strings.Contains(j, "--setenv HOME /home/alice") {
		t.Error("HOME is not the mapped path")
	}
}

// A job's scratch appears at a fixed, uninformative path.
func TestJobScratchIsMappedToANeutralPath(t *testing.T) {
	args := SandboxArgv(SandboxOpts{
		BwrapPath: "bwrap",
		Mounts:    []Mount{{Host: "/srv/shome/local/jobs/job-7", In: JobScratchPath, Write: true, Required: true}},
		Chdir:     JobScratchPath, Home: JobScratchPath,
	}, []string{"/bin/sh", "job-script"})
	if got := boundAs(args, "/srv/shome/local/jobs/job-7"); got != "/scratch" {
		t.Errorf("scratch appears at %q, want /scratch", got)
	}
	if strings.Contains(joined(args), "--chdir /srv") {
		t.Error("the job starts in a host path")
	}
}

// A mount with no In keeps its own path, which is what the read-only system
// trees want.
func TestMountWhereDefaultsToTheHostPath(t *testing.T) {
	m := Mount{Host: "/usr/share/misc"}
	if m.Where() != "/usr/share/misc" {
		t.Errorf("Where() = %q", m.Where())
	}
	if (Mount{Host: "/a", In: "/b"}).Where() != "/b" {
		t.Error("In was ignored")
	}
}

func TestNetworkIsOffByDefault(t *testing.T) {
	off := SandboxArgv(SandboxOpts{BwrapPath: "bwrap"}, []string{"/bin/sh"})
	if !strings.Contains(joined(off), "--unshare-net") {
		t.Error("network was not unshared by default")
	}
	on := SandboxArgv(SandboxOpts{BwrapPath: "bwrap", Network: true}, []string{"/bin/sh"})
	if strings.Contains(joined(on), "--unshare-net") {
		t.Error("network was unshared despite being requested")
	}
}

func TestGPUDevicesOnlyWhenRequested(t *testing.T) {
	off := joined(SandboxArgv(SandboxOpts{BwrapPath: "bwrap"}, []string{"/bin/sh"}))
	if strings.Contains(off, "nvidia") {
		t.Error("NVIDIA devices bound for a job that asked for no GPU")
	}
	on := joined(SandboxArgv(SandboxOpts{BwrapPath: "bwrap", GPU: true}, []string{"/bin/sh"}))
	if !strings.Contains(on, "/dev/nvidia0") {
		t.Error("NVIDIA devices not bound for a GPU job")
	}
}

func TestInnerCommandComesLast(t *testing.T) {
	args := SandboxArgv(SandboxOpts{BwrapPath: "bwrap"}, []string{"/bin/sh", "-c", "echo hi"})
	if got := args[len(args)-3:]; got[0] != "/bin/sh" || got[2] != "echo hi" {
		t.Errorf("argv does not end with the command: %v", got)
	}
	if args[0] != "bwrap" {
		t.Errorf("argv[0] = %q, want the bwrap binary", args[0])
	}
}

// An empty host path must not become a bind of "".
func TestEmptyMountsAreSkipped(t *testing.T) {
	args := SandboxArgv(SandboxOpts{BwrapPath: "bwrap",
		Mounts: []Mount{{Host: ""}, {Host: "/real"}}}, []string{"/bin/sh"})
	j := joined(args)
	if strings.Contains(j, "--ro-bind-try  ") {
		t.Errorf("an empty path became a bind:\n%s", j)
	}
	if boundAs(args, "/real") == "" {
		t.Error("the real mount was dropped")
	}
}

// `srun bash` on a Linux node used to fail at launch: the backend demanded a
// script for every job, tried to open a file called "bash", and reported
// "read job script bash: no such file or directory". An argument vector is
// the command -- there is nothing to place.
func TestJobArgvRunsAnArgumentVectorWithoutAScript(t *testing.T) {
	materialised := false
	got, err := JobArgv([]string{"bash"}, func() (string, error) {
		materialised = true
		return "", fmt.Errorf("read job script bash: no such file or directory")
	})
	if err != nil {
		t.Fatalf("an argument vector was refused: %v", err)
	}
	if materialised {
		t.Error("a script was written for a job that is not one")
	}
	if len(got) != 1 || got[0] != "bash" {
		t.Errorf("runs %v, want [bash]", got)
	}
}

// A submitted script is still placed in the job's scratch and run from
// there, which is the sbatch path and the reason materialising exists.
func TestJobArgvRunsTheScriptWhenThereIsOne(t *testing.T) {
	got, err := JobArgv(nil, func() (string, error) { return "/scratch/job-script", nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "/scratch/job-script" {
		t.Errorf("runs %v, want the placed script", got)
	}
}

// A job that is neither still fails, and says so.
func TestJobArgvReportsAJobThatIsNeither(t *testing.T) {
	if _, err := JobArgv(nil, func() (string, error) {
		return "", fmt.Errorf("job has neither a script body nor a path")
	}); err == nil {
		t.Fatal("a job with no script and no command was accepted")
	}
}

// bubblewrap cannot mount a tmpfs over a path that does not exist, and does
// not shrug it off: it fails the sandbox, so every job on that machine
// fails. A Crostini container has no NetworkManager, which is how this was
// found -- every job on such a machine died at launch with
// "bwrap: Can't create file /etc/NetworkManager: Read-only file system".
func TestMasksSkipWhatTheMachineDoesNotHave(t *testing.T) {
	has := map[string]bool{"/usr/local": true, "/etc/ssh": true}
	got := MaskPaths(func(p string) bool { return has[p] })
	if len(got) != 2 {
		t.Fatalf("masks %v, want only the two this machine has", got)
	}
	for _, p := range got {
		if !has[p] {
			t.Errorf("masking %s, which is not on this machine", p)
		}
	}
	args := SandboxArgv(SandboxOpts{BwrapPath: "bwrap", Masks: got}, []string{"/bin/sh"})
	if masked(args, "/etc/NetworkManager") {
		t.Error("a path the machine does not have is still masked, which fails the launch")
	}
	if !masked(args, "/etc/ssh") {
		t.Error("a path it does have is not masked")
	}
}

// And a machine that has them all still hides them all: the filter is about
// what exists, not about what matters.
func TestMasksCoverEverythingPresent(t *testing.T) {
	got := MaskPaths(func(string) bool { return true })
	if len(got) != len(maskedPaths) {
		t.Errorf("masks %d of %d paths on a machine that has them all", len(got), len(maskedPaths))
	}
}

// The script is written to the host's scratch directory and run from the
// path the sandbox shows it at. Naming the host's path made bubblewrap
// report "No such file or directory" for a file that had just been written:
// the scratch is bound at a fixed place inside, and the host's own path for
// it is not in the namespace at all.
func TestTheJobRunsTheScriptByItsPathInside(t *testing.T) {
	sb := &platform.Sandbox{
		ScratchDir: "/tmp/shome/jobs/job-129",
		ScratchIn:  JobScratchPath,
	}
	got := scriptPathIn(sb)
	if got != JobScratchPath+"/job-script" {
		t.Errorf("job runs %q, want the path inside the sandbox", got)
	}
	if strings.Contains(got, sb.ScratchDir) {
		t.Errorf("job runs %q, which names the host's path", got)
	}
}
