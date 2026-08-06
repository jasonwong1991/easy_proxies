# Easy Proxies

[简体中文](README_ZH.md)

> A sing-box based proxy pool manager -- aggregate many upstream proxy nodes into one stable, health-checked, load-balanced local proxy endpoint.

## Features

- **Three runtime modes**: `pool` (single-port load balancing), `multi-port` (one port per node), and `hybrid` (both simultaneously)
- **Wide protocol support**: VLESS, VMess, Trojan, Shadowsocks, Hysteria2, TUIC, AnyTLS, SOCKS5, HTTP/HTTPS
- **Automatic health checking** with configurable failure thresholds and blacklist duration, plus manual blacklist/release from the dashboard
- **Automatic fail-over retry**: when a node's dial fails, the request is retried on another healthy node (configurable attempts)
- **GeoIP region routing**: classify nodes by country and route traffic through a specific region via a dedicated HTTP proxy endpoint
- **Sticky sessions**: optional dedicated port that pins each client (by source IP) to a fixed upstream node for a stable egress IP, coexisting with the rotating pool entry
- **Multiple node sources**: inline config, `nodes.txt` file, or subscription URLs (Base64, plain text, Clash YAML)
- **Subscription auto-refresh with hot-reload**: periodically fetches subscription updates and reloads without restart
- **Stable per-node ports**: in `multi-port`/`hybrid` mode each node keeps the same local port across subscription refreshes and restarts (persisted to `node_ports.json`)
- **WebUI dashboard**: real-time node status, traffic charts, diagnostics, log console, and full settings management
- **Management API**: RESTful endpoints for node CRUD, probing, blacklisting, subscription management, and config reload
- **Configurable DNS resolver** with fallback servers and IPv4/IPv6 strategy control
- **Log rotation**: size-based rotation with configurable backup count, age, and compression
- **Multi-platform Docker**: supports amd64 and arm64 with host networking

## Quick Start

### 1. Prepare Configuration

```bash
cp config.example.yaml config.yaml
touch nodes.txt
```

Edit `config.yaml` and add your proxy nodes (inline nodes, `nodes.txt` file, or subscription URLs).

> Why `touch nodes.txt`? If you bind-mount a host *file* path that doesn't exist yet (e.g. `-v ./data/nodes.txt:/etc/easy_proxies/nodes.txt`), Docker creates a **directory** named `nodes.txt` on the host and mounts that. The container then sees a directory where it expects a file and fails opaquely. Pre-creating the file (or mapping the **directory** `./data:/etc/easy_proxies`, which auto-generates files) avoids this. If you hit it, run `rm -rf ./data/nodes.txt && touch ./data/nodes.txt` and restart.

### 2. Run with Docker (Recommended)

Zero-config: config files are auto-generated on first run.

```bash
mkdir -p data logs
docker run --user $(id -u):$(id -g) \
  -v $(pwd)/data:/etc/easy_proxies \
  -v $(pwd)/logs:/app/logs \
  --network host \
  ghcr.io/jasonwong1991/easy_proxies:latest
```

Or use docker compose:
```bash
docker compose up -d
```

### 3. Run from Source

```bash
go run ./cmd/easy_proxies --config config.yaml
```

### 4. Access WebUI

Open `http://localhost:9091` in your browser.

## Configuration

### Runtime Modes

| Mode | Description |
|------|-------------|
| `pool` | Single port proxy pool. All nodes share one port with load balancing |
| `multi-port` | One local port per node for direct access |
| `hybrid` | Both pool + multi-port simultaneously |

### Pool Scheduling

| Algorithm | Description |
|-----------|-------------|
| `sequential` | Round-robin through healthy nodes |
| `random` | Random node selection |
| `balance` | Least-connections balancing |
| `latency` | Pick the node with the lowest measured latency |

### Minimal Config Example

```yaml
mode: pool

listener:
  address: 0.0.0.0
  port: 2323
  username: user
  password: pass

pool:
  mode: sequential    # sequential / random / balance / latency
  failure_threshold: 3
  blacklist_duration: 24h
  retry_enabled: true # retry on another node when a dial fails
  retry_attempts: 3   # max total dial attempts per request

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

### Sticky Proxy (optional, pool/hybrid mode)

When enabled, a dedicated extra port is opened (default `listener.port + 1`, i.e. `2324`) that coexists with the regular `2323` entry. Clients connecting through the sticky port are pinned to a single upstream node by **source IP**, keeping the egress IP stable instead of rotating on every connection. The pin is permanent until the pinned node is blacklisted/removed. Listen address and credentials are inherited from `listener`.

```yaml
sticky:
  enabled: true
  port: 2324    # defaults to listener.port + 1 when omitted
