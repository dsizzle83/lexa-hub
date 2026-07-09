package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"lexa-hub/internal/journal"
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

	// TLS enables HTTPS on ListenAddr via a per-device self-signed cert
	// (cmd/api/tlscert.go, DEVICE_ROADMAP.md §4.1). nil/absent ⇒ true —
	// this is the PRODUCT default (a field-deployed Digi i.MX93 unit ships
	// TLS on). Bench/dev configs set this to false explicitly as the
	// escape hatch (see this repo's checked-in configs/api.json, which
	// does exactly that) until the dashboard/proxy chain learns the
	// fingerprint TOFU flow. A misprovisioned unit that fails to load/
	// generate its cert does NOT fall back to plaintext — see main.go,
	// where that failure is fatal at startup.
	TLS *bool `json:"tls"`

	// MDNS enables the _lexa-hub._tcp mDNS advertisement (cmd/api/mdns.go,
	// DEVICE_ROADMAP.md §4.4). nil/absent ⇒ true. Set false for a
	// multicast-hostile network or a privacy-sensitive deployment — mDNS
	// registration failure is already non-fatal (logged WARN once), this
	// key is for an operator who wants it off outright.
	MDNS *bool `json:"mdns"`

	// SerialFile is the device-identity serial used for the TLS cert's
	// CN/SAN (cmd/api/tlscert.go) and the mDNS TXT record's "serial="
	// field. Default "/etc/lexa/identity/serial"; an unreadable/empty file
	// (no identity partition provisioned yet — a bench box, or a factory
	// unit ahead of commissioning) falls back to os.Hostname().
	SerialFile string `json:"serial_file"`

	// CertDir is where the generated HTTPS server cert/key persist across
	// restarts (cmd/api/tlscert.go's ensureServerCert). Default
	// "/var/lib/lexa/api".
	CertDir string `json:"cert_dir"`

	// SiteCacheFile is the passthrough path GET /site reads for cloud-pushed
	// site metadata (cmd/api/site.go, DEVICE_ROADMAP.md §4.3/§10). Default
	// "/var/lib/lexa/site.json"; absent/unreadable/non-JSON ⇒ the "site_cache"
	// field is simply omitted from /site's response, never fabricated.
	SiteCacheFile string `json:"site_cache"`

	Devices []DeviceConfig `json:"devices"`

	// Journal is the "journal" block — DEVICE_ROADMAP.md §4.5/TASK-090's
	// config_write audit trail (POST /config/{service}). Same non-optional
	// shape as lexa-cloudlink's JournalConfig (cmd/cloudlink/config.go),
	// unlike lexa-hub's *JournalConfig (nil ⇒ disabled entirely): the
	// commissioning write path's audit trail is the whole point of this
	// unit, so an absent "journal" key means "use the default dir," never
	// "skip journaling." See JournalConfig's own doc for why this isn't
	// internal/journal.Config embedded directly.
	Journal JournalConfig `json:"journal"`
}

// JournalConfig is the on-disk "journal" block. Its own snake_case JSON tags
// (rather than embedding internal/journal.Config directly) mirror
// cmd/hub/config.go's JournalConfig and cmd/cloudlink/config.go's
// JournalConfig: that library type carries a `Now func() time.Time` and a
// `*Metrics` field with no sensible JSON representation, and its field names
// (MaxBytes, MaxFiles) don't match this repo's snake_case wire keys without
// added tags.
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

// defaultJournalDir is Config.Journal.Dir's default when the "journal" block
// (or its "dir" key) is absent from api.json.
const defaultJournalDir = "/var/lib/lexa/journal/api"

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
	if cfg.SerialFile == "" {
		cfg.SerialFile = defaultSerialFile
	}
	if cfg.CertDir == "" {
		cfg.CertDir = "/var/lib/lexa/api"
	}
	if cfg.SiteCacheFile == "" {
		cfg.SiteCacheFile = defaultSiteCacheFile
	}
	if cfg.Journal.Dir == "" {
		cfg.Journal.Dir = defaultJournalDir
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

// TLSEnabled reports whether the HTTPS listener is on. nil/absent TLS ⇒
// true — the product default (DEVICE_ROADMAP.md §4.1).
func (c *Config) TLSEnabled() bool {
	return c.TLS == nil || *c.TLS
}

// MDNSEnabled reports whether the mDNS advertisement is on. nil/absent MDNS
// ⇒ true — the product default (DEVICE_ROADMAP.md §4.4).
func (c *Config) MDNSEnabled() bool {
	return c.MDNS == nil || *c.MDNS
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
