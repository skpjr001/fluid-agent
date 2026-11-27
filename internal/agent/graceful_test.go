package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skpjr001/fluid-agent/internal/config"
	"github.com/skpjr001/fluid-agent/internal/database"
	"github.com/skpjr001/fluid-agent/internal/hooks"
)

// servedApp runs the real Serve (all listeners, background loops, shutdown).
type servedApp struct {
	*App
	cancel context.CancelFunc
	done   chan struct{} // closed when Serve returns
	err    error
	dir    string
}

func serveApp(t *testing.T, mutate func(*config.StaticConfig)) *servedApp {
	t.Helper()
	return serveAppHooks(t, mutate, hooks.Hooks{})
}

func serveAppHooks(t *testing.T, mutate func(*config.StaticConfig), h hooks.Hooks) *servedApp {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.ProxyAddr, cfg.WebhookAddr, cfg.AdminAddr = "127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0"
	cfg.DynamicDir = filepath.Join(dir, "dynamic")
	cfg.Database.Path = filepath.Join(dir, "data", "test.db")
	cfg.Outbox.PollInterval = config.Duration(20 * time.Millisecond)
	cfg.ShutdownTimeout = config.Duration(time.Second)
	if mutate != nil {
		mutate(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(cfg.DynamicDir, 0o750)
	write := func(rel string, v any) {
		b, _ := json.Marshal(v)
		os.MkdirAll(filepath.Dir(filepath.Join(cfg.DynamicDir, rel)), 0o750)
		os.WriteFile(filepath.Join(cfg.DynamicDir, rel), b, 0o640)
	}
	write("admin.json", AdminConfig{Tokens: []AdminToken{{Name: "ed", Token: editorToken, Role: RoleEditor}}})
	write("proxy.json", ProxyConfig{Clients: []Client{{Name: "dev", Token: clientToken}},
		AllowedHosts: []string{"127.0.0.1"}, AllowedPorts: []int{1, 65535}, PrivateAllowlist: []string{"127.0.0.1"}})
	ctx, cancel := context.WithCancel(context.Background())
	app, err := NewApp(ctx, cfg)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	app.hooks = h
	sa := &servedApp{App: app, cancel: cancel, done: make(chan struct{}), dir: cfg.DynamicDir}
	go func() { sa.err = app.Serve(ctx); close(sa.done) }()
	select {
	case <-app.listening:
	case <-sa.done:
		t.Fatalf("serve: %v", sa.err)
	case <-time.After(5 * time.Second):
		t.Fatal("listeners did not start")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-sa.done:
		case <-time.After(10 * time.Second):
			t.Error("Serve did not return")
		}
	})
	return sa
}

func (sa *servedApp) url(listener, path string) string { return "http://" + sa.addrs[listener] + path }

func (sa *servedApp) writeDynamic(t *testing.T, rel string, v any) {
	b, _ := json.Marshal(v)
	os.MkdirAll(filepath.Dir(filepath.Join(sa.dir, rel)), 0o750)
	if err := os.WriteFile(filepath.Join(sa.dir, rel), b, 0o640); err != nil {
		t.Fatal(err)
	}
	sa.store.Reload(true)
}

// stop sends the shutdown signal and returns how long Serve took to return.
func (sa *servedApp) stop(t *testing.T, max time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	sa.cancel()
	select {
	case <-sa.done:
		return time.Since(start)
	case <-time.After(max):
		t.Fatalf("Serve did not return within %s", max)
		return 0
	}
}

