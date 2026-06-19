package orchestrator

import (
	"math"
	"testing"

	"lexa-hub/internal/northbound/model"
)

// TestExportLimit_PhantomEVCredit_StillCurtails guards Bug E: the export-limit
// curtailment fallback must not credit commanded EV current the EV isn't
// actually drawing.  A plugged-in-but-full EV (commanded ~22.6 A but drawing
// ~0 W) used to make the rule believe ~5.2 kW was being absorbed, collapsing
// the computed excess to ~0 so solar was never curtailed — and the export cap
// was violated.  The measured-export backstop must curtail regardless.
func TestExportLimit_PhantomEVCredit_StillCurtails(t *testing.T) {
	o := NewDefaultOptimizer()
	expLim := &model.ActivePower{Value: 1000, Multiplier: 0}
	const (
		potential = 6000.0 // panel potential
		loadW     = 500.0  // site load
		nameW     = 8000.0 // inverter nameplate
	)
	// Closed-loop: feed the curtail ceiling back as next tick's generation, the
	// way the real inverter would, and check the controller converges to hold
	// the export cap.  Battery is full and the EV is plugged in but full
	// (drawing ~0 while commanded high) — so neither can absorb; curtailment is
	// the only remedy.  This would fail on the old ratchet (solar collapses to
	// 0) and on the phantom-EV bug (no curtailment at all).
	solarOut := potential
	var finalExport float64
	for i := 0; i < 10; i++ {
		export := solarOut - loadW
		st := SystemState{
			Solar: []SolarState{{Name: "pv", PowerW: solarOut, MaxW: nameW, Connected: true, Energized: true}},
			Batteries: []BatteryState{{
				Name: "bat", PowerW: 0, SOC: 100, MaxChargeW: 5000, MaxDischargeW: 5000,
				Connected: true, Energized: true,
			}},
			EVSEs: []EVSEState{{
				StationID: "evse-001", ConnectorID: 1, Connected: true, SessionActive: true,
				MaxCurrentA: 32, VoltageV: 240, PowerW: 7, SOC: 100,
			}},
			Grid: GridState{
				NetW: -export, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN(),
			},
			CSIPControl: &CSIPControlState{Source: "event", Base: model.DERControlBase{OpModExpLimW: expLim}},
		}
		plan := o.Optimize(st)
		ceiling := math.NaN()
		for _, sc := range plan.SolarCommands {
			if sc.Name == "pv" {
				ceiling = sc.CurtailToW
			}
		}
		if math.IsNaN(ceiling) {
			solarOut = potential // released → back to full potential
		} else {
			solarOut = math.Min(potential, ceiling) // device clamps to ceiling
		}
		finalExport = solarOut - loadW
	}
	if finalExport > 1000+complianceMarginW {
		t.Errorf("after convergence, export = %.0fW exceeds the 1000W cap (curtailment failed to hold)", finalExport)
	}
	if solarOut < 200 {
		t.Errorf("generation collapsed to %.0fW — over-curtailed (ratchet bug); cap only needs export ≤ 1000W", solarOut)
	}
}

// complianceMarginW mirrors the dashboard replay's ±150 W compliance tolerance.
const complianceMarginW = 150.0

