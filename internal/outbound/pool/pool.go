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
	"crypto/tls"

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

	// defaultDialAttempts controls how many different candidates we try in one Dial/ListenPacket
	// before returning error to client.
	defaultDialAttempts = 3
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
	entry    *monitor.EntryHandle
	shared   *sharedMemberState
}

type poolOutbound struct {
	outbound.Adapter
	ctx            context.Context
	logger         log.ContextLogger
	manager        adapter.OutboundManager
	options        Options
	mode           string
	members        []*memberState
	mu             sync.Mutex
	rrCounter      atomic.Uint32
	rng            *rand.Rand
	rngMu          sync.Mutex // protects rng for random mode
	monitor        *monitor.Manager
	candidatesPool sync.Pool
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
	memberCount := len(normalized.Members)
	p := &poolOutbound{
		Adapter: outbound.NewAdapter(Type, tag, []string{N.NetworkTCP, N.NetworkUDP}, normalized.Members),
		ctx:     ctx,
		logger:  logger,
		manager: manager,
		options: normalized,
		mode:    normalized.Mode,
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
		monitor: monitorMgr,
		candidatesPool: sync.Pool{
			New: func() any {
				return make([]*memberState, 0, memberCount)
			},
		},
	}

	// Register nodes immediately if monitor is available
	if monitorMgr != nil {
		logger.Info("registering ", len(normalized.Members), " nodes to monitor")
		for _, memberTag := range normalized.Members {
			// Acquire shared state for this tag (creates if not exists)
			state := acquireSharedState(memberTag)

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
				// Attach entry to shared state so all pool instances share it
				state.attachEntry(entry)
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
	// 不在这里做全量逐个测活（节点多时会很慢且重复）
	// 健康检查由 monitor.Manager 的 periodic health check 统一执行
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

		// Acquire shared state (creates if not exists, reuses if already created)
		state := acquireSharedState(tag)

		member := &memberState{
			outbound: detour,
			tag:      tag,
			shared:   state,
			entry:    state.entryHandle(),
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
				state.attachEntry(entry)
				member.entry = entry
				entry.SetRelease(p.makeReleaseFunc(member))
				if probe := p.makeProbeFunc(member); probe != nil {
					entry.SetProbe(probe)
				}
			}
		}
		members = append(members, member)
	}
	p.members = members
	p.logger.Info("pool initialized with ", len(members), " members")

	return nil
}

// probeAllMembersOnStartup performs initial health checks on all members
// NOTE: 已不再从 Start() 调用，保留代码避免破坏现有逻辑引用。
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

	for _, member := range members {
		// Create a timeout context for each probe
		ctx, cancel := context.WithTimeout(p.ctx, 15*time.Second)

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
	candidates, err := p.pickCandidates(network)
	if err != nil {
		return nil, err
	}
	defer p.putCandidateBuffer(candidates)

	conn, member, err := p.dialCandidates(ctx, network, destination, candidates)
	if err != nil {
		return nil, err
	}
	return p.wrapConn(conn, member), nil
}

func (p *poolOutbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	candidates, err := p.pickCandidates(N.NetworkUDP)
	if err != nil {
		return nil, err
	}
	defer p.putCandidateBuffer(candidates)

	conn, member, err := p.listenPacketCandidates(ctx, destination, candidates)
	if err != nil {
		return nil, err
	}
	return p.wrapPacketConn(conn, member), nil
}

// pickCandidates returns a candidate list with:
// 1) not blacklisted
// 2) supports requested network
// 3) if node has health-check result and it is unavailable -> skip
// If everything is excluded by health-check, it falls back to ignore health-check to avoid total outage.
func (p *poolOutbound) pickCandidates(network string) ([]*memberState, error) {
	now := time.Now()
	candidates := p.getCandidateBuffer()

	p.mu.Lock()
	if len(p.members) == 0 {
		if err := p.initializeMembersLocked(); err != nil {
			p.mu.Unlock()
			p.putCandidateBuffer(candidates)
			return nil, err
		}
	}

	candidates = p.availableMembersLocked(now, network, candidates)
	if len(candidates) == 0 {
		// fall back: ignore health result, still respect blacklist/network
		candidates = p.availableMembersIgnoringHealthLocked(now, network, candidates)
	}

	if len(candidates) == 0 && p.releaseIfAllBlacklistedLocked(now) {
		candidates = p.availableMembersLocked(now, network, candidates)
		if len(candidates) == 0 {
			candidates = p.availableMembersIgnoringHealthLocked(now, network, candidates)
		}
	}
	p.mu.Unlock()

	if len(candidates) == 0 {
		p.putCandidateBuffer(candidates)
		return nil, E.New("no healthy proxy available")
	}
	return candidates, nil
}

