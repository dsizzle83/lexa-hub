package main

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/journal"
	"lexa-hub/internal/orchestrator"
	"lexa-hub/internal/reconcile"
)

// breachEpisodes is the named breach-episode component (TASK-031): the single
// owner of CannotComply episode state and arbitration. It replaces the former
// `activeBreachMRID` closure variable + standalone breachAlert func in
// cmd/hub/main.go (05 §4: "if it has a name in a bug report, it needs a name in
// the code").
//
// It merges TWO evidence sources into ONE episode stream so the grid server
// sees exactly one CannotComply per real episode, not one per source:
//
//   - Optimizer meter-level breaches (orchestrator.Plan.Breach): "the SITE is
//     not meeting the limit", fed by OnPlan on every economic tick.
//   - Reconciler device-level non-convergence (bus.ReconcileReport
//     NonConvergedBegin/End): "the HARDWARE won't do what the active control
//     asked", fed by OnReport off the retained lexa/reconcile/+/+/report topics.
//
// Arbitration (deliberately simple and explicit — no debounce here; both
// sources already damp onset via the optimizer's expOverTicks counters and the
// reconciler's ConvergeTimeout, and adding a third layer would recreate the
// guard×guard class): an episode BEGINS when EITHER source reports
// non-compliance for a control mRID; it ENDS when ALL sources are clear. The
// meter-level breach, when present, is authoritative for the alert's mRID and
// its limit/measured/shortfall detail; device evidence supplies the mRID only
// when no meter breach is active.
//
// Edge semantics preserved from the former breachAlert exactly (ledger L5):
// one Active alert at onset, one Active alert when the breaching control
// changes to a NEW mRID with no intervening clear (the reject-write/enable-gate
// fix — an mRID-agnostic latch dropped the second control's breach), one
// !Active alert when all sources clear, and — critically — a fast-loop safety
// plan (Plan.Safety) is NOT assessed: its nil Breach must never read as a
// breach-clear edge (2026-07-03 fix), so OnPlan leaves evidence untouched on
// safety plans.
//
// Concurrency: OnPlan runs on the engine's control goroutine; OnReport runs on
// the MQTT subscription goroutine. mu serializes them. Both return the edge
// alerts to publish; the caller (main.go) stamps Ts and publishes outside any
// lock. One writer per state struct (05 §4).
//
// TASK-041 snapshot note: the fields a crash-recovery snapshot must persist to
// stop the duplicate-begin-after-restart noise (§11) are activeMRID, episodeID,
// and counter — restoring them lets a restarted hub recognize the still-open
// episode instead of forming a fresh episodeID for it. The evidence maps
// (planBreach, deviceReports) are re-seeded live from the retained reconciler
// reports + the next optimizer tick, so they do NOT need snapshotting; only the
// episode identity does. (Northbound owns the posted-once flag, in its
// responseTracker.alerted map.)
type breachEpisodes struct {
	mu sync.Mutex

	// activeMRID is the control mRID of the currently-open episode; "" = no
	// episode active. episodeID is the stable identity formed once at onset and
	// reused for the whole episode across both sources; counter guarantees
	// uniqueness across successive episodes. (Snapshot these three — TASK-041.)
	activeMRID string
	episodeID  string
	counter    uint64

	// planBreach is the latest optimizer meter-level breach (nil = the last
	// non-safety plan assessed no breach). Updated ONLY on non-safety plans.
	planBreach *orchestrator.ComplianceBreach

	// deviceReports holds the latest convergence state per reconciled device
	// (keyed by DeviceID). A device contributes to an episode only while
	// nonConverged AND its mrid is non-empty (an empty-mrid device fault with no
	// active control is not a CannotComply — it is logged by the shell, not an
	// episode).
	deviceReports map[string]deviceEvidence

	// jw is the optional TASK-040 event journal. nil disables the
	// breach_begin/breach_end emits in emit() below (fire-and-forget,
	// guarded, matching every other journal call site in cmd/hub).
	jw *journal.Writer

	// snapPath is the TASK-041 snapshot file path ("" disables snapshot
	// writing entirely — no "snapshot" block, or an empty path, in
	// hub.json). Writing is unconditional on snapPath being set; it does NOT
	// depend on the restore-enabled flag (main.go gates restore separately),
	// so the rollout can run a write-only soak campaign first.
	snapPath string
}

