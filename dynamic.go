package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

var headerNameRe = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

// ---------- proxy.json ----------

// ProxyConfig lives in <dynamic_dir>/proxy.json.
type ProxyConfig struct {
	Schema           string   `json:"$schema,omitempty"`
	Clients          []Client `json:"clients"`
	AllowedHosts     []string `json:"allowed_hosts"`               // "api.example.com", "*.example.com"
	AllowedPorts     []int    `json:"allowed_ports,omitempty"`     // default [443]
	AllowPrivate     bool     `json:"allow_private,omitempty"`     // allow private/loopback/metadata destinations
	PrivateAllowlist []string `json:"private_allowlist,omitempty"` // specific private CIDRs that are allowed

	privateOK []netip.Prefix
}

// Client is a named credential for the proxy and /forward.
type Client struct {
	Name     string `json:"name"`
	Token    string `json:"token"`
	Disabled bool   `json:"disabled,omitempty"`
}

func (p *ProxyConfig) normalize() error {
	var errs []error
	seen := map[string]bool{}
	for i, c := range p.Clients {
		if !idRe.MatchString(c.Name) {
			errs = append(errs, fmt.Errorf("clients[%d].name must match %s", i, idRe))
		}
		if seen[c.Name] {
			errs = append(errs, fmt.Errorf("duplicate client %q", c.Name))
		}
		seen[c.Name] = true
		if len(c.Token) < 16 {
			errs = append(errs, fmt.Errorf("client %q: token must be at least 16 characters", c.Name))
		}
	}
	for i, h := range p.AllowedHosts {
		h = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
		if h == "" || h == "*" || strings.Contains(h, "/") || (strings.Contains(h, "*") && !strings.HasPrefix(h, "*.")) || strings.Count(h, "*") > 1 {
			errs = append(errs, fmt.Errorf("allowed_hosts[%d] %q: use an exact host or *.domain", i, h))
		}
		p.AllowedHosts[i] = h
	}
	if len(p.AllowedPorts) == 0 {
		p.AllowedPorts = []int{443}
	}
	for _, port := range p.AllowedPorts {
		if port < 1 || port > 65535 {
			errs = append(errs, fmt.Errorf("allowed_ports: %d out of range", port))
		}
	}
	ps, err := parsePrefixes(p.PrivateAllowlist)
	if err != nil {
		errs = append(errs, fmt.Errorf("private_allowlist: %w", err))
	}
	p.privateOK = ps
	return errors.Join(errs...)
}

