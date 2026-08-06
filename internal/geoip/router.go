package geoip

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const regionHeader = "X-Easyproxy-Region"

// RouterConfig holds configuration for the GeoIP router
type RouterConfig struct {
	Listen   string
	Port     uint16
	Username string
	Password string
}

// PoolDialer is an interface for dialing through a specific pool
type PoolDialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Router handles HTTP proxy requests with path-based region routing
type Router struct {
	cfg        RouterConfig
	pools      map[string]PoolDialer          // region -> dialer
	global     PoolDialer                     // default pool for requests without region path
	transports map[PoolDialer]*http.Transport // cached transports per dialer
	server     *http.Server
	mu         sync.RWMutex
	logger     *log.Logger
}

// NewRouter creates a new GeoIP router
func NewRouter(cfg RouterConfig, logger *log.Logger) *Router {
	if logger == nil {
		logger = log.Default()
	}
	return &Router{
		cfg:        cfg,
		pools:      make(map[string]PoolDialer),
		transports: make(map[PoolDialer]*http.Transport),
		logger:     logger,
	}
}

// SetPool registers a pool dialer for a specific region
func (r *Router) SetPool(region string, dialer PoolDialer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pools[region] = dialer
	// Clear transport cache since pools changed
	r.transports = make(map[PoolDialer]*http.Transport)
}

// SetGlobalPool sets the default pool for requests without region path
func (r *Router) SetGlobalPool(dialer PoolDialer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.global = dialer
	// Clear transport cache since pools changed
	r.transports = make(map[PoolDialer]*http.Transport)
}

// Start starts the GeoIP router HTTP server
func (r *Router) Start(ctx context.Context) error {
	addr := fmt.Sprintf("%s:%d", r.cfg.Listen, r.cfg.Port)

	r.server = &http.Server{
		Addr:    addr,
		Handler: r,
	}

	go func() {
		r.logger.Printf("🌐 GeoIP Router started on %s", addr)
		r.logger.Println("   Routes: /jp, /kr, /us, /hk, /tw, /sg, /other (default: all nodes)")
		if err := r.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			r.logger.Printf("GeoIP router error: %v", err)
		}
	}()

	go func() {
		<-ctx.Done()
		r.Stop()
	}()

	return nil
}

// Stop stops the GeoIP router
func (r *Router) Stop() error {
	if r.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return r.server.Shutdown(ctx)
	}
	return nil
}

