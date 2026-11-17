package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Store is the file provider: everything dynamic lives as JSON files under
// one directory and is hot-reloaded when files change.
//
//	<dir>/proxy.json          proxy clients + destination rules
//	<dir>/admin.json          admin/UI tokens (viewer | editor)
//	<dir>/webhooks/<id>.json  one webhook per file
//	<dir>/profiles/<name>.json one /forward profile per file
//	<dir>/ingest/<id>.json    one callback ingest rule per file
type Store struct {
	dir      string
	snap     atomic.Pointer[snapshot]
	mu       sync.Mutex // serializes reloads and writes
	lastHash string
	onReload func(ok bool)
}

type snapshot struct {
	proxy    *ProxyConfig
	admin    *AdminConfig
	webhooks map[string]*Webhook
	profiles map[string]*Profile
	ingest   []*IngestRule // sorted: priority desc, id asc
	errors   map[string]string
	loadedAt time.Time
}

var dynamicSubdirs = []string{"webhooks", "profiles", "ingest"}

func NewStore(dir string) (*Store, error) {
	for _, sub := range dynamicSubdirs {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			return nil, err
		}
	}
	s := &Store{dir: dir}
	s.snap.Store(&snapshot{
		proxy: &ProxyConfig{AllowedPorts: []int{443}}, admin: &AdminConfig{},
		webhooks: map[string]*Webhook{}, profiles: map[string]*Profile{}, errors: map[string]string{},
	})
	if err := s.Reload(true); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Proxy() *ProxyConfig           { return s.snap.Load().proxy }
func (s *Store) Admin() *AdminConfig           { return s.snap.Load().admin }
func (s *Store) Webhook(id string) *Webhook    { return s.snap.Load().webhooks[id] }
func (s *Store) Webhooks() map[string]*Webhook { return s.snap.Load().webhooks }
func (s *Store) Profile(n string) *Profile     { return s.snap.Load().profiles[n] }
func (s *Store) Profiles() map[string]*Profile { return s.snap.Load().profiles }
func (s *Store) Ingest() []*IngestRule         { return s.snap.Load().ingest }
func (s *Store) Errors() map[string]string     { return s.snap.Load().errors }
func (s *Store) LoadedAt() time.Time           { return s.snap.Load().loadedAt }

