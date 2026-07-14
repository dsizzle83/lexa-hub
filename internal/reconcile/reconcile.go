// Package reconcile is the pure, I/O-free per-device desired-state
// reconciliation state machine at the heart of AD-002 (the Device Reconciler)
// and its wire contract AD-013 (bus.DesiredState). It subsumes the four legacy
// convergence mechanisms tracked in docs/refactor/PRESERVATION_LEDGER.md rows
// L1–L4 into ONE exhaustively-testable core; the shells that map its Fields to
// real drivers (TASK-027 battery/solar, TASK-030 EVSE) wire it to hardware.
//
// # No I/O, ever
//
// A Reconciler performs no I/O and starts no goroutines. It consumes input
// events plus an INJECTED wall clock and returns Actions (writes the shell
// performs) and Reports (evidence the shell forwards). It never calls a driver,
// never touches the network or a clock of its own — there is no time.Now /
// time.Sleep / time.After anywhere in this package. That discipline is what
// keeps the entire transition table drivable from a table-driven test with a
// fake clock (05 §1/§4; the orchestrator earned real unit tests the same way).
//
// # The state machine, in words
//
// State per device: the standing desired document (last accepted), the
// last-applied (seq, issuedAt) staleness baseline, the last readback, the last
// convergence assessment, a divergence-episode timer, and a retry-backoff
// cursor. Four input events drive it:
//
//   - SetDesired(doc, now): applies the AD-013 seq/issuedAt/staleness/NaN gate
//     FIRST. A rejected doc changes nothing (RejectedDoc report). An accepted
//     doc becomes the standing intent; a NEW target resets the convergence
//     window and emits an unconditional Write; a same-target refresh (heartbeat,
//     or a publisher-restart SeqReset) updates the baseline without a write.
//   - Observe(o, now): verify-by-readback. Convergence is judged ONLY from a
//     plausible, connected sample (never from a write's success — "trust
//     measurement, not the command"). A diverged read triggers a corrective
//     Write (rate-limited by the backoff schedule); a converged read closes any
//     open episode. Implausible / disconnected / field-incomplete samples hold
//     the previous assessment and never provoke a write storm (the plausibleW
//     discipline, ledger L9).
//   - Reconnected(now): L4 — the next action is an UNCONDITIONAL Write of the
//     standing desired, even if the last pre-drop readback matched, because a
//     reconnected device may have rebooted to defaults.
//   - Tick(now): drives the wall-clock timers — non-convergence begin, held-doc
//     staleness reporting, retry-backoff re-writes, and the optional slow
//     reassert watchdog. Cadence is the caller's; thresholds are seconds, never
//     ticks (05 §5 — FAST and STOCK must mean the same seconds).
//
// # Ledger rows subsumed
//
//   - L1/L2 standing intent + watchdog: the desired doc is durable intent; the
//     reconciler re-writes on divergence and optionally re-asserts on a slow
//     timer (Config.ReassertEvery).
//   - L3 revert detection: verify-by-readback compares every plausible read to
//     desired; a device that ACKs then reverts gets a corrective write bounded
//     by the readback interval, not a 60 s watchdog.
//   - L4 reassert-on-reconnect: Reconnected forces a write of current desired.
//
// # Concurrency
//
// A Reconciler is single-writer: exactly one goroutine (the shell's control
// loop) may call its methods, mirroring cmdDeduper's contract. It holds no
// locks and is not safe for concurrent use.
package reconcile

import (
	"math"
	"time"

	"lexa-hub/internal/bus"
)

