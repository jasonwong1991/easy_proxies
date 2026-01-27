package subscription

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"easy_proxies/internal/boxmgr"
	"easy_proxies/internal/config"
	"easy_proxies/internal/monitor"
)

// Logger defines logging interface.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

// Option configures the Manager.
type Option func(*Manager)

// WithLogger sets a custom logger.
func WithLogger(l Logger) Option {
	return func(m *Manager) { m.logger = l }
}

// Manager handles periodic subscription refresh.
type Manager struct {
	mu sync.RWMutex

	baseCfg *config.Config
	boxMgr  *boxmgr.Manager
	logger  Logger

	status        monitor.SubscriptionStatus
	ctx           context.Context
	cancel        context.CancelFunc
	refreshMu     sync.Mutex // prevents concurrent refreshes
	manualRefresh chan struct{}

	// Track subscription cache file content hash to detect modifications
	lastSubHash      string
	lastNodesModTime time.Time
}

// New creates a SubscriptionManager.
func New(cfg *config.Config, boxMgr *boxmgr.Manager, opts ...Option) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		baseCfg:       cfg,
		boxMgr:        boxMgr,
		ctx:           ctx,
		cancel:        cancel,
		manualRefresh: make(chan struct{}, 1),
	}
	for _, opt := range opts {
		opt(m)
	}
	if m.logger == nil {
		m.logger = defaultLogger{}
	}
	return m
}

// Start begins the periodic refresh loop.
func (m *Manager) Start() {
	cfg := m.cfgSnapshot()
	if cfg == nil {
		m.logger.Warnf("subscription refresh disabled: config is nil")
		return
	}
	if !cfg.SubscriptionRefresh.Enabled {
		m.logger.Infof("subscription refresh disabled")
		return
	}
	if len(cfg.Subscriptions) == 0 {
		m.logger.Infof("no subscriptions configured, refresh disabled")
		return
	}

	interval := cfg.SubscriptionRefresh.Interval
	m.logger.Infof("starting subscription refresh, interval: %s", interval)

	go m.refreshLoop(interval)
}

// Stop stops the periodic refresh.
func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
}

// RefreshNow triggers an immediate refresh.
func (m *Manager) RefreshNow() error {
	select {
	case m.manualRefresh <- struct{}{}:
	default:
	}

	cfg := m.cfgSnapshot()
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}

	timeout := cfg.SubscriptionRefresh.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	hcTimeout := cfg.SubscriptionRefresh.HealthCheckTimeout
	if hcTimeout <= 0 {
		hcTimeout = 60 * time.Second
	}

	ctx, cancel := context.WithTimeout(m.ctx, timeout+hcTimeout)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	startCount := m.Status().RefreshCount
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("refresh timeout")
		case <-ticker.C:
			status := m.Status()
			if status.RefreshCount > startCount {
				if status.LastError != "" {
					return fmt.Errorf("refresh failed: %s", status.LastError)
				}
				return nil
			}
		}
	}
}

// Status returns the current refresh status.
func (m *Manager) Status() monitor.SubscriptionStatus {
	m.mu.RLock()
	status := m.status
	m.mu.RUnlock()

	status.NodesModified = m.CheckNodesModified()
	return status
}

func (m *Manager) cfgSnapshot() *config.Config {
	if m.boxMgr != nil {
		if snap := m.boxMgr.ConfigSnapshot(); snap != nil {
			return snap
		}
	}
	return m.baseCfg
}

func (m *Manager) refreshLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	m.mu.Lock()
	m.status.NextRefresh = time.Now().Add(interval)
	m.mu.Unlock()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.doRefresh()
			m.mu.Lock()
			m.status.NextRefresh = time.Now().Add(interval)
			m.mu.Unlock()
		case <-m.manualRefresh:
			m.doRefresh()
			ticker.Reset(interval)
			m.mu.Lock()
			m.status.NextRefresh = time.Now().Add(interval)
			m.mu.Unlock()
		}
	}
}

