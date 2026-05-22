package scheduler

import (
	"testing"

	"lexa-hub/internal/northbound/discovery"
	"lexa-hub/internal/northbound/model"
)

// ───────────────────────────────────────────────────────────────────────
// Helpers
// ───────────────────────────────────────────────────────────────────────

const epoch int64 = 1700000000 // fixed reference time for all tests

func boolPtr(v bool) *bool   { return &v }
func int32Ptr(v int32) *int32 { return &v }
func int16Ptr(v int16) *int16 { return &v }

// makeProgram builds a ProgramState with the given primacy, a
// DefaultDERControl with OpModExpLimW = defaultW, and zero or more
// DERControl events.
func makeProgram(primacy uint8, mrid string, defaultW int16, events ...model.DERControl) discovery.ProgramState {
	return discovery.ProgramState{
		Program: model.DERProgram{MRID: mrid, Primacy: primacy},
		DefaultControl: &model.DefaultDERControl{
			MRID: "DDERC-" + mrid,
			DERControlBase: model.DERControlBase{
				OpModExpLimW: &model.ActivePower{Value: defaultW},
			},
		},
		Controls: &model.DERControlList{DERControl: events},
	}
}

// scheduledEvent builds a DERControl with the given parameters.
func scheduledEvent(mrid string, creationTime, start int64, duration uint32, limitW int16) model.DERControl {
	return model.DERControl{
		Resource:     model.Resource{Href: "/derp/0/derc/" + mrid},
		MRID:         mrid,
		CreationTime: creationTime,
		EventStatus:  &model.EventStatus{CurrentStatus: 0}, // Scheduled
		Interval:     model.DateTimeInterval{Start: start, Duration: duration},
		DERControlBase: model.DERControlBase{
			OpModExpLimW: &model.ActivePower{Value: limitW},
		},
	}
}

// ───────────────────────────────────────────────────────────────────────
// Tests: DefaultDERControl fallback
// ───────────────────────────────────────────────────────────────────────

func TestEvaluate_NilPrograms(t *testing.T) {
	s := New()
	if got := s.Evaluate(nil, epoch); got != nil {
		t.Errorf("expected nil for nil programs, got %+v", got)
	}
}

func TestEvaluate_EmptyPrograms(t *testing.T) {
	s := New()
	if got := s.Evaluate([]discovery.ProgramState{}, epoch); got != nil {
		t.Errorf("expected nil for empty programs, got %+v", got)
	}
}

func TestEvaluate_DefaultWhenNoEvents(t *testing.T) {
	s := New()
	programs := []discovery.ProgramState{makeProgram(1, "SP", 5000)}

	ac := s.Evaluate(programs, epoch)
	if ac == nil {
		t.Fatal("expected ActiveControl, got nil")
	}
	if ac.Source != "default" {
		t.Errorf("Source = %q, want default", ac.Source)
	}
	if ac.Base.OpModExpLimW == nil || ac.Base.OpModExpLimW.Value != 5000 {
		t.Errorf("OpModExpLimW = %v, want 5000", ac.Base.OpModExpLimW)
	}
	if ac.ValidUntil != 0 {
		t.Errorf("ValidUntil = %d, want 0 (default has no expiry)", ac.ValidUntil)
	}
}

func TestEvaluate_DefaultWhenEventNotYetStarted(t *testing.T) {
	s := New()
	evt := scheduledEvent("E1", epoch, epoch+600, 300, 3000) // starts 10 min from now
	programs := []discovery.ProgramState{makeProgram(1, "SP", 5000, evt)}

	ac := s.Evaluate(programs, epoch) // now is before the event
	if ac == nil || ac.Source != "default" {
		t.Errorf("expected default before event start, got %v", ac)
	}
}

func TestEvaluate_DefaultWhenEventEnded(t *testing.T) {
	s := New()
	evt := scheduledEvent("E1", epoch-1000, epoch-300, 120, 3000) // ended 180s ago
	programs := []discovery.ProgramState{makeProgram(1, "SP", 5000, evt)}

	ac := s.Evaluate(programs, epoch) // now is after the event ended
	if ac == nil || ac.Source != "default" {
		t.Errorf("expected default after event end, got %v", ac)
	}
}

// ───────────────────────────────────────────────────────────────────────
// Tests: Active event
// ───────────────────────────────────────────────────────────────────────

