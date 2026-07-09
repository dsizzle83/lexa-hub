package mqttutil

import (
	"os"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
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
	// subscribed captures the topic/handler pair from the most recent
	// Subscribe call so a test can drive it directly with a fakeMessage,
	// without a real broker round trip.
	subscribed map[string]mqtt.MessageHandler
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
	if f.subscribed == nil {
		f.subscribed = make(map[string]mqtt.MessageHandler)
	}
	f.subscribed[topic] = callback
	return newDoneToken()
}

// doneToken is a mqtt.Token that has already completed successfully — the
// SUBSCRIBE-ack equivalent of the real broker's immediate response. Used by
// fakeClient.Subscribe so Subscribe[T]'s WaitTimeout(10*time.Second) call
// returns instantly in tests instead of actually waiting.
type doneToken struct{}

func newDoneToken() *doneToken                        { return &doneToken{} }
func (t *doneToken) Wait() bool                       { return true }
func (t *doneToken) WaitTimeout(d time.Duration) bool { return true }
func (t *doneToken) Done() <-chan struct{}            { c := make(chan struct{}); close(c); return c }
func (t *doneToken) Error() error                     { return nil }

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

// fakeMessage is a minimal mqtt.Message double carrying just what
// CheckVersion and json.Unmarshal need: topic and payload.
type fakeMessage struct {
	topic   string
	payload []byte
}

func (m *fakeMessage) Duplicate() bool   { return false }
func (m *fakeMessage) Qos() byte         { return 1 }
func (m *fakeMessage) Retained() bool    { return false }
func (m *fakeMessage) Topic() string     { return m.topic }
func (m *fakeMessage) MessageID() uint16 { return 0 }
func (m *fakeMessage) Payload() []byte   { return m.payload }
func (m *fakeMessage) Ack()              {}

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

// deliver invokes the handler Subscribe registered for topic on fc, as if
// the broker had just delivered payload on that topic — without any real
// broker round trip. It fails the test if Subscribe was never called for
// topic, which would otherwise make a bad test pass by never running the
// handler at all.
func deliver(t *testing.T, fc *fakeClient, topic string, payload []byte) {
	t.Helper()
	h, ok := fc.subscribed[topic]
	if !ok {
		t.Fatalf("no handler registered for topic %q (Subscribe was not called with it)", topic)
	}
	h(fc, &fakeMessage{topic: topic, payload: payload})
}

// TestSubscribeVersionGate exercises mqttutil.Subscribe's decode-time
// version gate (TASK-018): CheckVersion runs before json.Unmarshal, so an
// unknown-major payload never reaches handler at all, while an absent-v
// (legacy) or in-range payload does.
func TestSubscribeVersionGate(t *testing.T) {
	const topic = "lexa/csip/control" // ActiveControlV == 1

	t.Run("absent v (legacy) reaches handler", func(t *testing.T) {
		fc := &fakeClient{}
		var got string
		if err := Subscribe(fc, topic, func(_ string, msg struct {
			Source string `json:"source"`
		}) {
			got = msg.Source
		}); err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		deliver(t, fc, topic, []byte(`{"source":"event"}`))
		if got != "event" {
			t.Errorf("handler.Source = %q, want %q (legacy v0 payload must reach the handler)", got, "event")
		}
	})

	t.Run("v within supported reaches handler", func(t *testing.T) {
		fc := &fakeClient{}
		var got string
		if err := Subscribe(fc, topic, func(_ string, msg struct {
			Source string `json:"source"`
		}) {
			got = msg.Source
		}); err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		deliver(t, fc, topic, []byte(`{"v":1,"source":"default"}`))
		if got != "default" {
			t.Errorf("handler.Source = %q, want %q (v1 payload must reach the handler)", got, "default")
		}
	})

	t.Run("v exceeding supported is dropped and counted, not delivered", func(t *testing.T) {
		fc := &fakeClient{}
		called := false
		if err := Subscribe(fc, topic, func(_ string, msg struct {
			Source string `json:"source"`
		}) {
			called = true
		}); err != nil {
			t.Fatalf("Subscribe: %v", err)
		}

		before := bus.VersionRejects()[topic]
		deliver(t, fc, topic, []byte(`{"v":99,"source":"should-not-arrive"}`))
		after := bus.VersionRejects()[topic]

		if called {
			t.Error("handler was called for a v=99 payload; the version gate must drop it before decode")
		}
		if after != before+1 {
			t.Errorf("VersionRejects()[%q] = %d, want %d (exactly one new rejection recorded)", topic, after, before+1)
		}
	})

	t.Run("malformed JSON still falls through to the existing log-and-drop, not the version gate", func(t *testing.T) {
		fc := &fakeClient{}
		called := false
		if err := Subscribe(fc, topic, func(_ string, msg struct {
			Source string `json:"source"`
		}) {
			called = true
		}); err != nil {
			t.Fatalf("Subscribe: %v", err)
		}

		before := bus.VersionRejects()[topic]
		deliver(t, fc, topic, []byte(`not json at all`))
		after := bus.VersionRejects()[topic]

		if called {
			t.Error("handler was called for malformed JSON")
		}
		if after != before {
			t.Errorf("VersionRejects()[%q] changed (%d -> %d) for malformed JSON; "+
				"CheckVersion must defer to the real unmarshal's error, not count it as a version rejection",
				topic, before, after)
		}
	})
}

