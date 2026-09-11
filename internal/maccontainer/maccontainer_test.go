package maccontainer

import (
	"strings"
	"testing"
)

func argsFor(s Spec) []string { return Args(Runtime{Bin: "container"}, s) }

func target(args []string, source string) string {
	for i, a := range args {
		if a == "--mount" && i+1 < len(args) {
			for _, part := range strings.Split(args[i+1], ",") {
				if part == "source="+source {
					for _, p2 := range strings.Split(args[i+1], ",") {
						if strings.HasPrefix(p2, "target=") {
							return strings.TrimPrefix(p2, "target=")
						}
					}
				}
			}
		}
	}
	return ""
}

// The whole reason for running a session in a container: from inside, the
// account's own folder is the only place, and nothing says where it lives.
func TestNoHostPathIsVisibleInside(t *testing.T) {
	const host = "/Users/Shared/shome-dave/users/alice"
	s := Spec{
		Home: host, HomeIn: HomePath("alice", "mini"),
		Tools: "/Users/Shared/shome-dave/env/linux/bin", ToolsIn: ToolsPath,
		Env:  map[string]string{"HOME": HomePath("alice", "mini")},
		Argv: []string{"/bin/bash", "-i"}, TTY: true, Network: true,
	}
	args := argsFor(s)

	if got := target(args, host); got != "/home/alice" {
		t.Fatalf("the account's directory appears at %q, want /home/alice", got)
	}
	// A host path may appear as the *source* of a mount -- that is how a
	// bind works -- but nothing the session can read should contain one.
	for i, a := range args {
		switch a {
		case "--workdir", "--env":
			if i+1 < len(args) && strings.Contains(args[i+1], "/Users/") {
				t.Errorf("a host path leaks into the session: %s %s", a, args[i+1])
			}
		}
	}
}

