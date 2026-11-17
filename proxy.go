package main

import (
	"crypto/subtle"
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func stripHop(h http.Header) {
	for _, f := range h.Values("Connection") {
		for _, k := range strings.Split(f, ",") {
			h.Del(strings.TrimSpace(k))
		}
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

type ForwardProxy struct {
	store *Store
	sem   chan struct{}
	tr    *http.Transport
}

func NewForwardProxy(s *Store, maxConns int) *ForwardProxy {
	return &ForwardProxy{
		store: s,
		sem:   make(chan struct{}, maxConns),
		tr: &http.Transport{
			Proxy:                 nil, // never chain to another proxy
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:          200,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		},
	}
}

// tokenOK accepts "Proxy-Authorization: Basic base64(anyuser:TOKEN)" or "Bearer TOKEN".
func tokenOK(hdr string, tokens []string) bool {
	var got string
	switch {
	case strings.HasPrefix(hdr, "Basic "):
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(hdr, "Basic "))
		if err != nil {
			return false
		}
		_, pass, ok := strings.Cut(string(raw), ":")
		if !ok {
			return false
		}
		got = pass
	case strings.HasPrefix(hdr, "Bearer "):
		got = strings.TrimPrefix(hdr, "Bearer ")
	default:
		return false
	}
	ok := false
	for _, t := range tokens {
		if t != "" && subtle.ConstantTimeCompare([]byte(got), []byte(t)) == 1 {
			ok = true
		}
	}
	return ok
}

func hostAllowed(r ProxyRules, host string, port int) bool {
	portOK := false
	for _, p := range r.AllowedPorts {
		if p == port {
			portOK = true
		}
	}
	if !portOK {
		return false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, a := range r.AllowedHosts {
		a = strings.ToLower(a)
		if a == host {
			return true
		}
		if strings.HasPrefix(a, "*.") && strings.HasSuffix(host, a[1:]) {
			return true
		}
	}
	return false
}

func splitHostPort(hp string, defPort int) (string, int, bool) {
	h, p, err := net.SplitHostPort(hp)
	if err != nil {
		return hp, defPort, hp != ""
	}
	n, err := strconv.Atoi(p)
	return h, n, err == nil
}

func (fp *ForwardProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case fp.sem <- struct{}{}:
		defer func() { <-fp.sem }()
	default:
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}
	start := time.Now()
	rules := fp.store.Rules()

	if !tokenOK(r.Header.Get("Proxy-Authorization"), rules.Tokens) {
		w.Header().Set("Proxy-Authenticate", `Basic realm="fluid-agent"`)
		http.Error(w, "proxy auth required", http.StatusProxyAuthRequired)
		slog.Warn("proxy auth failed", "client", r.RemoteAddr, "host", r.Host)
		return
	}

	var target string
	defPort := 443
	if r.Method == http.MethodConnect {
		target = r.Host
	} else {
		if !r.URL.IsAbs() {
			http.Error(w, "this is a forward proxy; send absolute URLs", http.StatusBadRequest)
			return
		}
		target = r.URL.Host
		if r.URL.Scheme == "http" {
			defPort = 80
		}
	}
	host, port, ok := splitHostPort(target, defPort)
	if !ok || !hostAllowed(rules, host, port) {
		http.Error(w, "destination not allowed", http.StatusForbidden)
		slog.Warn("proxy denied", "client", r.RemoteAddr, "dest", target)
		return
	}

	if r.Method == http.MethodConnect {
		fp.tunnel(w, r, net.JoinHostPort(host, strconv.Itoa(port)), start)
		return
	}

	out := r.Clone(r.Context())
	out.RequestURI = ""
	stripHop(out.Header)
	resp, err := fp.tr.RoundTrip(out)
	if err != nil {
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		slog.Error("proxy upstream", "dest", target, "err", err)
		return
	}
	defer resp.Body.Close()
	stripHop(resp.Header)
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(w, resp.Body)
	slog.Info("proxy http", "client", r.RemoteAddr, "method", r.Method, "url", r.URL.String(),
		"status", resp.StatusCode, "bytes", n, "ms", time.Since(start).Milliseconds())
}

func (fp *ForwardProxy) tunnel(w http.ResponseWriter, r *http.Request, dest string, start time.Time) {
	up, err := net.DialTimeout("tcp", dest, 10*time.Second)
	if err != nil {
		http.Error(w, "dial failed", http.StatusBadGateway)
		slog.Error("tunnel dial", "dest", dest, "err", err)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		client.Close()
		up.Close()
		return
	}
	// Flush anything the client already pipelined after the CONNECT headers.
	if n := buf.Reader.Buffered(); n > 0 {
		b, _ := buf.Reader.Peek(n)
		up.Write(b)
	}

	var wg sync.WaitGroup
	var sent, recv int64
	pipe := func(dst, src net.Conn, count *int64) {
		defer wg.Done()
		*count, _ = io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			tc.CloseWrite()
		} else {
			dst.Close()
		}
	}
	wg.Add(2)
	go pipe(up, client, &sent)
	go pipe(client, up, &recv)
	wg.Wait()
	client.Close()
	up.Close()
	slog.Info("proxy tunnel", "client", r.RemoteAddr, "dest", dest,
		"sent", sent, "recv", recv, "ms", time.Since(start).Milliseconds())
}
