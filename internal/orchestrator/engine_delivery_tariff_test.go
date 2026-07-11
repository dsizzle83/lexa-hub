package orchestrator

import (
	"math"
	"testing"
	"time"
)

// White-box tests for the PR-B delivery-tariff seam: SetDeliveryTariff (async
// command → replan) and the resolved price arrays PlanSnapshot carries for the
// GET /plan projection. They live in package orchestrator (like
// engine_plansnapshot_test.go) so they can drive replan() directly and read the
// snapshot. snapReader/snapBattState come from engine_plansnapshot_test.go and
// concOptimizer from engine_concurrency_test.go (same package).

// flatTOU returns a TOU model whose CurrentRate is `rate` at every hour (no
// periods → the default rate always applies), so the delivery/import assertions
// below are independent of the machine's time.Local zone (planStep* helpers
// render slot time in .Local()).
func flatTOU(rate float64) *TOUCostModel { return NewTOUCostModel(nil, rate, 0) }

// TestSetDeliveryTariff_SnapshotAndTotalCost pins the PR-B brief: the setter
// feeds the planner, the snapshot's ImportPriceKwh folds delivery into supply,
// DeliveryPriceKwh carries the delivery component, Currency/FixedDailyCharge are
// surfaced, and the flat daily charge lands in Plan.TotalCost.
func TestSetDeliveryTariff_SnapshotAndTotalCost(t *testing.T) {
	const (
		supply   = 0.20
		delivery = 0.05
		export   = 0.04
		fixed    = 1.75
	)
	e := New(snapReader{state: snapBattState()}, &concOptimizer{}, Config{Interval: time.Hour})

	// Deterministic, flat supply/export prices over all 288 slots so the all-in
	// import assertion is exact and zone-independent (planStepImportPrice reads
	// the array directly, bypassing FallbackTOU).
	impPrices := make([]float64, planSteps)
	expPrices := make([]float64, planSteps)
	for i := range impPrices {
		impPrices[i] = supply
		expPrices[i] = export
	}
	e.SetPrices(impPrices, expPrices)

	// Baseline: the SAME delivery tariff, NO fixed charge. Because the fixed
	// charge does not enter any per-slot MarginalCost, it cannot shift a
	// setpoint, so the DP dispatch (hence the pre-fixed cost) is identical to the
	// fixed-charge run below — the delta isolates exactly the fixed charge.
	e.SetDeliveryTariff(flatTOU(delivery), 0, "USD")
	e.drainCmds()
	e.replan()
	base := e.DailyPlanSnapshot()
	if base.Plan == nil {
		t.Fatal("baseline replan produced no plan")
	}
	cost0 := base.Plan.TotalCost

	// Now with the flat daily charge applied.
	e.SetDeliveryTariff(flatTOU(delivery), fixed, "USD")
	e.drainCmds()
	e.replan()
	snap := e.DailyPlanSnapshot()
	if snap.Plan == nil {
		t.Fatal("replan produced no plan")
	}

	if snap.Currency != "USD" {
		t.Errorf("Currency = %q, want USD", snap.Currency)
	}
	if math.Abs(snap.FixedDailyCharge-fixed) > 1e-9 {
		t.Errorf("FixedDailyCharge = %v, want %v", snap.FixedDailyCharge, fixed)
	}

	if len(snap.ImportPriceKwh) != planSteps ||
		len(snap.DeliveryPriceKwh) != planSteps ||
		len(snap.ExportPriceKwh) != planSteps {
		t.Fatalf("price array lengths: imp=%d del=%d exp=%d, want %d each",
			len(snap.ImportPriceKwh), len(snap.DeliveryPriceKwh), len(snap.ExportPriceKwh), planSteps)
	}
	for i := range snap.ImportPriceKwh {
		if math.Abs(snap.ImportPriceKwh[i]-(supply+delivery)) > 1e-9 {
			t.Fatalf("ImportPriceKwh[%d] = %v, want %v (supply+delivery)", i, snap.ImportPriceKwh[i], supply+delivery)
		}
		if math.Abs(snap.DeliveryPriceKwh[i]-delivery) > 1e-9 {
			t.Fatalf("DeliveryPriceKwh[%d] = %v, want %v (non-zero delivery)", i, snap.DeliveryPriceKwh[i], delivery)
		}
		if math.Abs(snap.ExportPriceKwh[i]-export) > 1e-9 {
			t.Fatalf("ExportPriceKwh[%d] = %v, want %v", i, snap.ExportPriceKwh[i], export)
		}
	}

	// The fixed charge is added once to TotalCost on top of an identical
	// dispatch.
	if diff := snap.Plan.TotalCost - cost0; math.Abs(diff-fixed) > 1e-6 {
		t.Errorf("TotalCost delta from fixed charge = %v, want %v", diff, fixed)
	}
}

// TestSetDeliveryTariff_Defaults covers the no-op / default branches: an empty
// currency resolves to "USD", and with no delivery tariff set the delivery
// component is zero everywhere while the all-in import still equals supply.
func TestSetDeliveryTariff_Defaults(t *testing.T) {
	e := New(snapReader{state: snapBattState()}, &concOptimizer{}, Config{Interval: time.Hour})

	const supply = 0.22
	impPrices := make([]float64, planSteps)
	for i := range impPrices {
		impPrices[i] = supply
	}
	e.SetPrices(impPrices, make([]float64, planSteps))

	// Empty currency + nil delivery + zero fixed charge.
	e.SetDeliveryTariff(nil, 0, "")
	e.drainCmds()
	e.replan()
	snap := e.DailyPlanSnapshot()
	if snap.Plan == nil {
		t.Fatal("replan produced no plan")
	}

	if snap.Currency != "USD" {
		t.Errorf("empty currency resolved to %q, want USD", snap.Currency)
	}
	if snap.FixedDailyCharge != 0 {
		t.Errorf("FixedDailyCharge = %v, want 0", snap.FixedDailyCharge)
	}
	for i := range snap.DeliveryPriceKwh {
		if snap.DeliveryPriceKwh[i] != 0 {
			t.Fatalf("DeliveryPriceKwh[%d] = %v, want 0 with no delivery tariff", i, snap.DeliveryPriceKwh[i])
		}
		if math.Abs(snap.ImportPriceKwh[i]-supply) > 1e-9 {
			t.Fatalf("ImportPriceKwh[%d] = %v, want %v (supply only)", i, snap.ImportPriceKwh[i], supply)
		}
	}
}
