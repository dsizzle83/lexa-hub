// Package mqttutil provides thin helpers for connecting to an MQTT broker,
// publishing JSON payloads, and subscribing with JSON unmarshalling.
package mqttutil

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
)

// subscription is one topic/handler pair registered via Subscribe.
type subscription struct {
	topic   string
	handler mqtt.MessageHandler
}

// subRegistry records the subscriptions made on one client so they can be
// re-established after an automatic reconnect.  With CleanSession=true the
// broker holds no session for the client and paho does not re-send SUBSCRIBE
// packets on reconnect — without this replay, a broker restart leaves the
// client connected but permanently deaf.
type subRegistry struct {
	mu   sync.Mutex
	subs []subscription
}

func (r *subRegistry) add(topic string, h mqtt.MessageHandler) {
	r.mu.Lock()
	r.subs = append(r.subs, subscription{topic: topic, handler: h})
	r.mu.Unlock()
}

// replay re-subscribes every registered topic.  Called from the OnConnect
// handler, which paho fires on the initial connect (registry empty — no-op)
// and on every reconnect.  Re-subscribing to retained topics redelivers the
// retained message; handlers must therefore be idempotent.
func (r *subRegistry) replay(c mqtt.Client) {
	r.mu.Lock()
	subs := make([]subscription, len(r.subs))
	copy(subs, r.subs)
	r.mu.Unlock()

	for _, s := range subs {
		tok := c.Subscribe(s.topic, 1, s.handler)
		if !tok.WaitTimeout(10 * time.Second) {
			log.Printf("[mqtt] resubscribe %s: timeout", s.topic)
			continue
		}
		if err := tok.Error(); err != nil {
			log.Printf("[mqtt] resubscribe %s: %v", s.topic, err)
		}
	}
	if len(subs) > 0 {
		log.Printf("[mqtt] re-established %d subscriptions", len(subs))
	}
}

// registries maps each client created by Connect to its subscription registry.
var registries sync.Map // mqtt.Client → *subRegistry

// Instrumentation holds optional metrics callbacks a caller can wire into a
// client created via ConnectAuthInstrumented (TASK-044). All fields are
// optional; a nil callback is simply never invoked. Function values, not a
// registry import: this package must not import internal/metrics (or
// anything else outside the stdlib and internal/bus) so the metrics package
// can stay a leaf and mqttutil stays decoupled from whichever metrics
// implementation a caller chooses to wire in.
type Instrumentation struct {
	// OnPublishFail is called after any PublishJSON/PublishJSONRetained/
	// PublishJSONQoS call on this client returns a non-nil error (marshal
	// failure, publish timeout, or the broker's own publish error).
	OnPublishFail func()
	// OnReconnect is called when paho's OnConnectHandler fires for a
	// connection that was already established once before — i.e. an actual
	// reconnect, not the initial Connect. (The initial connect is not a
	// "reconnect"; see connect() below for how the two are told apart.)
	OnReconnect func()
	// OnConnectionLost is called when paho's SetConnectionLostHandler fires
	// (the client noticed it dropped, before it starts retrying).
	OnConnectionLost func()
}

// instState pairs one client's Instrumentation with the connectedOnce flag
// connect() uses to tell an initial connect from a genuine reconnect.
type instState struct {
	inst Instrumentation

	mu            sync.Mutex
	connectedOnce bool
}

// noteConnected records that this client's OnConnectHandler fired and
// reports whether this firing is a reconnect (connectedOnce was already
// true) rather than the initial connect. Split out from connect() so the
// initial-vs-reconnect distinction is unit-testable without a real broker.
func (s *instState) noteConnected() (reconnect bool) {
	s.mu.Lock()
	reconnect = s.connectedOnce
	s.connectedOnce = true
	s.mu.Unlock()
	return reconnect
}