// Default reconciler tuning. Zero-valued Config fields resolve to these in ONE
// place (New → withDefaults), so no magic number is scattered across call sites
// (05 §6). All are wall-clock durations; the caller supplies real values from
// config (poll cadence differs between modbus and ocpp).
const (
	// DefaultConvergeTimeout is how long sustained, observed divergence must
	// persist before a NonConvergedBegin report fires.
	DefaultConvergeTimeout = 60 * time.Second
	// DefaultStaleAfter is the AD-013 held-doc staleness bound (300 s): both the
	// "no fresh doc → StaleDesired" threshold and the incoming "reject a doc
	// whose issuedAt is older than this" bound. Long enough to survive a broker
	// blip + publisher restart, short enough to surface a wedged publisher
	// within one CSIP reporting-grace window.
	DefaultStaleAfter = 300 * time.Second
	// defaultPowerTolerance is the readback tolerance for W/A fields (1 unit)
	// when a Field has no configured tolerance.
	defaultPowerTolerance = 1.0
	// defaultConnectTolerance keeps the boolean Connect surface exact: 0 and 1
	// differ by 1, so any tolerance below 1 (here 0.5) forbids aliasing.
	defaultConnectTolerance = 0.5
	// defaultPFTolerance is the readback tolerance for the FixedPF surface
	// (power factor, [0,1] signed): the W/A default of 1.0 spans the whole
	// range and would call any PF converged. The WP-10 shell feeds the DESIRED
	// value on a converged one-sided assessment and a genuinely-measured value
	// on divergence, so this only needs to be small enough to separate those.
	defaultPFTolerance = 0.005
)

// DefaultRetryBackoff is the escalating re-write schedule when a device will
// not converge: 2 s, 5 s, 15 s, then every 30 s (the last entry repeats). Never
// a tight loop.
var DefaultRetryBackoff = []time.Duration{
	2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second,
}

// Config tunes a Reconciler. All durations are wall-clock. A zero value for any
// field resolves to the documented default above (withDefaults); ReassertEvery
// is the one exception — zero DISABLES the optional slow reassert watchdog,
// because re-asserting on a slow timer is explicitly optional (ledger L2 makes
// the retained doc immediate on subscribe, so the watchdog is belt-and-braces).
type Config struct {
	// ReadbackTolerance is the per-Field allowed |read−desired| before a read is
	// judged diverged (inclusive: exactly at tolerance is converged). Missing
	// entries use defaultPowerTolerance (or defaultConnectTolerance for Connect).
	ReadbackTolerance map[Field]float64
	// ConvergeTimeout: sustained divergence before NonConvergedBegin.
	ConvergeTimeout time.Duration
	// RetryBackoff: escalating gap between corrective re-writes.
	RetryBackoff []time.Duration
	// StaleAfter: held-doc staleness bound (report + incoming reject).
	StaleAfter time.Duration
	// ReassertEvery: slow watchdog re-write cadence; zero disables it.
	ReassertEvery time.Duration
}

func (c Config) withDefaults() Config {
	if c.ConvergeTimeout <= 0 {
		c.ConvergeTimeout = DefaultConvergeTimeout
	}
	if c.StaleAfter <= 0 {
		c.StaleAfter = DefaultStaleAfter
	}
	if len(c.RetryBackoff) == 0 {
		c.RetryBackoff = DefaultRetryBackoff
	}
	// ReassertEvery: zero stays zero (disabled) — documented on the type.
	if c.ReassertEvery < 0 {
		c.ReassertEvery = 0
	}
	return c
}

// Observed is one normalized readback sample the shell hands the core. Read
// holds the fields it could decode (Connect as 1/0); Connected reports link
// state; Plausible is the shell's nameplate/plausibility verdict (the
// plausibleW gate, ledger L9) — an implausible sample is evidence of nothing.
type Observed struct {
	Read      map[Field]float64
	Connected bool
	At        time.Time
	Plausible bool
}

