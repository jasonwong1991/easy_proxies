# Easy Proxies

English | [简体中文](README_ZH.md)

Easy Proxies is a proxy pool manager built on top of [sing-box](https://github.com/SagerNet/sing-box).
It supports multi-protocol nodes (VMess/VLESS/Trojan/SS/Hysteria2) and also upstream **HTTP/HTTPS/SOCKS** proxy nodes.
It provides Pool / Multi-Port / Hybrid modes plus a Web dashboard for monitoring, probing, exporting and node management.

## Highlights

- Mixed inbound entrypoints: **one port supports both HTTP Proxy and SOCKS5**
- Upstream node schemes:
  - `vmess://`, `vless://`, `trojan://`, `hysteria2://`, `ss://`
  - `http://`, `https://` (HTTP proxy over TCP / TLS)
  - `socks5://` (also accepts `socks://`, `socks4://`, `socks4a://`, `socks5h://`)
- Subscription formats:
  - Base64 (V2Ray-style)
  - Clash YAML (`proxies:`), including `type: http` / `type: socks5`
  - Plain text lists (`scheme://...` per line, or `host:port` list with a URL hint)
- Pool mode with failover, blacklist, and **dial fallback retries**
- Multi-Port mode: each node gets its own local port
- Hybrid mode: Pool + Multi-Port at the same time
- WebUI:
  - Live node status, active connections, failures
  - Manual probing and batch probing
  - Export working endpoints as proxy URIs
  - Node CRUD + reload
  - Subscription refresh with status

## Quick Start

### 1) Prepare config

```bash
cp config.example.yaml config.yaml
cp nodes.example nodes.txt
```

Edit `config.yaml` and `nodes.txt`.

### 2) Run (Docker recommended)

```bash
docker compose up -d
```

Or:

```bash
./start.sh
```

### 3) Ports

- Pool entry (Pool/Hybrid): `2323`
- Web dashboard: `9090` (configured by `management.listen`)
- Multi-Port range (Multi-Port/Hybrid): `24000+` (starts at `multi_port.base_port`)

### 4) Connect (HTTP and SOCKS5 on the same port)

Pool entry examples:

- HTTP proxy:
  - `http://username:password@127.0.0.1:2323`
- SOCKS5 proxy:
  - `socks5://username:password@127.0.0.1:2323`

Test with curl:

```bash
# HTTP proxy test
curl -I -x http://username:password@127.0.0.1:2323 http://example.com

# SOCKS5 proxy test
curl -I --socks5 username:password@127.0.0.1:2323 http://example.com
```

## Configuration

### Basic example

```yaml
mode: pool               # pool, multi-port, hybrid
log_level: info
skip_cert_verify: false  # global TLS cert verification switch (affects TLS-based nodes)

management:
  enabled: true
  listen: 0.0.0.0:9090
  probe_target: www.apple.com:80
  password: ""           # set a strong password if you expose WebUI to the Internet

listener:
  address: 0.0.0.0
  port: 2323
  username: username
  password: password

pool:
  mode: sequential        # sequential, random, balance
  failure_threshold: 3
  blacklist_duration: 24h

multi_port:
  address: 0.0.0.0
  base_port: 24000
  username: mpuser
  password: mppass

# Nodes configuration (you can mix multiple ways)
nodes_file: nodes.txt
# nodes:
#   - uri: "socks5://1.2.3.4:1080#MySocks"
#   - uri: "http://5.6.7.8:8080#MyHTTP"

# Subscription URLs (optional, multiple supported)
# Supports Base64, Clash YAML, plain text
subscriptions:
  - "https://example.com/subscription.yaml"
  - "https://raw.githubusercontent.com/Proxy/socks5-2.txt"
  # Plain host:port list needs a hint:
  - "https://raw.githubusercontent.com/Proxy/socks5-1.txt#socks5"
  - "https://raw.githubusercontent.com/Proxy/http-1.txt#http"

subscription_refresh:
  enabled: true
  interval: 1h
  timeout: 30s
  health_check_timeout: 60s
  drain_timeout: 30s
  min_available_nodes: 1
```

## Modes

### Pool mode

- One entry port (`listener.port`)
- Requests are routed through one upstream node selected by the pool strategy

### Multi-Port mode

- Each node listens on its own local port, starting from `multi_port.base_port`
- Each per-node port is also a mixed inbound (HTTP + SOCKS5)

### Hybrid mode

- Pool entry + Multi-Port entries simultaneously
- Node blacklist / active counters are shared

## Nodes

### Supported URI schemes for nodes

You can place any of these in `nodes:` or `nodes.txt`:

- VMess: `vmess://...`
- VLESS: `vless://...`
- Trojan: `trojan://...`
- Shadowsocks: `ss://...`
- Hysteria2: `hysteria2://...`
- HTTP proxy upstream:
  - `http://host:port`
  - `https://host:port`
  - If you need TLS but the list only provides `host:443`, use:
    - `https://host:443`
    - or `http://host:443?tls=1&sni=example.com`
- SOCKS upstream:
  - `socks5://host:port`
  - (also accepted: `socks://`, `socks4://`, `socks4a://`, `socks5h://`)

### Plain host:port lists

Some subscriptions provide only:

```
146.103.98.171:54101
31.43.179.38:80
```

This is ambiguous (HTTP? SOCKS?). For **subscription URLs**, you can add a fragment hint:

- `#socks5` => treat all `host:port` lines as `socks5://host:port`
- `#http`   => treat as `http://host:port`
- `#https`  => treat as `https://host:port`

Example:

```yaml
subscriptions:
  - "https://raw.githubusercontent.com/Proxy/socks5-1.txt#socks5"
  - "https://raw.githubusercontent.com/Proxy/http-1.txt#http"
```

If the list already contains `socks5://...` / `http://...`, no hint is needed.

## Health Check & Reliability

- The probe target is `management.probe_target` (default: `www.apple.com:80`)
- On startup and periodically, Easy Proxies probes nodes and marks them available/unavailable
- Pool selection skips nodes that have already been checked and marked unavailable
- Dial fallback: on connection failure, the pool automatically retries up to a few different nodes before returning an error to the client

Recommended settings for large public proxy lists (unstable by nature):

```yaml
pool:
  failure_threshold: 1
  blacklist_duration: 10m
```

Also consider switching to:

```yaml
pool:
  mode: balance
```

## Web Dashboard

Open: `http://<host>:9090`

Main features:

- Monitor nodes
- Manual probe and probe-all
- Export available endpoints
- Node management (add/edit/delete) + reload
- Subscription refresh status + manual refresh

Important note:

- Subscription refresh triggers a core reload and will interrupt existing connections.

## Docker

### Option A: host network (recommended)

```yaml
services:
  easy-proxies:
    image: ghcr.io/jasonwong1991/easy_proxies:latest
    network_mode: host
    restart: unless-stopped
    volumes:
      - ./config.yaml:/etc/easy-proxies/config.yaml
      - ./nodes.txt:/etc/easy-proxies/nodes.txt
```

### Option B: port mappings

```yaml
services:
  easy-proxies:
    image: ghcr.io/jasonwong1991/easy_proxies:latest
    restart: unless-stopped
    ports:
      - "2323:2323"
      - "9090:9090"
      - "24000-24200:24000-24200"
    volumes:
      - ./config.yaml:/etc/easy-proxies/config.yaml
      - ./nodes.txt:/etc/easy-proxies/nodes.txt"
```

## Security Notes

- If you bind WebUI to `0.0.0.0`, set `management.password`
- Public HTTP/SOCKS lists can be hostile/unstable; treat them as untrusted sources

## Build

```bash
go build -tags "with_utls with_quic with_grpc with_wireguard with_gvisor" -o easy-proxies ./cmd/easy_proxies
```

## License

MIT License
