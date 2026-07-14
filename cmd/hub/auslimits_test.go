package main

// WP-11 cmd/hub tests: unconditional adoption of the CSIP-AUS envelope axes
// into GridState, the enforce_aus_limits config key, and the flag-gated
// planner-envelope mapping from the northbound schedule.

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lexa-hub/internal/bus"
)

// The AUS limits on a live control are adopted into GridState UNCONDITIONALLY
// — there is no flag on adoption, only on enforcement — and absent fields stay
// NaN (never zero: 0 is a real 0 W cap).
func TestReadSystemState_AdoptsAusLimits(t *testing.T) {
	r := newMQTTSystemReader(nil, testFastInterval, nil)
	genLim, loadLim := 4000.0, 6000.0
	r.onCSIPControl("lexa/csip/control", bus.ActiveControl{
		Source:   "event",
		MRID:     "aus-evt",
		GenLimW:  &genLim,
		LoadLimW: &loadLim,
		Ts:       time.Now().Unix(),
	})
	state, err := r.ReadSystemState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Grid.GenLimitW != 4000 {
		t.Errorf("Grid.GenLimitW = %v, want 4000", state.Grid.GenLimitW)
	}
	if state.Grid.LoadLimitW != 6000 {
		t.Errorf("Grid.LoadLimitW = %v, want 6000", state.Grid.LoadLimitW)
	}
}

// Absent AUS fields ⇒ NaN in GridState; and a control with no real source
// ("none") adopts nothing.
func TestReadSystemState_AusLimitsAbsentStayNaN(t *testing.T) {
	r := newMQTTSystemReader(nil, testFastInterval, nil)
	r.onCSIPControl("lexa/csip/control", bus.ActiveControl{
		Source: "event", MRID: "plain-evt", Ts: time.Now().Unix(),
	})
	state, err := r.ReadSystemState()
	if err != nil {
		t.Fatal(err)
	}
	if !math.IsNaN(state.Grid.GenLimitW) || !math.IsNaN(state.Grid.LoadLimitW) {
		t.Errorf("absent AUS limits must stay NaN, got gen=%v load=%v", state.Grid.GenLimitW, state.Grid.LoadLimitW)
	}

	genLim := 4000.0
	r2 := newMQTTSystemReader(nil, testFastInterval, nil)
	r2.onCSIPControl("lexa/csip/control", bus.ActiveControl{
		Source: "none", GenLimW: &genLim, Ts: time.Now().Unix(),
	})
	state2, err := r2.ReadSystemState()
	if err != nil {
		t.Fatal(err)
	}
	if !math.IsNaN(state2.Grid.GenLimitW) {
		t.Errorf(`a "none" control must adopt nothing, got gen=%v`, state2.Grid.GenLimitW)
	}
}

// enforce_aus_limits parses (default false) and the WP-11 constraint-mode keys
// are accepted.
func TestLoadConfig_EnforceAusLimits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")

	if err := os.WriteFile(path, []byte(`{"devices":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EnforceAusLimits {
		t.Error("enforce_aus_limits must default to false")
	}

	if err := os.WriteFile(path, []byte(`{
		"enforce_aus_limits": true,
		"constraint_shadow": true,
		"constraint_modes": {"gen_aus": "shadow", "load_aus": "shadow"}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = loadConfig(path)
	if err != nil {
		t.Fatalf("gen_aus/load_aus must be legal constraint_modes keys: %v", err)
	}
	if !cfg.EnforceAusLimits {
		t.Error("enforce_aus_limits=true did not parse")
	}
	modes, err := cfg.ResolveConstraintModes()
	if err != nil {
		t.Fatal(err)
	}
	if modes["gen_aus"] != ModeShadow || modes["load_aus"] != ModeShadow {
		t.Errorf("modes = %v, want gen_aus/load_aus shadow", modes)
	}
}

// The schedule's gen_lim_w/load_lim_w slots map into the planner envelope ONLY
// when enforce_aus_limits is on; with the flag off the fields stay NaN so
// daily plans are byte-identical to pre-WP-11 (the optimizer cascade's own
// flag-off contract, applied to the planner seam).
func TestDerConstraintsFromSchedule_AusFieldsFlagGated(t *testing.T) {
	genLim, loadLim := 4000.0, 6000.0
	now := time.Now().Unix()
	sched := bus.DERScheduleMsg{
		WindowStart: now,
		WindowEnd:   now + 24*3600,
		Slots: []bus.DERScheduleSlot{{
			Start: now, End: now + 3600, Source: "event",
			GenLimW: &genLim, LoadLimW: &loadLim,
		}},
	}

	on := derConstraintsFromSchedule(sched, true)
	if on[0].GenLimW != 4000 || on[0].LoadLimW != 6000 {
		t.Errorf("flag on: step 0 = %+v, want GenLimW 4000 / LoadLimW 6000", on[0])
	}
	// Steps outside the slot stay NaN.
	if !math.IsNaN(on[287].GenLimW) || !math.IsNaN(on[287].LoadLimW) {
		t.Errorf("uncovered step must stay NaN, got %+v", on[287])
	}

	off := derConstraintsFromSchedule(sched, false)
	if !math.IsNaN(off[0].GenLimW) || !math.IsNaN(off[0].LoadLimW) {
		t.Errorf("flag off: AUS fields must stay NaN, got %+v", off[0])
	}
}
