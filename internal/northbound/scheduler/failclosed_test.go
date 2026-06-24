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
