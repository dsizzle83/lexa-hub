package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"reflect"
	"strings"
	"time"

	"lexa-hub/internal/journal"
	"lexa-hub/internal/orchestrator"
	"lexa-hub/internal/orchestrator/constraint"
)

// DeviceConfig describes a device role and capacity for the orchestrator.
// The hub does not connect to Modbus directly; it reads measurements from MQTT.
type DeviceConfig struct {
	Name string  `json:"name"`
	Role string  `json:"role"`  // "inverter" | "battery" | "meter"
	MaxW float64 `json:"max_w"` // nameplate capacity (W)

	// Plant is the optional per-device physical-response block (TASK-057,
	// AD-007): ramp/latency/taper/lag parameters that will replace the
	// bench-calibrated optimizer globals. Absent block ⇒ nil ⇒ the orchestrator
	// uses its bench defaults. Decoded per Role into exactly one of the typed
	// fields below.
	Plant json.RawMessage `json:"plant,omitempty"`

	// Decoded plant model. loadConfig fills the field matching Role from Plant;
	// the others stay zero. UNUSED downstream until TASK-064 wires it — this is
	// the config half of the unwired vocabulary. Bench defaults are applied at
	// consume time (orchestrator withDefaults), not here, so a partial block
	// keeps its explicit values and legacy files stay byte-for-byte valid.
	InverterPlant orchestrator.InverterPlant `json:"-"`
	BatteryPlant  orchestrator.BatteryPlant  `json:"-"`
	MeterPlant    orchestrator.MeterPlant    `json:"-"`
}

// StationConfig describes an EV charging station known to the hub.
type StationConfig struct {
	ID          string  `json:"id"`
	MaxCurrentA float64 `json:"max_current_a"` // hardware limit (A); default 32

	// Plant is the optional EVSE plant block (TASK-057) — currently just the
	// OCPP MeterValues lag. Same rules as DeviceConfig.Plant.
	Plant     json.RawMessage        `json:"plant,omitempty"`
	EVSEPlant orchestrator.EVSEPlant `json:"-"` // decoded; unused until TASK-064
}

// GatewayConfig is hub.json's optional "gateway" block (Unit 3.4, §3.7): the
// operator's blanket EV-charging policy while the hub runs in gateway mode
// (a pure CSIP gateway has no cost-optimal EV opinion of its own). Its wire
// keys — evse_policy / evse_window{start_hh,end_hh} / evse_full_a — match
// DEVICE_ROADMAP §3.7; GatewayPolicy() maps them onto the constraint package's
// GatewayEVSEPolicy, which owns the actual defaulting (WithDefaults). An absent
// block ⇒ the constraint defaults (scheduled 23→7, 32 A). The window hours are
// read in the process's local zone (the GAP-05 provenance the TOU model and
// planner already depend on — see GatewayEVSEPolicy's doc).
type GatewayConfig struct {
	EVSEPolicy string `json:"evse_policy"` // "scheduled" | "full"
	EVSEWindow struct {
		StartHH int `json:"start_hh"` // local hour [0,23], inclusive
		EndHH   int `json:"end_hh"`   // local hour [0,23], exclusive; wraps midnight when start>end
	} `json:"evse_window"`
	EVSEFullA float64 `json:"evse_full_a"` // current ceiling (A) offered inside the window / at every hour in "full"
}

