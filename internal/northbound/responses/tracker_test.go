package responses

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"testing"

	"lexa-hub/internal/journal"
	"lexa-hub/internal/northbound/discovery"
	"lexa-hub/internal/northbound/egress"
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
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil, nil, State{})

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
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil, nil, State{})

	rt.Update(treeWith(ctrl("E2", 0)), eventActive("E2"), nil) // received + started
	rt.Update(treeWith(ctrl("E2", 6)), nil, nil)               // server cancels
	rt.Update(treeWith(ctrl("E2", 6)), nil, nil)               // still cancelled — no repeat

	eq(t, fp.statusesFor("E2"),
		model.ResponseEventReceived, model.ResponseEventStarted, model.ResponseEventCancelled)
}

// An event that arrives already cancelled is dropped — no responses at all.
func TestResponse_BornCancelledIgnored(t *testing.T) {
	fp := &fakePoster{}
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil, nil, State{})

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
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil, nil, State{})

	rt.Update(treeWith(ctrl("E4", 0)), eventActive("E4"), nil)           // received + started
	rt.Update(treeWith(ctrl("E4", 0)), nil, map[string]bool{"E4": true}) // now superseded
	rt.Update(treeWith(ctrl("E4", 0)), nil, map[string]bool{"E4": true}) // still — no repeat

	eq(t, fp.statusesFor("E4"),
		model.ResponseEventReceived, model.ResponseEventStarted, model.ResponseEventSuperseded)
}

// TASK-031: exactly one breach-onset POST per episode. A redelivered alert with
// the SAME episode ID must not double-post; ClearAlerts re-arms so the NEXT
// episode does post. WP-7 (D5): the default wire code is now Table 27 status 8
// (PartialOptOut) — the dedupe discipline itself is unchanged; the legacy 0xF0
// wire behavior is pinned separately in TestResponse_LegacyFlag0xF0ByteCompat.
func TestResponse_CannotComplyOncePerEpisode(t *testing.T) {
	fp := &fakePoster{}
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil, nil, State{})

	rt.AlertCannotComply("E5", "E5@100#1") // onset → post
	rt.AlertCannotComply("E5", "E5@100#1") // redelivered same episode → no re-post
	if got := fp.statusesFor("E5"); len(got) != 1 || got[0] != model.ResponsePartialOptOut {
		t.Fatalf("one onset Response (status 8) expected for the episode, got %v", got)
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
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil, nil, State{})

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
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil, nil, State{})

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
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, jw, nil, State{})

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

	rt := New(failingPoster{}, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, jw, nil, State{})
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
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil, nil, State{})
	rt.AlertCannotComply("E10", "E10@1#1")
	if got := fp.statusesFor("E10"); len(got) != 1 || got[0] != model.ResponsePartialOptOut {
		t.Fatalf("nil journal must not affect the POST itself, got %v", got)
	}
}

// ---------------------------------------------------------------------
// WP-7 (D5): Table 27 code emission per lifecycle path + legacy byte-compat
// ---------------------------------------------------------------------

// rawPoster records the exact request bodies, for the byte-compat pin.
type rawPoster struct{ bodies [][]byte }

func (p *rawPoster) Post(_ string, body []byte, _ string) ([]byte, string, error) {
	p.bodies = append(p.bodies, append([]byte(nil), body...))
	return nil, "", nil
}

// TestResponse_LegacyFlag0xF0ByteCompat pins legacy_cannotcomply_code=true
// against the pre-WP-7 wire bytes: the breach-onset Response must be the
// byte-identical xml.Marshal of the same model.Response postResponse has
// always built — status 0xF0 (240), same field set, same encoding.
func TestResponse_LegacyFlag0xF0ByteCompat(t *testing.T) {
	rp := &rawPoster{}
	rt := New(rp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil, nil, State{})
	rt.SetLegacyCannotComplyCode(true)

	rt.AlertCannotComply("E1", "E1@1#1")
	if len(rp.bodies) != 1 {
		t.Fatalf("POST bodies = %d, want 1", len(rp.bodies))
	}
	var got model.Response
	if err := xml.Unmarshal(rp.bodies[0], &got); err != nil {
		t.Fatalf("unmarshal posted Response: %v", err)
	}
	if got.Status != 0xF0 {
		t.Fatalf("legacy status = %d, want 240 (0xF0)", got.Status)
	}
	want, err := xml.Marshal(&model.Response{
		CreatedDateTime: got.CreatedDateTime, // only field the clock varies
		EndDeviceLFDI:   "LFDI",
		Status:          model.ResponseCannotComply,
		Subject:         "E1",
	})
	if err != nil {
		t.Fatalf("marshal expected Response: %v", err)
	}
	if !bytes.Equal(rp.bodies[0], want) {
		t.Fatalf("legacy wire bytes drifted:\n got: %s\nwant: %s", rp.bodies[0], want)
	}
}

