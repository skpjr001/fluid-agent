package agent

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/skpjr001/fluid-agent/internal/config"
)

func proxyClient(ta *testApp, token string) *http.Client {
	pu, _ := url.Parse(ta.proxy.URL)
	if token != "" {
		pu.User = url.UserPassword("any", token)
	}
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}, Timeout: 5 * time.Second}
}

func TestProxyHTTP(t *testing.T) {
	ta := newTestApp(t, nil)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy credentials leaked upstream")
		}
		fmt.Fprint(w, "hello "+r.URL.Path)
	}))
	defer up.Close()
	ta.allowUpstream(up.URL)

	resp, err := proxyClient(ta, clientToken).Get(up.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "hello /x" {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}

	// wrong token, disabled client
	resp, _ = proxyClient(ta, "nope-nope-nope-nope").Get(up.URL)
	if resp.StatusCode != 407 {
		t.Fatalf("bad token: %d", resp.StatusCode)
	}
	ta.allowUpstream(up.URL, func(pc *ProxyConfig) { pc.Clients[0].Disabled = true })
	resp, _ = proxyClient(ta, clientToken).Get(up.URL)
	if resp.StatusCode != 407 {
		t.Fatalf("disabled client: %d", resp.StatusCode)
	}

	// the client name is recorded with direction=outbound
	ta.flushLogs()
	rows, _, _ := QueryLogs(t.Context(), ta.db, LogQuery{Kinds: []string{"proxy_http"}, Direction: "outbound"})
	if len(rows) == 0 || rows[0].Client != "dev" || rows[0].Status != 200 {
		t.Fatalf("proxy log rows: %+v", rows)
	}
}

func TestProxyDeniesHostsAndPrivateAddresses(t *testing.T) {
	ta := newTestApp(t, nil)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))
	defer up.Close()

	// host allowed but private address not allowlisted -> 403 from the dial guard
	ta.allowUpstream(up.URL, func(pc *ProxyConfig) { pc.PrivateAllowlist = nil })
	resp, err := proxyClient(ta, clientToken).Get(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 403 {
		t.Fatalf("private destination: %d", resp.StatusCode)
	}
	// "localhost" resolves to loopback: also refused (DNS-time check)
	lu := strings.Replace(up.URL, "127.0.0.1", "localhost", 1)
	resp, _ = proxyClient(ta, clientToken).Get(lu)
	if resp.StatusCode != 403 {
		t.Fatalf("localhost destination: %d", resp.StatusCode)
	}
	// host not in allowed_hosts
	ta.allowUpstream(up.URL, func(pc *ProxyConfig) { pc.AllowedHosts = []string{"example.com"} })
	resp, _ = proxyClient(ta, clientToken).Get(up.URL)
	if resp.StatusCode != 403 {
		t.Fatalf("unlisted host: %d", resp.StatusCode)
	}
	ta.flushLogs()
	rows, _, _ := QueryLogs(t.Context(), ta.db, LogQuery{Kinds: []string{"proxy_denied"}})
	if len(rows) == 0 {
		t.Fatal("denials should be logged")
	}
}

// connect opens a CONNECT tunnel through the proxy and returns the raw conn.
func connect(t *testing.T, ta *testApp, dest, token string) (net.Conn, *bufio.Reader, int) {
	t.Helper()
	pu, _ := url.Parse(ta.proxy.URL)
	c, err := net.Dial("tcp", pu.Host)
	if err != nil {
		t.Fatal(err)
	}
	auth := base64.StdEncoding.EncodeToString([]byte("u:" + token))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n", dest, dest, auth)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c, br, resp.StatusCode
}

