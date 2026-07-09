# LEXA Hub — From V1.0-RC to a Field-Deployable Product Ecosystem

Prepared 2026-07-07 · **Revised 2026-07-08** against `dsizzle83/lexa-hub@main`
(post-TASK-064 — one merge past the V1RC gate the original draft assumed).
This document covers everything around the hub: cloud, app, commissioning,
fleet operations, new features, and the compliance-gateway mode. The
device-side work it implies is broken out, with code-level design, in the
companion **`docs/DEVICE_ROADMAP.md`** — that is the implementation contract;
this document is the ecosystem contract.

---

## Revision notes (2026-07-08)

Substantive edits from the 2026-07-07 draft, in decreasing order of consequence.
Everything else is refreshed status or light copyedit.

- **R1 — Intent transport redesign (§6).** The draft's single retained
  `lexa/cloud/intent` topic is a design bug: MQTT retention is
  last-writer-wins *per topic*, so a forecast intent would clobber the
  retained mode intent. Intents are now **one retained topic per kind**
  (`lexa/intent/{kind}`) for state-like intents — mirroring the
  `lexa/desired/{class}/{device}` pattern, so the hub re-seeds intent state
  from retained docs after a restart exactly like it re-seeds desired state —
  plus non-retained topics for edge intents ("charge now"), which carry a
  mandatory TTL so a command stuck behind a WAN outage cannot fire hours
  late. Adoption is staleness-checked and ID-deduped (the TASK-042 retained-
  control discipline, generalized). `lexa-api`'s local `POST /intent`
  publishes to the *same* topics, so local and cloud commands stay one
  mechanism.
- **R2 — Gateway mode rides the constraint stack (§14).** The draft's
  `gatewayAuthor` as a 1:1 CSIP→desired-doc mapper is not implementable as
  described: the wire `bus.ActiveControl` carries **no storage setpoints**
  (only `FixedW`), a **single** `Connect` flag (no separate energize), and —
  critically — `ExpLimW`/`ImpLimW` are *site-level meter envelopes* with no
  per-device mapping. Enforcing them requires exactly the envelope→device
  allocation plus measured-convergence backstops that
  `internal/orchestrator/constraint` already implements at `TierCompliance`
  (Export/GenLimit/ImportLimit, complete and 0-divergence in shadow since
  TASK-060–064). Gateway mode is therefore **the constraint Stack with the
  economics tier swapped for a CSIP passthrough constraint** — reusing
  shadow-validated code instead of writing a third enforcement path. A
  passthrough-only interim (MaxLimW/FixedW/Connect, no site envelopes) is the
  documented fallback if stack-actuation validation slips.
- **R3 — Discovery scanner reality check (§2, §9).** The draft credits an
  existing "address-sweep scanner." What exists (`lexa-proto/sunspec/scanner.go`)
  is a *within-one-device* SunSpec model-block walk. There is **no** bus
  address sweep and **no SunSpec model-1 (Common) identity reader anywhere**
  — nothing today can report manufacturer/model/serial. Both are new work,
  and they belong in `lexa-proto` (shared with the bench sims), which makes
  them **paired-PR / proto-pin-bump changes** under the MTR-4 lockstep rule.
- **R4 — Secure-element vs pure-Go cloudlink tension (§5, §17).** The draft
  simultaneously requires cloudlink to be `CGO_ENABLED=0` and the cloud
  device key to live in an ATECC608B. An SE-backed TLS key needs a
  `crypto.Signer` backed by cryptoauthlib or the CAAM — CGo either way.
  Resolution: **v1 keeps the cloud key as a file** on the encrypted/
  TrustFence-protected data partition (cloudlink stays pure Go and
  cross-compiles like modbus/hub/api); the SE/CAAM-backed key is a decision
  for the next PCB rev, taken with eyes open in ADR-0002.
- **R5 — Status refresh.** TASK-064 merged 2026-07-07: the R4 constraint
  stack is complete in shadow and the bench-calibrated constants are
  parameterized through the `plant` model (read by the constraint package;
  the `optimizer.go` cascade keeps its burned-in copies and remains the
  authoritative author until the P5 flips — both halves preserved by
  design). V1RC FINDING A (StartLimit/`Wants=mosquitto`) and FINDING D
  (`StateDirectory=lexa`) are **fixed in tree** (`c571419`, `29f0ddb`),
  pending one re-confirmation deploy; FINDING B's fix vector (adaptive breach
  window from the plant model) merged with TASK-064 and needs the
  `export-dither-at-breach` scenario re-run. The ConnectCore dev kit is the
  **live hub again** (2026-07-07, fresh custom DEY image) — and the
  `~/projects/meta-lexa` Yocto layer + `dey-image-lexa` recipe now exist,
  which converts §4.5's "you own the Yocto integration" from a cold start
  into an anchor for `meta-mender`.
- **R6 — Clock trust at boot (§8, §10, §16).** A field unit that boots with
  a dead RTC and no NTP fails TLS (cert NotBefore) and misprices TOU with no
  alarm. Added: RTC backup battery as a hardware requirement, `chrony` +
  `After=time-sync.target` ordering on the wolfSSL services, a
  clock-sanity gate in the OTA boot-success criteria, and NTP reachability in
  the commissioning functional test. (GAP-05 timezone enforcement stays as
  drafted.)
