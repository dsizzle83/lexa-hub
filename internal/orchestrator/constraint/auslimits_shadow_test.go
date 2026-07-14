package constraint

import (
	"testing"
	"time"

	"lexa-hub/internal/orchestrator"
)

// WP-11 shadow 0-diff harness gate, mirroring how TASK-060/063 pinned theirs
// (TestEconomics_ShadowZeroDiff...): with the legacy cascade's AUS enforcement
// ON and the candidate Stack carrying the full bench constraint set PLUS the
// gen-aus/load-aus mirrors, the Wrapper must observe ZERO divergent ticks over
// steady-state in-family scenarios — both sides implement the same logic, so
// any diff is a porting bug.

// benchAusLegacy is benchLegacy with WP-11 enforcement enabled.
func benchAusLegacy() *orchestrator.DefaultOptimizer {
	o := benchLegacy()
	o.EnforceAusLimits = true
	return o
}

// benchAusFullStack is benchFullStack plus the WP-11 mirrors.
func benchAusFullStack(st orchestrator.SystemState) *Stack {
	in := benchInput(st)
	cd := NewEVImportCooldown()
	return NewStack(in.Plant, 0,
		NewBatterySafetyConstraint(benchSOCReserve),
		NewExportConstraint(), NewGenLimitConstraint(), NewImportLimitConstraint(cd),
		NewAusGenLimitConstraint(), NewAusLoadLimitConstraint(),
		NewEconomicsConstraint(orchestrator.DefaultTOUCostModel(),
			benchSOCReserve, benchSOCFull, benchExcessSolar, benchExportMargin, benchEVCooldown, cd))
}

// Scenario A: an active gross-generation cap in steady state — solar running
// at the cap, battery full and idle. Legacy (enforcing) ceilings the inverter
// at the cap and restore-idles the battery; the candidate's gen-aus ceiling +
// battery participation cap resolve to the same values.
func TestAusShadow_ZeroDiff_GenCapSteadyState(t *testing.T) {
	st := func(ts time.Time) orchestrator.SystemState {
		g := orchestrator.NewGridState()
		g.NetW = -2000 // exporting: solar 3000 − home 1000
		g.GenLimitW = 3000
		return orchestrator.SystemState{
			Timestamp: ts,
			Solar:     []orchestrator.SolarState{{Name: "pv", PowerW: 3000, MaxW: 5000, Connected: true, Energized: true}},
			Batteries: []orchestrator.BatteryState{
				{Name: "bat", PowerW: 0, SOC: 96, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true},
			},
			Grid: g,
		}
	}

	legacy := benchAusLegacy()
	stack := benchAusFullStack(st(offPeakTime()))
	var diverged []Divergence
	w := Wrap(legacy, stack, Options{
		Now:       func() time.Time { return time.Unix(0, 0) },
		OnDiverge: func(d Divergence) { diverged = append(diverged, d) },
	})
	for i := 0; i < 4; i++ {
		w.Optimize(st(offPeakTime().Add(time.Duration(i) * 3 * time.Second)))
		if n := w.Divergences(); n != 0 {
			t.Fatalf("tick %d: %d divergence(s); first axes=%+v", i, n, diverged[0].Axes)
		}
	}
}

// Scenario B: an active gross-load cap trimming a self-consumption charge in
// steady state. Legacy's post-economics charge clamp and the candidate's
// charge floor must land the battery on the same setpoint.
func TestAusShadow_ZeroDiff_LoadCapChargeTrim(t *testing.T) {
	// Midday partial-peak (no TOU discharge): solar 4000, home 500, battery
	// measured charging 3000 → gross load 3500 over the 3000 cap.
	midday := time.Date(2026, 7, 6, 12, 0, 0, 0, time.Local)
	st := func(ts time.Time) orchestrator.SystemState {
		g := orchestrator.NewGridState()
		g.NetW = -500 // 500 + 3000 − 4000
		g.LoadLimitW = 3000
		return orchestrator.SystemState{
			Timestamp: ts,
			Solar:     []orchestrator.SolarState{{Name: "pv", PowerW: 4000, MaxW: 5000, Connected: true, Energized: true}},
			Batteries: []orchestrator.BatteryState{
				{Name: "bat", PowerW: -3000, SOC: 50, MaxChargeW: 5000, MaxDischargeW: 5000, Connected: true, Energized: true},
			},
			Grid: g,
		}
	}

	legacy := benchAusLegacy()
	stack := benchAusFullStack(st(midday))
	var diverged []Divergence
	w := Wrap(legacy, stack, Options{
		Now:       func() time.Time { return time.Unix(0, 0) },
		OnDiverge: func(d Divergence) { diverged = append(diverged, d) },
	})
	// Two ticks only: under the (shared, 3-tick) detection window, so neither
	// side's convergence backstop fires — this scenario pins the CONTROL
	// parity; breach parity has its own scenario below.
	for i := 0; i < 2; i++ {
		w.Optimize(st(midday.Add(time.Duration(i) * 3 * time.Second)))
		if n := w.Divergences(); n != 0 {
			t.Fatalf("tick %d: %d divergence(s); first axes=%+v", i, n, diverged[0].Axes)
		}
	}
}

