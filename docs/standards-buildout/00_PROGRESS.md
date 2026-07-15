# Standards Build-Out — Progress Board

Campaign started 2026-07-14. Branch: `standards-buildout` (lexa-hub). Plan: `work-packages.md` (17 WPs),
design: `architecture.md`, risks: `risks.md`. Scope decisions recorded in `architect-brief.md` header.

Status legend: ⬜ pending · 🔵 in progress · ✅ done (tests green) · 🟠 done-needs-bench · ⏸ blocked

| WP | Title | Size | Deps | Status | Notes |
|---|---|---|---|---|---|
| WP-1 | lexa-proto change-set (THE pin bump) | L | — | ✅ 24081a2 | derbase Measurements ext · csipmodel LogEvent+Table27 · scanner 3-base · ocppserver16 |
| WP-2 | bus.Measurement enrichment + modbus publishers | M | 1 | ✅ f639016 | BUS LANE start |
| WP-3 | tlsclient PUT + redirects | S | — | ✅ 6f14518 | closes ERR-001 code half |
| WP-4 | DER* reporting (dersite + derreport) | M | 2,3 | ✅ (see git log) | closes CORE-009/014, BASIC-028 |
| WP-5 | telemetry VAr + Wh MMRs | S | 2 | ✅ 656a0fe | closes BASIC-029 VAr gap |
| WP-6 | LogEvent pipeline | M | 1,2 | ✅ 16d2bef | closes BASIC-027; gridsim LogEvent endpoint pending (bench queue) |
| WP-7 | PIN verify + Table 27 codes | S | 1 | ✅ ea8b228 | 252 + 13/14 are documented seams; bench config keeps legacy 0xF0 until gridsim flips |
| WP-8 | Advanced-control carriage northbound | M | 2 | ✅ 619494c | BUS LANE; retires extended→simple silent drop |
| WP-9 | Adv desired doc + hub authoring | M | 8 | ✅ eae6b4d | BUS LANE; `advanced_der` flag |
| WP-10 | Advanced reconciler execution | L | 9 | ✅ bd6190e | `reconciler.adv` off/shadow/active; closes BASIC-004..012 exec half |
| WP-11 | GenLimW/LoadLimW enforcement (AUS) | M | 8 | ✅ 593620a | cascade+shadow pair; `enforce_aus_limits` |
| WP-12 | OCPP 1.6J dual-stack | M/L | 1 | ✅ fcdd98e | 2.0.1 path byte-identical |
| WP-13 | Pairing gate + ClearChargingProfile | M | 12 (+bus slot after 9) | ✅ 205e90c | closes R8; `pairing_mode`; apicontract /devices pinned |
| WP-14 | V2G type enablers | M | 9,13 (bus lane tail) | ✅ 776845c | `ev_storage`; flag-off byte-identical plans |
| WP-15 | lexa-openadr service | L | 2 (+bus slot last) | ✅ c9c2984+6adb74f | svc half + hub-adoption slice (D9); metrics :9108; pure Go |
| WP-16 | CSIP-AUS checklist + verify-sweep docs | S | 11 | ✅ | AUS spec-gap disclosure; `CSIP_AUS_CHECKLIST.md` + `VERIFICATION_SWEEP.md` |
| WP-17 | Integration & verification gate | M | all | ✅ | ACL re-derive (8 rows added), envelope/Finite audit clean, -race+cmd+vet+arm64 green, apicontract green, CLAUDE.md updated |

Bus-lane serialization (internal/bus/{messages,desired,topics,envelope}.go): WP-2 → WP-8 → WP-9 →
WP-13(slice) → WP-14 → WP-15(slice). One owner at a time.

Cross-repo: WP-1 commits in ../lexa-proto (local, no remote per AD-003(c)); proto.pin + vendor/
regenerated in BOTH lexa-hub and ../csip-tls-test same session (TASK-024 CI gate). WP-7 default-flip
of CannotComply codes needs gridsim expectation update in csip-tls-test same session (or bench configs
set legacy_cannotcomply_code=true).