// deviceEvidence is one reconciled device's latest non-convergence state.
type deviceEvidence struct {
	mrid         string
	nonConverged bool
	issuedAt     int64 // held desired doc issuedAt — stable half of a device-sourced episode ID
}

// breachEvidence is the merged "what is breaching now" verdict effectiveBreach
// hands emit: the mRID plus enough detail to fill an Active alert.
type breachEvidence struct {
	mrid       string
	limitType  string
	limitW     float64
	measuredW  float64
	shortfallW float64
	reason     string
	issuedAt   int64 // stable half of the episode ID (device doc issuedAt, or onset time for the optimizer)
}

// newBreachEpisodes builds the component. jw is the optional TASK-040 event
// journal (nil disables journaling); a bare nil is safe to pass everywhere
// journaling is not being exercised (e.g. every pre-existing test in
// breach_test.go). snapPath is the optional TASK-041 snapshot file path ("" —
// the zero value every pre-existing test passes implicitly via the two-arg
// call sites below — disables snapshot writing).
func newBreachEpisodes(jw *journal.Writer, snapPath string) *breachEpisodes {
	return &breachEpisodes{deviceReports: make(map[string]deviceEvidence), jw: jw, snapPath: snapPath}
}

// Restore seeds the episode identity from a validated TASK-041 snapshot, so
// the first breaching tick after a restart recognizes the still-open episode
// instead of forming a new one and re-publishing a duplicate Active=true
// alert. Callers must invoke this once, before OnPlan/OnReport are ever
// called (main.go calls it right after construction, before the MQTT
// subscriptions and eng.Start() — no ordering assumption is made about the
// retained-control re-seed racing this).
//
// Only identity is seeded (activeMRID/episodeID/counter) — never the
// evidence maps (planBreach/deviceReports); those re-seed live from the next
// optimizer tick and the retained reconciler reports respectively (see the
// "TASK-041 snapshot note" on the struct doc above). If the restored
// episode's underlying breach turns out to already be gone (plan.Breach ==
// nil on the first tick), the normal clear edge fires correctly: the breach
// may have ended while the hub was down, and northbound's clearAlerts then
// unlatches its own dedupe.
//
// counter is folded in with max(), never overwritten downward: a restored
// counter lower than one this process has already minted (e.g. a very stale
// but still-within-max_age_s snapshot racing a fresh onset) must never cause
// a future episode ID to collide with one already used this process
// lifetime.
func (b *breachEpisodes) Restore(mrid, episodeID string, counter uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if mrid == "" || episodeID == "" {
		return
	}
	b.activeMRID = mrid
	b.episodeID = episodeID
	if counter > b.counter {
		b.counter = counter
	}
}

// snapshotLocked builds the current hubSnapshot. Must be called with mu
// held.
func (b *breachEpisodes) snapshotLocked() hubSnapshot {
	snap := hubSnapshot{WrittenAt: time.Now().Unix()}
	if b.activeMRID != "" {
		snap.ActiveBreach = &breachSnapshot{
			EpisodeID: b.episodeID,
			MRID:      b.activeMRID,
			Counter:   b.counter,
		}
	}
	return snap
}

// ResaveIfActive rewrites the snapshot with a fresh written_at while an
// episode is open (called by main.go's 60 s ticker — TASK-041's "every 60 s
// while a breach is active" cadence). This exists solely so a legitimately
// long-running breach's snapshot does not go stale against max_age_s (a
// breach can easily outlast the default 300 s); it is NOT a per-tick write
// path (RSK-14's "Common mistakes to avoid" — writing on every tick — is
// exactly what this fixed, independent 60 s wall-clock cadence avoids). A
// no-op when no episode is active or no snapshot path is configured.
func (b *breachEpisodes) ResaveIfActive() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.activeMRID == "" || b.snapPath == "" {
		return
	}
	b.writeSnapshotLocked()
}

