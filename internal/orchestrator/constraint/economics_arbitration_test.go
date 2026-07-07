package constraint

import (
	"math"
	"testing"

	"lexa-hub/internal/orchestrator"
)

// These prove the AD-007 STRUCTURAL invariant TASK-063 exists to deliver:
// economics PROPOSES, the compliance/safety tiers DISPOSE — an economics demand
// can never widen, flip, or step outside a bound a higher tier set. The proof is
// the tier-aware arbiter (arbiter.go): economics is clamped into the higher-tier
// interval by construction, not by convention.

// ── arbiter unit: the cross-tier clamp is load-bearing ──────────────────────────

// A compliance tier pins the battery to a DISCHARGE point; economics pins a
// contradictory CHARGE point. The higher tier MUST win. This is the case the
// pre-063 min-only arbiter got WRONG — it collapsed to the lowest value, letting
// the economics charge (−3000) override the compliance discharge (+1700), which
// on a live import cap is a cap violation. The tier-aware fold keeps +1700.
func TestResolve_EconomicsChargeCannotOverrideComplianceDischarge(t *testing.T) {
	demands := []Demand{
		PointDemand("bat", AxisBatterySetpointW, 1700, TierCompliance, "import"),
		PointDemand("bat", AxisBatterySetpointW, -3000, TierEconomics, "self-use"),
	}
	iv := Resolve(demands)["bat"].Bounds[AxisBatterySetpointW]
	if iv.Min != 1700 || iv.Max != 1700 {
		t.Fatalf("resolved battery = %+v, want pinned 1700 (compliance); economics must not override it", iv)
	}
	// The seam is recorded so an operator sees WHICH tiers disagreed.
	if len(Resolve(demands)["bat"].Conflicts) == 0 {
		t.Error("cross-tier clamp did not record a conflict")
	}
}

// Economics proposes MORE EV current than a compliance ceiling allows; it is
// clamped INTO the ceiling (a plan-rule / EV-allocation current disposed by a
// compliance cap). Structural analogue of "plan-rule EV current clamped by a cap"
// — proven at the arbiter because compliance EV-ceiling emission is deferred in
// the stack (export.go EV-defer note) until the active flip.
func TestResolve_EconomicsEVCurrentClampedByComplianceCeiling(t *testing.T) {
	demands := []Demand{
		CeilingDemand("cs1#1", AxisEVSECurrentA, 10, TierCompliance, "import"),
		PointDemand("cs1#1", AxisEVSECurrentA, 32, TierEconomics, "ev-charging"),
	}
	iv := Resolve(demands)["cs1#1"].Bounds[AxisEVSECurrentA]
	if iv.Max != 10 {
		t.Fatalf("resolved EV current ceiling = %v, want 10 (compliance); economics proposal must clamp into it", iv.Max)
	}
}

// Safety (post-arbitration in the stack, but here modelled as a Tier-0 interval)
// bounds the battery to [-1000,1000]; an economics point OUTSIDE it (4000) cannot
// widen the admissible interval — the higher tier's bound is kept intact and the
// out-of-range proposal is discarded (projectSetpoint then idles inside it). The
// invariant under test is "economics cannot widen a higher bound", not the exact
// projection.
func TestResolve_EconomicsCannotWidenSafetyInterval(t *testing.T) {
	demands := []Demand{
		{Device: "bat", Axis: AxisBatterySetpointW, Min: -1000, Max: 1000, Tier: TierSafety, Source: "protect"},
		PointDemand("bat", AxisBatterySetpointW, 4000, TierEconomics, "tou"),
	}
	iv := Resolve(demands)["bat"].Bounds[AxisBatterySetpointW]
	if iv.Min != -1000 || iv.Max != 1000 {
		t.Fatalf("resolved = %+v, want the safety bound [-1000,1000] kept (economics 4000 must not widen it)", iv)
	}
	// And a point INSIDE the safety interval is honoured (the normal case).
	iv2 := Resolve([]Demand{
		{Device: "bat", Axis: AxisBatterySetpointW, Min: -1000, Max: 1000, Tier: TierSafety, Source: "protect"},
		PointDemand("bat", AxisBatterySetpointW, 500, TierEconomics, "tou"),
	})["bat"].Bounds[AxisBatterySetpointW]
	if iv2.Min != 500 || iv2.Max != 500 {
		t.Fatalf("in-range economics point resolved = %+v, want pinned 500", iv2)
	}
}

// ── full stack: economics is clamped by an active compliance cap ─────────────────

// THE required mutation test (task step 3, launch brief): a TOU discharge proposal
// is clamped by an active import cap. The import compliance constraint discharges
// the battery only as far as defending the cap requires (+1700); economics, at
// peak, proposes the full MaxDischargeW (+4000). In the resolved plan the battery
// discharges at the COMPLIANCE value, never the larger economics value — economics
// cannot push the site past the cap.
func TestEconomics_TOUDischargeClampedByImportCapInStack(t *testing.T) {
	st := orchestrator.SystemState{
		Timestamp: peakTime(),
		Batteries: []orchestrator.BatteryState{{Name: "bat", PowerW: 0, SOC: 80, MaxChargeW: 5000, MaxDischargeW: 4000, Connected: true, Energized: true}},
		// Importing 2500 over a 1000 cap → compliance discharges ~1700 to defend it.
		Grid: orchestrator.GridState{NetW: 2500, ImportLimitW: 1000, ExportLimitW: math.NaN(), MaxLimitW: math.NaN()},
	}

	// Economics ALONE (no compliance) would discharge the full 4000 — proves the
	// proposal really is the larger value being clamped, not coincidentally equal.
	ec, es := newEconomicsPair()
	if got := battDemand(mustDemands(ec.Evaluate(benchInput(st), es)), "bat"); math.Abs(got-4000) > 1 {
		t.Fatalf("economics-only proposal = %.0f, want 4000 (the value to be clamped)", got)
	}

	plan := benchFullStack(st).Optimize(st)
	got := planBatterySetpoint(plan, "bat")
	if math.IsNaN(got) {
		t.Fatal("no battery command emitted")
	}
	if got > 1700+50 {
		t.Fatalf("resolved battery discharge = %.0f exceeds the import-cap defense ~1700; economics was NOT clamped", got)
	}
	if math.Abs(got-4000) < 1 {
		t.Fatalf("resolved battery discharge = %.0f — the full economics proposal won; the cap was violated", got)
	}
	// The cross-tier clamp must be logged.
	if !hasArbiterConflict(plan) {
		t.Error("economics/compliance clamp not recorded as an arbiter decision")
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func mustDemands(d []Demand, _ *orchestrator.ComplianceBreach) []Demand { return d }

func planBatterySetpoint(p orchestrator.Plan, name string) float64 {
	for _, c := range p.BatteryCommands {
		if c.Name == name {
			return c.SetpointW
		}
	}
	return math.NaN()
}

func hasArbiterConflict(p orchestrator.Plan) bool {
	for _, d := range p.Decisions {
		if d.Rule == "constraint-arbiter" {
			return true
		}
	}
	return false
}