// checkProxyAuth validates the Proxy-Authorization header.
// Proxy clients send credentials via "Proxy-Authorization", not "Authorization".
func (r *Router) checkProxyAuth(req *http.Request) bool {
	auth := req.Header.Get("Proxy-Authorization")
	if auth == "" {
		return false
	}
	const prefix = "Basic "
	if !strings.HasPrefix(auth, prefix) {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(auth[len(prefix):])
	if err != nil {
		return false
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return false
	}
	return parts[0] == r.cfg.Username && parts[1] == r.cfg.Password
}

// ServeHTTP handles incoming HTTP proxy requests
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// Check proxy authentication if configured
	if r.cfg.Username != "" {
		if !r.checkProxyAuth(req) {
			w.Header().Set("Proxy-Authenticate", `Basic realm="Proxy"`)
			http.Error(w, "Proxy authentication required", http.StatusProxyAuthRequired)
			return
		}
	}

	// Extract the requested region after proxy authentication succeeds.
	region, targetHost, err := r.parseRequest(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Get the appropriate pool
	r.mu.RLock()
	var dialer PoolDialer
	if region != "" {
		dialer = r.pools[region]
	}
	if dialer == nil {
		dialer = r.global
	}
	r.mu.RUnlock()

	if dialer == nil {
		http.Error(w, "No proxy pool available", http.StatusServiceUnavailable)
		return
	}

	if req.Method == http.MethodConnect {
		r.handleConnect(w, req, dialer, targetHost)
	} else {
		r.handleHTTP(w, req, dialer, targetHost)
	}
}

// parseRequest extracts region and target host from the request
func (r *Router) parseRequest(req *http.Request) (region, targetHost string, err error) {
	// A proxy-specific header works for both regular HTTP requests and CONNECT
	// tunnels. Remove it before forwarding a regular HTTP request upstream.
	headerRegion := strings.TrimSpace(req.Header.Get(regionHeader))
	req.Header.Del(regionHeader)
	if headerRegion != "" {
		region = strings.ToLower(headerRegion)
		for _, reg := range AllRegions() {
			if region == reg {
				return region, req.Host, nil
			}
		}
		return "", "", fmt.Errorf("unsupported GeoIP region %q", headerRegion)
	}

	// Retain the existing path and CONNECT authority forms as fallbacks.
	if req.Method == http.MethodConnect {
		// CONNECT requests: check if host starts with region prefix
		// e.g., CONNECT jp/example.com:443 or just example.com:443
		host := req.Host
		// net/http parses jp/example.com:443 as URL.Host=jp and
		// URL.Path=/example.com:443. Rebuild the original authority.
		if req.URL != nil && req.URL.Host != "" && req.URL.Path != "" {
			host = req.URL.Host + req.URL.Path
		}
		for _, reg := range AllRegions() {
			prefix := reg + "/"
			if strings.HasPrefix(host, prefix) {
				return reg, strings.TrimPrefix(host, prefix), nil
			}
		}
		return "", host, nil
	}

	// For regular HTTP requests, check URL path.
	path := req.URL.Path
	for _, reg := range AllRegions() {
		prefix := "/" + reg + "/"
		if strings.HasPrefix(path, prefix) {
			// Rewrite the path
			req.URL.Path = "/" + strings.TrimPrefix(path, prefix)
			return reg, req.Host, nil
		}
		// Also check for exact match like /jp
		if path == "/"+reg {
			req.URL.Path = "/"
			return reg, req.Host, nil
		}
	}

	return "", req.Host, nil
}

// handleConnect handles HTTPS CONNECT tunneling
func (r *Router) handleConnect(w http.ResponseWriter, req *http.Request, dialer PoolDialer, targetHost string) {
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()

	targetConn, err := dialer.DialContext(ctx, "tcp", targetHost)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to connect: %v", err), http.StatusBadGateway)
		return
	}
	defer targetConn.Close()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, fmt.Sprintf("Hijack failed: %v", err), http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	// Send 200 Connection Established
	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	// Bidirectional copy
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(targetConn, clientConn)
	}()

	go func() {
		defer wg.Done()
		io.Copy(clientConn, targetConn)
	}()

	wg.Wait()
}

// getTransport returns a cached http.Transport for the given dialer, creating one if needed.
func (r *Router) getTransport(dialer PoolDialer) *http.Transport {
	r.mu.RLock()
	t, ok := r.transports[dialer]
	r.mu.RUnlock()
	if ok {
		return t
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Double-check after acquiring write lock
	if t, ok = r.transports[dialer]; ok {
		return t
	}
	t = &http.Transport{
		DialContext:         dialer.DialContext,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}
	r.transports[dialer] = t
	return t
}

// handleHTTP handles regular HTTP requests
func (r *Router) handleHTTP(w http.ResponseWriter, req *http.Request, dialer PoolDialer, targetHost string) {
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()

	// Create a new request to the target
	targetURL := req.URL
	if targetURL.Host == "" {
		targetURL.Host = targetHost
	}
	if targetURL.Scheme == "" {
		targetURL.Scheme = "http"
	}

	outReq, err := http.NewRequestWithContext(ctx, req.Method, targetURL.String(), req.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to create request: %v", err), http.StatusInternalServerError)
		return
	}

	// Copy headers
	for key, values := range req.Header {
		for _, value := range values {
			outReq.Header.Add(key, value)
		}
	}

	// Remove hop-by-hop headers
	outReq.Header.Del("Proxy-Connection")
	outReq.Header.Del("Proxy-Authorization")

	// Use cached transport with connection pooling
	transport := r.getTransport(dialer)

	resp, err := transport.RoundTrip(outReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("Request failed: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
