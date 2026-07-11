package main

// tariff.go compiles a user/cloud-supplied bus.TariffSpec (arriving on
// lexa/intent/tariff, TASK-082/§3.4) into the pieces the optimizer's cost model
// needs: a supply (import) orchestrator.TOUCostModel, an optional delivery/
// distribution TOUCostModel adder, a flat daily service charge, and the
// currency — bundled in a compiledTariff. They are installed on three seams by
// the intent-adoption layer (unit 3.3, NOT this file): the planner's
// Engine.SetFallbackTOU (the supply model, used only when CSIP SetPrices arrays
// are nil — utility pricing keeps winning by the planner's nil-slice fallback
// rule), the reactive optimizer's SwapCostModel (the supply model, §3.4), and
// Engine.SetDeliveryTariff (the delivery adder + fixed charge + currency, PR-D).
// compileTariff is a pure function: it performs no I/O and touches no engine
// state, so unit 3.3 and its tests can call it freely.
//
// Zone discipline: the compiled TOUCostModel evaluates its hour boundaries via
// t.Hour() in whatever time.Location the caller's time carries — i.e. the SOM
// process zone — exactly like DefaultTOUCostModel and the planner. This file
// adds NO zone handling of its own; the process-zone/tariff-zone match is
// asserted once at startup by checkTariffZone (tariffzone.go, WS-8/TASK-079/
// GAP-05). TariffPeriod.StartHH/EndHH are therefore interpreted as local
// tariff-zone clock hours, and DST spring-forward/fall-back price correctly as
// long as that assertion holds (pinned in tariff_test.go).

import (
	"fmt"
	"math"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/orchestrator"
)

// dayNames maps the TariffPeriod.Days convention (0=Sun … 6=Sat) to labels used
// in error messages.
var dayNames = [7]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}

// hourCell is one resolved hour×day grid slot during compilation. It carries
// BOTH rate axes the spec can define for that hour: the supply/import rate
// (rate) and the volumetric delivery adder (delivery, 0 when the owning period
// omits DeliveryPerKwh), so the single validated grid feeds both compiled models.
type hourCell struct {
	set      bool
	rate     float64
	delivery float64
	label    string
	fromIdx  int // index of the period that filled this cell (for overlap messages)
}

// compiledTariff bundles compileTariff's outputs. The intent-adoption layer
// (intent.go applyTariff) feeds Supply to Engine.SetFallbackTOU + the reactive
// optimizer's SwapCostModel, and Delivery/FixedDaily/Currency to
// Engine.SetDeliveryTariff.
type compiledTariff struct {
	// Supply is the import/supply cost model — the one rate axis the legacy
	// path already used. Never nil on a nil error.
	Supply *orchestrator.TOUCostModel
	// Delivery is the volumetric delivery/distribution adder, built the same
	// way as Supply but from each period's DeliveryPerKwh. nil when NO period
	// carries a delivery charge (⇒ no delivery adder), matching
	// SetDeliveryTariff's nil-means-none contract.
	Delivery *orchestrator.TOUCostModel
	// FixedDaily is the flat daily service charge ($/day), spec.FixedDailyCharge
	// dereferenced (0 when absent).
	FixedDaily float64
	// Currency is the ISO currency code (compileTariff enforces "USD").
	Currency string
}

