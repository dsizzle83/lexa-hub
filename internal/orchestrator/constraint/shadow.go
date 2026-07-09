package constraint

import (
	"fmt"
	"math"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lexa-hub/internal/orchestrator"
)

// Shadow-mode dual-run harness (TASK-059, AD-007, 03 §P5, RSK-03).
//
// Wrapper runs the legacy authoritative optimizer and the candidate constraint
// Stack on the SAME SystemState every economic tick, diffs their FINAL
// per-device outputs under tolerance bands, and hands each divergent tick to an
// injected sink. It ALWAYS returns the legacy plan unmodified — the candidate's
// plan is observed and discarded, never actuated. This is the gate that makes a
// P5 constraint flip validatable: ≥1 week of bench shadow data with ~0 diff
// rate before any constraint goes active.
//
// I/O discipline (05 §1): internal/orchestrator stays I/O-free. Wrapper does no
// logging, marshalling, or clock reads of its own beyond the INJECTED `now`
// func; it emits structured Divergence records to the injected `onDiverge`
// sink, which cmd/hub implements (JSON line + rate-limited log). The rate
// limiter here only GATES the sink (a pure per-signature decision on the
// injected clock); it never writes.
//
// Single-goroutine contract: like DefaultOptimizer and Stack, Wrapper.Optimize
// runs on the Engine's one control goroutine, so its diff/debounce/rate-limit
// state is unsynchronised. Only the divergence COUNTER is atomic, because a
// metrics scrape (cmd/hub's Collect hook) reads it from another goroutine.

// ── What "divergence" means, and why the diff is CANDIDATE-SCOPED ─────────────
//
// The optimizer migration is INCREMENTAL: the candidate Stack owns only the
// axes its wired constraints cover (empty today, export-ceiling in TASK-060),
// while the legacy cascade still authors every axis. A symmetric diff would
// therefore flag every legacy command the not-yet-migrated Stack has no opinion
// on — pure false positives that would bury the real signal and make the
// empty-Stack shadow counter pathologically non-zero.
//
// So a divergence is scoped to what the CANDIDATE claims: for each axis we
// evaluate a divergence ONLY when the candidate expresses an opinion on that
// axis (emits a real command / breach). Given a candidate opinion:
//   - legacy agrees within tolerance          → agree
//   - legacy commands a different value        → DIVERGENCE
//   - legacy has NO opinion on that axis       → DIVERGENCE (the candidate would
//                                                actuate a change the cascade
//                                                would not — exactly RSK-03)
// A candidate with no opinion on an axis is skipped: the cascade owning an axis
// the candidate hasn't migrated yet is expected, not a divergence.
//
// Consequence, as the bench validation requires: an empty Stack expresses no
// opinion on any axis, so it diverges on zero ticks. Once TASK-060 wires the
// export constraint, the candidate expresses a solar-ceiling / breach opinion on
// active ticks and the diff becomes meaningful on exactly those axes.

// Tolerances are the per-axis agreement bands. Bit-exact comparison is WRONG
// (03 §P5): the legacy cascade slews (maxDropW/maxRiseW) and low-pass filters
// (filterAlpha) its outputs, and the Stack reproduces those approximately from
// plant parameters — so the two agree in steady state but lag each other by a
// tick or two through a transient. Each band is sized to swallow that phase lag
// without hiding a genuine ordering difference.
type Tolerances struct {
	// WattAbs is the absolute watt band on every watt axis (solar ceiling,
	// battery setpoint). 150 W. Provenance: one filterAlpha=0.4 low-pass step
	// on a slew-limited signal leaves a residual well under this; the legacy
	// meter itself only resolves to the SunSpec watt multiplier. Small enough
	// that a real re-ordered dispatch (hundreds of W to kW) still trips it.
	WattAbs float64
	// WattFrac is the relative watt band, taken as the LARGER of WattAbs and
	// WattFrac×|commanded| so large setpoints tolerate the same fractional slew
	// lag a small one does. 0.05 (5 %) ≈ one maxRiseW=500 W relax step on a
	// ~10 kW ceiling.
	WattFrac float64
	// CurrentA is the EVSE current band (A). 0.5 A: below the 1 A granularity an
	// OCPP SetChargingProfile realistically actuates, so any real limit
	// difference (which moves in whole amps) still trips it.
	CurrentA float64
	// BreachDebounceTicks is how many consecutive ticks a breach-presence
	// mismatch must persist before it counts as a divergence. 2 ticks: the
	// legacy detector uses a fixed 3-tick window while the Stack uses an
	// adaptive DetectionWindowTicks (AD-007), so onset legitimately differs by
	// ≤2 ticks; a mismatch outlasting that is a real disagreement, not phase.
	BreachDebounceTicks int
}

