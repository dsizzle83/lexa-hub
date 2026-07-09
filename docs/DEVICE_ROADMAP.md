# LEXA Hub — Device-Only Software Roadmap (ecosystem interface work)

Companion to `docs/ECOSYSTEM_ROADMAP.md` (the ecosystem contract). This
document is the **device-side implementation contract**: every new on-box
component, the code seams it plugs into, and how it interacts with the cloud,
app, and bench. Prepared 2026-07-08 against `main` (post-TASK-064).

**Ground rule: this is an extension program, not a refactor.** Every item
below is additive — new packages, new services, new topics, new config keys,
new methods following existing idioms. Where an existing file is touched, the
touch is enumerated in §12 and is a bounded insertion (a new `case`, a new
subscribe block, a gate around an existing block), never a restructuring.
The authoritative control paths (legacy optimizer cascade, reconcilers,
Tier-0/Tier-1 safety, northbound compliance path) are **not modified**.

Contents:

1. New bus surface (topics, message types, versions, QoS, ACL)
2. `lexa-cloudlink` — the seventh service
3. `lexa-hub` additions — intents, planner inputs, mode manager, gateway author
4. `lexa-api` additions — HTTPS, REST, mDNS, commissioning config-write
5. `lexa-modbus` + `lexa-proto` — commissioning scan
6. `lexa-ocpp` — pending-station surface
7. `lexactl` — power-user CLI
8. OTA on-box hooks — `lexa-healthcheck`, config migration
9. Uncommissioned/factory mode + clock trust
10. Config additions summary
11. Task breakdown (TASK-082…099) with dependencies, phases, exit criteria
12. What changes where — and what explicitly does not change

---

## 1. New bus surface

### 1.1 Topic map additions

| Topic | Publisher | Subscribers | QoS | Retained |
|---|---|---|---|---|
| `lexa/intent/mode` | lexa-cloudlink, lexa-api | lexa-hub | 1 | **yes** |
| `lexa/intent/evgoal` | lexa-cloudlink, lexa-api | lexa-hub | 1 | **yes** |
| `lexa/intent/reserve` | lexa-cloudlink, lexa-api | lexa-hub | 1 | **yes** |
| `lexa/intent/tariff` | lexa-cloudlink, lexa-api | lexa-hub | 1 | **yes** |
| `lexa/intent/solarforecast` | lexa-cloudlink | lexa-hub | 1 | **yes** |
| `lexa/intent/loadprofile` | lexa-cloudlink | lexa-hub | 1 | **yes** |
| `lexa/intent/chargenow` | lexa-cloudlink, lexa-api | lexa-hub | 1 | no (edge, TTL) |
| `lexa/intent/result` | lexa-hub | lexa-cloudlink, lexa-api | 1 | no (edge) |
| `lexa/hub/mode` | lexa-hub | lexa-api, lexa-cloudlink | 1 | **yes** |
| `lexa/cloudlink/status` | lexa-cloudlink | lexa-api | 1 | **yes** |
| `lexa/scan/request` | lexa-api | lexa-modbus | 1 | no |
| `lexa/scan/status` | lexa-modbus | lexa-api | 1 | no |
| `lexa/scan/result` | lexa-modbus | lexa-api, lexa-cloudlink | 1 | **yes** |
| `lexa/ocpp/pending` | lexa-ocpp | lexa-api | 1 | **yes** |

Design rules, inherited from the AD-013/TASK-042 disciplines:

- **State-like intents are retained, one topic per kind** — the current goal
  state, exactly like `lexa/desired/{class}/{device}`. A restarting hub
  re-seeds every user/cloud goal from the broker; a broker reconnect
  redelivers them, so adoption is **ID-deduped** (harmless replays are
  filtered before journaling).
- **Edge intents (`chargenow`) are non-retained and carry a mandatory TTL** —
  a command stuck behind a WAN outage must not fire hours late.
- **No wildcard subscription on `lexa/intent/+`** anywhere: `lexa/intent/result`
  would match it. The hub subscribes to each kind topic explicitly (seven
  subscribe blocks — also what makes the ACL exact).
- All new topics default to QoS 1 via `bus.PubQoS`'s existing `default:` arm —
  **no `PubQoS` change needed**. Each new message family gets a version
  constant and a `SupportedV` case (§1.3).

### 1.2 Message types — `internal/bus/intent.go` (new file)

All types follow house rules: embed `Envelope` by value, `*float64` for
optional quantities (never NaN on the wire), `Finite() error` on every
float-bearing type (picked up automatically by `mqttutil.Subscribe`'s
type-assertion), no field colliding with the `"v"` JSON key.

```go
package bus

// IntentMeta is common to every intent kind. Origin/Actor make the journal
// audit trail meaningful; ID makes retained redelivery idempotent; TTLS is
// mandatory for edge kinds (chargenow) and ignored for state kinds.
type IntentMeta struct {
	ID       string `json:"id"`                 // caller-generated, unique
	Origin   string `json:"origin"`             // "cloud" | "app" | "cli"
	Actor    string `json:"actor,omitempty"`    // user email / token id / "root"
	IssuedAt int64  `json:"issued_at"`          // Unix seconds at the source
	TTLS     int    `json:"ttl_s,omitempty"`    // edge intents only
}

// ModeIntent — lexa/intent/mode (retained).
type ModeIntent struct {
	Envelope
	IntentMeta
	Mode string `json:"mode"` // "optimizer" | "gateway"
}

// EVGoalIntent — lexa/intent/evgoal (retained). kWh terms: the app/cloud
// resolves per-weekday defaults and %→kWh before publishing; the hub stays
// unit-simple (PlannerParams already works in kWh).
type EVGoalIntent struct {
	Envelope
	IntentMeta
	StationID     string   `json:"station_id,omitempty"` // empty = the single/default station
	TargetSocKwh  *float64 `json:"target_soc_kwh"`
	DepartureUnix int64    `json:"departure_unix"`
	InitialSocKwh *float64 `json:"initial_soc_kwh,omitempty"` // user estimate at plug-in ("estimated" in UI)
	CapacityKwh   *float64 `json:"capacity_kwh,omitempty"`    // user-stated vehicle pack size
}

// BackupReserveIntent — lexa/intent/reserve (retained). The hub clamps to
// >= the configured safety floor; intents can only RAISE the reserve.
type BackupReserveIntent struct {
	Envelope
	IntentMeta
	ReservePct *float64 `json:"reserve_pct"`
}

// SolarForecastIntent — lexa/intent/solarforecast (retained). StepKw is on
// the planner's 5-min grid starting at WindowStart (<=288 entries; shorter
// is zero-filled by the planner, same rule as SolarForecastKw today).
type SolarForecastIntent struct {
	Envelope
	IntentMeta
	WindowStart int64     `json:"window_start"` // Unix seconds, 5-min aligned
	StepKw      []float64 `json:"step_kw"`
	SourceTs    int64     `json:"source_ts"` // when the weather model ran (staleness input)
}

// LoadProfileIntent — lexa/intent/loadprofile (retained). Same grid rules.
type LoadProfileIntent struct {
	Envelope
	IntentMeta
	StepKw []float64 `json:"step_kw"`
}

// TariffIntent — lexa/intent/tariff (retained). Compiled on the hub into a
// TOUCostModel; CSIP-published pricing (SetPrices arrays) still wins when
// present, by the planner's existing nil-slice fallback rule.
type TariffIntent struct {
	Envelope
	IntentMeta
	Tariff TariffSpec `json:"tariff"`
}

type TariffSpec struct {
	Currency string         `json:"currency"` // "USD"
	Periods  []TariffPeriod `json:"periods"`
}

type TariffPeriod struct {
	Label        string   `json:"label"`      // "peak", "off-peak", …
	Days         []int    `json:"days"`       // 0=Sun … 6=Sat
	StartHH      int      `json:"start_hh"`   // local tariff-zone hour, inclusive
	EndHH        int      `json:"end_hh"`     // exclusive
	ImportPerKwh float64  `json:"import_per_kwh"`
	ExportPerKwh *float64 `json:"export_per_kwh,omitempty"`
}

// ChargeNowIntent — lexa/intent/chargenow (NOT retained; TTLS mandatory).
type ChargeNowIntent struct {
	Envelope
	IntentMeta
	StationID string `json:"station_id,omitempty"`
}

// IntentResult — lexa/intent/result (not retained). One per received intent.
type IntentResult struct {
	Envelope
	ID      string `json:"id"`      // echoes IntentMeta.ID
	Kind    string `json:"kind"`    // "mode" | "evgoal" | …
	Outcome string `json:"outcome"` // "applied" | "clamped" | "rejected" | "expired" | "duplicate"
	Detail  string `json:"detail,omitempty"`
	Ts      int64  `json:"ts"`
}

// ModeStatus — lexa/hub/mode (retained). Authoritative mode state; also the
// hub's own restart re-seed (subscribe-own-retained, like breach snapshots).
type ModeStatus struct {
	Envelope
	Mode     string `json:"mode"`
	Since    int64  `json:"since"`
	Actor    string `json:"actor,omitempty"`
	IntentID string `json:"intent_id,omitempty"`
	Ts       int64  `json:"ts"`
}

// CloudlinkStatus — lexa/cloudlink/status (retained). Folded into lexa-api's
// /status as "cloud_link" and uplinked as part of the health stream.
type CloudlinkStatus struct {
	Envelope
	Connected     bool   `json:"connected"`
	Endpoint      string `json:"endpoint,omitempty"`
	SpoolBytes    int64  `json:"spool_bytes"`
	SpoolOldestTs int64  `json:"spool_oldest_ts,omitempty"`
	LastUplinkTs  int64  `json:"last_uplink_ts,omitempty"`
	CertDaysLeft  int    `json:"cert_days_left,omitempty"`
	Ts            int64  `json:"ts"`
}
```

