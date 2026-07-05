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

// connect is the shared implementation behind ConnectAuth and
// ConnectAuthInstrumented — identical behavior either way, the only
// difference being whether inst's callbacks are non-nil.
func connect(broker, clientID, user, pass string, inst Instrumentation) (mqtt.Client, error) {
	reg := &subRegistry{}
	state := &instState{inst: inst}

	opts := mqtt.NewClientOptions().
		AddBroker(broker).
		SetClientID(clientID).
		SetAutoReconnect(true).
		SetMaxReconnectInterval(30 * time.Second).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
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
	return publishJSON(client, topic, 1, false, v)
}

// PublishJSONRetained is like PublishJSON but the message is retained.
func PublishJSONRetained(client mqtt.Client, topic string, v any) error {
	return publishJSON(client, topic, 1, true, v)
}

// PublishJSONQoS marshals v to JSON and publishes it to topic at the given
// QoS (0 or 1), not retained. Callers should pass bus.PubQoS(topic) for qos
// rather than a hardcoded literal, so the per-topic QoS policy stays owned
// in one place (internal/bus). QoS 1 behaves exactly like PublishJSON
// (bounded publishTimeout wait for the PUBACK). QoS 0 does not wait for a
// PUBACK — there isn't one — but still bounds the call at qos0AckTimeout so
// a marshal or send error is not silently swallowed.
func PublishJSONQoS(client mqtt.Client, topic string, qos byte, v any) error {
	return publishJSON(client, topic, qos, false, v)
}

func publishJSON(client mqtt.Client, topic string, qos byte, retained bool, v any) error {
	err := publishJSONInner(client, topic, qos, retained, v)
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

func publishJSONInner(client mqtt.Client, topic string, qos byte, retained bool, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("mqttutil: marshal %T: %w", v, err)
	}
	tok := client.Publish(topic, qos, retained, payload)
	wait := publishTimeout
	if qos == 0 {
		wait = qos0AckTimeout
	}
	if !tok.WaitTimeout(wait) {
		return fmt.Errorf("mqttutil: publish %s: no ack after %s", topic, wait)
	}
	return tok.Error()
}

// Subscribe registers handler for messages on topic (supports MQTT wildcards).
// handler receives the raw topic string and the JSON-decoded value of type T.
// The subscription is recorded so that it survives broker reconnects; handler
// may therefore be invoked again with the retained message after a reconnect.
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
func Subscribe[T any](client mqtt.Client, topic string, handler func(topic string, msg T)) error {
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
			return
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
