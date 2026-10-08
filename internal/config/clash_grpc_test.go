package config

import (
	"fmt"
	"net/url"
	"testing"
)

func TestParseClashYAML_TransportOptions(t *testing.T) {
	parsers := []struct {
		name  string
		parse func(string) ([]NodeConfig, error)
	}{
		{name: "clash", parse: parseClashYAML},
		{name: "subscription", parse: ParseSubscriptionContent},
	}
	cases := []struct {
		name    string
		network string
		opts    string
		params  url.Values
	}{
		{
			name: "grpc_service", network: "grpc",
			opts:   "    grpc-opts:\n      grpc-service-name: grpc\n",
			params: url.Values{"type": {"grpc"}, "serviceName": {"grpc"}},
		},
		{
			name: "grpc_escaped_service", network: "grpc",
			opts:   "    grpc-opts:\n      grpc-service-name: \"service/name &+ 服务\"\n",
			params: url.Values{"type": {"grpc"}, "serviceName": {"service/name &+ 服务"}},
		},
		{
			name: "grpc_without_options", network: "grpc",
			params: url.Values{"type": {"grpc"}},
		},
		{
			name: "grpc_empty_options", network: "grpc",
			opts:   "    grpc-opts: {}\n",
			params: url.Values{"type": {"grpc"}},
		},
		{
			name: "grpc_empty_service", network: "grpc",
			opts:   "    grpc-opts:\n      grpc-service-name: \"\"\n",
			params: url.Values{"type": {"grpc"}},
		},
		{
			name: "websocket", network: "ws",
			opts:   "    ws-opts:\n      path: \"/ws?mode=fast&tag=服务\"\n      headers:\n        Host: ws.example.com\n",
			params: url.Values{"type": {"ws"}, "path": {"/ws?mode=fast&tag=服务"}, "host": {"ws.example.com"}},
		},
		{name: "tcp", network: "tcp", params: url.Values{}},
	}

	for _, parser := range parsers {
		for _, protocol := range []string{"vmess", "trojan", "vless"} {
			for _, tc := range cases {
				t.Run(parser.name+"/"+protocol+"/"+tc.name, func(t *testing.T) {
					content := fmt.Sprintf(`proxies:
  - name: test-node
    type: %s
    server: example.com
    port: 443
    uuid: 11111111-1111-1111-1111-111111111111
    password: secret
    network: %s
    tls: true
    servername: tls.example.com
    skip-cert-verify: true
    client-fingerprint: chrome
%s`, protocol, tc.network, tc.opts)
					nodes, err := parser.parse(content)
					if err != nil {
						t.Fatalf("parse Clash subscription: %v", err)
					}
					if len(nodes) != 1 {
						t.Fatalf("expected 1 node, got %d", len(nodes))
					}
					if nodes[0].Name != "test-node" {
						t.Errorf("expected node name test-node, got %q", nodes[0].Name)
					}
					u, err := url.Parse(nodes[0].URI)
					if err != nil {
						t.Fatalf("parse generated URI: %v", err)
					}
					if u.Scheme != protocol || u.Host != "example.com:443" {
						t.Errorf("unexpected URI scheme or host: %s", nodes[0].URI)
					}
					params, err := url.ParseQuery(u.RawQuery)
					if err != nil {
						t.Fatalf("parse generated URI query: %v", err)
					}
					want := url.Values{"sni": {"tls.example.com"}, "fp": {"chrome"}}
					if protocol == "trojan" {
						want.Set("allowInsecure", "1")
					} else {
						want.Set("security", "tls")
					}
					if protocol == "vless" {
						want.Set("encryption", "none")
					}
					for key, values := range tc.params {
						want[key] = values
					}
					if params.Encode() != want.Encode() {
						t.Errorf("expected query %q, got %q", want.Encode(), params.Encode())
					}
				})
			}
		}
	}
}
