package main

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
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

// tokenFrom extracts a client token from "Basic base64(anyuser:TOKEN)" or "Bearer TOKEN".
func tokenFrom(hdr string) string {
	switch {
	case strings.HasPrefix(hdr, "Basic "):
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(hdr, "Basic "))
		if err != nil {
			return ""
		}
		_, pass, _ := strings.Cut(string(raw), ":")
		return pass
	case strings.HasPrefix(hdr, "Bearer "):
		return strings.TrimPrefix(hdr, "Bearer ")
	}
	return ""
}

// authenticateClient returns the client name for token. Every client is
// compared (constant time each) so timing does not reveal which one matched.
func authenticateClient(pc *ProxyConfig, token string) (string, bool) {
	if token == "" {
		return "", false
	}
	name, ok := "", false
	for _, c := range pc.Clients {
		if ctEqual(token, c.Token) && !c.Disabled {
			name, ok = c.Name, true
		}
	}
	return name, ok
}

type ForwardProxy struct {
	app     *App
	sem     chan struct{}
	tr      *http.Transport
	tunnels atomic.Int64
}

func NewForwardProxy(app *App) *ForwardProxy {
	return &ForwardProxy{
		app: app,
		sem: make(chan struct{}, app.cfg.MaxConns),
		tr: &http.Transport{
			Proxy:                 nil, // never chain to another proxy
			DialContext:           app.guard.DialContext,
			MaxIdleConns:          200,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		},
	}
}

func (fp *ForwardProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect && !r.URL.IsAbs() {
		path := r.URL.Path
		if b := fp.app.cfg.ProxyBasePath; b != "" && (path == b || strings.HasPrefix(path, b+"/")) {
			path = strings.TrimPrefix(path, b)
		}
		switch path {
		case "/forward", "/api/v1/forward":
			fp.app.forward.ServeHTTP(w, r)
		case "/healthz":
			w.Write([]byte("ok"))
		default:
			http.Error(w, "this is a forward proxy; send absolute URLs or POST /forward", http.StatusBadRequest)
		}
		return
	}
	select {
	case fp.sem <- struct{}{}:
		defer func() { <-fp.sem }()
	default:
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		fp.app.metrics.Inc("fluid_proxy_requests_total", "kind", kindOf(r), "result", "overloaded")
		return
	}
	start := time.Now()
	pc := fp.app.store.Proxy()
	src := ClientIP(r, fp.app.cfg.trusted).String()

	client, ok := authenticateClient(pc, tokenFrom(r.Header.Get("Proxy-Authorization")))
	if !ok {
		w.Header().Set("Proxy-Authenticate", `Basic realm="fluid-agent"`)
		http.Error(w, "proxy auth required", http.StatusProxyAuthRequired)
		fp.app.metrics.Inc("fluid_proxy_requests_total", "kind", kindOf(r), "result", "unauthorized")
		slog.Warn("proxy auth failed", "kind", "auth", "direction", "outbound", "source_ip", src, "url", r.Host, "status", 407)
		return
	}

	target, defPort := r.Host, 443
	if r.Method != http.MethodConnect {
		target = r.URL.Host
		if r.URL.Scheme == "http" {
			defPort = 80
		}
	}
	host, port, ok := splitHostPort(target, defPort)
	if !ok || !pc.hostAllowed(host, port) {
		http.Error(w, "destination not allowed", http.StatusForbidden)
		fp.app.metrics.Inc("fluid_proxy_requests_total", "kind", kindOf(r), "result", "denied")
		slog.Warn("proxy denied", "kind", "proxy_denied", "direction", "outbound", "client", client,
			"source_ip", src, "host", host, "url", target, "status", 403)
		return
	}

	ctx, span := tracer.Start(r.Context(), "proxy "+kindOf(r), trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("server.address", host), attribute.Int("server.port", port),
			attribute.String("fluid.client", client)))
	r = r.WithContext(ctx)

	if r.Method == http.MethodConnect {
		fp.tunnel(w, r, net.JoinHostPort(host, strconv.Itoa(port)), client, src, host, start, span)
		return
	}

	out := r.Clone(ctx)
	out.RequestURI = ""
	stripHop(out.Header)
	resp, err := fp.tr.RoundTrip(out)
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, ErrBlockedAddress) {
			code = http.StatusForbidden
		}
		http.Error(w, "upstream error: "+err.Error(), code)
		fp.app.metrics.Inc("fluid_proxy_requests_total", "kind", "http", "result", "error")
		slog.Error("proxy upstream failed", "kind", "proxy_http", "direction", "outbound", "client", client,
			"source_ip", src, "method", r.Method, "host", host, "url", r.URL.String(), "status", code,
			"ms", time.Since(start).Milliseconds(), "err", err)
		endSpan(span, code, err)
		return
	}
	defer resp.Body.Close()
	stripHop(resp.Header)
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(w, resp.Body)
	fp.app.metrics.Inc("fluid_proxy_requests_total", "kind", "http", "result", "ok")
	slog.Info("proxy http", "kind", "proxy_http", "direction", "outbound", "client", client, "source_ip", src,
		"method", r.Method, "host", host, "url", r.URL.String(), "status", resp.StatusCode,
		"bytes_in", r.ContentLength, "bytes_out", n, "ms", time.Since(start).Milliseconds())
	endSpan(span, resp.StatusCode, nil)
}

