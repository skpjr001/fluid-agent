package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/skpjr001/fluid-agent/internal/config"
)

// Mask replaces secrets in API responses. Sending it back unchanged keeps
// the stored value, so the UI can round-trip objects without seeing secrets.
const Mask = "********"

type ctxKey int

const principalKey ctxKey = 0

type Principal struct {
	Name string `json:"name"`
	Role Role   `json:"role"`
}

func principalOf(r *http.Request) Principal {
	p, _ := r.Context().Value(principalKey).(Principal)
	return p
}

type Admin struct {
	app *App
}

func (a *Admin) tokens() []AdminToken {
	ts := slices.Clone(a.app.store.Admin().Tokens)
	if env := os.Getenv("PROXY_ADMIN_TOKEN"); len(env) >= 16 {
		ts = append(ts, AdminToken{Name: "env", Token: env, Role: RoleEditor})
	}
	return ts
}

func (a *Admin) authenticate(r *http.Request) (Principal, bool) {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if got == "" {
		return Principal{}, false
	}
	var p Principal
	ok := false
	for _, t := range a.tokens() { // compare all, constant time each
		if ctEqual(got, t.Token) {
			p, ok = Principal{Name: t.Name, Role: t.Role}, true
		}
	}
	return p, ok
}

func (a *Admin) Handler() http.Handler {
	api := http.NewServeMux()
	route := func(pattern string, editor bool, h http.HandlerFunc) {
		api.Handle(pattern, a.guard(editor, h))
	}
	route("GET /api/whoami", false, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, principalOf(r)) })
	route("GET /api/status", false, a.status)
	route("POST /api/reload", true, a.reload)
	route("GET /metrics", false, a.metrics)
	api.HandleFunc("GET /api/schema/{name}", a.schema) // public: the schemas are published anyway

	route("GET /api/access", false, a.getAccess)
	route("PUT /api/access", true, a.putAccess)
	route("POST /api/access/test", false, a.testAccess)

	route("GET /api/proxy", false, a.getProxy)
	route("PUT /api/proxy", true, a.putProxy)
	route("POST /api/proxy/clients", true, a.createClient)
	route("PATCH /api/proxy/clients/{name}", true, a.patchClient)
	route("POST /api/proxy/clients/{name}/rotate", true, a.rotateClient)
	route("DELETE /api/proxy/clients/{name}", true, a.deleteClient)

	registerCRUD(a, api, webhookResource(a.app))
	registerCRUD(a, api, profileResource(a.app))
	registerCRUD(a, api, ingestResource(a.app))

	route("GET /api/events", false, a.listEvents)
	route("GET /api/events/{id}", false, a.getEvent)
	route("GET /api/deliveries", false, a.listDeliveries)
	route("POST /api/deliveries/{id}/retry", true, a.retryDelivery)
	route("POST /api/deliveries/retry-dead", true, a.retryDead)
	route("GET /api/logs", false, a.listLogs)
	route("GET /api/logs/{id}", false, a.getLog)
	route("GET /api/logs/summary", false, a.summaryLogs)
	route("GET /api/stream", false, a.stream)

	root := http.NewServeMux()
	root.Handle("/api/", api)
	root.Handle("/metrics", api)
	var ui fs.FS = uiFS
	root.Handle("/", a.uiHandler(ui))
	return root
}

func (a *Admin) guard(editor bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := a.authenticate(r)
		if !ok {
			a.app.metrics.Inc("fluid_admin_requests_total", "role", "", "result", "unauthorized")
			slog.Warn("admin auth failed", "kind", "admin", "source_ip", ClientIP(r, a.app.cfg.Trusted).String(),
				"method", r.Method, "url", r.URL.Path, "status", 401)
			errJSON(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if editor && p.Role != RoleEditor {
			a.app.metrics.Inc("fluid_admin_requests_total", "role", string(p.Role), "result", "forbidden")
			errJSON(w, http.StatusForbidden, "this action needs an editor token")
			return
		}
		a.app.metrics.Inc("fluid_admin_requests_total", "role", string(p.Role), "result", "ok")
		if r.Method != http.MethodGet {
			slog.Info("admin change", "kind", "admin", "client", p.Name, "method", r.Method, "url", r.URL.Path,
				"source_ip", ClientIP(r, a.app.cfg.Trusted).String())
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	})
}

func errJSON(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// saveErr maps store errors to HTTP answers.
func saveErr(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrInvalid) {
		errJSON(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), "invalid: "))
		return
	}
	errJSON(w, http.StatusInternalServerError, err.Error())
}

