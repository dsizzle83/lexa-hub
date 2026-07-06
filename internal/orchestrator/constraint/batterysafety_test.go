package constraint

import (
	"math"
	"testing"

	"lexa-hub/internal/orchestrator"
)

// Constraint-package twins of the battery-safety suite in internal/orchestrator
// (convergence_test.go:149-234). Each drives BatterySafetyConstraint.Evaluate /
// EvaluateFast through the same tick sequence the legacy test drives
// checkBatterySafety / EvaluateSafety through and asserts the SAME behavioral
// outcome, so the ported SAFETY-tier protection is pinned against the identical
// contract before TASK-063/066 build on it. White-box (package constraint) so the
// session counters and lastCmdW can be inspected directly for the mutation checks.

// ── helpers ────────────────────────────────────────────────────────────────────

// newBattSafetyPair returns a fresh constraint (SOCReserve=20, the legacy default)
// and its base session at the tuned tick (ScaleTicks is a no-op → threshold=3).
func newBattSafetyPair() (*BatterySafetyConstraint, *Session) {
	return NewBatterySafetyConstraint(20.0), NewSession("battery-safety", 0)
}

func battState(name string, powerW, soc float64) orchestrator.SystemState {
	return orchestrator.SystemState{Batteries: []orchestrator.BatteryState{
		{Name: name, PowerW: powerW, SOC: soc, Connected: true, Energized: true},
	}}
}

// hasDisconnectDemand reports whether the demand set force-disconnects the pack
// (AxisConnect, Connect=false) — the SAFETY-tier twin of hasDisconnectCommand.
func hasDisconnectDemand(demands []Demand, name string) bool {
	for _, d := range demands {
		if d.Device == name && d.Axis == AxisConnect && d.Connect != nil && !*d.Connect {
			return true
		}
	}
	return false
}

// ── economic-tick twins (checkBatterySafety) ───────────────────────────────────

// Twin of TestCheckBatterySafety_DisconnectsReserveDrain: a pack measured
// discharging below its reserve for batteryReserveDrainTicks force-disconnects.
func TestBatterySafetyConstraint_DisconnectsReserveDrain(t *testing.T) {
	c, s := newBattSafetyPair()
	var last []Demand
	for i := 0; i < batteryReserveDrainTicks; i++ {
		// 4800 W discharge at 10% SOC — below the 20% reserve.
		last, _ = c.Evaluate(benchInput(battState("bat", 4800, 10)), s)
	}
	if !hasDisconnectDemand(last, "bat") {
		t.Errorf("expected force-disconnect of reserve-draining pack; demands=%+v", last)
	}
}

// Twin of TestCheckBatterySafety_AllowsDischargeAboveReserve: a pack discharging
// legally above its reserve is never disconnected, however long it runs.
func TestBatterySafetyConstraint_AllowsDischargeAboveReserve(t *testing.T) {
	c, s := newBattSafetyPair()
	for i := 0; i < batteryReserveDrainTicks+2; i++ {
		demands, _ := c.Evaluate(benchInput(battState("bat", 4800, 60)), s)
		if hasDisconnectDemand(demands, "bat") {
			t.Fatalf("tick %d: disconnected a pack discharging legally above reserve", i)
		}
	}
}

// Twin of TestCheckBatterySafety_RidesOutSingleTick: one below-reserve reading
// must NOT trip — the debounce confirms over batteryReserveDrainTicks. This is
// half of the reserve-drain mutation guard (see the Mutation test below): if the
// drainTicks counter were unwired to fire on the first sample, this fails.
func TestBatterySafetyConstraint_RidesOutSingleTick(t *testing.T) {
	c, s := newBattSafetyPair()
	demands, _ := c.Evaluate(benchInput(battState("bat", 4800, 10)), s)
	if hasDisconnectDemand(demands, "bat") {
		t.Error("disconnected on a single tick; should confirm over batteryReserveDrainTicks")
	}
}

// Wrong-direction path: with a commanded charge on record, a pack measured
// discharging ABOVE its reserve (so neither the reserve-drain nor the critical
// path fires) still disconnects after batteryReserveDrainTicks — the sign is
// inverted regardless of SOC (audit: battery-wrong-sign). Twin of the
// battWrongDirTicks leg of checkBatterySafety.
func TestBatterySafetyConstraint_DisconnectsWrongDirection(t *testing.T) {
	c, s := newBattSafetyPair()
	c.RecordCommands([]orchestrator.BatteryCommand{{Name: "bat", SetpointW: -3000}}) // commanded charge
	var last []Demand
	for i := 0; i < batteryReserveDrainTicks; i++ {
		// 4800 W DISCHARGE at 80% SOC: above reserve+5, so critical + reserve-drain
		// are both silent; only the wrong-direction counter climbs.
		last, _ = c.Evaluate(benchInput(battState("bat", 4800, 80)), s)
	}
	if !hasDisconnectDemand(last, "bat") {
		t.Errorf("expected wrong-direction disconnect; demands=%+v", last)
	}
}

