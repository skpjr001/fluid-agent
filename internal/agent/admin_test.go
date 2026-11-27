package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAdminRoles(t *testing.T) {
	ta := newTestApp(t, nil)
	if code, _ := ta.api("GET", "/api/status", "", nil); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := ta.api("GET", "/api/status", "wrong-token-wrong-token", nil); code != 401 {
		t.Fatalf("wrong token: %d", code)
	}
	code, body := ta.api("GET", "/api/whoami", viewerToken, nil)
	if code != 200 || !strings.Contains(string(body), `"viewer"`) {
		t.Fatalf("%d %s", code, body)
	}
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/webhooks"}, {"PUT", "/api/webhooks/x"}, {"PATCH", "/api/webhooks/x"}, {"DELETE", "/api/webhooks/x"},
		{"POST", "/api/profiles"}, {"POST", "/api/ingest"}, {"PUT", "/api/proxy"}, {"POST", "/api/proxy/clients"},
		{"POST", "/api/reload"}, {"POST", "/api/deliveries/retry-dead"},
	} {
		if code, _ := ta.api(c.method, c.path, viewerToken, "{}"); code != 403 {
			t.Errorf("viewer %s %s: %d", c.method, c.path, code)
		}
	}
	for _, p := range []string{"/api/status", "/api/webhooks", "/api/profiles", "/api/ingest", "/api/proxy", "/api/events",
		"/api/deliveries", "/api/logs", "/api/logs/summary", "/metrics"} {
		if code, b := ta.api("GET", p, viewerToken, nil); code != 200 {
			t.Errorf("viewer GET %s: %d %s", p, code, b)
		}
	}
}

func TestAdminTokensHotReloadAndEnv(t *testing.T) {
	ta := newTestApp(t, nil)
	newTok := "rotated-editor-token-0123456789"
	ta.writeJSON("admin.json", AdminConfig{Tokens: []AdminToken{{Name: "ed2", Token: newTok, Role: RoleEditor}}})
	if code, _ := ta.api("GET", "/api/status", editorToken, nil); code != 401 {
		t.Fatal("removed token still works")
	}
	if code, _ := ta.api("GET", "/api/status", newTok, nil); code != 200 {
		t.Fatal("new token does not work")
	}
	t.Setenv("PROXY_ADMIN_TOKEN", "env-token-0123456789abcdef")
	code, body := ta.api("GET", "/api/whoami", "env-token-0123456789abcdef", nil)
	if code != 200 || !strings.Contains(string(body), `"editor"`) {
		t.Fatalf("env token: %d %s", code, body)
	}
}

func TestWebhookCRUDAndSecretMasking(t *testing.T) {
	ta := newTestApp(t, nil)
	in := map[string]any{"id": "w", "target": "https://t.example/cb", "async": true,
		"auth":        map[string]any{"hmac": map[string]any{"header": "X-Sig", "secret": "hmac-secret-1"}},
		"target_auth": map[string]any{"type": "bearer", "token": "target-token"},
		"add_headers": map[string]any{"X-Env": "dev"}}
	code, body := ta.api("POST", "/api/webhooks", editorToken, in)
	if code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	if strings.Contains(string(body), "hmac-secret-1") || strings.Contains(string(body), "target-token") {
		t.Fatalf("secrets returned: %s", body)
	}
	if code, _ := ta.api("POST", "/api/webhooks", editorToken, in); code != 409 {
		t.Fatalf("duplicate: %d", code)
	}
	// editors see add_headers, viewers do not
	_, eb := ta.api("GET", "/api/webhooks/w", editorToken, nil)
	_, vb := ta.api("GET", "/api/webhooks/w", viewerToken, nil)
	if !strings.Contains(string(eb), `"X-Env": "dev"`) || strings.Contains(string(vb), `"dev"`) {
		t.Fatalf("editor=%s\nviewer=%s", eb, vb)
	}

	// PATCH a field; masked secrets sent back unchanged keep their stored values
	var got Webhook
	mustJSON(t, eb, &got)
	code, body = ta.api("PATCH", "/api/webhooks/w", editorToken, map[string]any{"disabled": true, "auth": got.Auth, "target_auth": got.TargetAuth})
	if code != 200 {
		t.Fatalf("patch: %d %s", code, body)
	}
	w := ta.store.Webhook("w")
	if !w.Disabled || w.Auth.HMAC.Secret != "hmac-secret-1" || w.TargetAuth.Token != "target-token" || w.Target != "https://t.example/cb" {
		t.Fatalf("after patch: %+v %+v %+v", w, w.Auth.HMAC, w.TargetAuth)
	}
	if w.CreatedAt.IsZero() || !w.UpdatedAt.After(w.CreatedAt) && !w.UpdatedAt.Equal(w.CreatedAt) {
		t.Fatal("timestamps")
	}
	// file on disk is the source of truth
	b, _ := os.ReadFile(filepath.Join(ta.dir, "webhooks", "w.json"))
	if !strings.Contains(string(b), "hmac-secret-1") || !strings.Contains(string(b), `"disabled": true`) {
		t.Fatalf("file: %s", b)
	}
	// validation errors are 400 with the reason
	code, body = ta.api("PUT", "/api/webhooks/w", editorToken, map[string]any{"target": "ftp://x"})
	if code != 400 || !strings.Contains(string(body), "target") {
		t.Fatalf("invalid put: %d %s", code, body)
	}
	if code, _ := ta.api("PATCH", "/api/webhooks/w", editorToken, map[string]any{"bogus": 1}); code != 400 {
		t.Fatal("unknown field accepted")
	}
	if code, _ := ta.api("DELETE", "/api/webhooks/w", editorToken, nil); code != 204 {
		t.Fatal("delete")
	}
	if ta.store.Webhook("w") != nil {
		t.Fatal("still loaded after delete")
	}
	if code, _ := ta.api("DELETE", "/api/webhooks/w", editorToken, nil); code != 404 {
		t.Fatal("delete missing")
	}
}

