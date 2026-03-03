package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// Config describes the high level settings for the proxy pool server.
type Config struct {
	Mode                string                    `yaml:"mode"`
	Listener            ListenerConfig            `yaml:"listener"`
	MultiPort           MultiPortConfig           `yaml:"multi_port"`
	Pool                PoolConfig                `yaml:"pool"`
	Management          ManagementConfig          `yaml:"management"`
	SubscriptionRefresh SubscriptionRefreshConfig `yaml:"subscription_refresh"`
	GeoIP               GeoIPConfig               `yaml:"geoip"`
	Nodes               []NodeConfig              `yaml:"nodes"`
	NodesFile           string                    `yaml:"nodes_file"`    // 节点文件路径，每行一个 URI
	Subscriptions       []string                  `yaml:"subscriptions"` // 订阅链接列表
	ExternalIP          string                    `yaml:"external_ip"`      // 外部 IP 地址，用于导出时替换 0.0.0.0
	LogLevel            string                    `yaml:"log_level"`
	SkipCertVerify      bool                      `yaml:"skip_cert_verify"` // 全局跳过 SSL 证书验证

	filePath string `yaml:"-"` // 配置文件路径，用于保存
}

// GeoIPConfig controls GeoIP-based region routing.
type GeoIPConfig struct {
	Enabled            bool          `yaml:"enabled"`              // 是否启用 GeoIP 地域分区
	DatabasePath       string        `yaml:"database_path"`        // GeoLite2-Country.mmdb 文件路径
	Listen             string        `yaml:"listen"`               // GeoIP 路由监听地址，默认使用 listener 配置
	Port               uint16        `yaml:"port"`                 // GeoIP 路由监听端口，默认 2323
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

// PoolConfig configures scheduling + failure handling.
type PoolConfig struct {
	Mode              string        `yaml:"mode"`
	FailureThreshold  int           `yaml:"failure_threshold"`
	BlacklistDuration time.Duration `yaml:"blacklist_duration"`
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
	Enabled     *bool  `yaml:"enabled"`
	Listen      string `yaml:"listen"`
	ProbeTarget string `yaml:"probe_target"`
	Password    string `yaml:"password"` // WebUI 访问密码，为空则不需要密码
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
	Name     string     `yaml:"name" json:"name"`
	URI      string     `yaml:"uri" json:"uri"`
	Port     uint16     `yaml:"port,omitempty" json:"port,omitempty"`
	Username string     `yaml:"username,omitempty" json:"username,omitempty"`
	Password string     `yaml:"password,omitempty" json:"password,omitempty"`
	Source   NodeSource `yaml:"-" json:"source,omitempty"` // Runtime only, not persisted
}

// NodeKey returns a unique identifier for the node based on its URI.
// This is used to preserve port assignments across reloads.
func (n *NodeConfig) NodeKey() string {
	return n.URI
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
	return &cfg, nil
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
	}

	if len(c.Nodes) == 0 {
		return errors.New("config.nodes cannot be empty (configure nodes in config or use nodes_file)")
	}
	portCursor := c.MultiPort.BasePort
	for idx := range c.Nodes {
		c.Nodes[idx].Name = strings.TrimSpace(c.Nodes[idx].Name)
		c.Nodes[idx].URI = strings.TrimSpace(c.Nodes[idx].URI)

		if c.Nodes[idx].URI == "" {
			return fmt.Errorf("node %d is missing uri", idx)
		}

		// Auto-extract name from URI fragment (#name) if not provided
		if c.Nodes[idx].Name == "" {
			if parsed, err := url.Parse(c.Nodes[idx].URI); err == nil && parsed.Fragment != "" {
				// URL decode the fragment to handle encoded characters
				if decoded, err := url.QueryUnescape(parsed.Fragment); err == nil {
					c.Nodes[idx].Name = decoded
				} else {
					c.Nodes[idx].Name = parsed.Fragment
				}
			}
		}

		// Fallback to default name if still empty
		if c.Nodes[idx].Name == "" {
			c.Nodes[idx].Name = fmt.Sprintf("node-%d", idx)
		}

		// Auto-assign port in multi-port/hybrid mode, skip occupied ports
		if c.Nodes[idx].Port == 0 && (c.Mode == "multi-port" || c.Mode == "hybrid") {
			for !IsPortAvailable(c.MultiPort.Address, portCursor) {
				log.Printf("⚠️  Port %d is in use, trying next port", portCursor)
				portCursor++
				if portCursor > 65535 {
					return fmt.Errorf("no available ports found starting from %d", c.MultiPort.BasePort)
				}
			}
			c.Nodes[idx].Port = portCursor
			portCursor++
		} else if c.Nodes[idx].Port == 0 {
			c.Nodes[idx].Port = portCursor
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

		// Extract name from URI fragment if not provided
		if c.Nodes[idx].Name == "" {
			if parsed, err := url.Parse(c.Nodes[idx].URI); err == nil && parsed.Fragment != "" {
				if decoded, err := url.QueryUnescape(parsed.Fragment); err == nil {
					c.Nodes[idx].Name = decoded
				} else {
					c.Nodes[idx].Name = parsed.Fragment
				}
			}
		}
		if c.Nodes[idx].Name == "" {
			c.Nodes[idx].Name = fmt.Sprintf("node-%d", idx)
		}

		// Check if this node has a preserved port from portMap
		if c.Mode == "multi-port" || c.Mode == "hybrid" {
			nodeKey := c.Nodes[idx].NodeKey()
			if existingPort, ok := portMap[nodeKey]; ok && existingPort > 0 {
				c.Nodes[idx].Port = existingPort
				usedPorts[existingPort] = true
				log.Printf("✅ Preserved port %d for node %q", existingPort, c.Nodes[idx].Name)
			}
		}
	}

	// Second pass: assign new ports for nodes without preserved ports
	portCursor := c.MultiPort.BasePort
	for idx := range c.Nodes {
		if c.Nodes[idx].Port == 0 && (c.Mode == "multi-port" || c.Mode == "hybrid") {
			// Find next available port that's not used
			for usedPorts[portCursor] || !IsPortAvailable(c.MultiPort.Address, portCursor) {
				portCursor++
				if portCursor > 65535 {
					return fmt.Errorf("no available ports found starting from %d", c.MultiPort.BasePort)
				}
			}
			c.Nodes[idx].Port = portCursor
			usedPorts[portCursor] = true
			log.Printf("📌 Assigned new port %d for node %q", portCursor, c.Nodes[idx].Name)
			portCursor++
		} else if c.Nodes[idx].Port == 0 {
			c.Nodes[idx].Port = portCursor
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

	return nil
}

// ManagementEnabled reports whether the monitoring endpoint should run.
func (c *Config) ManagementEnabled() bool {
	if c.Management.Enabled == nil {
		return true
	}
	return *c.Management.Enabled
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

	// Set common headers to avoid being blocked
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
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

	// Quick check for YAML format (check first 200 chars for "proxies:")
	sampleSize := 200
	if len(content) < sampleSize {
		sampleSize = len(content)
	}
	if strings.Contains(content[:sampleSize], "proxies:") {
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
	schemes := []string{"vmess://", "vless://", "trojan://", "ss://", "ssr://", "hysteria://", "hysteria2://", "hy2://", "socks5://", "socks://", "http://", "https://"}
	lower := strings.ToLower(s)
	for _, scheme := range schemes {
		if strings.HasPrefix(lower, scheme) {
			return true
		}
	}
	return false
}

// clashConfig represents a minimal Clash configuration for parsing proxies
type clashConfig struct {
	Proxies []clashProxy `yaml:"proxies"`
}

type clashProxy struct {
	Name           string                 `yaml:"name"`
	Type           string                 `yaml:"type"`
	Server         string                 `yaml:"server"`
	Port           int                    `yaml:"port"`
	UUID           string                 `yaml:"uuid"`
	Password       string                 `yaml:"password"`
	Cipher         string                 `yaml:"cipher"`
	AlterId        int                    `yaml:"alterId"`
	Network        string                 `yaml:"network"`
	TLS            bool                   `yaml:"tls"`
	SkipCertVerify bool                   `yaml:"skip-cert-verify"`
	ServerName     string                 `yaml:"servername"`
	SNI            string                 `yaml:"sni"`
	Flow           string                 `yaml:"flow"`
	UDP            bool                   `yaml:"udp"`
	WSOpts         *clashWSOptions        `yaml:"ws-opts"`
	GrpcOpts       *clashGrpcOptions      `yaml:"grpc-opts"`
	RealityOpts    *clashRealityOptions   `yaml:"reality-opts"`
	ClientFingerprint string              `yaml:"client-fingerprint"`
	Plugin         string                 `yaml:"plugin"`
	PluginOpts     map[string]interface{} `yaml:"plugin-opts"`
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

// parseClashYAML parses Clash YAML format and converts to NodeConfig
func parseClashYAML(content string) ([]NodeConfig, error) {
	var clash clashConfig
	if err := yaml.Unmarshal([]byte(content), &clash); err != nil {
		return nil, fmt.Errorf("parse clash yaml: %w", err)
	}

	var nodes []NodeConfig
	for _, proxy := range clash.Proxies {
		uri := convertClashProxyToURI(proxy)
		if uri != "" {
			nodes = append(nodes, NodeConfig{
				Name: proxy.Name,
				URI:  uri,
			})
		}
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
	case "ss", "shadowsocks":
		return buildShadowsocksURI(p)
	case "hysteria2", "hy2":
		return buildHysteria2URI(p)
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

	return fmt.Sprintf("vmess://%s@%s:%d%s#%s", p.UUID, p.Server, p.Port, query, url.QueryEscape(p.Name))
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

	return fmt.Sprintf("vless://%s@%s:%d?%s#%s", p.UUID, p.Server, p.Port, params.Encode(), url.QueryEscape(p.Name))
}

func buildTrojanURI(p clashProxy) string {
	params := url.Values{}
	if p.Network != "" && p.Network != "tcp" {
		params.Set("type", p.Network)
	}
	if p.ServerName != "" {
		params.Set("sni", p.ServerName)
	} else if p.SNI != "" {
		params.Set("sni", p.SNI)
	}
	if p.SkipCertVerify {
		params.Set("allowInsecure", "1")
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

	return fmt.Sprintf("trojan://%s@%s:%d%s#%s", p.Password, p.Server, p.Port, query, url.QueryEscape(p.Name))
}

func buildShadowsocksURI(p clashProxy) string {
	// Encode method:password in base64
	userInfo := base64.StdEncoding.EncodeToString([]byte(p.Cipher + ":" + p.Password))
	return fmt.Sprintf("ss://%s@%s:%d#%s", userInfo, p.Server, p.Port, url.QueryEscape(p.Name))
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

	query := ""
	if len(params) > 0 {
		query = "?" + params.Encode()
	}

	return fmt.Sprintf("hysteria2://%s@%s:%d%s#%s", p.Password, p.Server, p.Port, query, url.QueryEscape(p.Name))
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

	// Separate nodes by source
	var inlineNodes []NodeConfig
	var fileNodes []NodeConfig

	for _, node := range c.Nodes {
		// Create a clean copy without runtime fields for saving
		cleanNode := NodeConfig{
			Name:     node.Name,
			URI:      node.URI,
			Port:     node.Port,
			Username: node.Username,
			Password: node.Password,
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

	// Write file-based nodes to nodes.txt
	if len(fileNodes) > 0 || c.NodesFile != "" {
		nodesFilePath := c.NodesFile
		if nodesFilePath == "" {
			nodesFilePath = filepath.Join(filepath.Dir(c.filePath), "nodes.txt")
		}
		if err := writeNodesToFile(nodesFilePath, fileNodes); err != nil {
			return fmt.Errorf("write nodes file %q: %w", nodesFilePath, err)
		}
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

// File locking helpers

// lockFile acquires an exclusive lock on the file.
func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// unlockFile releases the lock on the file.
func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
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
