package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// TestLoadConfig_RetainedAdoptionMaxAgeDefault verifies TASK-042's rollout
// default: a config file with no "retained_adoption_max_age_s" key gets 300s
// (RetainedAdoptionMaxAge() == 5m), not zero — an absent/zero bound must
// never silently disable the staleness check.
func TestLoadConfig_RetainedAdoptionMaxAgeDefault(t *testing.T) {
	path := writeTempConfig(t, `{"mqtt_broker":"tcp://localhost:1883"}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.RetainedAdoptionMaxAgeS != 300 {
		t.Fatalf("RetainedAdoptionMaxAgeS = %d, want default 300", cfg.RetainedAdoptionMaxAgeS)
	}
	if cfg.RetainedAdoptionMaxAge() != 5*time.Minute {
		t.Fatalf("RetainedAdoptionMaxAge() = %s, want 5m", cfg.RetainedAdoptionMaxAge())
	}
}

// TestLoadConfig_RetainedAdoptionMaxAgeOverride verifies an explicit,
// positive value in the config file is honored instead of the default.
func TestLoadConfig_RetainedAdoptionMaxAgeOverride(t *testing.T) {
	path := writeTempConfig(t, `{"mqtt_broker":"tcp://localhost:1883","retained_adoption_max_age_s":60}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.RetainedAdoptionMaxAgeS != 60 {
		t.Fatalf("RetainedAdoptionMaxAgeS = %d, want 60 (explicit override)", cfg.RetainedAdoptionMaxAgeS)
	}
	if cfg.RetainedAdoptionMaxAge() != 60*time.Second {
		t.Fatalf("RetainedAdoptionMaxAge() = %s, want 60s", cfg.RetainedAdoptionMaxAge())
	}
}

// ── FIX-F: ConstraintModes / ResolveConstraintModes ─────────────────────────

// TestResolveConstraintModes_BackCompatAbsentMap pins config.go's absolute
// back-compat rule (TASK-060 §4): with no "constraint_modes" block,
// constraint_shadow alone decides — true resolves every key to "shadow"
// (today's TASK-059 behaviour, bit-identical), false to "off" (moot: cmd/hub
// never constructs the wrapper in that case, but the map must still be
// honest).
func TestResolveConstraintModes_BackCompatAbsentMap(t *testing.T) {
	allKeys := []string{"export", "gen", "import", "economics", "battery_safety"}

	shadowOn := &Config{ConstraintShadow: true}
	modes, err := shadowOn.ResolveConstraintModes()
	if err != nil {
		t.Fatalf("constraint_shadow=true, no map: %v", err)
	}
	for _, k := range allKeys {
		if modes[k] != ModeShadow {
			t.Errorf("constraint_shadow=true modes[%q] = %q, want shadow", k, modes[k])
		}
	}

	shadowOff := &Config{ConstraintShadow: false}
	modes, err = shadowOff.ResolveConstraintModes()
	if err != nil {
		t.Fatalf("constraint_shadow=false, no map: %v", err)
	}
	for _, k := range allKeys {
		if modes[k] != ModeOff {
			t.Errorf("constraint_shadow=false modes[%q] = %q, want off", k, modes[k])
		}
	}
}

// TestResolveConstraintModes_PresentMapOmittedKeyDefaultsOff pins the OTHER
// half of back-compat: once constraint_modes is present, an omitted key
// defaults to "off" (TASK-060 step 4's per-constraint flip default), NOT
// "shadow" — a partially-specified map must not silently shadow-enable
// constraints the author never named.
func TestResolveConstraintModes_PresentMapOmittedKeyDefaultsOff(t *testing.T) {
	cfg := &Config{
		ConstraintShadow: true,
		ConstraintModes:  map[string]ConstraintMode{"export": ModeShadow},
	}
	modes, err := cfg.ResolveConstraintModes()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if modes["export"] != ModeShadow {
		t.Errorf(`modes["export"] = %q, want shadow (explicit)`, modes["export"])
	}
	for _, k := range []string{"gen", "import", "economics", "battery_safety"} {
		if modes[k] != ModeOff {
			t.Errorf("modes[%q] = %q, want off (omitted from a present map)", k, modes[k])
		}
	}
}

// TestResolveConstraintModes_ValidationTable is the deliverable-5e table
// test: every unknown-key/unknown-value/active-without-shadow validation
// path fails loud, and every valid combination resolves cleanly.
func TestResolveConstraintModes_ValidationTable(t *testing.T) {
	cases := []struct {
		name             string
		constraintShadow bool
		modes            map[string]ConstraintMode
		wantErr          bool
		wantErrSubstring string
	}{
		{
			name:             "nil map, shadow on",
			constraintShadow: true,
			modes:            nil,
			wantErr:          false,
		},
		{
			name:             "nil map, shadow off",
			constraintShadow: false,
			modes:            nil,
			wantErr:          false,
		},
		{
			name:             "all off, shadow on",
			constraintShadow: true,
			modes: map[string]ConstraintMode{
				"export": ModeOff, "gen": ModeOff, "import": ModeOff,
				"economics": ModeOff, "battery_safety": ModeOff,
			},
			wantErr: false,
		},
		{
			name:             "mixed shadow/active with shadow on",
			constraintShadow: true,
			modes: map[string]ConstraintMode{
				"export": ModeActive, "gen": ModeShadow, "import": ModeOff,
			},
			wantErr: false,
		},
		{
			name:             "every key active with shadow on",
			constraintShadow: true,
			modes: map[string]ConstraintMode{
				"export": ModeActive, "gen": ModeActive, "import": ModeActive,
				"economics": ModeActive, "battery_safety": ModeActive,
			},
			wantErr: false,
		},
		{
			name:             "unknown key",
			constraintShadow: true,
			modes:            map[string]ConstraintMode{"exprot": ModeShadow},
			wantErr:          true,
			wantErrSubstring: "unknown key",
		},
		{
			name:             "unknown value",
			constraintShadow: true,
			modes:            map[string]ConstraintMode{"export": "aggressive"},
			wantErr:          true,
			wantErrSubstring: "unknown mode",
		},
		{
			name:             "active without constraint_shadow",
			constraintShadow: false,
			modes:            map[string]ConstraintMode{"export": ModeActive},
			wantErr:          true,
			wantErrSubstring: "requires constraint_shadow=true",
		},
		{
			name:             "one active key legal, sibling active key without shadow still fails (shadow is process-wide)",
			constraintShadow: false,
			modes:            map[string]ConstraintMode{"export": ModeShadow, "gen": ModeActive},
			wantErr:          true,
			wantErrSubstring: "requires constraint_shadow=true",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{ConstraintShadow: tc.constraintShadow, ConstraintModes: tc.modes}
			modes, err := cfg.ResolveConstraintModes()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got modes=%v", modes)
				}
				if tc.wantErrSubstring != "" && !strings.Contains(err.Error(), tc.wantErrSubstring) {
					t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantErrSubstring)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(modes) != 5 {
				t.Fatalf("resolved modes = %v, want all 5 keys present", modes)
			}
		})
	}
}

