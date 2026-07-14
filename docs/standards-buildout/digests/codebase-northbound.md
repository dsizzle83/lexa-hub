# lexa-hub capability inventory — NORTHBOUND + platform surface (IEEE 2030.5-2018 / CSIP v2.1)

Repo: /home/dmitri/projects/lexa-hub (branch main, 2026-07-14). All paths absolute-relative to repo root.
"Enforced" = the optimizer/hub acts on it; "consumed" = parsed/scheduled/republished on MQTT only.

## Sources examined

- CLAUDE.md; internal/northbound/CLAUDE.md (walk order, scheduler rules, MUP flow, DNS-SD)
- cmd/northbound/{main,config,certmon,rotate}.go; cmd/telemetry/{main,config}.go; cmd/api/{main,handlers,heartbeat,mdns,tlscert,version,plan}.go
- internal/northbound/discovery/{walker,helpers,pricing,billing,flowreservation}.go; scheduler/scheduler.go; schedule/schedule.go; run/{run,pollrate}.go; publish/publish.go; responses/{tracker,persist}.go; flowres/manager.go; identity/identity.go; dnssd/browse.go
- internal/tlsclient/{client,config,request,fetcher,dcap}.go + httpwire/httpwire.go; internal/wolfssl (via client.go); internal/utilitytime/{utilitytime,expiry}.go
- vendor/lexa-proto/csipmodel/{resources,der,pricing,billing,flowreservation}.go (lexa-proto vendored at vendor/lexa-proto/, pinned via proto.pin; go.work adds ../lexa-proto)
- internal/bus/{messages,topics,envelope}.go; internal/orchestrator/optimizer.go (enforcement side); cmd/hub/state.go (adoption side)
- Docs: docs/extension/00_PROGRESS.md, docs/V1RC_FINDINGS.md, AD-016-draft.md; ../csip-tls-test/docs/refactor/{10_BACKLOG.md,PRESERVATION_LEDGER.md} (referenced by CLAUDE.md, live in harness repo)

## Function-set coverage table

