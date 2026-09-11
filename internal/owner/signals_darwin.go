//go:build darwin && cgo

package owner

/*
#cgo CFLAGS: -x objective-c -fmodules -fobjc-arc
#cgo LDFLAGS: -framework Foundation
#import <Foundation/Foundation.h>

// shome_thermal_state maps NSProcessInfoThermalState onto 0..3.
//
// pmset -g therm reports nothing on Apple Silicon ("No thermal warning level
// has been recorded") and machdep.xcpm does not exist there, so this is the
// only reliable source.
static int shome_thermal_state(void) {
    @autoreleasepool {
        switch ([[NSProcessInfo processInfo] thermalState]) {
            case NSProcessInfoThermalStateNominal:  return 0;
            case NSProcessInfoThermalStateFair:     return 1;
            case NSProcessInfoThermalStateSerious:  return 2;
            case NSProcessInfoThermalStateCritical: return 3;
        }
        return 0;
    }
}
*/
import "C"

import (
	"bufio"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// DarwinSensor reads the owner-activity signals on macOS.
//
// Most come from small command-line tools rather than private APIs: they are
// stable across releases, need no entitlements, and keep this readable. They
// are polled on an interval rather than per-decision, because a handful of
// fork/exec per second on the owner's machine is exactly the kind of overhead
// this feature exists to avoid.
type DarwinSensor struct{}

func (DarwinSensor) Read() Signals {
	s := Signals{Now: time.Now()}
	s.UserIdle = hidIdle()
	s.OnBattery = onBattery()
	s.ScreenLocked = screenLocked()
	s.MemoryPressure = memoryPressure()
	s.Thermal = thermalState()
	s.RunningApps = runningApps()
	return s
}

func run(name string, args ...string) string {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// hidIdle reports time since the last keyboard or mouse event.
func hidIdle() time.Duration {
	out := run("/usr/sbin/ioreg", "-c", "IOHIDSystem")
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, "HIDIdleTime") {
			continue
		}
		i := strings.LastIndex(line, "=")
		if i < 0 {
			continue
		}
		ns, err := strconv.ParseInt(strings.TrimSpace(line[i+1:]), 10, 64)
		if err != nil {
			continue
		}
		return time.Duration(ns) * time.Nanosecond
	}
	// Unknown idle time must not look like "owner is away": defaulting to zero
	// keeps the protective triggers armed.
	return 0
}

func onBattery() bool {
	return strings.Contains(run("/usr/bin/pmset", "-g", "ps"), "Battery Power")
}

func screenLocked() bool {
	out := run("/usr/sbin/ioreg", "-n", "Root", "-d1", "-r", "-k", "CGSSessionScreenIsLocked")
	return strings.Contains(out, "CGSSessionScreenIsLocked") && strings.Contains(out, "Yes")
}

// memoryPressure maps kern.memorystatus_vm_pressure_level (1/2/4).
func memoryPressure() string {
	v := strings.TrimSpace(run("/usr/sbin/sysctl", "-n", "kern.memorystatus_vm_pressure_level"))
	switch v {
	case "1":
		return "nominal"
	case "2":
		return "warn"
	case "4":
		return "critical"
	}
	return "nominal"
}

func thermalState() string {
	switch int(C.shome_thermal_state()) {
	case 1:
		return "fair"
	case 2:
		return "serious"
	case 3:
		return "critical"
	}
	return "nominal"
}

// runningApps lists GUI applications by bundle name.
//
// Only .app processes are considered: the policy names things like "Xcode" or
// "Final Cut Pro", and matching against every daemon on the system would make
// accidental matches far too easy.
func runningApps() []string {
	out := run("/bin/ps", "-axo", "comm=")
	seen := map[string]bool{}
	var apps []string
	for _, line := range strings.Split(out, "\n") {
		i := strings.Index(line, ".app/")
		if i < 0 {
			continue
		}
		// ".../Xcode.app/Contents/MacOS/Xcode" -> "Xcode"
		prefix := line[:i]
		name := prefix[strings.LastIndex(prefix, "/")+1:]
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		apps = append(apps, name)
	}
	return apps
}

// Live is true: this sensor reads real HID idle time, power source, thermal
// and memory pressure from the system.
func (DarwinSensor) Live() bool { return true }
