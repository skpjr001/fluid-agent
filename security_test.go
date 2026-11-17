package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
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
	pc := &ProxyConfig{PrivateAllowlist: []string{"10.0.5.0/24"}}
	pc.normalize()
	g := &Guard{cfg: func() *ProxyConfig { return pc }, metrics: NewMetrics()}
	if err := g.control("tcp4", "10.0.5.7:443", nil); err != nil {
		t.Fatalf("allowlisted private address refused: %v", err)
	}
	if err := g.control("tcp4", "10.0.6.7:443", nil); err == nil {
		t.Fatal("other private address allowed")
	}
	if g.metrics.Get("fluid_blocked_destinations_total") != 1 {
		t.Fatal("blocked destination not counted")
	}
	pc.AllowPrivate = true
	if err := g.control("tcp4", "127.0.0.1:443", nil); err != nil {
		t.Fatal("allow_private should allow loopback")
	}
}

func TestClientIP(t *testing.T) {
	trusted, _ := parsePrefixes([]string{"10.0.0.0/8"})
	cases := []struct {
		remote, xff string
		trusted     bool
		want        string
	}{
		{"203.0.113.9:1", "", true, "203.0.113.9"},               // direct
		{"203.0.113.9:1", "1.2.3.4", true, "203.0.113.9"},        // untrusted peer: header ignored
		{"10.0.0.2:1", "52.66.1.1", true, "52.66.1.1"},           // one trusted hop
		{"10.0.0.2:1", "52.66.1.1, 10.0.0.9", true, "52.66.1.1"}, // two trusted hops
		{"10.0.0.2:1", "52.66.1.1, 6.6.6.6", true, "6.6.6.6"},    // spoofed left entry ignored
		{"10.0.0.2:1", "garbage, 6.6.6.6", true, "6.6.6.6"},      // right-most real client
		{"10.0.0.2:1", "6.6.6.6, garbage", true, "10.0.0.2"},     // malformed: fall back to peer
		{"10.0.0.2:1", "52.66.1.1", false, "10.0.0.2"},           // no trusted proxies configured
		{"[::ffff:10.0.0.2]:1", "52.66.1.1", true, "52.66.1.1"},  // mapped peer
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		tr := trusted
		if !c.trusted {
			tr = nil
		}
		if got := ClientIP(r, tr).String(); got != c.want {
			t.Errorf("remote=%s xff=%q: got %s want %s", c.remote, c.xff, got, c.want)
		}
	}
}

func TestSpoofedForwardedForCannotBypassSourceCIDRs(t *testing.T) {
	// Regression for the original bug: the left-most X-Forwarded-For entry was trusted.
	trusted, _ := parsePrefixes([]string{"10.0.0.0/8"})
	opts := &RouteOptions{SourceCIDRs: []string{"52.66.0.0/16"}}
	opts.normalize(false)
	r := httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "10.0.0.2:5555"                        // Traefik
	r.Header.Set("X-Forwarded-For", "52.66.1.1, 6.6.6.6") // attacker prepended an allowed IP
	if opts.allowsIP(ClientIP(r, trusted)) {
		t.Fatal("spoofed X-Forwarded-For bypassed source_cidrs")
	}
}

func TestRateLimiter(t *testing.T) {
	rl := NewRateLimiter()
	now := time.Unix(1000, 0)
	rl.now = func() time.Time { return now }
	lim := RateLimit{RequestsPerMinute: 60, Burst: 3}
	for i := 0; i < 3; i++ {
		if ok, _ := rl.Allow("k", lim); !ok {
			t.Fatalf("request %d within burst refused", i)
		}
	}
	ok, wait := rl.Allow("k", lim)
	if ok || wait <= 0 || wait > time.Second {
		t.Fatalf("4th request: ok=%v wait=%v", ok, wait)
	}
	if ok, _ := rl.Allow("other", lim); !ok {
		t.Fatal("keys must be independent")
	}
	now = now.Add(time.Second)
	if ok, _ := rl.Allow("k", lim); !ok {
		t.Fatal("token should refill after 1s at 60/min")
	}
	if ok, _ := rl.Allow("k", RateLimit{}); !ok {
		t.Fatal("zero limit disables limiting")
	}
	now = now.Add(time.Hour)
	rl.Sweep(time.Minute)
	if len(rl.buckets) != 0 {
		t.Fatal("idle buckets should be swept")
	}
}

