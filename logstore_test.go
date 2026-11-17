package main

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newLogDB(t *testing.T) (*DB, *LogSink, *slog.Logger) {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "logs.db"))
	if err != nil {
		t.Fatal(err)
	}
	sink := NewLogSink(db, slog.LevelInfo, NewHub())
	var stdout bytes.Buffer
	log := slog.New(NewTeeHandler(slog.NewJSONHandler(&stdout, nil), sink))
	t.Cleanup(func() { db.Close() })
	return db, sink, log
}

func TestLogStoreColumnsAndQueries(t *testing.T) {
	db, sink, log := newLogDB(t)
	log.Debug("not stored", "kind", "app")
	log.Info("proxy http", "kind", "proxy_http", "direction", "outbound", "client", "dev", "method", "GET",
		"host", "api.example.com", "url", "https://api.example.com/x", "status", 200, "bytes_out", 512, "ms", 12)
	log.Warn("webhook rejected", "kind", "webhook", "direction", "inbound", "route", "pay", "source_ip", "1.2.3.4",
		"status", 401, "reason", "bad signature")
	log.Error("delivery failed", "kind", "delivery", "direction", "outbound", "route", "pay", "status", 503,
		"url", "https://core/cb", "err", "target answered 503")
	log.With("kind", "system").Info("started", "version", "v1")
	sink.Close()

	ctx := context.Background()
	all, _, err := QueryLogs(ctx, db, LogQuery{})
	if err != nil || len(all) != 4 {
		t.Fatalf("rows=%d err=%v", len(all), err)
	}
	p := all[3]
	if p.Kind != "proxy_http" || p.Direction != "outbound" || p.Client != "dev" || p.Status != 200 || p.BytesOut != 512 || p.DurationMs != 12 {
		t.Fatalf("columns: %+v", p)
	}
	if all[2].Attrs["reason"] != "bad signature" || all[0].Attrs["version"] != "v1" || all[0].Kind != "system" {
		t.Fatalf("attrs: %+v / %+v", all[2], all[0])
	}

	check := func(name string, q LogQuery, want int) {
		t.Helper()
		rows, _, err := QueryLogs(ctx, db, q)
		if err != nil || len(rows) != want {
			t.Errorf("%s: got %d rows (err %v), want %d", name, len(rows), err, want)
		}
	}
	check("inbound", LogQuery{Direction: "inbound"}, 1)
	check("outbound", LogQuery{Direction: "outbound"}, 2)
	check("kinds", LogQuery{Kinds: []string{"webhook", "delivery"}}, 2)
	check("level>=warn", LogQuery{Level: "warn"}, 2)
	check("status 5xx", LogQuery{Status: "5xx"}, 1)
	check("status error", LogQuery{Status: "error"}, 2)
	check("status 200", LogQuery{Status: "200"}, 1)
	check("route", LogQuery{Route: "pay"}, 2)
	check("client", LogQuery{Client: "dev"}, 1)
	check("host", LogQuery{Host: "API.example.com"}, 1)
	check("source ip", LogQuery{SourceIP: "1.2.3.4"}, 1)
	check("text in attrs", LogQuery{Text: "signature"}, 1)
	check("text in url", LogQuery{Text: "core/cb"}, 1)
	check("text with wildcard chars is literal", LogQuery{Text: "%"}, 0)
	check("since future", LogQuery{Since: time.Now().Add(time.Hour)}, 0)
	check("until past", LogQuery{Until: time.Now().Add(-time.Hour)}, 0)

	for _, bad := range []LogQuery{{Direction: "sideways"}, {Level: "loud"}, {Status: "abc"}} {
		if _, _, err := QueryLogs(ctx, db, bad); err == nil {
			t.Errorf("%+v should be rejected", bad)
		}
	}

	// pagination
	page1, next, _ := QueryLogs(ctx, db, LogQuery{Limit: 3})
	page2, next2, _ := QueryLogs(ctx, db, LogQuery{Limit: 3, BeforeID: next})
	if len(page1) != 3 || len(page2) != 1 || next2 != 0 || page2[0].ID >= page1[2].ID {
		t.Fatalf("pagination: %d %d next=%d next2=%d", len(page1), len(page2), next, next2)
	}

	sum, err := SummarizeLogs(ctx, db, LogQuery{}, "direction")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int64{}
	for _, s := range sum {
		counts[s.Key] = s.Count
	}
	if counts["outbound"] != 2 || counts["inbound"] != 1 || counts[""] != 1 {
		t.Fatalf("summary: %+v", sum)
	}
	sum, _ = SummarizeLogs(ctx, db, LogQuery{}, "route")
	for _, s := range sum {
		if s.Key == "pay" && (s.Count != 2 || s.Errors != 2) {
			t.Fatalf("route summary: %+v", s)
		}
	}
	if _, err := SummarizeLogs(ctx, db, LogQuery{}, "msg; DROP TABLE logs"); err == nil {
		t.Fatal("arbitrary group-by column accepted")
	}
}