func (p *poolOutbound) dialCandidates(ctx context.Context, network string, destination M.Socksaddr, candidates []*memberState) (net.Conn, *memberState, error) {
	if len(candidates) == 0 {
		return nil, nil, E.New("no healthy proxy available")
	}

	attempts := defaultDialAttempts
	if attempts > len(candidates) {
		attempts = len(candidates)
	}
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error

	switch p.mode {
	case modeRandom:
		p.rngMu.Lock()
		p.rng.Shuffle(len(candidates), func(i, j int) {
			candidates[i], candidates[j] = candidates[j], candidates[i]
		})
		p.rngMu.Unlock()

		for i := 0; i < attempts; i++ {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			member := candidates[i]
			p.incActive(member)
			conn, err := member.outbound.DialContext(ctx, network, destination)
			if err != nil {
				p.decActive(member)
				p.recordFailure(member, err)
				lastErr = err
				continue
			}
			p.recordSuccess(member)
			return conn, member, nil
		}

	case modeBalance:
		remaining := candidates
		for i := 0; i < attempts && len(remaining) > 0; i++ {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}

			idx := leastActiveIndex(remaining)
			member := remaining[idx]

			p.incActive(member)
			conn, err := member.outbound.DialContext(ctx, network, destination)
			if err != nil {
				p.decActive(member)
				p.recordFailure(member, err)
				lastErr = err

				// remove member from remaining
				last := len(remaining) - 1
				remaining[idx] = remaining[last]
				remaining = remaining[:last]
				continue
			}
			p.recordSuccess(member)
			return conn, member, nil
		}

	default: // sequential
		start := int(p.rrCounter.Add(1)-1) % len(candidates)
		for i := 0; i < attempts; i++ {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			member := candidates[(start+i)%len(candidates)]
			p.incActive(member)
			conn, err := member.outbound.DialContext(ctx, network, destination)
			if err != nil {
				p.decActive(member)
				p.recordFailure(member, err)
				lastErr = err
				continue
			}
			p.recordSuccess(member)
			return conn, member, nil
		}
	}

	if lastErr == nil {
		lastErr = E.New("no healthy proxy available")
	}
	return nil, nil, E.New("dial failed after ", attempts, " attempts: ", lastErr)
}

func (p *poolOutbound) listenPacketCandidates(ctx context.Context, destination M.Socksaddr, candidates []*memberState) (net.PacketConn, *memberState, error) {
	if len(candidates) == 0 {
		return nil, nil, E.New("no healthy proxy available")
	}

	attempts := defaultDialAttempts
	if attempts > len(candidates) {
		attempts = len(candidates)
	}
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error

	switch p.mode {
	case modeRandom:
		p.rngMu.Lock()
		p.rng.Shuffle(len(candidates), func(i, j int) {
			candidates[i], candidates[j] = candidates[j], candidates[i]
		})
		p.rngMu.Unlock()

		for i := 0; i < attempts; i++ {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			member := candidates[i]
			p.incActive(member)
			conn, err := member.outbound.ListenPacket(ctx, destination)
			if err != nil {
				p.decActive(member)
				p.recordFailure(member, err)
				lastErr = err
				continue
			}
			p.recordSuccess(member)
			return conn, member, nil
		}

	case modeBalance:
		remaining := candidates
		for i := 0; i < attempts && len(remaining) > 0; i++ {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}

			idx := leastActiveIndex(remaining)
			member := remaining[idx]

			p.incActive(member)
			conn, err := member.outbound.ListenPacket(ctx, destination)
			if err != nil {
				p.decActive(member)
				p.recordFailure(member, err)
				lastErr = err

				last := len(remaining) - 1
				remaining[idx] = remaining[last]
				remaining = remaining[:last]
				continue
			}
			p.recordSuccess(member)
			return conn, member, nil
		}

	default: // sequential
		start := int(p.rrCounter.Add(1)-1) % len(candidates)
		for i := 0; i < attempts; i++ {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			member := candidates[(start+i)%len(candidates)]
			p.incActive(member)
			conn, err := member.outbound.ListenPacket(ctx, destination)
			if err != nil {
				p.decActive(member)
				p.recordFailure(member, err)
				lastErr = err
				continue
			}
			p.recordSuccess(member)
			return conn, member, nil
		}
	}

	if lastErr == nil {
		lastErr = E.New("no healthy proxy available")
	}
	return nil, nil, E.New("listenPacket failed after ", attempts, " attempts: ", lastErr)
}

