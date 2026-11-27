package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skpjr001/fluid-agent/internal/config"
	"github.com/skpjr001/fluid-agent/internal/database"
)

// recorder is a target that records what it receives.
type recorder struct {
	mu     sync.Mutex
	reqs   []*http.Request
	bodies []string
	codes  []int // status to answer per call (last one repeats)
	srv    *httptest.Server
	calls  atomic.Int32
}

func newRecorder(codes ...int) *recorder {
	rc := &recorder{codes: codes}
	rc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		n := int(rc.calls.Add(1)) - 1
		rc.mu.Lock()
		rc.reqs = append(rc.reqs, r)
		rc.bodies = append(rc.bodies, string(b))
		code := 200
		if len(rc.codes) > 0 {
			code = rc.codes[min(n, len(rc.codes)-1)]
		}
		rc.mu.Unlock()
		w.Header().Set("X-From-Target", "yes")
		w.WriteHeader(code)
		fmt.Fprintf(w, "target says %d", code)
	}))
	return rc
}

func (rc *recorder) last() (*http.Request, string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if len(rc.reqs) == 0 {
		return nil, ""
	}
	return rc.reqs[len(rc.reqs)-1], rc.bodies[len(rc.bodies)-1]
}

func post(t *testing.T, u, body string, h map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", u, strings.NewReader(body))
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func deliveries(t *testing.T, ta *testApp, q DeliveryQuery) []Delivery {
	t.Helper()
	ds, _, err := ta.outbox.ListDeliveries(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

func TestWebhookSyncPassthroughAndRecord(t *testing.T) {
	ta := newTestApp(t, nil)
	tgt := newRecorder(201)
	defer tgt.srv.Close()
	ta.writeJSON("webhooks/pay.json", Webhook{Target: tgt.srv.URL + "/cb", RouteOptions: RouteOptions{
		AddHeaders: map[string]string{"X-Env": "dev"}, TargetAuth: &TargetAuth{Type: "bearer", Token: "internal-tok"}}})

	resp, body := post(t, ta.inbound.URL+"/hooks/pay/sub/path?a=1", `{"x":1}`, map[string]string{
		"Content-Type": "application/json", "Authorization": "Bearer should-not-leak", "Traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"})
	if resp.StatusCode != 201 || body != "target says 201" || resp.Header.Get("X-From-Target") != "yes" {
		t.Fatalf("sync passthrough: %d %q", resp.StatusCode, body)
	}
	evID := resp.Header.Get("X-Fluid-Event-Id")
	r, got := tgt.last()
	if r.URL.Path != "/cb/sub/path" || r.URL.RawQuery != "a=1" || got != `{"x":1}` {
		t.Fatalf("relayed %s?%s %q", r.URL.Path, r.URL.RawQuery, got)
	}
	if r.Header.Get("Authorization") != "Bearer internal-tok" {
		t.Fatalf("target_auth not applied / inbound credentials leaked: %q", r.Header.Get("Authorization"))
	}
	if r.Header.Get("X-Env") != "dev" || r.Header.Get("X-Webhook-Id") != "pay" || r.Header.Get("X-Fluid-Event-Id") != evID {
		t.Fatalf("headers: %v", r.Header)
	}
	if !strings.HasPrefix(r.Header.Get("Traceparent"), "00-0af7651916cd43dd8448eb211c80319c-") {
		t.Fatalf("trace not propagated: %q", r.Header.Get("Traceparent"))
	}

	ev, err := ta.outbox.GetEvent(context.Background(), evID)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Status != "delivered" || ev.Body != `{"x":1}` || len(ev.Deliveries) != 1 || ev.Deliveries[0].Mode != "sync" {
		t.Fatalf("event: %+v", ev)
	}
	if _, ok := ev.Headers["Authorization"]; ok {
		t.Fatal("credentials must not be stored")
	}
	ta.flushLogs()
	in, _, _ := QueryLogs(context.Background(), ta.db, LogQuery{Direction: "inbound", Route: "pay"})
	out, _, _ := QueryLogs(context.Background(), ta.db, LogQuery{Direction: "outbound", Kinds: []string{"delivery"}})
	if len(in) != 1 || in[0].Kind != "webhook" || len(out) != 1 || out[0].Status != 201 {
		t.Fatalf("inbound=%+v outbound=%+v", in, out)
	}
	if in[0].TraceID != "0af7651916cd43dd8448eb211c80319c" {
		t.Fatalf("trace id not logged: %+v", in[0])
	}
}

func TestWebhookChecks(t *testing.T) {
	ta := newTestApp(t, nil)
	tgt := newRecorder()
	defer tgt.srv.Close()
	ta.writeJSON("webhooks/w.json", Webhook{Target: tgt.srv.URL, RouteOptions: RouteOptions{
		Methods:   []string{"POST"},
		Auth:      &InboundAuth{HMAC: &HMACAuth{Header: "X-Sig", Secret: "secret-123456"}},
		RateLimit: &config.RateLimit{RequestsPerMinute: 60, Burst: 2},
	}})
	ta.writeJSON("webhooks/cidr.json", Webhook{Target: tgt.srv.URL, RouteOptions: RouteOptions{SourceCIDRs: []string{"52.66.0.0/16"}}})
	ta.writeJSON("webhooks/off.json", Webhook{Target: tgt.srv.URL, Disabled: true})

	good := map[string]string{"X-Sig": sign("secret-123456", "b")}
	if r, _ := post(t, ta.inbound.URL+"/hooks/w", "b", good); r.StatusCode != 200 {
		t.Fatalf("good: %d", r.StatusCode)
	}
	if r, _ := post(t, ta.inbound.URL+"/hooks/w", "b", map[string]string{"X-Sig": "00"}); r.StatusCode != 401 {
		t.Fatalf("bad signature: %d", r.StatusCode)
	}
	if r, _ := post(t, ta.inbound.URL+"/hooks/w", "b", good); r.StatusCode != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("rate limit: %d", r.StatusCode)
	}
	req, _ := http.NewRequest("GET", ta.inbound.URL+"/hooks/cidr", nil)
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != 405 {
		t.Fatalf("method: %d", r.StatusCode)
	}
	if r, _ := post(t, ta.inbound.URL+"/hooks/cidr", "b", nil); r.StatusCode != 403 {
		t.Fatalf("source cidr: %d", r.StatusCode)
	}
	if r, _ := post(t, ta.inbound.URL+"/hooks/off", "b", nil); r.StatusCode != 404 {
		t.Fatalf("disabled: %d", r.StatusCode)
	}
	if r, _ := post(t, ta.inbound.URL+"/hooks/missing", "b", nil); r.StatusCode != 404 {
		t.Fatalf("missing: %d", r.StatusCode)
	}
	if tgt.calls.Load() != 1 {
		t.Fatalf("rejected requests must not reach the target (calls=%d)", tgt.calls.Load())
	}
	if ta.metrics.Get("fluid_rate_limited_total", "source", "webhook", "route", "w") != 1 {
		t.Fatal("rate limit not counted")
	}
}

func TestWebhookBodyLimit(t *testing.T) {
	ta := newTestApp(t, func(c *config.StaticConfig) { c.MaxBodyBytes = 10 })
	tgt := newRecorder()
	defer tgt.srv.Close()
	ta.writeJSON("webhooks/w.json", Webhook{Target: tgt.srv.URL})
	if r, _ := post(t, ta.inbound.URL+"/hooks/w", strings.Repeat("x", 11), nil); r.StatusCode != 413 {
		t.Fatalf("got %d", r.StatusCode)
	}
}

func TestAsyncRetriesThenDelivers(t *testing.T) {
	ta := newTestApp(t, nil)
	tgt := newRecorder(500, 503, 200)
	defer tgt.srv.Close()
	ta.writeJSON("webhooks/a.json", Webhook{Target: tgt.srv.URL, Async: true, RouteOptions: RouteOptions{MaxRetries: intp(5)}})

	r, _ := post(t, ta.inbound.URL+"/hooks/a", "payload", nil)
	if r.StatusCode != 202 {
		t.Fatalf("async reply: %d", r.StatusCode)
	}
	eventually(t, "delivery", func() bool {
		ds := deliveries(t, ta, DeliveryQuery{Route: "a"})
		return len(ds) == 1 && ds[0].Status == "delivered" && ds[0].Attempts == 3
	})
	if tgt.calls.Load() != 3 {
		t.Fatalf("calls = %d", tgt.calls.Load())
	}
}

func TestAsyncDeadLetterAndManualRetry(t *testing.T) {
	ta := newTestApp(t, nil)
	tgt := newRecorder(500, 500, 500, 200)
	defer tgt.srv.Close()
	ta.writeJSON("webhooks/d.json", Webhook{Target: tgt.srv.URL, Async: true, RouteOptions: RouteOptions{MaxRetries: intp(2)}})
	post(t, ta.inbound.URL+"/hooks/d", "x", nil)
	var d Delivery
	eventually(t, "dead letter", func() bool {
		ds := deliveries(t, ta, DeliveryQuery{Route: "d"})
		if len(ds) == 1 {
			d = ds[0]
		}
		return d.Status == "dead"
	})
	if d.Attempts != 3 || d.LastStatus != 500 {
		t.Fatalf("dead delivery: %+v", d)
	}
	// viewers cannot retry, editors can
	if code, _ := ta.api("POST", fmt.Sprintf("/api/deliveries/%d/retry", d.ID), viewerToken, nil); code != 403 {
		t.Fatalf("viewer retry: %d", code)
	}
	if code, b := ta.api("POST", fmt.Sprintf("/api/deliveries/%d/retry", d.ID), editorToken, nil); code != 202 {
		t.Fatalf("editor retry: %d %s", code, b)
	}
	eventually(t, "redelivery", func() bool { return deliveries(t, ta, DeliveryQuery{Route: "d"})[0].Status == "delivered" })
	if code, _ := ta.api("POST", fmt.Sprintf("/api/deliveries/%d/retry", d.ID), editorToken, nil); code != 409 {
		t.Fatalf("retrying a delivered delivery: %d", code)
	}
}

func TestFourXXIsNotRetried(t *testing.T) {
	ta := newTestApp(t, nil)
	tgt := newRecorder(422)
	defer tgt.srv.Close()
	ta.writeJSON("webhooks/f.json", Webhook{Target: tgt.srv.URL, Async: true})
	post(t, ta.inbound.URL+"/hooks/f", "x", nil)
	eventually(t, "dead", func() bool {
		ds := deliveries(t, ta, DeliveryQuery{Route: "f"})
		return len(ds) == 1 && ds[0].Status == "dead"
	})
	if tgt.calls.Load() != 1 {
		t.Fatalf("4xx retried: %d calls", tgt.calls.Load())
	}
}

func TestMirrorsFanOut(t *testing.T) {
	ta := newTestApp(t, nil)
	primary, m1, m2 := newRecorder(200), newRecorder(200), newRecorder(500, 200)
	defer primary.srv.Close()
	defer m1.srv.Close()
	defer m2.srv.Close()
	ta.writeJSON("webhooks/m.json", Webhook{Target: primary.srv.URL, Mirrors: []string{m1.srv.URL + "/a", m2.srv.URL + "/b"}})
	r, body := post(t, ta.inbound.URL+"/hooks/m/x", "fan", nil)
	if r.StatusCode != 200 || body != "target says 200" {
		t.Fatalf("primary sync answer: %d %q", r.StatusCode, body)
	}
	eventually(t, "mirrors", func() bool { return m1.calls.Load() == 1 && m2.calls.Load() == 2 })
	if rq, b := m1.last(); rq.URL.Path != "/a/x" || b != "fan" {
		t.Fatalf("mirror got %s %q", rq.URL.Path, b)
	}
	ev, _ := ta.outbox.GetEvent(context.Background(), r.Header.Get("X-Fluid-Event-Id"))
	eventually(t, "event delivered", func() bool {
		ev, _ = ta.outbox.GetEvent(context.Background(), r.Header.Get("X-Fluid-Event-Id"))
		return ev.Status == "delivered"
	})
	if len(ev.Deliveries) != 3 {
		t.Fatalf("deliveries: %+v", ev.Deliveries)
	}
}

func TestSyncTargetDownGives502(t *testing.T) {
	ta := newTestApp(t, nil)
	ta.writeJSON("webhooks/s.json", Webhook{Target: "http://127.0.0.1:1/x"})
	if r, _ := post(t, ta.inbound.URL+"/hooks/s", "x", nil); r.StatusCode != 502 {
		t.Fatalf("got %d", r.StatusCode)
	}
	ds := deliveries(t, ta, DeliveryQuery{Route: "s"})
	if len(ds) != 1 || ds[0].Status != "failed" {
		t.Fatalf("%+v", ds)
	}
}

func TestRecoverAfterCrash(t *testing.T) {
	ta := newTestApp(t, nil)
	ctx := context.Background()
	ev := &Event{ID: database.NewID(), TS: time.Now(), Source: "webhook", Route: "r", Method: "POST", Path: "/hooks/r",
		Headers: map[string][]string{}, raw: []byte("x")}
	ta.cancel() // stop the dispatcher so rows stay where we put them
	time.Sleep(50 * time.Millisecond)
	ds, err := ta.outbox.Record(ctx, ev, []DeliverySpec{
		{Target: "http://t/a", Mode: "async", MaxAttempts: 3, TimeoutSec: 1},
		{Target: "http://t/s", Mode: "sync", MaxAttempts: 1, TimeoutSec: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	ta.db.Exec(`UPDATE deliveries SET status='inflight' WHERE id=?`, ds[0].ID) // crashed mid-send
	if err := ta.outbox.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, d := range deliveries(t, ta, DeliveryQuery{EventID: ev.ID}) {
		got[d.Mode] = d.Status
	}
	if got["async"] != "pending" || got["sync"] != "failed" {
		t.Fatalf("after recovery: %v", got)
	}
}

func TestIngestRules(t *testing.T) {
	ta := newTestApp(t, nil)
	core := newRecorder(200)
	defer core.srv.Close()
	ta.writeJSON("ingest/nse-pay.json", IngestRule{Priority: 10, Match: PathMatch{Type: "suffix", Value: "/apppaystatus"},
		Vendor: "NSE", EventType: "APP_PAY_STATUS", Targets: []string{core.srv.URL + "/nse"},
		RouteOptions: RouteOptions{Auth: &InboundAuth{Token: &TokenAuth{Header: "X-Callback-Token", Value: "callback-tok"}}}})
	ta.writeJSON("ingest/catch-all.json", IngestRule{Priority: 0, Match: PathMatch{Type: "regex", Value: `^/v1/.*`},
		Vendor: "ANY", Targets: []string{core.srv.URL + "/any"},
		Response: &AckResponse{Status: 202, ContentType: "text/plain", Body: "queued"}})

	r, body := post(t, ta.inbound.URL+"/tenant-a/v1/apppaystatus", `{"s":"ok"}`, map[string]string{"X-Callback-Token": "callback-tok"})
	if r.StatusCode != 200 || body != `{"status":"success"}` {
		t.Fatalf("ack: %d %q", r.StatusCode, body)
	}
	eventually(t, "ingest delivery", func() bool { return core.calls.Load() == 1 })
	rq, b := core.last()
	if rq.URL.Path != "/nse" || b != `{"s":"ok"}` || rq.Header.Get("X-Fluid-Vendor") != "NSE" ||
		rq.Header.Get("X-Fluid-Event-Type") != "APP_PAY_STATUS" || rq.Header.Get("X-Fluid-Path") != "/tenant-a/v1/apppaystatus" {
		t.Fatalf("relayed %s %q %v", rq.URL.Path, b, rq.Header)
	}
	if rq.Header.Get("X-Callback-Token") != "" {
		t.Fatal("inbound token must not be relayed")
	}
	if r, _ := post(t, ta.inbound.URL+"/tenant-a/v1/apppaystatus", "x", nil); r.StatusCode != 401 {
		t.Fatalf("missing token: %d", r.StatusCode)
	}
	r, body = post(t, ta.inbound.URL+"/v1/other", "y", nil)
	if r.StatusCode != 202 || body != "queued" || r.Header.Get("Content-Type") != "text/plain" {
		t.Fatalf("custom ack: %d %q", r.StatusCode, body)
	}
	if r, _ := post(t, ta.inbound.URL+"/nothing/here", "z", nil); r.StatusCode != 404 {
		t.Fatalf("unmatched: %d", r.StatusCode)
	}
	evs, _, _ := ta.outbox.ListEvents(context.Background(), EventQuery{Source: "ingest"})
	if len(evs) != 2 || evs[1].Vendor != "NSE" || evs[0].Route != "catch-all" {
		t.Fatalf("events: %+v", evs)
	}
}

func TestIngestStoreFailureMakesSenderRetry(t *testing.T) {
	ta := newTestApp(t, nil)
	ta.writeJSON("ingest/r.json", IngestRule{Match: PathMatch{Type: "prefix", Value: "/"}, Targets: []string{"http://127.0.0.1:1/"}})
	ta.db.Exec(`DROP TABLE deliveries`) // simulate a broken database
	if r, _ := post(t, ta.inbound.URL+"/cb", "x", nil); r.StatusCode != 503 {
		t.Fatalf("must not acknowledge what was not stored: %d", r.StatusCode)
	}
}

func TestEventBodiesPurgedWhenConfigured(t *testing.T) {
	ta := newTestApp(t, func(c *config.StaticConfig) { c.Database.StoreEventBodies = false })
	tgt := newRecorder(200)
	defer tgt.srv.Close()
	ta.writeJSON("webhooks/p.json", Webhook{Target: tgt.srv.URL})
	r, _ := post(t, ta.inbound.URL+"/hooks/p", "secret-body", nil)
	ev, _ := ta.outbox.GetEvent(context.Background(), r.Header.Get("X-Fluid-Event-Id"))
	if ev.Body != "" || !ev.BodyPurged || ev.BodySize != len("secret-body") {
		t.Fatalf("body should be purged after delivery: %+v", ev)
	}
}