func TestEvaluate_ActiveEvent(t *testing.T) {
	s := New()
	// Event running from epoch-60 to epoch+540 (10-minute window, started 1 min ago)
	evt := scheduledEvent("E1", epoch-600, epoch-60, 600, 3000)
	programs := []discovery.ProgramState{makeProgram(1, "SP", 5000, evt)}

	ac := s.Evaluate(programs, epoch)
	if ac == nil {
		t.Fatal("expected ActiveControl, got nil")
	}
	if ac.Source != "event" {
		t.Errorf("Source = %q, want event", ac.Source)
	}
	if ac.MRID != "E1" {
		t.Errorf("MRID = %q, want E1", ac.MRID)
	}
	if ac.Base.OpModExpLimW == nil || ac.Base.OpModExpLimW.Value != 3000 {
		t.Errorf("OpModExpLimW = %v, want 3000", ac.Base.OpModExpLimW)
	}
	// ValidUntil = start + duration = (epoch-60) + 600 = epoch+540
	if ac.ValidUntil != epoch+540 {
		t.Errorf("ValidUntil = %d, want %d", ac.ValidUntil, epoch+540)
	}
}

func TestEvaluate_EventAtExactStart(t *testing.T) {
	s := New()
	evt := scheduledEvent("E1", epoch-10, epoch, 300, 3000) // starts exactly at epoch
	programs := []discovery.ProgramState{makeProgram(1, "SP", 5000, evt)}

	ac := s.Evaluate(programs, epoch) // serverNow == start → included
	if ac == nil || ac.Source != "event" {
		t.Errorf("expected event at start boundary, got %v", ac)
	}
}

func TestEvaluate_EventAtExactEnd(t *testing.T) {
	s := New()
	// end = epoch+300; serverNow = epoch+300 → excluded (interval is half-open)
	evt := scheduledEvent("E1", epoch-10, epoch, 300, 3000)
	programs := []discovery.ProgramState{makeProgram(1, "SP", 5000, evt)}

	ac := s.Evaluate(programs, epoch+300) // serverNow == start+duration → excluded
	if ac == nil || ac.Source != "default" {
		t.Errorf("expected default at end boundary (open interval), got %v", ac)
	}
}

// ───────────────────────────────────────────────────────────────────────
// Tests: Cancelled events
// ───────────────────────────────────────────────────────────────────────

func TestEvaluate_CancelledEventSkipped(t *testing.T) {
	s := New()
	evt := scheduledEvent("E1", epoch-600, epoch-60, 600, 3000)
	evt.EventStatus = &model.EventStatus{CurrentStatus: 6} // Cancelled
	programs := []discovery.ProgramState{makeProgram(1, "SP", 5000, evt)}

	ac := s.Evaluate(programs, epoch)
	if ac == nil || ac.Source != "default" {
		t.Errorf("expected default when only event is cancelled, got %v", ac)
	}
}

func TestEvaluate_CancelledCoexistsWithActive(t *testing.T) {
	s := New()
	cancelled := scheduledEvent("E1", epoch-600, epoch-60, 600, 1000)
	cancelled.EventStatus = &model.EventStatus{CurrentStatus: 6}
	active := scheduledEvent("E2", epoch-600, epoch-60, 600, 3000)
	programs := []discovery.ProgramState{makeProgram(1, "SP", 5000, cancelled, active)}

	ac := s.Evaluate(programs, epoch)
	if ac == nil || ac.Source != "event" || ac.MRID != "E2" {
		t.Errorf("expected E2 (non-cancelled), got %v", ac)
	}
}

// ───────────────────────────────────────────────────────────────────────
// Tests: Supersede logic
// ───────────────────────────────────────────────────────────────────────

func TestEvaluate_SupersedeOlderEvent(t *testing.T) {
	s := New()
	// E1 and E2 overlap at epoch; E2 has later creationTime → E2 wins.
	e1 := scheduledEvent("E1", epoch-100, epoch-60, 600, 3000)
	e1.EventStatus = &model.EventStatus{CurrentStatus: 0, PotentiallySuperseded: true}
	e2 := scheduledEvent("E2", epoch-50, epoch-60, 600, 2500) // same start, newer
	programs := []discovery.ProgramState{makeProgram(1, "SP", 5000, e1, e2)}

	ac := s.Evaluate(programs, epoch)
	if ac == nil {
		t.Fatal("expected ActiveControl")
	}
	if ac.MRID != "E2" {
		t.Errorf("MRID = %q, want E2 (supersedes E1)", ac.MRID)
	}
	if ac.Base.OpModExpLimW == nil || ac.Base.OpModExpLimW.Value != 2500 {
		t.Errorf("OpModExpLimW = %v, want 2500", ac.Base.OpModExpLimW)
	}
}

