package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// ForwardRequest is the body of POST /forward.
type ForwardRequest struct {
	Method     string            `json:"method"`
	URL        string            `json:"url,omitempty"`     // absolute URL, or…
	Profile    string            `json:"profile,omitempty"` // …a profile name plus
	Path       string            `json:"path,omitempty"`    // a path (and query) under its base_url
	Headers    map[string]string `json:"headers,omitempty"`
	Body       string            `json:"body,omitempty"`        // text body
	BodyBase64 string            `json:"body_base64,omitempty"` // binary body
	TimeoutMs  int               `json:"timeout_ms,omitempty"`
}

// ForwardResponse is returned with HTTP 200 whenever the upstream answered.
type ForwardResponse struct {
	Status     int                 `json:"status"`
	Headers    map[string][]string `json:"headers"`
	Body       string              `json:"body,omitempty"`        // set when the body is valid UTF-8
	BodyBase64 string              `json:"body_base64,omitempty"` // set otherwise
	FinalURL   string              `json:"final_url"`
	Redirects  int                 `json:"redirects"`
	DurationMs int64               `json:"duration_ms"`
}

type fwdError struct {
	status int
	code   string
	msg    string
}

func (e *fwdError) Error() string { return e.msg }

func ferr(status int, code, format string, a ...any) *fwdError {
	return &fwdError{status, code, fmt.Sprintf(format, a...)}
}

// Forwarder implements the JSON relay (delta-agent compatible shape).
type Forwarder struct {
	app    *App
	client *http.Client
}

func NewForwarder(app *App) *Forwarder {
	return &Forwarder{app: app, client: &http.Client{
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         app.guard.DialContext,
			MaxIdleConns:        100,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		// redirects are followed by hand so every hop is re-checked
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func writeFwdError(w http.ResponseWriter, e *fwdError) {
	writeJSON(w, e.status, map[string]string{"error": e.msg, "code": e.code})
}

func (f *Forwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	src := ClientIP(r, f.app.cfg.trusted).String()
	if r.Method != http.MethodPost {
		writeFwdError(w, ferr(http.StatusMethodNotAllowed, "VALIDATION", "use POST"))
		return
	}
	token := r.Header.Get("X-Agent-Key")
	if token == "" {
		token = tokenFrom(r.Header.Get("Authorization"))
	}
	if token == "" {
		token = tokenFrom(r.Header.Get("Proxy-Authorization"))
	}
	client, ok := authenticateClient(f.app.store.Proxy(), token)
	if !ok {
		f.app.metrics.Inc("fluid_forward_requests_total", "result", "unauthorized")
		slog.Warn("forward auth failed", "kind", "auth", "direction", "outbound", "source_ip", src, "status", 401)
		writeFwdError(w, ferr(http.StatusUnauthorized, "UNAUTHORIZED", "invalid client token"))
		return
	}
	var req ForwardRequest
	if err := decodeStrict(http.MaxBytesReader(w, r.Body, f.app.cfg.MaxBodyBytes+(1<<20)), &req); err != nil {
		writeFwdError(w, ferr(http.StatusBadRequest, "VALIDATION", "bad request body: %v", err))
		return
	}
	ctx, span := tracer.Start(extractTrace(r.Context(), r.Header), "forward", trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("fluid.client", client), attribute.String("fluid.profile", req.Profile)))
	resp, target, err := f.do(ctx, client, &req)
	ms := time.Since(start).Milliseconds()
	if err != nil {
		var fe *fwdError
		if !errors.As(err, &fe) {
			fe = ferr(http.StatusBadGateway, "UPSTREAM_FAILURE", "%v", err)
		}
		f.app.metrics.Inc("fluid_forward_requests_total", "result", strings.ToLower(fe.code))
		slog.Warn("forward failed", "kind", "forward", "direction", "outbound", "client", client, "source_ip", src,
			"profile", req.Profile, "method", req.Method, "host", hostOf(target), "url", target, "status", fe.status,
			"ms", ms, "code", fe.code, "err", fe.msg)
		endSpan(span, fe.status, fe)
		writeFwdError(w, fe)
		return
	}
	resp.DurationMs = ms
	f.app.metrics.Inc("fluid_forward_requests_total", "result", "ok")
	slog.Info("forward", "kind", "forward", "direction", "outbound", "client", client, "source_ip", src,
		"profile", req.Profile, "method", req.Method, "host", hostOf(resp.FinalURL), "url", resp.FinalURL,
		"status", resp.Status, "bytes_out", len(resp.Body)+len(resp.BodyBase64), "ms", ms, "redirects", resp.Redirects)
	endSpan(span, resp.Status, nil)
	writeJSON(w, http.StatusOK, resp)
}

func hostOf(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return ""
	}
	return strings.ToLower(p.Hostname())
}

func defaultPort(u *url.URL) int {
	if p := u.Port(); p != "" {
		var n int
		fmt.Sscan(p, &n)
		return n
	}
	if u.Scheme == "http" {
		return 80
	}
	return 443
}

func sameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && strings.EqualFold(a.Hostname(), b.Hostname()) && defaultPort(a) == defaultPort(b)
}

