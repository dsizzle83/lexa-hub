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

	// OCPP 2.0.1 CSMS WebSocket server (Security Profile 2)
	Port     int    `json:"port"`      // default 8887
	CertPath string `json:"cert_path"` // TLS cert; plain WS when empty
	KeyPath  string `json:"key_path"`

	BasicAuthUser string `json:"basic_auth_user"`
	BasicAuthPass string `json:"basic_auth_pass"`

	Stations []StationConfig `json:"stations"`
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
	for i := range cfg.Stations {
		if cfg.Stations[i].MaxCurrentA == 0 {
			cfg.Stations[i].MaxCurrentA = 32
		}
		if cfg.Stations[i].VoltageV == 0 {
			cfg.Stations[i].VoltageV = 230
		}
	}
	return &cfg, nil
}
