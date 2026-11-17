package main

import (
	"encoding/json"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIPRuleForms(t *testing.T) {
	cases := []struct {
		rule string
		yes  []string
		no   []string
	}{
		{"*", []string{"1.2.3.4", "2001:db8::1"}, nil},
		{"203.0.113.7", []string{"203.0.113.7", "::ffff:203.0.113.7"}, []string{"203.0.113.8"}},
		{"10.0.0.0/8", []string{"10.255.1.2"}, []string{"11.0.0.1"}},
		{"2001:db8::/32", []string{"2001:db8:1::5"}, []string{"2001:db9::1", "10.0.0.1"}},
		{"10.0.*.*", []string{"10.0.0.1", "10.0.255.255"}, []string{"10.1.0.1"}},
		{"10.*", []string{"10.9.8.7"}, []string{"11.0.0.0"}},
		{"192.168.*", []string{"192.168.44.1"}, []string{"192.169.0.1"}},
		{"192.168.1-5.*", []string{"192.168.1.9", "192.168.5.200"}, []string{"192.168.6.1", "192.168.0.1"}},
		{"*.*.*.7", []string{"8.8.8.7"}, []string{"8.8.8.8", "::1"}},
		{"52.66.10-20.0-127", []string{"52.66.15.127"}, []string{"52.66.15.128"}},
	}
	for _, c := range cases {
		r, err := parseIPRule(c.rule)
		if err != nil {
			t.Fatalf("%s: %v", c.rule, err)
		}
		for _, s := range c.yes {
			if !r.match(netip.MustParseAddr(s)) {
				t.Errorf("%s should match %s", c.rule, s)
			}
		}
		for _, s := range c.no {
			if r.match(netip.MustParseAddr(s)) {
				t.Errorf("%s should not match %s", c.rule, s)
			}
		}
	}
	for _, bad := range []string{"", "10.0", "10.0.0.0.0", "10.0.*.256", "10.0.5-1.*", "10.01.*", "2001:db8::*", "host.example", "10.0.a.*", "-"} {
		if _, err := parseIPRule(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestAccessDefaultsAreRestricted(t *testing.T) {
	ac := emptyAccess()
	for _, name := range []string{"proxy", "admin"} {
		l := ac.list(name)
		for _, ok := range []string{"127.0.0.1", "::1", "10.1.2.3", "172.20.0.5", "192.168.1.1", "fd00::1"} {
			if _, m := l.list.Match(netip.MustParseAddr(ok)); !m {
				t.Errorf("%s default should allow %s", name, ok)
			}
		}
		for _, no := range []string{"8.8.8.8", "172.32.0.1", "2001:db8::1", "169.254.169.254"} {
			if _, m := l.list.Match(netip.MustParseAddr(no)); m {
				t.Errorf("%s default should refuse %s", name, no)
			}
		}
	}
	// Omitted section = default; [] = nobody.
	var a AccessConfig
	if err := json.Unmarshal([]byte(`{"proxy":{"allow":[]}}`), &a); err != nil {
		t.Fatal(err)
	}
	if err := a.normalize(); err != nil {
		t.Fatal(err)
	}
	if len(a.Proxy.Allow) != 0 || strings.Join(a.Admin.Allow, ",") != strings.Join(defaultAllow, ",") {
		t.Errorf("proxy %v admin %v", a.Proxy.Allow, a.Admin.Allow)
	}
	if err := decodeStrict(strings.NewReader(`{"webhook":{"allow":["*"]}}`), &AccessConfig{}); err == nil {
		t.Error("webhook lists belong in each route's allowed_ips, not access.json")
	}
	bad := AccessConfig{Proxy: AccessList{Allow: []string{"10.0.*.300"}}}
	if err := bad.normalize(); err == nil || !strings.Contains(err.Error(), "proxy.allow[0]") {
		t.Errorf("want a located error, got %v", err)
	}
}

// accessApp trusts 127.0.0.1 as a reverse proxy, so X-Forwarded-For decides
// the client address in these tests.
func accessApp(t *testing.T) *testApp {
	ta := newTestApp(t, func(c *StaticConfig) { c.TrustedProxies = []string{"127.0.0.1/32"} })
	ta.writeJSON("webhooks/w.json", Webhook{ID: "w", Target: "http://127.0.0.1:1/x", Async: true})
	return ta
}

func from(t *testing.T, method, u, ip string, hdr ...string) int {
	t.Helper()
	req, _ := http.NewRequest(method, u, strings.NewReader("{}"))
	if ip != "" {
		req.Header.Set("X-Forwarded-For", ip)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func TestRouteAllowedIPsDefaultIsRestricted(t *testing.T) {
	ta := accessApp(t) // webhook w has no allowed_ips
	if c := from(t, "POST", ta.inbound.URL+"/hooks/w", "10.20.30.40"); c != 202 {
		t.Fatalf("private sender: %d", c)
	}
	if c := from(t, "POST", ta.inbound.URL+"/hooks/w", ""); c != 202 {
		t.Fatalf("local sender must work: %d", c)
	}
	if c := from(t, "POST", ta.inbound.URL+"/hooks/w", "52.66.1.1"); c != 403 {
		t.Fatalf("public sender must be refused by default: %d", c)
	}
	ta.writeJSON("ingest/cb.json", IngestRule{ID: "cb", Match: PathMatch{Type: "prefix", Value: "/cb/"}, Targets: []string{"http://127.0.0.1:1/"}})
	if c := from(t, "POST", ta.inbound.URL+"/cb/x", "52.66.1.1"); c != 403 {
		t.Fatalf("ingest rule default: %d", c)
	}
	if c := from(t, "POST", ta.inbound.URL+"/cb/x", "192.168.0.9"); c != 200 {
		t.Fatalf("ingest rule private sender: %d", c)
	}
	ta.flushLogs()
	rows, _, _ := QueryLogs(t.Context(), ta.db, LogQuery{Text: "allowed_ips"})
	if len(rows) < 2 || rows[0].SourceIP != "52.66.1.1" || rows[0].Status != 403 || rows[0].Direction != "inbound" {
		t.Fatalf("refusals must be logged with the sender: %+v", rows)
	}
}

func TestRouteAllowedIPsPerRoute(t *testing.T) {
	ta := accessApp(t)
	tgt := "http://127.0.0.1:1/x"
	ta.writeJSON("webhooks/razorpay.json", Webhook{ID: "razorpay", Target: tgt, Async: true, RouteOptions: RouteOptions{AllowedIPs: []string{"52.66.*.*", "13.232.0.0/16"}}})
	ta.writeJSON("webhooks/github.json", Webhook{ID: "github", Target: tgt, Async: true, RouteOptions: RouteOptions{AllowedIPs: []string{"140.82.112-127.*"}}})
	ta.writeJSON("webhooks/legacy.json", Webhook{ID: "legacy", Target: tgt, Async: true, RouteOptions: RouteOptions{SourceCIDRs: []string{"203.0.113.0/24"}}})
	ta.writeJSON("webhooks/both.json", Webhook{ID: "both", Target: tgt, Async: true, RouteOptions: RouteOptions{AllowedIPs: []string{"198.51.100.*"}, SourceCIDRs: []string{"203.0.113.9"}}})
	ta.writeJSON("webhooks/any.json", Webhook{ID: "any", Target: tgt, Async: true, RouteOptions: RouteOptions{AllowedIPs: []string{"*"}}})
	ta.writeJSON("ingest/nse.json", IngestRule{ID: "nse", Match: PathMatch{Type: "exact", Value: "/nse"}, Targets: []string{tgt},
		RouteOptions: RouteOptions{AllowedIPs: []string{"115.112.*.*"}}})
	for _, c := range []struct {
		path, ip string
		want     int
	}{
		{"/hooks/razorpay", "52.66.9.9", 202}, {"/hooks/razorpay", "13.232.4.4", 202}, {"/hooks/razorpay", "140.82.115.1", 403},
		{"/hooks/github", "140.82.115.1", 202}, {"/hooks/github", "140.82.128.1", 403}, {"/hooks/github", "52.66.9.9", 403},
		{"/hooks/legacy", "203.0.113.50", 202}, {"/hooks/legacy", "10.0.0.1", 403}, // source_cidrs replaces the default too
		{"/hooks/both", "198.51.100.7", 202}, {"/hooks/both", "203.0.113.9", 202}, {"/hooks/both", "203.0.113.10", 403},
		{"/hooks/any", "8.8.8.8", 202},
		{"/nse", "115.112.1.2", 200}, {"/nse", "52.66.9.9", 403},
	} {
		if got := from(t, "POST", ta.inbound.URL+c.path, c.ip); got != c.want {
			t.Errorf("%s from %s: %d, want %d", c.path, c.ip, got, c.want)
		}
	}
	// Health checks are not routes and are always served.
	if c := from(t, "GET", ta.inbound.URL+"/livez", "8.8.8.8"); c != 200 {
		t.Errorf("/livez: %d", c)
	}
	w := Webhook{ID: "x", Target: "http://a/", RouteOptions: RouteOptions{AllowedIPs: []string{"10.*.x"}}}
	if err := w.normalize(); err == nil || !strings.Contains(err.Error(), "allowed_ips[0]") {
		t.Errorf("bad wildcard: %v", err)
	}
	// The API returns both fields as written.
	code, body := ta.api("GET", "/api/webhooks/both", viewerToken, nil)
	if code != 200 || !strings.Contains(string(body), `"allowed_ips"`) || !strings.Contains(string(body), `"source_cidrs"`) {
		t.Errorf("%d %s", code, body)
	}
}

func TestListenerAllowlistsProxyAndConsole(t *testing.T) {
	ta := accessApp(t)
	if c := from(t, "GET", ta.admin.URL+"/", "52.66.1.1"); c != 403 {
		t.Fatalf("console from public address (default): %d", c)
	}
	if c := from(t, "GET", ta.admin.URL+"/api/whoami", "52.66.1.1", "Authorization", "Bearer "+editorToken); c != 403 {
		t.Fatalf("a valid token does not bypass the allowlist: %d", c)
	}
	if c := from(t, "GET", ta.admin.URL+"/api/whoami", "", "Authorization", "Bearer "+editorToken); c != 200 {
		t.Fatalf("local console: %d", c)
	}
	ta.writeJSON("access.json", AccessConfig{
		Admin: AccessList{Allow: []string{"127.0.0.1", "203.0.113.1-9"}},
		Proxy: AccessList{Allow: []string{}},
	})
	if c := from(t, "GET", ta.admin.URL+"/", "203.0.113.5"); c != 200 {
		t.Errorf("console from range: %d", c)
	}
	if c := from(t, "GET", ta.admin.URL+"/", "203.0.113.10"); c != 403 {
		t.Errorf("console outside range: %d", c)
	}
	if c := from(t, "POST", ta.proxy.URL+"/forward", "", "X-Agent-Key", clientToken); c != 403 {
		t.Errorf("/forward with empty proxy list: %d", c)
	}
	pu, _ := url.Parse(ta.proxy.URL)
	req, _ := http.NewRequest(http.MethodConnect, ta.proxy.URL, nil)
	req.Host, req.URL = "127.0.0.1:443", &url.URL{Scheme: "http", Host: pu.Host}
	if res, err := http.DefaultTransport.RoundTrip(req); err != nil || res.StatusCode != 403 {
		t.Errorf("CONNECT with empty proxy list: %v %v", res, err)
	}
	if c := from(t, "GET", ta.proxy.URL+"/healthz", ""); c != 200 {
		t.Errorf("loopback proxy /healthz: %d", c)
	}
	ta.flushLogs()
	rows, _, _ := QueryLogs(t.Context(), ta.db, LogQuery{Kinds: []string{"access"}})
	if len(rows) < 3 || rows[0].Status != 403 {
		t.Fatalf("denials must be logged: %+v", rows)
	}
	// A broken edit keeps the previous lists.
	os.WriteFile(filepath.Join(ta.dir, "access.json"), []byte(`{"admin":{"allow":["203.0.*.x"]}}`), 0o640)
	ta.store.Reload(true)
	if c := from(t, "GET", ta.admin.URL+"/", "203.0.113.5"); c != 200 {
		t.Errorf("invalid access.json must keep the last good lists: %d", c)
	}
	if !strings.Contains(ta.store.Errors()[filepath.Join(ta.dir, "access.json")], "admin.allow[0]") {
		t.Errorf("error not reported: %v", ta.store.Errors())
	}
}

func TestAccessAPI(t *testing.T) {
	ta := accessApp(t)
	ta.writeJSON("webhooks/rz.json", Webhook{ID: "rz", Target: "http://127.0.0.1:1/", Async: true, RouteOptions: RouteOptions{AllowedIPs: []string{"52.66.*.*"}}})
	code, body := ta.api("GET", "/api/access", viewerToken, nil)
	var v accessView
	mustJSON(t, body, &v)
	if code != 200 || v.ClientIP != "127.0.0.1" || !v.AdminLocked || len(v.Routes) != 2 {
		t.Fatalf("%d %s", code, body)
	}
	if r := v.Routes[0]; r.ID != "rz" || r.Default || r.AllowedIPs[0] != "52.66.*.*" {
		t.Errorf("route rz: %+v", r)
	}
	if r := v.Routes[1]; r.ID != "w" || !r.Default || len(r.AllowedIPs) != len(defaultAllow) {
		t.Errorf("route w: %+v", r)
	}
	if code, _ := ta.api("PUT", "/api/access", viewerToken, map[string]any{"proxy": []string{"*"}}); code != 403 {
		t.Fatalf("viewer PUT: %d", code)
	}
	code, body = ta.api("PUT", "/api/access", editorToken, map[string]any{"proxy": []string{"10.0.0.0/8"}})
	mustJSON(t, body, &v)
	if code != 200 || v.Proxy[0] != "10.0.0.0/8" || len(v.Admin) != len(defaultAllow) {
		t.Fatalf("%d %s", code, body)
	}
	b, _ := os.ReadFile(filepath.Join(ta.dir, "access.json"))
	if !strings.Contains(string(b), "access.schema.json") || !strings.Contains(string(b), `"10.0.0.0/8"`) {
		t.Fatalf("file not written: %s", b)
	}
	if code, body := ta.api("PUT", "/api/access", editorToken, map[string]any{"admin": []string{"*"}}); code != 403 || !strings.Contains(string(body), "file-only") {
		t.Fatalf("admin list must be file-only: %d %s", code, body)
	}
	if code, _ := ta.api("PUT", "/api/access", editorToken, map[string]any{"webhook": []string{"*"}}); code != 400 {
		t.Fatalf("webhook lists live on each route: %d", code)
	}
	if code, body := ta.api("PUT", "/api/access", editorToken, map[string]any{"proxy": []string{"1.2.3.*.4"}}); code != 400 {
		t.Fatalf("invalid rule: %d %s", code, body)
	}
	// Per-route allowed_ips are edited like any other field.
	if code, body := ta.api("PATCH", "/api/webhooks/w", editorToken, map[string]any{"allowed_ips": []string{"140.82.*"}}); code != 200 {
		t.Fatalf("patch allowed_ips: %d %s", code, body)
	}
	code, body = ta.api("POST", "/api/access/test", viewerToken, map[string]any{"ip": "140.82.1.1"})
	var tv struct {
		Proxy, Admin struct {
			Allowed bool
			Rule    string
		}
		Routes []struct {
			ID      string
			Allowed bool
			Rule    string
		}
	}
	json.Unmarshal(body, &tv)
	if code != 200 || tv.Proxy.Allowed || tv.Admin.Allowed || len(tv.Routes) != 2 ||
		tv.Routes[0].Allowed || !tv.Routes[1].Allowed || tv.Routes[1].Rule != "140.82.*" {
		t.Fatalf("%d %s", code, body)
	}
	if code, _ := ta.api("POST", "/api/access/test", viewerToken, map[string]any{"ip": "nope"}); code != 400 {
		t.Fatalf("bad ip: %d", code)
	}
}

func TestAccessNotes(t *testing.T) {
	cfg := noteCfg(t, nil)
	snap := &snapshot{access: emptyAccess(), webhooks: map[string]*Webhook{
		"def":    {ID: "def"},
		"open":   {ID: "open", RouteOptions: RouteOptions{AllowedIPs: []string{"*"}}},
		"signed": {ID: "signed", RouteOptions: RouteOptions{AllowedIPs: []string{"*"}, Auth: &InboundAuth{HMAC: &HMACAuth{}}}},
		"rz":     {ID: "rz", RouteOptions: RouteOptions{AllowedIPs: []string{"52.66.*.*"}, SourceCIDRs: []string{"13.232.0.0/16"}}},
		"off":    {ID: "off", Disabled: true},
	}, ingest: []*IngestRule{{ID: "nse"}}}
	for _, w := range snap.webhooks {
		w.RouteOptions.normalize(true)
	}
	snap.ingest[0].RouteOptions.normalize(true)
	ns := accessNotes(cfg, snap, hostEnv{Container: "docker"})
	for _, want := range [][2]string{
		{"warn", "webhook def accepts only loopback and private-network senders"},
		{"warn", "ingest rule nse accepts only loopback and private-network senders"},
		{"warn", "webhook open accepts requests from any address without auth"},
		{"info", "webhook signed accepts any address; requests must pass its auth"},
		{"info", "webhook rz accepts only: 52.66.*.*, 13.232.0.0/16"},
		{"info", "forward proxy and /forward accept only loopback and private networks"},
		{"warn", "rootless Podman"},
		{"info", "list it in trusted_proxies"},
	} {
		if !has(ns, want[0], want[1]) {
			t.Errorf("missing %s note %q in %+v", want[0], want[1], ns)
		}
	}
	if has(ns, "warn", "webhook off") || has(ns, "info", "webhook off") {
		t.Error("disabled routes need no note")
	}
	open := &AccessConfig{Proxy: AccessList{Allow: []string{"*"}}, Admin: AccessList{Allow: []string{"10.*"}}}
	open.normalize()
	ns = accessNotes(cfg, &snapshot{access: open}, hostEnv{})
	for _, want := range [][2]string{
		{"warn", "forward proxy accepts connections from any address"},
		{"error", "nobody can reach it"},
	} {
		if !has(ns, want[0], want[1]) {
			t.Errorf("missing %s note %q in %+v", want[0], want[1], ns)
		}
	}
	empty := &AccessConfig{Proxy: AccessList{Allow: []string{}}}
	empty.normalize()
	if !has(accessNotes(cfg, &snapshot{access: empty}, hostEnv{}), "error", "proxy.allow is empty, so every client is refused") {
		t.Error("an empty proxy list must be an error note")
	}
	wide := &AccessConfig{Admin: AccessList{Allow: []string{"0.0.0.0/0"}}}
	wide.normalize()
	if !has(accessNotes(cfg, &snapshot{access: wide}, hostEnv{}), "warn", "console and API accept only: 0.0.0.0/0") {
		t.Error("a public catch-all CIDR should warn")
	}
}

func TestAccessChangeIsLogged(t *testing.T) {
	ta := accessApp(t)
	ta.writeJSON("webhooks/w.json", Webhook{ID: "w", Target: "http://127.0.0.1:1/x", Async: true, RouteOptions: RouteOptions{AllowedIPs: []string{"*"}}})
	ta.writeJSON("access.json", AccessConfig{Admin: AccessList{Allow: []string{"*"}}})
	ta.flushLogs()
	for _, q := range []string{"webhook w accepts requests from any address without auth", "console and API accept connections from any address"} {
		rows, _, _ := QueryLogs(t.Context(), ta.db, LogQuery{Kinds: []string{"deploy"}, Text: q})
		if len(rows) == 0 {
			t.Errorf("change not logged: %q", q)
		}
	}
}

func TestBootstrapWritesRestrictedAccess(t *testing.T) {
	cfgPath, cfg := freshLayout(t)
	bootstrap(cfgPath, cfg, false, false)
	snap := loadDir(cfg.DynamicDir, nil)
	if len(snap.errors) != 0 || strings.Join(snap.access.Proxy.Allow, ",") != strings.Join(defaultAllow, ",") ||
		strings.Join(snap.access.Admin.Allow, ",") != strings.Join(defaultAllow, ",") {
		t.Fatalf("%v %+v", snap.errors, snap.access)
	}
}
