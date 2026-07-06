package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lexa-hub/internal/orchestrator"
)

// TestLoadConfig_JournalBlockAbsent verifies TASK-040's rollout default:
// a config file with no "journal" key leaves cfg.Journal nil — the
// no-op-everywhere gate every emit site in this package relies on.
func TestLoadConfig_JournalBlockAbsent(t *testing.T) {
	path := writeTempConfig(t, `{"mqtt_broker":"tcp://localhost:1883"}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Journal != nil {
		t.Fatalf("Journal = %+v, want nil (absent block)", cfg.Journal)
	}
}

// TestLoadConfig_JournalBlockParses verifies the snake_case "journal" block
// (as shipped in configs/hub.json) decodes into JournalConfig correctly,
// and ToLibrary carries every field through to journal.Config.
func TestLoadConfig_JournalBlockParses(t *testing.T) {
	path := writeTempConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"journal": {
			"dir": "/var/lib/lexa/journal/hub",
			"max_bytes": 1048576,
			"max_files": 4,
			"flush_every": 32,
			"flush_interval_s": 5
		}
	}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Journal == nil {
		t.Fatal("Journal = nil, want the parsed block")
	}
	if cfg.Journal.Dir != "/var/lib/lexa/journal/hub" || cfg.Journal.MaxBytes != 1048576 ||
		cfg.Journal.MaxFiles != 4 || cfg.Journal.FlushEvery != 32 || cfg.Journal.FlushIntervalS != 5 {
		t.Fatalf("JournalConfig = %+v, want the literal JSON values", cfg.Journal)
	}
	lib := cfg.Journal.ToLibrary()
	if lib.Dir != cfg.Journal.Dir || lib.MaxBytes != 1048576 || lib.MaxFiles != 4 ||
		lib.FlushEvery != 32 || lib.FlushInterval != 5*time.Second {
		t.Fatalf("ToLibrary() = %+v, want fields carried through with FlushIntervalS scaled to a Duration", lib)
	}
}

// TestLoadConfig_PlantBlocksAbsent verifies the TASK-057 unwired default: a
// legacy config with no "plant" keys still loads and leaves every decoded plant
// field zero (bench defaults are applied at consume time in TASK-064, not here).
func TestLoadConfig_PlantBlocksAbsent(t *testing.T) {
	path := writeTempConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"devices": [
			{"name": "inverter-0", "role": "inverter", "max_w": 10000},
			{"name": "battery-0",  "role": "battery",  "max_w": 5000}
		],
		"stations": [{"id": "cs-001", "max_current_a": 32.0}]
	}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Devices[0].InverterPlant != (orchestrator.InverterPlant{}) {
		t.Fatalf("InverterPlant = %+v, want zero (no plant block)", cfg.Devices[0].InverterPlant)
	}
	// BatteryPlant holds a slice (not != comparable); check the scalar fields.
	if bp := cfg.Devices[1].BatteryPlant; bp.CapacityKWh != 0 || bp.SOCTaperStartPct != 0 ||
		bp.ConvergeFrac != 0 || bp.ControlLatencyS != 0 || bp.TaperCurve != nil {
		t.Fatalf("BatteryPlant = %+v, want zero (no plant block)", bp)
	}
}

// TestLoadConfig_PlantBlocksParse verifies the shipped configs/hub.json shape:
// each device/station "plant" object decodes into the role-matched typed field.
func TestLoadConfig_PlantBlocksParse(t *testing.T) {
	path := writeTempConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"devices": [
			{"name": "inverter-0", "role": "inverter", "max_w": 10000,
			 "plant": {"max_ramp_down_w_per_s": 500, "max_ramp_up_w_per_s": 166.6666666667, "control_latency_s": 3}},
			{"name": "battery-0", "role": "battery", "max_w": 5000,
			 "plant": {"capacity_kwh": 10, "soc_taper_start_pct": 80, "converge_frac": 0.5, "control_latency_s": 3}},
			{"name": "meter-0", "role": "meter", "max_w": 0,
			 "plant": {"meter_lag_s": 5}}
		],
		"stations": [{"id": "cs-001", "max_current_a": 32.0, "plant": {"meter_lag_s": 10}}]
	}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Devices[0].InverterPlant.MaxRampDownWPerS != 500 || cfg.Devices[0].InverterPlant.ControlLatencyS != 3 {
		t.Fatalf("InverterPlant = %+v, want the JSON values", cfg.Devices[0].InverterPlant)
	}
	if cfg.Devices[1].BatteryPlant.CapacityKWh != 10 || cfg.Devices[1].BatteryPlant.ConvergeFrac != 0.5 {
		t.Fatalf("BatteryPlant = %+v, want the JSON values", cfg.Devices[1].BatteryPlant)
	}
	if cfg.Devices[2].MeterPlant.MeterLagS != 5 {
		t.Fatalf("MeterPlant = %+v, want meter_lag_s 5", cfg.Devices[2].MeterPlant)
	}
	if cfg.Stations[0].EVSEPlant.MeterLagS != 10 {
		t.Fatalf("EVSEPlant = %+v, want meter_lag_s 10", cfg.Stations[0].EVSEPlant)
	}
}

// TestLoadConfig_PlantUnknownKeyTolerated verifies the 05 §6 rule: an unknown
// key inside a plant block warns but does not fail the load, and the known
// keys still decode.
func TestLoadConfig_PlantUnknownKeyTolerated(t *testing.T) {
	path := writeTempConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"devices": [
			{"name": "inverter-0", "role": "inverter", "max_w": 10000,
			 "plant": {"max_ramp_down_w_per_s": 500, "bogus_future_key": 123}}
		]
	}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig must tolerate unknown plant keys, got: %v", err)
	}
	if cfg.Devices[0].InverterPlant.MaxRampDownWPerS != 500 {
		t.Fatalf("known key dropped alongside unknown one: %+v", cfg.Devices[0].InverterPlant)
	}
}

// TestLoadConfig_PlantTypeMismatchFails verifies a genuine value-type error in
// a plant block IS surfaced (authoring bug in a new-style file), not silently
// defaulted.
func TestLoadConfig_PlantTypeMismatchFails(t *testing.T) {
	path := writeTempConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"devices": [
			{"name": "inverter-0", "role": "inverter", "max_w": 10000,
			 "plant": {"max_ramp_down_w_per_s": "not-a-number"}}
		]
	}`)
	if _, err := loadConfig(path); err == nil {
		t.Fatal("loadConfig should fail on a type-mismatched plant value")
	}
}

// writeTempConfig writes body to a temp file and returns its path — a tiny
// helper so the two loadConfig tests above don't each hand-roll os.CreateTemp.
func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cfg.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &probe); err != nil {
		t.Fatalf("test fixture is not valid JSON: %v", err)
	}
	return path
}
