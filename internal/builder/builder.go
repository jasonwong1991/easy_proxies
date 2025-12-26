package builder

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"easy_proxies/internal/config"
	poolout "easy_proxies/internal/outbound/pool"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/auth"
	"github.com/sagernet/sing/common/json/badoption"
)

// Build converts high level config into sing-box Options tree.
func Build(cfg *config.Config) (option.Options, error) {
	baseOutbounds := make([]option.Outbound, 0, len(cfg.Nodes))
	memberTags := make([]string, 0, len(cfg.Nodes))
	metadata := make(map[string]poolout.MemberMeta)
	var failedNodes []string
	usedTags := make(map[string]int) // Track tag usage for uniqueness

	for _, node := range cfg.Nodes {
		baseTag := sanitizeTag(node.Name)
		if baseTag == "" {
			baseTag = fmt.Sprintf("node-%d", len(memberTags)+1)
		}

		// Ensure tag uniqueness by appending a counter if needed
		tag := baseTag
		if count, exists := usedTags[baseTag]; exists {
			usedTags[baseTag] = count + 1
			tag = fmt.Sprintf("%s-%d", baseTag, count+1)
		} else {
			usedTags[baseTag] = 1
		}

		outbound, err := buildNodeOutbound(tag, node.URI, cfg.SkipCertVerify)
		if err != nil {
			log.Printf("❌ Failed to build node '%s': %v (skipping)", node.Name, err)
			failedNodes = append(failedNodes, node.Name)
			continue
		}
		memberTags = append(memberTags, tag)
		baseOutbounds = append(baseOutbounds, outbound)
		meta := poolout.MemberMeta{
			Name: node.Name,
			URI:  node.URI,
			Mode: cfg.Mode,
		}
		// For multi-port and hybrid modes, use per-node port
		if cfg.Mode == "multi-port" || cfg.Mode == "hybrid" {
			meta.ListenAddress = cfg.MultiPort.Address
			meta.Port = node.Port
		} else {
			meta.ListenAddress = cfg.Listener.Address
			meta.Port = cfg.Listener.Port
		}
		metadata[tag] = meta
	}

	// Check if we have at least one valid node
	if len(baseOutbounds) == 0 {
		return option.Options{}, fmt.Errorf("no valid nodes available (all %d nodes failed to build)", len(cfg.Nodes))
	}

	// Log summary
	if len(failedNodes) > 0 {
		log.Printf("⚠️  %d/%d nodes failed and were skipped: %v", len(failedNodes), len(cfg.Nodes), failedNodes)
	}
	log.Printf("✅ Successfully built %d/%d nodes", len(baseOutbounds), len(cfg.Nodes))

	// Print proxy links for each node
	printProxyLinks(cfg, metadata)

	var (
		inbounds  []option.Inbound
		outbounds = make([]option.Outbound, len(baseOutbounds))
		route     option.RouteOptions
	)
	copy(outbounds, baseOutbounds)

	// Determine which components to enable based on mode
	enablePoolInbound := cfg.Mode == "pool" || cfg.Mode == "hybrid"
	enableMultiPort := cfg.Mode == "multi-port" || cfg.Mode == "hybrid"

	if !enablePoolInbound && !enableMultiPort {
		return option.Options{}, fmt.Errorf("unsupported mode %s", cfg.Mode)
	}

	// Build pool inbound (single entry point for all nodes)
	if enablePoolInbound {
		inbound, err := buildPoolInbound(cfg)
		if err != nil {
			return option.Options{}, err
		}
		inbounds = append(inbounds, inbound)
		poolOptions := poolout.Options{
			Mode:              cfg.Pool.Mode,
			Members:           memberTags,
			FailureThreshold:  cfg.Pool.FailureThreshold,
			BlacklistDuration: cfg.Pool.BlacklistDuration,
			Metadata:          metadata,
		}
		outbounds = append(outbounds, option.Outbound{
			Type:    poolout.Type,
			Tag:     poolout.Tag,
			Options: &poolOptions,
		})
		route.Final = poolout.Tag
	}

	// Build multi-port inbounds (one port per node)
	if enableMultiPort {
		addr, err := parseAddr(cfg.MultiPort.Address)
		if err != nil {
			return option.Options{}, fmt.Errorf("parse multi-port address: %w", err)
		}
		for _, tag := range memberTags {
			meta := metadata[tag]
			perMeta := map[string]poolout.MemberMeta{tag: meta}
			poolTag := fmt.Sprintf("%s-%s", poolout.Tag, tag)
			perOptions := poolout.Options{
				Mode:              "sequential",
				Members:           []string{tag},
				FailureThreshold:  cfg.Pool.FailureThreshold,
				BlacklistDuration: cfg.Pool.BlacklistDuration,
				Metadata:          perMeta,
			}
			perPool := option.Outbound{
				Type:    poolout.Type,
				Tag:     poolTag,
				Options: &perOptions,
			}
			outbounds = append(outbounds, perPool)

			inboundTag := fmt.Sprintf("in-%s", tag)
			var inbound option.Inbound

			// Choose inbound type based on configuration
			if strings.ToLower(cfg.MultiPort.InboundType) == "ss" {
				// Shadowsocks inbound
				ssOptions := &option.ShadowsocksInboundOptions{
					ListenOptions: option.ListenOptions{
						Listen:     addr,
						ListenPort: meta.Port,
					},
					Method:   cfg.MultiPort.SSMethod,
					Password: cfg.MultiPort.Password,
				}
				inbound = option.Inbound{
					Type:    C.TypeShadowsocks,
					Tag:     inboundTag,
					Options: ssOptions,
				}
			} else {
				// HTTP inbound (default)
				httpOptions := &option.HTTPMixedInboundOptions{
					ListenOptions: option.ListenOptions{
						Listen:     addr,
						ListenPort: meta.Port,
					},
				}
				username := cfg.MultiPort.Username
				password := cfg.MultiPort.Password
				if username != "" {
					httpOptions.Users = []auth.User{{Username: username, Password: password}}
				}
				inbound = option.Inbound{
					Type:    C.TypeHTTP,
					Tag:     inboundTag,
					Options: httpOptions,
				}
			}

			inbounds = append(inbounds, inbound)
			route.Rules = append(route.Rules, option.Rule{
				Type: C.RuleTypeDefault,
				DefaultOptions: option.DefaultRule{
					RawDefaultRule: option.RawDefaultRule{
						Inbound: badoption.Listable[string]{inboundTag},
					},
					RuleAction: option.RuleAction{
						Action: C.RuleActionTypeRoute,
						RouteOptions: option.RouteActionOptions{
							Outbound: poolTag,
						},
					},
				},
			})
		}
	}

	opts := option.Options{
		Log:       &option.LogOptions{Level: strings.ToLower(cfg.LogLevel)},
		Inbounds:  inbounds,
		Outbounds: outbounds,
		Route:     &route,
	}
	return opts, nil
}