// GatewayPolicy maps the optional "gateway" block onto the constraint package's
// GatewayEVSEPolicy. An absent block ⇒ a zero policy that WithDefaults fills to
// the shipped defaults (scheduled 23→7, 32 A). WithDefaults is idempotent, so
// applying it here (and again inside NewCSIPPassthrough) is harmless and makes
// the returned policy self-consistent for any reader.
func (c *Config) GatewayPolicy() constraint.GatewayEVSEPolicy {
	if c.Gateway == nil {
		return constraint.GatewayEVSEPolicy{}.WithDefaults()
	}
	return constraint.GatewayEVSEPolicy{
		Mode:          c.Gateway.EVSEPolicy,
		WindowStartHH: c.Gateway.EVSEWindow.StartHH,
		WindowEndHH:   c.Gateway.EVSEWindow.EndHH,
		FullCurrentA:  c.Gateway.EVSEFullA,
	}.WithDefaults()
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

	// ConstraintShadow enables the observe-only constraint-stack shadow harness
	// (TASK-059): every economic tick runs the candidate constraint Stack
	// ALONGSIDE the authoritative DefaultOptimizer, diffs their final per-device
	// outputs, and logs/count divergences. The legacy cascade remains the SOLE
	// author of actuated plans — the candidate's plan is discarded. Default
	// false ⇒ zero behaviour change (the wrapper is not even constructed).
	ConstraintShadow bool `json:"constraint_shadow"`

	// Mode selects the live plan author (Unit 3.4, §3.5): "optimizer" (the
	// default — the cost-optimal DefaultOptimizer cascade) or "gateway" (a pure
	// CSIP passthrough that forwards utility control with no economic opinion).
	// Empty ⇒ "optimizer"; any other value is a FATAL config error (loadConfig),
	// never a silent fallback — a typo must not quietly leave the hub optimizing
	// when the operator asked for gateway, or vice-versa. A retained
	// lexa/hub/mode message takes precedence over this at boot (modeManager's
	// re-seed, §3.5): retained value ▸ this ▸ "optimizer".
	Mode string `json:"mode"`

	// Gateway is the optional gateway-mode EVSE policy block (§3.7); nil/absent
	// ⇒ the constraint package's defaults (scheduled 23→7, 32 A) via
	// GatewayPolicy()'s WithDefaults. Read only in gateway mode.
	Gateway *GatewayConfig `json:"gateway,omitempty"`

	// ConstraintModes is the per-constraint off|shadow|active map (FIX-F,
	// TASK-060 §4 / TASK-061 §4): keys "export", "gen", "import", "economics",
	// "battery_safety". Back-compat is ABSOLUTE (see ResolveConstraintModes for
	// the exact resolution this field feeds):
	//
	//   - absent/nil (every hub.json before FIX-F, and the common case after)
	//     with constraint_shadow true  ⇒ every key defaults to "shadow" —
	//     today's shadow-everything behaviour, bit-identical;
	//   - absent/nil with constraint_shadow false ⇒ irrelevant: the wrapper is
	//     not constructed at all regardless of this field;
	//   - present ⇒ a key it OMITS defaults to "off" (TASK-060 step 4's
	//     "off (default; stack has no export constraint)" — the per-constraint
	//     task docs' individual flip default, not the TASK-059-era
	//     shadow-everything one).
	//
	// "active" on ANY key requires constraint_shadow true (the wrapper is what
	// hosts composition) — ResolveConstraintModes fails loud otherwise (05 §6),
	// it never silently downgrades to shadow or off.
	ConstraintModes map[string]ConstraintMode `json:"constraint_modes,omitempty"`

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
	// Journal's rollout shape). Whenever Path is set, the hub WRITES a
	// snapshot on every breach begin/end transition and every 60 s while a
	// breach is open, independent of Enabled. Enabled additionally gates
	// RESTORE-ON-START (main.go, below): the shipped configs/hub.json ran
	// one full write-only soak campaign (TASK-041's "Implementation
	// strategy") — the 2026-07-08 8-cycle campaign passed hub-restart-mid-cap
	// with restore off — and now ships Enabled:true (WS-4.1, 2026-07-09);
	// restore never touches a device command path (main.go's TASK-041
	// restore block seeds only breachEpisodes' identity fields).
	Snapshot *SnapshotConfig `json:"snapshot,omitempty"`

	// TariffZone is an IANA time zone name (e.g. "America/Los_Angeles") that
	// the TOU tariff `TOUCostModel`/planner `localHourOf`/price-shaping
	// helpers (internal/orchestrator/costmodel.go, planner.go) are written
	// in local clock time for (TASK-079/WS-8, GAP-05; see CLAUDE.md "SOM
	// zone must match the tariff zone"). Those helpers key on t.Hour() in
	// WHATEVER zone the caller's time.Time carries — correct only when the
	// process's configured zone (time.Local, i.e. the SOM's /etc/localtime)
	// actually IS the tariff's zone. An empty TariffZone (the default)
	// leaves today's behavior unchanged (no assertion, just a startup WARN);
	// a non-empty value makes main() compare time.Local's offset behavior
	// against LoadLocation(TariffZone) across a year sample and, on
	// mismatch, log a LOUD error and set the lexa_tariff_zone_mismatch gauge
	// to 1 — an additive assertion only, it changes no control behavior.
	TariffZone string `json:"tariff_zone"`

	// RetainedAdoptionMaxAgeS bounds (in seconds) how old a retained
	// lexa/csip/control message's Ts may be at ADOPTION time before the hub
	// treats it as a stale-suspect resurrection (TASK-042, §8.3/GAP-01):
	// mosquitto's `autosave_interval 60` (systemd/mosquitto-lexa.conf) can
	// resurrect a control up to ~60 s stale after an unclean broker death,
	// and the hub adopts whatever it (re)subscribes to. A stale-suspect
	// control is still ENFORCED unchanged (enforce-but-verify, never
	// fail-open — rejecting a stale-but-decodable cap can only increase
	// export/import) — this only raises an edge-triggered alarm and asks
	// lexa-northbound (via lexa/csip/rewalk) to republish current truth
	// within seconds. 0/absent defaults to 300 in loadConfig: a handful of
	// discovery-walk periods even at the slowest realistic (STOCK) cadence,
	// comfortably above normal republish jitter and comfortably below "this
	// might be a resurrected corpse."
	RetainedAdoptionMaxAgeS int `json:"retained_adoption_max_age_s"`

	// LogEventMinIntervalS is the per-device-per-alarm-bit rate floor (in
	// seconds) on the WP-6 LogEvent edge detector (cmd/hub/logevent.go): a
	// chattering device's alarm-bit flapping must not turn every measurement
	// poll into a lexa/hub/logevent edge + journal line (flash budget,
	// docs/FLASH_BUDGET.md). A transition suppressed by the floor is
	// re-detected on the first measurement after it, so alarm/RTN pairs
	// complete late, never lost. 0/absent defaults to 10 in loadConfig
	// (architecture.md §3's table).
	LogEventMinIntervalS int `json:"logevent_min_interval_s"`
}

