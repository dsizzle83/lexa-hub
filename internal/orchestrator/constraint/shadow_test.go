package constraint

import (
	"math"
	"reflect"
	"testing"
	"time"

	"lexa-hub/internal/orchestrator"
)

// stubOptimizer returns a fixed plan and records how many times it was called.
type stubOptimizer struct {
	plan  orchestrator.Plan
	calls int
}

func (s *stubOptimizer) Optimize(orchestrator.SystemState) orchestrator.Plan {
	s.calls++
	return s.plan
}

// safetyStub also implements SafetyEvaluator, recording delegation.
type safetyStub struct {
	stubOptimizer
	safetyCalls int
	safetyPlan  orchestrator.Plan
}

func (s *safetyStub) EvaluateSafety(orchestrator.SystemState) orchestrator.Plan {
	s.safetyCalls++
	return s.safetyPlan
}

// collector is a test sink; it appends every emitted divergence.
type collector struct {
	recs []Divergence
}

func (c *collector) sink(d Divergence) { c.recs = append(c.recs, d) }

func boolPtr(b bool) *bool { return &b }

// fixedClock is an injectable, advanceable clock.
type fixedClock struct{ t time.Time }

func (f *fixedClock) now() time.Time      { return f.t }
func (f *fixedClock) add(d time.Duration) { f.t = f.t.Add(d) }

func newFixedClock() *fixedClock {
	return &fixedClock{t: time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)}
}

// ── passthrough / safety ──────────────────────────────────────────────────────

func TestWrapper_ReturnsLegacyPlanUnmodified(t *testing.T) {
	legacyPlan := orchestrator.Plan{
		Timestamp:       time.Unix(1000, 0),
		BatteryCommands: []orchestrator.BatteryCommand{{Name: "b0", SetpointW: 3000}},
		SolarCommands:   []orchestrator.SolarCommand{{Name: "s0", CurtailToW: 2000}},
	}
	// Candidate wildly disagrees; it must NOT leak into the returned plan.
	candPlan := orchestrator.Plan{
		Timestamp:       time.Unix(1000, 0),
		BatteryCommands: []orchestrator.BatteryCommand{{Name: "b0", SetpointW: -5000}},
	}
	legacy := &stubOptimizer{plan: legacyPlan}
	cand := &stubOptimizer{plan: candPlan}
	w := Wrap(legacy, cand, Options{})

	got := w.Optimize(orchestrator.SystemState{})
	if !reflect.DeepEqual(got, legacyPlan) {
		t.Fatalf("returned plan = %+v, want the legacy plan %+v", got, legacyPlan)
	}
	if legacy.calls != 1 || cand.calls != 1 {
		t.Fatalf("both optimizers must run once per tick: legacy=%d candidate=%d", legacy.calls, cand.calls)
	}
}

func TestWrapper_EvaluateSafetyReturnsLegacyObservesCandidate(t *testing.T) {
	// WS-5.3: the candidate's Tier-1 safety path IS consulted (observe-only,
	// shadow-diffed into safetyCount) but the RETURNED plan is always the
	// legacy one — the cascade stays the sole author of protective
	// disconnects until the flip. (Supersedes the pre-WS-5.3 invariant that
	// the candidate was never consulted on this path.)
	legacy := &safetyStub{safetyPlan: orchestrator.Plan{Safety: true, BatteryCommands: []orchestrator.BatteryCommand{
		{Name: "b0", SetpointW: 0, Connect: boolPtr(true)},
	}}}
	cand := &safetyStub{safetyPlan: orchestrator.Plan{Safety: true, BatteryCommands: []orchestrator.BatteryCommand{
		{Name: "b0", SetpointW: 0, Connect: boolPtr(false)}, // exact-diff axis
	}}}
	w := Wrap(legacy, cand, Options{Now: newFixedClock().now, OnDiverge: (&collector{}).sink})

	// The engine type-asserts optimizer.(SafetyEvaluator); prove the wrapper passes.
	se, ok := orchestrator.Optimizer(w).(orchestrator.SafetyEvaluator)
	if !ok {
		t.Fatal("Wrapper must implement orchestrator.SafetyEvaluator")
	}
	plan := se.EvaluateSafety(orchestrator.SystemState{})
	if legacy.safetyCalls != 1 || cand.safetyCalls != 1 {
		t.Fatalf("calls legacy=%d cand=%d, want 1/1", legacy.safetyCalls, cand.safetyCalls)
	}
	if !plan.Safety || len(plan.BatteryCommands) != 1 || *plan.BatteryCommands[0].Connect != true {
		t.Fatalf("legacy safety plan not returned unmodified: %+v", plan)
	}
	if w.SafetyDivergences() != 1 {
		t.Fatalf("safety divergence count = %d, want 1", w.SafetyDivergences())
	}
	if w.Divergences() != 0 {
		t.Fatalf("slow-path count must stay 0, got %d", w.Divergences())
	}
	ac := w.AxisDivergences()
	if ac["safety:"+AxisConnect.String()] != 1 {
		t.Fatalf("want safety-prefixed connect axis tally, got %v", ac)
	}
}