// The machine's name is in the folder name: a cluster has no shared
// filesystem, so the same account's files differ per machine.
func TestHomePathIsTheSameOnEveryMachine(t *testing.T) {
	// Deliberate: an account's installed software records this path, so a
	// name that differed per machine broke every environment that was
	// copied to another one.
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

// The tool directory is read-only. A session that could write it could
// replace the shome binary every other session runs.
func TestToolsAreReadOnly(t *testing.T) {
	args := argsFor(Spec{Home: "/h", HomeIn: "/home/a", Tools: "/t", ToolsIn: ToolsPath})
	var toolMount string
	for i, a := range args {
		if a == "--mount" && i+1 < len(args) && strings.Contains(args[i+1], "source=/t,") {
			toolMount = args[i+1]
		}
	}
	if toolMount == "" {
		t.Fatal("the tool directory was not mounted")
	}
	if !strings.Contains(toolMount, "readonly") {
		t.Errorf("the tool directory is writable: %s", toolMount)
	}
	// And the account's own directory is not read-only.
	for i, a := range args {
		if a == "--mount" && i+1 < len(args) &&
			strings.Contains(args[i+1], "source=/h,") && strings.Contains(args[i+1], "readonly") {
			t.Error("the account's own directory was mounted read-only")
		}
	}
}

// Every container is thrown away, so nothing carries between sessions except
// what is in the account's own directory.
func TestContainerIsEphemeral(t *testing.T) {
	args := argsFor(Spec{Home: "/h", HomeIn: "/home/a"})
	if args[0] != "run" {
		t.Errorf("argv[0] = %q, want run", args[0])
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--rm") {
		t.Error("the container is not removed after it stops")
	}
}

func TestTTYAndNetworkAreRequested(t *testing.T) {
	plain := strings.Join(argsFor(Spec{Home: "/h", HomeIn: "/home/a"}), " ")
	if strings.Contains(plain, "--tty") {
		t.Error("a terminal was requested for a non-interactive run")
	}
	if !strings.Contains(plain, "--network none") {
		t.Error("network was not withheld from a run that did not ask for it")
	}
	session := strings.Join(argsFor(Spec{Home: "/h", HomeIn: "/home/a",
		TTY: true, Network: true}), " ")
	if !strings.Contains(session, "--tty") {
		t.Error("an interactive session got no terminal")
	}
	if strings.Contains(session, "--network none") {
		t.Error("network was withheld from a session that needs a package manager")
	}
}

// The environment is the session's, expressed in inside paths, and ordered so
// two identical sessions produce identical command lines.
func TestEnvironmentIsPassedAndOrdered(t *testing.T) {
	args := argsFor(Spec{Home: "/h", HomeIn: "/home/a",
		Env: map[string]string{"ZZZ": "1", "AAA": "2", "HOME": "/home/a"}})
	var envs []string
	for i, a := range args {
		if a == "--env" && i+1 < len(args) {
			envs = append(envs, args[i+1])
		}
	}
	// The caller's three, plus the basics a shell needs that the image
	// cannot know: HOME, USER, LOGNAME, TMPDIR, TERM.
	if len(envs) < 3 {
		t.Fatalf("passed %d env vars: %v", len(envs), envs)
	}
	if envs[0] != "AAA=2" || envs[len(envs)-1] != "ZZZ=1" {
		t.Errorf("environment is not ordered: %v", envs)
	}
	// A caller's value wins over the default.
	for _, e := range envs {
		if e == "HOME=/root" {
			t.Error("the image's HOME survived")
		}
	}
}

// The image sets HOME=/root and knows nothing of the account. Without these
// a session starts in the image's own home and everything it writes goes
// into a container that is thrown away.
func TestShellBasicsAreSet(t *testing.T) {
	args := argsFor(Spec{Home: "/h", HomeIn: "/home/alice", User: "alice"})
	got := map[string]string{}
	for i, a := range args {
		if a == "--env" && i+1 < len(args) {
			k, v, _ := strings.Cut(args[i+1], "=")
			got[k] = v
		}
	}
	for k, want := range map[string]string{
		"HOME": "/home/alice", "USER": "alice", "LOGNAME": "alice",
		"TMPDIR": "/home/alice/.tmp",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
}

// The runtime narrates image and kernel fetching; in a session that is noise
// before the prompt, and in a job it looks like output the job produced.
func TestProgressIsSilenced(t *testing.T) {
	if !strings.Contains(strings.Join(argsFor(Spec{Home: "/h"}), " "), "--progress none") {
		t.Error("progress output is not silenced")
	}
}

// The image comes last, before the command, or the runtime reads a flag as
// the image name.
func TestImageThenCommand(t *testing.T) {
	args := Args(Runtime{Bin: "container", Image: "example.test/img:1"},
		Spec{Home: "/h", HomeIn: "/home/a", Argv: []string{"/bin/bash", "-i"}})
	for i, a := range args {
		if a == "example.test/img:1" {
			if i != len(args)-3 {
				t.Errorf("image at %d of %d; the command must follow it", i, len(args))
			}
			if args[i+1] != "/bin/bash" {
				t.Errorf("command does not follow the image: %v", args[i+1:])
			}
			return
		}
	}
	t.Errorf("image missing from %v", args)
}

func TestDefaultImageWhenUnset(t *testing.T) {
	if !strings.Contains(strings.Join(argsFor(Spec{Home: "/h", HomeIn: "/a"}), " "), DefaultImage) {
		t.Error("no image was chosen")
	}
}

// A spec with no mapped path must still produce a runnable command. Leaving
// target= empty makes the runtime complain about directive format, which is
// an obscure way to discover an unset field.
func TestMissingInsidePathFallsBackToTheHostPath(t *testing.T) {
	args := argsFor(Spec{Home: "/some/where"})
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "target=") && strings.Contains(joined, "target= ") {
		t.Errorf("empty target in:\n%s", joined)
	}
	if target(args, "/some/where") != "/some/where" {
		t.Errorf("fallback target = %q", target(args, "/some/where"))
	}
	if !strings.Contains(joined, "--workdir /some/where") {
		t.Errorf("workdir not set from the fallback:\n%s", joined)
	}
}