// ConstraintMode is one FIX-F constraint's per-axis operating mode.
type ConstraintMode string

const (
	// ModeOff: the constraint is not constructed into the candidate Stack at
	// all — the legacy cascade owns every axis it would have owned, exactly
	// as if FIX-F did not exist for that constraint.
	ModeOff ConstraintMode = "off"
	// ModeShadow: constructed into the candidate Stack and observed
	// (diffed against legacy) every tick, never actuated — TASK-059's
	// shadow harness, unchanged.
	ModeShadow ConstraintMode = "shadow"
	// ModeActive: constructed into the candidate Stack AND its Name() is in
	// the Wrapper's ActiveConstraints set, so the axes its demand wins each
	// tick are composed into the actuated plan (constraint/shadow.go compose).
	ModeActive ConstraintMode = "active"
)

// constraintKeys is the fixed FIX-F vocabulary: hub.json config key →
// constraint.Constraint.Name(). Every key but "battery_safety" matches its
// Name() verbatim; battery_safety/battery-safety differ only because the
// config key convention is snake_case and the constraint identity (also used
// as Demand.Source and the metrics axis vocabulary) is hyphenated.
var constraintKeys = map[string]string{
	"export":         "export",
	"gen":            "gen",
	"import":         "import",
	"economics":      "economics",
	"battery_safety": "battery-safety",
}

// ResolveConstraintModes validates cfg.ConstraintModes against constraintKeys
// and applies the back-compat defaults documented on the field, returning the
// effective mode for every config key (never a partial map — every one of the
// five keys is always present in the result). It fails loud (05 §6) on an
// unknown key, an unknown mode value, or "active" set without
// constraint_shadow — never silently coerces any of those into a safer mode.
func (c *Config) ResolveConstraintModes() (map[string]ConstraintMode, error) {
	out := make(map[string]ConstraintMode, len(constraintKeys))

	if c.ConstraintModes == nil {
		// Back-compat: constraint_shadow is the sole switch, as it was before
		// FIX-F. true ⇒ every key defaults to "shadow" (today's behaviour,
		// bit-identical); false ⇒ the values here are moot (cmd/hub never
		// constructs the wrapper), but "off" is the honest answer regardless.
		def := ModeOff
		if c.ConstraintShadow {
			def = ModeShadow
		}
		for key := range constraintKeys {
			out[key] = def
		}
		return out, nil
	}

	for key := range constraintKeys {
		out[key] = ModeOff // present map ⇒ an omitted key is "off", not "shadow"
	}
	for key, mode := range c.ConstraintModes {
		if _, known := constraintKeys[key]; !known {
			return nil, fmt.Errorf("constraint_modes: unknown key %q (want one of export|gen|import|economics|battery_safety)", key)
		}
		switch mode {
		case ModeOff, ModeShadow, ModeActive:
		default:
			return nil, fmt.Errorf("constraint_modes[%q]: unknown mode %q (want off|shadow|active)", key, mode)
		}
		out[key] = mode
	}
	for key, mode := range out {
		if mode == ModeActive && !c.ConstraintShadow {
			return nil, fmt.Errorf(`constraint_modes[%q]: "active" requires constraint_shadow=true (the wrapper hosts composition)`, key)
		}
	}
	return out, nil
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
	// Mode (Unit 3.4, §3.5): empty ⇒ "optimizer"; anything else unknown is a
	// FATAL config error, not a silent fallback — a typo'd mode must not quietly
	// leave the hub running the wrong plan author.
	if cfg.Mode == "" {
		cfg.Mode = "optimizer"
	}
	if cfg.Mode != "optimizer" && cfg.Mode != "gateway" {
		return nil, fmt.Errorf("invalid mode %q: want \"optimizer\" or \"gateway\"", cfg.Mode)
	}
	if cfg.RetainedAdoptionMaxAgeS <= 0 {
		cfg.RetainedAdoptionMaxAgeS = 300
	}
	if cfg.LogEventMinIntervalS <= 0 {
		cfg.LogEventMinIntervalS = 10
	}
	if err := decodePlantBlocks(&cfg); err != nil {
		return nil, err
	}
	// Fail loud at LOAD time (05 §6) on a malformed constraint_modes block —
	// an unknown key/value or "active" without constraint_shadow is an
	// authoring bug, not a degraded default. cmd/hub (main.go) calls
	// ResolveConstraintModes again to get the map for wiring; recomputing is
	// cheap and keeps this function's job to parsing/validation only.
	if _, err := cfg.ResolveConstraintModes(); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &cfg, nil
}

