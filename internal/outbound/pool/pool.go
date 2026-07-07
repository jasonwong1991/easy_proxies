package pool

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"math/rand"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"easy_proxies/internal/monitor"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	singlog "github.com/sagernet/sing-box/log"
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
	modeLatency    = "latency"
)

// Options controls pool outbound behaviour.
type Options struct {
	Mode              string
	Members           []string
	FailureThreshold  int
	BlacklistDuration time.Duration
	// RetryEnabled toggles automatic fail-over on dial failure.
	RetryEnabled bool
	// RetryAttempts is the maximum total dial attempts (including the first).
	// Multi-member pools pick a different member per retry; single-member pools retry the same member.
	RetryAttempts int
	Metadata      map[string]MemberMeta
	// Sticky pins each client (by source IP) to a single member, only
	// re-selecting when the pinned member becomes unavailable. Pool/hybrid entry only.
	Sticky bool
}

// MemberMeta carries optional descriptive information for monitoring UI.
type MemberMeta struct {
	Name          string
	URI           string
	Mode          string
	ListenAddress string
	Port          uint16
	PoolEnabled   bool
	Region        string // GeoIP region code: "jp", "kr", "us", "hk", "tw", "other"
	Country       string // Full country name from GeoIP
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
	logger         singlog.ContextLogger
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
	sticky         bool
	stickyMu       sync.Mutex        // protects stickyMap
	stickyMap      map[string]string // sticky key (client source IP) -> member tag
}

func newPool(ctx context.Context, _ adapter.Router, logger singlog.ContextLogger, tag string, options Options) (adapter.Outbound, error) {
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
		sticky:  normalized.Sticky,
		candidatesPool: sync.Pool{
			New: func() any {
				return make([]*memberState, 0, memberCount)
			},
		},
	}
	if p.sticky {
		p.stickyMap = make(map[string]string)
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
				PoolEnabled:   meta.PoolEnabled,
				Region:        meta.Region,
				Country:       meta.Country,
			}
			entry := monitorMgr.Register(info)
			if entry != nil {
				// Attach entry to shared state so all pool instances share it
				state.attachEntry(entry)
				logger.Info("registered node: ", memberTag)
				// Set probe, release, and blacklist functions immediately
				entry.SetRelease(p.makeReleaseByTagFunc(memberTag))
				entry.SetBlacklistFn(p.makeBlacklistByTagFunc(memberTag))
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

	// Register this pool outbound in the dialer registry for GeoIP router
	registerDialer(tag, p)

	return p, nil
}