| 2030.5/CSIP function set | Status | Evidence | Notes |
|---|---|---|---|
| DNS-SD / xmDNS server discovery (_ieee2030._tls._tcp, TXT path=) | **Partial — implemented but UNWIRED** | internal/northbound/dnssd/browse.go:29-56; no non-test caller (`grep dnssd\.` finds none in cmd/); cmd/northbound/config.go:24 uses static `server` | Package complete (SRV+TXT, zeroconf); production service dials the configured address only. cmd/api/mdns.go:29 advertises `_lexa-hub._tcp` for the phone app — unrelated to 2030.5 |
| DeviceCapability (/dcap) | **Full** | discovery/walker.go:157-175; csipmodel/resources.go:60-78 | Never hardcodes URLs past /dcap (walker.go:6-9); follows EndDeviceList/Time/MUP/ResponseSet/TariffProfile links; pollRate attr consumed (run/pollrate.go:123) |
| Time function set | **Partial** | walker.go:177-188 (CurrentTime→ClockOffset); resources.go:86-106 | Only `currentTime` consumed. DstEndTime/DstOffset/TzOffset/Quality parsed, never read. Offset single-owned by internal/utilitytime (Clock.SetOffset/Anchor/ServerNow, utilitytime.go:204-340; run/run.go:264-296) — monotonic-anchored so local wall-clock steps can't move utility time (TASK-037/GAP-04). CSIP §5.2.1.3 30 s sync met by per-walk resync; poll pacing clamps max staleness to 15 min (pollrate.go:81). DST/local-time correctness delegated to SOM zone + WS-8 `tariff_zone` startup check (cmd/hub/tariffzone.go per CLAUDE.md), not to the Time resource's TzOffset |
| EndDevice / EndDeviceList | **Full (client role)** | walker.go:190-204, findSelfDevice :363-370 (case-insensitive LFDI match) | Single self EndDevice; hub's DERs (battery/solar/EVSE) live behind it via DERList — NOT one EndDevice per DER, and no aggregator multi-EndDevice management. Never POSTs an EndDevice (no in-band registration/creation) |
| SelfDevice (/sdev) | **Absent** | resources.go:77 (link parsed); no fetch site in walker.go | SelfDeviceLink never followed |
| Registration (PIN) | **Partial — helper only, not in the walk** | discovery/helpers.go:10-25 `VerifyRegistration` (BASIC-001/CORE-009, PIN 111115); callers: helpers_test.go only | Production walk never verifies PIN; registration is out-of-band (server pre-provisions EndDevice by LFDI). Conformance procedures exercising PIN would need this wired |
| FunctionSetAssignments | **Full** | walker.go:251-259; per-FSA DERProgram/TariffProfile/CustomerAccount walks :262-335 | |
| DERProgram / primacy | **Full within interpretation** | resources.go:206-226; helpers.go:28-44 HighestPriorityProgram (lowest primacy, mRID tiebreak) | **Absolute-primacy interpretation**: active control resolved from the single highest-priority program only, no cross-program merge — self-documented as "revisit against aggregator BASIC-021..026 matrix" (scheduler.go:17-25) |
| DERControl event lifecycle | **Full (scalar modes)** | scheduler.go:134-293 Evaluate/failClosed; :345-394 activeEvent; :400-449 randomizeStart/Duration cached per MRID (§11.10.4.2); :483-499 supersede (creationTime + overlap + potentiallySuperseded); cancelled currentStatus=6 skipped :353 | Plus non-spec hardening: fail-closed hold of last-known-good on empty/malformed cycle (scheduler.go:144-159), plausibility gate ≤1 GW (:105-111, :327-343), clock-regression guards (:209-293). Explicit server clear releases immediately (:152-159) |
| DefaultDERControl fallback | **Full** | scheduler.go:184-193; walker.go:276-283; helpers.go:46-53 (BASIC-016/CORE-012) | |
| DERCurve consumption | **Partial — fetched + display only** | walker.go:306-318 (Curves map); publish.go:113-127 (DERCurveSummary on 24 h schedule topic); der.go:104-169 full DERCurve model | Curve-linked modes NEVER dispatched to inverters — de-scoped V1.0 (AD-010; csip-tls-test/docs/refactor/10_BACKLOG.md:29-86 has the 10-item missing-pieces list). extendedListToSimple/extendedDefaultToSimple silently drop curve fields before the scheduler (walker.go:472-532); the backlogged "ignored-curve-field alarm" (10_BACKLOG.md:87-97) is not yet implemented |
| DERCapability/DERSettings/DERStatus/DERAvailability — **reporting to server** | **Absent** | tlsclient has GET/POST only, no PUT (grep `func.*Put` → none in internal/tlsclient); no POST of any DER* resource anywhere in cmd/ or internal/northbound | The hub never publishes its own nameplate/settings/status to the head-end — a core CSIP DER-client duty (§10.10.2). Walker only GETs the server's copies (walker.go:214-248) and mirrors them onto MQTT (publish.go:132-161) |
| ModesSupported bitmask | **Absent (constants only)** | der.go:14-42 constants defined; 10_BACKLOG.md:78-81 "unused today" | Never asserted at registration nor used to gate adoption |
| Response function set (GEN.044/CORE-022/023) | **Full for 1,2,3,6,7; extension 0xF0** | responses/tracker.go:221-289 Update (Received/Started/Completed/Cancelled/Superseded); postResponse :312-338 (application/sep+xml); POST path discovered via ResponseSetListLink→ResponseSet[0].ResponseList.Href (walker.go:166-175, 456-468), default /rsps/0/r (config.go:149-151) | Opt-in/opt-out (4/5) never posted. **CannotComply is a LEXA vendor extension, status 0xF0 — not an IEEE Table 27 code** (resources.go:462-469); one POST per breach episode, deduped by episodeID+mRID (tracker.go:120-176), durably persisted WS-4.2 (responses/persist.go; cmd/northbound/config.go:84-100) |
| FlowReservation (§10.9) | **Partial — wired but dormant** | Read side: walker.go:338-343 + discovery/flowreservation.go:47 (FlowReservationResponseList), published retained (publish.go:273-304). Write side: flowres/manager.go:55-103 POSTs FlowReservationRequest on bus request; subscribed at cmd/northbound/main.go:201-211 | **No producer**: nothing in cmd/hub publishes bus.TopicCSIPFRRequest (grep confirms subscriber only), so no FlowReservationRequest is ever POSTed in production. EnergyRequested/PowerRequested marshalled with multiplier 0 (manager.go:74-85) |
| Pricing (§10.5 TariffProfile/RateComponent/TimeTariffInterval) | **Full (consume)** | discovery/pricing.go:13-55 (incl. ActiveTimeTariffIntervalListLink + ConsumptionTariffIntervalList fetchers); walker.go:324-330; publish.go:188-234 → retained lexa/csip/pricing; feeds hub tariff compile (memory: plan-economics) | |
| Billing (§10.7 CustomerAccount/Agreement/BillingPeriod) | **Full (consume)** | discovery/billing.go (walk); walker.go:332-336; publish.go:238-269 | |
| Metering mirror (MUP/MMR) | **Partial** | see "Posting/telemetry coverage" below | |
| Subscription/notification (§ subscribable resources, /ntfy) | **Absent — polling only** | `Subscribable` attrs parsed (resources.go:118-119,176-177,210-211) but never read; no notification HTTP server anywhere; walk loop is pure poll (run/run.go:147-193) | Poll cadence: discovery_interval_s default 60 s (cmd/northbound/config.go:146-148); `poll_rate_mode:"honor"` (product default, config.go:33-45) paces to MAX advertised pollRate, floored at operator interval, capped min(MaxInterval≤24 h, 15 min clock-staleness) (pollrate.go:62-93,123); "override" = fixed cadence (bench). Whole-tree atomic walk each cycle; per-class scheduling + conditional GET backlogged (AD-014) |
| Rewalk / retained-control repair (non-spec, TASK-042) | **Full** | run/run.go:394-458 (rewalkGate 10 s, cached-control republish, single-flight walk); hub side cmd/hub/state.go:196-233 (retained_adoption_max_age_s default 300, reasons "stale"/"decode"); bus/messages.go RewalkRequest | Enforce-but-verify: a stale retained control is still enforced while re-requested |
| Time-of-registration LogEvent function set | **Absent** | LogEventListLink parsed only (resources.go:137); zero other references | |
| Prepayment / Messaging / DRLC / Metering (native, non-mirror) | **Absent (declared out of scope)** | csipmodel package doc resources.go:13-15 | |