// TestResponse_LegacyEndOfEventAlwaysCompleted pins the other half of legacy
// byte-compat: even a breached-throughout event still ends with Completed(3),
// exactly as before WP-7 — 8/10 never appear on the wire in legacy mode.
func TestResponse_LegacyEndOfEventAlwaysCompleted(t *testing.T) {
	fp := &fakePoster{}
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil, nil, State{})
	rt.SetLegacyCannotComplyCode(true)

	rt.Update(treeWith(ctrl("E1", 0)), eventActive("E1"), nil) // received + started
	rt.AlertCannotComply("E1", "E1@1#1")                       // 0xF0 onset
	rt.Update(treeWith(ctrl("E1", 0)), eventActive("E1"), nil) // breach sample
	rt.Update(treeWith(ctrl("E1", 0)), nil, nil)               // end — still breaching

	eq(t, fp.statusesFor("E1"),
		model.ResponseEventReceived, model.ResponseEventStarted,
		model.ResponseCannotComply, model.ResponseEventCompleted)
}

// TestResponse_EndOfEvent_NoOverlapCompletes3: a breach on some OTHER
// control never contaminates this event's end-of-event reconciliation.
func TestResponse_EndOfEvent_NoOverlapCompletes3(t *testing.T) {
	fp := &fakePoster{}
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil, nil, State{})

	rt.Update(treeWith(ctrl("E1", 0)), eventActive("E1"), nil) // received + started
	rt.AlertCannotComply("UNRELATED", "U@1#1")                 // breach elsewhere
	rt.Update(treeWith(ctrl("E1", 0)), eventActive("E1"), nil) // compliant sample
	rt.Update(treeWith(ctrl("E1", 0)), nil, nil)               // end

	eq(t, fp.statusesFor("E1"),
		model.ResponseEventReceived, model.ResponseEventStarted, model.ResponseEventCompleted)
}

// TestResponse_EndOfEvent_PartialBreachPosts8: compliant and breaching
// cycles both observed → end code 8 (on top of the onset 8).
func TestResponse_EndOfEvent_PartialBreachPosts8(t *testing.T) {
	fp := &fakePoster{}
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil, nil, State{})

	rt.Update(treeWith(ctrl("E1", 0)), eventActive("E1"), nil) // received + started
	rt.Update(treeWith(ctrl("E1", 0)), eventActive("E1"), nil) // compliant sample
	rt.AlertCannotComply("E1", "E1@1#1")                       // onset → 8
	rt.Update(treeWith(ctrl("E1", 0)), eventActive("E1"), nil) // breach sample
	rt.ClearAlerts()                                           // breach clears mid-event
	rt.Update(treeWith(ctrl("E1", 0)), eventActive("E1"), nil) // compliant again
	rt.Update(treeWith(ctrl("E1", 0)), nil, nil)               // end → partial

	eq(t, fp.statusesFor("E1"),
		model.ResponseEventReceived, model.ResponseEventStarted,
		model.ResponsePartialOptOut, model.ResponsePartialOptOut)
}

// TestResponse_EndOfEvent_BreachedThroughoutPosts10: every observed sample
// of the event's tenure breached → end code 10, which is terminal (a later
// supersede signal for the ended event must not re-post).
func TestResponse_EndOfEvent_BreachedThroughoutPosts10(t *testing.T) {
	fp := &fakePoster{}
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil, nil, State{})

	rt.Update(treeWith(ctrl("E1", 0)), eventActive("E1"), nil) // received + started (no sample yet)
	rt.AlertCannotComply("E1", "E1@1#1")                       // onset within first cycle → 8
	rt.Update(treeWith(ctrl("E1", 0)), eventActive("E1"), nil) // breach sample
	rt.Update(treeWith(ctrl("E1", 0)), nil, nil)               // end — still breaching → 10

	want := []uint8{model.ResponseEventReceived, model.ResponseEventStarted,
		model.ResponsePartialOptOut, model.ResponseNoParticipation}
	eq(t, fp.statusesFor("E1"), want...)

	// 10 is terminal: nothing further for this mRID.
	rt.Update(treeWith(ctrl("E1", 0)), nil, map[string]bool{"E1": true})
	eq(t, fp.statusesFor("E1"), want...)
}

