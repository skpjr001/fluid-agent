package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type logDetail struct {
	Log     LogRow      `json:"log"`
	Payload *LogPayload `json:"payload"`
	Masked  bool        `json:"masked"`
}

// capturedLog waits for the first log row matching f and returns it as the
// given token sees it.
func capturedLog(t *testing.T, ta *testApp, token string, f map[string]string) logDetail {
	t.Helper()
	var id int64
	eventually(t, "captured log row", func() bool {
		q := LogQuery{Limit: 1}
		for k, v := range f {
			c, a, _ := filterCond(logListSpec.cols[k], v)
			q.List.Conds, q.List.Args = append(q.List.Conds, c), append(q.List.Args, a...)
		}
		rows, _, _ := QueryLogs(t.Context(), ta.db, q)
		if len(rows) == 1 && rows[0].HasPayload {
			id = rows[0].ID
			return true
		}
		return false
	})
	code, body := ta.api("GET", fmt.Sprintf("/api/logs/%d", id), token, nil)
	if code != 200 {
		t.Fatalf("GET log %d: %d %s", id, code, body)
	}
	var d logDetail
	mustJSON(t, body, &d)
	if d.Payload == nil || d.Payload.Request == nil {
		t.Fatalf("no payload: %s", body)
	}
	return d
}

