package main

import (
	"bufio"
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DeployNote is one piece of deployment advice, logged at startup with the
// matching level and shown in `fluid-agent check` and the console.
type DeployNote struct {
	Level   string `json:"level"` // info | warn | error
	Topic   string `json:"topic"` // runtime, proxy, webhook, admin, tls, storage, config
	Message string `json:"message"`
}

// hostEnv describes where the agent runs; detectEnv fills it, tests fake it.
type hostEnv struct {
	Container string   // "", docker, podman, kubernetes, container
	Systemd   bool     // started by systemd
	EUID      int      // effective user id
	Mounts    []string // mount points (from /proc/self/mountinfo)
}

func detectEnv() hostEnv {
	e := hostEnv{EUID: os.Geteuid(), Systemd: os.Getenv("INVOCATION_ID") != ""}
	switch {
	case os.Getenv("KUBERNETES_SERVICE_HOST") != "":
		e.Container = "kubernetes"
	case fileExists("/run/.containerenv"):
		e.Container = "podman"
	case fileExists("/.dockerenv"):
		e.Container = "docker"
	case os.Getenv("container") != "":
		e.Container = os.Getenv("container")
	}
	if f, err := os.Open("/proc/self/mountinfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if fs := strings.Fields(sc.Text()); len(fs) > 4 {
				e.Mounts = append(e.Mounts, fs[4])
			}
		}
		f.Close()
	}
	return e
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// onMount reports whether path lives on a mount other than the root filesystem.
func (e hostEnv) onMount(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	for _, m := range e.Mounts {
		if m != "/" && (abs == m || strings.HasPrefix(abs, strings.TrimSuffix(m, "/")+"/")) {
			return true
		}
	}
	return false
}

func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".write-test-*")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}

type notes []DeployNote

func (n *notes) add(level, topic, format string, a ...any) {
	*n = append(*n, DeployNote{Level: level, Topic: topic, Message: fmt.Sprintf(format, a...)})
}

// listenScope classifies a listen address.
func listenScope(addr string) (host string, port int, loopback bool) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, 0, false
	}
	port, _ = strconv.Atoi(p)
	if h == "localhost" {
		return h, port, true
	}
	if a, err := netip.ParseAddr(h); err == nil {
		return h, port, a.IsLoopback()
	}
	return h, port, false
}

