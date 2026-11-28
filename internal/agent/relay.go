package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/skpjr001/fluid-agent/internal/netguard"
	"github.com/skpjr001/fluid-agent/internal/telemetry"
)

// The relay sends the caller's own request (method, headers, body) to a
// target URL and returns the target's answer unchanged. The caller only puts
// the agent in front of the URL and adds X-Agent-Key:
//
//	POST /relay?target_url=<url-encoded target>
//	POST /relay/https://api.example.com/v1/x?a=1   (everything after /relay/ is the target)
//
// It applies the same checks as /forward: client token, allowed_hosts and
// allowed_ports, private-address blocking, re-checked redirects, timeouts.

// Headers the relay never sends to the target: the agent's own controls, the
// caller's client key, and what a reverse proxy in front of the agent adds
// (which would reveal internal addresses to the target).
var relayDropHeaders = []string{
	"X-Agent-Key", "X-Fluid-Timeout-Ms", "Host", "Expect",
	"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Port",
	"X-Forwarded-Prefix", "X-Forwarded-Server", "X-Real-Ip", "Forwarded", "Via",
}

const (
	relayTargetParam  = "target_url"
	relayErrorHeader  = "X-Fluid-Error"
	relayTimeoutHdr   = "X-Fluid-Timeout-Ms"
	relayPathPrefix   = "/relay/"
	relayAPIPrefix    = "/api/v1/relay/"
	relayQueryPath    = "/relay"
	relayAPIQueryPath = "/api/v1/relay"
)

// isRelayPath reports whether path (base path already removed) is the relay.
func isRelayPath(path string) bool {
	return path == relayQueryPath || path == relayAPIQueryPath ||
		strings.HasPrefix(path, relayPathPrefix) || strings.HasPrefix(path, relayAPIPrefix)
}

// relayTarget finds the target URL in the query form or the path form.
// raw is the request URI as received, with the base path removed.
func relayTarget(raw string) (string, *fwdError) {
	for _, p := range []string{relayPathPrefix, relayAPIPrefix} {
		if rest, ok := strings.CutPrefix(raw, p); ok && rest != "" {
			return pathFormTarget(rest)
		}
	}
	_, query, _ := strings.Cut(raw, "?")
	q, err := url.ParseQuery(query)
	if err != nil {
		return "", ferr(400, "VALIDATION", "bad query string: %v", err)
	}
	t := q.Get(relayTargetParam)
	if t == "" {
		return "", ferr(400, "VALIDATION", "target missing: use /relay?%s=<url-encoded URL> or /relay/<URL>", relayTargetParam)
	}
	if len(q) > 1 || len(q[relayTargetParam]) > 1 {
		// "?target_url=https://x/api?a=1&b=2" splits into target_url and b:
		// guessing which parameters belonged to the target is unsafe.
		return "", ferr(400, "VALIDATION", "the query has parameters besides %s: URL-encode the target URL, or use /relay/<URL>", relayTargetParam)
	}
	return t, nil
}

// pathFormTarget turns what follows /relay/ into the target URL. It accepts
// the URL as typed, URL-encoded, or with "//" merged into "/" by a reverse
// proxy (nginx merge_slashes).
func pathFormTarget(rest string) (string, *fwdError) {
	lower := strings.ToLower(rest)
	if strings.HasPrefix(lower, "http%3a") || strings.HasPrefix(lower, "https%3a") ||
		strings.HasPrefix(lower, "http:%2f") || strings.HasPrefix(lower, "https:%2f") {
		u, err := url.PathUnescape(rest)
		if err != nil {
			return "", ferr(400, "VALIDATION", "bad URL encoding in the target")
		}
		rest = u
	}
	for _, s := range []string{"https:/", "http:/"} {
		if len(rest) > len(s) && strings.EqualFold(rest[:len(s)], s) && rest[len(s)] != '/' {
			rest = s + "/" + rest[len(s):]
			break
		}
	}
	return rest, nil
}

func writeRelayError(w http.ResponseWriter, e *fwdError) {
	w.Header().Set(relayErrorHeader, e.code)
	writeFwdError(w, e)
}

