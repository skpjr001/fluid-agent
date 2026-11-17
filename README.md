# fluid-agent

Single Go binary, zero dependencies. Runs three listeners on the whitelisted server:

| Listener | Default | Purpose |
|---|---|---|
| Forward proxy | `:8080` | HTTP + `CONNECT` tunnel. Token auth + host/port allowlist. |
| Webhook ingress | `:8081` | `/hooks/{id}[/sub/path]` → relayed to the webhook's `target`. |
| Admin API | `127.0.0.1:9000` | CRUD webhooks, view deliveries, reload. Bearer token. |

## Config layout (Traefik-style)

```
config.json                 # static: ports, admin token, dynamic_dir (read once)
dynamic/
  proxy.json                # tokens, allowed_hosts, allowed_ports (hot-reloaded)
  webhooks/<id>.json        # one file per webhook (API writes these; hand edits also hot-reload)
```

* Files are polled every `reload_interval` (default 2s). A bad file is logged and skipped; the last good version stays live.
* API writes are atomic (temp file + rename), so a half-written file is never read.
* `PROXY_ADMIN_TOKEN` env overrides `admin_token`.

## Run

```bash
go build -o fluid-agent .
cp config.example.json config.json   # set admin_token or export PROXY_ADMIN_TOKEN
./fluid-agent -config config.json
```

Docker/Traefik: `cp config.docker.json config.json`, put `PROXY_ADMIN_TOKEN=...` in `.env`, edit the host in `compose.yml`, then `docker compose up -d` (or `podman-compose`).
Set `trust_forwarded_for: true` only when the ingress is behind Traefik/Nginx. Otherwise `source_cidrs` checks can be spoofed.

## Using the forward proxy

```bash
curl -x http://dev:<TOKEN>@proxy.server:8080 https://api.razorpay.com/v1/...
export HTTPS_PROXY=http://dev:<TOKEN>@proxy.server:8080    # most SDKs / Java: -Dhttps.proxyHost etc.
```
Username is ignored; the password is the token. `Proxy-Authorization: Bearer <TOKEN>` also works.
`allowed_hosts` supports exact names and `*.domain.com` (subdomains only).

## Admin API

```bash
A="Authorization: Bearer $PROXY_ADMIN_TOKEN"; API=http://127.0.0.1:9000/api

# create
curl -H "$A" -X POST $API/webhooks -d '{
  "id": "razorpay-dev",
  "target": "https://my-dev-box.example.com/payments/callback",
  "methods": ["POST"],
  "source_cidrs": ["52.66.0.0/16"],
  "add_headers": {"X-Env": "dev"},
  "async": false,
  "max_retries": 2,
  "timeout_sec": 15
}'
# -> public URL: https://hooks.example.com/hooks/razorpay-dev

curl -H "$A" $API/webhooks                               # list
curl -H "$A" $API/webhooks/razorpay-dev                  # get
curl -H "$A" -X PATCH $API/webhooks/razorpay-dev -d '{"enabled":false}'   # partial update
curl -H "$A" -X PUT   $API/webhooks/razorpay-dev -d '{...full object...}' # replace / upsert
curl -H "$A" -X DELETE $API/webhooks/razorpay-dev
curl -H "$A" $API/webhooks/razorpay-dev/deliveries      # last 50 deliveries (metadata only, no bodies)
curl -H "$A" $API/proxy                                  # current proxy rules (tokens masked)
curl -H "$A" -X POST $API/reload                         # force reload
```

### Webhook fields

| Field | Default | Notes |
|---|---|---|
| `id` | required | `[a-z0-9][a-z0-9_-]{0,63}`, also the filename and URL segment |
| `target` | required | absolute http(s) URL; `/hooks/{id}/x/y?q` → `target/x/y?q` |
| `methods` | `["POST"]` | `"*"` allows any method |
| `enabled` | `true` | disabled → 404 |
| `async` | `false` | `true`: reply 202 immediately and deliver in the background with retries |
| `max_retries` | `0` | retries on network error / 5xx, exponential backoff from 500ms |
| `timeout_sec` | `15` | per attempt |
| `source_cidrs` | none | allowlist sender IPs/CIDRs |
| `add_headers` | none | set on the relayed request (plus `X-Webhook-Id`, `X-Forwarded-For/Host`) |

## Hardening checklist
- Firewall `:8080` to your office/VPN IPs; this server's IP is whitelisted with your providers.
- Keep `allowed_hosts` to the exact providers you need. Never use `*`.
- Keep admin on localhost and reach it via `ssh -L 9000:127.0.0.1:9000 server`.
- Use long random tokens (`openssl rand -hex 32`) and rotate them by editing `proxy.json`; no restart needed.

## Releases

Prebuilt binaries for Linux, macOS and Windows (amd64/arm64) are on the [Releases](https://github.com/skpjr001/fluid-agent/releases) page, with `checksums.txt`. To cut a release, push a tag:

```sh
git tag v0.1.0 && git push origin v0.1.0
```

`fluid-agent -version` prints the version a binary was built from.
