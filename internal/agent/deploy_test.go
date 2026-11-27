package agent

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skpjr001/fluid-agent/internal/config"
)

func get(t *testing.T, c *http.Client, u string, h map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", u, nil)
	for k, v := range h {
		req.Header.Set(k, v)
	}
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

var bearer = map[string]string{"Authorization": "Bearer " + viewerToken}

func TestBasePathNormalize(t *testing.T) {
	for in, want := range map[string]string{"": "", "/": "", "/a": "/a", "/a/b/": "/a/b", " /x-1.y_~ ": "/x-1.y_~"} {
		if got, err := config.NormalizeBasePath(in); err != nil || got != want {
			t.Errorf("%q: got %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"a", "/a b", "/a/../b", "/a?x", `/"><script>`, "//"} {
		if _, err := config.NormalizeBasePath(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// Every listener must work whether or not the reverse proxy strips the prefix.
func TestBasePathsWithAndWithoutStripping(t *testing.T) {
	ta := newTestApp(t, func(c *config.StaticConfig) {
		c.WebhookBasePath, c.AdminBasePath, c.ProxyBasePath = "/gw", "/console", "/relay"
	})
	tgt := newRecorder(200)
	defer tgt.srv.Close()
	ta.writeJSON("webhooks/w.json", Webhook{Target: tgt.srv.URL})
	ta.writeJSON("ingest/r.json", IngestRule{Match: PathMatch{Type: "exact", Value: "/v1/status"}, Targets: []string{tgt.srv.URL}})

	// webhooks + health: prefixed and stripped
	for _, p := range []string{"/gw/hooks/w", "/hooks/w"} {
		if r, _ := post(t, ta.inbound.URL+p, "x", nil); r.StatusCode != 200 {
			t.Errorf("POST %s: %d", p, r.StatusCode)
		}
	}
	for _, p := range []string{"/gw/readyz", "/readyz", "/gw/livez"} {
		if r, _ := get(t, nil, ta.inbound.URL+p, nil); r.StatusCode != 200 {
			t.Errorf("GET %s: %d", p, r.StatusCode)
		}
	}
	// ingest rules match the path inside the base
	if r, _ := post(t, ta.inbound.URL+"/gw/v1/status", "x", nil); r.StatusCode != 200 {
		t.Errorf("ingest under base: %d", r.StatusCode)
	}
	eventually(t, "ingest delivery", func() bool { return tgt.calls.Load() == 3 })
	if rq, _ := tgt.last(); rq.Header.Get("X-Fluid-Path") != "/v1/status" {
		t.Errorf("X-Fluid-Path = %q", rq.Header.Get("X-Fluid-Path"))
	}

	// console: index carries the base, assets and API work both ways
	for _, p := range []string{"/console/", "/console", "/", "/console/events", "/events"} {
		r, body := get(t, nil, ta.admin.URL+p, nil)
		if r.StatusCode != 200 || !strings.Contains(body, `src="/console/app.js"`) || !strings.Contains(body, `content="/console"`) {
			t.Errorf("GET %s: %d\n%s", p, r.StatusCode, body)
		}
	}
	for _, p := range []string{"/console/app.js", "/app.js"} {
		if r, body := get(t, nil, ta.admin.URL+p, nil); r.StatusCode != 200 || !strings.Contains(body, "fluid-agent console") {
			t.Errorf("GET %s: %d", p, r.StatusCode)
		}
	}
	for _, p := range []string{"/console/api/whoami", "/api/whoami", "/console/metrics"} {
		if r, _ := get(t, nil, ta.admin.URL+p, bearer); r.StatusCode != 200 {
			t.Errorf("GET %s: %d", p, r.StatusCode)
		}
	}
	if r, _ := get(t, nil, ta.admin.URL+"/console/api/schema/webhook.schema.json", nil); r.StatusCode != 200 {
		t.Errorf("public schema: %d", r.StatusCode)
	}

	// proxy listener: /forward under the base, but proxied paths are untouched
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, r.URL.Path) }))
	defer up.Close()
	ta.allowUpstream(up.URL)
	for _, p := range []string{"/relay/forward", "/forward"} {
		hr, _ := http.NewRequest("POST", ta.proxy.URL+p, strings.NewReader(`{"method":"GET","url":"`+up.URL+`/relay/x"}`))
		hr.Header.Set("X-Agent-Key", clientToken)
		resp, err := http.DefaultClient.Do(hr)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(b), `"body": "/relay/x"`) {
			t.Errorf("POST %s: %d %s", p, resp.StatusCode, b)
		}
	}
	resp, err := proxyClient(ta, clientToken).Get(up.URL + "/relay/forward")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "/relay/forward" {
		t.Fatalf("proxied destination path was rewritten: %q", b)
	}
}

func TestForwardedPrefixOnlyFromTrustedProxy(t *testing.T) {
	ta := newTestApp(t, func(c *config.StaticConfig) { c.TrustedProxies = []string{"127.0.0.1"} })
	_, body := get(t, nil, ta.admin.URL+"/", map[string]string{"X-Forwarded-Prefix": "/fa/"})
	if !strings.Contains(body, `src="/fa/app.js"`) {
		t.Fatalf("trusted prefix ignored:\n%s", body)
	}
	_, body = get(t, nil, ta.admin.URL+"/", map[string]string{"X-Forwarded-Prefix": `/x"><script>alert(1)</script>`})
	if strings.Contains(body, "<script>alert") || !strings.Contains(body, `src="/app.js"`) {
		t.Fatalf("invalid prefix must be ignored:\n%s", body)
	}

	untrusted := newTestApp(t, nil)
	_, body = get(t, nil, untrusted.admin.URL+"/", map[string]string{"X-Forwarded-Prefix": "/fa"})
	if !strings.Contains(body, `src="/app.js"`) {
		t.Fatalf("prefix from an untrusted peer must be ignored:\n%s", body)
	}
}

// reverseProxy emulates Traefik (strip=true, sets X-Forwarded-Prefix) or
// nginx without a trailing-slash proxy_pass (strip=false).
func reverseProxy(t *testing.T, backend, prefix string, strip bool) *httptest.Server {
	t.Helper()
	bu, _ := url.Parse(backend)
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(bu)
			pr.SetXForwarded()
			if strip {
				pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, prefix)
				if pr.Out.URL.Path == "" {
					pr.Out.URL.Path = "/"
				}
				pr.Out.Header.Set("X-Forwarded-Prefix", prefix)
			}
		},
		FlushInterval: -1, // stream SSE
	}
	mux := http.NewServeMux()
	mux.Handle(prefix+"/", rp)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestBehindReverseProxies(t *testing.T) {
	cases := []struct {
		name      string
		strip     bool
		adminBase string
	}{
		{"traefik StripPrefix + X-Forwarded-Prefix", true, ""},
		{"traefik StripPrefix + admin_base_path", true, "/fluid-admin"},
		{"nginx without stripping + admin_base_path", false, "/fluid-admin"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ta := newTestApp(t, func(cfg *config.StaticConfig) {
				cfg.TrustedProxies = []string{"127.0.0.1"}
				cfg.AdminBasePath = c.adminBase
				cfg.WebhookBasePath = c.adminBase // reuse: "" or "/fluid-admin"
			})
			front := reverseProxy(t, ta.admin.URL, "/fluid-admin", c.strip)
			r, body := get(t, nil, front.URL+"/fluid-admin/", nil)
			if r.StatusCode != 200 || !strings.Contains(body, `src="/fluid-admin/app.js"`) {
				t.Fatalf("index: %d\n%s", r.StatusCode, body)
			}
			if r, _ := get(t, nil, front.URL+"/fluid-admin/app.js", nil); r.StatusCode != 200 {
				t.Fatalf("asset: %d", r.StatusCode)
			}
			if r, b := get(t, nil, front.URL+"/fluid-admin/api/whoami", bearer); r.StatusCode != 200 || !strings.Contains(b, "viewer") {
				t.Fatalf("api: %d %s", r.StatusCode, b)
			}
			// live stream through the proxy
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "GET", front.URL+"/fluid-admin/api/stream?topics=logs", nil)
			req.Header.Set("Authorization", "Bearer "+viewerToken)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			line, _ := bufio.NewReader(resp.Body).ReadString('\n')
			resp.Body.Close()
			if !strings.HasPrefix(line, ": connected") {
				t.Fatalf("stream through proxy: %q", line)
			}

			// webhooks behind the same proxy. It sets X-Forwarded-For to the real
			// peer (like the nginx example), so a spoofed allowed IP is ignored.
			tgt := newRecorder(200)
			defer tgt.srv.Close()
			hooks := reverseProxy(t, ta.inbound.URL, "/fluid-admin", c.strip)
			ta.writeJSON("webhooks/w.json", Webhook{Target: tgt.srv.URL, RouteOptions: RouteOptions{SourceCIDRs: []string{"52.66.0.0/16"}}})
			if r, _ := post(t, hooks.URL+"/fluid-admin/hooks/w", "x", map[string]string{"X-Forwarded-For": "52.66.1.1"}); r.StatusCode != 403 {
				t.Fatalf("spoofed allowed sender via proxy: %d", r.StatusCode)
			}
			ta.writeJSON("webhooks/w.json", Webhook{Target: tgt.srv.URL, RouteOptions: RouteOptions{SourceCIDRs: []string{"127.0.0.1"}}})
			if r, _ := post(t, hooks.URL+"/fluid-admin/hooks/w", "x", nil); r.StatusCode != 200 {
				t.Fatalf("genuine sender via proxy: %d", r.StatusCode)
			}
		})
	}
}

