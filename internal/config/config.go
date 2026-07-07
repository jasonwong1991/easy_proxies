package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config describes the high level settings for the proxy pool server.
type Config struct {
	Mode                string                    `yaml:"mode"`
	Listener            ListenerConfig            `yaml:"listener"`
	MultiPort           MultiPortConfig           `yaml:"multi_port"`
	Pool                PoolConfig                `yaml:"pool"`
	Sticky              StickyConfig              `yaml:"sticky"`
	Management          ManagementConfig          `yaml:"management"`
	SubscriptionRefresh SubscriptionRefreshConfig `yaml:"subscription_refresh"`
	GeoIP               GeoIPConfig               `yaml:"geoip"`
	Log                 LogConfig                 `yaml:"log"`
	Nodes               []NodeConfig              `yaml:"nodes"`
	NodesFile           string                    `yaml:"nodes_file"`    // 节点文件路径，每行一个 URI
	Subscriptions       []string                  `yaml:"subscriptions"` // 订阅链接列表
	ExternalIP          string                    `yaml:"external_ip"`   // 外部 IP 地址，用于导出时替换 0.0.0.0
	LogLevel            string                    `yaml:"log_level"`
	SkipCertVerify      bool                      `yaml:"skip_cert_verify"` // 全局跳过 SSL 证书验证

	filePath string `yaml:"-"` // 配置文件路径，用于保存
}

// LogConfig controls log output and rotation.
type LogConfig struct {
	Output     string `yaml:"output"`      // 日志输出: "stdout", "file", 默认 "stdout"
	File       string `yaml:"file"`        // 日志文件路径，默认 "logs/easy_proxies.log"
	MaxSize    int    `yaml:"max_size"`    // 单个日志文件最大 MB，默认 50
	MaxBackups int    `yaml:"max_backups"` // 保留旧日志文件个数，默认 3
	MaxAge     int    `yaml:"max_age"`     // 保留旧日志文件天数，默认 7
	Compress   bool   `yaml:"compress"`    // 是否压缩旧日志，默认 false
}

// GeoIPConfig controls GeoIP-based region routing.
type GeoIPConfig struct {
	Enabled            bool          `yaml:"enabled"`              // 是否启用 GeoIP 地域分区
	DatabasePath       string        `yaml:"database_path"`        // GeoLite2-Country.mmdb 文件路径
	Listen             string        `yaml:"listen"`               // GeoIP 路由监听地址，默认使用 listener 配置
	Port               uint16        `yaml:"port"`                 // GeoIP 路由监听端口，默认 1221
	AutoUpdateEnabled  bool          `yaml:"auto_update_enabled"`  // 是否启用自动更新数据库
	AutoUpdateInterval time.Duration `yaml:"auto_update_interval"` // 自动更新间隔，默认 24 小时
}

// ListenerConfig defines how the HTTP/SOCKS5 mixed proxy should listen for clients.
type ListenerConfig struct {
	Address  string `yaml:"address"`
	Port     uint16 `yaml:"port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// StickyConfig configures an optional dedicated sticky-session entry port.
// When enabled (pool/hybrid mode only), clients connecting to this port are
// pinned to a single upstream node by source IP, keeping the egress IP stable.
// The sticky port reuses the listener's address and credentials.
type StickyConfig struct {
	Enabled bool   `yaml:"enabled"`
	Port    uint16 `yaml:"port"`
}

// PoolConfig configures scheduling + failure handling.
type PoolConfig struct {
	Mode              string        `yaml:"mode"`
	FailureThreshold  int           `yaml:"failure_threshold"`
	BlacklistDuration time.Duration `yaml:"blacklist_duration"`
	ExcludeKeywords   []string      `yaml:"exclude_keywords,omitempty"`
	// RetryEnabled toggles automatic fail-over to another member when a dial fails.
	// nil/unset → default true. Use *bool so users can explicitly disable via YAML.
	RetryEnabled *bool `yaml:"retry_enabled,omitempty"`
	// RetryAttempts is the maximum total dial attempts per request (including the first).
	// Default 3. Values <= 0 are normalized to 3.
	// For pools with multiple members, each retry picks a different member when possible.
	// For single-member pools (e.g. per-node multi-port pools), retries dial the same member.
	RetryAttempts int `yaml:"retry_attempts,omitempty"`
}

// RetryEnabledOrDefault reports whether retry is enabled (default true).
func (p PoolConfig) RetryEnabledOrDefault() bool {
	if p.RetryEnabled == nil {
		return true
	}
	return *p.RetryEnabled
}

// MultiPortConfig defines address/credential defaults for multi-port mode.
type MultiPortConfig struct {
	Address  string `yaml:"address"`
	BasePort uint16 `yaml:"base_port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// ManagementConfig controls the monitoring HTTP endpoint.
type ManagementConfig struct {
	Enabled          *bool  `yaml:"enabled"`
	Listen           string `yaml:"listen"`
	ProbeTarget      string `yaml:"probe_target"`
	Password         string `yaml:"password"`          // WebUI 访问密码，为空则不需要密码
	ProbeConcurrency int    `yaml:"probe_concurrency"` // 并发探测线程数（8-1024，默认 32），大规模节点可调高以加快探测
}

// SubscriptionRefreshConfig controls subscription auto-refresh and reload settings.
type SubscriptionRefreshConfig struct {
	Enabled            bool          `yaml:"enabled"`              // 是否启用定时刷新
	Interval           time.Duration `yaml:"interval"`             // 刷新间隔，默认 1 小时
	Timeout            time.Duration `yaml:"timeout"`              // 获取订阅的超时时间
	HealthCheckTimeout time.Duration `yaml:"health_check_timeout"` // 新节点健康检查超时
	DrainTimeout       time.Duration `yaml:"drain_timeout"`        // 旧实例排空超时时间
	MinAvailableNodes  int           `yaml:"min_available_nodes"`  // 最少可用节点数，低于此值不切换
}

// NodeSource indicates where a node configuration originated from.
type NodeSource string

const (
	NodeSourceInline       NodeSource = "inline"       // Defined directly in config.yaml nodes array
	NodeSourceFile         NodeSource = "nodes_file"   // Loaded from external nodes file
	NodeSourceSubscription NodeSource = "subscription" // Fetched from subscription URL
)

// NodeConfig describes a single upstream proxy endpoint expressed as URI.
type NodeConfig struct {
	Name        string     `yaml:"name" json:"name"`
	URI         string     `yaml:"uri" json:"uri"`
	Port        uint16     `yaml:"port,omitempty" json:"port,omitempty"`
	Username    string     `yaml:"username,omitempty" json:"username,omitempty"`
	Password    string     `yaml:"password,omitempty" json:"password,omitempty"`
	PoolEnabled *bool      `yaml:"pool_enabled,omitempty" json:"pool_enabled,omitempty"`
	Source      NodeSource `yaml:"-" json:"source,omitempty"` // Runtime only, not persisted
}

// NodeKey returns a stable identifier for the node, used to preserve port
// assignments across subscription refreshes and reloads.
//
// The identity deliberately ignores the parts of a proxy URI that subscription
// providers commonly mutate without changing the underlying server: the
// display name (#fragment) and query-parameter ordering. As long as the
// scheme, credentials, host, port and parameter set are unchanged, a node
// keeps the same key — and therefore the same proxy port.
func (n *NodeConfig) NodeKey() string {
	return stableNodeKey(n.URI)
}

// NodePoolEnabled reports whether a node should participate in shared pool
// entries. Per-node configuration wins over keyword matching.
func (c *Config) NodePoolEnabled(node NodeConfig, tag string) bool {
	if node.PoolEnabled != nil {
		return *node.PoolEnabled
	}
	if c == nil || len(c.Pool.ExcludeKeywords) == 0 {
		return true
	}
	haystack := strings.ToLower(node.Name + "\n" + tag + "\n" + node.URI)
	for _, keyword := range c.Pool.ExcludeKeywords {
		keyword = strings.ToLower(strings.TrimSpace(keyword))
		if keyword != "" && strings.Contains(haystack, keyword) {
			return false
		}
	}
	return true
}

// stableNodeKey derives a port-stable identity from a proxy URI by stripping the
// volatile display name and canonicalizing query order. It never errors: on any
// parse failure it falls back to the raw URI minus its fragment, so the result
// is always at least as stable as the previous full-URI behavior.
func stableNodeKey(uri string) string {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return ""
	}

	// vmess:// is base64-encoded JSON rather than a standard URL; only strip an
	// appended fragment and keep the payload as the identity.
	if strings.HasPrefix(uri, "vmess://") {
		if idx := strings.Index(uri, "#"); idx != -1 {
			return strings.TrimSpace(uri[:idx])
		}
		return uri
	}

	u, err := url.Parse(uri)
	if err != nil {
		if idx := strings.LastIndex(uri, "#"); idx != -1 {
			return strings.TrimSpace(uri[:idx])
		}
		return uri
	}

	u.Fragment = "" // display name — volatile, not part of node identity
	if u.RawQuery != "" {
		// Encode() sorts keys, so reordered parameters yield the same key.
		u.RawQuery = u.Query().Encode()
	}
	return u.String()
}

