package boxmgr

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"easy_proxies/internal/builder"
	"easy_proxies/internal/config"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/outbound/pool"

	"github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/include"
)

// Ensure Manager implements monitor.NodeManager.
var _ monitor.NodeManager = (*Manager)(nil)

const (
	defaultDrainTimeout       = 10 * time.Second
	defaultHealthCheckTimeout = 30 * time.Second
	healthCheckPollInterval   = 500 * time.Millisecond
	periodicHealthInterval    = 5 * time.Minute
	periodicHealthTimeout     = 10 * time.Second
)

// Logger defines logging interface for the manager.
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

// Manager owns the lifecycle of the active sing-box instance.
type Manager struct {
	mu sync.RWMutex

	currentBox    *box.Box
	monitorMgr    *monitor.Manager
	monitorServer *monitor.Server
	cfg           *config.Config
	monitorCfg    monitor.Config

	drainTimeout      time.Duration
	minAvailableNodes int
	logger            Logger

	baseCtx            context.Context
	healthCheckStarted bool
}

// New creates a BoxManager with the given config.
func New(cfg *config.Config, monitorCfg monitor.Config, opts ...Option) *Manager {
	m := &Manager{
		cfg:        cfg,
		monitorCfg: monitorCfg,
	}
	m.applyConfigSettings(cfg)
	for _, opt := range opts {
		opt(m)
	}
	if m.logger == nil {
		m.logger = defaultLogger{}
	}
	if m.drainTimeout <= 0 {
		m.drainTimeout = defaultDrainTimeout
	}
	return m
}

// Start creates and starts the initial sing-box instance.
func (m *Manager) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := m.ensureMonitor(ctx); err != nil {
		return err
	}

	m.mu.Lock()
	if m.cfg == nil {
		m.mu.Unlock()
		return errors.New("box manager requires config")
	}
	if m.currentBox != nil {
		m.mu.Unlock()
		return errors.New("sing-box already running")
	}
	m.applyConfigSettings(m.cfg)
	m.baseCtx = ctx
	cfg := m.cfg
	m.mu.Unlock()

	var (
		instance  *box.Box
		lastErr   error
		started   bool
		maxRetries = 10
	)

	for retry := 0; retry < maxRetries; retry++ {
		var err error
		instance, err = m.createBox(ctx, cfg)
		if err != nil {
			return err
		}

		if err = instance.Start(); err != nil {
			lastErr = err
			_ = instance.Close()
			instance = nil

			if conflictPort := extractPortFromBindError(err); conflictPort > 0 {
				m.logger.Warnf("port %d is in use, reassigning and retrying...", conflictPort)
				if reassigned := reassignConflictingPort(cfg, conflictPort); reassigned {
					pool.ResetSharedStateStore()
					continue
				}
			}

			return fmt.Errorf("start sing-box: %w", err)
		}

		started = true
		break
	}

	if !started {
		if lastErr == nil {
			lastErr = errors.New("max retries exceeded")
		}
		return fmt.Errorf("start sing-box: exceeded max retries (%d): %w", maxRetries, lastErr)
	}

	m.mu.Lock()
	m.currentBox = instance
	m.mu.Unlock()

	m.mu.Lock()
	if m.monitorMgr != nil && !m.healthCheckStarted {
		m.monitorMgr.StartPeriodicHealthCheck(periodicHealthInterval, periodicHealthTimeout)
		m.healthCheckStarted = true
	}
	m.mu.Unlock()

	if cfg.SubscriptionRefresh.MinAvailableNodes > 0 {
		timeout := cfg.SubscriptionRefresh.HealthCheckTimeout
		if timeout <= 0 {
			timeout = defaultHealthCheckTimeout
		}
		if err := m.waitForHealthCheck(timeout); err != nil {
			m.logger.Warnf("initial health check warning: %v", err)
		}
	}

	m.logger.Infof("sing-box instance started with %d nodes", len(cfg.Nodes))
	return nil
}

