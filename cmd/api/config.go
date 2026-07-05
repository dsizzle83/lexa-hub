package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
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

	// MQTTUser/MQTTPassFile: broker credentials (TASK-013/W7/AD-008). Empty
	// MQTTUser ⇒ anonymous connect (default; see mqttutil.LoadPassword and
	// cmd/hub/config.go's Config for the staged-rollout rationale shared by
	// all six services).
	MQTTUser     string `json:"mqtt_user"`
	MQTTPassFile string `json:"mqtt_pass_file"`

	// ListenAddr is the HTTP listen address (host:port). Default ":9100".
	ListenAddr string `json:"listen_addr"`

	// StaleAfterS is the seconds since the last measurement after which a
	// device is reported as Connected=false. Default 30.
	StaleAfterS int `json:"stale_after_s"`

	// LogBufferSize is the number of recent log lines retained in memory and
	// replayed to new SSE subscribers. Default 256.
	LogBufferSize int `json:"log_buffer_size"`

	// APITokenFile, if non-empty, is the path to a file holding the bearer
	// token that /status and /logs require in an `Authorization: Bearer
	// <token>` header. Empty (the default, and the repo's example config) ⇒
	// auth disabled — today's behavior, preserved for the staged rollout
	// (TASK-014 / AD-008: token support is additive; the bench flips this on
	// only once every consumer presents the token). /healthz never checks it.
	APITokenFile string `json:"api_token_file"`

	// LogLevel selects the slog level ("debug"|"info"|"warn"|"error");
	// default "info" (TASK-045). See internal/logutil.ParseLevel.
	LogLevel string `json:"log_level"`

	// PlanStallAfterS bounds how long since the last lexa/hub/plan (TopicHubPlan)
	// ARRIVAL before the heartbeat is reported "stalled" (TASK-045). Default 75 s
	// — safe at both the STOCK economic cadence (5 × 15 s engine_interval_s) and
	// the FAST bench cadence (5 × 15 s FAST engine_interval_s worst case; the hub
	// also publishes on every 1 s safety tick, so in practice the heartbeat
	// advances far faster than this bound in both modes). lexa-api does not know
	// the hub's actual interval, so this is its own config rather than derived.
	PlanStallAfterS int `json:"plan_stall_after_s"`

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
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.PlanStallAfterS == 0 {
		cfg.PlanStallAfterS = 75
	}
	return &cfg, nil
}

func (c *Config) StaleAfter() time.Duration {
	return time.Duration(c.StaleAfterS) * time.Second
}

// PlanStallAfter is PlanStallAfterS as a time.Duration (TASK-045).
func (c *Config) PlanStallAfter() time.Duration {
	return time.Duration(c.PlanStallAfterS) * time.Second
}

// LoadAPIToken reads the bearer token from APITokenFile. An unset
// APITokenFile returns ("", nil) — auth disabled, the legacy-open default.
// A configured-but-unreadable-or-empty file is a startup-time configuration
// error (fail loud rather than silently run open or silently reject every
// request with an unusable token).
func (c *Config) LoadAPIToken() (string, error) {
	if c.APITokenFile == "" {
		return "", nil
	}
	data, err := os.ReadFile(c.APITokenFile)
	if err != nil {
		return "", fmt.Errorf("read api_token_file %s: %w", c.APITokenFile, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("api_token_file %s is configured but empty", c.APITokenFile)
	}
	return token, nil
}
