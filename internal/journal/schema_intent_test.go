package journal

import "testing"

// TestGoldenIntentReceived locks the intent_received payload shape
// (TASK-082, §3.6): the audit-trail record cloudlink's downlink (§2.6) or the
// hub's own adopter (§3.1) appends BEFORE forwarding/applying an intent.
func TestGoldenIntentReceived(t *testing.T) {
	p := NewIntentReceived("evgoal", "intent-1", "cloud", "user@example.com", 1_700_000_000)
	got := marshalOrFatal(t, p)
	want := `{"kind":"evgoal","id":"intent-1","origin":"cloud","actor":"user@example.com","issued_at":1700000000}`
	if got != want {
		t.Errorf("IntentReceived shape changed:\n got:  %s\n want: %s", got, want)
	}

	// Actor omitted when empty (e.g. a cli/root-less origin).
	noActor := NewIntentReceived("mode", "intent-2", "app", "", 1)
	gotNoActor := marshalOrFatal(t, noActor)
	wantNoActor := `{"kind":"mode","id":"intent-2","origin":"app","issued_at":1}`
	if gotNoActor != wantNoActor {
		t.Errorf("IntentReceived with empty actor shape changed:\n got:  %s\n want: %s", gotNoActor, wantNoActor)
	}

	rawEv, err := NewIntentReceivedEvent("cloudlink", p)
	ev := stampFixed(t, rawEv, err)
	if ev.Type != TypeIntentReceived || ev.Svc != "cloudlink" || ev.V != SchemaV {
		t.Errorf("envelope fields wrong: %+v", ev)
	}
	gotEv := marshalOrFatal(t, ev)
	wantEv := `{"v":1,"ts":1720000005,"seq":42,"type":"intent_received","svc":"cloudlink","data":` + want + `}`
	if gotEv != wantEv {
		t.Errorf("intent_received Event JSON shape changed:\n got:  %s\n want: %s", gotEv, wantEv)
	}
}

// TestGoldenIntentApplied locks the intent_applied payload shape, including
// both documented Outcome values ("applied" and "clamped").
func TestGoldenIntentApplied(t *testing.T) {
	cases := []struct {
		outcome, detail string
		want            string
	}{
		{"applied", "", `{"kind":"evgoal","id":"i1","outcome":"applied"}`},
		{"clamped", "reserve raised to config floor 20%", `{"kind":"reserve","id":"i2","outcome":"clamped","detail":"reserve raised to config floor 20%"}`},
	}
	for _, c := range cases {
		kind := "evgoal"
		id := "i1"
		if c.outcome == "clamped" {
			kind, id = "reserve", "i2"
		}
		p := NewIntentApplied(kind, id, c.outcome, c.detail, "")
		got := marshalOrFatal(t, p)
		if got != c.want {
			t.Errorf("IntentApplied(%q) shape changed:\n got:  %s\n want: %s", c.outcome, got, c.want)
		}
	}

	p := NewIntentApplied("mode", "i3", "applied", "", "root")
	rawEv, err := NewIntentAppliedEvent("hub", p)
	ev := stampFixed(t, rawEv, err)
	gotEv := marshalOrFatal(t, ev)
	wantEv := `{"v":1,"ts":1720000005,"seq":42,"type":"intent_applied","svc":"hub","data":{"kind":"mode","id":"i3","outcome":"applied","actor":"root"}}`
	if gotEv != wantEv {
		t.Errorf("intent_applied Event JSON shape changed:\n got:  %s\n want: %s", gotEv, wantEv)
	}
}

// TestGoldenIntentRejected locks the intent_rejected payload shape.
func TestGoldenIntentRejected(t *testing.T) {
	p := NewIntentRejected("evgoal", "i4", "expired", "departure_unix in the past", "")
	got := marshalOrFatal(t, p)
	want := `{"kind":"evgoal","id":"i4","outcome":"expired","detail":"departure_unix in the past"}`
	if got != want {
		t.Errorf("IntentRejected shape changed:\n got:  %s\n want: %s", got, want)
	}

	rawEv, err := NewIntentRejectedEvent("hub", p)
	ev := stampFixed(t, rawEv, err)
	if ev.Type != TypeIntentRejected {
		t.Errorf("expected intent_rejected type, got %q", ev.Type)
	}
	gotEv := marshalOrFatal(t, ev)
	wantEv := `{"v":1,"ts":1720000005,"seq":42,"type":"intent_rejected","svc":"hub","data":` + want + `}`
	if gotEv != wantEv {
		t.Errorf("intent_rejected Event JSON shape changed:\n got:  %s\n want: %s", gotEv, wantEv)
	}
}