## DERControl mode coverage table

Consumed = parsed + carried into scheduler/24 h schedule bus msg. Live-enforce path: scheduler → bus.ActiveControl (only Source/MRID/Connect/ExpLimW/ImpLimW/MaxLimW/FixedW/ValidUntil — bus/messages.go:46-58; publish.go:314-345) → cmd/hub/state.go:824-835 → optimizer.

| opMod* | Consumed? | Enforced by optimizer? | Evidence |
|---|---|---|---|
| opModConnect | yes | **yes** — disconnect batteries + curtail solar to 0, breach-tracked | resources.go:272; publish.go:327; optimizer.go:487-531 (disconnect rule); cease-to-energize precedence also in constraint shadow (ledger E6) |
| opModEnergize | yes (parsed + 24 h schedule slot) | **no** — not in bus.ActiveControl, no live path | resources.go:273; publish.go:62-64; absent from publish.ToActiveControl:314-345 |
| opModFixedW | yes | **yes** — grid dispatch target, solar credited first, battery covers remainder; suppresses plan-following | resources.go:277; publish.go:340-343; optimizer.go:359, 675-681 applyFixedDispatchRule |
| opModMaxLimW | yes | **yes** — absolute generation cap; applyGenLimitRule + convergence backstop w/ meter-independent floor (gen ≥ export − battDischarge) | resources.go:278; optimizer.go:549, 1331-1349, checkGenLimitConvergence:1397; shadow copy constraint/genlimit.go |
| opModExpLimW | yes | **yes** — export cap + battery-absorption guard + checkExportLimitConvergence (CannotComply on sustained gap) | resources.go:279; optimizer.go:546, checkExportLimitConvergence:1292; shadow constraint/export.go (TASK-060, 0-diff gate passed, still shadow) |
| opModImpLimW | yes | **yes** — import cap + checkImportConvergence (NaN-hold counter) | resources.go:281; optimizer.go:552, checkImportConvergence:1499; shadow constraint/importlimit.go |
| opModGenLimW | yes (parsed + schedule slot) | **no** — never mapped into bus.ActiveControl; the enforced "gen limit" is MaxLimW | resources.go:280; publish.go:82-85 (schedule only) |
| opModLoadLimW | yes (parsed + schedule slot) | **no** | resources.go:282; publish.go:86-89 |
| opModFixedPFAbsorbW / opModFixedPFInjectW | yes (schedule slot, /100 display) | **no** — no reactive-power southbound path | resources.go:274-275; publish.go:99-105 |
| opModFixedVar | yes (schedule slot) | **no** | resources.go:276; publish.go:95-98 |
| opModTargetW / opModTargetVar | yes (ExtendedDERControlBase; TargetW in schedule slot) | **no** | der.go:232-233; publish.go:91-93 |
| opModVoltVar / opModFreqWatt / opModWattPF / opModVoltWatt (curve-linked) | yes — curves resolved + summarized on schedule topic | **no** — display only; AD-010 V1.0 de-scope | der.go:236-244; walker.go:306-318; publish.go:114-117; 10_BACKLOG.md:29-86 |
| HFRT/HVRT/LFRT/LVRT MayTrip/MustTrip/MomentaryCessation (10 curve modes) | yes (parsed + schedule summaries) | **no** — ride-through never dispatched to hardware | der.go:246-259; publish.go:118-127 |
| opModFreqDroop (inline params) | yes (schedule slot FreqDroopMsg) | **no** | der.go:262, 177-190; publish.go:107-112 |
| rampTms | yes (parsed, schedule slot) | **no** — ramping comes from local plant model instead | resources.go:283; publish.go:57 |
| storage charge/discharge modes (CSIP opModStorageTargetW etc.) | **absent from model** | n/a — battery dispatch is autonomous/economic, plus setMaxCharge/DischargeRateW parsed read-only in DERSettingsFull (der.go:428-430) | |

