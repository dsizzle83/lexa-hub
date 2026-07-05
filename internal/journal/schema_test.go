package journal

import (
	"encoding/json"
	"testing"
)

// stampFixed checks a constructor's error and stamps deterministic Ts/Seq so
// a marshaled golden string is stable across runs; every other field
// (V/Type/Svc/Data) comes straight from the constructor under test. Callers
// destructure the constructor's (Event, error) first (Go does not allow a
// multi-value call as one of several arguments to another call), then pass
// both values in here.
func stampFixed(t *testing.T, ev Event, err error) Event {
	t.Helper()
	if err != nil {
		t.Fatalf("constructor returned error: %v", err)
	}
	ev.Ts = 1720000005
	ev.Seq = 42
	return ev
}

func marshalOrFatal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestGoldenControlAdopted locks the control_adopted payload's JSON shape:
// field names, order (Go's encoding/json marshals struct fields in
// declaration order), and the nil-limit-pointer omission behavior AD-013
// depends on elsewhere (nil = no opinion, omitted; never a silent zero).
func TestGoldenControlAdopted(t *testing.T) {
	exp, imp, max, fixed := 5000.0, 3000.0, 10000.0, 0.0
	p := NewControlAdopted("event", "abc123-mrid-uuid", &exp, &imp, &max, &fixed, 1720000000, -2)
	got := marshalOrFatal(t, p)
	want := `{"source":"event","mrid":"abc123-mrid-uuid","exp_lim_w":5000,"imp_lim_w":3000,"max_lim_w":10000,"fixed_w":0,"valid_until":1720000000,"clock_offset":-2}`
	if got != want {
		t.Errorf("ControlAdopted JSON shape changed:\n got:  %s\n want: %s", got, want)
	}

	// Every limit nil (a "none/no active control" adoption): all four limit
	// fields must vanish from the wire, not render as null or 0.
	none := NewControlAdopted("none", "", nil, nil, nil, nil, 0, 0)
	gotNone := marshalOrFatal(t, none)
	wantNone := `{"source":"none","mrid":"","clock_offset":0}`
	if gotNone != wantNone {
		t.Errorf("ControlAdopted with nil limits changed shape:\n got:  %s\n want: %s", gotNone, wantNone)
	}

	rawEv, err := NewControlAdoptedEvent("hub", p)
	ev := stampFixed(t, rawEv, err)
	if ev.Type != TypeControlAdopted || ev.Svc != "hub" || ev.V != SchemaV {
		t.Errorf("envelope fields wrong: %+v", ev)
	}
	gotEv := marshalOrFatal(t, ev)
	wantEv := `{"v":1,"ts":1720000005,"seq":42,"type":"control_adopted","svc":"hub","data":` + want + `}`
	if gotEv != wantEv {
		t.Errorf("control_adopted Event JSON shape changed:\n got:  %s\n want: %s", gotEv, wantEv)
	}
}

// TestGoldenControlReleased locks the control_released payload shape and
// the three documented Reason constants' wire strings.
func TestGoldenControlReleased(t *testing.T) {
	cases := []struct {
		reason string
		want   string
	}{
		{ReasonExpired, `{"mrid":"m1","reason":"expired"}`},
		{ReasonCleared, `{"mrid":"m1","reason":"cleared"}`},
		{ReasonReplaced, `{"mrid":"m1","reason":"replaced"}`},
	}
	for _, c := range cases {
		got := marshalOrFatal(t, NewControlReleased("m1", c.reason))
		if got != c.want {
			t.Errorf("ControlReleased(%q) shape changed:\n got:  %s\n want: %s", c.reason, got, c.want)
		}
	}

	rawEv, err := NewControlReleasedEvent("hub", NewControlReleased("m1", ReasonExpired))
	ev := stampFixed(t, rawEv, err)
	got := marshalOrFatal(t, ev)
	want := `{"v":1,"ts":1720000005,"seq":42,"type":"control_released","svc":"hub","data":{"mrid":"m1","reason":"expired"}}`
	if got != want {
		t.Errorf("control_released Event JSON shape changed:\n got:  %s\n want: %s", got, want)
	}
}

