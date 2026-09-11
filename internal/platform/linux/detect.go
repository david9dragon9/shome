//go:build linux

package linux

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Detect probes the environment. It never fails: an unknown capability is
// simply absent, and the tier calculation downgrades accordingly.
func Detect() Environment {
	e := Environment{IsRoot: os.Geteuid() == 0}
	e.Cgroup, e.CgroupRoot = detectCgroup()
	e.CanDelegate = cgroupWritable(e.CgroupRoot)
	e.CanCreateUser = e.IsRoot && lookPath("useradd") != ""
	e.HasLandlock = landlockAvailable()
	e.HasSeccomp = seccompAvailable()
	e.BwrapPath = lookPath("bwrap")
	e.HasBwrap = e.BwrapPath != ""
	e.Container = detectContainer()
	e.NvidiaGPUs = detectNvidia()
	return e
}

func lookPath(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
}

// detectCgroup distinguishes the unified hierarchy from legacy v1.
//
// The unified hierarchy is mounted as cgroup2fs; v1 shows individual
// controller mounts. Crostini is v1-only, which is why this matters.
func detectCgroup() (CgroupMode, string) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return CgroupAbsent, ""
	}
	defer f.Close()
	var v1Root string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		switch fields[2] {
		case "cgroup2":
			return CgroupV2, fields[1]
		case "cgroup":
			if v1Root == "" {
				v1Root = filepath.Dir(fields[1])
			}
		}
	}
	if v1Root != "" {
		return CgroupV1, v1Root
	}
	return CgroupAbsent, ""
}

// cgroupWritable reports whether we may create our own subtree.
//
// Checked by attempting a real mkdir rather than inspecting ownership:
// delegation rules vary with systemd version and session type, and the only
// answer that matters is whether the operation succeeds.
func cgroupWritable(root string) bool {
	if root == "" {
		return false
	}
	probe := filepath.Join(root, "shome-probe")
	if err := os.Mkdir(probe, 0o755); err != nil {
		return false
	}
	os.Remove(probe)
	return true
}

// landlockAvailable reports whether the Landlock LSM is usable.
func landlockAvailable() bool {
	b, err := os.ReadFile("/sys/kernel/security/lsm")
	if err == nil && strings.Contains(string(b), "landlock") {
		return true
	}
	// Landlock can be compiled in without appearing in the LSM list on some
	// kernels; the ABI file is the more reliable signal.
	if _, err := os.Stat("/sys/kernel/security/landlock"); err == nil {
		return true
	}
	return false
}

func seccompAvailable() bool {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	return strings.Contains(string(b), "Seccomp:")
}

// detectContainer identifies common nested environments.
func detectContainer() string {
	if b, err := os.ReadFile("/proc/1/environ"); err == nil {
		if strings.Contains(string(b), "container=lxc") {
			// Crostini runs the Linux environment in LXD inside a VM.
			if _, err := os.Stat("/opt/google/cros-containers"); err == nil {
				return "crostini"
			}
			return "lxc"
		}
	}
	if _, err := os.Stat("/opt/google/cros-containers"); err == nil {
		return "crostini"
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return "docker"
	}
	if b, err := os.ReadFile("/proc/version"); err == nil &&
		strings.Contains(strings.ToLower(string(b)), "microsoft") {
		return "wsl"
	}
	return ""
}

// detectNvidia enumerates CUDA devices via nvidia-smi.
//
// nvidia-smi rather than NVML: it avoids a cgo dependency on the driver
// libraries, which would make shome unbuildable on machines without them.
func detectNvidia() []NvidiaGPU {
	smi := lookPath("nvidia-smi")
	if smi == "" {
		return nil
	}
	out, err := exec.Command(smi,
		"--query-gpu=index,name,memory.total,uuid", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil
	}
	var gpus []NvidiaGPU
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, ",")
		if len(parts) < 4 {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			continue
		}
		mib, err := strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64)
		if err != nil {
			continue
		}
		gpus = append(gpus, NvidiaGPU{
			Index:    idx,
			Name:     strings.TrimSpace(parts[1]),
			MemBytes: mib << 20, // nvidia-smi reports MiB with nounits
			UUID:     strings.TrimSpace(parts[3]),
		})
	}
	return gpus
}
