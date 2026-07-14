// Package main implements lexa-openadr — the OpenADR 3.1 VEN service
// (WP-15, standards-buildout E1). This file is the config half: Config +
// loadConfig follow the house pattern (cmd/telemetry/config.go /
// cmd/cloudlink/config.go), with the WP-15 additions: top-level unknown keys
// WARN, never fail (architecture.md §3's rule, the same 05 §6 discipline
// cmd/hub's plant blocks use), and the OAuth2 client secret loaded from a
// 0600 file exactly like mqtt_pass_file.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"reflect"
	"strings"
	"time"
)

// Config is the JSON configuration for lexa-openadr (/etc/lexa/openadr.json).
type Config struct {
	// SchemaVersion is lexa-migrate's generic "schema_version" key — carried
	// for round-tripping like every other lexa-*.json; this service never
	// branches on it.
	SchemaVersion int `json:"schema_version"`

	// VTNURL is the OpenADR 3.1 VTN base URL (e.g. "https://vtn.example:8443").
	// "" = UNCOMMISSIONED: the service idles cleanly (metrics + healthz +
	// MQTT + watchdog still served; no VTN traffic, no bus docs) until
	// commissioning writes a real config and restarts it — the
	// cmd/telemetry Uncommissioned pattern. http:// is tolerated with a WARN
	// (bench VTNs on the air-gapped LAN); the product expectation is https.
	VTNURL string `json:"vtn_url"`

	// ClientID/ClientSecretFile are the OAuth2 client-credentials identity.
	// ClientID "" = unauthenticated VEN (3.1 allows plain GETs against
	// public-tariff VTNs — no token flow at all). ClientSecretFile is a
	// 0600 secret file (like mqtt_pass_file — provisioned on-device, never
	// committed); required when ClientID is set (fail loud at load, the
	// cloudlink enabled/endpoint pattern).
	ClientID         string `json:"client_id"`
	ClientSecretFile string `json:"client_secret_file"`

	// TokenURL overrides OAuth2 token-endpoint discovery. "" = discover via
	// GET {vtn_url}/auth/server at startup, falling back to
	// {vtn_url}/auth/token (the spec's optional built-in endpoint).
	TokenURL string `json:"token_url"`

	// ProgramIDs filters which VTN programs this VEN tracks (matched
	// against program id OR programName). Empty = all visible programs.
	ProgramIDs []string `json:"program_ids"`

	// PollIntervalS is the VTN poll cadence in seconds; default 60.
	PollIntervalS int `json:"poll_interval_s"`

	// VenName is the 3.1 ven-object name (unique per VTN) and the
	// clientName stamped on POSTed reports; default "lexa-hub".
	VenName string `json:"ven_name"`

	// ReportEnabled gates TELEMETRY_USAGE report POSTs. nil/absent ⇒ true
	// (reporting truthful data is the participation duty; same *bool
	// nil⇒true pattern as telemetry's post_var/post_wh).
	ReportEnabled *bool `json:"report_enabled"`

	MQTTBroker   string `json:"mqtt_broker"`
	MQTTClientID string `json:"mqtt_client_id"`
	// MQTTUser/MQTTPassFile: broker credentials (TASK-013/W7/AD-008); empty
	// MQTTUser ⇒ anonymous connect, the staged-rollout default.
	MQTTUser     string `json:"mqtt_user"`
	MQTTPassFile string `json:"mqtt_pass_file"`

	// MetricsAddr is the Prometheus /metrics (+ /healthz) listen address.
	// Empty ⇒ "127.0.0.1:9108" (architecture.md §0 note 1: 9106 is taken by
	// lexa-cloudlink, 9107 reserved; loopback-only product default); the
	// literal "off" disables the listener.
	MetricsAddr string `json:"metrics_addr"`

	// LogLevel selects the slog level ("debug"|"info"|"warn"|"error");
	// default "info" (TASK-045).
	LogLevel string `json:"log_level"`
}

