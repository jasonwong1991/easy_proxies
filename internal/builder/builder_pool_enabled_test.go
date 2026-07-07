package builder

import (
	"strings"
	"testing"

	"easy_proxies/internal/config"
	poolout "easy_proxies/internal/outbound/pool"
)

func boolPtr(v bool) *bool {
	return &v
}

func poolOptionsByTag(t *testing.T, cfg *config.Config, tag string) *poolout.Options {
	t.Helper()
	opts, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	for _, outbound := range opts.Outbounds {
		if outbound.Tag != tag {
			continue
		}
		poolOpts, ok := outbound.Options.(*poolout.Options)
		if !ok {
			t.Fatalf("outbound %q options = %T, want *pool.Options", tag, outbound.Options)
		}
		return poolOpts
	}
	t.Fatalf("pool outbound %q not found", tag)
	return nil
}

func TestBuildHybrid_PoolDisabledNodeKeepsMultiPortOnly(t *testing.T) {
	cfg := &config.Config{
		Mode:      "hybrid",
		Listener:  config.ListenerConfig{Address: "127.0.0.1", Port: 2323},
		MultiPort: config.MultiPortConfig{Address: "127.0.0.1", BasePort: 24000},
		Pool:      config.PoolConfig{Mode: "sequential"},
		Nodes: []config.NodeConfig{
			{Name: "Normal", URI: "socks5://127.0.0.1:1080#Normal", Port: 24000},
			{Name: "Premium", URI: "socks5://127.0.0.1:1081#Premium", Port: 24001, PoolEnabled: boolPtr(false)},
		},
	}

	mainPool := poolOptionsByTag(t, cfg, poolout.Tag)
	if got, want := strings.Join(mainPool.Members, ","), "normal"; got != want {
		t.Fatalf("main pool members = %q, want %q", got, want)
	}

	premiumPortPool := poolOptionsByTag(t, cfg, poolout.Tag+"-premium")
	if got, want := strings.Join(premiumPortPool.Members, ","), "premium"; got != want {
		t.Fatalf("premium multi-port pool members = %q, want %q", got, want)
	}
	if meta := premiumPortPool.Metadata["premium"]; meta.PoolEnabled {
		t.Fatalf("premium metadata PoolEnabled = true, want false")
	}
}

func TestBuild_PoolExcludeKeywordsAndExplicitOverride(t *testing.T) {
	cfg := &config.Config{
		Mode:      "hybrid",
		Listener:  config.ListenerConfig{Address: "127.0.0.1", Port: 2323},
		MultiPort: config.MultiPortConfig{Address: "127.0.0.1", BasePort: 24000},
		Pool: config.PoolConfig{
			Mode:            "sequential",
			ExcludeKeywords: []string{"premium"},
		},
		Nodes: []config.NodeConfig{
			{Name: "Normal", URI: "socks5://127.0.0.1:1080#Normal", Port: 24000},
			{Name: "Premium", URI: "socks5://127.0.0.1:1081#Premium", Port: 24001},
			{Name: "Premium Force", URI: "socks5://127.0.0.1:1082#PremiumForce", Port: 24002, PoolEnabled: boolPtr(true)},
		},
	}

	mainPool := poolOptionsByTag(t, cfg, poolout.Tag)
	if got, want := strings.Join(mainPool.Members, ","), "normal,premium-force"; got != want {
		t.Fatalf("main pool members = %q, want %q", got, want)
	}
}

func TestBuild_AllPoolMembersExcludedFails(t *testing.T) {
	cfg := &config.Config{
		Mode:     "pool",
		Listener: config.ListenerConfig{Address: "127.0.0.1", Port: 2323},
		Pool: config.PoolConfig{
			Mode:            "sequential",
			ExcludeKeywords: []string{"premium"},
		},
		Nodes: []config.NodeConfig{
			{Name: "Premium", URI: "socks5://127.0.0.1:1081#Premium"},
		},
	}

	_, err := Build(cfg)
	if err == nil {
		t.Fatalf("expected build error")
	}
	if !strings.Contains(err.Error(), "no pool-enabled nodes available") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuild_GeoIPRegionPoolsUseOnlyPoolEnabledMembers(t *testing.T) {
	cfg := &config.Config{
		Mode:      "hybrid",
		Listener:  config.ListenerConfig{Address: "127.0.0.1", Port: 2323},
		MultiPort: config.MultiPortConfig{Address: "127.0.0.1", BasePort: 24000},
		Pool:      config.PoolConfig{Mode: "sequential"},
		GeoIP:     config.GeoIPConfig{Enabled: true},
		Nodes: []config.NodeConfig{
			{Name: "Normal", URI: "socks5://127.0.0.1:1080#Normal", Port: 24000},
			{Name: "Premium", URI: "socks5://127.0.0.1:1081#Premium", Port: 24001, PoolEnabled: boolPtr(false)},
		},
	}

	regionPool := poolOptionsByTag(t, cfg, "pool-other")
	if got, want := strings.Join(regionPool.Members, ","), "normal"; got != want {
		t.Fatalf("region pool members = %q, want %q", got, want)
	}
}