// Scenario C: an active gross-load cap curtailing an EV session, run through
// engagement + steady holding. The sticky guards on both sides are the same
// arithmetic over the same measured inputs, so the commanded currents match
// tick-for-tick.
func TestAusShadow_ZeroDiff_LoadCapEVCurtail(t *testing.T) {
	night := time.Date(2026, 7, 6, 3, 0, 0, 0, time.Local)
	st := func(ts time.Time, evPowerW float64) orchestrator.SystemState {
		g := orchestrator.NewGridState()
		g.NetW = 1000 + evPowerW // importing: home 1000 + EV draw
		g.LoadLimitW = 4000
		return orchestrator.SystemState{
			Timestamp: ts,
			EVSEs: []orchestrator.EVSEState{{
				StationID: "cs1", ConnectorID: 1, Connected: true, SessionActive: true,
				MaxCurrentA: 32, VoltageV: 230, PowerW: evPowerW,
			}},
			Grid: g,
		}
	}

	legacy := benchAusLegacy()
	stack := benchAusFullStack(st(night, 7360))
	var diverged []Divergence
	w := Wrap(legacy, stack, Options{
		Now:       func() time.Time { return time.Unix(0, 0) },
		OnDiverge: func(d Divergence) { diverged = append(diverged, d) },
	})

	// Tick 0: EV at full rate → engagement. Ticks 1..3: EV settled at the
	// commanded ceiling → sticky hold, site compliant.
	evPower := 7360.0
	for i := 0; i < 4; i++ {
		plan := w.Optimize(st(night.Add(time.Duration(i)*3*time.Second), evPower))
		if n := w.Divergences(); n != 0 {
			t.Fatalf("tick %d: %d divergence(s); first axes=%+v", i, n, diverged[0].Axes)
		}
		// World model: the EV settles to the actuated command next tick.
		for _, c := range plan.EVSECommands {
			if c.StationID == "cs1" {
				evPower = c.MaxCurrentA * 230
			}
		}
	}
}

// Scenario D: breach parity — an unsheddable gross-load breach must escalate
// on BOTH sides with the same LimitType inside the diff debounce, so the
// breach axis never diverges (both fire "load-aus" at the same fixed-vs-
// adaptive window, which coincide at bench defaults).
func TestAusShadow_ZeroDiff_LoadBreachParity(t *testing.T) {
	night := time.Date(2026, 7, 6, 3, 0, 0, 0, time.Local)
	st := func(ts time.Time) orchestrator.SystemState {
		g := orchestrator.NewGridState()
		g.NetW = 6000 // pure home load, nothing to shed
		g.LoadLimitW = 4000
		return orchestrator.SystemState{Timestamp: ts, Grid: g}
	}

	legacy := benchAusLegacy()
	stack := benchAusFullStack(st(night))
	var diverged []Divergence
	w := Wrap(legacy, stack, Options{
		Now:       func() time.Time { return time.Unix(0, 0) },
		OnDiverge: func(d Divergence) { diverged = append(diverged, d) },
	})
	for i := 0; i < 6; i++ { // through and past both detection windows
		w.Optimize(st(night.Add(time.Duration(i) * 3 * time.Second)))
		if n := w.Divergences(); n != 0 {
			t.Fatalf("tick %d: %d divergence(s); first axes=%+v", i, n, diverged[0].Axes)
		}
	}
}