// TestApplyAuth_SetsCredentialsOnlyWhenUserNonEmpty is TASK-013's option-
// plumbing test (W7/AD-008): the broker credential fields must be additive —
// an empty user (today's every-config default) must leave the options
// exactly as anonymous Connect built them, and a non-empty user must set
// both Username and Password so ConnectAuth's caller-supplied pass reaches
// the CONNECT packet paho builds from these options.
func TestApplyAuth_SetsCredentialsOnlyWhenUserNonEmpty(t *testing.T) {
	anon := mqtt.NewClientOptions()
	applyAuth(anon, "", "")
	if anon.Username != "" || anon.Password != "" {
		t.Fatalf("anonymous case: got Username=%q Password=%q, want both empty", anon.Username, anon.Password)
	}

	authed := mqtt.NewClientOptions()
	applyAuth(authed, "lexa-modbus", "s3cr3t")
	if authed.Username != "lexa-modbus" {
		t.Fatalf("Username = %q, want lexa-modbus", authed.Username)
	}
	if authed.Password != "s3cr3t" {
		t.Fatalf("Password = %q, want s3cr3t", authed.Password)
	}
}

// TestBuildClientOptions_OrderedStoreAndWriteTimeout is WS-9.2's option-
// plumbing test: every client this package builds must use an
// *mqtt.OrderedMemoryStore (not paho's default, unordered MemoryStore) and a
// non-zero WriteTimeout, without dialing a real broker (buildClientOptions
// is the piece of connect() that runs before client.Connect()'s network
// round trip).
//
// What this does NOT cover: proving OrderedMemoryStore.All() actually
// replays queued messages in original-publish order end-to-end. That would
// need a real or fake broker connection driving an actual resend after a
// reconnect — this package has no fake-broker test harness today (its
// existing tests, e.g. TestPublishJSONQoS0DoesNotBlockOnUnackedPublish and
// mqttutil_async_test.go's Harvest tests, all drive fakeClient/token doubles
// rather than a real handshake+resend). Accepted gap; OrderedMemoryStore's
// own replay-ordering correctness is paho's to prove, not this package's —
// this test only pins that connect() actually asks for it.
func TestBuildClientOptions_OrderedStoreAndWriteTimeout(t *testing.T) {
	reg := &subRegistry{}
	state := &instState{}
	opts := buildClientOptions("tcp://localhost:1883", "lexa-test", "", "", reg, state)

	if _, ok := opts.Store.(*mqtt.OrderedMemoryStore); !ok {
		t.Fatalf("opts.Store = %T, want *mqtt.OrderedMemoryStore", opts.Store)
	}
	if opts.WriteTimeout != publishTimeout {
		t.Fatalf("opts.WriteTimeout = %s, want publishTimeout (%s)", opts.WriteTimeout, publishTimeout)
	}
	if opts.WriteTimeout <= 0 {
		t.Fatalf("opts.WriteTimeout = %s, want a bounded (>0) duration — 0 means paho never times out a write", opts.WriteTimeout)
	}
}