func leastActiveIndex(candidates []*memberState) int {
	if len(candidates) <= 1 {
		return 0
	}
	minIdx := 0
	minActive := int32(0)
	if candidates[0].shared != nil {
		minActive = candidates[0].shared.activeCount()
	}
	for i := 1; i < len(candidates); i++ {
		active := int32(0)
		if candidates[i].shared != nil {
			active = candidates[i].shared.activeCount()
		}
		if active < minActive {
			minActive = active
			minIdx = i
		}
	}
	return minIdx
}

func (p *poolOutbound) pickMember(network string) (*memberState, error) {
	now := time.Now()
	candidates := p.getCandidateBuffer()

	p.mu.Lock()
	if len(p.members) == 0 {
		if err := p.initializeMembersLocked(); err != nil {
			p.mu.Unlock()
			p.putCandidateBuffer(candidates)
			return nil, err
		}
	}
	candidates = p.availableMembersLocked(now, network, candidates)
	p.mu.Unlock()

	if len(candidates) == 0 {
		p.mu.Lock()
		if p.releaseIfAllBlacklistedLocked(now) {
			candidates = p.availableMembersLocked(now, network, candidates)
		}
		p.mu.Unlock()
	}

	if len(candidates) == 0 {
		p.putCandidateBuffer(candidates)
		return nil, E.New("no healthy proxy available")
	}

	member := p.selectMember(candidates)
	p.putCandidateBuffer(candidates)
	return member, nil
}

func (p *poolOutbound) availableMembersLocked(now time.Time, network string, buf []*memberState) []*memberState {
	result := buf[:0]
	for _, member := range p.members {
		// Check blacklist via shared state (auto-clears if expired)
		if member.shared != nil && member.shared.isBlacklisted(now) {
			continue
		}
		if network != "" && !common.Contains(member.outbound.Network(), network) {
			continue
		}

		// 如果健康检查已完成且明确不可用，则跳过
		if member.entry != nil {
			done, available := member.entry.Health()
			if done && !available {
				continue
			}
		}

		result = append(result, member)
	}
	return result
}