// A compliant tick between over-reserve ticks resets the reserve-drain counter,
// so the disconnect never accumulates — pins the reset-on-compliant-tick semantics
// (optimizer.go:1539).
func TestBatterySafetyConstraint_CompliantTickResetsDrain(t *testing.T) {
	c, s := newBattSafetyPair()
	in := benchInput(battState("bat", 4800, 10)) // below reserve
	c.Evaluate(in, s)
	c.Evaluate(in, s)
	// Compliant tick (above reserve) resets the counter.
	c.Evaluate(benchInput(battState("bat", 4800, 60)), s)
	// Two more below-reserve ticks: only 2 in a row now, still under threshold 3.
	c.Evaluate(in, s)
	demands, _ := c.Evaluate(in, s)
	if hasDisconnectDemand(demands, "bat") {
		t.Error("disconnected after a compliant tick reset the drain counter")
	}
	if got := c.sess.drainTicks["bat"]; got != 2 {
		t.Errorf("drainTicks = %d after reset+2, want 2", got)
	}
}

// ── fast-path twins (EvaluateSafety) ────────────────────────────────────────────

// Twin of TestEvaluateSafety_DisconnectsSignInversionAtReserve: the fast loop
// disconnects a pack commanded to charge but measured discharging at/near its
// reserve — immediately, no debounce, no economic plan.
func TestBatterySafetyFast_DisconnectsSignInversionAtReserve(t *testing.T) {
	c, _ := newBattSafetyPair()
	c.RecordCommands([]orchestrator.BatteryCommand{{Name: "bat", SetpointW: -3000}})
	demands := c.EvaluateFast(battState("bat", 4800, 12))
	if !hasDisconnectDemand(demands, "bat") {
		t.Fatalf("expected fast-loop disconnect; demands=%+v", demands)
	}
}

// Twin of TestEvaluateSafety_NoDisconnectWhenCharging.
func TestBatterySafetyFast_NoDisconnectWhenCharging(t *testing.T) {
	c, _ := newBattSafetyPair()
	c.RecordCommands([]orchestrator.BatteryCommand{{Name: "bat", SetpointW: -3000}})
	if d := c.EvaluateFast(battState("bat", -3000, 12)); hasDisconnectDemand(d, "bat") {
		t.Error("fast loop disconnected a correctly-charging pack")
	}
}

// Twin of TestEvaluateSafety_NoDisconnectAboveReserve.
func TestBatterySafetyFast_NoDisconnectAboveReserve(t *testing.T) {
	c, _ := newBattSafetyPair()
	c.RecordCommands([]orchestrator.BatteryCommand{{Name: "bat", SetpointW: -3000}})
	if d := c.EvaluateFast(battState("bat", 4800, 80)); hasDisconnectDemand(d, "bat") {
		t.Error("fast loop disconnected far from reserve; should defer to the economic tick")
	}
}

// Twin of TestEvaluateSafety_NoDisconnectWhenDischargeCommanded.
func TestBatterySafetyFast_NoDisconnectWhenDischargeCommanded(t *testing.T) {
	c, _ := newBattSafetyPair()
	c.RecordCommands([]orchestrator.BatteryCommand{{Name: "bat", SetpointW: 3000}}) // DISCHARGE commanded
	if d := c.EvaluateFast(battState("bat", 4800, 12)); hasDisconnectDemand(d, "bat") {
		t.Error("fast loop disconnected a legitimately commanded discharge")
	}
}

// ── mutation guard (reserve-drain disconnect is load-bearing on the counter) ────

// The reserve-drain disconnect must fire AT the threshold and NOT before. Together
// with the threshold-exact trip this is the mutation check: unwiring the drainTicks
// counter (e.g. never incrementing it, or tripping on the first sample) breaks
// exactly one side of this assertion. Verified by hand 2026-07-06 by stubbing the
// `sess.drainTicks[b.Name]++` increment — the "trips at threshold" leg then fails.
func TestBatterySafetyConstraint_MutationReserveDrainThresholdExact(t *testing.T) {
	c, s := newBattSafetyPair()
	in := benchInput(battState("bat", 4800, 10))
	for i := 1; i <= batteryReserveDrainTicks; i++ {
		demands, _ := c.Evaluate(in, s)
		disc := hasDisconnectDemand(demands, "bat")
		switch {
		case i < batteryReserveDrainTicks && disc:
			t.Fatalf("tick %d/%d: disconnected before the debounce threshold", i, batteryReserveDrainTicks)
		case i == batteryReserveDrainTicks && !disc:
			t.Fatalf("tick %d/%d: did NOT disconnect at the threshold — counter not load-bearing", i, batteryReserveDrainTicks)
		}
	}
	if got := c.sess.drainTicks["bat"]; got != batteryReserveDrainTicks {
		t.Errorf("drainTicks = %d at trip, want %d", got, batteryReserveDrainTicks)
	}
}

