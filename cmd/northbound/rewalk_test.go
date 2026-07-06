package main

import (
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
)

// fakeNBClient is a minimal mqtt.Client double capturing Publish calls, for
// testing handleRewalkRequest without a real broker. Every other method
// panics if called, so a test that accidentally depends on unimplemented
// behavior fails loudly rather than silently no-opping.
type fakeNBClient struct {
	publishes []fakeNBPublish
}

type fakeNBPublish struct {
	topic    string
	retained bool
	payload  []byte
}

func (f *fakeNBClient) IsConnected() bool      { return true }
func (f *fakeNBClient) IsConnectionOpen() bool { return true }
func (f *fakeNBClient) Connect() mqtt.Token    { panic("not implemented") }
func (f *fakeNBClient) Disconnect(quiesce uint) {
}
func (f *fakeNBClient) Publish(topic string, qos byte, retained bool, payload interface{}) mqtt.Token {
	var b []byte
	switch p := payload.(type) {
	case []byte:
		b = p
	case string:
		b = []byte(p)
	}
	f.publishes = append(f.publishes, fakeNBPublish{topic: topic, retained: retained, payload: b})
	return &fakeNBDoneToken{}
}
func (f *fakeNBClient) Subscribe(topic string, qos byte, callback mqtt.MessageHandler) mqtt.Token {
	panic("not implemented")
}
func (f *fakeNBClient) SubscribeMultiple(filters map[string]byte, callback mqtt.MessageHandler) mqtt.Token {
	panic("not implemented")
}
func (f *fakeNBClient) Unsubscribe(topics ...string) mqtt.Token { panic("not implemented") }
func (f *fakeNBClient) AddRoute(topic string, callback mqtt.MessageHandler) {
	panic("not implemented")
}
func (f *fakeNBClient) OptionsReader() mqtt.ClientOptionsReader {
	panic("not implemented")
}

// fakeNBDoneToken is an mqtt.Token that has already completed successfully —
// standing in for a healthy broker's immediate PUBACK.
type fakeNBDoneToken struct{}

func (t *fakeNBDoneToken) Wait() bool                       { return true }
func (t *fakeNBDoneToken) WaitTimeout(d time.Duration) bool { return true }
func (t *fakeNBDoneToken) Done() <-chan struct{}            { c := make(chan struct{}); close(c); return c }
func (t *fakeNBDoneToken) Error() error                     { return nil }

// TestRewalkGate_AllowRateLimits is the pure rate-limit decision test: a
// second call within nbRewalkRateLimit of the first must be refused; one
// after the window has elapsed must be allowed.
func TestRewalkGate_AllowRateLimits(t *testing.T) {
	g := &rewalkGate{}
	t0 := time.Now()

	if !g.allow(t0) {
		t.Fatal("first allow() call refused, want true")
	}
	if g.allow(t0.Add(1 * time.Second)) {
		t.Error("allow() 1s later returned true, want false (inside nbRewalkRateLimit)")
	}
	if g.allow(t0.Add(nbRewalkRateLimit - time.Millisecond)) {
		t.Error("allow() just under the limit returned true, want false")
	}
	if !g.allow(t0.Add(nbRewalkRateLimit + time.Millisecond)) {
		t.Error("allow() just past the limit returned false, want true")
	}
}

// TestLastPublishedStore_SetGetRoundTrip verifies the cache stores an
// independent copy (mutating the caller's struct after Set must not affect
// what Get later returns) and that Get reports nil before anything is set.
func TestLastPublishedStore_SetGetRoundTrip(t *testing.T) {
	s := &lastPublishedStore{}
	if got := s.get(); got != nil {
		t.Fatalf("get() before any set() = %+v, want nil", got)
	}

	ctrl := bus.ActiveControl{Source: "event", MRID: "evt-1", Ts: 100}
	s.set(ctrl)
	ctrl.MRID = "mutated-after-set" // must not affect the stored copy

	got := s.get()
	if got == nil {
		t.Fatal("get() after set() = nil, want the stored control")
	}
	if got.MRID != "evt-1" {
		t.Errorf("get().MRID = %q, want %q (set() must copy, not alias)", got.MRID, "evt-1")
	}
}