Bench-deferred queue (WP-17f): adv-shell shadow soak · AUS shadow week · 1.6 evsim · PIN drill ·
COMM-004 pcaps · dersite/PUT vs gridsim (gridsim needs PUT/LogEvent endpoints — csip-tls-test work) ·
lexa-openadr deploy wiring (WP-15 service half shipped WITHOUT deploy-hub-pi.sh changes: the script
must still provision /etc/lexa/mqtt/openadr.pass + patch openadr.json creds + install/enable the
unit + a client_secret_file, per architecture §2.3's deploy note — tracked separately).

Vendor quirks for next proto bump: (WP-4) csipmodel DERList lacks pollRate; DERCapability/Settings lack rtg/setMaxWh; DERStatus lacks alarmStatus; derreport parses putResult error strings for 404/405 — switch to typed error when tlsclient gains one. (WP-5) csipmodel KindVoltage=12 collides with 2030.5 KindType energy(12); KindFreq=38 is not a KindType member. Telemetry uses local spec-correct constants meanwhile. (WP-13) ocppserver16 lacks an Authorize seam — 1.6 Authorize gating needs it. **(QA-A, 2026-07-15, real latent bug) `lexa-proto/modbus` `ReadRegisters` errors on any read >125 registers without chunking — the full 137-register SunSpec model 701 (a 1547-2018-certified inverter's primary measurement model) is UNREADABLE in one transaction; a real device serving it would fail. The QA sim truncates 701 to 121 regs as a workaround. FIX: split reads at the 125-register Modbus limit in the transport.**

## Campaign status (WP-17 gate, 2026-07-14)

**Code-complete: all 17 WPs landed and verified.** Tree is releasable; every WP is
additive + feature-flagged, product defaults fail-closed (byte-identical to
pre-campaign behavior). Verification (branch `standards-buildout`, `GOWORK=off`):

- `make test` (-race, ./internal/...): **PASS, 0 FAIL**.
- `go test ./cmd/...`: **PASS** (12 packages, incl. cgo northbound/telemetry with host wolfSSL).
- `go vet ./...`: **clean**. `gofmt -l`: **empty**.
- Cross-compile: 5 pure-Go arm64 binaries (hub/modbus/ocpp/api/openadr) + host-CGo
  northbound/telemetry — **all build** (arm64 wolfSSL sysroot is a bench step, deferred).
- apicontract drift gate (`make contract`): **green** (13 contract tests incl. `Pairing`).
- ACL re-derived from every Subscribe/Publish call site: **8 gap rows added** to
  `systemd/mosquitto-lexa.acl` (northbound `write certstatus`; api `read certstatus`
  §0.3; modbus `write reconcile/adv` + `read reconcile/{battery,solar,adv}`; ocpp `read
  reconcile/evse`; hub `read hub/mode`). Envelope/SupportedV/Finite audit: **no gaps**
  (every family has a const + arm; DesiredAdvanced arm precedes `lexa/desired/`; all
  `*float64` types wired through mqttutil.Subscribe have comprehensive Finite()).
  FLASH_BUDGET: **no per-tick Info logging** at product defaults (verbose reconcile-adv/
  adv logs are behind opt-in bench flags, edge/transition-gated).

**Flag inventory + shipped defaults** (mirrors architecture §3; also in CLAUDE.md
"Standards build-out" section):

| File | Key | Default |
|---|---|---|
| northbound.json | `registration_pin` | `0` (disabled+WARN) |
| northbound.json | `der_report` | `true` |
| northbound.json | `legacy_cannotcomply_code` | loader `false`; **bench config ships `true`** until gridsim flips |
| northbound.json | `redirect_max` | `3` |
| hub.json | `advanced_der` | `"off"` |
| hub.json | `enforce_aus_limits` | `false` |
| hub.json | `ev_storage` | `false` |
| hub.json | `logevent_min_interval_s` | `10` |
| hub.json | `der_type` | `0` (derive) |
| hub.json | `openadr_adopt` / `openadr_price_max_age_s` | `true` / `3600` |
| modbus.json | `reconciler.adv` | `"off"` |
| ocpp.json | `port_16` | `0` (1.6J off) |
| ocpp.json | `pairing_mode` | `""` ⇒ gated (product) / open (bench) |
| ocpp.json | `allowlist_path` | `/var/lib/lexa/ocpp-allowlist.json` |
| telemetry.json | `post_var` / `post_wh` | `true` / `true` |
| openadr.json | `vtn_url` | `""` (uncommissioned idle) |

**Ordered bench-deferred queue** (from `VERIFICATION_SWEEP.md`; earlier = cheaper/
prerequisite):

1. **PIN mismatch drill** — arm `registration_pin` vs a mismatching Test Server; confirm D4 fail-closed (held control, suspended egress, `pin_ok`/gauge).
2. **COMM-004 D–G pcaps** — blocked on 7 cert-chain fixtures + a `UseCertificateChainFile` wrapper in csip-tls-test (paired work item).
3. **ERR-001 redirect bench check** — blocked on a redirect-injection `/admin/malform` kind in gridsim (paired work item).
4. **dersite/PUT + LogEvent vs gridsim** — gridsim needs PUT + LogEventList endpoints (paired work item; blocks WP-4/WP-6 bench validation).
5. **CannotComply code-flip pairing** — flip `legacy_cannotcomply_code` → false with gridsim's expectation updated same session (currently bench ships `true`).
6. **AUS shadow week** — ≥1-week `constraint_shadow` zero-diff before any `enforce_aus_limits` default-flip proposal.
7. **adv-shell shadow soak** — `reconciler.adv` shadow→active against real inverter hardware (unvalidatable in sim alone, RSK-08).
8. **1.6 evsim** — OCPP 1.6J evsim vs `port_16` dual-stack; 2.0.1 byte-identical preserved.
9. **lexa-openadr deploy wiring** — `deploy-hub-pi.sh` must provision `/etc/lexa/mqtt/openadr.pass`, patch `openadr.json` creds, install/enable the unit + `client_secret_file` (service half shipped without script changes).
