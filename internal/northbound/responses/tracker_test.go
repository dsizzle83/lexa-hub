package responses

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"testing"

	"lexa-hub/internal/journal"
	"lexa-hub/internal/northbound/discovery"
	"lexa-hub/internal/northbound/scheduler"
	"lexa-hub/internal/utilitytime"
	model "lexa-proto/csipmodel"
)

// fakePoster records every Response the tracker POSTs, decoding the XML body
// back into (subject, status) pairs so tests can assert the exact sequence.
type fakePoster struct{ calls []postCall }

type postCall struct {
	subject string
	status  uint8
}

func (f *fakePoster) Post(path string, body []byte, contentType string) ([]byte, string, error) {
	var r model.Response
	_ = xml.Unmarshal(body, &r)
	f.calls = append(f.calls, postCall{r.Subject, r.Status})
	return nil, "", nil
}

func (f *fakePoster) statusesFor(mrid string) []uint8 {
	var out []uint8
	for _, c := range f.calls {
		if c.subject == mrid {
			out = append(out, c.status)
		}
	}
	return out
}

// treeWith builds a one-program ResourceTree containing the given controls.
func treeWith(ctrls ...model.DERControl) *discovery.ResourceTree {
	return &discovery.ResourceTree{
		ClockOffset: 0,
		Programs: []discovery.ProgramState{{
			Program:  model.DERProgram{MRID: "SP", Primacy: 1},
			Controls: &model.DERControlList{DERControl: ctrls},
		}},
	}
}

func ctrl(mrid string, status uint8) model.DERControl {
	return model.DERControl{
		MRID:        mrid,
		EventStatus: &model.EventStatus{CurrentStatus: status},
		Interval:    model.DateTimeInterval{Start: 1700000000, Duration: 600},
	}
}

func eventActive(mrid string) *scheduler.ActiveControl {
	// ValidUntil far in the future so Update() never auto-completes it.
	return &scheduler.ActiveControl{Source: "event", MRID: mrid, ValidUntil: 1 << 40}
}

func eq(t *testing.T, got []uint8, want ...uint8) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("status sequence = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("status sequence = %v, want %v", got, want)
		}
	}
}

// Received(1) → Started(2) → Completed(3) over three poll cycles.
func TestResponse_ReceivedStartedCompleted(t *testing.T) {
	fp := &fakePoster{}
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil)

	rt.Update(treeWith(ctrl("E1", 0)), eventActive("E1"), nil) // cycle 1: received+started
	rt.Update(treeWith(ctrl("E1", 0)), eventActive("E1"), nil) // cycle 2: no change
	rt.Update(treeWith(ctrl("E1", 0)), nil, nil)               // cycle 3: event gone → completed

	eq(t, fp.statusesFor("E1"),
		model.ResponseEventReceived, model.ResponseEventStarted, model.ResponseEventCompleted)
}

// CORE-022 step 7: an event the client received, then the server cancels,
// must be acknowledged with status=6 — exactly once.
func TestResponse_CancelledAfterReceived(t *testing.T) {
	fp := &fakePoster{}
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil)

	rt.Update(treeWith(ctrl("E2", 0)), eventActive("E2"), nil) // received + started
	rt.Update(treeWith(ctrl("E2", 6)), nil, nil)               // server cancels
	rt.Update(treeWith(ctrl("E2", 6)), nil, nil)               // still cancelled — no repeat

	eq(t, fp.statusesFor("E2"),
		model.ResponseEventReceived, model.ResponseEventStarted, model.ResponseEventCancelled)
}

// An event that arrives already cancelled is dropped — no responses at all.
func TestResponse_BornCancelledIgnored(t *testing.T) {
	fp := &fakePoster{}
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil)

	rt.Update(treeWith(ctrl("E3", 6)), nil, nil)
	rt.Update(treeWith(ctrl("E3", 6)), nil, nil)

	if got := fp.statusesFor("E3"); len(got) != 0 {
		t.Fatalf("born-cancelled event produced responses: %v", got)
	}
}

// CORE-023: a received event that loses to an overlapping event is
// acknowledged with status=7 (superseded), once.
func TestResponse_Superseded(t *testing.T) {
	fp := &fakePoster{}
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil)

	rt.Update(treeWith(ctrl("E4", 0)), eventActive("E4"), nil)           // received + started
	rt.Update(treeWith(ctrl("E4", 0)), nil, map[string]bool{"E4": true}) // now superseded
	rt.Update(treeWith(ctrl("E4", 0)), nil, map[string]bool{"E4": true}) // still — no repeat

	eq(t, fp.statusesFor("E4"),
		model.ResponseEventReceived, model.ResponseEventStarted, model.ResponseEventSuperseded)
}

// TASK-031: exactly one CannotComply POST per episode. A redelivered alert with
// the SAME episode ID must not double-post; ClearAlerts re-arms so the NEXT
// episode does post.
func TestResponse_CannotComplyOncePerEpisode(t *testing.T) {
	fp := &fakePoster{}
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil)

	rt.AlertCannotComply("E5", "E5@100#1") // onset → post
	rt.AlertCannotComply("E5", "E5@100#1") // redelivered same episode → no re-post
	if got := fp.statusesFor("E5"); len(got) != 1 || got[0] != model.ResponseCannotComply {
		t.Fatalf("one CannotComply expected for the episode, got %v", got)
	}

	rt.ClearAlerts()                       // episode ends, re-arm
	rt.AlertCannotComply("E5", "E5@200#2") // a genuinely new episode for the same mRID → posts again
	if got := fp.statusesFor("E5"); len(got) != 2 {
		t.Fatalf("a new episode after clear must re-post, got %v", got)
	}
}

