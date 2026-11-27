package agent

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // CSV times in the viewer's zone, even in a distroless image
)

// CSV export: GET /api/events|deliveries|logs?format=csv takes the same search,
// column filters (f.*) and sort as the list, ignores paging, and streams every
// matching row (up to exportMax) with times in the viewer's tz.

const exportMax = 100_000

type csvOut struct {
	w   *csv.Writer
	loc *time.Location
}

// text guards against spreadsheet formula injection: a cell starting with
// = + - @ or a control character is prefixed with a quote.
func (c *csvOut) text(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func (c *csvOut) time(t time.Time) string {
	if t.IsZero() || t.Year() < 2000 {
		return ""
	}
	return t.In(c.loc).Format(time.RFC3339)
}

func num[T ~int | ~int64](n T) string { return strconv.FormatInt(int64(n), 10) }

func numOrEmpty[T ~int | ~int64](n T) string {
	if n == 0 {
		return ""
	}
	return num(n)
}

// startCSV writes the headers; the file name carries the list and time.
func startCSV(w http.ResponseWriter, r *http.Request, name string, header []string) (*csvOut, error) {
	loc := time.UTC
	if tz := r.URL.Query().Get("tz"); tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil {
			return nil, fmt.Errorf("tz: unknown time zone %q", tz)
		}
		loc = l
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="fluid-%s-%s.csv"`, name, time.Now().In(loc).Format("20060102-150405")))
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte{0xEF, 0xBB, 0xBF}) // BOM: spreadsheets then read UTF-8 correctly
	c := &csvOut{w: csv.NewWriter(w), loc: loc}
	c.w.Write(header)
	return c, nil
}

func (c *csvOut) done(w http.ResponseWriter, rows int) {
	c.w.Flush()
	if rows >= exportMax {
		c.w.Write([]string{fmt.Sprintf("# truncated at %d rows: narrow the filters", exportMax)})
		c.w.Flush()
	}
}

func (a *Admin) exportEvents(w http.ResponseWriter, r *http.Request, q EventQuery) {
	c, err := startCSV(w, r, "events", []string{"id", "received", "source", "route", "vendor", "event_type", "method", "path", "query", "source_ip", "body_size", "status", "traceparent"})
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	base, n := q.List.Offset, 0
	q.Limit = maxPageSize
	for n < exportMax {
		q.List.Offset = base + n
		evs, _, err := a.app.outbox.ListEvents(r.Context(), q)
		if err != nil {
			break
		}
		for _, e := range evs {
			c.w.Write([]string{e.ID, c.time(e.TS), e.Source, c.text(e.Route), c.text(e.Vendor), c.text(e.EventType), e.Method,
				c.text(e.Path), c.text(e.Query), e.SourceIP, num(e.BodySize), e.Status, e.Traceparent})
		}
		n += len(evs)
		if len(evs) < q.Limit {
			break
		}
	}
	c.done(w, n)
}

func (a *Admin) exportDeliveries(w http.ResponseWriter, r *http.Request, q DeliveryQuery) {
	c, err := startCSV(w, r, "deliveries", []string{"id", "event_id", "route", "target", "mode", "status", "attempts", "max_attempts", "last_status", "last_error", "duration_ms", "next_attempt", "created", "updated"})
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	base, n := q.List.Offset, 0
	q.Limit = maxPageSize
	for n < exportMax {
		q.List.Offset = base + n
		ds, _, err := a.app.outbox.ListDeliveries(r.Context(), q)
		if err != nil {
			break
		}
		for _, d := range ds {
			next := ""
			if d.Status == "pending" {
				next = c.time(d.NextAt)
			}
			c.w.Write([]string{num(d.ID), d.EventID, c.text(d.Route), c.text(d.Target), d.Mode, d.Status, num(d.Attempts), num(d.MaxAttempts),
				numOrEmpty(d.LastStatus), c.text(d.LastError), numOrEmpty(d.DurationMs), next, c.time(d.CreatedAt), c.time(d.UpdatedAt)})
		}
		n += len(ds)
		if len(ds) < q.Limit {
			break
		}
	}
	c.done(w, n)
}

func (a *Admin) exportLogs(w http.ResponseWriter, r *http.Request, q LogQuery) {
	c, err := startCSV(w, r, "logs", []string{"id", "time", "level", "kind", "direction", "message", "client", "method", "host", "url", "status", "bytes_in", "bytes_out", "duration_ms", "source_ip", "route", "trace_id", "attributes"})
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	base, n := q.List.Offset, 0
	q.Limit = 1000
	for n < exportMax {
		q.List.Offset = base + n
		rows, _, err := QueryLogs(r.Context(), a.app.db, q)
		if err != nil {
			break
		}
		for _, l := range rows {
			attrs := ""
			if len(l.Attrs) > 0 {
				b, _ := json.Marshal(l.Attrs)
				attrs = string(b)
			}
			c.w.Write([]string{num(l.ID), c.time(l.TS), l.Level, l.Kind, l.Direction, c.text(l.Msg), c.text(l.Client), c.text(l.Method),
				c.text(l.Host), c.text(l.URL), numOrEmpty(l.Status), numOrEmpty(l.BytesIn), numOrEmpty(l.BytesOut), numOrEmpty(l.DurationMs),
				l.SourceIP, c.text(l.Route), l.TraceID, c.text(attrs)})
		}
		n += len(rows)
		if len(rows) < q.Limit {
			break
		}
	}
	c.done(w, n)
}