func normalizeOptions(options Options) Options {
	if options.FailureThreshold <= 0 {
		options.FailureThreshold = 3
	}
	if options.BlacklistDuration <= 0 {
		options.BlacklistDuration = 24 * time.Hour
	}
	if options.RetryAttempts <= 0 {
		options.RetryAttempts = 3
	}
	if options.Metadata == nil {
		options.Metadata = make(map[string]MemberMeta)
	}
	switch strings.ToLower(options.Mode) {
	case modeRandom:
		options.Mode = modeRandom
	case modeBalance:
		options.Mode = modeBalance
	case modeLatency:
		options.Mode = modeLatency
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
	// Initial health checks are driven centrally by the monitor's probeAllNodes
	// (StartPeriodicHealthCheck on boot, ProbeAllNow on reload), bounded by the
	// configured probe concurrency. A per-pool startup probe re-probed the same
	// nodes and, with one pool per node, required a process-wide semaphore to
	// avoid fd exhaustion; routing all probing through the monitor removes both.
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
				PoolEnabled:   meta.PoolEnabled,
				Region:        meta.Region,
				Country:       meta.Country,
			}
			entry := p.monitor.Register(info)
			if entry != nil {
				state.attachEntry(entry)
				member.entry = entry
				entry.SetRelease(p.makeReleaseFunc(member))
				entry.SetBlacklistFn(p.makeBlacklistByTagFunc(member.tag))
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

func (p *poolOutbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	maxAttempts := p.maxAttempts()
	stickyKey := p.stickyKeyFromCtx(ctx)
	singleMember := len(p.options.Members) <= 1
	var tried map[string]bool
	if !singleMember && maxAttempts > 1 {
		tried = make(map[string]bool, maxAttempts)
	}
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		member, err := p.pickMemberFiltered(network, tried, stickyKey)
		if err != nil {
			if lastErr != nil {
				return nil, fmt.Errorf("%w (after %d attempt(s); last: %v)", err, attempt-1, lastErr)
			}
			return nil, err
		}
		p.incActive(member)
		conn, dialErr := member.outbound.DialContext(ctx, network, destination)
		if dialErr != nil {
			p.decActive(member)
			p.recordFailure(member, dialErr)
			lastErr = dialErr
			if tried != nil {
				tried[member.tag] = true
			}
			if attempt < maxAttempts {
				p.logger.Warn("dial via ", member.tag, " failed (attempt ", attempt, "/", maxAttempts, "), retrying: ", dialErr)
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				continue
			}
			break
		}
		if attempt > 1 {
			p.logger.Info("dial succeeded via ", member.tag, " after ", attempt, " attempts")
		}
		p.recordSuccess(member)
		return p.wrapConn(conn, member), nil
	}
	if lastErr == nil {
		lastErr = E.New("no healthy proxy available")
	}
	return nil, fmt.Errorf("dial failed after %d attempts: %w", maxAttempts, lastErr)
}

func (p *poolOutbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	maxAttempts := p.maxAttempts()
	stickyKey := p.stickyKeyFromCtx(ctx)
	singleMember := len(p.options.Members) <= 1
	var tried map[string]bool
	if !singleMember && maxAttempts > 1 {
		tried = make(map[string]bool, maxAttempts)
	}
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		member, err := p.pickMemberFiltered(N.NetworkUDP, tried, stickyKey)
		if err != nil {
			if lastErr != nil {
				return nil, fmt.Errorf("%w (after %d attempt(s); last: %v)", err, attempt-1, lastErr)
			}
			return nil, err
		}
		p.incActive(member)
		conn, listenErr := member.outbound.ListenPacket(ctx, destination)
		if listenErr != nil {
			p.decActive(member)
			p.recordFailure(member, listenErr)
			lastErr = listenErr
			if tried != nil {
				tried[member.tag] = true
			}
			if attempt < maxAttempts {
				p.logger.Warn("listen-packet via ", member.tag, " failed (attempt ", attempt, "/", maxAttempts, "), retrying: ", listenErr)
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				continue
			}
			break
		}
		if attempt > 1 {
			p.logger.Info("listen-packet succeeded via ", member.tag, " after ", attempt, " attempts")
		}
		p.recordSuccess(member)
		return p.wrapPacketConn(conn, member), nil
	}
	if lastErr == nil {
		lastErr = E.New("no healthy proxy available")
	}
	return nil, fmt.Errorf("listen-packet failed after %d attempts: %w", maxAttempts, lastErr)
}

// maxAttempts returns the configured retry budget (>=1).
func (p *poolOutbound) maxAttempts() int {
	if !p.options.RetryEnabled {
		return 1
	}
	if p.options.RetryAttempts < 1 {
		return 1
	}
	return p.options.RetryAttempts
}

// pickMemberFiltered selects a healthy member, optionally excluding tags in `tried`.
// When `tried` is nil or every healthy member has been tried, falls back to picking
// any healthy member (ensuring single-member pools retry the same node).
func (p *poolOutbound) pickMemberFiltered(network string, tried map[string]bool, stickyKey string) (*memberState, error) {
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
			candidates = p.availableMembersLocked(now, network, candidates[:0])
		}
		p.mu.Unlock()
	}

	if len(candidates) == 0 {
		p.putCandidateBuffer(candidates)
		return nil, E.New("no healthy proxy available")
	}

	// Filter out members already tried in this request.
	if len(tried) > 0 {
		filtered := candidates[:0]
		for _, m := range candidates {
			if !tried[m.tag] {
				filtered = append(filtered, m)
			}
		}
		// If filtering left nothing, fall back to all candidates so we still
		// dial something (matches per-node single-member retry semantics).
		if len(filtered) > 0 {
			candidates = filtered
		}
	}

	member := p.selectMember(candidates, stickyKey)
	p.putCandidateBuffer(candidates)
	return member, nil
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

	member := p.selectMember(candidates, "")
	p.putCandidateBuffer(candidates)
	return member, nil
}

