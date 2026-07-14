package main

// WP-15 hub-adoption slice tests (openadr_adopt.go, D9): price precedence
// (CSIP > OpenADR > app/cloud tariff intent), the most-restrictive limits
// merge + per-axis CannotComply attribution, valid_until expiry, and the
// price-staleness skip.

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/orchestrator"
)

// ── pricesFromOpenADR conversion ─────────────────────────────────────────────

func TestPricesFromOpenADR_NoUsableSeries(t *testing.T) {
	imp, exp := pricesFromOpenADR(bus.OpenADRPrices{}, time.Now())
	if imp != nil || exp != nil {
		t.Errorf("empty Series: got imp=%v exp=%v, want nil,nil", imp, exp)
	}
	// A document with ONLY a GHG/ALERT_* series (no PRICE, no EXPORT_PRICE)
	// carries nothing this planner-facing conversion understands.
	imp, exp = pricesFromOpenADR(bus.OpenADRPrices{Series: []bus.OpenADRPriceSeries{
		{Kind: "GHG", Intervals: []bus.OpenADRPriceInterval{{StartTs: time.Now().Unix(), DurationS: 3600, Value: 42}}},
	}}, time.Now())
	if imp != nil || exp != nil {
		t.Errorf("GHG-only series: got imp=%v exp=%v, want nil,nil", imp, exp)
	}
}

func TestPricesFromOpenADR_PriceSeriesFillsWindowGapsFallback(t *testing.T) {
	now := time.Now()
	ws := now.Unix() - (now.Unix() % openADRPlanStepSec)
	msg := bus.OpenADRPrices{Series: []bus.OpenADRPriceSeries{
		{Kind: "PRICE", Intervals: []bus.OpenADRPriceInterval{
			{StartTs: ws, DurationS: 3600, Value: 0.42},
		}},
	}}
	imp, exp := pricesFromOpenADR(msg, now)
	if imp == nil {
		t.Fatal("expected non-nil import prices")
	}
	if exp != nil {
		t.Errorf("no EXPORT_PRICE series present: got exp=%v, want nil", exp)
	}
	fallback := orchestrator.DefaultTOUCostModel()
	for i, v := range imp {
		stepT := ws + int64(i)*openADRPlanStepSec
		inWindow := stepT >= ws && stepT < ws+3600
		want := fallback.CurrentRate(time.Unix(stepT, 0).Local())
		if inWindow {
			want = 0.42
		}
		if v != want {
			t.Fatalf("step %d: got %v, want %v (inWindow=%v)", i, v, want, inWindow)
		}
	}
}

func TestPricesFromOpenADR_ExportPriceZeroFilledOutsideIntervals(t *testing.T) {
	now := time.Now()
	ws := now.Unix() - (now.Unix() % openADRPlanStepSec)
	msg := bus.OpenADRPrices{Series: []bus.OpenADRPriceSeries{
		{Kind: "PRICE", Intervals: []bus.OpenADRPriceInterval{{StartTs: ws, DurationS: 0}}}, // unbounded, value 0
		{Kind: "EXPORT_PRICE", Intervals: []bus.OpenADRPriceInterval{
			{StartTs: ws, DurationS: openADRPlanStepSec, Value: 0.10}, // just the first step
		}},
	}}
	imp, exp := pricesFromOpenADR(msg, now)
	if imp == nil {
		t.Fatal("expected non-nil import prices")
	}
	if exp == nil {
		t.Fatal("expected non-nil export prices (EXPORT_PRICE series present)")
	}
	if exp[0] != 0.10 {
		t.Errorf("exp[0] = %v, want 0.10", exp[0])
	}
	for i := 1; i < len(exp); i++ {
		if exp[i] != 0 {
			t.Fatalf("exp[%d] = %v, want 0 (no remuneration data outside the declared interval)", i, exp[i])
		}
	}
}