// panicOptimizer panics on every Optimize/EvaluateSafety call after recording it.
type panicOptimizer struct {
	calls       int
	safetyCalls int
}

func (p *panicOptimizer) Optimize(orchestrator.SystemState) orchestrator.Plan {
	p.calls++
	panic("candidate boom")
}
func (p *panicOptimizer) EvaluateSafety(orchestrator.SystemState) orchestrator.Plan {
	p.safetyCalls++
	panic("candidate safety boom")
}

func TestWrapper_PanicLatchDisablesCandidate(t *testing.T) {
	// WS-5.1: a candidate panic never escapes to the control loop; the first
	// panic latches the shadow OFF for the process lifetime.
	legacy := &safetyStub{
		stubOptimizer: stubOptimizer{plan: orchestrator.Plan{BatteryCommands: []orchestrator.BatteryCommand{{Name: "b0", SetpointW: 500}}}},
		safetyPlan:    orchestrator.Plan{Safety: true},
	}
	cand := &panicOptimizer{}
	var gotPanic any
	w := Wrap(legacy, cand, Options{Now: newFixedClock().now,
		OnPanic: func(r any, _ []byte) { gotPanic = r }})

	plan := w.Optimize(orchestrator.SystemState{}) // must not panic
	if len(plan.BatteryCommands) != 1 || plan.BatteryCommands[0].SetpointW != 500 {
		t.Fatalf("legacy plan not returned after candidate panic: %+v", plan)
	}
	if gotPanic == nil || w.Panics() != 1 || !w.Latched() {
		t.Fatalf("panic not latched: onPanic=%v panics=%d latched=%v", gotPanic, w.Panics(), w.Latched())
	}
	// Latched: candidate must not be consulted again on either path.
	w.Optimize(orchestrator.SystemState{})
	w.EvaluateSafety(orchestrator.SystemState{})
	if cand.calls != 1 || cand.safetyCalls != 0 {
		t.Fatalf("latched candidate consulted again: calls=%d safetyCalls=%d", cand.calls, cand.safetyCalls)
	}
	if w.Panics() != 1 {
		t.Fatalf("panics grew after latch: %d", w.Panics())
	}
}

func TestWrapper_SafetyPanicLatches(t *testing.T) {
	legacy := &safetyStub{safetyPlan: orchestrator.Plan{Safety: true, BatteryCommands: []orchestrator.BatteryCommand{{Name: "b0"}}}}
	cand := &panicOptimizer{}
	w := Wrap(legacy, cand, Options{Now: newFixedClock().now})
	plan := w.EvaluateSafety(orchestrator.SystemState{})
	if !plan.Safety || len(plan.BatteryCommands) != 1 {
		t.Fatalf("legacy safety plan not returned after candidate safety panic: %+v", plan)
	}
	if !w.Latched() || w.Panics() != 1 {
		t.Fatalf("safety panic did not latch: latched=%v panics=%d", w.Latched(), w.Panics())
	}
}

func TestWrapper_AxisCountsSlowPath(t *testing.T) {
	legacy := &stubOptimizer{plan: orchestrator.Plan{BatteryCommands: []orchestrator.BatteryCommand{
		{Name: "b0", SetpointW: 1000, Connect: boolPtr(true)},
	}}}
	cand := &stubOptimizer{plan: orchestrator.Plan{BatteryCommands: []orchestrator.BatteryCommand{
		{Name: "b0", SetpointW: 1100, Connect: boolPtr(false)},
	}}}
	w := Wrap(legacy, cand, Options{Now: newFixedClock().now, OnDiverge: (&collector{}).sink})
	w.Optimize(orchestrator.SystemState{})
	ac := w.AxisDivergences()
	if ac[AxisConnect.String()] != 1 {
		t.Fatalf("want unprefixed connect tally 1, got %v", ac)
	}
}