// Reload gracefully switches to a new configuration.
// For multi-port mode, we must stop the old instance first to release ports.
func (m *Manager) Reload(newCfg *config.Config) error {
	if newCfg == nil {
		return errors.New("new config is nil")
	}

	m.mu.Lock()
	if m.currentBox == nil {
		m.mu.Unlock()
		return errors.New("manager not started")
	}
	ctx := m.baseCtx
	oldBox := m.currentBox
	oldCfg := m.cfg
	m.currentBox = nil
	m.mu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}

	// Start a new monitor generation so we can prune stale nodes after reload succeeds.
	var gen uint64
	if mm := m.MonitorManager(); mm != nil {
		gen = mm.BeginNodeSync()
	}

	m.logger.Infof("reloading with %d nodes", len(newCfg.Nodes))

	if oldBox != nil {
		m.logger.Infof("stopping old instance to release ports...")
		if err := oldBox.Close(); err != nil {
			m.logger.Warnf("error closing old instance: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}

	pool.ResetSharedStateStore()

	var (
		instance   *box.Box
		lastErr    error
		started    bool
		maxRetries = 10
	)

	for retry := 0; retry < maxRetries; retry++ {
		var err error
		instance, err = m.createBox(ctx, newCfg)
		if err != nil {
			m.rollbackToOldConfig(ctx, oldCfg)
			return fmt.Errorf("create new box: %w", err)
		}

		if err = instance.Start(); err != nil {
			lastErr = err
			_ = instance.Close()
			instance = nil

			if conflictPort := extractPortFromBindError(err); conflictPort > 0 {
				m.logger.Warnf("port %d is in use, reassigning and retrying...", conflictPort)
				if reassigned := reassignConflictingPort(newCfg, conflictPort); reassigned {
					pool.ResetSharedStateStore()
					continue
				}
			}

			m.rollbackToOldConfig(ctx, oldCfg)
			return fmt.Errorf("start new box: %w", err)
		}

		started = true
		break
	}

	if !started {
		m.rollbackToOldConfig(ctx, oldCfg)
		if lastErr == nil {
			lastErr = errors.New("max retries exceeded")
		}
		return fmt.Errorf("start new box: exceeded max retries (%d): %w", maxRetries, lastErr)
	}

	m.applyConfigSettings(newCfg)

	m.mu.Lock()
	m.currentBox = instance
	// IMPORTANT: keep m.cfg pointer stable; sync content instead of replacing pointer.
	m.syncConfigLocked(newCfg)
	m.mu.Unlock()

	// Prune monitor nodes not registered by the active config generation.
	if gen != 0 {
		if mm := m.MonitorManager(); mm != nil {
			mm.PruneNodesNotInGeneration(gen)
		}
	}

	m.logger.Infof("reload completed successfully with %d nodes", len(newCfg.Nodes))
	return nil
}

// rollbackToOldConfig attempts to restart with the previous configuration.
func (m *Manager) rollbackToOldConfig(ctx context.Context, oldCfg *config.Config) {
	if oldCfg == nil {
		return
	}
	m.logger.Warnf("attempting rollback to previous config...")

	// New generation for rollback so we can prune any nodes registered by the failed attempt.
	var gen uint64
	if mm := m.MonitorManager(); mm != nil {
		gen = mm.BeginNodeSync()
	}

	instance, err := m.createBox(ctx, oldCfg)
	if err != nil {
		m.logger.Errorf("rollback failed to create box: %v", err)
		return
	}
	if err := instance.Start(); err != nil {
		_ = instance.Close()
		m.logger.Errorf("rollback failed to start box: %v", err)
		return
	}

	m.mu.Lock()
	m.currentBox = instance
	// Keep pointer stable; sync content so all other holders (monitor/server/subscription) stay consistent.
	m.syncConfigLocked(oldCfg)
	m.mu.Unlock()

	// Prune monitor nodes not registered by rollback generation.
	if gen != 0 {
		if mm := m.MonitorManager(); mm != nil {
			mm.PruneNodesNotInGeneration(gen)
		}
	}

	m.logger.Infof("rollback successful")
}

// Close terminates the active instance and auxiliary components.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var err error
	if m.currentBox != nil {
		err = m.currentBox.Close()
		m.currentBox = nil
	}
	if m.monitorServer != nil {
		m.monitorServer.Shutdown(context.Background())
		m.monitorServer = nil
	}
	if m.monitorMgr != nil {
		m.monitorMgr.Stop()
		m.monitorMgr = nil
		m.healthCheckStarted = false
	}
	m.baseCtx = nil
	return err
}

// MonitorManager returns the shared monitor manager.
func (m *Manager) MonitorManager() *monitor.Manager {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.monitorMgr
}

// MonitorServer returns the monitor HTTP server.
func (m *Manager) MonitorServer() *monitor.Server {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.monitorServer
}

// createBox builds a sing-box instance from config.
func (m *Manager) createBox(ctx context.Context, cfg *config.Config) (*box.Box, error) {
	if cfg == nil {
		return nil, errors.New("config is nil")
	}
	if m.monitorMgr == nil {
		return nil, errors.New("monitor manager not initialized")
	}

	opts, err := builder.Build(cfg)
	if err != nil {
		return nil, fmt.Errorf("build sing-box options: %w", err)
	}

	inboundRegistry := include.InboundRegistry()
	outboundRegistry := include.OutboundRegistry()
	pool.Register(outboundRegistry)
	endpointRegistry := include.EndpointRegistry()
	dnsRegistry := include.DNSTransportRegistry()
	serviceRegistry := include.ServiceRegistry()

	boxCtx := box.Context(ctx, inboundRegistry, outboundRegistry, endpointRegistry, dnsRegistry, serviceRegistry)
	boxCtx = monitor.ContextWith(boxCtx, m.monitorMgr)

	instance, err := box.New(box.Options{Context: boxCtx, Options: opts})
	if err != nil {
		return nil, fmt.Errorf("create sing-box instance: %w", err)
	}
	return instance, nil
}

// gracefulSwitch swaps the current box with a new one.
func (m *Manager) gracefulSwitch(newBox *box.Box) error {
	if newBox == nil {
		return errors.New("new box is nil")
	}

	m.mu.Lock()
	old := m.currentBox
	m.currentBox = newBox
	drainTimeout := m.drainTimeout
	m.mu.Unlock()

	if old != nil {
		go m.drainOldBox(old, drainTimeout)
	}

	m.logger.Infof("switched to new instance, draining old for %s", drainTimeout)
	return nil
}

// drainOldBox waits for drain timeout then closes the old box.
func (m *Manager) drainOldBox(oldBox *box.Box, timeout time.Duration) {
	if oldBox == nil {
		return
	}
	if timeout > 0 {
		time.Sleep(timeout)
	}
	if err := oldBox.Close(); err != nil {
		m.logger.Errorf("failed to close old instance: %v", err)
		return
	}
	m.logger.Infof("old instance closed after %s drain", timeout)
}

// waitForHealthCheck polls until enough nodes are available or timeout.
func (m *Manager) waitForHealthCheck(timeout time.Duration) error {
	if m.monitorMgr == nil || m.minAvailableNodes <= 0 {
		return nil
	}
	if timeout <= 0 {
		timeout = defaultHealthCheckTimeout
	}

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(healthCheckPollInterval)
	defer ticker.Stop()

	for {
		available, total := m.availableNodeCount()
		if available >= m.minAvailableNodes {
			m.logger.Infof("health check passed: %d/%d nodes available", available, total)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout: %d/%d nodes available (need >= %d)", available, total, m.minAvailableNodes)
		}
		<-ticker.C
	}
}

// availableNodeCount returns (available, total) node counts.
func (m *Manager) availableNodeCount() (int, int) {
	if m.monitorMgr == nil {
		return 0, 0
	}
	snapshots := m.monitorMgr.Snapshot()
	total := len(snapshots)
	available := 0
	for _, snap := range snapshots {
		if snap.InitialCheckDone && snap.Available {
			available++
		}
	}
	return available, total
}

// ensureMonitor initializes monitor manager and server if needed.
func (m *Manager) ensureMonitor(ctx context.Context) error {
	m.mu.Lock()
	if m.monitorMgr != nil {
		m.mu.Unlock()
		return nil
	}

	monitorMgr, err := monitor.NewManager(m.monitorCfg)
	if err != nil {
		m.mu.Unlock()
		return fmt.Errorf("init monitor manager: %w", err)
	}
	monitorMgr.SetLogger(monitorLoggerAdapter{logger: m.logger})
	m.monitorMgr = monitorMgr

	var serverToStart *monitor.Server
	if m.monitorCfg.Enabled {
		if m.monitorServer == nil {
			serverToStart = monitor.NewServer(m.monitorCfg, monitorMgr, log.Default())
			m.monitorServer = serverToStart
		}
		if m.monitorServer != nil {
			m.monitorServer.SetNodeManager(m)
		}
	}
	m.mu.Unlock()

	if serverToStart != nil {
		serverToStart.Start(ctx)
	}
	return nil
}

// applyConfigSettings extracts runtime settings from config.
func (m *Manager) applyConfigSettings(cfg *config.Config) {
	if cfg == nil {
		return
	}
	if cfg.SubscriptionRefresh.DrainTimeout > 0 {
		m.drainTimeout = cfg.SubscriptionRefresh.DrainTimeout
	} else if m.drainTimeout == 0 {
		m.drainTimeout = defaultDrainTimeout
	}
	m.minAvailableNodes = cfg.SubscriptionRefresh.MinAvailableNodes
}

// defaultLogger is the fallback logger using standard log.
type defaultLogger struct{}

func (defaultLogger) Infof(format string, args ...any) {
	log.Printf("[boxmgr] "+format, args...)
}

func (defaultLogger) Warnf(format string, args ...any) {
	log.Printf("[boxmgr] WARN: "+format, args...)
}

func (defaultLogger) Errorf(format string, args ...any) {
	log.Printf("[boxmgr] ERROR: "+format, args...)
}

// monitorLoggerAdapter adapts Logger to monitor.Logger interface.
type monitorLoggerAdapter struct {
	logger Logger
}

func (a monitorLoggerAdapter) Info(args ...any) {
	if a.logger != nil {
		a.logger.Infof("%s", fmt.Sprint(args...))
	}
}

func (a monitorLoggerAdapter) Warn(args ...any) {
	if a.logger != nil {
		a.logger.Warnf("%s", fmt.Sprint(args...))
	}
}

// --- NodeManager interface implementation ---

var errConfigUnavailable = errors.New("config is not initialized")

func (m *Manager) GetSettings(ctx context.Context) (externalIP string, probeTarget string, skipCertVerify bool, err error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return "", "", false, err
		}
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.cfg == nil {
		return "", "", false, errConfigUnavailable
	}
	return m.cfg.ExternalIP, m.cfg.Management.ProbeTarget, m.cfg.SkipCertVerify, nil
}

