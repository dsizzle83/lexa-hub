package main

import (
	"math"
	"testing"

	"lexa-hub/internal/bus"
)

// TestSolarCommandToControl_RestoreClearsCurtailment guards the restore path:
// a nil CurtailToW must produce a control that actively un-curtails the inverter
// (ceiling at full nameplate), not an empty control that Base.ApplyControl
// silently ignores — which left inverters stuck curtailed after a genLimit event.
func TestSolarCommandToControl_RestoreClearsCurtailment(t *testing.T) {
	restore := solarCommandToControl(bus.SolarCommand{Device: "pv"}) // CurtailToW nil
	if restore.OpModMaxLimW == nil {
		t.Fatal("restore produced an empty control (no OpModMaxLimW) — a silent no-op")
	}
	// The ceiling is encoded with a multiplier, so compare the *effective*
	// watts (Value × 10^Multiplier), not the raw int16 field. It must sit far
	// above any nameplate so the device clamps it to WMax → 100% (no curtail),
	// even for systems above the int16 watt range.
	restoreW := float64(restore.OpModMaxLimW.Value) * math.Pow10(int(restore.OpModMaxLimW.Multiplier))
	if restoreW < math.MaxInt16 {
		t.Errorf("restore ceiling = %g W (value=%d mult=%d), want ≥ MaxInt16 (clamps to WMax → 100%%)",
			restoreW, restore.OpModMaxLimW.Value, restore.OpModMaxLimW.Multiplier)
	}

	w := 2000.0
	curtail := solarCommandToControl(bus.SolarCommand{Device: "pv", CurtailToW: &w})
	if curtail.OpModMaxLimW == nil || curtail.OpModMaxLimW.Value != 2000 {
		t.Errorf("curtail ceiling = %v, want 2000", curtail.OpModMaxLimW)
	}
}

// TestActivePowerFromWatts_ScalesNotClips guards audit GS-1/MTR-1: watt values
// above the int16 range must scale into the SunSpec multiplier, not clip at
// 32767 W (which silently caps whole-home systems above 32.7 kW).
func TestActivePowerFromWatts_ScalesNotClips(t *testing.T) {
	cases := []float64{0, 1500, 32767, 50000, 120000, 250000}
	for _, want := range cases {
		ap := activePowerFromWatts(want)
		got := float64(ap.Value) * math.Pow10(int(ap.Multiplier))
		// Allow rounding loss from the chosen multiplier (< 0.1%).
		if tol := want * 0.001; math.Abs(got-want) > tol+1 {
			t.Errorf("activePowerFromWatts(%g) = %g W (value=%d mult=%d), want ≈ %g",
				want, got, ap.Value, ap.Multiplier, want)
		}
		if ap.Value > math.MaxInt16 || ap.Value < math.MinInt16 {
			t.Errorf("activePowerFromWatts(%g) value=%d out of int16 range", want, ap.Value)
		}
	}
}

// wattEncoderAgreement is the TASK-053 cross-encoder golden table — see the
// identical copy + full rationale in cmd/hub/state_test.go. Computed once
// from the shared "divide by 10 until it fits int16" algorithm both
// activePowerFromWatts (this file) and wattsToActivePower (cmd/hub)
// implement independently for non-negative watts, the domain where they
// must agree (MTR-5/GS-1). Keep both copies byte-identical.
var wattEncoderAgreement = []struct {
	watts float64
	value int16
	mult  int8
}{
	{0, 0, 0},
	{1, 1, 0},
	{100, 100, 0},
	{1500, 1500, 0},
	{32767, 32767, 0},
	{32768, 3277, 1},
	{32769, 3277, 1},
	{50000, 5000, 1},
	{120000, 12000, 1},
	{250000, 25000, 1},
	{500000, 5000, 2},
	{1_000_000, 10000, 2},
	{10_000_000, 10000, 3},
	{100_000_000, 10000, 4},
	{1_000_000_000, 10000, 5},
	{123456789, 12346, 4},
	{999999999, 10000, 5},
}

// TestActivePowerFromWatts_CrossEncoderAgreement is the step-3 "product's two
// watt-encoders agree" acceptance criterion (TASK-053) for
// activePowerFromWatts's half of the pair.
func TestActivePowerFromWatts_CrossEncoderAgreement(t *testing.T) {
	for _, tc := range wattEncoderAgreement {
		ap := activePowerFromWatts(tc.watts)
		if ap.Value != tc.value || ap.Multiplier != tc.mult {
			t.Errorf("activePowerFromWatts(%g) = {Value:%d Mult:%d}, want {Value:%d Mult:%d} (cross-encoder golden table)",
				tc.watts, ap.Value, ap.Multiplier, tc.value, tc.mult)
		}
	}
}

// TestActivePowerFromWatts_Sweep0To1e9 is the step-3 encode-scaling property:
// across a dense log-scale sweep of watt values from 0 to 1e9, Value must
// stay in int16 range and Value×10^Multiplier must reconstruct the input
// within half a scale step.
func TestActivePowerFromWatts_Sweep0To1e9(t *testing.T) {
	step := 1.0
	for w := 0.0; w <= 1e9; w += step {
		ap := activePowerFromWatts(w)
		if ap.Value > math.MaxInt16 || ap.Value < math.MinInt16 {
			t.Fatalf("activePowerFromWatts(%g) value=%d out of int16 range", w, ap.Value)
		}
		got := float64(ap.Value) * math.Pow10(int(ap.Multiplier))
		tol := 0.5 * math.Pow10(int(ap.Multiplier))
		if math.Abs(got-w) > tol {
			t.Fatalf("activePowerFromWatts(%g) = {Value:%d Mult:%d} -> %g, want within %g (half scale step)",
				w, ap.Value, ap.Multiplier, got, tol)
		}
		if w > 0 {
			step = w * 0.001
			if step < 1 {
				step = 1
			}
		}
	}
}