Scan + pending types (same file or `internal/bus/scan.go`):

```go
// ScanRequest — lexa/scan/request. Honored by lexa-modbus ONLY when
// uncommissioned (see §5.2); otherwise answered with a refused ScanStatus.
type ScanRequest struct {
	Envelope
	ID      string   `json:"id"`
	TCPCidr string   `json:"tcp_cidr,omitempty"`  // e.g. "192.168.1.0/24"; empty = local /24
	TCPPort int      `json:"tcp_port,omitempty"`  // default 502
	RTUDev  string   `json:"rtu_dev,omitempty"`   // e.g. "/dev/ttyUSB0"; empty = skip RTU
	Bauds   []int    `json:"bauds,omitempty"`     // default {9600, 19200}
	UnitIDs []uint8  `json:"unit_ids,omitempty"`  // default 1..247 (RTU), {1,2,3,126} (TCP)
	Ts      int64    `json:"ts"`
}

// ScanStatus — lexa/scan/status. Progress lines while a sweep runs.
type ScanStatus struct {
	Envelope
	ID      string `json:"id"`
	Phase   string `json:"phase"` // "refused" | "tcp" | "rtu" | "identify" | "done"
	Probed  int    `json:"probed"`
	Found   int    `json:"found"`
	Detail  string `json:"detail,omitempty"`
	Ts      int64  `json:"ts"`
}

// ScanResult — lexa/scan/result (retained until commissioning completes).
type ScanResult struct {
	Envelope
	ID      string     `json:"id"`
	Devices []ScanHit  `json:"devices"`
	Ts      int64      `json:"ts"`
}

type ScanHit struct {
	URL          string   `json:"url"`     // "tcp://192.168.1.40:502" | "rtu:///dev/ttyUSB0"
	UnitID       uint8    `json:"unit_id"`
	Manufacturer string   `json:"manufacturer,omitempty"` // SunSpec model 1
	Model        string   `json:"model,omitempty"`
	Serial       string   `json:"serial,omitempty"`
	FwVersion    string   `json:"fw_version,omitempty"`
	Class        string   `json:"class"`             // "inverter"|"battery"|"meter"|"unknown-sunspec"|"unknown-modbus"
	Models       []uint16 `json:"models,omitempty"`  // SunSpec model IDs present
	NameplateW   *float64 `json:"nameplate_w,omitempty"`
}

// PendingStations — lexa/ocpp/pending (retained). Unknown chargers that
// dialed the CSMS; surfaced for installer approval instead of silent adoption.
type PendingStations struct {
	Envelope
	Stations []PendingStation `json:"stations"`
	Ts       int64            `json:"ts"`
}

type PendingStation struct {
	StationID   string `json:"station_id"`
	Vendor      string `json:"vendor,omitempty"`   // from BootNotification if seen
	ModelName   string `json:"model,omitempty"`
	FirstSeenTs int64  `json:"first_seen_ts"`
	RemoteAddr  string `json:"remote_addr,omitempty"`
}
```

`Finite()` implementations (in `internal/bus/finite.go`, following the
existing `finite`/`finiteVal` helpers) for: `EVGoalIntent`,
`BackupReserveIntent`, `SolarForecastIntent` (loop `StepKw` with
`finiteVal`), `LoadProfileIntent`, `TariffIntent` (loop period rates),
`ScanResult` (nameplates).

### 1.3 Versions + topic constants

`internal/bus/envelope.go` — append to the existing const block (all born at 1):

```go
	ModeIntentV          = 1 // lexa/intent/mode
	EVGoalIntentV        = 1 // lexa/intent/evgoal
	BackupReserveIntentV = 1 // lexa/intent/reserve
	TariffIntentV        = 1 // lexa/intent/tariff
	SolarForecastIntentV = 1 // lexa/intent/solarforecast
	LoadProfileIntentV   = 1 // lexa/intent/loadprofile
	ChargeNowIntentV     = 1 // lexa/intent/chargenow
	IntentResultV        = 1 // lexa/intent/result
	ModeStatusV          = 1 // lexa/hub/mode
	CloudlinkStatusV     = 1 // lexa/cloudlink/status
	ScanRequestV         = 1 // lexa/scan/request
	ScanStatusV          = 1 // lexa/scan/status
	ScanResultV          = 1 // lexa/scan/result
	PendingStationsV     = 1 // lexa/ocpp/pending
```

`internal/bus/topics.go` — new constants (`TopicIntentMode = "lexa/intent/mode"`,
… , `TopicHubMode`, `TopicCloudlinkStatus`, `TopicScanRequest/Status/Result`,
`TopicOCPPPending`) plus one `IntentTopic(kind string)` builder and the
matching cases in `SupportedV` (a `strings.HasPrefix(topic, "lexa/intent/")`
arm dispatching per kind, plus the singles). **No `PubQoS` change** — QoS 1
default is correct for all of them.

### 1.4 ACL delta — `systemd/mosquitto-lexa.acl`

Derived from the publish/subscribe call sites specified in this document
(re-verify against code at review time, per the ACL-is-an-authorization-
boundary rule):

```
user lexa-cloudlink
topic read  lexa/measurements/+
topic read  lexa/battery/+/metrics
topic read  lexa/evse/+/state
topic read  lexa/hub/plan
topic read  lexa/hub/mode
topic read  lexa/csip/compliance/alert
topic read  lexa/reconcile/+/+/report
topic read  lexa/northbound/certstatus
topic read  lexa/intent/result
topic read  lexa/scan/result
topic write lexa/intent/mode
topic write lexa/intent/evgoal
topic write lexa/intent/reserve
topic write lexa/intent/tariff
topic write lexa/intent/solarforecast
topic write lexa/intent/loadprofile
topic write lexa/intent/chargenow
topic write lexa/cloudlink/status

# lexa-api additions (existing read grants unchanged)
topic write lexa/intent/mode
topic write lexa/intent/evgoal
topic write lexa/intent/reserve
topic write lexa/intent/tariff
topic write lexa/intent/chargenow
topic write lexa/scan/request
topic read  lexa/intent/result
topic read  lexa/hub/mode
topic read  lexa/cloudlink/status
topic read  lexa/scan/status
topic read  lexa/scan/result
topic read  lexa/ocpp/pending

# lexa-hub additions
topic read  lexa/intent/mode
topic read  lexa/intent/evgoal
topic read  lexa/intent/reserve
topic read  lexa/intent/tariff
topic read  lexa/intent/solarforecast
topic read  lexa/intent/loadprofile
topic read  lexa/intent/chargenow
topic write lexa/intent/result
topic write lexa/hub/mode

# lexa-modbus additions
topic read  lexa/scan/request
topic write lexa/scan/status
topic write lexa/scan/result

# lexa-ocpp additions
topic write lexa/ocpp/pending
```

Note the deliberate asymmetry: **lexa-api cannot write `solarforecast` or
`loadprofile`** (cloud-computed data, no local source), and **nobody but the
hub writes `lexa/intent/result` or `lexa/hub/mode`**.

### 1.5 Port map delta

| Service | Metrics |
|---|---|
| lexa-cloudlink | `127.0.0.1:9106` (config `metrics_addr`, `"off"` disables — house convention) |

lexa-api keeps `/metrics` as a route on `:9100` (no separate port today; unchanged).

---

## 2. `lexa-cloudlink` — the seventh service

Pure Go, `CGO_ENABLED=0`, standard `crypto/tls` (mTLS to the cloud broker;
CCM-8 is a CSIP requirement, not a cloud one). Peer of the existing six in
every discipline: own broker user, `Type=notify`, `WatchdogSec=60`, journald
rate caps, `StateDirectory=lexa`, `/metrics`, journal under
`/var/lib/lexa/journal/cloudlink`.

### 2.1 Package layout

