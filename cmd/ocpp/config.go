package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// StationConfig pre-configures a known charging station.
type StationConfig struct {
	ID          string  `json:"id"`
	MaxCurrentA float64 `json:"max_current_a"` // hardware limit (A); default 32
	VoltageV    float64 `json:"voltage_v"`     // supply voltage (V); default 230
}

// Config is the JSON configuration for lexa-ocpp.
type Config struct {
	MQTTBroker   string `json:"mqtt_broker"`
	MQTTClientID string `json:"mqtt_client_id"`

	// MQTTUser/MQTTPassFile: broker credentials (TASK-013/W7/AD-008); empty
	// MQTTUser ⇒ anonymous connect (staged-rollout default, see cmd/hub/config.go).
	MQTTUser     string `json:"mqtt_user"`
	MQTTPassFile string `json:"mqtt_pass_file"`

	// OCPP 2.0.1 CSMS WebSocket server (Security Profile 2: TLS + HTTP Basic
	// Auth — TASK-074, AD-008, 09 Security hard gate). PRODUCT DEFAULT is
	// profile 2 enabled: CertPath/KeyPath/BasicAuthUser/BasicAuthPass all set.
	// `ws://` (both fields empty) is a BENCH-ONLY fallback for dev/demo
	// convenience on the air-gapped 69.0.0.x LAN — never ship a product
	// config with these empty. See scripts/deploy-hub-pi.sh
	// --enable-ocpp-sp2 (csip-tls-test docs/BENCH.md has the bench runbook).
	//
	// WS-1 (V1.0 punch list, security fail-closed by default): loadConfig
	// below now REFUSES to start with any of CertPath/KeyPath/BasicAuthUser/
	// BasicAuthPass blank unless Bench is true or OCPP_PROFILE=bench is set
	// in the environment — see the Bench field doc and benchProfile(). This
	// closes the CLAUDE.md-vs-code contradiction: the invariant already
	// claimed SP2 was the product default; now the code enforces it.
	Port     int    `json:"port"`      // default 8887
	CertPath string `json:"cert_path"` // TLS cert; plain WS when empty (bench-only)
	KeyPath  string `json:"key_path"`

	// BasicAuthUser/BasicAuthPass: HTTP Basic Auth for the charging station
	// link. Ignored (no auth enforced) when BasicAuthUser is empty — that
	// state is bench-only, same as an empty CertPath/KeyPath above.
	BasicAuthUser string `json:"basic_auth_user"`
	BasicAuthPass string `json:"basic_auth_pass"`

	// Bench is the explicit escape hatch (WS-1) that lets lexa-ocpp start
	// with SP2 fields blank — plaintext ws://, no Basic Auth — on the
	// air-gapped 69.0.0.x bench LAN. The same effect is available without
	// editing the config via the OCPP_PROFILE=bench environment variable
	// (see benchProfile), which is what deploy-hub-pi.sh's plain (no
	// --enable-ocpp-sp2) path now sets so a fresh bench deploy keeps working
	// unchanged. PRODUCT deploys must leave this false/absent.
	Bench bool `json:"bench"`

	Stations []StationConfig `json:"stations"`

	// MetricsAddr is the Prometheus /metrics listen address (TASK-044).
	// Empty ⇒ default "127.0.0.1:9104" (product default: loopback-only); the
	// literal "off" disables the listener. See cmd/hub/config.go's
	// MetricsAddr doc for the bench-vs-product bind rationale (AD-008).
	MetricsAddr string `json:"metrics_addr"`

	// LogLevel selects the slog level ("debug"|"info"|"warn"|"error");
	// default "info" (TASK-045). See internal/logutil.ParseLevel.
	LogLevel string `json:"log_level"`

	// Reconciler selects the EVSE Device Reconciler mode (AD-002/AD-013):
	// "off" | "shadow" | "active", a scalar (exactly one class, evse). TASK-032
	// deleted the legacy lexa/evse/{station}/command write path, so with any
	// station configured the mode MUST be "active" — loadConfig rejects
	// off/shadow/empty because no legacy path remains to fall back to. "active"
	// makes the reconciler own SetChargingProfile writes with verify-by-readback
	// and reassert-on-reconnect.
	Reconciler string `json:"reconciler"`

	// MQTTDeafRestartAfterS bounds how long lexa-ocpp keeps kicking the
	// systemd watchdog while mc.IsConnected() reports true but the broker
	// connection has actually been down the whole time (WS-9.1):
	// mqtt.Client.IsConnected() stays true for paho's entire AutoReconnect
	// retry loop, not just while actually connected, so a naive kick gated
	// on it alone never trips during a sustained outage. A watchdog.DeafTracker
	// tracks continuous-disconnected time from mqttutil's OnConnectionLost/
	// OnReconnect hooks; once that exceeds this many seconds, the kick gate
	// stops firing and systemd's WatchdogSec restarts the service — matching
	// the WatchdogSec comment in systemd/lexa-ocpp.service. Default 300 (5
	// min) when unset/zero.
	MQTTDeafRestartAfterS int `json:"mqtt_deaf_restart_after_s"`
}

// Reconciler mode values.
const (
	ReconcilerOff    = "off"
	ReconcilerShadow = "shadow"
	ReconcilerActive = "active"
)

// ReconcilerMode returns the configured EVSE reconciler mode, defaulting to
// ReconcilerOff when empty. loadConfig has already rejected any other value.
func (c *Config) ReconcilerMode() string {
	if c.Reconciler == "" {
		return ReconcilerOff
	}
	return c.Reconciler
}

