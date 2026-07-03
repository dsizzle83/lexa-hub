package scheduler

import (
	"testing"

	"lexa-hub/internal/northbound/discovery"
	"lexa-hub/internal/northbound/model"
)

// progs wraps a single event in the highest-priority program (primacy 1) with a
// plausible default, matching makeProgram's shape.
func progs(events ...model.DERControl) []discovery.ProgramState {
	return []discovery.ProgramState{makeProgram(1, "SP", 5000, events...)}
}

// eventWithExpLim builds a DERControl active at `start` for `duration` seconds
// whose OpModExpLimW decodes to value×10^multiplier watts. Used to exercise the
// fail-closed retention and the implausible-limit rejection.
func eventWithExpLim(mrid string, start int64, duration uint32, value int16, mult int8) model.DERControl {
	return model.DERControl{
		Resource:     model.Resource{Href: "/derp/0/derc/" + mrid},
		MRID:         mrid,
		CreationTime: start,
		EventStatus:  &model.EventStatus{CurrentStatus: 0},
		Interval:     model.DateTimeInterval{Start: start, Duration: duration},
		DERControlBase: model.DERControlBase{
			OpModExpLimW: &model.ActivePower{Value: value, Multiplier: mult},
		},
	}
}

// A transient empty/malformed discovery cycle must NOT drop an unexpired
// control: the scheduler re-serves the last-known-good, marked Held.
func TestEvaluate_HoldsLastGoodOnEmptyPrograms(t *testing.T) {
	s := New()
	evt := eventWithExpLim("E1", epoch, 3600, 1000, 0) // active [epoch, epoch+3600)
	programs := progs(evt)

	good := s.Evaluate(programs, epoch)
	if good == nil || good.MRID != "E1" || good.Held {
		t.Fatalf("expected fresh control E1 (not held), got %+v", good)
	}

	// Empty programs mid-window: hold last-known-good rather than failing open.
	held := s.Evaluate(nil, epoch+10)
	if held == nil {
		t.Fatal("expected last-known-good to be held, got nil (failed open)")
	}
	if held.MRID != "E1" {
		t.Errorf("held MRID = %q, want E1", held.MRID)
	}
	if !held.Held {
		t.Error("expected Held=true on a re-served last-known-good control")
	}
	if held.Base.OpModExpLimW == nil || held.Base.OpModExpLimW.Value != 1000 {
		t.Errorf("held control lost its export limit: %+v", held.Base.OpModExpLimW)
	}
}

// Once the held control passes its own ValidUntil, the scheduler releases it.
func TestEvaluate_ReleasesLastGoodAfterExpiry(t *testing.T) {
	s := New()
	evt := eventWithExpLim("E1", epoch, 100, 1000, 0) // ValidUntil = epoch+100
	programs := progs(evt)

	if good := s.Evaluate(programs, epoch); good == nil {
		t.Fatal("expected fresh control, got nil")
	}
	// Empty programs AFTER expiry: release (nil), do not hold a stale cap forever.
	if got := s.Evaluate(nil, epoch+200); got != nil {
		t.Errorf("expected nil after last-known-good expired, got %+v", got)
	}
}

// A malformed control (overflow-bait ActivePower) is never adopted; the
// scheduler holds the prior good control instead of the absurd cap.
func TestEvaluate_RejectsImplausibleHoldsLastGood(t *testing.T) {
	s := New()
	good := progs(eventWithExpLim("GOOD", epoch, 3600, 1000, 0))
	if ac := s.Evaluate(good, epoch); ac == nil || ac.MRID != "GOOD" {
		t.Fatalf("expected GOOD control, got %+v", ac)
	}

	// 32767×10^9 ≈ 3.3e13 W — an effectively-infinite cap.
	garbage := progs(eventWithExpLim("BAD", epoch, 3600, 32767, 9))
	got := s.Evaluate(garbage, epoch+10)
	if got == nil {
		t.Fatal("expected held last-known-good, got nil")
	}
	if got.MRID != "GOOD" || !got.Held {
		t.Errorf("expected held GOOD control, got mrid=%q held=%v", got.MRID, got.Held)
	}
	if w := float64(got.Base.OpModExpLimW.Value) * 1; w != 1000 {
		t.Errorf("held control adopted the garbage limit: value=%d", got.Base.OpModExpLimW.Value)
	}
}

