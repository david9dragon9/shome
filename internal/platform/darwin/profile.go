//go:build darwin

// Package darwin implements the macOS node backend.
//
// Isolation is a generated per-job Seatbelt profile with no OS account created,
// because account creation on macOS cannot be reversed without Recovery Mode
// (see docs/design-notes.md). Every non-obvious rule below was established empirically; the
// comments record why, since getting any of them wrong fails silently.
package darwin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ProfileConfig is the input to profile generation.
type ProfileConfig struct {
	ScratchDir string   // this job's private read-write dir
	OwnerHome  string   // the machine owner's home -- always denied
	DenyPaths  []string // the installation root, other jobs' scratch, etc.

	// ReadPaths are re-allowed for reading after the denies.
	//
	// Needed because DenyPaths is deliberately coarse -- it names the whole
	// shome installation so that a file added there later is covered without
	// anyone remembering to -- and a few things inside it are meant to be
	// readable: the managed tool directory a login shell runs uv from, and a
	// shared read-only model cache. Carving them back out explicitly is
	// safer than narrowing the deny and hoping the next file added lands
	// outside it.
	ReadPaths []string

	// TTYPath is this session's own pseudo-terminal, when it has one.
	//
	// Without an ioctl allowance for it, tcsetattr fails under the sandbox's
	// deny-by-default, which means readline cannot turn off the terminal's
	// echo -- so the tty driver echoes every line and readline echoes it
	// again, and each command appears twice. It also breaks anything else
	// that needs terminal control: window size, raw mode, an editor.
	//
	// Exactly this terminal, not a pattern over /dev/ttys*. The profile
	// grants a broad file-read*, so a pattern would let a job open and
	// ioctl a terminal belonging to the machine's owner -- and TIOCSTI on
	// somebody else's terminal injects keystrokes into their shell.
	TTYPath string

	AllowGPU bool // grants the two Metal allowances
	AllowNet bool // network is deny-by-default

	// WritePaths are additional read-write trees, for a session that needs
	// more than one (a home plus a cache, say).
	WritePaths []string

	// NoWritePaths are readable but not writable, even where they sit
	// inside a writable tree. For the corners of the account's own storage
	// that belong to this machine rather than to the account -- see
	// machineLocalPaths.
	NoWritePaths []string
}

// sbplEscape quotes a path for embedding in an SBPL string literal.
//
// This is security-relevant, not cosmetic: an unescaped quote or backslash in a
// path would terminate the literal early and let the remainder of the path be
// parsed as policy.
func sbplEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return r.Replace(s)
}