func TestEvaluate_NeitherSuperseded(t *testing.T) {
	// Two events overlap; neither has potentiallySuperseded=true. The one
	// with the later creationTime still wins (spec §12.3).
	s := New()
	e1 := scheduledEvent("E1", epoch-100, epoch-60, 600, 3000) // older
	e2 := scheduledEvent("E2", epoch-50, epoch-60, 600, 2500)  // newer
	programs := []discovery.ProgramState{makeProgram(1, "SP", 5000, e1, e2)}

	ac := s.Evaluate(programs, epoch)
	if ac == nil || ac.MRID != "E2" {
		t.Errorf("newer creationTime should win: got %v", ac)
	}
}

func TestEvaluate_SupersedeCreationTimeTiebreaker(t *testing.T) {
	// Same creationTime: higher MRID wins (lexicographic tiebreaker).
	s := New()
	e1 := scheduledEvent("E1", epoch-100, epoch-60, 600, 3000)
	e2 := scheduledEvent("E2", epoch-100, epoch-60, 600, 2500) // same creationTime
	programs := []discovery.ProgramState{makeProgram(1, "SP", 5000, e1, e2)}

	ac := s.Evaluate(programs, epoch)
	if ac == nil || ac.MRID != "E2" {
		t.Errorf("MRID tiebreaker: expected E2, got %v", ac)
	}
}

// ───────────────────────────────────────────────────────────────────────
// Tests: Randomized start
// ───────────────────────────────────────────────────────────────────────

func TestEvaluate_RandomizeStartCached(t *testing.T) {
	s := New()
	evt := scheduledEvent("E1", epoch-600, epoch, 600, 3000)
	evt.RandomizeStart = int32Ptr(30) // up to ±30s randomization
	programs := []discovery.ProgramState{makeProgram(1, "SP", 5000, evt)}

	// Call Evaluate twice — the randomized start must be the same both times.
	ac1 := s.Evaluate(programs, epoch+15)
	ac2 := s.Evaluate(programs, epoch+15)

	// Both calls must agree on whether the event is active.
	if (ac1 == nil) != (ac2 == nil) {
		t.Fatalf("inconsistent results: first=%v second=%v", ac1, ac2)
	}
	if ac1 != nil && ac2 != nil {
		if ac1.Source != ac2.Source || ac1.MRID != ac2.MRID {
			t.Errorf("calls disagree: first=%+v second=%+v", ac1, ac2)
		}
	}
}

func TestEvaluate_RandomizeStartWithinBounds(t *testing.T) {
	// Run many schedulers to verify the randomized offset stays in [-30, +30].
	window := int32(30)
	for i := 0; i < 100; i++ {
		s := New()
		evt := scheduledEvent("E1", epoch-600, epoch, 600, 3000)
		evt.RandomizeStart = &window

		// randomizedStart handles its own locking; call it first, then read the cache.
		_ = s.randomizedStart(&evt)

		s.mu.Lock()
		offset, ok := s.randOffsets["E1"]
		s.mu.Unlock()

		if !ok {
			t.Fatal("randOffsets not populated after randomizedStart")
		}
		if offset < -window || offset > window {
			t.Errorf("randomized offset %d out of bounds [-%d, +%d]", offset, window, window)
		}
	}
}

// ───────────────────────────────────────────────────────────────────────
// Tests: Multi-program precedence
// ───────────────────────────────────────────────────────────────────────

func TestEvaluate_HighPriorityEventWins(t *testing.T) {
	s := New()
	// SP (primacy=1) has an active event; SYS (primacy=10) also has one.
	// SP wins because it has lower primacy (higher priority).
	spEvt := scheduledEvent("SP-E1", epoch-600, epoch-60, 600, 2000) // SP active
	sysEvt := scheduledEvent("SY-E1", epoch-600, epoch-60, 600, 8000) // SYS active

	programs := []discovery.ProgramState{
		makeProgram(10, "SYS", 9000, sysEvt),
		makeProgram(1, "SP", 5000, spEvt),
	}

	ac := s.Evaluate(programs, epoch)
	if ac == nil {
		t.Fatal("expected ActiveControl")
	}
	if ac.MRID != "SP-E1" {
		t.Errorf("expected SP-E1 (highest priority), got %q", ac.MRID)
	}
	if ac.Base.OpModExpLimW == nil || ac.Base.OpModExpLimW.Value != 2000 {
		t.Errorf("OpModExpLimW = %v, want 2000", ac.Base.OpModExpLimW)
	}
}