// instrumentations maps each client created via connect() to its instState.
// A client created via the original Connect/ConnectAuth (zero-value
// Instrumentation) is still stored here so publishJSON's lookup is uniform
// (Load always succeeds for any client this package created); its callbacks
// are simply all nil, so invoking them is a no-op check, not a missing-entry
// special case.
var instrumentations sync.Map // mqtt.Client → *instState

// publishTimeout bounds how long a QoS 1 publish waits for its PUBACK.
// Publishers like the hub's control loop call PublishJSON synchronously from
// their main goroutine; an unbounded Wait on a wedged broker would stall the
// loop entirely (including CSIP disconnect enforcement).  A timed-out publish
// is not cancelled — paho may still deliver it after reconnecting — but every
// command topic is re-issued on the next tick and all handlers are idempotent,
// so a late or dropped command is harmless.
const publishTimeout = 5 * time.Second

// PublishTimeout exports publishTimeout's value for callers harvesting a
// PendingPub from PublishJSON(Retained)Async (TASK-046): "how long before I
// give up on this publish" should mean the same thing whether the caller
// waits synchronously (PublishJSON) or fires-and-harvests (PublishJSONAsync)
// — a caller switching from one to the other keeps the same effective
// staleness bound unless it has a specific reason not to (see
// cmd/hub/desired.go's actuators, which do).
const PublishTimeout = publishTimeout

// qos0AckTimeout bounds how long a QoS 0 publish waits before returning.
// QoS 0 has no PUBACK: paho completes the token locally as soon as the
// message is written to the wire, so this is never a broker round trip —
// it exists solely to still surface a marshal error or a "not connected"
// send failure to the caller instead of silently dropping it, without
// paying anything close to publishTimeout's 5 s hot-path cost.
const qos0AckTimeout = 100 * time.Millisecond

// Connect creates an anonymous MQTT client connected to broker with the given
// clientID. Auto-reconnect is enabled; the call blocks until the initial
// connection succeeds or times out after 30 s. Subscriptions made through
// Subscribe are replayed automatically after every reconnect.
//
// Connect is a thin wrapper over ConnectAuth("", ""): it keeps every existing
// caller anonymous-by-default, which is the staged-rollout requirement for
// TASK-013's broker credentials — a service only authenticates once its
// config carries mqtt_user/mqtt_pass_file.
func Connect(broker, clientID string) (mqtt.Client, error) {
	return ConnectAuth(broker, clientID, "", "")
}

// ConnectAuthInstrumented is like ConnectAuth but additionally wires inst's
// callbacks (TASK-044): OnPublishFail from every publishJSON error path on
// the returned client, OnReconnect on every reconnect (not the initial
// connect — see connect()'s connectedOnce tracking), and OnConnectionLost
// from paho's connection-lost handler. Every existing call site keeps using
// Connect/ConnectAuth unchanged; this is purely additive.
func ConnectAuthInstrumented(broker, clientID, user, pass string, inst Instrumentation) (mqtt.Client, error) {
	return connect(broker, clientID, user, pass, inst)
}

// ConnectAuth is like Connect but authenticates with user/pass when user is
// non-empty (paho only sends the MQTT CONNECT username/password flags once a
// username is set — SetUsername("") followed by SetPassword("x") would still
// connect anonymously from the broker's point of view, so callers must pass
// user == "" to mean "no credentials", not just an empty password). An empty
// user connects anonymously, identical to Connect — the broker's
// allow_anonymous stays the on/off switch; a service's own credentials are
// additive and harmless while the bench broker still allows anonymous
// clients (W7, AD-008).
func ConnectAuth(broker, clientID, user, pass string) (mqtt.Client, error) {
	return connect(broker, clientID, user, pass, Instrumentation{})
}