func buildPoolInbound(cfg *config.Config) (option.Inbound, error) {
	listenAddr, err := parseAddr(cfg.Listener.Address)
	if err != nil {
		return option.Inbound{}, fmt.Errorf("parse listener address: %w", err)
	}
	inboundOptions := &option.HTTPMixedInboundOptions{
		ListenOptions: option.ListenOptions{
			Listen:     listenAddr,
			ListenPort: cfg.Listener.Port,
		},
	}
	if cfg.Listener.Username != "" {
		inboundOptions.Users = []auth.User{{
			Username: cfg.Listener.Username,
			Password: cfg.Listener.Password,
		}}
	}
	inbound := option.Inbound{
		Type:    C.TypeHTTP,
		Tag:     "http-in",
		Options: inboundOptions,
	}
	return inbound, nil
}

func buildNodeOutbound(tag, rawURI string, skipCertVerify bool) (option.Outbound, error) {
	parsed, err := url.Parse(rawURI)
	if err != nil {
		return option.Outbound{}, fmt.Errorf("parse uri: %w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "vless":
		opts, err := buildVLESSOptions(parsed, skipCertVerify)
		if err != nil {
			return option.Outbound{}, err
		}
		return option.Outbound{Type: C.TypeVLESS, Tag: tag, Options: &opts}, nil
	case "hysteria2":
		opts, err := buildHysteria2Options(parsed, skipCertVerify)
		if err != nil {
			return option.Outbound{}, err
		}
		return option.Outbound{Type: C.TypeHysteria2, Tag: tag, Options: &opts}, nil
	case "ss", "shadowsocks":
		opts, err := buildShadowsocksOptions(parsed)
		if err != nil {
			return option.Outbound{}, err
		}
		return option.Outbound{Type: C.TypeShadowsocks, Tag: tag, Options: &opts}, nil
	case "trojan":
		opts, err := buildTrojanOptions(parsed, skipCertVerify)
		if err != nil {
			return option.Outbound{}, err
		}
		return option.Outbound{Type: C.TypeTrojan, Tag: tag, Options: &opts}, nil
	case "vmess":
		opts, err := buildVMessOptions(rawURI, skipCertVerify)
		if err != nil {
			return option.Outbound{}, err
		}
		return option.Outbound{Type: C.TypeVMess, Tag: tag, Options: &opts}, nil
	default:
		return option.Outbound{}, fmt.Errorf("unsupported scheme %q", parsed.Scheme)
	}
}

func buildVLESSOptions(u *url.URL, skipCertVerify bool) (option.VLESSOutboundOptions, error) {
	uuid := u.User.Username()
	if uuid == "" {
		return option.VLESSOutboundOptions{}, errors.New("vless uri missing uuid in userinfo")
	}
	server, port, err := hostPort(u, 443)
	if err != nil {
		return option.VLESSOutboundOptions{}, err
	}
	query := u.Query()
	opts := option.VLESSOutboundOptions{
		UUID:          uuid,
		ServerOptions: option.ServerOptions{Server: server, ServerPort: uint16(port)},
		Network:       option.NetworkList(""),
	}
	if flow := query.Get("flow"); flow != "" {
		opts.Flow = flow
	}
	if packetEncoding := query.Get("packetEncoding"); packetEncoding != "" {
		opts.PacketEncoding = &packetEncoding
	}
	if transport, err := buildV2RayTransport(query); err != nil {
		return option.VLESSOutboundOptions{}, err
	} else if transport != nil {
		opts.Transport = transport
	}
	if tlsOptions, err := buildTLSOptions(query, skipCertVerify); err != nil {
		return option.VLESSOutboundOptions{}, err
	} else if tlsOptions != nil {
		opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: tlsOptions}
	}
	return opts, nil
}