func TestEvaluate_HighPriorityDefaultBeatsLowPriorityEvent(t *testing.T) {
	// SP (primacy=1) has no active event → falls to its default (5kW).
	// SYS (primacy=10) has an active event (8kW).
	// SP's default wins over SYS's event because SP has higher priority.
	s := New()
	sysEvt := scheduledEvent("SY-E1", epoch-600, epoch-60, 600, 8000)
	programs := []discovery.ProgramState{
		makeProgram(10, "SYS", 9000, sysEvt),
		makeProgram(1, "SP", 5000), // no events — SP has no DERControls at all
	}

	ac := s.Evaluate(programs, epoch)
	if ac == nil {
		t.Fatal("expected ActiveControl")
	}
	if ac.Source != "default" {
		t.Errorf("Source = %q, want default", ac.Source)
	}
	if ac.MRID != "DDERC-SP" {
		t.Errorf("MRID = %q, want DDERC-SP", ac.MRID)
	}
	if ac.Base.OpModExpLimW == nil || ac.Base.OpModExpLimW.Value != 5000 {
		t.Errorf("OpModExpLimW = %v, want 5000", ac.Base.OpModExpLimW)
	}
}

// ───────────────────────────────────────────────────────────────────────
// Tests: State transitions (default → event → default)
// ───────────────────────────────────────────────────────────────────────

func TestEvaluate_StateTransitions(t *testing.T) {
	s := New()
	eventStart := epoch + 300
	eventDuration := uint32(600)
	evt := scheduledEvent("E1", epoch, eventStart, eventDuration, 3000)

	programs := []discovery.ProgramState{makeProgram(1, "SP", 5000, evt)}

	cases := []struct {
		name       string
		serverNow  int64
		wantSource string
	}{
		{"before start", eventStart - 1, "default"},
		{"at start", eventStart, "event"},
		{"mid event", eventStart + 300, "event"},
		{"at end (exclusive)", eventStart + int64(eventDuration), "default"},
		{"after end", eventStart + int64(eventDuration) + 60, "default"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ac := s.Evaluate(programs, tc.serverNow)
			if ac == nil {
				t.Fatalf("got nil, want source=%q", tc.wantSource)
			}
			if ac.Source != tc.wantSource {
				t.Errorf("Source = %q, want %q", ac.Source, tc.wantSource)
			}
		})
	}
}

// ───────────────────────────────────────────────────────────────────────
// Tests: ServerNow helper
// ───────────────────────────────────────────────────────────────────────

func TestServerNow_ZeroOffset(t *testing.T) {
	before := ServerNow(0)
	after := ServerNow(0)
	if before <= 0 || after <= 0 {
		t.Error("ServerNow should return a positive unix timestamp")
	}
	if after < before {
		t.Error("ServerNow should be non-decreasing")
	}
}

func TestServerNow_PositiveOffset(t *testing.T) {
	got := ServerNow(100)
	local := ServerNow(0)
	if got < local+99 || got > local+101 {
		t.Errorf("ServerNow(100) = %d, local = %d, want ~%d", got, local, local+100)
	}
}

// ───────────────────────────────────────────────────────────────────────
// Tests: randOffsets pruning
// ───────────────────────────────────────────────────────────────────────

func TestPruneRandOffsets_RemovesStaleEntries(t *testing.T) {
	s := New()
	r := int32(10)

	evKeep := scheduledEvent("keep", epoch-600, epoch-60, 600, 3000)
	evKeep.RandomizeStart = &r
	evStale := scheduledEvent("stale", epoch-600, epoch-60, 600, 3000)
	evStale.RandomizeStart = &r

	programs := []discovery.ProgramState{makeProgram(1, "SP", 5000, evKeep, evStale)}
	s.Evaluate(programs, epoch)

	if len(s.randOffsets) != 2 {
		t.Fatalf("expected 2 cached offsets, got %d", len(s.randOffsets))
	}

	// Remove evStale from the program.
	programs = []discovery.ProgramState{makeProgram(1, "SP", 5000, evKeep)}
	s.Evaluate(programs, epoch)

	if _, ok := s.randOffsets["stale"]; ok {
		t.Error("stale MRID should have been pruned from randOffsets")
	}
	if _, ok := s.randOffsets["keep"]; !ok {
		t.Error("active MRID 'keep' should be retained in randOffsets")
	}
}

func TestPruneRandOffsets_EmptyMapNoPanic(t *testing.T) {
	s := New()
	programs := []discovery.ProgramState{makeProgram(1, "SP", 5000)}
	// Should not panic with empty randOffsets map.
	s.Evaluate(programs, epoch)
}