// TestExportLimit_StickyCurtailment_NoRelease guards the oscillation fix: once
// solar is curtailed to hold the export cap, the rule must KEEP issuing the
// curtail command even when the meter momentarily reads compliant — otherwise
// the restore rule un-curtails, generation jumps back to full, and the cap is
// breached every other tick.  A phantom EV (commanded high, drawing nothing)
// must not trick the rule into releasing.
func TestExportLimit_StickyCurtailment_NoRelease(t *testing.T) {
	o := NewDefaultOptimizer()
	// Already mid-episode: solar curtailed to ~1300 W, meter sitting at the
	// conservative target (~800 W export), EV commanded high but drawing ~0.
	o.expGuard = exportGuard{
		evSetpointA:     22.6,
		evCmdW:          22.6 * 240,
		batteryAbsorbW:  math.NaN(),
		activeLimitW:    1000,
		filteredExportW: 800, // meter currently compliant thanks to curtailment
		solarCeilingW:   1300,
		safeCount:       10,
	}
	expLim := &model.ActivePower{Value: 1000, Multiplier: 0}
	st := SystemState{
		Solar: []SolarState{{Name: "pv", PowerW: 1300, MaxW: 8000, Connected: true, Energized: true}},
		Batteries: []BatteryState{{
			Name: "bat", PowerW: 0, SOC: 100, MaxChargeW: 5000, MaxDischargeW: 5000,
			Connected: true, Energized: true,
		}},
		EVSEs: []EVSEState{{
			StationID: "evse-001", ConnectorID: 1, Connected: true, SessionActive: true,
			MaxCurrentA: 32, VoltageV: 240, PowerW: 7, SOC: 100,
		}},
		Grid: GridState{
			NetW: -800, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN(),
		},
		CSIPControl: &CSIPControlState{Source: "event", Base: model.DERControlBase{OpModExpLimW: expLim}},
	}
	plan := o.Optimize(st)

	var curtailed bool
	for _, sc := range plan.SolarCommands {
		if sc.Name == "pv" && !math.IsNaN(sc.CurtailToW) && sc.CurtailToW < 7000 {
			curtailed = true
		}
	}
	if !curtailed {
		t.Error("curtailment was released while compliant only BECAUSE of curtailment — will oscillate and breach the cap")
	}
}

// TestImportLimit_RaisesPlanBatterySetpoint guards Bug F: when the cost plan has
// already set a soft battery discharge, the import-limit rule must be able to
// RAISE it to defend a CSIP import cap, rather than skipping the battery and
// leaving import over the limit.
func TestImportLimit_RaisesPlanBatterySetpoint(t *testing.T) {
	o := NewDefaultOptimizer()
	impLim := &model.ActivePower{Value: 1600, Multiplier: 0}
	st := SystemState{
		Batteries: []BatteryState{{
			Name: "bat", PowerW: 249, SOC: 66, MaxChargeW: 5000, MaxDischargeW: 5000,
			Connected: true, Energized: true,
		}},
		Grid: GridState{
			NetW: 2300, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN(),
		},
		CSIPControl:     &CSIPControlState{Source: "event", Base: model.DERControlBase{OpModImpLimW: impLim}},
		DailyPlanTarget: &PlanTarget{BattSetpointW: 249, EVMaxCurrentA: 0},
	}
	plan := o.Optimize(st)

	var setpoint float64 = math.NaN()
	for _, bc := range plan.BatteryCommands {
		if bc.Name == "bat" {
			setpoint = bc.SetpointW
		}
	}
	if math.IsNaN(setpoint) {
		t.Fatal("no battery command issued")
	}
	// Import 2300 W over a 1600 W cap with 66% SOC available: the battery must
	// discharge well above the plan's soft 249 W (target ≈ 1269 W to reach the
	// conservative limit).
	if setpoint < 1000 {
		t.Errorf("battery setpoint = %.0fW; want > 1000W to defend the 1600W import cap (plan was 249W)", setpoint)
	}
}

