package subscription

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"easy_proxies/internal/config"
)

func TestCreateNewConfig_PreservesInlineNodes(t *testing.T) {
	// Setup base config with inline nodes
	baseCfg := &config.Config{
		Mode: "pool",
		Nodes: []config.NodeConfig{
			{
				Name:   "inline-node-1",
				URI:    "ss://test1@example.com:8388",
				Source: config.NodeSourceInline,
			},
			{
				Name:   "inline-node-2",
				URI:    "ss://test2@example.com:8389",
				Source: config.NodeSourceInline,
			},
		},
	}

	mgr := &Manager{
		baseCfg: baseCfg,
	}

	// Subscription nodes
	subNodes := []config.NodeConfig{
		{Name: "sub-node-1", URI: "ss://sub1@example.com:8390"},
		{Name: "sub-node-2", URI: "ss://sub2@example.com:8391"},
	}

	// Create new config
	newCfg := mgr.createNewConfig(subNodes)

	// Verify inline nodes are preserved
	if len(newCfg.Nodes) != 4 {
		t.Fatalf("expected 4 nodes (2 inline + 2 subscription), got %d", len(newCfg.Nodes))
	}

	// Verify inline nodes come first
	if newCfg.Nodes[0].Name != "inline-node-1" {
		t.Errorf("expected first node to be inline-node-1, got %s", newCfg.Nodes[0].Name)
	}
	if newCfg.Nodes[0].Source != config.NodeSourceInline {
		t.Errorf("expected first node source to be inline, got %s", newCfg.Nodes[0].Source)
	}

	if newCfg.Nodes[1].Name != "inline-node-2" {
		t.Errorf("expected second node to be inline-node-2, got %s", newCfg.Nodes[1].Name)
	}
	if newCfg.Nodes[1].Source != config.NodeSourceInline {
		t.Errorf("expected second node source to be inline, got %s", newCfg.Nodes[1].Source)
	}

	// Verify subscription nodes come after inline nodes
	if newCfg.Nodes[2].Name != "sub-node-1" {
		t.Errorf("expected third node to be sub-node-1, got %s", newCfg.Nodes[2].Name)
	}
	if newCfg.Nodes[2].Source != config.NodeSourceSubscription {
		t.Errorf("expected third node source to be subscription, got %s", newCfg.Nodes[2].Source)
	}

	if newCfg.Nodes[3].Name != "sub-node-2" {
		t.Errorf("expected fourth node to be sub-node-2, got %s", newCfg.Nodes[3].Name)
	}
	if newCfg.Nodes[3].Source != config.NodeSourceSubscription {
		t.Errorf("expected fourth node source to be subscription, got %s", newCfg.Nodes[3].Source)
	}
}

func TestCreateNewConfig_OnlySubscriptionNodes(t *testing.T) {
	// Setup base config without inline nodes
	baseCfg := &config.Config{
		Mode:  "pool",
		Nodes: []config.NodeConfig{},
	}

	mgr := &Manager{
		baseCfg: baseCfg,
	}

	// Subscription nodes
	subNodes := []config.NodeConfig{
		{Name: "sub-node-1", URI: "ss://sub1@example.com:8390"},
		{Name: "sub-node-2", URI: "ss://sub2@example.com:8391"},
	}

	// Create new config
	newCfg := mgr.createNewConfig(subNodes)

	// Verify only subscription nodes exist
	if len(newCfg.Nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(newCfg.Nodes))
	}

	for i, node := range newCfg.Nodes {
		if node.Source != config.NodeSourceSubscription {
			t.Errorf("node %d: expected source to be subscription, got %s", i, node.Source)
		}
	}
}

func TestCreateNewConfig_SubscriptionSourceMarked(t *testing.T) {
	baseCfg := &config.Config{
		Mode:  "pool",
		Nodes: []config.NodeConfig{},
	}

	mgr := &Manager{
		baseCfg: baseCfg,
	}

	// Subscription nodes without source set
	subNodes := []config.NodeConfig{
		{Name: "sub-node-1", URI: "ss://sub1@example.com:8390"},
	}

	newCfg := mgr.createNewConfig(subNodes)

	// Verify source is set to subscription
	if newCfg.Nodes[0].Source != config.NodeSourceSubscription {
		t.Errorf("expected source to be subscription, got %s", newCfg.Nodes[0].Source)
	}
}

func TestCreateNewConfig_AppliesPersistedSubscriptionPoolPrefs(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("mode: hybrid\nnodes: []\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	oldURI := "vless://uuid-a@example.com:443?type=ws&security=tls#Old"
	newURI := "vless://uuid-a@example.com:443?security=tls&type=ws#New"
	poolEnabled := false
	prefs := map[string]config.NodePrefs{
		(&config.NodeConfig{URI: oldURI}).NodeKey(): {PoolEnabled: &poolEnabled},
	}
	data, err := json.Marshal(prefs)
	if err != nil {
		t.Fatalf("encode prefs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node_prefs.json"), data, 0o644); err != nil {
		t.Fatalf("write prefs: %v", err)
	}

	baseCfg := &config.Config{Mode: "hybrid"}
	baseCfg.SetFilePath(cfgPath)
	mgr := &Manager{baseCfg: baseCfg}

	newCfg := mgr.createNewConfig([]config.NodeConfig{{Name: "New", URI: newURI}})
	if len(newCfg.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(newCfg.Nodes))
	}
	if newCfg.Nodes[0].PoolEnabled == nil || *newCfg.Nodes[0].PoolEnabled {
		t.Fatalf("PoolEnabled = %v, want false", newCfg.Nodes[0].PoolEnabled)
	}
}
