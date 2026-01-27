package monitor

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"runtime"

	"easy_proxies/internal/config"
)

//go:embed assets/index.html
var embeddedFS embed.FS

// NodeManager exposes config node CRUD and reload operations.
type NodeManager interface {
	ListConfigNodes(ctx context.Context) ([]config.NodeConfig, error)
	CreateNode(ctx context.Context, node config.NodeConfig) (config.NodeConfig, error)
	UpdateNode(ctx context.Context, name string, node config.NodeConfig) (config.NodeConfig, error)
	DeleteNode(ctx context.Context, name string) error
	TriggerReload(ctx context.Context) error

	// Settings (thread-safe; implemented by boxmgr.Manager)
	GetSettings(ctx context.Context) (externalIP string, probeTarget string, skipCertVerify bool, err error)
	UpdateSettings(ctx context.Context, externalIP, probeTarget string, skipCertVerify bool) error
}

// Sentinel errors for node operations.
var (
	ErrNodeNotFound = errors.New("节点不存在")
	ErrNodeConflict = errors.New("节点名称或端口已存在")
	ErrInvalidNode  = errors.New("无效的节点配置")
)

// SubscriptionRefresher interface for subscription manager.
type SubscriptionRefresher interface {
	RefreshNow() error
	Status() SubscriptionStatus
}

// SubscriptionStatus represents subscription refresh status.
type SubscriptionStatus struct {
	LastRefresh   time.Time `json:"last_refresh"`
	NextRefresh   time.Time `json:"next_refresh"`
	NodeCount     int       `json:"node_count"`
	LastError     string    `json:"last_error,omitempty"`
	RefreshCount  int       `json:"refresh_count"`
	IsRefreshing  bool      `json:"is_refreshing"`
	NodesModified bool      `json:"nodes_modified"` 
}

// Server exposes HTTP endpoints for monitoring.
type Server struct {
	cfg          Config
	cfgMu        sync.RWMutex   // 保护动态配置字段
	cfgSrc       *config.Config // 可持久化的配置对象
	mgr          *Manager
	srv          *http.Server
	logger       *log.Logger
	sessionToken string // 简单的 session token，重启后失效
	subRefresher SubscriptionRefresher
	nodeMgr      NodeManager
}

// NewServer constructs a server; it can be nil when disabled.
func NewServer(cfg Config, mgr *Manager, logger *log.Logger) *Server {
	if !cfg.Enabled || mgr == nil {
		return nil
	}
	if logger == nil {
		logger = log.Default()
	}
	s := &Server{cfg: cfg, mgr: mgr, logger: logger}

	// 生成随机 session token
	tokenBytes := make([]byte, 32)
	rand.Read(tokenBytes)
	s.sessionToken = hex.EncodeToString(tokenBytes)

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/auth", s.handleAuth)
	mux.HandleFunc("/api/settings", s.withAuth(s.handleSettings))
	mux.HandleFunc("/api/nodes", s.withAuth(s.handleNodes))
	mux.HandleFunc("/api/nodes/config", s.withAuth(s.handleConfigNodes))
	mux.HandleFunc("/api/nodes/config/", s.withAuth(s.handleConfigNodeItem))
	mux.HandleFunc("/api/nodes/probe-all", s.withAuth(s.handleProbeAll))
	mux.HandleFunc("/api/nodes/", s.withAuth(s.handleNodeAction))
	mux.HandleFunc("/api/debug", s.withAuth(s.handleDebug))
	mux.HandleFunc("/api/export", s.withAuth(s.handleExport))
	mux.HandleFunc("/api/subscription/status", s.withAuth(s.handleSubscriptionStatus))
	mux.HandleFunc("/api/subscription/refresh", s.withAuth(s.handleSubscriptionRefresh))
	mux.HandleFunc("/api/reload", s.withAuth(s.handleReload))
	s.srv = &http.Server{Addr: cfg.Listen, Handler: mux}
	return s
}

// SetSubscriptionRefresher sets the subscription refresher for API endpoints.
func (s *Server) SetSubscriptionRefresher(sr SubscriptionRefresher) {
	if s != nil {
		s.subRefresher = sr
	}
}