func buildHysteria2Options(u *url.URL, skipCertVerify bool) (option.Hysteria2OutboundOptions, error) {
	password := u.User.String()
	server, port, err := hostPort(u, 443)
	if err != nil {
		return option.Hysteria2OutboundOptions{}, err
	}
	query := u.Query()
	opts := option.Hysteria2OutboundOptions{
		ServerOptions: option.ServerOptions{Server: server, ServerPort: uint16(port)},
		Password:      password,
	}
	if up := query.Get("upMbps"); up != "" {
		opts.UpMbps = atoiDefault(up)
	}
	if down := query.Get("downMbps"); down != "" {
		opts.DownMbps = atoiDefault(down)
	}
	if obfs := query.Get("obfs"); obfs != "" {
		opts.Obfs = &option.Hysteria2Obfs{Type: obfs, Password: query.Get("obfs-password")}
	}
	opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: hysteriaTLSOptions(server, query, skipCertVerify)}
	return opts, nil
}

func hysteriaTLSOptions(host string, query url.Values, skipCertVerify bool) *option.OutboundTLSOptions {
	tlsOptions := &option.OutboundTLSOptions{
		Enabled:    true,
		ServerName: host,
		Insecure:   skipCertVerify,
	}
	if sni := query.Get("sni"); sni != "" {
		tlsOptions.ServerName = sni
	}
	insecure := query.Get("insecure")
	if insecure == "" {
		insecure = query.Get("allowInsecure")
	}
	if insecure != "" {
		tlsOptions.Insecure = insecure == "1" || strings.EqualFold(insecure, "true")
	}
	if alpn := query.Get("alpn"); alpn != "" {
		tlsOptions.ALPN = badoption.Listable[string](strings.Split(alpn, ","))
	}
	return tlsOptions
}

