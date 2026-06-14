package orchestrator

// Whitebox tests for individual optimizer rule functions.
// Integration-level tests (full Optimize path) live in optimizer_test.go.

import (
	"math"
	"testing"

	"lexa-hub/internal/northbound/model"
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

func TestExportLimitRule_NoActionWithinLimit(t *testing.T) {
	solar := []SolarState{ruleSol("pv", 1000)}
	bats := []BatteryState{ruleBat("bat", 0, 50, 5000)}
	limits := gridConstraints{exportLimitW: 5000, importLimitW: math.NaN(), maxLimitW: math.NaN()}
	plan := &Plan{}

	applyExportLimitRule(solar, nil, 0, limits, -1000, 95, 1000, bats, plan)

	if len(plan.BatteryCommands) != 0 || len(plan.SolarCommands) != 0 {
		t.Error("expected no commands when export is within limit")
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

func TestGenLimitRule_NoActionWithinCapOrNaN(t *testing.T) {
	plan := &Plan{}
	applyGenLimitRule([]SolarState{ruleSol("pv", 2000)}, 3000, plan)
	if len(plan.SolarCommands) != 0 {
		t.Errorf("generation within cap should not curtail, got %d", len(plan.SolarCommands))
	}
	applyGenLimitRule([]SolarState{ruleSol("pv", 9000)}, math.NaN(), plan)
	if len(plan.SolarCommands) != 0 {
		t.Errorf("NaN cap must be a no-op, got %d", len(plan.SolarCommands))
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

	applyRestoreRule(solar, nil, 20, plan)

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

	applyRestoreRule(solar, nil, 20, plan)

	if len(plan.SolarCommands) != 1 {
		t.Errorf("must not add second solar command, got %d", len(plan.SolarCommands))
	}
}

func TestRestoreRule_RestoresBattery(t *testing.T) {
	bats := []BatteryState{ruleBat("bat", 0, 50, 5000)}
	plan := &Plan{}

	applyRestoreRule(nil, bats, 20, plan)

	if len(plan.BatteryCommands) == 0 {
		t.Fatal("expected restore command for unconstrained battery")
	}
	// Battery restore idles at 0 W (clears any latched charge/discharge setpoint).
	if plan.BatteryCommands[0].SetpointW != 0 {
		t.Errorf("restore command should idle at 0W, got %.0f", plan.BatteryCommands[0].SetpointW)
	}
}

func TestRestoreRule_SkipsBatteryBelowSOCReserve(t *testing.T) {
	bats := []BatteryState{ruleBat("bat", 0, 15, 5000)} // SOC=15 <= reserve=20
	plan := &Plan{}

	applyRestoreRule(nil, bats, 20, plan)

	if len(plan.BatteryCommands) != 0 {
		t.Errorf("must not restore battery below SOC reserve, got %d commands", len(plan.BatteryCommands))
	}
}

func TestRestoreRule_SkipsBatteryAlreadyCommanded(t *testing.T) {
	bats := []BatteryState{ruleBat("bat", 0, 50, 5000)}
	plan := &Plan{BatteryCommands: []BatteryCommand{{Name: "bat", SetpointW: -2000}}}

	applyRestoreRule(nil, bats, 20, plan)

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