// With no prior good control, a malformed resource resolves to nil — the absurd
// limit is rejected outright rather than adopted as an infinite cap.
func TestEvaluate_RejectsImplausibleWithNoPrior(t *testing.T) {
	s := New()
	garbage := progs(eventWithExpLim("BAD", epoch, 3600, 32767, 9))
	if got := s.Evaluate(garbage, epoch); got != nil {
		t.Errorf("expected nil for a malformed control with no last-known-good, got %+v", got)
	}
}

// When the server explicitly clears its controls — the DERProgram exists but
// carries no active event and no DefaultDERControl — the scheduler must release
// the last-known-good immediately (curtailment-release fix). Holding in this
// case would prevent the hub from relaxing a curtailment after the utility
// signals the event window is over via DELETE /admin/control.
func TestEvaluate_ReleasesOnExplicitServerClear(t *testing.T) {
	s := New()
	// Establish a last-known-good: active event [epoch, epoch+3600).
	evt := eventWithExpLim("E1", epoch, 3600, 1000, 0)
	lkg := s.Evaluate(progs(evt), epoch)
	if lkg == nil || lkg.MRID != "E1" {
		t.Fatalf("expected fresh control E1, got %+v", lkg)
	}

	// Server explicitly clears: program exists but has no events and no default.
	emptyProgram := []discovery.ProgramState{{
		Program:        model.DERProgram{MRID: "SP", Primacy: 1},
		DefaultControl: nil,
		Controls:       &model.DERControlList{DERControl: nil},
	}}
	got := s.Evaluate(emptyProgram, epoch+10)
	if got != nil {
		t.Errorf("expected nil after explicit server clear, got %+v (mrid=%s held=%v)",
			got, got.MRID, got.Held)
	}
}

// Contrast: an empty PROGRAM LIST (no programs at all) still holds last-known-good
// because an empty list could be a transient/hostile injection, not an explicit clear.
func TestEvaluate_HoldsOnEmptyProgramList(t *testing.T) {
	s := New()
	evt := eventWithExpLim("E1", epoch, 3600, 1000, 0)
	if lkg := s.Evaluate(progs(evt), epoch); lkg == nil {
		t.Fatal("expected fresh control, got nil")
	}

	// Nil programs (no list at all) → hold.
	held := s.Evaluate(nil, epoch+10)
	if held == nil {
		t.Fatal("expected last-known-good held on nil program list, got nil")
	}
	if held.MRID != "E1" || !held.Held {
		t.Errorf("expected held E1, got mrid=%q held=%v", held.MRID, held.Held)
	}

	// Empty slice (no programs) → also hold.
	held2 := s.Evaluate([]discovery.ProgramState{}, epoch+10)
	if held2 == nil || !held2.Held {
		t.Errorf("expected held on empty program slice, got %+v", held2)
	}
}

func TestPlausibleLimit(t *testing.T) {
	cases := []struct {
		name  string
		ap    *model.ActivePower
		valid bool
	}{
		{"nil is unconstrained", nil, true},
		{"zero", &model.ActivePower{Value: 0}, true},
		{"normal 5kW", &model.ActivePower{Value: 5000}, true},
		{"normal negative", &model.ActivePower{Value: -5000}, true},
		{"1MW scaled", &model.ActivePower{Value: 1000, Multiplier: 3}, true},
		{"at ceiling 1GW", &model.ActivePower{Value: 1000, Multiplier: 6}, true},
		{"overflow bait 3.3e13", &model.ActivePower{Value: 32767, Multiplier: 9}, false},
		{"extreme multiplier", &model.ActivePower{Value: 1, Multiplier: 100}, false},
	}
	for _, c := range cases {
		if got := plausibleLimit(c.ap); got != c.valid {
			t.Errorf("%s: plausibleLimit = %v, want %v", c.name, got, c.valid)
		}
	}
}

