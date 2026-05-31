package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// DeviceConfig names a southbound device and its role, so the API can label
// it correctly in the /status response.
type DeviceConfig struct {
	Name string  `json:"name"`
	Role string  `json:"role"` // "inverter" | "battery" | "meter"
	MaxW float64 `json:"max_w"`
}

// Config is the JSON configuration for lexa-api.
type Config struct {
	MQTTBroker   string `json:"mqtt_broker"`    // tcp://localhost:1883
	MQTTClientID string `json:"mqtt_client_id"` // default "lexa-api"

	// ListenAddr is the HTTP listen address (host:port). Default ":9100".
	ListenAddr string `json:"listen_addr"`

	// StaleAfterS is the seconds since the last measurement after which a
	// device is reported as Connected=false. Default 30.
	StaleAfterS int `json:"stale_after_s"`

	// LogBufferSize is the number of recent log lines retained in memory and
	// replayed to new SSE subscribers. Default 256.
	LogBufferSize int `json:"log_buffer_size"`

	Devices []DeviceConfig `json:"devices"`
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
		cfg.MQTTClientID = "lexa-api"
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":9100"
	}
	if cfg.StaleAfterS == 0 {
		cfg.StaleAfterS = 30
	}
	if cfg.LogBufferSize == 0 {
		cfg.LogBufferSize = 256
	}
	return &cfg, nil
}

func (c *Config) StaleAfter() time.Duration {
	return time.Duration(c.StaleAfterS) * time.Second
}