// SetNodeManager enables config-node CRUD endpoints.
func (s *Server) SetNodeManager(nm NodeManager) {
	if s != nil {
		s.nodeMgr = nm
	}
}

// SetConfig binds the persistable config object for settings API.
func (s *Server) SetConfig(cfg *config.Config) {
	if s == nil {
		return
	}
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	s.cfgSrc = cfg
	if cfg != nil {
		s.cfg.ExternalIP = cfg.ExternalIP
		s.cfg.ProbeTarget = cfg.Management.ProbeTarget
		s.cfg.SkipCertVerify = cfg.SkipCertVerify
	}
}

// getSettings returns current dynamic settings (thread-safe).
func (s *Server) getSettings() (externalIP, probeTarget string, skipCertVerify bool) {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.ExternalIP, s.cfg.ProbeTarget, s.cfg.SkipCertVerify
}

// updateSettings updates dynamic settings and persists to config file.
func (s *Server) updateSettings(externalIP, probeTarget string, skipCertVerify bool) error {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()

	prevExternalIP := s.cfg.ExternalIP
	prevProbeTarget := s.cfg.ProbeTarget
	prevSkip := s.cfg.SkipCertVerify

	var prevCfgSrcExternalIP string
	var prevCfgSrcProbeTarget string
	var prevCfgSrcSkip bool
	if s.cfgSrc != nil {
		prevCfgSrcExternalIP = s.cfgSrc.ExternalIP
		prevCfgSrcProbeTarget = s.cfgSrc.Management.ProbeTarget
		prevCfgSrcSkip = s.cfgSrc.SkipCertVerify
	}

	// Update in-memory server config first
	s.cfg.ExternalIP = externalIP
	s.cfg.ProbeTarget = probeTarget
	s.cfg.SkipCertVerify = skipCertVerify

	if s.cfgSrc == nil {
		// rollback server cfg
		s.cfg.ExternalIP = prevExternalIP
		s.cfg.ProbeTarget = prevProbeTarget
		s.cfg.SkipCertVerify = prevSkip
		return errors.New("配置存储未初始化")
	}

	// Update persistable config object
	s.cfgSrc.ExternalIP = externalIP
	s.cfgSrc.Management.ProbeTarget = probeTarget
	s.cfgSrc.SkipCertVerify = skipCertVerify

	// Apply probe target to runtime monitor manager (also validates probeTarget)
	if s.mgr != nil {
		if err := s.mgr.UpdateProbeTarget(probeTarget); err != nil {
			// rollback server cfg
			s.cfg.ExternalIP = prevExternalIP
			s.cfg.ProbeTarget = prevProbeTarget
			s.cfg.SkipCertVerify = prevSkip

			// rollback cfgSrc
			s.cfgSrc.ExternalIP = prevCfgSrcExternalIP
			s.cfgSrc.Management.ProbeTarget = prevCfgSrcProbeTarget
			s.cfgSrc.SkipCertVerify = prevCfgSrcSkip

			return err
		}
	}

	if err := s.cfgSrc.SaveSettings(); err != nil {
		// rollback runtime manager (best-effort)
		if s.mgr != nil {
			_ = s.mgr.UpdateProbeTarget(prevProbeTarget)
		}

		// rollback server cfg
		s.cfg.ExternalIP = prevExternalIP
		s.cfg.ProbeTarget = prevProbeTarget
		s.cfg.SkipCertVerify = prevSkip

		// rollback cfgSrc
		s.cfgSrc.ExternalIP = prevCfgSrcExternalIP
		s.cfgSrc.Management.ProbeTarget = prevCfgSrcProbeTarget
		s.cfgSrc.SkipCertVerify = prevCfgSrcSkip

		return fmt.Errorf("保存配置失败: %w", err)
	}

	return nil
}

// Start launches the HTTP server.
func (s *Server) Start(ctx context.Context) {
	if s == nil || s.srv == nil {
		return
	}
	s.logger.Printf("Starting monitor server on %s", s.cfg.Listen)
	go func() {
		if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Printf("❌ Monitor server error: %v", err)
		}
	}()
	// Give server a moment to start and check for immediate errors
	time.Sleep(100 * time.Millisecond)
	s.logger.Printf("✅ Monitor server started on http://%s", s.cfg.Listen)

	go func() {
		<-ctx.Done()
		s.Shutdown(context.Background())
	}()
}

