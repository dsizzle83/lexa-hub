package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"lexa-hub/internal/journal"
)

// Config is the JSON configuration for lexa-northbound.
type Config struct {
	MQTTBroker   string `json:"mqtt_broker"`
	MQTTClientID string `json:"mqtt_client_id"`

	// MQTTUser/MQTTPassFile: broker credentials (TASK-013/W7/AD-008); empty
	// MQTTUser ⇒ anonymous connect (staged-rollout default, see cmd/hub/config.go).
	MQTTUser     string `json:"mqtt_user"`
	MQTTPassFile string `json:"mqtt_pass_file"`

	// Northbound CSIP server (wolfSSL mTLS)
	Server     string `json:"server"`
	CACert     string `json:"ca_cert"`
	ClientCert string `json:"client_cert"`
	ClientKey  string `json:"client_key"`
	LFDI       string `json:"lfdi"` // derived from ClientCert when empty

	DiscoveryIntervalS int    `json:"discovery_interval_s"` // default 60
	ResponseSetPath    string `json:"response_set_path"`    // default "/rsps/0/r"

	// MetricsAddr is the Prometheus /metrics listen address (TASK-044).
	// Empty ⇒ default "127.0.0.1:9102" (product default: loopback-only); the
	// literal "off" disables the listener. See cmd/hub/config.go's
	// MetricsAddr doc for the bench-vs-product bind rationale (AD-008).
	MetricsAddr string `json:"metrics_addr"`

	// LogLevel selects the slog level ("debug"|"info"|"warn"|"error");
	// default "info" (TASK-045). See internal/logutil.ParseLevel.
	LogLevel string `json:"log_level"`

	// Journal is the optional durable event-journal block (TASK-040): here,
	// northbound only ever emits cannot_comply_posted + service_start. A
	// nil/absent "journal" key disables journaling entirely.
	Journal *JournalConfig `json:"journal,omitempty"`

	// CertExpiryWarnDays is the cert-expiry monitor's WARN threshold in days
	// remaining (TASK-072/§10.5) — 0/absent defaults to
	// defaultCertExpiryWarnDays (30, the release-checklist gate). lexa-telemetry
	// shares these same cert files (its own ca_cert/client_cert paths point at
	// the same on-disk PEMs) but does not run a second monitor — see
	// cmd/northbound/certmon.go's package doc.
	CertExpiryWarnDays int `json:"cert_expiry_warn_days,omitempty"`
}

// JournalConfig is the on-disk "journal" block — a duplicate of cmd/hub's
// JournalConfig (same shape, same rationale for not embedding
// internal/journal.Config directly; see that copy's doc comment). Not
// shared between the two: cmd/* packages don't import each other (05 §1).
type JournalConfig struct {
	Dir            string `json:"dir"`
	MaxBytes       int64  `json:"max_bytes"`
	MaxFiles       int    `json:"max_files"`
	FlushEvery     int    `json:"flush_every"`
	FlushIntervalS int    `json:"flush_interval_s"`
}

// ToLibrary converts jc into a journal.Config for journal.Open.
func (jc *JournalConfig) ToLibrary() journal.Config {
	return journal.Config{
		Dir:           jc.Dir,
		MaxBytes:      jc.MaxBytes,
		MaxFiles:      jc.MaxFiles,
		FlushEvery:    jc.FlushEvery,
		FlushInterval: time.Duration(jc.FlushIntervalS) * time.Second,
	}
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
		cfg.MQTTClientID = "lexa-northbound"
	}
	if cfg.DiscoveryIntervalS == 0 {
		cfg.DiscoveryIntervalS = 60
	}
	if cfg.ResponseSetPath == "" {
		cfg.ResponseSetPath = "/rsps/0/r"
	}
	if cfg.MetricsAddr == "" {
		cfg.MetricsAddr = "127.0.0.1:9102"
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	return &cfg, nil
}

func (c *Config) DiscoveryInterval() time.Duration {
	return time.Duration(c.DiscoveryIntervalS) * time.Second
}
