package discovery

import (
	"net"
	"testing"
)

func TestBroadcastFor(t *testing.T) {
	// Find a real non-loopback IPv4 interface address and confirm BroadcastFor
	// returns a plausible directed-broadcast (host bits all 1).
	ifaces, _ := net.Interfaces()
	var found bool
	for _, ifc := range ifaces {
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || ipn.IP.IsLoopback() {
				continue
			}
			b, err := BroadcastFor(ipn.IP)
			if err != nil {
				t.Fatalf("BroadcastFor(%v): %v", ipn.IP, err)
			}
			// Every host bit (where mask is 0) must be set in the broadcast.
			ip := ipn.IP.To4()
			mask := ipn.Mask
			if len(mask) != 4 {
				mask = mask[len(mask)-4:]
			}
			for i := 0; i < 4; i++ {
				want := ip[i] | ^mask[i]
				if b.To4()[i] != want {
					t.Errorf("octet %d: got %d want %d", i, b.To4()[i], want)
				}
			}
			found = true
		}
	}
	if !found {
		t.Skip("no non-loopback IPv4 interface to test against")
	}
}

func TestBroadcastForRejectsUnknown(t *testing.T) {
	// An address owned by no local interface must error, not panic.
	if _, err := BroadcastFor(net.ParseIP("203.0.113.7")); err == nil {
		t.Error("expected error for an address no interface owns")
	}
}

func TestLocalIPTowardLoopback(t *testing.T) {
	// Routing toward loopback yields a loopback source IP; mainly checks the
	// call works and returns IPv4 without error.
	ip, err := LocalIPToward("127.0.0.1")
	if err != nil {
		t.Fatalf("LocalIPToward: %v", err)
	}
	if ip.To4() == nil {
		t.Errorf("got non-IPv4 %v", ip)
	}
}

func TestAutoAdvertiseAddrsAcceptsHostPort(t *testing.T) {
	// Should accept both "host" and "host:port" forms without error for a
	// routable target (loopback is always routable).
	for _, target := range []string{"127.0.0.1", "127.0.0.1:2222"} {
		pip, bcast, err := AutoAdvertiseAddrs(target)
		if err != nil {
			t.Errorf("AutoAdvertiseAddrs(%q): %v", target, err)
			continue
		}
		if net.ParseIP(pip) == nil || net.ParseIP(bcast) == nil {
			t.Errorf("AutoAdvertiseAddrs(%q) = %q,%q — not valid IPs", target, pip, bcast)
		}
	}
}
