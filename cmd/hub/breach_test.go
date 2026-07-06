package main

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/journal"
	"lexa-hub/internal/orchestrator"
	"lexa-hub/internal/reconcile"
)

var testNow = time.Unix(1700000000, 0)

func breachPlan(mrid string) orchestrator.Plan {
	return orchestrator.Plan{Breach: &orchestrator.ComplianceBreach{
		MRID: mrid, LimitType: "generation", LimitW: 1000, MeasuredW: 4650, ShortfallW: 3650,
	}}
}

func nonConverged(dev, mrid string) bus.ReconcileReport {
	return bus.ReconcileReport{
		Kind: reconcile.ReportNonConvergedBegin.String(), DeviceClass: "battery",
		DeviceID: dev, MRID: mrid, IssuedAt: 1700000042,
	}
}

func converged(dev, mrid string) bus.ReconcileReport {
	return bus.ReconcileReport{
		Kind: reconcile.ReportNonConvergedEnd.String(), DeviceClass: "battery",
		DeviceID: dev, MRID: mrid,
	}
}

// one asserts a single edge alert was returned and returns it.
func one(t *testing.T, alerts []bus.ComplianceAlert) bus.ComplianceAlert {
	t.Helper()
	if len(alerts) != 1 {
		t.Fatalf("expected exactly one edge alert, got %d: %+v", len(alerts), alerts)
	}
	return alerts[0]
}

// none asserts no edge alert was returned.
func none(t *testing.T, alerts []bus.ComplianceAlert) {
	t.Helper()
	if len(alerts) != 0 {
		t.Fatalf("expected no edge alert, got %+v", alerts)
	}
}

// Ported from breachalert_test.go (TestBreachAlert): the component must publish
// once per control episode but NEVER miss a new control's breach because a
// previous control was still breaching (the reject-write/enable-gate-curtail
// flakiness — an mRID-agnostic latch dropped the second scenario's alert).
func TestBreachEpisodes_OnsetSwitchClear(t *testing.T) {
	b := newBreachEpisodes(nil, "")

	// First breach (control A) → publish Active alert for A, with an episode ID.
	a := one(t, b.OnPlan(breachPlan("A"), testNow))
	if !a.Active || a.MRID != "A" {
		t.Fatalf("first breach should publish an active alert for A, got %+v", a)
	}
	if a.EpisodeID == "" {
		t.Fatalf("onset alert must carry an episode ID, got %+v", a)
	}
	epA := a.EpisodeID
	if !b.Active() {
		t.Fatalf("episode should be active after onset")
	}

	// Same breach continues → no re-publish (one per episode).
	none(t, b.OnPlan(breachPlan("A"), testNow))

	// A DIFFERENT control breaches with NO intervening clear → must publish for B
	// (the bug: an mRID-agnostic flag would stay latched and drop this).
	a = one(t, b.OnPlan(breachPlan("B"), testNow))
	if !a.Active || a.MRID != "B" {
		t.Fatalf("a new control's breach must publish even while one was active, got %+v", a)
	}
	if a.EpisodeID == epA {
		t.Fatalf("an mRID switch must open a NEW episode ID, got same %q", a.EpisodeID)
	}

	// Breach clears → publish a clear.
	a = one(t, b.OnPlan(orchestrator.Plan{Breach: nil}, testNow))
	if a.Active {
		t.Fatalf("clearing breach should publish an inactive alert, got %+v", a)
	}
	if b.Active() {
		t.Fatalf("episode should be inactive after clear")
	}

	// No breach, none active → nothing.
	none(t, b.OnPlan(orchestrator.Plan{Breach: nil}, testNow))
}

// Ported from breachalert_test.go (TestBreachAlert_SafetyPlanHoldsEpisode). A
// fast-loop safety plan carries no breach evaluation; its nil Breach must NOT
// read as a clear edge (2026-07-03 fix), or a wrong-sign disconnect firing
// between economic ticks mid-breach would publish a spurious "compliance
// restored" to the grid server.
//
// This is also the Safety-guard mutation test (05 §8): remove the `if
// plan.Safety { return nil }` guard in OnPlan and this test fails — the safety
// plan's nil Breach would clear the still-open episode A.
func TestBreachEpisodes_SafetyPlanHoldsEpisode(t *testing.T) {
	b := newBreachEpisodes(nil, "")
	one(t, b.OnPlan(breachPlan("A"), testNow)) // open episode A

	// Safety plan mid-episode: no edge, episode preserved.
	none(t, b.OnPlan(orchestrator.Plan{Safety: true}, testNow))
	if !b.Active() {
		t.Fatalf("safety plan must preserve the active episode")
	}

	// A following economic tick with the breach still present must NOT re-alert
	// (proves the safety plan did not silently clear the evidence).
	none(t, b.OnPlan(breachPlan("A"), testNow))

	// Safety plan with no episode active is equally a no-op.
	b2 := newBreachEpisodes(nil, "")
	none(t, b2.OnPlan(orchestrator.Plan{Safety: true}, testNow))
	if b2.Active() {
		t.Fatalf("safety plan with no episode must stay inactive")
	}
}

