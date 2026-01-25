# Easy Proxies

[English](README.md) | 简体中文

Easy Proxies 是一个基于 [sing-box](https://github.com/SagerNet/sing-box) 的代理节点池管理工具。
除了常见的 VMess/VLESS/Trojan/SS/Hysteria2 节点外，也支持将 **HTTP/HTTPS/SOCKS 上游代理**直接作为节点加入代理池。
提供 Pool / Multi-Port / Hybrid 三种模式，并配套 WebUI 用于监控、测活、导出、订阅刷新与节点管理。

## 亮点特性

- 入口为 mixed（同端口同时支持 **HTTP Proxy + SOCKS5**）
- 节点（上游代理）支持的 scheme：
  - `vmess://`, `vless://`, `trojan://`, `hysteria2://`, `ss://`
  - `http://`, `https://`（上游 HTTP 代理，可走 TCP 或 TLS）
  - `socks5://`（也支持 `socks://`, `socks4://`, `socks4a://`, `socks5h://`）
- 订阅支持：
  - Base64（v2ray 订阅）
  - Clash YAML（含 `type: http` / `type: socks5`）
  - 纯文本列表（每行一个 URI，或 `host:port` 列表 + URL hint）
- Pool 模式支持黑名单与失败处理，并带 **拨号失败自动 fallback 重试**
- Multi-Port 模式：每个节点独立端口
- Hybrid 模式：Pool + Multi-Port 同时启用，节点状态共享
- WebUI：
  - 节点状态/延迟/失败次数/活跃连接
  - 单节点探测与“探测全部”
  - 一键导出可用入口
  - 节点增删改查 + 重载
  - 订阅刷新状态 + 手动刷新

## 快速开始

### 1) 准备配置

```bash
cp config.example.yaml config.yaml
cp nodes.example nodes.txt
```

编辑 `config.yaml` 和 `nodes.txt`。

### 2) 运行（推荐 Docker）

```bash
docker compose up -d
```

或：

```bash
./start.sh
```

### 3) 端口说明

- Pool 入口（Pool/Hybrid）：`2323`
- WebUI：`9090`（由 `management.listen` 决定）
- Multi-Port 范围（Multi-Port/Hybrid）：`24000+`（从 `multi_port.base_port` 起）

### 4) 连接方式（同端口支持 HTTP + SOCKS5）

Pool 入口示例：

- HTTP 代理：
  - `http://username:password@127.0.0.1:2323`
- SOCKS5 代理：
  - `socks5://username:password@127.0.0.1:2323`

curl 测试：

```bash
# HTTP 代理
curl -I -x http://username:password@127.0.0.1:2323 http://example.com

# SOCKS5 代理
curl -I --socks5 username:password@127.0.0.1:2323 http://example.com
```

## 配置说明

### 基础示例

```yaml
mode: pool               # pool / multi-port / hybrid
log_level: info
skip_cert_verify: false  # 全局跳过 TLS 证书验证（影响 TLS 类节点与 https/http(tls) 代理）

management:
  enabled: true
  listen: 0.0.0.0:9090
  probe_target: www.apple.com:80
  password: ""           # 若 WebUI 暴露公网，务必设置强密码

listener:
  address: 0.0.0.0
  port: 2323
  username: username
  password: password

pool:
  mode: sequential        # sequential / random / balance
  failure_threshold: 3
  blacklist_duration: 24h

multi_port:
  address: 0.0.0.0
  base_port: 24000
  username: mpuser
  password: mppass

# 节点配置（三选一或混用）
nodes_file: nodes.txt
# nodes:
#   - uri: "socks5://1.2.3.4:1080#MySocks"
#   - uri: "http://5.6.7.8:8080#MyHTTP"

# 订阅（可选）
subscriptions:
  - "https://example.com/subscription.yaml"
  - "https://raw.githubusercontent.com/Proxy/socks5-2.txt"
  # 纯 host:port 列表需要 hint：
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

## 模式说明

### Pool 模式

- 单入口（`listener.port`）
- 每次建立新连接时，从节点池中选择一个上游节点转发

### Multi-Port 模式

- 每个节点一个独立端口（从 `multi_port.base_port` 起递增）
- 每个独立端口同样是 mixed（HTTP + SOCKS5 都能连）

### Hybrid 模式

- 同时提供 Pool 入口与 Multi-Port 入口
- 黑名单/活跃连接数等状态在两种入口之间共享

## 节点与订阅

### 节点支持的 URI scheme

你可以在 `nodes:` 或 `nodes.txt` 里直接放：

- `vmess://...`
- `vless://...`
- `trojan://...`
- `ss://...`
- `hysteria2://...`
- 上游 HTTP 代理：
  - `http://host:port`
  - `https://host:port`
  - 如果列表只给了 `host:443`，且实际上是 TLS HTTP 代理，建议用：
    - `https://host:443`
    - 或 `http://host:443?tls=1&sni=example.com`
- 上游 SOCKS 代理：
  - `socks5://host:port`
  -（也支持：`socks://`, `socks4://`, `socks4a://`, `socks5h://`）

### 纯文本 host:port 列表（必须指定默认协议）

有些订阅只提供：

```
146.103.98.171:54101
31.43.179.38:80
```

这种格式无法判断是 HTTP 还是 SOCKS。对于 **订阅 URL**，你可以用 `#hint` 指定默认协议：

- `#socks5`：把 `host:port` 当 `socks5://host:port`
- `#http`：把 `host:port` 当 `http://host:port`
- `#https`：把 `host:port` 当 `https://host:port`

示例：

```yaml
subscriptions:
  - "https://raw.githubusercontent.com/Proxy/socks5-1.txt#socks5"
  - "https://raw.githubusercontent.com/Proxy/http-1.txt#http"
```

如果列表本身已经是 `socks5://...` / `http://...`，则不需要 hint。

## 健康检查与可用性

- 探测目标由 `management.probe_target` 决定（默认 `www.apple.com:80`）
- 启动后会进行初始测活，并定期更新节点可用性
- Pool 选择节点时会跳过“已测活且不可用”的节点
- 拨号失败时会自动 fallback 重试多个候选节点，尽量减少客户端看到的错误

大量公开代理列表（天然不稳定）建议配置：

```yaml
pool:
  failure_threshold: 1
  blacklist_duration: 10m
```

也可以试：

```yaml
pool:
  mode: balance
```

## WebUI

访问：`http://<host>:9090`

能力：

- 节点监控
- 单节点探测 / 探测全部
- 导出可用入口
- 节点管理（增删改查）+ 重载
- 订阅刷新状态与手动刷新

注意：

- 订阅刷新会触发核心重载，现有连接会被中断。

## Docker 部署

### 方式一：host 网络（推荐）

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

### 方式二：端口映射

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
      - ./nodes.txt:/etc/easy-proxies/nodes.txt
```

## 安全提示

- 如果 `management.listen` 绑定到 `0.0.0.0`，务必设置 `management.password`
- 公共 HTTP/SOCKS 列表可能不稳定甚至恶意，建议按“不可信输入”处理

## 构建

```bash
go build -tags "with_utls with_quic with_grpc with_wireguard with_gvisor" -o easy-proxies ./cmd/easy_proxies
```

## 许可证

MIT License