func buildTLSOptions(query url.Values, skipCertVerify bool) (*option.OutboundTLSOptions, error) {
	security := strings.ToLower(query.Get("security"))
	if security == "" || security == "none" {
		return nil, nil
	}
	tlsOptions := &option.OutboundTLSOptions{Enabled: true, Insecure: skipCertVerify}
	if sni := query.Get("sni"); sni != "" {
		tlsOptions.ServerName = sni
	}
	insecure := query.Get("allowInsecure")
	if insecure == "" {
		insecure = query.Get("insecure")
	}
	if insecure != "" {
		tlsOptions.Insecure = insecure == "1" || strings.EqualFold(insecure, "true")
	}
	if alpn := query.Get("alpn"); alpn != "" {
		tlsOptions.ALPN = badoption.Listable[string](strings.Split(alpn, ","))
	}
	fp := query.Get("fp")
	if fp != "" {
		tlsOptions.UTLS = &option.OutboundUTLSOptions{Enabled: true, Fingerprint: fp}
	}
	if security == "reality" {
		tlsOptions.Reality = &option.OutboundRealityOptions{Enabled: true, PublicKey: query.Get("pbk"), ShortID: query.Get("sid")}
		// Reality requires uTLS; use default fingerprint if not specified
		if tlsOptions.UTLS == nil {
			if fp == "" {
				fp = "chrome"
			}
			tlsOptions.UTLS = &option.OutboundUTLSOptions{Enabled: true, Fingerprint: fp}
		}
	}
	return tlsOptions, nil
}

func buildV2RayTransport(query url.Values) (*option.V2RayTransportOptions, error) {
	transportType := strings.ToLower(query.Get("type"))
	if transportType == "" || transportType == "tcp" {
		return nil, nil
	}
	options := &option.V2RayTransportOptions{Type: transportType}
	switch transportType {
	case C.V2RayTransportTypeWebsocket:
		wsPath := query.Get("path")
		// 解析 path 中的 early data 参数，如 /path?ed=2048
		if idx := strings.Index(wsPath, "?ed="); idx != -1 {
			edPart := wsPath[idx+4:]
			wsPath = wsPath[:idx]
			// 解析 ed 值
			edValue := edPart
			if ampIdx := strings.Index(edPart, "&"); ampIdx != -1 {
				edValue = edPart[:ampIdx]
			}
			if ed, err := strconv.Atoi(edValue); err == nil && ed > 0 {
				options.WebsocketOptions.MaxEarlyData = uint32(ed)
				options.WebsocketOptions.EarlyDataHeaderName = "Sec-WebSocket-Protocol"
			}
		}
		options.WebsocketOptions.Path = wsPath
		if host := query.Get("host"); host != "" {
			options.WebsocketOptions.Headers = badoption.HTTPHeader{"Host": {host}}
		}
	case C.V2RayTransportTypeHTTP:
		options.HTTPOptions.Path = query.Get("path")
		if host := query.Get("host"); host != "" {
			options.HTTPOptions.Host = badoption.Listable[string]{host}
		}
	case C.V2RayTransportTypeGRPC:
		options.GRPCOptions.ServiceName = query.Get("serviceName")
	case C.V2RayTransportTypeHTTPUpgrade:
		options.HTTPUpgradeOptions.Path = query.Get("path")
	case "xhttp":
		// XHTTP is not supported by sing-box, fallback to HTTPUpgrade
		log.Printf("⚠️  XHTTP transport not supported by sing-box, falling back to HTTPUpgrade")
		options.Type = C.V2RayTransportTypeHTTPUpgrade
		options.HTTPUpgradeOptions.Path = query.Get("path")
		if host := query.Get("host"); host != "" {
			options.HTTPUpgradeOptions.Headers = badoption.HTTPHeader{"Host": {host}}
		}
	default:
		return nil, fmt.Errorf("unsupported transport type %q", transportType)
	}
	return options, nil
}

