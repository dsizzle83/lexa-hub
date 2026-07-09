package responses

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"lexa-hub/internal/utilitytime"
	model "lexa-proto/csipmodel"
)

// ---------------------------------------------------------------------
// WS-4.2: persist.go — Store/LoadState unit tests
// ---------------------------------------------------------------------

// TestPersist_PostedSurvivesRestart_NoDuplicateReceived is the HANDOFF
// acceptance test: "post/alert → restart (new tracker from same path) → no
// duplicate POST for the same event/mrid." E1 is received but not yet
// active; a restarted Tracker replaying the identical tree must not
// re-post Received for it, because the restored posted map already has it.
func TestPersist_PostedSurvivesRestart_NoDuplicateReceived(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.ndjson")

	store1, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	fp1 := &fakePoster{}
	rt1 := New(fp1, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil, store1, State{})
	rt1.Update(treeWith(ctrl("E1", 0)), nil, nil) // received only, not (yet) active
	if err := store1.Close(); err != nil {
		t.Fatalf("store1.Close: %v", err)
	}
	eq(t, fp1.statusesFor("E1"), model.ResponseEventReceived)

	// "Restart": a fresh Tracker loads persisted state from the same path.
	initial, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if initial.Posted["E1"] != model.ResponseEventReceived {
		t.Fatalf("restored posted[E1] = %v, want Received", initial.Posted["E1"])
	}
	store2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore (restart): %v", err)
	}
	defer store2.Close()
	fp2 := &fakePoster{}
	rt2 := New(fp2, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil, store2, initial)
	rt2.Update(treeWith(ctrl("E1", 0)), nil, nil) // same tree again

	if got := fp2.statusesFor("E1"); len(got) != 0 {
		t.Fatalf("restarted tracker re-posted %v for an already-received event", got)
	}
}

// TestPersist_CannotComplyRecordedButUnpostedRetriesAfterRestart is the
// HANDOFF acceptance test: "a CannotComply recorded-but-unPOSTed survives
// restart and POSTs exactly once." Run 1's POST fails (network/utility-side
// error) — AlertCannotComply's in-memory dedupe marks it "seen" for the
// rest of that process's life, but WS-4.2 must NOT persist that mark until
// the POST is confirmed, precisely so a restart gets a genuine retry
// instead of a false "already handled."
func TestPersist_CannotComplyRecordedButUnpostedRetriesAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.ndjson")

	store1, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	rt1 := New(failingPoster{}, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil, store1, State{})
	rt1.AlertCannotComply("E20", "E20@100#1") // POST fails
	if err := store1.Close(); err != nil {
		t.Fatalf("store1.Close: %v", err)
	}

	initial, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if initial.Alerted["E20"] {
		t.Fatalf("a failed POST must not be persisted as alerted; got alerted[E20]=true")
	}

	// "Restart": fresh Tracker, same path, this time the POST succeeds.
	store2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore (restart): %v", err)
	}
	defer store2.Close()
	fp2 := &fakePoster{}
	rt2 := New(fp2, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil, store2, initial)
	rt2.AlertCannotComply("E20", "E20@100#1")
	rt2.AlertCannotComply("E20", "E20@100#1") // redelivered — must not double-post

	if got := fp2.statusesFor("E20"); len(got) != 1 || got[0] != model.ResponseCannotComply {
		t.Fatalf("CannotComply after restart = %v, want exactly one CannotComply POST", got)
	}
}

// TestPersist_LoadMissingFileStartsEmpty verifies LoadState passes through
// os.ErrNotExist unwrapped for a never-written path (first boot / fresh
// volume), same convention as cmd/hub/snapshot.go's loadHubSnapshot.
func TestPersist_LoadMissingFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.ndjson")
	st, err := LoadState(path)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadState(missing) err = %v, want os.ErrNotExist", err)
	}
	if len(st.Posted) != 0 || len(st.Alerted) != 0 {
		t.Fatalf("LoadState(missing) state = %+v, want empty", st)
	}
}

// TestPersist_LoadCorruptFileStartsEmpty verifies a file that is present
// but contains no valid entries at all is rejected with ErrStateCorrupt
// rather than silently misparsed — the caller (cmd/northbound/main.go)
// turns this into a WARN and starts empty, never fatal (AD-011).
func TestPersist_LoadCorruptFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.ndjson")
	if err := os.WriteFile(path, []byte("not json at all\n{also not json\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	st, err := LoadState(path)
	if !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("LoadState(corrupt) err = %v, want ErrStateCorrupt", err)
	}
	if len(st.Posted) != 0 || len(st.Alerted) != 0 {
		t.Fatalf("LoadState(corrupt) state = %+v, want empty", st)
	}
}

