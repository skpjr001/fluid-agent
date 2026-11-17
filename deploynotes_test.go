package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func noteCfg(t *testing.T, mutate func(*StaticConfig)) *StaticConfig {
	t.Helper()
	c := defaultStatic()
	c.DynamicDir = t.TempDir()
	c.Database.Path = filepath.Join(t.TempDir(), "x.db")
	if mutate != nil {
		mutate(c)
	}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	return c
}

func goodSnap() *snapshot {
	return &snapshot{
		proxy: &ProxyConfig{Clients: []Client{{Name: "a", Token: "x"}}, AllowedHosts: []string{"api.example.com"}},
		admin: &AdminConfig{Tokens: []AdminToken{{Name: "e", Token: "t", Role: RoleEditor}}},
		webhooks: map[string]*Webhook{
			"open":   {ID: "open"},
			"signed": {ID: "signed", RouteOptions: RouteOptions{Auth: &InboundAuth{HMAC: &HMACAuth{}}}},
			"cidr":   {ID: "cidr", RouteOptions: RouteOptions{SourceCIDRs: []string{"1.2.3.4"}}},
			"off":    {ID: "off", Disabled: true},
		},
		ingest: []*IngestRule{{ID: "nse"}},
	}
}

// has reports whether a note with this level contains every fragment.
func has(ns []DeployNote, level string, frags ...string) bool {
	for _, n := range ns {
		if n.Level != level {
			continue
		}
		ok := true
		for _, f := range frags {
			ok = ok && strings.Contains(n.Message, f)
		}
		if ok {
			return true
		}
	}
	return false
}

func dump(ns []DeployNote) string {
	var b strings.Builder
	for _, n := range ns {
		b.WriteString(n.Level + " [" + n.Topic + "] " + n.Message + "\n")
	}
	return b.String()
}

func TestNotesPlainHostEverythingExposed(t *testing.T) {
	cfg := noteCfg(t, func(c *StaticConfig) { c.AdminAddr = "0.0.0.0:9000" })
	ns := deploymentNotes(cfg, goodSnap(), hostEnv{EUID: 0}, false, time.Now())
	for _, want := range []struct{ level, frag string }{
		{"info", "running on the host (not under systemd"},
		{"warn", "running as root"},
		{"warn", "forward proxy is plain HTTP on all interfaces"},
		{"info", "cannot sit behind an HTTP reverse proxy"},
		{"warn", "webhooks are plain HTTP and no reverse proxy is trusted"},
		{"warn", "console/API on 0.0.0.0:9000 is plain HTTP on all interfaces"},
		{"warn", "webhook open accepts requests from anyone"},
		{"warn", "ingest rule nse accepts requests from anyone"},
		{"info", "proxy listener: http://:8080 (all interfaces), path /"},
		{"info", "admin listener: http://0.0.0.0:9000 (all interfaces)"},
		{"info", "callback bodies are kept for 14 days"},
	} {
		if !has(ns, want.level, want.frag) {
			t.Errorf("missing %s %q in:\n%s", want.level, want.frag, dump(ns))
		}
	}
	for _, unwanted := range []string{"webhook signed", "webhook cidr", "webhook off"} {
		if has(ns, "warn", unwanted) {
			t.Errorf("protected/disabled route flagged: %s", unwanted)
		}
	}
}

func TestNotesSystemdLowPort(t *testing.T) {
	cfg := noteCfg(t, func(c *StaticConfig) { c.WebhookAddr = ":443" })
	ns := deploymentNotes(cfg, goodSnap(), hostEnv{Systemd: true, EUID: 1000}, false, time.Now())
	if !has(ns, "info", "under systemd") || !has(ns, "info", "port 443 is privileged", "CAP_NET_BIND_SERVICE") {
		t.Fatal(dump(ns))
	}
	if has(ns, "warn", "running as root") || has(ns, "warn", "console/API") {
		t.Fatalf("loopback admin and non-root must not warn:\n%s", dump(ns))
	}
}

