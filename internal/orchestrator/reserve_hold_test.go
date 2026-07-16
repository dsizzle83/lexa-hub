package orchestrator

import (
	"math"
	"testing"

	model "lexa-proto/csipmodel"
)

// instReserve reproduces the pre-latch instantaneous SOC ≤ reserve discharge
// gate, so unit tests that exercise a single discharge rule in isolation keep
// their exact prior semantics without wiring up an optimizer's reserve latch.
func instReserve(reserve float64) func(BatteryState) bool {
	return func(b BatteryState) bool { return !math.IsNaN(b.SOC) && b.SOC <= reserve }
}

// setpointFor returns the commanded battery setpoint (W) for name in the plan,
// or NaN when the pack has no command this tick.
func setpointFor(p *Plan, name string) float64 {
	for _, c := range p.BatteryCommands {
		if c.Name == name {
			return c.SetpointW
		}
	}
	return math.NaN()
}

// TestReserveHold_DitherAtReserve_NoDischargeSquareWave is the finding-B
// regression (audit B-1 / GAP-08). Under an import cap with the pack's measured
// SOC dithering ±1pt across the 20% reserve line, the pre-fix hub re-authorized
// full discharge on every above-line tick — a ~4.7 kW↔0 W square wave that
// walked the pack below reserve at ~50% duty. Once the latch has entered its
// hold (first sub-reserve tick), no discharge may be commanded again until SOC
// recovers past reserve+margin for a sustained run — so the 21% ticks after the
// first 19% tick must NOT command discharge.
func TestReserveHold_DitherAtReserve_NoDischargeSquareWave(t *testing.T) {
	o := NewDefaultOptimizer() // SOCReserve = 20
	impLim := &model.ActivePower{Value: 0, Multiplier: 0}
	mkState := func(soc float64) SystemState {
		return SystemState{
			Solar: []SolarState{{Name: "pv", PowerW: 300, MaxW: 8000, Connected: true, Energized: true}},
			Batteries: []BatteryState{{
				Name: "bat", PowerW: 0, SOC: soc, MaxChargeW: 5000, MaxDischargeW: 5000,
				Connected: true, Energized: true,
			}},
			Grid: GridState{
				NetW: 5000, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN(),
			},
			CSIPControl: &CSIPControlState{
				Source: "event", MRID: "EVT-IMP-DITHER",
				Base: model.DERControlBase{OpModImpLimW: impLim},
			},
		}
	}

	// Dither pattern: two ticks above reserve, then a sub-reserve tick that
	// arms the hold, then keep dithering. socs indexes the per-tick measured SOC.
	socs := []float64{21, 21, 19, 21, 19, 21, 21, 19, 21}
	holdArmedAt := -1
	for i, soc := range socs {
		p := o.Optimize(mkState(soc))
		if soc <= o.SOCReserve && holdArmedAt < 0 {
			holdArmedAt = i
		}
		if holdArmedAt >= 0 && i > holdArmedAt {
			// After the hold is armed, no tick — above OR below the line — may
			// command a discharge (SetpointW > 0).
			if sp := setpointFor(&p, "bat"); sp > 100 {
				t.Fatalf("tick %d (SOC %.0f%%): commanded %.0fW discharge after the reserve hold armed at tick %d — the square wave was not suppressed",
					i, soc, sp, holdArmedAt)
			}
		}
	}
	if holdArmedAt < 0 {
		t.Fatal("test bug: the dither pattern never crossed the reserve line")
	}
}

