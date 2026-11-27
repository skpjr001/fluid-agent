package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/skpjr001/fluid-agent/internal/config"
)

func freshLayout(t *testing.T) (cfgPath string, cfg *config.StaticConfig) {
	t.Helper()
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "etc", "config.json")
	cfg = config.Default()
	cfg.DynamicDir = filepath.Join(dir, "dynamic")
	cfg.Database.Path = filepath.Join(dir, "var", "data", "fluid.db")
	return cfgPath, cfg
}

func messages(items []BootstrapItem) string {
	var b strings.Builder
	for _, it := range items {
		b.WriteString(it.Level + " " + it.Message + "\n")
	}
	return b.String()
}

func TestBootstrapFreshInstall(t *testing.T) {
	cfgPath, cfg := freshLayout(t)
	items := bootstrap(cfgPath, cfg, false, false)
	for _, it := range items {
		if it.Level != "info" {
			t.Fatalf("unexpected warning: %s", it.Message)
		}
	}
	for _, d := range []string{filepath.Dir(cfgPath), filepath.Dir(cfg.Database.Path), cfg.DynamicDir,
		filepath.Join(cfg.DynamicDir, "webhooks"), filepath.Join(cfg.DynamicDir, "profiles"), filepath.Join(cfg.DynamicDir, "ingest")} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Errorf("directory %s not created: %v", d, err)
		}
	}

	// config.json: strict-decodes back to the same settings.
	got, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("written config.json does not load: %v", err)
	}
	if got.Schema != config.SchemaBase+"config.schema.json" || got.DynamicDir != cfg.DynamicDir || got.Database != cfg.Database ||
		got.ProxyAddr != cfg.ProxyAddr || got.ReloadInterval != cfg.ReloadInterval || !reflect.DeepEqual(got.Outbox, cfg.Outbox) {
		t.Errorf("config.json round trip differs: %+v", got)
	}
	if fi, _ := os.Stat(cfgPath); fi.Mode().Perm() != 0o640 {
		t.Errorf("config.json mode %v", fi.Mode().Perm())
	}

	// dynamic files: valid, deny-by-default proxy, one generated editor token.
	snap := loadDir(cfg.DynamicDir, nil)
	if len(snap.errors) != 0 {
		t.Fatalf("written dynamic files invalid: %v", snap.errors)
	}
	if len(snap.proxy.Clients) != 0 || len(snap.proxy.AllowedHosts) != 0 || !reflect.DeepEqual(snap.proxy.AllowedPorts, []int{443}) {
		t.Errorf("proxy.json not deny-by-default: %+v", snap.proxy)
	}
	if len(snap.admin.Tokens) != 1 || snap.admin.Tokens[0].Role != RoleEditor || len(snap.admin.Tokens[0].Token) != 64 {
		t.Fatalf("admin.json tokens: %+v", snap.admin.Tokens)
	}
	if fi, _ := os.Stat(filepath.Join(cfg.DynamicDir, "admin.json")); fi.Mode().Perm() != 0o600 {
		t.Errorf("admin.json mode %v, want 0600", fi.Mode().Perm())
	}
	msgs := messages(items)
	if strings.Contains(msgs, snap.admin.Tokens[0].Token) {
		t.Error("the generated token must never be logged")
	}
	if !strings.Contains(msgs, `generated editor token named "admin"`) {
		t.Errorf("token location not reported:\n%s", msgs)
	}

	// The store and the whole app start on the result.
	if _, err := NewStore(cfg.DynamicDir); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapIdempotentAndNeverOverwrites(t *testing.T) {
	cfgPath, cfg := freshLayout(t)
	bootstrap(cfgPath, cfg, false, false)
	adminPath := filepath.Join(cfg.DynamicDir, "admin.json")
	before, _ := os.ReadFile(adminPath)

	if items := bootstrap(cfgPath, cfg, true, false); len(items) != 0 {
		t.Fatalf("second run created things:\n%s", messages(items))
	}
	if after, _ := os.ReadFile(adminPath); string(after) != string(before) {
		t.Fatal("admin.json was rewritten")
	}

	// A hand-written config file is left alone even if loadStatic was told it was missing.
	os.WriteFile(cfgPath, []byte(`{"proxy_addr":":1"}`), 0o600)
	bootstrap(cfgPath, cfg, false, false)
	if b, _ := os.ReadFile(cfgPath); string(b) != `{"proxy_addr":":1"}` {
		t.Fatalf("config.json overwritten: %s", b)
	}
}

func TestBootstrapEnvTokenSkipsGeneratedToken(t *testing.T) {
	cfgPath, cfg := freshLayout(t)
	items := bootstrap(cfgPath, cfg, false, true)
	snap := loadDir(cfg.DynamicDir, nil)
	if len(snap.errors) != 0 || len(snap.admin.Tokens) != 0 {
		t.Fatalf("with PROXY_ADMIN_TOKEN admin.json should have no tokens: %+v %v", snap.admin, snap.errors)
	}
	if strings.Contains(messages(items), "generated editor token") {
		t.Error("no token was generated, the message must not say so")
	}
}