func sign(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

func TestVerifyInbound(t *testing.T) {
	body := `{"event":"payment.captured"}`
	req := func(h map[string]string) *http.Request {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		for k, v := range h {
			r.Header.Set(k, v)
		}
		return r
	}
	tok := &InboundAuth{Token: &TokenAuth{Header: "X-Callback-Token", Value: "s3cret-value"}}
	if ok, _ := verifyInbound(tok, req(map[string]string{"X-Callback-Token": "s3cret-value"}), []byte(body)); !ok {
		t.Error("good token refused")
	}
	if ok, _ := verifyInbound(tok, req(map[string]string{"X-Callback-Token": "wrong"}), []byte(body)); ok {
		t.Error("bad token accepted")
	}

	basic := &InboundAuth{Basic: &BasicAuth{Username: "nse", Password: "pa55word!"}}
	r := req(nil)
	r.SetBasicAuth("nse", "pa55word!")
	if ok, _ := verifyInbound(basic, r, []byte(body)); !ok {
		t.Error("good basic refused")
	}
	r = req(nil)
	r.SetBasicAuth("nse", "nope")
	if ok, _ := verifyInbound(basic, r, []byte(body)); ok {
		t.Error("bad basic accepted")
	}

	hm := &InboundAuth{HMAC: &HMACAuth{Header: "X-Razorpay-Signature", Secret: "whsec_12345678", Algorithm: "sha256", Encoding: "hex"}}
	if ok, _ := verifyInbound(hm, req(map[string]string{"X-Razorpay-Signature": sign("whsec_12345678", body)}), []byte(body)); !ok {
		t.Error("good signature refused")
	}
	if ok, _ := verifyInbound(hm, req(map[string]string{"X-Razorpay-Signature": sign("whsec_12345678", body+" ")}), []byte(body)); ok {
		t.Error("signature of other body accepted")
	}
	gh := &InboundAuth{HMAC: &HMACAuth{Header: "X-Hub-Signature-256", Secret: "whsec_12345678", Algorithm: "sha256", Encoding: "hex", Prefix: "sha256="}}
	if ok, _ := verifyInbound(gh, req(map[string]string{"X-Hub-Signature-256": "sha256=" + sign("whsec_12345678", body)}), []byte(body)); !ok {
		t.Error("prefixed signature refused")
	}
	m := hmac.New(sha256.New, []byte("whsec_12345678"))
	m.Write([]byte(body))
	b64 := &InboundAuth{HMAC: &HMACAuth{Header: "X-Sig", Secret: "whsec_12345678", Algorithm: "sha256", Encoding: "base64"}}
	if ok, _ := verifyInbound(b64, req(map[string]string{"X-Sig": base64.StdEncoding.EncodeToString(m.Sum(nil))}), []byte(body)); !ok {
		t.Error("base64 signature refused")
	}

	all := &InboundAuth{Token: tok.Token, HMAC: hm.HMAC}
	if ok, _ := verifyInbound(all, req(map[string]string{"X-Callback-Token": "s3cret-value"}), []byte(body)); ok {
		t.Error("all configured checks must pass")
	}
	if ok, _ := verifyInbound(nil, req(nil), nil); !ok {
		t.Error("no auth configured should accept")
	}
}

func TestTargetAuthApply(t *testing.T) {
	h := http.Header{}
	(&TargetAuth{Type: "basic", Username: "u", Password: "p"}).apply(h)
	if h.Get("Authorization") != "Basic dTpw" {
		t.Fatal(h)
	}
	(&TargetAuth{Type: "header", Header: "X-Key", Value: "v"}).apply(h)
	(&TargetAuth{Type: "bearer", Token: "tk"}).apply(h)
	if h.Get("X-Key") != "v" || h.Get("Authorization") != "Bearer tk" {
		t.Fatal(h)
	}
	if (&TargetAuth{Type: "bearer"}).validate() == nil || (&TargetAuth{Type: "x"}).validate() == nil {
		t.Fatal("invalid target_auth accepted")
	}
}