// hostAllowed checks the destination against allowed_hosts and allowed_ports.
func (p *ProxyConfig) hostAllowed(host string, port int) bool {
	if !slices.Contains(p.AllowedPorts, port) {
		return false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	for _, a := range p.AllowedHosts {
		if a == host {
			return true
		}
		if strings.HasPrefix(a, "*.") && strings.HasSuffix(host, a[1:]) && len(host) > len(a)-1 {
			return true
		}
	}
	return false
}

// ---------- admin.json ----------

type Role string

const (
	RoleViewer Role = "viewer"
	RoleEditor Role = "editor"
)

// AdminConfig lives in <dynamic_dir>/admin.json. It is never editable over
// the API, so a viewer token can never be turned into an editor token.
type AdminConfig struct {
	Schema string       `json:"$schema,omitempty"`
	Tokens []AdminToken `json:"tokens"`
}

type AdminToken struct {
	Name  string `json:"name"`
	Token string `json:"token"`
	Role  Role   `json:"role"`
}

func (a *AdminConfig) normalize() error {
	var errs []error
	seen := map[string]bool{}
	for i, t := range a.Tokens {
		if !idRe.MatchString(t.Name) {
			errs = append(errs, fmt.Errorf("tokens[%d].name must match %s", i, idRe))
		}
		if seen[t.Name] {
			errs = append(errs, fmt.Errorf("duplicate token name %q", t.Name))
		}
		seen[t.Name] = true
		if len(t.Token) < 16 {
			errs = append(errs, fmt.Errorf("token %q must be at least 16 characters", t.Name))
		}
		if t.Role != RoleViewer && t.Role != RoleEditor {
			errs = append(errs, fmt.Errorf("token %q: role must be \"viewer\" or \"editor\"", t.Name))
		}
	}
	return errors.Join(errs...)
}

// ---------- shared inbound route options ----------

// RouteOptions are shared by webhooks and ingest rules.
type RouteOptions struct {
	Methods     []string          `json:"methods,omitempty"`      // default ["POST"]; "*" = any
	SourceCIDRs []string          `json:"source_cidrs,omitempty"` // allowed sender IPs
	Auth        *InboundAuth      `json:"auth,omitempty"`
	RateLimit   *RateLimit        `json:"rate_limit,omitempty"`  // overrides the global per-IP limit
	AddHeaders  map[string]string `json:"add_headers,omitempty"` // set on relayed requests
	TargetAuth  *TargetAuth       `json:"target_auth,omitempty"` // credentials sent to the destination(s)
	MaxRetries  *int              `json:"max_retries,omitempty"` // default 0 (sync) or 8 (async)
	TimeoutSec  int               `json:"timeout_sec,omitempty"` // per attempt, default 15

	nets []netip.Prefix
}

// InboundAuth is optional; when set, every configured check must pass.
type InboundAuth struct {
	Token *TokenAuth `json:"token,omitempty"`
	Basic *BasicAuth `json:"basic,omitempty"`
	HMAC  *HMACAuth  `json:"hmac,omitempty"`
}

// BasicAuth requires HTTP Basic credentials from the sender.
type BasicAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// TargetAuth is added to every relayed request so the destination can
// verify that it came through the agent.
type TargetAuth struct {
	Type     string `json:"type"`               // bearer, basic, header
	Token    string `json:"token,omitempty"`    // bearer
	Username string `json:"username,omitempty"` // basic
	Password string `json:"password,omitempty"` // basic
	Header   string `json:"header,omitempty"`   // header
	Value    string `json:"value,omitempty"`    // header
}

func (t *TargetAuth) validate() error {
	switch t.Type {
	case "bearer":
		if t.Token == "" {
			return errors.New("target_auth: bearer needs token")
		}
	case "basic":
		if t.Username == "" {
			return errors.New("target_auth: basic needs username")
		}
	case "header":
		if !headerNameRe.MatchString(t.Header) || t.Value == "" {
			return errors.New("target_auth: header needs a valid header and value")
		}
	default:
		return errors.New("target_auth.type must be bearer, basic or header")
	}
	return nil
}

// apply sets the credentials on h.
func (t *TargetAuth) apply(h http.Header) {
	if t == nil {
		return
	}
	switch t.Type {
	case "bearer":
		h.Set("Authorization", "Bearer "+t.Token)
	case "basic":
		h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(t.Username+":"+t.Password)))
	case "header":
		h.Set(t.Header, t.Value)
	}
}

// TokenAuth requires Header to equal Value (constant-time compare).
type TokenAuth struct {
	Header string `json:"header"`
	Value  string `json:"value"`
}

// HMACAuth verifies a signature of the raw body, e.g. Razorpay's
// X-Razorpay-Signature (sha256, hex) or GitHub's X-Hub-Signature-256 (prefix "sha256=").
type HMACAuth struct {
	Header    string `json:"header"`
	Secret    string `json:"secret"`
	Algorithm string `json:"algorithm,omitempty"` // sha256 (default), sha1, sha512
	Encoding  string `json:"encoding,omitempty"`  // hex (default), base64
	Prefix    string `json:"prefix,omitempty"`    // stripped from the header value, e.g. "sha256="
}

