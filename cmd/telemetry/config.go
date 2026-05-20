package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Config is the JSON configuration for lexa-telemetry.
type Config struct {
	MQTTBroker   string   `json:"mqtt_broker"`
	MQTTClientID string   `json:"mqtt_client_id"`

	// CSIP server (same mTLS parameters as lexa-csip)
	Server     string `json:"server"`
	CACert     string `json:"ca_cert"`
	ClientCert string `json:"client_cert"`
	ClientKey  string `json:"client_key"`
	LFDI       string `json:"lfdi"`

	// Devices to register MUPs for (must match modbus device names)
	Devices []string `json:"devices"`

	MUPPostRateS int `json:"mup_post_rate_s"` // default 300
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
		cfg.MQTTClientID = "lexa-telemetry"
	}
	if cfg.MUPPostRateS == 0 {
		cfg.MUPPostRateS = 300
	}
	return &cfg, nil
}

func (c *Config) MUPPostRate() time.Duration {
	return time.Duration(c.MUPPostRateS) * time.Second
}
