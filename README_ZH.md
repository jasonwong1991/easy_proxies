# Easy Proxies

[English](README.md) | 简体中文

基于 [sing-box](https://github.com/SagerNet/sing-box) 的代理节点池管理工具，支持多协议、多节点自动故障转移和负载均衡。

## 特性

- **多协议支持**: VMess、VLESS、Hysteria2、Shadowsocks、Trojan
- **多种传输层**: TCP、WebSocket、HTTP/2、gRPC、HTTPUpgrade
- **订阅链接支持**: 自动从订阅链接获取节点，支持 Base64、Clash YAML 等格式
- **订阅定时刷新**: 自动定时刷新订阅，支持 WebUI 手动触发（⚠️ 刷新会导致连接中断）
- **节点池模式**: 自动故障转移、负载均衡
- **多端口模式**: 每个节点独立监听端口
- **混合模式**: 同时启用节点池 + 多端口，节点状态共享同步
- **Web 监控面板**: 实时查看节点状态、延迟探测、一键导出节点
- **WebUI 设置**: 无需编辑配置文件即可修改 external_ip 和 probe_target
- **密码保护**: WebUI 支持密码认证，保护节点信息安全
- **自动健康检查**: 启动时自动检测所有节点可用性，定期（5分钟）检查节点状态
- **智能节点过滤**: 自动过滤不可用节点，WebUI 和导出按延迟排序
- **端口保留**: 添加/更新节点时，已有节点保持原有端口不变
- **灵活配置**: 支持配置文件、节点文件、订阅链接多种方式
- **多架构支持**: Docker 镜像同时支持 AMD64 和 ARM64

## 快速开始

### 1. 配置

复制示例配置文件：

```bash
cp config.example.yaml config.yaml
cp nodes.example nodes.txt
```

编辑 `config.yaml` 配置监听地址和认证信息，编辑 `nodes.txt` 添加代理节点。

### 2. 运行

**Docker 方式（推荐）：**

```bash
./start.sh
```

或手动执行：

```bash
docker compose up -d
```

**本地编译运行：**

```bash
go build -tags "with_utls with_quic with_grpc" -o easy-proxies ./cmd/easy_proxies
./easy-proxies --config config.yaml
```

## 配置说明

### 基础配置

```yaml
mode: pool                    # 运行模式: pool (节点池)、multi-port (多端口) 或 hybrid (混合)
log_level: info               # 日志级别: debug, info, warn, error
external_ip: ""               # 外部 IP 地址，用于导出时替换 0.0.0.0（Docker 部署时建议配置）

# 订阅链接（可选，支持多个）
subscriptions:
  - "https://example.com/subscribe"

# 管理接口
management:
  enabled: true
  listen: 0.0.0.0:9090        # Web 监控面板地址
  probe_target: www.apple.com:80  # 延迟探测目标
  password: ""                # WebUI 访问密码，为空则不需要密码（可选）
  api_key: ""                 # 订阅 API Key，用于 URL 参数认证（可选）

# 统一入口监听
listener:
  address: 0.0.0.0
  port: 2323
  username: username
  password: password

# 节点池配置
pool:
  mode: sequential            # sequential (顺序) 或 random (随机)
  failure_threshold: 3        # 失败阈值，超过后拉黑节点
  blacklist_duration: 24h     # 拉黑时长

# 多端口模式
multi_port:
  address: 0.0.0.0
  base_port: 24000            # 起始端口，节点依次递增
  username: mpuser
  password: mppass
```

### 运行模式详解

#### Pool 模式（节点池）

所有节点共享一个入口地址，程序自动选择可用节点：

```yaml
mode: pool

listener:
  address: 0.0.0.0
  port: 2323
  username: user
  password: pass

pool:
  mode: sequential  # sequential (顺序) 或 random (随机)
  failure_threshold: 3
  blacklist_duration: 24h
```

**适用场景：** 自动故障转移、负载均衡

**使用方式：** 配置代理为 `http://user:pass@localhost:2323`

#### Multi-Port 模式（多端口）

每个节点独立监听一个端口，精确控制使用哪个节点：

**配置格式：** 支持两种写法

```yaml
mode: multi-port  # 推荐：连字符格式
# 或
mode: multi_port  # 兼容：下划线格式
```

**完整配置示例：**

```yaml
mode: multi-port

multi_port:
  address: 0.0.0.0
  base_port: 24000  # 端口从这里开始自动递增
  username: user
  password: pass

# 使用 nodes_file 简化配置
nodes_file: nodes.txt
```

**启动时输出：**

```
📡 Proxy Links:
═══════════════════════════════════════════════════════════════
🔌 Multi-Port Mode (3 nodes):

   [24000] 台湾节点
       http://user:pass@0.0.0.0:24000
   [24001] 香港节点
       http://user:pass@0.0.0.0:24001
   [24002] 美国节点
       http://user:pass@0.0.0.0:24002
═══════════════════════════════════════════════════════════════
```

**适用场景：** 需要指定特定节点、测试节点性能

**使用方式：** 每个节点有独立的代理地址，可精确选择

#### Hybrid 模式（混合模式）

同时启用节点池和多端口模式，两者共享节点状态：

```yaml
mode: hybrid

listener:
  address: 0.0.0.0
  port: 2323           # 节点池入口
  username: user
  password: pass

multi_port:
  address: 0.0.0.0
  base_port: 24000     # 多端口起始端口
  username: mpuser
  password: mppass

pool:
  mode: balance        # sequential (顺序)、random (随机) 或 balance (负载均衡)
  failure_threshold: 3
  blacklist_duration: 24h
```

**启动时输出：**

```
📡 Proxy Links:
═══════════════════════════════════════════════════════════════
🌐 Pool Entry Point:
   http://user:pass@0.0.0.0:2323

   Nodes in pool (3):
   • 台湾节点
   • 香港节点
   • 美国节点

🔌 Multi-Port Entry Points (3 nodes):

   [24000] 台湾节点
       http://mpuser:mppass@0.0.0.0:24000
   [24001] 香港节点
       http://mpuser:mppass@0.0.0.0:24001
   [24002] 美国节点
       http://mpuser:mppass@0.0.0.0:24002
═══════════════════════════════════════════════════════════════
```

**核心特性：**

- **状态共享**: 节点黑名单状态在节点池和多端口之间同步
  - 节点池中某节点失败被拉黑，多端口模式也会同步标记为不可用
  - 健康检查结果同时更新两种模式
- **端口自动重分配**: 如果端口被占用，自动分配下一个可用端口
- **灵活访问**: 节点池用于负载均衡，多端口用于直连特定节点

**适用场景：** 既需要自动故障转移，又需要直连特定节点

### 节点配置

**方式 1: 使用订阅链接（推荐）**

支持从订阅链接自动获取节点，支持多种格式：

```yaml
subscriptions:
  - "https://example.com/subscribe/v2ray"
  - "https://example.com/subscribe/clash"
```

支持的订阅格式：
- **Base64 编码**: V2Ray 标准订阅格式
- **Clash YAML**: Clash 配置文件格式
- **纯文本**: 每行一个节点 URI

**方式 2: 使用节点文件**

在 `config.yaml` 中指定：

```yaml
nodes_file: nodes.txt
```

`nodes.txt` 每行一个节点 URI：

```
vless://uuid@server:443?security=reality&sni=example.com#节点名称
hysteria2://password@server:443?sni=example.com#HY2节点
ss://base64@server:8388#SS节点
trojan://password@server:443?sni=example.com#Trojan节点
vmess://base64...#VMess节点
```

**方式 3: 直接在配置文件中**

```yaml
nodes:
  - uri: "vless://uuid@server:443#节点1"
  - name: custom-name
    uri: "ss://base64@server:8388"
    port: 24001  # 可选，手动指定端口
```

> **提示**: 可以同时使用多种方式，节点会自动合并。

## 支持的协议

| 协议 | URI 格式 | 特性 |
|------|----------|------|
| VMess | `vmess://` | WebSocket、HTTP/2、gRPC、TLS |
| VLESS | `vless://` | Reality、XTLS-Vision、多传输层 |
| Hysteria2 | `hysteria2://` | 带宽控制、混淆 |
| Shadowsocks | `ss://` | 多加密方式 |
| Trojan | `trojan://` | TLS、多传输层 |

### VMess 参数

VMess 支持两种 URI 格式：

**格式一：Base64 JSON（标准格式）**
```
vmess://base64({"v":"2","ps":"名称","add":"server","port":443,"id":"uuid","aid":0,"scy":"auto","net":"ws","type":"","host":"example.com","path":"/path","tls":"tls","sni":"example.com"})
```

**格式二：URL 格式**
```
vmess://uuid@server:port?encryption=auto&security=tls&sni=example.com&type=ws&host=example.com&path=/path#名称
```

- `net/type`: tcp, ws, h2, grpc
- `tls/security`: tls 或空
- `scy/encryption`: auto, aes-128-gcm, chacha20-poly1305 等

### VLESS 参数

```
vless://uuid@server:port?encryption=none&security=reality&sni=example.com&fp=chrome&pbk=xxx&sid=xxx&type=tcp&flow=xtls-rprx-vision#名称
```

- `security`: none, tls, reality
- `type`: tcp, ws, http, grpc, httpupgrade
- `flow`: xtls-rprx-vision (仅 TCP)
- `fp`: 指纹 (chrome, firefox, safari 等)

### Hysteria2 参数

```
hysteria2://password@server:port?sni=example.com&insecure=0&obfs=salamander&obfs-password=xxx#名称
```

- `upMbps` / `downMbps`: 带宽限制
- `obfs`: 混淆类型
- `obfs-password`: 混淆密码

## Web 监控面板

访问 `http://localhost:9090` 查看：

- 节点状态（健康/警告/异常/拉黑）
- 实时延迟
- 活跃连接数
- 失败次数统计
- 手动探测延迟
- 解除节点拉黑
- **一键导出节点**: 导出所有可用节点的代理池 URI（格式：`http://user:pass@host:port`）
- **订阅地址**: 支持 API Key 认证，可直接用于 Surge/Clash 等客户端订阅
- **设置**: 点击齿轮图标修改 `external_ip` 和 `probe_target`（立即保存生效）

### WebUI 设置

点击页面顶部的 ⚙️ 齿轮图标进入设置：

| 设置项 | 说明 |
|--------|------|
| 外部 IP 地址 | 导出节点时使用的 IP 地址（替换 `0.0.0.0`） |
| 探测目标 | 健康检查目标地址（格式：`host:port`） |

修改后立即保存到 `config.yaml`，无需重启即可生效。

### 节点管理

Web UI 提供**节点管理** Tab 页，支持节点的增删改查操作：

- **添加节点**: 通过 URI 添加新节点（名称自动从 URI fragment 提取）
- **编辑节点**: 修改现有节点配置
- **删除节点**: 从配置中移除节点
- **重载配置**: 重启 sing-box 内核使更改生效（⚠️ 会中断现有连接）
- **端口保留**: 重载后已有节点保持原有端口不变

Multi-Port 模式下，端口从 `base_port` 自动分配。

**API 端点：**

| 方法 | 端点 | 说明 |
|------|------|------|
| GET | `/api/nodes/config` | 获取所有配置节点 |
| POST | `/api/nodes/config` | 添加新节点 |
| PUT | `/api/nodes/config/:name` | 按名称更新节点 |
| DELETE | `/api/nodes/config/:name` | 按名称删除节点 |
| POST | `/api/reload` | 重载配置 |
| GET | `/api/export` | 导出可用节点（支持 `?format=surge&key=xxx`） |
| GET | `/api/settings` | 获取当前设置 |
| PUT | `/api/settings` | 更新设置（external_ip, probe_target） |

**请求示例：**

```bash
# 添加节点
curl -X POST http://localhost:9090/api/nodes/config \
  -H "Content-Type: application/json" \
  -d '{"uri": "vless://uuid@server:443#节点名称"}'

# 删除节点
curl -X DELETE http://localhost:9090/api/nodes/config/节点名称

# 重载配置
curl -X POST http://localhost:9090/api/reload
```

### 订阅地址

配置 `api_key` 后，可通过 URL 参数认证直接订阅，无需登录：

```yaml
management:
  password: "admin123"
  api_key: "your-secret-key"  # 订阅认证密钥
```

**订阅 URL 格式：**

| 格式 | URL |
|------|-----|
| HTTP | `http://host:9090/api/export?key=your-secret-key` |
| Surge | `http://host:9090/api/export?format=surge&key=your-secret-key` |

在 WebUI 导出弹窗中可直接复制订阅地址。

### 健康检查机制

程序启动时会自动对所有节点进行健康检查，之后定期检查：

- **初始检查**: 启动后立即检测所有节点的连通性
- **定期检查**: 每 5 分钟检查一次所有节点状态
- **智能过滤**: 不可用节点自动从 WebUI 和导出列表中隐藏
- **探测目标**: 通过 `management.probe_target` 配置（默认 `www.apple.com:80`）

```yaml
management:
  enabled: true
  listen: 0.0.0.0:9090
  probe_target: www.apple.com:80  # 健康检查探测目标
```

### 密码保护

为了保护节点信息安全，可以为 WebUI 设置访问密码：

```yaml
management:
  enabled: true
  listen: 0.0.0.0:9090
  password: "your_secure_password"  # 设置 WebUI 访问密码
```

- 如果 `password` 为空或不设置，则无需密码即可访问
- 设置密码后，首次访问会弹出登录界面
- 登录成功后，session 会保存 7 天

### 订阅定时刷新

支持定时自动刷新订阅链接，获取最新节点：

```yaml
subscription_refresh:
  enabled: true                 # 启用定时刷新
  interval: 1h                  # 刷新间隔（默认 1 小时）
  timeout: 30s                  # 获取订阅超时
  health_check_timeout: 60s     # 新节点健康检查超时
  drain_timeout: 30s            # 旧实例排空超时
  min_available_nodes: 1        # 最少可用节点数，低于此值不切换
```

> ⚠️ **重要提示：订阅刷新会导致连接中断**
>
> 订阅刷新时，程序会**重启 sing-box 内核**以加载新节点配置。这意味着：
>
> - **所有现有连接将被断开**
> - 正在进行的下载、流媒体播放等会中断
> - 客户端需要重新建立连接
>
> **建议：**
> - 将刷新间隔设置为较长时间（如 `1h` 或更长）
> - 避免在业务高峰期手动触发刷新
> - 如果对连接稳定性要求极高，建议关闭此功能（`enabled: false`）

**WebUI 和 API 支持：**

- WebUI 显示订阅状态（节点数、上次刷新时间、错误信息）
- 支持手动触发刷新按钮
- API 端点：
  - `GET /api/subscription/status` - 获取订阅状态
  - `POST /api/subscription/refresh` - 手动触发刷新

## 端口说明

| 端口 | 用途 |
|------|------|
| 2323 | 统一代理入口（节点池/混合模式） |
| 9090 | Web 监控面板 |
| 24000+ | 每节点独立端口（多端口/混合模式） |

## Docker 部署

**方式一：主机网络模式（推荐）**

使用 `network_mode: host` 直接使用主机网络，无需手动映射端口：

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

> **注意**: 配置文件需要可写权限以支持 WebUI 设置保存。如遇权限问题，请执行 `chmod 666 config.yaml nodes.txt`

> **优点**: 容器直接使用主机网络，所有端口自动对外开放。端口自动重分配功能可完美工作。

**方式二：端口映射模式**

手动指定需要映射的端口：

```yaml
# docker-compose.yml
services:
  easy-proxies:
    image: ghcr.io/jasonwong1991/easy_proxies:latest
    container_name: easy-proxies
    restart: unless-stopped
    ports:
      - "2323:2323"       # 节点池/混合模式入口
      - "9091:9091"       # Web 监控面板
      - "24000-24200:24000-24200"  # 多端口/混合模式
    volumes:
      - ./config.yaml:/etc/easy-proxies/config.yaml
      - ./nodes.txt:/etc/easy-proxies/nodes.txt
```

> **注意**: 多端口和混合模式需要映射足够的端口范围，建议预留一些缓冲端口用于自动重分配。

## 构建

```bash
# 基础构建
go build -o easy-proxies ./cmd/easy_proxies

# 完整功能构建
go build -tags "with_utls with_quic with_grpc with_wireguard with_gvisor" -o easy-proxies ./cmd/easy_proxies
```

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=jasonwong1991/easy_proxies&type=Date)](https://star-history.com/#jasonwong1991/easy_proxies&Date)

## 许可证

MIT License
