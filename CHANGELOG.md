# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [3.1.2] - 2026-10-08

### Fixed
- Preserve `grpc-opts.grpc-service-name` when converting Clash VMess and Trojan nodes to URIs, so their gRPC transports use the configured service name instead of an empty one (#37).

## [3.1.1] - 2026-09-19

### Fixed
- **Failure cooldowns were force-released almost immediately, so `rate_limit_cooldown` barely took effect**: the cooldown added in 3.1.0 is stored in the same state as a threshold blacklist, and two pre-existing paths cleared that state without distinguishing the two.
  - `releaseIfAllBlacklistedLocked` is a last-resort "every member is banned, let them all back in" escape hatch on the request path. In `multi-port`/`hybrid` mode the builder gives each node its own **one-member** pool, so "every member is banned" became trivially true the instant that single member cooled down — the node was released on the very next request and the effective cooldown was ~0
  - A successful health probe called `forceRelease()`. The sweep runs every 5 minutes over all nodes and probes a different host than the failing request used, so a node whose upstream quota was exhausted still passed and was released — capping any cooldown at ~5 minutes
  - Live cooldowns now survive both automatic paths. Permanent blacklists keep their original auto-clear behaviour (the #8/#9 fix is preserved), and manual release from the WebUI or `POST /api/nodes/{tag}/release` still clears everything immediately
  - **Behaviour change**: when every member of a pool is cooling down at once, requests now fail until the earliest cooldown expires instead of being handed back to a node known to be rate-limited. The 60s default keeps this brief; size `rate_limit_cooldown` against your pool accordingly
- **`"429"` matched inside unrelated numbers**: classification used a bare substring test, so `dial tcp 203.0.113.5:44290: i/o timeout` was treated as rate limiting — applying the long rate-limit cooldown instead of the 60s transient one, and, because cooldowns never increment the failure counter, permanently exempting such a node from the blacklist. Status codes are now matched with digit boundaries (`429` and `503` alike), so ports and dotted quads no longer collide
- Blacklist log lines printed the configured `blacklist_duration` even when an already-running longer ban won, contradicting the expiry timestamp on the same line; they now print the real remaining span

## [3.1.0] - 2026-09-19

### Added
- **Stable per-node ports**: in `multi-port`/`hybrid` mode, each node keeps the same local port across subscription refreshes and process restarts
  - Ports are preserved by a stable node identity derived from the URI (ignoring the display name and query-parameter order), so renamed or reordered subscription nodes keep their port
  - Assignments are persisted to `node_ports.json` next to `config.yaml` and restored on startup
- **Shadowsocks-compatible link format**: support for additional Shadowsocks URI variants (#28)
- **WebUI: export all nodes**: new "全部导出" button and `GET /api/export?all=true` that exports every node regardless of health (dead or alive); the default export still returns only healthy/available nodes
- **WebUI: initial health-check progress**: the startup/periodic node sweep now publishes live progress (`done/total`, available, failed) through `GET /api/nodes` and drives the shared progress bar, so the dashboard shows "初始化探测中 N/M" instead of appearing frozen until the sweep finishes
- **Configurable transient and rate-limit cooldowns**: two new `pool` keys replace the previously hardcoded 60s cooldown — `transient_cooldown` (default `60s`) for timeouts, connection resets and 503, and `rate_limit_cooldown` (falls back to `transient_cooldown` when unset) for HTTP 429 / "too many requests". Both accept Go duration strings (`30s`, `10m`, `2h`), are editable live from the WebUI 设置 → 节点池配置 (Pool) panel via the 「暂时性故障冷却」 and 「429 限流冷却」 fields, and are exposed through `GET`/`PUT /api/settings` as `pool.transient_cooldown` / `pool.rate_limit_cooldown`. Setting `rate_limit_cooldown: 2h` keeps a rate-limited node out of rotation for hours instead of a minute; changing either value requires a config reload (#34)

### Changed
- **Transient failures (429 rate-limit, timeouts, connection resets) no longer trigger the 24h blacklist**: such errors are common for shared/free nodes briefly rate-limited by their CDN and usually clear on their own. They now impose only a configurable cooldown — `pool.transient_cooldown`, default `60s`, with rate-limit (429) failures separately tunable via `pool.rate_limit_cooldown` — and do not count toward the 3-strikes permanent-blacklist threshold; permanent faults (handshake/cert/protocol failures, 404, etc.) keep the original 3 → 24h behavior. This stops a burst of concurrent requests from mass-blacklisting otherwise-healthy nodes for a full day
- Improved configuration persistence diagnostics and error handling
- `entrypoint.sh` now detects the "bind-mount of a non-existent file → Docker creates a directory" foot-gun for `config.yaml`/`nodes.txt` and exits with an actionable fix instead of a vague runtime crash
- Removed `start.sh` and `diagnose.sh` helper scripts; `docker compose up -d` (with a directory mount) is now the documented path. README/docs updated to inline the equivalent checks

### Fixed
- **GeoIP region selection now works through a proxy request header**: the region could previously only be given as a path/authority prefix (`jp/example.com:443`), which most HTTP clients mangle — `net/http` parses a `CONNECT jp/example.com:443` authority into `URL.Host=jp` + `URL.Path=/example.com:443`, so the region was silently dropped. Clients can now send `X-Easyproxy-Region: jp`, which works identically for plain HTTP requests and CONNECT tunnels; the header is stripped before the request is forwarded upstream, an unknown region returns `400` instead of falling back to the default pool, and the original prefix forms still work (#32) — thanks @xingpingcn
- **Error messages now match actual mount configuration**: entrypoint.sh error messages previously hardcoded `./data/` paths, causing confusion when using file-mount mode (`-v ./nodes.txt:/etc/easy_proxies/nodes.txt`). Now displays correct fix instructions for both directory-mount and file-mount configurations
- **Initial/periodic health check hangs, leaving 0 nodes available**: `probeAllNodes` (the startup and periodic sweep) called `probe(ctx)` inline with no hard-timeout guard — a separate path from the batch probe. A protocol dial that ignores `ctx` blocked the worker forever, wedging its semaphore slot; the 32 slots filled up and the sweep stalled, so nodes stayed `initialCheckDone=false` and the dashboard showed 0 available even though manual/batch probes found thousands reachable. Each worker now races the probe against its deadline in a goroutine and always releases its slot within the timeout
- **Batch probe hangs forever (WebUI frozen at "N/M")**: a probe against a node that stalls (accepts TCP but never responds, or a protocol dial/handshake that ignores context cancellation) could block indefinitely, so the stuck goroutines occupied every semaphore slot and `wg.Wait` never returned — freezing the run (e.g. stuck around 1600/8363). Fixed with two layers: (1) `Probe` now runs the check in its own goroutine and races it against the 10s context deadline, so it always returns even if the underlying dial ignores `ctx`; (2) a connection watchdog force-closes the probe connection when the deadline fires, unwinding the stalled goroutine (some sing-box connection types don't honor `SetDeadline`, so the deadline alone was insufficient)
- **WebUI: dashboard blacklist/abnormal count stuck at 0**: `GET /api/nodes` returned only the filtered healthy set, so the frontend never saw blacklisted/unavailable nodes and their count always showed 0. It now returns the full node set, restoring the count and making blacklisted nodes visible in the table with a working "解封" (release) button
- WebUI: long node names and URIs are now truncated so they no longer break the table layout
- Prevent crash from malformed VLESS `packetEncoding` nodes
- Preserve inline nodes when a subscription update occurs

## [3.0.1] - 2026-06-17

### Added
- WebUI: sticky proxy settings are now editable from the dashboard

## [3.0.0] - 2026-06-17

### Added
- **Sticky Proxy**: Optional dedicated entry port (default `listener.port + 1`, e.g. `2324`) that pins each client to a single upstream node by source IP, keeping the egress IP stable instead of rotating per connection
  - Coexists with the regular non-sticky pool entry (`2323`) — choose per port
  - Pin is permanent until the node is blacklisted/removed, then re-selects automatically
  - New `sticky` config section (`enabled`, `port`); listen address and credentials inherited from `listener`
  - Pool/hybrid mode only
- **Log Rotation**: Configurable log file rotation with size limits, backup count, and compression
  - New `log` section in config with `output`, `file`, `max_size`, `max_backups`, `max_age`, `compress` options
  - Uses lumberjack for automatic log rotation
  - Defaults: 50MB max size, 3 backups, 7 days retention
- **WebUI Console**: Real-time log streaming in the dashboard
  - In-memory ring buffer captures last 1000 log lines
  - WebSocket-based live log streaming to browser
  - Console tab in WebUI for instant log viewing
- **AnyTLS Protocol**: Support for AnyTLS outbound protocol
  - Parse `anytls://` URIs from subscriptions
  - Full TLS configuration support
- **TUIC Protocol**: Support for TUIC outbound protocol
  - Parse `tuic://` URIs with UUID and password authentication
  - Congestion control and UDP relay mode configuration
  - Full TLS/ALPN support
- **Clash API Integration**: Embedded Clash API controller
  - Internal controller at `127.0.0.1:9092`
  - Enables Clash-compatible tooling integration

### Changed
- **Subscription Parsing**: Improved Clash YAML format detection
  - User-Agent changed to `clash-verge/v2.2.3` for better compatibility
  - YAML detection sample size increased from 200 to 16384 characters
  - Better support for modern proxy types (AnyTLS, TUIC) in Clash format
- **Docker Entrypoint**: Fixed bind-mount permission issues
  - Uses gosu for privilege dropping
  - Ensures proper file ownership for nodes.txt and logs

### Fixed
- Docker nodes.txt permission denied on bind-mount
- VMess node name extraction from base64 payload
- Cross-platform file locking for Windows support

## [2.0.0] - 2025-01-XX

### Added
- SOCKS5 inbound protocol support via Mixed type
- Cross-platform file locking for Windows support
- GeoIP database auto-download and hot-reload
- Hysteria2 (hy2://) protocol support
- Comprehensive security and performance improvements

### Changed
- Major protocol, performance and UI overhaul
- Improved subscription parsing with better error handling
- Enhanced dashboard with real-time statistics

## [1.1.0] - 2024-12-XX

### Added
- GeoIP region routing and dashboard statistics
- Global skip_cert_verify option
- Node port assignment persistence across reloads
- ARM64 support for Docker image

### Fixed
- Hybrid mode export credentials
- Settings save permission issues
- Health check timing after node registration

## [1.0.0] - 2024-11-XX

### Added
- Initial release
- Pool, multi-port, and hybrid runtime modes
- Support for vmess, vless, trojan, ss, hysteria2, socks5, http protocols
- Subscription support (Base64/plain text/Clash YAML)
- Web dashboard with node management
- Automatic health checks and blacklist recovery
- Configurable DNS resolver