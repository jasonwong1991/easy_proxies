package pool

import (
	"bufio"
	"context"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"easy_proxies/internal/monitor"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
)

const (
	// Type is the outbound type name exposed to sing-box.
	Type = "pool"
	// Tag is the default outbound tag used by builder.
	Tag = "proxy-pool"

	modeSequential = "sequential"
	modeRandom     = "random"
	modeBalance    = "balance"
)

// Options controls pool outbound behaviour.
type Options struct {
	Mode              string
	Members           []string
	FailureThreshold  int
	BlacklistDuration time.Duration
	Metadata          map[string]MemberMeta
}

// MemberMeta carries optional descriptive information for monitoring UI.
type MemberMeta struct {
	Name          string
	URI           string
	Mode          string
	ListenAddress string
	Port          uint16
}

// Register wires the pool outbound into the registry.
func Register(registry *outbound.Registry) {
	outbound.Register[Options](registry, Type, newPool)
}

type memberState struct {
	outbound adapter.Outbound
	tag      string

	failures         int
	blacklisted      bool
	blacklistedUntil time.Time
	active           atomic.Int32
	entry            *monitor.EntryHandle
}

type poolOutbound struct {
	outbound.Adapter
	ctx     context.Context
	logger  log.ContextLogger
	manager adapter.OutboundManager
	options Options
	mode    string
	members []*memberState
	mu      sync.Mutex
	rrIndex int
	rng     *rand.Rand
	monitor *monitor.Manager
}

func newPool(ctx context.Context, _ adapter.Router, logger log.ContextLogger, tag string, options Options) (adapter.Outbound, error) {
	if len(options.Members) == 0 {
		return nil, E.New("pool requires at least one member")
	}
	manager := service.FromContext[adapter.OutboundManager](ctx)
	if manager == nil {
		return nil, E.New("missing outbound manager in context")
	}
	monitorMgr := monitor.FromContext(ctx)
	normalized := normalizeOptions(options)
	p := &poolOutbound{
		Adapter: outbound.NewAdapter(Type, tag, []string{N.NetworkTCP, N.NetworkUDP}, normalized.Members),
		ctx:     ctx,
		logger:  logger,
		manager: manager,
		options: normalized,
		mode:    normalized.Mode,
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
		monitor: monitorMgr,
	}

	// Register nodes immediately if monitor is available
	if monitorMgr != nil {
		logger.Info("registering ", len(normalized.Members), " nodes to monitor")
		for _, memberTag := range normalized.Members {
			meta := normalized.Metadata[memberTag]
			info := monitor.NodeInfo{
				Tag:           memberTag,
				Name:          meta.Name,
				URI:           meta.URI,
				Mode:          meta.Mode,
				ListenAddress: meta.ListenAddress,
				Port:          meta.Port,
			}
			entry := monitorMgr.Register(info)
			if entry != nil {
				logger.Info("registered node: ", memberTag)
				// Set probe and release functions immediately
				entry.SetRelease(p.makeReleaseByTagFunc(memberTag))
				if probeFn := p.makeProbeByTagFunc(memberTag); probeFn != nil {
					entry.SetProbe(probeFn)
				}
			} else {
				logger.Warn("failed to register node: ", memberTag)
			}
		}
	} else {
		logger.Warn("monitor manager is nil, skipping node registration")
	}

	return p, nil
}

func normalizeOptions(options Options) Options {
	if options.FailureThreshold <= 0 {
		options.FailureThreshold = 3
	}
	if options.BlacklistDuration <= 0 {
		options.BlacklistDuration = 24 * time.Hour
	}
	if options.Metadata == nil {
		options.Metadata = make(map[string]MemberMeta)
	}
	switch strings.ToLower(options.Mode) {
	case modeRandom:
		options.Mode = modeRandom
	case modeBalance:
		options.Mode = modeBalance
	default:
		options.Mode = modeSequential
	}
	return options
}

func (p *poolOutbound) Start(stage adapter.StartStage) error {
	if stage != adapter.StartStateStart {
		return nil
	}
	p.mu.Lock()
	err := p.initializeMembersLocked()
	p.mu.Unlock()
	if err != nil {
		return err
	}
	// 在初始化完成后，立即在后台触发健康检查
	if p.monitor != nil {
		go p.probeAllMembersOnStartup()
	}
	return nil
}