func (p *poolOutbound) availableMembersLocked(now time.Time, network string, buf []*memberState) []*memberState {
	result := buf[:0]
	for _, member := range p.members {
		// Check blacklist via shared state (auto-clears if expired)
		if member.shared != nil && member.shared.isBlacklisted(now) {
			// Log blacklisted nodes for debugging
			remaining := member.shared.blacklistRemaining(now)
			if remaining > 0 {
				p.logger.Debug("skipping blacklisted node: ", member.tag, ", remaining: ", remaining.Round(time.Second))
			}
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

const stickyFallbackKey = "_global_"

// stickyKeyFromCtx returns the sticky key (client source IP) for this request,
// or "" when stickiness is disabled. Falls back to a shared global key when the
// source address cannot be determined, so such requests still pin together.
func (p *poolOutbound) stickyKeyFromCtx(ctx context.Context) string {
	if !p.sticky {
		return ""
	}
	if md := adapter.ContextFrom(ctx); md != nil && md.Source.IsValid() {
		return md.Source.AddrString()
	}
	return stickyFallbackKey
}

// selectMember picks a member, honouring stickiness when stickyKey is non-empty.
func (p *poolOutbound) selectMember(candidates []*memberState, stickyKey string) *memberState {
	if stickyKey != "" {
		return p.selectSticky(candidates, stickyKey)
	}
	return p.selectByMode(candidates)
}

// selectSticky returns the member pinned to stickyKey if it is still among the
// candidates; otherwise it selects a fresh member (by mode) and pins it. The
// pin is permanent until the member drops out of the candidate set
// (blacklisted/removed), at which point a new member is chosen and pinned.
func (p *poolOutbound) selectSticky(candidates []*memberState, stickyKey string) *memberState {
	p.stickyMu.Lock()
	defer p.stickyMu.Unlock()
	if tag, ok := p.stickyMap[stickyKey]; ok {
		for _, member := range candidates {
			if member.tag == tag {
				return member
			}
		}
		// Pinned member is no longer available: drop the stale pin and re-select.
		delete(p.stickyMap, stickyKey)
	}
	member := p.selectByMode(candidates)
	if member != nil {
		p.stickyMap[stickyKey] = member.tag
	}
	return member
}

// selectByMode applies the configured scheduling strategy.
func (p *poolOutbound) selectByMode(candidates []*memberState) *memberState {
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
	case modeLatency:
		// Pick the candidate with the lowest measured latency.
		// Candidates without a latency reading (never probed) are deprioritized;
		// if all are unmeasured, fall back to round-robin.
		var selected *memberState
		var minLatency time.Duration
		hasMeasured := false
		for _, member := range candidates {
			if member.entry == nil {
				continue
			}
			latency := member.entry.LastLatency()
			if latency <= 0 {
				continue
			}
			if !hasMeasured || latency < minLatency {
				selected = member
				minLatency = latency
				hasMeasured = true
			}
		}
		if selected != nil {
			return selected
		}
		// Fallback: no measurements yet — round-robin.
		idx := int(p.rrCounter.Add(1)-1) % len(candidates)
		return candidates[idx]
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
	failures, blacklisted, until, transient := member.shared.recordFailure(cause, p.options.FailureThreshold, p.options.BlacklistDuration)
	switch {
	case transient:
		// Transient (e.g. 429 rate-limit): short cooldown, not counted toward the
		// 24h blacklist. The node is retried automatically once the cooldown ends.
		p.logger.Warn("proxy ", member.tag, " transient failure, cooling down until ", until.Format("15:04:05"), ": ", cause)
		log.Printf("[pool] %s transient failure, cooldown until %s: %v", member.tag, until.Format("15:04:05"), cause)
	case blacklisted:
		p.logger.Warn("proxy ", member.tag, " blacklisted for ", p.options.BlacklistDuration, ": ", cause)
		log.Printf("⚠️  [pool] %s BLACKLISTED for %s (until %s): %v", member.tag, p.options.BlacklistDuration, until.Format("15:04:05"), cause)
		log.Printf("    To release immediately, use WebUI or: POST /api/nodes/%s/release", member.tag)
	default:
		p.logger.Warn("proxy ", member.tag, " failure ", failures, "/", p.options.FailureThreshold, ": ", cause)
		log.Printf("[pool] %s failure %d/%d: %v", member.tag, failures, p.options.FailureThreshold, cause)
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

// upgradeProbeConn optionally upgrades a plain TCP connection to TLS with
// strict certificate verification. When useTLS is false the connection is
// returned unchanged. host is used as the TLS SNI. A handshake failure
// (e.g. self-signed or hijacked certificate) is returned as an error so the
// caller can record it and eventually blacklist the node.
func upgradeProbeConn(ctx context.Context, conn net.Conn, host string, useTLS bool) (net.Conn, error) {
	if !useTLS {
		return conn, nil
	}
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName:         host,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: false,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		// Return the original connection so the caller's deferred Close keeps
		// working; the failed handshake did not take over its lifetime.
		return conn, fmt.Errorf("tls handshake: %w", err)
	}
	return tlsConn, nil
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

// probeMember dials the probe destination through a member and measures latency.
// A context watchdog force-closes the underlying connection when ctx is cancelled
// or its deadline fires. This is essential: some sing-box outbound connection
// types do not honor SetDeadline, so without the watchdog a probe against a node
// that accepts TCP but never sends an HTTP response would block on Read / the TLS
// handshake forever, leaking goroutines and hanging the whole batch probe
// (wg.Wait would never return, freezing the WebUI at "N/M").
func (p *poolOutbound) probeMember(ctx context.Context, member *memberState, destination M.Socksaddr, host string, useTLS bool) (time.Duration, error) {
	start := time.Now()
	rawConn, err := member.outbound.DialContext(ctx, N.NetworkTCP, destination)
	if err != nil {
		if member.entry != nil {
			member.entry.RecordFailure(err)
		}
		return 0, err
	}
	// Watchdog: close the raw connection as soon as ctx is done so any blocked
	// Read/Write/handshake below is unblocked even when SetDeadline is a no-op.
	probeDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = rawConn.Close()
		case <-probeDone:
		}
	}()
	defer close(probeDone)
	defer rawConn.Close()

	conn := rawConn
	// Strict mode: upgrade to TLS and verify the certificate chain so that
	// nodes whose exit hijacks TLS (self-signed certs) fail the probe.
	if conn, err = upgradeProbeConn(ctx, conn, host, useTLS); err != nil {
		if member.entry != nil {
			member.entry.RecordFailure(err)
		}
		return 0, err
	}

	// Perform HTTP probe to measure actual latency (TTFB).
	if _, err = httpProbe(conn, destination.AddrString()); err != nil {
		if member.entry != nil {
			member.entry.RecordFailure(err)
		}
		return 0, err
	}

	duration := time.Since(start)
	if member.entry != nil {
		member.entry.RecordSuccessWithLatency(duration)
	}
	// Clear pool blacklist on successful probe — a node that passes health check
	// should be available for selection immediately (fixes #8, #9).
	if member.shared != nil {
		member.shared.forceRelease()
	}
	return duration, nil
}

func (p *poolOutbound) makeProbeFunc(member *memberState) func(ctx context.Context) (time.Duration, error) {
	if p.monitor == nil {
		return nil
	}
	destination, host, useTLS, ok := p.monitor.DestinationForProbe()
	if !ok {
		return nil
	}
	return func(ctx context.Context) (time.Duration, error) {
		return p.probeMember(ctx, member, destination, host, useTLS)
	}
}

// makeProbeByTagFunc creates a probe function that works before member initialization
func (p *poolOutbound) makeProbeByTagFunc(tag string) func(ctx context.Context) (time.Duration, error) {
	if p.monitor == nil {
		return nil
	}
	destination, host, useTLS, ok := p.monitor.DestinationForProbe()
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
		return p.probeMember(ctx, member, destination, host, useTLS)
	}
}

// makeReleaseByTagFunc creates a release function that works before member initialization
func (p *poolOutbound) makeReleaseByTagFunc(tag string) func() {
	return func() {
		releaseSharedMember(tag)
	}
}

// makeBlacklistByTagFunc creates a blacklist function for manual ban via API
func (p *poolOutbound) makeBlacklistByTagFunc(tag string) func(time.Duration) {
	return func(duration time.Duration) {
		blacklistSharedMember(tag, duration)
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