// decodePlantBlocks decodes each device/station's optional "plant" object into
// the typed plant field matching its role (TASK-057). Unknown keys are warned
// (05 §6) but never fail the load, so legacy hub.json files — which have no
// plant blocks at all — parse byte-for-byte unchanged. A genuine type mismatch
// inside a plant block IS an error (an authoring bug in a new-style file), not
// a silent default.
func decodePlantBlocks(cfg *Config) error {
	for i := range cfg.Devices {
		d := &cfg.Devices[i]
		if len(d.Plant) == 0 {
			continue
		}
		ctx := fmt.Sprintf("device %q (role %q)", d.Name, d.Role)
		var dst any
		switch d.Role {
		case "inverter":
			dst = &d.InverterPlant
		case "battery":
			dst = &d.BatteryPlant
		case "meter":
			dst = &d.MeterPlant
		default:
			log.Printf("lexa-hub config: %s: plant block on role with no plant model, ignored", ctx)
			continue
		}
		warnUnknownPlantKeys(d.Plant, dst, ctx)
		if err := json.Unmarshal(d.Plant, dst); err != nil {
			return fmt.Errorf("parse plant for %s: %w", ctx, err)
		}
	}
	for i := range cfg.Stations {
		s := &cfg.Stations[i]
		if len(s.Plant) == 0 {
			continue
		}
		ctx := fmt.Sprintf("station %q", s.ID)
		warnUnknownPlantKeys(s.Plant, &s.EVSEPlant, ctx)
		if err := json.Unmarshal(s.Plant, &s.EVSEPlant); err != nil {
			return fmt.Errorf("parse plant for %s: %w", ctx, err)
		}
	}
	return nil
}

// warnUnknownPlantKeys logs (but tolerates) any key in raw not present on the
// destination plant struct's JSON tags — the 05 §6 "unknown keys warn, never
// fail" rule. A malformed object is left for the typed Unmarshal to surface.
func warnUnknownPlantKeys(raw json.RawMessage, dst any, ctx string) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	known := jsonTagSet(dst)
	for k := range m {
		if !known[k] {
			log.Printf("lexa-hub config: %s: ignoring unknown plant key %q (05 §6)", ctx, k)
		}
	}
}

// jsonTagSet returns the set of wire keys (json tag names) on the struct dst
// points to, so unknown-key warnings track the schema without a hand-kept list.
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
		if name := strings.Split(tag, ",")[0]; name != "" {
			set[name] = true
		}
	}
	return set
}

func (c *Config) EngineInterval() time.Duration {
	return time.Duration(c.EngineIntervalS) * time.Second
}

func (c *Config) SafetyInterval() time.Duration {
	return time.Duration(c.SafetyIntervalS) * time.Second
}

// RetainedAdoptionMaxAge is RetainedAdoptionMaxAgeS as a time.Duration, for
// MQTTSystemReader.SetRetainedAdoptionMaxAge (TASK-042).
func (c *Config) RetainedAdoptionMaxAge() time.Duration {
	return time.Duration(c.RetainedAdoptionMaxAgeS) * time.Second
}

// LogEventMinInterval is LogEventMinIntervalS as a time.Duration, for
// newLogEventDetector (WP-6).
func (c *Config) LogEventMinInterval() time.Duration {
	return time.Duration(c.LogEventMinIntervalS) * time.Second
}