func buildShadowsocksOptions(u *url.URL) (option.ShadowsocksOutboundOptions, error) {
	server, port, err := hostPort(u, 8388)
	if err != nil {
		return option.ShadowsocksOutboundOptions{}, err
	}

	// Decode userinfo (base64 encoded method:password)
	userInfo := u.User.String()
	decoded, err := base64.RawURLEncoding.DecodeString(userInfo)
	if err != nil {
		// Try standard base64
		decoded, err = base64.StdEncoding.DecodeString(userInfo)
		if err != nil {
			return option.ShadowsocksOutboundOptions{}, fmt.Errorf("decode shadowsocks userinfo: %w", err)
		}
	}

	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return option.ShadowsocksOutboundOptions{}, errors.New("shadowsocks userinfo format must be method:password")
	}

	method := parts[0]
	password := parts[1]

	opts := option.ShadowsocksOutboundOptions{
		ServerOptions: option.ServerOptions{Server: server, ServerPort: uint16(port)},
		Method:        method,
		Password:      password,
	}

	query := u.Query()
	if plugin := query.Get("plugin"); plugin != "" {
		opts.Plugin = plugin
		opts.PluginOptions = query.Get("plugin-opts")
	}

	return opts, nil
}

func buildTrojanOptions(u *url.URL, skipCertVerify bool) (option.TrojanOutboundOptions, error) {
	password := u.User.Username()
	if password == "" {
		return option.TrojanOutboundOptions{}, errors.New("trojan uri missing password in userinfo")
	}

	server, port, err := hostPort(u, 443)
	if err != nil {
		return option.TrojanOutboundOptions{}, err
	}

	query := u.Query()
	opts := option.TrojanOutboundOptions{
		ServerOptions: option.ServerOptions{Server: server, ServerPort: uint16(port)},
		Password:      password,
		Network:       option.NetworkList(""),
	}

	// Parse TLS options
	if tlsOptions, err := buildTrojanTLSOptions(query, skipCertVerify); err != nil {
		return option.TrojanOutboundOptions{}, err
	} else if tlsOptions != nil {
		opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: tlsOptions}
	}

	// Parse transport options
	if transport, err := buildV2RayTransport(query); err != nil {
		return option.TrojanOutboundOptions{}, err
	} else if transport != nil {
		opts.Transport = transport
	}

	return opts, nil
}

// vmessJSON represents the JSON structure of a VMess URI
type vmessJSON struct {
	V    interface{} `json:"v"`    // Version, can be string or int
	PS   string      `json:"ps"`   // Remarks/name
	Add  string      `json:"add"`  // Server address
	Port interface{} `json:"port"` // Server port, can be string or int
	ID   string      `json:"id"`   // UUID
	Aid  interface{} `json:"aid"`  // Alter ID, can be string or int
	Scy  string      `json:"scy"`  // Security/cipher
	Net  string      `json:"net"`  // Network type (tcp, ws, etc.)
	Type string      `json:"type"` // Header type
	Host string      `json:"host"` // Host header
	Path string      `json:"path"` // Path
	TLS  string      `json:"tls"`  // TLS (tls or empty)
	SNI  string      `json:"sni"`  // SNI
	ALPN string      `json:"alpn"` // ALPN
	FP   string      `json:"fp"`   // Fingerprint
}

func (v *vmessJSON) GetPort() int {
	switch p := v.Port.(type) {
	case float64:
		return int(p)
	case int:
		return p
	case string:
		port, _ := strconv.Atoi(p)
		return port
	}
	return 443
}

func (v *vmessJSON) GetAlterId() int {
	switch a := v.Aid.(type) {
	case float64:
		return int(a)
	case int:
		return a
	case string:
		aid, _ := strconv.Atoi(a)
		return aid
	}
	return 0
}

