package main

// openadr_adopt.go is the WP-15 hub-adoption slice (standards-buildout E1,
// deferred half of architecture.md §5(d)): cmd/hub consumes the two retained
// documents lexa-openadr publishes (internal/bus/openadr.go) —
//
//	lexa/openadr/prices  bus.OpenADRPrices  → Engine.SetPrices (D9 precedence)
//	lexa/openadr/limits  bus.OpenADRLimits  → GridState assembly (D9 most-
//	                                          restrictive merge, state.go)
//
// — under architecture.md decision D9 (NORMATIVE):
//
//	Prices:  CSIP tariff (§10.5 walk) > OpenADR CP prices > app/cloud tariff
//	         intent. Each layer fills only seams the higher one left empty.
//	Limits:  combine MOST-RESTRICTIVE with CSIP (min per axis), with
//	         per-source attribution kept for breach reporting.
//	Breach:  a merged-cap breach attributes to the CSIP MRID only when CSIP is
//	         the binding cap; an OpenADR-only bind must never produce a 2030.5
//	         CannotComply.
//
// Prices ride the EXISTING two-seam Engine surface (SetPrices/SetFallbackTOU)
// rather than a new orchestrator seam — see openADRAdopter's doc. Limits ride
// the EXISTING GridState.Import/ExportLimitW fields, which nothing in this
// package wrote before this slice (state.go's ReadSystemState left them at
// NewGridState's NaN, so the internal/orchestrator optimizer's
// deriveGridConstraints only ever saw the CSIP side) — see
// mergeOpenADRLimitsLocked's doc for why simply assigning here gets the
// most-restrictive merge for free, with zero orchestrator changes.
//
// The CannotComply gate (D9's last bullet) lives in main.go's planObserver,
// immediately before episodes.OnPlan — see OpenADRBoundAxis's doc and this
// task's report for the one path it deliberately does NOT cover (device-level
// reconciler non-convergence evidence).

import (
	"log/slog"
	"math"
	"sync"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/orchestrator"
)

// openADRPlanStepSec/openADRPlanSteps mirror pricesFromPricingUpdate's 5-min,
// 24h grid (main.go) exactly, so an OpenADR price series and a CSIP pricing
// update produce directly comparable []float64 shapes for Engine.SetPrices.
const (
	openADRPlanStepSec = int64(5 * 60)
	openADRPlanSteps   = 288
)

// pricesFromOpenADR converts an OpenADRPrices document into the per-step
// import/export price arrays ($/kWh) Engine.SetPrices wants, using absolute
// RFC3339 (StartTs/DurationS) intervals rather than CSIP's hour-of-day
// tariff-interval shape — see OpenADRPriceInterval's doc (internal/bus/openadr.go).
//
// Only the FIRST "PRICE" series and the FIRST "EXPORT_PRICE" series are used
// (deterministic ordering per OpenADRPrices' doc: Kind then EventID) — the
// same "first tariff profile only" simplification pricesFromPricingUpdate
// applies for CSIP. GHG/ALERT_* kinds are not pricing data and are ignored
// here (a future consumer, not this slice).
//
// importPrices is nil when no "PRICE" series is present (mirrors
// pricesFromPricingUpdate's "no usable pricing data" return) — the caller
// (openADRAdopter.AdoptPrices) never promotes an export-only document into
// SetPrices, matching the CSIP handler's own `if imp != nil` gate in main.go.
// Uncovered import steps fall back to the same DefaultTOUCostModel rate
// pricesFromPricingUpdate uses (never zero — zero would tell the planner
// uncovered hours are free electricity). Uncovered export steps are zero
// (no remuneration data for that step, matching "nil export means zero"
// throughout this codebase).
func pricesFromOpenADR(msg bus.OpenADRPrices, now time.Time) (importPrices, exportPrices []float64) {
	ws := now.Unix() - (now.Unix() % openADRPlanStepSec)

	var priceSeries, exportSeries *bus.OpenADRPriceSeries
	for i := range msg.Series {
		s := &msg.Series[i]
		switch s.Kind {
		case "PRICE":
			if priceSeries == nil {
				priceSeries = s
			}
		case "EXPORT_PRICE":
			if exportSeries == nil {
				exportSeries = s
			}
		}
	}
	if priceSeries == nil {
		return nil, nil
	}

	importPrices = make([]float64, openADRPlanSteps)
	fallback := orchestrator.DefaultTOUCostModel()
	for i := range importPrices {
		stepT := ws + int64(i)*openADRPlanStepSec
		importPrices[i] = fallback.CurrentRate(time.Unix(stepT, 0).Local())
	}
	fillOpenADRIntervals(importPrices, priceSeries.Intervals, ws)

	if exportSeries != nil {
		exportPrices = make([]float64, openADRPlanSteps) // zero-filled outside declared intervals
		fillOpenADRIntervals(exportPrices, exportSeries.Intervals, ws)
	}
	return importPrices, exportPrices
}