// ---------- status / metrics / schema ----------

func (a *Admin) status(w http.ResponseWriter, r *http.Request) {
	st := a.app.store
	var dbSize int64
	if fi, err := os.Stat(a.app.db.Path()); err == nil {
		dbSize = fi.Size()
	}
	writeJSON(w, 200, map[string]any{
		"version":    version,
		"started_at": a.app.started.UTC(),
		"uptime_sec": int64(time.Since(a.app.started).Seconds()),
		"listeners":  map[string]string{"proxy": a.app.cfg.ProxyAddr, "webhook": a.app.cfg.WebhookAddr, "admin": a.app.cfg.AdminAddr},
		"config": map[string]any{
			"dir": a.app.cfg.DynamicDir, "loaded_at": st.LoadedAt().UTC(), "errors": st.Errors(),
			"webhooks": len(st.Webhooks()), "profiles": len(st.Profiles()), "ingest_rules": len(st.Ingest()),
			"clients": len(st.Proxy().Clients), "allowed_hosts": len(st.Proxy().AllowedHosts),
		},
		"deliveries":     a.app.outbox.Counts(r.Context()),
		"database":       map[string]any{"path": a.app.db.Path(), "size_bytes": dbSize, "retention_days": a.app.cfg.Database.RetentionDays},
		"logs_dropped":   a.app.sink.Dropped(),
		"active_tunnels": a.app.proxy.tunnels.Load(),
		"tracing":        a.app.tracing,
		"environment":    map[string]any{"container": a.app.env.Container, "systemd": a.app.env.Systemd},
		"deployment":     a.currentNotes(),
	})
}

// currentNotes is the startup notes with the access section recomputed, so
// the Status page reflects access.json changes made since startup.
func (a *Admin) currentNotes() []DeployNote {
	out := []DeployNote{}
	for _, n := range a.app.notes {
		if n.Topic != "access" {
			out = append(out, n)
		}
	}
	return append(out, accessNotes(a.app.cfg, a.app.store.snap.Load(), a.app.env)...)
}

func (a *Admin) reload(w http.ResponseWriter, r *http.Request) {
	if err := a.app.store.Reload(true); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"reloaded": true, "errors": a.app.store.Errors()})
}

func (a *Admin) metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	a.app.metrics.WritePrometheus(w)
}

func (a *Admin) schema(w http.ResponseWriter, r *http.Request) {
	b, err := schemaFS.ReadFile(r.PathValue("name"))
	if err != nil {
		errJSON(w, 404, "no such schema")
		return
	}
	w.Header().Set("Content-Type", "application/schema+json")
	w.Write(b)
}

// ---------- proxy rules & clients ----------

type proxyView struct {
	Clients          []clientView `json:"clients"`
	AllowedHosts     []string     `json:"allowed_hosts"`
	AllowedPorts     []int        `json:"allowed_ports"`
	AllowPrivate     bool         `json:"allow_private"`
	PrivateAllowlist []string     `json:"private_allowlist"`
}