// compileTariff validates and compiles a TariffSpec into a compiledTariff (the
// supply TOUCostModel plus the optional delivery adder, fixed daily charge, and
// currency).
//
// Delivery/distribution charges (per-period DeliveryPerKwh) compile into a
// SECOND TOUCostModel built exactly like the supply model — same 168-cell
// coverage/overlap/day-invariance validation, same coalescing — from each
// period's delivery rate (a period that omits DeliveryPerKwh contributes 0 for
// its hours). If NO period carries a delivery charge, Delivery is nil (no
// adder). A non-finite or negative delivery rate is rejected exactly like an
// import rate is. The fixed daily charge is spec.FixedDailyCharge (nil ⇒ 0) and
// the currency is spec.Currency ("USD", enforced below).
//
// The honest v1 mapping — TOUCostModel carries FEWER dimensions than TariffSpec,
// so unsupported dimensions are REJECTED loudly rather than silently flattened
// (the GAP-05 lesson: a tariff with holes, or one that quietly drops a rate
// axis, misprices with no error anywhere):
//
//   - Import rate → TOUPeriod.RatePerKwh (the one rate axis TOUCostModel has).
//   - Day-of-week variation → NOT representable (TOUCostModel keys off t.Hour()
//     only, day-blind). A spec whose rate OR label differs across days for the
//     same hour is rejected. A spec that merely lists Days per period is fine
//     as long as every hour resolves day-invariantly (the usual case: each
//     period spans all 7 days, or the day sets partition the week identically).
//   - Export rate → NOT representable (TOUCostModel has no export field; the
//     planner's FallbackTOU path already treats export as 0). A non-zero
//     ExportPerKwh is rejected so it is never silently dropped; absent or an
//     explicit 0.0 is accepted (nothing is lost).
//
// Validation, all rejected with a descriptive error: currency must be exactly
// "USD"; ≥1 period; per period Days non-empty and each in 0–6, StartHH in 0–23,
// EndHH in 1–24, StartHH < EndHH (midnight-crossing/wrap windows must be split
// by the caller into [start,24) + [0,end) — TOUCostModel's own natural
// representation, cf. DefaultTOUCostModel's off-peak split; import/export rates
// finite and ≥ 0. Full coverage: every one of the 168 hour×day cells must be
// filled by exactly one period — a gap names the first uncovered hour, an
// overlap names the colliding cell.
func compileTariff(spec bus.TariffSpec) (compiledTariff, error) {
	if spec.Currency != "USD" {
		return compiledTariff{}, fmt.Errorf("tariff: currency %q not supported in v1 (only \"USD\"; milli-currency/other-currency handling is unverified — see the orchestrator review memo)", spec.Currency)
	}
	if len(spec.Periods) == 0 {
		return compiledTariff{}, fmt.Errorf("tariff: at least one period is required")
	}

	// 7 days × 24 hours resolution grid; every cell must end up filled exactly once.
	var grid [7][24]hourCell

	// anyDelivery tracks whether ANY period carries a delivery charge; when none
	// does, the delivery model is nil (no adder) rather than an all-zero model.
	var anyDelivery bool

	for i, p := range spec.Periods {
		if len(p.Days) == 0 {
			return compiledTariff{}, fmt.Errorf("tariff: period %d (%q): Days is empty", i, p.Label)
		}
		if p.StartHH < 0 || p.StartHH > 23 {
			return compiledTariff{}, fmt.Errorf("tariff: period %d (%q): start_hh %d out of range [0,23]", i, p.Label, p.StartHH)
		}
		if p.EndHH < 1 || p.EndHH > 24 {
			return compiledTariff{}, fmt.Errorf("tariff: period %d (%q): end_hh %d out of range [1,24]", i, p.Label, p.EndHH)
		}
		if p.StartHH >= p.EndHH {
			return compiledTariff{}, fmt.Errorf("tariff: period %d (%q): start_hh %d must be < end_hh %d — a midnight-crossing window must be split into [start,24) and [0,end) by the caller (wrap is not allowed in v1)", i, p.Label, p.StartHH, p.EndHH)
		}
		if math.IsNaN(p.ImportPerKwh) || math.IsInf(p.ImportPerKwh, 0) || p.ImportPerKwh < 0 {
			return compiledTariff{}, fmt.Errorf("tariff: period %d (%q): import_per_kwh %v must be finite and ≥ 0", i, p.Label, p.ImportPerKwh)
		}
		if p.ExportPerKwh != nil {
			ex := *p.ExportPerKwh
			if math.IsNaN(ex) || math.IsInf(ex, 0) || ex < 0 {
				return compiledTariff{}, fmt.Errorf("tariff: period %d (%q): export_per_kwh %v must be finite and ≥ 0", i, p.Label, ex)
			}
			if ex > 0 {
				return compiledTariff{}, fmt.Errorf("tariff: period %d (%q): export_per_kwh %v not supported in v1 — TOUCostModel is import-only, so a non-zero export rate would be silently dropped; export pricing rides the CSIP ExportPricePerKwh path, not the tariff-intent fallback", i, p.Label, ex)
			}
		}
		// Delivery adder: same finite/≥0 rejection as the import rate. Absent ⇒
		// 0 for this period's hours (no delivery component modeled).
		var deliveryRate float64
		if p.DeliveryPerKwh != nil {
			deliveryRate = *p.DeliveryPerKwh
			if math.IsNaN(deliveryRate) || math.IsInf(deliveryRate, 0) || deliveryRate < 0 {
				return compiledTariff{}, fmt.Errorf("tariff: period %d (%q): delivery_per_kwh %v must be finite and ≥ 0", i, p.Label, deliveryRate)
			}
			anyDelivery = true
		}

		for _, d := range p.Days {
			if d < 0 || d > 6 {
				return compiledTariff{}, fmt.Errorf("tariff: period %d (%q): day %d out of range [0,6] (0=Sun … 6=Sat)", i, p.Label, d)
			}
			for h := p.StartHH; h < p.EndHH; h++ {
				if grid[d][h].set {
					prev := grid[d][h].fromIdx
					return compiledTariff{}, fmt.Errorf("tariff: overlap at %s %02d:00 — period %d (%q) collides with period %d (%q)",
						dayNames[d], h, i, p.Label, prev, spec.Periods[prev].Label)
				}
				grid[d][h] = hourCell{set: true, rate: p.ImportPerKwh, delivery: deliveryRate, label: p.Label, fromIdx: i}
			}
		}
	}

	// Full coverage: name the first uncovered hour×day.
	for d := 0; d < 7; d++ {
		for h := 0; h < 24; h++ {
			if !grid[d][h].set {
				return compiledTariff{}, fmt.Errorf("tariff: gap — %s %02d:00 is not covered by any period (every hour of every day must resolve to exactly one rate)", dayNames[d], h)
			}
		}
	}

	// Day-invariance: TOUCostModel is day-blind, so every hour must resolve to
	// the same rate AND label across all 7 days, else the spec genuinely needs
	// a per-day-of-week dimension the model cannot represent. The delivery adder
	// is checked the same way (only when in play): two distinct periods can share
	// an import rate + label yet differ in delivery, which the day-blind delivery
	// model likewise cannot represent.
	for h := 0; h < 24; h++ {
		ref := grid[0][h]
		for d := 1; d < 7; d++ {
			c := grid[d][h]
			if c.rate != ref.rate {
				return compiledTariff{}, fmt.Errorf("tariff: per-day-of-week rates not supported in v1 — hour %02d:00 is %.5f on %s but %.5f on %s (TOUCostModel keys off hour-of-day only)",
					h, ref.rate, dayNames[0], c.rate, dayNames[d])
			}
			if c.label != ref.label {
				return compiledTariff{}, fmt.Errorf("tariff: per-day-of-week labels not supported in v1 — hour %02d:00 is %q on %s but %q on %s (TOUCostModel keys off hour-of-day only)",
					h, ref.label, dayNames[0], c.label, dayNames[d])
			}
			if anyDelivery && c.delivery != ref.delivery {
				return compiledTariff{}, fmt.Errorf("tariff: per-day-of-week delivery rates not supported in v1 — hour %02d:00 delivery is %.5f on %s but %.5f on %s (TOUCostModel keys off hour-of-day only)",
					h, ref.delivery, dayNames[0], c.delivery, dayNames[d])
			}
		}
	}

	// Coalesce day-0's row (representative of all days, per the invariance check
	// above) into each compiled model. The supply model always exists; the
	// delivery model only when some period carried a delivery charge.
	supply := buildTOUModel(&grid[0], func(c hourCell) float64 { return c.rate })
	var delivery *orchestrator.TOUCostModel
	if anyDelivery {
		delivery = buildTOUModel(&grid[0], func(c hourCell) float64 { return c.delivery })
	}

	var fixedDaily float64
	if spec.FixedDailyCharge != nil {
		fixedDaily = *spec.FixedDailyCharge
	}

	return compiledTariff{
		Supply:     supply,
		Delivery:   delivery,
		FixedDaily: fixedDaily,
		Currency:   spec.Currency, // "USD" (enforced above)
	}, nil
}

