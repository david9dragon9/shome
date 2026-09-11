package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// This file turns "run a daemon" into something the user does not have to
// think about. Before it existed, starting shome meant backgrounding a process
// with `&`, remembering four flags, and losing the logs when the terminal
// closed; stopping it meant finding the pid by hand. Both are now one word.

// PIDPath is where a running daemon records its process id.
func PIDPath(root string) string { return filepath.Join(root, "shome.pid") }

// LogPath is where a detached daemon's output goes.
func LogPath(root string) string { return filepath.Join(root, "shome.log") }

// configPath records how this machine connects, so restarting needs no flags.
func configPath(root string) string { return filepath.Join(root, "node.json") }

// Role is what a machine does for the cluster.
type Role string

const (
	RoleController Role = "controller"
	RoleAgent      Role = "agent"
	RoleNone       Role = ""
)

// SavedConfig is the persisted answer to "what was this machine doing?", so
// `shome up` after a reboot needs no arguments and cannot pick a different
// node name than it used last time.
type SavedConfig struct {
	Role       Role   `json:"role"`
	Node       string `json:"node"`
	Controller string `json:"controller,omitempty"`
	ServerName string `json:"server_name,omitempty"`
	Listen     string `json:"listen,omitempty"`
	Advertise  string `json:"advertise,omitempty"`
	WebAddr    string `json:"web_addr,omitempty"`
	SSHAddr    string `json:"ssh_addr,omitempty"`
	// MonitorAddr is the machine owner's local page. Loopback only, and on
	// every node rather than just the controller, because every machine has
	// an owner.
	MonitorAddr string `json:"monitor_addr,omitempty"`
}

// SaveConfig records this machine's role. Best effort: a machine that cannot
// write its own state directory has larger problems, and failing to remember
// should never stop a daemon that is otherwise about to work.
func SaveConfig(root string, c SavedConfig) {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(configPath(root), append(b, '\n'), 0o600)
}

// SaveAgentConfig records an agent's connection details.
func SaveAgentConfig(root string, cfg AgentConfig) {
	SaveConfig(root, SavedConfig{
		Role:        RoleAgent,
		Node:        cfg.Node,
		Controller:  cfg.Controller,
		ServerName:  cfg.ServerName,
		MonitorAddr: cfg.MonitorAddr,
	})
}

// LoadConfig returns what this machine was last doing, if anything.
func LoadConfig(root string) (SavedConfig, bool) {
	b, err := os.ReadFile(configPath(root))
	if err != nil {
		return SavedConfig{}, false
	}
	var c SavedConfig
	if json.Unmarshal(b, &c) != nil || c.Role == RoleNone {
		return SavedConfig{}, false
	}
	return c, true
}

// Status reports the running daemon's pid, if there is one.
//
// A stale pid file is treated as "not running" rather than an error: crashes
// and hard reboots leave them behind, and refusing to start because of a file
// left by a process that died last week is a bad failure mode.
func Status(root string) (pid int, running bool) {
	b, err := os.ReadFile(PIDPath(root))
	if err != nil {
		return 0, false
	}
	pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	if !alive(pid) {
		return pid, false
	}
	return pid, true
}

// alive reports whether a process exists and we may signal it. Signal 0
// performs the permission and existence checks without delivering anything.
func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || err == os.ErrPermission
}

// WritePID records the current process as root's daemon.
func WritePID(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	return os.WriteFile(PIDPath(root), []byte(fmt.Sprintln(os.Getpid())), 0o600)
}

// ClearPID removes the pid file if it still names this process, so a clean
// exit does not leave a file that makes the next start think it is running.
func ClearPID(root string) {
	b, err := os.ReadFile(PIDPath(root))
	if err != nil {
		return
	}
	if strings.TrimSpace(string(b)) == strconv.Itoa(os.Getpid()) {
		os.Remove(PIDPath(root))
	}
}

// StartDetached re-executes this binary with the given arguments as a
// background process that survives the terminal closing, sending its output to
// the log file.
//
// Re-executing rather than forking a goroutine keeps one code path: the
// detached daemon runs exactly the command a user could have typed, which
// means `shome logs` shows the same thing they would have seen in the
// foreground.
//
// The returned channel carries the child's exit if it happens while this
// process is still alive. Callers need it because a daemon that dies during
// startup -- a port clash, an unreadable state directory -- must be reported
// as a failure rather than as a successful launch. Checking liveness by
// signalling the pid does not work here: an unreaped child is a zombie, and a
// zombie accepts signal 0 exactly like a healthy process, so a crashed daemon
// would look alive for as long as the CLI kept running.
func StartDetached(root string, args []string) (pid int, exited <-chan error, err error) {
	if pid, running := Status(root); running {
		return pid, nil, ErrAlreadyRunning
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return 0, nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return 0, nil, fmt.Errorf("locate this binary: %w", err)
	}
	logf, err := os.OpenFile(LogPath(root), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, nil, fmt.Errorf("open log: %w", err)
	}
	defer logf.Close()

	cmd := exec.Command(self, args...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.Stdin = nil
	cmd.SysProcAttr = detachAttr()
	if err := cmd.Start(); err != nil {
		return 0, nil, err
	}
	ch := make(chan error, 1)
	// Reaping only matters while this process lives; once it exits the child
	// is reparented to init, which reaps it. Setsid means it survives either
	// way, so waiting here costs nothing and buys an accurate exit signal.
	go func() { ch <- cmd.Wait() }()
	return cmd.Process.Pid, ch, nil
}

// ErrAlreadyRunning means a daemon for this root is already up. Callers report
// it as ordinary output, not a failure -- `shome up` twice is not a mistake.
var ErrAlreadyRunning = fmt.Errorf("already running")

// Stop asks the daemon to exit and waits for it to go.
//
// SIGTERM only. shome jobs run as child processes with scratch directories and
// sandbox state to clean up, and SIGKILL would strand both; a controller that
// will not stop is something the user should see and decide about, not
// something to paper over with a harder signal.
func Stop(root string, timeout time.Duration) (stopped bool, err error) {
	pid, running := Status(root)
	if !running {
		os.Remove(PIDPath(root))
		return false, nil
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false, err
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		return false, fmt.Errorf("signal pid %d: %w", pid, err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			os.Remove(PIDPath(root))
			return true, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false, fmt.Errorf("process %d did not stop within %s; "+
		"check %s, then stop it yourself with:  kill %d", pid, timeout, LogPath(root), pid)
}