// TestGoldenDispatch locks the dispatch payload shape across all three
// device kinds and confirms unset optional fields are omitted, not null.
func TestGoldenDispatch(t *testing.T) {
	setpoint := 5000.0
	battery := NewDispatch("batt1", KindBattery, &setpoint, nil, nil, nil)
	got := marshalOrFatal(t, battery)
	want := `{"device":"batt1","kind":"battery","setpoint_w":5000}`
	if got != want {
		t.Errorf("battery Dispatch shape changed:\n got:  %s\n want: %s", got, want)
	}

	ceiling := 1e9
	solar := NewDispatch("solar1", KindSolar, nil, &ceiling, nil, nil)
	gotSolar := marshalOrFatal(t, solar)
	wantSolar := `{"device":"solar1","kind":"solar","ceiling_w":1000000000}`
	if gotSolar != wantSolar {
		t.Errorf("solar Dispatch shape changed:\n got:  %s\n want: %s", gotSolar, wantSolar)
	}

	amps := 32.0
	connect := true
	evse := NewDispatch("station1", KindEVSE, nil, nil, &amps, &connect)
	gotEVSE := marshalOrFatal(t, evse)
	wantEVSE := `{"device":"station1","kind":"evse","max_current_a":32,"connect":true}`
	if gotEVSE != wantEVSE {
		t.Errorf("evse Dispatch shape changed:\n got:  %s\n want: %s", gotEVSE, wantEVSE)
	}

	rawEv, err := NewDispatchEvent("modbus", battery)
	ev := stampFixed(t, rawEv, err)
	gotEv := marshalOrFatal(t, ev)
	wantEv := `{"v":1,"ts":1720000005,"seq":42,"type":"dispatch","svc":"modbus","data":` + want + `}`
	if gotEv != wantEv {
		t.Errorf("dispatch Event JSON shape changed:\n got:  %s\n want: %s", gotEv, wantEv)
	}
}

// TestGoldenBreach locks the breach_begin/breach_end payload shape and
// EpisodeID's derivation.
func TestGoldenBreach(t *testing.T) {
	episodeID := EpisodeID("abc123-mrid-uuid", 1720000000)
	if episodeID != "abc123-mrid-uuid/1720000000" {
		t.Errorf("EpisodeID shape changed: got %q", episodeID)
	}

	br := NewBreach(episodeID, "abc123-mrid-uuid", "import", 5000, 5400, 400, "battery at SOC reserve")
	got := marshalOrFatal(t, br)
	want := `{"episode_id":"abc123-mrid-uuid/1720000000","mrid":"abc123-mrid-uuid","limit_type":"import","limit_w":5000,"measured_w":5400,"shortfall_w":400,"reason":"battery at SOC reserve"}`
	if got != want {
		t.Errorf("Breach shape changed:\n got:  %s\n want: %s", got, want)
	}

	rawBegin, errBegin := NewBreachBeginEvent("hub", br)
	begin := stampFixed(t, rawBegin, errBegin)
	if begin.Type != TypeBreachBegin {
		t.Errorf("expected breach_begin type, got %q", begin.Type)
	}
	rawEnd, errEnd := NewBreachEndEvent("hub", br)
	end := stampFixed(t, rawEnd, errEnd)
	if end.Type != TypeBreachEnd {
		t.Errorf("expected breach_end type, got %q", end.Type)
	}
}

// TestGoldenCannotComplyPosted locks the cannot_comply_posted payload shape.
func TestGoldenCannotComplyPosted(t *testing.T) {
	p := NewCannotComplyPosted(EpisodeID("m1", 100), "m1", 200)
	got := marshalOrFatal(t, p)
	want := `{"episode_id":"m1/100","mrid":"m1","http_status":200}`
	if got != want {
		t.Errorf("CannotComplyPosted shape changed:\n got:  %s\n want: %s", got, want)
	}

	rawEv, err := NewCannotComplyPostedEvent("northbound", p)
	ev := stampFixed(t, rawEv, err)
	gotEv := marshalOrFatal(t, ev)
	wantEv := `{"v":1,"ts":1720000005,"seq":42,"type":"cannot_comply_posted","svc":"northbound","data":` + want + `}`
	if gotEv != wantEv {
		t.Errorf("cannot_comply_posted Event JSON shape changed:\n got:  %s\n want: %s", gotEv, wantEv)
	}
}

// TestGoldenServiceStart locks the service_start payload shape.
func TestGoldenServiceStart(t *testing.T) {
	p := NewServiceStart("v1.2.3", "deadbeef")
	got := marshalOrFatal(t, p)
	want := `{"version":"v1.2.3","config_hash":"deadbeef"}`
	if got != want {
		t.Errorf("ServiceStart shape changed:\n got:  %s\n want: %s", got, want)
	}
	rawEv, err := NewServiceStartEvent("hub", p)
	ev := stampFixed(t, rawEv, err)
	if ev.Type != TypeServiceStart {
		t.Errorf("expected service_start type, got %q", ev.Type)
	}
}