// deploymentNotes inspects the configuration and environment and returns
// every caveat that applies to this way of running the agent.
func deploymentNotes(cfg *StaticConfig, snap *snapshot, env hostEnv, envAdminToken bool, now time.Time) []DeployNote {
	var n notes

	// ---- runtime ----
	switch {
	case env.Container != "":
		n.add("info", "runtime", "running in a %s container", env.Container)
		n.add("info", "runtime", "outbound calls (proxy, /forward, deliveries) leave through the host's network (NAT): the host's public IP is the one providers must allowlist")
		if !env.onMount(filepath.Dir(cfg.Database.Path)) {
			n.add("warn", "storage", "database %s is not on a mounted volume: logs, events and pending deliveries are lost when the container is removed; mount a volume at %s",
				cfg.Database.Path, filepath.Dir(cfg.Database.Path))
		}
		if !env.onMount(cfg.DynamicDir) {
			n.add("warn", "config", "dynamic config dir %s is not mounted from the host: changes made in the console are lost when the container is removed", cfg.DynamicDir)
		}
	case env.Systemd:
		n.add("info", "runtime", "running on the host under systemd")
	default:
		n.add("info", "runtime", "running on the host (not under systemd or a container)")
	}
	if env.Container == "" && env.EUID == 0 {
		n.add("warn", "runtime", "running as root: use a dedicated user (see deploy/systemd/fluid-agent.service)")
	}
	if fileExists(cfg.DynamicDir) && !dirWritable(cfg.DynamicDir) {
		n.add("warn", "config", "dynamic config dir %s is read-only: editors cannot save changes from the console or API; edit the files directly", cfg.DynamicDir)
	}

	// ---- listeners ----
	type lst struct {
		name, addr, base string
		tls              *TLSConfig
	}
	for _, l := range []lst{
		{"proxy", cfg.ProxyAddr, cfg.ProxyBasePath, cfg.ProxyTLS},
		{"webhook", cfg.WebhookAddr, cfg.WebhookBasePath, cfg.WebhookTLS},
		{"admin", cfg.AdminAddr, cfg.AdminBasePath, cfg.AdminTLS},
	} {
		scheme := "http"
		if l.tls != nil {
			scheme = "https"
		}
		_, port, loop := listenScope(l.addr)
		scope := "all interfaces"
		if loop {
			scope = "loopback only"
		}
		base := l.base
		if base == "" {
			base = "/"
		}
		n.add("info", l.name, "%s listener: %s://%s (%s), path %s", l.name, scheme, l.addr, scope, base)
		if port > 0 && port < 1024 && env.EUID != 0 && env.Container == "" {
			n.add("info", l.name, "port %d is privileged: the process needs CAP_NET_BIND_SERVICE (see deploy/systemd) or a port above 1024", port)
		}
		if l.tls != nil {
			n.tlsNotes(l.name, l.tls, port, now)
		}
	}

	// ---- proxy ----
	_, _, proxyLoop := listenScope(cfg.ProxyAddr)
	n.add("info", "proxy", "the HTTP/CONNECT forward proxy cannot sit behind an HTTP reverse proxy: expose it directly (firewalled), use proxy_tls, or TCP passthrough (nginx stream, Traefik TCP router)")
	switch {
	case cfg.ProxyTLS != nil:
		n.add("info", "proxy", "HTTPS proxy: clients use https://<name>:<token>@host:port; some SDKs accept only http:// proxy URLs")
	case !proxyLoop && env.Container == "":
		n.add("warn", "proxy", "forward proxy is plain HTTP on all interfaces: client tokens travel in clear text; firewall %s to trusted networks or set proxy_tls", cfg.ProxyAddr)
	case !proxyLoop:
		n.add("info", "proxy", "forward proxy is plain HTTP: publish its port only to trusted networks (firewall), or set proxy_tls")
	}
	if cfg.ProxyBasePath != "" {
		n.add("info", "proxy", "JSON relay at %s/forward (also /forward if the reverse proxy strips the prefix)", cfg.ProxyBasePath)
	}
	pc := snap.proxy
	if len(pc.Clients) == 0 {
		n.add("warn", "proxy", "no proxy clients in proxy.json: the proxy and /forward reject every request")
	}
	if len(pc.AllowedHosts) == 0 {
		n.add("warn", "proxy", "allowed_hosts is empty: every outbound destination is denied")
	}
	if pc.AllowPrivate {
		n.add("warn", "proxy", "allow_private is on: clients can reach private, loopback and cloud-metadata addresses through the agent")
	}

	// ---- webhook / ingest ----
	switch {
	case cfg.WebhookTLS != nil:
		n.add("info", "webhook", "webhooks are served over HTTPS directly (no reverse proxy needed)")
	case len(cfg.TrustedProxies) == 0:
		n.add("warn", "webhook", "webhooks are plain HTTP and no reverse proxy is trusted: most providers require HTTPS; set webhook_tls, or put a TLS reverse proxy in front and list it in trusted_proxies")
	default:
		n.add("info", "webhook", "expecting TLS to be terminated by a reverse proxy at %s; X-Forwarded-For is believed only from those addresses",
			strings.Join(cfg.TrustedProxies, ", "))
	}
	for _, p := range cfg.trusted {
		if (p.Addr().Is4() && p.Bits() < 8) || (p.Addr().Is6() && p.Bits() < 16) {
			n.add("warn", "webhook", "trusted_proxies contains %s: almost any client can spoof its address with X-Forwarded-For; list only your reverse proxies", p)
		}
	}
	if cfg.WebhookBasePath != "" {
		n.add("info", "webhook", "webhooks at %s/hooks/<id>, ingest and health under %s/ (requests work with or without the prefix)", cfg.WebhookBasePath, cfg.WebhookBasePath)
	}
	var open []string
	for id, w := range snap.webhooks {
		if !w.Disabled && w.Auth == nil && len(w.SourceCIDRs) == 0 {
			open = append(open, "webhook "+id)
		}
	}
	for _, r := range snap.ingest {
		if !r.Disabled && r.Auth == nil && len(r.SourceCIDRs) == 0 {
			open = append(open, "ingest rule "+r.ID)
		}
	}
	sort.Strings(open)
	for _, o := range open {
		n.add("warn", "webhook", "%s accepts requests from anyone: add auth (token, basic or hmac) or source_cidrs", o)
	}

	// ---- admin ----
	_, _, adminLoop := listenScope(cfg.AdminAddr)
	switch {
	case cfg.AdminTLS != nil:
	case !adminLoop && env.Container == "":
		n.add("warn", "admin", "console/API on %s is plain HTTP on all interfaces: tokens travel in clear text; bind 127.0.0.1 (SSH tunnel), put a TLS reverse proxy in front, or set admin_tls", cfg.AdminAddr)
	case !adminLoop:
		n.add("info", "admin", "console/API listens on all interfaces inside the container: publish it as 127.0.0.1:<port> only, or behind a TLS reverse proxy")
	}
	switch {
	case cfg.AdminBasePath != "":
		n.add("info", "admin", "console at %s/ (works with or without prefix stripping)", cfg.AdminBasePath)
	case len(cfg.TrustedProxies) > 0:
		n.add("info", "admin", "console base path follows X-Forwarded-Prefix from trusted proxies (e.g. Traefik StripPrefix); set admin_base_path to fix it")
	}
	editors, viewers := 0, 0
	for _, t := range snap.admin.Tokens {
		if t.Role == RoleEditor {
			editors++
		} else {
			viewers++
		}
	}
	if envAdminToken {
		editors++
	}
	switch {
	case editors+viewers == 0:
		n.add("warn", "admin", "no admin tokens: add <dynamic_dir>/admin.json or set PROXY_ADMIN_TOKEN to use the console and API")
	case editors == 0:
		n.add("info", "admin", "only viewer tokens: configuration can be changed only by editing files")
	}

	// ---- storage / telemetry ----
	if cfg.Database.StoreEventBodies {
		n.add("info", "storage", "callback bodies are kept for %d days (store_event_bodies); set it to false if payloads must not be retained", cfg.Database.RetentionDays)
	}
	if strings.EqualFold(cfg.Database.LogLevel, "debug") {
		n.add("info", "storage", "log_level is debug: the log table will grow quickly")
	}
	return n
}