// Load reads YAML config from disk and applies defaults/validation.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	cfg.filePath = path

	// Resolve nodes_file path relative to config file directory
	if cfg.NodesFile != "" && !filepath.IsAbs(cfg.NodesFile) {
		configDir := filepath.Dir(path)
		cfg.NodesFile = filepath.Join(configDir, cfg.NodesFile)
	}

	if err := cfg.normalize(); err != nil {
		return nil, err
	}

	cfg.ApplyNodePrefs()

	// Restore persisted proxy ports so a restart keeps the same port per node.
	if err := cfg.applyPersistedPorts(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// ExtractNodeName extracts a human-readable name from a proxy URI.
// For standard URIs (vless://, ss://, trojan://), it extracts from the URL fragment (#name).
// For vmess:// URIs, it base64-decodes the payload and extracts the "ps" field.
func ExtractNodeName(uri string) string {
	uri = strings.TrimSpace(uri)

	// Handle vmess:// specially - it's base64-encoded JSON, not a standard URL
	if strings.HasPrefix(uri, "vmess://") {
		payload := strings.TrimPrefix(uri, "vmess://")
		// Remove any fragment that might be appended
		if idx := strings.Index(payload, "#"); idx != -1 {
			payload = payload[:idx]
		}
		payload = strings.TrimSpace(payload)
		// Try standard base64 first, then raw/URL-safe variants
		var decoded []byte
		var err error
		decoded, err = base64.StdEncoding.DecodeString(payload)
		if err != nil {
			decoded, err = base64.RawStdEncoding.DecodeString(payload)
		}
		if err != nil {
			decoded, err = base64.RawURLEncoding.DecodeString(payload)
		}
		if err == nil {
			var vmess struct {
				PS string `json:"ps"`
			}
			if json.Unmarshal(decoded, &vmess) == nil && vmess.PS != "" {
				return strings.TrimSpace(vmess.PS)
			}
		}
		return ""
	}

	// For standard URIs, extract from URL fragment (#name)
	if idx := strings.LastIndex(uri, "#"); idx != -1 && idx < len(uri)-1 {
		fragment := uri[idx+1:]
		if decoded, err := url.QueryUnescape(fragment); err == nil && decoded != "" {
			return strings.TrimSpace(decoded)
		}
		return strings.TrimSpace(fragment)
	}

	return ""
}

func (c *Config) normalize() error {
	if c.Mode == "" {
		c.Mode = "pool"
	}
	// Normalize mode name: support both multi-port and multi_port
	if c.Mode == "multi_port" {
		c.Mode = "multi-port"
	}
	switch c.Mode {
	case "pool", "multi-port", "hybrid":
	default:
		return fmt.Errorf("unsupported mode %q (use 'pool', 'multi-port', or 'hybrid')", c.Mode)
	}
	if c.Listener.Address == "" {
		c.Listener.Address = "0.0.0.0"
	}
	if c.Listener.Port == 0 {
		c.Listener.Port = 2323
	}
	if c.Pool.Mode == "" {
		c.Pool.Mode = "sequential"
	}
	if c.Pool.FailureThreshold <= 0 {
		c.Pool.FailureThreshold = 3
	}
	if c.Pool.BlacklistDuration <= 0 {
		c.Pool.BlacklistDuration = 24 * time.Hour
	}
	if c.Pool.RetryAttempts <= 0 {
		c.Pool.RetryAttempts = 3
	}
	if c.MultiPort.Address == "" {
		c.MultiPort.Address = "0.0.0.0"
	}
	if c.MultiPort.BasePort == 0 {
		c.MultiPort.BasePort = 24000
	}
	if c.Management.Listen == "" {
		c.Management.Listen = "127.0.0.1:9091"
	}
	if c.Management.ProbeTarget == "" {
		c.Management.ProbeTarget = "www.apple.com:80"
	}
	if c.Management.Enabled == nil {
		defaultEnabled := true
		c.Management.Enabled = &defaultEnabled
	}

	// Subscription refresh defaults
	if c.SubscriptionRefresh.Interval <= 0 {
		c.SubscriptionRefresh.Interval = 1 * time.Hour
	}
	if c.SubscriptionRefresh.Timeout <= 0 {
		c.SubscriptionRefresh.Timeout = 30 * time.Second
	}
	if c.SubscriptionRefresh.HealthCheckTimeout <= 0 {
		c.SubscriptionRefresh.HealthCheckTimeout = 60 * time.Second
	}
	if c.SubscriptionRefresh.DrainTimeout <= 0 {
		c.SubscriptionRefresh.DrainTimeout = 30 * time.Second
	}
	if c.SubscriptionRefresh.MinAvailableNodes <= 0 {
		c.SubscriptionRefresh.MinAvailableNodes = 1
	}

	// Mark inline nodes with source
	for idx := range c.Nodes {
		c.Nodes[idx].Source = NodeSourceInline
	}

	// Load nodes from file if specified (but NOT if subscriptions exist - subscription takes priority)
	if c.NodesFile != "" && len(c.Subscriptions) == 0 {
		fileNodes, err := loadNodesFromFile(c.NodesFile)
		if err != nil {
			return fmt.Errorf("load nodes from file %q: %w", c.NodesFile, err)
		}
		for idx := range fileNodes {
			fileNodes[idx].Source = NodeSourceFile
		}
		c.Nodes = append(c.Nodes, fileNodes...)
	}

	// Load nodes from subscriptions (highest priority - writes to nodes.txt)
	if len(c.Subscriptions) > 0 {
		var subNodes []NodeConfig
		subTimeout := c.SubscriptionRefresh.Timeout
		for _, subURL := range c.Subscriptions {
			nodes, err := loadNodesFromSubscription(subURL, subTimeout)
			if err != nil {
				log.Printf("⚠️ Failed to load subscription %q: %v (skipping)", subURL, err)
				continue
			}
			log.Printf("✅ Loaded %d nodes from subscription", len(nodes))
			subNodes = append(subNodes, nodes...)
		}
		// Mark subscription nodes and write to nodes.txt
		for idx := range subNodes {
			subNodes[idx].Source = NodeSourceSubscription
		}
		if len(subNodes) > 0 {
			// Determine nodes.txt path
			nodesFilePath := c.NodesFile
			if nodesFilePath == "" {
				nodesFilePath = filepath.Join(filepath.Dir(c.filePath), "nodes.txt")
				c.NodesFile = nodesFilePath
			}
			// Write subscription nodes to nodes.txt
			if err := writeNodesToFile(nodesFilePath, subNodes); err != nil {
				log.Printf("⚠️ Failed to write nodes to %q: %v", nodesFilePath, err)
			} else {
				log.Printf("✅ Written %d subscription nodes to %s", len(subNodes), nodesFilePath)
			}
		}
		c.Nodes = append(c.Nodes, subNodes...)
		// Fallback: if all subscriptions failed, try loading cached nodes.txt
		if len(subNodes) == 0 && c.NodesFile != "" {
			cachedNodes, err := loadNodesFromFile(c.NodesFile)
			if err == nil && len(cachedNodes) > 0 {
				log.Printf("⚠️  All subscriptions failed, using %d cached nodes from %s", len(cachedNodes), c.NodesFile)
				for idx := range cachedNodes {
					cachedNodes[idx].Source = NodeSourceSubscription
				}
				c.Nodes = append(c.Nodes, cachedNodes...)
			}
		}
	}

	if len(c.Nodes) == 0 {
		return errors.New("config.nodes cannot be empty (configure nodes in config or use nodes_file)")
	}
	// portCursor is an int (not uint16) so the >65535 exhaustion guard fires
	// instead of wrapping to 0 and assigning unbindable low ports.
	portCursor := int(c.MultiPort.BasePort)
	for idx := range c.Nodes {
		c.Nodes[idx].Name = strings.TrimSpace(c.Nodes[idx].Name)
		c.Nodes[idx].URI = strings.TrimSpace(c.Nodes[idx].URI)

		if c.Nodes[idx].URI == "" {
			return fmt.Errorf("node %d is missing uri", idx)
		}

		// Auto-extract name from URI if not provided
		if c.Nodes[idx].Name == "" {
			c.Nodes[idx].Name = ExtractNodeName(c.Nodes[idx].URI)
		}
		// Fallback to default name if still empty
		if c.Nodes[idx].Name == "" {
			c.Nodes[idx].Name = fmt.Sprintf("node-%d", idx)
		}

		// Provisional port assignment only. The real, bind-checked, collision-safe
		// assignment is done once by applyPersistedPorts → NormalizeWithPortMap
		// right after normalize() returns. Probing IsPortAvailable per node here
		// would double the socket open/close work at startup (16k+ syscalls for
		// 8k nodes) for a result that is immediately discarded.
		if c.Nodes[idx].Port == 0 && (c.Mode == "multi-port" || c.Mode == "hybrid") {
			c.Nodes[idx].Port = uint16(portCursor)
			portCursor++
		} else if c.Nodes[idx].Port == 0 {
			c.Nodes[idx].Port = uint16(portCursor)
			portCursor++
		}

		if c.Mode == "multi-port" || c.Mode == "hybrid" {
			if c.Nodes[idx].Username == "" {
				c.Nodes[idx].Username = c.MultiPort.Username
				c.Nodes[idx].Password = c.MultiPort.Password
			}
		}
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}

	// Log config defaults
	c.normalizeLogConfig()

	// Auto-fix port conflicts in hybrid mode (pool port vs multi-port)
	if c.Mode == "hybrid" {
		poolPort := c.Listener.Port
		usedPorts := make(map[uint16]bool)
		usedPorts[poolPort] = true
		for idx := range c.Nodes {
			usedPorts[c.Nodes[idx].Port] = true
		}
		for idx := range c.Nodes {
			if c.Nodes[idx].Port == poolPort {
				// Find next available port
				newPort := c.Nodes[idx].Port + 1
				for usedPorts[newPort] || !IsPortAvailable(c.MultiPort.Address, newPort) {
					newPort++
					if newPort > 65535 {
						return fmt.Errorf("no available port for node %q after conflict with pool port %d", c.Nodes[idx].Name, poolPort)
					}
				}
				log.Printf("⚠️  Node %q port %d conflicts with pool port, reassigned to %d", c.Nodes[idx].Name, poolPort, newPort)
				usedPorts[newPort] = true
				c.Nodes[idx].Port = newPort
			}
		}
	}

	if err := c.normalizeSticky(); err != nil {
		return err
	}

	return nil
}

// BuildPortMap creates a mapping from node URI to port for existing nodes.
// This is used to preserve port assignments when reloading configuration.
func (c *Config) BuildPortMap() map[string]uint16 {
	portMap := make(map[string]uint16)
	for _, node := range c.Nodes {
		if node.Port > 0 {
			portMap[node.NodeKey()] = node.Port
		}
	}
	return portMap
}

// nodePortMapFile is the sidecar file storing node→port assignments so they
// survive a process restart.
const nodePortMapFile = "node_ports.json"

const nodePrefsFile = "node_prefs.json"

type NodePrefs struct {
	PoolEnabled *bool `json:"pool_enabled,omitempty"`
}

// portMapPath returns the path of the port-map sidecar, located next to the
// main config file. It is empty when the config path is unknown.
func (c *Config) portMapPath() string {
	if c.filePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(c.filePath), nodePortMapFile)
}

