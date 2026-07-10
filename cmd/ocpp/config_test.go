package main

import (
	"encoding/json"
	"fmt"
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

// Unit 6.1 amendment (2026-07-09): the SP2-blank refusal only applies when a
// config would actually SERVE chargers. This fixture's whole point is that
// dangerous case, so it now explicitly configures a station (the omission
// was incidental — the station-awareness carve-out didn't exist yet when
// this test was written) rather than relying on the pre-amendment behavior
// of refusing regardless of station count. See
// TestLoadConfig_UncommissionedIdleTruthTable for the full 8-combination
// pin, including this one.
func TestLoadConfig_ProductProfile_BlankSP2Fails(t *testing.T) {
	path := writeTempConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"reconciler": "active",
		"stations": [{"id": "cs-001"}]
	}`)
	_, err := loadConfig(path)
	if err == nil {
		t.Fatal("loadConfig succeeded with blank SP2 fields, a configured station, and no bench profile; want a fail-closed error")
	}
	for _, want := range []string{"cert_path", "key_path", "basic_auth_user", "basic_auth_pass", "bench"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

func TestLoadConfig_ProductProfile_PartiallyBlankSP2Fails(t *testing.T) {
	// A realistic near-miss: cert/key set but Basic Auth still blank. A
	// station is configured (Unit 6.1 amendment: the refusal only applies
	// when this config would actually serve chargers).
	path := writeTempConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"reconciler": "active",
		"cert_path": "/etc/lexa/certs/ocpp-cert.pem",
		"key_path": "/etc/lexa/certs/ocpp-key.pem",
		"stations": [{"id": "cs-001"}]
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
	// A station is configured (Unit 6.1 amendment: the refusal only applies
	// when this config would actually serve chargers) — otherwise this would
	// now be the uncommissioned-idle case instead of a refusal.
	path := writeTempConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"reconciler": "active",
		"stations": [{"id": "cs-001"}]
	}`)
	if _, err := loadConfig(path); err == nil {
		t.Fatal("loadConfig succeeded with OCPP_PROFILE=production, a configured station, and blank SP2 fields; want a fail-closed error")
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

// Unit 6.1 amendment (2026-07-09) SUPERSEDES this test's original premise:
// before the amendment, SP2 fail-closed applied "independent of whether any
// station is present," so this exact fixture (no stations, no bench, blank
// SP2) used to be refused. That is now precisely the wrong outcome: zero
// stations + not bench is the uncommissioned-idle state
// (docs/DEVICE_ROADMAP.md §6/§9) that must load successfully so
// configs/factory/ocpp.json is loadable — main() simply never binds the
// CSMS listener in this state, so there is no open ws:// surface to protect
// by refusing here. See TestLoadConfig_UncommissionedIdleTruthTable for the
// full 8-combination pin, including the two refusal cases that are
// UNCHANGED (stations configured, blank SP2, not bench).
func TestLoadConfig_NoStations_NotBench_BlankSP2_IsUncommissionedIdle(t *testing.T) {
	path := writeTempConfig(t, `{"mqtt_broker": "tcp://localhost:1883"}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig failed with no stations, no bench profile, blank SP2 (uncommissioned idle): %v", err)
	}
	if !uncommissionedIdle(cfg) {
		t.Error("uncommissionedIdle(cfg) = false, want true for zero stations + not bench")
	}
}

// TestLoadConfig_UncommissionedIdleTruthTable enumerates all eight
// (stations empty/configured) × (SP2 blank/full) × (bench false/true)
// combinations (Unit 6.1 amendment). The two "stations configured, SP2
// blank, bench false" combinations are the pre-existing WS-1 refusal cases
// and must remain UNCHANGED refusals; every other combination must load,
// and uncommissionedIdle must read true exactly when stations are empty and
// bench is not set (independent of SP2 field state).
func TestLoadConfig_UncommissionedIdleTruthTable(t *testing.T) {
	blankSP2 := ``
	fullSP2 := `,
		"cert_path": "/etc/lexa/certs/ocpp-cert.pem",
		"key_path": "/etc/lexa/certs/ocpp-key.pem",
		"basic_auth_user": "evse-bench",
		"basic_auth_pass": "s3cret"`
	withStation := `,
		"reconciler": "active",
		"stations": [{"id": "cs-001"}]`
	noStations := ``

	cases := []struct {
		name     string
		stations string
		sp2      string
		bench    bool
		wantErr  bool
		wantIdle bool // only checked when wantErr is false
	}{
		{"noStations_blankSP2_notBench", noStations, blankSP2, false, false, true},
		{"noStations_blankSP2_bench", noStations, blankSP2, true, false, false},
		{"noStations_fullSP2_notBench", noStations, fullSP2, false, false, true},
		{"noStations_fullSP2_bench", noStations, fullSP2, true, false, false},
		{"stations_blankSP2_notBench", withStation, blankSP2, false, true, false},
		{"stations_blankSP2_bench", withStation, blankSP2, true, false, false},
		{"stations_fullSP2_notBench", withStation, fullSP2, false, false, false},
		{"stations_fullSP2_bench", withStation, fullSP2, true, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{
				"mqtt_broker": "tcp://localhost:1883",
				"bench": %t%s%s
			}`, tc.bench, tc.sp2, tc.stations)
			path := writeTempConfig(t, body)
			cfg, err := loadConfig(path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("loadConfig succeeded, want a fail-closed error (fixture: %s)", body)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig failed, want success: %v (fixture: %s)", err, body)
			}
			if got := uncommissionedIdle(cfg); got != tc.wantIdle {
				t.Errorf("uncommissionedIdle(cfg) = %v, want %v (fixture: %s)", got, tc.wantIdle, body)
			}
		})
	}
}

// TestLoadConfig_FactoryProfileLoadsAndIsIdle is the acceptance test for the
// Unit 1.6 dependency (docs/extension/00_PROGRESS.md, configs/factory/
// README.md "Known gaps" #2): the factory ocpp.json profile (stations: [],
// every SP2 field blank, bench: false) was deliberately shipped in
// TARGET (not yet loadable) state until this unit's uncommissioned-idle
// gate landed. It must now load without error and be recognized as
// uncommissioned-idle.
func TestLoadConfig_FactoryProfileLoadsAndIsIdle(t *testing.T) {
	cfg, err := loadConfig("../../configs/factory/ocpp.json")
	if err != nil {
		t.Fatalf("loadConfig(configs/factory/ocpp.json) failed: %v", err)
	}
	if !uncommissionedIdle(cfg) {
		t.Error("uncommissionedIdle(cfg) = false for the factory profile, want true")
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
