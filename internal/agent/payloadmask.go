package agent

import (
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
)

// GET /api/logs/{id}: one log row with its captured request and response.
// Editors get everything exactly as stored. Viewers get the same with
// credentials masked: the values of credential headers, and any occurrence
// of a secret the agent knows (tokens, passwords, profile credentials).

const payloadMask = "********"

// alwaysSecretHeaders carry credentials in any API.
var alwaysSecretHeaders = []string{"Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie",
	"X-Agent-Key", "X-Api-Key", "Api-Key", "X-Auth-Token", "X-Access-Token", "X-Csrf-Token"}

// secretsForMasking collects header names and values the agent knows to be
// credentials. Values shorter than 6 characters are skipped (too common).
func (a *Admin) secretsForMasking() (headers map[string]bool, values []string) {
	headers = map[string]bool{}
	for _, h := range alwaysSecretHeaders {
		headers[http.CanonicalHeaderKey(h)] = true
	}
	add := func(v string) {
		if len(v) >= 6 {
			values = append(values, v)
		}
	}
	snap := a.app.store.snap.Load()
	for _, c := range snap.proxy.Clients {
		add(c.Token)
	}
	for _, t := range snap.admin.Tokens {
		add(t.Token)
	}
	if env := os.Getenv("PROXY_ADMIN_TOKEN"); env != "" {
		add(env)
	}
	route := func(o *RouteOptions) {
		if au := o.Auth; au != nil {
			if au.Token != nil {
				headers[http.CanonicalHeaderKey(au.Token.Header)] = true
				add(au.Token.Value)
			}
			if au.Basic != nil {
				add(au.Basic.Password)
				add(base64.StdEncoding.EncodeToString([]byte(au.Basic.Username + ":" + au.Basic.Password)))
			}
			if au.HMAC != nil {
				add(au.HMAC.Secret)
			}
		}
		if ta := o.TargetAuth; ta != nil {
			add(ta.Token)
			add(ta.Password)
			if ta.Username != "" || ta.Password != "" {
				add(base64.StdEncoding.EncodeToString([]byte(ta.Username + ":" + ta.Password)))
			}
			if ta.Header != "" {
				headers[http.CanonicalHeaderKey(ta.Header)] = true
				add(ta.Value)
			}
		}
	}
	for _, w := range snap.webhooks {
		route(&w.RouteOptions)
	}
	for _, r := range snap.ingest {
		route(&r.RouteOptions)
	}
	for _, p := range snap.profiles {
		for k, v := range p.Headers {
			headers[http.CanonicalHeaderKey(k)] = true
			add(v)
		}
	}
	// Longest first, so a secret containing another is replaced whole.
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	return headers, values
}

func maskString(s string, values []string) string {
	for _, v := range values {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, payloadMask)
		}
	}
	return s
}

func maskPart(p *PayloadPart, headers map[string]bool, values []string) {
	if p == nil {
		return
	}
	for k, vs := range p.Headers {
		for i, v := range vs {
			if headers[http.CanonicalHeaderKey(k)] {
				vs[i] = payloadMask
			} else {
				vs[i] = maskString(v, values)
			}
		}
	}
	p.Body = maskString(p.Body, values)
}

func (a *Admin) getLog(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		errJSON(w, 400, "bad id")
		return
	}
	rows, _, err := QueryLogs(r.Context(), a.app.db, LogQuery{List: ListParams{Conds: []string{"id = ?"}, Args: []any{id}}, Limit: 1})
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	if len(rows) == 0 {
		errJSON(w, 404, "not found")
		return
	}
	p, err := GetLogPayload(r.Context(), a.app.db, id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		errJSON(w, 500, err.Error())
		return
	}
	masked := false
	if principalOf(r).Role != RoleEditor && p != nil {
		hs, vs := a.secretsForMasking()
		maskPart(p.Request, hs, vs)
		maskPart(p.Response, hs, vs)
		masked = true
	}
	writeJSON(w, 200, map[string]any{"log": rows[0], "payload": p, "masked": masked})
}