// initializeMembersLocked must be called with p.mu held
func (p *poolOutbound) initializeMembersLocked() error {
	if len(p.members) > 0 {
		return nil // Already initialized
	}

	members := make([]*memberState, 0, len(p.options.Members))
	for _, tag := range p.options.Members {
		detour, loaded := p.manager.Outbound(tag)
		if !loaded {
			return E.New("pool member not found: ", tag)
		}
		member := &memberState{
			outbound: detour,
			tag:      tag,
		}
		// Connect to existing monitor entry if available
		if p.monitor != nil {
			meta := p.options.Metadata[tag]
			info := monitor.NodeInfo{
				Tag:           tag,
				Name:          meta.Name,
				URI:           meta.URI,
				Mode:          meta.Mode,
				ListenAddress: meta.ListenAddress,
				Port:          meta.Port,
			}
			entry := p.monitor.Register(info)
			if entry != nil {
				entry.SetRelease(p.makeReleaseFunc(member))
				if probe := p.makeProbeFunc(member); probe != nil {
					entry.SetProbe(probe)
				}
			}
			member.entry = entry
		}
		members = append(members, member)
	}
	p.members = members
	p.logger.Info("pool initialized with ", len(members), " members")

	return nil
}

// probeAllMembersOnStartup performs initial health checks on all members
func (p *poolOutbound) probeAllMembersOnStartup() {
	destination, ok := p.monitor.DestinationForProbe()
	if !ok {
		p.logger.Warn("probe target not configured, skipping initial health check")
		// 没有配置探测目标时，标记所有节点为可用
		p.mu.Lock()
		for _, member := range p.members {
			if member.entry != nil {
				member.entry.MarkInitialCheckDone(true)
			}
		}
		p.mu.Unlock()
		return
	}

	p.logger.Info("starting initial health check for all nodes")

	p.mu.Lock()
	members := make([]*memberState, len(p.members))
	copy(members, p.members)
	p.mu.Unlock()

	availableCount := 0
	failedCount := 0

	// Get probe timeout from monitor config
	probeTimeout := 30 * time.Second
	if p.monitor != nil {
		probeTimeout = p.monitor.ProbeTimeout()
	}

	for _, member := range members {
		// Create a timeout context for each probe
		ctx, cancel := context.WithTimeout(p.ctx, probeTimeout)

		start := time.Now()
		conn, err := member.outbound.DialContext(ctx, N.NetworkTCP, destination)

		if err != nil {
			p.logger.Warn("initial probe failed for ", member.tag, ": ", err)
			failedCount++
			if member.entry != nil {
				member.entry.RecordFailure(err)
				member.entry.MarkInitialCheckDone(false) // 标记为不可用
			}
			cancel()
			continue
		}

		// Perform HTTP probe to measure actual latency (TTFB)
		_, err = httpProbe(conn, destination.AddrString())
		conn.Close()

		if err != nil {
			p.logger.Warn("initial HTTP probe failed for ", member.tag, ": ", err)
			failedCount++
			if member.entry != nil {
				member.entry.RecordFailure(err)
				member.entry.MarkInitialCheckDone(false)
			}
			cancel()
			continue
		}

		// Total latency = dial + HTTP probe
		latency := time.Since(start)
		latencyMs := latency.Milliseconds()
		p.logger.Info("initial probe success for ", member.tag, ", latency: ", latencyMs, "ms")
		availableCount++
		if member.entry != nil {
			member.entry.RecordSuccessWithLatency(latency)
			member.entry.MarkInitialCheckDone(true)
		}

		cancel()
	}

	p.logger.Info("initial health check completed: ", availableCount, " available, ", failedCount, " failed")
}

func (p *poolOutbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	member, err := p.pickMember(network)
	if err != nil {
		return nil, err
	}
	p.incActive(member)
	conn, err := member.outbound.DialContext(ctx, network, destination)
	if err != nil {
		p.decActive(member)
		p.recordFailure(member, err)
		return nil, err
	}
	p.recordSuccess(member)
	return p.wrapConn(conn, member), nil
}

func (p *poolOutbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	member, err := p.pickMember(N.NetworkUDP)
	if err != nil {
		return nil, err
	}
	p.incActive(member)
	conn, err := member.outbound.ListenPacket(ctx, destination)
	if err != nil {
		p.decActive(member)
		p.recordFailure(member, err)
		return nil, err
	}
	p.recordSuccess(member)
	return p.wrapPacketConn(conn, member), nil
}

func (p *poolOutbound) pickMember(network string) (*memberState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Lazy initialization: initialize members on first use if not already done
	if len(p.members) == 0 {
		if err := p.initializeMembersLocked(); err != nil {
			return nil, err
		}
	}
	now := time.Now()
	available := p.availableMembersLocked(now, network)
	if len(available) == 0 {
		if p.releaseIfAllBlacklistedLocked(now) {
			available = p.availableMembersLocked(now, network)
		}
	}
	if len(available) == 0 {
		return nil, E.New("no healthy proxy available")
	}
	return p.selectMemberLocked(available), nil
}