func TestPricesFromOpenADR_UnboundedIntervalFillsToEnd(t *testing.T) {
	now := time.Now()
	ws := now.Unix() - (now.Unix() % openADRPlanStepSec)
	msg := bus.OpenADRPrices{Series: []bus.OpenADRPriceSeries{
		{Kind: "PRICE", Intervals: []bus.OpenADRPriceInterval{
			{StartTs: ws, DurationS: 0, Value: 0.99}, // "P9999Y" infinity sentinel, DurationS<=0
		}},
	}}
	imp, _ := pricesFromOpenADR(msg, now)
	for i, v := range imp {
		if v != 0.99 {
			t.Fatalf("step %d = %v, want 0.99 (unbounded interval covers every step)", i, v)
		}
	}
}

// ── D9 price precedence (openADRAdopter) ────────────────────────────────────

func openADRPriceMsg(t time.Time, value float64) bus.OpenADRPrices {
	ws := t.Unix() - (t.Unix() % openADRPlanStepSec)
	return bus.OpenADRPrices{
		Series: []bus.OpenADRPriceSeries{
			{Kind: "PRICE", Intervals: []bus.OpenADRPriceInterval{{StartTs: ws, DurationS: 0, Value: value}}},
		},
		Ts: t.Unix(),
	}
}

// TestOpenADRAdopter_PricePrecedence pins the three-tier D9 precedence table
// at the AdoptPrices/MarkCSIPPriceSeen boundary: CSIP-present ⇒ OpenADR
// ignored; CSIP-absent ⇒ OpenADR adopted; both-absent (no usable series) ⇒
// AdoptPrices reports not-ok, so main.go never calls Engine.SetPrices and the
// engine's own pre-existing nil-importPrices fallback keeps using whatever
// app/cloud tariff intent supplied via SetFallbackTOU (untouched by this
// slice, already covered by tariff_test.go/intent_test.go).
func TestOpenADRAdopter_PricePrecedence(t *testing.T) {
	now := time.Now()

	t.Run("CSIP present -> OpenADR price ignored", func(t *testing.T) {
		a := newOpenADRAdopter(time.Hour)
		a.MarkCSIPPriceSeen()
		imp, exp, ok := a.AdoptPrices(openADRPriceMsg(now, 0.5), now)
		if ok || imp != nil || exp != nil {
			t.Fatalf("AdoptPrices = (%v,%v,%v), want (nil,nil,false) once CSIP has been seen", imp, exp, ok)
		}
		// Stays permanently ignored across further calls (sticky, "utility
		// pricing keeps winning" — tariff.go's doc).
		imp, exp, ok = a.AdoptPrices(openADRPriceMsg(now, 0.7), now)
		if ok || imp != nil || exp != nil {
			t.Fatalf("second AdoptPrices = (%v,%v,%v), want still (nil,nil,false)", imp, exp, ok)
		}
	})

	t.Run("CSIP absent -> OpenADR price adopted", func(t *testing.T) {
		a := newOpenADRAdopter(time.Hour)
		imp, _, ok := a.AdoptPrices(openADRPriceMsg(now, 0.5), now)
		if !ok || imp == nil {
			t.Fatalf("AdoptPrices = (%v,_,%v), want (non-nil,true)", imp, ok)
		}
		if imp[0] != 0.5 {
			t.Errorf("imp[0] = %v, want 0.5", imp[0])
		}
	})

	t.Run("both absent -> not adopted (intent fallback stays in force)", func(t *testing.T) {
		a := newOpenADRAdopter(time.Hour)
		imp, exp, ok := a.AdoptPrices(bus.OpenADRPrices{Ts: now.Unix()}, now)
		if ok || imp != nil || exp != nil {
			t.Fatalf("AdoptPrices with no series = (%v,%v,%v), want (nil,nil,false)", imp, exp, ok)
		}
	})
}