// writeSnapshotLocked atomically writes the component's current snapshot
// state and journals snapshot_written on success. Must be called with mu
// held. A save failure is logged (edge case: disk full/read-only) but never
// propagated — a snapshot write failing must not affect the breach-episode
// state machine or its bus-facing alerts (mirrors the journal package's own
// "never crash on a journal/snapshot failure" stance, AD-011).
func (b *breachEpisodes) writeSnapshotLocked() {
	if b.snapPath == "" {
		return
	}
	snap := b.snapshotLocked()
	if err := saveHubSnapshot(b.snapPath, snap); err != nil {
		slog.Warn("breach snapshot: save failed", "path", b.snapPath, "err", err)
		return
	}
	b.journalSnapshotWritten(snap)
}

// journalSnapshotWritten appends a snapshot_written event for the snapshot
// just saved (called with b.mu held). Guarded fire-and-forget like every
// other journal call site in this file. Forces an immediate Flush, exactly
// like journalBegin/journalEnd above: this only runs on a begin/end
// transition (never per-tick — writeSnapshotLocked's only callers are the
// two emit() edges and the 60 s while-active resave), so it is the same
// "rare and high-value" class of write those two force-flush for, and
// batching it separately from the breach_begin/breach_end event it
// accompanies would leave a window where a crash could lose the
// snapshot_written record while the breach_begin/end it corresponds to
// already made it to disk.
func (b *breachEpisodes) journalSnapshotWritten(snap hubSnapshot) {
	if b.jw == nil {
		return
	}
	episode := ""
	if snap.ActiveBreach != nil {
		episode = snap.ActiveBreach.EpisodeID
	}
	e, err := journal.NewSnapshotWrittenEvent("hub", journal.NewSnapshot(b.snapPath, episode))
	if err != nil {
		return
	}
	_ = b.jw.Append(e)
	_ = b.jw.Flush()
}

// OnPlan feeds one optimizer plan. Safety plans are not assessed (their nil
// Breach means "not evaluated", never "compliant") so they leave evidence
// untouched and never produce an edge. Returns the edge alerts to publish
// (0 or 1; caller stamps Ts).
func (b *breachEpisodes) OnPlan(plan orchestrator.Plan, now time.Time) []bus.ComplianceAlert {
	b.mu.Lock()
	defer b.mu.Unlock()
	if plan.Safety {
		return nil
	}
	b.planBreach = plan.Breach
	return b.emit(now)
}

// OnReport feeds one reconciler report. Only the two convergence-state kinds
// participate in episodes; every other kind (StaleDesired, Rejected*, SeqReset,
// InterlockHold) is log-only here (the shell already logs it). An empty-mrid
// NonConvergedBegin is dropped (a device fault with no active control is not a
// CannotComply). Returns the edge alerts to publish (0 or 1; caller stamps Ts).
func (b *breachEpisodes) OnReport(rep bus.ReconcileReport, now time.Time) []bus.ComplianceAlert {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch rep.Kind {
	case reconcile.ReportNonConvergedBegin.String():
		if rep.MRID == "" {
			return nil // empty-mrid device fault: log-only, never opens a CSIP episode
		}
		b.deviceReports[rep.DeviceID] = deviceEvidence{
			mrid: rep.MRID, nonConverged: true, issuedAt: rep.IssuedAt,
		}
	case reconcile.ReportNonConvergedEnd.String():
		d := b.deviceReports[rep.DeviceID]
		d.nonConverged = false
		b.deviceReports[rep.DeviceID] = d
	default:
		return nil
	}
	return b.emit(now)
}

// Active reports whether an episode is currently open (drives the
// lexa_hub_breach_active gauge).
func (b *breachEpisodes) Active() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.activeMRID != ""
}

