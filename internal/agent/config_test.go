package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skpjr001/fluid-agent/internal/config"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o750)
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestStaticConfigStrict(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		`{"proxy_adr": ":1"}`:                                            `unknown field "proxy_adr"`,
		`{"admin_token": "x"}`:                                           "admin.json",
		`{"trust_forwarded_for": true}`:                                  "trusted_proxies",
		`{"reload_interval": 5}`:                                         "duration must be a string",
		`{"reload_interval": "soon"}`:                                    "bad duration",
		`{"trusted_proxies": ["nope"]}`:                                  "bad IP or CIDR",
		`{"database": {"log_level": "loud"}}`:                            "log_level",
		`{"webhook_rate_limit": {"requests_per_minute": 5, "burst": 0}}`: "burst must be > 0",
		`{} {}`: "unexpected data",
	}
	for body, want := range cases {
		p := filepath.Join(dir, "c.json")
		writeFile(t, p, body)
		_, err := config.Load(p)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want error containing %q", body, err, want)
		}
	}
	// missing file = defaults
	c, err := config.Load(filepath.Join(dir, "missing.json"))
	if err != nil || c.ProxyAddr != ":8080" || c.Database.RetentionDays != 14 {
		t.Fatalf("defaults: %+v %v", c, err)
	}
}

func TestExampleConfigsAreValid(t *testing.T) {
	t.Chdir(t.TempDir()) // its relative ./dynamic must not land in the source tree
	if p := checkConfig(repoFile("examples/config.example.json")); len(p) != 0 {
		// config.example.json points at ./dynamic, which is git-ignored; validate examples/ instead
		if !strings.Contains(p[0], "dynamic") {
			t.Fatal(p)
		}
	}
	snap := loadDir(repoFile("examples/dynamic"), nil)
	if len(snap.errors) != 0 {
		t.Fatalf("examples/dynamic has invalid files: %v", snap.errors)
	}
	if len(snap.webhooks) == 0 || len(snap.ingest) == 0 || len(snap.profiles) == 0 || len(snap.proxy.Clients) == 0 || len(snap.admin.Tokens) == 0 {
		t.Fatal("examples should include every kind of dynamic file")
	}
	for _, f := range []string{repoFile("examples/config.example.json"), repoFile("examples/config.docker.json")} {
		if _, err := config.Load(f); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
}

func TestCheckCommandReportsEveryBadFile(t *testing.T) {
	dir := t.TempDir()
	dyn := filepath.Join(dir, "dyn")
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"dynamic_dir": "`+dyn+`"}`)
	writeFile(t, filepath.Join(dyn, "proxy.json"), `{"clients": [], "allowed_hosts": ["*"]}`)
	writeFile(t, filepath.Join(dyn, "admin.json"), `{"tokens": [{"name": "a", "token": "short", "role": "god"}]}`)
	writeFile(t, filepath.Join(dyn, "webhooks", "a.json"), `{"target": "ftp://x", "typo": 1}`)
	writeFile(t, filepath.Join(dyn, "webhooks", "b.json"), `{"id": "c", "target": "https://x"}`)
	writeFile(t, filepath.Join(dyn, "ingest", "r.json"), `{"match": {"type": "regex", "value": "("}, "targets": []}`)
	writeFile(t, filepath.Join(dyn, "profiles", "Bad Name.json"), `{}`)
	problems := checkConfig(cfg)
	joined := strings.Join(problems, "\n")
	for _, want := range []string{"proxy.json", "admin.json", "a.json: json: unknown field", "does not match filename",
		"r.json", "match.value", "targets: at least one", "Bad Name.json", "role must be"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
}

func TestBadFileKeepsPreviousVersion(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "webhooks", "a.json"), `{"target": "https://good.example"}`)
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Webhook("a") == nil {
		t.Fatal("webhook not loaded")
	}
	writeFile(t, filepath.Join(dir, "webhooks", "a.json"), `{"target": "nope"}`)
	s.Reload(true)
	if w := s.Webhook("a"); w == nil || w.Target != "https://good.example" {
		t.Fatalf("previous version should be kept, got %+v", w)
	}
	if len(s.Errors()) != 1 {
		t.Fatalf("errors: %v", s.Errors())
	}
	// fixing the file clears the error
	writeFile(t, filepath.Join(dir, "webhooks", "a.json"), `{"target": "https://new.example"}`)
	s.Reload(true)
	if s.Webhook("a").Target != "https://new.example" || len(s.Errors()) != 0 {
		t.Fatal("fixed file should load")
	}
}

func TestWebhookDefaults(t *testing.T) {
	w := &Webhook{ID: "x", Target: "https://t.example"}
	if err := w.normalize(); err != nil {
		t.Fatal(err)
	}
	if w.retries() != 0 || w.TimeoutSec != 15 || w.Methods[0] != "POST" {
		t.Fatalf("sync defaults: %+v", w)
	}
	a := &Webhook{ID: "y", Target: "https://t.example", Async: true}
	a.normalize()
	if a.retries() != 8 {
		t.Fatalf("async default retries = %d", a.retries())
	}
	bad := &Webhook{ID: "z", Target: "https://user:pw@t.example"}
	if err := bad.normalize(); err == nil {
		t.Fatal("credentials in target URL must be rejected")
	}
}

func TestHostAllowed(t *testing.T) {
	pc := &ProxyConfig{AllowedHosts: []string{"api.example.com", "*.nseindia.com"}, AllowedPorts: []int{443}}
	if err := pc.normalize(); err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]bool{
		"api.example.com": true, "API.EXAMPLE.COM.": true, "x.nseindia.com": true, "a.b.nseindia.com": true,
		"nseindia.com": false, "evilnseindia.com": false, "api.example.com.evil": false,
	} {
		if got := pc.hostAllowed(host, 443); got != want {
			t.Errorf("%s: got %v", host, got)
		}
	}
	if pc.hostAllowed("api.example.com", 80) {
		t.Error("port 80 should be denied")
	}
	for _, bad := range []string{"*", "*.*.x", "a*.x", "x/y"} {
		p := &ProxyConfig{AllowedHosts: []string{bad}}
		if p.normalize() == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}