// Shutdown stops the server gracefully.
func (s *Server) Shutdown(ctx context.Context) {
	if s == nil || s.srv == nil {
		return
	}
	_ = s.srv.Shutdown(ctx)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	data, err := embeddedFS.ReadFile("assets/index.html")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// 只返回监控页需要的字段，避免 timeline/uri 等额外数据导致前端解析与渲染变慢
	type nodeListItem struct {
		Tag              string    `json:"tag"`
		Name             string    `json:"name"`
		Mode             string    `json:"mode"`
		Port             uint16    `json:"port,omitempty"`
		FailureCount     int       `json:"failure_count"`
		Blacklisted      bool      `json:"blacklisted"`
		BlacklistedUntil time.Time `json:"blacklisted_until"`
		ActiveConnections int32    `json:"active_connections"`
		LastError        string    `json:"last_error,omitempty"`
		LastFailure      time.Time `json:"last_failure,omitempty"`
		LastSuccess      time.Time `json:"last_success,omitempty"`
		LastLatencyMs    int64     `json:"last_latency_ms"`
	}

	snapshots := s.mgr.SnapshotFiltered(true)
	nodes := make([]nodeListItem, 0, len(snapshots))
	for _, snap := range snapshots {
		nodes = append(nodes, nodeListItem{
			Tag:               snap.Tag,
			Name:              snap.Name,
			Mode:              snap.Mode,
			Port:              snap.Port,
			FailureCount:      snap.FailureCount,
			Blacklisted:       snap.Blacklisted,
			BlacklistedUntil:  snap.BlacklistedUntil,
			ActiveConnections: snap.ActiveConnections,
			LastError:         snap.LastError,
			LastFailure:       snap.LastFailure,
			LastSuccess:       snap.LastSuccess,
			LastLatencyMs:     snap.LastLatencyMs,
		})
	}

	writeJSON(w, map[string]any{"nodes": nodes})
}

func (s *Server) handleDebug(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	snapshots := s.mgr.Snapshot()
	var totalCalls, totalSuccess int64
	debugNodes := make([]map[string]any, 0, len(snapshots))
	for _, snap := range snapshots {
		totalCalls += snap.SuccessCount + int64(snap.FailureCount)
		totalSuccess += snap.SuccessCount
		debugNodes = append(debugNodes, map[string]any{
			"tag":                snap.Tag,
			"name":               snap.Name,
			"mode":               snap.Mode,
			"port":               snap.Port,
			"failure_count":      snap.FailureCount,
			"success_count":      snap.SuccessCount,
			"active_connections": snap.ActiveConnections,
			"last_latency_ms":    snap.LastLatencyMs,
			"last_success":       snap.LastSuccess,
			"last_failure":       snap.LastFailure,
			"last_error":         snap.LastError,
			"blacklisted":        snap.Blacklisted,
			"timeline":           snap.Timeline,
		})
	}
	var successRate float64
	if totalCalls > 0 {
		successRate = float64(totalSuccess) / float64(totalCalls) * 100
	}
	writeJSON(w, map[string]any{
		"nodes":         debugNodes,
		"total_calls":   totalCalls,
		"total_success": totalSuccess,
		"success_rate":  successRate,
	})
}

func (s *Server) handleNodeAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/nodes/"), "/")
	if len(parts) < 1 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	tag := parts[0]
	if tag == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	switch action {
	case "probe":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		latency, err := s.mgr.Probe(ctx, tag)
		if err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		latencyMs := latency.Milliseconds()
		if latencyMs == 0 && latency > 0 {
			latencyMs = 1 // Round up sub-millisecond latencies to 1ms
		}
		writeJSON(w, map[string]any{"message": "探测成功", "latency_ms": latencyMs})
	case "release":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := s.mgr.Release(tag); err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"message": "已解除拉黑"})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// handleProbeAll probes all nodes with bounded concurrency and returns results via SSE.