func TestNotesContainer(t *testing.T) {
	cfg := noteCfg(t, func(c *StaticConfig) { c.AdminAddr = ":9000" })
	// no volumes mounted
	ns := deploymentNotes(cfg, goodSnap(), hostEnv{Container: "docker", EUID: 65532, Mounts: []string{"/"}}, false, time.Now())
	for _, want := range []struct{ level, frag string }{
		{"info", "running in a docker container"},
		{"info", "host's public IP is the one providers must allowlist"},
		{"warn", "is not on a mounted volume"},
		{"warn", "is not mounted from the host"},
		{"info", "publish it as 127.0.0.1:<port> only"},
		{"info", "publish its port only to trusted networks"},
	} {
		if !has(ns, want.level, want.frag) {
			t.Errorf("missing %s %q in:\n%s", want.level, want.frag, dump(ns))
		}
	}
	if has(ns, "warn", "running as root") || has(ns, "warn", "console/API on") {
		t.Errorf("host-only warnings in a container:\n%s", dump(ns))
	}
	// with volumes
	ns = deploymentNotes(cfg, goodSnap(), hostEnv{Container: "podman", Mounts: []string{"/", filepath.Dir(cfg.Database.Path), cfg.DynamicDir}}, false, time.Now())
	if has(ns, "warn", "mounted volume") || has(ns, "warn", "not mounted from the host") {
		t.Errorf("volumes not recognised:\n%s", dump(ns))
	}
}

func TestNotesBehindReverseProxy(t *testing.T) {
	cfg := noteCfg(t, func(c *StaticConfig) {
		c.TrustedProxies = []string{"10.0.0.0/8", "0.0.0.0/0"}
		c.AdminBasePath, c.WebhookBasePath, c.ProxyBasePath = "/console", "/gw", "/relay"
	})
	ns := deploymentNotes(cfg, goodSnap(), hostEnv{EUID: 1000}, false, time.Now())
	for _, want := range []struct{ level, frag string }{
		{"info", "expecting TLS to be terminated by a reverse proxy at 10.0.0.0/8"},
		{"warn", "trusted_proxies contains 0.0.0.0/0"},
		{"info", "console at /console/"},
		{"info", "webhooks at /gw/hooks/<id>"},
		{"info", "JSON relay at /relay/forward"},
		{"info", "admin listener: http://127.0.0.1:9000 (loopback only), path /console"},
	} {
		if !has(ns, want.level, want.frag) {
			t.Errorf("missing %s %q in:\n%s", want.level, want.frag, dump(ns))
		}
	}
	if has(ns, "warn", "trusted_proxies contains 10.0.0.0/8") || has(ns, "warn", "no reverse proxy is trusted") {
		t.Errorf("unexpected warning:\n%s", dump(ns))
	}
	cfg.AdminBasePath = ""
	if ns := deploymentNotes(cfg, goodSnap(), hostEnv{}, false, time.Now()); !has(ns, "info", "X-Forwarded-Prefix") {
		t.Errorf("X-Forwarded-Prefix note missing:\n%s", dump(ns))
	}
}

func TestNotesCertificates(t *testing.T) {
	dir := t.TempDir()
	cert, key, _ := writeCert(t, dir, "c", "hooks.example.com")
	cfg := noteCfg(t, func(c *StaticConfig) {
		c.WebhookTLS = &TLSConfig{CertFile: cert, KeyFile: key}
		c.ProxyTLS = &TLSConfig{CertFile: cert, KeyFile: key}
	})
	ns := deploymentNotes(cfg, goodSnap(), hostEnv{EUID: 1000}, false, time.Now())
	if !has(ns, "info", "certificate", "hooks.example.com", "expires") || !has(ns, "info", "webhooks are served over HTTPS directly") ||
		!has(ns, "info", "HTTPS proxy: clients use https://") || has(ns, "warn", "forward proxy is plain HTTP") {
		t.Fatal(dump(ns))
	}
	// writeCert certificates are valid for one hour: expiring soon / expired
	if !has(ns, "warn", "expires in 0 days") {
		t.Errorf("expiry warning missing:\n%s", dump(ns))
	}
	if ns := deploymentNotes(cfg, goodSnap(), hostEnv{}, false, time.Now().Add(2*time.Hour)); !has(ns, "error", "EXPIRED") {
		t.Errorf("expired error missing:\n%s", dump(ns))
	}
	nameless, nkey, _ := writeCert(t, dir, "n")
	cfg.WebhookTLS = &TLSConfig{CertFile: nameless, KeyFile: nkey}
	if ns := deploymentNotes(cfg, goodSnap(), hostEnv{}, false, time.Now()); !has(ns, "warn", "no subject alternative names") {
		t.Errorf("SAN warning missing:\n%s", dump(ns))
	}
}