func TestWrapper_EvaluateSafetyInertWhenLegacyNotSafety(t *testing.T) {
	w := Wrap(&stubOptimizer{}, &stubOptimizer{}, Options{})
	plan := w.EvaluateSafety(orchestrator.SystemState{Timestamp: time.Unix(5, 0)})
	if !plan.Safety || len(plan.BatteryCommands) != 0 {
		t.Fatalf("want inert safety plan, got %+v", plan)
	}
}

// ── empty stack is inert ──────────────────────────────────────────────────────

func TestWrapper_EmptyCandidateNeverDiverges(t *testing.T) {
	// Legacy commands aggressively; candidate (empty Stack) has no opinion.
	legacy := &stubOptimizer{plan: orchestrator.Plan{
		BatteryCommands: []orchestrator.BatteryCommand{{Name: "b0", SetpointW: 4000, Connect: boolPtr(false)}},
		SolarCommands:   []orchestrator.SolarCommand{{Name: "s0", CurtailToW: 1000}},
		EVSECommands:    []orchestrator.EVSECommand{{StationID: "cs1", MaxCurrentA: 16}},
		Breach:          &orchestrator.ComplianceBreach{LimitType: "export"},
	}}
	stack := NewStack(Plant{}, 3*time.Second) // zero constraints
	col := &collector{}
	clk := newFixedClock()
	w := Wrap(legacy, stack, Options{Now: clk.now, OnDiverge: col.sink})

	for i := 0; i < 50; i++ {
		w.Optimize(orchestrator.SystemState{})
	}
	if got := w.Divergences(); got != 0 {
		t.Fatalf("empty candidate stack diverged %d times, want 0", got)
	}
	if len(col.recs) != 0 {
		t.Fatalf("empty candidate stack emitted %d records, want 0", len(col.recs))
	}
}

// ── watt tolerance band edges ─────────────────────────────────────────────────

func TestWrapper_SolarToleranceEdges(t *testing.T) {
	tol := DefaultTolerances() // WattAbs=150, WattFrac=0.05
	cases := []struct {
		name          string
		legacy, cand  float64
		wantDivergent bool
	}{
		{"exact", 2000, 2000, false},
		{"within-abs", 2000, 2140, false},              // 140 < 150
		{"at-abs-edge", 2000, 2150, false},             // 150 == band
		{"just-over-abs", 2000, 2151, true},            // 151 > 150
		{"frac-dominates-within", 10000, 10400, false}, // band=max(150,500)=500; 400<500
		{"frac-dominates-over", 10000, 10600, true},    // 600>500
		{"legacy-none-cand-curtails", math.NaN(), 3000, true},
		{"both-none", math.NaN(), math.NaN(), false}, // candidate NaN = no opinion
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			legacy := &stubOptimizer{plan: solarPlan("s0", tc.legacy)}
			cand := &stubOptimizer{plan: solarPlan("s0", tc.cand)}
			col := &collector{}
			w := Wrap(legacy, cand, Options{Tolerances: tol, Now: newFixedClock().now, OnDiverge: col.sink})
			w.Optimize(orchestrator.SystemState{})
			gotDiv := w.Divergences() == 1
			if gotDiv != tc.wantDivergent {
				t.Fatalf("divergent=%v, want %v (recs=%d)", gotDiv, tc.wantDivergent, len(col.recs))
			}
		})
	}
}

func solarPlan(name string, ceil float64) orchestrator.Plan {
	if math.IsNaN(ceil) {
		return orchestrator.Plan{} // no solar command at all
	}
	return orchestrator.Plan{SolarCommands: []orchestrator.SolarCommand{{Name: name, CurtailToW: ceil}}}
}

