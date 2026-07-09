package orchestrator_test

import (
	"math"
	"testing"
	"time"

	"lexa-hub/internal/orchestrator"
	model "lexa-proto/csipmodel"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func boolPtr(b bool) *bool { return &b }

func ap(w int16) *model.ActivePower { return &model.ActivePower{Value: w, Multiplier: 0} }

func newOpt() *orchestrator.DefaultOptimizer {
	return orchestrator.NewDefaultOptimizer()
}

// state0 returns a minimal system state with no devices and no CSIP signal.
func state0() orchestrator.SystemState {
	return orchestrator.SystemState{
		Timestamp: time.Now(),
		Grid:      orchestrator.NewGridState(),
	}
}

// battery returns a BatteryState for use in tests.
func battery(name string, powerW, soc, maxW float64) orchestrator.BatteryState {
	b := orchestrator.NewBatteryState(name)
	b.PowerW = powerW
	b.SOC = soc
	b.MaxChargeW = maxW
	b.MaxDischargeW = maxW
	b.Connected = true
	b.Energized = true
	return b
}

func solar(name string, powerW, maxW float64) orchestrator.SolarState {
	return orchestrator.SolarState{
		Name: name, PowerW: powerW, MaxW: maxW, Connected: true, Energized: true,
	}
}

func evse(stationID string, sessionActive bool, currentA, maxA, voltageV float64) orchestrator.EVSEState {
	powerW := 0.0
	if sessionActive {
		powerW = currentA * voltageV
	}
	return orchestrator.EVSEState{
		StationID:     stationID,
		ConnectorID:   1,
		Connected:     true,
		SessionActive: sessionActive,
		CurrentA:      currentA,
		MaxCurrentA:   maxA,
		VoltageV:      voltageV,
		PowerW:        powerW,
		Status:        "Occupied",
	}
}

// ── No devices ────────────────────────────────────────────────────────────────

func TestOptimizer_NoDevices_EmptyPlan(t *testing.T) {
	opt := newOpt()
	plan := opt.Optimize(state0())
	if len(plan.BatteryCommands) != 0 || len(plan.SolarCommands) != 0 || len(plan.EVSECommands) != 0 {
		t.Error("expected empty plan with no devices")
	}
}

// ── CSIP disconnect ───────────────────────────────────────────────────────────

func TestOptimizer_CSIPDisconnect_DisconnectsBatteries(t *testing.T) {
	opt := newOpt()
	s := state0()
	s.Batteries = []orchestrator.BatteryState{
		battery("bat-0", 0, 50, 5000),
		battery("bat-1", 0, 80, 5000),
	}
	s.CSIPControl = &orchestrator.CSIPControlState{
		Base: model.DERControlBase{OpModConnect: boolPtr(false)},
	}

	plan := opt.Optimize(s)

	if len(plan.BatteryCommands) != 2 {
		t.Fatalf("expected 2 disconnect commands, got %d", len(plan.BatteryCommands))
	}
	for _, cmd := range plan.BatteryCommands {
		if cmd.Connect == nil || *cmd.Connect {
			t.Errorf("battery %s: expected Connect=false", cmd.Name)
		}
	}
	// Disconnect is the only action — no solar or EVSE commands.
	if len(plan.SolarCommands) != 0 {
		t.Error("unexpected solar commands after disconnect")
	}
}

// ── CSIP export limit ─────────────────────────────────────────────────────────

func TestOptimizer_CSIPExportLimit_ChargesBattery(t *testing.T) {
	opt := newOpt()
	s := state0()

	// Solar generating 8 kW, export limit is 2 kW → 6 kW must be absorbed.
	s.Solar = []orchestrator.SolarState{solar("pv-0", 8000, 10000)}
	s.Batteries = []orchestrator.BatteryState{battery("bat-0", 0, 50, 7000)}
	s.Grid.NetW = -8000 // 8 kW export (negative = export)
	s.CSIPControl = &orchestrator.CSIPControlState{
		Base: model.DERControlBase{OpModExpLimW: ap(2000)},
	}

	plan := opt.Optimize(s)

	// Expect the battery to be commanded to charge.
	if len(plan.BatteryCommands) == 0 {
		t.Fatal("expected battery charge command to absorb excess export")
	}
	cmd := plan.BatteryCommands[0]
	if cmd.SetpointW >= 0 {
		t.Errorf("battery setpoint = %.0f; expected negative (charging)", cmd.SetpointW)
	}
	// Absorbed at least 6 kW.
	if cmd.SetpointW > -5500 {
		t.Errorf("battery setpoint = %.0f; expected ≤ -5500 to absorb 6 kW excess", cmd.SetpointW)
	}
}

func TestOptimizer_CSIPExportLimit_CurtailsSolar_WhenBatteryFull(t *testing.T) {
	opt := newOpt()
	s := state0()

	// Solar 8 kW, battery full (SOC=96%), export limit 2 kW → curtail solar.
	s.Solar = []orchestrator.SolarState{solar("pv-0", 8000, 10000)}
	b := battery("bat-0", 0, 96, 5000) // battery "full" per SOCFullThreshold=95
	s.Batteries = []orchestrator.BatteryState{b}
	s.Grid.NetW = -8000
	s.CSIPControl = &orchestrator.CSIPControlState{
		Base: model.DERControlBase{OpModExpLimW: ap(2000)},
	}

	plan := opt.Optimize(s)

	// Expect solar curtailment.
	if len(plan.SolarCommands) == 0 {
		t.Fatal("expected solar curtailment when battery is full and export limit violated")
	}
	sc := plan.SolarCommands[0]
	if sc.CurtailToW > 2500 {
		t.Errorf("solar curtailed to %.0f W; expected ≤ 2500 W", sc.CurtailToW)
	}
}

// ── Self-consumption ──────────────────────────────────────────────────────────

func TestOptimizer_ExcessSolar_ChargesBattery(t *testing.T) {
	opt := newOpt()
	s := state0()

	// Solar 5 kW, no load, no CSIP. Battery has headroom.
	s.Solar = []orchestrator.SolarState{solar("pv-0", 5000, 10000)}
	s.Batteries = []orchestrator.BatteryState{battery("bat-0", 0, 40, 5000)}

	plan := opt.Optimize(s)

	if len(plan.BatteryCommands) == 0 {
		t.Fatal("expected battery charge command for excess solar self-consumption")
	}
	cmd := plan.BatteryCommands[0]
	if cmd.SetpointW >= 0 {
		t.Errorf("battery setpoint = %.0f; expected negative (charging)", cmd.SetpointW)
	}
	logDecisions(t, plan)
}

func TestOptimizer_ExcessSolar_SkipCharge_WhenBatteryFull(t *testing.T) {
	opt := newOpt()
	s := state0()
	s.Solar = []orchestrator.SolarState{solar("pv-0", 5000, 10000)}
	b := battery("bat-0", 0, 96, 5000) // over SOCFullThreshold
	s.Batteries = []orchestrator.BatteryState{b}

	plan := opt.Optimize(s)

	for _, cmd := range plan.BatteryCommands {
		if cmd.SetpointW < 0 {
			t.Errorf("battery charged when full: setpoint=%.0f", cmd.SetpointW)
		}
	}
}

// ── Fixed dispatch (OpModFixedW) ──────────────────────────────────────────────

func TestOptimizer_FixedDispatch_DischargesBattery(t *testing.T) {
	opt := newOpt()
	s := state0()

	// Grid requests 3 kW export (OpModFixedW). No solar → battery must cover it.
	s.Batteries = []orchestrator.BatteryState{battery("bat-0", 0, 80, 5000)}
	s.CSIPControl = &orchestrator.CSIPControlState{
		Base: model.DERControlBase{OpModFixedW: ap(3000)},
	}

	plan := opt.Optimize(s)

	found := false
	for _, cmd := range plan.BatteryCommands {
		if cmd.SetpointW > 0 {
			found = true
		}
	}
	if !found {
		t.Error("expected battery discharge command for fixed dispatch")
	}
	logDecisions(t, plan)
}

func TestOptimizer_FixedDispatch_RespectsSOCReserve(t *testing.T) {
	opt := newOpt()
	s := state0()

	// Battery at 15% — below SOCReserve=20%; should NOT discharge even for dispatch.
	s.Batteries = []orchestrator.BatteryState{battery("bat-0", 0, 15, 5000)}
	s.CSIPControl = &orchestrator.CSIPControlState{
		Base: model.DERControlBase{OpModFixedW: ap(3000)},
	}

	plan := opt.Optimize(s)

	for _, cmd := range plan.BatteryCommands {
		if cmd.SetpointW > 0 {
			t.Errorf("battery discharged below SOC reserve: setpoint=%.0f SOC=15%%", cmd.SetpointW)
		}
	}
}

// ── EV charging ───────────────────────────────────────────────────────────────

func TestOptimizer_EV_FullRate_WhenSolarAmple(t *testing.T) {
	opt := newOpt()
	s := state0()

	// 10 kW solar, EVSE at 16A / 230V = 3.68 kW → plenty of surplus.
	s.Solar = []orchestrator.SolarState{solar("pv-0", 10000, 10000)}
	s.EVSEs = []orchestrator.EVSEState{evse("cs-001", true, 0, 16.0, 230.0)}

	plan := opt.Optimize(s)

	if len(plan.EVSECommands) == 0 {
		t.Fatal("expected EVSE command")
	}
	cmd := plan.EVSECommands[0]
	if cmd.MaxCurrentA != 16.0 {
		t.Errorf("EVSE MaxCurrentA = %.1f, want 16.0", cmd.MaxCurrentA)
	}
}

func TestOptimizer_EV_ThrottledWhenUnconstrainedAndLowSolar(t *testing.T) {
	opt := newOpt()
	s := state0()

	// Self-consumption priority: even with no grid constraint, the EV must not
	// be driven past available solar surplus or we'd be importing from the
	// grid to charge the car.  Solar 1 kW + EV at 32A=7.36 kW would import
	// ~6 kW; throttle instead.
	s.Solar = []orchestrator.SolarState{solar("pv-0", 1000, 10000)}
	s.EVSEs = []orchestrator.EVSEState{evse("cs-001", true, 0, 32.0, 230.0)}

	plan := opt.Optimize(s)

	if len(plan.EVSECommands) == 0 {
		t.Fatal("expected EVSE command")
	}
	if plan.EVSECommands[0].MaxCurrentA >= 32.0 {
		t.Errorf("EVSE MaxCurrentA = %.1f, want throttled below 32A when solar < EV max",
			plan.EVSECommands[0].MaxCurrentA)
	}
}

func TestOptimizer_EV_FullRateWhenUnconstrainedAndSolarAmple(t *testing.T) {
	opt := newOpt()
	s := state0()

	// Solar comfortably covers full EV draw — no need to throttle.
	s.Solar = []orchestrator.SolarState{solar("pv-0", 10000, 10000)}
	s.EVSEs = []orchestrator.EVSEState{evse("cs-001", true, 0, 16.0, 230.0)}

	plan := opt.Optimize(s)

	if len(plan.EVSECommands) == 0 {
		t.Fatal("expected EVSE command")
	}
	if plan.EVSECommands[0].MaxCurrentA != 16.0 {
		t.Errorf("EVSE MaxCurrentA = %.1f, want 16A (full) when solar amply covers EV draw",
			plan.EVSECommands[0].MaxCurrentA)
	}
}

func TestOptimizer_EV_Throttled_WhenExportLimited(t *testing.T) {
	opt := newOpt()
	s := state0()

	// Export limit active with only 1 kW solar surplus — EV should be throttled/suspended.
	s.Solar = []orchestrator.SolarState{solar("pv-0", 1000, 10000)}
	s.EVSEs = []orchestrator.EVSEState{evse("cs-001", true, 0, 32.0, 230.0)}
	s.Grid.ExportLimitW = 500

	plan := opt.Optimize(s)

	// Export-limit rule handles the EVSE command; it should not command full rate.
	evCmd := plan.EVSECommands
	if len(evCmd) == 0 {
		t.Fatal("expected EVSE command")
	}
	if evCmd[0].MaxCurrentA >= 32.0 {
		t.Errorf("EVSE MaxCurrentA = %.1f, want < 32A when export-limited with low solar", evCmd[0].MaxCurrentA)
	}
}

func TestOptimizer_EV_Suspended_WhenImportLimitReached(t *testing.T) {
	opt := newOpt()
	s := state0()
	s.Grid.ImportLimitW = 3000
	s.Grid.NetW = 3500 // already over limit
	s.EVSEs = []orchestrator.EVSEState{evse("cs-001", true, 10, 16.0, 230.0)}

	plan := opt.Optimize(s)

	if len(plan.EVSECommands) == 0 {
		t.Fatal("expected EVSE command")
	}
	if plan.EVSECommands[0].MaxCurrentA != 0 {
		t.Errorf("EVSE should be suspended when grid import limit exceeded, got %.1f A",
			plan.EVSECommands[0].MaxCurrentA)
	}
}

func TestOptimizer_EV_NoSession_NoCommand(t *testing.T) {
	opt := newOpt()
	s := state0()
	// EVSE connected but no active session.
	s.EVSEs = []orchestrator.EVSEState{evse("cs-001", false, 0, 32.0, 230.0)}

	plan := opt.Optimize(s)

	// No active session → no EVSE command.
	if len(plan.EVSECommands) != 0 {
		t.Errorf("expected no EVSE commands with no active session, got %d", len(plan.EVSECommands))
	}
}

// ── Combined scenario: peak demand event ─────────────────────────────────────

func TestScenario_PeakDemandEvent(t *testing.T) {
	// Setup: CSIP sends 5 kW export limit.  Solar = 8 kW.  Battery at 70%.
	// Expected: battery absorbs 3 kW excess solar.
	opt := newOpt()
	s := state0()
	s.Solar = []orchestrator.SolarState{solar("pv-0", 8000, 10000)}
	s.Batteries = []orchestrator.BatteryState{battery("bat-0", 0, 70, 5000)}
	s.Grid.NetW = -8000 // exporting 8 kW
	s.CSIPControl = &orchestrator.CSIPControlState{
		Base: model.DERControlBase{OpModExpLimW: ap(5000)},
	}

	plan := opt.Optimize(s)

	// Expect battery to charge.
	if len(plan.BatteryCommands) == 0 {
		t.Fatal("expected battery command in peak demand scenario")
	}
	bc := plan.BatteryCommands[0]
	if bc.SetpointW >= 0 {
		t.Errorf("expected battery to charge (negative setpoint), got %.0f", bc.SetpointW)
	}
	logDecisions(t, plan)
}

// ── Combined scenario: excess solar + EV charging ────────────────────────────

func TestScenario_ExcessSolarWithEV(t *testing.T) {
	// Solar 7 kW, battery at 50%, EVSE active at 16A/230V ≈ 3.7 kW.
	// Expected: battery gets some charge, EVSE gets throttled or full rate.
	opt := newOpt()
	s := state0()
	s.Solar = []orchestrator.SolarState{solar("pv-0", 7000, 10000)}
	s.Batteries = []orchestrator.BatteryState{battery("bat-0", 0, 50, 5000)}
	s.EVSEs = []orchestrator.EVSEState{evse("cs-001", true, 0, 16.0, 230.0)}

	plan := opt.Optimize(s)

	// Battery should charge.
	hasBatteryCharge := false
	for _, cmd := range plan.BatteryCommands {
		if cmd.SetpointW < 0 {
			hasBatteryCharge = true
		}
	}
	if !hasBatteryCharge {
		t.Error("expected battery to charge with 7 kW solar excess")
	}

	// EVSE should get a command.
	if len(plan.EVSECommands) == 0 {
		t.Error("expected EVSE command")
	}
	logDecisions(t, plan)
}

// ── TOU cost model integration ────────────────────────────────────────────────

func TestOptimizer_TOU_PeakHour_DischargeBattery(t *testing.T) {
	opt := orchestrator.NewDefaultOptimizer()
	opt.CostModel = orchestrator.DefaultTOUCostModel()

	s := state0()
	// Force 5 pm — within the 16:00–21:00 peak window in DefaultTOUCostModel.
	s.Timestamp = time.Date(2025, 1, 15, 17, 0, 0, 0, time.Local)
	s.Batteries = []orchestrator.BatteryState{battery("bat-0", 0, 80, 5000)}

	plan := opt.Optimize(s)

	found := false
	for _, cmd := range plan.BatteryCommands {
		if cmd.SetpointW > 0 {
			found = true
		}
	}
	if !found {
		t.Error("expected battery discharge during TOU peak hour")
	}
}

// TestOptimizer_TOU_PeakHour_UsesServerTimeViaClockOffset is the AD-004/
// TASK-036 regression for optimizer.go's Rule 5: peak detection must key off
// SERVER time (state.Timestamp + state.ClockOffset, via utilitytime.ServerNowAt)
// rather than the local Timestamp alone. state.Timestamp is set to 3 AM local
// (well outside DefaultTOUCostModel's 16:00-21:00 peak window), but a +14h
// ClockOffset — as a hub whose local clock is 14 hours behind the CSIP server
// would report via bus.ActiveControl.ClockOffset — pushes server time to 5 PM,
// squarely inside the peak window. If Rule 5 ever regressed to reading local
// time only, this discharge would not fire.
func TestOptimizer_TOU_PeakHour_UsesServerTimeViaClockOffset(t *testing.T) {
	opt := orchestrator.NewDefaultOptimizer()
	opt.CostModel = orchestrator.DefaultTOUCostModel()

	s := state0()
	s.Timestamp = time.Date(2025, 1, 15, 3, 0, 0, 0, time.Local) // 3 AM local: off-peak
	s.ClockOffset = 14 * 3600                                    // server time = 3am+14h = 5pm: peak
	s.Batteries = []orchestrator.BatteryState{battery("bat-0", 0, 80, 5000)}

	plan := opt.Optimize(s)

	found := false
	for _, cmd := range plan.BatteryCommands {
		if cmd.SetpointW > 0 {
			found = true
		}
	}
	if !found {
		t.Error("expected battery discharge: server time (local+ClockOffset) is within the TOU peak window even though local time is not")
	}
}

// ── Decision trace ────────────────────────────────────────────────────────────

func TestOptimizer_DecisionsAreRecorded(t *testing.T) {
	opt := newOpt()
	s := state0()
	s.Solar = []orchestrator.SolarState{solar("pv-0", 5000, 10000)}
	s.Batteries = []orchestrator.BatteryState{battery("bat-0", 0, 40, 5000)}

	plan := opt.Optimize(s)

	if len(plan.Decisions) == 0 {
		t.Error("expected at least one decision in trace when action is taken")
	}
	for _, d := range plan.Decisions {
		if d.Rule == "" {
			t.Error("decision has empty Rule")
		}
		if d.Reason == "" {
			t.Error("decision has empty Reason")
		}
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func logDecisions(t *testing.T, plan orchestrator.Plan) {
	t.Helper()
	for _, d := range plan.Decisions {
		t.Logf("  [%s] %s → %s", d.Rule, d.Reason, d.Impact)
	}
}

// ── Surplus calculation with non-zero home load ───────────────────────────────
//
// This test catches the historical sign-convention bug where surplusW was
// computed as solarW - (evseW - batteryW + Grid.NetW), which gave double the
// correct value when the grid was exporting.  The correct formula is:
//   homeLoadW = solarW + max(0,batteryW) + Grid.NetW - evseW
//   surplusW  = solarW - homeLoadW

func TestOptimizer_SurplusRespectHomeLoad(t *testing.T) {
	opt := newOpt()
	s := state0()

	// 5 kW solar, 2 kW home load (implied: grid exports 3 kW → NetW = -3000).
	// Battery has 5 kW headroom.  Surplus = 3 kW, so battery should charge ≤ 3 kW.
	s.Solar = []orchestrator.SolarState{solar("pv-0", 5000, 10000)}
	s.Batteries = []orchestrator.BatteryState{battery("bat-0", 0, 50, 5000)}
	s.Grid.NetW = -3000 // 3 kW export → 2 kW goes to home loads

	plan := opt.Optimize(s)

	if len(plan.BatteryCommands) == 0 {
		t.Fatal("expected battery charge command")
	}
	cmd := plan.BatteryCommands[0]
	if cmd.SetpointW >= 0 {
		t.Errorf("battery setpoint = %.0f; expected negative (charging)", cmd.SetpointW)
	}
	// Old (buggy) formula gave surplusW = 8000, charging up to 5 kW.
	// Correct formula gives surplusW = 3000, so setpoint must be ≥ -3100 (allow rounding).
	if cmd.SetpointW < -3100 {
		t.Errorf("battery setpoint = %.0f exceeds available 3 kW surplus — sign-convention bug?", cmd.SetpointW)
	}
	logDecisions(t, plan)
}

// ── BatteryState helpers ──────────────────────────────────────────────────────

func TestBatteryState_AvailableChargeW(t *testing.T) {
	b := battery("bat", -2000, 50, 5000) // charging at 2kW, max 5kW
	// headroom = MaxChargeW + PowerW = 5000 + (-2000) = 3000
	if got := b.AvailableChargeW(); math.Abs(got-3000) > 1 {
		t.Errorf("AvailableChargeW = %.0f, want 3000", got)
	}
}

func TestBatteryState_AvailableChargeW_WhenDischarging(t *testing.T) {
	// Battery discharging at 3kW: full swing to max charge = 5000+3000 = 8000W.
	// This is the cross-zero case — the battery can swing from +3kW to −5kW.
	b := battery("bat", 3000, 50, 5000)
	if got := b.AvailableChargeW(); math.Abs(got-8000) > 1 {
		t.Errorf("AvailableChargeW when discharging = %.0f, want 8000", got)
	}
}

func TestBatteryState_AvailableDischargeW(t *testing.T) {
	b := battery("bat", 1000, 50, 5000) // discharging at 1kW, max 5kW
	// headroom = MaxDischargeW − PowerW = 5000 − 1000 = 4000
	if got := b.AvailableDischargeW(); math.Abs(got-4000) > 1 {
		t.Errorf("AvailableDischargeW = %.0f, want 4000", got)
	}
}

func TestBatteryState_AvailableDischargeW_WhenCharging(t *testing.T) {
	// Battery charging at 3kW: full swing to max discharge = 5000−(−3000) = 8000W.
	b := battery("bat", -3000, 50, 5000)
	if got := b.AvailableDischargeW(); math.Abs(got-8000) > 1 {
		t.Errorf("AvailableDischargeW when charging = %.0f, want 8000", got)
	}
}

func TestBatteryState_Disconnected_ZeroHeadroom(t *testing.T) {
	b := battery("bat", 0, 50, 5000)
	b.Connected = false
	if got := b.AvailableChargeW(); got != 0 {
		t.Errorf("AvailableChargeW disconnected = %.0f, want 0", got)
	}
	if got := b.AvailableDischargeW(); got != 0 {
		t.Errorf("AvailableDischargeW disconnected = %.0f, want 0", got)
	}
}

// TestOptimizer_ExportLimit_SwitchesBatteryFromDischargeToCharge verifies that
// when the battery is discharging and an export limit is applied, the optimizer
// commands immediate charging in a single tick rather than only reducing discharge.
//
// Scenario: battery +3kW, solar 5kW, load 2kW → 6kW export. Limit = 0W.
// Required setpoint: 3000 − 6000 = −3000W.  Old (buggy) headroom capped at
// MaxChargeW=5kW, absorbing only 5kW → setpoint −2000W, still 1kW over limit.
func TestOptimizer_ExportLimit_SwitchesBatteryFromDischargeToCharge(t *testing.T) {
	opt := newOpt()
	s := state0()
	s.Solar = []orchestrator.SolarState{solar("pv-0", 5000, 10000)}
	s.Batteries = []orchestrator.BatteryState{battery("bat-0", 3000, 50, 5000)} // discharging
	s.Grid.NetW = -6000                                                         // 6kW export
	s.CSIPControl = &orchestrator.CSIPControlState{
		Base: model.DERControlBase{OpModExpLimW: ap(0)},
	}

	plan := opt.Optimize(s)

	if len(plan.BatteryCommands) == 0 {
		t.Fatal("expected battery command")
	}
	cmd := plan.BatteryCommands[0]
	// Must absorb the full 6kW excess: 3000 − 6000 = −3000W.
	if cmd.SetpointW > -2500 {
		t.Errorf("setpoint = %.0fW; expected ≤ −2500W to absorb the 6kW excess in one tick (was discharging at 3kW)", cmd.SetpointW)
	}
	logDecisions(t, plan)
}

// ── Document scenarios ────────────────────────────────────────────────────────

// Case 1: export limit 1kW, solar 2kW, home 1kW, battery full, EV needs charge.
// Expected: EV charges using solar surplus; no grid export above limit.
func TestScenario_Case1_EVChargesWithSolarSurplus(t *testing.T) {
	opt := newOpt()
	s := state0()
	s.Solar = []orchestrator.SolarState{solar("pv-0", 2000, 3000)}
	b := battery("bat-0", 0, 96, 5000) // full (SOC above threshold)
	s.Batteries = []orchestrator.BatteryState{b}
	s.Grid.NetW = -1000 // exporting 1kW, exactly at limit
	s.CSIPControl = &orchestrator.CSIPControlState{
		Base: model.DERControlBase{OpModExpLimW: ap(1000)},
	}
	s.EVSEs = []orchestrator.EVSEState{evse("cs-001", true, 0, 32.0, 230.0)}

	plan := opt.Optimize(s)

	// EV should receive a charge command at ≥ 6A (minimum) using solar + grid supplement.
	if len(plan.EVSECommands) == 0 {
		t.Fatal("expected EVSE command to charge EV with solar surplus")
	}
	cmd := plan.EVSECommands[0]
	if cmd.MaxCurrentA < 6.0 {
		t.Errorf("EV should charge at ≥6A minimum, got %.1fA", cmd.MaxCurrentA)
	}
	// Battery should not charge (already full).
	for _, bc := range plan.BatteryCommands {
		if bc.SetpointW < 0 {
			t.Errorf("battery should not charge when full (SOC=96%%)")
		}
	}
	logDecisions(t, plan)
}

// Case 2: grid requests 10kW (OpModFixedW), solar 10kW, home 1kW, battery full.
// Expected: solar provides 9kW; battery discharges 1kW to cover shortfall.
func TestScenario_Case2_FixedDispatch_BatteryCoversShortfall(t *testing.T) {
	opt := newOpt()
	s := state0()
	s.Solar = []orchestrator.SolarState{solar("pv-0", 10000, 10000)}
	b := battery("bat-0", 0, 100, 5000) // full, idle
	s.Batteries = []orchestrator.BatteryState{b}
	s.Grid.NetW = -9000 // exporting 9kW (solar minus home load)
	s.CSIPControl = &orchestrator.CSIPControlState{
		Base: model.DERControlBase{OpModFixedW: ap(10000)},
	}

	plan := opt.Optimize(s)

	// Battery must discharge to cover the 1kW shortfall.
	found := false
	for _, cmd := range plan.BatteryCommands {
		if cmd.SetpointW > 0 {
			found = true
			if cmd.SetpointW < 500 || cmd.SetpointW > 2000 {
				t.Errorf("battery setpoint = %.0fW; expected ~1000W (1kW shortfall)", cmd.SetpointW)
			}
		}
	}
	if !found {
		t.Fatal("expected battery discharge to cover grid dispatch shortfall")
	}
	logDecisions(t, plan)
}

// Case 3: export limit 0W, solar 2kW, home 1kW, battery 50%, EV full.
// Expected: battery absorbs the 1kW surplus; solar not curtailed.
// When battery is full: solar gets curtailed instead.
func TestScenario_Case3_ExportZero_BatteryAbsorbsSurplus(t *testing.T) {
	opt := newOpt()
	s := state0()
	s.Solar = []orchestrator.SolarState{solar("pv-0", 2000, 3000)}
	b := battery("bat-0", 0, 50, 5000)
	s.Batteries = []orchestrator.BatteryState{b}
	s.Grid.NetW = -1000 // exporting 1kW (= excess over export limit 0)
	s.CSIPControl = &orchestrator.CSIPControlState{
		Base: model.DERControlBase{OpModExpLimW: ap(0)},
	}

	plan := opt.Optimize(s)

	// Battery should charge to absorb the 1kW surplus.
	found := false
	for _, cmd := range plan.BatteryCommands {
		if cmd.SetpointW < 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("expected battery to absorb 1kW surplus when export=0W")
	}
	// Solar should not be curtailed (battery has headroom).
	for _, sc := range plan.SolarCommands {
		if !math.IsNaN(sc.CurtailToW) && sc.CurtailToW < 1900 {
			t.Errorf("solar curtailed to %.0fW; battery has headroom, should not curtail", sc.CurtailToW)
		}
	}
	logDecisions(t, plan)
}

// Regression for the demo S2 oscillation: solar 1 kW, home load 2 kW,
// CSIP import cap 500 W.  Pre-fix, the stateless import-limit rule fired
// only when import strictly exceeded the limit, then applyRestoreRule
// idled the battery on the next tick, the import jumped back to 1 kW,
// and the system chattered at the tick period (battery 0→500→0,
// grid 1000→500→1000).  The fixed rule must hold the discharge across
// multiple ticks without an external prompt and settle at a steady
// operating point inside the limit.
func TestScenario_S2_ImportLimit_NoOscillation(t *testing.T) {
	opt := newOpt()
	s := state0()
	s.Solar = []orchestrator.SolarState{solar("pv-0", 1000, 3000)}
	s.Batteries = []orchestrator.BatteryState{battery("bat-0", 0, 75, 5000)}
	s.Grid.NetW = 1000 // importing 1 kW
	s.CSIPControl = &orchestrator.CSIPControlState{
		Base: model.DERControlBase{OpModImpLimW: ap(500)},
	}

	// Tick 1 — should command the battery to discharge.
	plan1 := opt.Optimize(s)
	dis1 := 0.0
	for _, bc := range plan1.BatteryCommands {
		if bc.SetpointW > 0 {
			dis1 = bc.SetpointW
		}
	}
	if dis1 < 400 {
		t.Fatalf("tick 1: battery discharge = %.0fW, want ≥400W to pull import below 500W limit", dis1)
	}

	// Tick 2 — meter now reflects the battery's contribution: import has
	// dropped to ~500W (right at the hard limit).  The pre-fix rule would
	// see "at limit, do nothing", let restore idle the battery, and the
	// import would spike back to 1 kW.  The new rule must hold.
	s.Batteries[0].PowerW = dis1
	s.Grid.NetW = math.Max(0, 1000-dis1) // unconstrained 1 kW import minus battery's contribution
	plan2 := opt.Optimize(s)
	dis2 := 0.0
	idledByRestore := false
	for _, bc := range plan2.BatteryCommands {
		if bc.SetpointW > 0 {
			dis2 = bc.SetpointW
		}
		if bc.SetpointW == 0 && bc.Connect != nil && *bc.Connect {
			idledByRestore = true
		}
	}
	if idledByRestore {
		t.Fatalf("tick 2: applyRestoreRule idled the battery while import limit is active — would cause oscillation")
	}
	if dis2 < 400 {
		t.Errorf("tick 2: battery discharge = %.0fW, want held near tick 1 value %.0fW to avoid oscillation", dis2, dis1)
	}

	// Tick 3 — another stable read from the meter at the limit.  Must still hold.
	s.Batteries[0].PowerW = dis2
	s.Grid.NetW = math.Max(0, 1000-dis2)
	plan3 := opt.Optimize(s)
	dis3 := 0.0
	for _, bc := range plan3.BatteryCommands {
		if bc.SetpointW > 0 {
			dis3 = bc.SetpointW
		}
	}
	if dis3 < 400 {
		t.Errorf("tick 3: battery discharge = %.0fW, want held to defend import limit", dis3)
	}
	logDecisions(t, plan3)
}

// Regression for import-limit oscillation when Modbus readings are stale.
// The engine may tick faster than the poll interval (e.g. 5 s engine vs 10 s
// poll).  If the prior *commanded* discharge is substituted into the
// conservation identity, each stale tick adds it again to importW, driving
// the target ever higher → massive over-discharge → ramp-down → guard reset →
// battery idles → import spikes → repeat.  Using *measured* discharge breaks
// the compounding: stale battery=0 means unconstrained = importW + 0, so the
// target is stable and the guard's hold logic keeps the prior command in place.
func TestScenario_S2_StaleReadings_NoOscillation(t *testing.T) {
	opt := newOpt()
	s := state0()
	s.Solar = []orchestrator.SolarState{solar("pv-0", 1000, 3000)}
	s.Batteries = []orchestrator.BatteryState{battery("bat-0", 0, 75, 5000)}
	s.Grid.NetW = 1000 // importing 1 kW with battery idle
	s.CSIPControl = &orchestrator.CSIPControlState{
		Base: model.DERControlBase{OpModImpLimW: ap(500)},
	}

	// Tick 1 — initial, battery PowerW == 0 (never discharged yet).
	plan1 := opt.Optimize(s)
	dis1 := 0.0
	for _, bc := range plan1.BatteryCommands {
		if bc.SetpointW > 0 {
			dis1 = bc.SetpointW
		}
	}
	if dis1 < 200 {
		t.Fatalf("tick 1: battery discharge = %.0fW, want ≥200W", dis1)
	}

	// Ticks 2 and 3: Modbus readings are STALE — battery PowerW is still 0
	// and grid is still 1000 W (Modbus hasn't polled yet).  The pre-fix code
	// would compound prior commanded discharge into the target, causing runaway.
	for tick := 2; tick <= 3; tick++ {
		// Do NOT update s.Batteries[0].PowerW or s.Grid.NetW — simulate stale reads.
		plan := opt.Optimize(s)
		var dis float64
		for _, bc := range plan.BatteryCommands {
			if bc.SetpointW > dis {
				dis = bc.SetpointW
			}
		}
		// With the fix, target stays at dis1 and the hold logic keeps the command
		// near dis1.  With the bug, it would compound to 2×, 3×, … of dis1.
		if dis > dis1*1.5 {
			t.Errorf("tick %d: battery discharge = %.0fW (%.1f× tick-1 %.0fW) — compounding detected",
				tick, dis, dis/dis1, dis1)
		}
	}
}

// Regression for the demo S1 *discovery-gap* overshoot: the dashboard starts
// the EV session and publishes the export-limit event at the same instant,
// but the hub doesn't fetch the new event until its next discovery cycle
// (~15-20 s later).  In the gap the orchestrator sees an active EV session
// with no grid constraint and used to slam the EV to MaxCurrentA, dragging
// the site into a multi-second 3 kW grid import.  Verify the unconstrained
// branch now throttles to the post-battery solar surplus so we never *create*
// a new import while waiting for the constraint to arrive.
func TestScenario_S1_DiscoveryGap_NoImportFromUnconstrainedEV(t *testing.T) {
	opt := newOpt()
	s := state0()
	// Solar 8 kW, battery 40 % SOC (5 kW max), 1 kW home load, EV session just
	// started.  No CSIP control yet (mirrors the gap between the EV session
	// command and the next discovery walk fetching the export limit).
	s.Solar = []orchestrator.SolarState{solar("pv-0", 8000, 10000)}
	s.Batteries = []orchestrator.BatteryState{battery("bat-0", 0, 40, 5000)}
	s.EVSEs = []orchestrator.EVSEState{evse("cs-001", true, 0, 32.0, 230.0)}
	s.Grid.NetW = -7000 // exporting 7 kW pre-EV-start

	plan := opt.Optimize(s)

	// Find the EV command.
	if len(plan.EVSECommands) == 0 {
		t.Fatal("expected EVSE command")
	}
	evA := plan.EVSECommands[0].MaxCurrentA

	// Find the battery charge command.
	battChargeW := 0.0
	for _, bc := range plan.BatteryCommands {
		if bc.SetpointW < 0 {
			battChargeW += -bc.SetpointW
		}
	}

	// Predicted site export after this tick's commands settle:
	//   8000 (solar) − 1000 (load) − battery_charge − EV
	predictedExportW := 8000.0 - 1000.0 - battChargeW - evA*230.0
	if predictedExportW < -100 { // tolerate sub-100W rounding
		t.Errorf("unconstrained EV would create %.0fW grid import (battery=%.0fW, EV=%.1fA) — self-consumption guardrail failed",
			-predictedExportW, battChargeW, evA)
	}
	logDecisions(t, plan)
}

// Regression for the "demo S1 overshoot": after the first tick commands
// battery=-5kW and EV=6A, the Modbus meter settles to the new export within
// ~1 s but OCPP MeterValues lag ~10 s, so evseW still reports the pre-event
// current.  The old conservation identity
//
//	unconstrainedExportW = signedNetExportW + measuredBatteryAbsorbW + evseW
//
// then over-estimated the surplus, the pre-flight branch boosted the EV by
// 15-20 A, and the site flipped from a clean +620 W export into a 3.4 kW
// import.  Verify the optimizer no longer over-tightens the EV when evseW is
// stale relative to the meter.
func TestScenario_S1_ExportOvershoot_StaleEVMeasurement(t *testing.T) {
	opt := newOpt()
	s := state0()
	s.Solar = []orchestrator.SolarState{solar("pv-0", 8000, 10000)}
	s.Batteries = []orchestrator.BatteryState{battery("bat-0", 0, 40, 5000)}
	s.EVSEs = []orchestrator.EVSEState{evse("cs-001", true, 0, 32.0, 230.0)}
	s.Grid.NetW = -7000
	s.CSIPControl = &orchestrator.CSIPControlState{
		Base: model.DERControlBase{OpModExpLimW: ap(1000)},
	}

	// Tick 1 — first time the export limit is seen.  Soft-start should clamp
	// the EV to 6 A, battery should absorb 5 kW.
	plan := opt.Optimize(s)
	if len(plan.EVSECommands) == 0 {
		t.Fatal("tick 1: expected EVSE command")
	}
	if plan.EVSECommands[0].MaxCurrentA != 6.0 {
		t.Errorf("tick 1: EV MaxCurrentA = %.1fA, want 6.0 (soft-start)",
			plan.EVSECommands[0].MaxCurrentA)
	}

	// Tick 2 — the meter has settled to the post-command export (~620 W) but
	// OCPP MeterValues hasn't arrived yet, so evseW still reports the pre-event
	// current.  Simulate this skew explicitly.
	s.Batteries[0].PowerW = -5000  // battery actuator confirmed
	s.EVSEs[0].CurrentA = 13.4     // stale OCPP reading from pre-event tick
	s.EVSEs[0].PowerW = 13.4 * 230 // ≈ 3082 W stale
	s.Grid.NetW = -620             // meter shows 620 W export (post-command reality)

	plan = opt.Optimize(s)

	if len(plan.EVSECommands) == 0 {
		t.Fatal("tick 2: expected EVSE command")
	}
	ev2 := plan.EVSECommands[0].MaxCurrentA
	if ev2 > 8.0 {
		t.Errorf("tick 2: EV ramped to %.1fA from 6A despite stale measurement — the pre-flight branch over-tightened (want ≤ 8A)", ev2)
	}

	// Predicted post-command export should sit just below the 1 kW limit, not
	// drive the site into import.
	battCmdW := 0.0
	for _, bc := range plan.BatteryCommands {
		if bc.SetpointW < 0 {
			battCmdW += -bc.SetpointW
		}
	}
	predictedExportW := 8000.0 - 1000.0 - battCmdW - ev2*230.0
	if predictedExportW < 0 {
		t.Errorf("tick 2: predicted export %.0fW < 0 — site would import (battery=%.0fW, EV=%.1fA)",
			predictedExportW, battCmdW, ev2)
	}
	if predictedExportW > 1000 {
		t.Errorf("tick 2: predicted export %.0fW exceeds 1 kW limit (battery=%.0fW, EV=%.1fA)",
			predictedExportW, battCmdW, ev2)
	}
	logDecisions(t, plan)
}

// Regression for EV oscillation during the S2 import-limit battery transient.
//
// When the import guard first fires it commands heavy battery discharge to
// bring the grid below the 500 W cap.  During that transient the meter may
// briefly show net export (negative grid.NetW), making surplusW look large and
// causing the EV charging rule to resume the EV mid-transient.  The resumed
// EV then kicks the battery into a new over-limit event → suspend → repeat.
//
// Fix: EV is blocked until evSafeCount (consecutive ticks of positive import
// ≤ cap) reaches EVImportCooldownCycles.  Negative netW resets evSafeCount.
func TestScenario_S2_EV_BlockedDuringImportGuardTransient(t *testing.T) {
	opt := newOpt()
	// Shorten cooldown to 3 for test speed; default is 20.
	opt.EVImportCooldownCycles = 3

	s := state0()
	// No PV: an import-limit episode is a low/no-solar condition, and zeroing PV is
	// what makes the cooldown-gate LIFT observable as plan output. With no solar
	// surplus the EV charging rule falls to its grid-charge branch, which resumes
	// the EV at full rate the moment the gate clears — so tick 5 flips from 0 A
	// (gate up) to a positive current (gate down). With PV present the resume stays
	// at 0 A for lack of surplus and the gate lift would show only in the decision
	// text (the old string oracle); zeroing PV keeps the tick-5 assertion behavioral
	// without perturbing the cooldown mechanics (evSafeCount tracks netW only).
	s.Batteries = []orchestrator.BatteryState{battery("bat-0", 0, 75, 5000)}
	s.EVSEs = []orchestrator.EVSEState{evse("cs-001", true, 0, 32.0, 230.0)}
	s.Grid.NetW = 2380 // EV just started, site way over 500 W cap
	s.CSIPControl = &orchestrator.CSIPControlState{
		Base: model.DERControlBase{OpModImpLimW: ap(500)},
	}

	// Tick 1 — import over limit: EV must be suspended.
	plan1 := opt.Optimize(s)
	for _, ec := range plan1.EVSECommands {
		if ec.MaxCurrentA > 0 {
			t.Errorf("tick 1 (import over limit): EV should be suspended, got %.1fA", ec.MaxCurrentA)
		}
	}

	// Tick 2 — battery over-shot, site now exporting (negative netW).
	// evSafeCount should reset (negative grid), EV must remain suspended.
	s.Batteries[0].PowerW = 3020
	s.Grid.NetW = -2020
	plan2 := opt.Optimize(s)
	for _, ec := range plan2.EVSECommands {
		if ec.MaxCurrentA > 0 {
			t.Errorf("tick 2 (site exporting from over-discharge): EV should remain suspended, got %.1fA", ec.MaxCurrentA)
		}
	}

	// Ticks 3–5 — stable positive import under cap; evSafeCount climbs.
	// EV still blocked until cooldown (3) is reached.
	s.Batteries[0].PowerW = 600
	s.Grid.NetW = 400
	for tick := 3; tick <= 4; tick++ {
		plan := opt.Optimize(s)
		for _, ec := range plan.EVSECommands {
			if ec.MaxCurrentA > 0 {
				t.Errorf("tick %d (cooldown not reached): EV should still be suspended, got %.1fA", tick, ec.MaxCurrentA)
			}
		}
	}

	// Tick 5 — cooldown complete (3 positive-import-under-cap ticks seen).
	// The suppression gate is now down, so the EV charging rule resumes the EV.
	// Behavioral oracle: the plan must now command the EV to a positive current
	// (the export-limit-active-but-importing branch charges at full rate). While
	// the gate was up (ticks 1–4) every EV command was 0 A; the gate lift is what
	// flips the plan output — we assert that flip, not the decision wording.
	plan5 := opt.Optimize(s)
	var evCmd5 *orchestrator.EVSECommand
	for i := range plan5.EVSECommands {
		if plan5.EVSECommands[i].StationID == "cs-001" {
			evCmd5 = &plan5.EVSECommands[i]
		}
	}
	if evCmd5 == nil {
		t.Fatal("tick 5 (cooldown expired): expected an EV command once the gate lifted, got none")
	}
	if evCmd5.MaxCurrentA <= 0 {
		t.Errorf("tick 5 (cooldown expired): EV still held at 0 A — cooldown gate did not lift; got %.1fA", evCmd5.MaxCurrentA)
	}
	logDecisions(t, plan5)
}

// ── Stuck-curtailment-on-reconnect + export convergence (QA 2026-07-03) ────────

// TestOptimizer_RestoresDisconnectedSolarAfterCapClears drives the full Optimize
// path through the stuck-curtailment failure: an export cap curtails the
// inverter, then the cap clears on a tick where the inverter is DARK. The plan
// must still carry the restore command (CurtailToW=NaN) for the dark inverter —
// that command is what lexa-modbus's retryDevice records as the desired state
// and re-asserts on reconnect (see TestRetryDevice_ReassertsDesiredStateOnReconnect
// in cmd/modbus for the southbound half). Without it the device returns still
// clamped at the stale ceiling (QA 2026-07-03: curtailment-release,
// clock-jump-forward, release-while-rebooting).
func TestOptimizer_RestoresDisconnectedSolarAfterCapClears(t *testing.T) {
	opt := newOpt()

	// Tick 1: zero-export cap active, inverter connected and exporting → curtailed.
	s1 := state0()
	s1.Solar = []orchestrator.SolarState{solar("pv", 4000, 5000)}
	s1.Grid.NetW = -4000
	s1.CSIPControl = &orchestrator.CSIPControlState{
		Source: "event", MRID: "cap-1",
		Base: model.DERControlBase{OpModExpLimW: ap(0)},
	}
	p1 := opt.Optimize(s1)
	curtailed := false
	for _, c := range p1.SolarCommands {
		if c.Name == "pv" && !math.IsNaN(c.CurtailToW) && c.CurtailToW < 4500 {
			curtailed = true
		}
	}
	if !curtailed {
		t.Fatalf("setup: expected the export cap to curtail pv; commands=%+v", p1.SolarCommands)
	}

	// Tick 2: the cap has cleared and the inverter is dark on this very tick.
	s2 := state0()
	dark := solar("pv", 0, 5000)
	dark.Connected = false
	dark.Energized = false
	s2.Solar = []orchestrator.SolarState{dark}
	p2 := opt.Optimize(s2)

	restored := false
	for _, c := range p2.SolarCommands {
		if c.Name == "pv" && math.IsNaN(c.CurtailToW) {
			restored = true
		}
	}
	if !restored {
		t.Errorf("cap cleared while inverter dark: plan must carry the restore for the dark device; commands=%+v",
			p2.SolarCommands)
	}
}

// TestOptimizer_DarkSolarUnderActiveCapNotRestored: the counterpart guard — while
// the export cap is still ACTIVE, a dark inverter must NOT be sent a restore
// (its held curtailment is the correct desired state for its reconnect).
func TestOptimizer_DarkSolarUnderActiveCapNotRestored(t *testing.T) {
	opt := newOpt()
	s := state0()
	dark := solar("pv", 0, 5000)
	dark.Connected = false
	dark.Energized = false
	s.Solar = []orchestrator.SolarState{dark}
	s.Grid.NetW = -200
	s.CSIPControl = &orchestrator.CSIPControlState{
		Source: "event", MRID: "cap-1",
		Base: model.DERControlBase{OpModExpLimW: ap(0)},
	}
	plan := opt.Optimize(s)
	for _, c := range plan.SolarCommands {
		if c.Name == "pv" && math.IsNaN(c.CurtailToW) {
			t.Errorf("dark inverter under an ACTIVE cap must not be restored; commands=%+v", plan.SolarCommands)
		}
	}
}

// TestOptimizer_ExportChurnEscalatesCannotComply drives the full Optimize path
// through the control-churn fault: the export cap is rewritten every tick
// (alternating 0 W / 500 W — each rewrite resets the ceiling controller's
// expGuard) while measured export stays far over BOTH caps and the ceiling still
// has room to spare (so the export rule's own zero-lever breach cannot fire).
// The convergence counter must survive the guard resets and escalate to a
// CannotComply breach, stamped with the active control's MRID. This is the
// integration test that pins the counter OUTSIDE the per-value guard reset — the
// design gap that let control-churn/clock-jitter breach silently (QA 2026-07-03).
func TestOptimizer_ExportChurnEscalatesCannotComply(t *testing.T) {
	opt := newOpt()
	churn := []int16{0, 500, 0, 500, 0, 500}
	var last orchestrator.Plan
	firedAt := -1
	for i, cap := range churn {
		s := state0()
		// PV 4500 W with 1600 W of home load → export 2900 W: over both caps,
		// but the computed solar ceiling (~load) stays well above zero, so only
		// the convergence check can report this.
		s.Solar = []orchestrator.SolarState{solar("pv", 4500, 5000)}
		s.Grid.NetW = -2900
		s.CSIPControl = &orchestrator.CSIPControlState{
			Source: "event", MRID: "churn-mrid",
			Base: model.DERControlBase{OpModExpLimW: ap(cap)},
		}
		last = opt.Optimize(s)
		if last.Breach != nil && firedAt < 0 {
			firedAt = i
		}
	}
	if last.Breach == nil {
		t.Fatal("sustained over-cap export across rapid cap rewrites must escalate to CannotComply")
	}
	if last.Breach.LimitType != "export" {
		t.Errorf("LimitType = %q, want export", last.Breach.LimitType)
	}
	if last.Breach.MRID != "churn-mrid" {
		t.Errorf("breach MRID = %q, want churn-mrid (northbound needs it to address the Response)", last.Breach.MRID)
	}
	if firedAt < 2 {
		t.Errorf("breach fired at tick %d — too early, the sustained gate must ride out a normal ramp", firedAt)
	}
}

// TestOptimizer_CommandsStampedWithActiveControlMRID is WS-4.3's optimizer
// test: every per-device command Optimize() produces this tick must carry
// state.CSIPControl.MRID, the same source plan.Breach.MRID already used —
// mirroring, not duplicating, the existing Breach.MRID stamp. A battery
// (connected, dischargeable), a solar inverter, and a session-active EVSE
// together guarantee at least one command lands in all three Plan slices
// (BatteryCommands via the restore/idle fallback, SolarCommands via the
// restore fallback, EVSECommands via applyEVChargingRule — see
// applyRestoreRule/applyEVChargingRule), so this is not a vacuous check.
func TestOptimizer_CommandsStampedWithActiveControlMRID(t *testing.T) {
	opt := newOpt()
	s := state0()
	s.Batteries = []orchestrator.BatteryState{battery("bat-0", 0, 50, 5000)}
	s.Solar = []orchestrator.SolarState{solar("pv-0", 1000, 5000)}
	s.EVSEs = []orchestrator.EVSEState{evse("cs-001", true, 0, 16.0, 230.0)}
	s.CSIPControl = &orchestrator.CSIPControlState{
		Source: "event", MRID: "mrid-active-1",
	}

	plan := opt.Optimize(s)

	if len(plan.BatteryCommands) == 0 || len(plan.SolarCommands) == 0 || len(plan.EVSECommands) == 0 {
		t.Fatalf("expected at least one command in every slice, got battery=%d solar=%d evse=%d",
			len(plan.BatteryCommands), len(plan.SolarCommands), len(plan.EVSECommands))
	}
	for _, c := range plan.BatteryCommands {
		if c.MRID != "mrid-active-1" {
			t.Errorf("BatteryCommand[%s].MRID = %q, want mrid-active-1", c.Name, c.MRID)
		}
	}
	for _, c := range plan.SolarCommands {
		if c.MRID != "mrid-active-1" {
			t.Errorf("SolarCommand[%s].MRID = %q, want mrid-active-1", c.Name, c.MRID)
		}
	}
	for _, c := range plan.EVSECommands {
		if c.MRID != "mrid-active-1" {
			t.Errorf("EVSECommand[%s].MRID = %q, want mrid-active-1", c.StationID, c.MRID)
		}
	}
}

// TestOptimizer_CommandsUnstampedWithNoActiveControl is the counterpart
// guard: with state.CSIPControl == nil (no real CSIP control active — the
// pure-economic case), every command's MRID must stay "" — the brief's
// "if a device axis is authored with no CSIP control active, MRID stays
// empty and that's correct" behavior, not a regression of the fix above.
func TestOptimizer_CommandsUnstampedWithNoActiveControl(t *testing.T) {
	opt := newOpt()
	s := state0()
	s.Batteries = []orchestrator.BatteryState{battery("bat-0", 0, 50, 5000)}
	s.Solar = []orchestrator.SolarState{solar("pv-0", 1000, 5000)}
	s.EVSEs = []orchestrator.EVSEState{evse("cs-001", true, 0, 16.0, 230.0)}
	// s.CSIPControl left nil.

	plan := opt.Optimize(s)

	if len(plan.BatteryCommands) == 0 || len(plan.SolarCommands) == 0 || len(plan.EVSECommands) == 0 {
		t.Fatalf("expected at least one command in every slice, got battery=%d solar=%d evse=%d",
			len(plan.BatteryCommands), len(plan.SolarCommands), len(plan.EVSECommands))
	}
	for _, c := range plan.BatteryCommands {
		if c.MRID != "" {
			t.Errorf("BatteryCommand[%s].MRID = %q, want empty (no active CSIP control)", c.Name, c.MRID)
		}
	}
	for _, c := range plan.SolarCommands {
		if c.MRID != "" {
			t.Errorf("SolarCommand[%s].MRID = %q, want empty (no active CSIP control)", c.Name, c.MRID)
		}
	}
	for _, c := range plan.EVSECommands {
		if c.MRID != "" {
			t.Errorf("EVSECommand[%s].MRID = %q, want empty (no active CSIP control)", c.StationID, c.MRID)
		}
	}
}