```

### Full Config Reference

See [config.example.yaml](config.example.yaml) for the full documented configuration with all available options.

## GeoIP Region Routing

### Overview

When GeoIP is enabled, Easy Proxies automatically classifies your proxy nodes by geographic region and provides a separate HTTP proxy endpoint that lets you route traffic through nodes in a specific country/region.

### Supported Regions

| Code | Region |
|------|--------|
| `jp` | Japan 🇯🇵 |
| `kr` | South Korea 🇰🇷 |
| `us` | United States 🇺🇸 |
| `hk` | Hong Kong 🇭🇰 |
| `tw` | Taiwan 🇹🇼 |
| `sg` | Singapore 🇸🇬 |
| `other` | All other regions |

### Configuration

```yaml
geoip:
  enabled: true
  database_path: "./GeoLite2-Country.mmdb"
  listen: "0.0.0.0"          # defaults to listener.address if omitted
  port: 1221                  # defaults to listener.port if omitted
  auto_update_enabled: true   # auto-update the GeoIP database
  auto_update_interval: 24h   # check interval
```

The GeoIP router reuses the `listener.username` and `listener.password` for proxy authentication.

Key behaviors:
- The GeoIP database (MaxMind GeoLite2-Country) is **auto-downloaded** on first startup
- Auto-update is enabled by default (checks every 24h) with hot-reload -- no restart needed
- Node region classification happens automatically during startup and on every reload
- Nodes whose IP cannot be resolved or looked up are placed in the `other` category

### How to Use

The GeoIP router is an HTTP proxy that listens on its own port. Select a
region with the `X-Easyproxy-Region` proxy request header. This works for
both regular HTTP requests and HTTPS CONNECT tunnels without changing the
target URL.

```bash
# Route through Japanese nodes.
curl --proxy http://user:pass@localhost:1221 \
  --proxy-header 'X-Easyproxy-Region: jp' \
  https://www.google.com

# Route through US nodes.
curl --proxy http://user:pass@localhost:1221 \
  --proxy-header 'X-Easyproxy-Region: us' \
  http://example.com

# No header uses the global pool.
curl --proxy http://user:pass@localhost:1221 https://www.google.com
```

Supported values are `jp`, `kr`, `us`, `hk`, `tw`, `sg`, and `other`.
Unsupported values return `400 Bad Request`. The header is removed before a
regular HTTP request is forwarded to the target server.

#### Legacy Path Fallback

Path-based region selection remains available for clients that actually send
the path in the proxy request, including `CONNECT jp/example.com:443`.
Standard HTTP proxy clients do not transmit a path included in the proxy URL,
so a proxy URL such as `http://localhost:1221/jp/` cannot select a region.
Use a proxy-specific header facility instead.

### How It Works

1. On startup, each node's server IP is resolved and looked up in the MaxMind GeoLite2-Country database
2. Nodes are grouped into per-region pools (`pool-jp`, `pool-kr`, `pool-us`, etc.) with independent health checking
3. The GeoIP router listens on its own port and selects a region from a proxy request header
4. Matching requests are routed through the corresponding region pool; unmatched requests use the global pool
5. Each region pool uses the same scheduling algorithm configured in the `pool` section
6. DNS lookup results are cached to avoid repeated resolution on reload

## Supported Protocols

| Protocol | URI Schemes | Transport |
|----------|-------------|-----------|
| VLESS | `vless://` | TCP, WS, HTTP/2, gRPC, HTTPUpgrade; TLS/Reality/uTLS |
| VMess | `vmess://` | WS, HTTP/2, gRPC, HTTPUpgrade; TLS/uTLS |
| Trojan | `trojan://` | WS, HTTP/2, gRPC, HTTPUpgrade; TLS/Reality/uTLS |
| Shadowsocks | `ss://`, `shadowsocks://` | Direct; SIP002 and legacy whole-payload Base64 formats |
| Hysteria2 | `hysteria2://`, `hy2://` | QUIC-based |
| TUIC | `tuic://` | QUIC-based |
| AnyTLS | `anytls://` | TLS |
| SOCKS5 | `socks5://`, `socks5h://`, `socks://` | Direct |
| HTTP | `http://`, `https://` | Direct |

