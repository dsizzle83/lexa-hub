# CSIP-AUS Checklist (WP-16)

Companion: `VERIFICATION_SWEEP.md` (bench procedures), `architecture.md` (D2/D4/D5/§6),
`work-packages.md` WP-11/WP-16, `derms-expert-review.md` (market rationale for building this
axis at all), `standards-coverage-audit.md` (original gap note), `architect-brief.md` §F (scope
decision + spec-gap caveat).

## Spec-gap disclosure

**The standards library available to this build-out contains NO CSIP-AUS specification.**
`architect-brief.md` §F1 states this explicitly: "The standards library has NO CSIP-AUS
document — AUS items must be scoped from domain knowledge and marked 'verify against CSIP-AUS
v1.1a before cert'." Every requirement-level claim in this document was derived from (a) general
IEEE 2030.5-2018 vocabulary the hub already implements (`opModExpLimW`/`opModImpLimW`/
`opModGenLimW`/`opModLoadLimW` are all standard 2030.5 DERControlBase/extended fields — CSIP-AUS
is known, from domain knowledge, to be the market profile that actually exercises the gross
generation/load pair) and (b) the reviewer's domain expertise
(`derms-expert-review.md`), not from a normatively-read CSIP-AUS document.

**Every requirement-level claim below is marked UNVERIFIED — verify against CSIP-AUS v1.1a
before any AUS certification/deployment.** Nothing in this file may be read as a conformance
claim. It is a map of (1) what code exists and what it does, cross-referenced to file:line
evidence, versus (2) what CSIP-AUS v1.1a is believed to require, which is unverified pending the
actual spec.

## Axis coverage table

Four dynamic-envelope axes carry CSIP-AUS significance in this codebase. All four are ordinary
IEEE 2030.5 `DERControlBase`/extended fields; what makes them "CSIP-AUS" is market usage
(dynamic operating envelopes, SA/QLD-style flexible export schemes), not a distinct wire format —
so "coverage" below is *enforcement maturity in this codebase*, not a certified-against-spec
claim.

| Axis | 2030.5 field | Status | Evidence | Convergence backstop | Gate |
|---|---|---|---|---|---|
| Export limit | `opModExpLimW` | **Enforced pre-campaign** (predates WP-11/this build-out) | `internal/orchestrator/optimizer.go:854` `applyExportLimitRule`; adopted via `busToCSIPControl` (`cmd/hub/state.go:840-842`, `ExpLimW`) | `internal/orchestrator/optimizer.go:1404` `checkExportLimitConvergence` (meter-independent; battery-absorption guard) | Always on — no flag. Live/authoritative today. |
| Import limit | `opModImpLimW` | **Enforced pre-campaign** (predates WP-11/this build-out) | `internal/orchestrator/optimizer.go:2145` `applyImportLimitRule`; adopted via `cmd/hub/state.go:843-845` (`ImpLimW`) | `internal/orchestrator/optimizer.go:1617` `checkImportConvergence` (NaN-hold leaky counter) | Always on — no flag. Live/authoritative today. |
| Gross generation limit | `opModGenLimW` | **WP-11, cascade + shadow pair, flag-gated** | `internal/orchestrator/auslimits.go:55` `applyAusGenerationLimitRule` (battery-discharge participation cap + solar ceiling, two levers); called from `internal/orchestrator/optimizer.go:459` (`if o.EnforceAusLimits`) | `internal/orchestrator/auslimits.go:136` `checkAusGenerationConvergence` (meter-independent gross-gen floor, `grossGen ≥ −netW`) — mirrors the export rule's hard-preserve floor | `EnforceAusLimits` (`internal/orchestrator/optimizer.go:196` field; `cmd/hub/config.go:139` json key `enforce_aus_limits`; `configs/hub.json:10` default **false**). Adoption into `GridState.GenLimitW` is **unconditional** regardless of the flag (`cmd/hub/state.go:744-754`) — only enforcement is gated. Shadow mirror: `internal/orchestrator/constraint/genlimaus.go` (`AusGenLimitConstraint`), runs under `constraint_shadow` **regardless** of `enforce_aus_limits` (architecture.md §6). |
| Gross load limit | `opModLoadLimW` | **WP-11, cascade + shadow pair, flag-gated** | `internal/orchestrator/auslimits.go:250` `applyAusLoadLimitRule` (battery-charge cap + sticky single-EVSE curtail, two levers); called from `internal/orchestrator/optimizer.go:531` (`if o.EnforceAusLimits`) | `internal/orchestrator/auslimits.go:455` `checkAusLoadConvergence` (leaky counter; a genuinely-unmeetable home load is explicitly documented as a valid CannotComply outcome, unlike the generation axis) | Same `EnforceAusLimits` flag/default as above. Adoption into `GridState.LoadLimitW` is unconditional (`cmd/hub/state.go:755-756`). Shadow mirror: `internal/orchestrator/constraint/loadlimaus.go` (`AusLoadLimitConstraint`). |

