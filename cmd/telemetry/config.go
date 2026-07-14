package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Config is the JSON configuration for lexa-telemetry.
type Config struct {
	MQTTBroker   string `json:"mqtt_broker"`
	MQTTClientID string `json:"mqtt_client_id"`

	// MQTTUser/MQTTPassFile: broker credentials (TASK-013/W7/AD-008); empty
	// MQTTUser ⇒ anonymous connect (staged-rollout default, see cmd/hub/config.go).
	MQTTUser     string `json:"mqtt_user"`
	MQTTPassFile string `json:"mqtt_pass_file"`

	// CSIP server (same mTLS parameters as lexa-csip)
	Server     string `json:"server"`
	CACert     string `json:"ca_cert"`
	ClientCert string `json:"client_cert"`
	ClientKey  string `json:"client_key"`
	LFDI       string `json:"lfdi"`

	// Devices to register MUPs for (must match modbus device names)
	Devices []string `json:"devices"`

	MUPPostRateS int `json:"mup_post_rate_s"` // default 300

	// PostVar/PostWh gate the WP-5 MMR quantity rows: reactive power (VAr,
	// uom 63) and lifetime import/export energy (Wh, uom 72, flowDirection
	// split) — see cmd/telemetry/main.go's buildMMRs for the encoding.
	// nil/absent ⇒ true: the product default posts all four CSIP Table 2
	// mandatory quantities (W/V/Hz/VAr — BASIC-029) plus the Wh
	// accumulators; the keys exist so a bench against an older stand-in
	// server that rejects the new ReadingTypes can switch the rows off
	// without a rebuild. Same *bool nil⇒true pattern as cmd/api's TLS/MDNS.
	PostVar *bool `json:"post_var"`
	PostWh  *bool `json:"post_wh"`

	// MetricsAddr is the Prometheus /metrics listen address (TASK-044).
	// Empty ⇒ default "127.0.0.1:9105" (product default: loopback-only); the
	// literal "off" disables the listener. See cmd/hub/config.go's
	// MetricsAddr doc for the bench-vs-product bind rationale (AD-008).
	MetricsAddr string `json:"metrics_addr"`

	// LogLevel selects the slog level ("debug"|"info"|"warn"|"error");
	// default "info" (TASK-045). See internal/logutil.ParseLevel.
	LogLevel string `json:"log_level"`
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
	if cfg.MetricsAddr == "" {
		cfg.MetricsAddr = "127.0.0.1:9105"
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	return &cfg, nil
}

func (c *Config) MUPPostRate() time.Duration {
	return time.Duration(c.MUPPostRateS) * time.Second
}

// PostVarEnabled reports whether the reactive-power (VAr) MMR row is posted
// (WP-5). nil/absent PostVar ⇒ true.
func (c *Config) PostVarEnabled() bool {
	return c.PostVar == nil || *c.PostVar
}

// PostWhEnabled reports whether the lifetime import/export energy (Wh) MMR
// rows are posted (WP-5). nil/absent PostWh ⇒ true.
func (c *Config) PostWhEnabled() bool {
	return c.PostWh == nil || *c.PostWh
}

// Uncommissioned reports whether this config describes a factory-fresh or
// factory-reset unit with nothing to post to (Unit 1.7, closing a gap found
// in unit 1.6: DEVICE_ROADMAP.md §9 / configs/factory/README.md "Known
// gaps" #1, same discipline as cmd/northbound/config.go's Uncommissioned).
// The factory profile (configs/factory/telemetry.json) ships Server == ""
// precisely to mean this — MUP registration/posting has nowhere to go
// without a server, regardless of how many (if any) devices are
// configured.
//
// Cert paths (CACert/ClientCert/ClientKey) do NOT factor in, even though
// the factory profile ships them pointing at the standard
// /etc/lexa/certs/* locations (the same files lexa-northbound points at —
// see cmd/northbound/certmon.go's package doc for why telemetry does not
// run its own monitor). Those paths may not exist on disk yet on a virgin
// device; their presence/absence is not what "uncommissioned" means, only
// whether a server has been configured. A config with Server set but a
// missing/unreadable cert file is configured-but-broken and must keep
// failing loudly at TLS-fetcher construction, exactly as it does today —
// this method must never mask that.
func (c *Config) Uncommissioned() bool {
	return c.Server == ""
}
