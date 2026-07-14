package orchestrator

// Whitebox tests for individual optimizer rule functions.
// Integration-level tests (full Optimize path) live in optimizer_test.go.

import (
	"math"
	"testing"
	"time"

	model "lexa-proto/csipmodel"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func ruleBat(name string, powerW, soc, maxW float64) BatteryState {
	b := NewBatteryState(name)
	b.PowerW = powerW
	b.SOC = soc
	b.MaxChargeW = maxW
	b.MaxDischargeW = maxW
	b.Connected = true
	b.Energized = true
	return b
}

func ruleSol(name string, powerW float64) SolarState {
	return SolarState{Name: name, PowerW: powerW, MaxW: powerW, Connected: true, Energized: true}
}

func ruleEVSE(id string, sessionActive bool, maxA, voltV float64) EVSEState {
	return EVSEState{
		StationID: id, ConnectorID: 1,
		Connected: true, SessionActive: sessionActive,
		MaxCurrentA: maxA, VoltageV: voltV,
	}
}

func noLimits() gridConstraints {
	return gridConstraints{exportLimitW: math.NaN(), importLimitW: math.NaN(), maxLimitW: math.NaN()}
}

// applyExportLimitRule is a stateless test shim: each call uses a fresh optimizer
// so the guard state doesn't carry over between independent test cases.
func applyExportLimitRule(
	solar []SolarState, evses []EVSEState, evseW float64,
	limits gridConstraints, netW, socFull, surplusW float64,
	batteries []BatteryState, plan *Plan,
) ([]BatteryState, float64) {
	o := NewDefaultOptimizer()
	return o.applyExportLimitRule(solar, evses, evseW, limits, netW, socFull, surplusW, batteries, plan)
}

// ── deriveGridConstraints ─────────────────────────────────────────────────────

func TestDeriveGridConstraints_NilCSIP_AllNaN(t *testing.T) {
	c := deriveGridConstraints(NewGridState(), nil)
	if !math.IsNaN(c.exportLimitW) || !math.IsNaN(c.importLimitW) || !math.IsNaN(c.maxLimitW) {
		t.Error("expected all NaN with nil CSIP and no grid limits")
	}
}

func TestDeriveGridConstraints_CSIPTighterThanGrid(t *testing.T) {
	g := NewGridState()
	g.ExportLimitW = 5000
	cc := &CSIPControlState{Base: model.DERControlBase{
		OpModExpLimW: &model.ActivePower{Value: 2000},
	}}
	c := deriveGridConstraints(g, cc)
	if c.exportLimitW != 2000 {
		t.Errorf("exportLimitW = %.0f, want 2000 (CSIP tighter)", c.exportLimitW)
	}
}

func TestDeriveGridConstraints_GridTighterThanCSIP(t *testing.T) {
	g := NewGridState()
	g.ExportLimitW = 1000
	cc := &CSIPControlState{Base: model.DERControlBase{
		OpModExpLimW: &model.ActivePower{Value: 3000},
	}}
	c := deriveGridConstraints(g, cc)
	if c.exportLimitW != 1000 {
		t.Errorf("exportLimitW = %.0f, want 1000 (grid tighter)", c.exportLimitW)
	}
}

func TestDeriveGridConstraints_MaxLimIsGenCap(t *testing.T) {
	// MaxLimW (absolute generation cap) is enforced by curtailing the inverter
	// (applyGenLimitRule), NOT folded into the export limit — folding it made the
	// hub absorb into the battery while generation stayed over the cap.
	cc := &CSIPControlState{Base: model.DERControlBase{
		OpModMaxLimW: &model.ActivePower{Value: 3000},
	}}
	c := deriveGridConstraints(NewGridState(), cc)
	if c.maxLimitW != 3000 {
		t.Errorf("maxLimitW = %.0f, want 3000", c.maxLimitW)
	}
	if !math.IsNaN(c.exportLimitW) {
		t.Errorf("exportLimitW = %.0f, want NaN (gen cap enforced by curtailment, not export absorption)", c.exportLimitW)
	}
}

func TestDeriveGridConstraints_ImportLimit(t *testing.T) {
	cc := &CSIPControlState{Base: model.DERControlBase{
		OpModImpLimW: &model.ActivePower{Value: 4000},
	}}
	c := deriveGridConstraints(NewGridState(), cc)
	if c.importLimitW != 4000 {
		t.Errorf("importLimitW = %.0f, want 4000", c.importLimitW)
	}
}

// ── computePowerBalance ───────────────────────────────────────────────────────

func TestComputePowerBalance_WithMeter(t *testing.T) {
	// 5 kW solar, battery charging at 1 kW, 2 kW home load → 2 kW export.
	// homeLoad = 5000 + max(0,-1000) + (-2000) - 0 = 3000
	// surplus  = 5000 - 3000 = 2000
	state := SystemState{
		Solar:     []SolarState{ruleSol("pv", 5000)},
		Batteries: []BatteryState{ruleBat("bat", -1000, 50, 5000)},
		Grid:      GridState{NetW: -2000},
	}
	solarW, batteryW, evseW, surplusW := computePowerBalance(state)
	if solarW != 5000 {
		t.Errorf("solarW = %.0f, want 5000", solarW)
	}
	if batteryW != -1000 {
		t.Errorf("batteryW = %.0f, want -1000", batteryW)
	}
	if evseW != 0 {
		t.Errorf("evseW = %.0f, want 0", evseW)
	}
	if math.Abs(surplusW-2000) > 1 {
		t.Errorf("surplusW = %.0f, want 2000", surplusW)
	}
}

func TestComputePowerBalance_NoMeter(t *testing.T) {
	state := SystemState{
		Solar: []SolarState{ruleSol("pv", 4000)},
		Grid:  GridState{NetW: math.NaN()},
	}
	_, _, _, surplusW := computePowerBalance(state)
	if surplusW != 4000 {
		t.Errorf("surplusW = %.0f, want 4000 (= solar when no meter)", surplusW)
	}
}

// ── csipDisconnectRule ────────────────────────────────────────────────────────

func TestCSIPDisconnectRule_DisconnectsOnFalse(t *testing.T) {
	f := false
	cc := &CSIPControlState{Base: model.DERControlBase{OpModConnect: &f}}
	state := SystemState{
		Batteries: []BatteryState{ruleBat("bat-0", 0, 50, 5000), ruleBat("bat-1", 0, 80, 5000)},
		Solar:     []SolarState{ruleSol("pv", 8000)},
		EVSEs:     []EVSEState{ruleEVSE("evse-0", true, 32, 230)},
	}
	plan := &Plan{}

	stop := csipDisconnectRule(cc, state, plan)

	if !stop {
		t.Fatal("expected stop=true when OpModConnect=false")
	}
	if len(plan.BatteryCommands) != 2 {
		t.Fatalf("expected 2 disconnect commands, got %d", len(plan.BatteryCommands))
	}
	for _, cmd := range plan.BatteryCommands {
		if cmd.Connect == nil || *cmd.Connect {
			t.Errorf("battery %s: expected Connect=false", cmd.Name)
		}
	}
	// Cease-to-energize applies to the whole DER: solar must be curtailed to
	// zero, not left generating at the last setpoint.
	if len(plan.SolarCommands) != 1 {
		t.Fatalf("expected 1 solar curtailment command, got %d", len(plan.SolarCommands))
	}
	if plan.SolarCommands[0].CurtailToW != 0 {
		t.Errorf("solar CurtailToW = %v, want 0", plan.SolarCommands[0].CurtailToW)
	}
	// Connected EVSEs are suspended for the duration of the event.
	if len(plan.EVSECommands) != 1 {
		t.Fatalf("expected 1 EVSE suspend command, got %d", len(plan.EVSECommands))
	}
	if plan.EVSECommands[0].MaxCurrentA != 0 {
		t.Errorf("EVSE MaxCurrentA = %v, want 0 (suspend)", plan.EVSECommands[0].MaxCurrentA)
	}
}

func TestCSIPDisconnectRule_SkipsDisconnectedDevices(t *testing.T) {
	f := false
	cc := &CSIPControlState{Base: model.DERControlBase{OpModConnect: &f}}
	offlineSol := ruleSol("pv-off", 0)
	offlineSol.Connected = false
	offlineEVSE := ruleEVSE("evse-off", false, 32, 230)
	offlineEVSE.Connected = false
	state := SystemState{
		Solar: []SolarState{offlineSol},
		EVSEs: []EVSEState{offlineEVSE},
	}
	plan := &Plan{}

	if !csipDisconnectRule(cc, state, plan) {
		t.Fatal("expected stop=true when OpModConnect=false")
	}
	if len(plan.SolarCommands) != 0 || len(plan.EVSECommands) != 0 {
		t.Errorf("expected no commands for disconnected devices, got %d solar / %d evse",
			len(plan.SolarCommands), len(plan.EVSECommands))
	}
}

func TestCSIPDisconnectRule_PassthroughOnTrue(t *testing.T) {
	tr := true
	cc := &CSIPControlState{Base: model.DERControlBase{OpModConnect: &tr}}
	plan := &Plan{}
	if csipDisconnectRule(cc, SystemState{}, plan) {
		t.Error("expected stop=false when OpModConnect=true")
	}
	if len(plan.BatteryCommands) != 0 {
		t.Error("expected no commands when OpModConnect=true")
	}
}

func TestCSIPDisconnectRule_PassthroughOnNilCSIP(t *testing.T) {
	plan := &Plan{}
	if csipDisconnectRule(nil, SystemState{}, plan) {
		t.Error("expected stop=false with nil CSIP")
	}
}

// ── applyExportLimitRule ──────────────────────────────────────────────────────

func TestExportLimitRule_ChargesBattery(t *testing.T) {
	// 8 kW export, limit 2 kW → 6 kW must be absorbed; battery has 7 kW headroom.
	solar := []SolarState{ruleSol("pv", 8000)}
	bats := []BatteryState{ruleBat("bat", 0, 50, 7000)}
	limits := gridConstraints{exportLimitW: 2000, importLimitW: math.NaN(), maxLimitW: math.NaN()}
	plan := &Plan{}

	updated, _ := applyExportLimitRule(solar, nil, 0, limits, -8000, 95, 8000, bats, plan)

	if len(plan.BatteryCommands) == 0 {
		t.Fatal("expected battery charge command")
	}
	if plan.BatteryCommands[0].SetpointW >= 0 {
		t.Errorf("expected negative setpoint (charging), got %.0f", plan.BatteryCommands[0].SetpointW)
	}
	if updated[0].PowerW >= 0 {
		t.Errorf("updated batteries[0].PowerW = %.0f; expected negative", updated[0].PowerW)
	}
}

func TestExportLimitRule_CurtailsSolarWhenBatteryFull(t *testing.T) {
	solar := []SolarState{ruleSol("pv", 8000)}
	bats := []BatteryState{ruleBat("bat", 0, 96, 5000)} // SOC above 95% threshold
	limits := gridConstraints{exportLimitW: 2000, importLimitW: math.NaN(), maxLimitW: math.NaN()}
	plan := &Plan{}

	applyExportLimitRule(solar, nil, 0, limits, -8000, 95, 8000, bats, plan)

	if len(plan.SolarCommands) == 0 {
		t.Fatal("expected solar curtailment when battery full")
	}
	if plan.SolarCommands[0].CurtailToW > 2500 {
		t.Errorf("curtailed to %.0fW, want ≤ 2500W", plan.SolarCommands[0].CurtailToW)
	}
}

func TestExportLimitRule_CurtailsProportionally(t *testing.T) {
	// Two inverters: 6 kW and 4 kW. Export limit 2 kW. Battery full.
	// Conservative target = 2000 * 0.80 = 1600 W (20% margin).
	// Excess = 10000 - 1600 = 8400 W. Fraction = 0.84 → pv1→960W, pv2→640W.
	solar := []SolarState{ruleSol("pv1", 6000), ruleSol("pv2", 4000)}
	bats := []BatteryState{ruleBat("bat", 0, 96, 5000)} // SOC above threshold
	limits := gridConstraints{exportLimitW: 2000, importLimitW: math.NaN(), maxLimitW: math.NaN()}
	plan := &Plan{}

	applyExportLimitRule(solar, nil, 0, limits, -10000, 95, 10000, bats, plan)

	if len(plan.SolarCommands) != 2 {
		t.Fatalf("expected 2 solar commands, got %d", len(plan.SolarCommands))
	}
	if math.Abs(plan.SolarCommands[0].CurtailToW-960) > 1 {
		t.Errorf("pv1 curtailTo = %.0fW, want 960W", plan.SolarCommands[0].CurtailToW)
	}
	if math.Abs(plan.SolarCommands[1].CurtailToW-640) > 1 {
		t.Errorf("pv2 curtailTo = %.0fW, want 640W", plan.SolarCommands[1].CurtailToW)
	}
}

func TestExportLimitRule_NoCurtailWithinLimit(t *testing.T) {
	// Export (1 kW) well within the 5 kW limit.  The rule is a sticky controller:
	// it issues a no-op generation ceiling at nameplate every tick while the limit
	// window is active (so it stays engaged and can re-curtail instantly if export
	// climbs), but it must take no battery action and must not actually curtail
	// the inverter below its output.
	solar := []SolarState{ruleSol("pv", 1000)}
	bats := []BatteryState{ruleBat("bat", 0, 50, 5000)}
	limits := gridConstraints{exportLimitW: 5000, importLimitW: math.NaN(), maxLimitW: math.NaN()}
	plan := &Plan{}

	applyExportLimitRule(solar, nil, 0, limits, -1000, 95, 1000, bats, plan)

	if len(plan.BatteryCommands) != 0 {
		t.Errorf("expected no battery command when export is within limit, got %d", len(plan.BatteryCommands))
	}
	// A ceiling at or above the inverter's output is a harmless no-op; anything
	// below would be a spurious curtailment.
	for _, sc := range plan.SolarCommands {
		if !math.IsNaN(sc.CurtailToW) && sc.CurtailToW < solar[0].PowerW {
			t.Errorf("solar curtailed to %.0fW below output %.0fW; should not curtail within limit",
				sc.CurtailToW, solar[0].PowerW)
		}
	}
}

func TestExportLimitRule_NoActionWhenUnconstrained(t *testing.T) {
	solar := []SolarState{ruleSol("pv", 10000)}
	bats := []BatteryState{ruleBat("bat", 0, 50, 5000)}
	plan := &Plan{}

	applyExportLimitRule(solar, nil, 0, noLimits(), -10000, 95, 10000, bats, plan)

	if len(plan.BatteryCommands) != 0 || len(plan.SolarCommands) != 0 {
		t.Error("expected no commands with NaN export limit")
	}
}

// ── scaleRateWPerTick / ceiling slew cadence-invariance ───────────────────────
//
// STOCK QA (malform-huge-activepower, wan-outage-hold, 2026-07-10 campaign)
// found the export ceiling's slew-limited re-tighten converging right at the
// edge of (or past) the scenario's fault-recovery window at STOCK cadence
// while the identical fault converges in seconds at FAST. optimizer.go's
// maxDropW/maxRiseW were a raw watts-PER-TICK allowance (1500/500), not a
// wall-clock rate — internal/orchestrator/constraint/export.go's ceilingSlewW
// (TASK-057/064, AD-007) already migrated the candidate path to a per-second
// physical rate scaled by the real tick length; the legacy cascade tested
// here was left on the un-scaled constants ("flagged for the STOCK spot-check
// at the wave gate", plantwiring_test.go) and never got the matching fix.
// scaleRateWPerTick (optimizer.go) closes that gap for the legacy path.

func TestScaleRateWPerTick_PreservesWallClockRateAcrossCadence(t *testing.T) {
	o := NewDefaultOptimizer()

	// No configured interval (unit-test default): unchanged, exactly the
	// pre-fix constant — every existing whitebox rule test in this file that
	// never calls SetTickInterval must see byte-identical behaviour.
	if got := o.scaleRateWPerTick(1500); got != 1500 {
		t.Errorf("no interval configured: scaleRateWPerTick(1500) = %v, want 1500 unchanged", got)
	}

	// At the tuned/FAST cadence (3 s), a no-op: reproduces the historical
	// 1500/500 W/tick exactly.
	o.SetTickInterval(tunedTickInterval)
	if got := o.scaleRateWPerTick(1500); got != 1500 {
		t.Errorf("FAST (tuned) cadence: scaleRateWPerTick(1500) = %v, want 1500", got)
	}
	if got := o.scaleRateWPerTick(500); got != 500 {
		t.Errorf("FAST (tuned) cadence: scaleRateWPerTick(500) = %v, want 500", got)
	}

	// At STOCK (15 s, 5x the tuned tick): the SAME physical rate (500 W/s /
	// 166.7 W/s) now moves 5x further per (5x longer) tick — matching
	// constraint/plantwiring_test.go's already-established STOCK expectation
	// (7500 W/tick drop, ~2500 W/tick rise) for the candidate path's
	// equivalent InverterPlant.MaxRampDownWPerS/MaxRampUpWPerS.
	o.SetTickInterval(15 * time.Second)
	if got := o.scaleRateWPerTick(1500); got != 7500 {
		t.Errorf("STOCK cadence: scaleRateWPerTick(1500) = %v, want 7500 (500 W/s x 15s)", got)
	}
	if got := o.scaleRateWPerTick(500); math.Abs(got-2500) > 1e-6 {
		t.Errorf("STOCK cadence: scaleRateWPerTick(500) = %v, want ~2500 (166.7 W/s x 15s)", got)
	}
}

// TestExportLimitRule_CeilingSlewHoldsWallClockRateAtStockCadence proves the
// applyExportLimitRule fix directly: seeded with a relaxed ceiling and a
// sustained large export (isolating the slew-limited feedback path from the
// feed-forward override by inflating the feed-forward term's own inputs far
// past anything the slew could reach in these few ticks — see the huge
// sol.PowerW below), the ceiling must tighten at the SAME wall-clock rate
// (watts per real second) regardless of engine cadence. Before the fix, the
// SAME tick-count budget took 5x longer in wall-clock seconds at STOCK's 15 s
// tick than at the FAST/tuned 3 s tick.
func TestExportLimitRule_CeilingSlewHoldsWallClockRateAtStockCadence(t *testing.T) {
	const startCeilingW = 20000.0 // an artificially large relaxed ceiling (many ticks' worth of tightening at either cadence)

	// ticksAndWallSecondsToZero drives applyExportLimitRule tick-by-tick from a
	// pre-relaxed ceiling under a sustained, huge export, and returns how many
	// ticks (and equivalent wall-clock seconds) it takes for the commanded
	// ceiling to first reach 0 (fully compliant against the 0 W cap).
	ticksAndWallSecondsToZero := func(tickInterval time.Duration) (ticks int, wallS float64) {
		o := NewDefaultOptimizer()
		o.SetTickInterval(tickInterval)
		// Seed the guard as already engaged mid-episode (not the unclamped
		// "first tick" one-step correction) with a relaxed ceiling.
		o.expGuard = exportGuard{
			evSetpointA: math.NaN(), evCmdW: math.NaN(), batteryAbsorbW: math.NaN(),
			activeLimitW: 0, filteredExportW: startCeilingW, solarCeilingW: startCeilingW,
		}
		limits := gridConstraints{exportLimitW: 0, importLimitW: math.NaN(), maxLimitW: math.NaN()}
		bats := []BatteryState{ruleBat("bat", 0, 100, 5000)} // full — no absorption lever
		// PowerW is deliberately far past nameplate so the feed-forward term
		// (homeSinkW = totalSolarW − actualExportW − ...) computes a value the
		// slew-limited feedback never catches up to within these ticks — this
		// isolates the slew mechanism under test from the feed-forward bypass.
		solar := []SolarState{{Name: "pv", PowerW: 1_000_000, MaxW: startCeilingW, Connected: true, Energized: true}}

		for i := 1; i <= 100; i++ {
			plan := &Plan{}
			o.applyExportLimitRule(solar, nil, 0, limits, -20000 /* sustained 20 kW export reading */, 95, 20000, bats, plan)
			for _, sc := range plan.SolarCommands {
				if sc.Name == "pv" && sc.CurtailToW <= 0 {
					return i, float64(i) * o.tickSeconds()
				}
			}
		}
		t.Fatalf("tickInterval=%v: ceiling never reached 0 within 100 ticks", tickInterval)
		return 0, 0
	}

	fastTicks, fastWallS := ticksAndWallSecondsToZero(tunedTickInterval) // 3 s
	stockTicks, stockWallS := ticksAndWallSecondsToZero(15 * time.Second)

	if stockTicks >= fastTicks {
		t.Errorf("STOCK ticks-to-zero = %d, want fewer than FAST's %d ticks (the scaled-up per-tick drop should close the SAME gap in fewer, longer ticks)", stockTicks, fastTicks)
	}
	// Cadence-invariant wall-clock convergence: within one STOCK tick's slop
	// (15 s) of each other. Pre-fix this would have been off by ~5x (STOCK
	// wall-clock time ≈ 5x FAST's, since both took the SAME tick count but
	// STOCK's ticks are 5x longer).
	if diff := math.Abs(fastWallS - stockWallS); diff > 15.0 {
		t.Errorf("wall-clock time to re-converge diverged across cadence: FAST=%.1fs (%d ticks) STOCK=%.1fs (%d ticks) — want within one STOCK tick (15s) of each other",
			fastWallS, fastTicks, stockWallS, stockTicks)
	}
}

// ── applyGenLimitRule (generation cap) ────────────────────────────────────────

func TestGenLimitRule_CurtailsToGenCap(t *testing.T) {
	// 6 kW total generation, 3.2 kW cap → curtail to 3.2 kW total, proportionally.
	solar := []SolarState{ruleSol("pv1", 4000), ruleSol("pv2", 2000)}
	plan := &Plan{}
	applyGenLimitRule(solar, 3200, plan)

	if len(plan.SolarCommands) != 2 {
		t.Fatalf("expected 2 solar commands, got %d", len(plan.SolarCommands))
	}
	total := 0.0
	for _, c := range plan.SolarCommands {
		total += c.CurtailToW
	}
	if math.Abs(total-3200) > 1 {
		t.Errorf("curtailed total = %.0fW, want 3200W (generation capped)", total)
	}
	if want := 3200 * 4.0 / 6.0; math.Abs(plan.SolarCommands[0].CurtailToW-want) > 1 {
		t.Errorf("pv1 curtailTo = %.0fW, want %.0fW (proportional)", plan.SolarCommands[0].CurtailToW, want)
	}
}

func TestGenLimitRule_WithinCapIssuesNoOpCeiling(t *testing.T) {
	// Sticky: the rule re-issues the ceiling every tick so the restore rule can't
	// un-curtail the inverter between ticks (the cause of the every-tick
	// oscillation across the cap).  When generation is within the cap, that
	// ceiling is at/above current output — a no-op the device clamps away.
	plan := &Plan{}
	applyGenLimitRule([]SolarState{ruleSol("pv", 2000)}, 3000, plan)
	if len(plan.SolarCommands) != 1 {
		t.Fatalf("sticky gen-limit must re-issue the ceiling, got %d commands", len(plan.SolarCommands))
	}
	if plan.SolarCommands[0].CurtailToW < 2000 {
		t.Errorf("ceiling %.0fW is below generation 2000W — would curtail within the cap",
			plan.SolarCommands[0].CurtailToW)
	}
}

func TestGenLimitRule_NaNIsNoOp(t *testing.T) {
	plan := &Plan{}
	applyGenLimitRule([]SolarState{ruleSol("pv", 9000)}, math.NaN(), plan)
	if len(plan.SolarCommands) != 0 {
		t.Errorf("NaN cap must be a no-op, got %d", len(plan.SolarCommands))
	}
}

// TestGenLimitRule_Sticky_HoldsCeilingAcrossTicks guards the oscillation fix: an
// inverter whose live reading is already AT the cap (because we curtailed it last
// tick) must still receive the ceiling command, so the restore rule doesn't
// un-curtail it and let generation jump back to full nameplate.
func TestGenLimitRule_Sticky_HoldsCeilingAcrossTicks(t *testing.T) {
	atCap := SolarState{Name: "pv", PowerW: 4300, MaxW: 5000, Connected: true, Energized: true}
	plan := &Plan{}
	applyGenLimitRule([]SolarState{atCap}, 4300, plan)
	if len(plan.SolarCommands) != 1 {
		t.Fatalf("expected a held ceiling command, got %d", len(plan.SolarCommands))
	}
	if plan.SolarCommands[0].CurtailToW > 4300+1 {
		t.Errorf("ceiling = %.0fW, want ≤ 4300W (held at the cap)", plan.SolarCommands[0].CurtailToW)
	}
}

func TestGenLimitRule_KeepsTighterExistingCurtailment(t *testing.T) {
	// The export-limit rule already curtailed pv to 1000 W; the gen cap (3200 W)
	// is looser, so the tighter existing curtailment must be preserved.
	solar := []SolarState{ruleSol("pv", 6000)}
	plan := &Plan{SolarCommands: []SolarCommand{{Name: "pv", CurtailToW: 1000}}}
	applyGenLimitRule(solar, 3200, plan)

	if len(plan.SolarCommands) != 1 {
		t.Fatalf("expected 1 command, got %d", len(plan.SolarCommands))
	}
	if plan.SolarCommands[0].CurtailToW != 1000 {
		t.Errorf("curtailTo = %.0fW, want 1000W (tighter export curtailment kept)", plan.SolarCommands[0].CurtailToW)
	}
}

// ── checkGenLimitConvergence (closed-loop actuation) ──────────────────────────

// TestGenLimitConvergence_SustainedOverageBreaches is the regression for the
// mayhem "ack-before-effect" FAIL: the inverter ACKs the curtailment but its
// output stays over the cap. After genBreachTicks sustained ticks the hub must
// record a generation breach (→ CannotComply) instead of asserting compliance.
func TestGenLimitConvergence_SustainedOverageBreaches(t *testing.T) {
	o := NewDefaultOptimizer()
	stuck := []SolarState{ruleSol("pv", 4650)} // output stuck at 4650W under a 1000W cap

	var plan *Plan
	for i := 0; i < genBreachTicks; i++ {
		plan = &Plan{}
		o.checkGenLimitConvergence(stuck, nil, math.NaN(), 1000, plan)
		if i < genBreachTicks-1 && plan.Breach != nil {
			t.Fatalf("breached early on tick %d (want a sustained gate)", i)
		}
	}
	if plan.Breach == nil {
		t.Fatal("expected a generation breach after sustained overage")
	}
	if plan.Breach.LimitType != "generation" {
		t.Errorf("LimitType = %q, want generation", plan.Breach.LimitType)
	}
	if plan.Breach.LimitW != 1000 || plan.Breach.MeasuredW != 4650 {
		t.Errorf("breach limit=%.0f measured=%.0f, want 1000/4650", plan.Breach.LimitW, plan.Breach.MeasuredW)
	}
	if math.Abs(plan.Breach.ShortfallW-3650) > 1 {
		t.Errorf("shortfall = %.0fW, want 3650W", plan.Breach.ShortfallW)
	}
}

// TestGenLimitConvergence_ConvergedNeverBreaches: an inverter that actually
// honours the cap must never trip the breach, no matter how long it's held.
func TestGenLimitConvergence_ConvergedNeverBreaches(t *testing.T) {
	o := NewDefaultOptimizer()
	ok := []SolarState{ruleSol("pv", 950)} // within the 1000W cap
	for i := 0; i < genBreachTicks+3; i++ {
		plan := &Plan{}
		o.checkGenLimitConvergence(ok, nil, math.NaN(), 1000, plan)
		if plan.Breach != nil {
			t.Fatalf("converged output must not breach (tick %d)", i)
		}
	}
}

// TestGenLimitConvergence_TransientRampResets: a normal ramp-down that spends a
// few ticks over the cap then converges must NOT breach — the over-count leaks
// back down on convergence, so a later single over-tick can't immediately fire.
func TestGenLimitConvergence_TransientRampResets(t *testing.T) {
	o := NewDefaultOptimizer()
	over := []SolarState{ruleSol("pv", 4650)}
	under := []SolarState{ruleSol("pv", 900)}

	for i := 0; i < genBreachTicks-1; i++ { // a few ticks over (normal ramp), not yet a breach
		o.checkGenLimitConvergence(over, nil, math.NaN(), 1000, &Plan{})
	}
	converged := &Plan{}
	o.checkGenLimitConvergence(under, nil, math.NaN(), 1000, converged)
	if converged.Breach != nil {
		t.Fatal("a transient overage that converged must not breach")
	}
	next := &Plan{}
	o.checkGenLimitConvergence(over, nil, math.NaN(), 1000, next)
	if next.Breach != nil {
		t.Fatal("over-count must reset after convergence — one later over-tick should not breach")
	}
}

// TestGenLimitConvergence_NewCapResetsGuard: changing the cap starts a fresh
// convergence session, so a backlog of over-ticks from the old cap doesn't carry.
func TestGenLimitConvergence_NewCapResetsGuard(t *testing.T) {
	o := NewDefaultOptimizer()
	over := []SolarState{ruleSol("pv", 4650)}
	for i := 0; i < genBreachTicks-1; i++ {
		o.checkGenLimitConvergence(over, nil, math.NaN(), 1000, &Plan{})
	}
	plan := &Plan{}
	o.checkGenLimitConvergence(over, nil, math.NaN(), 2000, plan) // a different cap → guard resets
	if plan.Breach != nil {
		t.Fatal("a new cap must reset the convergence guard")
	}
}

// TestGenLimitConvergence_ToleratesBlip is the regression for the
// reject-write/enable-gate-curtail nondeterminism: a sustained over-cap breach
// with a single sub-threshold sample mid-run (an HIL meter blip) must still
// escalate. The leaky counter decrements on the blip instead of resetting, so the
// breach is only delayed, not lost.
func TestGenLimitConvergence_ToleratesBlip(t *testing.T) {
	o := NewDefaultOptimizer()
	over := []SolarState{ruleSol("pv", 4650)}
	under := []SolarState{ruleSol("pv", 900)}

	// over×4 → count 4; one blip → 3; then over until it reaches genBreachTicks.
	seq := []bool{true, true, true, true, false, true, true, true}
	var last *Plan
	for _, isOver := range seq {
		last = &Plan{}
		s := over
		if !isOver {
			s = under
		}
		o.checkGenLimitConvergence(s, nil, math.NaN(), 1000, last)
	}
	if last.Breach == nil {
		t.Fatal("a sustained breach with one sub-threshold blip must still escalate (leaky counter)")
	}
}

// TestGenLimitConvergence_ToleratesCapJitter: a cap value that jitters within the
// noise band tick-to-tick (watts→ActivePower round-trip) must not reset the guard
// — otherwise overCount never reaches genBreachTicks. With bit-exact reset this
// breach was silently lost (the other half of the nondeterminism).
func TestGenLimitConvergence_ToleratesCapJitter(t *testing.T) {
	o := NewDefaultOptimizer()
	over := []SolarState{ruleSol("pv", 4650)}
	jitter := []float64{1000.0, 1000.04, 999.97, 1000.02, 999.99, 1000.01}
	var last *Plan
	for _, cap := range jitter {
		last = &Plan{}
		o.checkGenLimitConvergence(over, nil, math.NaN(), cap, last)
	}
	if last.Breach == nil {
		t.Fatal("a sustained breach under a jittering cap must still escalate (tolerance-band guard reset)")
	}
}

func TestExportLimitRule_UpdatesBatteryPowerW(t *testing.T) {
	// Verify the returned slice has updated PowerW so later rules see residual headroom.
	solar := []SolarState{ruleSol("pv", 5000)}
	bats := []BatteryState{ruleBat("bat", 0, 50, 3000)} // max charge 3 kW
	limits := gridConstraints{exportLimitW: 1000, importLimitW: math.NaN(), maxLimitW: math.NaN()}
	plan := &Plan{}

	updated, _ := applyExportLimitRule(solar, nil, 0, limits, -5000, 95, 5000, bats, plan)

	// 4 kW excess, battery absorbs 3 kW → PowerW = 0 - 3000 = -3000
	if math.Abs(updated[0].PowerW+3000) > 1 {
		t.Errorf("updated PowerW = %.0f, want -3000", updated[0].PowerW)
	}
}

func TestExportLimitRule_BatteryChargesBeforeEVSE(t *testing.T) {
	// 5 kW export, limit 2 kW, conservative target 1700 W, excess 3300 W.
	// Battery has 5 kW headroom — it should absorb all 3300 W.  EV stays at
	// the IEC 6A minimum: charging from grid import doesn't violate an export
	// limit, and dropping the EV session every time the battery happens to
	// cover the excess made sessions visibly stutter in the lab.
	solar := []SolarState{ruleSol("pv", 5000)}
	evses := []EVSEState{ruleEVSE("cs-001", true, 16, 230)} // 16A max, currently 0A
	bats := []BatteryState{ruleBat("bat", 0, 50, 5000)}
	limits := gridConstraints{exportLimitW: 2000, importLimitW: math.NaN(), maxLimitW: math.NaN()}
	plan := &Plan{}

	applyExportLimitRule(solar, evses, 0, limits, -5000, 95, 5000, bats, plan)

	if len(plan.BatteryCommands) == 0 {
		t.Fatal("expected battery charge command")
	}
	if plan.BatteryCommands[0].SetpointW >= 0 {
		t.Errorf("battery should be charging, got %.0fW", plan.BatteryCommands[0].SetpointW)
	}
	if len(plan.EVSECommands) == 0 || plan.EVSECommands[0].MaxCurrentA < 6 {
		t.Errorf("expected EVSE held at 6A minimum, got %v", plan.EVSECommands)
	}
}

func TestExportLimitRule_EVSEAbsorbsWhenBatteryCapLimited(t *testing.T) {
	// 8 kW solar, 1 kW load, 1 kW export limit, conservative target 850 W.
	// Projected excess = 7000 - 850 = 6150 W.
	// Battery max charge 5 kW: absorbs 5000 W, leaving 1150 W for EV.
	// EV at 230V: 1150/230 = 5A < 6A minimum → set to 6A.
	solar := []SolarState{ruleSol("pv", 8000)}
	evses := []EVSEState{ruleEVSE("cs-001", true, 32, 230)} // EV session active, 0W measured
	bats := []BatteryState{ruleBat("bat", 0, 40, 5000)}     // 5 kW max charge
	limits := gridConstraints{exportLimitW: 1000, importLimitW: math.NaN(), maxLimitW: math.NaN()}
	plan := &Plan{}

	// netW=-7000: site exports 7000W (solar 8000 - load 1000), no EV load yet.
	applyExportLimitRule(solar, evses, 0, limits, -7000, 95, 7000, bats, plan)

	if len(plan.BatteryCommands) == 0 {
		t.Fatal("expected battery charge command")
	}
	if math.Abs(plan.BatteryCommands[0].SetpointW+5000) > 1 {
		t.Errorf("battery setpoint = %.0fW, want -5000W", plan.BatteryCommands[0].SetpointW)
	}
	if len(plan.EVSECommands) == 0 {
		t.Fatal("expected EVSE command for remaining excess after battery")
	}
	// 1150W remaining → 5A < min 6A → set to 6A minimum
	if plan.EVSECommands[0].MaxCurrentA < 6 {
		t.Errorf("EV setpoint = %.1fA, want ≥ 6A", plan.EVSECommands[0].MaxCurrentA)
	}
}

// ── applySelfConsumptionRule ──────────────────────────────────────────────────

func TestSelfConsumptionRule_BelowThreshold(t *testing.T) {
	bats := []BatteryState{ruleBat("bat", 0, 50, 5000)}
	plan := &Plan{}

	_, surplusOut := applySelfConsumptionRule(bats, 50, 100, 95, plan)

	if len(plan.BatteryCommands) != 0 {
		t.Error("expected no command below excess threshold")
	}
	if surplusOut != 50 {
		t.Errorf("surplusW unchanged below threshold, got %.0f", surplusOut)
	}
}

func TestSelfConsumptionRule_ChargesWhenSurplus(t *testing.T) {
	bats := []BatteryState{ruleBat("bat", 0, 50, 5000)}
	plan := &Plan{}

	updated, surplusOut := applySelfConsumptionRule(bats, 3000, 100, 95, plan)

	if len(plan.BatteryCommands) == 0 {
		t.Fatal("expected battery charge command")
	}
	if plan.BatteryCommands[0].SetpointW >= 0 {
		t.Errorf("expected negative setpoint (charging), got %.0f", plan.BatteryCommands[0].SetpointW)
	}
	if updated[0].PowerW >= 0 {
		t.Errorf("updated PowerW should be negative (charging), got %.0f", updated[0].PowerW)
	}
	if surplusOut >= 3000 {
		t.Errorf("surplusW should decrease after charging, got %.0f", surplusOut)
	}
}

func TestSelfConsumptionRule_SkipsExistingCommand(t *testing.T) {
	bats := []BatteryState{ruleBat("bat", 0, 50, 5000)}
	plan := &Plan{BatteryCommands: []BatteryCommand{{Name: "bat", SetpointW: -3000}}}

	applySelfConsumptionRule(bats, 5000, 100, 95, plan)

	if len(plan.BatteryCommands) != 1 {
		t.Errorf("expected 1 command (no duplicate), got %d", len(plan.BatteryCommands))
	}
}

func TestSelfConsumptionRule_SkipsFullBattery(t *testing.T) {
	bats := []BatteryState{ruleBat("bat", 0, 96, 5000)} // SOC > threshold 95
	plan := &Plan{}

	applySelfConsumptionRule(bats, 5000, 100, 95, plan)

	for _, cmd := range plan.BatteryCommands {
		if cmd.SetpointW < 0 {
			t.Errorf("should not charge battery above SOCFull, setpoint=%.0f", cmd.SetpointW)
		}
	}
}

// ── applyDemandResponseRule ───────────────────────────────────────────────────

func TestDemandResponseRule_DischargesWhenDR(t *testing.T) {
	bats := []BatteryState{ruleBat("bat", 0, 80, 5000)}
	plan := &Plan{}

	updated, surplusW := applyDemandResponseRule(bats, 0, 20, true, false, "", math.NaN(), plan)

	if len(plan.BatteryCommands) == 0 {
		t.Fatal("expected discharge command during DR")
	}
	if plan.BatteryCommands[0].SetpointW <= 0 {
		t.Errorf("expected positive setpoint (discharge), got %.0f", plan.BatteryCommands[0].SetpointW)
	}
	if updated[0].PowerW <= 0 {
		t.Errorf("updated PowerW should be positive (discharging)")
	}
	if surplusW <= 0 {
		t.Errorf("surplusW should increase after discharge, got %.0f", surplusW)
	}
}

func TestDemandResponseRule_RespectsSOCReserve(t *testing.T) {
	bats := []BatteryState{ruleBat("bat", 0, 15, 5000)} // SOC=15 < reserve=20
	plan := &Plan{}

	applyDemandResponseRule(bats, 0, 20, true, false, "", math.NaN(), plan)

	for _, cmd := range plan.BatteryCommands {
		if cmd.SetpointW > 0 {
			t.Errorf("must not discharge below SOC reserve, got setpoint=%.0f", cmd.SetpointW)
		}
	}
}

func TestDemandResponseRule_NoActionWhenInactive(t *testing.T) {
	bats := []BatteryState{ruleBat("bat", 0, 80, 5000)}
	plan := &Plan{}

	applyDemandResponseRule(bats, 0, 20, false, false, "", math.NaN(), plan)

	if len(plan.BatteryCommands) != 0 {
		t.Errorf("expected no commands when isDR=false isPeak=false, got %d", len(plan.BatteryCommands))
	}
}

func TestDemandResponseRule_SkipsExistingCommand(t *testing.T) {
	bats := []BatteryState{ruleBat("bat", 0, 80, 5000)}
	plan := &Plan{BatteryCommands: []BatteryCommand{{Name: "bat", SetpointW: -2000}}}

	applyDemandResponseRule(bats, 0, 20, true, false, "", math.NaN(), plan)

	if len(plan.BatteryCommands) != 1 {
		t.Errorf("expected 1 command (no duplicate), got %d", len(plan.BatteryCommands))
	}
}

func TestDemandResponseRule_DischargesWhenPeak(t *testing.T) {
	bats := []BatteryState{ruleBat("bat", 0, 80, 5000)}
	plan := &Plan{}

	applyDemandResponseRule(bats, 0, 20, false, true, "peak TOU hour", math.NaN(), plan)

	if len(plan.BatteryCommands) == 0 {
		t.Fatal("expected discharge command during TOU peak")
	}
	if plan.BatteryCommands[0].SetpointW <= 0 {
		t.Errorf("expected positive setpoint (discharge), got %.0f", plan.BatteryCommands[0].SetpointW)
	}
}

func TestDemandResponseRule_CappedByExportHeadroom(t *testing.T) {
	// 5 kW battery but only 1.2 kW of export headroom → setpoint must be
	// capped, not MaxDischargeW (C6: no one-tick export-limit overshoot).
	bats := []BatteryState{ruleBat("bat", 0, 80, 5000)}
	plan := &Plan{}

	updated, _ := applyDemandResponseRule(bats, 0, 20, false, true, "peak TOU hour", 1200, plan)

	if len(plan.BatteryCommands) != 1 {
		t.Fatalf("expected 1 discharge command, got %d", len(plan.BatteryCommands))
	}
	if got := plan.BatteryCommands[0].SetpointW; got != 1200 {
		t.Errorf("setpoint = %.0f, want 1200 (capped by export headroom)", got)
	}
	if updated[0].PowerW != 1200 {
		t.Errorf("updated PowerW = %.0f, want 1200", updated[0].PowerW)
	}
}

func TestDemandResponseRule_CapSharedAcrossBatteries(t *testing.T) {
	// 3 kW total headroom over two 5 kW batteries: first takes 3 kW, second
	// must be withheld entirely.
	bats := []BatteryState{ruleBat("bat-0", 0, 80, 5000), ruleBat("bat-1", 0, 80, 5000)}
	plan := &Plan{}

	applyDemandResponseRule(bats, 0, 20, true, false, "", 3000, plan)

	if len(plan.BatteryCommands) != 1 {
		t.Fatalf("expected 1 discharge command (second withheld), got %d", len(plan.BatteryCommands))
	}
	if got := plan.BatteryCommands[0].SetpointW; got != 3000 {
		t.Errorf("setpoint = %.0f, want 3000", got)
	}
}

func TestDemandResponseRule_ZeroHeadroomNoDischarge(t *testing.T) {
	bats := []BatteryState{ruleBat("bat", 0, 80, 5000)}
	plan := &Plan{}

	applyDemandResponseRule(bats, 0, 20, false, true, "peak TOU hour", 0, plan)

	if len(plan.BatteryCommands) != 0 {
		t.Errorf("expected no discharge with zero export headroom, got %d commands", len(plan.BatteryCommands))
	}
}

// ── applyImportLimitRule EV gate seeding ──────────────────────────────────────

func TestImportLimitRule_FreshCompliantLimit_DoesNotGateEV(t *testing.T) {
	// An import limit that arrives while the site is already under the cap
	// must not trip the EV cooldown gate (it exists for post-violation
	// recovery, not limit arrival).
	o := NewDefaultOptimizer()
	bats := []BatteryState{ruleBat("bat", 0, 80, 5000)}
	limits := gridConstraints{importLimitW: 3000, exportLimitW: math.NaN(), maxLimitW: math.NaN()}
	plan := &Plan{}

	o.applyImportLimitRule(bats, limits, 1000, 20, plan) // importing 1 kW, cap 3 kW

	if o.impGuard.evSafeCount < o.EVImportCooldownCycles {
		t.Errorf("evSafeCount = %d, want ≥ cooldown %d when limit arrives while compliant",
			o.impGuard.evSafeCount, o.EVImportCooldownCycles)
	}
}

func TestImportLimitRule_FreshViolatedLimit_GatesEV(t *testing.T) {
	o := NewDefaultOptimizer()
	bats := []BatteryState{ruleBat("bat", 0, 80, 5000)}
	limits := gridConstraints{importLimitW: 3000, exportLimitW: math.NaN(), maxLimitW: math.NaN()}
	plan := &Plan{}

	o.applyImportLimitRule(bats, limits, 4000, 20, plan) // importing 4 kW, over the 3 kW cap

	if o.impGuard.evSafeCount >= o.EVImportCooldownCycles {
		t.Errorf("evSafeCount = %d, want < cooldown %d when limit arrives in violation",
			o.impGuard.evSafeCount, o.EVImportCooldownCycles)
	}
}

// ── applyEVChargingRule ───────────────────────────────────────────────────────

func TestEVChargingRule_SuspendsAtImportLimit(t *testing.T) {
	evses := []EVSEState{ruleEVSE("cs-001", true, 16, 230)}
	limits := gridConstraints{exportLimitW: math.NaN(), importLimitW: 3000, maxLimitW: math.NaN()}
	plan := &Plan{}

	applyEVChargingRule(evses, limits, 3500, 0, 0, false, plan) // grid 3500 W > limit 3000 W

	if len(plan.EVSECommands) == 0 {
		t.Fatal("expected EVSE command")
	}
	if plan.EVSECommands[0].MaxCurrentA != 0 {
		t.Errorf("expected suspend (0A), got %.1fA", plan.EVSECommands[0].MaxCurrentA)
	}
}

func TestEVChargingRule_FullRateWithAmpleSolar(t *testing.T) {
	evses := []EVSEState{ruleEVSE("cs-001", true, 16, 230)}
	plan := &Plan{}

	// 10 kW solar, 10 kW surplus → EVSE (3.68 kW) gets full rate.
	applyEVChargingRule(evses, noLimits(), math.NaN(), 10000, 10000, false, plan)

	if len(plan.EVSECommands) == 0 {
		t.Fatal("expected EVSE command")
	}
	if plan.EVSECommands[0].MaxCurrentA != 16 {
		t.Errorf("expected 16A (full rate), got %.1fA", plan.EVSECommands[0].MaxCurrentA)
	}
}

func TestEVChargingRule_SuspendsWhenZeroSurplusExportAndImportLimited(t *testing.T) {
	evses := []EVSEState{ruleEVSE("cs-001", true, 32, 230)}
	plan := &Plan{}

	// Export limit + import limit both active, zero solar surplus.
	// budgetW=0 → can't supplement (import headroom=0 from tight import limit), suspend.
	limits := gridConstraints{exportLimitW: 5000, importLimitW: 0, maxLimitW: math.NaN()}
	// netW=0 → import headroom = 0 - 0 = 0; supplement of 1380W > 0 headroom → suspend.
	applyEVChargingRule(evses, limits, 0, 1000, 0, false, plan)

	if len(plan.EVSECommands) == 0 {
		t.Fatal("expected EVSE command")
	}
	if plan.EVSECommands[0].MaxCurrentA != 0 {
		t.Errorf("expected suspend (0A) when both limits set and no import headroom, got %.1fA",
			plan.EVSECommands[0].MaxCurrentA)
	}
}

func TestEVChargingRule_FullChargeWhenUnconstrainedAndNoSolar(t *testing.T) {
	// No constraint AND no solar production — nothing to throttle against,
	// so the EV charges at full rate (grid-only scenarios like night charging).
	evses := []EVSEState{ruleEVSE("cs-001", true, 32, 230)}
	plan := &Plan{}

	applyEVChargingRule(evses, noLimits(), math.NaN(), 0, 0, false, plan)

	if len(plan.EVSECommands) == 0 {
		t.Fatal("expected EVSE command")
	}
	if plan.EVSECommands[0].MaxCurrentA != 32 {
		t.Errorf("expected full 32A when unconstrained and solar=0, got %.1fA", plan.EVSECommands[0].MaxCurrentA)
	}
}

func TestEVChargingRule_ThrottledWhenUnconstrainedAndLowSolar(t *testing.T) {
	// Self-consumption priority: with solar producing but below EV max draw,
	// the EV must throttle to the surplus rather than drawing from the grid.
	evses := []EVSEState{ruleEVSE("cs-001", true, 32, 230)}
	plan := &Plan{}

	// solar=2000W, surplus=2000W, EV max=32A*230=7360W → should throttle.
	applyEVChargingRule(evses, noLimits(), math.NaN(), 2000, 2000, false, plan)

	if len(plan.EVSECommands) == 0 {
		t.Fatal("expected EVSE command")
	}
	got := plan.EVSECommands[0].MaxCurrentA
	if got >= 32 {
		t.Errorf("expected throttle below 32A when solar < EV max, got %.1fA", got)
	}
	if got < 6 {
		t.Errorf("expected at least minimum 6A, got %.1fA", got)
	}
}

func TestEVChargingRule_NoSessionNoCommand(t *testing.T) {
	evses := []EVSEState{ruleEVSE("cs-001", false, 16, 230)} // no session
	plan := &Plan{}

	applyEVChargingRule(evses, noLimits(), math.NaN(), 10000, 10000, false, plan)

	if len(plan.EVSECommands) != 0 {
		t.Errorf("expected no command with no active session, got %d", len(plan.EVSECommands))
	}
}

// ── applyRestoreRule ──────────────────────────────────────────────────────────

func TestRestoreRule_RestoresUnconstrainedSolar(t *testing.T) {
	solar := []SolarState{ruleSol("pv", 5000)}
	plan := &Plan{}

	applyRestoreRule(solar, nil, 20, false, plan)

	if len(plan.SolarCommands) == 0 {
		t.Fatal("expected restore command for unconstrained solar")
	}
	if !math.IsNaN(plan.SolarCommands[0].CurtailToW) {
		t.Errorf("restore command must have NaN CurtailToW, got %.0f", plan.SolarCommands[0].CurtailToW)
	}
}

func TestRestoreRule_SkipsSolarAlreadyCommanded(t *testing.T) {
	solar := []SolarState{ruleSol("pv", 5000)}
	plan := &Plan{SolarCommands: []SolarCommand{{Name: "pv", CurtailToW: 3000}}}

	applyRestoreRule(solar, nil, 20, false, plan)

	if len(plan.SolarCommands) != 1 {
		t.Errorf("must not add second solar command, got %d", len(plan.SolarCommands))
	}
}

// TestRestoreRule_RestoresDisconnectedSolarWhenNoCap is the regression for the
// stuck-curtailment-on-reconnect cluster (QA 2026-07-03: curtailment-release,
// clock-jump-forward, release-while-rebooting). An inverter that is DARK on the
// ticks after the export cap clears must still get the restore queued: the
// southbound retryDevice records every command as the device's desired state
// while disconnected and re-asserts it on reconnect, so this command is the only
// thing that overwrites the stale curtailment latched before the fault. Gating
// on Connected left the device clamped at the old ceiling forever.
func TestRestoreRule_RestoresDisconnectedSolarWhenNoCap(t *testing.T) {
	dark := ruleSol("pv", 0)
	dark.Connected = false
	dark.Energized = false
	plan := &Plan{}

	applyRestoreRule([]SolarState{dark}, nil, 20, false, plan)

	if len(plan.SolarCommands) != 1 {
		t.Fatalf("expected a restore command for the disconnected inverter, got %d", len(plan.SolarCommands))
	}
	if plan.SolarCommands[0].Name != "pv" || !math.IsNaN(plan.SolarCommands[0].CurtailToW) {
		t.Errorf("restore command must be {pv, NaN}, got %+v", plan.SolarCommands[0])
	}
}

// TestRestoreRule_HoldsDarkSolarWhileCapActive: while an export/generation cap is
// ACTIVE the cap rules command only connected inverters, so a dark inverter's
// recorded desired state (southbound lastCtrl) still holds the live curtailment.
// Queuing a restore would overwrite it and a reconnecting inverter would snap to
// full nameplate under the active cap.
func TestRestoreRule_HoldsDarkSolarWhileCapActive(t *testing.T) {
	dark := ruleSol("pv", 0)
	dark.Connected = false
	dark.Energized = false
	plan := &Plan{}

	applyRestoreRule([]SolarState{dark}, nil, 20, true, plan)

	if len(plan.SolarCommands) != 0 {
		t.Errorf("dark inverter under an active cap must keep its held curtailment, got %+v", plan.SolarCommands)
	}
}

// A CONNECTED uncommanded inverter is still restored while a cap is active (the
// cap rules command every connected inverter each tick, so an uncommanded one is
// genuinely unconstrained — preserve the pre-existing behavior).
func TestRestoreRule_RestoresConnectedSolarEvenWithCapActive(t *testing.T) {
	solar := []SolarState{ruleSol("pv", 5000)}
	plan := &Plan{}

	applyRestoreRule(solar, nil, 20, true, plan)

	if len(plan.SolarCommands) != 1 || !math.IsNaN(plan.SolarCommands[0].CurtailToW) {
		t.Errorf("connected uncommanded inverter must still be restored, got %+v", plan.SolarCommands)
	}
}

func TestRestoreRule_RestoresBattery(t *testing.T) {
	bats := []BatteryState{ruleBat("bat", 0, 50, 5000)}
	plan := &Plan{}

	applyRestoreRule(nil, bats, 20, false, plan)

	if len(plan.BatteryCommands) == 0 {
		t.Fatal("expected restore command for unconstrained battery")
	}
	// Battery restore idles at 0 W (clears any latched charge/discharge setpoint).
	if plan.BatteryCommands[0].SetpointW != 0 {
		t.Errorf("restore command should idle at 0W, got %.0f", plan.BatteryCommands[0].SetpointW)
	}
}

func TestRestoreRule_IdlesLatchedDischargeBelowSOCReserve(t *testing.T) {
	// Regression for the 92-day replay reserve drain: a battery left discharging
	// (latched setpoint) at or below the SOC reserve, with no command issued by
	// any other rule this tick, MUST be idled to 0 W.  Skipping it (the old
	// behavior) left the device running its latched discharge and drained the
	// pack straight through the reserve to 0%.
	bats := []BatteryState{ruleBat("bat", 2000, 15, 5000)} // discharging 2 kW at SOC=15 (≤ reserve=20)
	plan := &Plan{}

	applyRestoreRule(nil, bats, 20, false, plan)

	if len(plan.BatteryCommands) != 1 {
		t.Fatalf("expected an idle command to stop discharge below reserve, got %d", len(plan.BatteryCommands))
	}
	if plan.BatteryCommands[0].SetpointW != 0 {
		t.Errorf("must idle a below-reserve battery to 0W to protect the reserve, got %.0fW",
			plan.BatteryCommands[0].SetpointW)
	}
}

func TestRestoreRule_SkipsBatteryAlreadyCommanded(t *testing.T) {
	bats := []BatteryState{ruleBat("bat", 0, 50, 5000)}
	plan := &Plan{BatteryCommands: []BatteryCommand{{Name: "bat", SetpointW: -2000}}}

	applyRestoreRule(nil, bats, 20, false, plan)

	if len(plan.BatteryCommands) != 1 {
		t.Errorf("must not add second battery command, got %d", len(plan.BatteryCommands))
	}
}

// ── applyFixedDispatchRule ────────────────────────────────────────────────────

func TestFixedDispatchRule_NilCSIP_NoAction(t *testing.T) {
	bats := []BatteryState{ruleBat("bat", 0, 80, 5000)}
	plan := &Plan{}

	applyFixedDispatchRule(nil, bats, 0, math.NaN(), 20, plan)

	if len(plan.BatteryCommands) != 0 {
		t.Error("expected no commands with nil CSIP")
	}
}

func TestFixedDispatchRule_SolarCoversTarget_NoBatteryNeeded(t *testing.T) {
	// Solar 10kW, home 1kW → 9kW available. Target = 5kW. Solar covers it.
	bats := []BatteryState{ruleBat("bat", 0, 80, 5000)}
	cc := &CSIPControlState{Base: model.DERControlBase{OpModFixedW: &model.ActivePower{Value: 5000}}}
	plan := &Plan{}

	applyFixedDispatchRule(cc, bats, 10000, 1000, 20, plan)

	if len(plan.BatteryCommands) != 0 {
		t.Error("expected no battery commands when solar covers target")
	}
	if len(plan.Decisions) == 0 {
		t.Error("expected a decision recording the no-op")
	}
}

func TestFixedDispatchRule_DischargesBatteryForShortfall(t *testing.T) {
	// Solar 10kW, home 1kW → 9kW available. Target = 10kW. Shortfall = 1kW.
	bats := []BatteryState{ruleBat("bat", 0, 80, 5000)}
	cc := &CSIPControlState{Base: model.DERControlBase{OpModFixedW: &model.ActivePower{Value: 10000}}}
	plan := &Plan{}

	updated := applyFixedDispatchRule(cc, bats, 10000, 1000, 20, plan)

	if len(plan.BatteryCommands) == 0 {
		t.Fatal("expected battery discharge command")
	}
	cmd := plan.BatteryCommands[0]
	if cmd.SetpointW <= 0 {
		t.Errorf("expected positive setpoint (discharge), got %.0f", cmd.SetpointW)
	}
	// Shortfall = 1kW → setpoint should be ~1000W.
	if cmd.SetpointW < 500 || cmd.SetpointW > 2000 {
		t.Errorf("setpoint = %.0fW; expected ~1000W", cmd.SetpointW)
	}
	if updated[0].PowerW <= 0 {
		t.Error("updated PowerW should be positive (discharging)")
	}
}

func TestFixedDispatchRule_RespectsSOCReserve(t *testing.T) {
	bats := []BatteryState{ruleBat("bat", 0, 15, 5000)} // SOC=15 < reserve=20
	cc := &CSIPControlState{Base: model.DERControlBase{OpModFixedW: &model.ActivePower{Value: 5000}}}
	plan := &Plan{}

	applyFixedDispatchRule(cc, bats, 0, math.NaN(), 20, plan)

	for _, cmd := range plan.BatteryCommands {
		if cmd.SetpointW > 0 {
			t.Errorf("must not discharge below SOC reserve, got %.0fW", cmd.SetpointW)
		}
	}
}

func TestFixedDispatchRule_NoMeter_UsesSolarAsFallback(t *testing.T) {
	// No grid meter (homeLoadW=NaN) → solar output used as available export.
	// Solar 3kW, target 5kW → shortfall 2kW → discharge battery.
	bats := []BatteryState{ruleBat("bat", 0, 80, 5000)}
	cc := &CSIPControlState{Base: model.DERControlBase{OpModFixedW: &model.ActivePower{Value: 5000}}}
	plan := &Plan{}

	applyFixedDispatchRule(cc, bats, 3000, math.NaN(), 20, plan)

	if len(plan.BatteryCommands) == 0 {
		t.Fatal("expected battery discharge for shortfall")
	}
	cmd := plan.BatteryCommands[0]
	if cmd.SetpointW < 1500 || cmd.SetpointW > 2500 {
		t.Errorf("setpoint = %.0fW; expected ~2000W (shortfall=5000-3000)", cmd.SetpointW)
	}
}

// ── EV minimum-current supplement ────────────────────────────────────────────

func TestEVChargingRule_MinCurrentSupplementWithExportLimit(t *testing.T) {
	// Export limit active, 1kW solar surplus, no import limit.
	// Surplus (4.35A) < minimum 6A but supplement from grid is allowed.
	evses := []EVSEState{ruleEVSE("cs-001", true, 32, 230)}
	limits := gridConstraints{exportLimitW: 0, importLimitW: math.NaN(), maxLimitW: math.NaN()}
	plan := &Plan{}

	// netW=-1000 (exporting 1kW), solarW=2000, surplusW=1000.
	applyEVChargingRule(evses, limits, -1000, 2000, 1000, false, plan)

	if len(plan.EVSECommands) == 0 {
		t.Fatal("expected EVSE command")
	}
	cmd := plan.EVSECommands[0]
	if cmd.MaxCurrentA < 6.0 {
		t.Errorf("expected ≥6A (minimum with supplement), got %.1fA", cmd.MaxCurrentA)
	}
}

func TestEVChargingRule_NoSupplementNeededWhenUnconstrained(t *testing.T) {
	// No constraint AND solar amply covers EV draw — no throttle, no supplement.
	evses := []EVSEState{ruleEVSE("cs-001", true, 32, 230)}
	plan := &Plan{}

	// solar=20kW, surplus=20kW, EV max=32A*230=7360W → solar amply covers; full rate.
	applyEVChargingRule(evses, noLimits(), math.NaN(), 20000, 20000, false, plan)

	if len(plan.EVSECommands) == 0 {
		t.Fatal("expected EVSE command")
	}
	if plan.EVSECommands[0].MaxCurrentA != 32 {
		t.Errorf("expected full 32A when solar comfortably covers EV draw, got %.1fA", plan.EVSECommands[0].MaxCurrentA)
	}
}

func TestEVChargingRule_FullChargeWhenExportLimitActiveButImporting(t *testing.T) {
	// Export limit active (5 kW CSIP default) but site is currently importing from
	// grid (netW > 0).  The export-limit rule found no excess; EV should charge at
	// full rate rather than being throttled by the solar-surplus path.
	evses := []EVSEState{ruleEVSE("cs-001", true, 32, 230)}
	limits := gridConstraints{exportLimitW: 5000, importLimitW: math.NaN(), maxLimitW: math.NaN()}
	plan := &Plan{}

	// netW=2880 → site importing 2880W (EV load + home - solar)
	applyEVChargingRule(evses, limits, 2880, 2000, -880, false, plan)

	if len(plan.EVSECommands) == 0 {
		t.Fatal("expected EVSE command")
	}
	if plan.EVSECommands[0].MaxCurrentA != 32 {
		t.Errorf("expected full 32A when export-limited but importing, got %.1fA",
			plan.EVSECommands[0].MaxCurrentA)
	}
}

func TestEVChargingRule_SkipsAlreadyCommandedEVSE(t *testing.T) {
	// EVSE already has a command from the export-limit rule; EV rule must not override it.
	evses := []EVSEState{ruleEVSE("cs-001", true, 32, 230)}
	plan := &Plan{EVSECommands: []EVSECommand{{StationID: "cs-001", ConnectorID: 1, MaxCurrentA: 10}}}

	applyEVChargingRule(evses, noLimits(), math.NaN(), 10000, 10000, false, plan)

	if len(plan.EVSECommands) != 1 {
		t.Errorf("must not add duplicate EVSE command, got %d", len(plan.EVSECommands))
	}
}

// ── D8/WP-14: applyPlanRule EV setpoint wiring ────────────────────────────────

// TestPlanRule_EVDischargeTarget_SetsSetpointW: a genuine discharge target
// (positive EVSetpointW, battery convention — only reachable when the
// planner's ev_storage flag was on) switches the EVSE command to setpoint
// mode, and MaxCurrentA is zeroed so a stale ceiling never rides alongside
// it (EVSECommand.SetpointW's doc: "nil ⇒ ceiling mode; non-nil ⇒
// MaxCurrentA ignored downstream").
func TestPlanRule_EVDischargeTarget_SetsSetpointW(t *testing.T) {
	batteries := []BatteryState{ruleBat("bat", 0, 50, 5000)}
	evses := []EVSEState{ruleEVSE("cs-001", true, 32, 230)}
	target := &PlanTarget{BattSetpointW: 1000, EVMaxCurrentA: 0, EVSetpointW: 3000}
	plan := &Plan{}

	applyPlanRule(target, batteries, evses, 10, 90, 0, plan)

	if len(plan.EVSECommands) != 1 {
		t.Fatalf("expected 1 EVSE command, got %d", len(plan.EVSECommands))
	}
	cmd := plan.EVSECommands[0]
	if cmd.SetpointW == nil || *cmd.SetpointW != 3000 {
		t.Errorf("SetpointW = %v, want *3000 (discharge, battery convention)", cmd.SetpointW)
	}
	if cmd.MaxCurrentA != 0 {
		t.Errorf("MaxCurrentA = %.1f, want 0 (ignored once SetpointW is set)", cmd.MaxCurrentA)
	}
}

// TestPlanRule_EVChargeTarget_UsesMaxCurrentAOnly: EVSetpointW <= 0
// (charge/idle — the ENTIRE ev_storage-off universe, since the DP can only
// ever produce a positive EVSetpointW when the flag was on) must leave
// SetpointW nil and pass target.EVMaxCurrentA through exactly as before
// D8/WP-14 — the flag-off byte-identity contract at the optimizer layer.
func TestPlanRule_EVChargeTarget_UsesMaxCurrentAOnly(t *testing.T) {
	batteries := []BatteryState{ruleBat("bat", 0, 50, 5000)}
	evses := []EVSEState{ruleEVSE("cs-001", true, 32, 230)}
	target := &PlanTarget{BattSetpointW: 1000, EVMaxCurrentA: 16, EVSetpointW: -1500}
	plan := &Plan{}

	applyPlanRule(target, batteries, evses, 10, 90, 0, plan)

	if len(plan.EVSECommands) != 1 {
		t.Fatalf("expected 1 EVSE command, got %d", len(plan.EVSECommands))
	}
	cmd := plan.EVSECommands[0]
	if cmd.SetpointW != nil {
		t.Errorf("SetpointW = %v, want nil (charge/idle target must stay in ceiling mode)", *cmd.SetpointW)
	}
	if cmd.MaxCurrentA != 16 {
		t.Errorf("MaxCurrentA = %.1f, want 16 (target.EVMaxCurrentA passthrough, unchanged)", cmd.MaxCurrentA)
	}
}
