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
	PollIntervalS int            `json:"poll_interval_s"` // default 10
	Devices       []DeviceConfig `json:"devices"`
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
	return &cfg, nil
}

func (c *Config) PollInterval() time.Duration {
	return time.Duration(c.PollIntervalS) * time.Second
}
