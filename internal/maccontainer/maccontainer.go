// Package maccontainer runs a login session inside a Linux container on a Mac.
//
// # Why a container on macOS
//
// A Seatbelt profile is a policy over the real filesystem. It can refuse
// access, but it cannot move a path -- so an account always sees where its
// directory really lives, can walk up out of it, and can tell an existing
// file from a missing one by the difference between EPERM and ENOENT. Those
// three are not bugs to be fixed in the profile; they follow from what a
// policy is.
//
// Closing them needs a filesystem namespace, and macOS has no unprivileged
// one: there is no bind, null or union mount to build a jail with, the dyld
// shared cache a jail would need is six gigabytes inside a cryptex that
// cannot be relocated, and chroot needs root regardless. Apple's own answer
// to sandboxing arbitrary processes outside the App Store is a container,
// which on macOS 26 is a lightweight virtual machine per container.
//
// Inside it, the isolation is Linux's: the account's directory is the only
// thing mounted, and everything about the host is not merely forbidden but
// absent. That makes a session on a Mac behave exactly like one on a Linux
// node, which is the point.
//
// # What it costs
//
// A virtual machine has no Metal. That is fine for a login node, which exists
// to prepare and submit work rather than run it, and it is why compute jobs
// on a Mac stay native.
package maccontainer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultImage is the session image.
//
// Debian rather than Alpine because uv's managed interpreters are built
// against glibc, and a musl host turns "install Python" into a confusing
// failure. It ships uv already, so a session has a package manager before
// anything is mounted into it.
const DefaultImage = "ghcr.io/astral-sh/uv:debian-slim"

// Prefix marks every container shome starts, so leftovers can be recognised
// and cleaned up -- and so containers started by anything else are left
// alone.
const Prefix = "shome-"

// StatusTimeout bounds a question asked of the container runtime.
//
// The runtime manages virtual machines and can be slow or wedged, and a
// caller asking whether it is available must get an answer either way: a
// question that never returns is indistinguishable from a runtime that is
// missing, except that it also stops whatever asked.
const StatusTimeout = 30 * time.Second

// ToolsPath is where shome's Linux binaries appear inside.
const ToolsPath = "/opt/shome/bin"

// InstallTag identifies one shome installation on this machine.
//
// The container runtime has a single namespace for the whole machine, while
// shome's state is per directory -- so two installations, which is what a
// second SHOME_ROOT is, see each other's containers and neither can tell
// them from its own. The tidy-up sweep then stops the other one's running
// jobs: a controller restarting in one root killed a job that a second root
// was running, and reported it as exit 137 with nothing to explain it.
//
// Derived from the state directory rather than from the node name, because
// the state directory is what actually distinguishes two installations, and
// because a backend knows its own root without being told who it is.
func InstallTag(stateRoot string) string {
	p, err := filepath.Abs(stateRoot)
	if err != nil {
		p = stateRoot
	}
	h := sha256.Sum256([]byte(filepath.Clean(p)))
	return hex.EncodeToString(h[:])[:6]
}

// HomePath is where an account's storage appears inside.
//
// A bare /home/<user>, with nothing in it naming the machine.
//
// It used to be /home/alice@mini, so that a cluster with no shared
// filesystem did not look like one directory following the user around.
// That name is recorded inside everything the account installs -- a
// virtualenv's interpreter symlinks, a script's shebang, a compiled
// extension's rpath -- so the environment an account built on one machine
// broke on the next, for no reason other than the directory's name. Which
// machine you are on is worth saying, but the prompt, the session banner and
// $SHOME_HOST all say it, and none of them get baked into a file.
func HomePath(user, machine string) string {
	// machine is still in the signature: callers name a placement, and this
	// is the one place that decides whether the name matters.
	_ = machine
	return "/home/" + user
}

// Mount is one host directory placed somewhere inside.
type Mount struct {
	Host  string
	In    string
	Write bool
}

// Runtime is a usable container CLI.
type Runtime struct {
	Bin   string
	Image string
}

// Spec is one session to run.
type Spec struct {
	// Home is the account's directory on this machine, and HomeIn where it
	// should appear. Everything else about the host stays out.
	Home, HomeIn string
	// Tools is a directory of Linux shome binaries, mounted read-only.
	Tools, ToolsIn string
	// User is the account, for USER and LOGNAME.
	User string
	// Env is the session's environment, already expressed in inside paths.
	Env map[string]string
	// Argv is the command to run. Empty means the image's default shell.
	Argv []string
	// TTY asks for a terminal, for an interactive session.
	TTY bool
	// Network permits outbound network, which a package manager needs.
	Network bool

	// Workdir is where the command runs, as a path inside. Empty means the
	// account's directory, which is where a login session starts.
	Workdir string

	// Name is the container's id, so it can be stopped later. Killing the
	// process that started it does not stop it -- the container outlives
	// its launcher, still holding memory and CPUs.
	Name string

	// Extra are further directories to place inside, beyond the account's
	// own and the tool directory.
	Extra []Mount

	// CPUs and MemMB are the job's limits. The virtual machine enforces
	// both, which on macOS makes the memory limit a hard boundary rather
	// than the polled kill a native job gets.
	CPUs  int
	MemMB int64
}

