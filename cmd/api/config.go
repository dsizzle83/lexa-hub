package main

import (
	"encoding/json"
	"fmt"
	"net"
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

	// ListenAddr is the HTTP listen address (host:port). Default
	// "127.0.0.1:9100" (WS-1, V1.0 punch list: loopback-only is the product
	// default — a wildcard/LAN bind with no token is refused below unless
	// Bench is set). The bench's deployed api.json explicitly overrides this
	// to a LAN-reachable ":9100" (dashboard/metersim on other 69.0.0.x hosts
	// need to reach it), same bench-vs-product framing as MetricsAddr in
	// cmd/hub/config.go.
	ListenAddr string `json:"listen_addr"`

	// Bench is the explicit escape hatch (WS-1) that lets lexa-api bind a
	// non-loopback ListenAddr with no APITokenFile configured — the pre-WS-1
	// default, and still what the bench's LAN-reachable :9100 needs when
	// deploy-hub-pi.sh's --bench-insecure opt-out is used. PRODUCT deploys
	// must leave this false/absent.
	Bench bool `json:"bench"`

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

	// MQTTDeafRestartAfterS bounds how long lexa-api keeps kicking the
	// systemd watchdog while mc.IsConnected() reports true but the broker
	// connection has actually been down the whole time (WS-9.1):
	// mqtt.Client.IsConnected() stays true for paho's entire AutoReconnect
	// retry loop, not just while actually connected, so a naive kick gated
	// on it alone never trips during a sustained outage. A watchdog.DeafTracker
	// tracks continuous-disconnected time from mqttutil's OnConnectionLost/
	// OnReconnect hooks; once that exceeds this many seconds, the kick gate
	// stops firing (alongside the existing probeHealthz gate) and systemd's
	// WatchdogSec restarts the service. Default 300 (5 min) when unset/zero.
	MQTTDeafRestartAfterS int `json:"mqtt_deaf_restart_after_s"`

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
		cfg.ListenAddr = "127.0.0.1:9100"
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
	if cfg.MQTTDeafRestartAfterS == 0 {
		cfg.MQTTDeafRestartAfterS = 300
	}
	// WS-1 (V1.0 punch list, security fail-closed by default): a wildcard/LAN
	// bind with no bearer-token auth is an unauthenticated control-adjacent
	// HTTP surface reachable from the whole LAN. Refuse it unless Bench opts
	// out — deploy-hub-pi.sh's default path now always provisions
	// api_token_file (see its header), so this only fires for a hand-edited
	// or pre-WS-1 config, or the explicit --bench-insecure path (which also
	// sets bench:true).
	if !cfg.Bench && cfg.APITokenFile == "" && !isLoopbackAddr(cfg.ListenAddr) {
		return nil, fmt.Errorf("api: refusing to bind non-loopback listen_addr %q with no api_token_file configured: the product default requires bearer-token auth on any externally-reachable bind (TASK-014, WS-1) — set \"bench\": true in the config to run open on the air-gapped bench LAN, or configure api_token_file", cfg.ListenAddr)
	}
	return &cfg, nil
}

// isLoopbackAddr reports whether addr (host:port, or a bare host/port form
// such as ":9100") resolves to a loopback-only bind (127.0.0.1, ::1,
// localhost) as opposed to a wildcard ("", "0.0.0.0") or LAN-reachable
// interface address.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// No ":port" present (or a malformed addr) — treat the whole string
		// as the host part so "localhost" (no port) still classifies right;
		// an addr net.Listen would itself reject is not our problem to
		// diagnose here.
		host = addr
	}
	if host == "" {
		return false // wildcard bind (":9100" form)
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Config) StaleAfter() time.Duration {
	return time.Duration(c.StaleAfterS) * time.Second
}

// PlanStallAfter is PlanStallAfterS as a time.Duration (TASK-045).
func (c *Config) PlanStallAfter() time.Duration {
	return time.Duration(c.PlanStallAfterS) * time.Second
}

// MQTTDeafRestartAfter is MQTTDeafRestartAfterS as a time.Duration (WS-9.1).
func (c *Config) MQTTDeafRestartAfter() time.Duration {
	return time.Duration(c.MQTTDeafRestartAfterS) * time.Second
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
