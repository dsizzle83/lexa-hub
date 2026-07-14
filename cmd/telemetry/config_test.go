package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeConfig writes a config JSON body to a temp file and loads it.
func writeConfig(t *testing.T, body string) *Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "telemetry.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	return cfg
}

// TestConfig_PostVarPostWh_DefaultTrue pins the WP-5 flag defaults: a config
// that predates the keys (absent post_var/post_wh) posts the new quantity
// rows — nil/absent ⇒ true, the same *bool pattern as cmd/api's TLS/MDNS.
func TestConfig_PostVarPostWh_DefaultTrue(t *testing.T) {
	cfg := writeConfig(t, `{"server":"69.0.0.20:11111"}`)
	if cfg.PostVar != nil || cfg.PostWh != nil {
		t.Fatal("absent post_var/post_wh keys must load as nil (default-true sentinel)")
	}
	if !cfg.PostVarEnabled() {
		t.Error("PostVarEnabled() = false with key absent, want true (product default)")
	}
	if !cfg.PostWhEnabled() {
		t.Error("PostWhEnabled() = false with key absent, want true (product default)")
	}
}

// TestConfig_PostVarPostWh_ExplicitFalse pins the opt-out: a bench posting to
// an older stand-in server can switch each row set off independently.
func TestConfig_PostVarPostWh_ExplicitFalse(t *testing.T) {
	cfg := writeConfig(t, `{"server":"69.0.0.20:11111","post_var":false,"post_wh":false}`)
	if cfg.PostVarEnabled() {
		t.Error("PostVarEnabled() = true with post_var:false")
	}
	if cfg.PostWhEnabled() {
		t.Error("PostWhEnabled() = true with post_wh:false")
	}
}

// TestConfig_PostVarPostWh_ExplicitTrue covers the redundant-but-legal
// spelled-out form the shipped example config uses.
func TestConfig_PostVarPostWh_ExplicitTrue(t *testing.T) {
	cfg := writeConfig(t, `{"server":"69.0.0.20:11111","post_var":true,"post_wh":true}`)
	if !cfg.PostVarEnabled() {
		t.Error("PostVarEnabled() = false with post_var:true")
	}
	if !cfg.PostWhEnabled() {
		t.Error("PostWhEnabled() = false with post_wh:true")
	}
}

// TestConfig_ExampleConfig_PostsAllQuantities pins the shipped example
// (configs/telemetry.json): the bench deploys it as-is, and it must post the
// full WP-5 quantity set.
func TestConfig_ExampleConfig_PostsAllQuantities(t *testing.T) {
	cfg, err := loadConfig(repoPath(t, "configs", "telemetry.json"))
	if err != nil {
		t.Fatalf("loadConfig(example config): %v", err)
	}
	if !cfg.PostVarEnabled() || !cfg.PostWhEnabled() {
		t.Fatal("example config must enable post_var and post_wh (fixture drifted?)")
	}
}