func (c *Config) nodePrefsPath() string {
	if c.filePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(c.filePath), nodePrefsFile)
}

// loadNodePortMap reads a previously saved stableNodeKey→port mapping. It
// returns nil on any error (missing file, unreadable, bad JSON); callers treat
// that as "no persisted ports", so a corrupt sidecar never blocks startup.
func loadNodePortMap(path string) map[string]uint16 {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m map[string]uint16
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	return m
}

func loadNodePrefs(path string) map[string]NodePrefs {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m map[string]NodePrefs
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	return m
}

// ApplyNodePrefs restores persisted per-node preferences for file/subscription
// nodes. Inline nodes keep their explicit config.yaml values.
func (c *Config) ApplyNodePrefs() {
	if c == nil {
		return
	}
	prefs := loadNodePrefs(c.nodePrefsPath())
	if len(prefs) == 0 {
		return
	}
	for i := range c.Nodes {
		switch c.Nodes[i].Source {
		case NodeSourceFile, NodeSourceSubscription:
		default:
			continue
		}
		pref, ok := prefs[c.Nodes[i].NodeKey()]
		if !ok || pref.PoolEnabled == nil {
			continue
		}
		c.Nodes[i].PoolEnabled = pref.PoolEnabled
	}
}

func (c *Config) BuildNodePrefs() map[string]NodePrefs {
	prefs := make(map[string]NodePrefs)
	for _, node := range c.Nodes {
		switch node.Source {
		case NodeSourceFile, NodeSourceSubscription:
		default:
			continue
		}
		if node.PoolEnabled == nil {
			continue
		}
		prefs[node.NodeKey()] = NodePrefs{PoolEnabled: node.PoolEnabled}
	}
	return prefs
}

func (c *Config) SaveNodePrefs() error {
	if c == nil {
		return errors.New("config is nil")
	}
	path := c.nodePrefsPath()
	if path == "" {
		return errors.New("config file path is unknown")
	}
	data, err := json.MarshalIndent(c.BuildNodePrefs(), "", "  ")
	if err != nil {
		return fmt.Errorf("encode node prefs: %w", err)
	}
	if err := writeFileWithLock(path, data, 0o644); err != nil {
		return fmt.Errorf("write node prefs %q: %w", path, err)
	}
	return nil
}