// A device that refuses while the meter is fine opens an episode from the
// reconciler evidence alone, and clears when it converges.
func TestBreachEpisodes_ReconcilerOnlyEpisode(t *testing.T) {
	b := newBreachEpisodes(nil, "")

	a := one(t, b.OnReport(nonConverged("bat0", "X"), testNow))
	if !a.Active || a.MRID != "X" || a.EpisodeID == "" {
		t.Fatalf("reconciler non-convergence should open an episode for X, got %+v", a)
	}

	// Redelivered retained NonConvergedBegin (restart-shaped replay) → no new edge.
	none(t, b.OnReport(nonConverged("bat0", "X"), testNow))

	// Device converges → clear.
	a = one(t, b.OnReport(converged("bat0", "X"), testNow))
	if a.Active {
		t.Fatalf("device convergence should clear the episode, got %+v", a)
	}
}

// Both sources reporting the same control form ONE episode: one begin, one end
// only when BOTH have cleared.
func TestBreachEpisodes_BothSourcesOneEpisode(t *testing.T) {
	b := newBreachEpisodes(nil, "")

	// Optimizer opens episode A.
	a := one(t, b.OnPlan(breachPlan("A"), testNow))
	epA := a.EpisodeID

	// Device also reports non-convergence for the same control → NO new edge
	// (same episode).
	none(t, b.OnReport(nonConverged("bat0", "A"), testNow))

	// Optimizer clears, but the device is still non-converged → episode HOLDS,
	// no clear edge.
	none(t, b.OnPlan(orchestrator.Plan{Breach: nil}, testNow))
	if !b.Active() {
		t.Fatalf("episode must stay open while the device is still non-converged")
	}

	// Device converges → NOW the episode clears (one end), same episode ID.
	a = one(t, b.OnReport(converged("bat0", "A"), testNow))
	if a.Active || a.EpisodeID != epA {
		t.Fatalf("clear should close the same episode %q, got %+v", epA, a)
	}
}

// An empty-mRID device fault (no active control) is NOT a CannotComply: it must
// not open a CSIP episode.
func TestBreachEpisodes_EmptyMRIDNoEpisode(t *testing.T) {
	b := newBreachEpisodes(nil, "")
	none(t, b.OnReport(nonConverged("bat0", ""), testNow))
	if b.Active() {
		t.Fatalf("empty-mRID device fault must not open an episode")
	}
}

// The episode ID formed at onset is reused for the whole episode across sources.
func TestBreachEpisodes_EpisodeIDStable(t *testing.T) {
	b := newBreachEpisodes(nil, "")
	open := one(t, b.OnPlan(breachPlan("A"), testNow))
	clear := one(t, b.OnPlan(orchestrator.Plan{Breach: nil}, testNow))
	if open.EpisodeID != clear.EpisodeID || open.EpisodeID == "" {
		t.Fatalf("onset and clear must carry the same non-empty episode ID, got %q / %q",
			open.EpisodeID, clear.EpisodeID)
	}
}

// ---------------------------------------------------------------------
// TASK-040: journal wiring (breach_begin/breach_end, one episode ID shared)
// ---------------------------------------------------------------------

