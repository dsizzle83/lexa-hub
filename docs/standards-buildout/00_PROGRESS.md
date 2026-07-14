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
| WP-10 | Advanced reconciler execution | L | 9 | 🔵 | `reconciler.adv` off/shadow/active; closes BASIC-004..012 exec half |
| WP-11 | GenLimW/LoadLimW enforcement (AUS) | M | 8 | ✅ 593620a | cascade+shadow pair; `enforce_aus_limits` |
| WP-12 | OCPP 1.6J dual-stack | M/L | 1 | ✅ fcdd98e | 2.0.1 path byte-identical |
| WP-13 | Pairing gate + ClearChargingProfile | M | 12 (+bus slot after 9) | 🔵 | closes R8 |
| WP-14 | V2G type enablers | M | 9,13 (bus lane tail) | ⬜ | `ev_storage`; flag-off byte-identical plans |
| WP-15 | lexa-openadr service | L | 2 (+bus slot last) | 🟡 c9c2984 svc half | metrics :9108; pure Go |
| WP-16 | CSIP-AUS checklist + verify-sweep docs | S | 11 | ⬜ | AUS spec-gap disclosure |
| WP-17 | Integration & verification gate | M | all | ⬜ | ACL re-derive (incl. pre-existing lexa-api certstatus gap), envelope audit, -race+arm64, apicontract, CLAUDE.md updates |

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

Vendor quirks for next proto bump: (WP-4) csipmodel DERList lacks pollRate; DERCapability/Settings lack rtg/setMaxWh; DERStatus lacks alarmStatus; derreport parses putResult error strings for 404/405 — switch to typed error when tlsclient gains one. (WP-5) csipmodel KindVoltage=12 collides with 2030.5 KindType energy(12); KindFreq=38 is not a KindType member. Telemetry uses local spec-correct constants meanwhile.