// realPath resolves symlinks. Seatbelt matches RESOLVED paths, so a profile
// written with an unresolved path silently matches nothing -- on macOS /tmp is
// /private/tmp and $TMPDIR is under /private/var/folders (see docs/design-notes.md). A rule that
// matches nothing fails open for allows and closed for denies, and in neither
// case does it report an error.
func realPath(p string) string {
	if p == "" {
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	// Path may not exist yet; resolve the deepest existing ancestor so the
	// prefix is still correct.
	dir, base := filepath.Split(abs)
	if resolved, err := filepath.EvalSymlinks(filepath.Clean(dir)); err == nil {
		return filepath.Join(resolved, base)
	}
	return abs
}

// ancestors returns every directory from / down to p inclusive.
func ancestors(p string) []string {
	var out []string
	cur := filepath.Clean(p)
	for {
		out = append([]string{cur}, out...)
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return out
}

// traversal returns the paths that must be stat-able for p to be reachable:
// the ancestors of the resolved path, and the ancestors of the path as
// written.
//
// Both, because Seatbelt matches the resolved path for the file itself but
// the kernel still walks the spelling the process used, and a symlink is
// resolved by reading the link -- which needs a rule naming the link, not
// its target. That is the same reason /etc appears in systemMetadataPaths
// alongside /private/etc.
//
// Without it, a state root anywhere under a symlinked directory produces a
// profile that looks correct and denies everything inside it: on macOS /tmp
// is a symlink to /private/tmp, so a job whose scratch was under /tmp could
// not read its own scratch -- not even the script it was about to run, which
// failed as "Operation not permitted" with nothing naming the cause.
func traversal(resolved, written string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(paths []string) {
		for _, a := range paths {
			if a == "" || seen[a] {
				continue
			}
			seen[a] = true
			out = append(out, a)
		}
	}
	add(ancestors(resolved))
	add(linkTraversal(written))
	return out
}

// linkTraversal returns every prefix of p in the spelling the kernel checks
// while walking it: the prefix's parent resolved, its own last component left
// as written.
//
// Neither the written path nor the resolved one is what gets asked about. For
// a scratch at /tmp/s/link/job-1 the kernel reports the denial as
// /private/tmp/s/link -- /tmp resolved because it was already walked, link
// left alone because reading it is the operation being checked. So a rule has
// to name that mixed spelling, and there is one per component.
func linkTraversal(p string) []string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range ancestors(abs) {
		dir, base := filepath.Split(a)
		if base == "" {
			out = append(out, a) // the root itself
			continue
		}
		out = append(out, filepath.Join(realPath(filepath.Clean(dir)), base))
	}
	return out
}

// TTYRules permits terminal control on one pseudo-terminal.
//
// Separated out because it is needed in two places: written into the profile
// up front for an interactive session, where the terminal exists before the
// policy does, and appended for a job that turns out to have one, where the
// policy is generated before the terminal is allocated.
//
// /dev/tty is the process's own controlling terminal, whatever that is, so
// naming it grants nothing beyond the terminal the process already has.
func TTYRules(ttyPath string) string {
	return fmt.Sprintf(";; terminal control for this session's own pty.\n"+
		";; Without it tcsetattr is denied, readline cannot turn off the\n"+
		";; terminal's echo, and every command typed appears twice.\n"+
		"(allow file-ioctl (literal \"%s\"))\n"+
		"(allow file-ioctl (literal \"/dev/tty\"))\n",
		sbplEscape(realPath(ttyPath)))
}

// systemReadPaths are the OS's own program files.
//
// This is an allowlist, and everything not on it is invisible. Nothing here
// belongs to the machine's owner: no /Applications, no /Users, no /Volumes,
// no /Library, no /private/tmp, no /private/var/folders.
//
// The profile used to grant a broad (allow file-read*) and narrow it with
// denies, on the recorded finding that "allowlisting file-read* by subpath
// aborts dyld". That finding was wrong, and the consequence was that a
// logged-in user could browse the owner's applications and files. The
// allowlist failed for one missing line -- read access to the root directory
// itself, which dyld stats on every exec. With `(literal "/")` present it
// works, which is why that entry is first and load-bearing.
var systemReadPaths = []string{
	// dyld stats the root on every exec. Without this nothing runs at all,
	// and it fails silently with SIGABRT before the process prints anything,
	// which is what made an allowlist look impossible.
	//
	// It permits listing the top-level directory names -- the same names
	// every Mac has -- and nothing underneath them.
	`(literal "/")`,

	// The runtime and the system tools.
	`(subpath "/usr/lib")`, `(subpath "/usr/share")`, `(subpath "/usr/bin")`,
	`(subpath "/usr/libexec")`, `(subpath "/usr/sbin")`,
	`(subpath "/System")`, `(subpath "/bin")`, `(subpath "/sbin")`,

	// Which shell and toolchain the OS considers default. Both spellings:
	// the resolved path and the /var symlink, because readlink is asked
	// about the link itself and does not resolve first.
	`(subpath "/private/var/select")`, `(subpath "/var/select")`,

	// Name resolution, time zones and certificates. Named individually --
	// /private/etc as a whole holds rather more than a job needs to see.
	`(literal "/private/etc/passwd")`, `(literal "/private/etc/group")`,
	`(literal "/private/etc/localtime")`, `(literal "/private/etc/hosts")`,
	`(literal "/private/etc/resolv.conf")`, `(literal "/private/etc/services")`,
	`(subpath "/private/etc/ssl")`, `(subpath "/private/etc/pam.d")`,

	// The time zone database. /private/etc/localtime is a symlink into it,
	// so without this a job's clock is UTC whatever the machine is set to
	// and ZoneInfo("America/Los_Angeles") raises "no time zone found" --
	// which reads as a broken interpreter rather than a policy decision.
	`(subpath "/private/var/db/timezone")`,
}

// systemMetadataPaths may be stat'ed but not read.
//
// Path resolution walks every component, so a directory on the way to
// something allowed has to be stat-able even when its contents are not.
var systemMetadataPaths = []string{
	`(literal "/usr")`, `(literal "/private")`, `(literal "/private/etc")`,
	`(literal "/var")`, `(literal "/private/var")`,
	// /etc is a symlink to private/etc, and readlink is asked about the link
	// itself rather than resolving first -- so a path spelled /etc/... is
	// denied even when its target is allowed. That is how a job with a
	// perfectly good /private/etc/ssl/cert.pem still failed every HTTPS
	// request: curl asks for /etc/ssl/cert.pem.
	`(literal "/etc")`,
}

// deviceNodes are the character devices a shell or interpreter expects.
var deviceNodes = []string{
	"/dev/null", "/dev/zero", "/dev/random", "/dev/urandom",
	"/dev/tty", "/dev/stdin", "/dev/stdout", "/dev/stderr", "/dev/dtracehelper",
}

// GenerateProfile builds the SBPL policy for one job or session.
//
// Deny by default, then allow exactly what is needed. The writable set is one
// directory -- the job's scratch, or the account's own storage -- plus
// anything the caller adds explicitly.
//
// Rule ordering still matters where an allow and a deny overlap, and SBPL
// resolves by operation specificity rather than purely by order: a
// (deny file-write*) is NOT overridden by a later (allow file*), because the
// wildcard is treated as less specific. Only a matching (allow file-write*)
// overrides it.
func GenerateProfile(c ProfileConfig) (string, error) {
	scratch := realPath(c.ScratchDir)
	if scratch == "" {
		return "", fmt.Errorf("scratch dir is required")
	}

	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	w(";; shome sandbox profile (generated -- do not edit)")
	w(";; writable: %s", scratch)
	w("(version 1)")
	w("(deny default)")
	w("")
	w(";; process basics required for any binary to start")
	w("(allow process-exec*)")
	w("(allow process-fork)")
	w("(allow sysctl-read)")
	w("")
	w(";; ---- readable: the OS's own program files, and nothing else ----")
	w(";; An allowlist. Anything not named here is invisible, including every")
	w(";; application, every other account's files, and everything the")
	w(";; machine's owner has.")
	for _, p := range systemReadPaths {
		w("(allow file-read* %s)", p)
	}
	w("")
	w(";; stat-able on the way to something allowed, but not readable")
	for _, p := range systemMetadataPaths {
		w("(allow file-read-metadata %s)", p)
	}
	w("")
	w(";; standard device nodes: shells and interpreters redirect to these")
	for _, dev := range deviceNodes {
		w(`(allow file-read*  (literal "%s"))`, dev)
		w(`(allow file-write* (literal "%s"))`, dev)
	}
	w(`(allow file-read*  (subpath "/dev/fd"))`)
	w(`(allow file-write* (subpath "/dev/fd"))`)
	w("")
	w(";; POSIX semaphores and shared memory: what multiprocessing is built")
	w(";; on. Without them a worker pool fails with a bare EPERM, which takes")
	w(";; every data loader and every parallel map with it -- and the error")
	w(";; names nothing, so it reads as a broken Python.")
	w(";;")
	w(";; These namespaces are global rather than per-directory, so this is a")
	w(";; real widening: a job that guessed the name of another process's")
	w(";; segment could open it. Names are random and the objects are the")
	w(";; creator's to permit, and only a GPU job takes this path -- of which")
	w(";; a Mac runs one at a time, since the GPU is allocated whole. Every")
	w(";; other job is in a container and shares none of this.")
	w("(allow ipc-posix-sem)")
	w("(allow ipc-posix-shm)")
	w("")
	w(";; Resolving the account this job runs as. getpwuid(getuid()) goes to")
	w(";; opendirectoryd, and on a Mac the flat /etc/passwd holds only system")
	w(";; accounts -- so with this denied, getpass.getuser() raises and so")
	w(";; does anything that asks who it is running as.")
	w(";;")
	w(";; It permits reading the machine's account records: names, uids, home")
	w(";; directory paths. Not credentials, and not the contents of any of")
	w(";; those directories, which the allowlist above still denies.")
	w(`(allow mach-lookup (global-name "com.apple.system.opendirectoryd.libinfo"))`)
	w("")

	if c.TTYPath != "" {
		w("%s", TTYRules(c.TTYPath))
	}
	if c.AllowGPU {
		w(";; Metal. Established by bisection (see docs/design-notes.md) and then by running a")
		w(";; real framework:")
		w(";;   AGXDeviceUserClient      -> device enumeration")
		w(";;   MTLCompilerService       -> runtime MSL shader compilation")
		w(";;   IOSurfaceRootUserClient  -> Metal shared events")
		w(";; The first two are enough for a hand-written compute shader,")
		w(";; which is what the original bisection used, and that is why")
		w(";; IOSurface was recorded as unnecessary. It is not: MLX creates a")
		w(";; shared event to order work on the queue, and without this the")
		w(";; first array operation fails with \"Failed to create Metal shared")
		w(";; event\" -- so every GPU framework that synchronises this way was")
		w(";; unusable. IOGPUDeviceUserClient and gpumemd remain unnecessary.")
		w(`(allow iokit-open  (iokit-user-client-class "AGXDeviceUserClient"))`)
		w(`(allow iokit-open  (iokit-user-client-class "IOSurfaceRootUserClient"))`)
		w(`(allow mach-lookup (global-name "com.apple.MTLCompilerService"))`)
		w("")
	}
	if c.AllowNet {
		w(";; network was requested; otherwise deny-by-default applies")
		w("(allow network*)")
		w("")
	}

	if len(c.ReadPaths) > 0 {
		w(";; ---- extra read-only trees ----")
		w(";; shome's managed tools, and any shared read-only cache.")
		for _, p := range c.ReadPaths {
			rp := realPath(p)
			if rp == "" {
				continue
			}
			w(`(allow file-read* (subpath "%s"))`, sbplEscape(rp))
			for _, a := range traversal(rp, p) {
				w(`(allow file-read-metadata (literal "%s"))`, sbplEscape(a))
			}
		}
		w("")
	}

	w(";; ---- the one writable place ----")
	w(";; getcwd(3) stats every ancestor of the working directory, so the")
	w(";; chain down to it is stat-able even though those directories are")
	w(";; not readable.")
	// The paths as the caller spelled them, not as resolved: a rule has to
	// name both, so the resolving happens per path below rather than up
	// front. See traversal.
	writable := append([]string{c.ScratchDir}, c.WritePaths...)
	for _, p := range writable {
		rp := realPath(p)
		if rp == "" {
			continue
		}
		w(`(allow file-read*  (subpath "%s"))`, sbplEscape(rp))
		w(`(allow file-write* (subpath "%s"))`, sbplEscape(rp))
		// A unix socket here. Seatbelt treats binding one as a network
		// operation, so with network denied a job could not talk to itself:
		// this is what PyTorch's shared-memory manager does, and it failed
		// as "torch_shm_manager: Operation not permitted" -- which took
		// every data loader with worker processes with it. The same is true
		// of anything else that coordinates locally over a socket.
		//
		// Filtered by path, so it grants nothing over IP: an address filter
		// and a path filter cannot both match one socket, and network access
		// still needs --network. The path is one the job already writes.
		w(`(allow network-bind     (subpath "%s"))`, sbplEscape(rp))
		w(`(allow network-outbound (subpath "%s"))`, sbplEscape(rp))
		for _, a := range traversal(rp, p) {
			w(`(allow file-read-metadata (literal "%s"))`, sbplEscape(a))
		}
	}
	if len(c.NoWritePaths) > 0 {
		w("")
		w(";; ---- carved out of the writable trees ----")
		w(";; Readable, so a job can see what is there, and not writable, so")
		w(";; nothing new lands in them. Emitted after the allows above, and")
		w(";; with the same operation, which is what makes them win.")
		for _, p := range c.NoWritePaths {
			rp := realPath(p)
			if rp == "" {
				continue
			}
			w(`(deny file-write* (subpath "%s"))`, sbplEscape(rp))
		}
	}
	w("")
	w(";; Belt and braces over the allowlist above: these are the paths whose")
	w(";; exposure would matter most, and naming them means a future widening")
	w(";; of the allowlist cannot quietly re-expose them.")
	if h := realPath(c.OwnerHome); h != "" {
		w(`(deny file-read*  (subpath "%s"))`, sbplEscape(h))
		w(`(deny file-write* (subpath "%s"))`, sbplEscape(h))
	}
	for _, p := range c.DenyPaths {
		rp := realPath(p)
		if rp == "" || rp == scratch {
			continue
		}
		// A deny would otherwise swallow a writable area nested inside it.
		nested := false
		for _, wp := range writable {
			if r := realPath(wp); r == rp || strings.HasPrefix(r, rp+string(os.PathSeparator)) {
				nested = true
				break
			}
		}
		if nested {
			continue
		}
		w(`(deny file-read*  (subpath "%s"))`, sbplEscape(rp))
		w(`(deny file-write* (subpath "%s"))`, sbplEscape(rp))
	}
	w("")
	w(";; no signalling other jobs, but a process must signal itself or the")
	w(";; runtime dies -- so re-allow (target self) AFTER the deny.")
	w("(deny signal)")
	w("(allow signal (target self))")
	w("")
	w(";; no inspecting other processes. Must be scoped to (target others):")
	w(";; unscoped (deny process-info*) aborts the runtime with exit 133.")
	w("(deny process-info* (target others))")

	return b.String(), nil
}