// TestHandleRewalkRequest_RepublishesCachedControlWithFreshTsAndPokesWalk is
// TASK-042's northbound acceptance case: a rewalk request with a cached
// control present must republish it retained, with Ts refreshed to now (not
// the cached Ts), and poke rewalkChan exactly once.
func TestHandleRewalkRequest_RepublishesCachedControlWithFreshTsAndPokesWalk(t *testing.T) {
	fc := &fakeNBClient{}
	lp := &lastPublishedStore{}
	lp.set(bus.ActiveControl{Source: "event", MRID: "evt-1", Ts: 100})
	gate := &rewalkGate{}
	rewalkChan := make(chan struct{}, 1)
	now := time.Now()

	handleRewalkRequest(fc, lp, gate, rewalkChan, bus.RewalkRequest{Reason: "stale", Ts: now.Unix()}, now)

	if len(fc.publishes) != 1 {
		t.Fatalf("Publish called %d times, want 1", len(fc.publishes))
	}
	if fc.publishes[0].topic != bus.TopicCSIPControl {
		t.Errorf("published topic = %q, want %q", fc.publishes[0].topic, bus.TopicCSIPControl)
	}
	if !fc.publishes[0].retained {
		t.Error("republish was not retained; lexa/csip/control must stay retained")
	}

	select {
	case <-rewalkChan:
	default:
		t.Fatal("rewalkChan was not poked")
	}

	// The cache itself should now carry the refreshed Ts too (so a second
	// rewalk request without an intervening walk still republishes something
	// fresh, not the original stale Ts again).
	got := lp.get()
	if got == nil {
		t.Fatal("lastPublishedStore is nil after a successful republish")
	}
	if got.Ts != now.Unix() {
		t.Errorf("cached Ts after republish = %d, want %d (refreshed)", got.Ts, now.Unix())
	}
	if got.Ts == 100 {
		t.Error("cached Ts still the original stale value; republish must stamp Ts=now")
	}
}

// TestHandleRewalkRequest_NoCacheStillPokesWalk covers the no-cache case
// (startup, or every walk so far has failed): nothing to republish, but the
// walk trigger must still fire — ground truth still needs refreshing.
func TestHandleRewalkRequest_NoCacheStillPokesWalk(t *testing.T) {
	fc := &fakeNBClient{}
	lp := &lastPublishedStore{} // never set
	gate := &rewalkGate{}
	rewalkChan := make(chan struct{}, 1)

	handleRewalkRequest(fc, lp, gate, rewalkChan, bus.RewalkRequest{Reason: "decode"}, time.Now())

	if len(fc.publishes) != 0 {
		t.Errorf("Publish called %d times with no cached control, want 0", len(fc.publishes))
	}
	select {
	case <-rewalkChan:
	default:
		t.Fatal("rewalkChan was not poked even with no cached control")
	}
}

// TestHandleRewalkRequest_RateLimited verifies a second request arriving
// inside nbRewalkRateLimit is dropped entirely: no republish, no walk poke.
func TestHandleRewalkRequest_RateLimited(t *testing.T) {
	fc := &fakeNBClient{}
	lp := &lastPublishedStore{}
	lp.set(bus.ActiveControl{Source: "event", MRID: "evt-1", Ts: 100})
	gate := &rewalkGate{}
	rewalkChan := make(chan struct{}, 1)
	t0 := time.Now()

	handleRewalkRequest(fc, lp, gate, rewalkChan, bus.RewalkRequest{Reason: "stale"}, t0)
	// Drain the first poke so the second call's behavior is unambiguous.
	<-rewalkChan
	firstPublishes := len(fc.publishes)

	handleRewalkRequest(fc, lp, gate, rewalkChan, bus.RewalkRequest{Reason: "stale"}, t0.Add(1*time.Second))

	if len(fc.publishes) != firstPublishes {
		t.Errorf("Publish called again inside the rate limit: %d -> %d", firstPublishes, len(fc.publishes))
	}
	select {
	case <-rewalkChan:
		t.Fatal("rewalkChan poked again inside the rate limit")
	default:
	}
}

// TestHandleRewalkRequest_RepeatedPokesCoalesce verifies the buffered size-1
// rewalkChan means a second, ALLOWED (i.e. outside the rate limit) request
// arriving before the walk loop drains the channel does not block
// handleRewalkRequest and does not queue a second pending walk.
func TestHandleRewalkRequest_RepeatedPokesCoalesce(t *testing.T) {
	fc := &fakeNBClient{}
	lp := &lastPublishedStore{}
	gate := &rewalkGate{}
	rewalkChan := make(chan struct{}, 1)
	t0 := time.Now()

	handleRewalkRequest(fc, lp, gate, rewalkChan, bus.RewalkRequest{Reason: "stale"}, t0)
	// Second call well past the rate limit, but BEFORE the channel is
	// drained — must not block (buffered chan, non-blocking send with a
	// default branch) even though one poke is already sitting there.
	done := make(chan struct{})
	go func() {
		handleRewalkRequest(fc, lp, gate, rewalkChan, bus.RewalkRequest{Reason: "stale"}, t0.Add(nbRewalkRateLimit+time.Second))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleRewalkRequest blocked on an already-full rewalkChan")
	}

	// Exactly one pending poke should be observable.
	select {
	case <-rewalkChan:
	default:
		t.Fatal("expected one pending poke on rewalkChan")
	}
	select {
	case <-rewalkChan:
		t.Fatal("a second poke queued; expected coalescing into one")
	default:
	}
}
