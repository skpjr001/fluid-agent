package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"hash"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// ---------- client IP ----------

// ClientIP returns the caller's address. X-Forwarded-For is only believed
// when the direct peer is a trusted proxy, and it is walked from the right:
// the first address that is not itself a trusted proxy is the client. The
// left-most entries are attacker-controlled and are never used.
func ClientIP(r *http.Request, trusted []netip.Prefix) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	peer = peer.Unmap()
	if len(trusted) == 0 || !prefixesContain(trusted, peer) {
		return peer
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return peer // malformed chain: fall back to the trusted peer
		}
		a = a.Unmap()
		if !prefixesContain(trusted, a) {
			return a
		}
	}
	return peer
}

// ---------- rate limiting ----------

// RateLimiter is a token bucket per key (route + source IP).
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func NewRateLimiter() *RateLimiter {
	rl := &RateLimiter{buckets: map[string]*bucket{}, now: time.Now}
	return rl
}

// Allow takes one token; when refused it returns how long until one is free.
func (rl *RateLimiter) Allow(key string, lim RateLimit) (bool, time.Duration) {
	if lim.RequestsPerMinute <= 0 {
		return true, 0
	}
	rate := float64(lim.RequestsPerMinute) / 60
	now := rl.now()
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b := rl.buckets[key]
	if b == nil {
		b = &bucket{tokens: float64(lim.Burst), last: now}
		rl.buckets[key] = b
	}
	b.tokens = math.Min(float64(lim.Burst), b.tokens+now.Sub(b.last).Seconds()*rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / rate * float64(time.Second))
}

// Sweep drops buckets idle for longer than idle (they would be full anyway).
func (rl *RateLimiter) Sweep(idle time.Duration) {
	cutoff := rl.now().Add(-idle)
	rl.mu.Lock()
	defer rl.mu.Unlock()
	for k, b := range rl.buckets {
		if b.last.Before(cutoff) {
			delete(rl.buckets, k)
		}
	}
}

// ---------- inbound authentication ----------

func ctEqual(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// verifyInbound checks every configured auth method; reason explains a failure.
func verifyInbound(a *InboundAuth, r *http.Request, body []byte) (ok bool, reason string) {
	if a == nil {
		return true, ""
	}
	if t := a.Token; t != nil {
		if !ctEqual(r.Header.Get(t.Header), t.Value) {
			return false, "bad or missing token"
		}
	}
	if b := a.Basic; b != nil {
		u, p, has := r.BasicAuth()
		// evaluate both comparisons to keep timing independent of which failed
		uOK, pOK := ctEqual(u, b.Username), ctEqual(p, b.Password)
		if !has || !uOK || !pOK {
			return false, "bad or missing basic credentials"
		}
	}
	if h := a.HMAC; h != nil {
		if !verifyHMAC(h, r.Header.Get(h.Header), body) {
			return false, "bad or missing signature"
		}
	}
	return true, ""
}

func verifyHMAC(h *HMACAuth, got string, body []byte) bool {
	got = strings.TrimSpace(got)
	if h.Prefix != "" {
		if !strings.HasPrefix(got, h.Prefix) {
			return false
		}
		got = strings.TrimPrefix(got, h.Prefix)
	}
	var sig []byte
	var err error
	if h.Encoding == "base64" {
		sig, err = base64.StdEncoding.DecodeString(got)
	} else {
		sig, err = hex.DecodeString(strings.ToLower(got))
	}
	if err != nil || len(sig) == 0 {
		return false
	}
	return hmac.Equal(sig, computeHMAC(h, body))
}

func computeHMAC(h *HMACAuth, body []byte) []byte {
	var fn func() hash.Hash
	switch h.Algorithm {
	case "sha1":
		fn = sha1.New
	case "sha512":
		fn = sha512.New
	default:
		fn = sha256.New
	}
	m := hmac.New(fn, []byte(h.Secret))
	m.Write(body)
	return m.Sum(nil)
}

// credentialHeaders lists inbound headers that carry credentials for the
// agent itself; they are not stored or relayed.
func credentialHeaders(a *InboundAuth) []string {
	out := []string{"Authorization", "Proxy-Authorization", "Cookie"}
	if a != nil && a.Token != nil {
		out = append(out, a.Token.Header)
	}
	return out
}
