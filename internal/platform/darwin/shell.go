//go:build darwin

package darwin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/davidwu/shome/internal/maccontainer"
	"github.com/davidwu/shome/internal/platform"
	"github.com/davidwu/shome/internal/userenv"
)

// ShellCommand builds an isolated interactive session.
//
// The profile is the same generator the batch path uses, with three
// differences, each of which is a deliberate choice rather than a
// convenience:
//
//   - The writable tree is the account's *persistent* directory rather than a
//     throwaway scratch dir, because a uv environment the user builds has to
//     still be there tomorrow.
//   - Network is allowed. A package manager with no network is not a package
//     manager, and installing things is the point of this session.
//   - The managed tool directory is re-allowed for reading, since it lives
//     inside the installation root that the profile otherwise denies whole.
//
// The profile still denies the owner's home, every other account's files, and
// everything else under the installation root -- which includes the admin
// token, the certificate authority's private key and the job database.
func (b *Backend) ShellCommand(ctx context.Context, s platform.ShellSpec) (*exec.Cmd, error) {
	if s.Home == "" {
		return nil, fmt.Errorf("an interactive session needs a home directory")
	}
	if err := os.MkdirAll(s.Home, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", s.Home, err)
	}

	// A session runs in a container, because a Seatbelt profile cannot hide
	// the host: it can refuse access to a path but not move it, so an account
	// would still see where its directory lives, could walk up out of it, and
	// could tell an existing file from a missing one by the error. Inside a
	// container none of those are questions the account can ask.
	rt, why := b.container()
	if rt == nil {
		return nil, fmt.Errorf("%s", why)
	}
	// Never unnamed. Killing the launcher does not stop a container, so an
	// unnamed one cannot be stopped at all -- not by its caller, which has
	// no handle for it, and not by the sweep, which cannot tell it from
	// somebody else's work. It keeps a virtual machine and its gigabyte of
	// memory until the machine is rebooted.
	//
	// This is not hypothetical: shome's own tests called this without a
	// name, and every run of them left another one behind.
	name := s.Name
	if name == "" {
		name = b.sessionPrefix() + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	cmd := rt.Command(ctx, maccontainer.Spec{
		Home: s.Home, HomeIn: s.HomeAs,
		Tools: s.ToolsHost, ToolsIn: s.ToolsAs,
		User: s.User,
		Env:  s.Env,
		Argv: s.Argv,
		TTY:  s.TTY, Network: s.AllowNet,
		Name: name,
	})
	noteSession(name, true)
	return cmd, nil
}

// defaultShell is the shell an interactive session gets.
//
// /bin/sh rather than the owner's login shell: the owner's shell may be
// anywhere, including inside their home directory, which this sandbox denies
// -- and a session that dies at startup because the shell binary is
// unreadable is a confusing way to learn that.
func defaultShell() string {
	for _, sh := range []string{"/bin/bash", "/bin/sh"} {
		if fi, err := os.Stat(sh); err == nil && !fi.IsDir() {
			return sh
		}
	}
	return "/bin/sh"
}

// shellEnv builds the session's environment from nothing.
//
// Nothing is inherited. The caller's environment holds SHOME_ROOT, the admin
// token's location and whatever else the daemon was started with, and a
// remote user must not see or be able to influence any of it.
func shellEnv(s platform.ShellSpec) []string {
	base := map[string]string{
		"HOME":    s.Home,
		"USER":    s.User,
		"LOGNAME": s.User,
		"SHELL":   defaultShell(),
		// TMPDIR inside the home, because the system temp directory is
		// outside every allowed path and a shell that cannot write a temp
		// file fails in obscure ways.
		"TMPDIR": filepath.Join(s.Home, ".tmp"),
		"PATH":   "/usr/bin:/bin:/usr/sbin:/sbin",
		"TERM":   "xterm-256color",
		"LANG":   "en_US.UTF-8",
	}
	for k, v := range s.Env {
		base[k] = v
	}
	os.MkdirAll(base["TMPDIR"], 0o700)
	out := make([]string, 0, len(base))
	for k, v := range base {
		out = append(out, k+"="+v)
	}
	// Sorted so a session's environment is reproducible, which makes
	// "it works for me" arguments shorter.
	sort.Strings(out)
	return out
}

// ShellPATH prepends shome's managed tool directory to a PATH.
func ShellPATH(toolDir string) string {
	if toolDir == "" {
		return "/usr/bin:/bin:/usr/sbin:/sbin"
	}
	return strings.Join([]string{toolDir, "/usr/bin", "/bin", "/usr/sbin", "/sbin"}, ":")
}

// Layout reports the account's directories as they appear inside.
//
// Unchanged: a Seatbelt profile is a policy over the real filesystem and
// cannot remap a path, so an account on a Mac sees the host's own layout.
// Mapped is false to say so rather than leaving the caller to assume.
func (b *Backend) Layout(p platform.Placement) platform.Layout {
	// A GPU job runs natively whatever the runtime can do, so it gets the
	// host's layout even on a machine where everything else is contained.
	if _, why := b.container(); why == "" && p.GPUs == 0 {
		// Inside a container the paths are ours to choose, so the account's
		// directory gets a name that says which machine it is on and nothing
		// about where it lives.
		return platform.Layout{
			Home:  maccontainer.HomePath(p.User, p.Machine),
			Tools: maccontainer.ToolsPath,
			// The container is Linux on this machine's architecture, which
			// is not this machine's OS: a job here does not share an
			// environment with a GPU job on the same Mac.
			Slot:   "linux-" + runtime.GOARCH,
			Mapped: true,
		}
	}
	// Native: the job runs on the machine's own filesystem, so it gets the
	// machine's own software as well as shome's managed tools.
	return platform.Layout{
		Home: p.Home, Tools: p.Tools, Mapped: false,
		Slot:     runtime.GOOS + "-" + runtime.GOARCH,
		Software: userenv.SoftwareBinDirs(userenv.MachineSoftware()),
	}
}
