package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// ErrBlockedAddress is returned when a destination resolves to an address the
// guard refuses (loopback, private, link-local/metadata, ...).
var ErrBlockedAddress = errors.New("destination address is not allowed")

var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16",
		"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "100::/64", "2001:db8::/32", "fc00::/7", "fe80::/10", "ff00::/8",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

var (
	nat64Prefix  = netip.MustParsePrefix("64:ff9b::/96")
	nat64Local   = netip.MustParsePrefix("64:ff9b:1::/48")
	sixToFour    = netip.MustParsePrefix("2002::/16")
	teredoPrefix = netip.MustParsePrefix("2001::/32")
)

// embeddedIPv4 returns IPv4 addresses hidden inside IPv6 forms that routers
// translate back to IPv4 (mapped, compatible, NAT64, 6to4, Teredo).
func embeddedIPv4(a netip.Addr) []netip.Addr {
	if !a.Is6() || a.Is4In6() {
		return nil
	}
	b := a.As16()
	var out []netip.Addr
	if nat64Prefix.Contains(a) || nat64Local.Contains(a) {
		out = append(out, netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
	}
	if sixToFour.Contains(a) {
		out = append(out, netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}))
	}
	if teredoPrefix.Contains(a) { // client IPv4 is stored inverted in the last 4 bytes
		out = append(out, netip.AddrFrom4([4]byte{^b[12], ^b[13], ^b[14], ^b[15]}))
	}
	allZero := true
	for _, x := range b[:12] {
		allZero = allZero && x == 0
	}
	if allZero { // deprecated IPv4-compatible ::a.b.c.d
		out = append(out, netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
	}
	return out
}

// isBlockedAddr reports whether a is a non-public address.
func isBlockedAddr(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	for _, e := range embeddedIPv4(a) {
		if isBlockedAddr(e) {
			return true
		}
	}
	return false
}

// Guard decides per dial whether a resolved address may be used.
type Guard struct {
	cfg     func() *ProxyConfig
	metrics *Metrics
}

func (g *Guard) allowed(a netip.Addr) bool {
	if !isBlockedAddr(a) {
		return true
	}
	pc := g.cfg()
	return pc.AllowPrivate || prefixesContain(pc.privateOK, a)
}

// control runs after DNS resolution, right before connect(2), so the check
// applies to the address actually dialed (this also defeats DNS rebinding).
func (g *Guard) control(network, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrBlockedAddress, address)
	}
	if !g.allowed(ap.Addr()) {
		g.metrics.Inc("fluid_blocked_destinations_total")
		return fmt.Errorf("%w: %s", ErrBlockedAddress, ap.Addr())
	}
	return nil
}

// Dialer returns a dialer that enforces the guard.
func (g *Guard) Dialer() *net.Dialer {
	return &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: g.control}
}

func (g *Guard) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return g.Dialer().DialContext(ctx, network, addr)
}