func (p *poolOutbound) availableMembersLocked(now time.Time, network string) []*memberState {
	result := make([]*memberState, 0, len(p.members))
	for _, member := range p.members {
		if member.blacklisted && now.After(member.blacklistedUntil) {
			member.blacklisted = false
			member.blacklistedUntil = time.Time{}
			member.failures = 0
			if member.entry != nil {
				member.entry.ClearBlacklist()
			}
		}
		if member.blacklisted {
			continue
		}
		if network != "" && !common.Contains(member.outbound.Network(), network) {
			continue
		}
		result = append(result, member)
	}
	return result
}

func (p *poolOutbound) releaseIfAllBlacklistedLocked(now time.Time) bool {
	if len(p.members) == 0 {
		return false
	}
	for _, member := range p.members {
		if !member.blacklisted {
			return false
		}
		if now.After(member.blacklistedUntil) {
			member.blacklisted = false
			member.blacklistedUntil = time.Time{}
			member.failures = 0
			if member.entry != nil {
				member.entry.ClearBlacklist()
			}
		}
	}
	for _, member := range p.members {
		if member.blacklisted {
			member.blacklisted = false
			member.blacklistedUntil = time.Time{}
			member.failures = 0
			if member.entry != nil {
				member.entry.ClearBlacklist()
			}
		}
	}
	p.logger.Warn("all upstream proxies were blacklisted, releasing them for retry")
	return true
}

func (p *poolOutbound) selectMemberLocked(candidates []*memberState) *memberState {
	switch p.mode {
	case modeRandom:
		return candidates[p.rng.Intn(len(candidates))]
	case modeBalance:
		var selected *memberState
		var minActive int32
		for _, member := range candidates {
			active := member.active.Load()
			if selected == nil || active < minActive {
				selected = member
				minActive = active
			}
		}
		return selected
	default:
		member := candidates[p.rrIndex%len(candidates)]
		p.rrIndex = (p.rrIndex + 1) % len(candidates)
		return member
	}
}

func (p *poolOutbound) recordFailure(member *memberState, cause error) {
	p.mu.Lock()
	member.failures++
	failures := member.failures
	threshold := p.options.FailureThreshold
	if failures >= threshold {
		member.failures = 0
		member.blacklisted = true
		member.blacklistedUntil = time.Now().Add(p.options.BlacklistDuration)
	}
	p.mu.Unlock()

	if member.entry != nil {
		member.entry.RecordFailure(cause)
		if failures >= threshold {
			member.entry.Blacklist(member.blacklistedUntil)
		}
	}
	if failures >= threshold {
		p.logger.Warn("proxy ", member.tag, " blacklisted for ", p.options.BlacklistDuration, ": ", cause)
	} else {
		p.logger.Warn("proxy ", member.tag, " failure ", failures, "/", threshold, ": ", cause)
	}
}

func (p *poolOutbound) recordSuccess(member *memberState) {
	p.mu.Lock()
	member.failures = 0
	p.mu.Unlock()
	if member.entry != nil {
		member.entry.RecordSuccess()
		if !member.blacklisted {
			member.entry.ClearBlacklist()
		}
	}
}

func (p *poolOutbound) wrapConn(conn net.Conn, member *memberState) net.Conn {
	return &trackedConn{Conn: conn, release: func() {
		p.decActive(member)
	}}
}

func (p *poolOutbound) wrapPacketConn(conn net.PacketConn, member *memberState) net.PacketConn {
	return &trackedPacketConn{PacketConn: conn, release: func() {
		p.decActive(member)
	}}
}

func (p *poolOutbound) makeReleaseFunc(member *memberState) func() {
	return func() {
		p.mu.Lock()
		member.blacklisted = false
		member.blacklistedUntil = time.Time{}
		member.failures = 0
		p.mu.Unlock()
		if member.entry != nil {
			member.entry.ClearBlacklist()
		}
	}
}