// TASK-031 hub-restart invariant: a hub restart mid-episode re-derives a FRESH
// episode ID for the same still-breaching control (northbound keeps its alerted
// map because it did not restart). Keying on episode ID alone would re-post; the
// mRID safety net must suppress it — preserving the pre-TASK-031 behavior
// (hub-restart-mid-cap posts exactly once).
func TestResponse_CannotComplyRestartNoDoublePost(t *testing.T) {
	fp := &fakePoster{}
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil)

	rt.AlertCannotComply("E6", "E6@100#1") // pre-restart onset → post
	// Hub restarts, re-opens the SAME control's episode with a new counter/time.
	rt.AlertCannotComply("E6", "E6@500#1") // different episode ID, same mRID → must NOT re-post
	if got := fp.statusesFor("E6"); len(got) != 1 {
		t.Fatalf("hub-restart re-alert for the same mRID must post once, got %v", got)
	}
}

// TASK-031 mixed-version tolerance: an alert with no episode ID (a pre-TASK-031
// hub) dedupes by mRID exactly as before.
func TestResponse_CannotComplyLegacyNoEpisodeID(t *testing.T) {
	fp := &fakePoster{}
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil)

	rt.AlertCannotComply("E7", "") // legacy publisher, mRID-only
	rt.AlertCannotComply("E7", "") // redelivered → no re-post
	if got := fp.statusesFor("E7"); len(got) != 1 {
		t.Fatalf("legacy mRID-only dedupe must post once, got %v", got)
	}
}

// ---------------------------------------------------------------------
// TASK-040: journal wiring (cannot_comply_posted, success-gated)
// ---------------------------------------------------------------------

// failingPoster always errors, standing in for a POST the utility server
// rejects/times out on — used to prove a failed CannotComply attempt never
// journals a record of something that didn't actually happen.
type failingPoster struct{}

func (failingPoster) Post(string, []byte, string) ([]byte, string, error) {
	return nil, "", errors.New("no ack")
}

// journalEventsByType scans dir's active journal file and groups events by
// Type — this package's own copy of cmd/hub's test helper of the same name
// (separate package; cmd/* packages don't import each other, 05 §1).
func journalEventsByType(t *testing.T, dir string) map[string][]journal.Event {
	t.Helper()
	out := make(map[string][]journal.Event)
	if _, err := journal.Scan(dir, journal.DefaultName, func(e journal.Event) error {
		out[e.Type] = append(out[e.Type], e)
		return nil
	}); err != nil {
		t.Fatalf("journal.Scan: %v", err)
	}
	return out
}

// TestResponse_CannotComplyJournalsOnSuccess verifies AlertCannotComply
// journals cannot_comply_posted, carrying the episode ID and mRID, exactly
// once per episode — same dedupe boundary as the POST itself.
func TestResponse_CannotComplyJournalsOnSuccess(t *testing.T) {
	dir := t.TempDir()
	jw, err := journal.Open(journal.Config{Dir: dir})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	defer jw.Close()

	fp := &fakePoster{}
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, jw)

	rt.AlertCannotComply("E8", "E8@100#1")
	rt.AlertCannotComply("E8", "E8@100#1") // redelivered same episode → no re-journal
	if err := jw.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	events := journalEventsByType(t, dir)
	ccp := events[journal.TypeCannotComplyPosted]
	if len(ccp) != 1 {
		t.Fatalf("cannot_comply_posted events = %d, want 1", len(ccp))
	}
	var payload journal.CannotComplyPosted
	if err := json.Unmarshal(ccp[0].Data, &payload); err != nil {
		t.Fatalf("unmarshal CannotComplyPosted: %v", err)
	}
	if payload.EpisodeID != "E8@100#1" || payload.MRID != "E8" {
		t.Fatalf("CannotComplyPosted payload = %+v, want episode_id=E8@100#1 mrid=E8", payload)
	}
}

// TestResponse_CannotComplyDoesNotJournalOnFailedPOST verifies the "err ==
// nil branch" gate: AlertCannotComply must not journal a
// cannot_comply_posted when the POST itself fails — a durable record must
// never claim a POST happened when it didn't.
func TestResponse_CannotComplyDoesNotJournalOnFailedPOST(t *testing.T) {
	dir := t.TempDir()
	jw, err := journal.Open(journal.Config{Dir: dir})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	defer jw.Close()

	rt := New(failingPoster{}, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, jw)
	rt.AlertCannotComply("E9", "E9@1#1")
	if err := jw.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	events := journalEventsByType(t, dir)
	if got := len(events[journal.TypeCannotComplyPosted]); got != 0 {
		t.Fatalf("cannot_comply_posted events = %d, want 0 (a failed POST must not journal)", got)
	}
}

// TestResponse_NilJournalIsNoop verifies a nil Writer (no "journal" config
// block) never panics and leaves AlertCannotComply's own POST/dedupe
// behavior untouched.
func TestResponse_NilJournalIsNoop(t *testing.T) {
	fp := &fakePoster{}
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil)
	rt.AlertCannotComply("E10", "E10@1#1")
	if got := fp.statusesFor("E10"); len(got) != 1 || got[0] != model.ResponseCannotComply {
		t.Fatalf("nil journal must not affect the POST itself, got %v", got)
	}
}
