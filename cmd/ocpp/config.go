package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// StationConfig pre-configures a known charging station.
type StationConfig struct {
	ID          string  `json:"id"`
	MaxCurrentA float64 `json:"max_current_a"` // hardware limit (A); default 32
	VoltageV    float64 `json:"voltage_v"`     // supply voltage (V); default 230
}

// Config is the JSON configuration for lexa-ocpp.
type Config struct {
	MQTTBroker   string `json:"mqtt_broker"`
	MQTTClientID string `json:"mqtt_client_id"`

	// MQTTUser/MQTTPassFile: broker credentials (TASK-013/W7/AD-008); empty
	// MQTTUser ⇒ anonymous connect (staged-rollout default, see cmd/hub/config.go).
	MQTTUser     string `json:"mqtt_user"`
	MQTTPassFile string `json:"mqtt_pass_file"`

	// OCPP 2.0.1 CSMS WebSocket server (Security Profile 2)
	Port     int    `json:"port"`      // default 8887
	CertPath string `json:"cert_path"` // TLS cert; plain WS when empty
	KeyPath  string `json:"key_path"`

	BasicAuthUser string `json:"basic_auth_user"`
	BasicAuthPass string `json:"basic_auth_pass"`

	Stations []StationConfig `json:"stations"`

	// MetricsAddr is the Prometheus /metrics listen address (TASK-044).
	// Empty ⇒ default "127.0.0.1:9104" (product default: loopback-only); the
	// literal "off" disables the listener. See cmd/hub/config.go's
	// MetricsAddr doc for the bench-vs-product bind rationale (AD-008).
	MetricsAddr string `json:"metrics_addr"`

	// LogLevel selects the slog level ("debug"|"info"|"warn"|"error");
	// default "info" (TASK-045). See internal/logutil.ParseLevel.
	LogLevel string `json:"log_level"`

	// Reconciler selects the EVSE Device Reconciler mode (AD-002/AD-013,
	// TASK-030): "off" | "shadow" | "active". Missing/empty ⇒ "off". Unlike
	// lexa-modbus there is exactly ONE class here (evse), so this is a scalar,
	// not a per-class map. "shadow" runs the reconciler as a passive recorder
	// alongside the legacy command path (zero SetChargingProfile writes from the
	// reconciler); "active" makes it own SetChargingProfile writes with
	// verify-by-readback and reassert-on-reconnect. loadConfig rejects any other
	// value.
	Reconciler string `json:"reconciler"`
}

// Reconciler mode values.
const (
	ReconcilerOff    = "off"
	ReconcilerShadow = "shadow"
	ReconcilerActive = "active"
)

// ReconcilerMode returns the configured EVSE reconciler mode, defaulting to
// ReconcilerOff when empty. loadConfig has already rejected any other value.
func (c *Config) ReconcilerMode() string {
	if c.Reconciler == "" {
		return ReconcilerOff
	}
	return c.Reconciler
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.MQTTBroker == "" {
		cfg.MQTTBroker = "tcp://localhost:1883"
	}
	if cfg.MQTTClientID == "" {
		cfg.MQTTClientID = "lexa-ocpp"
	}
	if cfg.Port == 0 {
		cfg.Port = 8887
	}
	if cfg.MetricsAddr == "" {
		cfg.MetricsAddr = "127.0.0.1:9104"
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	for i := range cfg.Stations {
		if cfg.Stations[i].MaxCurrentA == 0 {
			cfg.Stations[i].MaxCurrentA = 32
		}
		if cfg.Stations[i].VoltageV == 0 {
			cfg.Stations[i].VoltageV = 230
		}
	}
	switch cfg.Reconciler {
	case "", ReconcilerOff, ReconcilerShadow, ReconcilerActive:
		// ok
	default:
		return nil, fmt.Errorf("reconciler: unknown mode %q (want off|shadow|active)", cfg.Reconciler)
	}
	return &cfg, nil
}