func (s *Server) handleProbeAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	snapshots := s.mgr.Snapshot()
	total := len(snapshots)
	if total == 0 {
		_ = writeSSE(w, map[string]any{"type": "complete", "total": 0, "success": 0, "failed": 0})
		flusher.Flush()
		return
	}

	if err := writeSSE(w, map[string]any{"type": "start", "total": total}); err != nil {
		return
	}
	flusher.Flush()

	const perProbeTimeout = 10 * time.Second

	workerLimit := runtime.NumCPU() * 2
	if workerLimit < 8 {
		workerLimit = 8
	}
	if workerLimit > 32 {
		workerLimit = 32
	}
	if workerLimit > total {
		workerLimit = total
	}

	// Overall timeout: enough for worst-case (each probe uses full perProbeTimeout)
	rounds := (total + workerLimit - 1) / workerLimit
	overallTimeout := time.Duration(rounds)*perProbeTimeout + 5*time.Second

	ctx, cancel := context.WithTimeout(r.Context(), overallTimeout)
	defer cancel()

	type probeResult struct {
		Tag     string
		Name    string
		Latency int64
		Err     string
	}

	jobs := make(chan Snapshot)
	results := make(chan probeResult, workerLimit)

	var wg sync.WaitGroup
	for i := 0; i < workerLimit; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for snap := range jobs {
				if ctx.Err() != nil {
					return
				}

				pctx, pcancel := context.WithTimeout(ctx, perProbeTimeout)
				latency, err := s.mgr.Probe(pctx, snap.Tag)
				pcancel()

				res := probeResult{
					Tag:     snap.Tag,
					Name:    snap.Name,
					Latency: -1,
				}
				if err != nil {
					res.Err = err.Error()
				} else {
					ms := latency.Milliseconds()
					if ms == 0 && latency > 0 {
						ms = 1
					}
					res.Latency = ms
				}

				select {
				case results <- res:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, snap := range snapshots {
			select {
			case jobs <- snap:
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	successCount := 0
	failedCount := 0
	current := 0

	for {
		select {
		case <-ctx.Done():
			remaining := total - current
			if remaining > 0 {
				failedCount += remaining
				current = total
			}
			goto complete
		case res, ok := <-results:
			if !ok {
				goto complete
			}

			current++
			if res.Err != "" {
				failedCount++
			} else {
				successCount++
			}

			progress := float64(current) / float64(total) * 100
			event := map[string]any{
				"type":     "progress",
				"tag":      res.Tag,
				"name":     res.Name,
				"latency":  res.Latency,
				"error":    res.Err,
				"current":  current,
				"total":    total,
				"progress": progress,
			}
			if err := writeSSE(w, event); err != nil {
				return
			}
			flusher.Flush()
		}
	}

complete:
	if r.Context().Err() != nil {
		return
	}
	_ = writeSSE(w, map[string]any{
		"type":    "complete",
		"total":   total,
		"success": successCount,
		"failed":  failedCount,
	})
	flusher.Flush()
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	_ = enc.Encode(payload)
}

func writeSSE(w http.ResponseWriter, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}

// withAuth 认证中间件，如果配置了密码则需要验证
func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 如果没有配置密码，直接放行
		if s.cfg.Password == "" {
			next(w, r)
			return
		}

		// 检查 Cookie 中的 session token
		cookie, err := r.Cookie("session_token")
		if err == nil && cookie.Value == s.sessionToken {
			next(w, r)
			return
		}

		// 检查 Authorization header (Bearer token)
		authHeader := r.Header.Get("Authorization")
		if authHeader != "" {
			token := strings.TrimPrefix(authHeader, "Bearer ")
			if token == s.sessionToken {
				next(w, r)
				return
			}
		}

		// 未授权
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"error": "未授权，请先登录"})
	}
}

