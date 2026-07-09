// Package main implements lexa-cloudlink, the seventh lexa-hub service —
// the device's link to the cloud command and telemetry plane
// (docs/DEVICE_ROADMAP.md §2). This file is the config.go half of Unit 2.1
// (the service skeleton): Config + loadConfig follow the same house pattern
// as every other lexa-* service config (see cmd/telemetry/config.go), with
// field names matching configs/factory/cloudlink.json — written ahead of
// this file by Unit 1.6, before cmd/cloudlink existed (see that repo file's
// own README.md note: "It has not been validated against any real
// loadConfig because none exists yet"). This is that first validation.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"lexa-hub/internal/journal"
)

// UplinkConfig is the "uplink" block: batching cadence for each uplink
// stream (docs/DEVICE_ROADMAP.md §2.4's stream table — events/health/plan/
// telemetry). Unused until 2.2's batcher lands; carried here now so
// configs/cloudlink.json's shape never has to change underneath that unit.
type UplinkConfig struct {
	MeasurementsBatchS int `json:"measurements_batch_s"`
	EVSEBatchS         int `json:"evse_batch_s"`
	PlanIntervalS      int `json:"plan_interval_s"`
	HealthIntervalS    int `json:"health_interval_s"`
}

// JournalConfig is the on-disk "journal" block — the same snake_case-tags-
// over-internal/journal.Config shape as cmd/hub/config.go's JournalConfig
// (see that type's doc comment for why journal.Config isn't embedded
// directly: it carries a Now func() time.Time and a *Metrics field with no
// sensible JSON representation).
//
// Unlike hub's *JournalConfig (nil ⇒ journaling disabled entirely),
// cloudlink's Journal is never optional: this unit's brief is explicit that
// the journal must be wired NOW, at startup, so its directory exists ahead
// of 2.4's downlink-intent audit trail — an absent "journal" key in the
// config file means "use the default dir," never "skip journaling."
type JournalConfig struct {
	Dir            string `json:"dir"`              // default set by loadConfig if empty
	MaxBytes       int64  `json:"max_bytes"`        // 0 → journal.DefaultMaxBytes
	MaxFiles       int    `json:"max_files"`        // 0 → journal.DefaultMaxFiles
	FlushEvery     int    `json:"flush_every"`      // 0 → journal.DefaultFlushEvery
	FlushIntervalS int    `json:"flush_interval_s"` // 0 → journal.DefaultFlushInterval
}

// ToLibrary converts jc into a journal.Config for journal.Open, mirroring
// cmd/hub/config.go's JournalConfig.ToLibrary.
func (jc JournalConfig) ToLibrary() journal.Config {
	return journal.Config{
		Dir:           jc.Dir,
		MaxBytes:      jc.MaxBytes,
		MaxFiles:      jc.MaxFiles,
		FlushEvery:    jc.FlushEvery,
		FlushInterval: time.Duration(jc.FlushIntervalS) * time.Second,
	}
}