Malformed-value defense: adopted Exp/Max/Imp/FixedW limits must decode finite and ≤1e9 W or the control is rejected & last-known-good held (scheduler.go:105-111, 327-343; metric lexa_nb_implausible_rejects_total, run/run.go:299-311).

## Posting/telemetry coverage (cmd/telemetry → MUP)

- Registration: `POST /mup` with MirrorUsagePoint per configured device — mRID = LFDI[0:8]+"-"+device, roleFlags 0x0002, serviceCategoryKind 0 (electricity), status 1, deviceLFDI, postRate (cmd/telemetry/main.go:364-388). Location header saved; re-registered after 3 consecutive POST failures (main.go:263-272).
- Readings: one MirrorMeterReading per quantity per tick, each with self-describing ReadingType (dataQualifier=2 avg, kind, powerOfTenMultiplier, uom, intervalLength — audit finding S-2 closed): **W (uom 38, mult 0) · V (uom 29, ×100, mult −2) · Hz (uom 33, ×100, mult −2)** (main.go:404-457; csipmodel/resources.go:472-487). NaN quantities skipped, never posted (main.go:426).
- Cadence: `mup_post_rate_s` default 300 s (cmd/telemetry/config.go:30,58-60). Timestamps use serverNow = local + ClockOffset learned from retained lexa/csip/control (main.go:189-196, 412).
- **Gaps**: instantaneous W/V/Hz only — no energy (Wh) accumulation, no flowDirection import/export split, no per-phase readings, no battery SOC / DER-status mirroring, no reading batching (one Reading per set), no MUP for the EVSE beyond its device Measurement if configured. lexa-telemetry shares northbound's cert files; no second cert monitor by design (cmd/northbound/config.go:62-67).
- Uncommissioned unit (`server:""`) idles cleanly without certs — no MUP registration (main.go:56-64, 283-362).

## Security posture

