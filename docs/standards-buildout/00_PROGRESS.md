# Standards Build-Out — Progress Board

Campaign started 2026-07-14. Branch: `standards-buildout` (lexa-hub). Plan: `work-packages.md` (17 WPs),
design: `architecture.md`, risks: `risks.md`. Scope decisions recorded in `architect-brief.md` header.

Status legend: ⬜ pending · 🔵 in progress · ✅ done (tests green) · 🟠 done-needs-bench · ⏸ blocked

| WP | Title | Size | Deps | Status | Notes |
|---|---|---|---|---|---|
| WP-1 | lexa-proto change-set (THE pin bump) | L | — | ✅ 24081a2 | derbase Measurements ext · csipmodel LogEvent+Table27 · scanner 3-base · ocppserver16 |
| WP-2 | bus.Measurement enrichment + modbus publishers | M | 1 | 🔵 | BUS LANE start |
| WP-3 | tlsclient PUT + redirects | S | — | ✅ 6f14518 | closes ERR-001 code half |
| WP-4 | DER* reporting (dersite + derreport) | M | 2,3 | ⬜ | closes CORE-009/014, BASIC-028 |
| WP-5 | telemetry VAr + Wh MMRs | S | 2 | ⬜ | closes BASIC-029 VAr gap |
| WP-6 | LogEvent pipeline | M | 1,2 | ⬜ | closes BASIC-027 |
| WP-7 | PIN verify + Table 27 codes | S | 1 | ⬜ | closes CORE-003 PIN, CORE-022 codes; kills 0xF0 default |
| WP-8 | Advanced-control carriage northbound | M | 2 | ⬜ | BUS LANE; retires extended→simple silent drop |
| WP-9 | Adv desired doc + hub authoring | M | 8 | ⬜ | BUS LANE; `advanced_der` flag |
| WP-10 | Advanced reconciler execution | L | 9 | ⬜ | `reconciler.adv` off/shadow/active; closes BASIC-004..012 exec half |
| WP-11 | GenLimW/LoadLimW enforcement (AUS) | M | 8 | ⬜ | cascade+shadow pair; `enforce_aus_limits` |
| WP-12 | OCPP 1.6J dual-stack | M/L | 1 | 🔵 | port_16, ocppserver16; worktree |
| WP-13 | Pairing gate + ClearChargingProfile | M | 12 (+bus slot after 9) | ⬜ | closes R8 |
| WP-14 | V2G type enablers | M | 9,13 (bus lane tail) | ⬜ | `ev_storage`; flag-off byte-identical plans |
| WP-15 | lexa-openadr service | L | 2 (+bus slot last) | ⬜ | metrics :9108; pure Go |
| WP-16 | CSIP-AUS checklist + verify-sweep docs | S | 11 | ⬜ | AUS spec-gap disclosure |
| WP-17 | Integration & verification gate | M | all | ⬜ | ACL re-derive (incl. pre-existing lexa-api certstatus gap), envelope audit, -race+arm64, apicontract, CLAUDE.md updates |

Bus-lane serialization (internal/bus/{messages,desired,topics,envelope}.go): WP-2 → WP-8 → WP-9 →
WP-13(slice) → WP-14 → WP-15(slice). One owner at a time.

Cross-repo: WP-1 commits in ../lexa-proto (local, no remote per AD-003(c)); proto.pin + vendor/
regenerated in BOTH lexa-hub and ../csip-tls-test same session (TASK-024 CI gate). WP-7 default-flip
of CannotComply codes needs gridsim expectation update in csip-tls-test same session (or bench configs
set legacy_cannotcomply_code=true).

Bench-deferred queue (WP-17f): adv-shell shadow soak · AUS shadow week · 1.6 evsim · PIN drill ·
COMM-004 pcaps · dersite/PUT vs gridsim (gridsim needs PUT/LogEvent endpoints — csip-tls-test work).
