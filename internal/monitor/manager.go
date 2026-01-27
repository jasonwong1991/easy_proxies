package monitor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	M "github.com/sagernet/sing/common/metadata"
)

// Config mirrors user settings needed by the monitoring server.
type Config struct {
	Enabled        bool
	Listen         string
	ProbeTarget    string
	Password       string
	ProxyUsername  string // 代理池的用户名（用于导出）
	ProxyPassword  string // 代理池的密码（用于导出）
	ExternalIP     string // 外部 IP 地址，用于导出时替换 0.0.0.0
	SkipCertVerify bool   // 全局跳过 SSL 证书验证
}

// NodeInfo is static metadata about a proxy entry.
type NodeInfo struct {
	Tag           string `json:"tag"`
	Name          string `json:"name"`
	URI           string `json:"uri"`
	Mode          string `json:"mode"`
	ListenAddress string `json:"listen_address,omitempty"`
	Port          uint16 `json:"port,omitempty"`
}

// TimelineEvent represents a single usage event for debug tracking.
type TimelineEvent struct {
	Time      time.Time `json:"time"`
	Success   bool      `json:"success"`
	LatencyMs int64     `json:"latency_ms"`
	Error     string    `json:"error,omitempty"`
}

const maxTimelineSize = 20

// Snapshot is a runtime view of a proxy node.
type Snapshot struct {
	NodeInfo
	FailureCount      int             `json:"failure_count"`
	SuccessCount      int64           `json:"success_count"`
	Blacklisted       bool            `json:"blacklisted"`
	BlacklistedUntil  time.Time       `json:"blacklisted_until"`
	ActiveConnections int32           `json:"active_connections"`
	LastError         string          `json:"last_error,omitempty"`
	LastFailure       time.Time       `json:"last_failure,omitempty"`
	LastSuccess       time.Time       `json:"last_success,omitempty"`
	LastProbeLatency  time.Duration   `json:"last_probe_latency,omitempty"`
	LastLatencyMs     int64           `json:"last_latency_ms"`
	Available         bool            `json:"available"`
	InitialCheckDone  bool            `json:"initial_check_done"`
	Timeline          []TimelineEvent `json:"timeline,omitempty"`
}

type probeFunc func(ctx context.Context) (time.Duration, error)
type releaseFunc func()

type EntryHandle struct {
	ref *entry
}

type entry struct {
	generation 		 uint64
	info             NodeInfo
	failure          int
	success          int64
	timeline         []TimelineEvent
	blacklist        bool
	until            time.Time
	lastError        string
	lastFail         time.Time
	lastOK           time.Time
	lastProbe        time.Duration
	active           atomic.Int32
	probe            probeFunc
	release          releaseFunc
	initialCheckDone bool
	available        bool
	mu               sync.RWMutex
}

// Manager aggregates all node states for the UI/API.
type Manager struct {
	cfg        Config
	probeDst   M.Socksaddr
	probeReady bool

	// probeTLS indicates whether to do TLS handshake before HTTP probing.
	probeTLS        bool
	probeServerName string // SNI
	probeHostHeader string // Host header (may include :port, IPv6 brackets)

	mu         sync.RWMutex
	nodes      map[string]*entry
	generation uint64

	ctx    context.Context
	cancel context.CancelFunc
	logger Logger
}

// Logger interface for logging
type Logger interface {
	Info(args ...any)
	Warn(args ...any)
}

// NewManager constructs a manager and pre-validates the probe target.
func NewManager(cfg Config) (*Manager, error) {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		cfg:    cfg,
		nodes:  make(map[string]*entry),
		ctx:    ctx,
		cancel: cancel,
	}

	dst, hostHeader, useTLS, serverName, ready, err := parseProbeTarget(cfg.ProbeTarget)
	if err != nil {
		cancel()
		return nil, err
	}
	m.probeDst = dst
	m.probeReady = ready
	m.probeTLS = useTLS
	m.probeServerName = serverName
	m.probeHostHeader = hostHeader

	return m, nil
}

// SetLogger sets the logger for the manager.
func (m *Manager) SetLogger(logger Logger) {
	m.logger = logger
}