// TestOpenADRAdopter_StalenessSkip pins the TASK-042-pattern staleness gate:
// a document whose Ts is older than the configured max age is skipped (WARN,
// never fabricated); 0 disables the check entirely (matches
// MQTTSystemReader.retainedAdoptionMaxAge's zero-disables convention).
func TestOpenADRAdopter_StalenessSkip(t *testing.T) {
	now := time.Now()
	old := now.Add(-2 * time.Hour)

	t.Run("stale doc skipped", func(t *testing.T) {
		a := newOpenADRAdopter(time.Hour)
		imp, _, ok := a.AdoptPrices(openADRPriceMsg(old, 0.5), now)
		if ok || imp != nil {
			t.Fatalf("AdoptPrices(stale) = (%v,_,%v), want (nil,false)", imp, ok)
		}
	})

	t.Run("fresh doc adopted", func(t *testing.T) {
		a := newOpenADRAdopter(time.Hour)
		imp, _, ok := a.AdoptPrices(openADRPriceMsg(now, 0.5), now)
		if !ok || imp == nil {
			t.Fatalf("AdoptPrices(fresh) = (%v,_,%v), want (non-nil,true)", imp, ok)
		}
	})

	t.Run("zero max age disables the staleness check", func(t *testing.T) {
		a := newOpenADRAdopter(0)
		imp, _, ok := a.AdoptPrices(openADRPriceMsg(old, 0.5), now)
		if !ok || imp == nil {
			t.Fatalf("AdoptPrices(stale, maxAge=0) = (%v,_,%v), want (non-nil,true)", imp, ok)
		}
	})
}

// ── D9 most-restrictive limits merge + attribution ──────────────────────────

func openADRLimitsMsg(imp, exp *float64, validUntil int64) bus.OpenADRLimits {
	return bus.OpenADRLimits{ImpLimW: imp, ExpLimW: exp, ValidUntil: validUntil}
}

func f64(v float64) *float64 { return &v }

// TestMergeOpenADRLimits_OnlyOpenADRPresent: no CSIP control at all (CSIP
// absent) ⇒ the OpenADR cap lands on GridState and is attributed OpenADR-only
// on both axes.
func TestMergeOpenADRLimits_OnlyOpenADRPresent(t *testing.T) {
	r := newMQTTSystemReader(nil, testFastInterval, nil)
	r.onOpenADRLimits("lexa/openadr/limits", openADRLimitsMsg(f64(3000), f64(4000), 0))

	state, err := r.ReadSystemState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Grid.ImportLimitW != 3000 {
		t.Errorf("Grid.ImportLimitW = %v, want 3000", state.Grid.ImportLimitW)
	}
	if state.Grid.ExportLimitW != 4000 {
		t.Errorf("Grid.ExportLimitW = %v, want 4000", state.Grid.ExportLimitW)
	}
	if !r.OpenADRBoundAxis("import") {
		t.Error("import axis should be OpenADR-only bound (CSIP absent)")
	}
	if !r.OpenADRBoundAxis("export") {
		t.Error("export axis should be OpenADR-only bound (CSIP absent)")
	}
	// Never attributable for non-capacity-limit axes.
	if r.OpenADRBoundAxis("generation") || r.OpenADRBoundAxis("device") || r.OpenADRBoundAxis("") {
		t.Error("non import/export LimitType must never be OpenADR-attributable")
	}
}

// TestMergeOpenADRLimits_CSIPTighterWins: CSIP carries a strictly tighter cap
// than OpenADR on both axes ⇒ NOT OpenADR-attributable (CSIP is binding).
// GridState still carries the OpenADR value verbatim (the min() itself is
// internal/orchestrator's deriveGridConstraints' job, exercised there).
func TestMergeOpenADRLimits_CSIPTighterWins(t *testing.T) {
	r := newMQTTSystemReader(nil, testFastInterval, nil)
	impLim, expLim := 2000.0, 2500.0
	r.onCSIPControl("lexa/csip/control", bus.ActiveControl{
		Source: "event", MRID: "csip-evt", ImpLimW: &impLim, ExpLimW: &expLim, Ts: time.Now().Unix(),
	})
	r.onOpenADRLimits("lexa/openadr/limits", openADRLimitsMsg(f64(5000), f64(6000), 0)) // looser than CSIP

	state, err := r.ReadSystemState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Grid.ImportLimitW != 5000 || state.Grid.ExportLimitW != 6000 {
		t.Errorf("Grid limits = (%v,%v), want (5000,6000) (the OpenADR raw value; CSIP's own leg is applied later by deriveGridConstraints)",
			state.Grid.ImportLimitW, state.Grid.ExportLimitW)
	}
	if r.OpenADRBoundAxis("import") {
		t.Error("import: CSIP is strictly tighter, must NOT be OpenADR-attributed")
	}
	if r.OpenADRBoundAxis("export") {
		t.Error("export: CSIP is strictly tighter, must NOT be OpenADR-attributed")
	}
}

