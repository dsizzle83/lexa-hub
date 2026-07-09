package main

// cloud.go is unit 2.3: the CLOUD MQTT session — a paho client speaking mqtts
// (ssl://) to the IoT-Core endpoint over a standard crypto/tls config with
// per-device mTLS identity. This is deliberately NOT built through
// internal/mqttutil: that package's Connect* helpers are for the LOCAL broker
// (username/password auth, no TLS seam), whereas the cloud dial is TLS-only
// (the cert IS the credential — ECOSYSTEM_ROADMAP §5) and pure Go
// (CGO_ENABLED=0 holds: crypto/tls, not wolfSSL — wolfSSL is CSIP-client-only,
// lexa-northbound/telemetry). paho is already a module dependency, so the
// cloud session is paho-over-crypto/tls with no new deps.

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// cloudPublisher is everything the batcher (batch.go) needs from the cloud
// session. It is a superset of status.go's cloudSession seam (Connected()), so
// a value of this type satisfies statusPublisher's cloudSession parameter too
// — one concrete object serves both. The batcher depends on this interface,
// not the concrete *cloudMQTT, so tests inject a recording fake.
type cloudPublisher interface {
	// Connected reports whether the cloud link is up right now.
	Connected() bool
	// Serial is the device serial (cloud MQTT ClientID and the {serial}
	// segment of the uplink topics). Empty on the disabled stub.
	Serial() string
	// PublishFrame publishes a QoS 1 frame to topic, waiting up to timeout for
	// the PUBACK. A returned error (timeout, broker error, or not-connected)
	// means the frame was NOT durably delivered: the batcher keeps it spooled
	// and redelivers it (same seq) — at-least-once, never exactly-once.
	PublishFrame(topic string, payload []byte, timeout time.Duration) error
}

// Uplink topics (ECOSYSTEM_ROADMAP §5 / DEVICE_ROADMAP §2.4): telemetry (P2)
// frames on …/telemetry, everything else (events P0, plan/health P1) on
// …/events.
func telemetryTopic(serial string) string { return "lexa/v1/" + serial + "/telemetry" }
func eventsTopic(serial string) string    { return "lexa/v1/" + serial + "/events" }

// stubCloudSession (defined in status.go) also satisfies cloudPublisher so the
// disabled path and the local-only default can share one type. Serial is empty
// and PublishFrame never runs (the batcher is not started when !cfg.Enabled),
// but the methods exist so newCloudSession can return a stub as a cloudPublisher.
func (stubCloudSession) Serial() string { return "" }
func (stubCloudSession) PublishFrame(string, []byte, time.Duration) error {
	return fmt.Errorf("cloudlink: cloud session disabled")
}

// cloudMQTT is the live cloud session.
type cloudMQTT struct {
	client mqtt.Client
	serial string
}

func (c *cloudMQTT) Connected() bool { return c.client.IsConnected() }
func (c *cloudMQTT) Serial() string  { return c.serial }

func (c *cloudMQTT) PublishFrame(topic string, payload []byte, timeout time.Duration) error {
	if !c.client.IsConnected() {
		return fmt.Errorf("cloudlink: cloud not connected")
	}
	tok := c.client.Publish(topic, 1, false, payload)
	if !tok.WaitTimeout(timeout) {
		return fmt.Errorf("cloudlink: publish %s: no ack after %s", topic, timeout)
	}
	return tok.Error()
}

// newCloudSession builds the cloud session. When cfg.Enabled is false it
// returns the stub and never dials the WAN — the local-only box is a
// first-class configuration (§2.2). When enabled, a cert/key/CA load failure
// or an unreadable serial is FATAL (returned as an error → main log.Fatalf):
// a misprovisioned unit must fail loud at startup, not silently never connect
// (spec item 2). A WAN outage at startup is NOT fatal: the dial is bounded and
// auto-reconnect carries the session up whenever the link returns (the whole
// point of the store-and-forward spool behind it).
func newCloudSession(cfg *Config, m *cloudlinkMetrics) (cloudPublisher, error) {
	if !cfg.Enabled {
		return stubCloudSession{}, nil
	}

	serial, err := readSerial(cfg.SerialFile)
	if err != nil {
		return nil, fmt.Errorf("serial: %w", err)
	}
	tlsCfg, err := buildTLSConfig(cfg)
	if err != nil {
		return nil, err // misprovisioned identity — fail loud
	}

	sess := &cloudMQTT{serial: serial}
	opts := mqtt.NewClientOptions().
		AddBroker(cfg.Endpoint).
		SetClientID(serial).
		SetTLSConfig(tlsCfg).
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetMaxReconnectInterval(60 * time.Second).
		SetConnectRetry(true).
		SetConnectRetryInterval(10 * time.Second).
		SetOnConnectHandler(func(mqtt.Client) {
			slog.Info("cloudlink: cloud connected", "endpoint", cfg.Endpoint, "serial", serial)
			m.connected.Set(1)
		}).
		SetConnectionLostHandler(func(_ mqtt.Client, e error) {
			slog.Warn("cloudlink: cloud connection lost", "err", e)
			m.connected.Set(0)
			m.cloudReconn.Inc()
		})

	sess.client = mqtt.NewClient(opts)
	tok := sess.client.Connect()
	// Bounded courtesy wait so a healthy WAN logs "connected" before main()
	// proceeds; a down WAN is not fatal — ConnectRetry keeps trying in the
	// background and the spool absorbs everything until the link returns.
	if !tok.WaitTimeout(15 * time.Second) {
		slog.Warn("cloudlink: cloud not connected at startup — spooling until the WAN returns",
			"endpoint", cfg.Endpoint)
	} else if err := tok.Error(); err != nil {
		slog.Warn("cloudlink: cloud initial connect error — retrying in background",
			"endpoint", cfg.Endpoint, "err", err)
	}
	return sess, nil
}

// buildTLSConfig assembles the mTLS config from the configured PEM files:
// cloud_ca pins the server, cloud_cert/cloud_key are the device identity.
// MinVersion TLS 1.2 (the cloud floor; CSIP's CCM-8-only cipher is a utility
// requirement, not a cloud one, so the default cipher suites apply here).
func buildTLSConfig(cfg *Config) (*tls.Config, error) {
	caPEM, err := os.ReadFile(cfg.CloudCA)
	if err != nil {
		return nil, fmt.Errorf("cloud_ca %s: %w", cfg.CloudCA, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("cloud_ca %s: no certificates parsed", cfg.CloudCA)
	}
	cert, err := tls.LoadX509KeyPair(cfg.CloudCert, cfg.CloudKey)
	if err != nil {
		return nil, fmt.Errorf("cloud cert/key (%s / %s): %w", cfg.CloudCert, cfg.CloudKey, err)
	}
	return &tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// readSerial loads the device serial, trimming surrounding whitespace (the
// provisioning writer appends a newline). An empty file is a provisioning
// error — fail loud rather than dial with an empty ClientID.
func readSerial(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return "", fmt.Errorf("serial file %s is empty", path)
	}
	return s, nil
}

// compile-time proof the live session satisfies the interfaces it must.
var (
	_ cloudPublisher = (*cloudMQTT)(nil)
	_ cloudPublisher = stubCloudSession{}
	_ cloudSession   = (*cloudMQTT)(nil)
)