- **TLS**: wolfSSL CGo; cipher pinned `ECDHE-ECDSA-AES128-CCM-8` TLSv1.2 (CSIP §5.2.1.1), verified post-handshake, mismatched negotiation torn down (tlsclient/client.go:174-178; config.go:5-7). Server-domain verification via SetVerifyDomain (client.go:163-169). mTLS client cert/key + CA loaded per fetcher (client.go:70-80). wolfSSL_Init once per process, northbound+telemetry only (cmd/northbound/main.go:89; CLAUDE.md invariant).
- **Identity**: LFDI = leftmost 160 bits of SHA-256(full cert DER); SFDI = leftmost 36 bits + digit-sum checksum (§6.3.4) (identity/identity.go:48-86). Derived at boot from client cert unless configured (cmd/northbound/main.go:108-114). Re-issue caveat: LFDI hashes the FULL DER, so even same-key reissue changes it (CLAUDE.md, CERT_ROTATION_RUNBOOK).
- **Sessions**: three isolated wolfSSL sessions — discovery / response-POST / flow-reservation (main.go:98-106); keep-alive with auto-redial (fetcher.go); per-read SO_RCVTIMEO 15 s bound since Go deadlines can't reach wolfSSL's blocking read (client.go:142-161, config.go:12-19); ctx cancellation is between-request only (walker.go:35-48).
- **HTTP semantics**: hand-rolled HTTP/1.1; Accept/Content-Type `application/sep+xml` only (request.go:24-46) — **no EXI (application/sep-exi)**, no conditional GET/ETag, no PUT/DELETE verbs. Hostile-server caps: 64 KiB header / 10 MiB body; chunked Transfer-Encoding DECODED and size-capped (AD-009 option (b), TASK-069) with 4 KiB chunk-line cap; CGo-free fuzzed parser (client.go:18-28; httpwire/httpwire.go:17-48). CRLF/space injection guard on path/host (request.go:11-16).
- **Cert monitoring** (TASK-072): startup + 24 h inspection of client+CA PEMs, retained `lexa/northbound/certstatus`, gauges lexa_cert_expiry_{client,ca}_seconds / lexa_cert_expiring_{client,ca}, WARN ≤ cert_expiry_warn_days (default 30), fail-closed reporting of unreadable certs (cmd/northbound/certmon.go:63-180; folds into GET /status cert_status, cmd/api/handlers.go:46).
- **Cert rotation** (TASK-073): sentinel-file trigger (default /etc/lexa/certs/rotate.request, poll 5 s) → probe-then-commit Reload per fetcher (real GET must be 200 — catches wrong-device certs that pass the handshake but 403 at CSIP layer), one at a time, old session freed under existing mutex (tlsclient/fetcher.go:78-150; cmd/northbound/rotate.go:124-276). 24 h reconnect-churn soak still pending (CLAUDE.md).
- **Clock integrity**: utilitytime Clock — offset step classification (Wobble ≤60 s / Step ≥30 s), monotonic anchoring per successful walk so local wall steps during outages can't shift ServerNow (utilitytime.go:85-158, 204-340; run/run.go:264-296).
- **cmd/api**: loopback-default :9100, bearer-token (non-loopback bind without token refused unless bench), TLS w/ api_cert_fp pinning surface (cmd/api/tlscert.go), contract Version=1 header X-Lexa-Contract-Version (internal/apicontract/version.go:21-25). Routes /healthz /status /logs /site /devices /telemetry/recent /mode /plan /intent /scan /config/ /metrics (cmd/api/main.go:310-345). Standards-evidence surfaces: /status carries cert_status + plan_heartbeat (handlers.go:41-46); retained lexa/hub/plan heartbeat with arrival-time (not payload-Ts) stall detection (heartbeat.go per CLAUDE.md).

## Known-deferred items from docs