// bridgeLegacyPortKeys upgrades a port-map sidecar written by an older version
// (keyed by the raw full node URI) to the current stable-key scheme. For each
// node whose stable key is absent but whose raw URI is present in the map, it
// copies the port under the stable key. This makes a one-time format upgrade
// transparent: ports are preserved instead of all being reassigned on the first
// boot after upgrading. It mutates saved in place and is a no-op once the
// sidecar has been rewritten with stable keys.
func bridgeLegacyPortKeys(nodes []NodeConfig, saved map[string]uint16) {
	for i := range nodes {
		stableKey := nodes[i].NodeKey()
		if _, ok := saved[stableKey]; ok {
			continue
		}
		legacyKey := strings.TrimSpace(nodes[i].URI)
		if port, ok := saved[legacyKey]; ok && port > 0 {
			saved[stableKey] = port
		}
	}
}

// SaveNodePortMap persists the current node→port assignments next to the config
// file so a restart can restore them. It is a no-op in pool mode, where nodes
// have no per-node ports.
func (c *Config) SaveNodePortMap() error {
	if c == nil {
		return errors.New("config is nil")
	}
	if c.Mode != "multi-port" && c.Mode != "hybrid" {
		return nil
	}
	path := c.portMapPath()
	if path == "" {
		return errors.New("config file path is unknown")
	}
	data, err := json.MarshalIndent(c.BuildPortMap(), "", "  ")
	if err != nil {
		return fmt.Errorf("encode port map: %w", err)
	}
	if err := writeFileWithLock(path, data, 0o644); err != nil {
		return fmt.Errorf("write port map %q: %w", path, err)
	}
	return nil
}

// applyPersistedPorts restores the on-disk node→port mapping so a restart keeps
// every node on the proxy port it previously used, then rewrites the mapping so
// the sidecar exists from first boot and drops entries for removed nodes.
//
// It clears the provisional ports assigned by normalize() first, making
// NormalizeWithPortMap the single, collision-safe authority: nodes whose stable
// identity matches a saved entry get their saved port, and the rest get fresh,
// non-conflicting ports. A corrupt or missing sidecar simply means "no saved
// ports" and the freshly assigned ports stand.
func (c *Config) applyPersistedPorts() error {
	if c.Mode != "multi-port" && c.Mode != "hybrid" {
		return nil
	}
	// normalize() only assigned provisional ports (no bind checks). Run the
	// authoritative, bind-checked assignment exactly once here. A saved sidecar
	// supplies preserved ports; an empty/missing one means "assign all fresh".
	saved := loadNodePortMap(c.portMapPath())
	if len(saved) > 0 {
		// Migrate any legacy entries: sidecars written before stableNodeKey was
		// keyed by the raw full URI. Without this bridge, every node's stable key
		// would miss on the first post-upgrade boot and all proxy ports would be
		// reassigned at once. Map legacy raw-URI keys onto the new stable keys.
		bridgeLegacyPortKeys(c.Nodes, saved)
	}
	for i := range c.Nodes {
		c.Nodes[i].Port = 0
	}
	if err := c.NormalizeWithPortMap(saved); err != nil {
		return fmt.Errorf("restore persisted ports: %w", err)
	}
	// Persisting is best-effort by design: the proxy runs correctly without the
	// sidecar; only a subsequent restart would re-derive ports. A write failure
	// is logged rather than fatal.
	if err := c.SaveNodePortMap(); err != nil {
		log.Printf("⚠️  Failed to persist node ports: %v", err)
	}
	return nil
}

// NormalizeWithPortMap applies defaults and validation, preserving port assignments
// for nodes that exist in the provided port map.
func (c *Config) NormalizeWithPortMap(portMap map[string]uint16) error {
	if c.Mode == "" {
		c.Mode = "pool"
	}
	if c.Mode == "multi_port" {
		c.Mode = "multi-port"
	}
	switch c.Mode {
	case "pool", "multi-port", "hybrid":
	default:
		return fmt.Errorf("unsupported mode %q (use 'pool', 'multi-port', or 'hybrid')", c.Mode)
	}
	if c.Listener.Address == "" {
		c.Listener.Address = "0.0.0.0"
	}
	if c.Listener.Port == 0 {
		c.Listener.Port = 2323
	}
	if c.Pool.Mode == "" {
		c.Pool.Mode = "sequential"
	}
	if c.Pool.FailureThreshold <= 0 {
		c.Pool.FailureThreshold = 3
	}
	if c.Pool.BlacklistDuration <= 0 {
		c.Pool.BlacklistDuration = 24 * time.Hour
	}
	if c.Pool.RetryAttempts <= 0 {
		c.Pool.RetryAttempts = 3
	}
	if c.MultiPort.Address == "" {
		c.MultiPort.Address = "0.0.0.0"
	}
	if c.MultiPort.BasePort == 0 {
		c.MultiPort.BasePort = 24000
	}
	if c.Management.Listen == "" {
		c.Management.Listen = "127.0.0.1:9091"
	}
	if c.Management.ProbeTarget == "" {
		c.Management.ProbeTarget = "www.apple.com:80"
	}
	if c.Management.Enabled == nil {
		defaultEnabled := true
		c.Management.Enabled = &defaultEnabled
	}
	if c.SubscriptionRefresh.Interval <= 0 {
		c.SubscriptionRefresh.Interval = 1 * time.Hour
	}
	if c.SubscriptionRefresh.Timeout <= 0 {
		c.SubscriptionRefresh.Timeout = 30 * time.Second
	}
	if c.SubscriptionRefresh.HealthCheckTimeout <= 0 {
		c.SubscriptionRefresh.HealthCheckTimeout = 60 * time.Second
	}
	if c.SubscriptionRefresh.DrainTimeout <= 0 {
		c.SubscriptionRefresh.DrainTimeout = 30 * time.Second
	}
	if c.SubscriptionRefresh.MinAvailableNodes <= 0 {
		c.SubscriptionRefresh.MinAvailableNodes = 1
	}

	if len(c.Nodes) == 0 {
		return errors.New("config.nodes cannot be empty")
	}

	// Build set of ports already assigned from portMap
	usedPorts := make(map[uint16]bool)
	if c.Mode == "hybrid" {
		usedPorts[c.Listener.Port] = true
	}

	// First pass: assign ports from portMap for existing nodes
	for idx := range c.Nodes {
		c.Nodes[idx].Name = strings.TrimSpace(c.Nodes[idx].Name)
		c.Nodes[idx].URI = strings.TrimSpace(c.Nodes[idx].URI)
		if c.Nodes[idx].URI == "" {
			return fmt.Errorf("node %d is missing uri", idx)
		}

		// Auto-extract name from URI if not provided
		if c.Nodes[idx].Name == "" {
			c.Nodes[idx].Name = ExtractNodeName(c.Nodes[idx].URI)
		}
		if c.Nodes[idx].Name == "" {
			c.Nodes[idx].Name = fmt.Sprintf("node-%d", idx)
		}

		// Check if this node has a preserved port from portMap. Guard against a
		// port that was already claimed by an earlier node sharing the same
		// stable key (e.g. a subscription listing the same server twice under
		// different display names): preserving it again would bind the same
		// proxy port twice (EADDRINUSE). Such a node is left at Port==0 so the
		// second pass assigns it a fresh, collision-free port.
		if c.Mode == "multi-port" || c.Mode == "hybrid" {
			nodeKey := c.Nodes[idx].NodeKey()
			if existingPort, ok := portMap[nodeKey]; ok && existingPort > 0 {
				if usedPorts[existingPort] {
					log.Printf("⚠️  Port %d already assigned to another node with the same identity; node %q will get a fresh port", existingPort, c.Nodes[idx].Name)
				} else {
					c.Nodes[idx].Port = existingPort
					usedPorts[existingPort] = true
					log.Printf("✅ Preserved port %d for node %q", existingPort, c.Nodes[idx].Name)
				}
			}
		}
	}

	// Second pass: assign new ports for nodes without preserved ports. portCursor
	// is an int (not uint16) so the >65535 exhaustion guard actually fires:
	// a uint16 cursor would wrap to 0 and silently hand out unbindable low ports.
	portCursor := int(c.MultiPort.BasePort)
	for idx := range c.Nodes {
		if c.Nodes[idx].Port == 0 && (c.Mode == "multi-port" || c.Mode == "hybrid") {
			// Find next available port that's not used
			for usedPorts[uint16(portCursor)] || !IsPortAvailable(c.MultiPort.Address, uint16(portCursor)) {
				portCursor++
				if portCursor > 65535 {
					return fmt.Errorf("no available ports found starting from %d", c.MultiPort.BasePort)
				}
			}
			c.Nodes[idx].Port = uint16(portCursor)
			usedPorts[uint16(portCursor)] = true
			log.Printf("📌 Assigned new port %d for node %q", portCursor, c.Nodes[idx].Name)
			portCursor++
		} else if c.Nodes[idx].Port == 0 {
			c.Nodes[idx].Port = uint16(portCursor)
			portCursor++
		}

		// Apply default credentials
		if c.Mode == "multi-port" || c.Mode == "hybrid" {
			if c.Nodes[idx].Username == "" {
				c.Nodes[idx].Username = c.MultiPort.Username
				c.Nodes[idx].Password = c.MultiPort.Password
			}
		}
	}

	if c.LogLevel == "" {
		c.LogLevel = "info"
	}

	c.normalizeLogConfig()

	if err := c.normalizeSticky(); err != nil {
		return err
	}

	return nil
}

