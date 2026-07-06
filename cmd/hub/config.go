package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"lexa-hub/internal/journal"
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

	// MQTTUser/MQTTPassFile are the broker credentials (TASK-013/W7/AD-008).
	// Empty MQTTUser (the repo example default) ⇒ anonymous connect — today's
	// behavior, preserved for the staged rollout: the deploy script populates
	// these once passwords exist on the Pi, while the broker still allows
	// anonymous, and only later flips allow_anonymous off. MQTTPassFile is a
	// path to a 0600 lexa-owned file holding the password, never the password
	// inline (it must never enter git or a deploy artifact).
	MQTTUser     string `json:"mqtt_user"`
	MQTTPassFile string `json:"mqtt_pass_file"`

	// MetricsAddr is the Prometheus /metrics listen address (TASK-044).
	// Empty ⇒ default "127.0.0.1:9101" (product default: loopback-only, no
	// new externally-reachable surface); the literal "off" disables the
	// listener entirely. The bench scrape config (csip-tls-test
	// scripts/prometheus-bench.yml) needs the LAN IP, so the bench's
	// deployed configs/hub.json overrides this to "0.0.0.0:9101" — a
	// bench-only property (AD-008's framing: bench binds LAN, product
	// default stays localhost), never the product default.
	MetricsAddr string `json:"metrics_addr"`

	EngineIntervalS int  `json:"engine_interval_s"` // default 15
	SafetyIntervalS int  `json:"safety_interval_s"` // fast protection loop; default 1, 0 disables
	Debug           bool `json:"debug"`

	// LogLevel selects the slog level ("debug"|"info"|"warn"|"error");
	// default "info" (TASK-045). See internal/logutil.ParseLevel.
	LogLevel string `json:"log_level"`

	Devices  []DeviceConfig  `json:"devices"`
	Stations []StationConfig `json:"stations"`

	// Planner holds the 24-hour cost-optimal dispatch configuration.
	Planner orchestrator.PlannerCfg `json:"planner"`

	// Journal is the optional durable event-journal block (TASK-040):
	// control adoptions/releases, dispatches, breach episodes, and (on the
	// northbound side) CannotComply POSTs. A nil/absent "journal" key
	// disables journaling entirely — every emit site in cmd/hub is guarded
	// by `if jw != nil`, so this is a true no-op, not a degraded default.
	Journal *JournalConfig `json:"journal,omitempty"`

	// Snapshot is the optional breach-episode snapshot block (TASK-041,
	// AD-005 second half). A nil/absent "snapshot" key, or one with an empty
	// path, disables snapshot writing entirely (true no-op, matching
	// Journal's rollout shape). When Path is set but Enabled is false (the
	// shipped default for one full campaign — see TASK-041's "Implementation
	// strategy"), the hub still WRITES a snapshot on every breach begin/end
	// and every 60 s while a breach is open, but never reads one back at
	// start: a write-only soak before an ops-only config flip turns restore
	// on (no code change accompanies that flip).
	Snapshot *SnapshotConfig `json:"snapshot,omitempty"`
}

// JournalConfig is the on-disk "journal" block. It intentionally has its
// own JSON tags (snake_case, matching this repo's config convention —
// PlannerCfg is the precedent) rather than embedding internal/journal.Config
// directly: that library type carries a `Now func() time.Time` and a
// `*Metrics` field with no sensible JSON representation, and its own field
// names (MaxBytes, MaxFiles) don't match the snake_case wire keys
// (`max_bytes`, `max_files`) without added tags — which would mean editing
// TASK-039's package for a JSON concern it was never designed to carry.
// ToLibrary converts this into the journal.Config Open() wants.
type JournalConfig struct {
	Dir            string `json:"dir"`              // required; journal.Open MkdirAlls it
	MaxBytes       int64  `json:"max_bytes"`        // 0 → journal.DefaultMaxBytes
	MaxFiles       int    `json:"max_files"`        // 0 → journal.DefaultMaxFiles
	FlushEvery     int    `json:"flush_every"`      // 0 → journal.DefaultFlushEvery
	FlushIntervalS int    `json:"flush_interval_s"` // 0 → journal.DefaultFlushInterval
}

// ToLibrary converts jc into a journal.Config for journal.Open. jc == nil
// (no "journal" block) is never called — callers gate construction on
// cfg.Journal != nil first.
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
		cfg.MQTTClientID = "lexa-hub"
	}
	if cfg.MetricsAddr == "" {
		cfg.MetricsAddr = "127.0.0.1:9101"
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
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	return &cfg, nil
}

func (c *Config) EngineInterval() time.Duration {
	return time.Duration(c.EngineIntervalS) * time.Second
}

func (c *Config) SafetyInterval() time.Duration {
	return time.Duration(c.SafetyIntervalS) * time.Second
}