// Reconciler is the per-device state machine. Construct with New; drive with
// SetDesired / Observe / Reconnected / Tick. Single-writer (see package doc).
type Reconciler struct {
	cfg      Config
	class    string
	deviceID string

	// Standing intent (nil until the first accepted doc). desiredFields is the
	// extracted opinion set (only non-nil doc fields), Connect as 1/0.
	desired       *bus.DesiredState
	desiredFields map[Field]float64

	// AD-013 staleness/seq baseline.
	lastAppliedSeq      uint64
	lastAppliedIssuedAt int64
	haveApplied         bool

	// Convergence / write tracking.
	lastRead       Observed
	lastWritten    map[Field]float64
	converged      bool
	divergentSince time.Time // zero = not observed diverged

	// Non-convergence episode (edge-triggered, once per episode).
	inEpisode      bool
	episodeCounter uint64

	// Retry backoff cursor (within one divergence episode).
	writeAttempts int
	lastWriteAt   time.Time

	// Staleness of the held doc.
	lastDocAt     time.Time
	staleReported bool

	// Slow reassert watchdog.
	lastReassertAt time.Time
}

// New builds a Reconciler for a device of the given class ("battery" | "solar"
// | "evse", per bus.DesiredClass*) and ID, applying Config defaults.
func New(class, deviceID string, cfg Config) *Reconciler {
	return &Reconciler{cfg: cfg.withDefaults(), class: class, deviceID: deviceID}
}

// DocMeta is the AD-013 identity/staleness surface of a desired document —
// the fields the acceptance gate and report attribution need, independent of
// which document TYPE carried them. WP-10's advanced shell extracts it from
// bus.DesiredAdvanced (whose curve payloads the scalar-fields core has no
// business decoding) and feeds SetDesiredFields; SetDesired derives it from
// bus.DesiredState internally.
type DocMeta struct {
	MRID     string
	Seq      uint64
	IssuedAt int64
}

// SetDesired applies the AD-013 seq/issuedAt/staleness/NaN gate and, on accept,
// adopts the document as the standing intent. It returns the resulting Action
// (an unconditional Write on a new target; None on a same-target refresh or a
// rejection) and any Reports (RejectedDoc, SeqReset, and a NonConvergedEnd if a
// target change closes an open episode).
func (r *Reconciler) SetDesired(doc bus.DesiredState, now time.Time) (Action, []Report) {
	// NaN defense first: a corrupt doc never becomes the standing intent.
	if docHasNaN(doc) {
		return none(), []Report{r.rejectReport(doc, RejectNaN, now)}
	}
	return r.setDesiredCore(doc, fieldsOf(doc), now)
}

// SetDesiredFields is SetDesired for callers whose document type is not
// bus.DesiredState (WP-10: bus.DesiredAdvanced, decomposed per axis by the adv
// shell). It applies the IDENTICAL AD-013 gate — NaN defense over the opinion
// values, staleness, seq/issuedAt regression — and on accept adopts the given
// opinion set as the standing intent, with meta carried for report
// attribution. Additive: SetDesired's behavior is byte-identical (both route
// through setDesiredCore).
func (r *Reconciler) SetDesiredFields(meta DocMeta, fields map[Field]float64, now time.Time) (Action, []Report) {
	doc := bus.DesiredState{MRID: meta.MRID, Seq: meta.Seq, IssuedAt: meta.IssuedAt}
	for _, v := range fields {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return none(), []Report{r.rejectReport(doc, RejectNaN, now)}
		}
	}
	return r.setDesiredCore(doc, copyFields(fields), now)
}