func TestPrune(t *testing.T) {
	db, sink, log := newLogDB(t)
	log.Info("old", "kind", "app")
	sink.Close()
	db.Exec(`UPDATE logs SET ts = ?`, time.Now().Add(-48*time.Hour).UnixMilli())
	db.Exec(`INSERT INTO events (id, ts, source, route, method, path, headers, body_size) VALUES ('old', ?, 'webhook', 'r', 'POST', '/', '{}', 0)`,
		time.Now().Add(-48*time.Hour).UnixMilli())
	db.Exec(`INSERT INTO events (id, ts, source, route, method, path, headers, body_size) VALUES ('busy', ?, 'webhook', 'r', 'POST', '/', '{}', 0)`,
		time.Now().Add(-48*time.Hour).UnixMilli())
	db.Exec(`INSERT INTO deliveries (event_id, route, target, mode, status, max_attempts, timeout_sec, next_at, created_at, updated_at)
		VALUES ('busy', 'r', 'http://t', 'async', 'pending', 3, 5, 0, 0, 0)`)
	n, err := Prune(context.Background(), db, 24*time.Hour)
	if err != nil || n != 2 {
		t.Fatalf("pruned %d err %v", n, err)
	}
	var left int
	db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&left)
	if left != 1 {
		t.Fatal("events with pending deliveries must be kept")
	}
}

func TestParseTimeArg(t *testing.T) {
	for _, s := range []string{"15m", "24h", "7d", "2026-01-02T03:04:05Z"} {
		if _, err := parseTimeArg(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	if _, err := parseTimeArg("yesterday"); err == nil {
		t.Error("garbage accepted")
	}
	if tm, _ := parseTimeArg("7d"); time.Since(tm) < 7*24*time.Hour-time.Minute {
		t.Error("7d wrong")
	}
}

func TestLogsCLI(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cli.db")
	db, _ := OpenDB(dbPath)
	sink := NewLogSink(db, slog.LevelInfo, nil)
	log := slog.New(NewTeeHandler(slog.NewJSONHandler(&bytes.Buffer{}, nil), sink))
	log.Info("proxy http", "kind", "proxy_http", "direction", "outbound", "client", "dev", "url", "https://a.example/x", "status", 200)
	log.Warn("webhook rejected", "kind", "webhook", "direction", "inbound", "route", "pay", "status", 401)
	sink.Close()
	db.Close()

	var out bytes.Buffer
	if err := cmdLogs([]string{"-db", dbPath, "-direction", "inbound"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "webhook") || strings.Contains(out.String(), "a.example") {
		t.Fatalf("table:\n%s", out.String())
	}
	out.Reset()
	if err := cmdLogs([]string{"-db", dbPath, "-by", "direction", "-json"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"key":"inbound"`) || !strings.Contains(out.String(), `"key":"outbound"`) {
		t.Fatalf("summary json: %s", out.String())
	}
	if err := cmdLogs([]string{"-db", filepath.Join(dir, "missing.db")}, &out); err == nil {
		t.Fatal("missing db should error")
	}
}