// doRefresh performs a single refresh operation.
func (m *Manager) doRefresh() {
	if !m.refreshMu.TryLock() {
		m.logger.Warnf("refresh already in progress, skipping")
		return
	}
	defer m.refreshMu.Unlock()

	m.mu.Lock()
	m.status.IsRefreshing = true
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		m.status.IsRefreshing = false
		m.status.RefreshCount++
		m.mu.Unlock()
	}()

	m.logger.Infof("starting subscription refresh")

	subNodes, err := m.fetchAllSubscriptions()
	if err != nil {
		m.logger.Errorf("fetch subscriptions failed: %v", err)
		m.mu.Lock()
		m.status.LastError = err.Error()
		m.status.LastRefresh = time.Now()
		m.mu.Unlock()
		return
	}
	if len(subNodes) == 0 {
		m.logger.Warnf("no nodes fetched from subscriptions")
		m.mu.Lock()
		m.status.LastError = "no nodes fetched"
		m.status.LastRefresh = time.Now()
		m.mu.Unlock()
		return
	}

	for i := range subNodes {
		subNodes[i].Source = config.NodeSourceSubscription
	}

	cachePath := m.getSubscriptionCacheFilePath()
	if cachePath != "" {
		if err := m.writeNodesToFile(cachePath, subNodes); err != nil {
			m.logger.Errorf("failed to write subscription cache: %v", err)
			m.mu.Lock()
			m.status.LastError = fmt.Sprintf("write subscription cache: %v", err)
			m.status.LastRefresh = time.Now()
			m.mu.Unlock()
			return
		}
	}

	newHash := m.computeNodesHash(subNodes)
	m.mu.Lock()
	m.lastSubHash = newHash
	if cachePath != "" {
		if info, err := os.Stat(cachePath); err == nil {
			m.lastNodesModTime = info.ModTime()
		} else {
			m.lastNodesModTime = time.Now()
		}
	}
	m.status.NodesModified = false
	m.mu.Unlock()

	portMap := m.boxMgr.CurrentPortMap()
	newCfg := m.createNewConfig(subNodes)

	if err := m.boxMgr.ReloadWithPortMap(newCfg, portMap); err != nil {
		m.logger.Errorf("reload failed: %v", err)
		m.mu.Lock()
		m.status.LastError = err.Error()
		m.status.LastRefresh = time.Now()
		m.mu.Unlock()
		return
	}

	m.mu.Lock()
	m.status.LastRefresh = time.Now()
	m.status.NodeCount = len(subNodes)
	m.status.LastError = ""
	m.mu.Unlock()

	m.logger.Infof("subscription refresh completed, %d subscription nodes active", len(subNodes))
}

func (m *Manager) getManualNodesFilePath() string {
	cfg := m.cfgSnapshot()
	if cfg == nil {
		return ""
	}
	p := strings.TrimSpace(cfg.NodesFile)
	if p == "" {
		return ""
	}
	return p
}

func (m *Manager) getSubscriptionCacheFilePath() string {
	cfg := m.cfgSnapshot()
	if cfg == nil {
		return ""
	}

	nodesFile := strings.TrimSpace(cfg.NodesFile)
	if nodesFile != "" {
		dir := filepath.Dir(nodesFile)
		base := filepath.Base(nodesFile)
		if strings.HasSuffix(base, ".txt") {
			base = strings.TrimSuffix(base, ".txt") + ".subscription.txt"
		} else {
			base = base + ".subscription"
		}
		return filepath.Join(dir, base)
	}

	cfgPath := strings.TrimSpace(cfg.FilePath())
	if cfgPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(cfgPath), "nodes.subscription.txt")
}