// fingerprint = names + sizes + mtimes of every JSON file; cheap change detection.
func (s *Store) fingerprint() string {
	h := sha256.New()
	var files []string
	for _, pat := range []string{"*.json", "webhooks/*.json", "profiles/*.json", "ingest/*.json"} {
		m, _ := filepath.Glob(filepath.Join(s.dir, pat))
		files = append(files, m...)
	}
	sort.Strings(files)
	for _, f := range files {
		if st, err := os.Stat(f); err == nil {
			fmt.Fprintf(h, "%s|%d|%d\n", f, st.Size(), st.ModTime().UnixNano())
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func readStrict(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return decodeStrict(bytes.NewReader(b), v)
}

// Reload re-reads all files if anything changed. A bad file is reported in
// Errors() and the previous good version of it is kept.
func (s *Store) Reload(force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reloadLocked(force)
}

func (s *Store) reloadLocked(force bool) error {
	fp := s.fingerprint()
	if !force && fp == s.lastHash {
		return nil
	}
	old := s.snap.Load()
	next := loadDir(s.dir, old)
	s.snap.Store(next)
	s.lastHash = fp
	for f, e := range next.errors {
		slog.Error("config file invalid, keeping previous version", "kind", "config", "file", f, "err", e)
	}
	slog.Info("config loaded", "kind", "config", "webhooks", len(next.webhooks), "profiles", len(next.profiles),
		"ingest_rules", len(next.ingest), "clients", len(next.proxy.Clients), "errors", len(next.errors))
	if s.onReload != nil {
		s.onReload(len(next.errors) == 0)
	}
	return nil
}

// loadDir builds a snapshot from dir. old supplies fallbacks for bad files
// (nil for a pure validation run, e.g. `fluid-agent check`).
func loadDir(dir string, old *snapshot) *snapshot {
	next := &snapshot{
		webhooks: map[string]*Webhook{}, profiles: map[string]*Profile{},
		errors: map[string]string{}, loadedAt: time.Now(),
	}
	fail := func(path string, err error) { next.errors[path] = err.Error() }

	// proxy.json
	next.proxy = &ProxyConfig{AllowedPorts: []int{443}}
	if old != nil {
		next.proxy = old.proxy
	}
	p := filepath.Join(dir, "proxy.json")
	var pc ProxyConfig
	switch err := readStrict(p, &pc); {
	case os.IsNotExist(err):
		next.proxy = &ProxyConfig{AllowedPorts: []int{443}}
	case err != nil:
		fail(p, err)
	default:
		if err := pc.normalize(); err != nil {
			fail(p, err)
		} else {
			next.proxy = &pc
		}
	}

	// admin.json
	next.admin = &AdminConfig{}
	if old != nil {
		next.admin = old.admin
	}
	p = filepath.Join(dir, "admin.json")
	var ac AdminConfig
	switch err := readStrict(p, &ac); {
	case os.IsNotExist(err):
		next.admin = &AdminConfig{}
	case err != nil:
		fail(p, err)
	default:
		if err := ac.normalize(); err != nil {
			fail(p, err)
		} else {
			next.admin = &ac
		}
	}

	// one-object-per-file collections
	loadEach(dir, "webhooks", fail, func(id, path string) {
		var w Webhook
		err := readStrict(path, &w)
		if err == nil && w.ID == "" {
			w.ID = id
		}
		if err == nil && w.ID != id {
			err = fmt.Errorf("id %q does not match filename", w.ID)
		}
		if err == nil {
			err = w.normalize()
		}
		if err != nil {
			fail(path, err)
			if old != nil && old.webhooks[id] != nil {
				next.webhooks[id] = old.webhooks[id]
			}
			return
		}
		next.webhooks[id] = &w
	})
	loadEach(dir, "profiles", fail, func(id, path string) {
		var pr Profile
		err := readStrict(path, &pr)
		if err == nil && pr.Name == "" {
			pr.Name = id
		}
		if err == nil && pr.Name != id {
			err = fmt.Errorf("name %q does not match filename", pr.Name)
		}
		if err == nil {
			err = pr.normalize()
		}
		if err != nil {
			fail(path, err)
			if old != nil && old.profiles[id] != nil {
				next.profiles[id] = old.profiles[id]
			}
			return
		}
		next.profiles[id] = &pr
	})
	oldRules := map[string]*IngestRule{}
	if old != nil {
		for _, r := range old.ingest {
			oldRules[r.ID] = r
		}
	}
	loadEach(dir, "ingest", fail, func(id, path string) {
		var r IngestRule
		err := readStrict(path, &r)
		if err == nil && r.ID == "" {
			r.ID = id
		}
		if err == nil && r.ID != id {
			err = fmt.Errorf("id %q does not match filename", r.ID)
		}
		if err == nil {
			err = r.normalize()
		}
		if err != nil {
			fail(path, err)
			if prev := oldRules[id]; prev != nil {
				next.ingest = append(next.ingest, prev)
			}
			return
		}
		next.ingest = append(next.ingest, &r)
	})
	sort.Slice(next.ingest, func(i, j int) bool {
		a, b := next.ingest[i], next.ingest[j]
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		return a.ID < b.ID
	})
	return next
}

func loadEach(dir, sub string, fail func(string, error), fn func(id, path string)) {
	files, _ := filepath.Glob(filepath.Join(dir, sub, "*.json"))
	for _, f := range files {
		id := strings.TrimSuffix(filepath.Base(f), ".json")
		if !idRe.MatchString(id) {
			fail(f, fmt.Errorf("filename must match %s", idRe))
			continue
		}
		fn(id, f)
	}
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
				slog.Error("reload failed", "kind", "config", "err", err)
			}
		}
	}
}

// ---------- writes (admin API / UI) ----------

// ErrInvalid wraps validation failures so the API can answer 400.
var ErrInvalid = errors.New("invalid")

func (s *Store) writeFile(rel string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := writeAtomic(filepath.Join(s.dir, rel), b); err != nil {
		return err
	}
	return s.reloadLocked(true)
}

func (s *Store) removeFile(rel string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(filepath.Join(s.dir, rel)); err != nil {
		return err
	}
	return s.reloadLocked(true)
}

func invalid(err error) error { return fmt.Errorf("%w: %w", ErrInvalid, err) }

func (s *Store) SaveWebhook(w *Webhook) error {
	if err := w.normalize(); err != nil {
		return invalid(err)
	}
	w.Schema = SchemaBase + "webhook.schema.json"
	return s.writeFile(filepath.Join("webhooks", w.ID+".json"), w)
}

func (s *Store) SaveProfile(p *Profile) error {
	if err := p.normalize(); err != nil {
		return invalid(err)
	}
	p.Schema = SchemaBase + "profile.schema.json"
	return s.writeFile(filepath.Join("profiles", p.Name+".json"), p)
}

func (s *Store) SaveIngestRule(r *IngestRule) error {
	if err := r.normalize(); err != nil {
		return invalid(err)
	}
	r.Schema = SchemaBase + "ingest.schema.json"
	return s.writeFile(filepath.Join("ingest", r.ID+".json"), r)
}

func (s *Store) SaveProxy(p *ProxyConfig) error {
	if err := p.normalize(); err != nil {
		return invalid(err)
	}
	p.Schema = SchemaBase + "proxy.schema.json"
	return s.writeFile("proxy.json", p)
}

// Delete removes webhooks/<id>.json, profiles/<id>.json or ingest/<id>.json.
func (s *Store) Delete(kind, id string) error {
	if !idRe.MatchString(id) {
		return os.ErrNotExist
	}
	return s.removeFile(filepath.Join(kind, id+".json"))
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