// buildTOUModel coalesces day-0's representative hour row into contiguous
// non-wrapping TOUPeriods for the rate rateOf reads off each cell, and derives
// the (defaultRate, peakThreshold) NewTOUCostModel needs. It runs on an
// already-validated grid (full coverage, day-invariant for this rate axis), so
// day-0 stands in for every day. It is shared by the supply (import) and
// delivery passes — the ONLY difference between the two models is which rate
// axis rateOf selects.
func buildTOUModel(row *[24]hourCell, rateOf func(hourCell) float64) *orchestrator.TOUCostModel {
	var periods []orchestrator.TOUPeriod
	maxRate := rateOf(row[0])
	firstRate := rateOf(row[0])
	multipleRates := false

	runStart := 0
	for h := 1; h <= 24; h++ {
		if h < 24 {
			if r := rateOf(row[h]); r > maxRate {
				maxRate = r
			}
			if rateOf(row[h]) != firstRate {
				multipleRates = true
			}
		}
		// Close the current run at a rate/label change or at end-of-day.
		if h == 24 || rateOf(row[h]) != rateOf(row[runStart]) || row[h].label != row[runStart].label {
			periods = append(periods, orchestrator.TOUPeriod{
				StartHour:  runStart,
				EndHour:    h,
				RatePerKwh: rateOf(row[runStart]),
				Label:      row[runStart].label,
			})
			runStart = h
		}
	}

	// peakThreshold: only the top rate tier is "peak" (mirrors
	// DefaultTOUCostModel, where 0.38 ≥ 0.30 is peak and the lower tiers are
	// not — setting the threshold at maxRate classifies identically since a
	// top-tier hour's rate is the same float64 as maxRate). A FLAT tariff (one
	// distinct rate) has no peak window to shave, so the threshold is set just
	// above every rate and IsPeakHour is false everywhere. (The delivery model
	// is only ever queried for CurrentRate, so its peak classification is moot.)
	peakThreshold := maxRate
	if !multipleRates {
		peakThreshold = maxRate + 1.0
	}

	// defaultRate is unreachable given the full-coverage check above; set it to
	// the most expensive rate so that if coverage validation is ever bypassed,
	// an uncovered hour prices HIGH (fail-safe), never silently cheap (GAP-05).
	return orchestrator.NewTOUCostModel(periods, maxRate, peakThreshold)
}
