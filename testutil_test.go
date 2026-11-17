package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	editorToken = "editor-token-0123456789abcdef"
	viewerToken = "viewer-token-0123456789abcdef"
	clientToken = "client-token-0123456789abcdef"
)

type testApp struct {
	*App
	t       *testing.T
	dir     string
	proxy   *httptest.Server
	inbound *httptest.Server
	admin   *httptest.Server
	cancel  context.CancelFunc
}

// newTestApp starts a full agent (all three handlers on httptest servers,
// outbox running) with a fresh dynamic dir and database.
func newTestApp(t *testing.T, mutate func(*StaticConfig)) *testApp {
	t.Helper()
	dir := t.TempDir()
	cfg := defaultStatic()
	cfg.DynamicDir = filepath.Join(dir, "dynamic")
	cfg.Database.Path = filepath.Join(dir, "data", "test.db")
	cfg.Outbox.PollInterval = Duration(20 * time.Millisecond)
	cfg.Outbox.MaxBackoff = Duration(30 * time.Millisecond)
	if mutate != nil {
		mutate(cfg)
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	ta := &testApp{t: t, dir: cfg.DynamicDir}
	os.MkdirAll(cfg.DynamicDir, 0o750)
	ta.writeJSON("admin.json", AdminConfig{Tokens: []AdminToken{
		{Name: "ed", Token: editorToken, Role: RoleEditor},
		{Name: "vi", Token: viewerToken, Role: RoleViewer},
	}})
	ta.writeJSON("proxy.json", ProxyConfig{Clients: []Client{{Name: "dev", Token: clientToken}},
		AllowedHosts: []string{"127.0.0.1"}, AllowedPorts: []int{443}})

	ctx, cancel := context.WithCancel(context.Background())
	app, err := NewApp(ctx, cfg)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	ta.App, ta.cancel = app, cancel
	done := make(chan struct{})
	go func() { app.outbox.Run(ctx); close(done) }()
	ta.proxy = httptest.NewServer(app.proxyHandler())
	ta.inbound = httptest.NewServer(app.webhookHandler())
	ta.admin = httptest.NewServer(app.adminHandler())
	t.Cleanup(func() {
		ta.proxy.Close()
		ta.inbound.Close()
		ta.admin.Close()
		cancel()
		<-done
		app.sink.Close()
		app.db.Close()
	})
	return ta
}

// writeJSON writes a dynamic file and reloads the store.
func (ta *testApp) writeJSON(rel string, v any) {
	ta.t.Helper()
	p := filepath.Join(ta.dir, rel)
	os.MkdirAll(filepath.Dir(p), 0o750)
	b, _ := json.MarshalIndent(v, "", "  ")
	if err := os.WriteFile(p, b, 0o640); err != nil {
		ta.t.Fatal(err)
	}
	if ta.App != nil {
		ta.store.Reload(true)
		if e := ta.store.Errors()[p]; e != "" {
			ta.t.Fatalf("%s invalid: %s", rel, e)
		}
	}
}

// allowUpstream lets the proxy reach a local httptest server.
func (ta *testApp) allowUpstream(u string, mutate ...func(*ProxyConfig)) {
	pu, _ := url.Parse(u)
	port := defaultPort(pu)
	pc := ProxyConfig{Clients: []Client{{Name: "dev", Token: clientToken}, {Name: "other", Token: "other-token-0123456789abcdef"}},
		AllowedHosts: []string{"127.0.0.1", "localhost"}, AllowedPorts: []int{port}, PrivateAllowlist: []string{"127.0.0.1"}}
	for _, m := range mutate {
		m(&pc)
	}
	ta.writeJSON("proxy.json", pc)
}

func (ta *testApp) api(method, path, token string, body any) (int, []byte) {
	ta.t.Helper()
	var rd io.Reader
	if body != nil {
		if s, ok := body.(string); ok {
			rd = strings.NewReader(s)
		} else {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
	}
	req, _ := http.NewRequest(method, ta.admin.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		ta.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// flushLogs waits until queued log rows are written.
func (ta *testApp) flushLogs() {
	time.Sleep(400 * time.Millisecond)
}

func eventually(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func mustJSON(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("bad JSON %q: %v", b, err)
	}
}

func intp(n int) *int { return &n }
