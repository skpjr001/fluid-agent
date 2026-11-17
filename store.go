package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------- dynamic config types ----------

// ProxyRules lives in <dynamic_dir>/proxy.json (edit by hand, hot-reloaded).
type ProxyRules struct {
	Tokens       []string `json:"tokens"`        // clients use http://any:<token>@host:8080
	AllowedHosts []string `json:"allowed_hosts"` // "api.example.com", "*.example.com"
	AllowedPorts []int    `json:"allowed_ports"` // default [443]
}

// Webhook lives in <dynamic_dir>/webhooks/<id>.json (managed via API or by hand).
type Webhook struct {
	ID          string            `json:"id"`
	Target      string            `json:"target"`            // where to relay, e.g. https://dev.internal/callback
	Methods     []string          `json:"methods,omitempty"` // default POST
	Enabled     bool              `json:"enabled"`
	Async       bool              `json:"async,omitempty"` // reply 202 at once, deliver in background
	MaxRetries  int               `json:"max_retries,omitempty"`
	TimeoutSec  int               `json:"timeout_sec,omitempty"`
	SourceCIDRs []string          `json:"source_cidrs,omitempty"` // only accept from these sender IPs
	AddHeaders  map[string]string `json:"add_headers,omitempty"`
	Description string            `json:"description,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`

	nets []*net.IPNet
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func (w *Webhook) normalize() error {
	if !idRe.MatchString(w.ID) {
		return fmt.Errorf("id must match %s", idRe)
	}
	u, err := url.Parse(w.Target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("target must be an absolute http(s) URL")
	}
	if len(w.Methods) == 0 {
		w.Methods = []string{"POST"}
	}
	for i, m := range w.Methods {
		w.Methods[i] = strings.ToUpper(m)
	}
	if w.TimeoutSec <= 0 {
		w.TimeoutSec = 15
	}
	if w.MaxRetries < 0 || w.MaxRetries > 10 {
		return errors.New("max_retries must be 0..10")
	}
	w.nets = nil
	for _, c := range w.SourceCIDRs {
		if !strings.Contains(c, "/") {
			if strings.Contains(c, ":") {
				c += "/128"
			} else {
				c += "/32"
			}
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return fmt.Errorf("bad source_cidrs entry %q", c)
		}
		w.nets = append(w.nets, n)
	}
	return nil
}

func (w *Webhook) allowsMethod(m string) bool {
	for _, x := range w.Methods {
		if x == m || x == "*" {
			return true
		}
	}
	return false
}

func (w *Webhook) allowsIP(ip net.IP) bool {
	if len(w.nets) == 0 {
		return true
	}
	for _, n := range w.nets {
		if ip != nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

// ---------- snapshot + store ----------

type snapshot struct {
	rules    ProxyRules
	webhooks map[string]*Webhook
}

type Store struct {
	dir      string
	snap     atomic.Pointer[snapshot]
	mu       sync.Mutex // serializes reloads and writes
	lastHash string
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "webhooks"), 0o750); err != nil {
		return nil, err
	}
	s := &Store{dir: dir}
	s.snap.Store(&snapshot{webhooks: map[string]*Webhook{}})
	if err := s.Reload(true); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Rules() ProxyRules             { return s.snap.Load().rules }
func (s *Store) Webhook(id string) *Webhook    { return s.snap.Load().webhooks[id] }
func (s *Store) Webhooks() map[string]*Webhook { return s.snap.Load().webhooks }

// fingerprint = names + sizes + mtimes of every JSON file; cheap change detection.
func (s *Store) fingerprint() (string, error) {
	h := sha256.New()
	var files []string
	for _, pat := range []string{"*.json", "webhooks/*.json"} {
		m, err := filepath.Glob(filepath.Join(s.dir, pat))
		if err != nil {
			return "", err
		}
		files = append(files, m...)
	}
	sort.Strings(files)
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			continue
		}
		fmt.Fprintf(h, "%s|%d|%d\n", f, st.Size(), st.ModTime().UnixNano())
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Reload re-reads all files if anything changed. Bad files are logged and
// skipped; the previous good version of that webhook is kept.
func (s *Store) Reload(force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fp, err := s.fingerprint()
	if err != nil {
		return err
	}
	if !force && fp == s.lastHash {
		return nil
	}
	old := s.snap.Load()
	next := &snapshot{rules: old.rules, webhooks: map[string]*Webhook{}}

	if b, err := os.ReadFile(filepath.Join(s.dir, "proxy.json")); err == nil {
		var r ProxyRules
		if err := json.Unmarshal(b, &r); err != nil {
			slog.Error("proxy.json invalid, keeping previous rules", "err", err)
		} else {
			if len(r.AllowedPorts) == 0 {
				r.AllowedPorts = []int{443}
			}
			next.rules = r
		}
	} else if os.IsNotExist(err) {
		next.rules = ProxyRules{AllowedPorts: []int{443}}
	}

	files, _ := filepath.Glob(filepath.Join(s.dir, "webhooks", "*.json"))
	for _, f := range files {
		id := strings.TrimSuffix(filepath.Base(f), ".json")
		b, err := os.ReadFile(f)
		var w Webhook
		if err == nil {
			err = json.Unmarshal(b, &w)
		}
		if err == nil {
			if w.ID == "" {
				w.ID = id
			}
			if w.ID != id {
				err = fmt.Errorf("id %q does not match filename", w.ID)
			} else {
				err = w.normalize()
			}
		}
		if err != nil {
			slog.Error("skipping bad webhook file", "file", f, "err", err)
			if prev, ok := old.webhooks[id]; ok {
				next.webhooks[id] = prev
			}
			continue
		}
		next.webhooks[id] = &w
	}
	s.snap.Store(next)
	s.lastHash = fp
	slog.Info("config loaded", "webhooks", len(next.webhooks), "allowed_hosts", len(next.rules.AllowedHosts), "tokens", len(next.rules.Tokens))
	return nil
}

func (s *Store) Watch(every time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if err := s.Reload(false); err != nil {
				slog.Error("reload failed", "err", err)
			}
		}
	}
}

// SaveWebhook writes atomically (temp file + rename) then reloads.
func (s *Store) SaveWebhook(w *Webhook) error {
	if err := w.normalize(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(s.dir, "webhooks", w.ID+".json"), b); err != nil {
		return err
	}
	return s.Reload(true)
}

func (s *Store) DeleteWebhook(id string) error {
	if !idRe.MatchString(id) {
		return os.ErrNotExist
	}
	if err := os.Remove(filepath.Join(s.dir, "webhooks", id+".json")); err != nil {
		return err
	}
	return s.Reload(true)
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o640); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