// httpProbe performs an HTTP probe through the connection and measures TTFB.
// It sends a minimal HTTP request and waits for the first byte of response.
func httpProbe(conn net.Conn, host string) (time.Duration, error) {
	// Build HTTP request
	req := fmt.Sprintf("GET /generate_204 HTTP/1.1\r\nHost: %s\r\nConnection: close\r\nUser-Agent: Mozilla/5.0\r\n\r\n", host)

	// Set write deadline
	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return 0, err
	}

	// Record time just before sending request
	start := time.Now()

	// Send HTTP request
	if _, err := conn.Write([]byte(req)); err != nil {
		return 0, fmt.Errorf("write request: %w", err)
	}

	// Set read deadline
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return 0, err
	}

	// Read first byte (TTFB - Time To First Byte)
	reader := bufio.NewReader(conn)
	_, err := reader.ReadByte()
	if err != nil {
		return 0, fmt.Errorf("read response: %w", err)
	}

	// Calculate TTFB
	ttfb := time.Since(start)
	return ttfb, nil
}

func (p *poolOutbound) makeProbeFunc(member *memberState) func(ctx context.Context) (time.Duration, error) {
	if p.monitor == nil {
		return nil
	}
	destination, ok := p.monitor.DestinationForProbe()
	if !ok {
		return nil
	}
	return func(ctx context.Context) (time.Duration, error) {
		start := time.Now()
		conn, err := member.outbound.DialContext(ctx, N.NetworkTCP, destination)
		if err != nil {
			if member.entry != nil {
				member.entry.RecordFailure(err)
			}
			return 0, err
		}
		defer conn.Close()

		// Perform HTTP probe to measure actual latency (TTFB)
		_, err = httpProbe(conn, destination.AddrString())
		if err != nil {
			if member.entry != nil {
				member.entry.RecordFailure(err)
			}
			return 0, err
		}

		// Total duration = dial time + HTTP probe
		duration := time.Since(start)
		if member.entry != nil {
			member.entry.RecordSuccessWithLatency(duration)
		}
		return duration, nil
	}
}

// makeProbeByTagFunc creates a probe function that works before member initialization
func (p *poolOutbound) makeProbeByTagFunc(tag string) func(ctx context.Context) (time.Duration, error) {
	if p.monitor == nil {
		return nil
	}
	destination, ok := p.monitor.DestinationForProbe()
	if !ok {
		return nil
	}
	return func(ctx context.Context) (time.Duration, error) {
		// Ensure members are initialized
		p.mu.Lock()
		if len(p.members) == 0 {
			if err := p.initializeMembersLocked(); err != nil {
				p.mu.Unlock()
				return 0, err
			}
		}

		// Find the member by tag
		var member *memberState
		for _, m := range p.members {
			if m.tag == tag {
				member = m
				break
			}
		}
		p.mu.Unlock()

		if member == nil {
			return 0, E.New("member not found: ", tag)
		}

		start := time.Now()
		conn, err := member.outbound.DialContext(ctx, N.NetworkTCP, destination)
		if err != nil {
			if member.entry != nil {
				member.entry.RecordFailure(err)
			}
			return 0, err
		}
		defer conn.Close()

		// Perform HTTP probe to measure actual latency (TTFB)
		_, err = httpProbe(conn, destination.AddrString())
		if err != nil {
			if member.entry != nil {
				member.entry.RecordFailure(err)
			}
			return 0, err
		}

		// Total duration = dial time + TTFB
		duration := time.Since(start)
		if member.entry != nil {
			member.entry.RecordSuccessWithLatency(duration)
		}
		return duration, nil
	}
}

// makeReleaseByTagFunc creates a release function that works before member initialization
func (p *poolOutbound) makeReleaseByTagFunc(tag string) func() {
	return func() {
		// Ensure members are initialized
		p.mu.Lock()
		if len(p.members) == 0 {
			if err := p.initializeMembersLocked(); err != nil {
				p.mu.Unlock()
				return
			}
		}

		// Find the member by tag
		var member *memberState
		for _, m := range p.members {
			if m.tag == tag {
				member = m
				break
			}
		}

		if member != nil {
			member.blacklisted = false
			member.blacklistedUntil = time.Time{}
			member.failures = 0
		}
		p.mu.Unlock()

		if member != nil && member.entry != nil {
			member.entry.ClearBlacklist()
		}
	}
}

type trackedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

type trackedPacketConn struct {
	net.PacketConn
	once    sync.Once
	release func()
}

func (c *trackedPacketConn) Close() error {
	err := c.PacketConn.Close()
	c.once.Do(c.release)
	return err
}

func (p *poolOutbound) incActive(member *memberState) {
	member.active.Add(1)
	if member.entry != nil {
		member.entry.IncActive()
	}
}

func (p *poolOutbound) decActive(member *memberState) {
	member.active.Add(-1)
	if member.entry != nil {
		member.entry.DecActive()
	}
}