// normalizeSticky applies defaults and validation for the optional sticky entry port.
// Sticky sessions only apply to the shared pool entry, so they are disabled
// outside pool/hybrid mode. Must run after node ports are assigned.
func (c *Config) normalizeSticky() error {
	if !c.Sticky.Enabled {
		return nil
	}
	if c.Mode != "pool" && c.Mode != "hybrid" {
		log.Printf("⚠️  sticky.enabled is set but mode is %q; sticky only applies to pool/hybrid mode, disabling", c.Mode)
		c.Sticky.Enabled = false
		return nil
	}
	if c.Sticky.Port == 0 {
		if c.Listener.Port >= 65535 {
			return fmt.Errorf("cannot auto-assign sticky.port (listener.port is %d); set sticky.port explicitly", c.Listener.Port)
		}
		c.Sticky.Port = c.Listener.Port + 1
	}
	if c.Sticky.Port == c.Listener.Port {
		return fmt.Errorf("sticky.port %d conflicts with listener.port", c.Sticky.Port)
	}
	for idx := range c.Nodes {
		if c.Nodes[idx].Port == c.Sticky.Port {
			return fmt.Errorf("sticky.port %d conflicts with node %q port", c.Sticky.Port, c.Nodes[idx].Name)
		}
	}
	return nil
}

// normalizeLogConfig applies defaults to the log config.
func (c *Config) normalizeLogConfig() {
	if c.Log.Output == "" {
		c.Log.Output = "stdout"
	}
	if c.Log.File == "" {
		c.Log.File = "logs/easy_proxies.log"
	}
	// Resolve relative log file path against config dir
	if c.filePath != "" && !filepath.IsAbs(c.Log.File) {
		c.Log.File = filepath.Join(filepath.Dir(c.filePath), c.Log.File)
	}
	if c.Log.MaxSize <= 0 {
		c.Log.MaxSize = 50
	}
	if c.Log.MaxBackups <= 0 {
		c.Log.MaxBackups = 3
	}
	if c.Log.MaxAge <= 0 {
		c.Log.MaxAge = 7
	}
}

// ManagementEnabled reports whether the monitoring endpoint should run.
func (c *Config) ManagementEnabled() bool {
	if c.Management.Enabled == nil {
		return true
	}
	return *c.Management.Enabled
}

// ProbeConcurrencyOrDefault returns the configured probe concurrency clamped
// to a safe range (1-1024). When unset or invalid, a sensible default is used.
func (c *Config) ProbeConcurrencyOrDefault() int {
	v := c.Management.ProbeConcurrency
	if v <= 0 {
		return 32
	}
	if v > 1024 {
		return 1024
	}
	return v
}

// loadNodesFromFile reads a nodes file where each line is a proxy URI
// Lines starting with # are comments, empty lines are ignored
func loadNodesFromFile(path string) ([]NodeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseNodesFromContent(string(data))
}

// loadNodesFromSubscription fetches and parses nodes from a subscription URL
// Supports multiple formats: base64 encoded, plain text, clash yaml, etc.
func loadNodesFromSubscription(subURL string, timeout time.Duration) ([]NodeConfig, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	client := &http.Client{
		Timeout: timeout,
	}

	req, err := http.NewRequest("GET", subURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	// Use clash-compatible User-Agent to get Clash YAML format from subscription servers
	// This ensures we receive structured YAML with all proxy types (AnyTLS, TUIC, etc.)
	// instead of base64-encoded content that may only contain basic SS nodes
	req.Header.Set("User-Agent", "clash-verge/v2.2.3")
	req.Header.Set("Accept", "*/*")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch subscription: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("subscription returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	content := string(body)

	// Try to detect and parse different formats
	return parseSubscriptionContent(content)
}

// parseSubscriptionContent tries to parse subscription content in various formats (optimized)
func parseSubscriptionContent(content string) ([]NodeConfig, error) {
	content = strings.TrimSpace(content)

	// Detect Clash YAML by a line-anchored top-level "proxies:" key anywhere in
	// the document. The whole content is scanned (not just a 16 KB prefix): full
	// Clash configs often place proxies after large dns / rule / proxy-provider
	// sections, so a fixed-size window would misdetect them as base64/plaintext
	// and drop every node. Line-anchoring avoids matching a stray "proxies:"
	// inside a base64 blob.
	if strings.HasPrefix(content, "proxies:") || strings.Contains(content, "\nproxies:") {
		return parseClashYAML(content)
	}

	// Check if it's base64 encoded (common for v2ray subscriptions)
	if isBase64(content) {
		decoded, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			// Try URL-safe base64
			decoded, err = base64.RawStdEncoding.DecodeString(content)
			if err != nil {
				// Not base64, try as plain text
				return parseNodesFromContent(content)
			}
		}
		content = string(decoded)
	}

	// Parse as plain text (one URI per line)
	return parseNodesFromContent(content)
}

// ParseSubscriptionContent parses subscription content in various formats (base64, plain text, Clash YAML).
// This is exported for use by the subscription manager.
func ParseSubscriptionContent(content string) ([]NodeConfig, error) {
	return parseSubscriptionContent(content)
}

// parseNodesFromContent parses nodes from plain text content (one URI per line)
func parseNodesFromContent(content string) ([]NodeConfig, error) {
	var nodes []NodeConfig
	lines := strings.Split(content, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Check if it's a valid proxy URI
		if IsProxyURI(line) {
			nodes = append(nodes, NodeConfig{
				URI: line,
			})
		}
	}

	return nodes, nil
}

// isBase64 checks if a string looks like base64 encoded content (optimized version)
func isBase64(s string) bool {
	// Remove whitespace
	s = strings.TrimSpace(s)
	if len(s) == 0 {
		return false
	}

	// Remove newlines for checking
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, "\r", "")

	// Quick check: if it contains proxy URI schemes, it's not base64
	if strings.Contains(s, "://") {
		return false
	}

	// Check character set - base64 only contains A-Za-z0-9+/=
	// This is much faster than trying to decode
	for _, c := range s {
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '+' || c == '/' || c == '=') {
			return false
		}
	}

	// Length must be multiple of 4 (with padding)
	return len(s)%4 == 0
}