// progsNoDefault wraps events in a primacy-1 program with NO DefaultDERControl,
// so an inactive event resolves to (nil, programFound=true) — the explicit-clear
// path the clock-regression guard discriminates on.
func progsNoDefault(events ...model.DERControl) []discovery.ProgramState {
	return []discovery.ProgramState{{
		Program:  model.DERProgram{MRID: "SP", Primacy: 1},
		Controls: &model.DERControlList{DERControl: events},
	}}
}

// A server clock that steps BACK past the adopted event's start (NTP
// correction) makes the still-served event read as not-yet-started. That is
// not a withdrawal: the scheduler must hold the control, not flap it with the
// jitter (QA 2026-07-02: clock-jitter — whole windows went unenforced when the
// discovery period aliased against a ±60 s correction cycle).
func TestEvaluate_HoldsThroughClockRegression(t *testing.T) {
	s := New()
	evt := eventWithExpLim("E1", epoch, 600, 1000, 0) // active [epoch, epoch+600)
	programs := progsNoDefault(evt)

	if ac := s.Evaluate(programs, epoch+30); ac == nil || ac.MRID != "E1" || ac.Held {
		t.Fatalf("expected fresh control E1, got %+v", ac)
	}

	// Clock lurches 60 s back: serverNow < start, event still served.
	held := s.Evaluate(programs, epoch-30)
	if held == nil {
		t.Fatal("clock regression released the still-served control (jitter flap)")
	}
	if held.MRID != "E1" || !held.Held {
		t.Errorf("expected E1 held through the regression, got %+v", held)
	}

	// Clock recovers: the event is fresh-resolved again.
	if ac := s.Evaluate(programs, epoch+35); ac == nil || ac.MRID != "E1" || ac.Held {
		t.Errorf("expected fresh E1 after the clock recovered, got %+v", ac)
	}
}

// The guard must NOT defeat a genuine withdrawal: if the event is gone from
// the served list (utility deleted it), release even mid-regression.
func TestEvaluate_ClockRegressionStillReleasesWithdrawnEvent(t *testing.T) {
	s := New()
	evt := eventWithExpLim("E1", epoch, 600, 1000, 0)
	if ac := s.Evaluate(progsNoDefault(evt), epoch+30); ac == nil {
		t.Fatal("expected fresh control")
	}
	if got := s.Evaluate(progsNoDefault(), epoch-30); got != nil {
		t.Errorf("withdrawn event must release even during a clock regression, got %+v", got)
	}
}

// A cancelled event (currentStatus=6) is a withdrawal too — no hold.
func TestEvaluate_ClockRegressionStillReleasesCancelledEvent(t *testing.T) {
	s := New()
	evt := eventWithExpLim("E1", epoch, 600, 1000, 0)
	if ac := s.Evaluate(progsNoDefault(evt), epoch+30); ac == nil {
		t.Fatal("expected fresh control")
	}
	cancelled := eventWithExpLim("E1", epoch, 600, 1000, 0)
	cancelled.EventStatus = &model.EventStatus{CurrentStatus: 6}
	if got := s.Evaluate(progsNoDefault(cancelled), epoch-30); got != nil {
		t.Errorf("cancelled event must release even during a clock regression, got %+v", got)
	}
}

// A genuinely ENDED event (serverNow past its ValidUntil) releases even while
// still listed — the guard must not resurrect finished events, or
// curtailment-release regresses.
func TestEvaluate_ClockRegressionDoesNotResurrectEndedEvent(t *testing.T) {
	s := New()
	evt := eventWithExpLim("E1", epoch, 100, 1000, 0) // ValidUntil = epoch+100
	if ac := s.Evaluate(progsNoDefault(evt), epoch+30); ac == nil {
		t.Fatal("expected fresh control")
	}
	if got := s.Evaluate(progsNoDefault(evt), epoch+200); got != nil {
		t.Errorf("ended event must stay released while still listed, got %+v", got)
	}
}