// TestBreachEpisodes_JournalsBeginEndSameEpisode is the journal-side sibling
// of TestBreachEpisodes_EpisodeIDStable: the episode ID minted at onset must
// be the one carried on both the breach_begin AND breach_end journal
// entries, matching the ComplianceAlert edges above bit-for-bit.
func TestBreachEpisodes_JournalsBeginEndSameEpisode(t *testing.T) {
	dir := t.TempDir()
	jw, err := journal.Open(journal.Config{Dir: dir})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	defer jw.Close()

	b := newBreachEpisodes(jw, "")
	alert := one(t, b.OnPlan(breachPlan("A"), testNow))
	one(t, b.OnPlan(orchestrator.Plan{Breach: nil}, testNow))

	events := journalEventsByType(t, dir)
	begin := events[journal.TypeBreachBegin]
	end := events[journal.TypeBreachEnd]
	if len(begin) != 1 || len(end) != 1 {
		t.Fatalf("breach_begin=%d breach_end=%d, want 1/1", len(begin), len(end))
	}
	var bp, ep journal.Breach
	if err := json.Unmarshal(begin[0].Data, &bp); err != nil {
		t.Fatalf("unmarshal begin Breach: %v", err)
	}
	if err := json.Unmarshal(end[0].Data, &ep); err != nil {
		t.Fatalf("unmarshal end Breach: %v", err)
	}
	if bp.EpisodeID == "" || bp.EpisodeID != ep.EpisodeID || bp.EpisodeID != alert.EpisodeID {
		t.Fatalf("begin/end/alert episode IDs = %q / %q / %q, want all equal and non-empty",
			bp.EpisodeID, ep.EpisodeID, alert.EpisodeID)
	}
	if bp.MRID != "A" || bp.LimitType != "generation" || bp.LimitW != 1000 {
		t.Fatalf("begin Breach payload = %+v, want mrid=A limit_type=generation limit_w=1000", bp)
	}
}

// TestBreachEpisodes_JournalsNothingOnContinuingEpisode verifies the
// "one begin per episode, not per tick" write-budget property: a breach that
// continues across several OnPlan calls must journal exactly one
// breach_begin, matching the ComplianceAlert edge semantics (no re-alert
// while the same mRID keeps breaching).
func TestBreachEpisodes_JournalsNothingOnContinuingEpisode(t *testing.T) {
	dir := t.TempDir()
	jw, err := journal.Open(journal.Config{Dir: dir})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	defer jw.Close()

	b := newBreachEpisodes(jw, "")
	one(t, b.OnPlan(breachPlan("A"), testNow))
	none(t, b.OnPlan(breachPlan("A"), testNow))
	none(t, b.OnPlan(breachPlan("A"), testNow))

	events := journalEventsByType(t, dir)
	if got := len(events[journal.TypeBreachBegin]); got != 1 {
		t.Fatalf("breach_begin events = %d, want 1 (continuing episode must not re-journal)", got)
	}
}

// TestBreachEpisodes_NilJournalIsNoop verifies a nil Writer (no "journal"
// config block) never panics and leaves the component's own edge semantics
// untouched — the acceptance criterion that services behave exactly as
// before with journaling disabled.
func TestBreachEpisodes_NilJournalIsNoop(t *testing.T) {
	b := newBreachEpisodes(nil, "")
	a := one(t, b.OnPlan(breachPlan("A"), testNow))
	if !a.Active || a.EpisodeID == "" {
		t.Fatalf("nil journal must not affect the component's own alert output, got %+v", a)
	}
}

// ---------------------------------------------------------------------
// TASK-041: snapshot + restore-on-start (restart-mid-breach)
// ---------------------------------------------------------------------

// TestBreachEpisodes_RestartMidBreach_NoDuplicateBegin is the core TASK-041
// goal test: a component that opened episode A, "restarted" (a fresh
// component seeded only from a Restore call, exactly like main.go's
// restore-before-eng.Start() path), must NOT re-emit a duplicate Active=true
// alert when fed the SAME still-breaching plan again — and must still emit
// exactly one Active=false when the breach genuinely clears afterward.
func TestBreachEpisodes_RestartMidBreach_NoDuplicateBegin(t *testing.T) {
	dir := t.TempDir()
	snapPath := dir + "/hub-snapshot.json"

	// "Before restart": open episode A, which writes a snapshot at onset.
	before := newBreachEpisodes(nil, snapPath)
	onset := one(t, before.OnPlan(breachPlan("A"), testNow))
	if !onset.Active || onset.MRID != "A" {
		t.Fatalf("onset alert = %+v, want an Active alert for A", onset)
	}
	epA := onset.EpisodeID

	snap, err := loadHubSnapshot(snapPath, 300*time.Second, time.Now())
	if err != nil {
		t.Fatalf("loadHubSnapshot after onset: %v", err)
	}
	if snap.ActiveBreach == nil || snap.ActiveBreach.EpisodeID != epA || snap.ActiveBreach.MRID != "A" {
		t.Fatalf("snapshot after onset = %+v, want episode %q mrid A", snap.ActiveBreach, epA)
	}

	// "Restart": a brand new component (fresh process), seeded ONLY via
	// Restore — never told about `before`'s in-memory state directly. This
	// is exactly main.go's shape: construct, then Restore from the loaded
	// snapshot, before any OnPlan/OnReport call.
	after := newBreachEpisodes(nil, snapPath)
	after.Restore(snap.ActiveBreach.MRID, snap.ActiveBreach.EpisodeID, snap.ActiveBreach.Counter)
	if !after.Active() {
		t.Fatalf("restored component must report the episode active")
	}

	// The first economic tick after restart re-observes the SAME still-open
	// breach (the realistic case: the plant didn't magically fix itself
	// during the restart window). This must NOT re-alert.
	none(t, after.OnPlan(breachPlan("A"), testNow))

	// A later tick where the breach has genuinely cleared while the hub was
	// down must still produce exactly one clear edge, carrying the SAME
	// restored episode ID (so northbound's Completed Response, if it also
	// restored, correlates to the right episode).
	cleared := one(t, after.OnPlan(orchestrator.Plan{Breach: nil}, testNow))
	if cleared.Active {
		t.Fatalf("clear alert = %+v, want Active=false", cleared)
	}
	if cleared.EpisodeID != epA {
		t.Fatalf("clear episode ID = %q, want the restored episode %q", cleared.EpisodeID, epA)
	}
}

