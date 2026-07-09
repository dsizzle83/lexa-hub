package main

import (
	"encoding/json"
	"testing"
	"time"

	"lexa-hub/internal/bus"
)

// TestResultWaiter_DeliveryMatchesByID pins the core matching behavior: a
// registered id receives exactly the IntentResult whose ID matches, and the
// pending set is empty afterward (no leak).
func TestResultWaiter_DeliveryMatchesByID(t *testing.T) {
	fc := &fakeAPIMQTTClient{}
	w, err := newResultWaiter(fc)
	if err != nil {
		t.Fatalf("newResultWaiter: %v", err)
	}

	ch := w.expect("id-1")
	if w.pendingCount() != 1 {
		t.Fatalf("pendingCount = %d after expect, want 1", w.pendingCount())
	}

	payload, _ := json.Marshal(bus.IntentResult{ID: "id-1", Kind: "mode", Outcome: "applied", Ts: 1})
	deliverAPI(t, fc, bus.TopicIntentResult, payload)

	select {
	case res := <-ch:
		if res.Outcome != "applied" {
			t.Errorf("Outcome = %q, want %q", res.Outcome, "applied")
		}
	case <-time.After(time.Second):
		t.Fatal("expected result on channel, got nothing")
	}
	if w.pendingCount() != 0 {
		t.Errorf("pendingCount = %d after delivery, want 0 (leak)", w.pendingCount())
	}
}

// TestResultWaiter_UnmatchedIDDropped pins the "expected traffic, not an
// error" behavior: a result for an id nobody registered (already delivered,
// cancelled, or never issued by this process) must be dropped without
// panicking and without disturbing any OTHER pending waiter.
func TestResultWaiter_UnmatchedIDDropped(t *testing.T) {
	fc := &fakeAPIMQTTClient{}
	w, err := newResultWaiter(fc)
	if err != nil {
		t.Fatalf("newResultWaiter: %v", err)
	}

	ch := w.expect("id-known")
	payload, _ := json.Marshal(bus.IntentResult{ID: "id-unknown", Kind: "mode", Outcome: "applied", Ts: 1})
	deliverAPI(t, fc, bus.TopicIntentResult, payload)

	select {
	case res := <-ch:
		t.Fatalf("expected no delivery for id-known from an unmatched result, got %+v", res)
	case <-time.After(50 * time.Millisecond):
		// expected: nothing delivered
	}
	if w.pendingCount() != 1 {
		t.Errorf("pendingCount = %d, want 1 (id-known must still be pending)", w.pendingCount())
	}
}

// TestResultWaiter_CancelRemovesEntryWithoutDelivery pins cancel's cleanup
// role: after cancel, the id is gone from the pending set, and a LATE
// delivery for that id (arriving after cancel) is silently dropped rather
// than panicking on a closed/missing channel.
func TestResultWaiter_CancelRemovesEntryWithoutDelivery(t *testing.T) {
	fc := &fakeAPIMQTTClient{}
	w, err := newResultWaiter(fc)
	if err != nil {
		t.Fatalf("newResultWaiter: %v", err)
	}

	w.expect("id-1")
	w.cancel("id-1")
	if w.pendingCount() != 0 {
		t.Fatalf("pendingCount = %d after cancel, want 0", w.pendingCount())
	}

	// A late delivery for the cancelled id must not panic.
	payload, _ := json.Marshal(bus.IntentResult{ID: "id-1", Kind: "mode", Outcome: "applied", Ts: 1})
	deliverAPI(t, fc, bus.TopicIntentResult, payload)

	// Cancelling an already-absent id must also be a no-op, not a panic.
	w.cancel("id-1")
}

// TestResultWaiter_MultipleConcurrentExpectsDeliverIndependently is a
// goroutine-safety sanity check: several concurrently expected ids each get
// their own, distinct result.
func TestResultWaiter_MultipleConcurrentExpectsDeliverIndependently(t *testing.T) {
	fc := &fakeAPIMQTTClient{}
	w, err := newResultWaiter(fc)
	if err != nil {
		t.Fatalf("newResultWaiter: %v", err)
	}

	ids := []string{"a", "b", "c", "d"}
	chans := make(map[string]<-chan bus.IntentResult, len(ids))
	for _, id := range ids {
		chans[id] = w.expect(id)
	}
	if w.pendingCount() != len(ids) {
		t.Fatalf("pendingCount = %d, want %d", w.pendingCount(), len(ids))
	}

	for _, id := range ids {
		payload, _ := json.Marshal(bus.IntentResult{ID: id, Kind: "mode", Outcome: "applied-" + id, Ts: 1})
		deliverAPI(t, fc, bus.TopicIntentResult, payload)
	}

	for _, id := range ids {
		select {
		case res := <-chans[id]:
			if res.Outcome != "applied-"+id {
				t.Errorf("id %q: Outcome = %q, want %q", id, res.Outcome, "applied-"+id)
			}
		case <-time.After(time.Second):
			t.Fatalf("id %q: expected a result, got nothing", id)
		}
	}
	if w.pendingCount() != 0 {
		t.Errorf("pendingCount = %d after all delivered, want 0", w.pendingCount())
	}
}