func TestNotesACME(t *testing.T) {
	acme := func(addr, challenge, dir string) []DeployNote {
		cfg := noteCfg(t, func(c *StaticConfig) {
			c.WebhookAddr = addr
			c.WebhookTLS = &TLSConfig{ACME: &ACMEConfig{Domains: []string{"hooks.example.com"}, AcceptTOS: true,
				HTTPChallengeAddr: challenge, DirectoryURL: dir}}
		})
		return deploymentNotes(cfg, goodSnap(), hostEnv{EUID: 1000}, false, time.Now())
	}
	if ns := acme(":8443", "", ""); !has(ns, "warn", "TLS-ALPN-01 needs port 443") || !has(ns, "info", "Let's Encrypt production (rate limits apply)") {
		t.Errorf("unreachable challenge not flagged:\n%s", dump(ns))
	}
	if ns := acme(":443", "", "https://acme-staging-v02.api.letsencrypt.org/directory"); !has(ns, "info", "TLS-ALPN-01 challenges on port 443") ||
		has(ns, "info", "rate limits apply") {
		t.Errorf("ALPN on 443:\n%s", dump(ns))
	}
	if ns := acme(":8443", ":80", ""); !has(ns, "info", "HTTP-01 challenges on :80") || has(ns, "warn", "TLS-ALPN-01 needs") {
		t.Errorf("HTTP-01:\n%s", dump(ns))
	}
}

func TestNotesConfigProblems(t *testing.T) {
	cfg := noteCfg(t, nil)
	os.Chmod(cfg.DynamicDir, 0o500)
	defer os.Chmod(cfg.DynamicDir, 0o700)
	snap := &snapshot{proxy: &ProxyConfig{AllowPrivate: true}, admin: &AdminConfig{Tokens: []AdminToken{{Role: RoleViewer}}},
		webhooks: map[string]*Webhook{}}
	ns := deploymentNotes(cfg, snap, hostEnv{EUID: 1000}, false, time.Now())
	for _, want := range []struct{ level, frag string }{
		{"warn", "no proxy clients"},
		{"warn", "allowed_hosts is empty"},
		{"warn", "allow_private is on"},
		{"info", "only viewer tokens"},
	} {
		if !has(ns, want.level, want.frag) {
			t.Errorf("missing %s %q in:\n%s", want.level, want.frag, dump(ns))
		}
	}
	if os.Geteuid() != 0 && !has(ns, "warn", "is read-only") {
		t.Errorf("read-only dynamic dir not flagged:\n%s", dump(ns))
	}
	snap.admin = &AdminConfig{}
	if ns := deploymentNotes(cfg, snap, hostEnv{}, false, time.Now()); !has(ns, "warn", "no admin tokens") {
		t.Error("no-token warning missing")
	}
	if ns := deploymentNotes(cfg, snap, hostEnv{}, true, time.Now()); has(ns, "warn", "no admin tokens") || has(ns, "info", "only viewer") {
		t.Error("PROXY_ADMIN_TOKEN should count as an editor token")
	}
}

func TestNotesAreLoggedAndServed(t *testing.T) {
	ta := newTestApp(t, nil)
	ta.notes = deploymentNotes(ta.cfg, ta.store.snap.Load(), hostEnv{EUID: 1000}, false, time.Now())
	logDeploymentNotes(ta.notes)
	ta.flushLogs()
	rows, _, _ := QueryLogs(t.Context(), ta.db, LogQuery{Kinds: []string{"deploy"}, Level: "warn"})
	if len(rows) == 0 || rows[0].Attrs["topic"] == nil {
		t.Fatalf("deploy warnings not stored: %+v", rows)
	}
	_, body := ta.api("GET", "/api/status", viewerToken, nil)
	if !strings.Contains(string(body), `"deployment"`) || !strings.Contains(string(body), "cannot sit behind an HTTP reverse proxy") {
		t.Fatalf("status lacks deployment notes: %s", body)
	}
}

func TestCheckPrintsNotes(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	writeFile(t, cfgPath, `{"dynamic_dir": "`+filepath.Join(dir, "dyn")+`", "admin_addr": "0.0.0.0:9000"}`)
	os.MkdirAll(filepath.Join(dir, "dyn"), 0o750)
	f, _ := os.CreateTemp(dir, "out")
	defer f.Close()
	if err := cmdCheck([]string{"-config", cfgPath}, f); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f.Name())
	for _, want := range []string{"Deployment notes:", "WARN  [admin] console/API on 0.0.0.0:9000", "OK: all config files are valid"} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("check output lacks %q:\n%s", want, b)
		}
	}
}