// buildClientOptions constructs the *mqtt.ClientOptions shared by every
// client this package creates. Split out from connect() so the option
// plumbing — in particular the WS-9.2 Store/WriteTimeout settings — is
// unit-testable without dialing a real broker (connect() blocks on a network
// round trip via client.Connect()).
func buildClientOptions(broker, clientID, user, pass string, reg *subRegistry, state *instState) *mqtt.ClientOptions {
	opts := mqtt.NewClientOptions().
		AddBroker(broker).
		SetClientID(clientID).
		SetAutoReconnect(true).
		SetMaxReconnectInterval(30 * time.Second).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		// WS-9.2: paho's default Store (plain MemoryStore, used whenever
		// ClientOptions.Store is left nil — see NewClient/c.persist.Open())
		// iterates its internal Go map in undefined order, so a resend after
		// a reconnect (ResumeSubs / in-flight QoS>0 messages) can replay
		// queued messages out of the order they were originally published.
		// OrderedMemoryStore (vendor/.../memstore_ordered.go) records each
		// message's insertion time and sorts by it in All(), fixing that —
		// still purely in-memory (no on-disk persistence added or implied).
		SetStore(mqtt.NewOrderedMemoryStore()).
		// WS-9.2: bounds how long a single underlying net.Conn write may
		// block (default 0 = unbounded, options.go's SetWriteTimeout doc).
		// A wedged TCP write would otherwise stall paho's internal write
		// goroutine indefinitely — which would in turn stall the ordered
		// resend the Store change above exists to guarantee, and every
		// other publish on this client. Reuses publishTimeout (5s) so a
		// write-level stall and this package's own publish-ack wait share
		// one bound instead of introducing a second unrelated constant.
		SetWriteTimeout(publishTimeout).
		SetOnConnectHandler(func(c mqtt.Client) {
			if user != "" {
				// Deliberately logged so journal evidence can confirm which
				// broker user a service authenticated as (TASK-013 step 6/7:
				// "all six services + mqttproxy connected with per-user
				// credentials") without ever logging the password.
				log.Printf("[mqtt] connected to %s as %s (broker user=%s)", broker, clientID, user)
			} else {
				log.Printf("[mqtt] connected to %s as %s (anonymous)", broker, clientID)
			}
			reg.replay(c)

			// TASK-044: OnConnectHandler fires on the initial connect too
			// (reg.replay is a no-op then, since nothing has subscribed
			// yet — subscriptions are only recorded by Subscribe, called
			// after Connect/ConnectAuth returns). noteConnected is what
			// distinguishes that first call from every call after, which is
			// by definition a reconnect.
			if state.noteConnected() && state.inst.OnReconnect != nil {
				state.inst.OnReconnect()
			}
		}).
		SetConnectionLostHandler(func(c mqtt.Client, err error) {
			log.Printf("[mqtt] connection lost: %v", err)
			if state.inst.OnConnectionLost != nil {
				state.inst.OnConnectionLost()
			}
		})
	applyAuth(opts, user, pass)
	return opts
}

// connect is the shared implementation behind ConnectAuth and
// ConnectAuthInstrumented — identical behavior either way, the only
// difference being whether inst's callbacks are non-nil.
func connect(broker, clientID, user, pass string, inst Instrumentation) (mqtt.Client, error) {
	reg := &subRegistry{}
	state := &instState{inst: inst}
	opts := buildClientOptions(broker, clientID, user, pass, reg, state)

	client := mqtt.NewClient(opts)
	registries.Store(client, reg)
	instrumentations.Store(client, state)
	tok := client.Connect()
	if !tok.WaitTimeout(30 * time.Second) {
		return nil, fmt.Errorf("mqtt: connect timeout to %s", broker)
	}
	if err := tok.Error(); err != nil {
		return nil, fmt.Errorf("mqtt: connect %s: %w", broker, err)
	}
	return client, nil
}

// applyAuth sets opts.Username/Password when user is non-empty, leaving them
// unset (anonymous) otherwise. Split out from ConnectAuth so the option
// plumbing is unit-testable without dialing a real broker (Connect/
// ConnectAuth block on a network round trip).
func applyAuth(opts *mqtt.ClientOptions, user, pass string) {
	if user != "" {
		opts.SetUsername(user)
		opts.SetPassword(pass)
	}
}

