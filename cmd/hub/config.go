package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"lexa-hub/internal/orchestrator"
)

// DeviceConfig describes a device role and capacity for the orchestrator.
// The hub does not connect to Modbus directly; it reads measurements from MQTT.
type DeviceConfig struct {
	Name string  `json:"name"`
	Role string  `json:"role"`  // "inverter" | "battery" | "meter"
	MaxW float64 `json:"max_w"` // nameplate capacity (W)
}

// StationConfig describes an EV charging station known to the hub.
type StationConfig struct {
	ID          string  `json:"id"`
	MaxCurrentA float64 `json:"max_current_a"` // hardware limit (A); default 32
}

// Config is the JSON configuration for lexa-hub (orchestrator).
type Config struct {
	MQTTBroker   string `json:"mqtt_broker"`
	MQTTClientID string `json:"mqtt_client_id"`

	EngineIntervalS int  `json:"engine_interval_s"` // default 15
	SafetyIntervalS int  `json:"safety_interval_s"` // fast protection loop; default 1, 0 disables
	Debug           bool `json:"debug"`

	Devices  []DeviceConfig  `json:"devices"`
	Stations []StationConfig `json:"stations"`

	// Planner holds the 24-hour cost-optimal dispatch configuration.
	Planner orchestrator.PlannerCfg `json:"planner"`
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
		cfg.MQTTClientID = "lexa-hub"
	}
	if cfg.EngineIntervalS <= 0 {
		cfg.EngineIntervalS = 15
	}
	// Fast protection loop cadence: default 1 s. Set it ≥ engine_interval_s to
	// disable (safety then runs only on the economic tick, inside Optimize).
	if cfg.SafetyIntervalS == 0 {
		cfg.SafetyIntervalS = 1
	}
	for i := range cfg.Stations {
		if cfg.Stations[i].MaxCurrentA == 0 {
			cfg.Stations[i].MaxCurrentA = 32
		}
	}
	return &cfg, nil
}

func (c *Config) EngineInterval() time.Duration {
	return time.Duration(c.EngineIntervalS) * time.Second
}

func (c *Config) SafetyInterval() time.Duration {
	return time.Duration(c.SafetyIntervalS) * time.Second
}