func TestWrapper_BatterySetpointAndConnect(t *testing.T) {
	col := &collector{}
	legacy := &stubOptimizer{plan: orchestrator.Plan{BatteryCommands: []orchestrator.BatteryCommand{
		{Name: "b0", SetpointW: 1000, Connect: boolPtr(true)},
	}}}
	// candidate: setpoint within band (agree), connect disagrees (exact) → 1 axis.
	cand := &stubOptimizer{plan: orchestrator.Plan{BatteryCommands: []orchestrator.BatteryCommand{
		{Name: "b0", SetpointW: 1100, Connect: boolPtr(false)},
	}}}
	w := Wrap(legacy, cand, Options{Now: newFixedClock().now, OnDiverge: col.sink})
	w.Optimize(orchestrator.SystemState{})
	if w.Divergences() != 1 || len(col.recs) != 1 {
		t.Fatalf("want 1 divergent tick, got count=%d recs=%d", w.Divergences(), len(col.recs))
	}
	axes := col.recs[0].Axes
	if len(axes) != 1 || axes[0].Axis != AxisConnect.String() {
		t.Fatalf("want a single connect divergence, got %+v", axes)
	}
	if axes[0].Legacy != "connect" || axes[0].Candidate != "disconnect" {
		t.Fatalf("connect axis mis-formatted: %+v", axes[0])
	}
}

func TestWrapper_EVSECurrentTolerance(t *testing.T) {
	cases := []struct {
		name         string
		legacy, cand float64
		legacyAbsent bool
		want         bool
	}{
		{"within", 16, 16.4, false, false},   // 0.4 ≤ 0.5
		{"edge", 16, 16.5, false, false},     // 0.5 == band
		{"over", 16, 16.6, false, true},      // 0.6 > 0.5
		{"legacy-absent", 0, 10, true, true}, // candidate limits where legacy doesn't
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			legPlan := orchestrator.Plan{}
			if !tc.legacyAbsent {
				legPlan.EVSECommands = []orchestrator.EVSECommand{{StationID: "cs1", MaxCurrentA: tc.legacy}}
			}
			candPlan := orchestrator.Plan{EVSECommands: []orchestrator.EVSECommand{{StationID: "cs1", MaxCurrentA: tc.cand}}}
			w := Wrap(&stubOptimizer{plan: legPlan}, &stubOptimizer{plan: candPlan},
				Options{Now: newFixedClock().now, OnDiverge: (&collector{}).sink})
			w.Optimize(orchestrator.SystemState{})
			if got := w.Divergences() == 1; got != tc.want {
				t.Fatalf("divergent=%v want %v", got, tc.want)
			}
		})
	}
}

// ── breach presence debounce ──────────────────────────────────────────────────

func TestWrapper_BreachOnsetDebounce(t *testing.T) {
	// Candidate breaches, legacy does not: must NOT diverge until the mismatch
	// outlasts BreachDebounceTicks (2), i.e. on the 3rd consecutive tick.
	legacy := &stubOptimizer{plan: orchestrator.Plan{}}
	cand := &stubOptimizer{plan: orchestrator.Plan{Breach: &orchestrator.ComplianceBreach{LimitType: "export"}}}
	w := Wrap(legacy, cand, Options{Now: newFixedClock().now, OnDiverge: (&collector{}).sink})

	for i := 1; i <= 2; i++ {
		w.Optimize(orchestrator.SystemState{})
		if w.Divergences() != 0 {
			t.Fatalf("tick %d: diverged too early (count=%d)", i, w.Divergences())
		}
	}
	w.Optimize(orchestrator.SystemState{}) // 3rd tick → real divergence
	if w.Divergences() != 1 {
		t.Fatalf("want divergence on 3rd tick, count=%d", w.Divergences())
	}
}

func TestWrapper_BreachTransientResets(t *testing.T) {
	// A one-tick candidate-only breach (legacy catches up next tick) must be
	// swallowed by the debounce and reset — zero divergence.
	legacy := &stubOptimizer{}
	cand := &stubOptimizer{}
	w := Wrap(legacy, cand, Options{Now: newFixedClock().now, OnDiverge: (&collector{}).sink})

	cand.plan = orchestrator.Plan{Breach: &orchestrator.ComplianceBreach{LimitType: "export"}}
	w.Optimize(orchestrator.SystemState{}) // mismatch tick 1 (≤2, no diverge)
	// legacy now agrees (both breach same type) → reset.
	legacy.plan = orchestrator.Plan{Breach: &orchestrator.ComplianceBreach{LimitType: "export"}}
	w.Optimize(orchestrator.SystemState{})
	if w.Divergences() != 0 {
		t.Fatalf("transient breach onset must not diverge, count=%d", w.Divergences())
	}
}

