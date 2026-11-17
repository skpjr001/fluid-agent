package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Delivery is metadata only (no bodies) so nothing sensitive is retained.
type Delivery struct {
	At         time.Time `json:"at"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	SourceIP   string    `json:"source_ip"`
	ReqBytes   int       `json:"req_bytes"`
	Status     int       `json:"status"`
	Attempts   int       `json:"attempts"`
	DurationMs int64     `json:"duration_ms"`
	Error      string    `json:"error,omitempty"`
}

type deliveryLog struct {
	mu   sync.Mutex
	byID map[string][]Delivery
	max  int
}

func (l *deliveryLog) add(id string, d Delivery) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := append(l.byID[id], d)
	if len(s) > l.max {
		s = s[len(s)-l.max:]
	}
	l.byID[id] = s
}

func (l *deliveryLog) get(id string) []Delivery {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Delivery, len(l.byID[id]))
	copy(out, l.byID[id])
	// newest first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

type WebhookRelay struct {
	store        *Store
	log          *deliveryLog
	client       *http.Client
	trustXFF     bool
	maxBodyBytes int64
}

func NewWebhookRelay(s *Store, trustXFF bool, maxBody int64) *WebhookRelay {
	return &WebhookRelay{
		store:        s,
		log:          &deliveryLog{byID: map[string][]Delivery{}, max: 50},
		trustXFF:     trustXFF,
		maxBodyBytes: maxBody,
		client: &http.Client{
			Transport: &http.Transport{Proxy: nil, MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (wr *WebhookRelay) clientIP(r *http.Request) net.IP {
	if wr.trustXFF {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if ip := net.ParseIP(strings.TrimSpace(first)); ip != nil {
				return ip
			}
		}
	}
	h, _, _ := net.SplitHostPort(r.RemoteAddr)
	return net.ParseIP(h)
}

// Handles /hooks/{id} and /hooks/{id}/anything -> target + /anything
func (wr *WebhookRelay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/hooks/")
	if rest == r.URL.Path {
		http.NotFound(w, r)
		return
	}
	id, sub, _ := strings.Cut(rest, "/")
	hook := wr.store.Webhook(id)
	if hook == nil || !hook.Enabled {
		http.NotFound(w, r)
		return
	}
	if !hook.allowsMethod(r.Method) {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ip := wr.clientIP(r)
	if !hook.allowsIP(ip) {
		slog.Warn("webhook source rejected", "id", id, "ip", ip)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, wr.maxBodyBytes))
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}

	target, _ := url.Parse(hook.Target)
	if sub != "" {
		target.Path = strings.TrimSuffix(target.Path, "/") + "/" + sub
	}
	if r.URL.RawQuery != "" {
		if target.RawQuery != "" {
			target.RawQuery += "&" + r.URL.RawQuery
		} else {
			target.RawQuery = r.URL.RawQuery
		}
	}
	hdr := r.Header.Clone()
	stripHop(hdr)
	hdr.Del("Host")
	if ip != nil {
		hdr.Set("X-Forwarded-For", ip.String())
	}
	hdr.Set("X-Forwarded-Host", r.Host)
	hdr.Set("X-Webhook-Id", id)
	for k, v := range hook.AddHeaders {
		hdr.Set(k, v)
	}

	d := Delivery{At: time.Now(), Method: r.Method, Path: r.URL.Path, ReqBytes: len(body)}
	if ip != nil {
		d.SourceIP = ip.String()
	}

	if hook.Async {
		w.WriteHeader(http.StatusAccepted)
		go func() {
			resp, d := wr.deliver(context.Background(), hook, r.Method, target.String(), hdr, body, d)
			if resp != nil {
				resp.Body.Close()
			}
			wr.log.add(id, d)
		}()
		return
	}

	resp, d := wr.deliver(r.Context(), hook, r.Method, target.String(), hdr, body, d)
	wr.log.add(id, d)
	if resp == nil {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	stripHop(resp.Header)
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// deliver tries once, plus MaxRetries more on network errors / 5xx, with backoff.
func (wr *WebhookRelay) deliver(ctx context.Context, hook *Webhook, method, target string,
	hdr http.Header, body []byte, d Delivery) (*http.Response, Delivery) {

	start := time.Now()
	backoff := 500 * time.Millisecond
	var lastErr string
	for attempt := 0; attempt <= hook.MaxRetries; attempt++ {
		d.Attempts = attempt + 1
		if attempt > 0 {
			select {
			case <-ctx.Done():
				lastErr = ctx.Err().Error()
				attempt = hook.MaxRetries + 1
				continue
			case <-time.After(backoff):
			}
			backoff *= 2
		}
		actx, cancel := context.WithTimeout(ctx, time.Duration(hook.TimeoutSec)*time.Second)
		req, err := http.NewRequestWithContext(actx, method, target, bytes.NewReader(body))
		if err != nil {
			cancel()
			lastErr = err.Error()
			break
		}
		req.Header = hdr.Clone()
		resp, err := wr.client.Do(req)
		if err != nil {
			cancel()
			lastErr = err.Error()
			continue
		}
		if resp.StatusCode >= 500 && attempt < hook.MaxRetries {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			cancel()
			lastErr = resp.Status
			continue
		}
		// Buffer the response so the timeout context can be released safely.
		rb, err := io.ReadAll(io.LimitReader(resp.Body, wr.maxBodyBytes))
		resp.Body.Close()
		cancel()
		if err != nil {
			lastErr = err.Error()
			continue
		}
		resp.Body = io.NopCloser(bytes.NewReader(rb))
		d.Status = resp.StatusCode
		d.DurationMs = time.Since(start).Milliseconds()
		slog.Info("webhook relayed", "id", hook.ID, "status", resp.StatusCode, "attempts", d.Attempts, "ms", d.DurationMs)
		return resp, d
	}
	d.Error = lastErr
	d.DurationMs = time.Since(start).Milliseconds()
	slog.Error("webhook delivery failed", "id", hook.ID, "attempts", d.Attempts, "err", lastErr)
	return nil, d
}