// StartPeriodicHealthCheck starts a background goroutine that periodically checks all nodes.
// interval: how often to check (e.g., 30 * time.Second)
// timeout: timeout for each probe (e.g., 10 * time.Second)
func (m *Manager) StartPeriodicHealthCheck(interval, timeout time.Duration) {
	if m.logger != nil {
		if _, ok := m.DestinationForProbe(); !ok {
			m.logger.Warn("probe target not configured, periodic health check will wait until it is set")
		}
		m.logger.Info("periodic health check started, interval: ", interval)
	}

	go func() {
		// 启动后立即进行一次检查（如果 probe_target 可用）
		m.probeAllNodesIfReady(timeout)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-m.ctx.Done():
				return
			case <-ticker.C:
				m.probeAllNodesIfReady(timeout)
			}
		}
	}()
}

// probeAllNodes checks all registered nodes concurrently.
func (m *Manager) probeAllNodes(timeout time.Duration) {
	m.mu.RLock()
	entries := make([]*entry, 0, len(m.nodes))
	for _, e := range m.nodes {
		entries = append(entries, e)
	}
	m.mu.RUnlock()

	if len(entries) == 0 {
		return
	}

	if m.logger != nil {
		m.logger.Info("starting health check for ", len(entries), " nodes")
	}

	// 为大节点量场景提高并发，但避免过高造成抖动
	workerLimit := runtime.NumCPU() * 4
	if workerLimit < 16 {
		workerLimit = 16
	}
	if workerLimit > 32 {
		workerLimit = 32
	}
	if workerLimit > len(entries) {
		workerLimit = len(entries)
	}

	sem := make(chan struct{}, workerLimit)
	var wg sync.WaitGroup
	var availableCount atomic.Int32
	var failedCount atomic.Int32

	for _, e := range entries {
		e.mu.RLock()
		probeFn := e.probe
		tag := e.info.Tag
		e.mu.RUnlock()

		if probeFn == nil {
			continue
		}

		sem <- struct{}{}
		wg.Add(1)
		go func(entry *entry, probe probeFunc, tag string) {
			defer wg.Done()
			defer func() { <-sem }()

			ctx, cancel := context.WithTimeout(m.ctx, timeout)
			latency, err := probe(ctx)
			cancel()

			entry.mu.Lock()
			if err != nil {
				failedCount.Add(1)
				entry.lastError = err.Error()
				entry.lastFail = time.Now()
				entry.available = false
				entry.initialCheckDone = true
			} else {
				availableCount.Add(1)
				entry.lastOK = time.Now()
				entry.lastProbe = latency
				entry.available = true
				entry.initialCheckDone = true
			}
			entry.mu.Unlock()

			if err != nil && m.logger != nil {
				m.logger.Warn("probe failed for ", tag, ": ", err)
			}
		}(e, probeFn, tag)
	}
	wg.Wait()

	if m.logger != nil {
		m.logger.Info("health check completed: ", availableCount.Load(), " available, ", failedCount.Load(), " failed")
	}
}

// Stop stops the periodic health check.
func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
}

func parsePort(value string) uint16 {
	p, err := strconv.Atoi(value)
	if err != nil || p <= 0 || p > 65535 {
		return 80
	}
	return uint16(p)
}

func parsePortStrict(value string) (uint16, error) {
	p, err := strconv.Atoi(value)
	if err != nil || p <= 0 || p > 65535 {
		return 0, fmt.Errorf("invalid port %q", value)
	}
	return uint16(p), nil
}