func (p *poolOutbound) availableMembersIgnoringHealthLocked(now time.Time, network string, buf []*memberState) []*memberState {
	result := buf[:0]
	for _, member := range p.members {
		if member.shared != nil && member.shared.isBlacklisted(now) {
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
	// Check if all members are blacklisted
	for _, member := range p.members {
		if member.shared == nil || !member.shared.isBlacklisted(now) {
			return false
		}
	}
	// All blacklisted, force release all
	for _, member := range p.members {
		if member.shared != nil {
			member.shared.forceRelease()
		}
	}
	p.logger.Warn("all upstream proxies were blacklisted, releasing them for retry")
	return true
}

func (p *poolOutbound) selectMember(candidates []*memberState) *memberState {
	switch p.mode {
	case modeRandom:
		p.rngMu.Lock()
		idx := p.rng.Intn(len(candidates))
		p.rngMu.Unlock()
		return candidates[idx]
	case modeBalance:
		var selected *memberState
		var minActive int32
		for _, member := range candidates {
			var active int32
			if member.shared != nil {
				active = member.shared.activeCount()
			}
			if selected == nil || active < minActive {
				selected = member
				minActive = active
			}
		}
		return selected
	default:
		idx := int(p.rrCounter.Add(1)-1) % len(candidates)
		return candidates[idx]
	}
}

func (p *poolOutbound) recordFailure(member *memberState, cause error) {
	if member.shared == nil {
		p.logger.Warn("proxy ", member.tag, " failure (no shared state): ", cause)
		return
	}
	failures, blacklisted, _ := member.shared.recordFailure(cause, p.options.FailureThreshold, p.options.BlacklistDuration)
	if blacklisted {
		p.logger.Warn("proxy ", member.tag, " blacklisted for ", p.options.BlacklistDuration, ": ", cause)
	} else {
		p.logger.Warn("proxy ", member.tag, " failure ", failures, "/", p.options.FailureThreshold, ": ", cause)
	}
}

func (p *poolOutbound) recordSuccess(member *memberState) {
	if member.shared != nil {
		member.shared.recordSuccess()
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
		if member.shared != nil {
			member.shared.forceRelease()
		}
	}
}

// httpProbe performs an HTTP probe through the connection and measures TTFB.
// It sends a minimal HTTP request and waits for the first byte of response.
func httpProbe(conn net.Conn, host string) (time.Duration, error) {
	// Build HTTP request
	req := fmt.Sprintf("GET /generate_204 HTTP/1.1\r\nHost: %s\r\nConnection: close\r\nUser-Agent: Mozilla/5.0\r\n\r\n", host)

	// Try to set write deadline (ignore errors for connections that don't support it)
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))

	// Record time just before sending request
	start := time.Now()

	// Send HTTP request
	if _, err := conn.Write([]byte(req)); err != nil {
		return 0, fmt.Errorf("write request: %w", err)
	}

	// Try to set read deadline (ignore errors for connections that don't support it)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

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

func httpsProbe(ctx context.Context, conn net.Conn, serverName, hostHeader string) (time.Duration, error) {
	tlsCfg := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"http/1.1"},
	}
	if serverName != "" {
		tlsCfg.ServerName = serverName
	}

	tlsConn := tls.Client(conn, tlsCfg)

	// Best-effort deadline for handshake
	if deadline, ok := ctx.Deadline(); ok {
		_ = tlsConn.SetDeadline(deadline)
	} else {
		_ = tlsConn.SetDeadline(time.Now().Add(15 * time.Second))
	}

	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return 0, fmt.Errorf("tls handshake: %w", err)
	}

	// After TLS handshake, do HTTP probe over TLS.
	return httpProbe(tlsConn, hostHeader)
}

func (p *poolOutbound) makeProbeFunc(member *memberState) func(ctx context.Context) (time.Duration, error) {
	if p.monitor == nil {
		return nil
	}
	return func(ctx context.Context) (time.Duration, error) {
		destination, hostHeader, useTLS, serverName, ok := p.monitor.ProbeTargetInfo()
		if !ok {
			return 0, E.New("probe target not configured")
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

		if useTLS {
			_, err = httpsProbe(ctx, conn, serverName, hostHeader)
		} else {
			_, err = httpProbe(conn, hostHeader)
		}
		if err != nil {
			if member.entry != nil {
				member.entry.RecordFailure(err)
			}
			return 0, err
		}

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
	return func(ctx context.Context) (time.Duration, error) {
		destination, hostHeader, useTLS, serverName, ok := p.monitor.ProbeTargetInfo()
		if !ok {
			return 0, E.New("probe target not configured")
		}

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

		if useTLS {
			_, err = httpsProbe(ctx, conn, serverName, hostHeader)
		} else {
			_, err = httpProbe(conn, hostHeader)
		}
		if err != nil {
			if member.entry != nil {
				member.entry.RecordFailure(err)
			}
			return 0, err
		}

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
		releaseSharedMember(tag)
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
	if member.shared != nil {
		member.shared.incActive()
	}
}

func (p *poolOutbound) decActive(member *memberState) {
	if member.shared != nil {
		member.shared.decActive()
	}
}

func (p *poolOutbound) getCandidateBuffer() []*memberState {
	if buf := p.candidatesPool.Get(); buf != nil {
		return buf.([]*memberState)
	}
	return make([]*memberState, 0, len(p.options.Members))
}

func (p *poolOutbound) putCandidateBuffer(buf []*memberState) {
	if buf == nil {
		return
	}
	const maxCached = 4096
	if cap(buf) > maxCached {
		return
	}
	p.candidatesPool.Put(buf[:0])
}