func TestWrapper_BreachTypeMismatchImmediate(t *testing.T) {
	// Both breach the same tick but disagree on LimitType → immediate divergence.
	legacy := &stubOptimizer{plan: orchestrator.Plan{Breach: &orchestrator.ComplianceBreach{LimitType: "import"}}}
	cand := &stubOptimizer{plan: orchestrator.Plan{Breach: &orchestrator.ComplianceBreach{LimitType: "export"}}}
	w := Wrap(legacy, cand, Options{Now: newFixedClock().now, OnDiverge: (&collector{}).sink})
	w.Optimize(orchestrator.SystemState{})
	if w.Divergences() != 1 {
		t.Fatalf("type mismatch must diverge immediately, count=%d", w.Divergences())
	}
}

// ── rate limiter ──────────────────────────────────────────────────────────────

func TestWrapper_RateLimitPerSignature(t *testing.T) {
	clk := newFixedClock()
	col := &collector{}
	// Persistent identical solar divergence every tick.
	legacy := &stubOptimizer{plan: solarPlan("s0", 5000)}
	cand := &stubOptimizer{plan: solarPlan("s0", 1000)}
	w := Wrap(legacy, cand, Options{Now: clk.now, OnDiverge: col.sink, RateLimit: time.Minute})

	for i := 0; i < 20; i++ {
		w.Optimize(orchestrator.SystemState{})
		clk.add(2 * time.Second) // 20 ticks span 40 s < 1 min
	}
	if w.Divergences() != 20 {
		t.Fatalf("count must tally every divergent tick, got %d", w.Divergences())
	}
	if len(col.recs) != 1 {
		t.Fatalf("rate limit: want 1 emitted record in <1min, got %d", len(col.recs))
	}
	if col.recs[0].Suppressed != 0 {
		t.Fatalf("first record suppressed=%d, want 0", col.recs[0].Suppressed)
	}

	// Cross the window → second emission carries the suppressed backlog.
	clk.add(time.Minute)
	w.Optimize(orchestrator.SystemState{})
	if len(col.recs) != 2 {
		t.Fatalf("want 2 records after window, got %d", len(col.recs))
	}
	// 19 suppressed ticks between the two emissions.
	if col.recs[1].Suppressed != 19 {
		t.Fatalf("suppressed backlog = %d, want 19", col.recs[1].Suppressed)
	}
}

func TestWrapper_DistinctSignaturesEmitIndependently(t *testing.T) {
	clk := newFixedClock()
	col := &collector{}
	legacy := &stubOptimizer{}
	cand := &stubOptimizer{}
	w := Wrap(legacy, cand, Options{Now: clk.now, OnDiverge: col.sink, RateLimit: time.Minute})

	// Tick 1: solar diverges.
	legacy.plan = solarPlan("s0", 5000)
	cand.plan = solarPlan("s0", 1000)
	w.Optimize(orchestrator.SystemState{})
	// Tick 2: a DIFFERENT signature (evse) diverges — must emit despite rate window.
	legacy.plan = orchestrator.Plan{}
	cand.plan = orchestrator.Plan{EVSECommands: []orchestrator.EVSECommand{{StationID: "cs1", MaxCurrentA: 8}}}
	w.Optimize(orchestrator.SystemState{})

	if len(col.recs) != 2 {
		t.Fatalf("distinct signatures must each emit, got %d records", len(col.recs))
	}
}

// ── record content ────────────────────────────────────────────────────────────