## Node Sources

### Inline Nodes

```yaml
nodes:
  - uri: "vless://uuid@server:443?security=tls&type=ws&path=/path#Name"
  - uri: "ss://base64(method:password@server:port)#Name"
```

### Nodes File

```yaml
nodes_file: nodes.txt
```

One proxy URI per line. Lines starting with `#` are comments.

### Subscriptions

```yaml
subscriptions:
  - "https://provider.example/api?token=xxx"

subscription_refresh:
  enabled: true
  interval: 1h
  timeout: 30s
  fetch_concurrency: 16
```

Supports Base64, plain text, and Clash YAML formats. When subscriptions are configured, fetched nodes are written to `nodes_file`. Subscription changes trigger automatic hot-reload without restart.

**Multi-source Node Merging**: When both `nodes` and `subscriptions` are configured:
- Inline nodes (defined in `nodes` array) and subscription nodes are merged together
- Subscription updates preserve inline nodes instead of overwriting them
- Node order: inline nodes first, followed by subscription nodes
- Each node's source (inline/subscription) is tracked and displayed in the management UI

**Stable Ports** (`multi-port`/`hybrid`): each node is identified by a stable key derived from its URI (ignoring the display name and parameter order), so a node keeps the same local port even when the subscription renames or reorders it. Assignments are saved to `node_ports.json` next to `config.yaml` and restored on restart.

## WebUI Dashboard

Access at `http://your-server:9091` (configurable via the `management` section).

Features:

- **Dashboard**: Real-time node status, traffic charts, region availability, latency monitoring
- **Node Config**: Add/edit/delete inline nodes and subscription URLs
- **Diagnostics**: Connectivity testing and node state export
- **Console**: Real-time application logs (last 1000 lines, WebSocket streaming)
- **Settings**: All configuration options editable from the browser, changes persist to `config.yaml`

When `management.password` is empty, authentication is bypassed.

## Management API

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/auth` | POST | Login with password |
| `/api/settings` | GET, PUT | Read/update settings |
| `/api/nodes` | GET | List all nodes with status |
| `/api/nodes/{tag}/probe` | POST | Test node connectivity |
| `/api/nodes/{tag}/blacklist` | POST | Manually blacklist a node |
| `/api/nodes/{tag}/release` | POST | Release node from blacklist |
| `/api/nodes/probe-all` | POST | Probe all nodes (SSE stream) |
| `/api/export` | GET | Export node configuration |
| `/api/subscription/config` | GET, PUT | Manage subscription URLs |
| `/api/subscription/status` | GET | Check subscription status |
| `/api/subscription/refresh` | POST | Trigger manual refresh |
| `/api/nodes/config` | GET, POST, PUT, DELETE | CRUD for node config |
| `/api/reload` | POST | Reload sing-box instance |

## Docker Deployment

### docker-compose.yml

The default setup uses host networking (recommended for automatic port management). Config directory is auto-generated on first run:

```yaml
services:
  easy_proxies:
    image: ghcr.io/jasonwong1991/easy_proxies:latest
    container_name: easy_proxies
    restart: unless-stopped
    network_mode: host
    user: "${UID:-10001}:${GID:-10001}"
    volumes:
      - ./data:/etc/easy_proxies
      - ./logs:/app/logs
