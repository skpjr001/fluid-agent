# fluid-agent

A single Go binary for servers whose IP is allowlisted by payment providers, exchanges and other partners. It runs three listeners:

| Listener | Default | Purpose |
|---|---|---|
| Proxy | `:8080` | Real HTTP/`CONNECT` forward proxy (works with `HTTPS_PROXY`) **and** a JSON relay at `POST /forward`. Named client tokens, host/port allowlist, private-address guard. |
| Webhook / ingest | `:8081` | `/hooks/{id}[/sub/path]` webhooks, plus path-rule **callback ingest** on every other path. Optional auth, per-IP rate limit, durable delivery with retries. |
| Admin | `127.0.0.1:9000` | Web console (JIRA-style), REST API, live stream, Prometheus `/metrics`. **Viewer** and **editor** tokens. |

Everything is stored in one SQLite file (logs, events, deliveries) and all configuration lives in **JSON files** (the *file provider*) that are hot-reloaded and validated against published [JSON Schemas](schema/).

## Quick start

No setup is needed: on first start the agent creates every directory and config file it is missing (see [First start](#first-start-what-is-created)).

**On a host**

```bash
fluid-agent                              # creates ./config.json, ./dynamic/ and ./data/, then serves
cat dynamic/admin.json                   # the generated editor token for the console
```

**With systemd** (unit in [`deploy/systemd`](deploy/systemd/fluid-agent.service))

```bash
sudo useradd -r -s /usr/sbin/nologin -d /var/lib/fluid-agent fluid
sudo install -m 755 fluid-agent /usr/local/bin/
sudo cp deploy/systemd/fluid-agent.service /etc/systemd/system/
sudo systemctl enable --now fluid-agent  # everything is created under /var/lib/fluid-agent
sudo cat /var/lib/fluid-agent/dynamic/admin.json
```

**In a container**

```bash
docker build -t fluid-agent .
docker run -d --name fluid-agent -p 8080:8080 -p 8081:8081 -v fluid-data:/app/data fluid-agent
```

With no mounts the agent creates `/app/config.json` and `/app/dynamic/`. For the Traefik setup in [`compose.yml`](compose.yml): `cp config.docker.json config.json`, put `PROXY_ADMIN_TOKEN=...` in `.env`, set the host in `compose.yml`, then `docker compose up -d`. A bind-mounted file must exist on the host first (Docker would otherwise create a directory in its place, and the agent says so); a bind-mounted directory must be writable by uid 65532 for console edits.

**Then:** open the console through an SSH tunnel (`ssh -L 9000:127.0.0.1:9000 server`, then <http://localhost:9000>) and sign in with the token from `admin.json`. Add proxy clients, allowed hosts, webhooks and ingest rules there, or copy the samples from [`examples/dynamic`](examples/dynamic) (replace every CHANGE-ME value) and run `fluid-agent check`.

### First start: what is created

Existing files are never overwritten. Each item is logged as `info` with `kind: "deploy"`, `topic: "bootstrap"` and its absolute path; anything that cannot be created (read-only mount, wrong owner) is logged as `warn` and the agent runs on built-in defaults.

| Path (defaults) | Contents |
|---|---|
| `config.json` (`-config`) | every static setting with its default value and `$schema` |
| `dynamic/` + `webhooks/`, `ingest/`, `profiles/` | empty collections |
| `dynamic/proxy.json` | no clients, no allowed hosts, port 443: every proxy request is refused until you add some |
| `dynamic/access.json` | proxy and console IP allowlists, set to the **restricted default**: loopback and private networks only (local use works; the internet is refused until you add addresses) |
| `dynamic/admin.json` (mode 0600) | one editor token named `admin`, random 64 hex chars; the value is never logged. With `PROXY_ADMIN_TOKEN` set it is created with no tokens |
| `data/` (`database.path`) | the SQLite database |
| `data/acme/` or `acme.cache_dir` (mode 0700) | certificate cache, only when ACME is configured |

### Commands

| Command | What it does |
|---|---|
| `fluid-agent [serve] [-config config.json]` | create anything missing, then run the agent |
| `fluid-agent init [-config …]` | create anything missing and validate, without serving (exit 1 on invalid files or items it could not create) |
| `fluid-agent check [-config …]` | validate config.json and every dynamic file, print deployment notes; writes nothing |
| `fluid-agent logs [flags]` | query the SQLite log store ([below](#logs-sqlite-and-querying)) |
| `fluid-agent healthcheck [-config …]` | exit 0 if the local agent is live (container health checks) |
| `fluid-agent version` | print the version |

## Where it can run

On a host (systemd) or in a container; directly with built-in HTTPS or behind nginx, Traefik or Caddy; on a root domain, a subdomain or a path. See **[deploy/README.md](deploy/README.md)** for the full matrix and ready-made configs, all covered by an end-to-end test behind the real proxies.

* `webhook_base_path`, `admin_base_path`, `proxy_base_path` mount a listener under a path; requests work whether or not the reverse proxy strips the prefix. The console also honours `X-Forwarded-Prefix` from a trusted proxy.
* `webhook_tls`, `admin_tls`, `proxy_tls` serve HTTPS from certificate files (reloaded on renewal) or automatic ACME/Let's Encrypt certificates. `proxy_tls` turns the forward proxy into an `https://` proxy.
* The HTTP/CONNECT forward proxy cannot go through an HTTP reverse proxy; expose it directly (firewalled), use `proxy_tls`, or TCP passthrough (nginx `stream`, Traefik TCP router).

## Configuration: the file provider

`config.json` is read once at startup (created with defaults if missing). Everything else lives in `dynamic_dir`, is checked every `reload_interval`, and is written back by the console/API (atomic temp-file + rename). Every file is decoded strictly (unknown keys are errors) and carries a `$schema` for editor validation.

```
config.json                     static: listeners, database, outbox, telemetry   (schema/config.schema.json)
dynamic/
  proxy.json                    proxy clients, allowed hosts/ports, private-address rules  (proxy.schema.json)
  admin.json                    console/API tokens with role viewer | editor               (admin.schema.json)
  access.json                   client IP allowlists for the proxy and the console         (access.schema.json)
  webhooks/<id>.json            one webhook per file                                       (webhook.schema.json)
  ingest/<id>.json              one callback ingest rule per file                          (ingest.schema.json)
  profiles/<name>.json          one /forward profile per file                              (profile.schema.json)
```

* A file that fails validation is reported (`fluid-agent check`, the Status page, `fluid_config_errors` metric) and **the previous good version keeps running**.
* `admin.json` is never writable over the API, so a viewer token can never become an editor token. `PROXY_ADMIN_TOKEN` adds one extra editor token (`name: env`).
* Rotate any token by editing the file — no restart.

## Access: who may call what

**Each webhook and ingest rule decides who may call it**, in its own file, with `allowed_ips`:

```json
{ "id": "razorpay", "target": "https://dev.internal/cb", "allowed_ips": ["52.66.*.*", "13.232.0.0/16"] }
{ "id": "github",   "target": "https://ci.internal/hook", "allowed_ips": ["140.82.112-127.*", "192.30.252.0/22"] }
{ "id": "partner",  "target": "https://dev.internal/p",   "allowed_ips": ["*"], "auth": { "hmac": { "header": "X-Signature", "secret": "…" } } }
```

(The addresses are illustrative; use each provider's published IP ranges.)

| Rule | Matches |
|---|---|
| `*` | anyone |
| `203.0.113.7`, `2001:db8::1` | one address |
| `52.66.0.0/16`, `2001:db8::/32` | a CIDR |
| `52.66.*.*`, `52.66.*` | IPv4 wildcard octets (a trailing `*` covers the rest) |
| `140.82.112-127.*`, `52.66.10-20.0-127` | IPv4 octet ranges |

* **Restricted by default.** A route without `allowed_ips` accepts only loopback and private networks (`127.0.0.0/8`, `::1`, `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `fc00::/7`): it works locally and inside your network, and internet senders get `403` until you list them. `["*"]` lets anyone in (pair it with `auth`).
* `source_cidrs` is accepted too (same syntax, existing files keep working); when both are set the lists are combined.
* Refusals are logged with the route, `source_ip` and `403`, before authentication or reading the body.

**The proxy and the console** have allowlists in `dynamic/access.json`, with the same rules and the same restricted default:

```json
{
  "proxy": { "allow": ["10.0.0.0/8", "203.0.113.7"] },
  "admin": { "allow": ["127.0.0.1", "::1", "10.8.0.*"] }
}
```

`"allow": []` refuses everyone. Editors change the proxy list on the console's **Access** page or with `PUT /api/access`; the **admin list is file-only**, so nobody can lock the console or open it to the internet over the API. Loopback health checks (`/healthz`, `/livez`, `/readyz`) always pass.

**Warnings.** At startup, in `fluid-agent check`, on the Status and Access pages, and again when a route or `access.json` changes, the agent explains who may call what: a route on the restricted default (`warn`: internet providers get 403), a route open to `*` without auth (`warn`), `*` on the proxy or console (`warn`), an empty list or a loopback-only console that excludes loopback (`error`), and container NAT that may hide the real client address (`warn`). Behind a reverse proxy, list it in `trusted_proxies` so the real client from `X-Forwarded-For` is checked.

The **Access** page lists every route with its allowed IPs and has an address tester; the same is available over the API:

```bash
curl -H "$A" $API/access                                          # routes, proxy and console lists, warnings
curl -H "$A" -X PATCH $API/webhooks/razorpay -d '{"allowed_ips":["52.66.*.*"]}'
curl -H "$A" -X POST $API/access/test -d '{"ip":"52.66.10.20"}'   # allowed? and which rule matched, per route
```

## Outbound: proxy, `/forward` and profiles

```bash
export HTTPS_PROXY=http://dev:<CLIENT_TOKEN>@agent:8080        # any SDK; username is ignored
curl -x http://dev:<CLIENT_TOKEN>@agent:8080 https://api.razorpay.com/v1/...

# JSON relay (binary-safe: body_base64 in and out)
curl -X POST http://agent:8080/forward -H "X-Agent-Key: <CLIENT_TOKEN>" \
  -d '{"method":"GET","url":"https://api.razorpay.com/v1/payments"}'

# Profile: the agent adds the provider credentials; the client never holds them
curl -X POST http://agent:8080/forward -H "X-Agent-Key: <CLIENT_TOKEN>" \
  -d '{"profile":"razorpay","method":"GET","path":"/payments?count=10"}'
```

Safety rules:

* Destinations must match `allowed_hosts` (exact or `*.domain`) **and** `allowed_ports`.
* The resolved IP is checked at connect time (also defeats DNS rebinding): loopback, private, link-local/metadata (`169.254.169.254`), CGNAT, multicast and IPv6 forms that embed them (mapped, NAT64, 6to4, Teredo) are refused unless `allow_private` or `private_allowlist` says otherwise.
* `/forward` follows redirects itself, re-checking every hop, refusing https→http, and dropping profile credentials and `Authorization`/`Cookie` on cross-origin hops.
* Pooled connections are closed whenever the rules change, so revocations apply at once.
* CONNECT tunnels close after `tunnel_idle_timeout` without traffic.

## Inbound: webhooks and ingest rules

**Webhooks** — `POST /hooks/razorpay-dev/x?y=1` is relayed to `target + /x?y=1`.

* `async: false` (default): the target's answer is passed back to the sender.
* `async: true`: the event is stored in SQLite **before** replying `202`, then delivered in the background.
* `mirrors`: extra targets, always delivered durably in the background.

**Ingest rules** — any other path is matched against `ingest/*.json` (highest `priority` first; `exact`, `prefix`, `suffix` or `regex`), stored, acknowledged with the rule's `response` (default `200 {"status":"success"}`), and delivered to every `targets` URL with `X-Fluid-Vendor`, `X-Fluid-Event-Type` and `X-Fluid-Path` headers. An unmatched path gets `404`. If the event cannot be stored the caller gets `503`, never a false acknowledgement.

Options shared by both (all optional):

| Field | Meaning |
|---|---|
| `methods` | allowed methods (default `POST`, `*` = any) |
| `allowed_ips` | who may call this route: IPs, CIDRs, IPv4 wildcards (`52.66.*.*`, `52.66.1-5.*`) or `*`; omitted = loopback and private networks only ([Access](#access-who-may-call-what)) |
| `source_cidrs` | same as `allowed_ips` (kept for existing files); combined with it |
| `auth.token` | shared token in a header (`{"header","value"}`) |
| `auth.basic` | HTTP Basic credentials |
| `auth.hmac` | signature of the raw body: `header`, `secret`, `algorithm` sha1/sha256/sha512, `encoding` hex/base64, `prefix` (e.g. `sha256=` for GitHub). Razorpay: `{"header":"X-Razorpay-Signature","secret":"…"}` |
| `rate_limit` | per-IP token bucket (overrides `webhook_rate_limit`) |
| `target_auth` | credentials sent **to** the target: `bearer`, `basic` or `header` |
| `add_headers` | extra headers on relayed requests |
| `max_retries`, `timeout_sec` | delivery retries (default 0 sync / 8 async) and per-attempt timeout |

When several auth methods are set, all must pass. Credentials the sender used for the agent are never stored or relayed; signature headers are relayed so targets can verify them too.

Delivery: network errors, `408`, `429` and `5xx` are retried with exponential backoff and jitter (up to `outbox.max_backoff`); other `4xx` are final. Exhausted deliveries become **dead letters** and can be retried from the console or with `POST /api/deliveries/{id}/retry` / `POST /api/deliveries/retry-dead`. After a crash, in-flight async deliveries are resumed. Every request carries `X-Fluid-Event-Id` for de-duplication and `traceparent` for tracing.

Behind a reverse proxy, list it in `trusted_proxies`; `X-Forwarded-For` is then walked from the right, so senders cannot spoof an allowed IP.

## Logs (SQLite) and querying

Every proxy call, tunnel, forward, webhook, ingest, delivery, admin change and system message is stored with columns for `kind`, `direction` (**`inbound`** for calls arriving at the agent, **`outbound`** for calls it makes), `client`, `route`, `method`, `host`, `url`, `status`, bytes, duration, source IP and trace id. Retention: `database.retention_days`.

```bash
fluid-agent logs -direction inbound -since 24h               # incoming calls
fluid-agent logs -direction outbound -status 5xx -since 1h   # failing outgoing calls
fluid-agent logs -kind delivery -route razorpay-dev -q timeout
fluid-agent logs -by host -direction outbound -since 7d      # summary table
fluid-agent logs -json -limit 1000 | jq .
```

The same filters are available at `GET /api/logs` and `GET /api/logs/summary?by=kind|direction|route|client|host|status|source_ip|level|method`, and in the console's Logs page (list + summary, live updates).

### Full request and response of every API call

Every logged API call also stores what went over the wire, **in full**: request headers (**RQH**), request body (**RQB**), response headers (**RSH**) and response body (**RSB**).

| Call | What is captured |
|---|---|
| Webhook / ingest (inbound) | the request as the sender sent it, and the agent's answer (including a passed-through target answer); refused calls (auth, method, IP, rate limit) too |
| Delivery (outbound) | the request sent to the target (with `target_auth` and `add_headers`) and the target's answer, per attempt |
| `/forward` (outbound) | the request sent upstream (with profile credentials) and the upstream answer; if the upstream is never reached, the client's `/forward` call and the agent's answer |
| HTTP proxy (outbound) | the request sent upstream and the answer, bodies copied as they stream |
| HTTPS tunnel (`CONNECT`) | the CONNECT request and answer only: the traffic inside is end-to-end encrypted |

* Headers include the request line as `:method` and `:path` and the answer's `:status`.
* Bodies are stored up to `database.capture_max_body_bytes` (default 10 MiB) each; the full size is always recorded and longer bodies are marked truncated. `capture_max_body_bytes: 0` stores headers only, `capture_payloads: false` turns capture off. Stdout logs show body sizes only.
* **Stored raw.** Editors see every value exactly as sent, credentials included. **Viewer** tokens see the same with credentials masked (`********`): `Authorization`, `Proxy-Authorization`, cookies, `X-Agent-Key`, API-key headers, each route's token header, and any value equal to a secret the agent knows (client and admin tokens, route and target credentials, profile headers). Protect the database file accordingly.
* `GET /api/logs/{id}` returns one row with its `payload` (`request`/`response`, each with `headers`, `body` or `body_base64`, `size`, `stored`, `truncated`). In the console, open a log row: RQH, RQB, RSH and RSB are shown with copy buttons; rows with a capture are marked `RQ/RS`.

### Tables: column filters, multi-column sort, paging, CSV

Every table in the console (events, deliveries, logs, webhooks, ingest rules, profiles, proxy clients, access) has a filter under each column, sorting by several columns (click a header, Shift+click to add one), page sizes from 10 to 200 with first/previous/next/last, page numbers and jump-to-page, and **Export CSV** of every row that matches the current search, filters and sort. Dates and times are shown in your browser's time zone (named in the column header); the tooltip gives the UTC time.

The list APIs take the same parameters:

```bash
curl -H "$A" "$API/logs?f.kind==delivery&f.status=5xx&f.ts=24h..&sort=duration_ms:desc,ts:desc&offset=50&limit=50"
curl -H "$A" "$API/events?f.route==razorpay&f.ts=2026-11-17T00:00:00Z..2026-11-18T00:00:00Z" # {items,total,offset,limit,sort}
curl -H "$A" -o events.csv "$API/events?format=csv&tz=Asia/Kolkata&f.status==failed"         # every matching row
```

| Parameter | Meaning |
|---|---|
| `f.<column>=value` | text columns: contains (case-insensitive), `=value` for exact; number columns: `500`, `5xx`, `>=400`, `<100`, `200-299`; time columns: `from..to` (RFC3339 or `1h`, `7d` ago), `>=…`, `<…` |
| `sort=col:desc,col2:asc` | multi-column sort |
| `offset`, `limit` | paging (limit up to 500); the response has `total` |
| `format=csv&tz=<IANA zone>` | CSV of every matching row (up to 100,000), times in that zone; cells starting with `= + - @` are prefixed with `'` so spreadsheets do not run them as formulas |

## Console and API

The console is a plain-JS single page embedded in the binary (no build step, strict CSP). It has Overview, Events (body, headers and deliveries in a side panel), Deliveries (list + Kanban board with dead-letter retry), Logs, Webhooks, Ingest rules, Forward profiles, Proxy & clients and Status pages, with live updates over Server-Sent Events.

* **Viewer** tokens can read everything; secrets are masked and `add_headers` values hidden.
* **Editor** tokens can also create, edit and delete webhooks, ingest rules, profiles, proxy rules and clients, and retry deliveries. Secrets are write-only: sending the mask `********` back keeps the stored value.

```bash
A="Authorization: Bearer $TOKEN"; API=http://127.0.0.1:9000/api
curl -H "$A" $API/whoami
curl -H "$A" -X POST $API/webhooks -d '{"id":"razorpay-dev","target":"https://dev.internal/cb","async":true,
  "auth":{"hmac":{"header":"X-Razorpay-Signature","secret":"…"}}}'
curl -H "$A" -X PATCH $API/webhooks/razorpay-dev -d '{"disabled":true}'
curl -H "$A" "$API/events?route=razorpay-dev&status=failed"
curl -H "$A" -X POST $API/proxy/clients -d '{"name":"ci"}'          # returns the generated token once
curl -H "$A" -N "$API/stream?topics=logs,events,deliveries"        # SSE
curl -H "$A" http://127.0.0.1:9000/metrics                         # Prometheus
```

Health: `/healthz`, `/livez` and `/readyz` on the webhook listener; `fluid-agent healthcheck` for container health checks.

## Tracing

Set `telemetry.otlp_endpoint` (or `OTEL_EXPORTER_OTLP_ENDPOINT`) to export OpenTelemetry traces over OTLP/HTTP. Incoming `traceparent` headers are continued; background deliveries join the trace of the call that produced them, and log rows record the trace id.

## Hardening checklist

* Give every webhook and ingest rule `allowed_ips` with only its provider's published ranges, and keep `access.json` tight: your application servers for `proxy`, your office/VPN for `admin`. Firewall the ports as well; keep the admin listener on localhost.
* Keep `allowed_hosts` to the providers you need; never enable `allow_private` unless required.
* Use long random tokens (`openssl rand -hex 32`); give most people **viewer** tokens. Keep the generated `admin` editor token secret (`admin.json` is created 0600), or replace it.
* Turn on `auth` for every webhook/ingest rule whose sender supports it (HMAC where offered).
* Set `trusted_proxies` to exactly your reverse proxy.
* Set `database.store_event_bodies: false` if payloads must not be kept after delivery.
* Webhook and ingest targets are trusted editor configuration and may be internal hosts, so the private-address guard does not apply to them; only give editor tokens to people who may choose delivery targets.

## Development

```bash
go test -race ./...
node --check ui/app.js
./deploy/e2e/test.sh            # behind real nginx, Traefik and Caddy (Docker or Podman)
go run . check -config config.example.json
go run . init -config /tmp/fa/config.json   # what a first start creates
```

## Releases

Prebuilt binaries for Linux, macOS and Windows (amd64/arm64) are on the [Releases](https://github.com/skpjr001/fluid-agent/releases) page, with `checksums.txt`. To cut a release, push a tag:

```sh
git tag v0.3.0 && git push origin v0.3.0
```

`fluid-agent version` prints the version a binary was built from.
