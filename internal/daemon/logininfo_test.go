package daemon

import (
	"github.com/davidwu/shome/internal/ctl"
	"strings"
	"testing"
)

// The instructions `shome admin enroll` prints have to name a real address.
// They used to be worked out from local state, which produced a literal
// "<controller>" placeholder anywhere except on the controller itself; the
// controller now reports this, so these are the rules it reports by.
func TestLoginInfoUsesAdvertiseAddressesFirst(t *testing.T) {
	got := loginInfo(ControllerConfig{
		SSHAddr:   "0.0.0.0:2222",
		Advertise: "vpn.example.test,10.9.9.9:7817",
	}, "[::]:2222")
	if !got.Enabled {
		t.Error("a configured ssh address should report enabled")
	}
	if got.Port != 2222 {
		t.Errorf("port = %d, want 2222", got.Port)
	}
	if len(got.Hosts) < 2 {
		t.Fatalf("hosts = %v, want the advertise entries first", got.Hosts)
	}
	// An admin who set advertise did so precisely to say which address to
	// hand out, so those come before whatever the interfaces say.
	if got.Hosts[0] != "vpn.example.test" {
		t.Errorf("hosts[0] = %q, want the first advertise entry", got.Hosts[0])
	}
	// An advertise entry may carry the agent API's port, which is not the
	// login node's; only the host part is usable here.
	if got.Hosts[1] != "10.9.9.9" {
		t.Errorf("hosts[1] = %q, want the host without its port", got.Hosts[1])
	}
	for _, h := range got.Hosts {
		if strings.Contains(h, ":7817") {
			t.Errorf("an agent-API port leaked into an ssh address: %q", h)
		}
	}
}

// The port comes from the socket actually configured, not a constant.
func TestLoginInfoReadsTheConfiguredPort(t *testing.T) {
	if got := loginInfo(ControllerConfig{SSHAddr: "0.0.0.0:2022"}, ""); got.Port != 2022 {
		t.Errorf("port = %d, want 2022", got.Port)
	}
	// A default when it is unparseable rather than zero, which would render
	// as "ssh -p 0".
	if got := loginInfo(ControllerConfig{SSHAddr: "nonsense"}, ""); got.Port != 2222 {
		t.Errorf("port = %d, want the 2222 default", got.Port)
	}
}

// The port a login node actually bound wins over the one requested, which
// is what "port 0" means and what makes the printed instruction usable.
func TestLoginInfoPrefersThePortItReallyBound(t *testing.T) {
	got := loginInfo(ControllerConfig{SSHAddr: "0.0.0.0:0"}, "[::]:49213")
	if got.Port != 49213 {
		t.Errorf("port = %d, want the bound 49213", got.Port)
	}
}

// No login node means an enrollment code is useless, and saying so beats
// printing instructions that cannot work.
//
// loginInfo is only called once one is listening, so "disabled" is expressed
// by never calling it -- the zero LoginInfo. This is that contract: a
// configured-but-unstarted login node must not be reported as reachable.
func TestLoginInfoReportsDisabled(t *testing.T) {
	var off ctl.LoginInfo
	if off.Enabled {
		t.Error("the zero LoginInfo must be disabled")
	}
	if got := loginInfo(ControllerConfig{SSHAddr: "0.0.0.0:2222"}, "[::]:2222"); !got.Enabled {
		t.Error("a login node that is listening must report enabled")
	}
}

// Duplicates would make the "these also work" list repeat itself.
func TestLoginInfoDeduplicates(t *testing.T) {
	got := loginInfo(ControllerConfig{
		SSHAddr:   "0.0.0.0:2222",
		Advertise: "10.1.1.1, 10.1.1.1 ,,10.1.1.2",
	}, "[::]:2222")
	seen := map[string]int{}
	for _, h := range got.Hosts {
		seen[h]++
	}
	for h, n := range seen {
		if n > 1 {
			t.Errorf("%q appears %d times", h, n)
		}
	}
	if seen["10.1.1.1"] != 1 || seen["10.1.1.2"] != 1 {
		t.Errorf("hosts = %v, want both advertise entries once each", got.Hosts)
	}
	if seen[""] != 0 {
		t.Error("an empty advertise entry became a host")
	}
}

// IPv4 before IPv6: almost every reader wants the IPv4 one, and burying it
// under privacy addresses is how the useful line gets missed.
func TestLANAddressesPutIPv4First(t *testing.T) {
	got := LANAddresses()
	if len(got) == 0 {
		t.Skip("this machine has no non-loopback address")
	}
	seenV6 := false
	for _, a := range got {
		isV6 := strings.Contains(a, ":")
		if isV6 {
			seenV6 = true
			continue
		}
		if seenV6 {
			t.Errorf("IPv4 address %q came after an IPv6 one: %v", a, got)
		}
	}
	for _, a := range got {
		if strings.HasPrefix(a, "127.") || a == "::1" {
			t.Errorf("loopback address %q was offered as reachable", a)
		}
	}
}
