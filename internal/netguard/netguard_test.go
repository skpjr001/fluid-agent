package netguard

import (
	"net/netip"
	"testing"

	"github.com/skpjr001/fluid-agent/internal/metrics"
)

func TestBlockedAddresses(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0",
		"224.0.0.1", "255.255.255.255", "198.18.0.1", "::1", "::", "fe80::1", "fc00::1", "fd12::1", "ff02::1",
		"::ffff:127.0.0.1",         // IPv4-mapped
		"::ffff:169.254.169.254",   // mapped metadata
		"::127.0.0.1",              // IPv4-compatible
		"64:ff9b::a9fe:a9fe",       // NAT64 -> 169.254.169.254
		"64:ff9b::7f00:1",          // NAT64 -> 127.0.0.1
		"2002:7f00:1::1",           // 6to4 -> 127.0.0.1
		"2002:a9fe:a9fe::1",        // 6to4 -> 169.254.169.254
		"2001:0:0:0:0:0:80ff:fffe", // Teredo -> 127.0.0.1 (inverted)
	}
	for _, s := range blocked {
		if !isBlockedAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "64:ff9b::808:808", "2002:808:808::1"} {
		if isBlockedAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

func TestGuardAllowlist(t *testing.T) {
	allowAll := false
	g := &Guard{Policy: func() (bool, []netip.Prefix) {
		return allowAll, []netip.Prefix{netip.MustParsePrefix("10.0.5.0/24")}
	}, Metrics: metrics.NewMetrics()}
	if err := g.control("tcp4", "10.0.5.7:443", nil); err != nil {
		t.Fatalf("allowlisted private address refused: %v", err)
	}
	if err := g.control("tcp4", "10.0.6.7:443", nil); err == nil {
		t.Fatal("other private address allowed")
	}
	if g.Metrics.Get("fluid_blocked_destinations_total") != 1 {
		t.Fatal("blocked destination not counted")
	}
	allowAll = true
	if err := g.control("tcp4", "127.0.0.1:443", nil); err != nil {
		t.Fatal("allow_private should allow loopback")
	}
}
