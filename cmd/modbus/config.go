package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// DeviceConfig describes one southbound Modbus/SunSpec device.
type DeviceConfig struct {
	Name   string  `json:"name"`
	URL    string  `json:"url"` // e.g. "tcp://192.168.1.10:5020"
	UnitID uint8   `json:"unit_id"`
	Role   string  `json:"role"`  // "inverter" | "battery" | "meter"
	MaxW   float64 `json:"max_w"` // nameplate capacity (W)
	// SOCReservePct is the battery SOC reserve floor (%) used by the Tier-0 edge
	// safety interlock. 0 ⇒ default (20%). Only meaningful for battery devices.
	SOCReservePct float64 `json:"soc_reserve_pct"`
}

// Config is the JSON configuration for lexa-modbus.
type Config struct {
	MQTTBroker    string         `json:"mqtt_broker"`     // e.g. "tcp://localhost:1883"
	MQTTClientID  string         `json:"mqtt_client_id"`  // default "lexa-modbus"
	MQTTUser      string         `json:"mqtt_user"`       // broker credentials (TASK-013/W7); empty ⇒ anonymous
	MQTTPassFile  string         `json:"mqtt_pass_file"`  // path to 0600 password file; empty ⇒ anonymous
	PollIntervalS int            `json:"poll_interval_s"` // default 10
	Devices       []DeviceConfig `json:"devices"`

	// MetricsAddr is the Prometheus /metrics listen address (TASK-044).
	// Empty ⇒ default "127.0.0.1:9103" (product default: loopback-only); the
	// literal "off" disables the listener. See cmd/hub/config.go's
	// MetricsAddr doc for the bench-vs-product bind rationale (AD-008).
	MetricsAddr string `json:"metrics_addr"`
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
		cfg.MQTTClientID = "lexa-modbus"
	}
	if cfg.PollIntervalS == 0 {
		cfg.PollIntervalS = 10
	}
	if cfg.MetricsAddr == "" {
		cfg.MetricsAddr = "127.0.0.1:9103"
	}
	return &cfg, nil
}

func (c *Config) PollInterval() time.Duration {
	return time.Duration(c.PollIntervalS) * time.Second
}