func buildVMessOptions(rawURI string, skipCertVerify bool) (option.VMessOutboundOptions, error) {
	// Remove vmess:// prefix
	encoded := strings.TrimPrefix(rawURI, "vmess://")

	// Try to decode as base64 JSON (standard format)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		// Try URL-safe base64
		decoded, err = base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			// Try as URL format: vmess://uuid@server:port?...
			return buildVMessOptionsFromURL(rawURI, skipCertVerify)
		}
	}

	var vmess vmessJSON
	if err := json.Unmarshal(decoded, &vmess); err != nil {
		return option.VMessOutboundOptions{}, fmt.Errorf("parse vmess json: %w", err)
	}

	if vmess.Add == "" {
		return option.VMessOutboundOptions{}, errors.New("vmess missing server address")
	}
	if vmess.ID == "" {
		return option.VMessOutboundOptions{}, errors.New("vmess missing uuid")
	}

	port := vmess.GetPort()
	if port == 0 {
		port = 443
	}

	opts := option.VMessOutboundOptions{
		ServerOptions: option.ServerOptions{
			Server:     vmess.Add,
			ServerPort: uint16(port),
		},
		UUID:     vmess.ID,
		AlterId:  vmess.GetAlterId(),
		Security: vmess.Scy,
	}

	// Default security
	if opts.Security == "" {
		opts.Security = "auto"
	}

	// Build transport options
	if vmess.Net != "" && vmess.Net != "tcp" {
		transport := &option.V2RayTransportOptions{}
		switch vmess.Net {
		case "ws":
			transport.Type = C.V2RayTransportTypeWebsocket
			wsPath := vmess.Path
			// Handle early data in path
			if idx := strings.Index(wsPath, "?ed="); idx != -1 {
				edPart := wsPath[idx+4:]
				wsPath = wsPath[:idx]
				edValue := edPart
				if ampIdx := strings.Index(edPart, "&"); ampIdx != -1 {
					edValue = edPart[:ampIdx]
				}
				if ed, err := strconv.Atoi(edValue); err == nil && ed > 0 {
					transport.WebsocketOptions.MaxEarlyData = uint32(ed)
					transport.WebsocketOptions.EarlyDataHeaderName = "Sec-WebSocket-Protocol"
				}
			}
			transport.WebsocketOptions.Path = wsPath
			if vmess.Host != "" {
				transport.WebsocketOptions.Headers = badoption.HTTPHeader{"Host": {vmess.Host}}
			}
		case "h2":
			transport.Type = C.V2RayTransportTypeHTTP
			transport.HTTPOptions.Path = vmess.Path
			if vmess.Host != "" {
				transport.HTTPOptions.Host = badoption.Listable[string]{vmess.Host}
			}
		case "grpc":
			transport.Type = C.V2RayTransportTypeGRPC
			transport.GRPCOptions.ServiceName = vmess.Path
		default:
			transport.Type = vmess.Net
		}
		opts.Transport = transport
	}

	// Build TLS options
	if vmess.TLS == "tls" {
		tlsOptions := &option.OutboundTLSOptions{Enabled: true, Insecure: skipCertVerify}
		if vmess.SNI != "" {
			tlsOptions.ServerName = vmess.SNI
		} else if vmess.Host != "" {
			tlsOptions.ServerName = vmess.Host
		}
		if vmess.ALPN != "" {
			tlsOptions.ALPN = badoption.Listable[string](strings.Split(vmess.ALPN, ","))
		}
		if vmess.FP != "" {
			tlsOptions.UTLS = &option.OutboundUTLSOptions{Enabled: true, Fingerprint: vmess.FP}
		}
		opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: tlsOptions}
	}

	return opts, nil
}

func buildVMessOptionsFromURL(rawURI string, skipCertVerify bool) (option.VMessOutboundOptions, error) {
	parsed, err := url.Parse(rawURI)
	if err != nil {
		return option.VMessOutboundOptions{}, fmt.Errorf("parse vmess url: %w", err)
	}

	uuid := parsed.User.Username()
	if uuid == "" {
		return option.VMessOutboundOptions{}, errors.New("vmess uri missing uuid")
	}

	server, port, err := hostPort(parsed, 443)
	if err != nil {
		return option.VMessOutboundOptions{}, err
	}

	query := parsed.Query()
	opts := option.VMessOutboundOptions{
		ServerOptions: option.ServerOptions{
			Server:     server,
			ServerPort: uint16(port),
		},
		UUID:     uuid,
		Security: query.Get("encryption"),
	}

	if opts.Security == "" {
		opts.Security = "auto"
	}

	if aid := query.Get("alterId"); aid != "" {
		opts.AlterId, _ = strconv.Atoi(aid)
	}

	// Build transport
	if transport, err := buildV2RayTransport(query); err != nil {
		return option.VMessOutboundOptions{}, err
	} else if transport != nil {
		opts.Transport = transport
	}

	// Build TLS
	if tlsOptions, err := buildTLSOptions(query, skipCertVerify); err != nil {
		return option.VMessOutboundOptions{}, err
	} else if tlsOptions != nil {
		opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: tlsOptions}
	}

	return opts, nil
}

