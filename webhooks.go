package main

import (
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
)

// Inbound serves the public listener: /hooks/{id}[/sub/path] for webhooks,
// health endpoints, and every other path for callback ingest rules.
type Inbound struct {
	app     *App
	limiter *RateLimiter
}

func NewInbound(app *App) *Inbound { return &Inbound{app: app, limiter: NewRateLimiter()} }

func (in *Inbound) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", in.app.readyz)
	mux.HandleFunc("/hooks/", in.serveWebhook)
	mux.HandleFunc("/", in.serveIngest)
	return mux
}

// inboundRoute is what serveRoute needs from a webhook or an ingest rule.
type inboundRoute struct {
	source    string // webhook | ingest
	id        string
	opts      *RouteOptions
	vendor    string
	eventType string
	primary   string   // sync/async primary target (webhooks)
	extra     []string // always async (mirrors / ingest targets)
	async     bool
	ack       *AckResponse // ingest reply
}

func (in *Inbound) serveWebhook(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/hooks/")
	id, sub, _ := strings.Cut(rest, "/")
	hook := in.app.store.Webhook(id)
	if hook == nil || hook.Disabled {
		http.NotFound(w, r)
		return
	}
	join := func(t string) string {
		u, _ := url.Parse(t)
		if sub != "" {
			u.Path = strings.TrimSuffix(u.Path, "/") + "/" + sub
		}
		if r.URL.RawQuery != "" {
			if u.RawQuery != "" {
				u.RawQuery += "&" + r.URL.RawQuery
			} else {
				u.RawQuery = r.URL.RawQuery
			}
		}
		return u.String()
	}
	rt := inboundRoute{source: "webhook", id: id, opts: &hook.RouteOptions, primary: join(hook.Target), async: hook.Async}
	for _, m := range hook.Mirrors {
		rt.extra = append(rt.extra, join(m))
	}
	in.serveRoute(w, r, rt)
}

func (in *Inbound) serveIngest(w http.ResponseWriter, r *http.Request) {
	for _, rule := range in.app.store.Ingest() {
		if rule.Disabled || !rule.matches(r.URL.Path) {
			continue
		}
		in.serveRoute(w, r, inboundRoute{source: "ingest", id: rule.ID, opts: &rule.RouteOptions,
			vendor: rule.Vendor, eventType: rule.EventType, extra: rule.Targets, async: true, ack: rule.Response})
		return
	}
	src := ClientIP(r, in.app.cfg.trusted).String()
	in.app.metrics.Inc("fluid_inbound_requests_total", "source", "ingest", "route", "", "result", "unmatched")
	slog.Warn("no ingest rule matched", "kind", "ingest", "direction", "inbound", "source_ip", src,
		"method", r.Method, "url", r.URL.Path, "status", 404)
	http.NotFound(w, r)
}

func (in *Inbound) reject(w http.ResponseWriter, rt inboundRoute, r *http.Request, src string, code int, result, why string, start time.Time) {
	in.app.metrics.Inc("fluid_inbound_requests_total", "source", rt.source, "route", rt.id, "result", result)
	slog.Warn(rt.source+" rejected", "kind", rt.source, "direction", "inbound", "route", rt.id, "source_ip", src,
		"method", r.Method, "url", r.URL.Path, "status", code, "reason", why, "ms", time.Since(start).Milliseconds())
	http.Error(w, strings.ToLower(http.StatusText(code)), code)
}

