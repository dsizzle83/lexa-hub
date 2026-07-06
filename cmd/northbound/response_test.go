package main

import (
	"encoding/xml"
	"testing"

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
	// ValidUntil far in the future so update() never auto-completes it.
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
	rt := newResponseTracker(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil)

	rt.update(treeWith(ctrl("E1", 0)), eventActive("E1"), nil) // cycle 1: received+started
	rt.update(treeWith(ctrl("E1", 0)), eventActive("E1"), nil) // cycle 2: no change
	rt.update(treeWith(ctrl("E1", 0)), nil, nil)               // cycle 3: event gone → completed

	eq(t, fp.statusesFor("E1"),
		model.ResponseEventReceived, model.ResponseEventStarted, model.ResponseEventCompleted)
}

// CORE-022 step 7: an event the client received, then the server cancels,
// must be acknowledged with status=6 — exactly once.
func TestResponse_CancelledAfterReceived(t *testing.T) {
	fp := &fakePoster{}
	rt := newResponseTracker(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil)

	rt.update(treeWith(ctrl("E2", 0)), eventActive("E2"), nil) // received + started
	rt.update(treeWith(ctrl("E2", 6)), nil, nil)               // server cancels
	rt.update(treeWith(ctrl("E2", 6)), nil, nil)               // still cancelled — no repeat

	eq(t, fp.statusesFor("E2"),
		model.ResponseEventReceived, model.ResponseEventStarted, model.ResponseEventCancelled)
}

// An event that arrives already cancelled is dropped — no responses at all.
func TestResponse_BornCancelledIgnored(t *testing.T) {
	fp := &fakePoster{}
	rt := newResponseTracker(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil)

	rt.update(treeWith(ctrl("E3", 6)), nil, nil)
	rt.update(treeWith(ctrl("E3", 6)), nil, nil)

	if got := fp.statusesFor("E3"); len(got) != 0 {
		t.Fatalf("born-cancelled event produced responses: %v", got)
	}
}

// CORE-023: a received event that loses to an overlapping event is
// acknowledged with status=7 (superseded), once.
func TestResponse_Superseded(t *testing.T) {
	fp := &fakePoster{}
	rt := newResponseTracker(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil)

	rt.update(treeWith(ctrl("E4", 0)), eventActive("E4"), nil)           // received + started
	rt.update(treeWith(ctrl("E4", 0)), nil, map[string]bool{"E4": true}) // now superseded
	rt.update(treeWith(ctrl("E4", 0)), nil, map[string]bool{"E4": true}) // still — no repeat

	eq(t, fp.statusesFor("E4"),
		model.ResponseEventReceived, model.ResponseEventStarted, model.ResponseEventSuperseded)
}

// TASK-031: exactly one CannotComply POST per episode. A redelivered alert with
// the SAME episode ID must not double-post; clearAlerts re-arms so the NEXT
// episode does post.
func TestResponse_CannotComplyOncePerEpisode(t *testing.T) {
	fp := &fakePoster{}
	rt := newResponseTracker(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil)

	rt.alertCannotComply("E5", "E5@100#1") // onset → post
	rt.alertCannotComply("E5", "E5@100#1") // redelivered same episode → no re-post
	if got := fp.statusesFor("E5"); len(got) != 1 || got[0] != model.ResponseCannotComply {
		t.Fatalf("one CannotComply expected for the episode, got %v", got)
	}

	rt.clearAlerts()                       // episode ends, re-arm
	rt.alertCannotComply("E5", "E5@200#2") // a genuinely new episode for the same mRID → posts again
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
	rt := newResponseTracker(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil)

	rt.alertCannotComply("E6", "E6@100#1") // pre-restart onset → post
	// Hub restarts, re-opens the SAME control's episode with a new counter/time.
	rt.alertCannotComply("E6", "E6@500#1") // different episode ID, same mRID → must NOT re-post
	if got := fp.statusesFor("E6"); len(got) != 1 {
		t.Fatalf("hub-restart re-alert for the same mRID must post once, got %v", got)
	}
}

// TASK-031 mixed-version tolerance: an alert with no episode ID (a pre-TASK-031
// hub) dedupes by mRID exactly as before.
func TestResponse_CannotComplyLegacyNoEpisodeID(t *testing.T) {
	fp := &fakePoster{}
	rt := newResponseTracker(fp, "LFDI", "/rsps/0/r", utilitytime.New(utilitytime.Config{}), nil)

	rt.alertCannotComply("E7", "") // legacy publisher, mRID-only
	rt.alertCannotComply("E7", "") // redelivered → no re-post
	if got := fp.statusesFor("E7"); len(got) != 1 {
		t.Fatalf("legacy mRID-only dedupe must post once, got %v", got)
	}
}