func (m *Manager) writeNodesToFile(path string, nodes []config.NodeConfig) error {
	var lines []string
	for _, node := range nodes {
		uri := strings.TrimSpace(node.URI)
		if uri == "" {
			continue
		}
		lines = append(lines, uri)
	}
	content := strings.Join(lines, "\n")
	if len(lines) > 0 {
		content += "\n"
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func (m *Manager) computeNodesHash(nodes []config.NodeConfig) string {
	var uris []string
	for _, node := range nodes {
		uris = append(uris, strings.TrimSpace(node.URI))
	}
	content := strings.Join(uris, "\n")
	hash := sha256.Sum256([]byte(content))
	return hex.EncodeToString(hash[:])
}

// CheckNodesModified checks whether the subscription cache file was modified since last refresh.
func (m *Manager) CheckNodesModified() bool {
	m.mu.RLock()
	lastHash := m.lastSubHash
	lastMod := m.lastNodesModTime
	m.mu.RUnlock()

	if lastHash == "" {
		return false
	}

	cachePath := m.getSubscriptionCacheFilePath()
	if cachePath == "" {
		return false
	}

	info, err := os.Stat(cachePath)
	if err != nil {
		return false
	}
	modTime := info.ModTime()
	if !modTime.After(lastMod) {
		return false
	}

	data, err := os.ReadFile(cachePath)
	if err != nil {
		return false
	}

	nodes, err := config.ParseSubscriptionContent(string(data))
	if err != nil {
		return false
	}

	currentHash := m.computeNodesHash(nodes)
	changed := currentHash != lastHash

	m.mu.Lock()
	m.lastNodesModTime = modTime
	m.mu.Unlock()

	return changed
}

func (m *Manager) MarkNodesModified() {
	m.mu.Lock()
	m.status.NodesModified = true
	m.mu.Unlock()
}

func (m *Manager) fetchAllSubscriptions() ([]config.NodeConfig, error) {
	cfg := m.cfgSnapshot()
	if cfg == nil {
		return nil, fmt.Errorf("config is nil")
	}

	var allNodes []config.NodeConfig
	var lastErr error

	timeout := cfg.SubscriptionRefresh.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	subs := append([]string(nil), cfg.Subscriptions...)
	for _, subURL := range subs {
		nodes, err := m.fetchSubscription(subURL, timeout)
		if err != nil {
			m.logger.Warnf("failed to fetch %s: %v", subURL, err)
			lastErr = err
			continue
		}
		m.logger.Infof("fetched %d nodes from subscription", len(nodes))
		allNodes = append(allNodes, nodes...)
	}

	if len(allNodes) == 0 && lastErr != nil {
		return nil, lastErr
	}

	return allNodes, nil
}

func splitSubscriptionURL(raw string) (requestURL string, defaultScheme string) {
	defaultScheme = "http"
	requestURL = raw

	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return requestURL, defaultScheme
	}

	frag := strings.ToLower(strings.TrimSpace(u.Fragment))
	switch frag {
	case "http", "https", "socks5", "socks5h":
		defaultScheme = frag
	case "socks":
		defaultScheme = "socks5"
	default:
		pathLower := strings.ToLower(u.Path)
		if strings.Contains(pathLower, "socks5") || strings.Contains(pathLower, "socks") {
			defaultScheme = "socks5"
		} else if strings.Contains(pathLower, "https") {
			defaultScheme = "https"
		} else if strings.Contains(pathLower, "http") {
			defaultScheme = "http"
		}
	}

	u.Fragment = ""
	requestURL = u.String()
	return requestURL, defaultScheme
}

func (m *Manager) fetchSubscription(subURL string, timeout time.Duration) ([]config.NodeConfig, error) {
	ctx, cancel := context.WithTimeout(m.ctx, timeout)
	defer cancel()

	requestURL, defaultScheme := splitSubscriptionURL(subURL)

	req, err := http.NewRequestWithContext(ctx, "GET", requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept", "*/*")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	return config.ParseSubscriptionContentWithHint(string(body), defaultScheme)
}

func (m *Manager) loadManualFileNodes() []config.NodeConfig {
	path := m.getManualNodesFilePath()
	if path == "" {
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		m.logger.Warnf("failed to read nodes_file %q: %v", path, err)
		return nil
	}

	defaultScheme := "http"
	baseLower := strings.ToLower(filepath.Base(path))
	if strings.Contains(baseLower, "socks5") || strings.Contains(baseLower, "socks") {
		defaultScheme = "socks5"
	} else if strings.Contains(baseLower, "https") {
		defaultScheme = "https"
	} else if strings.Contains(baseLower, "http") {
		defaultScheme = "http"
	}

	nodes, err := config.ParseSubscriptionContentWithHint(string(data), defaultScheme)
	if err != nil {
		m.logger.Warnf("failed to parse nodes_file %q: %v", path, err)
		return nil
	}
	for i := range nodes {
		nodes[i].Source = config.NodeSourceFile
	}
	return nodes
}

func (m *Manager) inlineNodes() []config.NodeConfig {
	cfg := m.cfgSnapshot()
	if cfg == nil {
		return nil
	}

	var nodes []config.NodeConfig
	for _, n := range cfg.Nodes {
		if n.Source == config.NodeSourceInline {
			nodes = append(nodes, n)
		}
	}
	return nodes
}

// createNewConfig merges inline + subscription + file nodes.
func (m *Manager) createNewConfig(subNodes []config.NodeConfig) *config.Config {
	base := m.cfgSnapshot()
	if base == nil {
		return nil
	}

	newCfg := *base

	// Inline nodes from the same snapshot (avoid reading shared cfg directly).
	inline := make([]config.NodeConfig, 0)
	for _, n := range base.Nodes {
		if n.Source == config.NodeSourceInline {
			inline = append(inline, n)
		}
	}

	fileNodes := m.loadManualFileNodes()
	merged := mergeNodesKeepFirst(append(append([]config.NodeConfig{}, inline...), subNodes...), fileNodes)

	// Ensure names (extract from fragment if needed)
	for i := range merged {
		merged[i].Name = strings.TrimSpace(merged[i].Name)
		merged[i].URI = strings.TrimSpace(merged[i].URI)

		if merged[i].Name == "" {
			if parsed, err := url.Parse(merged[i].URI); err == nil && parsed.Fragment != "" {
				if decoded, err := url.QueryUnescape(parsed.Fragment); err == nil {
					merged[i].Name = decoded
				} else {
					merged[i].Name = parsed.Fragment
				}
			}
		}
		if merged[i].Name == "" {
			merged[i].Name = fmt.Sprintf("node-%d", i)
		}
	}

	newCfg.Nodes = merged
	return &newCfg
}

func mergeNodesKeepFirst(nodes []config.NodeConfig, extra ...[]config.NodeConfig) []config.NodeConfig {
	seen := make(map[string]struct{}, len(nodes))
	out := make([]config.NodeConfig, 0, len(nodes))

	for _, n := range nodes {
		key := strings.TrimSpace(n.URI)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, n)
	}

	for _, list := range extra {
		for _, n := range list {
			key := strings.TrimSpace(n.URI)
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, n)
		}
	}

	return out
}

type defaultLogger struct{}

func (defaultLogger) Infof(format string, args ...any) {
	log.Printf("[subscription] "+format, args...)
}

func (defaultLogger) Warnf(format string, args ...any) {
	log.Printf("[subscription] WARN: "+format, args...)
}

func (defaultLogger) Errorf(format string, args ...any) {
	log.Printf("[subscription] ERROR: "+format, args...)
}