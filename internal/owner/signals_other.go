//go:build !(darwin && cgo)

package owner

import "time"

// DarwinSensor is a no-op away from macOS. The Linux implementation belongs
// with the Linux node backend in M6; until then this reports "owner absent",
// which is the correct default for a headless machine.
type DarwinSensor struct{}

func (DarwinSensor) Read() Signals {
	return Signals{Now: time.Now(), UserIdle: 24 * time.Hour}
}

// Live is false: this reports a fixed "owner absent", so reactive yield rules
// cannot fire here. Contribution caps, availability schedules and an explicit
// `shome pause` all still work -- those need no live signals.
func (DarwinSensor) Live() bool { return false }
