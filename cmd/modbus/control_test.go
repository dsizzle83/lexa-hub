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