// DefaultTolerances returns the bench-justified default bands. Every value's
// physical source is documented on the Tolerances field it fills.
func DefaultTolerances() Tolerances {
	return Tolerances{
		WattAbs:             150,
		WattFrac:            0.05,
		CurrentA:            0.5,
		BreachDebounceTicks: 2,
	}
}

// wattsAgree reports whether two watt commands are within the tolerance band.
func (t Tolerances) wattsAgree(legacy, candidate float64) bool {
	band := math.Max(t.WattAbs, t.WattFrac*math.Max(math.Abs(legacy), math.Abs(candidate)))
	return math.Abs(legacy-candidate) <= band
}

// AxisDivergence is one device+axis on which the candidate would actuate
// differently from the legacy authority. Legacy/Candidate are pre-formatted
// human strings; Delta is the signed (candidate−legacy) watt/amp gap when both
// sides commanded a numeric value (nil for connect/breach or one-sided cases).
type AxisDivergence struct {
	Device    string   `json:"device"`
	Axis      string   `json:"axis"`
	Legacy    string   `json:"legacy"`
	Candidate string   `json:"candidate"`
	Delta     *float64 `json:"delta,omitempty"`
}

// Divergence is one tick on which the candidate and legacy plans disagreed. It
// is a self-contained triage record: the diverging axes, a compact state
// snapshot, and the candidate's live constraint names.
type Divergence struct {
	Ts    int64            `json:"ts"`
	Axes  []AxisDivergence `json:"axes"`
	State StateSnapshot    `json:"state"`
	// CandidateSessions are the candidate Stack's live constraint names.
	CandidateSessions []string `json:"candidate_sessions,omitempty"`
	// Total is the running divergent-tick count at the moment of this record.
	Total uint64 `json:"divergences_total"`
	// Suppressed is how many divergent ticks of this same signature were
	// rate-limited away since the last emitted record of it.
	Suppressed uint64 `json:"suppressed,omitempty"`
}

// StateSnapshot is a compact, JSON-safe view of the SystemState that produced a
// divergence. All floats are *float64 so NaN (absent) serialises as null, never
// the literal NaN a PromQL/JSON consumer would choke on (05 §2).
type StateSnapshot struct {
	GridNetW     *float64      `json:"grid_net_w,omitempty"`
	ImportLimitW *float64      `json:"import_limit_w,omitempty"`
	ExportLimitW *float64      `json:"export_limit_w,omitempty"`
	MaxLimitW    *float64      `json:"max_limit_w,omitempty"`
	CSIP         string        `json:"csip,omitempty"`
	Batteries    []BatterySnap `json:"batteries,omitempty"`
	Solar        []SolarSnap   `json:"solar,omitempty"`
	EVSEs        []EVSESnap    `json:"evses,omitempty"`
}

// BatterySnap is one battery's compact state.
type BatterySnap struct {
	Name      string   `json:"name"`
	PowerW    float64  `json:"power_w"`
	SOC       *float64 `json:"soc,omitempty"`
	Connected bool     `json:"connected"`
}

// SolarSnap is one inverter's compact state.
type SolarSnap struct {
	Name      string  `json:"name"`
	PowerW    float64 `json:"power_w"`
	Connected bool    `json:"connected"`
}