// handleAuth 处理登录认证
func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	// 如果没有配置密码，直接返回成功（不需要token）
	if s.cfg.Password == "" {
		writeJSON(w, map[string]any{"message": "无需密码", "no_password": true})
		return
	}

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Password string `json:"password"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "请求格式错误"})
		return
	}

	// 验证密码
	if req.Password != s.cfg.Password {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"error": "密码错误"})
		return
	}

	// 设置 cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    s.sessionToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   86400 * 7, // 7天
	})

	writeJSON(w, map[string]any{
		"message": "登录成功",
		"token":   s.sessionToken,
	})
}

// handleExport 导出所有可用代理池节点的 HTTP 代理 URI，每行一个
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// Export ONLY nodes that are available in the latest health-check result.
	// Also skip blacklisted nodes.
	snapshots := s.mgr.Snapshot()

	// Prefer reading external_ip from the node manager (source of truth).
	extIP := ""
	if s.nodeMgr != nil {
		type settingsGetter interface {
			GetSettings(ctx context.Context) (externalIP string, probeTarget string, skipCertVerify bool, err error)
		}
		if sg, ok := s.nodeMgr.(settingsGetter); ok {
			if v, _, _, err := sg.GetSettings(r.Context()); err == nil {
				extIP = v
			}
		}
	}
	// Fallback to server cached config.
	if extIP == "" {
		s.cfgMu.RLock()
		extIP = s.cfg.ExternalIP
		s.cfgMu.RUnlock()
	}

	seen := make(map[string]struct{}, len(snapshots))
	lines := make([]string, 0, len(snapshots))

	for _, snap := range snapshots {
		// Only export nodes that are confirmed available by the latest probe.
		if !snap.InitialCheckDone || !snap.Available {
			continue
		}
		if snap.Blacklisted {
			continue
		}

		if snap.ListenAddress == "" || snap.Port == 0 {
			continue
		}

		listenAddr := snap.ListenAddress
		if (listenAddr == "0.0.0.0" || listenAddr == "::") && extIP != "" {
			listenAddr = extIP
		}

		var proxyURI string
		if s.cfg.ProxyUsername != "" && s.cfg.ProxyPassword != "" {
			proxyURI = fmt.Sprintf("http://%s:%s@%s:%d",
				s.cfg.ProxyUsername, s.cfg.ProxyPassword,
				listenAddr, snap.Port)
		} else {
			proxyURI = fmt.Sprintf("http://%s:%d", listenAddr, snap.Port)
		}

		// Deduplicate (Pool mode would otherwise output the same entry N times).
		if _, ok := seen[proxyURI]; ok {
			continue
		}
		seen[proxyURI] = struct{}{}
		lines = append(lines, proxyURI)
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=proxy_pool.txt")

	if len(lines) == 0 {
		_, _ = w.Write([]byte(""))
		return
	}
	_, _ = w.Write([]byte(strings.Join(lines, "\n") + "\n"))
}

// handleSettings handles GET/PUT for dynamic settings (external_ip, probe_target, skip_cert_verify).
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if s.nodeMgr == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]any{"error": "节点管理未启用"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		extIP, probeTarget, skipCertVerify, err := s.nodeMgr.GetSettings(r.Context())
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{
			"external_ip":      extIP,
			"probe_target":     probeTarget,
			"skip_cert_verify": skipCertVerify,
		})

	case http.MethodPut:
		var req struct {
			ExternalIP     string `json:"external_ip"`
			ProbeTarget    string `json:"probe_target"`
			SkipCertVerify bool   `json:"skip_cert_verify"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": "请求格式错误"})
			return
		}

		extIP := strings.TrimSpace(req.ExternalIP)
		probeTarget := strings.TrimSpace(req.ProbeTarget)

		_, _, oldSkip, err := s.nodeMgr.GetSettings(r.Context())
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}

		if err := s.nodeMgr.UpdateSettings(r.Context(), extIP, probeTarget, req.SkipCertVerify); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}

		needReload := oldSkip != req.SkipCertVerify

		writeJSON(w, map[string]any{
			"message":          "设置已保存",
			"external_ip":      extIP,
			"probe_target":     probeTarget,
			"skip_cert_verify": req.SkipCertVerify,
			"need_reload":      needReload,
		})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleSubscriptionStatus returns the current subscription refresh status.
