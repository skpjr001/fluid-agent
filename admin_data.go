package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

func queryInt(r *http.Request, k string, def int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get(k)); err == nil {
		return n
	}
	return def
}

func queryInt64(r *http.Request, k string) int64 {
	n, _ := strconv.ParseInt(r.URL.Query().Get(k), 10, 64)
	return n
}

// ---------- events & deliveries ----------

func (a *Admin) listEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since, err := parseTimeArg(q.Get("since"))
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	evs, next, err := a.app.outbox.ListEvents(r.Context(), EventQuery{Route: q.Get("route"), Source: q.Get("source"),
		Status: q.Get("status"), Text: q.Get("q"), Since: since, BeforeID: q.Get("before_id"), Limit: queryInt(r, "limit", 100)})
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": evs, "next_before_id": next})
}

func (a *Admin) getEvent(w http.ResponseWriter, r *http.Request) {
	ev, err := a.app.outbox.GetEvent(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		errJSON(w, 404, "not found")
		return
	}
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, ev)
}

func (a *Admin) listDeliveries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ds, next, err := a.app.outbox.ListDeliveries(r.Context(), DeliveryQuery{Route: q.Get("route"), Status: q.Get("status"),
		EventID: q.Get("event_id"), BeforeID: queryInt64(r, "before_id"), Limit: queryInt(r, "limit", 100)})
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": ds, "next_before_id": next})
}

func (a *Admin) retryDelivery(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		errJSON(w, 400, "bad id")
		return
	}
	if err := a.app.outbox.Retry(r.Context(), id); err != nil {
		errJSON(w, 409, err.Error())
		return
	}
	writeJSON(w, 202, map[string]any{"queued": id})
}

func (a *Admin) retryDead(w http.ResponseWriter, r *http.Request) {
	n, err := a.app.outbox.RetryDead(r.Context(), r.URL.Query().Get("route"))
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 202, map[string]any{"queued": n})
}

// ---------- logs ----------

func logQueryFrom(r *http.Request) (LogQuery, error) {
	q := r.URL.Query()
	lq := LogQuery{Direction: q.Get("direction"), Level: q.Get("level"), Client: q.Get("client"), Route: q.Get("route"),
		Host: q.Get("host"), SourceIP: q.Get("source_ip"), Status: q.Get("status"), Text: q.Get("q"),
		BeforeID: queryInt64(r, "before_id"), Limit: queryInt(r, "limit", 100)}
	for _, k := range q["kind"] {
		for _, v := range strings.Split(k, ",") {
			if v = strings.TrimSpace(v); v != "" {
				lq.Kinds = append(lq.Kinds, v)
			}
		}
	}
	var err error
	if lq.Since, err = parseTimeArg(q.Get("since")); err != nil {
		return lq, err
	}
	if u := q.Get("until"); u != "" {
		if lq.Until, err = time.Parse(time.RFC3339, u); err != nil {
			return lq, fmt.Errorf("until must be RFC3339")
		}
	}
	return lq, nil
}

func (a *Admin) listLogs(w http.ResponseWriter, r *http.Request) {
	lq, err := logQueryFrom(r)
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	rows, next, err := QueryLogs(r.Context(), a.app.db, lq)
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": rows, "next_before_id": next})
}

func (a *Admin) summaryLogs(w http.ResponseWriter, r *http.Request) {
	lq, err := logQueryFrom(r)
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	by := r.URL.Query().Get("by")
	if by == "" {
		by = "kind"
	}
	rows, err := SummarizeLogs(r.Context(), a.app.db, lq, by)
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"by": by, "items": rows})
}

// ---------- live stream (Server-Sent Events) ----------

// stream sends live logs/events/deliveries. The UI reads it with fetch() so
// the token stays in the Authorization header (never in the URL).
func (a *Admin) stream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		errJSON(w, 500, "streaming unsupported")
		return
	}
	topics := strings.Split(r.URL.Query().Get("topics"), ",")
	if r.URL.Query().Get("topics") == "" {
		topics = []string{"logs", "events", "deliveries"}
	}
	type msg struct {
		topic string
		v     any
	}
	merged := make(chan msg, 256)
	for _, t := range topics {
		if t != "logs" && t != "events" && t != "deliveries" {
			errJSON(w, 400, "topics must be logs, events and/or deliveries")
			return
		}
		ch, cancel := a.app.hub.Subscribe(t)
		defer cancel()
		go func(t string, ch <-chan any) {
			for {
				select {
				case v := <-ch:
					select {
					case merged <- msg{t, v}:
					default:
					}
				case <-r.Context().Done():
					return
				}
			}
		}(t, ch)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case m := <-merged:
			b, _ := json.Marshal(m.v)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", m.topic, b)
			fl.Flush()
		}
	}
}

// ---------- UI ----------

// uiBase returns the external path prefix of the console: admin_base_path,
// or X-Forwarded-Prefix from a trusted reverse proxy that stripped it.
func (a *Admin) uiBase(r *http.Request) string {
	if a.app.cfg.AdminBasePath != "" {
		return a.app.cfg.AdminBasePath
	}
	fp := r.Header.Get("X-Forwarded-Prefix")
	if fp == "" || len(a.app.cfg.trusted) == 0 {
		return ""
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || !prefixesContain(a.app.cfg.trusted, peer) {
		return ""
	}
	if b, err := normalizeBasePath(fp); err == nil {
		return b
	}
	return ""
}

// uiHandler serves the embedded single-page UI with strict security headers.
// index.html gets the external base path so links and API calls work when
// the console is mounted under a path.
func (a *Admin) uiHandler(ui fs.FS) http.Handler {
	files := http.FileServer(http.FS(ui))
	index, _ := fs.ReadFile(ui, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache") // embedded files have no mtime; always revalidate after upgrades
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if fi, err := fs.Stat(ui, name); name != "" && name != "index.html" && err == nil && !fi.IsDir() {
			files.ServeHTTP(w, r)
			return
		}
		// "/" and client-side routes get index.html with the base path filled in
		base := html.EscapeString(a.uiBase(r))
		h.Set("Content-Type", "text/html; charset=utf-8")
		w.Write(bytes.ReplaceAll(index, []byte("{{BASE}}"), []byte(base)))
	})
}
