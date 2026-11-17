package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"
	"time"
)

// SchemaBase is where the published JSON Schemas live. Files written by the
// agent point their "$schema" here so editors can validate them.
const SchemaBase = "https://raw.githubusercontent.com/skpjr001/fluid-agent/main/schema/"

// StaticConfig is read once at startup (like Traefik's static config).
// Everything that changes at runtime lives in DynamicDir (the file provider).
type StaticConfig struct {
	Schema            string          `json:"$schema,omitempty"`
	ProxyAddr         string          `json:"proxy_addr"`            // forward proxy (HTTP + CONNECT) and JSON /forward
	WebhookAddr       string          `json:"webhook_addr"`          // public webhook + callback ingress
	AdminAddr         string          `json:"admin_addr"`            // admin API + UI; keep on 127.0.0.1
	ProxyBasePath     string          `json:"proxy_base_path"`       // path prefix for /forward (e.g. "/relay")
	WebhookBasePath   string          `json:"webhook_base_path"`     // path prefix for webhooks/ingest/health
	AdminBasePath     string          `json:"admin_base_path"`       // path prefix for the UI and API
	ProxyTLS          *TLSConfig      `json:"proxy_tls,omitempty"`   // HTTPS proxy (clients use https:// proxy URLs)
	WebhookTLS        *TLSConfig      `json:"webhook_tls,omitempty"` // serve webhooks over HTTPS without a reverse proxy
	AdminTLS          *TLSConfig      `json:"admin_tls,omitempty"`   // serve the UI/API over HTTPS
	DynamicDir        string          `json:"dynamic_dir"`           // hot-reloaded JSON files
	ReloadInterval    Duration        `json:"reload_interval"`       // how often to check DynamicDir for changes
	MaxConns          int             `json:"max_conns"`             // concurrent proxy requests/tunnels
	MaxBodyBytes      int64           `json:"max_body_bytes"`        // inbound webhook/forward body cap
	TrustedProxies    []string        `json:"trusted_proxies"`       // CIDRs whose X-Forwarded-For is believed
	TunnelIdleTimeout Duration        `json:"tunnel_idle_timeout"`   // close CONNECT tunnels idle this long
	WebhookRateLimit  RateLimit       `json:"webhook_rate_limit"`    // default per-source-IP limit on ingress
	Forward           ForwardSettings `json:"forward"`
	Database          DatabaseConfig  `json:"database"`
	Outbox            OutboxConfig    `json:"outbox"`
	Telemetry         TelemetryConfig `json:"telemetry"`

	trusted []netip.Prefix
}

type ForwardSettings struct {
	MaxResponseBytes int64    `json:"max_response_bytes"`
	MaxRedirects     int      `json:"max_redirects"`
	DefaultTimeout   Duration `json:"default_timeout"`
	MaxTimeout       Duration `json:"max_timeout"`
}

type DatabaseConfig struct {
	Path             string `json:"path"`
	RetentionDays    int    `json:"retention_days"`     // logs, events and finished deliveries older than this are pruned
	LogLevel         string `json:"log_level"`          // minimum level stored in the log table
	StoreEventBodies bool   `json:"store_event_bodies"` // keep bodies after all deliveries finish (for the UI)
}

type OutboxConfig struct {
	Workers      int      `json:"workers"`
	PollInterval Duration `json:"poll_interval"`
	MaxBackoff   Duration `json:"max_backoff"`
}

type TelemetryConfig struct {
	OTLPEndpoint string            `json:"otlp_endpoint"` // e.g. "http://otel-collector:4318"; empty = tracing off
	ServiceName  string            `json:"service_name"`
	SampleRatio  float64           `json:"sample_ratio"`
	Headers      map[string]string `json:"headers,omitempty"`
}

// RateLimit is a token bucket per source IP. Zero RequestsPerMinute disables it.
type RateLimit struct {
	RequestsPerMinute int `json:"requests_per_minute"`
	Burst             int `json:"burst"`
}

// Duration marshals as a Go duration string ("2s", "5m").
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New(`duration must be a string like "30s" or "5m"`)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("bad duration %q", s)
	}
	*d = Duration(v)
	return nil
}

func defaultStatic() *StaticConfig {
	return &StaticConfig{
		ProxyAddr:         ":8080",
		WebhookAddr:       ":8081",
		AdminAddr:         "127.0.0.1:9000",
		DynamicDir:        "./dynamic",
		ReloadInterval:    Duration(2 * time.Second),
		MaxConns:          2000,
		MaxBodyBytes:      10 << 20,
		TunnelIdleTimeout: Duration(5 * time.Minute),
		WebhookRateLimit:  RateLimit{RequestsPerMinute: 600, Burst: 100},
		Forward: ForwardSettings{
			MaxResponseBytes: 10 << 20,
			MaxRedirects:     5,
			DefaultTimeout:   Duration(20 * time.Second),
			MaxTimeout:       Duration(120 * time.Second),
		},
		Database: DatabaseConfig{Path: "./data/fluid-agent.db", RetentionDays: 14, LogLevel: "info", StoreEventBodies: true},
		Outbox:   OutboxConfig{Workers: 4, PollInterval: Duration(time.Second), MaxBackoff: Duration(10 * time.Minute)},
		Telemetry: TelemetryConfig{
			ServiceName: "fluid-agent",
			SampleRatio: 1,
		},
	}
}