func parseProbeTarget(raw string) (dst M.Socksaddr, hostHeader string, useTLS bool, serverName string, ready bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return M.Socksaddr{}, "", false, "", false, nil
	}

	target := raw
	lower := strings.ToLower(target)

	defaultPort := uint16(80)
	hasScheme := false

	if strings.HasPrefix(lower, "https://") {
		hasScheme = true
		useTLS = true
		defaultPort = 443
		target = target[len("https://"):]
	} else if strings.HasPrefix(lower, "http://") {
		hasScheme = true
		useTLS = false
		defaultPort = 80
		target = target[len("http://"):]
	}

	// Strip trailing path if present.
	if idx := strings.Index(target, "/"); idx != -1 {
		target = target[:idx]
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return M.Socksaddr{}, "", false, "", false, fmt.Errorf("invalid probe_target %q", raw)
	}

	host := ""
	port := defaultPort

	if strings.HasPrefix(target, "[") {
		// Bracketed IPv6.
		if strings.Contains(target, "]:") {
			h, p, e := net.SplitHostPort(target)
			if e != nil {
				return M.Socksaddr{}, "", false, "", false, fmt.Errorf("invalid probe_target %q: %w", raw, e)
			}
			pp, e := parsePortStrict(p)
			if e != nil {
				return M.Socksaddr{}, "", false, "", false, e
			}
			host = h
			port = pp
		} else if strings.HasSuffix(target, "]") {
			host = strings.TrimSuffix(strings.TrimPrefix(target, "["), "]")
			port = defaultPort
		} else {
			return M.Socksaddr{}, "", false, "", false, fmt.Errorf("invalid probe_target %q", raw)
		}
	} else if strings.Count(target, ":") == 1 {
		idx := strings.LastIndex(target, ":")
		h := strings.TrimSpace(target[:idx])
		p := strings.TrimSpace(target[idx+1:])
		if h == "" || p == "" {
			return M.Socksaddr{}, "", false, "", false, fmt.Errorf("invalid probe_target %q", raw)
		}
		pp, e := parsePortStrict(p)
		if e != nil {
			return M.Socksaddr{}, "", false, "", false, e
		}
		host = h
		port = pp
	} else {
		host = target
		port = defaultPort
	}

	host = strings.TrimSpace(host)
	if host == "" {
		return M.Socksaddr{}, "", false, "", false, fmt.Errorf("invalid probe_target %q", raw)
	}

	// Heuristic: no scheme + port 443 => treat as TLS.
	if !hasScheme && port == 443 {
		useTLS = true
	}

	serverName = host

	// Host header uses URI authority form; IPv6 must be bracketed.
	hostForHeader := host
	if strings.Contains(host, ":") {
		hostForHeader = "[" + host + "]"
	}

	// Include port in Host header only if non-default for the chosen scheme.
	if (useTLS && port == 443) || (!useTLS && port == 80) {
		hostHeader = hostForHeader
	} else {
		hostHeader = fmt.Sprintf("%s:%d", hostForHeader, port)
	}

	dst = M.ParseSocksaddrHostPort(host, port)
	return dst, hostHeader, useTLS, serverName, true, nil
}

// UpdateProbeTarget updates probe destination dynamically.
// After this, future probes / periodic health checks will use the new target.
func (m *Manager) UpdateProbeTarget(target string) error {
	dst, hostHeader, useTLS, serverName, ready, err := parseProbeTarget(target)
	if err != nil {
		return err
	}

	m.mu.Lock()
	m.cfg.ProbeTarget = strings.TrimSpace(target)
	m.probeDst = dst
	m.probeReady = ready
	m.probeTLS = useTLS
	m.probeServerName = serverName
	m.probeHostHeader = hostHeader
	m.mu.Unlock()

	if m.logger != nil {
		if ready {
			m.logger.Info("probe target updated: ", m.cfg.ProbeTarget)
		} else {
			m.logger.Warn("probe target cleared, health check will be skipped")
		}
	}
	return nil
}

func (m *Manager) probeAllNodesIfReady(timeout time.Duration) {
	if _, ok := m.DestinationForProbe(); !ok {
		return
	}
	m.probeAllNodes(timeout)
}

// BeginNodeSync starts a new "generation" for node registrations.
// Any Register() calls after this will mark entries with the new generation.
func (m *Manager) BeginNodeSync() uint64 {
	m.mu.Lock()
	m.generation++
	gen := m.generation
	m.mu.Unlock()
	return gen
}

// PruneNodesNotInGeneration removes nodes that were NOT registered in the given generation.
// Use this after a reload to drop stale nodes from previous configs/failed reload attempts.
func (m *Manager) PruneNodesNotInGeneration(gen uint64) int {
	m.mu.Lock()
	removed := 0
	for tag, e := range m.nodes {
		if e.generation != gen {
			delete(m.nodes, tag)
			removed++
		}
	}
	m.mu.Unlock()

	if removed > 0 && m.logger != nil {
		m.logger.Info("pruned ", removed, " stale nodes")
	}
	return removed
}

// Register ensures a node is tracked and returns its entry.
func (m *Manager) Register(info NodeInfo) *EntryHandle {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.nodes[info.Tag]
	if !ok {
		e = &entry{
			generation: m.generation,
			info:       info,
			timeline:   make([]TimelineEvent, 0, maxTimelineSize),
		}
		m.nodes[info.Tag] = e
	} else {
		e.info = info
		e.generation = m.generation
	}
	return &EntryHandle{ref: e}
}

