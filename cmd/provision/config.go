package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// devkitPoP is the documented development-only proof-of-possession used when
// pop_file is absent or empty. A PRODUCT image must ship a per-unit random PoP
// at pop_file (ADR-0002: manufacturing-provisioned to /etc/lexa/provision-pop);
// this hardcode exists only so the dev kit can commission without a
// manufacturing step (unit B4 replaces the loading story and forbids this on a
// product image). It is intentionally obvious, not secret.
const devkitPoP = "LEXA-DEVKIT-POP"

// Config is the JSON configuration for lexa-provision (/etc/lexa/provision.json).
type Config struct {
	// Adapter is the BlueZ adapter name (default "hci0").
	Adapter string `json:"adapter"`

	// PopFile holds the per-unit proof-of-possession (the HKDF salt). Absent or
	// empty ⇒ devkitPoP (dev only). Default "/etc/lexa/provision-pop".
	PopFile string `json:"pop_file"`

	// SerialFile is the device-identity serial, read for the info document and
	// the advertised local name (LEXA-<serial6>). Default
	// "/etc/lexa/identity/serial"; an unreadable/empty file falls back to the
	// hostname, then "unknown" (same resolution philosophy as cmd/api).
	SerialFile string `json:"serial_file"`

	// MarkerFile is the commissioned marker whose ABSENCE gates advertising.
	// Default "/etc/lexa/commissioned".
	MarkerFile string `json:"marker_file"`

	// HandoffPort is the API port reported in the join handoff (B3/B4 seam;
	// unused by B2's stub join). Default 9100.
	HandoffPort int `json:"handoff_port"`

	// ReconcileIntervalS is how often the advertising gate is re-evaluated so a
	// mid-run commit stops advertising. Default 10.
	ReconcileIntervalS int `json:"reconcile_interval_s"`

	// MetricsAddr is the Prometheus /metrics listen address (TASK-044 pattern).
	// Empty ⇒ default "127.0.0.1:9107" (loopback-only product default); the
	// literal "off" disables the listener.
	MetricsAddr string `json:"metrics_addr"`

	// LogLevel selects the slog level ("debug"|"info"|"warn"|"error"); default
	// "info". See internal/logutil.ParseLevel.
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
	cfg.applyDefaults()
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Adapter == "" {
		c.Adapter = "hci0"
	}
	if c.PopFile == "" {
		c.PopFile = "/etc/lexa/provision-pop"
	}
	if c.SerialFile == "" {
		c.SerialFile = "/etc/lexa/identity/serial"
	}
	if c.MarkerFile == "" {
		c.MarkerFile = "/etc/lexa/commissioned"
	}
	if c.HandoffPort == 0 {
		c.HandoffPort = 9100
	}
	if c.ReconcileIntervalS == 0 {
		c.ReconcileIntervalS = 10
	}
	if c.MetricsAddr == "" {
		c.MetricsAddr = "127.0.0.1:9107"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
}

// ReconcileInterval is the advertising re-check cadence.
func (c *Config) ReconcileInterval() time.Duration {
	return time.Duration(c.ReconcileIntervalS) * time.Second
}

// loadPoP reads the proof-of-possession from PopFile, falling back to the
// documented dev-kit default when the file is absent or empty. The bool reports
// whether a real file was used (false ⇒ dev-kit default, which main() logs a
// WARN about). B4 replaces this with the manufacturing-provisioned loading +
// the product-must-not-use-a-default rule.
func (c *Config) loadPoP() (pop string, fromFile bool) {
	if data, err := os.ReadFile(c.PopFile); err == nil {
		if s := strings.TrimSpace(string(data)); s != "" {
			return s, true
		}
	}
	return devkitPoP, false
}

// resolveSerial reads SerialFile (trimmed) as the device serial, falling back
// to the hostname and then "unknown" — the same resolution cmd/api uses so the
// info document's serial, the advertised name, and the API cert CN all agree.
func (c *Config) resolveSerial() string {
	if data, err := os.ReadFile(c.SerialFile); err == nil {
		if s := strings.TrimSpace(string(data)); s != "" {
			return s
		}
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown"
}
