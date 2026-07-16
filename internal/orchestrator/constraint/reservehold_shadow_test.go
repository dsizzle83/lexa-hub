package constraint

import (
	"math"
	"testing"
	"time"

	"lexa-hub/internal/orchestrator"
)

// TestReserveHold_ShadowParityUnderDither is the H1b parity proof (audit B-1):
// the legacy cascade and the candidate Stack must agree on the battery setpoint
// tick-for-tick while a pack's measured SOC dithers ±1pt across the 20% reserve
// under an import cap. Both sides now carry the hysteretic reserve latch, so
// once the hold arms neither re-authorizes discharge — no battery-axis
// divergence. Before the mirror the candidate would keep discharging on the
// above-line ticks while legacy held, and the shadow wrapper would log a
// battery-setpoint divergence every such tick, poisoning the ≥1-week 0-diff gate.
func TestReserveHold_ShadowParityUnderDither(t *testing.T) {
	legacy := benchLegacy()
	// benchFullStack wires safety+compliance+economics and SetReserveFloor(20),
	// exactly as main.go does.
	base := orchestrator.SystemState{
		Solar:       []orchestrator.SolarState{{Name: "pv", PowerW: 300, MaxW: 8000, Connected: true, Energized: true}},
		Grid:        orchestrator.GridState{NetW: 5000, ImportLimitW: math.NaN(), ExportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		CSIPControl: impLimControl(0), // 0 W import cap: without the latch the rule wants ~4.7 kW discharge
	}
	stack := benchFullStack(base)

	var divs []Divergence
	tickN := 0
	w := Wrap(legacy, stack, Options{
		Now: func() time.Time {
			ts := time.Unix(int64(tickN)*3600, 0) // 1 h/tick so the rate-limiter never suppresses a record
			tickN++
			return ts
		},
		OnDiverge: func(d Divergence) { divs = append(divs, d) },
	})

	socs := []float64{21, 21, 19, 21, 19, 21, 19, 21}
	sawHold := false
	for i, soc := range socs {
		st := base
		st.Timestamp = time.Unix(int64(i)*3, 0)
		st.Batteries = []orchestrator.BatteryState{{
			Name: "bat", PowerW: 0, SOC: soc, MaxChargeW: 5000, MaxDischargeW: 5000,
			Connected: true, Energized: true,
		}}
		plan := w.Optimize(st)
		if soc <= 20 {
			sawHold = true
		}
		// Sanity that the scenario is live: on the FIRST above-reserve tick
		// (before any hold), the legacy path must actually want to discharge to
		// defend the 0 W import cap — otherwise the test proves nothing.
		if i == 0 {
			if sp := planBatterySetpoint(plan, "bat"); sp <= 100 {
				t.Fatalf("scenario inert: legacy did not discharge to defend the import cap on tick 0 (setpoint %.0f)", sp)
			}
		}
		// After the hold arms, neither side may command discharge — so there is
		// no battery-setpoint divergence for the wrapper to record.
		if sawHold && soc > 20 {
			if sp := planBatterySetpoint(plan, "bat"); sp > 100 {
				t.Fatalf("tick %d (SOC %.0f%%): legacy commanded %.0fW discharge after the hold armed — latch not holding", i, soc, sp)
			}
		}
	}

	for _, d := range divs {
		for _, a := range d.Axes {
			if a.Device == "bat" && a.Axis == AxisBatterySetpointW.String() {
				t.Fatalf("battery-setpoint shadow divergence under reserve dither: legacy=%v candidate=%v author=%q — the constraint mirror is out of parity with the legacy latch",
					a.Legacy, a.Candidate, a.Author)
			}
		}
	}
	if w.Divergences() != 0 {
		t.Fatalf("expected zero shadow divergences under reserve dither, got %d: %+v", w.Divergences(), divs)
	}
}