func (s *Server) handleSubscriptionStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if s.subRefresher == nil {
		writeJSON(w, map[string]any{
			"enabled": false,
			"message": "订阅刷新未启用",
		})
		return
	}

	status := s.subRefresher.Status()
	writeJSON(w, map[string]any{
		"enabled":        true,
		"last_refresh":   status.LastRefresh,
		"next_refresh":   status.NextRefresh,
		"node_count":     status.NodeCount,
		"last_error":     status.LastError,
		"refresh_count":  status.RefreshCount,
		"is_refreshing":  status.IsRefreshing,
		"nodes_modified": status.NodesModified,
	})
}

// handleSubscriptionRefresh triggers an immediate subscription refresh.
func (s *Server) handleSubscriptionRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if s.subRefresher == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]any{"error": "订阅刷新未启用"})
		return
	}

	if err := s.subRefresher.RefreshNow(); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}

	status := s.subRefresher.Status()
	writeJSON(w, map[string]any{
		"message":    "刷新成功",
		"node_count": status.NodeCount,
	})
}

// nodePayload is the JSON request body for node CRUD operations.
type nodePayload struct {
	Name     string `json:"name"`
	URI      string `json:"uri"`
	Port     uint16 `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func (p nodePayload) toConfig() config.NodeConfig {
	return config.NodeConfig{
		Name:     p.Name,
		URI:      p.URI,
		Port:     p.Port,
		Username: p.Username,
		Password: p.Password,
	}
}

// handleConfigNodes handles GET (list) and POST (create) for config nodes.
func (s *Server) handleConfigNodes(w http.ResponseWriter, r *http.Request) {
	if !s.ensureNodeManager(w) {
		return
	}

	switch r.Method {
	case http.MethodGet:
		nodes, err := s.nodeMgr.ListConfigNodes(r.Context())
		if err != nil {
			s.respondNodeError(w, err)
			return
		}
		writeJSON(w, map[string]any{"nodes": nodes})
	case http.MethodPost:
		var payload nodePayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": "请求格式错误"})
			return
		}
		node, err := s.nodeMgr.CreateNode(r.Context(), payload.toConfig())
		if err != nil {
			s.respondNodeError(w, err)
			return
		}
		writeJSON(w, map[string]any{"node": node, "message": "节点已添加，请点击重载使配置生效"})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleConfigNodeItem handles PUT (update) and DELETE for a specific config node.
func (s *Server) handleConfigNodeItem(w http.ResponseWriter, r *http.Request) {
	if !s.ensureNodeManager(w) {
		return
	}

	namePart := strings.TrimPrefix(r.URL.Path, "/api/nodes/config/")
	nodeName, err := url.PathUnescape(namePart)
	if err != nil || nodeName == "" {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "节点名称无效"})
		return
	}

	switch r.Method {
	case http.MethodPut:
		var payload nodePayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": "请求格式错误"})
			return
		}
		node, err := s.nodeMgr.UpdateNode(r.Context(), nodeName, payload.toConfig())
		if err != nil {
			s.respondNodeError(w, err)
			return
		}
		writeJSON(w, map[string]any{"node": node, "message": "节点已更新，请点击重载使配置生效"})
	case http.MethodDelete:
		if err := s.nodeMgr.DeleteNode(r.Context(), nodeName); err != nil {
			s.respondNodeError(w, err)
			return
		}
		writeJSON(w, map[string]any{"message": "节点已删除，请点击重载使配置生效"})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleReload triggers a configuration reload.
func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.ensureNodeManager(w) {
		return
	}

	if err := s.nodeMgr.TriggerReload(r.Context()); err != nil {
		s.respondNodeError(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"message": "重载成功，现有连接已被中断",
	})
}

func (s *Server) ensureNodeManager(w http.ResponseWriter) bool {
	if s.nodeMgr == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]any{"error": "节点管理未启用"})
		return false
	}
	return true
}

func (s *Server) respondNodeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrNodeNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrNodeConflict), errors.Is(err, ErrInvalidNode):
		status = http.StatusBadRequest
	}
	w.WriteHeader(status)
	writeJSON(w, map[string]any{"error": err.Error()})
}