// EVSESnap is one EVSE connector's compact state.
type EVSESnap struct {
	StationID     string  `json:"station_id"`
	ConnectorID   int     `json:"connector_id"`
	PowerW        float64 `json:"power_w"`
	SessionActive bool    `json:"session_active"`
}

// Options configures Wrap. All fields are optional; sensible defaults apply.
type Options struct {
	// Tolerances are the agreement bands. Zero value ⇒ DefaultTolerances.
	Tolerances Tolerances
	// Now is the injected clock the rate limiter reads. nil ⇒ time.Now.
	Now func() time.Time
	// OnDiverge receives one record per emitted (non-rate-limited) divergent
	// tick. nil ⇒ divergences are counted but not emitted. Must not block (it
	// runs on the control goroutine) and must not mutate the record.
	OnDiverge func(Divergence)
	// RateLimit is the minimum gap between emitted records of the SAME
	// signature (the sorted set of diverging device+axis keys). ≤0 ⇒ 1 minute.
	// Bounds journald/flash pressure when a divergence persists (05 §9).
	RateLimit time.Duration
	// OnPanic, when non-nil, is invoked once per candidate panic with the
	// recovered value and stack. Same contract as OnDiverge: must not block,
	// runs on the control goroutine.
	OnPanic func(recovered any, stack []byte)
}

// Wrapper is the shadow harness. It implements orchestrator.Optimizer (and,
// by delegation, orchestrator.SafetyEvaluator).
type Wrapper struct {
	legacy    orchestrator.Optimizer
	candidate orchestrator.Optimizer
	tol       Tolerances
	now       func() time.Time
	onDiverge func(Divergence)
	rateLimit time.Duration

	// count is the running divergent-tick total. Atomic: read by a metrics
	// scrape on another goroutine (cmd/hub Collect), written only here.
	count uint64
	// safetyCount is the running divergent-tick total for the Tier-1 fast
	// path shadow diff (EvaluateSafety). The flip gate carves out NOTHING on
	// this counter — any safety divergence is a flip blocker. Atomic.
	safetyCount uint64
	// panics counts candidate panics; latched flips to 1 on the first panic
	// and permanently disables candidate observation for the process
	// lifetime (WS-5.1). A latch trip fails the soak gate. Both atomic.
	panics  uint64
	latched uint32
	// axisCounts tallies divergent ticks per axis key (small fixed
	// vocabulary; "safety:"-prefixed for the fast path). sync.Map because a
	// metrics scrape reads concurrently with control-goroutine writes.
	axisCounts sync.Map // string -> *uint64
	onPanic    func(recovered any, stack []byte)

	// ── single-goroutine (control-loop) state ──
	// breachMismatch counts consecutive ticks the candidate holds a breach
	// opinion whose presence disagrees with legacy, for the onset debounce.
	breachMismatch int
	// lastEmit / suppressed track the per-signature rate limiter.
	lastEmit   map[string]time.Time
	suppressed map[string]uint64
}

// compile-time proof the Wrapper is a drop-in Optimizer AND passes through the
// engine's optional SafetyEvaluator type-assertion (delegated to legacy).
var (
	_ orchestrator.Optimizer       = (*Wrapper)(nil)
	_ orchestrator.SafetyEvaluator = (*Wrapper)(nil)
)