func TestBootstrapACMECache(t *testing.T) {
	cfgPath, cfg := freshLayout(t)
	custom := filepath.Join(t.TempDir(), "certs", "acme")
	cfg.WebhookTLS = &config.TLSConfig{ACME: &config.ACMEConfig{Domains: []string{"a.example"}, AcceptTOS: true}}
	cfg.AdminTLS = &config.TLSConfig{ACME: &config.ACMEConfig{Domains: []string{"b.example"}, AcceptTOS: true, CacheDir: custom}}
	bootstrap(cfgPath, cfg, false, false)
	for _, d := range []string{filepath.Join(filepath.Dir(cfg.Database.Path), "acme"), custom} {
		fi, err := os.Stat(d)
		if err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("ACME cache %s: %v %v", d, fi, err)
		}
	}
}

func TestBootstrapReadOnlyWarnsAndAgentStillStarts(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	cfgPath, cfg := freshLayout(t)
	ro := filepath.Dir(cfg.DynamicDir)
	cfg.DynamicDir = filepath.Join(ro, "locked", "dynamic")
	os.MkdirAll(filepath.Dir(cfg.DynamicDir), 0o750)
	os.Chmod(filepath.Dir(cfg.DynamicDir), 0o500)
	t.Cleanup(func() { os.Chmod(filepath.Dir(cfg.DynamicDir), 0o750) })

	items := bootstrap(cfgPath, cfg, false, false)
	msgs := messages(items)
	if !strings.Contains(msgs, "warn could not create dynamic config directory "+absPath(cfg.DynamicDir)) {
		t.Fatalf("missing warning:\n%s", msgs)
	}
	// config.json and the database dir were still created.
	if _, err := os.Stat(cfgPath); err != nil {
		t.Error(err)
	}
	// The store runs on defaults instead of failing.
	s, err := NewStore(cfg.DynamicDir)
	if err != nil {
		t.Fatalf("store must start on defaults: %v", err)
	}
	if len(s.Proxy().Clients) != 0 {
		t.Error("expected default proxy config")
	}
}

func TestBootstrapReadOnlyMountWithExistingFiles(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	cfgPath, cfg := freshLayout(t)
	bootstrap(cfgPath, cfg, false, false)
	os.Remove(filepath.Join(cfg.DynamicDir, "profiles"))
	os.Chmod(cfg.DynamicDir, 0o500) // like a :ro container mount
	t.Cleanup(func() { os.Chmod(cfg.DynamicDir, 0o750) })
	if _, err := NewStore(cfg.DynamicDir); err != nil {
		t.Fatalf("read-only dynamic dir with a missing sub-directory must still load: %v", err)
	}
}

func TestConfigPathIsDirectory(t *testing.T) {
	t.Chdir(t.TempDir()) // bootstrap creates the default ./dynamic
	dir := filepath.Join(t.TempDir(), "config.json")
	os.Mkdir(dir, 0o750)
	_, err := config.Load(dir)
	if err == nil || !strings.Contains(err.Error(), "is a directory, not a file") {
		t.Fatalf("want a bind-mount hint, got %v", err)
	}
	if items := bootstrap(dir, config.Default(), pathPresent(dir), false); strings.Contains(messages(items), "static config") {
		t.Error("bootstrap must not touch a config path that exists as a directory")
	}
}

func TestCheckAcceptsMissingDynamicDir(t *testing.T) {
	cfgPath, cfg := freshLayout(t)
	os.MkdirAll(filepath.Dir(cfgPath), 0o750)
	os.WriteFile(cfgPath, []byte(`{"dynamic_dir":"`+cfg.DynamicDir+`"}`), 0o640)
	if p := checkConfig(cfgPath); len(p) != 0 {
		t.Fatalf("a not-yet-created dynamic dir is not a problem: %v", p)
	}
	for _, n := range deploymentNotes(defaultStaticWith(cfg.DynamicDir), loadDir(cfg.DynamicDir, nil), hostEnv{}, false, timeNow()) {
		if strings.Contains(n.Message, "read-only") {
			t.Errorf("missing dir reported as read-only: %s", n.Message)
		}
	}
}

func TestCmdInit(t *testing.T) {
	cfgPath, _ := freshLayout(t)
	dir := filepath.Dir(filepath.Dir(cfgPath))
	t.Chdir(dir)
	out, _ := os.Create(filepath.Join(t.TempDir(), "out"))
	defer out.Close()
	if err := cmdInit([]string{"-config", "etc/config.json"}, out); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"etc/config.json", "dynamic/proxy.json", "dynamic/admin.json", "dynamic/ingest", "data"} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if err := cmdInit([]string{"-config", "etc/config.json"}, out); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out.Name())
	if !strings.Contains(string(b), "nothing to create") {
		t.Errorf("second init output:\n%s", b)
	}
}

func defaultStaticWith(dynamicDir string) *config.StaticConfig {
	c := config.Default()
	c.DynamicDir = dynamicDir
	return c
}

func timeNow() time.Time { return time.Now() }