// IsProxyURI checks if a string is a valid proxy URI
func IsProxyURI(s string) bool {
	schemes := []string{"vmess://", "vless://", "trojan://", "ss://", "shadowsocks://", "ssr://", "hysteria://", "hysteria2://", "hy2://", "tuic://", "socks5://", "socks5h://", "socks://", "http://", "https://", "anytls://"}
	lower := strings.ToLower(s)
	for _, scheme := range schemes {
		if strings.HasPrefix(lower, scheme) {
			return true
		}
	}
	return false
}

// clashConfig represents a minimal Clash configuration for parsing proxies
// flexInt handles YAML values that may be either int or quoted string.
type flexInt int

func (fi *flexInt) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var intVal int
	if err := unmarshal(&intVal); err == nil {
		*fi = flexInt(intVal)
		return nil
	}
	var strVal string
	if err := unmarshal(&strVal); err != nil {
		return fmt.Errorf("cannot unmarshal port: expected int or string")
	}
	parsed, err := strconv.Atoi(strVal)
	if err != nil {
		return fmt.Errorf("cannot parse port %q as int: %w", strVal, err)
	}
	*fi = flexInt(parsed)
	return nil
}

type clashConfig struct {
	Proxies []yaml.Node `yaml:"proxies"`
}

type clashProxy struct {
	Name              string                 `yaml:"name"`
	Type              string                 `yaml:"type"`
	Server            string                 `yaml:"server"`
	Port              flexInt                `yaml:"port"`
	Ports             string                 `yaml:"ports"`
	UUID              string                 `yaml:"uuid"`
	Password          string                 `yaml:"password"`
	Cipher            string                 `yaml:"cipher"`
	AlterId           int                    `yaml:"alterId"`
	Network           string                 `yaml:"network"`
	TLS               bool                   `yaml:"tls"`
	SkipCertVerify    bool                   `yaml:"skip-cert-verify"`
	ServerName        string                 `yaml:"servername"`
	SNI               string                 `yaml:"sni"`
	Flow              string                 `yaml:"flow"`
	UDP               bool                   `yaml:"udp"`
	WSOpts            *clashWSOptions        `yaml:"ws-opts"`
	GrpcOpts          *clashGrpcOptions      `yaml:"grpc-opts"`
	RealityOpts       *clashRealityOptions   `yaml:"reality-opts"`
	ClientFingerprint string                 `yaml:"client-fingerprint"`
	Obfs              string                 `yaml:"obfs"`
	ObfsPassword      string                 `yaml:"obfs-password"`
	Plugin            string                 `yaml:"plugin"`
	PluginOpts        map[string]interface{} `yaml:"plugin-opts"`
	// TUIC-specific fields
	ALPN                 []string `yaml:"alpn"`
	CongestionController string   `yaml:"congestion-controller"`
	UDPRelayMode         string   `yaml:"udp-relay-mode"`
	// ShadowsocksR-specific fields
	Protocol      string `yaml:"protocol"`
	ProtocolParam string `yaml:"protocol-param"`
	ObfsParam     string `yaml:"obfs-param"`
	// Hysteria v1-specific fields
	AuthStr        string `yaml:"auth-str"`
	Auth           string `yaml:"auth"`
	UpMbps         int    `yaml:"up"`
	DownMbps       int    `yaml:"down"`
	PeerSNI        string `yaml:"peer"`
	RecvWindow     uint64 `yaml:"recv-window"`
	RecvWindowConn uint64 `yaml:"recv-window-conn"`
	DisableMTU     bool   `yaml:"disable_mtu_discovery"`
}

type clashWSOptions struct {
	Path    string            `yaml:"path"`
	Headers map[string]string `yaml:"headers"`
}

type clashGrpcOptions struct {
	GrpcServiceName string `yaml:"grpc-service-name"`
}

type clashRealityOptions struct {
	PublicKey string `yaml:"public-key"`
	ShortID   string `yaml:"short-id"`
}

// parseClashYAML parses Clash YAML format and converts to NodeConfig.
// Per-proxy decoding: a single malformed entry won't fail the whole subscription;
// failed proxies are logged and skipped (fixes #23).
func parseClashYAML(content string) ([]NodeConfig, error) {
	var clash clashConfig
	if err := yaml.Unmarshal([]byte(content), &clash); err != nil {
		return nil, fmt.Errorf("parse clash yaml: %w", err)
	}

	var nodes []NodeConfig
	skipped := 0
	for i, raw := range clash.Proxies {
		var proxy clashProxy
		if err := raw.Decode(&proxy); err != nil {
			skipped++
			log.Printf("[subscription] WARN: skip proxy #%d (decode error): %v", i, err)
			continue
		}
		uri := convertClashProxyToURI(proxy)
		if uri == "" {
			skipped++
			log.Printf("[subscription] WARN: skip proxy %q (unsupported type %q)", proxy.Name, proxy.Type)
			continue
		}
		nodes = append(nodes, NodeConfig{
			Name: proxy.Name,
			URI:  uri,
		})
	}
	if skipped > 0 {
		log.Printf("[subscription] parsed %d nodes, skipped %d malformed/unsupported entries", len(nodes), skipped)
	}

	return nodes, nil
}

// convertClashProxyToURI converts a Clash proxy config to a standard URI
func convertClashProxyToURI(p clashProxy) string {
	switch strings.ToLower(p.Type) {
	case "vmess":
		return buildVMessURI(p)
	case "vless":
		return buildVLESSURI(p)
	case "trojan":
		return buildTrojanURI(p)
	case "anytls":
		return buildAnyTLSURI(p)
	case "ss", "shadowsocks":
		return buildShadowsocksURI(p)
	case "hysteria2", "hy2":
		return buildHysteria2URI(p)
	case "tuic":
		return buildTUICURI(p)
	case "ssr", "shadowsocksr":
		return buildShadowsocksRURI(p)
	case "hysteria":
		return buildHysteriaURI(p)
	default:
		return ""
	}
}

func buildVMessURI(p clashProxy) string {
	params := url.Values{}
	if p.Network != "" && p.Network != "tcp" {
		params.Set("type", p.Network)
	}
	if p.TLS {
		params.Set("security", "tls")
		if p.ServerName != "" {
			params.Set("sni", p.ServerName)
		} else if p.SNI != "" {
			params.Set("sni", p.SNI)
		}
	}
	if p.WSOpts != nil {
		if p.WSOpts.Path != "" {
			params.Set("path", p.WSOpts.Path)
		}
		if host, ok := p.WSOpts.Headers["Host"]; ok {
			params.Set("host", host)
		}
	}
	if p.ClientFingerprint != "" {
		params.Set("fp", p.ClientFingerprint)
	}

	query := ""
	if len(params) > 0 {
		query = "?" + params.Encode()
	}

	return fmt.Sprintf("vmess://%s@%s:%d%s#%s", p.UUID, p.Server, int(p.Port), query, url.QueryEscape(p.Name))
}

