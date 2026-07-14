# OpenADR digest (2.0a / 2.0b / 3.1.0) — VEN-side view for a potential LEXA northbound addition

OpenADR is NOT implemented anywhere in lexa-hub today. This digest sizes what a VEN
(client) implementation would require under each profile and maps signals onto existing
hub capabilities.

## Document identities

| Doc | Identity | Status |
|---|---|---|
| OpenADR 2.0a Profile Specification | Rev 1.0, doc 20110712-1, OpenADR Alliance (2011-12) | Completed |
| OpenADR 2.0b Profile Specification | v1.1, OpenADR Alliance (2015) | Final |
| OpenADR 3 READ ME (Info & Certification) | v3.1.0, © 2025 | Certification program overview |
| OpenADR 3.1.0 Definitions | Rev 3.1.0, doc 20250807-X, updated 2025-08-07 | Final Specification |
| OpenADR 3.1.0 OpenAPI YAML | `1_OpenADR_3.1.0_20250801.yaml`, openapi 3.0.0 | **Normative/authoritative** for 3.x; Definitions/User Guide are readable companions |
| OpenADR 3.1.0 User Guide | 2025-08-01 | Non-normative scenarios/examples |

2.0a/2.0b are profiles (strict subsets + extensions) of OASIS Energy Interoperation (EI)
1.0, XML/XSD payloads. 3.x is a clean-slate REST/JSON re-expression ("near functional
equivalent of 2.0b"), NOT a replacement for 2.0x — both certification tracks coexist.
3.1.0's delta over 3.0.x: adds alternate notification bindings (MQTT first) + mDNS/DNS-SD
local-VTN discovery.

## VTN/VEN model

- **VTN** (Virtual Top Node) = server; publishes grid conditions (prices, reliability
  events, dispatches). Utility/aggregator side.
- **VEN** (Virtual End Node) = client; has operational control of resources, consumes
  events, responds (opt-in/out), generates reports. **The LEXA hub would be a VEN.**
- Strict client/server; no peer-to-peer. Hierarchies allowed: a node can be VEN upstream
  and VTN downstream (aggregator/gateway pattern — the 3.x "Local Scenarios" chapter
  explicitly blesses a building/HEMS/microgrid gateway that is VEN to the utility and VTN
  to local devices; hub-relevant if the product ever fans out DR signals).
- Interaction patterns: PUSH (VTN initiates; VEN must run a server endpoint) or PULL
  (VEN polls). 2.0x VENs: pull mandatory, push optional. 3.x: pull = REST GET; push =
  subscriptions with webhooks (VEN hosts HTTPS callback) or 3.1 MQTT notifier (VEN is an
  MQTT subscriber — no inbound listener needed).
- 3.x adds a third actor name: **Business Logic (BL)** = utility-side client that creates
  programs/events and reads reports; VEN and BL are both clients of the VTN REST server.

## 2.0a VEN requirements

Scope: minimal profile for constrained devices. **One service only: EiEvent** (limited
profile). EiReport/EiOpt/EiRegisterParty/EiQuote are NOT in 2.0a. Registration is
out-of-band (cert fingerprint conveyed manually; Annex A non-normative).

- Payloads (XML): receive `oadrDistributeEvent` (requestID, vtnID, oadrEvent[]); respond
  `oadrCreatedEvent` per event when `oadrResponseRequired="always"` (never respond on
  "never"), with optType optIn/optOut; pull request via `oadrRequestEvent`.
- Event model: eventDescriptor (eventID, modificationNumber, priority, marketContext,
  eventStatus far/near/active/cancelled, testEvent), one eiActivePeriod
  (dtstart + duration, optional tolerance/startafter randomization, x-eiRampUp,
  x-eiRecovery), eiEventSignals with **exactly one signal, signalName="simple"**,
  signalPayload ∈ {0=normal,1=moderate,2=high,3=special}; interval durations must sum to
  active-period duration; eiTarget (venID/groupID/resourceID/partyID, OR'd).
- State machine duties (conformance rules 1–68, the real spec): honor
  modificationNumber monotonicity (lower number ⇒ error), implied cancel when a known
  event vanishes from a distribute payload, explicit cancel ack (optIn=confirm),
  randomized start AND randomized termination when startafter set, overlapping events
  only across marketContexts by priority, respond-before-next-poll, app-level 4xx error
  codes, requestID echo rules.
- Transport: **simple HTTP mandatory** (all ops are HTTP POST of XML to
  `https://host(:port)/(prefix/)OpenADR2/Simple/<service>`); XMPP optional (draft).
  Pull mandatory, push optional; configurable poll interval + jitter; truncated binary
  exponential backoff on failure; no chunked transfer-encoding.
- Security: TLS (1.0-era text) with **mutual authentication — client cert mandatory**;
  default suites TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA / TLS_RSA_WITH_AES_128_CBC_SHA;
  X.509v3, ECC ≥224/256-bit or RSA ≥2048; VEN needs ≥1 cert from approved CA list;
  SHA-1-based cert fingerprint (last 10 bytes) for out-of-band registration. "High"
  security (XML signatures) optional.

## 2.0b VEN requirements

Scope: full-function profile ("advanced DR systems, wholesale/retail markets").
2.0b VENs must also satisfy the 2.0a EiEvent conformance rules. Four services:

- **EiEvent (full)**: everything in 2.0a plus: multiple signals per event; full signal
  catalog (Table 1): SIMPLE, ELECTRICITY_PRICE / ENERGY_PRICE / DEMAND_CHARGE (price,
  priceRelative, priceMultiplier), BID_PRICE/BID_LOAD/BID_ENERGY, CHARGE_STATE
  (setpoint/delta/multiplier — storage dispatch), LOAD_DISPATCH
  (setpoint/delta/multiplier/level), LOAD_CONTROL (x-loadControlCapacity/-LevelOffset/
  -Setpoint/-PercentOffset); signal-level eiTarget (endDeviceAsset device classes);
  baselines (eiEventBaseline); modificationDateTime/Reason; ISO-8601 durations incl.
  fractional seconds.
- **EiReport**: report registration + exchange engine. Report types: METADATA
  (capability catalog — MANDATORY to support even if you have nothing to report),
  TELEMETRY_USAGE, TELEMETRY_STATUS, HISTORY_USAGE (+ Green Button). Operations (both
  directions, VEN⇄VTN): `oadrRegisterReport`/`oadrRegisteredReport` (METADATA, may embed
  a report request), `oadrCreateReport`/`oadrCreatedReport` (request by reportSpecifierID,
  select data points by rID, one-shot vs periodic via granularity/reportBackDuration),
  `oadrUpdateReport`/`oadrUpdatedReport` (actual data), `oadrCancelReport`/
  `oadrCanceledReport` (+ reportToFollow final report, oadrPendingReports).
  Rule 510 minimums for a VEN: METADATA + TELEMETRY_STATUS (mandatory data points
  oadrOnline, oadrManualOverride per resource) + TELEMETRY_USAGE (sample data OK at
  certification) + ≥100 data points of local history storage. "Report Only" sub-profile
  (meters): EiReport + EiRegisterParty only, but must support all standard reports incl.
  HISTORY_USAGE.
- **EiRegisterParty** (in-band registration): `oadrQueryRegistration` (discover VTN
  profiles/transports) → `oadrCreatePartyRegistration` (VEN chooses profile 2.0a/2.0b,
  transport, push/pull, reportOnly, xmlSignature flags) → VTN returns registrationID +
  venID + `oadrRequestedOadrPollFreq`; re-registration (VTN can demand it via
  `oadrRequestReregistration`), `oadrCancelPartyRegistration` both directions.
- **EiOpt**: VEN-originated opt-in/opt-out **schedules** (temporary availability windows,
  per eiTarget/resource) via `oadrCreateOpt`/`oadrCancelOpt`; also post-ack opt changes
  for a specific eventID (takes precedence over subsequent oadrCreatedEvent).
- **oadrPoll**: service-independent poll endpoint; pull VEN emulates push — VTN answers
  with whatever it would have pushed (oadrDistributeEvent, oadrCreateReport,
  oadrRequestReregistration, oadrCancelPartyRegistration, oadrUpdateReport, ...), one
  payload per poll, VEN drains queue by repeat-polling until bare oadrResponse. Poll
  cadence set by VTN at registration.
- Application error codes: 45x compliance (450 out of sequence, 452 invalid ID, 454
  invalid data...), 46x deployment (460 signal not supported, 462 target mismatch, 463
  not registered/authorized).
- Transports: VTN must do both; **VEN chooses HTTP or XMPP** (either satisfies).
  Endpoint template `.../OpenADR2/Simple/2.0b/<service>` (rule 511).
- Security: **TLS 1.2 required**; default suites TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256
  / TLS_RSA_WITH_AES_128_CBC_SHA256; mutual auth client certs (ECC ≥256 or RSA ≥2048,
  X.509v3) governed by the OpenADR Certificate Policy (Alliance-contracted CAs);
  SHA-256 cert fingerprint (last 10 bytes) for registration. **High security = optional
  XML Signatures** (rule 514): detached sibling signature under an `oadrPayload` root
  (oadrSignedObject + Signature), RSA-SHA256/ECDSA-SHA256, SHA-256 digest, mandatory
  `ReplayProtect` property (sent-time + nonce, reject stale); if configured, unsigned
  messages must be ignored. Note the signed root element changes every payload's schema
  vs plain 2.0b.

## 3.1 VEN requirements (REST resources, auth)

Architecture: VTN = OAuth2-protected REST resource server, JSON bodies, OpenAPI-defined.
VEN = HTTP client. No XML, no XSD, no XMPP, no client-cert identity model.

Resource catalog (paths from the OpenAPI; full CRUD unless noted):
- `/programs`, `/programs/{id}` — program = DR program or **tariff**; carries
  intervalPeriod, payloadDescriptors, attributes (RETAILER_NAME, COUNTRY,
  BINDING_EVENTS, LOCAL...), targets. VEN: GET only (write_programs is BL-only).
- `/events`, `/events/{id}` — event = programID + priority + targets +
  reportDescriptors (how the VTN asks for reports) + payloadDescriptors +
  intervalPeriod + intervals[]. Each interval = id + optional intervalPeriod override +
  payloads (valuesMap: {type, values[]}). intervalPeriod = {start (RFC3339; sentinel
  "0001-01-01"≈now), duration (ISO8601; "P9999Y"≈infinity), randomizeStart}. VEN: GET
  (query params programID/targets/active/skip/limit).
- `/reports`, `/reports/{id}` — VEN POSTs/PUTs reports: {programID, eventID, clientName,
  payloadDescriptors, resources[] each with intervals}. Reports are requested via
  reportDescriptors inside events (startInterval, numIntervals, historical, frequency,
  repeat, aggregate).
- `/subscriptions`, `/subscriptions/{id}` — webhook registration: clientName, programID,
  objectOperations[{objects[PROGRAM|EVENT|REPORT|SUBSCRIPTION|VEN|RESOURCE],
  operations[CREATE|UPDATE|DELETE], callbackUrl}], targets. VTN POSTs a `notification`
  {objectType, operation, object, targets} to the callback. Callback URL must be HTTPS
  and pass an `echo` challenge GET at subscribe time.
- `/vens`, `/vens/{id}`, `/resources`, `/resources/{id}` — VEN self-registration objects:
  venName (unique per VTN), attributes (LOCATION, MAX_POWER_CONSUMPTION,
  MAX_POWER_EXPORT...), targets; resources = per-device children (resourceName unique
  per ven).
- `/auth/server` (required; returns token-endpoint URL), `/auth/token` (optional
  built-in OAuth2 token endpoint).
- 3.1 notifier discovery: `GET /notifiers` → {WEBHOOK: true, MQTT: {URIS, serialization:
  JSON, authentication: ANONYMOUS|OAUTH2_BEARER_TOKEN|CERTIFICATE}} and
  `GET /notifiers/mqtt/topics/...` (programs/events/reports/subscriptions/vens/resources,
  incl. per-ven filtered topic sets `/notifiers/mqtt/topics/vens/{venID}/events`).
  MQTT ≥3.1.1 (SHOULD 5.0), MQTTS mandatory; no server-side filtering on raw topics —
  client filters. Message format = same JSON notification object.
- mDNS/DNS-SD local-VTN discovery (SHOULD): service type `_openadr3._tcp`, TXT keys
  version/base_path/local_url/program_names/requires_auth/openapi_url.

Auth model: **OAuth2 client-credentials flow** — VEN is provisioned out-of-band with
clientID/clientSecret + VTN URL (end-user reconfigurable — a MUST), trades them for a
short-lived bearer token (JWT typical), sends `Authorization: Bearer` on every call.
Scopes gate operations: read_targets (VEN event/program reads, target-matched),
read_ven_objects (own-clientID objects), write_reports, write_subscriptions, write_vens
(VEN-writable); read_all/write_programs/write_events are BL-only. Object privacy: VTN
stamps clientID from token into VEN-created objects; program/event visibility enforced
via targets granted on the ven object. HTTPS/TLS ≥1.2 mandatory, server-cert
verification (SHOULD, with config override for self-signed local VTNs); NO client
certificates required. Non-authenticating VTNs allowed for public tariffs (VEN with no
clientID configured just GETs).

Payload descriptors: events/programs carry eventPayloadDescriptor {payloadType, units,
currency}; reports carry reportPayloadDescriptor {payloadType, readingType
(DIRECT_READ default /ESTIMATED/MEAN/PEAK/FORECAST...), units, accuracy, confidence} —
so interval payloads stay minimal ({type, values}). Units enum: KWH, KW, VOLTS, AMPS,
PERCENT, KVA(R)(H), GHG, CELSIUS/FAHRENHEIT; currency per ISO 4217.

Key event payloadType enums (Table 1): SIMPLE (0-3), PRICE, PRICE_ALTERNATE,
EXPORT_PRICE, GHG, CHARGE_STATE_SETPOINT, DISPATCH_SETPOINT(_RELATIVE),
DISPATCH_INSTRUCTION, CONTROL_SETPOINT, CONTROL_LEVEL_OFFSET(_PERCENT),
**IMPORT_CAPACITY_LIMIT / EXPORT_CAPACITY_LIMIT** (max site import/export),
IMPORT/EXPORT_CAPACITY_SUBSCRIPTION/RESERVATION/AVAILABLE(+_FEE/_PRICE), CURVE
(VoltVar/VoltWatt points), OLS, ALERT_* (GRID_EMERGENCY, BLACK_START, POSSIBLE_OUTAGE,
FLEX_ALERT, FIRE...), CTA2045 passthroughs.
Report payloadType enums (Table 2): READING, USAGE, DEMAND, SETPOINT, DELTA_USAGE,
BASELINE, OPERATING_STATE (NORMAL/ERROR/IDLE_*/RUNNING_* CTA-2045 states),
UP/DOWN_REGULATION_AVAILABLE, REGULATION_SETPOINT, STORAGE_USABLE_CAPACITY,
STORAGE_CHARGE_LEVEL, STORAGE_MAX_CHARGE/DISCHARGE_POWER, SIMPLE_LEVEL, USAGE_FORECAST,
STORAGE_DISPATCH_FORECAST, LOAD_SHED_DELTA_AVAILABLE, GENERATION_DELTA_AVAILABLE,
DATA_QUALITY (OK/MISSING/ESTIMATED/BAD), IMPORT/EXPORT_RESERVATION_CAPACITY/_FEE.
Private strings + ignored extra JSON fields are the sanctioned extension mechanisms.

VTN response codes: GET 200/400/403/404/500; POST adds 201+409; errors carry an RFC7807
`problem` object. VTN must support skip/limit pagination and additive target filtering.

## Certification profiles

- Only Alliance-certified products may claim OpenADR compliance; testing at authorized
  test providers, DUT is either side of the VTN⇄VEN interface.
- **2.0a VEN**: EiEvent limited profile + simple-HTTP pull + standard security.
  Still certifiable but "not recommended" (2025 READ ME).
- **2.0b VEN**: EiEvent full + EiReport + EiRegisterParty + EiOpt + oadrPoll; HTTP or
  XMPP; standard security mandatory, XML-signature high security optional; "Report Only"
  VEN sub-profile exists. Conformance = spec + PICS + rule set (~2.0a rules 1-68 plus
  b-rules through 51x).
- **OpenADR 3**: certification is per **Certification Profile**; VEN must implement ≥1:
  1. **Continuous Pricing (CP)** — price receiving, GHG, emergency alert VEN;
  2. **Baseline Profile (BP)** — "general flexibility system" (fuller event/report set).
  (More profiles planned; profile-by-profile detail is member-only test-spec material,
  not in the public package.)
- **VTNs**: commercial VTNs must implement ALL features and profiles — after a 6-month
  grace from initial 3.x profile publication that means 2.0b + 3.x (2.0a dropped);
  closed-system VTNs may certify 2.0b-only or 3-only. (VTN side irrelevant to the hub
  unless it ever exposes a downstream local VTN.)

## Mapping to existing hub capabilities

The hub already has the *consuming* half of nearly every OpenADR signal class; what is
missing is purely the northbound VEN protocol adapter.

| OpenADR signal (3.x / 2.0b) | Existing hub seam |
|---|---|
| PRICE / ELECTRICITY_PRICE, EXPORT_PRICE, day-ahead interval prices | `Engine.SetPrices(importPrices, exportPrices []float64)` (engine.go:295, async cmdCh); planner's per-step price vectors; `TOUCostModel` fallback (`SetFallbackTOU`, costmodel.go). An OpenADR price event's intervals map ~directly onto the plan-step price arrays that PR-A..E just wired for plan economics. Zone caveat WS-8/GAP-05 applies: OpenADR interval starts are absolute RFC3339 timestamps — actually *cleaner* than TOU hour-of-day rules. |
| DEMAND_CHARGE / delivery adders | `SetDeliveryTariff(delivery *TOUCostModel, fixedDaily, currency)` (engine.go:391, PR-B/D) |
| IMPORT_CAPACITY_LIMIT / EXPORT_CAPACITY_LIMIT, LOAD_DISPATCH setpoint | `GridState.ImportLimitW/ExportLimitW` (model.go:169-170) → export/import limit rules + convergence backstops in optimizer.go and constraint/export.go, importlimit.go. Today fed from CSIP DERControl via retained `lexa/csip/control`; an OpenADR adapter would publish the same bus message (or a sibling) — `Engine.SetDERConstraints` for step-scheduled caps. |
| SIMPLE levels 0-3 / LOAD_CONTROL offsets | No direct equivalent — needs a small policy table mapping level→(import cap % / battery bias), then same paths as above. |
| CHARGE_STATE_SETPOINT, storage dispatch | `SetBackupReserve`, `SetEVGoal`, battery charge/discharge levers in the DP planner |
| ALERT_GRID_EMERGENCY / ALERT_BLACK_START | Closest analog: compliance tier of the constraint stack (AD-007 safety>compliance>economics); would enter as a max-priority constraint/event |
| GHG signal, localPrice | Plan-economics layer could carry it, but nothing consumes GHG today |
| Event lifecycle (modificationNumber, cancel, implied cancel, randomizeStart) | New logic; analogous in spirit to `internal/northbound`'s DERControl scheduler (start/duration/supersede, fail-closed last-known-good — rule 6) so the discipline exists in-house |
| Opt-out / CannotComply | `breachEpisodes` (cmd/hub/breach.go) already emits one edge-triggered ComplianceAlert per episode → northbound `responses.Tracker` POSTs 2030.5 CannotComply. Same evidence stream could drive oadrCreatedEvent optOut (2.0b) or a report/opt update (3.x). |
| TELEMETRY_USAGE / USAGE / DEMAND / READING reports | `lexa/measurements/{device}` (lexa-modbus publisher) and lexa-telemetry's existing MUP posting loop — a 3.x report POST is a JSON re-encode of what telemetry already ships to 2030.5 MUP |
| TELEMETRY_STATUS (oadrOnline, oadrManualOverride) / OPERATING_STATE | `lexa/reconcile/{class}/{device}/report` retained reconcile reports + device registry connectivity |
| STORAGE_CHARGE_LEVEL / STORAGE_MAX_(DIS)CHARGE_POWER | `lexa/battery/{device}/metrics` |
| USAGE_FORECAST / STORAGE_DISPATCH_FORECAST / LOAD_SHED_DELTA_AVAILABLE | Daily plan + `SetSolarForecast`/`SetLoadProfile` inputs; plan snapshot already carries per-step battery/grid trajectories |
| ven/resource objects, MAX_POWER_CONSUMPTION/EXPORT attributes | hub.json `devices[]/stations[]` + plantmodel (TASK-057) supply nameplate data |
| OAuth2 client-credentials + bearer | New, but trivial (token POST + refresh-on-401); stdlib net/http suffices — OpenADR has NO CSIP-style cipher pin, so **no wolfSSL/CGo involvement**; hub's `tlsclient` is not needed. Cert-expiry monitor pattern (certmon) reusable if CERTIFICATE MQTT auth is used. |
| 3.1 MQTT notifier (VEN as MQTT subscriber) | paho + `internal/mqttutil` already in-tree (note: would be a SECOND broker connection, external, MQTTS — keep isolated from the internal bus and its ACL) |
| mDNS discovery of local VTN | grandcat/zeroconf already a dependency (northbound DNS-SD) |
| Poll-loop/watchdog/backoff discipline | `internal/northbound/run` walk loop, rewalk single-flight, watchdog Kick pattern — clone-able for a lexa-openadr service |

Architecture fit: a new `lexa-openadr` systemd service (own broker creds + ACL row per
AD-008) mirroring lexa-northbound's shape: poll VTN → translate events → publish a
retained control message on the bus → hub adopts via the same staleness-checked path
(TASK-042 pattern); reports fed by subscribing to the same measurement topics
lexa-telemetry uses. Priority arbitration between concurrent CSIP DERControl and OpenADR
events would need an explicit policy (constraint stack tiers are the natural home).

## Implementation-size assessment per profile

- **2.0a VEN — small-to-medium, poor fit.** One service, one signal type (level 0-3),
  XML payloads against the EI subset schema, HTTP-POST pull loop, mutual-TLS client
  certs from Alliance CAs. Cost drivers: XML/XSD data binding in Go (no in-tree XML
  infra; hand-rolled structs feasible for ~4 payload types) + the ~50 event-state
  conformance rules. But: no prices, no reports, no dispatch — delivers almost none of
  the hub's value, and the Alliance itself discourages new 2.0a. Est. 3-6 kLOC + cert
  logistics. Skip unless a specific utility program demands it.
- **2.0b VEN — large.** Four services + oadrPoll: full signal catalog, the
  register/request/update/cancel reporting engine with METADATA capability model and
  ≥100-point history buffer, in-band registration state machine, EiOpt schedules, TLS
  1.2 fixed suites + Alliance PKI, optional XML signatures (detached, canonicalization,
  ReplayProtect — painful in Go; only worth it if a target program mandates High
  security). Dominant cost = EI XML schema binding + conformance-rule state machinery
  (the b-rule set runs to 100+ pages with the PICS). Est. 15-25 kLOC, multi-month, plus
  member-only PICS/test-tool access and certificate procurement. Only justified if a
  target utility explicitly requires certified 2.0b.
- **3.x VEN — small, best fit.** JSON/REST against ~10 endpoints the VEN actually uses
  (GET programs/events, POST/PUT reports, POST subscription or MQTT notifier, POST
  vens/resources, auth/server+token), ~30 flat schemas, OAuth2 client-credentials,
  standard TLS (stdlib; pure Go, cross-compiles CGO_ENABLED=0). Pull-only VEN (poll
  GET /events?active) is legitimate and avoids any inbound listener; 3.1's MQTT
  notifier is an optional latency upgrade using in-tree paho. Certification needs only
  ONE profile — **Continuous Pricing** (price/GHG/alert receive) aligns almost 1:1 with
  the existing SetPrices/SetDeliveryTariff/plan-economics work; Baseline Profile adds
  the report/dispatch machinery later. Est. 3-5 kLOC for a CP-shaped VEN service incl.
  bus wiring; the genuinely new logic is the event lifecycle (modificationNumber-less in
  3.x — UPDATE/DELETE notifications + poll reconciliation, randomizeStart) and
  OpenADR→bus signal translation policy. Recommendation: if OpenADR is added, target
  3.1 VEN / Continuous Pricing first; treat 2.0b only as a market-driven follow-on.