const (
	defaultPollIntervalS = 60
	defaultVenName       = "lexa-hub"
	defaultMetricsAddr   = "127.0.0.1:9108" // WP-15; 9106=cloudlink, 9107 reserved
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
	warnUnknownKeys(data, &cfg, path)

	if cfg.PollIntervalS <= 0 {
		cfg.PollIntervalS = defaultPollIntervalS
	}
	if cfg.VenName == "" {
		cfg.VenName = defaultVenName
	}
	if cfg.MQTTBroker == "" {
		cfg.MQTTBroker = "tcp://localhost:1883"
	}
	if cfg.MQTTClientID == "" {
		cfg.MQTTClientID = "lexa-openadr"
	}
	if cfg.MetricsAddr == "" {
		cfg.MetricsAddr = defaultMetricsAddr
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}

	if cfg.VTNURL != "" {
		u, err := url.Parse(cfg.VTNURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("openadr: vtn_url %q is not a valid http(s) URL", cfg.VTNURL)
		}
		if u.Scheme == "http" {
			log.Printf("lexa-openadr config: vtn_url %s is plain http — acceptable on the air-gapped bench only; product VTNs are https (3.1 mandates TLS >= 1.2)", cfg.VTNURL)
		}
	}
	// Fail loud at load, not at token-fetch time (cloudlink's
	// enabled/endpoint discipline): client-credentials without a secret can
	// never authenticate.
	if cfg.ClientID != "" && cfg.ClientSecretFile == "" {
		return nil, fmt.Errorf("openadr: client_id is set but client_secret_file is empty")
	}
	return &cfg, nil
}

// Uncommissioned reports whether this config describes a unit with no VTN to
// poll (the factory/shipped state — configs/openadr.json ships vtn_url "").
// Same predicate discipline as cmd/telemetry's Uncommissioned: only the
// server field decides; a configured-but-broken secret file must keep
// failing loudly at startup, never be masked by this.
func (c *Config) Uncommissioned() bool { return c.VTNURL == "" }

// PollInterval returns the VTN poll cadence.
func (c *Config) PollInterval() time.Duration {
	return time.Duration(c.PollIntervalS) * time.Second
}

// ReportsEnabled reports whether report POSTs are on. nil/absent ⇒ true.
func (c *Config) ReportsEnabled() bool {
	return c.ReportEnabled == nil || *c.ReportEnabled
}

// loadSecret reads the OAuth2 client secret from secretFile, mirroring
// mqttutil.LoadPassword's contract: "" path ⇒ ("", nil); a configured but
// unreadable/empty file is a loud startup error. Additionally WARNs when the
// file is group/other-accessible — the deploy discipline is 0600 like
// /etc/lexa/mqtt/<svc>.pass (never fatal: permissions are the deploy
// script's job, and refusing to start over a 0640 would brick a working
// bench for a lint).
func loadSecret(secretFile string) (string, error) {
	if secretFile == "" {
		return "", nil
	}
	if fi, err := os.Stat(secretFile); err == nil && fi.Mode().Perm()&0o077 != 0 {
		log.Printf("lexa-openadr: client_secret_file %s is mode %o — expected 0600", secretFile, fi.Mode().Perm())
	}
	data, err := os.ReadFile(secretFile)
	if err != nil {
		return "", fmt.Errorf("openadr: read client_secret_file %s: %w", secretFile, err)
	}
	secret := strings.TrimSpace(string(data))
	if secret == "" {
		return "", fmt.Errorf("openadr: client_secret_file %s is configured but empty", secretFile)
	}
	return secret, nil
}

// warnUnknownKeys logs (but tolerates) any top-level key in raw not present
// on Config's JSON tags — architecture.md §3's "unknown keys warn-not-fail"
// rule, the same shape as cmd/hub's warnUnknownPlantKeys. A malformed object
// was already surfaced by the typed Unmarshal in loadConfig.
func warnUnknownKeys(raw []byte, dst any, path string) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	known := jsonTagSet(dst)
	for k := range m {
		if !known[k] {
			log.Printf("lexa-openadr config: %s: ignoring unknown key %q", path, k)
		}
	}
}

// jsonTagSet returns the set of wire keys (json tag names) on the struct dst
// points to, so unknown-key warnings track the schema without a hand-kept
// list (copied shape from cmd/hub/config.go).
func jsonTagSet(dst any) map[string]bool {
	t := reflect.TypeOf(dst)
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	set := make(map[string]bool, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		if idx := strings.IndexByte(tag, ','); idx >= 0 {
			tag = tag[:idx]
		}
		set[tag] = true
	}
	return set
}
