# Easy Proxies

[简体中文](README_ZH.md)

Easy Proxies is a sing-box based proxy pool manager.

It focuses on turning many upstream nodes into one stable local HTTP/SOCKS5 proxy entry, while still supporting per-node ports when needed.

## What It Does

- Supports `pool`, `multi-port`, and `hybrid` runtime modes.
- Builds upstream outbounds for: `vmess`, `vless`, `trojan`, `ss`/`shadowsocks`, `hysteria2`/`hy2`, `socks5`/`socks`, `http`/`https`.
- Supports node sources:
  - inline `nodes:` in `config.yaml`
  - `nodes_file` (one URI per line)
  - `subscriptions` (Base64/plain text/Clash YAML parsing)
- Provides automatic health checks and node blacklist recovery.
- Provides Web dashboard + API for:
  - node status/probe/export
  - settings update (`external_ip`, `probe_target`, `skip_cert_verify`)
  - config node CRUD + reload
  - subscription status + manual refresh
- Adds configurable DNS resolver for outbound domain resolution (important for VMess nodes with domain hosts).
- Optional GeoIP labeling with auto-update and hot-reload (region/country metadata in dashboard, pool mode only).

## Quick Start

### 1) Prepare config

```bash
cp config.example.yaml config.yaml
cp nodes.example nodes.txt
```

Edit `config.yaml` and your node source (`nodes.txt`, `subscriptions`, or inline `nodes`).

### 2) Run

Docker:

```bash
./start.sh
# or
docker compose up -d
```

Local:

```bash
go run ./cmd/easy_proxies -config config.yaml
```

## Minimal Config (Pool Mode)

```yaml
mode: pool

listener:
  address: 0.0.0.0
  port: 2323
  username: user
  password: pass

pool:
  mode: sequential    # sequential / random / balance
  failure_threshold: 3
  blacklist_duration: 24h

management:
  enabled: true
  listen: 0.0.0.0:9091
  probe_target: http://cp.cloudflare.com/generate_204
  password: ""

dns:
  server: 223.5.5.5
  port: 53
  strategy: prefer_ipv4

nodes_file: nodes.txt
```

## DNS Resolver Config

`dns` controls domain resolution used by sing-box DNS client and VMess domain dialing:

```yaml
dns:
  server: 223.5.5.5
  fallback_servers:    # Fallback DNS servers (used when primary fails)
    - 8.8.8.8
    - 1.1.1.1
  port: 53
  strategy: prefer_ipv4
```

Allowed `strategy` values:

- `as_is`
- `prefer_ipv4`
- `prefer_ipv6`
- `ipv4_only`
- `ipv6_only`

If you see logs like `lookup <domain>: empty result`, set a reachable resolver and an explicit strategy.

## Runtime Modes

- `pool`: one HTTP/SOCKS5 entry for all nodes.
- `multi-port`: one local HTTP/SOCKS5 port per node.
- `hybrid`: pool + multi-port together.

## Node Source Behavior

- If `subscriptions` is set:
  - subscription nodes are fetched and appended
  - `nodes_file` is used as output path for fetched nodes
  - `nodes_file` loading is skipped at startup
- Inline `nodes` always participate when present.

## Protocol Notes

Runtime builder supports:

- `vmess`
- `vless`
- `trojan`
- `ss` / `shadowsocks`
- `hysteria2` / `hy2`
- `socks5` / `socks`
- `http` / `https`

Parser may recognize additional URI prefixes in subscription text (for compatibility), but unsupported schemes are skipped during build.

## Management API

Main endpoints:

- `POST /api/auth`
- `GET|PUT /api/settings`
- `GET /api/nodes`
- `POST /api/nodes/{tag}/probe`
- `POST /api/nodes/{tag}/release`
- `POST /api/nodes/probe-all` (SSE)
- `GET /api/export`
- `GET|POST /api/subscription/status|refresh`
- `GET|POST|PUT|DELETE /api/nodes/config[...]`
- `POST /api/reload`

When `management.password` is empty, API/UI auth is bypassed.

## Important Operational Notes

- Reload (`/api/reload` or subscription refresh) interrupts active connections.
- Settings API persists values to `config.yaml`; some changes require reload to fully take effect.
- Default normalization values (when omitted) are in `internal/config/config.go`.

## Development

```bash
go test ./...
```