func (m *Manager) UpdateSettings(ctx context.Context, externalIP, probeTarget string, skipCertVerify bool) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	externalIP = strings.TrimSpace(externalIP)
	probeTarget = strings.TrimSpace(probeTarget)

	m.mu.Lock()
	if m.cfg == nil {
		m.mu.Unlock()
		return errConfigUnavailable
	}

	oldExternalIP := m.cfg.ExternalIP
	oldProbeTarget := m.cfg.Management.ProbeTarget
	oldSkip := m.cfg.SkipCertVerify

	// Validate/apply probe target to runtime monitor first (so invalid input won't touch config file).
	if m.monitorMgr != nil {
		if err := m.monitorMgr.UpdateProbeTarget(probeTarget); err != nil {
			m.mu.Unlock()
			return err
		}
	}

	m.cfg.ExternalIP = externalIP
	m.cfg.Management.ProbeTarget = probeTarget
	m.cfg.SkipCertVerify = skipCertVerify

	if err := m.cfg.SaveSettings(); err != nil {
		// rollback in-memory
		m.cfg.ExternalIP = oldExternalIP
		m.cfg.Management.ProbeTarget = oldProbeTarget
		m.cfg.SkipCertVerify = oldSkip
		m.mu.Unlock()

		// best-effort rollback runtime probe target
		if m.monitorMgr != nil {
			_ = m.monitorMgr.UpdateProbeTarget(oldProbeTarget)
		}
		return fmt.Errorf("save settings: %w", err)
	}

	m.mu.Unlock()
	return nil
}