// Config is the JSON configuration for lexa-cloudlink (/etc/lexa/cloudlink.json).
type Config struct {
	// SchemaVersion is lexa-migrate's generic "schema_version" key
	// (cmd/lexa-migrate/migrations.go already lists "cloudlink.json" in its
	// registry, ready for the day this file starts shipping). This service
	// does not branch on it itself — migration is lexa-migrate's job — it
	// is carried here only so json.Unmarshal round-trips the field like
	// every other lexa-*.json.
	SchemaVersion int `json:"schema_version"`

	// Enabled is the master on/off switch for the cloud link (§2.2/§2.3).
	// false is the safe shipped default (configs/cloudlink.json,
	// configs/factory/cloudlink.json) until identity provisioning exists.
	// The service still starts, still runs its local MQTT session and
	// retained status publisher, and NEVER attempts a WAN connection in
	// this unit regardless of this flag — see main.go's doc comment on
	// cloudSession for why that holds even when Enabled is true today.
	Enabled bool `json:"enabled"`

	// Endpoint is the cloud MQTT broker URI (e.g.
	// "ssl://xxxx-ats.iot.us-west-2.amazonaws.com:8883"). Required when
	// Enabled is true (enforced below, at load time — spec: "fail loud,
	// not at connect time"); ignored, and never dialed, while Enabled is
	// false.
	Endpoint string `json:"endpoint"`

	// SerialFile is the device-identity serial path — the same file
	// cmd/api's mDNS/TLS identity and cmd/lexa-migrate/factory-reset
	// already read (see cmd/api/tlscert.go's defaultSerialFile constant,
	// which this mirrors).
	SerialFile string `json:"serial_file"`

	// CloudCA/CloudCert/CloudKey are the mTLS identity used to dial
	// Endpoint via standard crypto/tls — NOT wolfSSL; this service is pure
	// Go, CGO_ENABLED=0, unlike lexa-northbound/lexa-telemetry. Unused
	// until 2.3's cloud session lands.
	CloudCA   string `json:"cloud_ca"`
	CloudCert string `json:"cloud_cert"`
	CloudKey  string `json:"cloud_key"`

	// CertExpiryWarnDays is the days-remaining threshold the future cloud
	// cert monitor (§2.7, a pattern copy of cmd/northbound/certmon.go)
	// warns at. Carried here now; no monitor runs in this unit.
	CertExpiryWarnDays int `json:"cert_expiry_warn_days"`

	// SpoolDir/SpoolMaxBytes configure the disk-backed FIFO between the
	// local bus and the cloud uplink (§2.5, internal/spool). Unused until
	// 2.2 wires a real spool into main.go's spoolStats seam (see
	// status.go).
	SpoolDir      string `json:"spool_dir"`
	SpoolMaxBytes int64  `json:"spool_max_bytes"`

	Uplink UplinkConfig `json:"uplink"`

	MQTTBroker   string `json:"mqtt_broker"`
	MQTTClientID string `json:"mqtt_client_id"`

	// MQTTUser/MQTTPassFile: broker credentials (TASK-013/W7/AD-008); empty
	// MQTTUser ⇒ anonymous connect, the same staged-rollout default every
	// other service uses (see cmd/hub/config.go).
	MQTTUser     string `json:"mqtt_user"`
	MQTTPassFile string `json:"mqtt_pass_file"`

	// MetricsAddr is the Prometheus /metrics listen address (TASK-044).
	// Empty ⇒ default "127.0.0.1:9106" (product default: loopback-only;
	// port assignment per docs/DEVICE_ROADMAP.md §1.5/§2.9); the literal
	// "off" disables the listener. See cmd/hub/config.go's MetricsAddr doc
	// for the bench-vs-product bind rationale (AD-008).
	MetricsAddr string `json:"metrics_addr"`

	// LogLevel selects the slog level ("debug"|"info"|"warn"|"error");
	// default "info" (TASK-045). See internal/logutil.ParseLevel.
	LogLevel string `json:"log_level"`

	// Journal is the "journal" block — see JournalConfig's doc for why
	// this is never nil/optional the way cmd/hub's Journal field is.
	Journal JournalConfig `json:"journal"`
}

// Defaults for Config's zero-valued fields (loadConfig). Named so
// config_test.go can pin each one without repeating the literal.
const (
	defaultSerialFile   = "/etc/lexa/identity/serial"
	defaultCertWarnDays = 30
	defaultSpoolDir     = "/var/lib/lexa/spool"
	defaultSpoolMaxB    = 32 << 20 // 33554432 — matches configs/factory/cloudlink.json
	defaultJournalDir   = "/var/lib/lexa/journal/cloudlink"
	defaultMetricsAddr  = "127.0.0.1:9106" // docs/DEVICE_ROADMAP.md §1.5/§2.9 port assignment
)

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	if cfg.SerialFile == "" {
		cfg.SerialFile = defaultSerialFile
	}
	if cfg.CertExpiryWarnDays == 0 {
		cfg.CertExpiryWarnDays = defaultCertWarnDays
	}
	if cfg.SpoolDir == "" {
		cfg.SpoolDir = defaultSpoolDir
	}
	if cfg.SpoolMaxBytes == 0 {
		cfg.SpoolMaxBytes = defaultSpoolMaxB
	}
	if cfg.Uplink.MeasurementsBatchS == 0 {
		cfg.Uplink.MeasurementsBatchS = 60
	}
	if cfg.Uplink.EVSEBatchS == 0 {
		cfg.Uplink.EVSEBatchS = 60
	}
	if cfg.Uplink.PlanIntervalS == 0 {
		cfg.Uplink.PlanIntervalS = 300
	}
	if cfg.Uplink.HealthIntervalS == 0 {
		cfg.Uplink.HealthIntervalS = 900
	}
	if cfg.MQTTBroker == "" {
		cfg.MQTTBroker = "tcp://localhost:1883"
	}
	if cfg.MQTTClientID == "" {
		cfg.MQTTClientID = "lexa-cloudlink"
	}
	if cfg.MetricsAddr == "" {
		cfg.MetricsAddr = defaultMetricsAddr
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.Journal.Dir == "" {
		cfg.Journal.Dir = defaultJournalDir
	}

	// Fail loud at load, not at connect time (spec requirement): a shipped
	// config that turns the cloud link on but forgot the endpoint is a
	// startup-time mistake, not a runtime one to discover only once 2.3
	// tries — and silently fails — to dial an empty string.
	if cfg.Enabled && cfg.Endpoint == "" {
		return nil, fmt.Errorf("cloudlink: enabled=true requires a non-empty endpoint")
	}

	return &cfg, nil
}

// HealthInterval returns the uplink health-stream cadence as a
// time.Duration — also the interval statusPublisher (status.go) republishes
// the retained CloudlinkStatus at.
func (c *Config) HealthInterval() time.Duration {
	return time.Duration(c.Uplink.HealthIntervalS) * time.Second
}