// TestUpdateReserveHolds_HysteresisReleaseRequiresSustainedRecovery pins the
// latch state machine directly: enter is immediate at/below reserve; release
// requires SOC >= reserve+margin for reserveReleaseTicks CONSECUTIVE ticks; a
// tick inside the margin band (above reserve but below reserve+margin) resets
// the recovery run.
func TestUpdateReserveHolds_HysteresisReleaseRequiresSustainedRecovery(t *testing.T) {
	o := NewDefaultOptimizer() // reserve 20, margin reserveReleaseMarginPct
	bat := func(soc float64) []BatteryState {
		return []BatteryState{{Name: "b", SOC: soc, Connected: true, MaxDischargeW: 5000}}
	}

	// Enter immediately at the reserve line.
	o.updateReserveHolds(bat(20))
	if !o.dischargeBlocked("b", 20) {
		t.Fatal("hold must arm immediately at the reserve line")
	}

	// A tick just above reserve but inside the margin band does NOT release.
	o.updateReserveHolds(bat(o.SOCReserve + reserveReleaseMarginPct - 0.5))
	if !o.dischargeBlocked("b", 21) {
		t.Fatal("a tick inside the release-margin band must not release the hold")
	}

	// Sustained recovery above reserve+margin: needs reserveReleaseTicks in a row.
	rel := o.scaleTicks(reserveReleaseTicks)
	recov := o.SOCReserve + reserveReleaseMarginPct + 1
	for i := 0; i < rel-1; i++ {
		o.updateReserveHolds(bat(recov))
		if !o.dischargeBlocked("b", recov) {
			t.Fatalf("released after only %d recovery tick(s); need %d sustained", i+1, rel)
		}
	}
	// One tick inside the band mid-recovery resets the run.
	o.updateReserveHolds(bat(o.SOCReserve + 0.5))
	if !o.dischargeBlocked("b", 20.5) {
		t.Fatal("hold must survive a mid-recovery dip back into the band")
	}
	// Now a full sustained run releases.
	for i := 0; i < rel; i++ {
		o.updateReserveHolds(bat(recov))
	}
	if o.dischargeBlocked("b", recov) {
		t.Fatal("hold must release after a full sustained recovery run above reserve+margin")
	}
}

// TestReserveHold_NaNSOCRetainsHold is the fail-safe: a pack that drops its SOC
// telemetry while held must STAY held — never discharge into an unknown reserve.
func TestReserveHold_NaNSOCRetainsHold(t *testing.T) {
	o := NewDefaultOptimizer()
	o.updateReserveHolds([]BatteryState{{Name: "b", SOC: 18, Connected: true, MaxDischargeW: 5000}})
	if !o.dischargeBlocked("b", 18) {
		t.Fatal("precondition: hold should be armed at 18% SOC")
	}
	// SOC goes unreadable.
	o.updateReserveHolds([]BatteryState{{Name: "b", SOC: math.NaN(), Connected: true, MaxDischargeW: 5000}})
	if !o.dischargeBlocked("b", math.NaN()) {
		t.Fatal("a NaN SOC must retain an existing hold (fail-safe), not release it")
	}
}

// TestReserveHold_AboveReserveNeverEntered_DischargesNormally is the no-regression
// case: a pack that never touches the reserve line is never blocked.
func TestReserveHold_AboveReserveNeverEntered_DischargesNormally(t *testing.T) {
	o := NewDefaultOptimizer()
	for i := 0; i < 5; i++ {
		o.updateReserveHolds([]BatteryState{{Name: "b", SOC: 60, Connected: true, MaxDischargeW: 5000}})
		if o.dischargeBlocked("b", 60) {
			t.Fatalf("tick %d: blocked a pack that never approached reserve", i)
		}
	}
}

// TestCheckBatterySafety_TripsUnderReserveDitherWhenDischarging is the guard
// half (audit B-2): a pack MEASURED discharging throughout a reserve episode
// must accumulate toward the disconnect even while its SOC telemetry dithers
// across the line — because the guard now reads the latched reserve state, not
// the instantaneous SOC. (An accept-but-ignore pack that keeps discharging is
// exactly what the guard exists to disconnect.)
func TestCheckBatterySafety_TripsUnderReserveDitherWhenDischarging(t *testing.T) {
	o := NewDefaultOptimizer()
	socs := []float64{19, 21, 19, 21, 19, 21, 19} // dithering across reserve
	var last *Plan
	for _, soc := range socs {
		bats := []BatteryState{{Name: "bat", PowerW: 4800, SOC: soc, Connected: true, Energized: true}}
		o.updateReserveHolds(bats) // as Optimize would, before the guard
		p := &Plan{}
		o.checkBatterySafety(bats, p)
		last = p
	}
	if !hasDisconnectCommand(last, "bat") {
		t.Errorf("guard failed to disconnect a pack discharging 4800W throughout a dithering reserve episode; commands=%+v", last.BatteryCommands)
	}
}