// TestGoldenModeChange locks the mode_change payload shape and confirms both
// transition directions (optimizer→gateway, gateway→optimizer) marshal
// identically in shape.
func TestGoldenModeChange(t *testing.T) {
	p := NewModeChange("optimizer", "gateway", "user@example.com", "app", "intent-9")
	got := marshalOrFatal(t, p)
	want := `{"from":"optimizer","to":"gateway","actor":"user@example.com","origin":"app","intent_id":"intent-9"}`
	if got != want {
		t.Errorf("ModeChange shape changed:\n got:  %s\n want: %s", got, want)
	}

	back := NewModeChange("gateway", "optimizer", "", "", "")
	gotBack := marshalOrFatal(t, back)
	wantBack := `{"from":"gateway","to":"optimizer"}`
	if gotBack != wantBack {
		t.Errorf("ModeChange with no actor/origin/intent shape changed:\n got:  %s\n want: %s", gotBack, wantBack)
	}

	rawEv, err := NewModeChangeEvent("hub", p)
	ev := stampFixed(t, rawEv, err)
	if ev.Type != TypeModeChange {
		t.Errorf("expected mode_change type, got %q", ev.Type)
	}
	gotEv := marshalOrFatal(t, ev)
	wantEv := `{"v":1,"ts":1720000005,"seq":42,"type":"mode_change","svc":"hub","data":` + want + `}`
	if gotEv != wantEv {
		t.Errorf("mode_change Event JSON shape changed:\n got:  %s\n want: %s", gotEv, wantEv)
	}
}

// TestGoldenConfigWrite locks the config_write payload shape against §4.5's
// exact specification: config_write{service, actor, sha256(before), sha256(after)}.
func TestGoldenConfigWrite(t *testing.T) {
	p := NewConfigWrite("modbus", "installer@example.com", "deadbeef", "cafebabe")
	got := marshalOrFatal(t, p)
	want := `{"service":"modbus","actor":"installer@example.com","before_sha256":"deadbeef","after_sha256":"cafebabe"}`
	if got != want {
		t.Errorf("ConfigWrite shape changed:\n got:  %s\n want: %s", got, want)
	}

	rawEv, err := NewConfigWriteEvent("api", p)
	ev := stampFixed(t, rawEv, err)
	if ev.Type != TypeConfigWrite || ev.Svc != "api" {
		t.Errorf("envelope fields wrong: %+v", ev)
	}
	gotEv := marshalOrFatal(t, ev)
	wantEv := `{"v":1,"ts":1720000005,"seq":42,"type":"config_write","svc":"api","data":` + want + `}`
	if gotEv != wantEv {
		t.Errorf("config_write Event JSON shape changed:\n got:  %s\n want: %s", gotEv, wantEv)
	}
}

// TestGoldenScanRun locks the scan_run payload shape, including both the
// completed and refused phases.
func TestGoldenScanRun(t *testing.T) {
	done := NewScanRun("scan-1", "done", 3, "")
	gotDone := marshalOrFatal(t, done)
	wantDone := `{"id":"scan-1","phase":"done","devices_found":3}`
	if gotDone != wantDone {
		t.Errorf("ScanRun(done) shape changed:\n got:  %s\n want: %s", gotDone, wantDone)
	}

	refused := NewScanRun("scan-2", "refused", 0, "reconciler battery is active")
	gotRefused := marshalOrFatal(t, refused)
	wantRefused := `{"id":"scan-2","phase":"refused","detail":"reconciler battery is active"}`
	if gotRefused != wantRefused {
		t.Errorf("ScanRun(refused) shape changed:\n got:  %s\n want: %s", gotRefused, wantRefused)
	}

	rawEv, err := NewScanRunEvent("modbus", done)
	ev := stampFixed(t, rawEv, err)
	if ev.Type != TypeScanRun || ev.Svc != "modbus" {
		t.Errorf("envelope fields wrong: %+v", ev)
	}
	gotEv := marshalOrFatal(t, ev)
	wantEv := `{"v":1,"ts":1720000005,"seq":42,"type":"scan_run","svc":"modbus","data":` + wantDone + `}`
	if gotEv != wantEv {
		t.Errorf("scan_run Event JSON shape changed:\n got:  %s\n want: %s", gotEv, wantEv)
	}
}

// TestIntentVocabularyNoSchemaVBump proves the append-only rule (§3.6): the
// new event types share SchemaV with every existing one — the addition of a
// new Type/payload is not a version bump.
func TestIntentVocabularyNoSchemaVBump(t *testing.T) {
	if SchemaV != 1 {
		t.Fatalf("SchemaV = %d, want 1 (adding new event types must not bump SchemaV)", SchemaV)
	}
	for _, typ := range []string{
		TypeIntentReceived, TypeIntentApplied, TypeIntentRejected,
		TypeModeChange, TypeConfigWrite, TypeScanRun,
	} {
		rawEv, err := newEvent(typ, "test", struct{}{})
		ev := stampFixed(t, rawEv, err)
		if ev.V != SchemaV {
			t.Errorf("event type %q: V = %d, want SchemaV (%d)", typ, ev.V, SchemaV)
		}
	}
}
