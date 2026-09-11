//go:build linux

package linux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"

	"github.com/davidwu/shome/internal/platform"
)

// ShellCommand builds an isolated interactive session.
//
// bubblewrap gives a mount namespace, so the session cannot even name paths
// outside its own tree -- a stronger statement than macOS's policy-based
// denial, and it means the installation root and every other account's files
// are simply absent rather than merely forbidden.
//
// Without bwrap this returns an error rather than an unsandboxed shell. A
// batch job on such a node runs unconfined and says so at join time, which is
// a defensible trade for a machine whose owner has accepted it; handing a
// remote user an unconfined interactive shell on somebody's computer is not.
func (b *Backend) ShellCommand(ctx context.Context, s platform.ShellSpec) (*exec.Cmd, error) {
	if s.Home == "" {
		return nil, fmt.Errorf("an interactive session needs a home directory")
	}
	if err := os.MkdirAll(s.Home, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", s.Home, err)
	}
	if !b.Env.HasBwrap {
		return nil, fmt.Errorf("this machine cannot isolate an interactive session: " +
			"bubblewrap is not installed.\n" +
			"Install it (apt install bubblewrap) and rejoin, or use 'sbatch' instead")
	}

	tmp := filepath.Join(s.Home, ".tmp")
	os.MkdirAll(tmp, 0o700)

	argv := s.Argv
	if len(argv) == 0 {
		argv = []string{defaultShell(), "-i"}
	}
	// Where the account's directory appears from inside. Chosen by the
	// caller, and containing the machine's name, because a cluster has no
	// shared filesystem: the same account has different files on every
	// machine, and a path that does not say which machine it is invites
	// somebody to think their files followed them.
	homeIn := s.HomeAs
	if homeIn == "" {
		homeIn = s.Home
	}
	mounts := []Mount{{Host: s.Home, In: homeIn, Write: true, Required: true}}
	for i, p := range s.ReadOnly {
		m := Mount{Host: p}
		// The first read-only tree is shome's managed tools, which the
		// session's PATH names; the rest keep their own paths.
		if i == 0 && s.ToolsAs != "" {
			m.In = s.ToolsAs
		}
		mounts = append(mounts, m)
	}
	for _, p := range s.Writable {
		if p != "" && p != s.Home {
			mounts = append(mounts, Mount{Host: p, Write: true})
		}
	}
	args := SandboxArgv(SandboxOpts{
		BwrapPath: b.Env.BwrapPath,
		Masks:     MaskPaths(PathExists),
		Mounts:    mounts,
		Chdir:     homeIn,
		Home:      homeIn,
		Network:   s.AllowNet,
	}, argv)

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = shellEnv(s)
	return cmd, nil
}

func defaultShell() string {
	for _, sh := range []string{"/bin/bash", "/bin/sh"} {
		if fi, err := os.Stat(sh); err == nil && !fi.IsDir() {
			return sh
		}
	}
	return "/bin/sh"
}

// shellEnv builds the session's environment from nothing, so a remote user
// never sees the daemon's own variables.
func shellEnv(s platform.ShellSpec) []string {
	base := map[string]string{
		"HOME":    s.Home,
		"USER":    s.User,
		"LOGNAME": s.User,
		"SHELL":   defaultShell(),
		"TMPDIR":  filepath.Join(s.Home, ".tmp"),
		"PATH":    "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"TERM":    "xterm-256color",
		"LANG":    "C.UTF-8",
	}
	for k, v := range s.Env {
		base[k] = v
	}
	out := make([]string, 0, len(base))
	for k, v := range base {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// Layout reports where an account's directories appear inside the sandbox.
//
// bubblewrap builds a fresh mount namespace, so these are names we choose
// rather than the host's. The account's storage appears under /home with the
// machine's name attached, and shome's tools at a fixed path; nothing in
// either says where the installation lives or what else is on the machine.
func (b *Backend) Layout(p platform.Placement) platform.Layout {
	if !b.Env.HasBwrap {
		// No namespace, so nothing is remapped -- and this machine will not
		// be running sessions anyway; IsolationFault drains it.
		return platform.Layout{Home: p.Home, Tools: p.Tools,
			Slot: slot(), Mapped: false}
	}
	return platform.Layout{
		Home:  HomePath(p.User, p.Machine),
		Tools: ToolsPath,
		// A mount namespace shares the kernel, so the environment inside is
		// this machine's own.
		Slot:   slot(),
		Mapped: true,
	}
}

// slot is this machine's os-arch, which is also what a namespace on it runs.
func slot() string {
	return runtime.GOOS + "-" + runtime.GOARCH
}

// StopIsolation has nothing to do on Linux: a bubblewrap namespace is a
// process tree started with --die-with-parent, so it goes when the session's
// process does.
func (b *Backend) StopIsolation(ctx context.Context, name string) error { return nil }

// SessionHostAddr is empty on Linux: a bubblewrap namespace shares the
// host's filesystem, so a unix socket in the account's directory works.
func (b *Backend) SessionHostAddr(ctx context.Context) string { return "" }
