package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"lexa-hub/internal/journal"
	"lexa-hub/internal/northbound/run"
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

	// PollRateModeStr selects TASK-071 (§12) walk-cadence pacing:
	// "honor" throttles the walk interval to the server-advertised pollRate
	// (DeviceCapability/Time/DERControlList — see run.effectiveInterval);
	// "override" walks at the fixed discovery_interval_s cadence every
	// cycle, ignoring pollRate (pre-TASK-071 behavior). Empty/absent
	// defaults to "honor" here (loadConfig) — the spec-polite PRODUCT
	// default. The BENCH config (configs/northbound.json) ships "override"
	// explicitly: Mayhem's adoption-latency-sensitive scenarios need
	// control adoption in seconds, far faster than gridsim's 60s
	// DERControlList pollRate allows in honor mode, and deploy-hub-pi.sh
	// reinstalls that file whole on every deploy so a redeploy always
	// reinstates the explicit override, never relying on this default.
	PollRateModeStr string `json:"poll_rate_mode,omitempty"`

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

	// CertRotateSentinel is the path RotationController (TASK-073/§10.5)
	// polls for a staged cert-rotation request — empty defaults to
	// defaultCertRotateSentinel (/etc/lexa/certs/rotate.request). Written by
	// scripts/rotate-cert.sh; see rotate.go's package doc and
	// docs/CERT_ROTATION_RUNBOOK.md.
	CertRotateSentinel string `json:"cert_rotate_sentinel,omitempty"`

	// CertRotatePollIntervalS is how often (seconds) RotationController
	// checks for a pending sentinel — 0/absent defaults to
	// defaultCertRotatePollInterval (5s). Short by design: a rotation
	// request should be picked up promptly, and an idle stat() of one file
	// every few seconds costs nothing.
	CertRotatePollIntervalS int `json:"cert_rotate_poll_interval_s,omitempty"`

	// ResponseStatePath is the WS-4.2 durable persistence path for
	// responses.Tracker's posted/alerted maps (TASK-041's acknowledged
	// northbound half — internal/northbound/responses/persist.go): a small
	// self-compacting NDJSON append-log so a lexa-northbound restart can
	// neither lose a not-yet-posted CannotComply nor re-POST a duplicate
	// Response for an event it already acknowledged. Unlike Journal/
	// Snapshot's opt-in-by-presence rollout shape, this ships ON by
	// default with NO config change needed on the bench: empty/absent
	// defaults to defaultResponseStatePath (a sibling of the journal dir,
	// created independently of whether "journal" is configured); the
	// literal "off" disables persistence entirely (RAM-only, pre-WS-4.2
	// behavior) — same convention as MetricsAddr's "off".
	ResponseStatePath string `json:"response_state_path,omitempty"`
}

// defaultResponseStatePath is ResponseStatePath's default when empty/absent.
const defaultResponseStatePath = "/var/lib/lexa/journal/northbound/response-state.ndjson"

// ResponseStateDisabled reports whether the config explicitly opted out of
// WS-4.2 persistence via the literal "off" value.
func (c *Config) ResponseStateDisabled() bool {
	return c.ResponseStatePath == "off"
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
	if cfg.PollRateModeStr == "" {
		cfg.PollRateModeStr = string(run.PollRateHonor)
	}
	if cfg.ResponseStatePath == "" {
		cfg.ResponseStatePath = defaultResponseStatePath
	}
	return &cfg, nil
}

func (c *Config) DiscoveryInterval() time.Duration {
	return time.Duration(c.DiscoveryIntervalS) * time.Second
}

// Uncommissioned reports whether this config describes a factory-fresh or
// factory-reset unit with nothing to walk (Unit 1.7, closing a gap found in
// unit 1.6: DEVICE_ROADMAP.md §9 / configs/factory/README.md "Known gaps"
// #1). The factory profile (configs/factory/northbound.json) ships
// Server == "" precisely to mean this; DEVICE_ROADMAP.md §9 describes the
// intended state as "no server URL + discovery disabled", but there is no
// separate discovery-enable config key today — an empty Server already
// means discovery has nothing to dial, so Server alone decides.
//
// Cert paths (CACert/ClientCert/ClientKey) do NOT factor in, even though
// the factory profile ships them pointing at the standard
// /etc/lexa/certs/* locations: those paths are baked into every profile
// and may not exist on disk yet on a virgin device, but their presence or
// absence is not what "uncommissioned" means — only whether a server has
// been configured to talk to. A config with Server set but a missing/
// unreadable cert file is a configured-but-broken unit and must keep
// failing loudly at TLS-fetcher construction, exactly as it does today —
// this method must never mask that.
func (c *Config) Uncommissioned() bool {
	return c.Server == ""
}

// PollRateMode returns the configured run.PollRateMode (TASK-071). Any
// value other than the literal "honor" string is treated as
// run.PollRateOverride by run.effectiveInterval — see that function's doc —
// so an unrecognized poll_rate_mode string fails toward today's-unchanged
// behavior rather than silently honoring pollRate.
func (c *Config) PollRateMode() run.PollRateMode {
	return run.PollRateMode(c.PollRateModeStr)
}

// CertRotatePollInterval returns the configured sentinel poll interval, or
// zero if unset — RotationController.Run treats <=0 as "use its own
// default" (defaultCertRotatePollInterval), the same convention
// DiscoveryInterval/Loop and certCheckInterval/Monitor.Run already use.
func (c *Config) CertRotatePollInterval() time.Duration {
	return time.Duration(c.CertRotatePollIntervalS) * time.Second
}