// TestPersist_TornTailLineTolerated verifies a single torn trailing line
// (the on-disk signature of a crash mid-write) is tolerated silently — the
// well-formed entries before it still replay — matching
// internal/journal's resumeSeq/reader.Scan discipline.
func TestPersist_TornTailLineTolerated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.ndjson")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	store.AppendPosted("E21", model.ResponseEventStarted)
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := f.WriteString(`{"v":1,"ts":1,"op":"posted","mrid":"E22","stat`); err != nil { // torn
		t.Fatalf("write torn line: %v", err)
	}
	f.Close()

	st, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState(torn tail) err = %v, want nil (tolerated)", err)
	}
	if st.Posted["E21"] != model.ResponseEventStarted {
		t.Fatalf("posted[E21] = %v, want Started (the well-formed line before the torn tail)", st.Posted["E21"])
	}
	if _, ok := st.Posted["E22"]; ok {
		t.Fatalf("torn line E22 must not appear in restored state")
	}
}

// TestPersist_PruningDropsTerminalPostedEntries drives E23 to a terminal
// status (Completed) and verifies a fresh LoadState does not restore it —
// "entries for expired events get pruned" using terminalResponse, the
// event-lifecycle signal the tracker already has (see persist.go's
// applyEntry doc).
func TestPersist_PruningDropsTerminalPostedEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.ndjson")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	fp := &fakePoster{}
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil, store, State{})

	rt.Update(treeWith(ctrl("E23", 0)), eventActive("E23"), nil) // received + started
	rt.Update(treeWith(ctrl("E23", 0)), nil, nil)                // event gone → completed
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	eq(t, fp.statusesFor("E23"),
		model.ResponseEventReceived, model.ResponseEventStarted, model.ResponseEventCompleted)

	st, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if _, ok := st.Posted["E23"]; ok {
		t.Fatalf("terminal (Completed) entry E23 must be pruned on restore, got %v", st.Posted["E23"])
	}
}

// TestStore_CompactBoundsFileSize verifies the self-compaction path: once
// CompactBytes is crossed, Compact rewrites the file down to a single
// checkpoint line, and the checkpoint alone reconstructs the same live
// state as replaying every line that preceded it would have.
func TestStore_CompactBoundsFileSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.ndjson")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	store.CompactBytes = 200 // tiny, so a handful of appends cross it
	defer store.Close()

	// One tree carrying 10 simultaneously-received (never active) events:
	// each posts Received (non-terminal) via pass 1, and with active==nil
	// pass 2's completeActive() stays a no-op throughout (activeMRID never
	// set), so all 10 stay live rather than completing one another —
	// unlike driving them one "active" event at a time, which would
	// auto-complete each prior one via activeMRID's single-slot tracking.
	fp := &fakePoster{}
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil, store, State{})
	ctrls := make([]model.DERControl, 10)
	for i := range ctrls {
		ctrls[i] = ctrl(fmt.Sprintf("E30%d", i), 0)
	}
	rt.Update(treeWith(ctrls...), nil, nil)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() >= 2000 { // ~10 uncompacted lines would exceed this comfortably
		t.Fatalf("state file size = %d bytes, want bounded by compaction (<2000)", info.Size())
	}

	st, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	for i := range ctrls {
		mrid := fmt.Sprintf("E30%d", i)
		if st.Posted[mrid] != model.ResponseEventReceived {
			t.Fatalf("posted[%s] = %v, want Received (compaction must preserve live state)", mrid, st.Posted[mrid])
		}
	}
}

// TestStore_NilIsNoop verifies every Store method is safe to call on a nil
// *Store (matches metrics.Counter/journal.Writer's nil-receiver
// convention) — a Tracker constructed with store=nil (persistence
// disabled/unavailable) must behave exactly as before WS-4.2.
func TestStore_NilIsNoop(t *testing.T) {
	var s *Store
	s.AppendPosted("E1", model.ResponseEventReceived)
	s.AppendAlerted("E1", "")
	s.AppendClear()
	if s.NeedsCompact() {
		t.Fatal("nil *Store.NeedsCompact() = true, want false")
	}
	if err := s.Compact(nil, nil); err != nil {
		t.Fatalf("nil *Store.Compact() = %v, want nil", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("nil *Store.Close() = %v, want nil", err)
	}
}

// TestPersist_ClearAlertsResetsPersistedState verifies ClearAlerts's
// persisted "clear" op actually wipes a previously-confirmed alert: after
// clear, a restarted tracker must treat the same mRID as a genuinely new
// episode (dedupe re-arms), matching the in-memory contract
// TestResponse_CannotComplyOncePerEpisode already covers for the RAM case.
func TestPersist_ClearAlertsResetsPersistedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.ndjson")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	fp := &fakePoster{}
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil, store, State{})
	rt.AlertCannotComply("E24", "E24@1#1")
	rt.ClearAlerts()
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	st, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(st.Alerted) != 0 {
		t.Fatalf("alerted state after ClearAlerts = %+v, want empty", st.Alerted)
	}
}