// ListConfigNodes returns a copy of all configured nodes.
func (m *Manager) ListConfigNodes(ctx context.Context) ([]config.NodeConfig, error) {
	_ = ctx
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.cfg == nil {
		return nil, errConfigUnavailable
	}
	return cloneNodes(m.cfg.Nodes), nil
}

// CreateNode adds a new node to the config and saves it.
func (m *Manager) CreateNode(ctx context.Context, node config.NodeConfig) (config.NodeConfig, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return config.NodeConfig{}, err
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cfg == nil {
		return config.NodeConfig{}, errConfigUnavailable
	}

	normalized, err := m.prepareNodeLocked(node, "")
	if err != nil {
		return config.NodeConfig{}, err
	}

	// Source selection:
	// - If nodes_file is configured, always write new nodes to nodes_file (manual nodes).
	// - Otherwise, write into config.yaml nodes array (inline nodes).
	if m.cfg.NodesFile != "" {
		normalized.Source = config.NodeSourceFile
	} else {
		normalized.Source = config.NodeSourceInline
	}

	m.cfg.Nodes = append(m.cfg.Nodes, normalized)
	if err := m.cfg.Save(); err != nil {
		m.cfg.Nodes = m.cfg.Nodes[:len(m.cfg.Nodes)-1]
		return config.NodeConfig{}, fmt.Errorf("save config: %w", err)
	}
	return normalized, nil
}

