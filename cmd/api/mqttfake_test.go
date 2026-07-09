package main

// mqttfake_test.go: a minimal mqtt.Client double shared by this package's
// TASK-088 write-path tests (intent, scan, resultWaiter) — same pattern as
// internal/mqttutil's fakeClient/fakeAckingClient and cmd/hub's
// fakeHubMQTTClient (desired_test.go). Only Publish/Subscribe are exercised
// by the code under test; every other method panics if called, so a test
// that accidentally depends on unimplemented behavior fails loudly.
import (
	"errors"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type fakeAPIPublish struct {
	topic    string
	qos      byte
	retained bool
	payload  []byte
}

type fakeAPIMQTTClient struct {
	publishes  []fakeAPIPublish
	subscribed map[string]mqtt.MessageHandler

	// failNextPublish, when true, makes the NEXT Publish call return an
	// already-failed token (simulates a broker publish error) and clears
	// itself.
	failNextPublish bool
	// neverAckNext, when true, makes the NEXT Publish call return a token
	// that never completes (simulates an unresponsive broker) and clears
	// itself.
	neverAckNext bool

	// publishedCh, if non-nil, additionally receives a copy of every
	// fakeAPIPublish — used by tests that need to react to a publish from a
	// SEPARATE goroutine (e.g. simulating the hub's async IntentResult
	// reply) without racing a concurrent read of the plain publishes slice
	// above (which is only ever safe to read from the same goroutine that
	// called the handler, after it returns).
	publishedCh chan fakeAPIPublish
}

func (f *fakeAPIMQTTClient) IsConnected() bool      { return true }
func (f *fakeAPIMQTTClient) IsConnectionOpen() bool { return true }
func (f *fakeAPIMQTTClient) Connect() mqtt.Token    { panic("not implemented") }
func (f *fakeAPIMQTTClient) Disconnect(uint)        {}

func (f *fakeAPIMQTTClient) Publish(topic string, qos byte, retained bool, payload interface{}) mqtt.Token {
	var b []byte
	switch p := payload.(type) {
	case []byte:
		b = p
	case string:
		b = []byte(p)
	}
	pub := fakeAPIPublish{topic: topic, qos: qos, retained: retained, payload: b}
	f.publishes = append(f.publishes, pub)
	if f.publishedCh != nil {
		f.publishedCh <- pub
	}
	if f.neverAckNext {
		f.neverAckNext = false
		return newAPINeverToken()
	}
	if f.failNextPublish {
		f.failNextPublish = false
		return &apiFailedToken{}
	}
	return apiDoneToken{}
}

func (f *fakeAPIMQTTClient) Subscribe(topic string, qos byte, callback mqtt.MessageHandler) mqtt.Token {
	if f.subscribed == nil {
		f.subscribed = make(map[string]mqtt.MessageHandler)
	}
	f.subscribed[topic] = callback
	return apiDoneToken{}
}

func (f *fakeAPIMQTTClient) SubscribeMultiple(map[string]byte, mqtt.MessageHandler) mqtt.Token {
	panic("not implemented")
}
func (f *fakeAPIMQTTClient) Unsubscribe(...string) mqtt.Token { panic("not implemented") }
func (f *fakeAPIMQTTClient) AddRoute(string, mqtt.MessageHandler) {
	panic("not implemented")
}
func (f *fakeAPIMQTTClient) OptionsReader() mqtt.ClientOptionsReader {
	panic("not implemented")
}

// apiDoneToken is an already-completed mqtt.Token, standing in for a broker
// that acked immediately.
type apiDoneToken struct{}

func (apiDoneToken) Wait() bool                     { return true }
func (apiDoneToken) WaitTimeout(time.Duration) bool { return true }
func (apiDoneToken) Done() <-chan struct{}          { c := make(chan struct{}); close(c); return c }
func (apiDoneToken) Error() error                   { return nil }

// apiFailedToken stands in for a publish that already errored by the time
// the caller checks (Done() closed immediately, Error() non-nil).
type apiFailedToken struct{}

var errFakePublishFailed = errors.New("fake publish failure")

func (apiFailedToken) Wait() bool                     { return true }
func (apiFailedToken) WaitTimeout(time.Duration) bool { return true }
func (apiFailedToken) Done() <-chan struct{}          { c := make(chan struct{}); close(c); return c }
func (apiFailedToken) Error() error                   { return errFakePublishFailed }

// apiNeverToken never completes — stands in for a wedged/absent broker.
type apiNeverToken struct{ done chan struct{} }

func newAPINeverToken() *apiNeverToken { return &apiNeverToken{done: make(chan struct{})} }
func (t *apiNeverToken) Wait() bool    { <-t.done; return true }
func (t *apiNeverToken) WaitTimeout(d time.Duration) bool {
	select {
	case <-t.done:
		return true
	case <-time.After(d):
		return false
	}
}
func (t *apiNeverToken) Done() <-chan struct{} { return t.done }
func (t *apiNeverToken) Error() error          { return nil }

// fakeAPIMessage is a minimal mqtt.Message double carrying just what
// bus.CheckVersion and json.Unmarshal need: topic and payload.
type fakeAPIMessage struct {
	topic   string
	payload []byte
}

func (m *fakeAPIMessage) Duplicate() bool   { return false }
func (m *fakeAPIMessage) Qos() byte         { return 1 }
func (m *fakeAPIMessage) Retained() bool    { return false }
func (m *fakeAPIMessage) Topic() string     { return m.topic }
func (m *fakeAPIMessage) MessageID() uint16 { return 0 }
func (m *fakeAPIMessage) Payload() []byte   { return m.payload }
func (m *fakeAPIMessage) Ack()              {}

// deliverAPI invokes the handler Subscribe registered for topic on fc, as if
// the broker had just delivered payload on that topic — without any real
// broker round trip. Fails the test if Subscribe was never called for topic.
func deliverAPI(t *testing.T, fc *fakeAPIMQTTClient, topic string, payload []byte) {
	t.Helper()
	h, ok := fc.subscribed[topic]
	if !ok {
		t.Fatalf("no handler registered for topic %q (Subscribe was not called with it)", topic)
	}
	h(fc, &fakeAPIMessage{topic: topic, payload: payload})
}