func (o *RouteOptions) normalize(async bool) error {
	var errs []error
	if len(o.Methods) == 0 {
		o.Methods = []string{http.MethodPost}
	}
	for i, m := range o.Methods {
		o.Methods[i] = strings.ToUpper(strings.TrimSpace(m))
		if o.Methods[i] != "*" && !headerNameRe.MatchString(o.Methods[i]) {
			errs = append(errs, fmt.Errorf("methods[%d] %q is not a valid method", i, m))
		}
	}
	nets, err := parsePrefixes(o.SourceCIDRs)
	if err != nil {
		errs = append(errs, fmt.Errorf("source_cidrs: %w", err))
	}
	o.nets = nets
	if o.Auth != nil {
		if o.Auth.Token == nil && o.Auth.HMAC == nil && o.Auth.Basic == nil {
			errs = append(errs, errors.New("auth: set token, basic and/or hmac, or remove auth"))
		}
		if b := o.Auth.Basic; b != nil && (b.Username == "" || len(b.Password) < 8) {
			errs = append(errs, errors.New("auth.basic: username required and password at least 8 characters"))
		}
		if t := o.Auth.Token; t != nil {
			if !headerNameRe.MatchString(t.Header) || len(t.Value) < 8 {
				errs = append(errs, errors.New("auth.token: header must be a valid name and value at least 8 characters"))
			}
		}
		if h := o.Auth.HMAC; h != nil {
			if h.Algorithm == "" {
				h.Algorithm = "sha256"
			}
			if h.Encoding == "" {
				h.Encoding = "hex"
			}
			if !headerNameRe.MatchString(h.Header) || len(h.Secret) < 8 {
				errs = append(errs, errors.New("auth.hmac: header must be a valid name and secret at least 8 characters"))
			}
			if !slices.Contains([]string{"sha1", "sha256", "sha512"}, h.Algorithm) {
				errs = append(errs, errors.New("auth.hmac.algorithm must be sha1, sha256 or sha512"))
			}
			if h.Encoding != "hex" && h.Encoding != "base64" {
				errs = append(errs, errors.New("auth.hmac.encoding must be hex or base64"))
			}
		}
	}
	if o.TargetAuth != nil {
		if err := o.TargetAuth.validate(); err != nil {
			errs = append(errs, err)
		}
	}
	if o.RateLimit != nil {
		if err := o.RateLimit.validate(); err != nil {
			errs = append(errs, fmt.Errorf("rate_limit: %w", err))
		}
	}
	for k := range o.AddHeaders {
		if !headerNameRe.MatchString(k) {
			errs = append(errs, fmt.Errorf("add_headers: %q is not a valid header name", k))
		}
	}
	if o.MaxRetries == nil {
		n := 0
		if async {
			n = 8
		}
		o.MaxRetries = &n
	}
	if *o.MaxRetries < 0 || *o.MaxRetries > 20 {
		errs = append(errs, errors.New("max_retries must be 0..20"))
	}
	if o.TimeoutSec == 0 {
		o.TimeoutSec = 15
	}
	if o.TimeoutSec < 1 || o.TimeoutSec > 300 {
		errs = append(errs, errors.New("timeout_sec must be 1..300"))
	}
	return errors.Join(errs...)
}

func (o *RouteOptions) allowsMethod(m string) bool {
	return slices.Contains(o.Methods, "*") || slices.Contains(o.Methods, m)
}

func (o *RouteOptions) allowsIP(a netip.Addr) bool {
	return len(o.nets) == 0 || (a.IsValid() && prefixesContain(o.nets, a))
}

func (o *RouteOptions) retries() int { return *o.MaxRetries }

func validTargetURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return fmt.Errorf("%q must be an absolute http(s) URL without credentials", s)
	}
	return nil
}

// ---------- webhooks/<id>.json ----------