// UpdateNode updates an existing node by name and saves the config.
func (m *Manager) UpdateNode(ctx context.Context, name string, node config.NodeConfig) (config.NodeConfig, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return config.NodeConfig{}, err
		}
	}

	name = strings.TrimSpace(name)
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cfg == nil {
		return config.NodeConfig{}, errConfigUnavailable
	}

	idx := m.nodeIndexLocked(name)
	if idx == -1 {
		return config.NodeConfig{}, monitor.ErrNodeNotFound
	}

	normalized, err := m.prepareNodeLocked(node, name)
	if err != nil {
		return config.NodeConfig{}, err
	}

	normalized.Source = m.cfg.Nodes[idx].Source

	prev := m.cfg.Nodes[idx]
	m.cfg.Nodes[idx] = normalized
	if err := m.cfg.Save(); err != nil {
		m.cfg.Nodes[idx] = prev
		return config.NodeConfig{}, fmt.Errorf("save config: %w", err)
	}
	return normalized, nil
}

// DeleteNode removes a node by name and saves the config.
func (m *Manager) DeleteNode(ctx context.Context, name string) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	name = strings.TrimSpace(name)
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cfg == nil {
		return errConfigUnavailable
	}

	idx := m.nodeIndexLocked(name)
	if idx == -1 {
		return monitor.ErrNodeNotFound
	}

	backup := cloneNodes(m.cfg.Nodes)
	m.cfg.Nodes = append(m.cfg.Nodes[:idx], m.cfg.Nodes[idx+1:]...)
	if err := m.cfg.Save(); err != nil {
		m.cfg.Nodes = backup
		return fmt.Errorf("save config: %w", err)
	}
	return nil
}

