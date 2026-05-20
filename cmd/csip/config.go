package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Config is the JSON configuration for lexa-csip.
type Config struct {
	MQTTBroker   string `json:"mqtt_broker"`
	MQTTClientID string `json:"mqtt_client_id"`

	// Northbound CSIP server (wolfSSL mTLS)
	Server     string `json:"server"`
	CACert     string `json:"ca_cert"`
	ClientCert string `json:"client_cert"`
	ClientKey  string `json:"client_key"`
	LFDI       string `json:"lfdi"` // derived from ClientCert when empty

	DiscoveryIntervalS int    `json:"discovery_interval_s"` // default 60
	ResponseSetPath    string `json:"response_set_path"`    // default "/rsps/0/r"
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
		cfg.MQTTClientID = "lexa-csip"
	}
	if cfg.DiscoveryIntervalS == 0 {
		cfg.DiscoveryIntervalS = 60
	}
	if cfg.ResponseSetPath == "" {
		cfg.ResponseSetPath = "/rsps/0/r"
	}
	return &cfg, nil
}

func (c *Config) DiscoveryInterval() time.Duration {
	return time.Duration(c.DiscoveryIntervalS) * time.Second
}
