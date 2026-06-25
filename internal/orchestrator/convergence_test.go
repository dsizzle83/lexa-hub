package orchestrator

import (
	"math"
	"strings"
	"testing"
)

// hasCurtailingSolarCommand reports whether the plan curtails any inverter
// meaningfully below the given nameplate (a real curtailment, not a no-op
// ceiling at/above nameplate).
func hasCurtailingSolarCommand(p *Plan, nameplateW float64) bool {
	for _, c := range p.SolarCommands {
		if !math.IsNaN(c.CurtailToW) && c.CurtailToW < nameplateW*0.9 {
			return true
		}
	}
	return false
}

func hasDecisionContaining(p *Plan, substr string) bool {
	for _, d := range p.Decisions {
		if strings.Contains(d.Reason, substr) || strings.Contains(d.Impact, substr) {
			return true
		}
	}
	return false
}

// A battery that ACKs a charge setpoint but never actually absorbs (PowerW stays
// at 0 every tick) must not keep the inverter uncurtailed: after battBreachTicks
// the export rule discredits the phantom absorption and curtails solar to hold
// the export cap. (audit: battery-charge-disabled)
func TestExportLimit_BatteryStallCurtailsSolar(t *testing.T) {
	o := NewDefaultOptimizer()
	limits := gridConstraints{exportLimitW: 0, importLimitW: math.NaN(), maxLimitW: math.NaN()}

	var last *Plan
	for i := 0; i < battBreachTicks; i++ {
		// Fresh state each tick: the pack refuses to charge, so its measured
		// PowerW is 0 even though the prior tick commanded a charge.
		bats := []BatteryState{ruleBat("bat", 0, 50, 5000)}
		sol := []SolarState{ruleSol("pv", 4800)}
		p := &Plan{}
		o.applyExportLimitRule(sol, nil, 0, limits, -4400, 95, 4400, bats, p)
		last = p
	}

	if !hasDecisionContaining(last, "battery not absorbing") {
		t.Errorf("expected a battery-stall decision after %d ticks; decisions=%+v", battBreachTicks, last.Decisions)
	}
	if !hasCurtailingSolarCommand(last, 4800) {
		t.Errorf("expected solar curtailment once the stall was declared; solarCommands=%+v", last.SolarCommands)
	}
}

// A refusing pack with a single mid-run blip (one tick where it momentarily
// absorbs) must still trip the stall guard — the leaky counter decrements on the
// blip instead of resetting, so the curtail is only delayed, not lost. (audit:
// battery-charge-disabled flakiness)
func TestExportLimit_BatteryStallToleratesBlip(t *testing.T) {
	o := NewDefaultOptimizer()
	limits := gridConstraints{exportLimitW: 0, importLimitW: math.NaN(), maxLimitW: math.NaN()}

	// refuse, refuse, blip(absorb), refuse, refuse, refuse — should still curtail.
	blips := []bool{false, false, true, false, false, false}
	var last *Plan
	for _, absorb := range blips {
		batPower, netW, surplus := 0.0, -4400.0, 4400.0
		if absorb { // the pack briefly charges ~half the surplus this tick
			batPower, netW, surplus = -2400, -2000, 2000
		}
		bats := []BatteryState{ruleBat("bat", batPower, 50, 5000)}
		sol := []SolarState{ruleSol("pv", 4800)}
		last = &Plan{}
		o.applyExportLimitRule(sol, nil, 0, limits, netW, 95, surplus, bats, last)
	}
	if !hasDecisionContaining(last, "battery not absorbing") {
		t.Errorf("a stall with one absorb-blip must still curtail (leaky counter); decisions=%+v", last.Decisions)
	}
}

// A battery that DOES absorb the commanded charge (measured tracks command) must
// never trip the stall guard — solar stays uncurtailed (battery-first).
func TestExportLimit_BatteryAbsorbingDoesNotTrip(t *testing.T) {
	o := NewDefaultOptimizer()
	limits := gridConstraints{exportLimitW: 0, importLimitW: math.NaN(), maxLimitW: math.NaN()}

	for i := 0; i < battBreachTicks+2; i++ {
		// The pack is charging at ~4400 W (PowerW negative = charging), i.e. it is
		// honouring the command — measured absorption matches the surplus.
		bats := []BatteryState{ruleBat("bat", -4400, 50, 5000)}
		sol := []SolarState{ruleSol("pv", 4800)}
		p := &Plan{}
		// Meter shows ~0 export because the battery is soaking the surplus.
		o.applyExportLimitRule(sol, nil, 0, limits, 0, 95, 0, bats, p)
		if hasDecisionContaining(p, "battery not absorbing") {
			t.Fatalf("tick %d: stall falsely declared while battery is absorbing", i)
		}
	}
}

// An inverter that echoes the gen-cap back (self-reported power looks compliant)
// but keeps generating must still be caught via the independent meter floor:
// generation ≥ grid_export − battery_discharge. (audit: enable-gate-curtail)
func TestCheckGenConvergence_CaughtByMeterFloorDespiteEchoedReport(t *testing.T) {
	o := NewDefaultOptimizer()
	// Inverter self-reports a compliant 1000 W (the echoed cap) but the meter
	// shows the site exporting 4550 W with no battery discharge — generation is
	// really ≥ 4550 W, over the 1000 W cap.
	solar := []SolarState{{Name: "pv", PowerW: 1000, MaxW: 5000, Connected: true, Energized: true}}
	var last *Plan
	for i := 0; i < genBreachTicks; i++ {
		p := &Plan{}
		o.checkGenLimitConvergence(solar, nil, -4550, 1000, p)
		last = p
	}
	if last.Breach == nil || last.Breach.LimitType != "generation" {
		t.Fatalf("expected a generation breach from the meter floor, got %+v", last.Breach)
	}
}