// fillOpenADRIntervals overlays intervals' absolute-time price values onto
// dst's 5-min grid (window start ws, openADRPlanSteps slots). DurationS <= 0
// is the OpenADR 3.1 "unbounded" sentinel (see OpenADRPriceInterval's doc) —
// such an interval fills every step from StartTs to the end of the grid.
func fillOpenADRIntervals(dst []float64, intervals []bus.OpenADRPriceInterval, ws int64) {
	for _, iv := range intervals {
		unbounded := iv.DurationS <= 0
		tEnd := iv.StartTs + iv.DurationS
		for i := 0; i < len(dst); i++ {
			stepT := ws + int64(i)*openADRPlanStepSec
			if stepT >= iv.StartTs && (unbounded || stepT < tEnd) {
				dst[i] = iv.Value
			}
		}
	}
}

// openADRAdopter owns the D9 price-precedence state that spans two
// independent subscription handlers in main.go: the pre-existing
// lexa/csip/pricing handler (MarkCSIPPriceSeen) and the new
// lexa/openadr/prices handler (AdoptPrices).
//
// The three-tier precedence (CSIP > OpenADR > tariff intent) rides the
// EXISTING two-seam Engine surface — SetPrices and SetFallbackTOU, see
// tariff.go's package doc — rather than a new orchestrator seam. main.go's
// CSIP pricing handler only ever calls eng.SetPrices when a pricing update
// carries real tariff-profile data (pricesFromPricingUpdate's nil,nil
// "no data" case is never promoted, and once real data has been promoted the
// engine's importPrices slot is never reset to nil by anything in this
// codebase — "utility pricing keeps winning", tariff.go's doc). This adopter
// mirrors that stickiness: csipPriceSeen latches true FOREVER the first time
// the CSIP handler sees real data, and from that point on OpenADR prices are
// never promoted into SetPrices again — CSIP wins outright and keeps
// winning. Until csipPriceSeen latches, every fresh (non-stale)
// lexa/openadr/prices document is promoted, filling the CSIP-absent seam.
// app/cloud tariff intent (SetFallbackTOU, cmd/hub/intent.go's applyTariff)
// is used only when NEITHER CSIP nor OpenADR has ever supplied a price
// array — already the engine's own nil-importPrices fallback rule,
// untouched by this slice.
type openADRAdopter struct {
	mu            sync.Mutex
	csipPriceSeen bool
	priceMaxAge   time.Duration
	lastStaleWarn time.Time // rate-limits the stale-doc WARN, rewalkRateLimit-style
}

// newOpenADRAdopter builds the adopter. priceMaxAge is cfg.OpenADRPriceMaxAge()
// (0 disables the staleness check entirely, matching
// MQTTSystemReader.retainedAdoptionMaxAge's zero-disables convention).
func newOpenADRAdopter(priceMaxAge time.Duration) *openADRAdopter {
	return &openADRAdopter{priceMaxAge: priceMaxAge}
}