```
cmd/cloudlink/
  main.go        wiring (config → logutil → journal → metrics → local MQTT →
                  spool → cloud session → collectors → downlink → watchdog)
  config.go      Config + loadConfig (house pattern, defaults, validation)
  uplink.go      collectors: local-bus subscribe set → stream framing → spool
  batch.go       batcher: spool drain → gzip frame ≤ 96 KiB → cloud publish
  downlink.go    cloud cmd topic → validation chain → lexa/intent/{kind}
  cloud.go       cloud MQTT session (paho over crypto/tls, mTLS, reconnect)
  certmon.go     cloud-cert expiry monitor (pattern copy of cmd/northbound/certmon.go)
  diag.go        diag-bundle command: tar journals → presigned-URL upload
internal/spool/
  spool.go       disk-backed FIFO with byte budget + priority classes
  spool_test.go
```

`internal/spool` is a leaf package (stdlib only), like `internal/metrics`.

### 2.2 Config — `/etc/lexa/cloudlink.json` (`configs/cloudlink.json`)

```json
{
  "enabled": true,
  "endpoint": "ssl://a1b2c3-ats.iot.us-west-2.amazonaws.com:8883",
  "serial_file": "/etc/lexa/identity/serial",
  "cloud_ca": "/etc/lexa/identity/cloud-ca.pem",
  "cloud_cert": "/etc/lexa/identity/cloud-cert.pem",
  "cloud_key": "/etc/lexa/identity/cloud-key.pem",
  "cert_expiry_warn_days": 30,

  "spool_dir": "/var/lib/lexa/spool",
  "spool_max_bytes": 33554432,

  "uplink": {
    "measurements_batch_s": 60,
    "evse_batch_s": 60,
    "plan_interval_s": 300,
    "health_interval_s": 900
  },

  "mqtt_broker": "tcp://localhost:1883",
  "mqtt_client_id": "lexa-cloudlink",
  "mqtt_user": "lexa-cloudlink",
  "mqtt_pass_file": "/etc/lexa/mqtt/cloudlink.pass",
  "metrics_addr": "",
  "log_level": "info",
  "journal": { "dir": "/var/lib/lexa/journal/cloudlink" }
}
```

`"enabled": false` ⇒ the service starts, publishes a retained
`CloudlinkStatus{Connected:false, Endpoint:""}`, and idles (watchdog still
kicked) — the local-only box is a first-class configuration, and the unit
does not flap in systemd.

### 2.3 `main()` skeleton

Follows `cmd/telemetry/main.go`'s shape exactly:

```go
func main() {
	cfgPath := flag.String("config", "/etc/lexa/cloudlink.json", "path to JSON config")
	flag.Parse()
	cfg, err := loadConfig(*cfgPath)
	if err != nil { log.Fatalf("lexa-cloudlink: load config: %v", err) }
	logutil.Setup("lexa-cloudlink", logutil.ParseLevel(cfg.LogLevel))

	jw, err := journal.Open(cfg.Journal.ToLibrary())          // AD-005 pattern
	if err != nil { log.Fatalf("lexa-cloudlink: journal: %v", err) }
	defer jw.Close()

	reg := metrics.New()
	metrics.StandardGauges(reg)
	m := newCloudlinkMetrics(reg)                              // §2.9 list
	reg.Collect(busDecodeFailureCollector)                     // house idiom

	mqttPass, err := mqttutil.LoadPassword(cfg.MQTTPassFile)
	if err != nil { log.Fatalf("lexa-cloudlink: mqtt pass: %v", err) }
	mc, err := mqttutil.ConnectAuthInstrumented(cfg.MQTTBroker, cfg.MQTTClientID,
		cfg.MQTTUser, mqttPass, mqttutil.Instrumentation{
			OnPublishFail: m.localPubFail.Inc, OnReconnect: m.localReconn.Inc})
	if err != nil { log.Fatalf("lexa-cloudlink: mqtt: %v", err) }
	defer mc.Disconnect(500)
	metrics.Serve(cfg.MetricsAddr, reg)

	sp, err := spool.Open(cfg.SpoolDir, cfg.SpoolMaxBytes, m.spoolMetrics())
	if err != nil { log.Fatalf("lexa-cloudlink: spool: %v", err) }

	ctx, cancel := context.WithCancel(context.Background())
	cloud := newCloudSession(cfg, m)     // nil-safe when !cfg.Enabled
	up := newUplink(mc, sp, cfg, m)      // local subscribes → spool (§2.4)
	up.subscribeAll()
	go newBatcher(sp, cloud, cfg, m).run(ctx)       // spool → cloud (§2.4)
	go runDownlink(ctx, cloud, mc, jw, cfg, m)      // cloud → intents (§2.6)
	go newCloudCertMon(mc, cfg, reg).Run(ctx, 24*time.Hour) // §2.7
	go statusPublisher(ctx, mc, cloud, sp, cfg)     // retained CloudlinkStatus

	watchdog.Ready()
	// Kick ticker in the same select as the status/health loop, gated on
	// mc.IsConnected() AND sp.Healthy() — the ocpp/api probe pattern.
	...
}
```

### 2.4 Uplink: collectors, streams, batcher

The collector subscribes to the §1.4 read set via `mqttutil.Subscribe` (which
gives version-gate + `Finite()` for free) and **re-frames** each message as a
spool record — it never interprets payloads beyond stamping stream + arrival
time. Streams and priority classes:

| Stream | Source topics | Priority | Drain framing |
|---|---|---|---|
| `events` | `lexa/csip/compliance/alert`, `lexa/reconcile/+/+/report`, `lexa/intent/result`, `lexa/hub/mode` | **P0** (drains first, never dropped while any P1 remains) | immediate, one frame per batch tick |
| `health` | `lexa/northbound/certstatus`, `lexa/cloudlink/status`, plan-heartbeat summary, service versions | P1 | every `health_interval_s` |
| `plan` | `lexa/hub/plan` | P1 | every `plan_interval_s` + on change |
| `telemetry` | `lexa/measurements/+`, `lexa/battery/+/metrics`, `lexa/evse/+/state` | P2 (dropped-oldest first under spool pressure) | gzip batch every `measurements_batch_s` |

Batcher contract:

- A batch frame is `{"v":1,"serial":…,"stream":…,"seq":…,"count":N,"gz":<base64 or raw bytes via payload>}`
  published to `lexa/v1/{serial}/telemetry` (P2) / `lexa/v1/{serial}/events`
  (P0/P1), QoS 1, **≤ 96 KiB compressed** (headroom under IoT Core's 128 KiB
  cap). Oversized batches split.
- Publish success (PUBACK) ⇒ `spool.Commit`; failure/timeout ⇒ frames stay
  spooled. **At-least-once with per-frame `seq`** — the cloud ingest dedupes
  on `(serial, stream, seq)`; on-box we never invent exactly-once.
- WAN outage: collectors keep spooling; drop-oldest applies **within P2
  first**, then P1; P0 events are only dropped when they alone exceed the
  entire budget (counted, alarmed).

### 2.5 `internal/spool` — disk-backed FIFO with byte budget

Flash-aware by design (FLASH_BUDGET.md): appends are buffered, fsync happens
on segment close and on a 5 s interval, never per-record; the budget bounds
total on-disk bytes regardless of outage length.

```go
package spool

type Record struct {
	Stream   string // "events" | "health" | "plan" | "telemetry"
	Priority int    // 0 highest
	Ts       int64
	Payload  []byte
}

type Metrics struct { // *metrics.Counter / *metrics.Gauge, nil-safe like journal.Metrics
	Bytes, Drops, Appends, Commits any
}

func Open(dir string, maxBytes int64, m *Metrics) (*Spool, error)

// Append adds a record. If the budget would be exceeded it evicts whole
// oldest segments of the LOWEST-priority stream first (drop-oldest), counts
// the eviction, and never blocks the caller on fsync.
func (s *Spool) Append(r Record) error

// Peek returns up to max records of the highest-priority non-empty class,
// oldest first, without consuming them.
func (s *Spool) Peek(max int, maxBytes int) ([]Record, error)

// Commit consumes the last Peek'd records (crash between Peek and Commit ⇒
// redelivery; see at-least-once contract).
func (s *Spool) Commit(n int) error

func (s *Spool) Bytes() int64
func (s *Spool) OldestTs() int64
func (s *Spool) Healthy() bool // dir writable + budget accounting consistent
```