// Args builds the command line, without running anything.
//
// Pure, so the argument list can be checked on any machine rather than only
// on one with the runtime installed.
func Args(r Runtime, s Spec) []string {
	image := r.Image
	if image == "" {
		image = DefaultImage
	}
	// --progress none because the runtime otherwise narrates image and
	// kernel fetching onto the session's own output. For an interactive
	// shell that is noise before the prompt; for a job it lands in the
	// captured output and looks like something the job printed.
	args := []string{"run", "--rm", "--interactive", "--progress", "none"}
	if s.TTY {
		args = append(args, "--tty")
	}

	// The account's own directory, read-write, and nothing else of the
	// host's. --mount rather than -v because only the former can say
	// readonly, and the tool directory must not be writable.
	//
	// A caller that names no inside path gets the host's, which is not
	// isolation but is a valid command -- an empty target is rejected by the
	// runtime with a message about directive format, which is a confusing
	// way to learn that a field was left unset.
	homeIn := s.HomeIn
	if homeIn == "" {
		homeIn = s.Home
	}
	args = append(args, "--mount",
		fmt.Sprintf("type=bind,source=%s,target=%s", s.Home, homeIn))
	if s.Tools != "" && s.ToolsIn != "" {
		args = append(args, "--mount",
			fmt.Sprintf("type=bind,source=%s,target=%s,readonly", s.Tools, s.ToolsIn))
	}
	for _, m := range s.Extra {
		if m.Host == "" || m.In == "" {
			continue
		}
		spec := fmt.Sprintf("type=bind,source=%s,target=%s", m.Host, m.In)
		if !m.Write {
			spec += ",readonly"
		}
		args = append(args, "--mount", spec)
	}
	workdir := s.Workdir
	if workdir == "" {
		workdir = homeIn
	}
	args = append(args, "--workdir", workdir)
	if s.Name != "" {
		args = append(args, "--name", s.Name)
	}
	if s.CPUs > 0 {
		args = append(args, "--cpus", strconv.Itoa(s.CPUs))
	}
	if s.MemMB > 0 {
		args = append(args, "--memory", strconv.FormatInt(s.MemMB, 10)+"m")
	}

	// The basics a shell expects. The image sets HOME=/root and knows
	// nothing of the account, so without these a session starts in the
	// image's own home and writes everything it does into a container that
	// is thrown away when it exits.
	env := map[string]string{
		"HOME":    homeIn,
		"USER":    s.User,
		"LOGNAME": s.User,
		"TMPDIR":  homeIn + "/.tmp",
		"TERM":    "xterm-256color",
	}
	for k, v := range s.Env {
		env[k] = v
	}

	// Sorted, so a session's command line is reproducible and a diff between
	// two of them means something.
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--env", k+"="+env[k])
	}
	if !s.Network {
		// A batch-style run gets no network, matching every other backend.
		// The flag is the runtime's own; sessions always pass Network.
		args = append(args, "--network", "none")
	}

	args = append(args, image)
	return append(args, s.Argv...)
}

// Command builds the process that runs a session.
func (r Runtime) Command(ctx context.Context, s Spec) *exec.Cmd {
	return exec.CommandContext(ctx, r.Bin, Args(r, s)...)
}

// Detect finds a usable container runtime, or explains what is missing.
//
// The reason is returned rather than logged because it ends up in front of
// whoever tried to log in, and "install this" is the only useful thing to say
// to them.
func Detect(ctx context.Context, bin, image string) (*Runtime, string) {
	if runtime.GOOS != "darwin" {
		return nil, "the container runtime is a macOS arrangement"
	}
	if runtime.GOARCH != "arm64" {
		return nil, "Apple's container runtime needs Apple silicon; this Mac is " +
			runtime.GOARCH
	}
	if bin == "" {
		bin = "container"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, "Apple's container runtime is not installed. Sessions on a Mac " +
			"run inside a Linux container because a Seatbelt profile cannot hide " +
			"the host's filesystem.\n" +
			"Install it from https://github.com/apple/container/releases, then run:\n" +
			"  container system start"
	}
	// Installed is not the same as running: the service is started
	// separately and a session that fails at launch is a worse message than
	// one refused up front.
	ctx, cancel := context.WithTimeout(ctx, StatusTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "system", "status").CombinedOutput()
	if err != nil {
		return nil, fmt.Sprintf("the container runtime is installed but not running "+
			"(%s).\nStart it with:  container system start", firstLine(out))
	}
	return &Runtime{Bin: path, Image: image}, ""
}