func echoTarget(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Target", "yes")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		fmt.Fprintf(w, `{"echo":%q,"auth":%q}`, b, r.Header.Get("Authorization"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCaptureInboundWebhookFull(t *testing.T) {
	ta := newTestApp(t, nil)
	tgt := echoTarget(t)
	ta.writeJSON("webhooks/pay.json", Webhook{ID: "pay", Target: tgt.URL + "/cb",
		RouteOptions: RouteOptions{TargetAuth: &TargetAuth{Type: "bearer", Token: "target-secret-token-123"}}})
	body := `{"amount":500,"note":"uses ` + clientToken + `"}`
	req, _ := http.NewRequest("POST", ta.inbound.URL+"/hooks/pay/sub?x=1", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer caller-credential-999")
	req.Header.Set("X-Custom", "hello")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Editor: everything exactly as on the wire.
	d := capturedLog(t, ta, editorToken, map[string]string{"kind": "=webhook"})
	rq, rs := d.Payload.Request, d.Payload.Response
	if d.Masked || rq.Headers.Get("Authorization") != "Bearer caller-credential-999" || rq.Headers.Get("X-Custom") != "hello" ||
		rq.Headers.Get(":method") != "POST" || rq.Headers.Get(":path") != "/hooks/pay/sub?x=1" || rq.Body != body || rq.Size != int64(len(body)) {
		t.Fatalf("inbound request: %+v", rq)
	}
	if rs == nil || rs.Headers.Get(":status") != "201" || rs.Headers.Get("X-Target") != "yes" || !strings.Contains(rs.Body, `"echo"`) {
		t.Fatalf("inbound response: %+v", rs)
	}

	// The delivery to the target is captured as sent, with target_auth.
	dl := capturedLog(t, ta, editorToken, map[string]string{"kind": "=delivery"})
	if dl.Payload.Request.Headers.Get("Authorization") != "Bearer target-secret-token-123" || dl.Payload.Request.Body != body ||
		!strings.HasPrefix(dl.Payload.Request.Headers.Get(":path"), tgt.URL+"/cb/sub") || dl.Payload.Response.Headers.Get(":status") != "201" {
		t.Fatalf("delivery capture: %+v %+v", dl.Payload.Request, dl.Payload.Response)
	}

	// Viewer: credentials masked, everything else intact.
	v := capturedLog(t, ta, viewerToken, map[string]string{"kind": "=webhook"})
	if !v.Masked || v.Payload.Request.Headers.Get("Authorization") != payloadMask || v.Payload.Request.Headers.Get("X-Custom") != "hello" ||
		strings.Contains(v.Payload.Request.Body, clientToken) || !strings.Contains(v.Payload.Request.Body, `"amount":500`) {
		t.Fatalf("viewer view: %+v", v.Payload.Request)
	}
	vd := capturedLog(t, ta, viewerToken, map[string]string{"kind": "=delivery"})
	if vd.Payload.Request.Headers.Get("Authorization") != payloadMask || strings.Contains(vd.Payload.Response.Body, "target-secret-token-123") {
		t.Fatalf("viewer delivery: %+v %+v", vd.Payload.Request, vd.Payload.Response)
	}
}

func TestCaptureRejectedAndIngest(t *testing.T) {
	ta := newTestApp(t, nil)
	ta.writeJSON("webhooks/sec.json", Webhook{ID: "sec", Target: "http://127.0.0.1:1/", Async: true,
		RouteOptions: RouteOptions{Auth: &InboundAuth{Token: &TokenAuth{Header: "X-Hook-Token", Value: "hook-token-abcdef"}}}})
	post(t, ta.inbound.URL+"/hooks/sec", `{"bad":true}`, map[string]string{"X-Hook-Token": "wrong-token-zzz"})
	d := capturedLog(t, ta, editorToken, map[string]string{"kind": "=webhook", "status": "401"})
	if d.Payload.Request.Headers.Get("X-Hook-Token") != "wrong-token-zzz" || d.Payload.Response.Headers.Get(":status") != "401" ||
		!strings.Contains(d.Payload.Response.Body, "unauthorized") {
		t.Fatalf("rejected: %+v %+v", d.Payload.Request, d.Payload.Response)
	}
	v := capturedLog(t, ta, viewerToken, map[string]string{"kind": "=webhook", "status": "401"})
	if v.Payload.Request.Headers.Get("X-Hook-Token") != payloadMask {
		t.Fatalf("route auth header must be masked for viewers: %v", v.Payload.Request.Headers)
	}

	ta.writeJSON("ingest/nse.json", IngestRule{ID: "nse", Match: PathMatch{Type: "exact", Value: "/nse"}, Targets: []string{"http://127.0.0.1:1/"},
		Response: &AckResponse{Status: 200, Body: `{"ok":1}`}})
	post(t, ta.inbound.URL+"/nse", "txn=1&status=OK", map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	in := capturedLog(t, ta, editorToken, map[string]string{"kind": "=ingest"})
	if in.Payload.Request.Body != "txn=1&status=OK" || in.Payload.Response.Body != `{"ok":1}` {
		t.Fatalf("ingest: %+v %+v", in.Payload.Request, in.Payload.Response)
	}
}

func TestCaptureForwardAndProxy(t *testing.T) {
	ta := newTestApp(t, func(c *StaticConfig) { c.Database.CaptureMaxBodyBytes = 64 })
	up := echoTarget(t)
	ta.allowUpstream(up.URL)
	ta.writeJSON("profiles/api.json", Profile{Name: "api", BaseURL: up.URL + "/v1", Headers: map[string]string{"Authorization": "Bearer provider-credential-777"}})

	b, _ := json.Marshal(ForwardRequest{Profile: "api", Method: "POST", Path: "/orders", Body: `{"qty":2}`})
	req, _ := http.NewRequest("POST", ta.proxy.URL+"/forward", bytes.NewReader(b))
	req.Header.Set("X-Agent-Key", clientToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	d := capturedLog(t, ta, editorToken, map[string]string{"kind": "=forward"})
	if d.Payload.Request.Headers.Get("Authorization") != "Bearer provider-credential-777" || d.Payload.Request.Body != `{"qty":2}` ||
		d.Payload.Request.Headers.Get(":path") != up.URL+"/v1/orders" || d.Payload.Response.Headers.Get(":status") != "201" {
		t.Fatalf("forward upstream capture: %+v %+v", d.Payload.Request, d.Payload.Response)
	}
	v := capturedLog(t, ta, viewerToken, map[string]string{"kind": "=forward"})
	if v.Payload.Request.Headers.Get("Authorization") != payloadMask || strings.Contains(v.Payload.Response.Body, "provider-credential-777") {
		t.Fatalf("viewer forward: %+v %+v", v.Payload.Request, v.Payload.Response)
	}

	// Plain HTTP through the proxy: a long body is cut at capture_max_body_bytes, its full size kept.
	long := strings.Repeat("A", 500)
	preq, _ := http.NewRequest("POST", up.URL+"/big", strings.NewReader(long))
	presp, err := proxyClient(ta, clientToken).Do(preq)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, presp.Body)
	presp.Body.Close()
	p := capturedLog(t, ta, editorToken, map[string]string{"kind": "=proxy_http"})
	if p.Payload.Request.Size != 500 || p.Payload.Request.Stored != 64 || !p.Payload.Request.Truncated ||
		p.Payload.Response.Headers.Get("X-Target") != "yes" || p.Payload.Response.Size <= 500 || !p.Payload.Response.Truncated {
		t.Fatalf("proxy capture: %+v %+v", p.Payload.Request, p.Payload.Response)
	}

	// CONNECT: only the CONNECT exchange (the tunnel is encrypted end to end).
	pu := strings.TrimPrefix(up.URL, "http://")
	conn, err := net.Dial("tcp", strings.TrimPrefix(ta.proxy.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Bearer %s\r\n\r\n", pu, pu, clientToken)
	cresp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || cresp.StatusCode != 200 {
		t.Fatalf("connect: %v %v", cresp, err)
	}
	conn.Close() // the tunnel is logged when it closes
	c := capturedLog(t, ta, editorToken, map[string]string{"kind": "=proxy_connect"})
	if c.Payload.Request.Headers.Get(":method") != "CONNECT" || c.Payload.Request.Headers.Get("Proxy-Authorization") != "Bearer "+clientToken ||
		c.Payload.Response.Headers.Get(":status") != "200" || c.Payload.Request.Stored != 0 {
		t.Fatalf("connect capture: %+v %+v", c.Payload.Request, c.Payload.Response)
	}
}

func TestCaptureOff(t *testing.T) {
	ta := newTestApp(t, func(c *StaticConfig) { c.Database.CapturePayloads = false })
	ta.writeJSON("webhooks/w.json", Webhook{ID: "w", Target: "http://127.0.0.1:1/", Async: true})
	post(t, ta.inbound.URL+"/hooks/w", "x", nil)
	ta.flushLogs()
	rows, _, _ := QueryLogs(t.Context(), ta.db, LogQuery{Kinds: []string{"webhook"}})
	if len(rows) == 0 || rows[0].HasPayload {
		t.Fatalf("capture_payloads=false must store no payload: %+v", rows)
	}
}

func TestCaptureWaitsForStreamedBody(t *testing.T) {
	c := newCapture(1 << 20)
	written := make(chan bool, 1)
	c.whenDone(func() { written <- true })
	c.SetRequest(http.Header{":method": {"GET"}}, nil)
	rc := c.TeeResponseBody(io.NopCloser(strings.NewReader("streamed body")))
	select {
	case <-written:
		t.Fatal("row written before the exchange finished")
	case <-time.After(20 * time.Millisecond):
	}
	io.ReadAll(rc)
	c.Finish()
	<-written
	if s := c.snapshot(); string(s.respBody) != "streamed body" || s.respSize != 13 {
		t.Fatalf("%+v", s)
	}
	// stdout gets sizes only.
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "capture", c)
	if strings.Contains(buf.String(), "streamed body") || !strings.Contains(buf.String(), `"rs_body_bytes":13`) {
		t.Fatalf("stdout: %s", buf.String())
	}
}

func TestCaptureRefusedCallsKeepTheirBody(t *testing.T) {
	ta := newTestApp(t, nil)
	req, _ := http.NewRequest("POST", ta.proxy.URL+"/forward", strings.NewReader(`{"url":"https://x.example/"}`))
	req.Header.Set("X-Agent-Key", "wrong-key-0000")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	d := capturedLog(t, ta, editorToken, map[string]string{"kind": "=auth"})
	if d.Payload.Request.Body != `{"url":"https://x.example/"}` || d.Payload.Request.Headers.Get("X-Agent-Key") != "wrong-key-0000" ||
		d.Payload.Response.Headers.Get(":status") != "401" || !strings.Contains(d.Payload.Response.Body, "UNAUTHORIZED") {
		t.Fatalf("refused /forward: %+v %+v", d.Payload.Request, d.Payload.Response)
	}
	ta.writeJSON("webhooks/w.json", Webhook{ID: "w", Target: "http://127.0.0.1:1/", Async: true, RouteOptions: RouteOptions{Methods: []string{"PUT"}}})
	post(t, ta.inbound.URL+"/hooks/w", "refused but recorded", nil)
	w := capturedLog(t, ta, editorToken, map[string]string{"kind": "=webhook", "status": "405"})
	if w.Payload.Request.Body != "refused but recorded" {
		t.Fatalf("405 webhook: %+v", w.Payload.Request)
	}
}
