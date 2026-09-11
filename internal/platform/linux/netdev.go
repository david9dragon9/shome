//go:build linux

package linux

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/davidwu/shome/internal/platform"
)

// readNetCounters sums /proc/net/dev across real interfaces.
//
// Loopback is excluded: it carries the cluster's own control traffic on a
// controller that also runs an agent, and counting it would report a busy
// network on a machine that has sent nothing anywhere.
func readNetCounters(now time.Time) platform.NetCounters {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return platform.NetCounters{}
	}
	defer f.Close()

	c := platform.NetCounters{At: now}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue // the two header lines
		}
		name = strings.TrimSpace(name)
		if name == "lo" || strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "veth") {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			continue
		}
		rx, err1 := strconv.ParseUint(fields[0], 10, 64)
		tx, err2 := strconv.ParseUint(fields[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		c.RxBytes += rx
		c.TxBytes += tx
	}
	return c
}