// TriggerReload reloads the sing-box instance with current config.
func (m *Manager) TriggerReload(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	m.mu.RLock()
	cfgCopy := m.copyConfigLocked()
	portMap := m.cfg.BuildPortMap()
	m.mu.RUnlock()

	if cfgCopy == nil {
		return errConfigUnavailable
	}
	return m.ReloadWithPortMap(cfgCopy, portMap)
}

// ReloadWithPortMap gracefully switches to a new configuration, preserving port assignments.
func (m *Manager) ReloadWithPortMap(newCfg *config.Config, portMap map[string]uint16) error {
	if newCfg == nil {
		return errors.New("new config is nil")
	}

	if portMap != nil && len(portMap) > 0 {
		if err := newCfg.NormalizeWithPortMap(portMap); err != nil {
			return fmt.Errorf("normalize config with port map: %w", err)
		}
	}

	return m.Reload(newCfg)
}

// CurrentPortMap returns the current port mapping from the active configuration.
func (m *Manager) CurrentPortMap() map[string]uint16 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cfg == nil {
		return nil
	}
	return m.cfg.BuildPortMap()
}

// ConfigSnapshot returns a deep copy of current config under lock.
// The returned config is safe to read/modify without racing with manager updates.
func (m *Manager) ConfigSnapshot() *config.Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.copyConfigLocked()
}

// --- Helper functions ---

var portBindErrorRegex = regexp.MustCompile(`listen tcp[46]? [^:]+:(\d+): bind: address already in use`)

func extractPortFromBindError(err error) uint16 {
	if err == nil {
		return 0
	}
	matches := portBindErrorRegex.FindStringSubmatch(err.Error())
	if len(matches) < 2 {
		return 0
	}
	var port int
	fmt.Sscanf(matches[1], "%d", &port)
	if port > 0 && port <= 65535 {
		return uint16(port)
	}
	return 0
}

