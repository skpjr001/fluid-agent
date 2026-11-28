package agent

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/skpjr001/fluid-agent/internal/config"
)

// echoServer answers with what it received, as JSON.
func echoServer(t *testing.T) *httptest.Server {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"method": r.Method, "uri": r.RequestURI, "host": r.Host,
			"headers": r.Header, "body": string(b)})
	}))
	t.Cleanup(up.Close)
	return up
}

type echoed struct {
	Method  string      `json:"method"`
	URI     string      `json:"uri"`
	Host    string      `json:"host"`
	Headers http.Header `json:"headers"`
	Body    string      `json:"body"`
}

func relayCall(t *testing.T, method, u, token string, body string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, u, strings.NewReader(body))
	if token != "" {
		req.Header.Set("X-Agent-Key", token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestRelayBothFormsPassTheRequestThrough(t *testing.T) {
	ta := newTestApp(t, nil)
	up := echoServer(t)
	ta.allowUpstream(up.URL)
	target := up.URL + "/ibbsmsgapi/v1/exchtime_new?a=1&b=two%20words"
	hdr := map[string]string{"Content-Type": "application/json", "Login": "SAKSHI", "Authorization": "Bearer target-secret",
		"X-Forwarded-For": "10.1.2.3", "X-Real-IP": "10.1.2.3"}
	for name, u := range map[string]string{
		"query form":              ta.proxy.URL + "/relay?target_url=" + url.QueryEscape(target),
		"path form":               ta.proxy.URL + "/relay/" + target,
		"path form encoded":       ta.proxy.URL + "/relay/" + url.PathEscape(target),
		"path form fully encoded": ta.proxy.URL + "/relay/" + url.QueryEscape(target),
		"merged slashes":          ta.proxy.URL + "/relay/" + strings.Replace(target, "http://", "http:/", 1),
		"api path":                ta.proxy.URL + "/api/v1/relay/" + target,
	} {
		t.Run(name, func(t *testing.T) {
			for _, method := range []string{"POST", "PUT", "GET", "DELETE"} {
				body := `{"tradedate":"28 Nov 2026"}`
				if method == "GET" {
					body = ""
				}
				resp, b := relayCall(t, method, u, clientToken, body, hdr)
				if resp.StatusCode != 201 || resp.Header.Get("X-Upstream") != "yes" || resp.Header.Get("X-Fluid-Error") != "" {
					t.Fatalf("%s: target's status and headers must come back as-is: %d %v %s", method, resp.StatusCode, resp.Header, b)
				}
				var e echoed
				mustJSON(t, b, &e)
				if e.Method != method || e.URI != "/ibbsmsgapi/v1/exchtime_new?a=1&b=two%20words" || e.Body != body {
					t.Fatalf("%s: method, path, query or body changed: %+v", method, e)
				}
				if e.Headers.Get("Content-Type") != "application/json" || e.Headers.Get("Login") != "SAKSHI" ||
					e.Headers.Get("Authorization") != "Bearer target-secret" {
					t.Fatalf("%s: the caller's headers must reach the target: %v", method, e.Headers)
				}
				for _, h := range []string{"X-Agent-Key", "X-Forwarded-For", "X-Real-Ip"} {
					if v := e.Headers.Get(h); v != "" {
						t.Fatalf("%s: %s must not reach the target, got %q", method, h, v)
					}
				}
				if !strings.HasPrefix(up.URL, "http://"+e.Host) {
					t.Fatalf("Host must be the target's: %q", e.Host)
				}
			}
		})
	}
}

func TestRelayWithBasePath(t *testing.T) {
	ta := newTestApp(t, func(c *config.StaticConfig) { c.ProxyBasePath = "/fluid-proxy" })
	up := echoServer(t)
	ta.allowUpstream(up.URL)
	for _, u := range []string{
		ta.proxy.URL + "/fluid-proxy/relay?target_url=" + url.QueryEscape(up.URL+"/x"),
		ta.proxy.URL + "/fluid-proxy/relay/" + up.URL + "/x",
		ta.proxy.URL + "/relay/" + up.URL + "/x", // the reverse proxy stripped the prefix
	} {
		resp, b := relayCall(t, "POST", u, clientToken, "hi", nil)
		var e echoed
		mustJSON(t, b, &e)
		if resp.StatusCode != 201 || e.URI != "/x" || e.Body != "hi" {
			t.Fatalf("%s: %d %+v", u, resp.StatusCode, e)
		}
	}
}

func TestRelayRefusals(t *testing.T) {
	ta := newTestApp(t, nil)
	up := echoServer(t)
	ta.allowUpstream(up.URL)
	cases := []struct {
		name, u, token string
		hdr            map[string]string
		status         int
		code           string
	}{
		{"no key", "/relay/" + up.URL, "", nil, 401, "UNAUTHORIZED"},
		{"bearer is not the key", "/relay/" + up.URL, "", map[string]string{"Authorization": "Bearer " + clientToken}, 401, "UNAUTHORIZED"},
		{"no target", "/relay", clientToken, nil, 400, "VALIDATION"},
		{"unencoded query", "/relay?target_url=" + up.URL + "/x?a=1&b=2", clientToken, nil, 400, "VALIDATION"},
		{"not http", "/relay?target_url=" + url.QueryEscape("ftp://127.0.0.1/x"), clientToken, nil, 400, "VALIDATION"},
		{"credentials in URL", "/relay/http://user:pw@127.0.0.1/x", clientToken, nil, 400, "VALIDATION"},
		{"host not allowed", "/relay/https://example.com/", clientToken, nil, 403, "HOST_NOT_ALLOWED"},
		{"bad timeout", "/relay/" + up.URL, clientToken, map[string]string{"X-Fluid-Timeout-Ms": "soon"}, 400, "VALIDATION"},
	}
	for _, c := range cases {
		resp, b := relayCall(t, "POST", ta.proxy.URL+c.u, c.token, "x", c.hdr)
		var fe map[string]string
		mustJSON(t, b, &fe)
		if resp.StatusCode != c.status || resp.Header.Get("X-Fluid-Error") != c.code || fe["code"] != c.code {
			t.Fatalf("%s: %d %q %s", c.name, resp.StatusCode, resp.Header.Get("X-Fluid-Error"), b)
		}
	}
	ta.allowUpstream(up.URL, func(pc *ProxyConfig) { pc.PrivateAllowlist = nil })
	if resp, b := relayCall(t, "POST", ta.proxy.URL+"/relay/"+up.URL, clientToken, "x", nil); resp.StatusCode != 403 ||
		resp.Header.Get("X-Fluid-Error") != "BLOCKED_ADDRESS" {
		t.Fatalf("private address: %d %s", resp.StatusCode, b)
	}
}

func TestRelayRedirectsAndRawBytes(t *testing.T) {
	ta := newTestApp(t, nil)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "auth=%q", r.Header.Get("Authorization"))
	}))
	defer other.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/same":
			http.Redirect(w, r, "/final", http.StatusTemporaryRedirect)
		case "/final":
			b, _ := io.ReadAll(r.Body)
			fmt.Fprintf(w, "%s auth=%q body=%s", r.Method, r.Header.Get("Authorization"), b)
		case "/cross":
			http.Redirect(w, r, strings.Replace(other.URL, "127.0.0.1", "localhost", 1)+"/x", http.StatusFound)
		case "/evil":
			http.Redirect(w, r, "https://evil.example/steal", http.StatusFound)
		case "/gzip":
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			gz.Write([]byte("compressed"))
			gz.Close()
		}
	}))
	defer up.Close()
	pu, _ := url.Parse(up.URL)
	ou, _ := url.Parse(other.URL)
	ta.allowUpstream(up.URL, func(pc *ProxyConfig) {
		pc.AllowedPorts = []int{defaultPort(pu), defaultPort(ou)}
		pc.AllowPrivate = true
	})
	auth := map[string]string{"Authorization": "Bearer s"}

	resp, b := relayCall(t, "POST", ta.proxy.URL+"/relay/"+up.URL+"/same", clientToken, "payload", auth)
	if string(b) != `POST auth="Bearer s" body=payload` || resp.Header.Get("X-Fluid-Final-Url") != up.URL+"/final" {
		t.Fatalf("307 keeps method, body and same-origin credentials: %s %v", b, resp.Header)
	}
	if _, b := relayCall(t, "GET", ta.proxy.URL+"/relay/"+up.URL+"/cross", clientToken, "", auth); string(b) != `auth=""` {
		t.Fatalf("cross-origin redirect must drop Authorization: %s", b)
	}
	if resp, _ := relayCall(t, "GET", ta.proxy.URL+"/relay/"+up.URL+"/evil", clientToken, "", nil); resp.StatusCode != 403 ||
		resp.Header.Get("X-Fluid-Error") != "HOST_NOT_ALLOWED" {
		t.Fatalf("redirect to an unlisted host: %d", resp.StatusCode)
	}
	resp, b = relayCall(t, "GET", ta.proxy.URL+"/relay/"+up.URL+"/gzip", clientToken, "", map[string]string{"Accept-Encoding": "gzip"})
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("compressed responses pass through untouched: %v", resp.Header)
	}
	zr, err := gzip.NewReader(strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	if plain, _ := io.ReadAll(zr); string(plain) != "compressed" {
		t.Fatalf("gzip body: %q", plain)
	}
}