// PullImage fetches the session image.
func (r Runtime) PullImage(ctx context.Context) ([]byte, error) {
	image := r.Image
	if image == "" {
		image = DefaultImage
	}
	return exec.CommandContext(ctx, r.Bin, "image", "pull", image).CombinedOutput()
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return "no output"
	}
	return s
}

// VerifyShared checks that a directory really is shared with the container.
//
// Some host paths mount without error and without sharing: a bind from the
// per-user temp area under /var/folders appears inside the container, accepts
// writes, and propagates none of them back. A session rooted there would look
// entirely normal and lose everything the moment it exited.
//
// So this writes a file from inside and looks for it outside. It costs one
// container start, once, which is the right price for not silently destroying
// somebody's work.
func VerifyShared(ctx context.Context, r Runtime, dir string) error {
	probe := ".shome-mount-probe"
	full := filepath.Join(dir, probe)
	os.Remove(full)
	defer os.Remove(full)

	// Named, and stopped afterwards. An unnamed container cannot be stopped
	// or swept -- it is indistinguishable from anything else on the machine
	// -- and --rm has not proved reliable enough to be the only thing
	// standing between a daemon restart and another abandoned virtual
	// machine.
	name := fmt.Sprintf("%sprobe-%d", Prefix, time.Now().UnixNano())
	defer func() {
		stop := exec.Command(r.Bin, "stop", "--time", "2", name)
		_ = stop.Run()
	}()

	cmd := r.Command(ctx, Spec{
		Home: dir, HomeIn: "/probe", Name: name,
		Argv: []string{"/bin/sh", "-c", "echo shared > /probe/" + probe},
	})
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("could not start a container to check %s: %w (%s)",
			dir, err, firstLine(out))
	}
	b, err := os.ReadFile(full)
	if err != nil || !strings.Contains(string(b), "shared") {
		return fmt.Errorf("%s is not shared with containers: a session there "+
			"would appear to work and lose everything it wrote.\n"+
			"The container runtime mounts some host paths -- the per-user temp "+
			"area under /var/folders among them -- without propagating writes "+
			"back.\nPut the shome root somewhere shareable, such as under "+
			"/Users/Shared, and rejoin", dir)
	}
	return nil
}

// Gateway is the host's address on the container network.
//
// The address a container reaches this machine on. Discovered rather than
// assumed: the runtime picks the subnet, and hard-coding 192.168.64.1 would
// work until it did not.
//
// It is a host-only bridge, so something bound there is reachable from a
// container on this machine and from nowhere else -- which is what makes it
// a reasonable place to put the control bridge a session talks to.
func (r Runtime) Gateway(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, StatusTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, r.Bin, "network", "inspect", "default").Output()
	if err != nil {
		return "", fmt.Errorf("ask the container runtime for its network: %w", err)
	}
	var nets []struct {
		Status struct {
			IPv4Gateway string `json:"ipv4Gateway"`
		} `json:"status"`
	}
	if err := json.Unmarshal(out, &nets); err != nil {
		return "", fmt.Errorf("read the container network: %w", err)
	}
	for _, n := range nets {
		if g := strings.TrimSpace(n.Status.IPv4Gateway); g != "" {
			return g, nil
		}
	}
	return "", fmt.Errorf("the container network reports no gateway address")
}

// Stats is a running container's resource usage.
type Stats struct {
	MemoryUsageBytes int64  `json:"memoryUsageBytes"`
	MemoryLimitBytes int64  `json:"memoryLimitBytes"`
	CPUUsageUsec     int64  `json:"cpuUsageUsec"`
	NumProcesses     int    `json:"numProcesses"`
	ID               string `json:"id"`
}

// Usage reports what a container is using.
//
// Needed because a containerised job's memory cannot be seen from the host
// process tree: the only host process is the launcher, and the job itself is
// inside a virtual machine. Polling the tree reports a few megabytes for a
// job using gigabytes, which would leave every containerised job's peak
// memory recorded as nothing.
func (r Runtime) Usage(ctx context.Context, name string) (Stats, error) {
	// Bounded: a session's usage is polled repeatedly, and a runtime that
	// stopped answering must not take the poller with it.
	ctx, cancel := context.WithTimeout(ctx, StatusTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, r.Bin, "stats",
		"--format", "json", "--no-stream", name).Output()
	if err != nil {
		return Stats{}, err
	}
	var rows []Stats
	if err := json.Unmarshal(out, &rows); err != nil {
		return Stats{}, fmt.Errorf("read container stats: %w", err)
	}
	for _, s := range rows {
		if s.ID == name || len(rows) == 1 {
			return s, nil
		}
	}
	return Stats{}, fmt.Errorf("no stats for %s", name)
}