var idempotent = map[string]bool{"GET": true, "HEAD": true, "OPTIONS": true, "PUT": true, "DELETE": true}

func (f *Forwarder) do(ctx context.Context, client string, req *ForwardRequest) (*ForwardResponse, string, error) {
	pc := f.app.store.Proxy()
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}
	if !headerNameRe.MatchString(method) || method == http.MethodConnect {
		return nil, "", ferr(400, "VALIDATION", "bad method %q", req.Method)
	}
	var body []byte
	switch {
	case req.Body != "" && req.BodyBase64 != "":
		return nil, "", ferr(400, "VALIDATION", "set body or body_base64, not both")
	case req.BodyBase64 != "":
		b, err := base64.StdEncoding.DecodeString(req.BodyBase64)
		if err != nil {
			return nil, "", ferr(400, "VALIDATION", "body_base64 is not valid base64")
		}
		body = b
	default:
		body = []byte(req.Body)
	}

	headers := http.Header{}
	for k, v := range req.Headers {
		if !headerNameRe.MatchString(k) {
			return nil, "", ferr(400, "VALIDATION", "bad header name %q", k)
		}
		headers.Set(k, v)
	}
	stripHop(headers)
	headers.Del("Host")
	injected := map[string]bool{} // headers added by the agent (secrets); never sent cross-origin

	var target string
	timeout := f.app.cfg.Forward.DefaultTimeout.D()
	switch {
	case req.Profile != "" && req.URL != "":
		return nil, "", ferr(400, "VALIDATION", "set url or profile, not both")
	case req.Profile != "":
		p := f.app.store.Profile(req.Profile)
		if p == nil {
			return nil, "", ferr(404, "PROFILE_NOT_FOUND", "profile %q not found", req.Profile)
		}
		if !p.allows(client, method) {
			return nil, "", ferr(403, "FORBIDDEN", "profile %q does not allow this client or method", req.Profile)
		}
		if req.Path != "" && !strings.HasPrefix(req.Path, "/") {
			return nil, "", ferr(400, "VALIDATION", "path must start with /")
		}
		base, _ := url.Parse(p.BaseURL)
		ref, err := url.Parse(req.Path)
		if err != nil || ref.Host != "" || ref.Scheme != "" {
			return nil, "", ferr(400, "VALIDATION", "bad path")
		}
		u := *base
		u.Path = strings.TrimSuffix(base.Path, "/") + ref.Path
		u.RawQuery = ref.RawQuery
		target = u.String()
		for k, v := range p.Headers {
			headers.Set(k, v)
			injected[http.CanonicalHeaderKey(k)] = true
		}
		if p.TimeoutSec > 0 {
			timeout = time.Duration(p.TimeoutSec) * time.Second
		}
	case req.URL != "":
		if err := validTargetURL(req.URL); err != nil {
			return nil, "", ferr(400, "VALIDATION", "url: %v", err)
		}
		target = req.URL
	default:
		return nil, "", ferr(400, "VALIDATION", "url or profile is required")
	}
	if req.TimeoutMs > 0 {
		timeout = time.Duration(req.TimeoutMs) * time.Millisecond
	}
	timeout = min(max(timeout, 100*time.Millisecond), f.app.cfg.Forward.MaxTimeout.D())
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cur, _ := url.Parse(target)
	redirects := 0
	for {
		if !pc.hostAllowed(cur.Hostname(), defaultPort(cur)) {
			return nil, cur.String(), ferr(403, "HOST_NOT_ALLOWED", "%q is not in allowed_hosts/allowed_ports", cur.Host)
		}
		resp, err := f.send(ctx, method, cur.String(), headers, body)
		if err != nil {
			switch {
			case errors.Is(err, ErrBlockedAddress):
				return nil, cur.String(), ferr(403, "BLOCKED_ADDRESS", "%v", err)
			case ctx.Err() != nil:
				return nil, cur.String(), ferr(504, "TIMEOUT", "upstream timed out after %s", timeout)
			}
			return nil, cur.String(), ferr(502, "UPSTREAM_FAILURE", "%v", err)
		}
		loc := resp.Header.Get("Location")
		if resp.StatusCode >= 300 && resp.StatusCode < 400 && loc != "" && resp.StatusCode != 304 {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			if redirects >= f.app.cfg.Forward.MaxRedirects {
				return nil, cur.String(), ferr(502, "UPSTREAM_FAILURE", "too many redirects")
			}
			next, err := cur.Parse(loc)
			if err != nil || (next.Scheme != "http" && next.Scheme != "https") {
				return nil, cur.String(), ferr(502, "UPSTREAM_FAILURE", "bad redirect location %q", loc)
			}
			if cur.Scheme == "https" && next.Scheme == "http" {
				return nil, cur.String(), ferr(502, "UPSTREAM_FAILURE", "refusing https to http redirect")
			}
			if !sameOrigin(cur, next) {
				for _, h := range []string{"Authorization", "Cookie", "Proxy-Authorization", "X-Agent-Key"} {
					headers.Del(h)
				}
				for h := range injected {
					headers.Del(h)
				}
			}
			if resp.StatusCode == 303 || ((resp.StatusCode == 301 || resp.StatusCode == 302) && method == http.MethodPost) {
				method, body = http.MethodGet, nil
				headers.Del("Content-Type")
			}
			cur = next
			redirects++
			continue
		}
		defer resp.Body.Close()
		rb, err := io.ReadAll(io.LimitReader(resp.Body, f.app.cfg.Forward.MaxResponseBytes+1))
		if err != nil {
			return nil, cur.String(), ferr(502, "UPSTREAM_FAILURE", "reading response: %v", err)
		}
		if int64(len(rb)) > f.app.cfg.Forward.MaxResponseBytes {
			return nil, cur.String(), ferr(502, "RESPONSE_TOO_LARGE", "response exceeds %d bytes", f.app.cfg.Forward.MaxResponseBytes)
		}
		stripHop(resp.Header)
		out := &ForwardResponse{Status: resp.StatusCode, Headers: resp.Header, FinalURL: cur.String(), Redirects: redirects}
		if utf8.Valid(rb) {
			out.Body = string(rb)
		} else {
			out.BodyBase64 = base64.StdEncoding.EncodeToString(rb)
		}
		return out, cur.String(), nil
	}
}

// send makes one request, retrying idempotent methods on network errors.
func (f *Forwarder) send(ctx context.Context, method, target string, h http.Header, body []byte) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if !idempotent[method] || errors.Is(lastErr, ErrBlockedAddress) {
				break
			}
			select {
			case <-ctx.Done():
				return nil, lastErr
			case <-time.After(time.Duration(attempt) * 200 * time.Millisecond):
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header = h.Clone()
		injectTrace(ctx, req.Header)
		resp, err := f.client.Do(req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