func buildVLESSURI(p clashProxy) string {
	params := url.Values{}
	params.Set("encryption", "none")

	if p.Network != "" && p.Network != "tcp" {
		params.Set("type", p.Network)
	}
	if p.Flow != "" {
		params.Set("flow", p.Flow)
	}
	if p.TLS {
		params.Set("security", "tls")
		if p.ServerName != "" {
			params.Set("sni", p.ServerName)
		} else if p.SNI != "" {
			params.Set("sni", p.SNI)
		}
	}
	if p.RealityOpts != nil {
		params.Set("security", "reality")
		if p.RealityOpts.PublicKey != "" {
			params.Set("pbk", p.RealityOpts.PublicKey)
		}
		if p.RealityOpts.ShortID != "" {
			params.Set("sid", p.RealityOpts.ShortID)
		}
		if p.ServerName != "" {
			params.Set("sni", p.ServerName)
		}
	}
	if p.WSOpts != nil {
		if p.WSOpts.Path != "" {
			params.Set("path", p.WSOpts.Path)
		}
		if host, ok := p.WSOpts.Headers["Host"]; ok {
			params.Set("host", host)
		}
	}
	if p.GrpcOpts != nil && p.GrpcOpts.GrpcServiceName != "" {
		params.Set("serviceName", p.GrpcOpts.GrpcServiceName)
	}
	if p.ClientFingerprint != "" {
		params.Set("fp", p.ClientFingerprint)
	}

	return fmt.Sprintf("vless://%s@%s:%d?%s#%s", p.UUID, p.Server, int(p.Port), params.Encode(), url.QueryEscape(p.Name))
}

func buildTrojanURI(p clashProxy) string {
	params := url.Values{}
	if p.ServerName != "" {
		params.Set("sni", p.ServerName)
	} else if p.SNI != "" {
		params.Set("sni", p.SNI)
	}
	if p.SkipCertVerify {
		params.Set("allowInsecure", "1")
	}
	if p.Network != "" && p.Network != "tcp" {
		params.Set("type", p.Network)
	}
	if p.WSOpts != nil {
		if p.WSOpts.Path != "" {
			params.Set("path", p.WSOpts.Path)
		}
		if host, ok := p.WSOpts.Headers["Host"]; ok {
			params.Set("host", host)
		}
	}
	if p.ClientFingerprint != "" {
		params.Set("fp", p.ClientFingerprint)
	}

	query := ""
	if len(params) > 0 {
		query = "?" + params.Encode()
	}

	return fmt.Sprintf("trojan://%s@%s:%d%s#%s", p.Password, p.Server, int(p.Port), query, url.QueryEscape(p.Name))
}

func buildAnyTLSURI(p clashProxy) string {
	params := url.Values{}
	if p.ServerName != "" {
		params.Set("sni", p.ServerName)
	} else if p.SNI != "" {
		params.Set("sni", p.SNI)
	}
	if p.SkipCertVerify {
		params.Set("allowInsecure", "1")
	}
	if p.ClientFingerprint != "" {
		params.Set("fp", p.ClientFingerprint)
	}

	query := ""
	if len(params) > 0 {
		query = "?" + params.Encode()
	}

	return fmt.Sprintf("anytls://%s@%s:%d%s#%s", p.Password, p.Server, int(p.Port), query, url.QueryEscape(p.Name))
}

func buildShadowsocksURI(p clashProxy) string {
	// Encode method:password in base64
	userInfo := base64.StdEncoding.EncodeToString([]byte(p.Cipher + ":" + p.Password))
	return fmt.Sprintf("ss://%s@%s:%d#%s", userInfo, p.Server, int(p.Port), url.QueryEscape(p.Name))
}

func buildHysteria2URI(p clashProxy) string {
	params := url.Values{}
	if p.ServerName != "" {
		params.Set("sni", p.ServerName)
	} else if p.SNI != "" {
		params.Set("sni", p.SNI)
	}
	if p.SkipCertVerify {
		params.Set("insecure", "1")
	}
	if p.Obfs != "" {
		params.Set("obfs", p.Obfs)
		if p.ObfsPassword != "" {
			params.Set("obfs-password", p.ObfsPassword)
		}
	}
	if strings.TrimSpace(p.Ports) != "" {
		params.Set("ports", normalizeHysteria2PortsValue(strings.TrimSpace(p.Ports)))
	}

	query := ""
	if len(params) > 0 {
		query = "?" + params.Encode()
	}

	port := int(p.Port)
	if port <= 0 {
		port = 443
	}

	return fmt.Sprintf("hysteria2://%s@%s:%d%s#%s", p.Password, p.Server, port, query, url.QueryEscape(p.Name))
}

func normalizeHysteria2PortsValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}

	parts := strings.Split(value, ",")
	normalized := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, ":") {
			normalized = append(normalized, part)
			continue
		}
		if strings.Count(part, "-") == 1 {
			normalized = append(normalized, strings.Replace(part, "-", ":", 1))
			continue
		}
		normalized = append(normalized, part)
	}

	return strings.Join(normalized, ",")
}

func buildTUICURI(p clashProxy) string {
	params := url.Values{}
	if p.ServerName != "" {
		params.Set("sni", p.ServerName)
	} else if p.SNI != "" {
		params.Set("sni", p.SNI)
	}
	if p.SkipCertVerify {
		params.Set("allowInsecure", "1")
	}
	if p.CongestionController != "" {
		params.Set("congestion_control", p.CongestionController)
	}
	if p.UDPRelayMode != "" {
		params.Set("udp_relay_mode", p.UDPRelayMode)
	}
	if len(p.ALPN) > 0 {
		params.Set("alpn", strings.Join(p.ALPN, ","))
	}

	query := ""
	if len(params) > 0 {
		query = "?" + params.Encode()
	}

	// TUIC URI format: tuic://uuid:password@server:port?params#name
	return fmt.Sprintf("tuic://%s:%s@%s:%d%s#%s", p.UUID, p.Password, p.Server, int(p.Port), query, url.QueryEscape(p.Name))
}

// buildShadowsocksRURI converts a Clash SSR proxy config to an SSR URI.
// Format: ssr://base64(host:port:protocol:method:obfs:base64(password)/?obfsparam=base64&protoparam=base64&remarks=base64)
func buildShadowsocksRURI(p clashProxy) string {
	passwordB64 := base64.URLEncoding.EncodeToString([]byte(p.Password))

	main := fmt.Sprintf("%s:%d:%s:%s:%s:%s",
		p.Server, int(p.Port),
		defaultStr(p.Protocol, "origin"),
		defaultStr(p.Cipher, "none"),
		defaultStr(p.Obfs, "plain"),
		passwordB64,
	)

	var params []string
	if p.ObfsParam != "" {
		params = append(params, "obfsparam="+base64.URLEncoding.EncodeToString([]byte(p.ObfsParam)))
	}
	if p.ProtocolParam != "" {
		params = append(params, "protoparam="+base64.URLEncoding.EncodeToString([]byte(p.ProtocolParam)))
	}
	if p.Name != "" {
		params = append(params, "remarks="+base64.URLEncoding.EncodeToString([]byte(p.Name)))
	}

	payload := main
	if len(params) > 0 {
		payload += "/?" + strings.Join(params, "&")
	}
	return "ssr://" + base64.URLEncoding.EncodeToString([]byte(payload))
}

// buildHysteriaURI converts a Clash Hysteria v1 proxy config to a hysteria:// URI.
// Format: hysteria://host:port?protocol=udp&auth=xxx&peer=sni&insecure=1&upmbps=N&downmbps=N&alpn=h3&obfs=xplus#name
func buildHysteriaURI(p clashProxy) string {
	params := url.Values{}
	params.Set("protocol", "udp")

	auth := p.AuthStr
	if auth == "" {
		auth = p.Auth
	}
	if auth == "" {
		auth = p.Password
	}
	if auth != "" {
		params.Set("auth", auth)
	}
	if p.ServerName != "" {
		params.Set("peer", p.ServerName)
	} else if p.SNI != "" {
		params.Set("peer", p.SNI)
	} else if p.PeerSNI != "" {
		params.Set("peer", p.PeerSNI)
	}
	if p.SkipCertVerify {
		params.Set("insecure", "1")
	}
	if p.UpMbps > 0 {
		params.Set("upmbps", strconv.Itoa(p.UpMbps))
	}
	if p.DownMbps > 0 {
		params.Set("downmbps", strconv.Itoa(p.DownMbps))
	}
	if len(p.ALPN) > 0 {
		params.Set("alpn", strings.Join(p.ALPN, ","))
	}
	if p.Obfs != "" {
		params.Set("obfs", p.Obfs)
		if p.ObfsPassword != "" {
			params.Set("obfsParam", p.ObfsPassword)
		}
	}

	return fmt.Sprintf("hysteria://%s:%d?%s#%s", p.Server, int(p.Port), params.Encode(), url.QueryEscape(p.Name))
}