// setDesiredCore is the shared gate+adopt body of SetDesired/SetDesiredFields.
// fields is owned by the callee after the call (both callers pass a fresh map).
func (r *Reconciler) setDesiredCore(doc bus.DesiredState, fields map[Field]float64, now time.Time) (Action, []Report) {
	// AD-013 rule 3: reject stale regardless of seq (an old retained doc must
	// not win over reality). issuedAt older than the staleness bound.
	if now.Unix()-doc.IssuedAt > int64(r.cfg.StaleAfter/time.Second) {
		return none(), []Report{r.rejectReport(doc, RejectStale, now)}
	}
	seqReset := false
	if r.haveApplied {
		newerIssued := doc.IssuedAt > r.lastAppliedIssuedAt
		higherSeq := doc.Seq > r.lastAppliedSeq
		switch {
		case higherSeq:
			// Normal forward progress within the publisher epoch.
		case newerIssued:
			// AD-013 rule 2: lower/reset seq but strictly newer issuedAt — a
			// publisher restart. Accept, but make it observable.
			seqReset = true
		default:
			// AD-013 rule 1: seq <= last AND issuedAt <= last — replay / retained
			// redelivery. Reject; state unchanged.
			return none(), []Report{r.rejectReport(doc, RejectSeqRegression, now)}
		}
	}

	// Accept. Adopt the doc and advance the baseline.
	prevFields := r.desiredFields
	stored := doc
	r.desired = &stored
	r.desiredFields = fields
	r.lastAppliedSeq = doc.Seq
	r.lastAppliedIssuedAt = doc.IssuedAt
	r.haveApplied = true
	r.lastDocAt = now
	r.staleReported = false

	var reports []Report
	if seqReset {
		reports = append(reports, r.report(ReportSeqReset, now))
	}

	// Same target as before? A heartbeat refresh (or a same-intent SeqReset):
	// update the baseline, do not disturb convergence, do not re-write.
	if fieldsEqual(prevFields, r.desiredFields) {
		return none(), reports
	}

	// New target: reset the convergence window and write unconditionally. Any
	// open episode is about the OLD target — close it so begins/ends stay paired.
	if r.inEpisode {
		r.inEpisode = false
		reports = append(reports, r.report(ReportNonConvergedEnd, now))
	}
	r.divergentSince = time.Time{}
	r.converged = false
	r.writeAttempts = 0
	return r.writeAction("new-desired", now), reports
}

// Observe applies one readback. It rejects NaN/Inf samples (RejectedObs), holds
// the previous assessment for implausible / disconnected / field-incomplete
// samples, and otherwise judges convergence: a converged read closes any open
// episode (None); a diverged read triggers a rate-limited corrective Write.
func (r *Reconciler) Observe(o Observed, now time.Time) (Action, []Report) {
	for _, v := range o.Read {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			rep := r.report(ReportRejectedObs, now)
			rep.Reject = RejectNaN
			return none(), []Report{rep}
		}
	}
	r.lastRead = o
	if r.desired == nil {
		return none(), nil // nothing to converge to yet
	}
	if !o.Connected || !o.Plausible {
		return none(), nil // hold previous assessment (plausibleW discipline)
	}
	ok, complete := r.matches(o.Read)
	if !complete {
		return none(), nil // missing a commanded field: cannot assess, hold
	}

	var reports []Report
	if ok {
		if !r.divergentSince.IsZero() {
			r.divergentSince = time.Time{}
			r.writeAttempts = 0
			if r.inEpisode {
				r.inEpisode = false
				reports = append(reports, r.report(ReportNonConvergedEnd, now))
			}
		}
		r.converged = true
		return none(), reports
	}

	// Diverged.
	r.converged = false
	if r.divergentSince.IsZero() {
		r.divergentSince = now
		r.writeAttempts = 0
	}
	reports = append(reports, r.checkEpisodeBegin(now)...)
	if r.backoffReady(now) {
		return r.writeAction("write-on-diff", now), reports
	}
	return none(), reports
}

// Episode returns the current non-convergence episode counter. The WP-10 adv
// shell stamps it on shell-authored (AdoptState) reports so they correlate
// with the same episode's NonConvergedBegin/End on the wire. Read-only,
// single-writer like every other method.
func (r *Reconciler) Episode() uint64 { return r.episodeCounter }

