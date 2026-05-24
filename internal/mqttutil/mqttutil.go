// Package mqttutil provides thin helpers for connecting to an MQTT broker,
// publishing JSON payloads, and subscribing with JSON unmarshalling.
package mqttutil

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// Connect creates an MQTT client connected to broker with the given clientID.
// Auto-reconnect is enabled; the call blocks until the initial connection succeeds
// or times out after 30 s.
func Connect(broker, clientID string) (mqtt.Client, error) {
	opts := mqtt.NewClientOptions().
		AddBroker(broker).
		SetClientID(clientID).
		SetAutoReconnect(true).
		SetMaxReconnectInterval(30 * time.Second).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetOnConnectHandler(func(c mqtt.Client) {
			log.Printf("[mqtt] connected to %s as %s", broker, clientID)
		}).
		SetConnectionLostHandler(func(c mqtt.Client, err error) {
			log.Printf("[mqtt] connection lost: %v", err)
		})

	client := mqtt.NewClient(opts)
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
func PublishJSON(client mqtt.Client, topic string, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("mqttutil: marshal %T: %w", v, err)
	}
	tok := client.Publish(topic, 1, false, payload)
	tok.Wait()
	return tok.Error()
}

// PublishJSONRetained is like PublishJSON but the message is retained.
func PublishJSONRetained(client mqtt.Client, topic string, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("mqttutil: marshal %T: %w", v, err)
	}
	tok := client.Publish(topic, 1, true, payload)
	tok.Wait()
	return tok.Error()
}

// Subscribe registers handler for messages on topic (supports MQTT wildcards).
// handler receives the raw topic string and the JSON-decoded value of type T.
func Subscribe[T any](client mqtt.Client, topic string, handler func(topic string, msg T)) error {
	tok := client.Subscribe(topic, 1, func(_ mqtt.Client, m mqtt.Message) {
		var v T
		if err := json.Unmarshal(m.Payload(), &v); err != nil {
			log.Printf("[mqtt] unmarshal on %s: %v", m.Topic(), err)
			return
		}
		handler(m.Topic(), v)
	})
	tok.Wait()
	return tok.Error()
}