// TestBreachEpisodes_RestartMidBreach_NewMRIDStillAlerts is the ledger
// preservation check (main.go:89-98 comment / breach.go's mRID-switch
// re-alert): even a restored, still-latched episode for mRID A must not
// suppress an alert for a genuinely NEW breaching mRID B with no intervening
// clear.
func TestBreachEpisodes_RestartMidBreach_NewMRIDStillAlerts(t *testing.T) {
	after := newBreachEpisodes(nil, "")
	after.Restore("A", "A@1700000000#1", 1)
	if !after.Active() {
		t.Fatalf("restored episode must be active")
	}

	a := one(t, after.OnPlan(breachPlan("B"), testNow))
	if !a.Active || a.MRID != "B" {
		t.Fatalf("a new mRID breaching after a restored episode must still alert, got %+v", a)
	}
	if a.EpisodeID == "A@1700000000#1" {
		t.Fatalf("the new mRID's episode ID must not collide with the restored one, got %q", a.EpisodeID)
	}
}

// TestBreachEpisodes_Restore_CounterNeverGoesDown proves Restore folds the
// snapshot's counter in with max(), so a future episode ID minted after
// restore can never reuse a counter value (and thus never collide with an
// episode ID) already used before the restart.
func TestBreachEpisodes_Restore_CounterNeverGoesDown(t *testing.T) {
	b := newBreachEpisodes(nil, "")
	b.Restore("A", "A@1700000000#5", 5)

	// Clear (no episode was really live in this fresh process for A per se,
	// but Restore seeded activeMRID=A, so a nil-breach plan clears it) then
	// open a brand new episode — its counter must be 6, not 1.
	one(t, b.OnPlan(orchestrator.Plan{Breach: nil}, testNow)) // clears the restored episode
	next := one(t, b.OnPlan(breachPlan("Z"), testNow))
	want := "Z@" + strconv.FormatInt(testNow.Unix(), 10) + "#6"
	if next.EpisodeID != want {
		t.Fatalf("post-restore episode ID = %q, want %q (counter must never be reused)", next.EpisodeID, want)
	}
}

// TestBreachEpisodes_Restore_EmptyIsNoop proves Restore with empty
// identity fields (no snapshot found, or one with no ActiveBreach) never
// marks an episode active — the ordinary "clean start" path.
func TestBreachEpisodes_Restore_EmptyIsNoop(t *testing.T) {
	b := newBreachEpisodes(nil, "")
	b.Restore("", "", 0)
	if b.Active() {
		t.Fatalf("Restore with empty mrid/episodeID must not activate an episode")
	}
}

// TestBreachEpisodes_WriteSnapshot_TransitionsOnly proves a snapshot is
// (re)written on begin AND end, but NOT on a continuing/no-edge tick (RSK-14
// — "writing on every tick" is the mistake this guards against).
func TestBreachEpisodes_WriteSnapshot_TransitionsOnly(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/hub-snapshot.json"
	b := newBreachEpisodes(nil, path)

	one(t, b.OnPlan(breachPlan("A"), testNow)) // begin: snapshot written
	info1, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after begin: %v", err)
	}

	none(t, b.OnPlan(breachPlan("A"), testNow)) // continuing: no edge
	none(t, b.OnPlan(breachPlan("A"), testNow)) // continuing: no edge
	info2, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after continuing ticks: %v", err)
	}
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Fatalf("snapshot mtime changed on a continuing (no-edge) tick: %v -> %v", info1.ModTime(), info2.ModTime())
	}

	one(t, b.OnPlan(orchestrator.Plan{Breach: nil}, testNow)) // end: snapshot rewritten
	snap, err := loadHubSnapshot(path, 300*time.Second, time.Now())
	if err != nil {
		t.Fatalf("loadHubSnapshot after end: %v", err)
	}
	if snap.ActiveBreach != nil {
		t.Fatalf("snapshot after the clearing edge must have no ActiveBreach, got %+v", snap.ActiveBreach)
	}
}

