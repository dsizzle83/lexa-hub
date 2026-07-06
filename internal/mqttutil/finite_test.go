package mqttutil

import (
	"testing"

	"lexa-hub/internal/bus"
)

// TestSubscribeDropsNonFiniteValueAndAlarms is the end-to-end proof (GAP-09,
// TASK-055) that Subscribe never hands handler a message whose Finite()
// reports non-finite, and that the drop is counted via
// bus.RecordDecodeFailure rather than only logged. It uses a stub decode
// target (alwaysNonFinite) whose Finite() always errors, standing in for
// whatever future lax-decode path might someday hand Subscribe a live NaN
// that a real bus.* type's own Finite() (see
// TestSubscribeValidActiveControlStillDelivered below and
// internal/bus/nan_reject_test.go for the real-type cases, including
// ActiveControl) would likewise reject — the payload here decodes cleanly
// into the stub (it has no fields), so this is purely exercising the
// Finite()-checked branch of Subscribe, not stdlib's separate NaN-token
// rejection.
func TestSubscribeDropsNonFiniteValueAndAlarms(t *testing.T) {
	const topic = "lexa/csip/control"
	fc := &fakeClient{}
	called := false
	if err := Subscribe(fc, topic, func(_ string, msg alwaysNonFinite) {
		called = true
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	before := bus.DecodeFailures()[topic]
	deliver(t, fc, topic, []byte(`{"v":1,"source":"event"}`))
	after := bus.DecodeFailures()[topic]

	if called {
		t.Error("handler was called for a message whose Finite() reports non-finite; " +
			"it must be dropped before handler runs (fail-closed: last-known-good control holds)")
	}
	if after != before+1 {
		t.Errorf("DecodeFailures()[%q] = %d, want %d (Finite() failure must be counted)", topic, after, before+1)
	}
}

// TestSubscribeCountsPlainUnmarshalFailure closes the other half of GAP-09:
// before this task, a malformed payload on a non-control topic was only
// log.Printf'd (invisible to anything scraping metrics) — the "silent" half
// of "silent drop" the task exists to fix. A raw json.Unmarshal failure must
// now also increment bus.RecordDecodeFailure, exactly like a Finite()
// failure or a version reject.
func TestSubscribeCountsPlainUnmarshalFailure(t *testing.T) {
	const topic = "lexa/measurements/inv0"
	fc := &fakeClient{}
	called := false
	if err := Subscribe(fc, topic, func(_ string, msg bus.Measurement) {
		called = true
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	before := bus.DecodeFailures()[topic]
	deliver(t, fc, topic, []byte(`{not valid json`))
	after := bus.DecodeFailures()[topic]

	if called {
		t.Error("handler was called for malformed JSON")
	}
	if after != before+1 {
		t.Errorf("DecodeFailures()[%q] = %d, want %d (a plain unmarshal failure must be counted, not just logged)",
			topic, after, before+1)
	}
}

// TestSubscribeValidActiveControlStillDelivered guards against a
// false-rejection regression: a genuinely valid ActiveControl (all-nil
// limits, or real finite ones) must still reach handler unchanged. Blast
// radius requirement: "valid messages ... must decode byte-identically."
func TestSubscribeValidActiveControlStillDelivered(t *testing.T) {
	const topic = "lexa/csip/control"
	fc := &fakeClient{}
	var got bus.ActiveControl
	called := false
	if err := Subscribe(fc, topic, func(_ string, msg bus.ActiveControl) {
		called = true
		got = msg
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	deliver(t, fc, topic, []byte(`{"v":1,"source":"event","mrid":"m1","exp_lim_w":5000,"ts":1}`))
	if !called {
		t.Fatal("handler was not called for a valid ActiveControl payload")
	}
	if got.ExpLimW == nil || *got.ExpLimW != 5000 {
		t.Errorf("ExpLimW = %v, want 5000", got.ExpLimW)
	}
}

// alwaysNonFinite is a decode target whose Finite() always fails, standing
// in for a message that decoded successfully but carries a value a real
// bus.* type's own Finite() would also reject. It has no fields — the JSON
// payload used in tests above is ignored by it entirely; only its Finite()
// behavior matters for TestSubscribeDropsNonFiniteActiveControlAndAlarms.
type alwaysNonFinite struct{}

func (alwaysNonFinite) Finite() error { return errAlwaysNonFinite }

var errAlwaysNonFinite = &nonFiniteStub{}

type nonFiniteStub struct{}

func (*nonFiniteStub) Error() string { return "stub: always non-finite" }