On-disk: per-priority subdirs (`p0/ p1/ p2/`), segment files
`seg-<seq>.log` of ≤256 KiB, records length-prefixed
(`[u32 len][u8 prio][u64 ts][u16 streamlen][stream][payload]`), a small
`cursor` file for the consumed offset (rewritten atomically via rename —
journal's rotation discipline). Torn final records are tolerated on open
(journal reader's discipline).

### 2.6 Downlink: cloud command → intent

The single choke point where the cloud's write authority is narrowed to the
intent vocabulary:

```go
// downlink.go — subscribed to lexa/v1/{serial}/cmd on the CLOUD session.
func handleCloudCmd(payload []byte, mc mqtt.Client, jw *journal.Writer, m *clMetrics) {
	var env struct {
		V    int             `json:"v"`
		Kind string          `json:"kind"`
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(payload, &env); err != nil { reject(m, "malformed", err); return }
	if env.V < 1 || env.V > cloudCmdV { reject(m, "version", nil); return }

	spec, ok := intentKinds[env.Kind] // kind → {topic, retained, decode func}
	if !ok { reject(m, "unknown-kind", nil); return }

	intent, err := spec.decode(env.Body) // json.Unmarshal into the typed bus struct
	if err != nil { reject(m, "decode", err); return }
	if fv, ok := intent.(interface{ Finite() error }); ok {
		if err := fv.Finite(); err != nil { reject(m, "non-finite", err); return }
	}
	meta := spec.meta(intent)
	if meta.Origin != "cloud" { reject(m, "origin-forgery", nil); return }
	if spec.edge && expired(meta, time.Now()) { reject(m, "expired", nil); return }
	if !rateLimiter.Allow(env.Kind) { reject(m, "rate-limited", nil); return }

	jw.Append(journal.NewIntentReceivedEvent("cloudlink", env.Kind, meta)) // audit BEFORE effect
	if err := mqttutil.PublishJSONTimeout(mc, spec.topic, spec.retained, intent, 2*time.Second); err != nil {
		m.intentPubFail.Inc(); return
	}
	m.intentForwarded.Inc()
}
```

Notes:
- Transport authenticity is the mTLS session + IoT Core policy (only the
  command service may publish to `…/cmd`). A signed-intent envelope (cloud
  command key verified on-box) is a documented v2 hardening, kept out of v1
  to avoid a hand-rolled crypto protocol.
- Cloudlink **never** publishes desired docs, engine commands, or any
  existing topic — the ACL enforces this mechanically (§1.4).
- The hub separately validates every intent again at adoption (§3.1):
  defense in depth, and the local-API path shares that validation.

### 2.7 Cloud cert monitor

Pattern copy of `cmd/northbound/certmon.go` (the report recommends copying
the file, changing paths/topic/gauges): inspects `cloud_cert`/`cloud_ca` at
startup + every 24 h, folds `days_left` into `CloudlinkStatus`
(`cert_days_left`), gauges `lexa_cloudlink_cert_expiry_seconds` /
`lexa_cloudlink_cert_expiring`. Rotation v1 is OTA-delivered certs +
service restart (documented); a `Reload` seam mirroring
`tlsclient.WolfSSLFetcher` is v2.

### 2.8 Diag bundle

A `diag` cloud command (not an intent — it never touches the hub):
`{kind:"diag", body:{upload_url:"https://s3…presigned", include:["journal","snapshot","config"]}}`.
Cloudlink tars `/var/lib/lexa/journal/*`, `/var/lib/lexa/snapshot/*`, and
`/etc/lexa/*.json` **with `*_pass*`/secret file contents redacted and cert
keys excluded**, streams it to the presigned URL (bounded size, bounded
time), and journals the request+outcome. Homeowner consent is enforced
cloud-side (the command service refuses to mint the URL without it) and
recorded on-box in the journal line.

### 2.9 Metrics, watchdog, unit

Metrics (flat names — `internal/metrics` has no labels):
`lexa_cloudlink_connected` (gauge 0/1), `lexa_cloudlink_spool_bytes`,
`lexa_cloudlink_spool_drops_total`, `lexa_cloudlink_uplink_frames_total`,
`lexa_cloudlink_uplink_fail_total`, `lexa_cloudlink_intents_forwarded_total`,
`lexa_cloudlink_intents_rejected_total`, `lexa_cloudlink_cert_expiry_seconds`,
plus the standard `lexa_mqtt_publish_failures_total` /
`lexa_mqtt_reconnects_total` pair on the local session.

`systemd/lexa-cloudlink.service`: copy of the telemetry template —
`Type=notify`, `NotifyAccess=main`, `WatchdogSec=60`,
`Wants=mosquitto.service` (never `Requires` — FINDING A), `StartLimit*` in
`[Unit]`, `StateDirectory=lexa`, `LogRateLimitIntervalSec=30`/`Burst=150`,
`ReadOnlyPaths=/etc/lexa/identity`, plus `After=time-sync.target` (§9).

### 2.10 Tests + bench scenarios

Unit: spool budget/eviction/priority/torn-record; batcher split at 96 KiB;
downlink rejection table (malformed / version / unknown kind / non-finite /
expired / origin-forgery / rate-limit); at-least-once redelivery on
crash-between-Peek-and-Commit.

Mayhem (bench repo, paired session): `wan-outage-spool-drain` (7-day outage
compressed: P2 evicted, P0 intact, clean oldest-first drain, **zero
safety/compliance deltas**), `cloud-cmd-forgery-rejected` (qa-inject
publishes forged frames on the cloud path analog), `stale-intent-expired`
(chargenow held past TTL never fires).

---

## 3. `lexa-hub` additions

### 3.1 Intent adoption layer — `cmd/hub/intent.go` (new file)

One adopter component, wired in `main.go` alongside the existing subscribe
blocks (the `derConstraintsFromSchedule` / `pricesFromPricingUpdate`
converter idiom):

```go
type intentAdopter struct {
	mu      sync.Mutex
	eng     *orchestrator.Engine
	modes   *modeManager          // §3.5
	jw      *journal.Writer
	mc      mqtt.Client
	cfg     *Config
	lastID  map[string]string     // kind → last applied intent ID (retained-redelivery dedupe)
	chargeNowRevert *time.Timer   // restores the standing EV goal at TTL expiry
}

// adopt is the single entry point every kind handler funnels through.
func (a *intentAdopter) adopt(kind string, meta bus.IntentMeta, apply func() (outcome, detail string)) {
	a.mu.Lock(); defer a.mu.Unlock()
	if a.lastID[kind] == meta.ID && meta.ID != "" {
		a.result(kind, meta, "duplicate", "")   // retained redelivery — no journal spam
		return
	}
	outcome, detail := apply()
	a.lastID[kind] = meta.ID
	a.journal(kind, meta, outcome, detail)      // intent_applied / intent_rejected
	a.result(kind, meta, outcome, detail)       // → lexa/intent/result (QoS1, 1s bound)
}
```

Per-kind subscribe blocks in `main.go` (seven explicit `mqttutil.Subscribe`
calls — the version gate and `Finite()` come free):

| Kind | Validation at adoption | Engine effect |
|---|---|---|
| `mode` | value ∈ {optimizer, gateway} | `modes.request(mode, meta)` → §3.5 transition |
| `evgoal` | `TargetSocKwh` ≥ 0 and ≤ stated capacity; **reject if `DepartureUnix` < now** ("expired") | `eng.SetEVGoal(...)` |
| `reserve` | 0–100; **clamped up to the configured safety floor** (`"clamped"` outcome when raised) | `eng.SetBackupReserve(pct)` |
| `tariff` | ≥1 period; rates ≥ 0; hours 0–24; days 0–6 | compile → `TOUCostModel` → `eng.SetFallbackTOU(m)` + `opt.SwapCostModel(m)` (§3.4) |
| `solarforecast` | len ≤ 288; each step ≥ 0 and ≤ 1.5 × Σ solar `max_w` (clamped, counted); `WindowStart` 5-min aligned | `eng.SetSolarForecast(...)` |
| `loadprofile` | len ≤ 288; steps ≥ 0, ≤ site plausibility bound | `eng.SetLoadProfile(...)` |
| `chargenow` | `TTLS` > 0 mandatory; `IssuedAt+TTLS` ≥ now else "expired" | `eng.SetEVGoal(full-by = now + horizon)` + revert timer restores the standing goal at TTL end |

Retained-adoption staleness follows the TASK-042 discipline: a retained
`solarforecast` older than `forecast_max_age_s` is still **adopted** (the
engine's own age gate decides diurnal fallback at planning time, §3.3) but
the adopter raises the edge-triggered stale alarm; a retained `evgoal` whose
departure has passed is dropped with `"expired"` — never silently.

### 3.2 New engine setters — `internal/orchestrator/engine.go`

Exact copies of the `SetPrices`/`SetDERConstraints` idiom
(engine.go:145–170): enqueue a closure onto `cmdCh` (cap 16, drop-and-count),
read-modify-write the atomic `plannerInput` snapshot, poke `plannerWake`.

```go
// plannerInput (engine.go:16) gains intent-fed fields; nil = not set.
type plannerInput struct {
	derConstraints []StepConstraint
	importPrices   []float64
	exportPrices   []float64

	evGoal        *EVGoal            // TASK-083
	reservePct    *float64           // TASK-083
	solarForecast *ExternalForecast  // TASK-083
	loadProfileKw []float64          // TASK-083
	fallbackTOU   *TOUCostModel      // TASK-094
}

type EVGoal struct {
	TargetSocKwh  float64
	DepartureUnix int64
	InitialSocKwh float64 // <0 = not stated
}

type ExternalForecast struct {
	StepKw       []float64
	WindowStart  int64
	ReceivedUnix int64 // ARRIVAL time on this box (clock-warp-safe staleness, like planHeartbeat)
}

func (e *Engine) SetEVGoal(g EVGoal) {
	e.enqueue(func(s *engineState) { s.setPlanIn(func(in *plannerInput) { in.evGoal = &g }) })
	e.signalReplan()
}

func (e *Engine) SetBackupReserve(pct float64) {
	e.enqueue(func(s *engineState) { s.setPlanIn(func(in *plannerInput) { in.reservePct = &pct }) })
	e.signalReplan()
}

func (e *Engine) SetSolarForecast(f ExternalForecast) {
	e.enqueue(func(s *engineState) { s.setPlanIn(func(in *plannerInput) { in.solarForecast = &f }) })
	e.signalReplan()
}

func (e *Engine) SetLoadProfile(stepKw []float64) {
	e.enqueue(func(s *engineState) { s.setPlanIn(func(in *plannerInput) { in.loadProfileKw = stepKw }) })
	e.signalReplan()
}

func (e *Engine) SetFallbackTOU(m *TOUCostModel) {
	e.enqueue(func(s *engineState) { s.setPlanIn(func(in *plannerInput) { in.fallbackTOU = m }) })
	e.signalReplan()
}
```

Latency contract unchanged: takes effect no later than the next
tick/safetyTick/Wake (drainCmds runs before every one).

### 3.3 `buildPlannerParams` insertions (engine.go)

Three bounded insertions, no restructuring:

**Solar forecast gate** — around the existing diurnal block at
engine.go:369–385, which today assigns `p.SolarForecastKw` unconditionally:

```go
const maxForecastAgeS = 12 * 3600 // §11 staleness rule; config-overridable later

if fc := inp.solarForecast; fc != nil && now.Unix()-fc.ReceivedUnix <= maxForecastAgeS {
	p.SolarForecastKw = resampleForecast(fc, p.WindowStart) // 5-min shift onto the window
	e.forecastSource.Store(forecastExternal)
} else {
	// ← existing diurnal high-water block, verbatim, unchanged
	e.forecastSource.Store(forecastDiurnal)
	if fc != nil { /* edge-triggered stale alarm, rewalk-style rate-limited */ }
}
```

`e.forecastSource` is an `atomic.Value` with an exported accessor
`ForecastSource() string` and gauge feed `lexa_hub_forecast_age_seconds` —
`cmd/hub`'s planObserver stamps both onto the plan log (§3.7).

**EV goal override** — after the config-derived assignment at
engine.go:448–452:

```go
if g := inp.evGoal; g != nil {
	p.EVTargetSocKwh = g.TargetSocKwh
	p.EVDepartureUnix = g.DepartureUnix
	if g.InitialSocKwh >= 0 { p.InitialEVSocKwh = g.InitialSocKwh } // seed for energy integration
}
```

**Reserve + load profile** — next to the `cfg.TerminalReservePct` derivation
(engine.go:421–425):

```go
if r := inp.reservePct; r != nil {
	pct := math.Max(*r, cfg.TerminalReservePct)          // intents only RAISE the floor
	p.TerminalSocKwh = p.BattCapacityKwh * pct / 100
	p.MinBattSocKwh = math.Max(p.MinBattSocKwh, p.BattCapacityKwh*pct/100)
}
if lp := inp.loadProfileKw; len(lp) > 0 { p.LoadProfileKw = lp }
if inp.fallbackTOU != nil { p.FallbackTOU = inp.fallbackTOU }
```

`PlannerParams` gains `LoadProfileKw []float64` and `planner.go` a
`planStepLoad(i)` accessor mirroring `planStepSolar` (profile if set, else
the scalar `LoadForecastKw`) — the DP's only change is calling the accessor.

**Safety invariant:** `SetBackupReserve` affects planner economics only. The
optimizer's `SOCReserve` safety checks and the Tier-0 interlock floor are
untouched; a user reserve can only be *more* conservative than the shipped
floor, never less.

### 3.4 Tariff swap — `internal/orchestrator/optimizer.go` (one additive method)

The planner side rides `FallbackTOU` (§3.2 — used exactly when CSIP
`SetPrices` arrays are nil, so utility pricing keeps winning by the existing
nil-fallback rule). The reactive optimizer reads its `CostModel` field on the
control goroutine, so the swap must be race-free without adding locks:

```go
// SwapCostModel atomically replaces the TOU model used by peak-shift
// decisions. Reads go through costModel(), which prefers the swapped model
// and falls back to the constructor-time CostModel field. Additive: existing
// construction and tests are untouched.
func (o *DefaultOptimizer) SwapCostModel(m *TOUCostModel) { o.costModelOverride.Store(m) }

func (o *DefaultOptimizer) costModel() *TOUCostModel {
	if m, _ := o.costModelOverride.Load().(*TOUCostModel); m != nil { return m }
	return o.CostModel
}
```

Internal call sites read `o.costModel()` instead of `o.CostModel` — a
mechanical, behavior-preserving substitution (the override is nil until the
first tariff intent). Tariff hours are evaluated in the process zone, which
GAP-05/TASK-079 already requires to equal the tariff zone; the tariff intent
is validated against the site's zone at the cloud/app layer.

### 3.5 Mode manager + gateway author — `cmd/hub/mode.go`, `internal/orchestrator/constraint/passthrough.go`

**Zero engine changes.** The engine already takes any
`orchestrator.Optimizer`; `SystemState` already carries `CSIPControl`. The
mode is which author runs:

```go
// cmd/hub/mode.go
type modeManager struct {
	mode      atomic.Value                  // "optimizer" | "gateway"
	optimizer orchestrator.Optimizer        // legacy cascade (shadow-wrapped when constraint_shadow)
	gateway   orchestrator.Optimizer        // constraint.Stack with CSIPPassthrough (below)
	safety    orchestrator.SafetyEvaluator  // ALWAYS the legacy DefaultOptimizer
	jw        *journal.Writer
	mc        mqtt.Client
	eng       *orchestrator.Engine          // set after construction, for Wake()
}

func (m *modeManager) Optimize(s orchestrator.SystemState) orchestrator.Plan {
	if m.Mode() == "gateway" { return m.gateway.Optimize(s) }
	return m.optimizer.Optimize(s)
}

// EvaluateSafety is deliberately MODE-INVARIANT: Tier-1 is a protection
// relay, not a policy. (ADR-0001; ecosystem roadmap §14.)
func (m *modeManager) EvaluateSafety(s orchestrator.SystemState) orchestrator.Plan {
	return m.safety.EvaluateSafety(s)
}

func (m *modeManager) request(mode string, meta bus.IntentMeta) (outcome, detail string) {
	from := m.Mode()
	if from == mode { return "duplicate", "" }
	m.jw.Append(journal.NewModeChangeEvent("hub", from, mode, meta.Actor, meta.Origin, meta.ID))
	m.mode.Store(mode)
	m.eng.Wake()                       // next tick runs the new author immediately
	m.publishModeStatus(mode, meta)    // retained lexa/hub/mode
	return "applied", ""
}
```

Wiring in `main.go`: `eng := orchestrator.New(reader, modeMgr, …)` where
`modeMgr.optimizer` is exactly what is passed to `New` today (including the
TASK-059 shadow wrapper when `constraint_shadow` is set), and
`modeMgr.gateway` is:

```go
constraint.NewStack(buildConstraintPlant(cfg), cfg.EngineInterval(),
	constraint.NewBatterySafety(...),   // TierSafety   — as in the shadow stack
	constraint.NewExport(...),          // TierCompliance — site ExpLimW envelope + convergence backstop
	constraint.NewGenLimit(...),        // TierCompliance — MaxLimW + meter floor
	constraint.NewImportLimit(...),     // TierCompliance — ImpLimW + NaN-hold
	constraint.NewCSIPPassthrough(cfg.Gateway)) // TierEconomics slot — replaces Economics
```

This is the R2 decision from the ecosystem roadmap: the site-level
`ExpLimW`/`ImpLimW` envelopes have no per-device 1:1 mapping, and the
shadow-validated compliance constraints are the code that already allocates
them to devices with measured-convergence backstops. The **only new
constraint** is the passthrough:

```go
// internal/orchestrator/constraint/passthrough.go (sketch)
// CSIPPassthrough occupies the economics slot in gateway mode: it expresses
// the utility's ACTIVE control (or defaults, or restore) as demands, and has
// no economic opinion of its own. The compliance-tier constraints above it
// narrow these demands for the site envelopes exactly as they do in shadow.
type CSIPPassthrough struct {
	session *Session
	policy  GatewayEVSEPolicy // §6 / hub.json "gateway" block
}

func (c *CSIPPassthrough) Name() string { return "csip-passthrough" }
func (c *CSIPPassthrough) Tier() Tier   { return TierEconomics }

func (c *CSIPPassthrough) Evaluate(in Input, s *Session) ([]Demand, *orchestrator.ComplianceBreach) {
	ctl := in.State.CSIPControl // nil ⇒ Source "none" ⇒ restore
	var out []Demand
	for name := range in.Plant.Inverters {
		if ctl != nil && ctl.Base.OpModMaxLimW != nil {
			out = append(out, CeilingDemand(name, AxisSolarCeilingW, apWatts(ctl.Base.OpModMaxLimW), TierEconomics, "csip-maxlim"))
		} else {
			// Restore is an EXPLICIT demand, never an absence (solar rule, generalized).
			out = append(out, CeilingDemand(name, AxisSolarCeilingW, restoreCeilingW, TierEconomics, "csip-restore"))
		}
	}
	for name := range in.Plant.Batteries {
		if ctl != nil && ctl.Base.OpModFixedW != nil {
			out = append(out, PointDemand(name, AxisBatterySetpointW, fixedWSetpoint(ctl.Base.OpModFixedW), TierEconomics, "csip-fixedw"))
		} else {
			// A pure gateway has no autonomous dispatch: batteries idle at 0 W
			// unless the utility commands otherwise. (Documented user-visible
			// consequence of gateway mode; §13 of the ecosystem roadmap.)
			out = append(out, PointDemand(name, AxisBatterySetpointW, 0, TierEconomics, "csip-idle"))
		}
	}
	if ctl != nil && ctl.Base.OpModConnect != nil {
		for _, dev := range in.allDevices() {
			out = append(out, ConnectDemand(dev, *ctl.Base.OpModConnect, TierEconomics, "csip-connect"))
		}
	}
	out = append(out, c.policy.evseDemands(in)...) // Scheduled window / full-power (§6)
	return out, nil
}
```

**Transition semantics** (both directions journal `mode_change` with actor):

- *optimizer→gateway*: nothing bespoke — the gateway stack's first pass emits
  a demand for **every** device (restore/idle/ceiling), so optimizer-authored
  holds are released by explicit desired-doc writes; the actuators'
  content-dedupe absorbs the no-ops. The retained desired docs carry the
  gateway state to dark devices on reconnect, as always.
- *gateway→optimizer*: the legacy cascade's first pass emits its own full
  set; engine state (guards, convergence counters) was never suspended
  because the compliance constraints kept running. Retained reconcile reports
  re-seed breach state exactly as after a restart.
- Boot: `main.go` subscribes retained `lexa/hub/mode` before `eng.Start()`;
  adoption order is retained-topic value ▸ `hub.json` `"mode"` ▸ "optimizer".

**Sequencing guard (R10):** TASK-095 lands only after the constraint stack's
compliance tier has passed its ≥1-week bench-shadow gate at 0 divergence
(already true post-TASK-064) **and** the per-axis P5 flip plan is settled, so
the bench never first-validates two plan authors at once. The fallback
(naive passthrough without the compliance constraints, MaxLimW/FixedW/
Connect only) is scoped in the task file but not the default.

### 3.6 Journal additions — `internal/journal/schema.go` (append-only)

New event types + typed payloads + `New*Event` constructors, **no `SchemaV`
bump** (append-only vocabulary rule): `intent_received` (cloudlink),
`intent_applied`, `intent_rejected`, `mode_change`, `config_write` (lexa-api,
§4.5), `scan_run` (lexa-modbus, §5). Gateway mode's per-mapping audit
(`event mRID, opMode, value, device, ts`) reuses the existing `dispatch`
event — the actuators already journal every desired-doc publish, so the
gateway audit surface comes free; the `mode_change` record anchors which
author was live.

### 3.7 Plan log + metrics additions

`bus.PlanLog` gains additive fields (v stays 1 — decoders ignore unknown
keys): `"mode"`, `"forecast_source"` (`"external"|"diurnal"`),
`"forecast_age_s"`. planObserver stamps them from `modeMgr.Mode()` and
`eng.ForecastSource()`. New hub metrics: `lexa_hub_intents_applied_total`,
`lexa_hub_intents_rejected_total`, `lexa_hub_forecast_age_seconds`,
`lexa_hub_forecast_external` (0/1), `lexa_hub_mode_gateway` (0/1).

New `hub.json` keys (all defaulted, absent = today's behavior):

```json
"mode": "optimizer",
"forecast_max_age_s": 43200,
"gateway": { "evse_policy": "scheduled", "evse_window": {"start_hh":23, "end_hh":7}, "evse_full_a": 32 }
```

---

## 4. `lexa-api` additions

### 4.1 HTTPS

`crypto/tls` server on the existing `ListenAddr` mux (no wolfSSL — that stack
is client-side CSIP only). Per-device self-signed cert generated on first
boot and persisted:

```go
// cmd/api/tlscert.go
// ensureServerCert loads /var/lib/lexa/api/{cert,key}.pem, generating a
// 10-year self-signed ECDSA P-256 cert on first boot (SANs: hostname,
// "<serial>.local", current LAN IPs). Returns the cert and its SHA-256
// fingerprint (published in CloudlinkStatus-adjacent health uplink and shown
// by `lexactl fingerprint` for TOFU comparison during commissioning).
func ensureServerCert(dir string) (tls.Certificate, string, error)
```

`srv.ListenAndServe()` becomes `srv.ListenAndServeTLS("","")` with the cert
in `TLSConfig`. Config: `"tls": true` default; `"tls": false` is the
bench/dev escape hatch (the deployed bench flips it until the dashboard
learns the fingerprint flow). `/healthz` stays reachable for the loopback
watchdog probe (it moves to `https://127.0.0.1…` with
`InsecureSkipVerify` inside the same process — the probe checks liveness,
not identity).

### 4.2 Auth

`requireBearer` (constant-time compare, empty ⇒ open staged rollout) is kept
verbatim; the change is provisioning, not code: manufacturing writes the
per-unit secret to `/etc/lexa/api-secret` (printed on the label),
`api_token_file` points at it, and claim-time rotation is a `config_write`
of that file through §4.5 (journaled). `POST` routes additionally require
the header on every deployment profile — the empty-token escape hatch
applies to reads only:

```go
mux.HandleFunc("/intent", requireBearerStrict(apiToken, intentHandler(mc, results)))
```

### 4.3 REST surface

All new handlers are bus→HTTP projections off the existing `stateStore` plus
one write path; CORS/OPTIONS handling copies the existing handlers.

| Route | Method | Source |
|---|---|---|
| `/site` | GET | claimed/site metadata cached from cloud push (file under `/var/lib/lexa/site.json`) + TZ + fw version |
| `/devices` | GET | `stateStore` device map + `lexa/scan/result` + `lexa/ocpp/pending` |
| `/telemetry/recent` | GET | ring buffer of last N minutes of measurements already flowing into `stateStore` (bounded, in-memory) |
| `/mode` | GET | retained `lexa/hub/mode` projection |
| `/intent` | POST | validate → publish `lexa/intent/{kind}` → await matching `lexa/intent/result` (3 s) → 200 with outcome / 202 if timeout |
| `/scan` | POST/GET | publish `lexa/scan/request` / project `lexa/scan/status`+`result` |
| `/config/{service}` | POST | §4.5 (commissioning only) |

```go
// cmd/api/intent.go
func intentHandler(mc mqtt.Client, res *resultWaiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Kind string          `json:"kind"`
			Body json.RawMessage `json:"body"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil { http.Error(w, "bad json", 400); return }
		spec, ok := localIntentKinds[req.Kind] // NOTE: no solarforecast/loadprofile (cloud-only, §1.4)
		if !ok { http.Error(w, "unknown kind", 400); return }
		intent, meta, err := spec.decodeAndStamp(req.Body) // Origin:"app", ID: uuid, IssuedAt: now
		if err != nil { http.Error(w, err.Error(), 400); return }
		ch := res.expect(meta.ID)
		if err := mqttutil.PublishJSONTimeout(mc, spec.topic, spec.retained, intent, 2*time.Second); err != nil {
			res.cancel(meta.ID); http.Error(w, "bus publish failed", 502); return
		}
		select {
		case r := <-ch:      writeJSON(w, 200, r)      // hub's IntentResult verbatim
		case <-time.After(3 * time.Second):
			writeJSON(w, 202, map[string]string{"id": meta.ID, "outcome": "pending"})
		}
	}
}
```

`resultWaiter` is one `mqttutil.Subscribe` on `lexa/intent/result` muxing by
ID (`sync.Map` of channels) — the api never invents outcomes; it relays the
hub's.

### 4.4 mDNS advertisement — `cmd/api/mdns.go`

The registration mirror of the northbound's existing `dnssd.Browse` (same
vendored `grandcat/zeroconf`, no go.mod change):

```go
srv, err := zeroconf.Register(
	"lexa-"+serial, "_lexa-hub._tcp", "local.", listenPort,
	[]string{"serial=" + serial, "fw=" + version.String(),
		"claimed=" + boolTo01(claimed), "api=https"}, nil)
// held for process lifetime; srv.SetText refreshes claimed= on claim;
// srv.Shutdown() on SIGTERM alongside the existing signal loop.
```

Config: `"mdns": true` (default; `false` for privacy-sensitive deployments).

### 4.5 Commissioning config-write (the R11 surface — treat as security-critical)

`POST /config/{service}` with body = full JSON config. Gate, validate, stage,
restart, journal:

- **Gate:** allowed only when `/etc/lexa/commissioned` is absent
  (uncommissioned unit) **or** a commissioning window has been armed by a
  cloud-verified installer session (retained arm doc with TTL, pushed via
  cloudlink). Otherwise 403.
- **Validate:** schema check against `/usr/share/lexa/schema/<service>.json`
  (shipped in the image; generated from the config structs at build time) —
  syntax, required keys, enum values (e.g. reconciler modes), path
  allowlists. Reject anything that sets `mqtt_pass_file` outside
  `/etc/lexa/mqtt/` etc.
- **Stage:** write to `<file>.staged`, fsync, rename over — never a partial
  config on disk.
- **Restart:** `sudo -n /bin/systemctl restart lexa-<svc>` permitted by a
  shipped `/etc/sudoers.d/lexa-api` fragment enumerating exactly the six
  unit names (api runs as `lexa`; the fragment is the entire privilege).
- **Journal:** `config_write{service, actor, sha256(before), sha256(after)}`.

The commissioning wizard (§8 of the ecosystem roadmap) is a pure client of
this endpoint plus `/scan` and `/intent` — no other write path exists.

---

## 5. Commissioning scan — `lexa-proto` + `cmd/modbus`

### 5.1 `lexa-proto` additions (paired PR + proto-pin bump, MTR-4 lockstep)

Nothing in the tree today can identify a device: `sunspec.Scan` walks model
blocks *within* one already-addressed device, and SunSpec model 1 (Common) is
declared but never parsed. Two new files in `lexa-proto/sunspec`:

```go
// identity.go
// Common is SunSpec model 1: the device's identity block.
type Common struct {
	Manufacturer string // 16 regs, NUL-padded
	Model        string // 16 regs
	Options      string // 8 regs
	Version      string // 8 regs
	Serial       string // 16 regs
	DeviceAddr   uint16
}

// ReadCommon locates model 1 via the cached block walk and decodes it.
func ReadCommon(r *Reader) (Common, error)

// sweep.go
type SweepHit struct {
	URL    string   // transport URL that answered
	UnitID uint8
	Blocks []Block  // model walk (classification input)
	Common *Common  // nil if model 1 read failed
	Err    string   // non-fatal per-device error
}

// SweepTCP probes each host in cidr on port for the SunS marker across unitIDs,
// invoking hit() per responder. Timeouts are per-probe; ctx cancels the sweep.
func SweepTCP(ctx context.Context, cidr string, port int, unitIDs []uint8,
	timeout time.Duration, hit func(SweepHit)) error

// SweepRTU probes unitIDs across baud rates on one serial device. Serial is
// a shared medium: strictly sequential, generous inter-probe quiet time.
func SweepRTU(ctx context.Context, dev string, bauds []int, unitIDs []uint8,
	timeout time.Duration, hit func(SweepHit)) error
```

Ship the same bench session: the sim Pis' modsim/batsim/metersim answer the
sweep (they already speak SunSpec), giving the scan a test target with zero
new sim work. Version bump = paired PRs + both `proto.pin`s + both
`vendor/lexa-proto/` trees regenerated, per CLAUDE.md.

### 5.2 `cmd/modbus/scan.go` — scan controller

```go
// scanController subscribes lexa/scan/request. Arming rule (v1, deliberately
// blunt): a scan runs ONLY when the service owns zero live devices — i.e.
// cfg.Devices is empty (uncommissioned) — OR every configured reconciler
// mode is "off". Any other state answers ScanStatus{Phase:"refused"}.
// A scan therefore can never share a serial line or TCP session with live
// polling/control. Post-commissioning re-scans are an operator action:
// stop lexa-modbus, run `lexactl scan --offline`, restart. (v2 may add an
// armed-pause handshake with the hub; not v1.)
```

Flow: request → refuse-or-run → `SweepTCP`/`SweepRTU` with per-hit
`ScanStatus` progress → classify each hit (`Blocks` model IDs: 701/10x ⇒
inverter, 802/12x ⇒ battery, 20x ⇒ meter; `SunS`-but-unknown ⇒
`unknown-sunspec`; Modbus-answering-but-no-marker ⇒ `unknown-modbus`) →
nameplate via `derbase.Init`-equivalent WMax read where present → retained
`ScanResult` → journal `scan_run`. The wizard turns confirmed hits into
`modbus.json` `devices[]` entries + `hub.json` device/plant blocks (per-model
template library lives cloud-side) and POSTs them via §4.5.

Metrics: `lexa_mb_scan_runs_total`, `lexa_mb_scan_refused_total`,
`lexa_mb_scan_devices_found` (gauge, last run).

---

## 6. `lexa-ocpp` — pending-station surface + gateway EVSE policy

**Pending stations (closes the silent-adoption gap):** today
`getOrCreateLocked` auto-creates a `stationState` for any inbound station ID;
unconfigured stations are tracked-but-uncontrolled with only a log line. Add:
on connect of a station **not** in `cfg.Stations`, append it to a retained
`bus.PendingStations` doc on `lexa/ocpp/pending` (vendor/model from
BootNotification when available) and hold it **measurement-only** (exactly
today's behavior — no shell, no control; nothing regresses). Approval is the
wizard writing it into `ocpp.json` via §4.5 and restarting lexa-ocpp;
the pending doc drops approved/departed entries on rebuild.

**Gateway EVSE policy lives in the hub** (§3.5's `CSIPPassthrough` emits the
EVSE demands from `hub.json`'s `"gateway"` block — Scheduled window or
full-power, still narrowed by any CSIP/site constraint). lexa-ocpp itself
needs **no mode awareness**: it keeps executing whatever desired docs arrive.
This keeps the single-author invariant clean.

---

## 7. `lexactl` — `cmd/lexactl`

Small stdlib-only CLI shipped in the image; talks to the local API over
loopback HTTPS (fingerprint check against the on-disk cert; token from
`/etc/lexa/api-secret`, root-readable). Subcommands (all thin wrappers over
§4.3 routes — **nothing bypasses the intent journal**):

```
lexactl status                      # GET /status, pretty-printed
lexactl mode get|set optimizer|gateway   # GET /mode, POST /intent kind=mode
lexactl intent <kind> [flags]       # POST /intent (evgoal/reserve/tariff/chargenow)
lexactl scan [--watch]              # POST /scan + stream /scan status
lexactl forecast show               # plan-log forecast_source/age projection
lexactl fingerprint                 # prints the api cert SHA-256 (TOFU aid)
lexactl spool                       # GET cloud_link status projection
```

---

## 8. OTA on-box hooks

### 8.1 `lexa-healthcheck` — `cmd/healthcheck`

One small binary, two consumers: the Mender `ArtifactCommit` state script
(commit-or-rollback gate) and ad-hoc operator use. Exit 0 = healthy.

```go
// Checks (each with a bounded timeout; total budget ~120 s with retries):
//  1. systemd: all seven lexa-* units + mosquitto active (systemctl is-active).
//  2. api:     GET https://127.0.0.1:9100/healthz == 200.
//  3. plan:    /status plan_heartbeat.state == "ok"  — OR "never" when
//              /etc/lexa/commissioned is absent (uncommissioned units idle).
//  4. northbound: journal shows a completed walk since boot, or config has
//              no server (cleanly idle).
//  5. modbus:  every configured device has published a measurement since boot
//              (via /status devices staleness) — skipped when none configured.
//  6. clock:   system year >= build year AND (chrony synced OR RTC valid).
//  7. cloudlink: enabled ⇒ connected OR spooling healthily (never gates on
//              WAN availability — an offline install must still commit).
```

Mender state scripts (`meta-lexa` additions): `ArtifactInstall_Enter` (noop
marker), `ArtifactCommit_Enter_00_health` (runs `lexa-healthcheck --commit`),
plus the standard bootloader A/B integration from `meta-mender`. A failed
commit rolls back to the previous slot automatically — the existing
watchdog/heartbeat work becomes brick-proofing with no new invariants.

### 8.2 Config schema migration — `cmd/lexa-migrate` + oneshot unit

Every `/etc/lexa/*.json` gains `"schema_version": 1` (absent = 0 = current
files, accepted). A `lexa-migrate.service` oneshot
(`Before=lexa-*.service`, `After=local-fs.target`) runs stepwise per-file
migrations on first boot of a new slot: back up (`<file>.pre-<ver>`), apply,
bump, fsync+rename. Migrations live with the release that needs them; the
runner refuses to *down*-migrate (rollback keeps the backup files). This is
the `bus.Envelope` habit applied to the data partition, and it is what makes
config-on-persistent-partition + A/B rootfs safe to evolve.

---

## 9. Uncommissioned/factory mode + clock trust

**Marker:** `/etc/lexa/commissioned` (empty file, created by the wizard's
final step via §4.5; removed by factory reset). Consumers:

| Service | Uncommissioned behavior |
|---|---|
| lexa-api | mDNS TXT `claimed=0`; `/config` writes allowed; `/status` reports `"commissioned": false` |
| lexa-modbus | scan requests honored (§5.2); no devices configured ⇒ no polling (existing behavior) |
| lexa-northbound | ships with no server URL + discovery disabled in the factory config ⇒ cleanly idle (existing fail-closed behavior; no code change — a config profile) |
| lexa-hub | no devices/stations ⇒ engine idles safe (existing); heartbeat state `never` accepted by healthcheck |
| lexa-cloudlink | connects and publishes health/claim state to the quarantine namespace (cloud-side routing by claim status) |

**Factory reset:** `scripts/factory-reset.sh` in the image (wired to a GPIO
button handler later — hardware-dependent, out of scope here): stop services,
wipe `/etc/lexa/*.json` back to factory profiles, wipe `/var/lib/lexa`
(journals, spool, snapshots, api cert), **preserve `/etc/lexa/identity`**
(device identity survives; CSIP LFDI cert is wiped — re-enrollment is
per-site anyway), remove `commissioned`, restart.

**Clock trust:** `chrony` in the image; `After=time-sync.target` added to
lexa-northbound, lexa-telemetry, lexa-cloudlink units (cert validation) —
with `RestartSec` unchanged so a never-syncing clock degrades to the existing
fail-closed loops rather than blocking boot forever; healthcheck check #6
(§8.1) gates OTA commits on clock sanity; commissioning functional test
asserts NTP reachability. RTC backup battery is a hardware-rev item tracked
in the ecosystem roadmap (R6).

---

## 10. Config additions summary

| File | New keys |
|---|---|
| `hub.json` | `mode`, `forecast_max_age_s`, `gateway{evse_policy, evse_window, evse_full_a}` |
| `cloudlink.json` | **new file** (§2.2) |
| `api.json` | `tls` (default true), `mdns` (default true), `serial_file`, `site_cache` (`/var/lib/lexa/site.json`) |
| `modbus.json` | *(none — scan is topic-driven; arming derives from existing `devices`/`reconciler`)* |
| `ocpp.json` | *(none — pending surface is automatic)* |
| all | `schema_version` (§8.2) |

---

## 11. Task breakdown

Sizes: S ≤ 3 days · M ≈ 1–2 weeks · L ≈ 2–4 weeks. Phases reference the
ecosystem roadmap §19. Every task follows the house delivery discipline:
`*_test.go` beside each component, journal/metrics wired, ACL re-derived from
call sites, bench scenario named before merge.

| Task | Deliverable | Size | Phase | Depends on | Exit criterion |
|---|---|---|---|---|---|
| **TASK-082** | Bus intent/scan/mode/status schema: `internal/bus/intent.go`, `scan.go`, version consts, `SupportedV` cases, topic consts, `Finite()`s, ACL delta | S | 1 | — | round-trip encode/decode + Finite-reject tests green; ACL matrix reviewed against this doc |
| **TASK-083** | Hub intent adoption (`cmd/hub/intent.go`) + engine setters + `buildPlannerParams` gates + plan-log fields + journal event types | L | 1 | 082 | each kind: applied/clamped/expired/duplicate paths pinned by tests; stale forecast falls back to diurnal with edge alarm; reserve can never go below config floor (test) |
| **TASK-084** | `internal/spool` | M | 1 | — | budget/priority/eviction/torn-record/crash-redelivery property tests |
| **TASK-085** | lexa-cloudlink uplink core (service skeleton, collectors, batcher, cloud session, status topic, unit file, ACL, configs) | L | 1 | 082, 084 | bench measurement lands in TimescaleDB end-to-end; `enabled:false` idles cleanly; frames ≤ 96 KiB enforced |
| **TASK-086** | cloudlink downlink → intents + diag bundle | M | 1 | 085, 083 | rejection table pinned; forged/expired/rate-limited never reach the bus; diag bundle redacts secrets (test on fixture tree) |
| **TASK-087** | cloudlink cert monitor | S | 1 | 085 | expiring bench cert drives gauge + status field + WARN |
| **TASK-098** | `lexa-healthcheck` + Mender state scripts + `lexa-migrate` | M | 1 | — | broken-service OTA auto-rolls back on the dev kit; schema_version=0 files migrate cleanly |
| **TASK-099** | Uncommissioned mode + factory profiles + clock-trust unit ordering + factory-reset script | M | 1–2 | — | factory-profile boot passes healthcheck with heartbeat `never`; reset returns a commissioned unit to factory state preserving identity |
| **TASK-088** | lexa-api HTTPS + strict-auth writes + REST (`/site /devices /telemetry/recent /mode /intent`) + resultWaiter | M | 2 | 082, 083 | `POST /intent` relays hub outcome verbatim; TLS fingerprint stable across restarts; existing `/status`/`/logs` tests still pin |
| **TASK-089** | lexa-api mDNS advertise + TXT lifecycle | S | 2 | 088 | discovered from a phone on the bench LAN; TXT flips on claim |
| **TASK-090** | Commissioning config-write path + schema files + sudoers fragment | M | 2 | 088, 099 | gated 403 when commissioned; staged-rename write; journaled; security review sign-off |
| **TASK-091** | `lexa-proto` sweep + model-1 identity (paired PR, pin bump both repos) | M | 2 | — | bench sims discovered + identified end-to-end (manufacturer/serial correct) |
| **TASK-092** | `cmd/modbus` scan controller + topics | M | 2 | 091, 082 | scan refused while any reconciler is live (test); wizard-consumable retained result |
| **TASK-093** | lexa-ocpp pending-station surface | S | 2 | 082 | unknown evsim ID appears pending, stays uncontrolled; approval path exercised on bench |
| **TASK-097** | `lexactl` | S | 2 | 088 | every subcommand round-trips against a live bench unit |
| **TASK-094** | Tariff: intent → `TOUCostModel` compile → `SetFallbackTOU` + `SwapCostModel` | M | 3 | 083 | DST tables from `costmodel_test.go` extended to a user tariff; CSIP pricing still wins when present (test) |
| **TASK-095** | Mode manager + `CSIPPassthrough` + transitions + `lexa/hub/mode` + boot re-seed | L | 4 | 083; constraint-stack P5 sequencing (R10) | bench: mode flip under active CSIP event breaches nothing beyond oracle; both transition directions journaled; restart preserves mode |
| **TASK-096** | Gateway EVSE policy (hub-side demands from `gateway` block) | S | 4 | 095 | Scheduled window honored in gateway mode; CSIP suspend still wins |

New Mayhem scenarios to land with their tasks (bench repo, paired sessions):
`wan-outage-spool-drain`, `cloud-cmd-forgery-rejected`,
`stale-intent-expired` (085/086); `intent-flood-rate-limit` (083);
`mode-flip-under-active-event`, `mode-flip-under-fault` (095);
`scan-during-live-control-refused` (092); `ota-broken-service-rollback`
(098).

Dependency spine: **082 → 083 → {085/086, 088} → everything else**; 084/098/
099 are parallel-start; 095 is deliberately last among the hub changes.

---

## 12. What changes where — and what does not

**Touched existing files (bounded insertions only):**
`internal/bus/{topics,envelope,finite}.go` (constants + cases + Finite impls) ·
`internal/orchestrator/engine.go` (5 setters, `plannerInput` fields, 3
gated insertions in `buildPlannerParams`, `ForecastSource` accessor) ·
`internal/orchestrator/planner.go` (`LoadProfileKw` + `planStepLoad`) ·
`internal/orchestrator/optimizer.go` (`SwapCostModel` + `costModel()`
substitution) · `internal/journal/schema.go` (append-only event types) ·
`cmd/hub/main.go` (subscribe blocks, modeManager wiring, plan-log fields) ·
`cmd/api/{main,config,handlers}.go` (TLS, routes, mDNS hook) ·
`cmd/modbus/main.go` (scan controller hook) · `cmd/ocpp/main.go` (pending
publish in `getOrCreateLocked`) · `systemd/*` (new unit, ACL, sudoers,
time-sync ordering) · `Makefile`/CI (three new binaries: cloudlink, lexactl,
healthcheck + migrate).

**Explicitly unchanged — the invariants this program exists to preserve:**

- The **legacy optimizer cascade remains the authoritative author in
  optimizer mode**; the constraint stack's shadow harness and P5 flip
  process are untouched by everything except TASK-095, which consumes (not
  modifies) the stack.
- **Reconcilers remain the only writers to hardware; retained desired docs
  remain the sole command path.** No new component publishes
  `lexa/desired/#` — the ACL forbids it mechanically.
- **Tier-0 interlock and Tier-1 `EvaluateSafety` are mode-invariant and
  senior to every intent**, including `chargenow` and gateway mode.
- **The northbound/utility compliance path is untouched**: wolfSSL CCM-8,
  scheduler fail-closed semantics, CannotComply episodes, cert
  rotation — cloudlink is a parallel northbound, never in the compliance
  path.
- **Crash-only (AD-011)** holds for the new services: no blanket `recover()`,
  retained topics + spool re-seed state after restart.
- **Envelope versioning, QoS-by-table, ACL-by-call-site, flash budget,
  journald caps** all extend to the new surface rather than being bypassed.
