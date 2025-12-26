# Easy Proxies

English | [简体中文](README_ZH.md)

A proxy node pool management tool based on [sing-box](https://github.com/SagerNet/sing-box), supporting multiple protocols, automatic failover, and load balancing.

## Features

- **Multi-Protocol Support**: VMess, VLESS, Hysteria2, Shadowsocks, Trojan
- **Multiple Transports**: TCP, WebSocket, HTTP/2, gRPC, HTTPUpgrade
- **Subscription Support**: Auto-fetch nodes from subscription links (Base64, Clash YAML, etc.)
- **Subscription Auto-Refresh**: Automatic periodic refresh with WebUI manual trigger (⚠️ causes connection interruption)
- **Pool Mode**: Automatic failover and load balancing
- **Multi-Port Mode**: Each node listens on independent port
- **Hybrid Mode**: Pool + Multi-Port simultaneously with shared node state
- **Web Dashboard**: Real-time node status, latency probing, one-click export
- **WebUI Settings**: Modify external_ip and probe_target without editing config files
- **Password Protection**: WebUI authentication support
- **Auto Health Check**: Initial check on startup, periodic checks every 5 minutes
- **Smart Node Filtering**: Auto-hide unavailable nodes, sort by latency
- **Port Preservation**: Existing nodes keep their ports when adding/updating nodes
- **Flexible Configuration**: Config file, node file, subscription links
- **Multi-Architecture**: Docker images for both AMD64 and ARM64

## Quick Start

### 1. Configuration

Copy example config files:

```bash
cp config.example.yaml config.yaml
cp nodes.example nodes.txt
```

Edit `config.yaml` to set listen address and credentials, edit `nodes.txt` to add proxy nodes.

### 2. Run

**Docker (Recommended):**

```bash
./start.sh
```

Or manually:

```bash
docker compose up -d
```

**Local Build:**

```bash
go build -tags "with_utls with_quic with_grpc" -o easy-proxies ./cmd/easy_proxies
./easy-proxies --config config.yaml
```

## Configuration

### Basic Config

```yaml
mode: pool                    # Mode: pool, multi-port, or hybrid
log_level: info               # Log level: debug, info, warn, error
external_ip: ""               # External IP for export (recommended for Docker)

# Subscription URLs (optional, multiple supported)
subscriptions:
  - "https://example.com/subscribe"

# Management Interface
management:
  enabled: true
  listen: 0.0.0.0:9090        # Web dashboard address
  probe_target: www.apple.com:80  # Latency probe target
  password: ""                # WebUI password (optional)
  api_key: ""                 # Subscription API key for URL auth (optional)

# Unified Entry Listener
listener:
  address: 0.0.0.0
  port: 2323
  username: username
  password: password

# Pool Settings
pool:
  mode: sequential            # sequential or random
  failure_threshold: 3        # Failures before blacklist
  blacklist_duration: 24h     # Blacklist duration

# Multi-Port Mode
multi_port:
  address: 0.0.0.0
  base_port: 24000            # Starting port, auto-increment
  username: mpuser
  password: mppass
```

### Operating Modes

#### Pool Mode

All nodes share a single entry point, program auto-selects available nodes:

```yaml
mode: pool

listener:
  address: 0.0.0.0
  port: 2323
  username: user
  password: pass

pool:
  mode: sequential  # sequential or random
  failure_threshold: 3
  blacklist_duration: 24h
```

**Use Case:** Automatic failover, load balancing

**Usage:** Set proxy to `http://user:pass@localhost:2323`

#### Multi-Port Mode

Each node listens on its own port for precise control:

**Config Format:** Two syntaxes supported

```yaml
mode: multi-port  # Recommended: hyphen format
# or
mode: multi_port  # Compatible: underscore format
```

**Full Example:**

```yaml
mode: multi-port

multi_port:
  address: 0.0.0.0
  base_port: 24000  # Ports auto-increment from here
  username: user
  password: pass

nodes_file: nodes.txt
```

**Startup Output:**

```
📡 Proxy Links:
═══════════════════════════════════════════════════════════════
🔌 Multi-Port Mode (3 nodes):

   [24000] Taiwan Node
       http://user:pass@0.0.0.0:24000
   [24001] Hong Kong Node
       http://user:pass@0.0.0.0:24001
   [24002] US Node
       http://user:pass@0.0.0.0:24002
═══════════════════════════════════════════════════════════════
```

**Use Case:** Specific node selection, performance testing

**Usage:** Each node has independent proxy address

#### Hybrid Mode

Combines Pool and Multi-Port modes, sharing node state between them:

```yaml
mode: hybrid

listener:
  address: 0.0.0.0
  port: 2323           # Pool entry point
  username: user
  password: pass

multi_port:
  address: 0.0.0.0
  base_port: 24000     # Multi-port starting port
  username: mpuser
  password: mppass

pool:
  mode: balance        # sequential, random, or balance
  failure_threshold: 3
  blacklist_duration: 24h
```

**Startup Output:**

```
📡 Proxy Links:
═══════════════════════════════════════════════════════════════
🌐 Pool Entry Point:
   http://user:pass@0.0.0.0:2323

   Nodes in pool (3):
   • Taiwan Node
   • Hong Kong Node
   • US Node

🔌 Multi-Port Entry Points (3 nodes):

   [24000] Taiwan Node
       http://mpuser:mppass@0.0.0.0:24000
   [24001] Hong Kong Node
       http://mpuser:mppass@0.0.0.0:24001
   [24002] US Node
       http://mpuser:mppass@0.0.0.0:24002
═══════════════════════════════════════════════════════════════
```

**Key Features:**

- **Shared State**: Node blacklist status syncs between Pool and Multi-Port
  - If a node fails in Pool mode, it's also marked unavailable in Multi-Port
  - Health checks update both modes simultaneously
- **Auto Port Reassignment**: If a port is occupied, automatically assigns next available port
- **Flexible Access**: Use Pool for load balancing, Multi-Port for specific node access

**Use Case:** Need both automatic failover AND direct node access

### Node Configuration

**Method 1: Subscription Links (Recommended)**

Auto-fetch nodes from subscription URLs:

```yaml
subscriptions:
  - "https://example.com/subscribe/v2ray"
  - "https://example.com/subscribe/clash"
```

Supported formats:
- **Base64 Encoded**: V2Ray standard subscription
- **Clash YAML**: Clash config format
- **Plain Text**: One URI per line

**Method 2: Node File**

Specify in `config.yaml`:

```yaml
nodes_file: nodes.txt
```

`nodes.txt` - one URI per line:

```
vless://uuid@server:443?security=reality&sni=example.com#NodeName
hysteria2://password@server:443?sni=example.com#HY2Node
ss://base64@server:8388#SSNode
trojan://password@server:443?sni=example.com#TrojanNode
vmess://base64...#VMessNode
```

**Method 3: Direct in Config**

```yaml
nodes:
  - uri: "vless://uuid@server:443#Node1"
  - name: custom-name
    uri: "ss://base64@server:8388"
    port: 24001  # Optional, manual port
```

> **Tip**: Multiple methods can be combined, nodes are merged automatically.

## Supported Protocols

| Protocol | URI Format | Features |
|----------|------------|----------|
| VMess | `vmess://` | WebSocket, HTTP/2, gRPC, TLS |
| VLESS | `vless://` | Reality, XTLS-Vision, multiple transports |
| Hysteria2 | `hysteria2://` | Bandwidth control, obfuscation |
| Shadowsocks | `ss://` | Multiple ciphers |
| Trojan | `trojan://` | TLS, multiple transports |

### VMess Parameters

VMess supports two URI formats:

**Format 1: Base64 JSON (Standard)**
```
vmess://base64({"v":"2","ps":"Name","add":"server","port":443,"id":"uuid","aid":0,"scy":"auto","net":"ws","type":"","host":"example.com","path":"/path","tls":"tls","sni":"example.com"})
```

**Format 2: URL Format**
```
vmess://uuid@server:port?encryption=auto&security=tls&sni=example.com&type=ws&host=example.com&path=/path#Name
```

- `net/type`: tcp, ws, h2, grpc
- `tls/security`: tls or empty
- `scy/encryption`: auto, aes-128-gcm, chacha20-poly1305, etc.

### VLESS Parameters

```
vless://uuid@server:port?encryption=none&security=reality&sni=example.com&fp=chrome&pbk=xxx&sid=xxx&type=tcp&flow=xtls-rprx-vision#Name
```

- `security`: none, tls, reality
- `type`: tcp, ws, http, grpc, httpupgrade
- `flow`: xtls-rprx-vision (TCP only)
- `fp`: fingerprint (chrome, firefox, safari, etc.)

### Hysteria2 Parameters

```
hysteria2://password@server:port?sni=example.com&insecure=0&obfs=salamander&obfs-password=xxx#Name
```

- `upMbps` / `downMbps`: Bandwidth limits
- `obfs`: Obfuscation type
- `obfs-password`: Obfuscation password

## Web Dashboard

Access `http://localhost:9090` to view:

- Node status (Healthy/Warning/Error/Blacklisted)
- Real-time latency
- Active connections
- Failure count
- Manual latency probing
- Release blacklisted nodes
- **One-click Export**: Export all available nodes as proxy URIs (`http://user:pass@host:port`)
- **Subscription URL**: API Key authentication for direct subscription in Surge/Clash clients
- **Settings**: Click the gear icon to modify `external_ip` and `probe_target` (changes saved immediately)

### WebUI Settings

Click the ⚙️ gear icon in the header to access settings:

| Setting | Description |
|---------|-------------|
| External IP | IP address used in exported proxy URIs (replaces `0.0.0.0`) |
| Probe Target | Health check target address (format: `host:port`) |

Changes are saved to `config.yaml` immediately and take effect without restart.

### Node Management

The Web UI provides a **Node Management** tab for CRUD operations on proxy nodes:

- **Add Node**: Add new proxy nodes via URI (name auto-extracted from URI fragment)
- **Edit Node**: Modify existing node configuration
- **Delete Node**: Remove nodes from configuration
- **Reload Config**: Apply changes by restarting sing-box core (⚠️ interrupts connections)
- **Port Preservation**: Existing nodes keep their assigned ports after reload

In Multi-Port mode, ports are automatically allocated from `base_port`.

**API Endpoints:**

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/nodes/config` | List all configured nodes |
| POST | `/api/nodes/config` | Add a new node |
| PUT | `/api/nodes/config/:name` | Update node by name |
| DELETE | `/api/nodes/config/:name` | Delete node by name |
| POST | `/api/reload` | Reload configuration |
| GET | `/api/export` | Export available nodes (`?format=surge&key=xxx`) |
| GET | `/api/settings` | Get current settings |
| PUT | `/api/settings` | Update settings (external_ip, probe_target) |

**Request/Response Example:**

```bash
# Add node
curl -X POST http://localhost:9090/api/nodes/config \
  -H "Content-Type: application/json" \
  -d '{"uri": "vless://uuid@server:443#NodeName"}'

# Delete node
curl -X DELETE http://localhost:9090/api/nodes/config/NodeName

# Reload config
curl -X POST http://localhost:9090/api/reload
```

### Subscription URL

Configure `api_key` to enable URL parameter authentication for direct subscription:

```yaml
management:
  password: "admin123"
  api_key: "your-secret-key"  # Subscription auth key
```

**Subscription URL Format:**

| Format | URL |
|--------|-----|
| HTTP | `http://host:9090/api/export?key=your-secret-key` |
| Surge | `http://host:9090/api/export?format=surge&key=your-secret-key` |

Copy subscription URLs directly from the WebUI export dialog.

### Health Check Mechanism

Auto health check on startup, then periodic checks:

- **Initial Check**: Test all nodes immediately after startup
- **Periodic Check**: Every 5 minutes
- **Smart Filtering**: Hide unavailable nodes from WebUI and export
- **Probe Target**: Configure via `management.probe_target` (default `www.apple.com:80`)

```yaml
management:
  enabled: true
  listen: 0.0.0.0:9090
  probe_target: www.apple.com:80  # Health check target
```

### Password Protection

Protect node information with WebUI password:

```yaml
management:
  enabled: true
  listen: 0.0.0.0:9090
  password: "your_secure_password"
```

- Empty or unset `password` means no authentication required
- Login prompt appears on first access when password is set
- Session persists for 7 days after login

### Subscription Auto-Refresh

Automatic periodic subscription refresh:

```yaml
subscription_refresh:
  enabled: true                 # Enable auto-refresh
  interval: 1h                  # Refresh interval (default 1 hour)
  timeout: 30s                  # Fetch timeout
  health_check_timeout: 60s     # New node health check timeout
  drain_timeout: 30s            # Old instance drain timeout
  min_available_nodes: 1        # Minimum available nodes required
```

> ⚠️ **Important: Subscription refresh causes connection interruption**
>
> During subscription refresh, the program **restarts the sing-box core** to load new node configuration. This means:
>
> - **All existing connections will be disconnected**
> - Ongoing downloads, streaming, etc. will be interrupted
> - Clients need to reconnect
>
> **Recommendations:**
> - Set longer refresh intervals (e.g., `1h` or more)
> - Avoid manual refresh during peak usage
> - Disable if connection stability is critical (`enabled: false`)

**WebUI and API Support:**

- WebUI shows subscription status (node count, last refresh time, errors)
- Manual refresh button available
- API endpoints:
  - `GET /api/subscription/status` - Get subscription status
  - `POST /api/subscription/refresh` - Trigger manual refresh

## Ports

| Port | Purpose |
|------|---------|
| 2323 | Unified proxy entry (Pool/Hybrid mode) |
| 9090 | Web dashboard |
| 24000+ | Per-node ports (Multi-Port/Hybrid mode) |

## Docker Deployment

**Method 1: Host Network Mode (Recommended)**

Use `network_mode: host` for direct host network access:

```yaml
# docker-compose.yml
services:
  easy-proxies:
    image: ghcr.io/jasonwong1991/easy_proxies:latest
    container_name: easy-proxies
    restart: unless-stopped
    network_mode: host
    volumes:
      - ./config.yaml:/etc/easy-proxies/config.yaml
      - ./nodes.txt:/etc/easy-proxies/nodes.txt
```

> **Note**: Config files need write permission for WebUI settings. Run `chmod 666 config.yaml nodes.txt` if you encounter permission errors.

> **Advantage**: Container uses host network directly, all ports exposed automatically. Auto port reassignment works seamlessly.

**Method 2: Port Mapping Mode**

Manually specify port mappings:

```yaml
# docker-compose.yml
services:
  easy-proxies:
    image: ghcr.io/jasonwong1991/easy_proxies:latest
    container_name: easy-proxies
    restart: unless-stopped
    ports:
      - "2323:2323"       # Pool/Hybrid mode entry
      - "9091:9091"       # Web dashboard
      - "24000-24200:24000-24200"  # Multi-Port/Hybrid mode
    volumes:
      - ./config.yaml:/etc/easy-proxies/config.yaml
      - ./nodes.txt:/etc/easy-proxies/nodes.txt
```

> **Note**: Multi-Port and Hybrid modes require mapping the port range. Map enough ports for your nodes plus some buffer for auto-reassignment.

## Building

```bash
# Basic build
go build -o easy-proxies ./cmd/easy_proxies

# Full feature build
go build -tags "with_utls with_quic with_grpc with_wireguard with_gvisor" -o easy-proxies ./cmd/easy_proxies
```

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=jasonwong1991/easy_proxies&type=Date)](https://star-history.com/#jasonwong1991/easy_proxies&Date)

## License

MIT License