// LoadPassword reads an MQTT broker password from passFile, trimming
// surrounding whitespace (the file is written by openssl rand -hex or
// similar, which appends a trailing newline). An empty passFile returns
// ("", nil) — the staged-rollout default (mqtt_pass_file unset, service
// connects anonymously via Connect/ConnectAuth("", "", "", "")). A
// configured-but-unreadable-or-empty file is a startup-time configuration
// error: fail loud rather than silently connect anonymously or send an
// empty password the broker will reject (mirrors cmd/api's LoadAPIToken).
func LoadPassword(passFile string) (string, error) {
	if passFile == "" {
		return "", nil
	}
	data, err := os.ReadFile(passFile)
	if err != nil {
		return "", fmt.Errorf("mqttutil: read mqtt_pass_file %s: %w", passFile, err)
	}
	pass := strings.TrimSpace(string(data))
	if pass == "" {
		return "", fmt.Errorf("mqttutil: mqtt_pass_file %s is configured but empty", passFile)
	}
	return pass, nil
}

// PublishJSON marshals v to JSON and publishes it to topic with QoS 1.
// Waits at most publishTimeout for the broker's acknowledgement.
func PublishJSON(client mqtt.Client, topic string, v any) error {
	return publishJSON(client, topic, 1, false, publishTimeout, v)
}

// PublishJSONRetained is like PublishJSON but the message is retained.
func PublishJSONRetained(client mqtt.Client, topic string, v any) error {
	return publishJSON(client, topic, 1, true, publishTimeout, v)
}

// PublishJSONQoS marshals v to JSON and publishes it to topic at the given
// QoS (0 or 1), not retained. Callers should pass bus.PubQoS(topic) for qos
// rather than a hardcoded literal, so the per-topic QoS policy stays owned
// in one place (internal/bus). QoS 1 behaves exactly like PublishJSON
// (bounded publishTimeout wait for the PUBACK). QoS 0 does not wait for a
// PUBACK — there isn't one — but still bounds the call at qos0AckTimeout so
// a marshal or send error is not silently swallowed.
func PublishJSONQoS(client mqtt.Client, topic string, qos byte, v any) error {
	wait := publishTimeout
	if qos == 0 {
		wait = qos0AckTimeout
	}
	return publishJSON(client, topic, qos, false, wait, v)
}

// PublishJSONTimeout is PublishJSON/PublishJSONRetained with a caller-chosen
// PUBACK wait bound instead of the package's publishTimeout default
// (TASK-046). It stays synchronous — unlike PublishJSONAsync, this call does
// not return until timeout elapses or the broker acks/errors — for the rare
// publish where waiting is still correct but the full 5s default is too
// long: the hub's compliance-alert publish (cmd/hub/main.go) uses a 1s bound
// because that publish is rare and edge-triggered, so ordering/latency
// against the CannotComply episode matter more than sparing the tick budget
// (unlike the per-tick actuator/plan-log publishes, which went fully async).
func PublishJSONTimeout(client mqtt.Client, topic string, retained bool, v any, timeout time.Duration) error {
	return publishJSON(client, topic, 1, retained, timeout, v)
}

func publishJSON(client mqtt.Client, topic string, qos byte, retained bool, wait time.Duration, v any) error {
	err := publishJSONInner(client, topic, qos, retained, wait, v)
	if err != nil {
		// TASK-044: nil-safe when the client wasn't created via this package
		// (Load simply misses) or was created via Connect/ConnectAuth with a
		// zero-value Instrumentation (OnPublishFail is nil, checked below).
		if v, ok := instrumentations.Load(client); ok {
			if cb := v.(*instState).inst.OnPublishFail; cb != nil {
				cb()
			}
		}
	}
	return err
}

