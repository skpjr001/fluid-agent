# Deploying fluid-agent

fluid-agent runs as a plain binary (systemd), in a container, with or without a reverse proxy, on a root domain, a subdomain or a path. Every layout below is exercised by tests: the Go suite emulates stripping and non-stripping proxies, and [`e2e/test.sh`](e2e/test.sh) runs the agent behind real nginx, Traefik and Caddy (also in CI).

## What works where

| Listener | Direct (built-in TLS) | Subdomain | Root domain | Path, proxy strips prefix | Path, proxy keeps prefix |
|---|---|---|---|---|---|
| Webhooks / ingest `:8081` | ✅ `webhook_tls` | ✅ | ✅ | ✅ | ✅ with `webhook_base_path` |
| Console + API `:9000` | ✅ `admin_tls` | ✅ | ✅ | ✅ with `admin_base_path` or `X-Forwarded-Prefix` | ✅ with `admin_base_path` |
| `POST /forward` `:8080` | ✅ `proxy_tls` | ✅ | ✅ | ✅ | ✅ with `proxy_base_path` |
| HTTP/CONNECT proxy `:8080` | ✅ `proxy_tls` (https:// proxy URL) | TCP passthrough only | TCP passthrough only | ❌ (not HTTP routing) | ❌ |

* **Base paths** (`proxy_base_path`, `webhook_base_path`, `admin_base_path`) accept requests both with and without the prefix, so it does not matter whether the reverse proxy strips it. The console learns its external prefix from `admin_base_path`, or from `X-Forwarded-Prefix` sent by a peer in `trusted_proxies` (Traefik's StripPrefix sets it; for Caddy add `header_up X-Forwarded-Prefix /console`).
* **The forward proxy** speaks HTTP proxy protocol (`CONNECT`, absolute URLs), which HTTP reverse proxies cannot route. Expose it directly (firewalled), give it `proxy_tls`, or pass it through as TCP (nginx `stream {}`, Traefik TCP router). Caddy's core has no TCP proxy.
* **IP allowlists** are restricted by default to loopback and private networks: each webhook and ingest rule takes `allowed_ips` in its own file, and the proxy and console take theirs from `dynamic/access.json`. Internet senders get `403` until their addresses are listed (wildcards like `52.66.*.*` work). Behind a reverse proxy the real client address is checked only if the proxy is in `trusted_proxies`; in a container with published ports, rootless Podman or Docker's userland proxy can replace it with the gateway address, which the agent warns about.
* **Behind any proxy** set `trusted_proxies` to the proxy's address so `source_cidrs`, rate limits and logs see the real sender. Configure the proxy to *overwrite* `X-Forwarded-For` (`$remote_addr`) where possible.

## First start creates what is missing

Every layout below starts from an empty directory: the agent creates `config.json` (all defaults), `dynamic/` (`webhooks/`, `ingest/`, `profiles/`, deny-by-default `proxy.json`, `access.json` restricting the proxy and console to loopback and private networks, `admin.json` with a generated editor token unless `PROXY_ADMIN_TOKEN` is set), the database directory and ACME caches, and logs each with `topic: "bootstrap"`. Existing files are never touched. `fluid-agent init` does it without serving (the systemd unit runs it as `ExecStartPre`).

| Where | What you do | What is created |
|---|---|---|
| Host | run `fluid-agent` | `./config.json`, `./dynamic/…`, `./data/` |
| systemd | `useradd fluid`, install the binary and unit | `/var/lib/fluid-agent/{config.json,dynamic/,data/}` (systemd's `StateDirectory` creates the parent) |
| Container, no mounts / named volumes | `docker run` | `/app/config.json`, `/app/dynamic/…`, `/app/data/` |
| Container, bind mounts | the mounted *file* must exist on the host (otherwise Docker creates a directory, and the agent says so); a mounted *directory* must be writable by uid 65532 | missing files and sub-directories inside the mounts |

If something cannot be created (read-only mount, wrong owner) it is logged as a warning and the agent runs on built-in defaults.

## Deployment notes in the logs

At startup the agent inspects how it is running and logs every caveat that applies, as `info`, `warn` or `error` records with `kind: "deploy"` (stored with the other logs, shown on the console's Status page, and printed by `fluid-agent check`):

| Situation | Examples of what is logged |
|---|---|
| Access | who may call each webhook / ingest rule: on the restricted default (warn), open to `*` without auth (warn); proxy and console lists: `*` (warn), empty or an unreachable loopback console (error); container NAT hiding client addresses (warn). New notes are logged when a route or `access.json` changes |
| First start | each directory and file created (`topic: bootstrap`), or a warning when it could not be |
| Host / systemd / container | where it runs; root on a host (warn); in a container: the host's IP is the egress IP, database or config not on a volume (warn), read-only config mount (warn) |
| Each listener | `scheme://addr (all interfaces / loopback only), path /base`; privileged ports and `CAP_NET_BIND_SERVICE` |
| Forward proxy | cannot go behind an HTTP reverse proxy; plain HTTP on all interfaces (warn on a host); `https://` proxy URL guidance with `proxy_tls` |
| Webhooks | plain HTTP with no trusted reverse proxy (warn); which proxies are trusted; over-broad `trusted_proxies` (warn); every route without auth or `source_cidrs` (warn) |
| Console | plain HTTP on all interfaces (warn on a host; publish-to-localhost advice in a container); base path or `X-Forwarded-Prefix`; no tokens / viewer-only tokens |
| TLS | certificate names and expiry, expiring within 21 days (warn), expired (error), missing SANs (warn); ACME directory, challenge type, and an unreachable challenge (warn) |

`fluid-agent logs -kind deploy -since 1h` lists them again later.

## Built-in TLS (no reverse proxy)

```json
"webhook_addr": ":443",
"webhook_tls": {
  "acme": { "domains": ["hooks.example.com"], "email": "ops@example.com", "accept_tos": true,
            "http_challenge_addr": ":80" }
},
"admin_tls": { "cert_file": "/etc/fluid/console.crt", "key_file": "/etc/fluid/console.key", "min_version": "1.3" },
"proxy_tls": { "cert_file": "/etc/fluid/proxy.crt", "key_file": "/etc/fluid/proxy.key" }
```

* `cert_file`/`key_file` are re-read when the certificate file changes (e.g. after certbot renews); a broken renewal keeps the last good certificate.
* `acme` obtains certificates automatically (Let's Encrypt; `directory_url` for staging or another CA, `ca_file` to trust a private ACME server such as step-ca). TLS-ALPN-01 works when the listener is on `:443`; otherwise set `http_challenge_addr` (`":80"`), which also redirects plain HTTP to HTTPS. Certificates are cached in `cache_dir` (default `<database dir>/acme`).
* `proxy_tls` makes the proxy an *HTTPS proxy*: `HTTPS_PROXY=https://name:<token>@agent:8443`. It is HTTP/1.1-only so `CONNECT` works. curl, Go and most modern clients support https:// proxy URLs; some SDKs only accept http://.
* Ports below 1024 need `CAP_NET_BIND_SERVICE` (systemd: `AmbientCapabilities=CAP_NET_BIND_SERVICE`) or a container port mapping.

## Files

| Path | What |
|---|---|
| [`systemd/fluid-agent.service`](systemd/fluid-agent.service) | Hardened unit for a host install |
| [`nginx/fluid-agent.conf`](nginx/fluid-agent.conf) + [`fluid-proxy.conf`](nginx/fluid-proxy.conf) | Subdomains, one domain with paths (stripped and not), `stream {}` for the proxy |
| [`traefik/compose.yml`](traefik/compose.yml) | Traefik v3: subdomain, path with StripPrefix, TCP router for the proxy, Let's Encrypt |
| [`caddy/Caddyfile`](caddy/Caddyfile) | Caddy: subdomain, `handle_path` with `X-Forwarded-Prefix` |
| [`e2e/`](e2e/) | Runnable test of the same routing behind nginx, Traefik and Caddy (`./deploy/e2e/test.sh`) |

The e2e suite also starts an agent with no mounts at all and checks what it creates, and runs agents with certificate-file TLS on every listener (no reverse proxy), and with ACME against [Pebble](https://github.com/letsencrypt/pebble) using both TLS-ALPN-01 and HTTP-01, and verifies the deployment notes each setup logs. The reverse-proxy variants are the same routing as the production examples on plain HTTP. Run the e2e suite with Docker, or with Podman via `DOCKER_HOST=unix:///run/user/$UID/podman/podman.sock` (or `COMPOSE="podman compose"`).