// MarkCSIPPriceSeen latches "CSIP has supplied real pricing" permanently
// true (see the struct doc). Called from main.go's existing lexa/csip/pricing
// handler exactly when pricesFromPricingUpdate yields non-nil import prices
// — the same moment the engine's own SetPrices seam gets its first real CSIP
// data.
func (a *openADRAdopter) MarkCSIPPriceSeen() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.csipPriceSeen = true
}

// AdoptPrices decides whether msg should be promoted into Engine.SetPrices
// under D9 precedence. ok is false when: CSIP already owns the seam
// (csipPriceSeen); the document is stale at adoption (TASK-042 pattern —
// WARN once per rate-limit window, never fabricate); or the document carries
// no usable "PRICE" series (pricesFromOpenADR's nil-import case). now is the
// adoption-time clock (a seam for tests; main.go passes time.Now()).
func (a *openADRAdopter) AdoptPrices(msg bus.OpenADRPrices, now time.Time) (imp, exp []float64, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.csipPriceSeen {
		return nil, nil, false // CSIP wins outright and keeps winning (D9)
	}
	if a.priceMaxAge > 0 && msg.Ts > 0 {
		age := now.Sub(time.Unix(msg.Ts, 0))
		if age > a.priceMaxAge {
			if now.Sub(a.lastStaleWarn) >= rewalkRateLimit {
				a.lastStaleWarn = now
				slog.Warn("lexa-hub: OpenADR prices document stale at adoption — skipping (never fabricated)",
					"age", age.Round(time.Second).String(), "max_age", a.priceMaxAge.String())
			}
			return nil, nil, false
		}
	}
	imp, exp = pricesFromOpenADR(msg, now)
	if imp == nil {
		return nil, nil, false // no usable "PRICE" series this pass
	}
	return imp, exp, true
}

// openADRLimitsSnapshot is the latest adopted lexa/openadr/limits document
// (bus.OpenADRLimits), held on MQTTSystemReader under its own mutex — see
// onOpenADRLimits/mergeOpenADRLimitsLocked below.
type openADRLimitsSnapshot struct {
	have       bool
	impLimW    float64 // NaN = axis absent on this doc
	expLimW    float64
	validUntil int64 // Unix seconds; 0 = unbounded
}

// onOpenADRLimits is the lexa/openadr/limits subscription handler (main.go):
// it only STORES the latest retained doc, under r.mu — merging into
// GridState happens once per tick inside ReadSystemState
// (mergeOpenADRLimitsLocked), mirroring onCSIPControl's store-now/merge-later
// split (state.go's doc on why: this handler runs on the MQTT subscription
// goroutine, ReadSystemState on the engine's control goroutine).
func (r *MQTTSystemReader) onOpenADRLimits(_ string, msg bus.OpenADRLimits) {
	r.mu.Lock()
	defer r.mu.Unlock()
	snap := openADRLimitsSnapshot{have: true, impLimW: math.NaN(), expLimW: math.NaN(), validUntil: msg.ValidUntil}
	if msg.ImpLimW != nil {
		snap.impLimW = *msg.ImpLimW
	}
	if msg.ExpLimW != nil {
		snap.expLimW = *msg.ExpLimW
	}
	r.openADRLimits = snap
}

// openADRBindEps (watts) tolerates float noise when comparing an OpenADR cap
// against CSIP's own raw cap on the same axis for CannotComply attribution
// (OpenADRBoundAxis). A tied/near-tied cap is classified as CSIP-attributable
// (i.e. NOT gated) — failing to report a genuine CSIP-attributable breach is
// worse than one avoidable extra Response, so ties resolve toward reporting.
const openADRBindEps = 1.0