func TestProxyConnectTunnelAndIdleTimeout(t *testing.T) {
	ta := newTestApp(t, func(c *config.StaticConfig) { c.TunnelIdleTimeout = config.Duration(300 * time.Millisecond) })
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { io.Copy(c, c) }(c) // echo
		}
	}()
	ta.allowUpstream("http://" + ln.Addr().String())

	c, br, code := connect(t, ta, ln.Addr().String(), clientToken)
	defer c.Close()
	if code != 200 {
		t.Fatalf("CONNECT: %d", code)
	}
	fmt.Fprint(c, "ping\n")
	line, _ := br.ReadString('\n')
	if line != "ping\n" {
		t.Fatalf("echo: %q", line)
	}
	if ta.proxy.Config == nil || ta.App.proxy.tunnels.Load() != 1 {
		t.Fatalf("active tunnels = %d", ta.App.proxy.tunnels.Load())
	}
	// stay idle past the timeout: the agent must close the tunnel
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	_, err := br.ReadByte()
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("idle tunnel not closed (err=%v after %v)", err, time.Since(start))
	}
	eventually(t, "tunnel gauge to drop", func() bool { return ta.App.proxy.tunnels.Load() == 0 })

	// unlisted port and bad token
	_, _, code = connect(t, ta, "127.0.0.1:1", clientToken)
	if code != 403 {
		t.Fatalf("unlisted port: %d", code)
	}
	_, _, code = connect(t, ta, ln.Addr().String(), "bad-token-bad-token")
	if code != 407 {
		t.Fatalf("bad token: %d", code)
	}
}

func TestProxyRejectsRelativeURLs(t *testing.T) {
	ta := newTestApp(t, nil)
	resp, _ := http.Get(ta.proxy.URL + "/something")
	if resp.StatusCode != 400 {
		t.Fatalf("got %d", resp.StatusCode)
	}
	resp, _ = http.Get(ta.proxy.URL + "/healthz")
	if resp.StatusCode != 200 {
		t.Fatalf("healthz %d", resp.StatusCode)
	}
}

// ---------- /forward ----------

func forward(t *testing.T, ta *testApp, token string, req any) (int, ForwardResponse, map[string]string) {
	t.Helper()
	b, _ := json.Marshal(req)
	hr, _ := http.NewRequest("POST", ta.proxy.URL+"/forward", strings.NewReader(string(b)))
	if token != "" {
		hr.Header.Set("X-Agent-Key", token)
	}
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var fr ForwardResponse
	var fe map[string]string
	if resp.StatusCode == 200 {
		mustJSON(t, raw, &fr)
	} else {
		mustJSON(t, raw, &fe)
	}
	return resp.StatusCode, fr, fe
}

func TestForwardURLAndBinaryBodies(t *testing.T) {
	ta := newTestApp(t, nil)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Got-Method", r.Method)
		w.Header().Set("X-Got-Header", r.Header.Get("X-Custom"))
		w.Write(append([]byte{0xff, 0x00}, b...)) // not UTF-8
	}))
	defer up.Close()
	ta.allowUpstream(up.URL)

	code, fr, fe := forward(t, ta, clientToken, ForwardRequest{Method: "PATCH", URL: up.URL + "/p",
		Headers: map[string]string{"X-Custom": "yes"}, BodyBase64: base64.StdEncoding.EncodeToString([]byte{1, 2, 3})})
	if code != 200 {
		t.Fatalf("%d %v", code, fe)
	}
	raw, _ := base64.StdEncoding.DecodeString(fr.BodyBase64)
	if string(raw) != string([]byte{0xff, 0, 1, 2, 3}) || fr.Body != "" {
		t.Fatalf("binary body not preserved: %+v", fr)
	}
	if fr.Headers["X-Got-Method"][0] != "PATCH" || fr.Headers["X-Got-Header"][0] != "yes" {
		t.Fatalf("method/header not relayed: %v", fr.Headers)
	}

	if code, _, fe := forward(t, ta, "", ForwardRequest{URL: up.URL}); code != 401 || fe["code"] != "UNAUTHORIZED" {
		t.Fatalf("no token: %d %v", code, fe)
	}
	if code, _, fe := forward(t, ta, clientToken, ForwardRequest{URL: "https://example.com/"}); code != 403 || fe["code"] != "HOST_NOT_ALLOWED" {
		t.Fatalf("unlisted host: %d %v", code, fe)
	}
	if code, _, fe := forward(t, ta, clientToken, map[string]any{"url": up.URL, "typo": 1}); code != 400 || fe["code"] != "VALIDATION" {
		t.Fatalf("unknown field: %d %v", code, fe)
	}
	ta.allowUpstream(up.URL, func(pc *ProxyConfig) { pc.PrivateAllowlist = nil })
	if code, _, fe := forward(t, ta, clientToken, ForwardRequest{URL: up.URL}); code != 403 || fe["code"] != "BLOCKED_ADDRESS" {
		t.Fatalf("private address: %d %v", code, fe)
	}
}