// decodeStrict decodes exactly one JSON value and rejects unknown fields,
// so typos in config files are errors instead of silently ignored.
func decodeStrict(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected data after the JSON object")
	}
	return nil
}

func loadStatic(path string) (*StaticConfig, error) {
	c := defaultStatic()
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := decodeStrict(bytes.NewReader(b), c); err != nil {
			return nil, fmt.Errorf("%s: %w", path, hintLegacy(err))
		}
	case !os.IsNotExist(err):
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func hintLegacy(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, `"admin_token"`):
		return fmt.Errorf("%w (admin tokens moved to <dynamic_dir>/admin.json or the PROXY_ADMIN_TOKEN env var)", err)
	case strings.Contains(msg, `"trust_forwarded_for"`):
		return fmt.Errorf("%w (use \"trusted_proxies\": [\"<proxy CIDR>\"] instead)", err)
	}
	return err
}

func (c *StaticConfig) validate() error {
	var errs []error
	for _, bp := range []struct {
		name string
		p    *string
	}{{"proxy_base_path", &c.ProxyBasePath}, {"webhook_base_path", &c.WebhookBasePath}, {"admin_base_path", &c.AdminBasePath}} {
		n, err := normalizeBasePath(*bp.p)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", bp.name, err))
		}
		*bp.p = n
	}
	for name, t := range map[string]*TLSConfig{"proxy_tls": c.ProxyTLS, "webhook_tls": c.WebhookTLS, "admin_tls": c.AdminTLS} {
		if err := t.validate(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	challenge := map[string]string{}
	for _, l := range []struct {
		name string
		t    *TLSConfig
	}{{"proxy_tls", c.ProxyTLS}, {"webhook_tls", c.WebhookTLS}, {"admin_tls", c.AdminTLS}} {
		if l.t == nil || l.t.ACME == nil || l.t.ACME.HTTPChallengeAddr == "" {
			continue
		}
		if other, dup := challenge[l.t.ACME.HTTPChallengeAddr]; dup {
			errs = append(errs, fmt.Errorf("%s and %s use the same acme.http_challenge_addr; set it on one listener only", other, l.name))
		}
		challenge[l.t.ACME.HTTPChallengeAddr] = l.name
	}
	if c.ReloadInterval <= 0 {
		errs = append(errs, errors.New("reload_interval must be > 0"))
	}
	if c.MaxConns <= 0 {
		errs = append(errs, errors.New("max_conns must be > 0"))
	}
	if c.MaxBodyBytes <= 0 {
		errs = append(errs, errors.New("max_body_bytes must be > 0"))
	}
	if c.TunnelIdleTimeout <= 0 {
		errs = append(errs, errors.New("tunnel_idle_timeout must be > 0"))
	}
	c.trusted = nil
	for _, s := range c.TrustedProxies {
		p, err := parsePrefix(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("trusted_proxies: %w", err))
			continue
		}
		c.trusted = append(c.trusted, p)
	}
	if err := c.WebhookRateLimit.validate(); err != nil {
		errs = append(errs, fmt.Errorf("webhook_rate_limit: %w", err))
	}
	f := &c.Forward
	if f.MaxResponseBytes <= 0 || f.MaxRedirects < 0 || f.DefaultTimeout <= 0 || f.MaxTimeout < f.DefaultTimeout {
		errs = append(errs, errors.New("forward: max_response_bytes > 0, max_redirects >= 0, 0 < default_timeout <= max_timeout"))
	}
	if c.Database.Path == "" {
		errs = append(errs, errors.New("database.path is required"))
	}
	if c.Database.RetentionDays < 1 {
		errs = append(errs, errors.New("database.retention_days must be >= 1"))
	}
	if _, ok := levelByName[strings.ToLower(c.Database.LogLevel)]; !ok {
		errs = append(errs, errors.New("database.log_level must be debug, info, warn or error"))
	}
	if c.Outbox.Workers < 1 || c.Outbox.PollInterval <= 0 || c.Outbox.MaxBackoff <= 0 {
		errs = append(errs, errors.New("outbox: workers >= 1, poll_interval and max_backoff > 0"))
	}
	if c.Telemetry.SampleRatio < 0 || c.Telemetry.SampleRatio > 1 {
		errs = append(errs, errors.New("telemetry.sample_ratio must be between 0 and 1"))
	}
	return errors.Join(errs...)
}

func (r RateLimit) validate() error {
	if r.RequestsPerMinute < 0 || r.Burst < 0 {
		return errors.New("values must be >= 0")
	}
	if r.RequestsPerMinute > 0 && r.Burst == 0 {
		return errors.New("burst must be > 0 when requests_per_minute is set")
	}
	return nil
}

// parsePrefix accepts "10.0.0.0/8" or a bare IP ("1.2.3.4" -> /32).
func parsePrefix(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "/") {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("bad IP or CIDR %q", s)
		}
		a = a.Unmap()
		return netip.PrefixFrom(a, a.BitLen()), nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("bad CIDR %q", s)
	}
	if p.Addr().Is4In6() {
		p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
	}
	return p.Masked(), nil
}

func parsePrefixes(list []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(list))
	for _, s := range list {
		p, err := parsePrefix(s)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func prefixesContain(ps []netip.Prefix, a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
