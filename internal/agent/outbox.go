package agent

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/skpjr001/fluid-agent/internal/database"
	"github.com/skpjr001/fluid-agent/internal/telemetry"
)

// Event is one inbound call (webhook or ingest) as stored.
type Event struct {
	ID          string              `json:"id"`
	TS          time.Time           `json:"ts"`
	Source      string              `json:"source"` // webhook | ingest
	Route       string              `json:"route"`
	Vendor      string              `json:"vendor,omitempty"`
	EventType   string              `json:"event_type,omitempty"`
	Method      string              `json:"method"`
	Path        string              `json:"path"`
	Query       string              `json:"query,omitempty"`
	SourceIP    string              `json:"source_ip,omitempty"`
	Headers     map[string][]string `json:"headers"`
	Body        string              `json:"body,omitempty"`
	BodyBase64  string              `json:"body_base64,omitempty"`
	BodySize    int                 `json:"body_size"`
	BodyPurged  bool                `json:"body_purged,omitempty"`
	Traceparent string              `json:"traceparent,omitempty"`
	Status      string              `json:"status"` // pending | delivered | failed | partial
	Deliveries  []Delivery          `json:"deliveries,omitempty"`

	raw []byte
}

// Delivery is one attempt-tracked send of an event to one target.
type Delivery struct {
	ID          int64     `json:"id"`
	EventID     string    `json:"event_id"`
	Route       string    `json:"route"`
	Target      string    `json:"target"`
	Mode        string    `json:"mode"`   // sync | async
	Status      string    `json:"status"` // pending | inflight | delivered | failed | dead
	Attempts    int       `json:"attempts"`
	MaxAttempts int       `json:"max_attempts"`
	TimeoutSec  int       `json:"timeout_sec"`
	NextAt      time.Time `json:"next_at"`
	LastStatus  int       `json:"last_status,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	DurationMs  int64     `json:"duration_ms,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// DeliverySpec describes a delivery to create with an event.
type DeliverySpec struct {
	Target      string
	Mode        string
	MaxAttempts int
	TimeoutSec  int
}

type Outbox struct {
	app    *App
	client *http.Client
	wake   chan struct{}
	wg     sync.WaitGroup

	// Graceful shutdown: Run stops claiming when its context ends; attempts
	// still in flight at the shutdown deadline are aborted and requeued.
	abortCtx    context.Context
	abort       context.CancelFunc
	interrupted atomic.Int64
}

func NewOutbox(app *App) *Outbox {
	abortCtx, abort := context.WithCancel(context.Background())
	return &Outbox{
		app:      app,
		abortCtx: abortCtx,
		abort:    abort,
		wake:     make(chan struct{}, 1),
		client: &http.Client{
			// Targets are configured by editors and are often internal hosts,
			// so the private-address guard is not applied here. Redirects are
			// never followed and environment proxies are ignored.
			Transport:     &http.Transport{Proxy: nil, MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Record stores the event and its deliveries in one transaction.
func (o *Outbox) Record(ctx context.Context, ev *Event, specs []DeliverySpec) ([]Delivery, error) {
	hdr, _ := json.Marshal(ev.Headers)
	now := database.NowMs()
	tx, err := o.app.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (id, ts, source, route, vendor, event_type, method, path,
		query, source_ip, headers, body, body_size, traceparent) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		ev.ID, ev.TS.UnixMilli(), ev.Source, ev.Route, nullStr(ev.Vendor), nullStr(ev.EventType), ev.Method,
		ev.Path, nullStr(ev.Query), nullStr(ev.SourceIP), string(hdr), ev.raw, len(ev.raw), nullStr(ev.Traceparent)); err != nil {
		return nil, err
	}
	out := make([]Delivery, 0, len(specs))
	for _, s := range specs {
		status := "pending"
		if s.Mode == "sync" {
			status = "inflight"
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO deliveries (event_id, route, target, mode, status, attempts,
			max_attempts, timeout_sec, next_at, created_at, updated_at) VALUES (?,?,?,?,?,0,?,?,?,?,?)`,
			ev.ID, ev.Route, s.Target, s.Mode, status, s.MaxAttempts, s.TimeoutSec, now, now, now)
		if err != nil {
			return nil, err
		}
		id, _ := res.LastInsertId()
		out = append(out, Delivery{ID: id, EventID: ev.ID, Route: ev.Route, Target: s.Target, Mode: s.Mode,
			Status: status, MaxAttempts: s.MaxAttempts, TimeoutSec: s.TimeoutSec,
			NextAt: time.UnixMilli(now), CreatedAt: time.UnixMilli(now), UpdatedAt: time.UnixMilli(now)})
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	ev.Status = "pending"
	ev.BodySize = len(ev.raw)
	o.app.hub.Publish("events", ev.summary())
	o.Wake()
	return out, nil
}

func (ev *Event) summary() *Event {
	c := *ev
	c.Headers, c.Body, c.BodyBase64, c.Deliveries = nil, "", "", nil
	return &c
}

func (o *Outbox) Wake() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// Recover runs at startup: async rows that were in flight when the process
// died go back to pending; interrupted sync rows are marked failed (the
// sender already got an error and will retry on its own).
func (o *Outbox) Recover(ctx context.Context) error {
	now := database.NowMs()
	if _, err := o.app.db.ExecContext(ctx, `UPDATE deliveries SET status='pending', next_at=?, updated_at=?
		WHERE status='inflight' AND mode='async'`, now, now); err != nil {
		return err
	}
	_, err := o.app.db.ExecContext(ctx, `UPDATE deliveries SET status='failed', last_error='interrupted by restart',
		updated_at=? WHERE status='inflight' AND mode='sync'`, now)
	return err
}

// Run dispatches due async deliveries until ctx is cancelled.
func (o *Outbox) Run(ctx context.Context) {
	sem := make(chan struct{}, o.app.cfg.Outbox.Workers)
	poll := time.NewTicker(o.app.cfg.Outbox.PollInterval.D())
	defer poll.Stop()
	for {
		for {
			free := cap(sem) - len(sem)
			if free == 0 {
				break
			}
			due, err := o.claim(ctx, free)
			if err != nil {
				if ctx.Err() == nil {
					slog.Error("outbox claim failed", "kind", "outbox", "err", err)
				}
				break
			}
			for _, d := range due {
				sem <- struct{}{}
				o.wg.Add(1)
				go func(d Delivery) {
					defer func() { <-sem; o.wg.Done() }()
					o.deliverAsync(ctx, d)
				}(d)
			}
			if len(due) < free {
				break
			}
		}
		select {
		case <-ctx.Done():
			o.wg.Wait()
			return
		case <-poll.C:
		case <-o.wake:
		}
	}
}

const deliveryCols = `id, event_id, route, target, mode, status, attempts, max_attempts, timeout_sec, next_at,
	last_status, last_error, duration_ms, created_at, updated_at`

func scanDelivery(sc interface{ Scan(...any) error }) (Delivery, error) {
	var d Delivery
	var next, created, updated int64
	var lastStatus, dur *int64
	var lastErr *string
	err := sc.Scan(&d.ID, &d.EventID, &d.Route, &d.Target, &d.Mode, &d.Status, &d.Attempts, &d.MaxAttempts,
		&d.TimeoutSec, &next, &lastStatus, &lastErr, &dur, &created, &updated)
	d.NextAt, d.CreatedAt, d.UpdatedAt = time.UnixMilli(next), time.UnixMilli(created), time.UnixMilli(updated)
	d.LastStatus, d.LastError, d.DurationMs = int(derefInt(lastStatus)), deref(lastErr), derefInt(dur)
	return d, err
}

func (o *Outbox) claim(ctx context.Context, n int) ([]Delivery, error) {
	now := database.NowMs()
	rows, err := o.app.db.QueryContext(ctx, `UPDATE deliveries SET status='inflight', updated_at=?
		WHERE id IN (SELECT id FROM deliveries WHERE status='pending' AND next_at <= ? ORDER BY next_at LIMIT ?)
		RETURNING `+deliveryCols, now, now, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// loadEvent fetches an event including its body.
func (o *Outbox) loadEvent(ctx context.Context, id string) (*Event, error) {
	row := o.app.db.QueryRowContext(ctx, `SELECT id, ts, source, route, vendor, event_type, method, path, query,
		source_ip, headers, body, body_size, traceparent FROM events WHERE id = ?`, id)
	var ev Event
	var ts int64
	var vendor, et, q, src, tp *string
	var hdr string
	if err := row.Scan(&ev.ID, &ts, &ev.Source, &ev.Route, &vendor, &et, &ev.Method, &ev.Path, &q, &src, &hdr,
		&ev.raw, &ev.BodySize, &tp); err != nil {
		return nil, err
	}
	ev.TS = time.UnixMilli(ts)
	ev.Vendor, ev.EventType, ev.Query, ev.SourceIP, ev.Traceparent = deref(vendor), deref(et), deref(q), deref(src), deref(tp)
	json.Unmarshal([]byte(hdr), &ev.Headers)
	ev.BodyPurged = ev.raw == nil && ev.BodySize > 0
	return &ev, nil
}

// routeOptions returns the current options of the route that produced ev.
func (o *Outbox) routeOptions(ev *Event) (*RouteOptions, bool) {
	if ev.Source == "ingest" {
		for _, r := range o.app.store.Ingest() {
			if r.ID == ev.Route {
				return &r.RouteOptions, true
			}
		}
		return nil, false
	}
	if w := o.app.store.Webhook(ev.Route); w != nil {
		return &w.RouteOptions, true
	}
	return nil, false
}

// outboundRequest builds the request sent to a target.
func (o *Outbox) outboundRequest(ctx context.Context, ev *Event, d Delivery, opts *RouteOptions) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, ev.Method, d.Target, bytes.NewReader(ev.raw))
	if err != nil {
		return nil, err
	}
	for k, vs := range ev.Headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	stripHop(req.Header)
	req.Header.Del("Host")
	req.Header.Del("Content-Length")
	if ev.SourceIP != "" {
		req.Header.Set("X-Forwarded-For", ev.SourceIP)
	}
	req.Header.Set("X-Fluid-Event-Id", ev.ID)
	req.Header.Set("X-Fluid-Delivery-Id", strconv.FormatInt(d.ID, 10))
	req.Header.Set("X-Fluid-Route", ev.Route)
	req.Header.Set("X-Fluid-Path", ev.Path)
	if ev.Source == "webhook" {
		req.Header.Set("X-Webhook-Id", ev.Route)
	}
	if ev.Vendor != "" {
		req.Header.Set("X-Fluid-Vendor", ev.Vendor)
	}
	if ev.EventType != "" {
		req.Header.Set("X-Fluid-Event-Type", ev.EventType)
	}
	if opts != nil {
		for k, v := range opts.AddHeaders {
			req.Header.Set(k, v)
		}
		opts.TargetAuth.apply(req.Header)
	}
	telemetry.Inject(ctx, req.Header)
	return req, nil
}

// attemptResult classifies one send.
type attemptResult struct {
	status    int
	err       error
	retryable bool
	ms        int64
	resp      *http.Response // only for sync deliveries; body buffered
	capture   *Capture       // the request sent and the response received
}

func (o *Outbox) attempt(ctx context.Context, ev *Event, d Delivery, opts *RouteOptions, keepResp bool) attemptResult {
	start := time.Now()
	ctx, span := telemetry.Tracer().Start(ctx, "delivery", trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("fluid.route", d.Route), attribute.Int64("fluid.delivery_id", d.ID),
			attribute.String("url.full", d.Target), attribute.Int("fluid.attempt", d.Attempts+1)))
	actx, cancel := context.WithTimeout(ctx, time.Duration(d.TimeoutSec)*time.Second)
	defer cancel()
	req, err := o.outboundRequest(actx, ev, d, opts)
	if err != nil {
		telemetry.EndSpan(span, 0, err)
		return attemptResult{err: err, ms: time.Since(start).Milliseconds()}
	}
	c := o.app.newCapture()
	defer c.Finish()
	c.SetRequest(requestHeaders(req.Method, req.URL.String(), req.URL.Host, req.Header), ev.raw)
	resp, err := o.client.Do(req)
	if err != nil {
		telemetry.EndSpan(span, 0, err)
		return attemptResult{err: err, retryable: true, ms: time.Since(start).Milliseconds(), capture: c}
	}
	rb, rerr := io.ReadAll(io.LimitReader(resp.Body, o.app.cfg.MaxBodyBytes))
	resp.Body.Close()
	c.SetResponse(responseHeaders(resp.StatusCode, resp.Header), rb)
	res := attemptResult{status: resp.StatusCode, ms: time.Since(start).Milliseconds(), capture: c}
	switch {
	case rerr != nil:
		res.err, res.retryable = rerr, true
	case resp.StatusCode >= 500 || resp.StatusCode == 408 || resp.StatusCode == 429:
		res.err, res.retryable = fmt.Errorf("target answered %s", resp.Status), true
	case resp.StatusCode >= 400:
		res.err = fmt.Errorf("target answered %s", resp.Status)
	}
	if keepResp && rerr == nil {
		resp.Body = io.NopCloser(bytes.NewReader(rb))
		res.resp = resp
	}
	telemetry.EndSpan(span, resp.StatusCode, res.err)
	return res
}

func (o *Outbox) backoff(attempts int) time.Duration {
	d := time.Second << min(attempts, 20)
	d = min(d, o.app.cfg.Outbox.MaxBackoff.D())
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1)) // jitter
}

func (o *Outbox) deliverAsync(ctx context.Context, d Delivery) {
	ev, err := o.loadEvent(ctx, d.EventID)
	if err != nil {
		o.finish(d, "dead", 0, 0, "event missing: "+err.Error())
		return
	}
	opts, ok := o.routeOptions(ev)
	if !ok {
		o.finish(d, "dead", 0, 0, "route "+ev.Route+" no longer exists")
		return
	}
	actx, cancel := context.WithCancel(telemetry.FromTraceparent(ev.Traceparent))
	defer cancel()
	stop := context.AfterFunc(o.abortCtx, cancel) // shutdown deadline reached
	defer stop()
	res := o.attempt(actx, ev, d, opts, false)
	if res.err != nil && o.abortCtx.Err() != nil {
		// Interrupted by shutdown, not the target's fault: back to the queue
		// with the attempt not counted; it resumes on the next start.
		o.requeue(d)
		return
	}
	d.Attempts++
	o.logAttempt(ev, d, res)
	switch {
	case res.err == nil:
		o.finish(d, "delivered", res.status, res.ms, "")
	case res.retryable && d.Attempts < d.MaxAttempts:
		next := time.Now().Add(o.backoff(d.Attempts))
		o.update(d, "pending", res.status, res.ms, res.err.Error(), next)
	default:
		o.finish(d, "dead", res.status, res.ms, res.err.Error())
	}
}

// requeue returns an interrupted delivery to pending, due now.
func (o *Outbox) requeue(d Delivery) {
	o.interrupted.Add(1)
	now := database.NowMs()
	if _, err := o.app.db.Exec(`UPDATE deliveries SET status='pending', next_at=?, updated_at=? WHERE id=? AND status='inflight'`,
		now, now, d.ID); err != nil {
		slog.Error("requeue after shutdown failed (recovered on next start)", "kind", "outbox", "delivery_id", d.ID, "err", err)
		return
	}
	slog.Warn("delivery interrupted by shutdown; it resumes on the next start", "kind", "delivery", "direction", "outbound",
		"route", d.Route, "url", d.Target, "delivery_id", d.ID, "event_id", d.EventID)
}

// DeliverSync performs a sync delivery inline (with the route's retries) and
// returns the target's response for the caller, or nil on failure.
func (o *Outbox) DeliverSync(ctx context.Context, ev *Event, d Delivery, opts *RouteOptions) *http.Response {
	backoff := 500 * time.Millisecond
	var res attemptResult
	for {
		res = o.attempt(ctx, ev, d, opts, true)
		d.Attempts++
		o.logAttempt(ev, d, res)
		if res.err == nil || !res.retryable || d.Attempts >= d.MaxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			res.err = ctx.Err()
		case <-time.After(backoff):
			backoff *= 2
			continue
		}
		break
	}
	switch {
	case res.err == nil:
		o.finish(d, "delivered", res.status, res.ms, "")
	case res.resp != nil: // target answered (4xx/5xx): pass its answer through
		o.finish(d, "failed", res.status, res.ms, res.err.Error())
	default:
		o.finish(d, "failed", res.status, res.ms, res.err.Error())
	}
	return res.resp
}

func (o *Outbox) logAttempt(ev *Event, d Delivery, res attemptResult) {
	level, msg := slog.LevelInfo, "delivery ok"
	result := "ok"
	if res.err != nil {
		level, msg, result = slog.LevelWarn, "delivery failed", "error"
	}
	o.app.metrics.Inc("fluid_deliveries_total", "mode", d.Mode, "result", result)
	attrs := []any{"kind", "delivery", "direction", "outbound", "route", d.Route, "method", ev.Method,
		"host", hostOf(d.Target), "url", d.Target, "status", res.status, "ms", res.ms, "bytes_in", len(ev.raw),
		"event_id", ev.ID, "delivery_id", d.ID, "attempt", d.Attempts, "max_attempts", d.MaxAttempts, "mode", d.Mode,
		"capture", res.capture}
	if res.err != nil {
		attrs = append(attrs, "err", res.err.Error())
	}
	slog.Log(telemetry.FromTraceparent(ev.Traceparent), level, msg, attrs...)
}

func (o *Outbox) finish(d Delivery, status string, code int, ms int64, errMsg string) {
	o.update(d, status, code, ms, errMsg, time.Now())
	if !o.app.cfg.Database.StoreEventBodies {
		o.app.db.Exec(`UPDATE events SET body = NULL WHERE id = ? AND NOT EXISTS (
			SELECT 1 FROM deliveries WHERE event_id = ? AND status IN ('pending','inflight'))`, d.EventID, d.EventID)
	}
}

func (o *Outbox) update(d Delivery, status string, code int, ms int64, errMsg string, next time.Time) {
	now := database.NowMs()
	_, err := o.app.db.Exec(`UPDATE deliveries SET status=?, attempts=?, last_status=?, last_error=?, duration_ms=?,
		next_at=?, updated_at=? WHERE id=?`, status, d.Attempts, nullInt(int64(code)), nullStr(errMsg), ms,
		next.UnixMilli(), now, d.ID)
	if err != nil {
		slog.Error("outbox update failed", "kind", "outbox", "delivery_id", d.ID, "err", err)
		return
	}
	d.Status, d.LastStatus, d.LastError, d.DurationMs = status, code, errMsg, ms
	d.NextAt, d.UpdatedAt = next, time.UnixMilli(now)
	o.app.hub.Publish("deliveries", d)
}

// Retry re-queues a failed or dead delivery for one more attempt now.
func (o *Outbox) Retry(ctx context.Context, id int64) error {
	now := database.NowMs()
	res, err := o.app.db.ExecContext(ctx, `UPDATE deliveries SET status='pending', max_attempts=attempts+1,
		next_at=?, updated_at=? WHERE id=? AND status IN ('dead','failed')`, now, now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("delivery not found or not in a failed/dead state")
	}
	var mode string
	o.app.db.QueryRowContext(ctx, `SELECT mode FROM deliveries WHERE id=?`, id).Scan(&mode)
	if mode == "sync" { // retried sync deliveries continue in the background
		o.app.db.ExecContext(ctx, `UPDATE deliveries SET mode='async' WHERE id=?`, id)
	}
	o.Wake()
	return nil
}

// RetryDead re-queues every dead delivery (optionally for one route).
func (o *Outbox) RetryDead(ctx context.Context, route string) (int64, error) {
	now := database.NowMs()
	q := `UPDATE deliveries SET status='pending', mode='async', max_attempts=attempts+1, next_at=?, updated_at=?
		WHERE status IN ('dead','failed')`
	args := []any{now, now}
	if route != "" {
		q += ` AND route=?`
		args = append(args, route)
	}
	res, err := o.app.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	o.Wake()
	return n, nil
}

// ---------- queries for the API / UI ----------

type DeliveryQuery struct {
	Route    string
	Status   string
	EventID  string
	BeforeID int64
	Limit    int
	List     ListParams // column filters, sort, offset
}

func (q DeliveryQuery) where() (string, []any) {
	conds, args := append([]string(nil), q.List.Conds...), append([]any(nil), q.List.Args...)
	if q.Route != "" {
		conds, args = append(conds, "route = ?"), append(args, q.Route)
	}
	if q.Status != "" {
		conds, args = append(conds, "status = ?"), append(args, q.Status)
	}
	if q.EventID != "" {
		conds, args = append(conds, "event_id = ?"), append(args, q.EventID)
	}
	if q.BeforeID > 0 {
		conds, args = append(conds, "id < ?"), append(args, q.BeforeID)
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// CountDeliveries is the total for the same filters (for paging).
func (o *Outbox) CountDeliveries(ctx context.Context, q DeliveryQuery) (int64, error) {
	where, args := q.where()
	var n int64
	err := o.app.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries`+where, args...).Scan(&n)
	return n, err
}

func (o *Outbox) ListDeliveries(ctx context.Context, q DeliveryQuery) ([]Delivery, int64, error) {
	if q.Limit <= 0 || q.Limit > maxPageSize {
		q.Limit = 100
	}
	where, args := q.where()
	order := q.List.Order
	if order == "" {
		order = "id DESC"
	}
	rows, err := o.app.db.QueryContext(ctx, `SELECT `+deliveryCols+` FROM deliveries`+where+` ORDER BY `+order+` LIMIT ? OFFSET ?`,
		append(args, q.Limit, q.List.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, d)
	}
	var next int64
	if len(out) == q.Limit {
		next = out[len(out)-1].ID
	}
	return out, next, rows.Err()
}

type EventQuery struct {
	Route    string
	Source   string
	Status   string // pending | delivered | failed | partial
	Text     string // substring of path or event type
	Since    time.Time
	BeforeID string
	Limit    int
	List     ListParams // column filters, sort, offset
}

const eventStatusSQL = `(SELECT CASE
	WHEN COUNT(*) = 0 THEN 'delivered'
	WHEN SUM(status IN ('pending','inflight')) > 0 THEN 'pending'
	WHEN SUM(status = 'delivered') = COUNT(*) THEN 'delivered'
	WHEN SUM(status = 'delivered') = 0 THEN 'failed'
	ELSE 'partial' END FROM deliveries d WHERE d.event_id = e.id)`

func (q EventQuery) where() (string, []any) {
	conds, args := append([]string(nil), q.List.Conds...), append([]any(nil), q.List.Args...)
	if q.Route != "" {
		conds, args = append(conds, "e.route = ?"), append(args, q.Route)
	}
	if q.Source != "" {
		conds, args = append(conds, "e.source = ?"), append(args, q.Source)
	}
	if q.Status != "" {
		conds, args = append(conds, eventStatusSQL+" = ?"), append(args, q.Status)
	}
	if q.Text != "" {
		conds, args = append(conds, "(e.path LIKE ? OR e.event_type LIKE ? OR e.vendor LIKE ?)"),
			append(args, "%"+q.Text+"%", "%"+q.Text+"%", "%"+q.Text+"%")
	}
	if !q.Since.IsZero() {
		conds, args = append(conds, "e.ts >= ?"), append(args, q.Since.UnixMilli())
	}
	if q.BeforeID != "" {
		conds, args = append(conds, "e.id < ?"), append(args, q.BeforeID)
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// CountEvents is the total for the same filters (for paging).
func (o *Outbox) CountEvents(ctx context.Context, q EventQuery) (int64, error) {
	where, args := q.where()
	var n int64
	err := o.app.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events e`+where, args...).Scan(&n)
	return n, err
}

func (o *Outbox) ListEvents(ctx context.Context, q EventQuery) ([]Event, string, error) {
	if q.Limit <= 0 || q.Limit > maxPageSize {
		q.Limit = 100
	}
	where, args := q.where()
	order := q.List.Order
	if order == "" {
		order = "e.id DESC"
	}
	rows, err := o.app.db.QueryContext(ctx, `SELECT e.id, e.ts, e.source, e.route, e.vendor, e.event_type, e.method,
		e.path, e.query, e.source_ip, e.body_size, e.traceparent, `+eventStatusSQL+` FROM events e`+where+
		` ORDER BY `+order+` LIMIT ? OFFSET ?`, append(args, q.Limit, q.List.Offset)...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var ev Event
		var ts int64
		var vendor, et, qs, src, tp *string
		if err := rows.Scan(&ev.ID, &ts, &ev.Source, &ev.Route, &vendor, &et, &ev.Method, &ev.Path, &qs, &src,
			&ev.BodySize, &tp, &ev.Status); err != nil {
			return nil, "", err
		}
		ev.TS = time.UnixMilli(ts)
		ev.Vendor, ev.EventType, ev.Query, ev.SourceIP, ev.Traceparent = deref(vendor), deref(et), deref(qs), deref(src), deref(tp)
		out = append(out, ev)
	}
	next := ""
	if len(out) == q.Limit {
		next = out[len(out)-1].ID
	}
	return out, next, rows.Err()
}

// GetEvent returns an event with its body and deliveries.
func (o *Outbox) GetEvent(ctx context.Context, id string) (*Event, error) {
	ev, err := o.loadEvent(ctx, id)
	if err != nil {
		return nil, err
	}
	if utf8.Valid(ev.raw) {
		ev.Body = string(ev.raw)
	} else {
		ev.BodyBase64 = base64.StdEncoding.EncodeToString(ev.raw)
	}
	o.app.db.QueryRowContext(ctx, `SELECT `+eventStatusSQL+` FROM events e WHERE e.id = ?`, id).Scan(&ev.Status)
	ev.Deliveries, _, err = o.ListDeliveries(ctx, DeliveryQuery{EventID: id, Limit: 500})
	return ev, err
}

// Counts returns delivery counts by status (for status page and metrics).
func (o *Outbox) Counts(ctx context.Context) map[string]int64 {
	out := map[string]int64{"pending": 0, "inflight": 0, "delivered": 0, "failed": 0, "dead": 0}
	rows, err := o.app.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM deliveries GROUP BY status`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		var n int64
		if rows.Scan(&s, &n) == nil {
			out[s] = n
		}
	}
	return out
}

var errNotFound = sql.ErrNoRows