Flag-off behavior (both GenLimW/LoadLimW): `internal/orchestrator/optimizer.go:196`'s field doc
states plans are byte-identical to pre-WP-11 with the flag off "regardless of what limits are
present" — this is pinned by `internal/orchestrator/auslimits_test.go` and the shadow harness's
own `auslimits_shadow_test.go`, not asserted here from the doc alone. The daily-planner path gets
the identical flag-off contract: `cmd/hub/main.go:1190-1194`'s `derConstraintsFromSchedule` only
maps a schedule slot's `gen_lim_w`/`load_lim_w` into the planner envelope when `ausLimits` (the
same `enforce_aus_limits` value) is true.

**Shadow-first gate before any default flip**: `architecture.md` §6 — "≥1-week bench shadow
before default-on" for the GenLimW/LoadLimW cascade, the same discipline TASK-060's export
constraint used. `enforce_aus_limits` must NOT be flipped to `true` in any deployment config
until that bench-shadow week has run and its divergence count is at the same "0 diff gate"
standard the other P5 flips required (CLAUDE.md's `constraint_shadow` description). As of this
writing that bench week has not run (`00_PROGRESS.md`'s bench-deferred queue lists "AUS shadow
week" as outstanding).

**Not covered by this table (out of AUS scope, noted to avoid confusion)**: `opModConnect`,
`opModEnergize`, `opModMaxLimW`, `opModFixedW`, and the curve/PF/volt-var/freq-droop axes
(WP-8/9/10) are ordinary IEEE 2030.5 controls the hub enforces independently of CSIP-AUS and are
documented in `architecture.md` D2/D6/D7, not here. `architecture.md` D2 notes the aggregate
`modesSupported` truth-mask includes "+ExpLimW/ImpLimW for AUS" alongside the base set — see
`architecture.md:336`.

## Known AUS requirements believed-but-unverified

Everything in this section is **UNVERIFIED — verify against CSIP-AUS v1.1a before any AUS
certification/deployment.** Source: domain knowledge relayed in `architect-brief.md` §F1 and
`derms-expert-review.md`'s AUS discussion (lines ~18, ~149-150, ~168-182) — no normative document
was consulted for any item below.

1. **Dynamic-envelope control-refresh cadence (~5-minute reissue).** Believed shape: a CSIP-AUS
   Utility Server reissues `opModGenLimW`/`opModLoadLimW` (and the export/import pair) on a short
   cadence (on the order of 5 minutes) to track real-time network conditions, materially tighter
   than the generic CSIP `DERControlList` polling default (G19: 10 min poll / 5 min monitoring
   post, per `digests/csip-conformance-tests.md`'s PICS section). **UNVERIFIED**: the exact
   cadence, whether it is poll-driven or subscription-driven, and whether the hub's walk-cadence
   pacing (`internal/northbound/CLAUDE.md`'s `poll_rate_mode: "honor"`, TASK-071/AD-014) is fast
   enough to track it without a CSIP-AUS-specific override, are all unconfirmed against a real
   spec.
2. **In-band registration variants.** Believed shape: CSIP-AUS registration/enrollment may differ
   from the base CSIP out-of-band PIN flow the hub implements today (`internal/northbound/run/pin.go`,
   WP-7/D4 — `registration_pin` config, `PinVerifier`). **UNVERIFIED**: whether CSIP-AUS mandates
   an additional or different in-band registration handshake, and whether the existing
   fail-closed PIN-mismatch posture (control held, egress suspended, loud alarm —
   `architecture.md` D4) satisfies it unmodified.
3. **DER capability/settings/status reporting deltas.** The hub's WP-4 DER* reporting
   (`internal/northbound/derreport`, GFEMS aggregate semantics per `architecture.md` D2) was
   designed against generic IEEE 2030.5/CSIP `DERCapability`/`DERSettings`/`DERStatus`/
   `DERAvailability` shapes. **UNVERIFIED**: whether CSIP-AUS specifies additional fields, units,
   or reporting cadences (e.g. an AUS-specific `rtgMaxW`/envelope-headroom field, or an
   AUS-mandated per-axis "why curtailed" reason code) beyond what D2's aggregation model covers.
4. **Utility Server specifics (e.g. SA Power Networks / Energy Queensland).** Believed shape:
   individual Australian DNSPs (SAPN in South Australia, Energex/Ergon in Queensland under the
   "EQ" umbrella) each operate their own CSIP-AUS-conformant Utility Server with possible
   handbook-level deltas (URL structure, PICS expectations, specific envelope semantics) the way
   CA IOU handbooks vary under base CSIP (`derms-expert-review.md`'s gap-1 discussion of
   handbook-level variance is the closest analog on record here). **UNVERIFIED**: no
   SAPN-specific or EQ-specific integration guide has been consulted; there is no code or config
   in this repo scoped to either utility by name.
5. **Whether ExpLimW/ImpLimW enforcement (pre-existing, see axis table) actually satisfies
   CSIP-AUS's dynamic-envelope requirement as-is.** The hub's export/import enforcement predates
   this build-out and was written against generic 2030.5 semantics, not CSIP-AUS. `architect-brief.md`
   §F1 and `derms-expert-review.md` both flag this as the hub's "oddball strength" for the AUS
   market, but **UNVERIFIED**: whether the existing convergence-backstop tolerance bands, breach
   escalation ticks, and margin fractions (`ImportMarginFrac`/`ExportMarginFrac`) meet whatever
   response-time or accuracy bound CSIP-AUS actually specifies for envelope compliance.
6. **CannotComply / Response-code mapping under CSIP-AUS.** `architecture.md` D5 maps the hub's
   two failure classes onto standard IEEE 2030.5 Table 27 codes (253/252 at rejection; 8/3/10 at
   episode onset/event-end). **UNVERIFIED**: whether CSIP-AUS defines its own response/reporting
   obligations for envelope non-compliance distinct from base Table 27 (e.g. a required
   curtailment-event log format for DNSP settlement/audit purposes).

## Pre-cert action list

1. **Obtain CSIP-AUS v1.1a** (or whatever version is current at cert time) from the relevant
   standards body/DNSP working group — this document cannot be written correctly without it;
   every UNVERIFIED item above is a placeholder for a real spec read, not a substitute for one.
2. **Gap-assess** the axis coverage table above against the actual spec, item by item: confirm
   field names/semantics for `opModGenLimW`/`opModLoadLimW` match what §Axis coverage assumes,
   confirm the envelope-reissue cadence, confirm registration variant requirements, confirm
   DER-reporting deltas, confirm any DNSP-specific (SAPN/EQ) handbook requirements. Produce a
   revised version of this checklist with each UNVERIFIED item resolved to Verified/Gap/N-A.
3. **Bench shadow week** (already gated independently of spec acquisition, per `architecture.md`
   §6 and `00_PROGRESS.md`'s bench-deferred queue): run `constraint_shadow` with real or
   simulated AUS envelope traffic for ≥1 week, confirm zero-diff between the cascade
   (`internal/orchestrator/auslimits.go`) and its shadow mirror
   (`internal/orchestrator/constraint/{genlimaus,loadlimaus}.go`), the same gate every other P5
   flip in this repo has required.
4. **Default-flip decision**: only after steps 1-3 close, decide whether to flip
   `enforce_aus_limits` to `true` in any AUS-market deployment config (`configs/hub.json`) — this
   is a deliberate, reviewed, per-deployment decision, not a blanket default change; the shipped
   repo default stays `false` until then.