| Item | Where | Status |
|---|---|---|
| Volt-var/volt-watt (all curve-linked + ride-through) closed-loop dispatch | AD-010, 10_BACKLOG.md:29-86 (10 sub-items, 2S/5M/3L) | de-scoped V1.0; revisit trigger = pilot/LOI or cert-lab scope referencing curve function sets |
| Ignored-curve-field alarm (flag silently-dropped curve controls) | 10_BACKLOG.md:87-97 | recommended companion, not done |
| Conformance ADV curve test group + asserting DERCapabilityFull.ModesSupported at registration | 10_BACKLOG.md:78-81 (item 10) | backlog |
| Per-class independent poll scheduling + conditional GET | AD-014 / CLAUDE.md TASK-071 note; northbound/CLAUDE.md:49-65 | backlog (whole-tree atomic walk today) |
| net.Conn shim under http.Transport (replace hand-rolled HTTP) | AD-009, httpwire.go:26-30 | deferred P6-with-time |
| Cert-rotation 24 h reconnect-churn soak | CLAUDE.md (TASK-073); CERT_ROTATION_RUNBOOK.md | code-complete, soak-pending |
| Cross-program primacy interpretation vs aggregator BASIC-021..026 | scheduler.go:17-25 | deliberate interpretation; revisit needs a Test Server |
| Second gridsim instance → DNS-SD flap / wrong-server scenarios | 10_BACKLOG.md:129 | backlog (DNS-SD itself unwired — see above) |
| OCPP Security Profile 3 (mTLS) | CLAUDE.md TASK-074 note; 10_BACKLOG.md:122 | backlog (adjacent, not 2030.5) |
| Bus envelope v0-tolerance flip to reject-only | AD-006 / CLAUDE.md | deliberate transition state |
| Multi-site / multi-meter topology semantics | 10_BACKLOG.md:98-100 | undefined today |
| Constraint-stack P5 flips (export/gen/import/battery-safety) | CLAUDE.md TASK-059/060-062 | live cascade authoritative; stack shadow-only |
| Bench-deferred validation queue (Mayhem gates for units 3.1 etc.) | docs/extension/00_PROGRESS.md:24-26,109 | waiting on bench |
| V1RC FINDING A (power-cut retained rollback / api start-limit), B (false CannotComply on sub-threshold dither), D (/var/lib/lexa provisioning) | docs/V1RC_FINDINGS.md:9,44,56 | tracked findings |
| TASK-041 NB response-state persistence | AD-016-draft.md | CLOSED (WS-4.2, commit 55f8c44) — draft AD awaiting paste into harness repo |

## Notable absences (2030.5/CSIP vocabulary)

1. **No DERCapability/DERSettings/DERStatus/DERAvailability reporting to the server** — no PUT verb exists in tlsclient at all; the hub never tells the head-end its nameplate, settings, or live status (a core CSIP DER-client duty and a conformance-test staple). GET-and-mirror only.
2. **No subscription/notification mechanism** — pure polling client; `subscribable` attributes ignored; no HTTP notification listener.
3. **No EXI media type** — `application/sep+xml` only, both Accept and Content-Type.
4. **Registration PIN verification never runs in production** — VerifyRegistration exists (helpers.go:13) but is test-only; commissioning is out-of-band by LFDI pre-provisioning.
5. **DNS-SD implemented but not wired into lexa-northbound** — server address is static config; xmDNS discovery would need plumbing from dnssd.Browse into main().
6. **opModGenLimW / opModLoadLimW / opModEnergize / all PF-var-target modes parsed but unenforced** — only Connect/FixedW/MaxLimW/ExpLimW/ImpLimW reach the live control path (bus.ActiveControl, messages.go:46-58).
7. **All curve-linked modes (VoltVar/FreqWatt/WattPF/VoltWatt, FreqDroop, 10 ride-through curves) display-only**; silently reduced to scalars for the scheduler with no alarm.
8. **CannotComply uses a non-standard Response status (0xF0)** — interoperable only with servers treating ≥0xF0 as vendor alerts (gridsim does); a strict 2030.5 head-end would reject/ignore it.
9. **Telemetry mirrors only instantaneous W/V/Hz** — no Wh energy, flowDirection, phase, or storage SOC readings; no MirrorReadingSet batching.
10. **FlowReservation request path dormant** — subscriber wired, but no hub-side producer of lexa/csip/flowreservation/request exists; only server→hub reservation status flows.
11. **Single-EndDevice model** — per-DER EndDevice (aggregator-style) not modeled; DERList under self is the only per-DER granularity.
12. **Time resource's TzOffset/DstOffset/DstEndTime unused** — local-time correctness rests entirely on SOM zone config + WS-8 assertion.
13. **SelfDevice and LogEvent function sets untouched**; Prepayment/Messaging out of scope by design (resources.go:13-15).
14. **No XSD validation** — encoding/xml with namespace-tagged XMLName roots (mis-namespaced roots error rather than zero-decode, resources.go:17-23), but no schema-level validation of element order/cardinality.