// MQTTDeafRestartAfter is MQTTDeafRestartAfterS as a time.Duration.
func (c *Config) MQTTDeafRestartAfter() time.Duration {
	return time.Duration(c.MQTTDeafRestartAfterS) * time.Second
}

// benchProfile reports whether loadConfig's SP2 fail-closed gate below
// should be relaxed (WS-1): either the config's explicit "bench": true, or
// the OCPP_PROFILE=bench environment variable. Two knobs because the config
// file is a deploy artifact (an operator/script flips it deliberately, as
// deploy-hub-pi.sh now does on its plain path) while the env var lets an
// ad-hoc local run (e.g. `go run ./cmd/ocpp`) opt out without editing or
// generating a config file at all.
func benchProfile(cfg *Config) bool {
	return cfg.Bench || os.Getenv("OCPP_PROFILE") == "bench"
}

// uncommissionedIdle reports whether cfg describes a valid UNCOMMISSIONED
// IDLE lexa-ocpp instance (Unit 6.1 amendment, 2026-07-09,
// docs/extension/00_PROGRESS.md "Scope amendments" / docs/DEVICE_ROADMAP.md
// §6/§9): no stations configured, and not an explicit bench profile.
//
// This is deliberately independent of the SP2 fields (CertPath/KeyPath/
// BasicAuthUser/BasicAuthPass): a config with zero stations but full SP2
// fields set is STILL idle — cert/auth paths alone don't imply any charger
// will ever dial in, so there is nothing to protect by refusing to start.
// main() checks this after loadConfig succeeds and, when true, never builds
// or starts the CSMS WS listener at all (no socket bound) — see main.go's
// top-level branch. loadConfig's own SP2 fail-closed gate below only
// refuses a config that would actually SERVE chargers (stations configured
// and not bench); it does not call this helper directly, but the two are
// consistent by construction: stations==0 && !bench never reaches the
// refusal branch (guarded by len(cfg.Stations) > 0), so it always loads,
// and main() then idles it.
func uncommissionedIdle(cfg *Config) bool {
	return len(cfg.Stations) == 0 && !benchProfile(cfg)
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
		cfg.MQTTClientID = "lexa-ocpp"
	}
	if cfg.Port == 0 {
		cfg.Port = 8887
	}
	if cfg.MetricsAddr == "" {
		cfg.MetricsAddr = "127.0.0.1:9104"
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.MQTTDeafRestartAfterS == 0 {
		cfg.MQTTDeafRestartAfterS = 300
	}
	for i := range cfg.Stations {
		if cfg.Stations[i].MaxCurrentA == 0 {
			cfg.Stations[i].MaxCurrentA = 32
		}
		if cfg.Stations[i].VoltageV == 0 {
			cfg.Stations[i].VoltageV = 230
		}
	}
	switch cfg.Reconciler {
	case "", ReconcilerOff, ReconcilerShadow, ReconcilerActive:
		// value syntax ok; migrated-class requirement checked below
	default:
		return nil, fmt.Errorf("reconciler: unknown mode %q (want off|shadow|active)", cfg.Reconciler)
	}
	// TASK-032: the legacy lexa/evse/{station}/command path was deleted, so EVSE
	// is reconciler-only. If stations are configured, the reconciler MUST be
	// "active": off/shadow (or empty) would leave no write path and silently
	// disable charging control — a pre-032 backup config must fail loud here.
	if len(cfg.Stations) > 0 && cfg.ReconcilerMode() != ReconcilerActive {
		return nil, fmt.Errorf("reconciler must be \"active\" (got %q): the legacy command path was deleted in TASK-032; off/shadow would silently disable EVSE actuation", cfg.ReconcilerMode())
	}
	// WS-1 (V1.0 punch list, security fail-closed by default): refuse to
	// start with OCPP Security Profile 2 disabled unless an explicit bench
	// profile opts out. Previously only the reconciler field fail-louded
	// here; SP2's blank fields silently fell back to plaintext ws:// with no
	// auth — the exact bench-only state CLAUDE.md already claimed was never
	// the product default. See the Bench field / benchProfile doc above.
	//
	// Unit 6.1 amendment (2026-07-09): this refusal only applies when the
	// config would actually SERVE chargers, i.e. one or more stations are
	// configured (the bench escape hatch is already handled by the outer
	// !benchProfile check). Zero stations + not bench is the uncommissioned-
	// idle state (uncommissionedIdle above) — main() never binds the CSMS
	// listener in that state, so there is no open ws:// surface to refuse
	// here; refusing anyway would make the fail-closed factory profile
	// (configs/factory/ocpp.json: SP2 blank, bench:false, stations: [])
	// unloadable, which defeats commissioning rather than protecting it.
	if !benchProfile(&cfg) && len(cfg.Stations) > 0 {
		var missing []string
		if cfg.CertPath == "" {
			missing = append(missing, "cert_path")
		}
		if cfg.KeyPath == "" {
			missing = append(missing, "key_path")
		}
		if cfg.BasicAuthUser == "" {
			missing = append(missing, "basic_auth_user")
		}
		if cfg.BasicAuthPass == "" {
			missing = append(missing, "basic_auth_pass")
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("ocpp: refusing to start with OCPP Security Profile 2 disabled (%s empty): the product default requires TLS + HTTP Basic Auth (TASK-074, 09 Security hard gate, WS-1) — set \"bench\": true in the config or OCPP_PROFILE=bench in the environment to run plaintext ws:// on the air-gapped bench LAN", strings.Join(missing, ", "))
		}
	}
	return &cfg, nil
}
