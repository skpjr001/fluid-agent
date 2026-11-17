package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

type Admin struct {
	store *Store
	relay *WebhookRelay
	token string
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func errJSON(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (a *Admin) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "webhooks": len(a.store.Webhooks())})
	})
	mux.HandleFunc("GET /api/webhooks", a.list)
	mux.HandleFunc("POST /api/webhooks", a.create)
	mux.HandleFunc("GET /api/webhooks/{id}", a.get)
	mux.HandleFunc("PUT /api/webhooks/{id}", a.put)
	mux.HandleFunc("PATCH /api/webhooks/{id}", a.patch)
	mux.HandleFunc("DELETE /api/webhooks/{id}", a.del)
	mux.HandleFunc("GET /api/webhooks/{id}/deliveries", a.deliveries)
	mux.HandleFunc("GET /api/proxy", a.proxyRules)
	mux.HandleFunc("POST /api/reload", func(w http.ResponseWriter, r *http.Request) {
		if err := a.store.Reload(true); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]bool{"reloaded": true})
	})
	return a.auth(mux)
}

func (a *Admin) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) != 1 {
			errJSON(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		next.ServeHTTP(w, r)
	})
}

func (a *Admin) list(w http.ResponseWriter, r *http.Request) {
	all := a.store.Webhooks()
	out := make([]*Webhook, 0, len(all))
	for _, h := range all {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, 200, out)
}

func (a *Admin) get(w http.ResponseWriter, r *http.Request) {
	h := a.store.Webhook(r.PathValue("id"))
	if h == nil {
		errJSON(w, 404, "not found")
		return
	}
	writeJSON(w, 200, h)
}

func decodeStrict(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (a *Admin) create(w http.ResponseWriter, r *http.Request) {
	var h Webhook
	h.Enabled = true // default when omitted
	if err := decodeStrict(r, &h); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	if a.store.Webhook(h.ID) != nil {
		errJSON(w, 409, "webhook already exists")
		return
	}
	now := time.Now().UTC()
	h.CreatedAt, h.UpdatedAt = now, now
	if err := a.store.SaveWebhook(&h); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	writeJSON(w, 201, a.store.Webhook(h.ID))
}

// PUT replaces the whole webhook (creates if missing).
func (a *Admin) put(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var h Webhook
	h.Enabled = true
	if err := decodeStrict(r, &h); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	h.ID = id
	now := time.Now().UTC()
	h.CreatedAt, h.UpdatedAt = now, now
	if old := a.store.Webhook(id); old != nil {
		h.CreatedAt = old.CreatedAt
	}
	if err := a.store.SaveWebhook(&h); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, a.store.Webhook(id))
}

// PATCH merges only the fields you send, e.g. {"enabled": false}.
func (a *Admin) patch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	old := a.store.Webhook(id)
	if old == nil {
		errJSON(w, 404, "not found")
		return
	}
	h := *old
	h.AddHeaders = cloneMap(old.AddHeaders)
	h.Methods = append([]string(nil), old.Methods...)
	h.SourceCIDRs = append([]string(nil), old.SourceCIDRs...)
	if err := decodeStrict(r, &h); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	h.ID, h.CreatedAt, h.UpdatedAt = id, old.CreatedAt, time.Now().UTC()
	if err := a.store.SaveWebhook(&h); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, a.store.Webhook(id))
}

func (a *Admin) del(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteWebhook(r.PathValue("id")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			errJSON(w, 404, "not found")
			return
		}
		errJSON(w, 500, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Admin) deliveries(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.relay.log.get(r.PathValue("id")))
}

func (a *Admin) proxyRules(w http.ResponseWriter, r *http.Request) {
	rules := a.store.Rules()
	masked := make([]string, len(rules.Tokens))
	for i, t := range rules.Tokens {
		if len(t) > 4 {
			masked[i] = t[:4] + "…"
		} else {
			masked[i] = "…"
		}
	}
	writeJSON(w, 200, map[string]any{
		"allowed_hosts": rules.AllowedHosts,
		"allowed_ports": rules.AllowedPorts,
		"tokens":        masked,
	})
}

func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