func TestProfilesAndIngestCRUD(t *testing.T) {
	ta := newTestApp(t, nil)
	code, body := ta.api("POST", "/api/profiles", editorToken, map[string]any{"name": "rz", "base_url": "https://api.razorpay.com/v1",
		"headers": map[string]string{"Authorization": "Basic abc"}})
	if code != 201 || strings.Contains(string(body), "Basic abc") {
		t.Fatalf("%d %s", code, body)
	}
	code, body = ta.api("POST", "/api/ingest", editorToken, map[string]any{"id": "nse", "match": map[string]string{"type": "suffix", "value": "/x"},
		"targets": []string{"https://core.example/cb"}})
	if code != 201 {
		t.Fatalf("%d %s", code, body)
	}
	var r IngestRule
	mustJSON(t, body, &r)
	if r.Response.Status != 200 || r.MaxRetries == nil || *r.MaxRetries != 8 {
		t.Fatalf("ingest defaults: %+v", r)
	}
}

func TestProxyClientsAPI(t *testing.T) {
	ta := newTestApp(t, nil)
	code, body := ta.api("POST", "/api/proxy/clients", editorToken, map[string]any{"name": "ci"})
	if code != 201 {
		t.Fatalf("%d %s", code, body)
	}
	var c clientView
	mustJSON(t, body, &c)
	if len(c.Token) != 64 {
		t.Fatalf("generated token shown once: %+v", c)
	}
	_, list := ta.api("GET", "/api/proxy", viewerToken, nil)
	if strings.Contains(string(list), c.Token) || strings.Contains(string(list), clientToken) {
		t.Fatalf("tokens must be masked: %s", list)
	}
	code, body = ta.api("POST", "/api/proxy/clients/ci/rotate", editorToken, nil)
	var c2 clientView
	mustJSON(t, body, &c2)
	if code != 200 || c2.Token == c.Token {
		t.Fatal("rotate")
	}
	if name, ok := authenticateClient(ta.store.Proxy(), c2.Token); !ok || name != "ci" {
		t.Fatal("rotated token should authenticate")
	}
	if _, ok := authenticateClient(ta.store.Proxy(), c.Token); ok {
		t.Fatal("old token should be revoked")
	}
	if code, _ := ta.api("PATCH", "/api/proxy/clients/ci", editorToken, map[string]any{"disabled": true}); code != 200 {
		t.Fatal("disable")
	}
	if _, ok := authenticateClient(ta.store.Proxy(), c2.Token); ok {
		t.Fatal("disabled client authenticated")
	}
	code, body = ta.api("PUT", "/api/proxy", editorToken, map[string]any{"allowed_hosts": []string{"*"}, "allowed_ports": []int{443}})
	if code != 400 {
		t.Fatalf("wildcard host must be rejected: %d %s", code, body)
	}
	if code, _ := ta.api("DELETE", "/api/proxy/clients/ci", editorToken, nil); code != 204 {
		t.Fatal("delete client")
	}
}

func TestAdminJSONIsNotEditableOverAPI(t *testing.T) {
	ta := newTestApp(t, nil)
	for _, p := range []string{"/api/admin", "/api/admin/tokens", "/api/tokens"} {
		if code, _ := ta.api("POST", p, editorToken, "{}"); code == 200 || code == 201 {
			t.Fatalf("%s should not exist", p)
		}
	}
}

