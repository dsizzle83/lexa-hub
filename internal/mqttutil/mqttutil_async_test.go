package mqttutil

import (
	"errors"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// erroredDoneToken stands in for a publish that the broker already rejected
// by the time the caller checks — Done() is closed immediately (as if the
// PUBACK/failure arrived synchronously in this fake), Error() non-nil.
type erroredDoneToken struct{}

func (erroredDoneToken) Wait() bool                     { return true }
func (erroredDoneToken) WaitTimeout(time.Duration) bool { return true }
func (erroredDoneToken) Done() <-chan struct{}          { c := make(chan struct{}); close(c); return c }
func (erroredDoneToken) Error() error                   { return errors.New("no ack") }

// clientStub is a minimal mqtt.Client double whose every Publish call
// returns the same pre-built token — used by the Harvest tests below to
// exercise exactly one completion shape per test.
type clientStub struct {
	fakeClient
	tok mqtt.Token
}

func (c *clientStub) Publish(topic string, qos byte, retained bool, payload interface{}) mqtt.Token {
	c.fakeClient.Publish(topic, qos, retained, payload)
	return c.tok
}

// TestPublishJSONRetainedAsync_ReturnsImmediately verifies the whole point of
// the async helper (§11): against a client whose token never completes
// (standing in for a sick-but-alive broker), PublishJSONRetainedAsync must
// return in effectively zero time — never waiting anywhere close to
// publishTimeout — handing back a PendingPub instead.
func TestPublishJSONRetainedAsync_ReturnsImmediately(t *testing.T) {
	fc := &fakeClient{} // fakeClient.Publish returns newNeverToken()
	start := time.Now()
	pp, err := PublishJSONRetainedAsync(fc, "lexa/desired/battery/b0", map[string]int{"setpoint_w": -500})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("PublishJSONRetainedAsync: %v", err)
	}
	if elapsed > 50*time.Millisecond {
		t.Fatalf("PublishJSONRetainedAsync took %s — must return immediately, never wait for the PUBACK", elapsed)
	}
	if pp == nil {
		t.Fatal("expected a non-nil PendingPub")
	}
	if pp.Topic() != "lexa/desired/battery/b0" {
		t.Fatalf("Topic() = %q, want lexa/desired/battery/b0", pp.Topic())
	}
	if len(fc.publishes) != 1 {
		t.Fatalf("got %d Publish calls, want 1", len(fc.publishes))
	}
	if !fc.publishes[0].retained {
		t.Fatal("PublishJSONRetainedAsync must publish retained")
	}
	if fc.publishes[0].qos != 1 {
		t.Fatalf("qos = %d, want 1", fc.publishes[0].qos)
	}

	// Still pending (never token): Harvest must not block either.
	start = time.Now()
	done, timedOut, herr := pp.Harvest(5 * time.Second)
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("Harvest must never block")
	}
	if done || timedOut || herr != nil {
		t.Fatalf("Harvest on a still-pending publish = (done=%v timedOut=%v err=%v), want (false,false,nil)", done, timedOut, herr)
	}
}

// TestPublishJSONAsync_MarshalErrorIsSynchronous verifies the one part of the
// async path that IS still synchronous: a marshal failure. There is nothing
// to defer — the message was never queued — so it must return the error
// directly instead of via a PendingPub.
func TestPublishJSONAsync_MarshalErrorIsSynchronous(t *testing.T) {
	fc := &fakeClient{}
	pp, err := PublishJSONAsync(fc, "lexa/x", func() {}) // funcs are not JSON-marshalable
	if err == nil {
		t.Fatal("expected a marshal error")
	}
	if pp != nil {
		t.Fatal("expected a nil PendingPub on marshal error")
	}
	if len(fc.publishes) != 0 {
		t.Fatalf("got %d Publish calls, want 0 (marshal must fail before Publish is ever called)", len(fc.publishes))
	}
}

// TestPendingPubHarvest_Done verifies a PendingPub whose token has already
// completed successfully harvests as done, no error, on the very first call —
// the fast-broker case, which must be indistinguishable in cost from the
// synchronous path once the caller happens to check after the ack arrived.
// It also re-harvests to confirm the read is stable (idempotent) on an
// already-closed completion channel — the property the Harvest doc comment
// leans on instead of paho's racy WaitTimeout(0).
func TestPendingPubHarvest_Done(t *testing.T) {
	cs := &clientStub{tok: newDoneToken()}
	pp, err := PublishJSONRetainedAsync(cs, "lexa/desired/solar/inv0", map[string]int{"ceiling_w": 3000})
	if err != nil {
		t.Fatalf("PublishJSONRetainedAsync: %v", err)
	}
	for i := 0; i < 1000; i++ {
		done, timedOut, herr := pp.Harvest(5 * time.Second)
		if !done || timedOut || herr != nil {
			t.Fatalf("Harvest iteration %d = (done=%v timedOut=%v err=%v), want (true,false,nil)", i, done, timedOut, herr)
		}
	}
}

// TestPendingPubHarvest_Error verifies a PendingPub whose token completed
// with a broker-reported error harvests as done=true with that error, and
// drives the OnPublishFail instrumentation hook exactly like a synchronous
// PublishJSON failure would (TASK-044 parity).
func TestPendingPubHarvest_Error(t *testing.T) {
	cs := &clientStub{tok: erroredDoneToken{}}
	failCount := 0
	instrumentations.Store(cs, &instState{inst: Instrumentation{OnPublishFail: func() { failCount++ }}})
	defer instrumentations.Delete(cs)

	pp, err := PublishJSONRetainedAsync(cs, "lexa/desired/evse/cs-001", map[string]int{"max_current_a": 16})
	if err != nil {
		t.Fatalf("PublishJSONRetainedAsync: %v", err)
	}
	done, timedOut, herr := pp.Harvest(5 * time.Second)
	if !done || timedOut || herr == nil {
		t.Fatalf("Harvest = (done=%v timedOut=%v err=%v), want (true,false,non-nil)", done, timedOut, herr)
	}
	if failCount != 1 {
		t.Fatalf("OnPublishFail called %d times, want 1", failCount)
	}
}

// TestPendingPubHarvest_PendingThenTimedOut verifies the "sick-but-alive
// broker" case: a token that never completes reports done=false always, and
// timedOut only once the caller's own bound has elapsed since the publish was
// sent — never before. This is the signal actuators use to reset their
// dedupe/retry state without waiting any further (TASK-046 step 2).
func TestPendingPubHarvest_PendingThenTimedOut(t *testing.T) {
	cs := &clientStub{tok: newNeverToken()}
	pp, err := PublishJSONRetainedAsync(cs, "lexa/desired/battery/b0", map[string]int{"setpoint_w": -500})
	if err != nil {
		t.Fatalf("PublishJSONRetainedAsync: %v", err)
	}

	// Immediately after sending, well within a generous bound: not timed out.
	done, timedOut, herr := pp.Harvest(time.Hour)
	if done || timedOut || herr != nil {
		t.Fatalf("Harvest with a huge timeout = (done=%v timedOut=%v err=%v), want (false,false,nil)", done, timedOut, herr)
	}

	// A negative timeout means "any elapsed time counts as too long" —
	// simulates having harvested well after the bound without an actual sleep.
	done, timedOut, herr = pp.Harvest(-1 * time.Nanosecond)
	if done || !timedOut || herr != nil {
		t.Fatalf("Harvest past its bound = (done=%v timedOut=%v err=%v), want (false,true,nil)", done, timedOut, herr)
	}
}