// ServeRelay handles the relay. raw is the request URI without the base path.
func (f *Forwarder) ServeRelay(w http.ResponseWriter, r *http.Request, raw string) {
	start := time.Now()
	src := ClientIP(r, f.app.cfg.Trusted).String()
	own := f.app.newCapture() // the caller's request, when the target is never reached
	defer own.Finish()
	if own != nil {
		own.SetRequest(requestHeaders(r.Method, r.URL.RequestURI(), r.Host, r.Header), nil)
		w = &captureWriter{ResponseWriter: w, c: own}
	}
	up := f.app.newCapture() // the call as sent to and answered by the target
	defer up.Finish()
	logCap := func() *Capture {
		if up.hasRequest() {
			return up
		}
		return own
	}
	fail := func(client, target string, fe *fwdError) {
		f.app.metrics.Inc("fluid_relay_requests_total", "result", strings.ToLower(fe.code))
		writeRelayError(w, fe)
		slog.Warn("relay failed", "kind", "forward", "mode", "relay", "direction", "outbound", "client", client,
			"source_ip", src, "method", r.Method, "host", hostOf(target), "url", target, "status", fe.status,
			"ms", time.Since(start).Milliseconds(), "code", fe.code, "err", fe.msg, "capture", logCap())
	}

	// The key comes from X-Agent-Key only (Proxy-Authorization also works):
	// Authorization belongs to the caller's request and goes to the target.
	token := r.Header.Get("X-Agent-Key")
	if token == "" {
		token = tokenFrom(r.Header.Get("Proxy-Authorization"))
	}
	client, ok := authenticateClient(f.app.store.Proxy(), token)
	if !ok {
		f.app.metrics.Inc("fluid_relay_requests_total", "result", "unauthorized")
		writeRelayError(w, ferr(http.StatusUnauthorized, "UNAUTHORIZED", "invalid client token: send it in X-Agent-Key"))
		slog.Warn("relay auth failed", "kind", "auth", "direction", "outbound", "source_ip", src, "status", 401, "capture", own)
		return
	}
	target, fe := relayTarget(raw)
	if fe == nil {
		if err := validTargetURL(target); err != nil {
			fe = ferr(400, "VALIDATION", "target: %v", err)
		}
	}
	if fe != nil {
		fail(client, target, fe)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, f.app.cfg.MaxBodyBytes))
	if own != nil {
		own.SetRequest(requestHeaders(r.Method, r.URL.RequestURI(), r.Host, r.Header), body)
	}
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			fail(client, target, ferr(http.StatusRequestEntityTooLarge, "TOO_LARGE", "body exceeds %d bytes", f.app.cfg.MaxBodyBytes))
		} else {
			fail(client, target, ferr(400, "VALIDATION", "reading body: %v", err))
		}
		return
	}

	timeout := f.app.cfg.Forward.DefaultTimeout.D()
	if v := r.Header.Get(relayTimeoutHdr); v != "" {
		ms, err := strconv.Atoi(v)
		if err != nil || ms <= 0 {
			fail(client, target, ferr(400, "VALIDATION", "%s must be a positive number of milliseconds", relayTimeoutHdr))
			return
		}
		timeout = time.Duration(ms) * time.Millisecond
	}
	timeout = min(max(timeout, 100*time.Millisecond), f.app.cfg.Forward.MaxTimeout.D())

	headers := r.Header.Clone()
	stripHop(headers)
	for _, h := range relayDropHeaders {
		headers.Del(h)
	}

	ctx, span := telemetry.Tracer().Start(telemetry.Extract(r.Context(), r.Header), "relay", trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("fluid.client", client)))
	ctx, cancel := context.WithTimeout(withCapture(ctx, up), timeout)
	defer cancel()
	resp, final, redirects, fe := f.relay(ctx, r.Method, target, headers, body, timeout)
	if fe != nil {
		telemetry.EndSpan(span, fe.status, fe)
		fail(client, final, fe)
		return
	}
	defer resp.Body.Close()
	stripHop(resp.Header)
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	if redirects > 0 {
		w.Header().Set("X-Fluid-Final-Url", final)
	}
	w.WriteHeader(resp.StatusCode)
	up.SetResponse(responseHeaders(resp.StatusCode, resp.Header), nil)
	n, err := io.Copy(w, up.TeeResponseBody(resp.Body))
	up.Finish()
	ms := time.Since(start).Milliseconds()
	if err != nil {
		f.app.metrics.Inc("fluid_relay_requests_total", "result", "upstream_failure")
		slog.Warn("relay response cut short", "kind", "forward", "mode", "relay", "direction", "outbound", "client", client,
			"source_ip", src, "method", r.Method, "host", hostOf(final), "url", final, "status", resp.StatusCode,
			"bytes_out", n, "ms", ms, "err", err, "capture", up)
		telemetry.EndSpan(span, http.StatusBadGateway, err)
		return
	}
	f.app.metrics.Inc("fluid_relay_requests_total", "result", "ok")
	slog.Info("relay", "kind", "forward", "mode", "relay", "direction", "outbound", "client", client, "source_ip", src,
		"method", r.Method, "host", hostOf(final), "url", final, "status", resp.StatusCode,
		"bytes_in", len(body), "bytes_out", n, "ms", ms, "redirects", redirects, "capture", up)
	telemetry.EndSpan(span, resp.StatusCode, nil)
}

