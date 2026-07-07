package boxmgr

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"easy_proxies/internal/config"
	"easy_proxies/internal/monitor"
)

// TestCreateNode_AlwaysInlineEvenWithSubscription is the regression test for the
// "WebUI-added node lost after subscription refresh" bug (issue #29). A node
// added through the WebUI is an explicit user configuration and must be stored
// as an inline node in config.yaml, even when subscriptions are configured.
// Classifying it as a subscription/file source routed it to nodes.txt, which the
// next subscription refresh overwrites — silently dropping the node.
func TestCreateNode_AlwaysInlineEvenWithSubscription(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	// Minimal on-disk config with a subscription configured. SaveNodes reads this
	// file back to preserve structure, so it must exist.
	if err := os.WriteFile(cfgPath, []byte(`mode: pool
subscriptions:
  - https://example.com/sub
nodes: []
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg := &config.Config{
		Mode:          "pool",
		Subscriptions: []string{"https://example.com/sub"},
		Nodes:         []config.NodeConfig{},
	}
	cfg.SetFilePath(cfgPath)

	m := New(cfg, monitor.Config{})

	created, err := m.CreateNode(context.Background(), config.NodeConfig{
		Name: "ManualNode",
		URI:  "vless://uuid-a@a.example.com:443?type=ws&security=tls",
	})
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	if created.Source != config.NodeSourceInline {
		t.Errorf("WebUI-added node source = %q, want %q (must survive subscription refresh)",
			created.Source, config.NodeSourceInline)
	}

	// The node must be persisted as an inline node in config.yaml, not nodes.txt.
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config back: %v", err)
	}
	if !strings.Contains(string(data), "a.example.com") {
		t.Errorf("config.yaml should contain the inline node URI, got:\n%s", data)
	}
}

func TestCreateNode_PreservesPoolEnabled(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("mode: hybrid\nnodes: []\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg := &config.Config{
		Mode:  "hybrid",
		Nodes: []config.NodeConfig{},
	}
	cfg.SetFilePath(cfgPath)

	m := New(cfg, monitor.Config{})
	poolEnabled := false
	created, err := m.CreateNode(context.Background(), config.NodeConfig{
		Name:        "Premium",
		URI:         "socks5://127.0.0.1:1080#Premium",
		PoolEnabled: &poolEnabled,
	})
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	if created.PoolEnabled == nil || *created.PoolEnabled {
		t.Fatalf("created PoolEnabled = %v, want false", created.PoolEnabled)
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config back: %v", err)
	}
	if !strings.Contains(string(data), "pool_enabled: false") {
		t.Errorf("config.yaml should contain pool_enabled: false, got:\n%s", data)
	}
}

func TestUpdateSubscriptionNode_PersistsPoolEnabledToSidecar(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	nodesPath := filepath.Join(dir, "nodes.txt")
	if err := os.WriteFile(cfgPath, []byte("mode: hybrid\nnodes_file: nodes.txt\nnodes: []\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	uri := "socks5://127.0.0.1:1080#Premium"
	cfg := &config.Config{
		Mode:      "hybrid",
		NodesFile: nodesPath,
		Nodes: []config.NodeConfig{
			{Name: "Premium", URI: uri, Source: config.NodeSourceSubscription},
		},
	}
	cfg.SetFilePath(cfgPath)

	m := New(cfg, monitor.Config{})
	poolEnabled := false
	updated, err := m.UpdateNode(context.Background(), "Premium", config.NodeConfig{
		Name:        "Premium",
		URI:         uri,
		PoolEnabled: &poolEnabled,
	})
	if err != nil {
		t.Fatalf("UpdateNode: %v", err)
	}
	if updated.Source != config.NodeSourceSubscription {
		t.Fatalf("source = %q, want subscription", updated.Source)
	}

	nodesData, err := os.ReadFile(nodesPath)
	if err != nil {
		t.Fatalf("read nodes: %v", err)
	}
	if got := string(nodesData); got != uri+"\n" {
		t.Fatalf("nodes.txt = %q, want URI only", got)
	}

	prefsData, err := os.ReadFile(filepath.Join(dir, "node_prefs.json"))
	if err != nil {
		t.Fatalf("read prefs: %v", err)
	}
	var prefs map[string]config.NodePrefs
	if err := json.Unmarshal(prefsData, &prefs); err != nil {
		t.Fatalf("decode prefs: %v", err)
	}
	pref := prefs[(&config.NodeConfig{URI: uri}).NodeKey()]
	if pref.PoolEnabled == nil || *pref.PoolEnabled {
		t.Fatalf("pool preference = %#v, want false", pref.PoolEnabled)
	}
}