func TestGracefulShutdownFinishesInFlightRequests(t *testing.T) {
	var inTarget atomic.Bool
	tgt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inTarget.Store(true)
		time.Sleep(400 * time.Millisecond)
		w.Write([]byte("finished"))
	}))
	defer tgt.Close()
	sa := serveApp(t, func(c *config.StaticConfig) { c.ShutdownTimeout = config.Duration(3 * time.Second) })
	sa.writeDynamic(t, "webhooks/slow.json", Webhook{ID: "slow", Target: tgt.URL})

	got := make(chan string, 1)
	go func() {
		resp, err := http.Post(sa.url("webhook", "/hooks/slow"), "text/plain", strings.NewReader("x"))
		if err != nil {
			got <- "error: " + err.Error()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		got <- fmt.Sprintf("%d %s", resp.StatusCode, b)
	}()
	eventually(t, "request in flight", inTarget.Load)

	// /readyz fails as soon as shutdown starts, while the request is still running.
	readyDuringDrain := make(chan int, 1)
	go func() {
		eventually(t, "draining", sa.draining.Load)
		if resp, err := http.Get(sa.url("webhook", "/readyz")); err == nil {
			readyDuringDrain <- resp.StatusCode
			resp.Body.Close()
		} else {
			readyDuringDrain <- 0 // listener already closed: also not ready
		}
	}()
	took := sa.stop(t, 5*time.Second)
	if r := <-got; r != "200 finished" {
		t.Fatalf("in-flight sync webhook must complete during shutdown, got %q", r)
	}
	if code := <-readyDuringDrain; code != 503 && code != 0 {
		t.Fatalf("/readyz while draining: %d", code)
	}
	if took > 2*time.Second {
		t.Fatalf("shutdown should end when the work does, took %s", took)
	}
}

func TestGracefulShutdownRequeuesUnfinishedDeliveries(t *testing.T) {
	release := make(chan struct{})
	var started atomic.Bool
	tgt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started.Store(true)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer tgt.Close()
	defer close(release)
	sa := serveApp(t, nil) // shutdown_timeout 1s
	sa.writeDynamic(t, "webhooks/a.json", Webhook{ID: "a", Target: tgt.URL, Async: true,
		RouteOptions: RouteOptions{TimeoutSec: 60}})
	resp, err := http.Post(sa.url("webhook", "/hooks/a"), "text/plain", strings.NewReader("x"))
	if err != nil || resp.StatusCode != 202 {
		t.Fatalf("%v %v", resp, err)
	}
	eventually(t, "delivery in flight", started.Load)
	db := sa.db
	took := sa.stop(t, 5*time.Second)
	if took < 900*time.Millisecond || took > 3*time.Second {
		t.Fatalf("shutdown should wait for shutdown_timeout then stop, took %s", took)
	}
	// Serve closed the database; reopen to inspect.
	_ = db
	rdb, err := database.Open(sa.cfg.Database.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	var status string
	var attempts int
	if err := rdb.QueryRow(`SELECT status, attempts FROM deliveries`).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || attempts != 0 {
		t.Fatalf("interrupted delivery must be requeued without counting the attempt: %s attempts=%d", status, attempts)
	}
	var msgs int
	rdb.QueryRow(`SELECT COUNT(*) FROM logs WHERE msg LIKE 'delivery interrupted by shutdown%' OR msg = 'shutdown complete'`).Scan(&msgs)
	if msgs != 2 {
		t.Fatalf("shutdown should be logged (interruption + completion), got %d rows", msgs)
	}
}

func TestGracefulShutdownClosesTunnelsAtDeadline(t *testing.T) {
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	go func() {
		for {
			c, err := up.Accept()
			if err != nil {
				return
			}
			go io.Copy(io.Discard, c) // keeps the tunnel open
		}
	}()
	sa := serveApp(t, nil)
	sa.writeDynamic(t, "proxy.json", ProxyConfig{Clients: []Client{{Name: "dev", Token: clientToken}},
		AllowedHosts: []string{"127.0.0.1"}, AllowedPorts: []int{up.Addr().(*net.TCPAddr).Port}, PrivateAllowlist: []string{"127.0.0.1"}})
	conn, err := net.Dial("tcp", sa.addrs["proxy"])
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Bearer %s\r\n\r\n", up.Addr(), up.Addr(), clientToken)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("connect: %v %v", resp, err)
	}
	eventually(t, "tunnel open", func() bool { return sa.proxy.tunnels.Load() == 1 })

	took := sa.stop(t, 5*time.Second)
	if took < 900*time.Millisecond || took > 4*time.Second {
		t.Fatalf("an open tunnel gets shutdown_timeout, then is closed: took %s", took)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("tunnel should be closed")
	}
}

func TestShutdownWithNothingInFlightIsImmediate(t *testing.T) {
	sa := serveApp(t, func(c *config.StaticConfig) { c.ShutdownTimeout = config.Duration(10 * time.Second) })
	if took := sa.stop(t, 5*time.Second); took > time.Second {
		t.Fatalf("idle shutdown took %s", took)
	}
}