func publishJSONInner(client mqtt.Client, topic string, qos byte, retained bool, wait time.Duration, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("mqttutil: marshal %T: %w", v, err)
	}
	tok := client.Publish(topic, qos, retained, payload)
	if !tok.WaitTimeout(wait) {
		return fmt.Errorf("mqttutil: publish %s: no ack after %s", topic, wait)
	}
	return tok.Error()
}

// PendingPub is an in-flight QoS 1 publish whose completion is checked later
// ("harvested") instead of waited for synchronously (TASK-046). It exists so
// a tick-path caller — e.g. cmd/hub's desired-doc actuators — can hand a
// message to paho and return immediately, deferring the PUBACK wait (and the
// decision of what a timed-out publish means for its own dedupe state) to
// the next time it has a natural reason to check, typically the next tick.
//
// A PendingPub is single-use: call Harvest once to learn its outcome. It is
// not safe for concurrent use from multiple goroutines (matches every actual
// caller today: one PendingPub lives in one actuator, read and written only
// from the engine's single control goroutine).
type PendingPub struct {
	tok    mqtt.Token
	client mqtt.Client
	topic  string
	sentAt time.Time
}

// PublishJSONAsync is PublishJSON's non-blocking counterpart: it marshals v
// and hands it to paho's Publish exactly like PublishJSON, but returns as
// soon as the message is queued rather than waiting up to publishTimeout for
// the PUBACK. Marshal errors are still returned synchronously (there is
// nothing to defer — the message was never queued); everything about
// delivery (ack, timeout, broker-reported error) is discovered later via the
// returned PendingPub's Harvest method.
//
// Use PublishJSON when the caller has nothing better to do than wait (rare)
// or needs the delivery result before proceeding. Use PublishJSONAsync in any
// loop that must not stall on a sick-but-alive broker — the tick path is the
// motivating case (§11): the retained desired-doc is re-issued on convergence
// mismatch / the next differing command, so a late or dropped async publish
// is exactly as harmless as a late or dropped synchronous one used to be.
func PublishJSONAsync(client mqtt.Client, topic string, v any) (*PendingPub, error) {
	return publishJSONAsync(client, topic, 1, false, v)
}

// PublishJSONRetainedAsync is PublishJSONAsync with the retained flag set —
// the async counterpart of PublishJSONRetained, used for every retained
// control-plane document (AD-013 desired-state docs, the hub plan log).
func PublishJSONRetainedAsync(client mqtt.Client, topic string, v any) (*PendingPub, error) {
	return publishJSONAsync(client, topic, 1, true, v)
}

func publishJSONAsync(client mqtt.Client, topic string, qos byte, retained bool, v any) (*PendingPub, error) {
	payload, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("mqttutil: marshal %T: %w", v, err)
	}
	tok := client.Publish(topic, qos, retained, payload)
	return &PendingPub{tok: tok, client: client, topic: topic, sentAt: time.Now()}, nil
}

