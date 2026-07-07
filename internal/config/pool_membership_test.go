package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testBoolPtr(v bool) *bool {
	return &v
}

func TestSaveNodes_PersistsFileNodePoolEnabledToSidecar(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	nodesPath := filepath.Join(dir, "nodes.txt")
	if err := os.WriteFile(cfgPath, []byte("mode: hybrid\nnodes_file: nodes.txt\nnodes: []\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	uri := "socks5://127.0.0.1:1080#Premium"
	cfg := &Config{
		Mode:      "hybrid",
		NodesFile: nodesPath,
		Nodes: []NodeConfig{
			{
				Name:        "Premium",
				URI:         uri,
				PoolEnabled: testBoolPtr(false),
				Source:      NodeSourceSubscription,
			},
		},
	}
	cfg.SetFilePath(cfgPath)

	if err := cfg.SaveNodes(); err != nil {
		t.Fatalf("SaveNodes: %v", err)
	}

	nodesData, err := os.ReadFile(nodesPath)
	if err != nil {
		t.Fatalf("read nodes: %v", err)
	}
	if got := string(nodesData); got != uri+"\n" {
		t.Fatalf("nodes.txt = %q, want URI only", got)
	}

	prefsData, err := os.ReadFile(filepath.Join(dir, nodePrefsFile))
	if err != nil {
		t.Fatalf("read prefs: %v", err)
	}
	var prefs map[string]NodePrefs
	if err := json.Unmarshal(prefsData, &prefs); err != nil {
		t.Fatalf("decode prefs: %v", err)
	}
	pref := prefs[stableNodeKey(uri)]
	if pref.PoolEnabled == nil || *pref.PoolEnabled {
		t.Fatalf("pool preference = %#v, want false", pref.PoolEnabled)
	}
}

func TestLoad_AppliesNodePrefsAcrossRenameAndQueryReorder(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	nodesPath := filepath.Join(dir, "nodes.txt")
	if err := os.WriteFile(cfgPath, []byte("mode: pool\nnodes_file: nodes.txt\nnodes: []\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	oldURI := "vless://uuid-a@example.com:443?type=ws&security=tls#Old"
	newURI := "vless://uuid-a@example.com:443?security=tls&type=ws#New"
	if err := os.WriteFile(nodesPath, []byte(newURI+"\n"), 0o644); err != nil {
		t.Fatalf("write nodes: %v", err)
	}
	prefs := map[string]NodePrefs{
		stableNodeKey(oldURI): {PoolEnabled: testBoolPtr(false)},
	}
	prefsData, err := json.Marshal(prefs)
	if err != nil {
		t.Fatalf("encode prefs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, nodePrefsFile), prefsData, 0o644); err != nil {
		t.Fatalf("write prefs: %v", err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(cfg.Nodes))
	}
	if cfg.Nodes[0].PoolEnabled == nil || *cfg.Nodes[0].PoolEnabled {
		t.Fatalf("PoolEnabled = %v, want false", cfg.Nodes[0].PoolEnabled)
	}
}

func TestLoad_AppliesNodePrefsToCachedSubscriptionNodes(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	nodesPath := filepath.Join(dir, "nodes.txt")
	if err := os.WriteFile(cfgPath, []byte("mode: pool\nnodes_file: nodes.txt\nsubscriptions:\n  - http://127.0.0.1:1/sub\nnodes: []\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	uri := "socks5://127.0.0.1:1080#Cached"
	if err := os.WriteFile(nodesPath, []byte(uri+"\n"), 0o644); err != nil {
		t.Fatalf("write nodes: %v", err)
	}
	prefs := map[string]NodePrefs{
		stableNodeKey(uri): {PoolEnabled: testBoolPtr(false)},
	}
	prefsData, err := json.Marshal(prefs)
	if err != nil {
		t.Fatalf("encode prefs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, nodePrefsFile), prefsData, 0o644); err != nil {
		t.Fatalf("write prefs: %v", err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(cfg.Nodes))
	}
	if cfg.Nodes[0].Source != NodeSourceSubscription {
		t.Fatalf("Source = %q, want %q", cfg.Nodes[0].Source, NodeSourceSubscription)
	}
	if cfg.Nodes[0].PoolEnabled == nil || *cfg.Nodes[0].PoolEnabled {
		t.Fatalf("PoolEnabled = %v, want false", cfg.Nodes[0].PoolEnabled)
	}
}

func TestSaveNodePrefs_PrunesRemovedNodes(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("mode: pool\nnodes: []\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	currentURI := "socks5://127.0.0.1:1080#Current"
	cfg := &Config{
		Mode: "pool",
		Nodes: []NodeConfig{
			{URI: currentURI, PoolEnabled: testBoolPtr(false), Source: NodeSourceSubscription},
		},
	}
	cfg.SetFilePath(cfgPath)

	if err := os.WriteFile(filepath.Join(dir, nodePrefsFile), []byte(`{"stale":{"pool_enabled":false}}`), 0o644); err != nil {
		t.Fatalf("write stale prefs: %v", err)
	}
	if err := cfg.SaveNodePrefs(); err != nil {
		t.Fatalf("SaveNodePrefs: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, nodePrefsFile))
	if err != nil {
		t.Fatalf("read prefs: %v", err)
	}
	var prefs map[string]NodePrefs
	if err := json.Unmarshal(data, &prefs); err != nil {
		t.Fatalf("decode prefs: %v", err)
	}
	if _, ok := prefs["stale"]; ok {
		t.Fatalf("stale preference was not pruned: %v", prefs)
	}
	if _, ok := prefs[stableNodeKey(currentURI)]; !ok {
		t.Fatalf("current preference missing: %v", prefs)
	}
}

func TestSaveNodes_PreservesInlinePoolEnabled(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("mode: hybrid\nnodes: []\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg := &Config{
		Mode: "hybrid",
		Nodes: []NodeConfig{
			{
				Name:        "Premium",
				URI:         "socks5://127.0.0.1:1080#Premium",
				PoolEnabled: testBoolPtr(false),
				Source:      NodeSourceInline,
			},
		},
	}
	cfg.SetFilePath(cfgPath)

	if err := cfg.SaveNodes(); err != nil {
		t.Fatalf("SaveNodes: %v", err)
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(data), "pool_enabled: false") {
		t.Fatalf("expected pool_enabled to be persisted, got:\n%s", data)
	}
}

func TestSaveSettings_PreservesPoolExcludeKeywords(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("mode: pool\nnodes: []\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg := &Config{
		Mode: "pool",
		Pool: PoolConfig{
			Mode:            "sequential",
			ExcludeKeywords: []string{"Premium", "HighRate"},
		},
	}
	cfg.SetFilePath(cfgPath)

	if err := cfg.SaveSettings(); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "exclude_keywords:") ||
		!strings.Contains(content, "- Premium") ||
		!strings.Contains(content, "- HighRate") {
		t.Fatalf("expected exclude_keywords to be persisted, got:\n%s", content)
	}
}
