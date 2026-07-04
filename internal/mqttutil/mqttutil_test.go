package mqttutil

import (
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// neverToken is a mqtt.Token that never completes: Wait/WaitTimeout always
// report "not done" (WaitTimeout returns false, as if the broker is wedged
// or absent) and Error is nil until the caller stops waiting. It stands in
// for a broker that never sends a PUBACK, so a QoS 1 publish against it
// times out at publishTimeout and a QoS 0 publish must NOT — QoS 0 has no
// PUBACK to wait for in the first place.
type neverToken struct{ done chan struct{} }

func newNeverToken() *neverToken { return &neverToken{done: make(chan struct{})} }

func (t *neverToken) Wait() bool { <-t.done; return true }
func (t *neverToken) WaitTimeout(d time.Duration) bool {
	select {
	case <-t.done:
		return true
	case <-time.After(d):
		return false
	}
}
func (t *neverToken) Done() <-chan struct{} { return t.done }
func (t *neverToken) Error() error          { return nil }

// fakeClient is a minimal mqtt.Client double. Only Publish is exercised by
// PublishJSONQoS; every other method is unused by the code under test and
// panics if called, so a test that accidentally depends on unimplemented
// behavior fails loudly instead of silently no-opping.
type fakeClient struct {
	publishes []fakePublish
}

type fakePublish struct {
	topic    string
	qos      byte
	retained bool
	payload  []byte
}

func (f *fakeClient) IsConnected() bool       { return true }
func (f *fakeClient) IsConnectionOpen() bool  { return true }
func (f *fakeClient) Connect() mqtt.Token     { panic("not implemented") }
func (f *fakeClient) Disconnect(quiesce uint) {}
func (f *fakeClient) Publish(topic string, qos byte, retained bool, payload interface{}) mqtt.Token {
	var b []byte
	switch p := payload.(type) {
	case []byte:
		b = p
	case string:
		b = []byte(p)
	}
	f.publishes = append(f.publishes, fakePublish{topic: topic, qos: qos, retained: retained, payload: b})
	return newNeverToken()
}
func (f *fakeClient) Subscribe(topic string, qos byte, callback mqtt.MessageHandler) mqtt.Token {
	panic("not implemented")
}
func (f *fakeClient) SubscribeMultiple(filters map[string]byte, callback mqtt.MessageHandler) mqtt.Token {
	panic("not implemented")
}
func (f *fakeClient) Unsubscribe(topics ...string) mqtt.Token { panic("not implemented") }
func (f *fakeClient) AddRoute(topic string, callback mqtt.MessageHandler) {
	panic("not implemented")
}
func (f *fakeClient) OptionsReader() mqtt.ClientOptionsReader {
	panic("not implemented")
}

// TestPublishJSONQoS0DoesNotBlockOnUnackedPublish verifies the core D5 fix:
// a QoS 0 publish against a client whose token never completes (standing in
// for a wedged or absent broker) returns almost immediately, bounded by
// qos0AckTimeout (100ms), never by publishTimeout (5s). This is the
// hot-path win the task exists for — the measurement plane no longer stacks
// a synchronous PUBACK wait per device per poll.
func TestPublishJSONQoS0DoesNotBlockOnUnackedPublish(t *testing.T) {
	fc := &fakeClient{}
	start := time.Now()
	err := PublishJSONQoS(fc, "lexa/measurements/inv0", 0, map[string]int{"w": 100})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error (no ack observed within qos0AckTimeout), got nil")
	}
	if elapsed >= publishTimeout {
		t.Fatalf("QoS 0 publish took %s — blocked for the QoS 1 publishTimeout (%s); "+
			"the whole point of QoS 0 is not stacking a PUBACK wait", elapsed, publishTimeout)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("QoS 0 publish took %s — expected roughly qos0AckTimeout (%s), not a multi-second stall",
			elapsed, qos0AckTimeout)
	}

	if len(fc.publishes) != 1 {
		t.Fatalf("expected exactly 1 Publish call, got %d", len(fc.publishes))
	}
	if got := fc.publishes[0].qos; got != 0 {
		t.Errorf("Publish called with qos=%d, want 0", got)
	}
}

// TestPublishJSONQoS1StillWaitsFullTimeout is the control: the existing
// QoS 1 helpers must keep waiting up to publishTimeout against the same
// never-acking client, so the QoS 0 change is additive and does not
// accidentally shorten QoS 1's bounded-wait guarantee (mqtt-broker-restart/
// -latency scenarios depend on it).
//
// publishTimeout is 5s in production; this test shrinks nothing (that would
// require making the timeout injectable), so it only asserts that the call
// takes at least qos0AckTimeout longer than the QoS 0 case would — i.e. it
// does NOT return early via the QoS 0 short-circuit. It is skipped in
// -short mode to avoid a 5s tax on every `go test ./...`.
func TestPublishJSONQoS1StillWaitsFullTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("waits the full 5s publishTimeout; skipped with -short")
	}
	fc := &fakeClient{}
	start := time.Now()
	err := PublishJSON(fc, "lexa/csip/control", map[string]int{"limit": 1})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if elapsed < publishTimeout {
		t.Fatalf("QoS 1 publish returned after %s — want it to wait the full publishTimeout (%s)",
			elapsed, publishTimeout)
	}
}
