package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// StaticConfig is read once at startup (like Traefik's static config).
type StaticConfig struct {
	ProxyAddr         string `json:"proxy_addr"`   // forward proxy (HTTP + CONNECT)
	WebhookAddr       string `json:"webhook_addr"` // public webhook ingress
	AdminAddr         string `json:"admin_addr"`   // admin API, keep on 127.0.0.1 or behind auth
	AdminToken        string `json:"admin_token"`  // overridable via PROXY_ADMIN_TOKEN
	DynamicDir        string `json:"dynamic_dir"`  // hot-reloaded JSON files live here
	ReloadInterval    string `json:"reload_interval"`
	TrustForwardedFor bool   `json:"trust_forwarded_for"` // true when behind Traefik/Nginx
	MaxConns          int    `json:"max_conns"`
	MaxBodyBytes      int64  `json:"max_body_bytes"`

	reloadEvery time.Duration
}

func loadStatic(path string) (*StaticConfig, error) {
	c := &StaticConfig{
		ProxyAddr:      ":8080",
		WebhookAddr:    ":8081",
		AdminAddr:      "127.0.0.1:9000",
		DynamicDir:     "./dynamic",
		ReloadInterval: "2s",
		MaxConns:       2000,
		MaxBodyBytes:   10 << 20,
	}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if v := os.Getenv("PROXY_ADMIN_TOKEN"); v != "" {
		c.AdminToken = v
	}
	if len(c.AdminToken) < 16 {
		return nil, fmt.Errorf("admin_token must be set (>=16 chars) in config or PROXY_ADMIN_TOKEN")
	}
	d, err := time.ParseDuration(c.ReloadInterval)
	if err != nil || d <= 0 {
		return nil, fmt.Errorf("bad reload_interval %q", c.ReloadInterval)
	}
	c.reloadEvery = d
	return c, nil
}