// Wrap builds a shadow Wrapper around the legacy (authoritative) optimizer and
// the candidate (observe-only) stack.
func Wrap(legacy, candidate orchestrator.Optimizer, opts Options) *Wrapper {
	tol := opts.Tolerances
	if tol == (Tolerances{}) {
		tol = DefaultTolerances()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	rl := opts.RateLimit
	if rl <= 0 {
		rl = time.Minute
	}
	return &Wrapper{
		legacy:     legacy,
		candidate:  candidate,
		tol:        tol,
		now:        now,
		onDiverge:  opts.OnDiverge,
		onPanic:    opts.OnPanic,
		rateLimit:  rl,
		lastEmit:   make(map[string]time.Time),
		suppressed: make(map[string]uint64),
	}
}

// Divergences returns the running count of divergent ticks. Safe to call from
// any goroutine (atomic) — cmd/hub mirrors it into a metric at scrape time.
func (w *Wrapper) Divergences() uint64 { return atomic.LoadUint64(&w.count) }

// SafetyDivergences returns the running count of Tier-1 fast-path divergent
// ticks (WS-5.3 shadow-safety diff). Safe from any goroutine.
func (w *Wrapper) SafetyDivergences() uint64 { return atomic.LoadUint64(&w.safetyCount) }

// Panics returns the number of candidate panics recovered; Latched reports
// whether candidate observation is permanently disabled (WS-5.1).
func (w *Wrapper) Panics() uint64 { return atomic.LoadUint64(&w.panics) }
func (w *Wrapper) Latched() bool  { return atomic.LoadUint32(&w.latched) == 1 }

// AxisDivergences snapshots the per-axis divergent-tick tallies ("safety:"
// prefix marks the fast path). Safe from any goroutine.
func (w *Wrapper) AxisDivergences() map[string]uint64 {
	out := make(map[string]uint64)
	w.axisCounts.Range(func(k, v any) bool {
		out[k.(string)] = atomic.LoadUint64(v.(*uint64))
		return true
	})
	return out
}

func (w *Wrapper) bumpAxis(key string) {
	v, _ := w.axisCounts.LoadOrStore(key, new(uint64))
	atomic.AddUint64(v.(*uint64), 1)
}

// Optimize runs both optimizers on the same state, diffs, and returns the
// LEGACY plan unmodified. The candidate plan is observed and discarded — it
// never reaches executePlan. This passthrough is the safety invariant of the
// whole harness (unit-tested by TestWrapper_ReturnsLegacyPlan).
//
// The candidate runs under a recover() latch (WS-5.1): a panic anywhere in
// the candidate stack must never kill the process controlling hardware. The
// first panic permanently disables observation (latch) — a tripped latch
// fails the soak gate, it is never silently tolerated.
func (w *Wrapper) Optimize(state orchestrator.SystemState) orchestrator.Plan {
	legacy := w.legacy.Optimize(state)
	if atomic.LoadUint32(&w.latched) == 0 {
		w.observeCandidate(state, legacy)
	}
	return legacy
}

func (w *Wrapper) observeCandidate(state orchestrator.SystemState, legacy orchestrator.Plan) {
	defer w.recoverCandidate()
	candidate := w.candidate.Optimize(state)
	w.compare(state, legacy, candidate, "")
}

// EvaluateSafety returns the LEGACY optimizer's Tier-1 fast protection plan —
// the cascade remains the sole author of protective disconnects until the
// flip. As of WS-5.3 the candidate Stack's safety path (EvaluateSafety →
// EvaluateFast) is ALSO run, observe-only under the same panic latch, and
// diffed into safetyCount/"safety:"-prefixed axes: the fast path accrues real
// bench hours during the soak instead of arriving at the flip with zero.
// When the legacy optimizer is not itself a SafetyEvaluator (only in tests —
// the product wires DefaultOptimizer, which is), this returns an inert safety
// plan so the engine's fast loop is a no-op.
func (w *Wrapper) EvaluateSafety(state orchestrator.SystemState) orchestrator.Plan {
	legacyPlan := orchestrator.Plan{Timestamp: state.Timestamp, Safety: true}
	if se, ok := w.legacy.(orchestrator.SafetyEvaluator); ok {
		legacyPlan = se.EvaluateSafety(state)
	}
	if atomic.LoadUint32(&w.latched) == 0 {
		if cse, ok := w.candidate.(orchestrator.SafetyEvaluator); ok {
			w.observeCandidateSafety(state, legacyPlan, cse)
		}
	}
	return legacyPlan
}

func (w *Wrapper) observeCandidateSafety(state orchestrator.SystemState, legacyPlan orchestrator.Plan, cse orchestrator.SafetyEvaluator) {
	defer w.recoverCandidate()
	candidatePlan := cse.EvaluateSafety(state)
	w.compare(state, legacyPlan, candidatePlan, "safety:")
}

// recoverCandidate is the WS-5.1 latch: record the panic, disable observation
// for the process lifetime, and let the control loop continue on legacy.
func (w *Wrapper) recoverCandidate() {
	if r := recover(); r != nil {
		atomic.AddUint64(&w.panics, 1)
		atomic.StoreUint32(&w.latched, 1)
		if w.onPanic != nil {
			w.onPanic(r, debug.Stack())
		}
	}
}

// compare diffs the two plans, updates the counter, and (rate-limited) emits.
// Breach is evaluated FIRST and unconditionally so its debounce counter tracks
// every tick regardless of whether any command axis diverged. prefix "" is the
// slow (Optimize) path; "safety:" is the Tier-1 fast-path diff (WS-5.3),
// which counts into safetyCount and prefixes its axis keys so the two streams
// are separable in metrics, signatures, and the soak-gate classification.
func (w *Wrapper) compare(state orchestrator.SystemState, legacy, candidate orchestrator.Plan, prefix string) {
	var axes []AxisDivergence

	if bd := w.diffBreach(legacy, candidate); bd != nil {
		axes = append(axes, *bd)
	}
	axes = append(axes, w.diffSolar(legacy, candidate)...)
	axes = append(axes, w.diffBattery(legacy, candidate)...)
	axes = append(axes, w.diffEVSE(legacy, candidate)...)

	if len(axes) == 0 {
		return
	}
	counter := &w.count
	if prefix != "" {
		counter = &w.safetyCount
	}
	atomic.AddUint64(counter, 1)
	for _, a := range axes {
		w.bumpAxis(prefix + a.Axis)
	}

	sig := prefix + signature(axes)
	now := w.now()
	if !w.allow(sig, now) {
		w.suppressed[sig]++
		return
	}
	if w.onDiverge == nil {
		return
	}
	rec := Divergence{
		Ts:         timestampOf(legacy, candidate, now),
		Axes:       axes,
		State:      snapshot(state),
		Total:      atomic.LoadUint64(counter),
		Suppressed: w.suppressed[sig],
	}
	if ns, ok := w.candidate.(interface{ SessionNames() []string }); ok {
		rec.CandidateSessions = ns.SessionNames()
	}
	w.suppressed[sig] = 0
	w.onDiverge(rec)
}

// allow reports whether a record of this signature may be emitted now, and
// records the emit time when it may. First sighting always emits.
func (w *Wrapper) allow(sig string, now time.Time) bool {
	last, seen := w.lastEmit[sig]
	if seen && now.Sub(last) < w.rateLimit {
		return false
	}
	w.lastEmit[sig] = now
	return true
}

// ── per-axis diffs ────────────────────────────────────────────────────────────

// diffSolar compares solar ceilings for every device the CANDIDATE curtails.
// A candidate SolarCommand with a NaN CurtailToW is "no curtailment" — no
// opinion — and is skipped. Legacy absence (or its own NaN ceiling) is treated
// as "no cap"; a real candidate curtailment against no legacy cap diverges.
func (w *Wrapper) diffSolar(legacy, candidate orchestrator.Plan) []AxisDivergence {
	legCeil := map[string]float64{}
	for _, c := range legacy.SolarCommands {
		legCeil[c.Name] = c.CurtailToW
	}
	var out []AxisDivergence
	for _, c := range candidate.SolarCommands {
		if math.IsNaN(c.CurtailToW) {
			continue // candidate expresses no restriction
		}
		lv, ok := legCeil[c.Name]
		if ok && !math.IsNaN(lv) {
			if w.tol.wattsAgree(lv, c.CurtailToW) {
				continue
			}
			out = append(out, wattAxis(c.Name, AxisSolarCeilingW, lv, c.CurtailToW))
			continue
		}
		// Legacy imposes no ceiling; candidate curtails.
		out = append(out, AxisDivergence{
			Device: c.Name, Axis: AxisSolarCeilingW.String(),
			Legacy: "none", Candidate: fmtW(c.CurtailToW),
		})
	}
	return out
}

// diffBattery compares battery setpoint and connect for every device the
// CANDIDATE commands. A NaN candidate setpoint is "leave unchanged" (no
// opinion) and skipped; a nil candidate Connect is skipped likewise.
func (w *Wrapper) diffBattery(legacy, candidate orchestrator.Plan) []AxisDivergence {
	type legBatt struct {
		setpoint float64
		connect  *bool
		present  bool
	}
	leg := map[string]legBatt{}
	for _, c := range legacy.BatteryCommands {
		leg[c.Name] = legBatt{setpoint: c.SetpointW, connect: c.Connect, present: true}
	}
	var out []AxisDivergence
	for _, c := range candidate.BatteryCommands {
		lb := leg[c.Name]
		// Setpoint axis.
		if !math.IsNaN(c.SetpointW) {
			switch {
			case lb.present && !math.IsNaN(lb.setpoint):
				if !w.tol.wattsAgree(lb.setpoint, c.SetpointW) {
					out = append(out, wattAxis(c.Name, AxisBatterySetpointW, lb.setpoint, c.SetpointW))
				}
			default:
				out = append(out, AxisDivergence{
					Device: c.Name, Axis: AxisBatterySetpointW.String(),
					Legacy: "none", Candidate: fmtW(c.SetpointW),
				})
			}
		}
		// Connect axis (exact — a connect/disconnect has no phase lag to forgive).
		if c.Connect != nil {
			if lb.connect == nil || *lb.connect != *c.Connect {
				out = append(out, AxisDivergence{
					Device: c.Name, Axis: AxisConnect.String(),
					Legacy: fmtConnect(lb.connect), Candidate: fmtConnect(c.Connect),
				})
			}
		}
	}
	return out
}

// diffEVSE compares charging-current ceilings for every connector the CANDIDATE
// limits. EVSE commands are keyed by station+connector.
func (w *Wrapper) diffEVSE(legacy, candidate orchestrator.Plan) []AxisDivergence {
	type key struct {
		station   string
		connector int
	}
	leg := map[key]float64{}
	for _, c := range legacy.EVSECommands {
		leg[key{c.StationID, c.ConnectorID}] = c.MaxCurrentA
	}
	var out []AxisDivergence
	for _, c := range candidate.EVSECommands {
		lv, ok := leg[key{c.StationID, c.ConnectorID}]
		dev := evseKey(c.StationID, c.ConnectorID)
		if ok {
			if math.Abs(lv-c.MaxCurrentA) <= w.tol.CurrentA {
				continue
			}
			d := c.MaxCurrentA - lv
			out = append(out, AxisDivergence{
				Device: dev, Axis: AxisEVSECurrentA.String(),
				Legacy: fmtA(lv), Candidate: fmtA(c.MaxCurrentA), Delta: &d,
			})
			continue
		}
		out = append(out, AxisDivergence{
			Device: dev, Axis: AxisEVSECurrentA.String(),
			Legacy: "none", Candidate: fmtA(c.MaxCurrentA),
		})
	}
	return out
}

// diffBreach compares compliance-breach presence and LimitType, with the onset
// debounce. It is CANDIDATE-scoped and MUST run every tick so the debounce
// counter stays accurate:
//   - candidate has no breach      → reset debounce, no divergence (the cascade
//     owning a breach class the candidate hasn't
//     migrated is expected).
//   - both present, same type      → reset debounce, agree.
//   - both present, type differs   → immediate divergence (both fired this tick
//     but disagree on WHAT — not a timing issue).
//   - candidate present, legacy not → presence mismatch: debounce
//     BreachDebounceTicks ticks (adaptive vs
//     fixed detection window), then diverge.
func (w *Wrapper) diffBreach(legacy, candidate orchestrator.Plan) *AxisDivergence {
	cand := candidate.Breach
	leg := legacy.Breach

	if cand == nil {
		w.breachMismatch = 0
		return nil
	}
	if leg != nil {
		w.breachMismatch = 0
		if leg.LimitType == cand.LimitType {
			return nil
		}
		return &AxisDivergence{
			Device: "grid", Axis: "breach",
			Legacy: "breach:" + leg.LimitType, Candidate: "breach:" + cand.LimitType,
		}
	}
	// Candidate breaches, legacy does not — allow the onset window to differ.
	w.breachMismatch++
	if w.breachMismatch <= w.tol.BreachDebounceTicks {
		return nil
	}
	return &AxisDivergence{
		Device: "grid", Axis: "breach",
		Legacy: "none", Candidate: "breach:" + cand.LimitType,
	}
}

// ── formatting & helpers ──────────────────────────────────────────────────────

// wattAxis builds a two-sided watt divergence with a signed delta.
func wattAxis(device string, axis Axis, legacy, candidate float64) AxisDivergence {
	d := candidate - legacy
	return AxisDivergence{
		Device: device, Axis: axis.String(),
		Legacy: fmtW(legacy), Candidate: fmtW(candidate), Delta: &d,
	}
}

func fmtW(v float64) string {
	if math.IsNaN(v) {
		return "none"
	}
	return fmt.Sprintf("%.0fW", v)
}

func fmtA(v float64) string { return fmt.Sprintf("%.1fA", v) }

func fmtConnect(c *bool) string {
	if c == nil {
		return "none"
	}
	if *c {
		return "connect"
	}
	return "disconnect"
}

func evseKey(station string, connector int) string {
	return fmt.Sprintf("%s#%d", station, connector)
}

// signature is the rate-limit key: the sorted set of diverging device+axis
// keys, so an identical divergence pattern maps to one signature (bounded to
// 1/RateLimit) while a changed pattern is allowed a fresh emission.
func signature(axes []AxisDivergence) string {
	keys := make([]string, 0, len(axes))
	for _, a := range axes {
		keys = append(keys, a.Device+"/"+a.Axis)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// timestampOf prefers the plans' own evaluation time (they share the tick's
// SystemState.Timestamp), falling back to the injected clock for a zero-value
// plan timestamp (tests that hand-build plans).
func timestampOf(legacy, candidate orchestrator.Plan, now time.Time) int64 {
	if !legacy.Timestamp.IsZero() {
		return legacy.Timestamp.Unix()
	}
	if !candidate.Timestamp.IsZero() {
		return candidate.Timestamp.Unix()
	}
	return now.Unix()
}

// snapshot builds the compact, NaN-safe state view for a divergence record.
func snapshot(state orchestrator.SystemState) StateSnapshot {
	s := StateSnapshot{
		GridNetW:     nanPtr(state.Grid.NetW),
		ImportLimitW: nanPtr(state.Grid.ImportLimitW),
		ExportLimitW: nanPtr(state.Grid.ExportLimitW),
		MaxLimitW:    nanPtr(state.Grid.MaxLimitW),
	}
	if state.CSIPControl != nil {
		s.CSIP = state.CSIPControl.Source + "(" + state.CSIPControl.MRID + ")"
	}
	for _, b := range state.Batteries {
		s.Batteries = append(s.Batteries, BatterySnap{
			Name: b.Name, PowerW: b.PowerW, SOC: nanPtr(b.SOC), Connected: b.Connected,
		})
	}
	for _, sol := range state.Solar {
		s.Solar = append(s.Solar, SolarSnap{Name: sol.Name, PowerW: sol.PowerW, Connected: sol.Connected})
	}
	for _, e := range state.EVSEs {
		s.EVSEs = append(s.EVSEs, EVSESnap{
			StationID: e.StationID, ConnectorID: e.ConnectorID,
			PowerW: e.PowerW, SessionActive: e.SessionActive,
		})
	}
	return s
}

// nanPtr maps NaN → nil (absent in JSON) and any finite value → a pointer to a
// copy, so the snapshot never encodes a bare NaN (05 §2).
func nanPtr(v float64) *float64 {
	if math.IsNaN(v) {
		return nil
	}
	return &v
}
