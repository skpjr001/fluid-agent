package main

import (
	"encoding/csv"
	"net/url"
	"strings"
	"testing"
)

func TestListSpecParse(t *testing.T) {
	ok := map[string]string{
		"f.status=5xx":       "status >= ? AND status < ?",
		"f.status=>=400":     "status >= ?",
		"f.status=200-299":   "status BETWEEN ? AND ?",
		"f.duration_ms=<100": "duration_ms < ?",
		"f.route=pay":        "LOWER(COALESCE(route,'')) LIKE ?",
		"f.route==pay":       "route = ?",
		"f.ts=1h..":          "ts >= ?",
		"f.ts=2026-11-17T00:00:00Z..2026-11-18T00:00:00Z": "ts >= ? AND ts < ?",
	}
	for qs, want := range ok {
		q, _ := url.ParseQuery(qs)
		p, err := logListSpec.parse(q)
		if err != nil || len(p.Conds) != 1 || !strings.Contains(p.Conds[0], want) {
			t.Errorf("%s: %v %v", qs, p.Conds, err)
		}
	}
	for _, bad := range []string{"f.nope=1", "f.status=abc", "f.ts=yesterday", "sort=nope:asc", "sort=ts:sideways", "offset=-1"} {
		q, _ := url.ParseQuery(bad)
		if _, err := logListSpec.parse(q); err == nil {
			t.Errorf("%s should fail", bad)
		}
	}
	q, _ := url.ParseQuery("sort=status:desc,route")
	p, _ := logListSpec.parse(q)
	if p.Order != "status DESC, route ASC, id DESC" || strings.Join(p.Sorts, ",") != "status:desc,route:asc" {
		t.Errorf("order %q sorts %v", p.Order, p.Sorts)
	}
	// LIKE wildcards in user input are literal.
	q, _ = url.ParseQuery("f.msg=50%25_off")
	p, _ = logListSpec.parse(q)
	if p.Args[0] != `%50\%\_off%` {
		t.Errorf("escaping: %v", p.Args)
	}
}

func TestListPagingSortAndCSV(t *testing.T) {
	ta := newTestApp(t, nil)
	tgt := newRecorder()
	defer tgt.srv.Close()
	ta.writeJSON("webhooks/a.json", Webhook{ID: "a", Target: tgt.srv.URL, Async: true})
	ta.writeJSON("webhooks/b.json", Webhook{ID: "b", Target: tgt.srv.URL, Async: true})
	for i := 0; i < 7; i++ {
		post(t, ta.inbound.URL+"/hooks/a", "x", nil)
	}
	for i := 0; i < 3; i++ {
		post(t, ta.inbound.URL+"/hooks/b", "=cmd|' /C calc'!A0", nil)
	}
	var page struct {
		Items  []Event  `json:"items"`
		Total  int64    `json:"total"`
		Offset int      `json:"offset"`
		Sort   []string `json:"sort"`
	}
	code, body := ta.api("GET", "/api/events?limit=4&offset=4&sort=route:desc,ts:asc", viewerToken, nil)
	mustJSON(t, body, &page)
	if code != 200 || page.Total != 10 || len(page.Items) != 4 || page.Offset != 4 || page.Items[0].Route != "a" {
		t.Fatalf("%d total=%d items=%d first=%v sort=%v", code, page.Total, len(page.Items), page.Items, page.Sort)
	}
	code, body = ta.api("GET", "/api/events?f.route==b&limit=2", viewerToken, nil)
	mustJSON(t, body, &page)
	if code != 200 || page.Total != 3 || len(page.Items) != 2 {
		t.Fatalf("filtered: %d %d %d", code, page.Total, len(page.Items))
	}

	// CSV: same filters, every page, times in the requested zone, formulas neutralised.
	code, body = ta.api("GET", "/api/events?format=csv&f.route==b&tz=Asia/Kolkata&limit=1", viewerToken, nil)
	if code != 200 {
		t.Fatalf("csv: %d %s", code, body)
	}
	recs, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(body), "\xef\xbb\xbf"))).ReadAll()
	if err != nil || len(recs) != 4 || recs[0][0] != "id" || recs[1][3] != "b" || !strings.HasSuffix(recs[1][1], "+05:30") {
		t.Fatalf("csv rows: %v %v", recs, err)
	}
	if code, _ := ta.api("GET", "/api/events?format=csv&tz=Mars/Base", viewerToken, nil); code != 400 {
		t.Fatalf("bad tz: %d", code)
	}
	eventually(t, "delivery logs", func() bool {
		rows, _, _ := QueryLogs(t.Context(), ta.db, LogQuery{Kinds: []string{"delivery"}})
		return len(rows) >= 10
	})
	code, body = ta.api("GET", "/api/logs?format=csv&f.kind==webhook&f.route==b", editorToken, nil)
	recs, _ = csv.NewReader(strings.NewReader(strings.TrimPrefix(string(body), "\xef\xbb\xbf"))).ReadAll()
	if code != 200 || len(recs) != 4 || recs[0][5] != "message" {
		t.Fatalf("logs csv: %d %v", code, recs)
	}
	code, body = ta.api("GET", "/api/deliveries?format=csv&f.route==a&sort=id:asc", editorToken, nil)
	recs, _ = csv.NewReader(strings.NewReader(strings.TrimPrefix(string(body), "\xef\xbb\xbf"))).ReadAll()
	if code != 200 || len(recs) != 8 || recs[1][2] != "a" {
		t.Fatalf("deliveries csv: %d %v", code, recs)
	}
	c := &csvOut{}
	if c.text("=SUM(A1)") != "'=SUM(A1)" || c.text("+1") != "'+1" || c.text("@x") != "'@x" || c.text("plain") != "plain" {
		t.Error("formula injection guard")
	}
}
