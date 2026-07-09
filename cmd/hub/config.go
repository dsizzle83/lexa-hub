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
	if cfg.RetainedAdoptionMaxAgeS <= 0 {
		cfg.RetainedAdoptionMaxAgeS = 300
	}
	if err := decodePlantBlocks(&cfg); err != nil {
		return nil, err
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