// Battery discharge legitimately raises export without raising generation, so the
// meter floor must net it out and NOT false-trip a gen breach.
func TestCheckGenConvergence_BatteryDischargeDoesNotFalseTrip(t *testing.T) {
	o := NewDefaultOptimizer()
	solar := []SolarState{{Name: "pv", PowerW: 800, MaxW: 5000, Connected: true, Energized: true}}
	bats := []BatteryState{{Name: "bat", PowerW: 2000, Connected: true, Energized: true}} // discharging
	for i := 0; i < genBreachTicks+2; i++ {
		p := &Plan{}
		// Export 2000 W is all from the battery; generation (800 W) is under the 1000 cap.
		o.checkGenLimitConvergence(solar, bats, -2000, 1000, p)
		if p.Breach != nil {
			t.Fatalf("tick %d: false gen breach while battery supplies the export: %+v", i, p.Breach)
		}
	}
}

// hasDisconnectCommand reports whether the plan force-disconnects the named pack.
func hasDisconnectCommand(p *Plan, name string) bool {
	for _, c := range p.BatteryCommands {
		if c.Name == name && c.Connect != nil && !*c.Connect {
			return true
		}
	}
	return false
}

// A pack discharging at/below its reserve (which no rule commands) is the device
// inverting its setpoint; after batteryReserveDrainTicks the hub disconnects it.
// (audit: battery-wrong-sign)
func TestCheckBatterySafety_DisconnectsReserveDrain(t *testing.T) {
	o := NewDefaultOptimizer() // SOCReserve = 20
	var last *Plan
	for i := 0; i < batteryReserveDrainTicks; i++ {
		// Pack discharging 4800 W at 10% SOC — below the 20% reserve.
		bats := []BatteryState{{Name: "bat", PowerW: 4800, SOC: 10, Connected: true, Energized: true}}
		p := &Plan{}
		o.checkBatterySafety(bats, p)
		last = p
	}
	if !hasDisconnectCommand(last, "bat") {
		t.Errorf("expected force-disconnect of reserve-draining pack; commands=%+v", last.BatteryCommands)
	}
}

// A pack discharging normally ABOVE its reserve must never be disconnected.
func TestCheckBatterySafety_AllowsDischargeAboveReserve(t *testing.T) {
	o := NewDefaultOptimizer()
	for i := 0; i < batteryReserveDrainTicks+2; i++ {
		bats := []BatteryState{{Name: "bat", PowerW: 4800, SOC: 60, Connected: true, Energized: true}}
		p := &Plan{}
		o.checkBatterySafety(bats, p)
		if hasDisconnectCommand(p, "bat") {
			t.Fatalf("tick %d: disconnected a pack discharging legally above reserve", i)
		}
	}
}

// A transient single-tick reserve reading must not trip the disconnect.
func TestCheckBatterySafety_RidesOutSingleTick(t *testing.T) {
	o := NewDefaultOptimizer()
	bats := []BatteryState{{Name: "bat", PowerW: 4800, SOC: 10, Connected: true, Energized: true}}
	p := &Plan{}
	o.checkBatterySafety(bats, p)
	if hasDisconnectCommand(p, "bat") {
		t.Error("disconnected on a single tick; should confirm over batteryReserveDrainTicks")
	}
}

func TestCheckImportConvergence_BreachAfterSustainedOverCap(t *testing.T) {
	o := NewDefaultOptimizer()
	var last *Plan
	for i := 0; i < importBreachTicks; i++ {
		p := &Plan{}
		o.checkImportConvergence(0, 1430, p) // cap 0 W, importing 1430 W
		if i < importBreachTicks-1 && p.Breach != nil {
			t.Fatalf("breach recorded too early at tick %d", i)
		}
		last = p
	}
	if last.Breach == nil {
		t.Fatal("expected a CannotComply breach after sustained import over cap")
	}
	if last.Breach.LimitType != "import" || last.Breach.MeasuredW != 1430 {
		t.Errorf("unexpected breach: %+v", last.Breach)
	}
}

func TestCheckImportConvergence_ResetsWhenCompliant(t *testing.T) {
	o := NewDefaultOptimizer()
	for i := 0; i < importBreachTicks-1; i++ {
		o.checkImportConvergence(0, 1430, &Plan{})
	}
	o.checkImportConvergence(0, 0, &Plan{}) // compliant tick resets the counter
	p := &Plan{}
	o.checkImportConvergence(0, 1430, p) // only one over-cap tick since reset
	if p.Breach != nil {
		t.Error("breach recorded after a compliant tick should have reset the counter")
	}
}

func TestCheckImportConvergence_ClearsOnNoCap(t *testing.T) {
	o := NewDefaultOptimizer()
	for i := 0; i < importBreachTicks-1; i++ {
		o.checkImportConvergence(0, 1430, &Plan{})
	}
	o.checkImportConvergence(math.NaN(), 1430, &Plan{}) // cap cleared → reset
	p := &Plan{}
	o.checkImportConvergence(0, 1430, p)
	if p.Breach != nil {
		t.Error("counter should reset when the import cap clears")
	}
}

// An existing breach from another rule this tick is not overwritten.
func TestCheckImportConvergence_DefersToExistingBreach(t *testing.T) {
	o := NewDefaultOptimizer()
	for i := 0; i < importBreachTicks-1; i++ {
		o.checkImportConvergence(0, 1430, &Plan{})
	}
	p := &Plan{Breach: &ComplianceBreach{LimitType: "import", ShortfallW: 999, Reason: "preexisting"}}
	o.checkImportConvergence(0, 1430, p)
	if p.Breach.Reason != "preexisting" {
		t.Errorf("existing breach was overwritten: %+v", p.Breach)
	}
}
