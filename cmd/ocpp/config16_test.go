package main

import (
	"strings"
	"testing"
)

// WP-12 config gate: port_16 (default 0 = 1.6J disabled) reuses the SAME
// cert/key + basic-auth fields as the 2.0.1 listener, so the WS-1 SP2
// fail-closed gate covers both listeners — these tests pin that enabling
// port_16 never opens a path around it.

// TestLoadConfig_Port16_DefaultDisabled: an absent port_16 stays 0 — the 1.6
// listener is never built (zero regression surface).
func TestLoadConfig_Port16_DefaultDisabled(t *testing.T) {
	path := writeTempConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"bench": true
	}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Port16 != 0 {
		t.Fatalf("Port16 default = %d, want 0 (disabled)", cfg.Port16)
	}
}

// TestLoadConfig_Port16_EnabledWithoutSP2Fails: port_16 > 0 with stations
// configured and no bench profile must be refused when the SP2 fields are
// blank — the same fail-closed gate as the 2.0.1 listener, since the 1.6
// listener would serve chargers off the same blank cert/auth fields.
func TestLoadConfig_Port16_EnabledWithoutSP2Fails(t *testing.T) {
	path := writeTempConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"reconciler": "active",
		"port_16": 8886,
		"stations": [{"id": "cp-001"}]
	}`)
	_, err := loadConfig(path)
	if err == nil {
		t.Fatal("loadConfig succeeded with port_16 enabled, blank SP2 fields, a configured station, and no bench profile; want a fail-closed error")
	}
	for _, want := range []string{"cert_path", "key_path", "basic_auth_user", "basic_auth_pass"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

// TestLoadConfig_Port16_BenchBlankSP2Succeeds: the bench escape hatch applies
// to the 1.6 listener exactly as it does to 2.0.1 (air-gapped LAN ws://).
func TestLoadConfig_Port16_BenchBlankSP2Succeeds(t *testing.T) {
	path := writeTempConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"reconciler": "active",
		"bench": true,
		"port_16": 8886,
		"stations": [{"id": "cp-001"}]
	}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig with bench:true and port_16 failed: %v", err)
	}
	if cfg.Port16 != 8886 {
		t.Fatalf("Port16 = %d, want 8886", cfg.Port16)
	}
}

// TestLoadConfig_Port16_FullSP2Succeeds: a product profile with the shared
// SP2 fields populated loads with the 1.6 listener enabled.
func TestLoadConfig_Port16_FullSP2Succeeds(t *testing.T) {
	path := writeTempConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"reconciler": "active",
		"port_16": 8886,
		"cert_path": "/etc/lexa/certs/ocpp-cert.pem",
		"key_path": "/etc/lexa/certs/ocpp-key.pem",
		"basic_auth_user": "evse-bench",
		"basic_auth_pass": "s3cret",
		"stations": [{"id": "cp-001"}]
	}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig with full SP2 + port_16 failed: %v", err)
	}
	if cfg.Port16 != 8886 {
		t.Fatalf("Port16 = %d, want 8886", cfg.Port16)
	}
}

// TestLoadConfig_Port16_ZeroIsIgnored: an explicit port_16: 0 is the disabled
// state — otherwise-valid product configs load exactly as before WP-12.
func TestLoadConfig_Port16_ZeroIsIgnored(t *testing.T) {
	path := writeTempConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"reconciler": "active",
		"port_16": 0,
		"cert_path": "/etc/lexa/certs/ocpp-cert.pem",
		"key_path": "/etc/lexa/certs/ocpp-key.pem",
		"basic_auth_user": "evse-bench",
		"basic_auth_pass": "s3cret",
		"stations": [{"id": "cp-001"}]
	}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig with port_16: 0 failed: %v", err)
	}
	if cfg.Port16 != 0 {
		t.Fatalf("Port16 = %d, want 0", cfg.Port16)
	}
}

// TestLoadConfig_Port16_UncommissionedIdleStillLoads: zero stations + not
// bench is the uncommissioned-idle state even with port_16 set — main()
// never binds EITHER listener there, so the config must load (the factory-
// profile discipline, Unit 6.1).
func TestLoadConfig_Port16_UncommissionedIdleStillLoads(t *testing.T) {
	path := writeTempConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"port_16": 8886
	}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig failed for uncommissioned-idle with port_16 set: %v", err)
	}
	if !uncommissionedIdle(cfg) {
		t.Error("uncommissionedIdle(cfg) = false, want true (no stations, not bench)")
	}
}
