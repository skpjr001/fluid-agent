package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

// First-run bootstrap: create every directory and config file the agent
// needs, so a fresh install (host, systemd, empty container) starts with no
// manual steps. Existing files are never overwritten. Anything that cannot be
// created is reported as a warning and the agent runs on built-in defaults.

// BootstrapItem is one thing bootstrap created or could not create.
type BootstrapItem struct {
	Level   string // "info" (created) or "warn" (could not create)
	Path    string
	Message string
}

// bootstrap creates what is missing. cfgExisted tells whether cfgPath was
// present before loadStatic fell back to defaults.
func bootstrap(cfgPath string, cfg *StaticConfig, cfgExisted, envAdminToken bool) []BootstrapItem {
	var out []BootstrapItem
	created := func(path, what, suffix string) {
		path = absPath(path)
		out = append(out, BootstrapItem{"info", path, "created " + what + " " + path + suffix})
	}
	failed := func(path, what string, err error) {
		path = absPath(path)
		out = append(out, BootstrapItem{"warn", path, fmt.Sprintf("could not create %s %s: %v", what, path, err)})
	}
	mkdir := func(dir string, perm fs.FileMode, what string) bool {
		dir = filepath.Clean(dir)
		if dir == "." {
			return true
		}
		if fi, err := os.Stat(dir); err == nil {
			if !fi.IsDir() {
				failed(dir, what, errors.New("a file with that name exists"))
				return false
			}
			return true
		}
		if err := os.MkdirAll(dir, perm); err != nil {
			failed(dir, what, err)
			return false
		}
		created(dir, what, "")
		return true
	}
	writeNew := func(path, what string, v any, perm fs.FileMode) {
		if _, err := os.Lstat(path); err == nil || !errors.Is(err, fs.ErrNotExist) {
			return // exists (or unreadable: leave it for the loader to report)
		}
		b, err := json.MarshalIndent(v, "", "  ")
		if err == nil {
			err = writeAtomicMode(path, b, perm)
		}
		if err != nil {
			failed(path, what, err)
			return
		}
		created(path, what, " with safe defaults")
	}

	// config.json
	if !cfgExisted {
		if mkdir(filepath.Dir(cfgPath), 0o750, "config directory") {
			writeNew(cfgPath, "static config", defaultConfigFile(cfg), 0o640)
		}
	}

	// database directory
	mkdir(filepath.Dir(cfg.Database.Path), 0o750, "database directory")

	// ACME certificate caches
	acmeDefault := filepath.Join(filepath.Dir(cfg.Database.Path), "acme")
	for _, t := range []*TLSConfig{cfg.ProxyTLS, cfg.WebhookTLS, cfg.AdminTLS} {
		if t != nil && t.ACME != nil {
			dir := t.ACME.CacheDir
			if dir == "" {
				dir = acmeDefault
			}
			mkdir(dir, 0o700, "ACME certificate cache")
		}
	}

	// dynamic dir, its sub-directories and the two singleton files
	if !mkdir(cfg.DynamicDir, 0o750, "dynamic config directory") {
		return out
	}
	for _, sub := range dynamicSubdirs {
		mkdir(filepath.Join(cfg.DynamicDir, sub), 0o750, "dynamic config directory")
	}
	writeNew(filepath.Join(cfg.DynamicDir, "proxy.json"), "proxy config", &ProxyConfig{
		Schema: SchemaBase + "proxy.schema.json", Clients: []Client{}, AllowedHosts: []string{}, AllowedPorts: []int{443},
	}, 0o640)
	adminPath := filepath.Join(cfg.DynamicDir, "admin.json")
	admin := &AdminConfig{Schema: SchemaBase + "admin.schema.json", Tokens: []AdminToken{}}
	if !envAdminToken {
		admin.Tokens = append(admin.Tokens, AdminToken{Name: "admin", Token: randomToken(), Role: RoleEditor})
	}
	before := len(out)
	writeNew(adminPath, "admin config", admin, 0o600)
	if len(out) > before && out[len(out)-1].Level == "info" && !envAdminToken {
		out[len(out)-1].Message += `; it holds a generated editor token named "admin": read it from that file to sign in to the console`
	}
	return out
}

// defaultConfigFile is the config.json written on first run: every setting
// spelled out with its default, so it is easy to edit.
func defaultConfigFile(cfg *StaticConfig) *StaticConfig {
	c := *cfg
	c.Schema = SchemaBase + "config.schema.json"
	if c.TrustedProxies == nil {
		c.TrustedProxies = []string{}
	}
	return &c
}

// absPath makes log messages unambiguous ("./dynamic" says little in a journal).
func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func logBootstrap(items []BootstrapItem) {
	for _, it := range items {
		level := slog.LevelInfo
		if it.Level == "warn" {
			level = slog.LevelWarn
		}
		slog.Log(context.Background(), level, it.Message, "kind", "deploy", "topic", "bootstrap", "path", it.Path)
	}
}

// pathPresent reports whether path exists (a directory counts, so loadStatic
// can report it instead of bootstrap writing next to it).
func pathPresent(path string) bool {
	_, err := os.Stat(path)
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}

// cmdInit creates missing directories and config files, then validates them.
func cmdInit(args []string, out *os.File) error {
	fset := flag.NewFlagSet("init", flag.ExitOnError)
	cfgPath := fset.String("config", "config.json", "static config file")
	fset.Parse(args)
	existed := pathPresent(*cfgPath)
	cfg, err := loadStatic(*cfgPath)
	if err != nil {
		return err
	}
	items := bootstrap(*cfgPath, cfg, existed, len(os.Getenv("PROXY_ADMIN_TOKEN")) >= 16)
	warns := 0
	for _, it := range items {
		fmt.Fprintf(out, "%-5s %s\n", map[string]string{"info": "OK", "warn": "WARN"}[it.Level], it.Message)
		if it.Level == "warn" {
			warns++
		}
	}
	if len(items) == 0 {
		fmt.Fprintln(out, "nothing to create: every directory and config file exists")
	}
	if problems := checkConfig(*cfgPath); len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(out, p)
		}
		return fmt.Errorf("%d invalid file(s)", len(problems))
	}
	if warns > 0 {
		return fmt.Errorf("%d item(s) could not be created", warns)
	}
	return nil
}