func kindOf(r *http.Request) string {
	if r.Method == http.MethodConnect {
		return "connect"
	}
	return "http"
}

func splitHostPort(hp string, defPort int) (string, int, bool) {
	h, p, err := net.SplitHostPort(hp)
	if err != nil {
		return hp, defPort, hp != ""
	}
	n, err := strconv.Atoi(p)
	return h, n, err == nil && h != ""
}

// idleConn closes the tunnel when no bytes move in either direction for timeout.
type idleConn struct {
	net.Conn
	timeout time.Duration
}

func (c *idleConn) Read(b []byte) (int, error) {
	c.Conn.SetDeadline(time.Now().Add(c.timeout))
	return c.Conn.Read(b)
}

func (c *idleConn) Write(b []byte) (int, error) {
	c.Conn.SetDeadline(time.Now().Add(c.timeout))
	return c.Conn.Write(b)
}

func (fp *ForwardProxy) tunnel(w http.ResponseWriter, r *http.Request, dest, client, src, host string, start time.Time, span trace.Span) {
	dctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	up, err := fp.app.guard.DialContext(dctx, "tcp", dest)
	cancel()
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, ErrBlockedAddress) {
			code = http.StatusForbidden
		}
		http.Error(w, "dial failed", code)
		fp.app.metrics.Inc("fluid_proxy_requests_total", "kind", "connect", "result", "error")
		slog.Error("tunnel dial failed", "kind", "proxy_connect", "direction", "outbound", "client", client,
			"source_ip", src, "host", host, "url", dest, "status", code, "err", err)
		endSpan(span, code, err)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		endSpan(span, 500, errors.New("hijack unsupported"))
		return
	}
	cl, buf, err := hj.Hijack()
	if err != nil {
		up.Close()
		endSpan(span, 500, err)
		return
	}
	if _, err := cl.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		cl.Close()
		up.Close()
		endSpan(span, 500, err)
		return
	}
	// Flush anything the client already pipelined after the CONNECT headers.
	if n := buf.Reader.Buffered(); n > 0 {
		b, _ := buf.Reader.Peek(n)
		up.Write(b)
	}
	fp.tunnels.Add(1)
	defer fp.tunnels.Add(-1)

	idle := fp.app.cfg.TunnelIdleTimeout.D()
	upC, clC := &idleConn{Conn: up, timeout: idle}, &idleConn{Conn: cl, timeout: idle}
	var wg sync.WaitGroup
	var sent, recv int64
	pipe := func(dst, src *idleConn, count *int64) {
		defer wg.Done()
		*count, _ = io.Copy(dst, src)
		if tc, ok := dst.Conn.(*net.TCPConn); ok {
			tc.CloseWrite()
		} else {
			dst.Close()
		}
	}
	wg.Add(2)
	go pipe(upC, clC, &sent)
	go pipe(clC, upC, &recv)
	wg.Wait()
	cl.Close()
	up.Close()
	fp.app.metrics.Inc("fluid_proxy_requests_total", "kind", "connect", "result", "ok")
	slog.Info("proxy tunnel", "kind", "proxy_connect", "direction", "outbound", "client", client, "source_ip", src,
		"method", "CONNECT", "host", host, "url", dest, "status", 200, "bytes_in", sent, "bytes_out", recv,
		"ms", time.Since(start).Milliseconds())
	endSpan(span, 200, nil)
}