// Webhook relays /hooks/{id}[/sub/path] to Target (and Mirrors).
type Webhook struct {
	Schema      string   `json:"$schema,omitempty"`
	ID          string   `json:"id"`
	Description string   `json:"description,omitempty"`
	Disabled    bool     `json:"disabled,omitempty"`
	Target      string   `json:"target"`            // primary destination
	Mirrors     []string `json:"mirrors,omitempty"` // extra destinations, always delivered durably in the background
	Async       bool     `json:"async,omitempty"`   // true: reply 202 once stored, deliver in the background
	RouteOptions
	CreatedAt time.Time `json:"created_at,omitzero"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

func (w *Webhook) normalize() error {
	var errs []error
	if !idRe.MatchString(w.ID) {
		errs = append(errs, fmt.Errorf("id must match %s", idRe))
	}
	if err := validTargetURL(w.Target); err != nil {
		errs = append(errs, fmt.Errorf("target: %w", err))
	}
	for i, m := range w.Mirrors {
		if err := validTargetURL(m); err != nil {
			errs = append(errs, fmt.Errorf("mirrors[%d]: %w", i, err))
		}
	}
	if err := w.RouteOptions.normalize(w.Async); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// ---------- ingest/<id>.json ----------

// IngestRule matches callbacks on any path of the webhook listener (outside
// /hooks/) and stores them durably before acknowledging, then fans them out.
type IngestRule struct {
	Schema      string       `json:"$schema,omitempty"`
	ID          string       `json:"id"`
	Description string       `json:"description,omitempty"`
	Disabled    bool         `json:"disabled,omitempty"`
	Priority    int          `json:"priority,omitempty"` // higher wins; ties broken by id
	Match       PathMatch    `json:"match"`
	Vendor      string       `json:"vendor,omitempty"`     // label, e.g. "NSE"
	EventType   string       `json:"event_type,omitempty"` // label, e.g. "APP_PAY_STATUS"
	Targets     []string     `json:"targets"`
	Response    *AckResponse `json:"response,omitempty"` // reply sent to the caller once stored
	RouteOptions
	CreatedAt time.Time `json:"created_at,omitzero"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`

	re *regexp.Regexp
}

type PathMatch struct {
	Type  string `json:"type"` // exact, prefix, suffix, regex
	Value string `json:"value"`
}

type AckResponse struct {
	Status      int    `json:"status,omitempty"`       // default 200
	ContentType string `json:"content_type,omitempty"` // default application/json
	Body        string `json:"body,omitempty"`         // default {"status":"success"}
}

func (r *IngestRule) normalize() error {
	var errs []error
	if !idRe.MatchString(r.ID) {
		errs = append(errs, fmt.Errorf("id must match %s", idRe))
	}
	switch r.Match.Type {
	case "exact", "prefix", "suffix":
		if r.Match.Value == "" {
			errs = append(errs, errors.New("match.value is required"))
		}
	case "regex":
		re, err := regexp.Compile(r.Match.Value)
		if err != nil {
			errs = append(errs, fmt.Errorf("match.value: %w", err))
		}
		r.re = re
	default:
		errs = append(errs, errors.New("match.type must be exact, prefix, suffix or regex"))
	}
	if len(r.Targets) == 0 {
		errs = append(errs, errors.New("targets: at least one is required"))
	}
	for i, t := range r.Targets {
		if err := validTargetURL(t); err != nil {
			errs = append(errs, fmt.Errorf("targets[%d]: %w", i, err))
		}
	}
	if r.Response == nil {
		r.Response = &AckResponse{}
	}
	if r.Response.Status == 0 {
		r.Response.Status = http.StatusOK
	}
	if r.Response.Status < 200 || r.Response.Status > 299 {
		errs = append(errs, errors.New("response.status must be 2xx"))
	}
	if r.Response.ContentType == "" {
		r.Response.ContentType = "application/json"
	}
	if r.Response.Body == "" {
		r.Response.Body = `{"status":"success"}`
	}
	if err := r.RouteOptions.normalize(true); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (r *IngestRule) matches(path string) bool {
	switch r.Match.Type {
	case "exact":
		return path == r.Match.Value
	case "prefix":
		return strings.HasPrefix(path, r.Match.Value)
	case "suffix":
		return strings.HasSuffix(path, r.Match.Value)
	case "regex":
		return r.re != nil && r.re.MatchString(path)
	}
	return false
}

// ---------- profiles/<name>.json ----------

// Profile is a named upstream for /forward. The agent adds Headers (usually
// provider credentials), so clients never hold them.
type Profile struct {
	Schema         string            `json:"$schema,omitempty"`
	Name           string            `json:"name"`
	Description    string            `json:"description,omitempty"`
	BaseURL        string            `json:"base_url"`
	Headers        map[string]string `json:"headers,omitempty"`
	AllowedMethods []string          `json:"allowed_methods,omitempty"` // default: any
	Clients        []string          `json:"clients,omitempty"`         // client names allowed; empty = all
	TimeoutSec     int               `json:"timeout_sec,omitempty"`
	CreatedAt      time.Time         `json:"created_at,omitzero"`
	UpdatedAt      time.Time         `json:"updated_at,omitzero"`
}

func (p *Profile) normalize() error {
	var errs []error
	if !idRe.MatchString(p.Name) {
		errs = append(errs, fmt.Errorf("name must match %s", idRe))
	}
	if err := validTargetURL(p.BaseURL); err != nil {
		errs = append(errs, fmt.Errorf("base_url: %w", err))
	}
	for k := range p.Headers {
		if !headerNameRe.MatchString(k) {
			errs = append(errs, fmt.Errorf("headers: %q is not a valid header name", k))
		}
	}
	for i, m := range p.AllowedMethods {
		p.AllowedMethods[i] = strings.ToUpper(m)
	}
	if p.TimeoutSec < 0 || p.TimeoutSec > 300 {
		errs = append(errs, errors.New("timeout_sec must be 0..300"))
	}
	return errors.Join(errs...)
}

func (p *Profile) allows(client, method string) bool {
	if len(p.AllowedMethods) > 0 && !slices.Contains(p.AllowedMethods, method) {
		return false
	}
	return len(p.Clients) == 0 || slices.Contains(p.Clients, client)
}