// ---------- TLS ----------

func writeCert(t *testing.T, dir, name string, hosts ...string) (certFile, keyFile string, serial int64) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serial = time.Now().UnixNano()
	tpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else {
			tpl.DNSNames = append(tpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile = filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	return certFile, keyFile, serial
}

func TestTLSConfigValidation(t *testing.T) {
	cases := map[string]*config.TLSConfig{
		"set cert_file + key_file, or acme": {},
		"must both be set":                  {CertFile: "a"},
		"not both":                          {CertFile: "a", KeyFile: "b", ACME: &config.ACMEConfig{Domains: []string{"x"}, AcceptTOS: true}},
		"acme.domains":                      {ACME: &config.ACMEConfig{AcceptTOS: true}},
		"accept_tos":                        {ACME: &config.ACMEConfig{Domains: []string{"x"}}},
		"min_version":                       {CertFile: "a", KeyFile: "b", MinVersion: "1.0"},
	}
	for want, c := range cases {
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
	}
	cfg := config.Default()
	cfg.ProxyTLS = &config.TLSConfig{ACME: &config.ACMEConfig{Domains: []string{"a"}, AcceptTOS: true, HTTPChallengeAddr: ":80"}}
	cfg.AdminTLS = &config.TLSConfig{ACME: &config.ACMEConfig{Domains: []string{"b"}, AcceptTOS: true, HTTPChallengeAddr: ":80"}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "same acme.http_challenge_addr") {
		t.Fatalf("shared challenge port: %v", err)
	}
}

func TestTLSListeners(t *testing.T) {
	dir := t.TempDir()
	cert, key, _ := writeCert(t, dir, "agent", "127.0.0.1")
	tc := &config.TLSConfig{CertFile: cert, KeyFile: key}
	ta := newTestApp(t, func(c *config.StaticConfig) {
		c.ProxyAddr, c.WebhookAddr, c.AdminAddr = "127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0"
		c.ProxyTLS, c.WebhookTLS, c.AdminTLS = tc, tc, &config.TLSConfig{CertFile: cert, KeyFile: key, MinVersion: "1.3"}
	})
	running, err := ta.openListeners()
	if err != nil {
		t.Fatal(err)
	}
	addr := map[string]string{}
	for _, rs := range running {
		addr[rs.name] = rs.ln.Addr().String()
		if rs.scheme != "https" {
			t.Fatalf("%s scheme %s", rs.name, rs.scheme)
		}
		go rs.srv.Serve(rs.ln)
		defer rs.srv.Close()
	}
	pool := x509.NewCertPool()
	pem, _ := os.ReadFile(cert)
	pool.AppendCertsFromPEM(pem)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}, ForceAttemptHTTP2: true}, Timeout: 5 * time.Second}

	if r, _ := get(t, client, "https://"+addr["webhook"]+"/livez", nil); r.StatusCode != 200 || r.TLS == nil {
		t.Fatalf("webhook https: %v", r.Status)
	}
	r, _ := get(t, client, "https://"+addr["admin"]+"/api/whoami", bearer)
	if r.StatusCode != 200 || r.ProtoMajor != 2 || r.TLS.Version != tls.VersionTLS13 {
		t.Fatalf("admin: %s proto=%s tls=%x", r.Status, r.Proto, r.TLS.Version)
	}
	// plain HTTP to a TLS listener fails
	if _, err := http.Get("http://" + addr["admin"] + "/livez"); err == nil {
		r, _ := http.Get("http://" + addr["admin"] + "/livez")
		if r.StatusCode == 200 {
			t.Fatal("plain HTTP accepted on TLS listener")
		}
	}

	// HTTPS proxy: CONNECT over TLS (HTTP/1.1 only) to an echo server
	echo, _ := net.Listen("tcp", "127.0.0.1:0")
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go io.Copy(c, c)
		}
	}()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "via https proxy") }))
	defer up.Close()
	_, echoPort, _ := net.SplitHostPort(echo.Addr().String())
	upURL, _ := url.Parse(up.URL)
	ta.writeJSON("proxy.json", ProxyConfig{Clients: []Client{{Name: "dev", Token: clientToken}}, AllowedHosts: []string{"127.0.0.1"},
		AllowedPorts: []int{atoi(echoPort), defaultPort(upURL)}, PrivateAllowlist: []string{"127.0.0.1"}})

	conn, err := tls.Dial("tcp", addr["proxy"], &tls.Config{RootCAs: pool, NextProtos: []string{"h2", "http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if p := conn.ConnectionState().NegotiatedProtocol; p == "h2" {
		t.Fatal("proxy listener must not negotiate HTTP/2")
	}
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Bearer %s\r\n\r\n", echo.Addr(), echo.Addr(), clientToken)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT over TLS: %v %v", resp, err)
	}
	fmt.Fprint(conn, "hello\n")
	if line, _ := br.ReadString('\n'); line != "hello\n" {
		t.Fatalf("tunnel echo: %q", line)
	}

	// a standard Go client with an https:// proxy URL
	pu, _ := url.Parse("https://dev:" + clientToken + "@" + addr["proxy"])
	pc := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu), TLSClientConfig: &tls.Config{RootCAs: pool}}, Timeout: 5 * time.Second}
	if r, body := get(t, pc, up.URL, nil); r.StatusCode != 200 || body != "via https proxy" {
		t.Fatalf("https proxy URL: %d %q", r.StatusCode, body)
	}
}