// Harvest reports p's completion state without blocking (an already-completed
// token returns instantly; an in-flight one returns done=false immediately —
// Harvest never itself waits).
//
// It deliberately does NOT use tok.WaitTimeout(0) as its non-blocking check.
// paho v1.4.3's baseToken.WaitTimeout (vendor/.../token.go) is a select
// between the token's completion channel and a timer — with a zero timer
// that fires essentially immediately, an already-complete token can lose the
// select at random, so WaitTimeout(0) is not a reliable "is it done"
// primitive despite reading like one. A select with a default case on the
// same completion channel (Token.Done()) is: exactly one of the two cases can
// ever be ready without blocking, and there is no race between "already
// closed" and "about to close". mqttutil_async_test.go exercises this
// directly against the vendored token so a future paho bump that changes the
// semantics fails loud here rather than in a flaky bench run.
//
// Return shape:
//   - done=true, err=nil: the broker acknowledged the publish (or, for a
//     hypothetical QoS 0 caller, paho wrote it to the wire — no PendingPub
//     helper is QoS 0 today, but Harvest makes no QoS assumption).
//   - done=true, err!=nil: the publish completed with an error.
//   - done=false: still in flight. timedOut reports whether it has been in
//     flight longer than the caller's own timeout budget — the caller's cue
//     to treat it as failed for its own bookkeeping (dedupe reset, retry)
//     WITHOUT waiting any further. The publish itself is not cancelled: paho
//     may still deliver it later (after a reconnect, say), exactly as a
//     timed-out synchronous PublishJSON leaves it today — see publishTimeout's
//     doc comment.
//
// On a broker-reported error (done=true, err!=nil), Harvest also invokes the
// client's OnPublishFail instrumentation hook if one was wired via
// ConnectAuthInstrumented, so lexa_mqtt_publish_failures_total keeps counting
// every failed publish regardless of which of PublishJSON/PublishJSONAsync
// sent it. A bare timeout (done=false) does not — the outcome isn't known
// yet, so it isn't a "failure" the instrumentation hook should count; if it
// later resolves to an error, a subsequent Harvest call (there won't be one,
// by construction — see the "single-use" note above) would be needed to see
// that, which is why callers that give up on a timed-out pending should
// simply treat the timeout itself as the failure signal (as the actuators do).
func (p *PendingPub) Harvest(timeout time.Duration) (done bool, timedOut bool, err error) {
	select {
	case <-p.tok.Done():
		err = p.tok.Error()
		if err != nil {
			if v, ok := instrumentations.Load(p.client); ok {
				if cb := v.(*instState).inst.OnPublishFail; cb != nil {
					cb()
				}
			}
		}
		return true, false, err
	default:
		return false, time.Since(p.sentAt) > timeout, nil
	}
}

// Topic returns the topic p was published to — useful for logging a harvested
// failure/timeout without the caller needing to have kept the topic string
// itself around.
func (p *PendingPub) Topic() string { return p.topic }

// Subscribe registers handler for messages on topic (supports MQTT wildcards).
// It is SubscribeDecodeErr(client, topic, handler, nil) — see that function's
// doc for the full decode pipeline (version gate, unmarshal, Finite() check).
// A nil onErr means every existing Subscribe caller's behavior is exactly
// what it was before SubscribeDecodeErr existed (TASK-042): the decode-error
// hook is purely additive and opt-in.
func Subscribe[T any](client mqtt.Client, topic string, handler func(topic string, msg T)) error {
	return SubscribeDecodeErr(client, topic, handler, nil)
}