// DestinationForProbe exposes the configured destination for health checks.
func (m *Manager) DestinationForProbe() (M.Socksaddr, bool) {
	m.mu.RLock()
	ready := m.probeReady
	dst := m.probeDst
	m.mu.RUnlock()

	if !ready {
		return M.Socksaddr{}, false
	}
	return dst, true
}

func (m *Manager) ProbeTargetInfo() (dst M.Socksaddr, hostHeader string, useTLS bool, serverName string, ok bool) {
	m.mu.RLock()
	ready := m.probeReady
	dst = m.probeDst
	hostHeader = m.probeHostHeader
	useTLS = m.probeTLS
	serverName = m.probeServerName
	m.mu.RUnlock()

	if !ready {
		return M.Socksaddr{}, "", false, "", false
	}
	return dst, hostHeader, useTLS, serverName, true
}

// Snapshot returns a sorted copy of current node states.
// If onlyAvailable is true, only returns nodes that passed initial health check.
func (m *Manager) Snapshot() []Snapshot {
	return m.SnapshotFiltered(false)
}

// SnapshotFiltered returns a sorted copy of current node states.
// If onlyAvailable is true, only returns nodes that passed initial health check.
// Nodes that haven't been checked yet are also included (they will be checked on first use).
func (m *Manager) SnapshotFiltered(onlyAvailable bool) []Snapshot {
	m.mu.RLock()
	list := make([]*entry, 0, len(m.nodes))
	for _, e := range m.nodes {
		list = append(list, e)
	}
	m.mu.RUnlock()
	snapshots := make([]Snapshot, 0, len(list))
	for _, e := range list {
		snap := e.snapshot()
		// 如果只要可用节点：
		// - 跳过已完成检查但不可用的节点
		// - 保留未完成检查的节点（它们会在首次使用时被检查）
		if onlyAvailable && snap.InitialCheckDone && !snap.Available {
			continue
		}
		snapshots = append(snapshots, snap)
	}
	// 按延迟排序（延迟小的在前面，未测试的排在最后）
	sort.Slice(snapshots, func(i, j int) bool {
		latencyI := snapshots[i].LastLatencyMs
		latencyJ := snapshots[j].LastLatencyMs
		// -1 表示未测试，排在最后
		if latencyI < 0 && latencyJ < 0 {
			return snapshots[i].Name < snapshots[j].Name // 都未测试时按名称排序
		}
		if latencyI < 0 {
			return false // i 未测试，排在后面
		}
		if latencyJ < 0 {
			return true // j 未测试，i 排在前面
		}
		if latencyI == latencyJ {
			return snapshots[i].Name < snapshots[j].Name // 延迟相同时按名称排序
		}
		return latencyI < latencyJ
	})
	return snapshots
}

// Probe triggers a manual health check.
func (m *Manager) Probe(ctx context.Context, tag string) (time.Duration, error) {
	e, err := m.entry(tag)
	if err != nil {
		return 0, err
	}
	if e.probe == nil {
		return 0, errors.New("probe not available for this node")
	}
	latency, err := e.probe(ctx)
	if err != nil {
		return 0, err
	}
	e.recordProbeLatency(latency)
	return latency, nil
}

// Release clears blacklist state for the given node.
func (m *Manager) Release(tag string) error {
	e, err := m.entry(tag)
	if err != nil {
		return err
	}
	if e.release == nil {
		return errors.New("release not available for this node")
	}
	e.release()
	return nil
}

func (m *Manager) entry(tag string) (*entry, error) {
	m.mu.RLock()
	e, ok := m.nodes[tag]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("node %s not found", tag)
	}
	return e, nil
}

func (e *entry) snapshot() Snapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()

	latencyMs := int64(-1)
	if e.lastProbe > 0 {
		latencyMs = e.lastProbe.Milliseconds()
		if latencyMs == 0 {
			latencyMs = 1
		}
	}

	var timelineCopy []TimelineEvent
	if len(e.timeline) > 0 {
		timelineCopy = make([]TimelineEvent, len(e.timeline))
		copy(timelineCopy, e.timeline)
	}

	return Snapshot{
		NodeInfo:          e.info,
		FailureCount:      e.failure,
		SuccessCount:      e.success,
		Blacklisted:       e.blacklist,
		BlacklistedUntil:  e.until,
		ActiveConnections: e.active.Load(),
		LastError:         e.lastError,
		LastFailure:       e.lastFail,
		LastSuccess:       e.lastOK,
		LastProbeLatency:  e.lastProbe,
		LastLatencyMs:     latencyMs,
		Available:         e.available,
		InitialCheckDone:  e.initialCheckDone,
		Timeline:          timelineCopy,
	}
}