func TestRelayIsLogged(t *testing.T) {
	ta := newTestApp(t, nil)
	up := echoServer(t)
	ta.allowUpstream(up.URL)
	relayCall(t, "POST", ta.proxy.URL+"/relay/"+up.URL+"/logged", clientToken, "x", nil)
	ta.flushLogs()
	var n int
	ta.db.QueryRow(`SELECT COUNT(*) FROM logs WHERE kind = 'relay' AND msg = 'relay'`).Scan(&n)
	if n != 1 {
		t.Fatalf("relay call should be logged once, got %d", n)
	}
}

func TestRelayWithBasePathNamedRelay(t *testing.T) {
	ta := newTestApp(t, func(c *config.StaticConfig) { c.ProxyBasePath = "/relay" })
	up := echoServer(t)
	ta.allowUpstream(up.URL)
	for _, u := range []string{
		ta.proxy.URL + "/relay/relay/" + up.URL + "/x",
		ta.proxy.URL + "/relay/" + up.URL + "/x",
		ta.proxy.URL + "/relay?target_url=" + url.QueryEscape(up.URL+"/x"),
	} {
		if resp, b := relayCall(t, "POST", u, clientToken, "hi", nil); resp.StatusCode != 201 {
			t.Fatalf("%s: %d %s", u, resp.StatusCode, b)
		}
	}
}