type clientView struct {
	Name      string    `json:"name"`
	Token     string    `json:"token"` // masked except on create/rotate
	Disabled  bool      `json:"disabled"`
	CreatedAt time.Time `json:"created_at,omitzero"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

func viewClient(c Client, token string) clientView {
	return clientView{Name: c.Name, Token: token, Disabled: c.Disabled, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}

func maskToken(t string) string {
	if len(t) <= 8 {
		return Mask
	}
	return t[:4] + "…" + t[len(t)-2:]
}

func (a *Admin) getProxy(w http.ResponseWriter, r *http.Request) {
	pc := a.app.store.Proxy()
	v := proxyView{AllowedHosts: orEmpty(pc.AllowedHosts), AllowedPorts: pc.AllowedPorts,
		AllowPrivate: pc.AllowPrivate, PrivateAllowlist: orEmpty(pc.PrivateAllowlist), Clients: []clientView{}}
	for _, c := range pc.Clients {
		v.Clients = append(v.Clients, viewClient(c, maskToken(c.Token)))
	}
	writeJSON(w, 200, v)
}

// ---------- access allowlists ----------

type accessView struct {
	Proxy        []string      `json:"proxy"`
	Admin        []string      `json:"admin"`
	Routes       []routeAccess `json:"routes"`       // per webhook / ingest rule allowed_ips
	Default      []string      `json:"default"`      // what an omitted list means
	ClientIP     string        `json:"client_ip"`    // the caller's address as the agent sees it
	AdminLocked  bool          `json:"admin_locked"` // admin list is file-only
	TrustedProxy []string      `json:"trusted_proxies"`
	Notes        []DeployNote  `json:"notes"`
}

type routeAccess struct {
	Source     string    `json:"source"` // webhook | ingest
	ID         string    `json:"id"`
	Disabled   bool      `json:"disabled,omitempty"`
	AllowedIPs []string  `json:"allowed_ips"` // allowed_ips + source_cidrs, or the default
	Default    bool      `json:"default"`     // no allowed_ips/source_cidrs: restricted default applies
	Auth       bool      `json:"auth"`
	UpdatedAt  time.Time `json:"updated_at,omitzero"`
	opts       *RouteOptions
}

// routeAccessList lists every webhook and ingest rule with its senders.
func (a *Admin) routeAccessList() []routeAccess {
	snap := a.app.store.snap.Load()
	out := []routeAccess{}
	add := func(src, id string, disabled bool, updated time.Time, o *RouteOptions) {
		ips := o.allowedIPs()
		if o.restrictedDefault() {
			ips = defaultAllow
		}
		out = append(out, routeAccess{Source: src, ID: id, Disabled: disabled, AllowedIPs: ips,
			Default: o.restrictedDefault(), Auth: o.Auth != nil, UpdatedAt: updated, opts: o})
	}
	ids := slices.Sorted(maps.Keys(snap.webhooks))
	for _, id := range ids {
		w := snap.webhooks[id]
		add("webhook", id, w.Disabled, w.UpdatedAt, &w.RouteOptions)
	}
	for _, r := range snap.ingest {
		add("ingest", r.ID, r.Disabled, r.UpdatedAt, &r.RouteOptions)
	}
	return out
}

func (a *Admin) getAccess(w http.ResponseWriter, r *http.Request) {
	ac := a.app.store.Access()
	writeJSON(w, 200, accessView{Proxy: ac.Proxy.Allow, Admin: ac.Admin.Allow, Routes: a.routeAccessList(),
		Default: defaultAllow, ClientIP: ClientIP(r, a.app.cfg.Trusted).String(), AdminLocked: true,
		TrustedProxy: orEmpty(a.app.cfg.TrustedProxies),
		Notes:        orEmpty(accessNotes(a.app.cfg, a.app.store.snap.Load(), a.app.env))})
}

// putAccess changes the proxy allowlist. The admin list can only be changed
// in the file, so an editor can neither lock everyone out of the console nor
// open it to the internet over the API. Webhooks and ingest rules carry their
// own allowed_ips and are edited like any other field of theirs.
func (a *Admin) putAccess(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Proxy *[]string `json:"proxy"`
		Admin *[]string `json:"admin"`
	}
	if err := config.DecodeStrict(r.Body, &in); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	cur := a.app.store.Access()
	if in.Admin != nil && !slices.Equal(*in.Admin, cur.Admin.Allow) {
		errJSON(w, 403, "the admin allowlist is file-only: edit access.json in the dynamic config directory")
		return
	}
	next := &AccessConfig{
		Proxy: AccessList{Allow: slices.Clone(cur.Proxy.Allow)},
		Admin: AccessList{Allow: slices.Clone(cur.Admin.Allow)},
	}
	if in.Proxy != nil {
		next.Proxy.Allow = orEmpty(*in.Proxy)
	}
	if err := a.app.store.SaveAccess(next); err != nil {
		saveErr(w, err)
		return
	}
	a.getAccess(w, r)
}

// testAccess answers "would this address be let in?" for the proxy, the
// console and every webhook and ingest rule.
func (a *Admin) testAccess(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IP string `json:"ip"`
	}
	if err := config.DecodeStrict(r.Body, &in); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(in.IP))
	if err != nil {
		errJSON(w, 400, "not an IP address")
		return
	}
	type verdict struct {
		Source  string `json:"source,omitempty"`
		ID      string `json:"id,omitempty"`
		Allowed bool   `json:"allowed"`
		Rule    string `json:"rule,omitempty"`
	}
	ac := a.app.store.Access()
	out := map[string]any{"ip": ip.Unmap().String()}
	for _, name := range []string{"proxy", "admin"} {
		rule, ok := ac.list(name).list.Match(ip)
		out[name] = verdict{Allowed: ok, Rule: rule}
	}
	routes := []verdict{}
	for _, ra := range a.routeAccessList() {
		rule, ok := ra.opts.ips.Match(ip)
		routes = append(routes, verdict{Source: ra.Source, ID: ra.ID, Allowed: ok, Rule: rule})
	}
	out["routes"] = routes
	writeJSON(w, 200, out)
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// cloneProxy deep-copies the live proxy config for modification.
func (a *Admin) cloneProxy() *ProxyConfig {
	var c ProxyConfig
	b, _ := json.Marshal(a.app.store.Proxy())
	json.Unmarshal(b, &c)
	return &c
}

func (a *Admin) putProxy(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AllowedHosts     []string `json:"allowed_hosts"`
		AllowedPorts     []int    `json:"allowed_ports"`
		AllowPrivate     bool     `json:"allow_private"`
		PrivateAllowlist []string `json:"private_allowlist"`
	}
	if err := config.DecodeStrict(r.Body, &in); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	pc := a.cloneProxy()
	pc.AllowedHosts, pc.AllowedPorts, pc.AllowPrivate, pc.PrivateAllowlist = in.AllowedHosts, in.AllowedPorts, in.AllowPrivate, in.PrivateAllowlist
	if err := a.app.store.SaveProxy(pc); err != nil {
		saveErr(w, err)
		return
	}
	a.getProxy(w, r)
}

func randomToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (a *Admin) createClient(w http.ResponseWriter, r *http.Request) {
	var in Client
	if err := config.DecodeStrict(r.Body, &in); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	pc := a.cloneProxy()
	for _, c := range pc.Clients {
		if c.Name == in.Name {
			errJSON(w, 409, "client already exists")
			return
		}
	}
	if in.Token == "" {
		in.Token = randomToken()
	}
	now := time.Now().UTC().Truncate(time.Second)
	in.CreatedAt, in.UpdatedAt = now, now
	pc.Clients = append(pc.Clients, in)
	if err := a.app.store.SaveProxy(pc); err != nil {
		saveErr(w, err)
		return
	}
	writeJSON(w, 201, viewClient(in, in.Token))
}

func (a *Admin) withClient(w http.ResponseWriter, name string, fn func(c *Client)) (*Client, bool) {
	pc := a.cloneProxy()
	for i := range pc.Clients {
		if pc.Clients[i].Name == name {
			fn(&pc.Clients[i])
			pc.Clients[i].UpdatedAt = time.Now().UTC().Truncate(time.Second)
			if err := a.app.store.SaveProxy(pc); err != nil {
				saveErr(w, err)
				return nil, false
			}
			return &pc.Clients[i], true
		}
	}
	errJSON(w, 404, "no such client")
	return nil, false
}

func (a *Admin) patchClient(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Disabled *bool `json:"disabled"`
	}
	if err := config.DecodeStrict(r.Body, &in); err != nil || in.Disabled == nil {
		errJSON(w, 400, `body must be {"disabled": true|false}`)
		return
	}
	if c, ok := a.withClient(w, r.PathValue("name"), func(c *Client) { c.Disabled = *in.Disabled }); ok {
		writeJSON(w, 200, viewClient(*c, maskToken(c.Token)))
	}
}

func (a *Admin) rotateClient(w http.ResponseWriter, r *http.Request) {
	tok := randomToken()
	if c, ok := a.withClient(w, r.PathValue("name"), func(c *Client) { c.Token = tok }); ok {
		writeJSON(w, 200, viewClient(*c, tok))
	}
}

func (a *Admin) deleteClient(w http.ResponseWriter, r *http.Request) {
	pc := a.cloneProxy()
	n := len(pc.Clients)
	pc.Clients = slices.DeleteFunc(pc.Clients, func(c Client) bool { return c.Name == r.PathValue("name") })
	if len(pc.Clients) == n {
		errJSON(w, 404, "no such client")
		return
	}
	if err := a.app.store.SaveProxy(pc); err != nil {
		saveErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- generic CRUD for file-backed resources ----------

type fileResource[T any] struct {
	path    string // e.g. "webhooks"
	kind    string // directory name in the dynamic dir
	all     func() []*T
	get     func(id string) *T
	save    func(*T) error
	id      func(*T) *string
	stamps  func(*T) (*time.Time, *time.Time)
	secrets func(*T, bool) []secretRef // secret fields; bool = also viewer-only fields
	extra   func(a *Admin, mux *http.ServeMux)
}

func deepCopy[T any](v *T) *T {
	var c T
	b, _ := json.Marshal(v)
	json.Unmarshal(b, &c)
	return &c
}

// masked returns a copy with secrets hidden for the given role.
func masked[T any](res fileResource[T], v *T, role Role) *T {
	c := deepCopy(v)
	for _, s := range res.secrets(c, role == RoleViewer) {
		if s.get() != "" {
			s.set(Mask)
		}
	}
	return c
}

// restoreSecrets keeps stored values for fields sent back as Mask.
func restoreSecrets[T any](res fileResource[T], in, old *T) {
	if old == nil {
		return
	}
	prev := map[string]string{}
	for _, s := range res.secrets(deepCopy(old), true) {
		prev[s.label] = s.get()
	}
	for _, s := range res.secrets(in, true) {
		if s.get() == Mask {
			s.set(prev[s.label])
		}
	}
}

func registerCRUD[T any](a *Admin, mux *http.ServeMux, res fileResource[T]) {
	base := "/api/" + res.path
	h := func(pattern string, editor bool, fn http.HandlerFunc) { mux.Handle(pattern, a.guard(editor, fn)) }
	h("GET "+base, false, func(w http.ResponseWriter, r *http.Request) {
		items := res.all()
		out := make([]*T, 0, len(items))
		for _, it := range items {
			out = append(out, masked(res, it, principalOf(r).Role))
		}
		sort.Slice(out, func(i, j int) bool { return *res.id(out[i]) < *res.id(out[j]) })
		writeJSON(w, 200, out)
	})
	h("GET "+base+"/{id}", false, func(w http.ResponseWriter, r *http.Request) {
		it := res.get(r.PathValue("id"))
		if it == nil {
			errJSON(w, 404, "not found")
			return
		}
		writeJSON(w, 200, masked(res, it, principalOf(r).Role))
	})
	write := func(w http.ResponseWriter, r *http.Request, id string, create, merge bool) {
		var old *T
		if !create {
			old = res.get(id)
		}
		if merge && old == nil {
			errJSON(w, 404, "not found")
			return
		}
		in := new(T)
		if merge {
			in = deepCopy(old)
		}
		if err := config.DecodeStrict(r.Body, in); err != nil {
			errJSON(w, 400, err.Error())
			return
		}
		if create {
			id = *res.id(in)
			if !idRe.MatchString(id) {
				errJSON(w, 400, "id must match "+idRe.String())
				return
			}
			if res.get(id) != nil {
				errJSON(w, 409, "already exists")
				return
			}
		} else {
			*res.id(in) = id
		}
		restoreSecrets(res, in, old)
		created, updated := res.stamps(in)
		now := time.Now().UTC()
		*created, *updated = now, now
		if old != nil {
			oc, _ := res.stamps(deepCopy(old))
			*created = *oc
		}
		if err := res.save(in); err != nil {
			saveErr(w, err)
			return
		}
		code := 200
		if old == nil {
			code = 201
		}
		writeJSON(w, code, masked(res, res.get(id), principalOf(r).Role))
	}
	h("POST "+base, true, func(w http.ResponseWriter, r *http.Request) { write(w, r, "", true, false) })
	h("PUT "+base+"/{id}", true, func(w http.ResponseWriter, r *http.Request) { write(w, r, r.PathValue("id"), false, false) })
	h("PATCH "+base+"/{id}", true, func(w http.ResponseWriter, r *http.Request) { write(w, r, r.PathValue("id"), false, true) })
	h("DELETE "+base+"/{id}", true, func(w http.ResponseWriter, r *http.Request) {
		if err := a.app.store.Delete(res.kind, r.PathValue("id")); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				errJSON(w, 404, "not found")
				return
			}
			errJSON(w, 500, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if res.extra != nil {
		res.extra(a, mux)
	}
}

// secretRef is a labelled, settable secret inside a config object.
type secretRef struct {
	label string
	get   func() string
	set   func(string)
}

func fieldRef(label string, p *string) secretRef {
	return secretRef{label, func() string { return *p }, func(v string) { *p = v }}
}

func mapRefs(prefix string, m map[string]string) []secretRef {
	out := make([]secretRef, 0, len(m))
	for k := range m {
		out = append(out, secretRef{prefix + k, func() string { return m[k] }, func(v string) { m[k] = v }})
	}
	return out
}

// routeSecrets lists secrets in route options; viewers also lose add_headers values.
func routeSecrets(o *RouteOptions, viewer bool) []secretRef {
	var out []secretRef
	if o.Auth != nil {
		if o.Auth.Token != nil {
			out = append(out, fieldRef("auth.token.value", &o.Auth.Token.Value))
		}
		if o.Auth.Basic != nil {
			out = append(out, fieldRef("auth.basic.password", &o.Auth.Basic.Password))
		}
		if o.Auth.HMAC != nil {
			out = append(out, fieldRef("auth.hmac.secret", &o.Auth.HMAC.Secret))
		}
	}
	if t := o.TargetAuth; t != nil {
		out = append(out, fieldRef("target_auth.token", &t.Token), fieldRef("target_auth.password", &t.Password),
			fieldRef("target_auth.value", &t.Value))
	}
	if viewer {
		out = append(out, mapRefs("add_headers.", o.AddHeaders)...)
	}
	return out
}

func webhookResource(app *App) fileResource[Webhook] {
	return fileResource[Webhook]{
		path: "webhooks", kind: "webhooks",
		all: func() []*Webhook {
			var out []*Webhook
			for _, w := range app.store.Webhooks() {
				out = append(out, w)
			}
			return out
		},
		get:     app.store.Webhook,
		save:    app.store.SaveWebhook,
		id:      func(w *Webhook) *string { return &w.ID },
		stamps:  func(w *Webhook) (*time.Time, *time.Time) { return &w.CreatedAt, &w.UpdatedAt },
		secrets: func(w *Webhook, viewer bool) []secretRef { return routeSecrets(&w.RouteOptions, viewer) },
		extra: func(a *Admin, mux *http.ServeMux) {
			mux.Handle("GET /api/webhooks/{id}/deliveries", a.guard(false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ds, next, err := a.app.outbox.ListDeliveries(r.Context(), DeliveryQuery{Route: r.PathValue("id"), Limit: queryInt(r, "limit", 50)})
				if err != nil {
					errJSON(w, 500, err.Error())
					return
				}
				writeJSON(w, 200, map[string]any{"items": ds, "next_before_id": next})
			})))
		},
	}
}

func profileResource(app *App) fileResource[Profile] {
	return fileResource[Profile]{
		path: "profiles", kind: "profiles",
		all: func() []*Profile {
			var out []*Profile
			for _, p := range app.store.Profiles() {
				out = append(out, p)
			}
			return out
		},
		get:    app.store.Profile,
		save:   app.store.SaveProfile,
		id:     func(p *Profile) *string { return &p.Name },
		stamps: func(p *Profile) (*time.Time, *time.Time) { return &p.CreatedAt, &p.UpdatedAt },
		// profile headers usually hold provider credentials: always write-only
		secrets: func(p *Profile, _ bool) []secretRef { return mapRefs("headers.", p.Headers) },
	}
}

func ingestResource(app *App) fileResource[IngestRule] {
	return fileResource[IngestRule]{
		path: "ingest", kind: "ingest",
		all: app.store.Ingest,
		get: func(id string) *IngestRule {
			for _, r := range app.store.Ingest() {
				if r.ID == id {
					return r
				}
			}
			return nil
		},
		save:    app.store.SaveIngestRule,
		id:      func(r *IngestRule) *string { return &r.ID },
		stamps:  func(r *IngestRule) (*time.Time, *time.Time) { return &r.CreatedAt, &r.UpdatedAt },
		secrets: func(r *IngestRule, viewer bool) []secretRef { return routeSecrets(&r.RouteOptions, viewer) },
	}
}
