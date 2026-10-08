package builder

import (
	"fmt"
	"testing"

	"easy_proxies/internal/config"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
)

func TestBuildNodeOutbound_ClashGRPCServiceName(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		omitOptions bool
	}{
		{name: "plain", serviceName: "my-grpc-service"},
		{name: "reserved_characters", serviceName: "grpc/service+name ?mode=test&percent=%#服务"},
		{name: "empty"},
		{name: "missing_options", omitOptions: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			grpcOptions := ""
			if !tt.omitOptions {
				grpcOptions = fmt.Sprintf("    grpc-opts:\n      grpc-service-name: %q\n", tt.serviceName)
			}
			content := fmt.Sprintf(`proxies:
  - name: vmess
    type: vmess
    server: example.com
    port: 443
    uuid: b831381d-6324-4d53-ad4f-8cda48b30811
    network: grpc
    tls: true
%s  - name: trojan
    type: trojan
    server: example.com
    port: 443
    password: test-password
    network: grpc
%s  - name: vless
    type: vless
    server: example.com
    port: 443
    uuid: b831381d-6324-4d53-ad4f-8cda48b30811
    network: grpc
    tls: true
%s`, grpcOptions, grpcOptions, grpcOptions)

			nodes, err := config.ParseSubscriptionContent(content)
			if err != nil {
				t.Fatalf("parse Clash subscription: %v", err)
			}
			if len(nodes) != 3 {
				t.Fatalf("expected 3 parsed nodes, got %d", len(nodes))
			}

			for _, node := range nodes {
				t.Run(node.Name, func(t *testing.T) {
					outbound, err := buildNodeOutbound(node.Name, node.URI, false)
					if err != nil {
						t.Fatalf("build outbound from Clash URI %q: %v", node.URI, err)
					}
					if outbound.Type != node.Name {
						t.Fatalf("expected outbound type %q, got %q", node.Name, outbound.Type)
					}

					var transport *option.V2RayTransportOptions
					switch opts := outbound.Options.(type) {
					case *option.VMessOutboundOptions:
						transport = opts.Transport
					case *option.TrojanOutboundOptions:
						transport = opts.Transport
					case *option.VLESSOutboundOptions:
						transport = opts.Transport
					default:
						t.Fatalf("unexpected outbound options type %T", outbound.Options)
					}
					if transport == nil {
						t.Fatal("expected gRPC transport, got nil")
					}
					if transport.Type != C.V2RayTransportTypeGRPC {
						t.Fatalf("expected gRPC transport, got %q", transport.Type)
					}
					if got := transport.GRPCOptions.ServiceName; got != tt.serviceName {
						t.Fatalf("gRPC service name not preserved: got %q, want %q (URI: %s)", got, tt.serviceName, node.URI)
					}
				})
			}
		})
	}
}