func (in *Inbound) serveRoute(w http.ResponseWriter, r *http.Request, rt inboundRoute) {
	start := time.Now()
	ip := ClientIP(r, in.app.cfg.trusted)
	src := ip.String()
	if !rt.opts.allowsMethod(r.Method) {
		in.reject(w, rt, r, src, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", start)
		return
	}
	if !rt.opts.allowsIP(ip) {
		in.reject(w, rt, r, src, http.StatusForbidden, "forbidden", "source IP not in source_cidrs", start)
		return
	}
	lim := in.app.cfg.WebhookRateLimit
	if rt.opts.RateLimit != nil {
		lim = *rt.opts.RateLimit
	}
	if ok, wait := in.limiter.Allow(rt.source+"/"+rt.id+"/"+src, lim); !ok {
		in.app.metrics.Inc("fluid_rate_limited_total", "source", rt.source, "route", rt.id)
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		in.reject(w, rt, r, src, http.StatusTooManyRequests, "rate_limited", "rate limit exceeded", start)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, in.app.cfg.MaxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			in.reject(w, rt, r, src, http.StatusRequestEntityTooLarge, "too_large", "body too large", start)
		} else {
			in.reject(w, rt, r, src, http.StatusBadRequest, "bad_request", "reading body: "+err.Error(), start)
		}
		return
	}
	if ok, why := verifyInbound(rt.opts.Auth, r, body); !ok {
		in.reject(w, rt, r, src, http.StatusUnauthorized, "unauthorized", why, start)
		return
	}

	ctx, span := tracer.Start(extractTrace(r.Context(), r.Header), rt.source+" "+rt.id,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attribute.String("fluid.route", rt.id), attribute.String("client.address", src)))

	hdr := r.Header.Clone()
	stripHop(hdr)
	for _, h := range credentialHeaders(rt.opts.Auth) {
		hdr.Del(h)
	}
	hdr.Del("Traceparent")
	hdr.Del("Tracestate")
	if r.Host != "" {
		hdr.Set("X-Forwarded-Host", r.Host)
	}
	ev := &Event{ID: newID(), TS: start, Source: rt.source, Route: rt.id, Vendor: rt.vendor, EventType: rt.eventType,
		Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, SourceIP: src, Headers: hdr, raw: body,
		Traceparent: traceparentOf(ctx)}

	retries := rt.opts.retries()
	var specs []DeliverySpec
	if rt.primary != "" {
		mode := "sync"
		if rt.async {
			mode = "async"
		}
		specs = append(specs, DeliverySpec{Target: rt.primary, Mode: mode, MaxAttempts: retries + 1, TimeoutSec: rt.opts.TimeoutSec})
	}
	asyncRetries := retries
	if !rt.async && asyncRetries == 0 {
		asyncRetries = 8 // mirrors of a sync webhook still get durable retries
	}
	for _, t := range rt.extra {
		specs = append(specs, DeliverySpec{Target: t, Mode: "async", MaxAttempts: asyncRetries + 1, TimeoutSec: rt.opts.TimeoutSec})
	}

	dels, err := in.app.outbox.Record(ctx, ev, specs)
	logDone := func(code int, extra ...any) {
		result := "ok"
		if code >= 400 {
			result = "error"
		}
		in.app.metrics.Inc("fluid_inbound_requests_total", "source", rt.source, "route", rt.id, "result", result)
		attrs := append([]any{"kind", rt.source, "direction", "inbound", "route", rt.id, "source_ip", src,
			"method", r.Method, "host", r.Host, "url", r.URL.Path, "status", code, "bytes_in", len(body),
			"ms", time.Since(start).Milliseconds(), "event_id", ev.ID, "vendor", rt.vendor, "event_type", rt.eventType}, extra...)
		slog.InfoContext(ctx, rt.source+" received", attrs...)
		endSpan(span, code, nil)
	}
	if err != nil {
		slog.ErrorContext(ctx, "storing event failed", "kind", rt.source, "route", rt.id, "err", err)
		if rt.async || rt.primary == "" {
			// We cannot promise delivery, so make the sender retry.
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			logDone(http.StatusServiceUnavailable, "err", err.Error())
			return
		}
		dels = []Delivery{{Target: rt.primary, Mode: "sync", MaxAttempts: retries + 1, TimeoutSec: rt.opts.TimeoutSec}}
	}

	if rt.ack != nil { // ingest: acknowledge once stored
		w.Header().Set("Content-Type", rt.ack.ContentType)
		w.Header().Set("X-Fluid-Event-Id", ev.ID)
		w.WriteHeader(rt.ack.Status)
		io.WriteString(w, rt.ack.Body)
		logDone(rt.ack.Status)
		return
	}
	if rt.async {
		w.Header().Set("X-Fluid-Event-Id", ev.ID)
		w.WriteHeader(http.StatusAccepted)
		logDone(http.StatusAccepted)
		return
	}

	resp := in.app.outbox.DeliverSync(ctx, ev, dels[0], rt.opts)
	if resp == nil {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		logDone(http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	stripHop(resp.Header)
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.Header().Set("X-Fluid-Event-Id", ev.ID)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
	logDone(resp.StatusCode)
}