// Reconnected forces the next action to be an unconditional Write of the
// standing desired (ledger L4). A reconnected device may have rebooted to
// defaults, so the pre-drop assessment is no longer trusted; convergence must
// be re-proven by a later Observe. Returns None if there is no standing intent.
func (r *Reconciler) Reconnected(now time.Time) (Action, []Report) {
	if r.desired == nil {
		return none(), nil
	}
	// The post-reconnect readback is unknown; drop the stale assessment. Do not
	// fabricate a divergence episode (none has been OBSERVED) — but if one is
	// already open, keep it until a converged read closes it.
	r.converged = false
	if r.divergentSince.IsZero() {
		r.writeAttempts = 0
	}
	return r.writeAction("reconnect-reassert", now), nil
}

// Tick advances the wall-clock timers: held-doc staleness reporting,
// non-convergence begin, retry-backoff re-writes for observed divergence, and
// the optional slow reassert watchdog. The caller chooses the cadence; every
// threshold is seconds, never ticks.
func (r *Reconciler) Tick(now time.Time) (Action, []Report) {
	if r.desired == nil {
		return none(), nil
	}
	var reports []Report
	if !r.lastDocAt.IsZero() && !r.staleReported &&
		now.Sub(r.lastDocAt) >= r.cfg.StaleAfter {
		r.staleReported = true
		reports = append(reports, r.report(ReportStaleDesired, now))
	}
	reports = append(reports, r.checkEpisodeBegin(now)...)

	// Retry re-write while observed-diverged, paced by the backoff schedule.
	if !r.divergentSince.IsZero() && r.backoffReady(now) {
		return r.writeAction("retry", now), reports
	}
	// Slow reassert watchdog (converged/idle only; diverged uses retry above).
	if r.cfg.ReassertEvery > 0 && r.divergentSince.IsZero() &&
		!r.lastReassertAt.IsZero() && now.Sub(r.lastReassertAt) >= r.cfg.ReassertEvery {
		return r.writeAction("reassert", now), reports
	}
	return none(), reports
}

// checkEpisodeBegin emits NonConvergedBegin exactly once per divergence episode,
// when observed divergence has persisted ConvergeTimeout.
func (r *Reconciler) checkEpisodeBegin(now time.Time) []Report {
	if !r.divergentSince.IsZero() && !r.inEpisode &&
		now.Sub(r.divergentSince) >= r.cfg.ConvergeTimeout {
		r.inEpisode = true
		r.episodeCounter++
		return []Report{r.report(ReportNonConvergedBegin, now)}
	}
	return nil
}

// backoffReady reports whether enough time has elapsed since the last write to
// issue another. The first write of a divergence episode (writeAttempts == 0)
// is immediate; subsequent writes follow RetryBackoff, its last entry repeating.
func (r *Reconciler) backoffReady(now time.Time) bool {
	if r.writeAttempts == 0 {
		return true
	}
	idx := r.writeAttempts - 1
	if idx >= len(r.cfg.RetryBackoff) {
		idx = len(r.cfg.RetryBackoff) - 1
	}
	return now.Sub(r.lastWriteAt) >= r.cfg.RetryBackoff[idx]
}

// writeAction builds a Write of the current desired fields and records it as the
// last write (advancing the backoff cursor and the reassert timer).
func (r *Reconciler) writeAction(reason string, now time.Time) Action {
	r.lastWriteAt = now
	r.lastReassertAt = now
	r.writeAttempts++
	r.lastWritten = copyFields(r.desiredFields)
	return Action{Kind: ActionWrite, Fields: copyFields(r.desiredFields), Reason: reason}
}