// mergeOpenADRLimitsLocked folds the latest adopted OpenADR capacity limits
// into grid (D9: combine MOST-RESTRICTIVE with CSIP, min per axis) and
// records, for THIS tick, which axis (if any) is bound SOLELY by the
// OpenADR-adopted cap — read back via OpenADRBoundAxis. Caller holds r.mu
// (called from ReadSystemState, after r.lastCSIP's expiry has already been
// resolved for this tick — see state.go).
//
// The most-restrictive MERGE needs no new arithmetic here: grid.
// Import/ExportLimitW start at NaN (NewGridState) and — before this slice —
// nothing in cmd/hub ever wrote them; internal/orchestrator's
// deriveGridConstraints (optimizer.go, UNCHANGED by this task) already does
// nanMin(grid.ExportLimitW, csip.Base.OpModExpLimW) whenever a CSIP
// export/import control is active. Simply assigning the OpenADR value to
// grid.ExportLimitW/ImportLimitW here means that existing nanMin computes
// min(OpenADR, CSIP) for free — the D9 most-restrictive rule — with zero
// orchestrator changes.
func (r *MQTTSystemReader) mergeOpenADRLimitsLocked(grid *orchestrator.GridState, now time.Time) {
	r.openADRBindImport = false
	r.openADRBindExport = false

	if !r.openADRLimits.have {
		return
	}
	if r.openADRLimits.validUntil != 0 && now.Unix() >= r.openADRLimits.validUntil {
		// Expired: an OpenADR limit names its own end time up front (unlike
		// CSIP's debounced ValidUntil), so there is no clock-jitter concern
		// to ride out — drop outright. Edge-logged (never per-tick).
		if !r.openADRLimitsExpiredLogged {
			r.openADRLimitsExpiredLogged = true
			slog.Info("[hub] OpenADR capacity limit past valid_until — dropped", "valid_until", r.openADRLimits.validUntil)
		}
		return
	}
	r.openADRLimitsExpiredLogged = false

	csip := r.lastCSIP // nil-safe: dereferenced only behind csip != nil below

	if !math.IsNaN(r.openADRLimits.impLimW) {
		grid.ImportLimitW = r.openADRLimits.impLimW
		csipImp := math.NaN()
		if csip != nil && csip.ImpLimW != nil {
			csipImp = *csip.ImpLimW
		}
		r.openADRBindImport = math.IsNaN(csipImp) || r.openADRLimits.impLimW < csipImp-openADRBindEps
	}
	if !math.IsNaN(r.openADRLimits.expLimW) {
		grid.ExportLimitW = r.openADRLimits.expLimW
		csipExp := math.NaN()
		if csip != nil && csip.ExpLimW != nil {
			csipExp = *csip.ExpLimW
		}
		r.openADRBindExport = math.IsNaN(csipExp) || r.openADRLimits.expLimW < csipExp-openADRBindEps
	}
}

// OpenADRBoundAxis reports whether limitType's cap this tick is bound SOLELY
// by the OpenADR-adopted capacity limit — CSIP carries no cap on that axis at
// all, or a strictly looser one (see mergeOpenADRLimitsLocked). Only
// "import"/"export" are ever OpenADR-attributable: CSIP-only axes
// ("generation"/"generation-aus"/"load-aus", opModMaxLimW/GenLimW/LoadLimW —
// OpenADRLimits carries neither) and the reconciler's device-level "device"
// evidence always return false.
//
// This is the ONLY seam main.go's planObserver uses to implement D9's last
// bullet ("an OpenADR-only bind produces an OpenADR opt-out/report, never a
// 2030.5 Response") — it deliberately covers only the optimizer's
// meter-level Plan.Breach path, NOT the reconciler's device-level
// NonConverged evidence: a device's commanded ceiling is stamped with
// state.CSIPControl.MRID regardless of which axis (CSIP's or OpenADR's) was
// actually binding when the command was authored (optimizer.go's
// post-hoc MRID stamp), and that provenance does not survive the
// desired-doc → reconciler → ReconcileReport round trip for this slice to
// re-derive. See this task's report for the deviation this leaves open.
//
// Thread-safe (RLock): called from main.go's planObserver, a different
// goroutine than ReadSystemState's control loop.
func (r *MQTTSystemReader) OpenADRBoundAxis(limitType string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	switch limitType {
	case "import":
		return r.openADRBindImport
	case "export":
		return r.openADRBindExport
	default:
		return false
	}
}