// TestPublishFailInstrumentationHook is TASK-044's mqttutil instrumentation
// test: a publish that never gets acked (the same neverToken/fakeClient
// forced-failure setup as TestPublishJSONQoS0DoesNotBlockOnUnackedPublish)
// must invoke Instrumentation.OnPublishFail exactly once. The client is
// wired to instrumentation directly via instrumentations.Store — the same
// state connect()/ConnectAuthInstrumented would populate — rather than via a
// real Connect call, since fakeClient doesn't implement a real handshake.
func TestPublishFailInstrumentationHook(t *testing.T) {
	fc := &fakeClient{}
	var fails int
	instrumentations.Store(fc, &instState{inst: Instrumentation{
		OnPublishFail: func() { fails++ },
	}})
	defer instrumentations.Delete(fc)

	if err := PublishJSONQoS(fc, "lexa/measurements/inv0", 0, map[string]int{"w": 1}); err == nil {
		t.Fatal("expected a publish error (no ack observed), got nil")
	}
	if fails != 1 {
		t.Fatalf("OnPublishFail called %d times, want 1", fails)
	}
}

// TestPublishSuccessDoesNotInvokeOnPublishFail is the control: a QoS 0
// publish that DOES ack (a client whose Publish returns an already-done
// token) must not call OnPublishFail at all.
func TestPublishSuccessDoesNotInvokeOnPublishFail(t *testing.T) {
	fc := &fakeAckingClient{}
	var fails int
	instrumentations.Store(fc, &instState{inst: Instrumentation{
		OnPublishFail: func() { fails++ },
	}})
	defer instrumentations.Delete(fc)

	if err := PublishJSONQoS(fc, "lexa/measurements/inv0", 0, map[string]int{"w": 1}); err != nil {
		t.Fatalf("unexpected publish error: %v", err)
	}
	if fails != 0 {
		t.Fatalf("OnPublishFail called %d times on a successful publish, want 0", fails)
	}
}

// TestPublishFailNilSafeWhenUninstrumented is the nil-safety requirement:
// publishJSON against a client that was never registered in instrumentations
// at all (every existing test's fakeClient, and any caller still using plain
// Connect/ConnectAuth without an instState — impossible today since connect()
// always stores one, but defensive nonetheless) must not panic.
func TestPublishFailNilSafeWhenUninstrumented(t *testing.T) {
	fc := &fakeClient{}
	if err := PublishJSONQoS(fc, "lexa/measurements/inv0", 0, map[string]int{"w": 1}); err == nil {
		t.Fatal("expected a publish error (no ack observed), got nil")
	}
	// No assertion beyond "did not panic" — reaching this line is the test.
}

// TestNoteConnectedDistinguishesReconnect covers the initial-connect-vs-
// reconnect logic connect()'s OnConnectHandler relies on to decide whether
// to fire OnReconnect: the first call must report false (not a reconnect),
// every call after must report true.
func TestNoteConnectedDistinguishesReconnect(t *testing.T) {
	s := &instState{}
	if s.noteConnected() {
		t.Fatal("first noteConnected() call reported reconnect=true, want false (initial connect)")
	}
	if !s.noteConnected() {
		t.Fatal("second noteConnected() call reported reconnect=false, want true")
	}
	if !s.noteConnected() {
		t.Fatal("third noteConnected() call reported reconnect=false, want true")
	}
}

// fakeAckingClient is a minimal mqtt.Client double whose Publish returns an
// already-acked token, standing in for a healthy broker (contrast fakeClient
// above, whose Publish never acks).
type fakeAckingClient struct{ fakeClient }

func (f *fakeAckingClient) Publish(topic string, qos byte, retained bool, payload interface{}) mqtt.Token {
	f.fakeClient.Publish(topic, qos, retained, payload)
	return newDoneToken()
}