// TestGoldenSnapshot locks the snapshot_written/snapshot_restored payload
// shape, including the omitted-when-no-episode case.
func TestGoldenSnapshot(t *testing.T) {
	p := NewSnapshot("/var/lib/lexa-hub/snapshot.json", "m1/100")
	got := marshalOrFatal(t, p)
	want := `{"path":"/var/lib/lexa-hub/snapshot.json","breach_episode":"m1/100"}`
	if got != want {
		t.Errorf("Snapshot shape changed:\n got:  %s\n want: %s", got, want)
	}

	noBreach := NewSnapshot("/var/lib/lexa-hub/snapshot.json", "")
	gotNoBreach := marshalOrFatal(t, noBreach)
	wantNoBreach := `{"path":"/var/lib/lexa-hub/snapshot.json"}`
	if gotNoBreach != wantNoBreach {
		t.Errorf("Snapshot with no active breach shape changed:\n got:  %s\n want: %s", gotNoBreach, wantNoBreach)
	}

	rawWritten, errWritten := NewSnapshotWrittenEvent("hub", p)
	written := stampFixed(t, rawWritten, errWritten)
	if written.Type != TypeSnapshotWritten {
		t.Errorf("expected snapshot_written type, got %q", written.Type)
	}
	rawRestored, errRestored := NewSnapshotRestoredEvent("hub", p)
	restored := stampFixed(t, rawRestored, errRestored)
	if restored.Type != TypeSnapshotRestored {
		t.Errorf("expected snapshot_restored type, got %q", restored.Type)
	}
}

// TestRepresentativeLineSize pins the exact on-disk byte counts (payload +
// envelope + trailing newline) the package doc comment's write-budget
// arithmetic cites. If this test's expected lengths ever change, update
// journal.go's doc comment in the same commit — the numbers there must stay
// real, not hand-waved (TASK-039 acceptance criteria).
func TestRepresentativeLineSize(t *testing.T) {
	exp, imp, max, fixed := 5000.0, 3000.0, 10000.0, 0.0
	rawAdopted, errAdopted := NewControlAdoptedEvent("hub", NewControlAdopted(
		"event", "abc123-mrid-uuid", &exp, &imp, &max, &fixed, 1720000000, -2))
	adopted := stampFixed(t, rawAdopted, errAdopted)
	adoptedLine := len(marshalOrFatal(t, adopted)) + 1 // +1 for the newline Append adds
	if adoptedLine != 229 {
		t.Errorf("control_adopted representative line size changed: got %d, doc comment says 229 (update both together)", adoptedLine)
	}

	setpoint := 5000.0
	rawDispatch, errDispatch := NewDispatchEvent("hub", NewDispatch("batt1", KindBattery, &setpoint, nil, nil, nil))
	dispatch := stampFixed(t, rawDispatch, errDispatch)
	dispatchLine := len(marshalOrFatal(t, dispatch)) + 1
	if dispatchLine != 124 {
		t.Errorf("dispatch representative line size changed: got %d, doc comment says 124 (update both together)", dispatchLine)
	}

	rawBreach, errBreach := NewBreachBeginEvent("hub", NewBreach(
		EpisodeID("abc123-mrid-uuid", 1720000000), "abc123-mrid-uuid", "import", 5000, 5400, 400, "battery at SOC reserve"))
	breach := stampFixed(t, rawBreach, errBreach)
	breachLine := len(marshalOrFatal(t, breach)) + 1
	if breachLine != 252 {
		t.Errorf("breach_begin representative line size changed: got %d, doc comment says 252 (update both together)", breachLine)
	}

	// The doc comment's 260 B/line round number must stay above every
	// measured representative line, including the largest (breach).
	if breachLine > 260 {
		t.Errorf("breach_begin line (%d B) now exceeds the doc comment's 260 B/line round number; raise it there too", breachLine)
	}
}

// TestNewEventMarshalError proves newEvent (and therefore every NewXxxEvent
// constructor) returns an error rather than panicking if ever handed a
// payload encoding/json cannot marshal. None of the typed payload structs
// this package defines can trigger this (every field is a plain JSON-safe
// type), so this reaches into newEvent directly with a payload type no
// constructor could ever produce, purely to prove the error path itself is
// wired correctly.
func TestNewEventMarshalError(t *testing.T) {
	_, err := newEvent("bad", "hub", make(chan int))
	if err == nil {
		t.Fatal("expected an error marshaling an unmarshalable payload")
	}
}