// TestExportLimit_NoReleaseWhenBatteryCreditCancelsExport guards the dominant
// export-cap failure in the 92-day replay: the controller must stay ENGAGED
// (sticky), never releasing the generation ceiling to free-running nameplate,
// even on a tick where this tick's battery-absorption credit makes the implied
// export momentarily land on the target so no real curtailment is computed.
//
// Setup mirrors the replay's failing midday: PV potential well above the 5 kW
// inverter nameplate and a tiny load, so free-running generation pins at
// nameplate and the site over-exports 4.7 kW.  The battery is idle this instant
// (measured ~0) but at 82% SOC has the headroom to be commanded ~3.1 kW of
// absorption.  Crediting that commanded-but-not-yet-delivered absorption drives
// the computed ceiling to exactly nameplate.  The OLD rule released the guard
// (solarCeilingW = NaN) here; with the meter lagging, the credit stayed inflated
// the next tick too, so it never re-curtailed and the inverter ran free at
// nameplate for the whole episode.  The fix must keep the guard engaged (clamped
// at nameplate as a no-op this tick) so the very next tick re-evaluates against
// fresh measured export and curtails the instant the credit illusion clears.
func TestExportLimit_NoReleaseWhenBatteryCreditCancelsExport(t *testing.T) {
	o := NewDefaultOptimizer()
	expLim := &model.ActivePower{Value: 2000, Multiplier: 0}
	st := SystemState{
		// Inverter at nameplate (potential is higher; the device clamps to 5 kW).
		Solar: []SolarState{{Name: "pv", PowerW: 5000, MaxW: 5000, Connected: true, Energized: true}},
		// Idle now (measured ~0) but 82% SOC → can be commanded ~3.1 kW absorption.
		Batteries: []BatteryState{{
			Name: "bat", PowerW: 0, SOC: 82, MaxChargeW: 5000, MaxDischargeW: 5000,
			Connected: true, Energized: true,
		}},
		// Exporting 4.7 kW against a 2 kW cap (tiny ~0.3 kW load).
		Grid: GridState{
			NetW: -4700, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN(),
		},
		CSIPControl: &CSIPControlState{Source: "event", Base: model.DERControlBase{OpModExpLimW: expLim}},
	}

	o.Optimize(st)

	// The guard must NOT be released: a NaN ceiling here is the bug — it drops the
	// inverter to free-running nameplate and (with the lagged credit) never
	// re-engages, sustaining the over-export for the rest of the episode.
	if math.IsNaN(o.expGuard.solarCeilingW) {
		t.Error("export guard released to NaN (free-running nameplate) on a battery-credit tick; " +
			"must stay engaged so it re-curtails next tick when the credit illusion clears")
	}
}

// TestExportLimit_FeedForwardSaturationCurtail guards the feed-forward fix for
// battery saturation. Mid-episode the ceiling has relaxed back toward nameplate
// while the battery absorbed; the instant the pack hits full SOC it stops
// absorbing and — under the bench meter's ~1 tick lag — the slew-limited
// feedback alone keeps generation high for a tick or more, over-exporting (the
// dominant remaining exportCap misses in the 92-day replay). The feed-forward
// term must lead this: drop the ceiling to the load + conservative-export target
// in ONE tick, bypassing the down-slew (a saturation-driven drop is
// deterministic, not meter-lag noise), yet floored at the conservative target so
// a stale-high export reading cannot crater it toward 0 and hunt.
func TestExportLimit_FeedForwardSaturationCurtail(t *testing.T) {
	o := NewDefaultOptimizer()
	// Mid-episode: already relaxed up to a 4 kW ceiling while the battery absorbed.
	o.expGuard = exportGuard{
		evSetpointA: math.NaN(), evCmdW: math.NaN(), batteryAbsorbW: math.NaN(),
		activeLimitW: 2000, filteredExportW: 5000, solarCeilingW: 4000,
	}
	expLim := &model.ActivePower{Value: 2000, Multiplier: 0}
	st := SystemState{
		Solar: []SolarState{{Name: "pv", PowerW: 4000, MaxW: 5000, Connected: true, Energized: true}},
		// Battery full — no absorption credit, so the ceiling carries the load.
		Batteries: []BatteryState{{
			Name: "bat", PowerW: 0, SOC: 100, MaxChargeW: 5000, MaxDischargeW: 5000,
			Connected: true, Energized: true,
		}},
		// Stale-high export reading (5 kW) — raw integrator wants a >1.5 kW drop.
		Grid: GridState{
			NetW: -5000, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN(),
		},
		CSIPControl: &CSIPControlState{Source: "event", Base: model.DERControlBase{OpModExpLimW: expLim}},
	}

	o.Optimize(st)

	// Battery full → no absorption, so the compliant ceiling is load + the
	// conservative-export target ≈ 0 + 1600 W. The feed-forward reaches it in one
	// tick, bypassing the 1.5 kW/tick slew that would otherwise hold it at 2500 W
	// — itself a 2500 W export over the 2000 W cap. Without the fix (gate
	// `predicted < commanded` = 0 < 0 for a full battery) the ceiling would sit at
	// the slew-limited 2500 W and over-export.
	if o.expGuard.solarCeilingW > 2000 {
		t.Errorf("ceiling = %.0fW exceeds the 2000W cap — feed-forward did not curtail a full battery's surplus (slew-limited stall)",
			o.expGuard.solarCeilingW)
	}
	// But it must NOT crater toward 0: the feed-forward is floored at the
	// conservative export target, so the stale-high 5 kW export reading cannot
	// collapse the ceiling and hunt.
	if o.expGuard.solarCeilingW < 1400 {
		t.Errorf("ceiling cratered to %.0fW — feed-forward must floor near the conservative target (~1600W), not collapse toward 0",
			o.expGuard.solarCeilingW)
	}
}