// TestBreachEpisodes_ResaveIfActive_NoEpisodeIsNoop proves the 60 s
// while-active resave ticker's target method is a true no-op with no
// episode open (main.go calls this unconditionally every 60 s; it must never
// create a spurious snapshot file when nothing is active).
func TestBreachEpisodes_ResaveIfActive_NoEpisodeIsNoop(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/hub-snapshot.json"
	b := newBreachEpisodes(nil, path)
	b.ResaveIfActive()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("ResaveIfActive must not create a snapshot file when no episode is active")
	}
}

// TestBreachEpisodes_ResaveIfActive_RefreshesWrittenAt proves the periodic
// resave keeps a long-running breach's snapshot fresh against max_age_s
// (otherwise a breach outlasting max_age_s would make its own snapshot look
// stale to a LATER restart, defeating the whole feature for exactly the
// breaches most worth surviving a restart for).
func TestBreachEpisodes_ResaveIfActive_RefreshesWrittenAt(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/hub-snapshot.json"
	b := newBreachEpisodes(nil, path)
	one(t, b.OnPlan(breachPlan("A"), testNow)) // onset: written_at ~ time.Now()

	snap1, err := loadHubSnapshot(path, 300*time.Second, time.Now())
	if err != nil {
		t.Fatalf("loadHubSnapshot after onset: %v", err)
	}

	time.Sleep(1100 * time.Millisecond) // clear the 1 s written_at resolution
	b.ResaveIfActive()
	snap2, err := loadHubSnapshot(path, 300*time.Second, time.Now())
	if err != nil {
		t.Fatalf("loadHubSnapshot after ResaveIfActive: %v", err)
	}
	if snap2.WrittenAt <= snap1.WrittenAt {
		t.Fatalf("ResaveIfActive must advance written_at, got %d -> %d", snap1.WrittenAt, snap2.WrittenAt)
	}
	if snap2.ActiveBreach == nil || snap2.ActiveBreach.EpisodeID != snap1.ActiveBreach.EpisodeID {
		t.Fatalf("ResaveIfActive must not change the episode identity, got %+v -> %+v", snap1.ActiveBreach, snap2.ActiveBreach)
	}
}

// TestBreachEpisodes_JournalsSnapshotWritten proves a snapshot_written event
// is journaled on the same begin/end transitions the snapshot file itself is
// written on (TASK-040's event vocabulary, wired here per TASK-041).
func TestBreachEpisodes_JournalsSnapshotWritten(t *testing.T) {
	dir := t.TempDir()
	jw, err := journal.Open(journal.Config{Dir: dir})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	defer jw.Close()

	snapPath := dir + "/hub-snapshot.json"
	b := newBreachEpisodes(jw, snapPath)
	one(t, b.OnPlan(breachPlan("A"), testNow))
	one(t, b.OnPlan(orchestrator.Plan{Breach: nil}, testNow))

	events := journalEventsByType(t, dir)
	written := events[journal.TypeSnapshotWritten]
	if len(written) != 2 {
		t.Fatalf("snapshot_written events = %d, want 2 (one per begin/end transition)", len(written))
	}
	var first, second journal.Snapshot
	if err := json.Unmarshal(written[0].Data, &first); err != nil {
		t.Fatalf("unmarshal first snapshot_written: %v", err)
	}
	if err := json.Unmarshal(written[1].Data, &second); err != nil {
		t.Fatalf("unmarshal second snapshot_written: %v", err)
	}
	if first.Path != snapPath || second.Path != snapPath {
		t.Fatalf("snapshot_written path = %q / %q, want %q", first.Path, second.Path, snapPath)
	}
	if first.BreachEpisode == "" {
		t.Fatalf("snapshot_written at begin must carry the episode ID, got empty")
	}
}

// TestBreachEpisodes_NoSnapshotPathIsNoop proves an empty snapPath (no
// "snapshot" block in hub.json) never creates a file and never panics —
// matching Journal's own true-no-op rollout default.
func TestBreachEpisodes_NoSnapshotPathIsNoop(t *testing.T) {
	b := newBreachEpisodes(nil, "")
	one(t, b.OnPlan(breachPlan("A"), testNow))
	one(t, b.OnPlan(orchestrator.Plan{Breach: nil}, testNow))
	// No assertion beyond "did not panic" — there is no path to check.
}