func atoi(s string) int {
	var n int
	fmt.Sscan(s, &n)
	return n
}

func TestCertificateHotReload(t *testing.T) {
	dir := t.TempDir()
	cert, key, s1 := writeCert(t, dir, "a", "localhost")
	cr, err := newCertReloader(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf := func() int64 {
		c, _ := cr.GetCertificate(nil)
		x, _ := x509.ParseCertificate(c.Certificate[0])
		return x.SerialNumber.Int64()
	}
	if leaf() != s1 {
		t.Fatal("initial cert")
	}
	// renew: new files with a later mtime
	time.Sleep(20 * time.Millisecond)
	_, _, s2 := writeCert(t, dir, "a", "localhost")
	future := time.Now().Add(time.Second)
	os.Chtimes(cert, future, future)
	if leaf() != s1 {
		t.Fatal("reload must be rate-limited")
	}
	cr.checked = time.Time{}
	if leaf() != s2 {
		t.Fatal("renewed cert not picked up")
	}
	// a broken renewal keeps serving the last good certificate
	os.WriteFile(cert, []byte("garbage"), 0o600)
	later := future.Add(time.Second)
	os.Chtimes(cert, later, later)
	cr.checked = time.Time{}
	if leaf() != s2 {
		t.Fatal("bad file replaced the good certificate")
	}
}

func TestACMECAFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := buildTLS(&config.TLSConfig{ACME: &config.ACMEConfig{Domains: []string{"a"}, AcceptTOS: true, CAFile: filepath.Join(dir, "missing.pem")}}, dir, false); err == nil {
		t.Fatal("missing ca_file accepted")
	}
	bad := filepath.Join(dir, "bad.pem")
	os.WriteFile(bad, []byte("not pem"), 0o600)
	if _, err := buildTLS(&config.TLSConfig{ACME: &config.ACMEConfig{Domains: []string{"a"}, AcceptTOS: true, CAFile: bad}}, dir, false); err == nil {
		t.Fatal("non-PEM ca_file accepted")
	}
	cert, _, _ := writeCert(t, dir, "ca", "pebble")
	if _, err := buildTLS(&config.TLSConfig{ACME: &config.ACMEConfig{Domains: []string{"a"}, AcceptTOS: true, CAFile: cert,
		DirectoryURL: "https://pebble:14000/dir"}}, dir, false); err != nil {
		t.Fatal(err)
	}
}

