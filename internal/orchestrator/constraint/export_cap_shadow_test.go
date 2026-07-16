package constraint

import (
	"math"
	"testing"
	"time"

	"lexa-hub/internal/orchestrator"
)

// TestPlanDischargeExportCap_ShadowParity is the EXPCAP-2 parity proof: under an
// active export cap with a 24h-plan discharge target, the candidate stack's
// economics layer must cap the plan discharge to the SAME headroom the legacy
// cascade's planExportDischargeCapW applies — otherwise a shadow battery-setpoint
// divergence fires on every such tick and the P5 flip re-ships the EXPCAP breach.
// Minimal scenario (no solar, pack idle at tick start) so the cap resolves to the
// conservative limit and the only export lever is the plan discharge itself.
func TestPlanDischargeExportCap_ShadowParity(t *testing.T) {
	legacy := benchLegacy()
	st := orchestrator.SystemState{
		Timestamp:   offPeakTime(),
		Solar:       []orchestrator.SolarState{{Name: "pv", PowerW: 0, MaxW: 8000, Connected: true, Energized: true}},
		Batteries:   []orchestrator.BatteryState{{Name: "bat", PowerW: 0, SOC: 80, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true}},
		Grid:        orchestrator.GridState{NetW: 0, ImportLimitW: math.NaN(), ExportLimitW: math.NaN(), MaxLimitW: math.NaN()},
		CSIPControl: expLimControl(1000), // 1000 W export cap ⇒ conservative 800 W
		// A stale plan wanting a big discharge the cap must trim.
		DailyPlanTarget: &orchestrator.PlanTarget{BattSetpointW: 5000, EVMaxCurrentA: math.NaN(), EVSetpointW: math.NaN()},
	}
	stack := benchFullStack(st)

	var divs []Divergence
	tickN := 0
	w := Wrap(legacy, stack, Options{
		Now:       func() time.Time { ts := time.Unix(int64(tickN)*3600, 0); tickN++; return ts },
		OnDiverge: func(d Divergence) { divs = append(divs, d) },
	})

	var plan orchestrator.Plan
	for i := 0; i < 4; i++ {
		plan = w.Optimize(st)
		st.Timestamp = st.Timestamp.Add(3 * time.Second)
	}

	// Sanity: the legacy path really did cap the 5000 W plan discharge to the
	// 800 W conservative export headroom (not follow it verbatim, not zero it).
	if sp := planBatterySetpoint(plan, "bat"); math.Abs(sp-800) > 1 {
		t.Fatalf("legacy plan discharge = %.0fW, want 800W (export headroom cap); scenario not exercising the cap", sp)
	}

	for _, d := range divs {
		for _, a := range d.Axes {
			if a.Device == "bat" && a.Axis == AxisBatterySetpointW.String() {
				t.Fatalf("battery-setpoint shadow divergence under a plan discharge + export cap: legacy=%v candidate=%v author=%q — EXPCAP-2 port out of parity",
					a.Legacy, a.Candidate, a.Author)
			}
		}
	}
	if w.Divergences() != 0 {
		t.Fatalf("expected zero shadow divergences, got %d: %+v", w.Divergences(), divs)
	}
}