// TestImportLimit_BatteryFloored_ReportsBreach guards the CannotComply alert
// path: when an import cap is active and the battery is at its SOC reserve (no
// discharge headroom), the cap is physically unmeetable — the optimizer cannot
// offset the load — and it must flag plan.Breach so the hub reports the miss
// upstream as a 2030.5 CannotComply Response rather than failing silently.
func TestImportLimit_BatteryFloored_ReportsBreach(t *testing.T) {
	o := NewDefaultOptimizer()
	impLim := &model.ActivePower{Value: 1700, Multiplier: 0}
	st := SystemState{
		// Night: no solar, battery drained to the reserve floor (20%).
		Solar: []SolarState{{Name: "pv", PowerW: 0, MaxW: 8000, Connected: true, Energized: true}},
		Batteries: []BatteryState{{
			Name: "bat", PowerW: 0, SOC: 20, MaxChargeW: 5000, MaxDischargeW: 5000,
			Connected: true, Energized: true,
		}},
		Grid: GridState{
			NetW: 2500, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN(),
		},
		CSIPControl: &CSIPControlState{
			Source: "event", MRID: "EVT-IMP-1",
			Base: model.DERControlBase{OpModImpLimW: impLim},
		},
	}

	plan := o.Optimize(st)

	if plan.Breach == nil {
		t.Fatal("expected a compliance breach (import cap unmeetable with battery at reserve), got none")
	}
	if plan.Breach.LimitType != "import" {
		t.Errorf("breach LimitType = %q, want \"import\"", plan.Breach.LimitType)
	}
	if plan.Breach.MRID != "EVT-IMP-1" {
		t.Errorf("breach MRID = %q, want the active control's mRID", plan.Breach.MRID)
	}
	if plan.Breach.ShortfallW <= 0 {
		t.Errorf("breach ShortfallW = %.0f, want > 0 (import %.0f over limit %.0f)",
			plan.Breach.ShortfallW, plan.Breach.MeasuredW, plan.Breach.LimitW)
	}
}

// TestImportLimit_BatteryHasHeadroom_NoBreach is the negative case: with charge
// in the battery the rule discharges to hold the cap, so no breach is reported.
func TestImportLimit_BatteryHasHeadroom_NoBreach(t *testing.T) {
	o := NewDefaultOptimizer()
	impLim := &model.ActivePower{Value: 1700, Multiplier: 0}
	st := SystemState{
		Solar: []SolarState{{Name: "pv", PowerW: 0, MaxW: 8000, Connected: true, Energized: true}},
		Batteries: []BatteryState{{
			Name: "bat", PowerW: 0, SOC: 80, MaxChargeW: 5000, MaxDischargeW: 5000,
			Connected: true, Energized: true,
		}},
		Grid: GridState{
			NetW: 2500, ExportLimitW: math.NaN(), ImportLimitW: math.NaN(), MaxLimitW: math.NaN(),
		},
		CSIPControl: &CSIPControlState{
			Source: "event", MRID: "EVT-IMP-2",
			Base: model.DERControlBase{OpModImpLimW: impLim},
		},
	}

	plan := o.Optimize(st)

	if plan.Breach != nil {
		t.Errorf("unexpected breach: %+v (battery had discharge headroom to meet the cap)", plan.Breach)
	}
}