// emit is the arbitration + edge core. Called with mu held. It computes the
// effective breaching mRID from all evidence and returns the edge alert(s):
// an Active alert on onset or mRID-switch, an !Active alert on full clear, or
// nil when nothing changed this pass (so one episode is reported once, not per
// tick). Mirrors the former breachAlert's exact edge semantics.
func (b *breachEpisodes) emit(now time.Time) []bus.ComplianceAlert {
	ev, breaching := b.effectiveBreach(now)
	switch {
	case breaching && ev.mrid != b.activeMRID:
		// Onset, or a NEW control breaches with no intervening clear: open a new
		// episode and re-alert. (mRID-switch re-alert — the reject-write/
		// enable-gate fix; dropping it would drop the second control's breach.)
		b.counter++
		b.activeMRID = ev.mrid
		b.episodeID = fmt.Sprintf("%s@%d#%d", ev.mrid, ev.issuedAt, b.counter)
		b.journalBegin(ev)
		b.writeSnapshotLocked()
		return []bus.ComplianceAlert{{
			Envelope:   bus.Envelope{V: bus.ComplianceAlertV},
			MRID:       ev.mrid,
			LimitType:  ev.limitType,
			LimitW:     ev.limitW,
			MeasuredW:  ev.measuredW,
			ShortfallW: ev.shortfallW,
			Reason:     ev.reason,
			Active:     true,
			EpisodeID:  b.episodeID,
		}}
	case !breaching && b.activeMRID != "":
		// All sources clear: close the episode with one !Active alert.
		ep := b.episodeID
		mrid := b.activeMRID
		b.activeMRID = ""
		b.episodeID = ""
		b.journalEnd(ep, mrid)
		b.writeSnapshotLocked()
		return []bus.ComplianceAlert{{
			Envelope:  bus.Envelope{V: bus.ComplianceAlertV},
			MRID:      mrid,
			Active:    false,
			EpisodeID: ep,
		}}
	default:
		return nil // no edge this pass (continuing episode, or clear with none active)
	}
}

// journalBegin/journalEnd append breach_begin/breach_end evidence for the
// episode edge emit just computed (called with b.mu held). Guarded
// fire-and-forget like every other journal call site; construction
// (marshal) errors are practically unreachable here (every Breach field is
// a plain string/float64) so are silently dropped rather than logged twice
// (Append's own failure path already edge-triggers a log — see
// internal/journal's doc). Breach transitions are rare and high-value, so
// — unlike the dispatch/adoption paths — this also forces an immediate
// Flush (TASK-040's "Common mistakes to avoid": never flush from a
// tick/dispatch path, but a breach edge is exactly the kind of transition
// worth the extra fsync).
func (b *breachEpisodes) journalBegin(ev breachEvidence) {
	if b.jw == nil {
		return
	}
	e, err := journal.NewBreachBeginEvent("hub", journal.NewBreach(b.episodeID, ev.mrid, ev.limitType, ev.limitW, ev.measuredW, ev.shortfallW, ev.reason))
	if err != nil {
		return
	}
	_ = b.jw.Append(e)
	_ = b.jw.Flush()
}

func (b *breachEpisodes) journalEnd(episodeID, mrid string) {
	if b.jw == nil {
		return
	}
	e, err := journal.NewBreachEndEvent("hub", journal.NewBreach(episodeID, mrid, "", 0, 0, 0, "cleared"))
	if err != nil {
		return
	}
	_ = b.jw.Append(e)
	_ = b.jw.Flush()
}

// effectiveBreach merges the evidence into a single verdict. The optimizer's
// meter-level breach, when present, is authoritative (it carries the mRID and
// the limit/measured/shortfall detail). Otherwise device-level non-convergence
// supplies the mRID: episode continuity is preferred (a device still
// non-converged under the active episode's mRID keeps that mRID), else the
// lowest mRID among non-converged devices is chosen deterministically.
func (b *breachEpisodes) effectiveBreach(now time.Time) (breachEvidence, bool) {
	if b.planBreach != nil {
		pb := b.planBreach
		return breachEvidence{
			mrid: pb.MRID, limitType: pb.LimitType, limitW: pb.LimitW,
			measuredW: pb.MeasuredW, shortfallW: pb.ShortfallW, reason: pb.Reason,
			issuedAt: now.Unix(),
		}, true
	}
	best := ""
	var bestIssued int64
	for _, dr := range b.deviceReports {
		if !dr.nonConverged || dr.mrid == "" {
			continue
		}
		if dr.mrid == b.activeMRID {
			best, bestIssued = dr.mrid, dr.issuedAt
			break // continuity: keep the active episode's mRID
		}
		if best == "" || dr.mrid < best {
			best, bestIssued = dr.mrid, dr.issuedAt
		}
	}
	if best == "" {
		return breachEvidence{}, false
	}
	return breachEvidence{
		mrid: best, limitType: "device",
		reason:   "device not converged (reconciler)",
		issuedAt: bestIssued,
	}, true
}