// ── pruning parity (§11 registries lesson) ──────────────────────────────────────

// When a pack disappears or reads a NaN SOC, its debounce-counter entries are
// pruned — no leaked per-device state (optimizer.go:1529-1530). lastCmdW is NOT
// pruned, matching legacy lastBattCmd (only overwritten, never deleted).
func TestBatterySafetyConstraint_PrunesCountersOnDisappearance(t *testing.T) {
	c, s := newBattSafetyPair()
	c.RecordCommands([]orchestrator.BatteryCommand{{Name: "bat", SetpointW: -3000}})
	// Accumulate some counter state.
	c.Evaluate(benchInput(battState("bat", 4800, 10)), s)
	if c.sess.drainTicks["bat"] == 0 {
		t.Fatal("expected a nonzero drain counter before pruning")
	}
	// Device disconnects: NaN SOC and Connected=false.
	stGone := orchestrator.SystemState{Batteries: []orchestrator.BatteryState{
		{Name: "bat", PowerW: 0, SOC: math.NaN(), Connected: false},
	}}
	c.Evaluate(benchInput(stGone), s)
	if _, ok := c.sess.drainTicks["bat"]; ok {
		t.Error("drainTicks not pruned on device disappearance — per-device leak")
	}
	if _, ok := c.sess.wrongDirTicks["bat"]; ok {
		t.Error("wrongDirTicks not pruned on device disappearance — per-device leak")
	}
	if _, ok := c.sess.lastCmdW["bat"]; !ok {
		t.Error("lastCmdW pruned — legacy lastBattCmd survives a transient disconnect")
	}
}

// ── RecordCommands parity (lastBattCmd write loop) ──────────────────────────────

// RecordCommands records finite setpoints and ignores NaN "leave unchanged"
// commands, so a subsequent fast tick infers direction correctly (optimizer.go:383-386).
func TestBatterySafety_RecordCommandsIgnoresNaN(t *testing.T) {
	c, _ := newBattSafetyPair()
	c.RecordCommands([]orchestrator.BatteryCommand{
		{Name: "a", SetpointW: -3000},
		{Name: "b", SetpointW: math.NaN()},
	})
	if !c.sess.chargeCommanded("a") {
		t.Error("finite charge command not recorded for a")
	}
	if _, ok := c.sess.lastCmdW["b"]; ok {
		t.Error("NaN command recorded for b; should be ignored")
	}
}

// ── fast-path latency contract (allocation-light, arbiter-bypassing) ────────────

// The quiescent fast path must not allocate: it is the 1 s protective loop and
// runs on every safety tick whether or not a fault exists. A regression that made
// EvaluateFast allocate on the no-fault path (e.g. building a demand slice
// unconditionally, or routing through the arbiter) would show here.
func TestBatterySafetyFast_NoAllocWhenQuiescent(t *testing.T) {
	c, _ := newBattSafetyPair()
	c.RecordCommands([]orchestrator.BatteryCommand{{Name: "bat", SetpointW: -3000}})
	st := battState("bat", -3000, 80) // charging correctly: no trip
	if allocs := testing.AllocsPerRun(100, func() {
		if d := c.EvaluateFast(st); len(d) != 0 {
			t.Fatalf("unexpected trip on a healthy pack: %+v", d)
		}
	}); allocs != 0 {
		t.Errorf("EvaluateFast allocated %.0f/run on the quiescent path; want 0", allocs)
	}
}

// ── single-goroutine contract (race-clean under -race, no lock) ─────────────────

// The engine serialises Evaluate (economic tick) and EvaluateFast (safety tick)
// on ONE control goroutine, so the shared session takes no lock. This interleaves
// them exactly as run() does — Evaluate, RecordCommands, then several EvaluateFast
// — in a tight loop; under `go test -race` it proves the serialized path is
// race-clean without a mutex hiding a future contract violation (TASK-062
// common-mistakes: "add a race test instead").
func TestBatterySafety_SerializedInterleaveRaceClean(t *testing.T) {
	c, s := newBattSafetyPair()
	for i := 0; i < 50; i++ {
		demands, _ := c.Evaluate(benchInput(battState("bat", 4800, 10)), s)
		c.RecordCommands([]orchestrator.BatteryCommand{{Name: "bat", SetpointW: -3000}})
		for j := 0; j < 3; j++ {
			_ = c.EvaluateFast(battState("bat", 4800, 12))
		}
		_ = demands
	}
}
