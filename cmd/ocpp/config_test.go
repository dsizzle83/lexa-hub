package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// WS-1 (V1.0 punch list, security fail-closed by default): loadConfig must
// refuse to start with OCPP Security Profile 2 disabled (any of
// cert_path/key_path/basic_auth_user/basic_auth_pass blank) unless an
// explicit bench profile opts out — "bench": true in the config, or
// OCPP_PROFILE=bench in the environment.

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ocpp.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &probe); err != nil {
		t.Fatalf("test fixture is not valid JSON: %v", err)
	}
	return path
}

func TestLoadConfig_ProductProfile_BlankSP2Fails(t *testing.T) {
	path := writeTempConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"reconciler": "active"
	}`)
	_, err := loadConfig(path)
	if err == nil {
		t.Fatal("loadConfig succeeded with blank SP2 fields and no bench profile; want a fail-closed error")
	}
	for _, want := range []string{"cert_path", "key_path", "basic_auth_user", "basic_auth_pass", "bench"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

func TestLoadConfig_ProductProfile_PartiallyBlankSP2Fails(t *testing.T) {
	// A realistic near-miss: cert/key set but Basic Auth still blank.
	path := writeTempConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"reconciler": "active",
		"cert_path": "/etc/lexa/certs/ocpp-cert.pem",
		"key_path": "/etc/lexa/certs/ocpp-key.pem"
	}`)
	_, err := loadConfig(path)
	if err == nil {
		t.Fatal("loadConfig succeeded with basic_auth_user/basic_auth_pass blank; want a fail-closed error")
	}
	if !strings.Contains(err.Error(), "basic_auth_user") || !strings.Contains(err.Error(), "basic_auth_pass") {
		t.Errorf("error %q should name the still-blank basic_auth_user/basic_auth_pass fields", err.Error())
	}
	if strings.Contains(err.Error(), "cert_path,") || strings.Contains(err.Error(), " cert_path ") {
		t.Errorf("error %q should not re-flag the already-populated cert_path", err.Error())
	}
}

func TestLoadConfig_BenchConfigKey_BlankSP2Succeeds(t *testing.T) {
	path := writeTempConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"reconciler": "active",
		"bench": true
	}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig with bench:true failed: %v", err)
	}
	if !cfg.Bench {
		t.Fatal("cfg.Bench = false, want true")
	}
}

func TestLoadConfig_OCPPProfileEnv_BlankSP2Succeeds(t *testing.T) {
	t.Setenv("OCPP_PROFILE", "bench")
	path := writeTempConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"reconciler": "active"
	}`)
	if _, err := loadConfig(path); err != nil {
		t.Fatalf("loadConfig with OCPP_PROFILE=bench failed: %v", err)
	}
}

func TestLoadConfig_OCPPProfileEnv_WrongValueStillFails(t *testing.T) {
	t.Setenv("OCPP_PROFILE", "production") // anything other than exactly "bench"
	path := writeTempConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"reconciler": "active"
	}`)
	if _, err := loadConfig(path); err == nil {
		t.Fatal("loadConfig succeeded with OCPP_PROFILE=production and blank SP2 fields; want a fail-closed error")
	}
}

func TestLoadConfig_ProductProfile_FullSP2Succeeds(t *testing.T) {
	path := writeTempConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"reconciler": "active",
		"cert_path": "/etc/lexa/certs/ocpp-cert.pem",
		"key_path": "/etc/lexa/certs/ocpp-key.pem",
		"basic_auth_user": "evse-bench",
		"basic_auth_pass": "s3cret"
	}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig with full SP2 fields failed: %v", err)
	}
	if cfg.CertPath == "" || cfg.KeyPath == "" || cfg.BasicAuthUser == "" || cfg.BasicAuthPass == "" {
		t.Fatalf("SP2 fields not carried through: %+v", cfg)
	}
}

// No stations configured ⇒ the pre-existing reconciler-empty-ok path; SP2
// fail-closed still applies independent of whether any station is present.
func TestLoadConfig_ProductProfile_NoStations_BlankSP2StillFails(t *testing.T) {
	path := writeTempConfig(t, `{"mqtt_broker": "tcp://localhost:1883"}`)
	if _, err := loadConfig(path); err == nil {
		t.Fatal("loadConfig succeeded with no stations, no bench profile, blank SP2; want a fail-closed error")
	}
}

// WS-9.1: mqtt_deaf_restart_after_s defaults to 300s (5 min) when unset, and
// carries through an explicit override.
func TestLoadConfig_MQTTDeafRestartAfter_Default(t *testing.T) {
	path := writeTempConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"bench": true
	}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.MQTTDeafRestartAfterS != 300 {
		t.Fatalf("MQTTDeafRestartAfterS default = %d, want 300", cfg.MQTTDeafRestartAfterS)
	}
	if got, want := cfg.MQTTDeafRestartAfter(), 300*time.Second; got != want {
		t.Fatalf("MQTTDeafRestartAfter() = %s, want %s", got, want)
	}
}

func TestLoadConfig_MQTTDeafRestartAfter_Override(t *testing.T) {
	path := writeTempConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"bench": true,
		"mqtt_deaf_restart_after_s": 60
	}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.MQTTDeafRestartAfterS != 60 {
		t.Fatalf("MQTTDeafRestartAfterS = %d, want 60", cfg.MQTTDeafRestartAfterS)
	}
}