func isPortAvailable(address string, port uint16) bool {
	addr := fmt.Sprintf("%s:%d", address, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

func reassignConflictingPort(cfg *config.Config, conflictPort uint16) bool {
	usedPorts := make(map[uint16]bool)
	if cfg.Mode == "hybrid" {
		usedPorts[cfg.Listener.Port] = true
	}
	for _, node := range cfg.Nodes {
		usedPorts[node.Port] = true
	}

	for idx := range cfg.Nodes {
		if cfg.Nodes[idx].Port == conflictPort {
			newPort := conflictPort + 1
			address := cfg.MultiPort.Address
			if address == "" {
				address = "0.0.0.0"
			}
			for usedPorts[newPort] || !isPortAvailable(address, newPort) {
				newPort++
				if newPort > 65535 {
					log.Printf("❌ No available port found for node %q", cfg.Nodes[idx].Name)
					return false
				}
			}
			log.Printf("⚠️  Port %d in use, reassigning node %q to port %d", conflictPort, cfg.Nodes[idx].Name, newPort)
			cfg.Nodes[idx].Port = newPort
			return true
		}
	}
	return false
}

func cloneNodes(nodes []config.NodeConfig) []config.NodeConfig {
	if len(nodes) == 0 {
		return []config.NodeConfig{}
	}
	out := make([]config.NodeConfig, len(nodes))
	copy(out, nodes)
	return out
}

func (m *Manager) copyConfigLocked() *config.Config {
	if m.cfg == nil {
		return nil
	}
	cloned := *m.cfg
	cloned.Nodes = cloneNodes(m.cfg.Nodes)
	cloned.SetFilePath(m.cfg.FilePath())
	return &cloned
}

func cloneBoolPtr(v *bool) *bool {
	if v == nil {
		return nil
	}
	b := *v
	return &b
}

// syncConfigLocked copies newCfg's content into the existing config pointer (m.cfg),
// so other components holding the original *config.Config keep seeing the latest config.
// NOTE: caller must hold m.mu.
func (m *Manager) syncConfigLocked(newCfg *config.Config) {
	if newCfg == nil {
		return
	}
	if m.cfg == nil {
		m.cfg = newCfg
		return
	}
	// Keep pointer stable: never replace m.cfg with a different pointer.
	if m.cfg == newCfg {
		return
	}

	oldPath := m.cfg.FilePath()
	newPath := newCfg.FilePath()

	// Copy scalar fields.
	*m.cfg = *newCfg

	// Deep-copy slices / pointer fields to avoid sharing with newCfg (and keep behavior predictable).
	m.cfg.Nodes = cloneNodes(newCfg.Nodes)
	m.cfg.Subscriptions = append([]string(nil), newCfg.Subscriptions...)
	m.cfg.Management.Enabled = cloneBoolPtr(newCfg.Management.Enabled)

	// Ensure file path is preserved even if newCfg doesn't carry it.
	if newPath != "" {
		m.cfg.SetFilePath(newPath)
	} else if oldPath != "" {
		m.cfg.SetFilePath(oldPath)
	}
}

func (m *Manager) nodeIndexLocked(name string) int {
	for idx, node := range m.cfg.Nodes {
		if node.Name == name {
			return idx
		}
	}
	return -1
}

func (m *Manager) portInUseLocked(port uint16, currentName string) bool {
	if port == 0 {
		return false
	}
	for _, node := range m.cfg.Nodes {
		if node.Name == currentName {
			continue
		}
		if node.Port == port {
			return true
		}
	}
	return false
}

func (m *Manager) nextAvailablePortLocked() uint16 {
	base := m.cfg.MultiPort.BasePort
	if base == 0 {
		base = 24000
	}
	used := make(map[uint16]struct{}, len(m.cfg.Nodes))
	for _, node := range m.cfg.Nodes {
		if node.Port > 0 {
			used[node.Port] = struct{}{}
		}
	}
	port := base
	for i := 0; i < 1<<16; i++ {
		if _, ok := used[port]; !ok && port != 0 {
			return port
		}
		port++
		if port == 0 {
			port = 1
		}
	}
	return base
}

func (m *Manager) prepareNodeLocked(node config.NodeConfig, currentName string) (config.NodeConfig, error) {
	node.Name = strings.TrimSpace(node.Name)
	node.URI = strings.TrimSpace(node.URI)

	if node.URI == "" {
		return config.NodeConfig{}, fmt.Errorf("%w: URI 不能为空", monitor.ErrInvalidNode)
	}

	if node.Name == "" {
		if currentName != "" {
			node.Name = currentName
		} else if idx := strings.LastIndex(node.URI, "#"); idx != -1 && idx < len(node.URI)-1 {
			fragment := node.URI[idx+1:]
			if decoded, err := url.QueryUnescape(fragment); err == nil && decoded != "" {
				node.Name = decoded
			}
		}
		if node.Name == "" {
			node.Name = fmt.Sprintf("node-%d", len(m.cfg.Nodes)+1)
		}
	}

	if idx := m.nodeIndexLocked(node.Name); idx != -1 {
		if currentName == "" || m.cfg.Nodes[idx].Name != currentName {
			return config.NodeConfig{}, fmt.Errorf("%w: 节点 %s 已存在", monitor.ErrNodeConflict, node.Name)
		}
	}

	if m.cfg.Mode == "multi-port" {
		if node.Port == 0 {
			node.Port = m.nextAvailablePortLocked()
		} else if m.portInUseLocked(node.Port, currentName) {
			return config.NodeConfig{}, fmt.Errorf("%w: 端口 %d 已被占用", monitor.ErrNodeConflict, node.Port)
		}
		if node.Username == "" {
			node.Username = m.cfg.MultiPort.Username
			node.Password = m.cfg.MultiPort.Password
		}
	}

	return node, nil
}