// SubscribeDecodeErr is like Subscribe but additionally invokes onErr — with
// the topic, the raw payload, and the error — whenever the decode path drops
// a message before handler ever sees it: a raw json.Unmarshal failure, or a
// message that unmarshalled successfully but failed its own Finite() check
// (GAP-09). onErr is nil-safe (nil simply means "no hook", exactly
// Subscribe's behavior); it is called synchronously, on the same paho
// callback goroutine as handler, immediately after the log.Printf/
// bus.RecordDecodeFailure pair that already runs for that failure — it never
// replaces or reorders that existing forensic trail, only adds a caller-
// supplied action alongside it.
//
// This does NOT fire for a bus.CheckVersion/RejectAndAlarm reject (AD-006's
// version gate is a distinct, already-alarmed rejection path with its own
// counter) — only for the two failure modes bus.RecordDecodeFailure covers:
// malformed JSON and a non-finite decoded value. TASK-042's motivating case
// is a corrupted RETAINED control-plane payload (mqtt-malformed-control):
// today that is a silent-forever drop until the next successful northbound
// walk republishes; a hub wiring onErr on TopicCSIPControl can instead alarm
// and request an immediate re-publish (bus.TopicCSIPRewalk) rather than
// waiting out however long that takes.
//
// The subscription is recorded so that it survives broker reconnects; the
// wrapped handler closure captures onErr, so replay (a reconnect re-issuing
// SUBSCRIBE) preserves the decode-error hook exactly like every other
// behavior of the original registered subscription — see subRegistry.replay.
//
// Before decoding, every message passes bus.CheckVersion (AD-006, TASK-018):
// a message whose envelope version exceeds what bus.SupportedV(topic)
// reports is dropped via bus.RejectAndAlarm without ever reaching handler —
// same treatment as a malformed-JSON drop, just gated one step earlier. A
// message with no "v" field (legacy v0, indistinguishable from an explicit
// "v":0) is accepted while bus.LegacyV0Accepted is true, which it is
// throughout this transition — rejecting absent-v here would refuse a
// retained pre-envelope message at boot (the exact §8.3 hazard this rollout
// exists to prevent). CheckVersion never flags malformed JSON itself (that
// stays the real json.Unmarshal's job, immediately below, unchanged).
//
// After a successful Unmarshal (GAP-09, TASK-055), if T implements
// interface{ Finite() error } — every message type in internal/bus that
// carries a *float64 does — its Finite() is called and a non-finite result
// (a NaN/±Inf that slipped past json.Unmarshal via some future lax decode
// path; stdlib itself already rejects bare/quoted NaN/Infinity into a typed
// numeric field, see internal/bus/nan_reject_test.go) is treated exactly
// like a malformed payload: logged, counted, dropped before handler ever
// sees it. A NaN ActiveControl limit is the safety-critical instance of
// this — it must never reach the optimizer, only ever cause the message to
// be dropped so the last-known-good control holds. T types with no Finite()
// method take neither branch — this type assertion adds nothing for them,
// so Subscribe's behavior for a caller that hasn't opted in (by giving its
// T a Finite() method) is unchanged.
//
// Both a plain unmarshal failure and a Finite() failure now also call
// bus.RecordDecodeFailure, which — unlike the log.Printf alone, previously
// the only trace of either — increments a per-topic counter alongside
// bus.RejectAndAlarm's version-reject counter (GAP-09: today's silent drop
// on a non-control topic hid a rogue or version-skewed publisher entirely).
func SubscribeDecodeErr[T any](client mqtt.Client, topic string, handler func(topic string, msg T), onErr func(topic string, payload []byte, err error)) error {
	h := func(_ mqtt.Client, m mqtt.Message) {
		if verr := bus.CheckVersion(m.Topic(), m.Payload(), bus.SupportedV(m.Topic())); verr != nil {
			if ve, ok := verr.(*bus.VersionError); ok {
				bus.RejectAndAlarm(ve)
			}
			return
		}
		var v T
		if err := json.Unmarshal(m.Payload(), &v); err != nil {
			log.Printf("[mqtt] unmarshal on %s: %v", m.Topic(), err)
			bus.RecordDecodeFailure(m.Topic(), err)
			if onErr != nil {
				onErr(m.Topic(), m.Payload(), err)
			}
			return
		}
		if fv, ok := any(v).(interface{ Finite() error }); ok {
			if err := fv.Finite(); err != nil {
				log.Printf("[mqtt] non-finite value on %s: %v", m.Topic(), err)
				bus.RecordDecodeFailure(m.Topic(), err)
				if onErr != nil {
					onErr(m.Topic(), m.Payload(), err)
				}
				return
			}
		}
		handler(m.Topic(), v)
	}
	// Record before subscribing so a reconnect racing this call still replays
	// the subscription (a duplicate SUBSCRIBE for the same topic is idempotent).
	if reg, ok := registries.Load(client); ok {
		reg.(*subRegistry).add(topic, h)
	}
	tok := client.Subscribe(topic, 1, h)
	if !tok.WaitTimeout(10 * time.Second) {
		return fmt.Errorf("mqttutil: subscribe %s: no ack after 10s", topic)
	}
	return tok.Error()
}