// TestLoadConfig_ConstraintModesFailsLoudAtLoad proves the load-time gate: a
// malformed constraint_modes block never produces a running hub — loadConfig
// itself returns the error, before any wiring code sees the config.
func TestLoadConfig_ConstraintModesFailsLoudAtLoad(t *testing.T) {
	path := writeTempConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"constraint_shadow": true,
		"constraint_modes": {"typo_key": "shadow"}
	}`)
	if _, err := loadConfig(path); err == nil {
		t.Fatal("loadConfig with an unknown constraint_modes key should fail loud, got nil error")
	}
}

// TestLoadConfig_ConstraintModesValidBlockParses is the mirror positive case:
// a well-formed constraint_modes block loads and resolves exactly as written.
func TestLoadConfig_ConstraintModesValidBlockParses(t *testing.T) {
	path := writeTempConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"constraint_shadow": true,
		"constraint_modes": {"export": "active", "gen": "shadow"}
	}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	modes, err := cfg.ResolveConstraintModes()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if modes["export"] != ModeActive || modes["gen"] != ModeShadow || modes["import"] != ModeOff {
		t.Fatalf("modes = %+v, want export=active gen=shadow import=off", modes)
	}
}

// TestLoadConfig_ShippedSnapshotEnabled pins WS-4.1 (2026-07-09): the
// shipped configs/hub.json must ship snapshot.enabled:true (flipped from
// the write-only-soak default after the 2026-07-08 8-cycle
// hub-restart-mid-cap campaign passed with restore off — see
// SnapshotConfig's doc). A regression back to false here would silently
// resurrect the duplicate-CannotComply-after-restart bug TASK-041 closed —
// this test exists so that resurrection fails CI instead of a bench.
func TestLoadConfig_ShippedSnapshotEnabled(t *testing.T) {
	cfg, err := loadConfig("../../configs/hub.json")
	if err != nil {
		t.Fatalf("loadConfig(configs/hub.json): %v", err)
	}
	if cfg.Snapshot == nil {
		t.Fatal("Snapshot = nil, want the shipped snapshot block")
	}
	if !cfg.Snapshot.Enabled {
		t.Fatal("Snapshot.Enabled = false, want true (WS-4.1 shipped default)")
	}
	if cfg.Snapshot.Path == "" {
		t.Fatal("Snapshot.Path = \"\", want the shipped path")
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