func TestACMESetupWithoutNetwork(t *testing.T) {
	ts, err := buildTLS(&config.TLSConfig{ACME: &config.ACMEConfig{Domains: []string{"hooks.example.com"}, AcceptTOS: true,
		CacheDir: t.TempDir(), HTTPChallengeAddr: "127.0.0.1:0"}}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(ts.config.NextProtos, ","), "acme-tls/1") {
		t.Fatalf("TLS-ALPN-01 not enabled: %v", ts.config.NextProtos)
	}
	// host policy refuses names that are not configured, before contacting any CA
	if _, err := ts.config.GetCertificate(&tls.ClientHelloInfo{ServerName: "evil.example.com"}); err == nil {
		t.Fatal("certificate requested for an unlisted host")
	}
	// the HTTP-01 listener redirects ordinary requests to HTTPS
	srv := httptest.NewServer(ts.httpHandler)
	defer srv.Close()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("GET", srv.URL+"/hooks/x", nil)
	req.Host = "hooks.example.com"
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), "https://hooks.example.com/hooks/x") {
		t.Fatalf("redirect: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	// a challenge request whose Host carries ":80" (as Pebble sends) is not
	// refused by the host policy: unknown token -> 404, not 403
	for _, host := range []string{"hooks.example.com", "hooks.example.com:80"} {
		req, _ := http.NewRequest("GET", srv.URL+"/.well-known/acme-challenge/unknown-token", nil)
		req.Host = host
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("challenge with Host %q: %d", host, resp.StatusCode)
		}
	}
}