// relay sends the request, following redirects by hand so each hop is
// checked against allowed_hosts and credentials never cross origins. The
// returned response body is still open (streamed to the caller).
func (f *Forwarder) relay(ctx context.Context, method, target string, headers http.Header, body []byte, timeout time.Duration) (*http.Response, string, int, *fwdError) {
	pc := f.app.store.Proxy()
	cur, _ := url.Parse(target)
	cur.Fragment = ""
	for redirects := 0; ; redirects++ {
		if !pc.hostAllowed(cur.Hostname(), defaultPort(cur)) {
			return nil, cur.String(), redirects, ferr(403, "HOST_NOT_ALLOWED", "%q is not in allowed_hosts/allowed_ports", cur.Host)
		}
		up := captureFrom(ctx)
		up.SetRequest(requestHeaders(method, cur.String(), cur.Host, headers), body)
		up.SetResponse(nil, nil)
		resp, err := f.sendWith(ctx, f.raw, method, cur.String(), headers, body)
		if err != nil {
			switch {
			case errors.Is(err, netguard.ErrBlockedAddress):
				return nil, cur.String(), redirects, ferr(403, "BLOCKED_ADDRESS", "%v", err)
			case ctx.Err() != nil:
				return nil, cur.String(), redirects, ferr(504, "TIMEOUT", "upstream timed out after %s", timeout)
			}
			return nil, cur.String(), redirects, ferr(502, "UPSTREAM_FAILURE", "%v", err)
		}
		loc := resp.Header.Get("Location")
		if resp.StatusCode < 300 || resp.StatusCode >= 400 || resp.StatusCode == 304 || loc == "" {
			return resp, cur.String(), redirects, nil
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if redirects >= f.app.cfg.Forward.MaxRedirects {
			return nil, cur.String(), redirects, ferr(502, "UPSTREAM_FAILURE", "too many redirects")
		}
		next, err := cur.Parse(loc)
		if err != nil || (next.Scheme != "http" && next.Scheme != "https") {
			return nil, cur.String(), redirects, ferr(502, "UPSTREAM_FAILURE", "bad redirect location %q", loc)
		}
		if cur.Scheme == "https" && next.Scheme == "http" {
			return nil, cur.String(), redirects, ferr(502, "UPSTREAM_FAILURE", "refusing https to http redirect")
		}
		if !sameOrigin(cur, next) {
			headers = headers.Clone()
			for _, h := range []string{"Authorization", "Cookie", "Proxy-Authorization"} {
				headers.Del(h)
			}
		}
		if resp.StatusCode == 303 || ((resp.StatusCode == 301 || resp.StatusCode == 302) && method == http.MethodPost) {
			method, body = http.MethodGet, nil
			headers = headers.Clone()
			headers.Del("Content-Type")
		}
		next.Fragment = ""
		cur = next
	}
}

// rawClient is used by the relay: no transparent decompression, so the
// caller gets the target's bytes and Content-Encoding exactly.
func rawClient(app *App) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         app.guard.DialContext,
			MaxIdleConns:        100,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			DisableCompression:  true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