func TestForwardProfiles(t *testing.T) {
	ta := newTestApp(t, nil)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %s?%s key=%s", r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("X-Api-Key"))
	}))
	defer up.Close()
	ta.allowUpstream(up.URL)
	ta.writeJSON("profiles/prov.json", Profile{BaseURL: up.URL + "/v1", Headers: map[string]string{"X-Api-Key": "provider-secret"},
		AllowedMethods: []string{"GET"}, Clients: []string{"dev"}})

	code, fr, fe := forward(t, ta, clientToken, ForwardRequest{Method: "GET", Profile: "prov", Path: "/orders?id=7",
		Headers: map[string]string{"X-Api-Key": "client-tries-to-override"}})
	if code != 200 || fr.Body != "GET /v1/orders?id=7 key=provider-secret" {
		t.Fatalf("%d %+v %v", code, fr, fe)
	}
	if code, _, fe := forward(t, ta, clientToken, ForwardRequest{Method: "POST", Profile: "prov", Path: "/x"}); code != 403 {
		t.Fatalf("method not allowed by profile: %d %v", code, fe)
	}
	if code, _, _ := forward(t, ta, "other-token-0123456789abcdef", ForwardRequest{Method: "GET", Profile: "prov", Path: "/x"}); code != 403 {
		t.Fatalf("client not allowed by profile: %d", code)
	}
	if code, _, fe := forward(t, ta, clientToken, ForwardRequest{Profile: "nope"}); code != 404 || fe["code"] != "PROFILE_NOT_FOUND" {
		t.Fatalf("%d %v", code, fe)
	}
	if code, _, _ := forward(t, ta, clientToken, ForwardRequest{Method: "GET", Profile: "prov", Path: "//evil.example/x"}); code != 400 {
		t.Fatalf("path that changes host must be rejected: %d", code)
	}
}

func TestForwardRedirectsAreRechecked(t *testing.T) {
	ta := newTestApp(t, nil)
	var other *httptest.Server
	other = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "key=%q auth=%q", r.Header.Get("X-Api-Key"), r.Header.Get("Authorization"))
	}))
	defer other.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/same":
			http.Redirect(w, r, "/final", http.StatusFound)
		case "/final":
			fmt.Fprintf(w, "key=%q", r.Header.Get("X-Api-Key"))
		case "/cross":
			http.Redirect(w, r, strings.Replace(other.URL, "127.0.0.1", "localhost", 1)+"/x", http.StatusTemporaryRedirect)
		case "/evil":
			http.Redirect(w, r, "https://evil.example/steal", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		}
	}))
	defer up.Close()
	pu, _ := url.Parse(up.URL)
	ou, _ := url.Parse(other.URL)
	ta.allowUpstream(up.URL, func(pc *ProxyConfig) {
		pc.AllowedPorts = []int{defaultPort(pu), defaultPort(ou)}
		pc.AllowedHosts = []string{"127.0.0.1", "localhost"}
		pc.AllowPrivate = true
	})
	ta.writeJSON("profiles/p.json", Profile{BaseURL: up.URL, Headers: map[string]string{"X-Api-Key": "secret", "Authorization": "Bearer s"}})

	code, fr, _ := forward(t, ta, clientToken, ForwardRequest{Method: "GET", Profile: "p", Path: "/same"})
	if code != 200 || fr.Body != `key="secret"` || fr.Redirects != 1 {
		t.Fatalf("same-origin redirect keeps credentials: %d %+v", code, fr)
	}
	code, fr, _ = forward(t, ta, clientToken, ForwardRequest{Method: "GET", Profile: "p", Path: "/cross"})
	if code != 200 || fr.Body != `key="" auth=""` {
		t.Fatalf("cross-origin redirect must drop injected credentials: %d %+v", code, fr)
	}
	if code, _, fe := forward(t, ta, clientToken, ForwardRequest{Method: "GET", Profile: "p", Path: "/evil"}); code != 403 || fe["code"] != "HOST_NOT_ALLOWED" {
		t.Fatalf("redirect to unlisted host: %d %v", code, fe)
	}
	if code, _, _ := forward(t, ta, clientToken, ForwardRequest{Method: "GET", Profile: "p", Path: "/loop"}); code != 502 {
		t.Fatalf("redirect loop: %d", code)
	}
}