func TestWrapper_DivergenceRecordSnapshotNaNSafe(t *testing.T) {
	col := &collector{}
	legacy := &stubOptimizer{plan: solarPlan("s0", math.NaN())} // no cap
	cand := &stubOptimizer{plan: solarPlan("s0", 1500)}
	w := Wrap(legacy, cand, Options{Now: newFixedClock().now, OnDiverge: col.sink})

	state := orchestrator.SystemState{
		Grid: orchestrator.GridState{
			NetW: 4200, ExportLimitW: 3000,
			ImportLimitW: math.NaN(), MaxLimitW: math.NaN(), FrequencyHz: math.NaN(),
		},
		Batteries:   []orchestrator.BatteryState{{Name: "b0", PowerW: -500, SOC: math.NaN(), Connected: true}},
		Solar:       []orchestrator.SolarState{{Name: "s0", PowerW: 4200, Connected: true}},
		CSIPControl: &orchestrator.CSIPControlState{Source: "event", MRID: "abc"},
	}
	w.Optimize(state)
	if len(col.recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(col.recs))
	}
	snap := col.recs[0].State
	if snap.GridNetW == nil || *snap.GridNetW != 4200 {
		t.Fatalf("grid net W snapshot wrong: %+v", snap.GridNetW)
	}
	if snap.ImportLimitW != nil || snap.MaxLimitW != nil {
		t.Fatalf("NaN limits must serialise as nil, got import=%v max=%v", snap.ImportLimitW, snap.MaxLimitW)
	}
	if snap.ExportLimitW == nil || *snap.ExportLimitW != 3000 {
		t.Fatalf("export limit snapshot wrong: %+v", snap.ExportLimitW)
	}
	if len(snap.Batteries) != 1 || snap.Batteries[0].SOC != nil {
		t.Fatalf("battery snapshot NaN SOC must be nil: %+v", snap.Batteries)
	}
	if snap.CSIP != "event(abc)" {
		t.Fatalf("csip snapshot = %q", snap.CSIP)
	}
	if col.recs[0].Total != 1 {
		t.Fatalf("record Total = %d, want 1", col.recs[0].Total)
	}
}

func TestWrapper_CandidateSessionNamesRecorded(t *testing.T) {
	col := &collector{}
	legacy := &stubOptimizer{plan: solarPlan("s0", 5000)}
	// A real Stack with a stub constraint so SessionNames is non-empty AND the
	// candidate actually emits a diverging solar ceiling.
	stack := NewStack(Plant{}, 3*time.Second, ceilConstraint{name: "export-cap", ceil: 1000})
	w := Wrap(legacy, stack, Options{Now: newFixedClock().now, OnDiverge: col.sink})
	w.Optimize(orchestrator.SystemState{
		Solar: []orchestrator.SolarState{{Name: "s0", Connected: true, PowerW: 5000, MaxW: 8000}},
	})
	if len(col.recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(col.recs))
	}
	if got := col.recs[0].CandidateSessions; len(got) != 1 || got[0] != "export-cap" {
		t.Fatalf("candidate sessions = %v, want [export-cap]", got)
	}
}

// ceilConstraint is a minimal test constraint that pins one solar ceiling.
type ceilConstraint struct {
	name string
	ceil float64
}

func (c ceilConstraint) Name() string { return c.name }
func (c ceilConstraint) Tier() Tier   { return TierCompliance }
func (c ceilConstraint) Evaluate(in Input, _ *Session) ([]Demand, *orchestrator.ComplianceBreach) {
	var out []Demand
	for _, s := range in.State.Solar {
		out = append(out, CeilingDemand(s.Name, AxisSolarCeilingW, c.ceil, TierCompliance, c.name))
	}
	return out, nil
}

// ── defaults ──────────────────────────────────────────────────────────────────

func TestWrap_DefaultsApplied(t *testing.T) {
	w := Wrap(&stubOptimizer{}, &stubOptimizer{}, Options{})
	if w.tol != DefaultTolerances() {
		t.Fatalf("default tolerances not applied: %+v", w.tol)
	}
	if w.rateLimit != time.Minute {
		t.Fatalf("default rate limit = %v, want 1m", w.rateLimit)
	}
	if w.now == nil {
		t.Fatal("default clock not applied")
	}
}

func TestWrapper_NoSinkStillCounts(t *testing.T) {
	legacy := &stubOptimizer{plan: solarPlan("s0", 5000)}
	cand := &stubOptimizer{plan: solarPlan("s0", 1000)}
	w := Wrap(legacy, cand, Options{Now: newFixedClock().now}) // OnDiverge nil
	w.Optimize(orchestrator.SystemState{})
	if w.Divergences() != 1 {
		t.Fatalf("count must advance even with nil sink, got %d", w.Divergences())
	}
}