- **R7 — Local API TLS trust model (§7).** `lexa-api` today is plain HTTP +
  bearer token; the draft said "HTTPS with per-device credentials" without a
  trust story. Added: per-device self-signed cert generated at first boot,
  fingerprint uplinked post-claim so the app pins it via the cloud; TOFU with
  fingerprint display for pre-claim commissioning; the browser dashboard is
  documented as cloud-transport-only (browsers can't pin).
- **R8 — OCPP pairing gate (§9).** Today an unknown charger that dials the
  CSMS is silently auto-created as a live, tracked-but-uncontrolled station
  (no station-ID allowlist; Basic Auth is the only gate). The commissioning
  design adds a pending-approval surface rather than treating this as
  hypothetical.
- **R9 — IoT Core constraints (§5).** Batching must respect the 128 KB
  message cap; QoS 0/1 only; persistent sessions expire — the disk spool, not
  broker queueing, is the outage story (as designed, now stated).
- **R10 — Plan-author collision risk (§14, §19, §20).** The program now has
  three prospective plan authors in flight (legacy cascade — authoritative;
  constraint stack — shadow, P5 flips pending; gateway author). Sequencing
  rule added: never validate two *new* authors on the bench simultaneously;
  gateway mode lands after (or as the first consumer of) the stack's
  compliance-tier flip, per R2.
- **R11 — Commissioning requires a config-write path (§8).** "The wizard
  writes the same config files" implies an authenticated local config-write +
  service-restart mechanism that does not exist today. Scoped in
  DEVICE_ROADMAP (uncommissioned/installer-gated `POST /config`, sudoers
  fragment for unit restarts); called out here because it is the single
  biggest new attack surface the app work introduces.
- **R12 — Tariff landing point (§13).** `PlannerParams.FallbackTOU` +
  the optimizer's cost model are the correct injection points for
  user-entered tariffs (used whenever CSIP pricing is absent); utility CSIP
  pricing continues to win via the existing `SetPrices` path. The draft's
  "SetPrices exists but nothing feeds it" undersold what exists: the
  `lexa/csip/pricing` → `SetPrices` path is live; what's missing is the
  *manual/user* tariff source.

---

## 1. Executive summary

The hub firmware is in better shape than most commercial DER gateways at
launch: six isolated services on an MQTT bus with per-service ACLs, a
three-tier safety hierarchy, desired-state reconcilers as the single command
path, CSIP/IEEE 2030.5 northbound over wolfSSL mTLS, OCPP 2.0.1 SP2,
watchdogs, cert rotation, compliance journaling, a hostile-QA bench — and,
as of TASK-064, a complete constraint-controller stack running in shadow.
What does not exist yet is the product ecosystem: no product cloud (telemetry
goes only to the utility's MUP endpoint), no customer app (only the
bearer-token `/status`/`/logs` adapter), no OTA update system (deploys are
SSH scripts), no installer commissioning flow (configs are hand-edited JSON),
no manufacturing identity/PKI story, and no user-selectable operating mode.

The plan below builds that ecosystem in six phases over roughly 9–15 months
(solo founder + Claude Code + selective contracting), ordered so that OTA and
device identity come first — because every subsequent feature ships through
them — and so that a 10–50 unit pilot fleet is running real homes by month
5–6, generating the telemetry the forecasting and battery-analytics features
need to be any good.

Three architectural principles carry through everything:

1. **The hub's internal invariants are preserved, not bypassed.** The cloud,
   the app, and the new gateway mode all interact with the system through the
   same seams the codebase already defends: retained desired documents
   authored by exactly one writer, intents flowing through the engine's
   `cmdCh`, and Tier-0/Tier-1 protection that no mode can disable.
2. **The box must degrade gracefully to standalone operation.** Cloud outage,
   expired forecast, or a dead app must never change safety or compliance
   behavior — the same fail-closed discipline the northbound already
   practices extends to every new dependency.
3. **Buy the undifferentiated parts.** Managed MQTT ingest, managed Postgres,
   an off-the-shelf OTA system, and a hybrid app framework keep the surface
   you personally maintain small. Your differentiation is the control plane
   on the box, not a bespoke cloud.

## 2. Where the codebase is today

An honest inventory, because the roadmap's sequencing depends on it.

**Already product-grade (keep, don't rebuild):**

| Area | State |
|---|---|
| Northbound CSIP | Discovery walker, pricing/billing/flow reservations, CannotComply episodes, DNS-SD browse of the utility server, cert expiry monitoring + zero-downtime rotation (`Reload` seam), wolfSSL CCM-8 per CSIP §5.2.1.1 |
| Southbound | SunSpec/Modbus poller with plausibility gating, within-device SunSpec model-block scanner (`lexa-proto/sunspec/scanner.go` — **not** a bus sweep; see §9/R3), Tier-0 battery interlock, OCPP 2.0.1 CSMS with Security Profile 2 |
| Control | Desired-doc reconcilers (battery/solar/EVSE) as sole command path, reassert-on-reconnect, closed-loop convergence checks, 3-tier safety split per ADR-0001 |
| Optimizer | Receding-horizon 288×5-min daily planner with battery and EV departure/target-SOC modeling already implemented (`PlannerParams.EVTargetSocKwh`, `EVDepartureUnix`), TOU cost model, `SetPrices`/`SetDERConstraints` async command channel — **plus** the full constraint stack (safety > compliance > economics, plant-model-parameterized) validated in shadow (TASK-059–064) |
| Ops hygiene | systemd watchdogs on all six services, journald caps + flash budget, compliance journal + snapshots, Prometheus `/metrics` everywhere, MQTT broker ACLs, crash-only design |
| QA | 42-scenario Mayhem suite, conformance suites, fuzzing, govulncheck in CI, V1RC gate report |

**Gaps this document addresses:**

| Gap | Current state |
|---|---|
| Product cloud & long-term telemetry | lexa-telemetry posts MUP readings to the utility's 2030.5 server only. Nothing reaches you or the customer. |
| Customer/installer app | lexa-api serves `/status` + `/logs` with a bearer token, plain HTTP. Read-only, LAN-only, engineer-facing. |
| OTA updates | `scripts/deploy-hub-pi.sh` over SSH. No A/B, no signing, no fleet rollout, no rollback. (The `meta-lexa` Yocto layer + `dey-image-lexa` recipe now exist — the Mender integration has an anchor.) |
| Commissioning | `make install-configs` + hand-edited `/etc/lexa/*.json`. Bus discovery and SunSpec identity reading don't exist (R3). GAP-05 (SOM timezone must match tariff zone) is a documented deployment requirement with no enforcement. |
| Device identity at scale | Bench certs staged from `csip-tls-test/certs/client-staging/`. No manufacturing PKI, no per-unit claim mechanism, no secure-element usage. |
| Operating modes | The optimizer always runs; CSIP controls enter as constraints. No user-selectable "pure gateway" mode. |
| Solar forecast | Clear-sky diurnal bell scaled by an observed high-water mark (`buildPlannerParams`, engine.go:369–385). The per-step hook (`SolarForecastKw []float64`) is exactly where real weather data plugs in — but note the diurnal block assigns it *unconditionally* today; an external forecast must gate it. |
| Load forecast | Flat constant (`LoadForecastKw float64` — noted "Flat for now"). |
| Tariffs | Hardcoded `DefaultTOUCostModel()`; the CSIP pricing→`SetPrices` path is live, but there is no manual/user tariff source (R12). |
| Battery health surfacing | Pack metrics flow on `lexa/battery/{device}/metrics` (incl. a BMS `soh_pct` field already on the wire) for the hub's use; no SoH trending, no customer-visible history. |
| Aggregator API | None. |

**V1RC loose ends to close first (Phase 0)** — status as of 2026-07-08:
FINDING A's two-part fix and FINDING D's `StateDirectory=lexa` are committed;
what remains is the **re-confirmation deploy** (re-run
`power-cut-retained-rollback`, assert lexa-api stays active), the ~40 s
retained-rollback export-breach tightening (042 tuning), and re-running
`export-dither-at-breach` to confirm TASK-064's adaptive breach window closed
FINDING B.

## 3. Target ecosystem architecture

```
                                   ┌──────────────────────────────────────────────┐
                                   │                PRODUCT CLOUD                 │
                                   │                                              │
   Utility / DERMS                 │  MQTT ingest (per-device X.509 mTLS)         │
   IEEE 2030.5 CSIP server         │   ├─ ingest workers → TimescaleDB (telemetry,│
        ▲                          │   │    device registry, sites, users, orgs)  │
        │ wolfSSL mTLS             │   ├─ S3/object store (journals, diag bundles,│
        │ (unchanged)              │   │    long-term Parquet archive)            │
        │                          │  Command service → per-device cmd topic      │
        │                          │  Forecast service (weather APIs + PV model)  │
        │                          │  Tariff service (manual + OpenEI URDB)       │
        │                          │  OTA server (Mender or equivalent)           │
        │                          │  Public REST API + OAuth2 (aggregators)      │
        │                          │  App/backend API (customers & installers)    │
        │                          │  Grafana + alerting (your fleet ops)         │
        │                          └───────▲──────────────────────▲───────────────┘
        │                                  │ MQTT/TLS 8883        │ HTTPS
        │                                  │ (standard Go TLS)    │
┌───────┴──────────────────────────────────┴────────┐   ┌─────────┴─────────┐
│                LEXA HUB (Digi SOM)                 │   │  Apps             │
│                                                    │   │  ├─ Customer app  │
│  mosquitto (localhost, per-service creds + ACL)    │   │  │  (Capacitor:   │
│   ├─ lexa-northbound   (CSIP client — unchanged)   │   │  │  iOS/Android/  │
│   ├─ lexa-modbus       (SunSpec poll + reconcile   │   │  │  web)          │
│   │                     + commissioning scan)      │   │  ├─ Installer    │
│   ├─ lexa-ocpp         (OCPP 2.0.1 CSMS)           │   │  │  mode in same │
│   ├─ lexa-hub          (mode mgr: optimizer │ gw)  │   │  │  app          │
│   ├─ lexa-telemetry    (MUP to utility — unchanged)│   │  └─ lexactl CLI  │
│   ├─ lexa-api          (local HTTPS API + mDNS)    │◄──┤     (power user) │
│   └─ lexa-cloudlink    (NEW: cloud agent,          │LAN└───────────────────┘
│        store-&-forward, cmd validation, OTA hooks; │
│        writes lexa/intent/{kind} — same topics     │
│        lexa-api's POST /intent uses)               │
│                                                    │
│  Modbus RTU/TCP ──► inverter, battery, meter       │
│  OCPP wss:8887  ◄── EV chargers                    │
└────────────────────────────────────────────────────┘
```

One new on-box service (`lexa-cloudlink`), consistent with the existing
one-process-per-concern pattern and MQTT ACL model (metrics on `:9106`,
`Type=notify`, its own broker user). Everything else new lives in the cloud
or the app. The northbound/utility path is untouched — the product cloud is a
parallel northbound, never in the compliance path.

## 4. Key decisions, with pros and cons

### 4.1 Cloud posture: managed IoT platform vs. self-hosted stack

| Option | Pros | Cons |
|---|---|---|
| **A. AWS IoT Core + managed Postgres (recommended)** | Per-device X.509 mTLS device auth out of the box — matches the PKI competence you already have; MQTT broker, connection lifecycle events, and device shadow/jobs primitives managed; scales 10→100k devices without re-architecture; pay-per-message is nearly free at pilot scale | Vendor coupling at the ingest edge; IoT Core policy/rules learning curve; per-message pricing needs watching at high telemetry rates (mitigate by batching); 128 KB payload cap and QoS 0/1-only shape the uplink design (R9) |
| B. Self-hosted EMQX/Mosquitto + your own auth on a VPS | Cheapest at small scale; full control | You become a 24/7 broker operator (patching, HA, cert distribution, DDoS surface); identical Go application code either way, so savings are small |
| C. Full IoT PaaS (Balena, Golioth, Digi Remote Manager) | Fastest to a demo; DRM is native to your ConnectCore SOM (evaluate it — it bundles OTA + monitoring) | Per-device fees scale badly; data model/API shaped by their product; harder to build the customer app and aggregator API on top; migration later is painful |

**Recommendation: A**, with a portability discipline: the ingest edge is AWS
IoT Core, but everything behind it (Go services, TimescaleDB, S3-compatible
object storage, Grafana) is portable. Evaluate Digi Remote Manager narrowly
for OTA (§4.5) — now unblocked, since the dev kit is the live hub again —
but don't build the customer data plane on it.

### 4.2 Time-series storage

| Option | Pros | Cons |
|---|---|---|
| **TimescaleDB (recommended)** | One database for relational data (users, sites, devices, tariffs) *and* telemetry; plain SQL; continuous aggregates give 1-min/1-hour rollups declaratively; compression ~10–20×; runs anywhere; Grafana-native | You manage retention/rollup policy yourself; very high-cardinality fleets eventually need care |
| InfluxDB | Purpose-built TSDB | Second database alongside Postgres anyway; Flux/IOx churn history; weaker joins |
| AWS Timestream | Serverless, zero ops | Query-pricing surprises; SQL dialect limits; lock-in for the one component that most benefits from portability |
| ClickHouse | Astonishing analytics performance | Overkill below ~10k devices; operationally heavier; still need Postgres |

**Recommendation: TimescaleDB.** A 5-second measurement cadence per device is
trivial for it; one `measurements(site_id, device_id, ts, field, value)`
hypertable plus continuous aggregates covers the app, the analytics, and the
aggregator API.

### 4.3 Device↔cloud link: mosquitto bridge vs. dedicated agent

| Option | Pros | Cons |
|---|---|---|
| Mosquitto's built-in bridge | Zero new code; reuses broker reconnect logic | No store-and-forward beyond broker queue limits (flash-unaware — collides with FLASH_BUDGET.md); no payload batching/compression; no place to validate inbound cloud commands before they touch the bus; no OTA/diag orchestration hooks; bridge auth becomes a special case in the ACL model |
| **lexa-cloudlink service (recommended)** | Fits the existing pattern exactly: own broker user, ACL derived from call sites, own watchdog, `Type=notify`, `/metrics`; disk-spooled store-and-forward with an explicit byte budget (respecting flash wear); batches + compresses uplink (cuts IoT Core message count 10–50×); single choke point where cloud commands are authenticated, schema-versioned (`bus.Envelope`), TTL-checked, and translated into **intents, never raw desired docs** | ~2–4 weeks of new Go, plus tests |

**Recommendation: the dedicated agent.** Critically, it uses standard Go
`crypto/tls`, not wolfSSL — CCM-8 is a CSIP requirement, not a cloud one — so
it stays `CGO_ENABLED=0` and cross-compiles trivially like modbus/hub/api do.
That constraint drives the v1 identity-key decision in §17/R4.

### 4.4 App stack

| Option | Pros | Cons |
|---|---|---|
| **Capacitor + web app (React or Svelte) — recommended** | One codebase yields iOS, Android, and the browser dashboard; plugins cover the two native needs (mDNS/zeroconf discovery, BLE later); largest contractor talent pool; Claude Code is very strong at this stack | Not fully native feel (fine for an energy dashboard); plugin quality varies — prototype the mDNS plugin on iOS early (iOS 14+ local-network permission + declared Bonjour service types in Info.plist) |
| React Native / Expo | Closer-to-native UX | Separate web dashboard build; more mobile-specific expertise assumed |
| Flutter | Excellent cross-platform consistency | Dart is a new language for the project; web output is heavyweight |
| Pure PWA | No app stores | iOS PWAs cannot do mDNS or BLE, limited push — kills the commissioning story |

**Recommendation: Capacitor.** Ship the same bundle three ways; installer
mode is a role flag in the same app, not a second app.

### 4.5 OTA system

The highest-stakes infrastructure decision: a bricked fleet is the failure
mode that kills hardware companies.

| Option | Pros | Cons |
|---|---|---|
| **Mender (open source, recommended)** | Industry-standard A/B full-rootfs updates with automatic rollback on failed boot; `meta-mender` integrates with the Yocto build Digi Embedded Linux is based on — and the `meta-lexa` layer + `dey-image-lexa` recipe already exist (R5), so this is layer-integration, not greenfield; signed artifacts; delta updates; phased rollouts; self-host free or hosted tiers | You own integrating `meta-mender` with Digi's BSP + TrustFence secure-boot chain (real but well-trodden); self-hosting is one more service |
| Digi Remote Manager | Vendor-native to the ConnectCore; TrustFence integration documented; support contract possible | Per-device pricing; couples fleet management to Digi's cloud; verify A/B + auto-rollback semantics match Mender's before committing |
| RAUC + hawkBit | Very flexible, fully open | You assemble more of the pipeline yourself |
| SWUpdate / custom | Maximum control | Custom OTA is how fleets get bricked; do not hand-roll this |

**Recommendation: Mender, self-hosted initially**, unless the two-week DRM
evaluation on the dev kit shows signed A/B + auto-rollback + phased rollout
at acceptable pricing. Either way the requirements are fixed: dual rootfs
slots; cryptographically signed artifacts; **boot-success confirmation gated
on the services actually coming up** (a health gate that checks `Type=notify`
READY across units, the plan heartbeat, a northbound walk-or-cleanly-idle,
modbus device visibility, *and clock sanity* — R6 — implemented as an on-box
`lexa-healthcheck`, see DEVICE_ROADMAP §8); automatic rollback; staged
rollout (1 → 10 → 10% → 100%); and config/certs/journals on a persistent
data partition (`/etc/lexa`, `/var/lib/lexa`) outside both slots.

### 4.6 Aggregator-facing API surface

| Option | Pros | Cons |
|---|---|---|
| **Cloud REST/OpenAPI + OAuth2 + webhooks (recommended primary)** | What DR aggregators and VPP platforms actually integrate against; you define semantics (sites, setpoints, schedules, priorities); works fleet-wide without touching boxes; sandboxable against your existing simulators | You must define and version a public contract |
| Point the box's native 2030.5 client at the aggregator's CSIP server | Zero new code — the northbound already speaks it; CSIP contemplates aggregator-operated servers; this is the gateway-mode story | Per-device only; requires the aggregator to run a CSIP head-end (many can't); single-server today — multi-head-end arbitration is real added scope |
| OpenADR 3.0 VEN in the cloud | Standard for utility DR programs | A whole protocol stack for programs you haven't signed — defer until a contract requires it |

**Recommendation:** build the REST API as the product surface; document "or
operate your own IEEE 2030.5 head-end and we enroll natively" as the
standards path for sophisticated partners. Add OpenADR only when a contract
demands it.

### 4.7 Solar forecast data source and placement

| Option | Pros | Cons |
|---|---|---|
| **Open-Meteo (recommended start)** | Free for non-commercial, inexpensive commercial tier; GHI/DNI/DHI + cloud cover at hourly/15-min resolution; no per-site contracts | Good-not-best radiation accuracy; you do plane-of-array transposition yourself |
| Solcast | Purpose-built PV forecasting, per-site power estimates directly | Paid per site at scale; another vendor in the value path |
| NOAA/NWS (US) | Free, authoritative | No direct irradiance in the friendly API; more assembly |

**Where it runs: in the cloud, not on the hub.** API keys never ship in field
units; one fetch per site instead of N boxes hammering a vendor; fleet-wide
model improvement is only possible where the fleet's data is; and the hub
already has the perfect degraded mode — no fresh forecast ⇒ fall back to the
clear-sky diurnal curve, exactly the fail-closed pattern used everywhere
else. The hub treats the forecast like it treats CSIP control:
staleness-checked, clamped to plausible peak from the plant model, never
blindly trusted.

**Model progression:** v1 is physics (Open-Meteo GHI/DNI/DHI →
plane-of-array transposition via pvlib's algorithms — run pvlib in a small
Python cloud service, or port the ~200 lines of Perez/HDKR math to Go → DC
power via array kWp/tilt/azimuth/temperature coefficient captured at
commissioning → AC via inverter limit). v2, once the pilot fleet has 2–3
months of history: a per-site ML residual correction that typically halves
pure-physics error. v2 is only possible because telemetry lands in your
TimescaleDB — which is why the cloud comes before the feature.

## 5. Workstream A — Product cloud

**Identity and enrollment.** Each unit gets a per-device X.509 keypair at
manufacturing (§17). v1: the private key is a file on the TrustFence-
protected data partition (R4 — keeps cloudlink pure Go); an ATECC608B or
CAAM-backed key is a next-PCB-rev decision recorded in ADR-0002. The device
certificate is the MQTT credential to IoT Core; certificate CN = device
serial. This is a **separate identity from the CSIP LFDI cert** (utility PKI,
enrolled per-program at commissioning) — do not conflate them; they rotate on
different authorities and schedules. The existing certmon/rotation machinery
generalizes: cloudlink runs the same monitor-and-report pattern for its own
cert family (DEVICE_ROADMAP §2.6).

**Claiming.** A device is bound to a customer account by a claim code printed
as a QR on the unit label (serial + one-time claim secret). The app scans it,
the backend verifies and attaches the device to the site/org. Unclaimed
devices can connect (factory burn-in, RMA diagnostics) but publish to a
quarantine namespace.

**Tenancy model.** org (installer company or fleet operator) → site (home/
building: address, lat/lon, timezone, tariff, utility program) → hub (the
unit) → devices (inverter/battery/meter/EVSE rows mirrored from the hub's
registry). Roles: homeowner (their site), installer (sites their org
commissioned, time-boxed post-handoff), fleet_operator (org-wide), support
(you, audit-logged). This model is exactly what the aggregator API scopes
against later — get it right in the schema now.

**Ingest pipeline.** Cloudlink publishes batched, gzipped JSON (or CBOR,
decide in ADR-0002; either way ≤128 KB per message, R9) to
`lexa/v1/{deviceSerial}/telemetry`; an IoT Core rule forwards to a queue; a
small Go ingest worker fans out to TimescaleDB. Target uplink content, all of
which already exists on the internal bus:

| Uplink stream | Source topic on box | Cadence |
|---|---|---|
| Measurements (per device: W, V, Hz, SoC, SoH…) | `lexa/measurements/{device}`, `lexa/battery/{device}/metrics` | batch every 30–60 s (raw 5–10 s samples inside the batch) |
| EVSE session state | `lexa/evse/{station}/state` | on change + 60 s |
| Plan/heartbeat + mode | `lexa/hub/plan` (retained), `lexa/hub/mode` (retained, new) | 5 min + on change |
| Compliance events | `lexa/csip/compliance/alert`, `lexa/reconcile/+/+/report` | on edge, QoS 1, spool-priority |
| Intent audit | `lexa/intent/result` (new) | on edge, QoS 1 |
| Cert status, service health, versions | `lexa/northbound/certstatus`, `lexa/cloudlink/status`, `/metrics` scrape summary | 15 min |
| Diagnostic bundle (on request) | journal dirs under `/var/lib/lexa/journal/*` | on demand → S3 presigned upload |

**Retention.** Raw samples 30–90 days in the hypertable; continuous
aggregates at 1-min (13 months — powers the app's charts) and 1-hour
(indefinitely); nightly Parquet export of raw to S3. Customer-visible history
promise: minute data for 1 year, hourly forever — cheap at these volumes and
a real differentiator against inverter-vendor portals.

**Downlink commands.** One topic per device, `lexa/v1/{deviceSerial}/cmd`,
QoS 1, individually authorized. Commands are a small, versioned vocabulary
that maps 1:1 onto the on-box intent kinds (§6); every command is idempotent,
carries an ID and TTL, and produces an ack on the uplink — modeled like the
reconcile reports, not like RPC.

**Cost reality check.** At 1,000 devices with the batching above: IoT Core
messaging is tens of dollars/month; a 2-vCPU Postgres/Timescale instance is
well under $200/month; S3 archive is negligible. Cloud COGS lands around
$0.30–$1.50 per device per month — worth knowing before pricing monitoring
subscriptions.

## 6. Workstream B — lexa-cloudlink (the seventh service)

Design it as a peer of the existing six, with the disciplines the repo
already enforces. Full code-level design: DEVICE_ROADMAP §2.

- **Broker identity:** its own mosquitto user; ACL grants read on the §5
  uplink set and write on exactly the intent-kind topics + its own status
  topic. Re-derive the ACL matrix from call sites, per the CLAUDE.md rule.
- **Intents, not desired docs — one topic per kind (R1).** Cloud/app
  requests publish as typed intent documents on `lexa/intent/{kind}`:
  *state-like* kinds (`mode`, `evgoal`, `reserve`, `tariff`,
  `solarforecast`, `loadprofile`) are **retained** — the current goal state,
  re-seeded into the hub after restart exactly like desired docs re-seed the
  reconcilers; *edge* kinds (`chargenow`, future one-shots) are
  **non-retained with a mandatory TTL**. Every intent carries an ID, origin
  (`cloud`/`app`/`cli`), actor, and `issued_at`; the hub staleness-checks at
  adoption (TASK-042 pattern), dedupes by ID, journals every accepted intent,
  and publishes an outcome on `lexa/intent/result`. lexa-hub applies them
  through the existing async `cmdCh` pattern (new `SetEVGoal`, `SetMode`,
  `SetBackupReserve`, `SetSolarForecast`, `SetLoadProfile`,
  `SwapCostModel`, alongside the existing `SetPrices`/`SetDERConstraints`).
  **The hub remains the only author of desired docs; the reconcilers remain
  the only writers to hardware.** A cloud compromise can therefore express
  nothing the optimizer+safety stack wouldn't itself emit — the W6/D3
  split-brain foot-gun stays closed, and Tier-0/Tier-1 stay senior to
  everything.
- **Store-and-forward:** disk spool under `/var/lib/lexa/spool/` with an
  explicit byte cap and drop-oldest policy, sized against FLASH_BUDGET.md
  (batched writes + segment-close fsync, never per-sample). WAN outage →
  spool; reconnect → drain oldest-first, **compliance events and intent
  results before bulk measurements**.
- **Command safety:** verify cloud authorization, schema-version gate
  (`bus.Envelope`), reject non-finite numerics (the `Finite()` defense —
  free via `mqttutil.Subscribe`'s existing pipeline), TTL/staleness gate,
  rate-limit, journal every accepted intent (extends the compliance journal —
  the audit trail for utility and customer disputes).
- **Watchdog + metrics:** `Type=notify`, kick tied to "MQTT connected AND
  spool drainable"; `lexa_cloudlink_spool_bytes`,
  `lexa_cloudlink_uplink_fail_total`, connection-state gauge; metrics on
  `127.0.0.1:9106` per the house convention.
- **Config:** `/etc/lexa/cloudlink.json` — endpoint, cert paths, spool
  budget, per-stream cadences, `enabled` flag. A customer who refuses cloud
  connectivity gets a fully functional local-only box; design for that from
  day one — it will come up in commercial deals.

Estimated effort: 2–4 weeks including tests and Mayhem scenarios
(`wan-outage-spool-drain`, `cloud-cmd-forgery-rejected`,
`stale-intent-expired`).

## 7. Workstream C — App and dashboard

**Connection model:** cloud-first (works from anywhere), with a local
fallback for commissioning and cloud outages: the app discovers the hub via
mDNS (§9) and talks to an expanded lexa-api over HTTPS on the LAN. Same UI,
two transports behind a thin client abstraction. lexa-api grows from
`/status`+`/logs` to a small authenticated REST surface (`GET /site`,
`GET /devices`, `GET /telemetry/recent`, `POST /intent` — which publishes to
the same `lexa/intent/{kind}` topics cloudlink uses, so local and cloud
commands are literally the same mechanism with different transports).

**Local TLS trust (R7):** lexa-api serves HTTPS with a per-device self-signed
cert generated at first boot (persisted under `/var/lib/lexa/api/`); the cert
fingerprint is uplinked post-claim so the app pins it via the cloud; during
pre-claim commissioning the installer app does TOFU with the fingerprint
displayed for comparison against the unit label. The browser dashboard uses
the cloud transport only — browsers can't pin a self-signed LAN cert.
The single bearer token becomes a per-unit secret printed on the label,
rotated on claim.

**Homeowner screens (v1):**
1. **Now** — live power-flow diagram (solar → home/battery/grid/EV), current
   tariff period, today's totals.
2. **Battery** — SoC, charging/discharging state, backup-reserve slider (an
   intent), health panel (§12).
3. **EV** — plugged/charging state, target % + departure time (intents
   feeding the planner fields that already exist), "Charge now" override,
   per-session history and cost.
4. **Solar** — production today vs. forecast curve, 7-day history, lifetime.
5. **History** — energy/cost charts from the 1-min aggregates; CSV export.
6. **Grid events** — human-readable log of utility CSIP events ("Utility
   limited export to 3 kW, 4:12–6:00 pm") sourced from the compliance
   journal. Homeowners accept curtailment far better when they can see it —
   and it halves your support load.
7. **Settings** — tariff editor (or utility-plan picker once OpenEI import
   exists), mode selector (§14) behind an "advanced" gate, notifications.

**Installer mode** (same app, role-gated): the commissioning wizard of §8,
device-scan results, signal/comms health, commissioning-report generation,
and a fleet list of their org's sites.

**Notifications (push):** grid event started/ended, battery fault/interlock
trip, hub offline > N minutes, cert expiring (certstatus already publishes —
surface it), OTA applied.

**Effort:** the largest contract-out candidate. A focused 2–3 months for v1
with you + Claude Code on the API and a contractor on UI polish is realistic.

## 8. Workstream D — Commissioning and installation procedure

Target: a licensed electrician who has never seen the product commissions a
site in under 45 minutes without editing JSON. The wizard drives everything
through the local API; every step writes the same config files
`make install-configs` seeds today. **Note (R11):** this requires an
authenticated local config-write + service-restart path that doesn't exist
yet — gated to uncommissioned state or installer role, schema-validated,
journaled; it is the largest new local attack surface and is specified in
DEVICE_ROADMAP §4.

**Factory state (before the truck):** unit has firmware, device identity
cert, claim QR + LFDI QR on the label, DHCP-client networking, mDNS
advertising `claimed=0`, and sits in "uncommissioned" mode (all reconcilers
off, northbound idle, obviously safe).

**The installer flow:**

1. **Mount and wire** — printed quick-start covers power, Ethernet (primary;
   Wi-Fi documented as optional variant), RS-485 daisy-chain
   polarity/termination, CTs. As the PCB designer, silk-screen the A/B/GND
   labeling generously; RS-485 miswiring will otherwise be your #1 support
   call.
2. **Claim** — installer app scans the claim QR → device binds to the site
   record (created in-app with address, lat/lon for forecasting, utility,
   tariff). Backend pushes site metadata down; hub sets its timezone from the
   site's tariff zone — closing GAP-05 with enforcement instead of
   documentation. The wizard hard-blocks completion if TZ, tariff zone, and
   lat/lon are inconsistent, **and verifies NTP sync + RTC validity** (R6).
3. **Discover DERs** — app triggers the commissioning scan (§9): RTU
   unit-ID sweep + TCP subnet probe → SunSpec model-1 identification
   (manufacturer/model/serial) + model-presence classification + nameplate.
   Installer confirms each found device and assigns roles; wizard writes
   `modbus.json` + `hub.json` device/plant blocks from a per-model template
   library (ramp rates, capacity — the TASK-057 plant parameters, seeded from
   a curated device DB you grow over time).
4. **EVSE pairing** — wizard displays the CSMS URL + Basic Auth credentials
   (or a QR) to enter into the charger's own config; unknown inbound OCPP
   connection attempts appear as **pending stations** for the installer to
   approve (R8); wizard confirms the SP2 `wss://` handshake and a MeterValues
   heartbeat.
5. **Utility enrollment (if applicable)** — the out-of-band CSIP step: app
   shows LFDI/SFDI for the utility's registration portal; installer enters
   the utility server URL or relies on DNS-SD discovery; wizard verifies the
   discovery walk completes and dcap fetch succeeds; northbound goes live.
6. **Functional test** — automated sequence with installer watching: read all
   devices; command a small battery charge and verify **metered**
   convergence (the reconcile report is the test oracle); trip and clear a
   simulated export cap; verify EVSE profile acceptance. Green checks only
   when measured effects match — reuse the trust-measurement-not-command
   philosophy.
7. **Commissioning report** — PDF to the site record: device inventory with
   serials/firmware, test results, CSIP enrollment status, installer
   sign-off. Utilities and AHJs ask for exactly this; generating it
   automatically is a differentiator.
8. **Handoff** — installer enters homeowner email → account invite; installer
   access auto-expires (default 30 days, homeowner-extendable).

Also specify: the **RMA/replacement flow** (new unit scans old unit's site QR
→ cloud restores config + re-issues CSIP enrollment guidance) and a
**factory-reset procedure** (physical button hold → wipe configs/certs to
uncommissioned, keep identity).

## 9. Workstream E — Discovery

**App → hub (LAN):** lexa-api registers `_lexa-hub._tcp.local` via mDNS with
TXT records `serial`, `fw`, `claimed=0|1`, `api=https://<ip>:9100`. The repo
already vendors `grandcat/zeroconf` for *browsing* the utility server; the
same library does *registration* (`zeroconf.Register`). Two field cautions:
verify the chosen Capacitor zeroconf plugin against iOS's local-network
permission prompt early (the flakiest part of this stack), and provide the
fallback — the app can always ask the cloud "what's my hub's last-reported
LAN IP" and connect directly, which also covers mDNS-hostile networks.

**Hub → Modbus DERs (R3 — more greenfield than the draft assumed):** build
the commissioning scan as (a) new `lexa-proto` primitives — a bus **sweep**
(RTU addresses 1–247 across configured baud rates; TCP probe of the local /24
on port 502; at each responder, look for the `SunS` marker) and a **SunSpec
model-1 (Common) identity reader** (manufacturer/model/serial — no such
reader exists today), classification via the model walk + `derbase`
model-presence flags (inverter 10x/70x, storage 12x/8xx, meter 20x) — and
(b) a `lexa-modbus` "commissioning scan" mode that runs them **only when
uncommissioned or explicitly armed — never during live control** — emitting
results on `lexa/scan/result` for the API to relay to the app. The
`lexa-proto` half is a paired-PR / proto-pin bump under the MTR-4 lockstep
rule, and the bench sims should grow scan-target fixtures in the same
session. Unknown-but-Modbus devices get reported too, with a manual
register-map escape hatch (your power users will love you).

**EVSE:** discovery is inverted — OCPP chargers dial the CSMS. "Discovery" is
the pairing UX in §8.4 plus surfacing unrecognized inbound OCPP connection
attempts (which today are silently auto-created as tracked-but-uncontrolled
stations — R8) as pending approvals.

**Hub → utility server:** already done (DNS-SD browse) — leave it.

## 10. Workstream F — OTA and fleet management

Beyond the Mender/DRM choice (§4.5), the parts you must own regardless:

- **Image pipeline:** CI builds a signed full-system artifact per release
  (Yocto image with all seven services baked in), not per-binary pushes.
  `deploy-hub-pi.sh` remains a bench tool only; the bench also consumes
  release artifacts so what QA validates is byte-identical to what ships
  (the V1RC gate already established this discipline — keep it).
- **Boot-success criteria:** an update commits only after: all services reach
  `Type=notify` READY; plan heartbeat = `ok` (or `never` on an
  uncommissioned unit); northbound completes a discovery walk or is cleanly
  idle; modbus sees its configured devices; **system clock is sane** (R6).
  Anything else → automatic rollback to the previous slot. Implemented as the
  on-box `lexa-healthcheck` (DEVICE_ROADMAP §8) so the existing
  watchdog/heartbeat work becomes brick-proofing directly.
- **Data partition:** `/etc/lexa`, `/var/lib/lexa` (journals, spool,
  snapshots, api cert, mode state), and all certs live outside the A/B slots,
  with **schema-versioned config migration** run by the new slot on first
  boot (the `bus.Envelope` versioning habit, applied to config files).
- **Rollout policy:** internal bench → your own home unit → pilot volunteers
  → 10% → 100%, with automatic halt on fleet-level error-rate regression
  (cloudlink's health uplink feeds this).
- **Fleet console (internal):** Grafana on TimescaleDB + Mender's UI covers
  v1 — fleet firmware histogram, offline devices, cert-expiry board (the
  `lexa_cert_expiry_*` gauges you already export, finally with somewhere to
  go), top fault codes.

## 11. Workstream G — Solar forecasting (feature detail)

**Cloud forecast-service** (Go, or Python if you run pvlib directly): every
6 h per site, fetch Open-Meteo hourly GHI/DNI/DHI + temp + cloud cover for
site lat/lon → transpose to plane-of-array using commissioning-captured
tilt/azimuth → array kWp and temperature derate → inverter AC clip →
resample to the planner's 288×5-min grid → publish a `solarforecast` intent
down the command channel.

**On the hub:** lexa-hub adopts the intent via `cmdCh` (`SetSolarForecast`),
validates (finite, non-negative, clamped to plant `max_w`, length ≤ 288),
stamps arrival time, and swaps it in as `PlannerParams.SolarForecastKw` —
**gating off the diurnal high-water block in `buildPlannerParams` that
currently assigns that field unconditionally**. Staleness rule: if the newest
forecast is older than 12 h at planning time, fall back to the existing
diurnal curve and raise the same style of edge-triggered alarm the
retained-control staleness check uses. Add `lexa_hub_forecast_age_seconds`
and a `forecast_source` field on `lexa/hub/plan` so QA can assert which path
was live.

**Accuracy loop (post-pilot):** nightly job computes per-site
forecast-vs-actual MAE from TimescaleDB; v2 trains the residual model per
site; the app's Solar screen shows forecast vs. actual honestly — trust is
the product.

**Load forecasting** (the other `PlannerParams` field, currently a flat
scalar): once 4+ weeks of site history exists, replace the flat constant with
a per-site profile (same-weekday-hour median from the 1-min aggregates, EV
excluded — EVSE load is already separated) delivered as a `loadprofile`
intent onto a new per-step `LoadProfileKw []float64` planner field. This
costs a few days and measurably improves the planner's evening peak
decisions.

## 12. Workstream H — Battery health and status

Three layers, honestly labeled in the UI:

1. **Reported (live):** SoC, power, voltage/current, temperature,
   alarm/fault words, and BMS-reported SoH where the pack exposes it — the
   bus type already carries `soh_pct` on `lexa/battery/{device}/metrics`;
   cloudlink uplinks them, app displays them.
2. **Derived (cloud analytics):** cumulative throughput → equivalent-full-
   cycle count (throughput ÷ 2×nameplate); measured-capacity trend from
   qualifying deep cycles (∫P dt across ≥60% SoC swings,
   temperature-filtered) plotted quarterly; charge-efficiency trend;
   time-at-SoC-extremes histogram (a leading indicator users can act on).
   Present capacity fade as a trend with uncertainty, never a precise "your
   battery is at 87.3%" — packs lie, meters drift, and overclaiming creates
   warranty fights.
3. **Protective (existing):** surface Tier-0 interlock trips, reserve-floor
   events, and sign-inversion detections in the app's event log in plain
   language. These already exist as journal/alert events; they're your most
   credible health signals because they're measured, adversarial, and tested.

**Alerting:** push on fault words, interlock trips, sustained temperature
excursions, and a capacity-trend crossing (e.g., −20% from commissioning
baseline → "talk to your installer").

## 13. Workstream I — EV charging UX and off-peak scheduling

The hard part is already built: the planner models EV target SOC and
departure time against TOU prices, and the OCPP reconciler enforces limits
from metered current. Productization is mostly plumbing user inputs to
`PlannerParams`:

- **App inputs → intents:** target %, departure time (per-weekday defaults
  resolved app/cloud-side; the hub receives concrete kWh + departure epoch
  via `SetEVGoal`), "Charge now" (an edge intent with TTL — bypasses
  economics, still under safety + CSIP constraints), and a mode within the
  optimizer: **Cheapest** (pure TOU), **Solar-first** (charge above a
  home-export threshold, fall back to cheapest window if departure at risk —
  drops out of the planner's existing solar/price co-optimization once real
  forecasts land), **Scheduled** (dumb window, for users who distrust
  automation — offer it, some do).
- **Tariff correctness is the feature's foundation (R12):** ship the manual
  tariff editor first — a `tariff` intent carrying periods/rates/seasons that
  the hub compiles into a `TOUCostModel`, swapped into both the planner's
  `FallbackTOU` and the optimizer's cost model on the control goroutine.
  Utility-published CSIP pricing continues to win via the existing
  `lexa/csip/pricing` → `SetPrices` path whenever present. Add OpenEI URDB
  import as a curated picker afterward (URDB data is messy; validate against
  a hand-checked library of the top ~30 US plans and grow it from support
  requests).
- **SoC honesty:** without vehicle telematics you know EVSE-metered energy,
  not true vehicle SoC. v1: user states battery size + current % at plug-in,
  hub integrates energy (the planner already works in kWh). Label it
  "estimated." Vehicle-API integrations are a v2 partnership question, not
  core.
- **Gateway-mode behavior (decide now, document):** when the optimizer is
  bypassed (§14), EVSE control degrades to the user's Scheduled window or
  full-power — configurable (`gateway.evse_policy`), defaulting to Scheduled.
  A pure CSIP gateway has no opinion about EVs; don't let the mode switch
  silently strand smart charging.

## 14. Workstream J — Gateway/compliance mode (the optimizer bypass)

Your architecture makes this clean, because of one property: **the mode is
"who authors the desired documents."** Everything below the desired docs —
reconcilers, device-level convergence checking, reassert-on-reconnect, Tier-0
interlock, plausibility gates, CannotComply reporting from reconcile
reports — is mode-invariant and stays exactly as validated.

**Design (revised — R2, R10):**

- `hub.json` gains `"mode": "optimizer" | "gateway"`, runtime-switchable via
  a `mode` intent (`SetMode` — async, applied at a tick boundary like every
  other mutation; the mode state is also published retained on
  `lexa/hub/mode` so lexa-api/cloud display it and the hub re-seeds it after
  restart).
- In gateway mode, lexa-hub swaps the **plan author**: a mode-switching
  wrapper (an `orchestrator.Optimizer` implementation, zero engine changes —
  `SystemState` already carries the active CSIP control) delegates to either
  the legacy cascade or the **gateway stack**: the existing
  `constraint.Stack` with the economics constraint replaced by a
  `CSIPPassthrough` constraint that maps the active control state to demands
  — `MaxLimW` → solar ceiling; `FixedW` → battery setpoint; `Connect` →
  connect flags; no active event → DefaultDERControl values; no default →
  nameplate/restore. The site-level `ExpLimW`/`ImpLimW` envelopes are
  enforced by the stack's **already-shadow-validated compliance constraints**
  (Export/GenLimit/ImportLimit with their measured-convergence backstops) —
  the honest answer to "the wire carries meter envelopes, not per-device
  setpoints." The northbound scheduler (event activation, randomization,
  expiry, fail-closed last-known-good) is reused untouched — it already
  computes "what is the active control now," which is precisely the gateway's
  input.
- **Sequencing rule (R10):** gateway mode is the constraint stack's first (or
  second, after the per-axis P5 flips) actuated consumer. Never bench-
  validate two new plan authors simultaneously; if stack actuation
  validation slips, the interim fallback is a passthrough-only gateway
  (MaxLimW/FixedW/Connect, no site-envelope allocation) — weeks, not months,
  but document its ExpLimW gap prominently.
- **Tier-0 and Tier-1 protection remain senior in both modes.** This is not
  a compliance compromise: IEEE 1547 DERs self-protect, and the interlock
  addresses miswiring/faults, not economic preference. Document it as the
  protection-relay layer, mode-independent. The economics tier is what the
  mode disables.
- **Transition semantics:** optimizer→gateway releases optimizer-authored
  holds by publishing explicit restore/adopt docs (never by absence — the
  solar restore-is-a-write rule generalizes; the gateway author's first pass
  emits a doc for every device, and the actuators' content-dedupe absorbs
  no-ops), then adopts current CSIP state. gateway→optimizer re-seeds the
  engine from retained reconcile reports (the machinery the post-restart
  re-seed already exercises). Both transitions journal a `mode_change` record
  with actor identity (app user / API token / CLI).
- **Audit surface:** in gateway mode, every northbound-control→desired-doc
  mapping appends a journal line (event mRID, opMode, value, device, ts) —
  with the existing journal + JOURNAL_FORENSICS.md tooling this becomes the
  evidence package utilities ask for during interconnection disputes.
- **Scope honestly — phase 2 of gateway mode:** a full Rule-21-style gateway
  must also pass grid-support curves (volt-var, volt-watt, freq-watt, fixed
  PF) from CSIP DERCurves down to SunSpec 705/706 (or legacy 126/127/128)
  writes. The optimizer never needed curves, so this is genuinely new
  southbound work: curve read/write in the SunSpec layer, curve types in the
  desired-doc schema, and readback verification. Ship
  gateway-mode-for-power-envelopes first, curves second (a milestone of its
  own with bench scenarios per curve type).

**Power-user access:** three doors to the same intent, in trust order —
(1) app Settings → Advanced (homeowner-visible but gated with an explanation
of consequences); (2) local API `POST /intent {"kind":"mode",...}` with the
per-unit secret; (3) `lexactl` — a small CLI shipped in the image
(`lexactl mode set gateway`, `lexactl status`, `lexactl scan`,
`lexactl forecast show`) that talks to the local API over loopback. Engineers
get SSH + lexactl; **nothing bypasses the intent journal.**

## 15. Workstream K — Third-party aggregator API

Cloud-side, versioned from day one (`/v1/`), OAuth2 client-credentials per
partner, scoped by org/site grants the homeowner approves in-app (energy data
is consumer data — consent must be explicit, revocable, and logged).

**Read:** `GET /v1/sites`, `/sites/{id}/devices`,
`/sites/{id}/telemetry?res=1m&from=…` (straight off the continuous
aggregates), `/sites/{id}/events`.

**Control:** `POST /v1/sites/{id}/dispatch` — a bounded vocabulary mirroring
the intent schema: export cap W, import cap W, battery target power/SoC band,
EVSE current cap, with start, duration, priority. Semantics you must publish
and enforce: **utility CSIP always wins** (aggregator dispatch is clamped by
any active utility control — the constraint arbiter's tier model is exactly
this; an aggregator tier slots between compliance and economics in the
priority stack, which the AD-007 architecture anticipated); homeowner
override policy is explicit per-program; every dispatch returns an ack now
and a result webhook later carrying **measured** outcome (delivered kW over
the window, from telemetry) — aggregators settle on measurement, and the
trust-measurement architecture makes honest settlement data a selling point.

**Webhooks:** dispatch result, device offline, compliance event, enrollment
change. Signed (HMAC), retried with backoff.

**Sandbox:** a hosted environment wired to the existing simulators
(csip-tls-test grid/device/EV sims) so partners integrate without hardware —
the bench becomes a product asset, cheap and unusually credible.

Defer OpenADR until a signed program requires it; the dispatch vocabulary
translates onto OpenADR events later without breaking `/v1`.

## 16. Security, certification, and regulatory

**Security posture (extends what exists):**

- **Secure boot chain:** Digi TrustFence (i.MX HAB) → signed kernel/rootfs →
  Mender-signed updates. Do this before the pilot, not after — retrofitting
  signed boot onto a deployed fleet is miserable.
- **Identity separation:** three cert families with independent CAs and
  rotation: (1) CSIP LFDI cert (utility PKI — note the documented caveat that
  LFDI hashes the full DER cert, so reissue = re-enrollment), (2) cloud
  device cert (your manufacturing CA; file-based key v1 per R4), (3) OCPP
  CSMS TLS cert. Certmon already watches (1); the same monitor pattern
  extends to (2) (in cloudlink) and (3).
- **Clock trust (R6):** RTC with backup battery on the next PCB rev; chrony
  in the image; `After=time-sync.target` on cert-sensitive services;
  clock-sanity in the OTA health gate; NTP check at commissioning. A box
  that boots in 1970 must fail obviously, not subtly.
- **On-box surface:** mosquitto stays loopback-only; lexa-api HTTPS with
  per-device credentials (per-unit secret on the label, rotate-on-claim);
  the commissioning config-write endpoint gated to
  uncommissioned/installer-role and journaled (R11); cloudlink outbound-only
  (no listening ports toward the WAN, ever).
- **Cloud surface:** OAuth2 everywhere, per-partner rate limits, audit log on
  every control-plane mutation, secrets in a manager not env files.
- **Process:** govulncheck and fuzzing already run — formalize with an SBOM
  per release (`cyclonedx-gomod` as a make target), a security.txt +
  disclosure policy, and a third-party penetration test of cloud+box before
  GA. If EU sales are plausible, the Cyber Resilience Act's obligations
  (SBOM, vulnerability handling, update support windows) come into force
  through 2026–27 — existing hygiene puts you unusually close; verify current
  timelines when you get there.

**Certifications (sequence matters — budget money and calendar):**

| Item | What/why | When |
|---|---|---|
| FCC Part 15 Class B (+ ISED Canada) | Unintentional radiator, residential | At hardware freeze; pre-scan early at an EMC lab — as the PCB designer this is your home turf |
| Product safety NRTL listing | Likely UL/IEC 62368-1; possibly UL 916 (energy management equipment) — confirm the category with the NRTL **before layout freeze** (it can drive spacing/fusing) | Parallel with FCC |
| SunSpec CSIP conformance certification | The credential utilities ask for; bench conformance suites are pre-work, but formal certification runs at an authorized lab | After gateway mode ships (certify the mode you'll sell to utilities) |
| Utility onboarding (per IOU) | Each CSIP utility has its own head-end onboarding/test process and cert provisioning | Start paperwork during the pilot; lead times are long |
| Rule 21 / IEEE 1547 positioning | The inverter carries UL 1741 SB; your box is the communications/management layer. Document the combined-system compliance story per supported inverter model | Pilot onward |
| Privacy | Energy interval data is regulated when utility-sourced (CPUC rules in CA) and consumer-protected generally (CCPA/CPRA): privacy policy, data-deletion path, consent records for aggregator sharing (§15) | Before GA |

## 17. Manufacturing and provisioning

Design the factory flow with the same rigor as the firmware, because unit
#500 is provisioned by a contract manufacturer, not by you:

1. **Program & burn-in:** flash the release image (both slots), run a
   self-test (RAM/flash/**RTC**/PHY/RS-485 loopback — as the board designer,
   add the loopback jumpers and testpoints now, plus a bed-of-nails-friendly
   layout).
2. **Identity injection (R4):** v1 — generate the device keypair on-device,
   store on the TrustFence-protected data partition; factory CA (offline
   root, online issuing intermediate — HSM-backed issuing CA or a hardened
   laptop ceremony at your scale) signs the device cert; record
   serial↔cert↔MAC in the manufacturing database. Next PCB rev: decide
   ATECC608B vs. CAAM-resident key (accepting the CGo consequence in
   cloudlink, or a CAAM/keyring indirection) in ADR-0002 — do not let this
   default silently.
3. **Label:** serial, MAC, claim QR, LFDI QR, per-unit local-API secret. The
   LFDI on the label is what makes utility enrollment (§8.5) a read-and-type
   step instead of an SSH session.
4. **Cloud pre-registration:** push the device record to the cloud registry
   in "manufactured/unclaimed" state.
5. **Final functional test:** boot, cloudlink connects to a factory-test
   cloud tenant, publishes a golden telemetry frame, receives and acks a test
   intent, Mender check-in. Green = box it.

Write this as a runbook you execute yourself for units 1–50; automate it into
a provisioning station (a Pi + pogo fixture + the runbook as a script) before
the CM engagement.

## 18. Support and operations

- **Fleet observability:** Grafana on TimescaleDB + the
  cert/heartbeat/version gauges already exported; alert rules for offline
  > 1 h, cert < 30 d (the WARN already emitted, now paging you),
  watchdog-restart clusters, OTA failure spikes.
- **Remote diagnostics:** a diag-bundle command — cloudlink tars the journals
  (`/var/lib/lexa/journal/*`, snapshot, configs-minus-secrets) to S3 via
  presigned URL. JOURNAL_FORENSICS.md becomes the support playbook;
  TASK-081-style findings become ticket templates.
- **Support access:** time-boxed, homeowner-approved, fully audited remote
  read access. Decide the remote-shell question deliberately: a break-glass
  reverse-SSH (off by default, enabled per-incident by the homeowner in-app,
  auto-expiring, journaled) is defensible; a standing backdoor is not.
- **Docs as product:** installer guide (from §8), homeowner guide, tariff
  how-to, utility-enrollment guides per IOU, and the public API reference
  (§15). Budget real time; these gate the pilot as much as code does.
- **RMA:** the §8 replacement flow + a returned-unit wipe/refurb procedure
  (certs revoked in cloud + CRL, identity re-keyed, journals harvested for
  engineering).

## 19. The roadmap

Assumptions: you full-time with Claude Code as the force multiplier, ~1
contractor for app UI in Phases 2–3, hardware spins in parallel on your side.
Durations are calendar, will overlap, and carry the usual ±50%.

**Phase 0 — Close the gate, pick the platforms (2–4 weeks).**
Run the re-confirmation deploy for FINDINGs A/D (fixes are already in tree);
re-run `export-dither-at-breach` against TASK-064 to close FINDING B; tune
the ~40 s retained-rollback window (042 tuning). Decide §4.1–4.5 (time-box
the DRM evaluation to two weeks on the dev kit — now live — against Mender);
stand up cloud accounts, environments (dev/staging/prod), and the
manufacturing-CA design on paper; add `cyclonedx-gomod` SBOM to CI.
*Exit:* decisions recorded as ADR-0002…0006 (ADR-0002 explicitly resolves
the R4 key-storage question); empty-but-deployed cloud skeleton; V1RC
checklist green.

**Phase 1 — Identity, uplink, OTA (6–8 weeks) — the foundation everything
ships through.**
Build lexa-cloudlink (§6) with the per-kind intent topics and hub-side
`cmdCh` handlers; IoT Core ingest → TimescaleDB with the §5 schema; claim
flow (QR → bind); Mender integrated into the existing `meta-lexa` Yocto build
with A/B + signed artifacts + the §10 boot-success criteria
(`lexa-healthcheck`); internal Grafana fleet board; convert the bench Pi and
dev kit into fleet members updated only via OTA.
*Exit:* a factory-fresh unit reaches the cloud with zero SSH; an OTA with a
deliberately broken service auto-rolls back; telemetry visible in Grafana;
`wan-outage-spool-drain` Mayhem scenario passes.

**Phase 2 — Commissioning + app v1 (8–10 weeks, overlaps Phase 1 tail).**
mDNS advertisement + expanded local lexa-api (HTTPS, per-unit secret, REST,
`POST /intent`, config-write path per R11); `lexa-proto` sweep + model-1
identity reader (paired PR) + `lexa-modbus` commissioning scan mode; the
Capacitor app with installer wizard (§8) and homeowner screens 1–2–5–7;
per-model plant-template library seeded with the bench devices; commissioning
report PDF; timezone/tariff-zone + clock-trust enforcement.
*Exit:* a non-author (recruit a friendly electrician) commissions the bench
site from sealed box to live optimization in < 45 min using only the app; you
commission your own home as fleet unit #1.

**Phase 3 — Feature wave: forecasting, EV UX, battery health, tariffs (6–8
weeks).**
Forecast service + hub intent + staleness fallback (§11); load-profile
learning; EV screens and intents incl. Solar-first (§13); manual tariff
editor → `tariff` intent → cost-model swap; battery analytics layer 2 +
event-log surfacing (§12); push notifications.
*Exit:* planner runs on real weather at ≥3 sites with forecast-vs-actual
tracked; an EV charges to target by departure through two TOU cycles
untouched; battery trend chart renders from ≥6 weeks of real data.

**Phase 4 — Gateway mode + pilot fleet (6–8 weeks; pilot recruitment starts
in Phase 2).**
Mode manager + gateway stack (CSIP passthrough over the shadow-validated
compliance constraints) + transition semantics + audit journaling + `lexactl`
(§14) — sequenced against the constraint-stack P5 flips per R10; bench Mayhem
scenarios for mode transitions under fault; deploy 10–25 pilot units
(installer partners, friendly customers) on staged OTA; support playbook v1.
*Exit:* mode switch under active CSIP event is clean on the bench (no breach
beyond oracle); pilot fleet ≥ 10 sites, ≥ 4 weeks, offline-rate and
watchdog-restart budgets defined and met; first support tickets resolved via
diag-bundle without site visits.

**Phase 5 — Aggregator API + certification (8–12 weeks, heavy on waiting).**
`/v1` read + dispatch + webhooks + sandbox-on-simulators (§15); consent UX;
SunSpec CSIP formal certification of gateway mode; FCC/NRTL runs on frozen
hardware; utility onboarding paperwork for the first target IOU; pen test;
grid-support curve passthrough begins as its own tracked milestone.
*Exit:* one external partner completes a sandbox integration and a live
dispatch on the pilot fleet with measured settlement; certification test
reports in hand or formally scheduled.

**Phase 6 — Manufacturing bring-up and GA (8–12 weeks).**
Provisioning station + runbook → CM pilot run (50–100 units); refurb/RMA flow
exercised; docs complete; pricing includes the §5 cloud COGS; staged-rollout
OTA discipline proven at fleet width.
*Exit:* CM builds a sellable, claimable, OTA-able unit with no founder in the
loop.

Aggressive-but-honest calendar: pilot homes live around month 5, GA-capable
around month 12–15.

## 20. Risks worth naming

1. **Solo-founder bus factor and burnout** — the roadmap has three products
   (box, cloud, app). Mitigations: the buy-list in §4, contractor on app UI,
   ruthless scope defense (everything labeled v2 stays v2).
2. **OTA is existential** — hence Phase 1, the boot-success gating on the
   existing heartbeats, and never shipping a change the bench didn't consume
   as a release artifact.
3. **Plan-author collision (R10)** — legacy cascade, constraint stack, and
   gateway author must never be in simultaneous first-validation on the
   bench. The sequencing in §14/§19 is the mitigation; hold the line on it.
4. **iOS local-network flakiness** — prototype the mDNS plugin in week 1 of
   Phase 2; the cloud-relayed-IP fallback is designed in from the start.
5. **Certification calendar** — CSIP certification and utility onboarding are
   queue-bound; start paperwork in Phase 4 even though testing lands in
   Phase 5.
6. **Device zoo** — every new inverter/battery model is integration work.
   The plant-template library + plausibility gates + power-user register-map
   escape hatch contain it; publish a supported-device list and say no often.
7. **Forecast disappointment** — pure-physics forecasts on cloudy coasts will
   miss; set expectations in-app (show the uncertainty band) and let the v2
   residual model be the upgrade story, not an apology.
8. **Cloud dependence creep** — enforce principle 2 with a standing Mayhem
   scenario: a 7-day WAN outage must produce zero safety/compliance deltas
   and a clean spool drain.
9. **Lockstep tax on lexa-proto** — the commissioning scan puts new shared
   code under the MTR-4 paired-PR discipline; batch proto changes
   deliberately rather than dribbling pin bumps.

## 21. The first 30 days, concretely

1. Run the FINDING A/D re-confirmation deploy; re-run
   `export-dither-at-breach` (FINDING B closure); tune the rollback window.
2. Write ADR-0002 (cloud posture **incl. the R4 key-storage decision**) and
   ADR-0003 (OTA) after the two-week DRM-vs-Mender spike on the live dev kit.
3. Stand up: AWS account structure, IoT Core dev endpoint, one RDS Postgres
   with Timescale, Grafana.
4. Start lexa-cloudlink: envelope-versioned uplink of `lexa/measurements/#`
   with disk spool; get one real measurement from the dev kit into
   TimescaleDB end-to-end. That single vertical slice de-risks more than
   anything else you could do this month.
5. Confirm the TrustFence/CAAM key-storage approach for the next PCB rev
   (order ATECC608B samples only if ADR-0002 picks the SE path); add the RTC
   backup battery to the rev; sketch the label (serial, MAC, claim QR, LFDI
   QR, API secret).
6. Recruit the friendly electrician and two pilot-volunteer homes now —
   recruiting lags code by months.

---

Everything above deliberately reuses the disciplines already in the repo —
envelope versioning, ACL-by-call-site, fail-closed staleness, single-writer
desired docs, measured-not-commanded verification, crash-only services —
because they are the right disciplines for the field, not just the bench. The
product work is mostly giving them somewhere to live beyond the LAN.

**Device-side implementation contract:** `docs/DEVICE_ROADMAP.md`.
