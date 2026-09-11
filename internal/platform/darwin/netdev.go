//go:build darwin && cgo

package darwin

/*
#include <sys/types.h>
#include <sys/socket.h>
#include <net/if.h>
#include <net/if_dl.h>
#include <ifaddrs.h>
#include <string.h>

// shome_net_counters sums interface byte counters.
//
// macOS has no /proc, and no sysctl that reports this without walking the
// routing table. getifaddrs with AF_LINK entries is the documented way: each
// carries an if_data struct with the driver's own counters.
//
// Loopback is skipped, because on a controller that also runs an agent it
// carries the cluster's own control traffic -- counting it would report a busy
// network on a machine that has sent nothing off-box.
static int shome_net_counters(unsigned long long *rx, unsigned long long *tx) {
    struct ifaddrs *ifap, *p;
    if (getifaddrs(&ifap) != 0) return -1;
    *rx = 0; *tx = 0;
    for (p = ifap; p != NULL; p = p->ifa_next) {
        if (p->ifa_addr == NULL || p->ifa_addr->sa_family != AF_LINK) continue;
        if (p->ifa_flags & IFF_LOOPBACK) continue;
        struct if_data *d = (struct if_data *)p->ifa_data;
        if (d == NULL) continue;
        *rx += (unsigned long long)d->ifi_ibytes;
        *tx += (unsigned long long)d->ifi_obytes;
    }
    freeifaddrs(ifap);
    return 0;
}
*/
import "C"

import (
	"time"

	"github.com/davidwu/shome/internal/platform"
)

func readNetCounters(now time.Time) platform.NetCounters {
	var rx, tx C.ulonglong
	if C.shome_net_counters(&rx, &tx) != 0 {
		return platform.NetCounters{}
	}
	return platform.NetCounters{RxBytes: uint64(rx), TxBytes: uint64(tx), At: now}
}