func TestStatusReportsConfigErrors(t *testing.T) {
	ta := newTestApp(t, nil)
	os.WriteFile(filepath.Join(ta.dir, "webhooks", "bad.json"), []byte(`{"target": 1}`), 0o640)
	ta.store.Reload(true)
	code, body := ta.api("GET", "/api/status", viewerToken, nil)
	if code != 200 || !strings.Contains(string(body), "bad.json") {
		t.Fatalf("%d %s", code, body)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	ta := newTestApp(t, nil)
	tgt := newRecorder(200)
	defer tgt.srv.Close()
	ta.writeJSON("webhooks/m.json", Webhook{Target: tgt.srv.URL})
	post(t, ta.inbound.URL+"/hooks/m", "x", nil)
	_, body := ta.api("GET", "/metrics", viewerToken, nil)
	for _, want := range []string{
		`# TYPE fluid_inbound_requests_total counter`,
		`fluid_inbound_requests_total{source="webhook",route="m",result="ok"} 1`,
		`fluid_deliveries_total{mode="sync",result="ok"} 1`,
		`# TYPE fluid_outbox_pending gauge`,
		`fluid_config_reloads_total{result="ok"}`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}

func TestHealthEndpoints(t *testing.T) {
	ta := newTestApp(t, nil)
	for _, p := range []string{"/healthz", "/livez", "/readyz"} {
		resp, err := http.Get(ta.inbound.URL + p)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: %v %v", p, resp, err)
		}
	}
	if err := cmdHealthcheck([]string{"-url", ta.inbound.URL + "/livez"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdHealthcheck([]string{"-url", ta.inbound.URL + "/nope"}); err == nil {
		t.Fatal("healthcheck should fail on 404")
	}
}

func TestLiveStream(t *testing.T) {
	ta := newTestApp(t, nil)
	tgt := newRecorder(200)
	defer tgt.srv.Close()
	ta.writeJSON("webhooks/s.json", Webhook{Target: tgt.srv.URL})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ta.admin.URL+"/api/stream?topics=events,deliveries,logs", nil)
	req.Header.Set("Authorization", "Bearer "+viewerToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal(resp.Header)
	}
	br := bufio.NewReader(resp.Body)
	br.ReadString('\n') // ": connected"
	go http.Post(ta.inbound.URL+"/hooks/s", "text/plain", strings.NewReader("BODY-MARKER-42"))
	seen := map[string]bool{}
	for !(seen["events"] && seen["deliveries"] && seen["logs"]) {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended, saw %v: %v", seen, err)
		}
		if strings.HasPrefix(line, "event: ") {
			seen[strings.TrimSpace(strings.TrimPrefix(line, "event: "))] = true
		}
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, "BODY-MARKER-42") {
			t.Fatal("event bodies must not be streamed")
		}
	}
}

func TestUIServedWithSecurityHeaders(t *testing.T) {
	ta := newTestApp(t, nil)
	for _, p := range []string{"/", "/events", "/webhooks/abc"} {
		resp, err := http.Get(ta.admin.URL + p)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: %v %v", p, resp, err)
		}
		if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'self'") {
			t.Fatal("CSP missing")
		}
	}
}

func TestEventsAPI(t *testing.T) {
	ta := newTestApp(t, nil)
	tgt := newRecorder(200)
	defer tgt.srv.Close()
	ta.writeJSON("webhooks/e.json", Webhook{Target: tgt.srv.URL})
	r, _ := post(t, ta.inbound.URL+"/hooks/e/abc", `{"k":"v"}`, nil)
	id := r.Header.Get("X-Fluid-Event-Id")
	code, body := ta.api("GET", "/api/events?route=e&status=delivered", viewerToken, nil)
	var list struct{ Items []Event }
	mustJSON(t, body, &list)
	if code != 200 || len(list.Items) != 1 || list.Items[0].ID != id {
		t.Fatalf("%d %s", code, body)
	}
	code, body = ta.api("GET", "/api/events/"+id, viewerToken, nil)
	var ev Event
	mustJSON(t, body, &ev)
	if code != 200 || ev.Body != `{"k":"v"}` || len(ev.Deliveries) != 1 {
		t.Fatalf("%d %s", code, body)
	}
	if code, _ := ta.api("GET", "/api/events/nope", viewerToken, nil); code != 404 {
		t.Fatal("missing event")
	}
	b, _ := json.Marshal(ev)
	if strings.Contains(string(b), `"raw"`) {
		t.Fatal("internal field leaked")
	}
}
