// Package mqttutil provides thin helpers for connecting to an MQTT broker,
// publishing JSON payloads, and subscribing with JSON unmarshalling.
package mqttutil

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
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

// publishTimeout bounds how long a QoS 1 publish waits for its PUBACK.
// Publishers like the hub's control loop call PublishJSON synchronously from
// their main goroutine; an unbounded Wait on a wedged broker would stall the
// loop entirely (including CSIP disconnect enforcement).  A timed-out publish
// is not cancelled — paho may still deliver it after reconnecting — but every
// command topic is re-issued on the next tick and all handlers are idempotent,
// so a late or dropped command is harmless.
const publishTimeout = 5 * time.Second

// Connect creates an MQTT client connected to broker with the given clientID.
// Auto-reconnect is enabled; the call blocks until the initial connection succeeds
// or times out after 30 s.  Subscriptions made through Subscribe are replayed
// automatically after every reconnect.
func Connect(broker, clientID string) (mqtt.Client, error) {
	reg := &subRegistry{}

	opts := mqtt.NewClientOptions().
		AddBroker(broker).
		SetClientID(clientID).
		SetAutoReconnect(true).
		SetMaxReconnectInterval(30 * time.Second).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetOnConnectHandler(func(c mqtt.Client) {
			log.Printf("[mqtt] connected to %s as %s", broker, clientID)
			reg.replay(c)
		}).
		SetConnectionLostHandler(func(c mqtt.Client, err error) {
			log.Printf("[mqtt] connection lost: %v", err)
		})

	client := mqtt.NewClient(opts)
	registries.Store(client, reg)
	tok := client.Connect()
	if !tok.WaitTimeout(30 * time.Second) {
		return nil, fmt.Errorf("mqtt: connect timeout to %s", broker)
	}
	if err := tok.Error(); err != nil {
		return nil, fmt.Errorf("mqtt: connect %s: %w", broker, err)
	}
	return client, nil
}

// PublishJSON marshals v to JSON and publishes it to topic with QoS 1.
// Waits at most publishTimeout for the broker's acknowledgement.
func PublishJSON(client mqtt.Client, topic string, v any) error {
	return publishJSON(client, topic, v, false)
}

// PublishJSONRetained is like PublishJSON but the message is retained.
func PublishJSONRetained(client mqtt.Client, topic string, v any) error {
	return publishJSON(client, topic, v, true)
}

func publishJSON(client mqtt.Client, topic string, v any, retained bool) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("mqttutil: marshal %T: %w", v, err)
	}
	tok := client.Publish(topic, 1, retained, payload)
	if !tok.WaitTimeout(publishTimeout) {
		return fmt.Errorf("mqttutil: publish %s: no ack after %s", topic, publishTimeout)
	}
	return tok.Error()
}

// Subscribe registers handler for messages on topic (supports MQTT wildcards).
// handler receives the raw topic string and the JSON-decoded value of type T.
// The subscription is recorded so that it survives broker reconnects; handler
// may therefore be invoked again with the retained message after a reconnect.
func Subscribe[T any](client mqtt.Client, topic string, handler func(topic string, msg T)) error {
	h := func(_ mqtt.Client, m mqtt.Message) {
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
