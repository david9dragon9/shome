package loginnode

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// CLIRunner executes allow-listed verbs by running the shome binary with the
// caller's identity attached.
//
// Running the same CLI the owner runs, rather than reimplementing the verbs
// here, means a remote user sees byte-identical output and there is no second
// copy of the formatting to drift. The cost is a short-lived process per
// command, which is the right trade for a gateway that handles a handful of
// commands a minute -- and it is what a real login node does anyway.
//
// Identity travels as SHOME_ACT_AS, which the controller honours only for an
// admin token and logs every time. The login node holds that admin token; the
// remote user never sees it and cannot influence the variable, because the
// SSH layer refuses caller-supplied environment.
type CLIRunner struct {
	// Binary is the shome executable to run. Defaults to this process's own.
	Binary string
	// Root is the state directory, passed through so the CLI finds the
	// controller socket without depending on the ambient environment.
	Root string
	// Timeout bounds a single command.
	Timeout time.Duration
}

// allowedVerbs is the complete set a remote user may run.
//
// Submission and inspection only. Absent, and absent on purpose: anything that
// administers the cluster (`admin`, `serve`), anything that manages this
// machine (`up`, `down`, `nuke`, `pause`, `contribute`), and anything that
// reveals a credential (`token`, `web`, `login`). A remote user's reach is
// their own jobs and their own files.
var allowedVerbs = map[string]string{
	"sbatch":   "submit a batch job",
	"srun":     "run something now and watch it (--pty for a shell)",
	"squeue":   "list jobs",
	"scancel":  "cancel your jobs",
	"sinfo":    "show cluster capacity",
	"sacct":    "accounting history",
	"scontrol": "job detail",
	"cat":      "print a finished job's output",
	"fetch":    "download a job's staged-out results",
	"storage":  "your persistent files (ls/put/get/rm/du)",
	"plan":     "can this run, and where?",
	"top":      "live cluster dashboard (one frame)",
	"whoami":   "which account you are",
	"squota":   "what you are using, and what you are allowed",
	"quota":    "the same as squota",
	"sshare":   "fair-share standing, and why jobs are ordered",
	"enroll":   "get a code to add another of your computers",
	"keys":     "which keys can log in as you",
	"unenroll": "sign this computer out; --all signs out everywhere",
	"events":   "recent cluster events",
	"help":     "this message",
}

// Verbs lists the allow-listed verbs, for an error message that tells the
// caller what they can do instead.
func (r *CLIRunner) Verbs() []string {
	out := make([]string, 0, len(allowedVerbs))
	for v := range allowedVerbs {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func (r *CLIRunner) Allowed(verb string) bool {
	_, ok := allowedVerbs[verb]
	return ok
}

// Help lists what a remote user may do.
func (r *CLIRunner) Help() string {
	verbs := make([]string, 0, len(allowedVerbs))
	for v := range allowedVerbs {
		verbs = append(verbs, v)
	}
	sort.Strings(verbs)
	var b strings.Builder
	b.WriteString("available commands:\n")
	for _, v := range verbs {
		fmt.Fprintf(&b, "  %-9s %s\n", v, allowedVerbs[v])
	}
	b.WriteString("\nType 'exit' to disconnect.\n")
	return b.String()
}

// Run executes one command as user.
func (r *CLIRunner) Run(ctx context.Context, user string, args []string, stdin []byte) ([]byte, []byte, int) {
	return r.RunAs(ctx, user, "", args, stdin)
}

// RunAs is Run with the fingerprint of the key this session authenticated
// with, so a command can act on "this machine" specifically -- which is what
// signing out of one computer means.
func (r *CLIRunner) RunAs(ctx context.Context, user, keyFP string, args []string, stdin []byte) ([]byte, []byte, int) {
	if len(args) == 0 {
		return nil, nil, 0
	}
	if args[0] == "help" {
		return []byte(r.Help()), nil, 0
	}
	bin := r.Binary
	if bin == "" {
		self, err := os.Executable()
		if err != nil {
			return nil, []byte("shome: cannot locate the shome binary\n"), 1
		}
		bin = self
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	// A deliberately minimal environment. Nothing is inherited from the SSH
	// session, so a remote user cannot set SHOME_ACT_AS, SHOME_TOKEN, PATH or
	// anything else that would change who the command runs as or what it runs.
	cmd.Env = []string{
		"SHOME_ROOT=" + r.Root,
		"SHOME_ACT_AS=" + user,
		// Which key opened this session. Set by the login node from the
		// verified connection, never from anything the caller sent.
		"SHOME_SESSION_KEY=" + keyFP,
		"HOME=" + os.TempDir(),
		"PATH=/usr/bin:/bin",
		"TERM=dumb",
	}
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()

	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = 1
			fmt.Fprintf(&errOut, "shome: %v\n", err)
		}
	}
	if ctx.Err() == context.DeadlineExceeded {
		fmt.Fprintf(&errOut, "shome: command timed out after %s\n", timeout)
		code = 124
	}
	return out.Bytes(), errOut.Bytes(), code
}