```

### Important Notes

- **Zero-config**: When mapping a directory, `config.yaml` and `nodes.txt` are auto-generated on first run.
- **Permissions**: Use `--user $(id -u):$(id -g)` to match your host user for file access.
- **Multi-platform**: Supports amd64 and arm64 architectures.
- **Reload**: `/api/reload` and subscription refresh will interrupt active connections.

### Ports

| Port | Usage |
|------|-------|
| 2323 | Pool proxy entry (pool/hybrid mode) |
| 9091 | WebUI and Management API |
| 9092 | Internal Clash API (localhost only, traffic stats) |
| 1221 | GeoIP region router (when enabled, configurable) |
| 24000+ | Multi-port mode (one per node) |

In `multi-port`/`hybrid` mode, per-node ports are persisted to `node_ports.json` (next to `config.yaml`) and restored on restart, so each node keeps a stable port across restarts and subscription refreshes. Delete this file to force a clean reassignment.

## Troubleshooting

### Configuration Persistence Issues

**Problem**: Configuration changes made via WebUI are lost after container restart or rebuild.

**Quick Diagnosis**:
```bash
# Check your data directory layout and permissions
ls -la data/
[ -f data/config.yaml ]  || echo "missing data/config.yaml"
[ -d data/config.yaml ]   && echo "BUG: data/config.yaml is a directory (see Quick Start note)"
[ -d data/nodes.txt ]    && echo "BUG: data/nodes.txt is a directory (see Quick Start note)"
```

**Common Causes and Solutions**:

1. **File Permission Issues**:
   ```bash
   # Fix permissions
   chown -R $(id -u):$(id -g) data
   chmod 755 data
   chmod 644 data/config.yaml data/nodes.txt
   ```

2. **Incorrect Volume Mapping**:
   - Ensure `docker-compose.yml` uses `./data:/etc/easy_proxies`
   - Do not use absolute paths or incorrect directories

3. **Missing UID/GID on Startup**:
   ```bash
   # Correct startup command
   UID=$(id -u) GID=$(id -g) docker-compose up -d
   ```

**Verify Configuration Persistence**:
```bash
# Check file modification times
ls -lh data/config.yaml data/nodes.txt

# Check container logs for save confirmations
docker-compose logs -f | grep "Saved"
```

**Detailed Troubleshooting**: See [docs/troubleshooting-persistence.md](docs/troubleshooting-persistence.md)

### Docker Permission Issues

**Problem**: When using `docker-compose.yml` with volume mapping, you may encounter permission errors like "permission denied" or "cannot write to /etc/easy_proxies".

**Root Cause**: The container runs as a non-root user (specified by `user: "${UID:-10001}:${GID:-10001}"` in docker-compose.yml), but the mounted host directory may have different ownership.

**Solutions**:

1. **Use docker compose with a directory mount (Recommended)**:
   ```bash
   mkdir -p data logs
   sudo chown -R $(id -u):$(id -g) data logs
   docker compose up -d
   ```
   This maps the whole `./data` directory; `config.yaml` and `nodes.txt` are auto-generated as files on first run.

2. **Pre-create config files** (alternative, for file-level mounts):
   ```bash
   mkdir -p data
   cp config.example.yaml data/config.yaml
   touch data/nodes.txt
   chown -R $(id -u):$(id -g) data
   docker compose up -d
   ```

**For docker run command**:
```bash
mkdir -p data logs
chmod -R u+w data logs
docker run --user $(id -u):$(id -g) \
  -v $(pwd)/data:/etc/easy_proxies \
  -v $(pwd)/logs:/app/logs \
  --network host \
  ghcr.io/jasonwong1991/easy_proxies:latest
```

### Other Common Issues

- **"Config file not found"**: Ensure `config.yaml` exists in the mounted directory
- **"Cannot bind port"**: Check if the port is already in use by another service
- **"All nodes failed health check"**: Verify your proxy URIs are correct and the upstream servers are reachable
- **"Proxy was working but suddenly stopped"**: Check if the node is blacklisted (appears after 3 consecutive failures, default duration: 24h)
  - **Solution 1**: Release via WebUI - click the "Release" button next to the node
  - **Solution 2**: Release via API - `POST http://localhost:9091/api/nodes/{tag}/release`
  - **Solution 3**: Reduce blacklist duration in `config.yaml`:
    ```yaml
    pool:
      blacklist_duration: 1h  # Change from default 24h to 1h
    ```
  - Check logs for blacklist events: `docker compose logs | grep "BLACKLISTED"`

## Changelog

See [CHANGELOG.md](CHANGELOG.md) for version history.

## Development

```bash
go test ./...
```

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=jasonwong1991/easy_proxies&type=Date)](https://star-history.com/#jasonwong1991/easy_proxies&Date)

## Acknowledgements

This project is built on top of [sing-box](https://github.com/SagerNet/sing-box) — a universal proxy platform that powers all the underlying protocol implementations and transports. Huge thanks to the SagerNet team and contributors for their outstanding work.

## License

MIT License
