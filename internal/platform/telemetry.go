package platform

import (
	"context"
	"time"
)

// Telemetry is what a machine is actually doing right now, as opposed to what
// the scheduler has allocated on it.
//
// The distinction matters and is the reason this type exists. `sinfo` reports
// allocation: how much of a node's advertised capacity has been handed out.
// A dashboard needs the other number -- a node with every core allocated to a
// job that is blocked on I/O is idle, and a node with nothing allocated can be
// pinned by the owner's own work. Only one of those is visible in a scheduler's
// bookkeeping.
//
// Fields that a platform cannot measure are set to Unknown rather than zero.
// Zero is a real, meaningful reading; conflating "nothing" with "no idea" is
// how a dashboard ends up confidently wrong.
type Telemetry struct {
	At time.Time `json:"at"`

	// CPUPercent is 0..100 across all cores, so a fully loaded 10-core machine
	// reads 100, not 1000. Averaged over the interval since the last sample.
	CPUPercent float64 `json:"cpu_percent"`
	LoadAvg1   float64 `json:"load_avg_1"`

	MemUsedBytes  int64 `json:"mem_used_bytes"`
	MemTotalBytes int64 `json:"mem_total_bytes"`
	SwapUsedBytes int64 `json:"swap_used_bytes"`

	// GPUPercent and GPUMemUsedBytes are Unknown on macOS: there is no public
	// API for Metal utilisation, and inventing a number from allocation would
	// be a lie in the one place a user is most likely to trust the display.
	GPUPercent      float64 `json:"gpu_percent"`
	GPUMemUsedBytes int64   `json:"gpu_mem_used_bytes"`

	// Owner-facing signals. A dashboard that shows a node as busy without
	// showing that its owner is sitting at the keyboard is missing the point
	// of this cluster.
	Thermal        string  `json:"thermal,omitempty"`
	MemoryPressure string  `json:"memory_pressure,omitempty"`
	OnBattery      bool    `json:"on_battery,omitempty"`
	BatteryPercent float64 `json:"battery_percent"`
	UserIdleSec    float64 `json:"user_idle_sec"`

	// Disk, for the filesystem holding shome's state, and for shome's own
	// tree within it. Both matter: the owner's contribution cap bounds the
	// second, while the first is what actually runs out.
	DiskTotalBytes int64 `json:"disk_total_bytes"`
	DiskFreeBytes  int64 `json:"disk_free_bytes"`
	ShomeDiskBytes int64 `json:"shome_disk_bytes"`
	InodesTotal    int64 `json:"inodes_total"`
	InodesFree     int64 `json:"inodes_free"`
	ShomeInodes    int64 `json:"shome_inodes"`

	// Throughput across the machine's real interfaces, in bytes per second.
	// Machine-wide rather than per-job: attributing bytes to a process needs
	// per-socket accounting neither platform exposes cheaply, and a figure
	// labelled per-job that was not would mislead.
	NetRxBytesPerSec float64 `json:"net_rx_bytes_per_sec"`
	NetTxBytesPerSec float64 `json:"net_tx_bytes_per_sec"`

	Uptime time.Duration `json:"uptime"`
}

// KnownDisk reports whether the filesystem figures were measured.
func (t Telemetry) KnownDisk() bool { return t.DiskTotalBytes > 0 }

// KnownInodes reports whether the filesystem tracks inodes. Some do not, and
// report zero rather than failing.
func (t Telemetry) KnownInodes() bool { return t.InodesTotal > 0 }

// KnownNet reports whether throughput was measured.
func (t Telemetry) KnownNet() bool { return t.NetRxBytesPerSec >= 0 }

// DiskPercent is filesystem use as a percentage, or Unknown.
func (t Telemetry) DiskPercent() float64 {
	if t.DiskTotalBytes <= 0 {
		return Unknown
	}
	return 100 * float64(t.DiskTotalBytes-t.DiskFreeBytes) / float64(t.DiskTotalBytes)
}

// Unknown marks a metric this platform cannot measure.
//
// Negative because every real reading of every field here is non-negative, so
// one sentinel works for counts, bytes and percentages alike, and any caller
// that forgets to check gets an obviously wrong value rather than a plausible
// one.
const Unknown = -1.0

// UnknownBytes is Unknown for the integer-valued fields.
const UnknownBytes int64 = -1

// KnownCPU reports whether CPUPercent was measured.
func (t Telemetry) KnownCPU() bool { return t.CPUPercent >= 0 }

// KnownGPU reports whether GPU utilisation was measured.
func (t Telemetry) KnownGPU() bool { return t.GPUPercent >= 0 }

// KnownGPUMem reports whether GPU memory use was measured.
func (t Telemetry) KnownGPUMem() bool { return t.GPUMemUsedBytes >= 0 }

// MemPercent is memory in use as a percentage, or Unknown.
func (t Telemetry) MemPercent() float64 {
	if t.MemTotalBytes <= 0 {
		return Unknown
	}
	return 100 * float64(t.MemUsedBytes) / float64(t.MemTotalBytes)
}

// UnknownTelemetry is the reading a platform with no implementation returns:
// honest about knowing nothing, rather than a zero-valued struct that would
// render as a healthy, idle machine.
func UnknownTelemetry(now time.Time) Telemetry {
	return Telemetry{
		At:               now,
		CPUPercent:       Unknown,
		LoadAvg1:         Unknown,
		MemUsedBytes:     UnknownBytes,
		MemTotalBytes:    UnknownBytes,
		SwapUsedBytes:    UnknownBytes,
		GPUPercent:       Unknown,
		GPUMemUsedBytes:  UnknownBytes,
		BatteryPercent:   Unknown,
		UserIdleSec:      Unknown,
		DiskTotalBytes:   UnknownBytes,
		DiskFreeBytes:    UnknownBytes,
		ShomeDiskBytes:   UnknownBytes,
		InodesTotal:      UnknownBytes,
		InodesFree:       UnknownBytes,
		ShomeInodes:      UnknownBytes,
		NetRxBytesPerSec: Unknown,
		NetTxBytesPerSec: Unknown,
	}
}

// TelemetrySource is implemented by backends that can report live machine
// utilisation.
//
// Separate from Backend so that a platform can support running jobs without
// also having to implement metrics, and so adding it to a backend is not a
// breaking change to the isolation interface -- which is the part that must
// not churn.
type TelemetrySource interface {
	Telemetry(context.Context) (Telemetry, error)
}

// Sample asks a backend for telemetry, falling back to an honest "unknown"
// reading for backends that do not implement TelemetrySource.
func Sample(ctx context.Context, b Backend, now time.Time) Telemetry {
	src, ok := b.(TelemetrySource)
	if !ok {
		return UnknownTelemetry(now)
	}
	t, err := src.Telemetry(ctx)
	if err != nil {
		return UnknownTelemetry(now)
	}
	if t.At.IsZero() {
		t.At = now
	}
	return t
}