// TestLoadPassword covers the three states config.go callers rely on: unset
// (anonymous default), a valid pass-file (trims the trailing newline
// openssl rand -hex writes), and configured-but-empty (fail loud rather than
// send an empty password the broker will reject or, worse, silently connect
// anonymously).
func TestLoadPassword(t *testing.T) {
	pass, err := LoadPassword("")
	if err != nil || pass != "" {
		t.Fatalf("empty path: got (%q, %v), want (\"\", nil)", pass, err)
	}

	dir := t.TempDir()
	valid := dir + "/lexa-modbus.pass"
	if err := os.WriteFile(valid, []byte("s3cr3t-hex\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pass, err = LoadPassword(valid)
	if err != nil {
		t.Fatalf("valid pass-file: unexpected error %v", err)
	}
	if pass != "s3cr3t-hex" {
		t.Fatalf("valid pass-file: got %q, want trimmed \"s3cr3t-hex\"", pass)
	}

	empty := dir + "/empty.pass"
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPassword(empty); err == nil {
		t.Fatal("configured-but-empty pass-file: want an error, got nil")
	}

	if _, err := LoadPassword(dir + "/does-not-exist.pass"); err == nil {
		t.Fatal("missing pass-file: want an error, got nil")
	}
}

// TestSubscribeDecodeErr_OnErrFiresOnMalformedJSON is TASK-042's mqttutil
// hook test: a payload that fails json.Unmarshal must invoke onErr with the
// topic and the raw payload, in addition to the existing log.Printf/
// bus.RecordDecodeFailure behavior (unchanged, asserted by
// TestSubscribeVersionGate's malformed-JSON subtest already). This is the
// GAP-02 mechanism's building block: mqtt-malformed-control today silently
// drops forever; a caller wiring onErr on TopicCSIPControl (cmd/hub/main.go)
// can alarm and request an immediate re-publish instead.
func TestSubscribeDecodeErr_OnErrFiresOnMalformedJSON(t *testing.T) {
	const topic = "lexa/csip/control"
	fc := &fakeClient{}
	var handlerCalled bool
	var errTopic string
	var errPayload []byte
	var errCalls int

	if err := SubscribeDecodeErr(fc, topic, func(_ string, msg struct {
		Source string `json:"source"`
	}) {
		handlerCalled = true
	}, func(topic string, payload []byte, err error) {
		errCalls++
		errTopic = topic
		errPayload = payload
		if err == nil {
			t.Error("onErr called with nil err")
		}
	}); err != nil {
		t.Fatalf("SubscribeDecodeErr: %v", err)
	}

	bad := []byte("not json at all")
	deliver(t, fc, topic, bad)

	if handlerCalled {
		t.Error("handler was called for malformed JSON")
	}
	if errCalls != 1 {
		t.Fatalf("onErr called %d times, want 1", errCalls)
	}
	if errTopic != topic {
		t.Errorf("onErr topic = %q, want %q", errTopic, topic)
	}
	if string(errPayload) != string(bad) {
		t.Errorf("onErr payload = %q, want %q", errPayload, bad)
	}
}

// TestSubscribeDecodeErr_OnErrNotCalledOnVersionReject confirms the
// documented boundary: a version-gate rejection (CheckVersion/RejectAndAlarm)
// is a distinct, already-alarmed path and must NOT also invoke onErr —
// onErr is scoped to the two RecordDecodeFailure failure modes only.
func TestSubscribeDecodeErr_OnErrNotCalledOnVersionReject(t *testing.T) {
	const topic = "lexa/csip/control" // ActiveControlV == 1
	fc := &fakeClient{}
	errCalls := 0
	if err := SubscribeDecodeErr(fc, topic, func(_ string, msg struct {
		Source string `json:"source"`
	}) {
	}, func(topic string, payload []byte, err error) {
		errCalls++
	}); err != nil {
		t.Fatalf("SubscribeDecodeErr: %v", err)
	}
	deliver(t, fc, topic, []byte(`{"v":99,"source":"nope"}`))
	if errCalls != 0 {
		t.Errorf("onErr called %d times for a version-gate reject, want 0", errCalls)
	}
}

// TestSubscribeDecodeErr_NilOnErrIsIdenticalToSubscribe is the byte-identical
// requirement from the task's "common mistakes": SubscribeDecodeErr(...,
// nil) must behave exactly like Subscribe for a normal in-range message
// (handler reached) and for a malformed one (dropped, no panic from a nil
// onErr).
func TestSubscribeDecodeErr_NilOnErrIsIdenticalToSubscribe(t *testing.T) {
	const topic = "lexa/csip/control"
	fc := &fakeClient{}
	var got string
	if err := SubscribeDecodeErr(fc, topic, func(_ string, msg struct {
		Source string `json:"source"`
	}) {
		got = msg.Source
	}, nil); err != nil {
		t.Fatalf("SubscribeDecodeErr: %v", err)
	}
	deliver(t, fc, topic, []byte(`{"source":"event"}`))
	if got != "event" {
		t.Errorf("handler.Source = %q, want %q", got, "event")
	}

	// Malformed payload with nil onErr must not panic and must not reach handler.
	fc2 := &fakeClient{}
	called := false
	if err := SubscribeDecodeErr(fc2, topic, func(_ string, msg struct{}) {
		called = true
	}, nil); err != nil {
		t.Fatalf("SubscribeDecodeErr: %v", err)
	}
	deliver(t, fc2, topic, []byte("not json"))
	if called {
		t.Error("handler was called for malformed JSON with nil onErr")
	}
}
