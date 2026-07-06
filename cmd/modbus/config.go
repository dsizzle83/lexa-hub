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

	// LogLevel selects the slog level ("debug"|"info"|"warn"|"error");
	// default "info" (TASK-045). See internal/logutil.ParseLevel.
	LogLevel string `json:"log_level"`
	// Reconciler selects the per-device-class Device Reconciler mode
	// (AD-002/AD-013), keyed by device class ("battery"/"solar"). TASK-032
	// deleted the legacy lexa/control/* write path, so battery and solar are
	// reconciler-only and their mode MUST be "active" when devices of that role
	// are configured — loadConfig rejects off/shadow (or an absent key) for a
	// present role, because there is no legacy path left to fall back to.
	// ("shadow" mode plumbing survives in the shells for future non-migrated
	// classes but is no longer a valid battery/solar config value.)
	Reconciler map[string]string `json:"reconciler"`
}

// Reconciler mode values (the "reconciler" config map's values).
const (
	ReconcilerOff    = "off"
	ReconcilerShadow = "shadow"
	ReconcilerActive = "active"
)

// ReconcilerMode returns the configured reconciler mode for class ("battery"
// | "solar"), defaulting to ReconcilerOff when the class is absent or its value
// is the empty string. loadConfig has already validated the values (and, for a
// present battery/solar role, required "active" — TASK-032).
func (c *Config) ReconcilerMode(class string) string {
	if c.Reconciler == nil {
		return ReconcilerOff
	}
	mode := c.Reconciler[class]
	if mode == "" {
		return ReconcilerOff
	}
	return mode
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
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	for class, mode := range cfg.Reconciler {
		switch mode {
		case "", ReconcilerOff, ReconcilerShadow, ReconcilerActive:
			// value syntax ok; migrated-class requirement checked below
		default:
			return nil, fmt.Errorf("reconciler: unknown mode %q for class %q (want off|shadow|active)", mode, class)
		}
		// EVSE stays fatal in THIS process regardless of mode — its reconciler
		// lives in lexa-ocpp (ocpp.json's own "reconciler" key), never in modbus.
		if class != "battery" && class != "solar" && mode == ReconcilerActive {
			return nil, fmt.Errorf("reconciler active mode is battery/solar-only in lexa-modbus (class %q; evse belongs to lexa-ocpp)", class)
		}
	}
	// TASK-032: the legacy lexa/control/* command path was deleted, so battery
	// and solar are reconciler-only. If devices of those roles exist, the
	// reconciler MUST be "active": off/shadow (or an absent key) would leave no
	// write path at all and silently disable actuation — a config restored from
	// a pre-032 backup must fail loud here rather than run dark.
	roleClass := map[string]string{"battery": "battery", "inverter": "solar"}
	haveClass := map[string]bool{}
	for _, dc := range cfg.Devices {
		if cls, ok := roleClass[dc.Role]; ok {
			haveClass[cls] = true
		}
	}
	for _, cls := range []string{"battery", "solar"} {
		if haveClass[cls] && cfg.ReconcilerMode(cls) != ReconcilerActive {
			return nil, fmt.Errorf("reconciler[%q] must be \"active\" (got %q): the legacy command path was deleted in TASK-032; off/shadow would silently disable %s actuation", cls, cfg.ReconcilerMode(cls), cls)
		}
	}
	return &cfg, nil
}

func (c *Config) PollInterval() time.Duration {
	return time.Duration(c.PollIntervalS) * time.Second
}