// TestMergeOpenADRLimits_OpenADRTighterWins: OpenADR is strictly tighter than
// CSIP on both axes ⇒ OpenADR-only attributed (this is the D9 "an
// OpenADR-only bind" case the CannotComply gate in main.go acts on).
func TestMergeOpenADRLimits_OpenADRTighterWins(t *testing.T) {
	r := newMQTTSystemReader(nil, testFastInterval, nil)
	impLim, expLim := 8000.0, 9000.0
	r.onCSIPControl("lexa/csip/control", bus.ActiveControl{
		Source: "event", MRID: "csip-evt", ImpLimW: &impLim, ExpLimW: &expLim, Ts: time.Now().Unix(),
	})
	r.onOpenADRLimits("lexa/openadr/limits", openADRLimitsMsg(f64(3000), f64(4000), 0)) // tighter than CSIP

	if _, err := r.ReadSystemState(); err != nil {
		t.Fatal(err)
	}
	if !r.OpenADRBoundAxis("import") {
		t.Error("import: OpenADR is strictly tighter, must be OpenADR-attributed")
	}
	if !r.OpenADRBoundAxis("export") {
		t.Error("export: OpenADR is strictly tighter, must be OpenADR-attributed")
	}
}

// TestMergeOpenADRLimits_TieResolvesToCSIP: a tied (within openADRBindEps)
// cap resolves toward CSIP attribution (i.e. NOT gated) — the safe default,
// since failing to report a genuine CSIP-attributable breach is worse than
// one avoidable extra Response.
func TestMergeOpenADRLimits_TieResolvesToCSIP(t *testing.T) {
	r := newMQTTSystemReader(nil, testFastInterval, nil)
	impLim := 3000.0
	r.onCSIPControl("lexa/csip/control", bus.ActiveControl{
		Source: "event", MRID: "csip-evt", ImpLimW: &impLim, Ts: time.Now().Unix(),
	})
	r.onOpenADRLimits("lexa/openadr/limits", openADRLimitsMsg(f64(3000), nil, 0)) // exact tie

	if _, err := r.ReadSystemState(); err != nil {
		t.Fatal(err)
	}
	if r.OpenADRBoundAxis("import") {
		t.Error("a tied cap must resolve to CSIP attribution (not gated)")
	}
}

// TestMergeOpenADRLimits_AxisAbsentStaysNaN: an OpenADR doc that only sets
// one axis leaves the other at NaN / not-attributed.
func TestMergeOpenADRLimits_AxisAbsentStaysNaN(t *testing.T) {
	r := newMQTTSystemReader(nil, testFastInterval, nil)
	r.onOpenADRLimits("lexa/openadr/limits", openADRLimitsMsg(f64(3000), nil, 0))

	state, err := r.ReadSystemState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Grid.ImportLimitW != 3000 {
		t.Errorf("Grid.ImportLimitW = %v, want 3000", state.Grid.ImportLimitW)
	}
	if !math.IsNaN(state.Grid.ExportLimitW) {
		t.Errorf("Grid.ExportLimitW = %v, want NaN (axis absent on the OpenADR doc)", state.Grid.ExportLimitW)
	}
	if r.OpenADRBoundAxis("export") {
		t.Error("export axis absent on the doc: must not be OpenADR-attributed")
	}
}

// TestMergeOpenADRLimits_NoDocIsNoop: never having received an OpenADR
// limits doc must leave GridState exactly as CSIP-only adoption would.
func TestMergeOpenADRLimits_NoDocIsNoop(t *testing.T) {
	r := newMQTTSystemReader(nil, testFastInterval, nil)
	state, err := r.ReadSystemState()
	if err != nil {
		t.Fatal(err)
	}
	if !math.IsNaN(state.Grid.ImportLimitW) || !math.IsNaN(state.Grid.ExportLimitW) {
		t.Errorf("no OpenADR doc ever received: Grid limits = (%v,%v), want (NaN,NaN)",
			state.Grid.ImportLimitW, state.Grid.ExportLimitW)
	}
	if r.OpenADRBoundAxis("import") || r.OpenADRBoundAxis("export") {
		t.Error("no OpenADR doc ever received: neither axis should be OpenADR-attributed")
	}
}