func (e *entry) recordFailure(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	errStr := err.Error()
	e.failure++
	e.lastError = errStr
	e.lastFail = time.Now()
	e.appendTimelineLocked(false, 0, errStr)
}

func (e *entry) recordSuccess() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.success++
	e.lastOK = time.Now()
	e.appendTimelineLocked(true, 0, "")
}

func (e *entry) recordSuccessWithLatency(latency time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.success++
	e.lastOK = time.Now()
	e.lastProbe = latency
	latencyMs := latency.Milliseconds()
	if latencyMs == 0 && latency > 0 {
		latencyMs = 1
	}
	e.appendTimelineLocked(true, latencyMs, "")
}

func (e *entry) appendTimelineLocked(success bool, latencyMs int64, errStr string) {
	evt := TimelineEvent{
		Time:      time.Now(),
		Success:   success,
		LatencyMs: latencyMs,
		Error:     errStr,
	}
	if len(e.timeline) >= maxTimelineSize {
		copy(e.timeline, e.timeline[1:])
		e.timeline[len(e.timeline)-1] = evt
	} else {
		e.timeline = append(e.timeline, evt)
	}
}

func (e *entry) blacklistUntil(until time.Time) {
	e.mu.Lock()
	e.blacklist = true
	e.until = until
	e.mu.Unlock()
}

func (e *entry) clearBlacklist() {
	e.mu.Lock()
	e.blacklist = false
	e.until = time.Time{}
	e.mu.Unlock()
}

func (e *entry) incActive() {
	e.active.Add(1)
}

func (e *entry) decActive() {
	e.active.Add(-1)
}

func (e *entry) setProbe(fn probeFunc) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.probe = fn
}

func (e *entry) setRelease(fn releaseFunc) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.release = fn
}

func (e *entry) recordProbeLatency(d time.Duration) {
	e.mu.Lock()
	e.lastProbe = d
	e.mu.Unlock()
}

// RecordFailure updates failure counters.
func (h *EntryHandle) RecordFailure(err error) {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.recordFailure(err)
}

// RecordSuccess updates the last success timestamp.
func (h *EntryHandle) RecordSuccess() {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.recordSuccess()
}

// RecordSuccessWithLatency updates the last success timestamp and latency.
func (h *EntryHandle) RecordSuccessWithLatency(latency time.Duration) {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.recordSuccessWithLatency(latency)
}

// Blacklist marks the node unavailable until the given deadline.
func (h *EntryHandle) Blacklist(until time.Time) {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.blacklistUntil(until)
}

// ClearBlacklist removes the blacklist flag.
func (h *EntryHandle) ClearBlacklist() {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.clearBlacklist()
}

// IncActive increments the active connection counter.
func (h *EntryHandle) IncActive() {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.incActive()
}

// DecActive decrements the active connection counter.
func (h *EntryHandle) DecActive() {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.decActive()
}

// SetProbe assigns a probe function.
func (h *EntryHandle) SetProbe(fn func(ctx context.Context) (time.Duration, error)) {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.setProbe(fn)
}

// SetRelease assigns a release function.
func (h *EntryHandle) SetRelease(fn func()) {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.setRelease(fn)
}

// Health returns current health-check status of the node.
func (h *EntryHandle) Health() (initialCheckDone bool, available bool) {
	if h == nil || h.ref == nil {
		return false, true
	}
	h.ref.mu.RLock()
	defer h.ref.mu.RUnlock()
	return h.ref.initialCheckDone, h.ref.available
}

// MarkInitialCheckDone marks the initial health check as completed.
func (h *EntryHandle) MarkInitialCheckDone(available bool) {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.mu.Lock()
	h.ref.initialCheckDone = true
	h.ref.available = available
	h.ref.mu.Unlock()
}

// MarkAvailable updates the availability status.
func (h *EntryHandle) MarkAvailable(available bool) {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.mu.Lock()
	h.ref.available = available
	h.ref.mu.Unlock()
}