// tlsNotes describes a listener's certificate setup.
func (n *notes) tlsNotes(name string, t *TLSConfig, port int, now time.Time) {
	if a := t.ACME; a != nil {
		dir := a.DirectoryURL
		if dir == "" {
			dir = "Let's Encrypt production"
			n.add("info", "tls", "%s: ACME uses Let's Encrypt production (rate limits apply); use the staging directory_url while testing", name)
		}
		n.add("info", "tls", "%s: certificates for %s from %s, obtained on the first HTTPS request", name, strings.Join(a.Domains, ", "), dir)
		switch {
		case a.HTTPChallengeAddr != "":
			n.add("info", "tls", "%s: HTTP-01 challenges on %s, which must be reachable from the internet as port 80; it also redirects plain HTTP to HTTPS", name, a.HTTPChallengeAddr)
		case port != 443:
			n.add("warn", "tls", "%s: listener is on port %d and http_challenge_addr is not set: TLS-ALPN-01 needs port 443, so certificates cannot be issued unless 443 is forwarded here", name, port)
		default:
			n.add("info", "tls", "%s: TLS-ALPN-01 challenges on port 443 (must be reachable from the internet)", name)
		}
		return
	}
	b, err := os.ReadFile(t.CertFile)
	if err != nil {
		n.add("error", "tls", "%s: cannot read %s: %v", name, t.CertFile, err)
		return
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		n.add("error", "tls", "%s: %s is not PEM", name, t.CertFile)
		return
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		n.add("error", "tls", "%s: %s: %v", name, t.CertFile, err)
		return
	}
	names := append(append([]string{}, c.DNSNames...), ipStrings(c.IPAddresses)...)
	left := c.NotAfter.Sub(now)
	n.add("info", "tls", "%s: certificate %s for %s, expires %s (reloaded automatically when the file changes)",
		name, t.CertFile, strings.Join(names, ", "), c.NotAfter.UTC().Format("2006-01-02"))
	switch {
	case left <= 0:
		n.add("error", "tls", "%s: certificate %s EXPIRED on %s", name, t.CertFile, c.NotAfter.UTC().Format("2006-01-02"))
	case left < 21*24*time.Hour:
		n.add("warn", "tls", "%s: certificate %s expires in %d days", name, t.CertFile, int(left.Hours()/24))
	}
	if c.IsCA && len(c.DNSNames) == 0 && len(c.IPAddresses) == 0 {
		n.add("warn", "tls", "%s: certificate %s has no subject alternative names; clients will reject it", name, t.CertFile)
	}
}

func ipStrings(ips []net.IP) []string {
	out := make([]string, len(ips))
	for i, ip := range ips {
		out[i] = ip.String()
	}
	return out
}

// logDeploymentNotes writes each note with its level (kind=deploy, so they
// are stored and searchable with the other logs).
func logDeploymentNotes(ns []DeployNote) {
	for _, d := range ns {
		level := slog.LevelInfo
		switch d.Level {
		case "warn":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		}
		slog.Log(context.Background(), level, d.Message, "kind", "deploy", "topic", d.Topic)
	}
}