// ── valid_until expiry ───────────────────────────────────────────────────────

// TestMergeOpenADRLimits_ValidUntilExpired: a limit past its declared
// valid_until is dropped outright (no debounce — unlike CSIP's ValidUntil,
// an OpenADR limit names its own end time up front, so there is no
// clock-jitter concern to ride out).
func TestMergeOpenADRLimits_ValidUntilExpired(t *testing.T) {
	r := newMQTTSystemReader(nil, testFastInterval, nil)
	past := time.Now().Add(-time.Minute).Unix()
	r.onOpenADRLimits("lexa/openadr/limits", openADRLimitsMsg(f64(3000), f64(4000), past))

	state, err := r.ReadSystemState()
	if err != nil {
		t.Fatal(err)
	}
	if !math.IsNaN(state.Grid.ImportLimitW) || !math.IsNaN(state.Grid.ExportLimitW) {
		t.Errorf("expired OpenADR limit must be dropped, got (%v,%v)", state.Grid.ImportLimitW, state.Grid.ExportLimitW)
	}
	if r.OpenADRBoundAxis("import") || r.OpenADRBoundAxis("export") {
		t.Error("expired OpenADR limit must not be attributed")
	}
}

// TestMergeOpenADRLimits_ValidUntilZeroIsUnbounded pins ValidUntil==0 as
// "never expires" (matches bus.OpenADRLimits' own doc comment).
func TestMergeOpenADRLimits_ValidUntilZeroIsUnbounded(t *testing.T) {
	r := newMQTTSystemReader(nil, testFastInterval, nil)
	r.onOpenADRLimits("lexa/openadr/limits", openADRLimitsMsg(f64(3000), nil, 0))

	state, err := r.ReadSystemState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Grid.ImportLimitW != 3000 {
		t.Errorf("Grid.ImportLimitW = %v, want 3000 (ValidUntil=0 must never expire)", state.Grid.ImportLimitW)
	}
}

// TestMergeOpenADRLimits_ValidUntilFutureIsAdopted pins the ordinary
// not-yet-expired case.
func TestMergeOpenADRLimits_ValidUntilFutureIsAdopted(t *testing.T) {
	r := newMQTTSystemReader(nil, testFastInterval, nil)
	future := time.Now().Add(time.Hour).Unix()
	r.onOpenADRLimits("lexa/openadr/limits", openADRLimitsMsg(f64(3000), nil, future))

	state, err := r.ReadSystemState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Grid.ImportLimitW != 3000 {
		t.Errorf("Grid.ImportLimitW = %v, want 3000 (not yet expired)", state.Grid.ImportLimitW)
	}
}

// ── config ───────────────────────────────────────────────────────────────────

// TestLoadConfig_OpenADRAdoptDefaults pins openadr_adopt's default-true *bool
// convention and openadr_price_max_age_s's default 3600.
func TestLoadConfig_OpenADRAdoptDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")

	if err := os.WriteFile(path, []byte(`{"devices":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.OpenADRAdoptEnabled() {
		t.Error("openadr_adopt must default to true (key absent)")
	}
	if cfg.OpenADRPriceMaxAgeS != 3600 {
		t.Errorf("openadr_price_max_age_s default = %d, want 3600", cfg.OpenADRPriceMaxAgeS)
	}

	if err := os.WriteFile(path, []byte(`{"devices":[], "openadr_adopt": false, "openadr_price_max_age_s": 120}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenADRAdoptEnabled() {
		t.Error("openadr_adopt: false must be honored, not silently defaulted back to true")
	}
	if cfg.OpenADRPriceMaxAge() != 120*time.Second {
		t.Errorf("OpenADRPriceMaxAge() = %v, want 120s", cfg.OpenADRPriceMaxAge())
	}
}