// TestResponse_EndOfEvent_AlertBeforeStartSeeds covers the onset-races-ahead
// case: a breach alert for the control can arrive (MQTT goroutine) before
// the walk's Update posts Started. The Started seed from the alerted map
// must count it as breached-from-start, so an event that ends still
// breaching with no compliant sample reconciles as 10, not 3.
func TestResponse_EndOfEvent_AlertBeforeStartSeeds(t *testing.T) {
	fp := &fakePoster{}
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil, nil, State{})

	rt.AlertCannotComply("E1", "E1@1#1")                       // races ahead of Started → 8
	rt.Update(treeWith(ctrl("E1", 0)), eventActive("E1"), nil) // received + started (seeded)
	rt.Update(treeWith(ctrl("E1", 0)), nil, nil)               // end — still breaching

	eq(t, fp.statusesFor("E1"),
		model.ResponsePartialOptOut, model.ResponseEventReceived,
		model.ResponseEventStarted, model.ResponseNoParticipation)
}

// TestResponse_ReceiptRejectPosts253OncePerMRID: the scheduler's
// plausibility-reject hook re-fires every walk while the malformed control
// stays served; the tracker posts 253 once, and a later server-side fix of
// the same mRID resumes the normal lifecycle (no Received re-post — the
// mRID was already acknowledged by the rejection).
func TestResponse_ReceiptRejectPosts253OncePerMRID(t *testing.T) {
	fp := &fakePoster{}
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil, nil, State{})

	rt.ReceiptReject("EX", model.ResponseRejectedInvalid)
	rt.ReceiptReject("EX", model.ResponseRejectedInvalid) // next walk, same reject → deduped
	eq(t, fp.statusesFor("EX"), model.ResponseRejectedInvalid)

	// Server fixes the event content under the same mRID → adoptable again.
	rt.Update(treeWith(ctrl("EX", 0)), eventActive("EX"), nil)
	rt.Update(treeWith(ctrl("EX", 0)), nil, nil)
	eq(t, fp.statusesFor("EX"),
		model.ResponseRejectedInvalid, model.ResponseEventStarted, model.ResponseEventCompleted)
}

// TestResponse_ReceiptRejectLegacyModeIsNoop: pre-WP-7 posted no rejection
// Responses at all, so legacy byte-compat means the hook goes quiet.
func TestResponse_ReceiptRejectLegacyModeIsNoop(t *testing.T) {
	fp := &fakePoster{}
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil, nil, State{})
	rt.SetLegacyCannotComplyCode(true)

	rt.ReceiptReject("EX", model.ResponseRejectedInvalid)
	if len(fp.calls) != 0 {
		t.Fatalf("legacy mode posted %d receipt-reject Responses, want 0", len(fp.calls))
	}
}

// ---------------------------------------------------------------------
// WP-7 (D4): egress gate backstop on the async CannotComply path
// ---------------------------------------------------------------------

// TestResponse_EgressGateSuppressesAlertPost: with the gate suspended
// (registration-PIN freeze) AlertCannotComply transmits nothing; the
// in-RAM dedupe still arms (same contract as a failed POST — no same-
// process retry), and after resume a genuinely new episode posts again.
func TestResponse_EgressGateSuppressesAlertPost(t *testing.T) {
	fp := &fakePoster{}
	rt := New(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil, nil, nil, State{})
	gate := &egress.Gate{}
	rt.SetEgressGate(gate)

	gate.Suspend("registration-pin")
	rt.AlertCannotComply("E1", "E1@1#1")
	if len(fp.calls) != 0 {
		t.Fatalf("POSTs during suspended egress = %d, want 0", len(fp.calls))
	}

	gate.Resume()
	rt.AlertCannotComply("E1", "E1@1#1") // same episode: in-RAM dedupe holds
	if len(fp.calls) != 0 {
		t.Fatalf("suppressed episode retried in-process = %d POSTs, want 0 (failed-POST contract)", len(fp.calls))
	}

	rt.ClearAlerts()
	rt.AlertCannotComply("E1", "E1@2#2") // new episode after clear → posts
	eq(t, fp.statusesFor("E1"), model.ResponsePartialOptOut)
}