// matches reports whether every commanded field is present in read and within
// tolerance. complete is false when a commanded field is absent from read (then
// ok is meaningless and the caller holds the previous assessment).
//
// Completeness is checked in a first, standalone pass over every key of
// r.desiredFields before any tolerance comparison runs. This is deliberate,
// not a style choice: map iteration order is randomized per Go's spec, and a
// single combined loop that returns as soon as EITHER "field absent" OR
// "field diverged" fires would make `complete` depend on which key the
// runtime happened to visit first — a doc expressing two fields (e.g. a
// battery's SetpointW AND Connect, ubiquitous in real commands) where only
// one is ever readable (TASK-027: lexa-modbus has no Connect-state register)
// would then flip between "hold, incomplete" and "diverged, complete" from
// call to call with byte-identical inputs. That directly contradicts this
// package's own documented guarantee (file doc: "field-incomplete samples
// hold the previous assessment and never provoke a write storm") — found
// while wiring TASK-027, the first real multi-field consumer.
func (r *Reconciler) matches(read map[Field]float64) (ok, complete bool) {
	for f := range r.desiredFields {
		if _, present := read[f]; !present {
			return false, false
		}
	}
	for f, want := range r.desiredFields {
		if math.Abs(read[f]-want) > r.tolerance(f) {
			return false, true
		}
	}
	return true, true
}

// tolerance returns the readback tolerance for a Field: the configured value,
// else the boolean-exact default for the boolean-like surfaces (Connect,
// Energize, AdvContent — 0/1 or exact-fingerprint semantics, where the W/A
// default of 1.0 would alias distinct values), else the unit-PF default for
// FixedPF (a [0,1]-ranged surface where 1.0 would call everything converged),
// else the W/A default.
func (r *Reconciler) tolerance(f Field) float64 {
	if r.cfg.ReadbackTolerance != nil {
		if v, ok := r.cfg.ReadbackTolerance[f]; ok {
			return v
		}
	}
	switch f {
	case Connect, Energize, AdvContent:
		return defaultConnectTolerance
	case FixedPF:
		return defaultPFTolerance
	default:
		return defaultPowerTolerance
	}
}

// report builds a Report attributed to the current standing desired.
func (r *Reconciler) report(kind ReportKind, now time.Time) Report {
	rep := Report{
		Kind:        kind,
		DeviceClass: r.class,
		DeviceID:    r.deviceID,
		Episode:     r.episodeCounter,
		At:          now,
	}
	if r.desired != nil {
		rep.MRID = r.desired.MRID
		rep.Seq = r.desired.Seq
		rep.IssuedAt = r.desired.IssuedAt
	}
	return rep
}

// rejectReport builds a RejectedDoc report attributed to the REJECTED doc (not
// the standing intent), so TASK-031 sees which doc was refused and why.
func (r *Reconciler) rejectReport(doc bus.DesiredState, why RejectReason, now time.Time) Report {
	return Report{
		Kind:        ReportRejectedDoc,
		DeviceClass: r.class,
		DeviceID:    r.deviceID,
		MRID:        doc.MRID,
		Seq:         doc.Seq,
		IssuedAt:    doc.IssuedAt,
		Episode:     r.episodeCounter,
		Reject:      why,
		At:          now,
	}
}

// fieldsOf extracts the opinion set (non-nil fields only) from a document,
// encoding Connect as 1/0. Per class only that class's fields carry opinion.
func fieldsOf(d bus.DesiredState) map[Field]float64 {
	m := make(map[Field]float64, 4)
	if d.SetpointW != nil {
		m[SetpointW] = *d.SetpointW
	}
	if d.CeilingW != nil {
		m[CeilingW] = *d.CeilingW
	}
	if d.Connect != nil {
		if *d.Connect {
			m[Connect] = 1
		} else {
			m[Connect] = 0
		}
	}
	if d.MaxCurrentA != nil {
		m[MaxCurrentA] = *d.MaxCurrentA
	}
	return m
}

// docHasNaN reports whether any expressed (non-nil) field is NaN/Inf.
func docHasNaN(d bus.DesiredState) bool {
	for _, p := range []*float64{d.SetpointW, d.CeilingW, d.MaxCurrentA} {
		if p != nil && (math.IsNaN(*p) || math.IsInf(*p, 0)) {
			return true
		}
	}
	return false
}

func copyFields(m map[Field]float64) map[Field]float64 {
	out := make(map[Field]float64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func fieldsEqual(a, b map[Field]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		w, ok := b[k]
		if !ok || v != w {
			return false
		}
	}
	return true
}
