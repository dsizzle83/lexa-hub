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
	if restore.OpModMaxLimW.Value < math.MaxInt16 {
		t.Errorf("restore ceiling = %d, want full nameplate (≥ MaxInt16, clamps to WMax → 100%%)", restore.OpModMaxLimW.Value)
	}

	w := 2000.0
	curtail := solarCommandToControl(bus.SolarCommand{Device: "pv", CurtailToW: &w})
	if curtail.OpModMaxLimW == nil || curtail.OpModMaxLimW.Value != 2000 {
		t.Errorf("curtail ceiling = %v, want 2000", curtail.OpModMaxLimW)
	}
}