func defaultStr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// FilePath returns the config file path.
func (c *Config) FilePath() string {
	if c == nil {
		return ""
	}
	return c.filePath
}

// SetFilePath sets the config file path (used when creating config programmatically).
func (c *Config) SetFilePath(path string) {
	if c != nil {
		c.filePath = path
	}
}

// writeNodesToFile writes nodes to a file (one URI per line) with file locking.
func writeNodesToFile(path string, nodes []NodeConfig) error {
	var lines []string
	for _, node := range nodes {
		lines = append(lines, node.URI)
	}
	content := strings.Join(lines, "\n")
	if len(lines) > 0 {
		content += "\n"
	}
	// Use file locking for safe concurrent writes
	return writeFileWithLock(path, []byte(content), 0o644)
}

// SaveNodes persists nodes to their appropriate locations based on source.
// - subscription/nodes_file nodes → nodes.txt (or configured nodes_file)
// - inline nodes → config.yaml nodes array
// Config.yaml structure (subscriptions, nodes_file) is preserved.
func (c *Config) SaveNodes() error {
	if c == nil {
		return errors.New("config is nil")
	}
	if c.filePath == "" {
		return errors.New("config file path is unknown")
	}

	// Check if config file is writable before attempting save
	if err := checkFileWritable(c.filePath); err != nil {
		return fmt.Errorf("config file not writable: %w (check file permissions and Docker volume mounts)", err)
	}

	// Separate nodes by source
	var inlineNodes []NodeConfig
	var fileNodes []NodeConfig

	for _, node := range c.Nodes {
		// Create a clean copy without runtime fields for saving
		cleanNode := NodeConfig{
			Name:        node.Name,
			URI:         node.URI,
			Port:        node.Port,
			Username:    node.Username,
			Password:    node.Password,
			PoolEnabled: node.PoolEnabled,
		}
		switch node.Source {
		case NodeSourceInline:
			inlineNodes = append(inlineNodes, cleanNode)
		case NodeSourceFile, NodeSourceSubscription:
			fileNodes = append(fileNodes, cleanNode)
		default:
			// Default to file nodes for unknown source
			fileNodes = append(fileNodes, cleanNode)
		}
	}

	if err := c.SaveNodePrefs(); err != nil {
		return fmt.Errorf("save node prefs: %w", err)
	}

	// Write file-based nodes to nodes.txt
	if len(fileNodes) > 0 || c.NodesFile != "" {
		nodesFilePath := c.NodesFile
		if nodesFilePath == "" {
			nodesFilePath = filepath.Join(filepath.Dir(c.filePath), "nodes.txt")
		}
		// Check writability before writing
		if err := checkFileWritable(nodesFilePath); err != nil {
			return fmt.Errorf("nodes file not writable: %w (check file permissions and Docker volume mounts)", err)
		}
		if err := writeNodesToFile(nodesFilePath, fileNodes); err != nil {
			return fmt.Errorf("write nodes file %q: %w", nodesFilePath, err)
		}
		log.Printf("✅ Saved %d nodes to %s", len(fileNodes), nodesFilePath)
	}

	// Update config.yaml nodes array (including clearing it when all inline nodes are deleted)
	{
		// Read original config to preserve structure
		data, err := os.ReadFile(c.filePath)
		if err != nil {
			return fmt.Errorf("read config: %w", err)
		}
		var saveCfg Config
		if err := yaml.Unmarshal(data, &saveCfg); err != nil {
			return fmt.Errorf("decode config: %w", err)
		}
		// Update only the inline nodes
		saveCfg.Nodes = inlineNodes

		newData, err := yaml.Marshal(&saveCfg)
		if err != nil {
			return fmt.Errorf("encode config: %w", err)
		}
		// Use file locking for safe concurrent writes
		if err := writeFileWithLock(c.filePath, newData, 0o644); err != nil {
			return fmt.Errorf("write config: %w", err)
		}
		log.Printf("✅ Saved %d inline nodes to %s", len(inlineNodes), c.filePath)
	}

	return nil
}

// Save is deprecated, use SaveNodes instead.
// This method is kept for backward compatibility but now delegates to SaveNodes.
func (c *Config) Save() error {
	return c.SaveNodes()
}

// SaveSettings persists only config settings (external_ip, probe_target, skip_cert_verify)
// without touching nodes.txt. Use this for settings API updates.
func (c *Config) SaveSettings() error {
	if c == nil {
		return errors.New("config is nil")
	}
	if c.filePath == "" {
		return errors.New("config file path is unknown")
	}

	data, err := os.ReadFile(c.filePath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var saveCfg Config
	if err := yaml.Unmarshal(data, &saveCfg); err != nil {
		return fmt.Errorf("decode config: %w", err)
	}

	saveCfg.ExternalIP = c.ExternalIP
	saveCfg.Management.ProbeTarget = c.Management.ProbeTarget
	saveCfg.SkipCertVerify = c.SkipCertVerify
	saveCfg.Log = c.Log
	saveCfg.Subscriptions = c.Subscriptions
	saveCfg.SubscriptionRefresh = c.SubscriptionRefresh
	saveCfg.GeoIP = c.GeoIP
	saveCfg.Mode = c.Mode
	saveCfg.Listener = c.Listener
	saveCfg.MultiPort = c.MultiPort
	saveCfg.Pool = c.Pool
	saveCfg.Management = c.Management

	newData, err := yaml.Marshal(&saveCfg)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}

	// Use file locking for safe concurrent writes
	if err := writeFileWithLock(c.filePath, newData, 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// IsPortAvailable checks if a port is available for binding.
func IsPortAvailable(address string, port uint16) bool {
	addr := fmt.Sprintf("%s:%d", address, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// checkFileWritable checks if a file is writable. Creates parent directories if needed.
func checkFileWritable(path string) error {
	// Ensure parent directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create directory %q: %w", dir, err)
	}

	// Check if file exists
	info, err := os.Stat(path)
	if err == nil {
		// File exists, check if writable
		if info.Mode().Perm()&0o200 == 0 {
			return fmt.Errorf("file %q is read-only (mode: %s)", path, info.Mode())
		}
		// Try to open for writing
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return fmt.Errorf("cannot open for writing: %w", err)
		}
		f.Close()
		return nil
	}

	if !os.IsNotExist(err) {
		return fmt.Errorf("stat file: %w", err)
	}

	// File doesn't exist, check if directory is writable
	testFile := filepath.Join(dir, ".write_test")
	f, err := os.Create(testFile)
	if err != nil {
		return fmt.Errorf("directory %q is not writable: %w", dir, err)
	}
	f.Close()
	os.Remove(testFile)
	return nil
}

// writeFileWithLock writes data to a file with exclusive locking.
func writeFileWithLock(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}
	defer f.Close()

	// Acquire exclusive lock
	if err := lockFile(f); err != nil {
		return fmt.Errorf("lock file: %w", err)
	}
	defer unlockFile(f)

	// Write data
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	// Ensure data is written to disk
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync file: %w", err)
	}

	return nil
}