func buildTrojanTLSOptions(query url.Values, skipCertVerify bool) (*option.OutboundTLSOptions, error) {
	// Trojan always uses TLS by default
	tlsOptions := &option.OutboundTLSOptions{Enabled: true, Insecure: skipCertVerify}

	if sni := query.Get("sni"); sni != "" {
		tlsOptions.ServerName = sni
	}
	if peer := query.Get("peer"); peer != "" && tlsOptions.ServerName == "" {
		tlsOptions.ServerName = peer
	}

	insecure := query.Get("allowInsecure")
	if insecure == "" {
		insecure = query.Get("insecure")
	}
	if insecure != "" {
		tlsOptions.Insecure = insecure == "1" || strings.EqualFold(insecure, "true")
	}

	if alpn := query.Get("alpn"); alpn != "" {
		tlsOptions.ALPN = badoption.Listable[string](strings.Split(alpn, ","))
	}

	if fp := query.Get("fp"); fp != "" {
		tlsOptions.UTLS = &option.OutboundUTLSOptions{Enabled: true, Fingerprint: fp}
	}

	return tlsOptions, nil
}

func hostPort(u *url.URL, defaultPort int) (string, int, error) {
	host := u.Hostname()
	if host == "" {
		return "", 0, errors.New("missing host")
	}
	portStr := u.Port()
	if portStr == "" {
		portStr = strconv.Itoa(defaultPort)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid port %q", portStr)
	}
	return host, port, nil
}

func parseAddr(value string) (*badoption.Addr, error) {
	addr := strings.TrimSpace(value)
	if addr == "" {
		return nil, nil
	}
	parsed, err := netip.ParseAddr(addr)
	if err != nil {
		return nil, err
	}
	bad := badoption.Addr(parsed)
	return &bad, nil
}

func sanitizeTag(name string) string {
	lower := strings.ToLower(name)
	lower = strings.TrimSpace(lower)
	if lower == "" {
		return ""
	}
	segments := strings.FieldsFunc(lower, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	result := strings.Join(segments, "-")
	result = strings.Trim(result, "-")
	return result
}

func atoiDefault(value string) int {
	if strings.HasSuffix(value, "mbps") {
		value = strings.TrimSuffix(value, "mbps")
	}
	if strings.HasSuffix(value, "Mbps") {
		value = strings.TrimSuffix(value, "Mbps")
	}
	v, _ := strconv.Atoi(value)
	return v
}

// printProxyLinks prints all proxy connection information at startup
func printProxyLinks(cfg *config.Config, metadata map[string]poolout.MemberMeta) {
	log.Println("")
	log.Println("📡 Proxy Links:")
	log.Println("═══════════════════════════════════════════════════════════════")

	showPoolEntry := cfg.Mode == "pool" || cfg.Mode == "hybrid"
	showMultiPort := cfg.Mode == "multi-port" || cfg.Mode == "hybrid"

	if showPoolEntry {
		// Pool mode: single entry point for all nodes
		var auth string
		if cfg.Listener.Username != "" {
			auth = fmt.Sprintf("%s:%s@", cfg.Listener.Username, cfg.Listener.Password)
		}
		proxyURL := fmt.Sprintf("http://%s%s:%d", auth, cfg.Listener.Address, cfg.Listener.Port)
		log.Printf("🌐 Pool Entry Point:")
		log.Printf("   %s", proxyURL)
		log.Println("")
		log.Printf("   Nodes in pool (%d):", len(metadata))
		for _, meta := range metadata {
			log.Printf("   • %s", meta.Name)
		}
		if showMultiPort {
			log.Println("")
		}
	}

	if showMultiPort {
		// Multi-port mode: each node has its own port
		log.Printf("🔌 Multi-Port Entry Points (%d nodes):", len(cfg.Nodes))
		log.Println("")
		for _, node := range cfg.Nodes {
			var auth string
			username := node.Username
			password := node.Password
			if username == "" {
				username = cfg.MultiPort.Username
				password = cfg.MultiPort.Password
			}
			if username != "" {
				auth = fmt.Sprintf("%s:%s@", username, password)
			}
			proxyURL := fmt.Sprintf("http://%s%s:%d", auth, cfg.MultiPort.Address, node.Port)
			log.Printf("   [%d] %s", node.Port, node.Name)
			log.Printf("       %s", proxyURL)
		}
	}

	log.Println("═══════════════════════════════════════════════════════════════")
	log.Println("")
}
