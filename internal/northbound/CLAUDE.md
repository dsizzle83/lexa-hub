# IEEE 2030.5 Northbound Stack

## Packages
```
model/      Go structs with XML tags. Root elements MUST have xmlns="urn:ieee:std:2030.5:ns".
discovery/  Link walker starting at /dcap. Follows href attributes — never hardcodes URLs past /dcap.
identity/   LFDI = leftmost 160 bits of SHA-256(cert DER). SFDI = first 36 bits decimal.
scheduler/  DER event state machine (cancelled, superseded, randomized-start, primacy, default fallback).
dnssd/      mDNS browse for _ieee2030._tls._tcp. TXT "path=X" overrides /dcap default.
run/        The discovery walk loop (Discovery.Loop/RunOnce) + TASK-042 rewalk
            single-flight (lastPublishedStore/rewalkGate/handleRewalkRequest).
            Owns the fail-closed walk-error hold — see RunOnce's doc comment.
publish/    Converts discovered resource state to bus messages and publishes
            retained (Schedule/Pricing/Billing/FlowReservations/ToActiveControl).
responses/  CORE-022/GEN.044 Response state machine (Tracker) — one
            CannotComply per breach episode (TASK-031 dedupe).
flowres/    §10.9 Flow Reservation client (Manager) — decodes hub requests,
            POSTs FlowReservationRequest.
```
`run`/`publish`/`responses`/`flowres` were extracted from the former
`cmd/northbound/main.go` god-file (TASK-068, D12/R5) as pure moves — no
behavior change. `cmd/northbound/main.go` is wiring only (config, TLS
fetchers, MQTT connect/subscribe, signal handling); it constructs one
`run.Discovery` and runs `discovery.Loop` in its own goroutine.

## Fetcher interface
`discovery.Fetcher`: `Get(path) ([]byte, error)` only. Keeps discovery decoupled from TLS.
- `WolfSSLFetcher` (tlsclient/): persistent TLS session, sync.Mutex, auto-redial on error.
- Conformance/integration tests against gridsim live in the **harness repo** (`~/projects/csip-tls-test`, `go test ./tests/` there) — this repo has unit tests only (`make test`).

## Walker traversal order
`/dcap` → Time (→ ClockOffset) → EndDeviceList (find self by LFDI) → DERList → FSAList → DERPrograms → DERControlList + DefaultDERControl → MUPList

**ClockOffset**: `serverNow = time.Now().Unix() + tree.ClockOffset` — the formula is unchanged but now **single-owned by `internal/utilitytime`** (AD-004, TASK-035): `cmd/northbound` feeds each walk's `tree.ClockOffset` to a `utilitytime.Clock` (`clk.SetOffset`) and reads `serverNow` back via `clk.ServerNow()`; the scheduler's expiry/window checks delegate to `utilitytime.Expired`/`InWindow`. `scheduler.ServerNow` is retained but deprecated. Required — CSIP §5.2.1.3 requires client within 30 s of server. Pass `serverNow` to every `scheduler.Evaluate()` call.

## Scheduler priority rules
1. `currentStatus=6` (cancelled) → always skip.
2. `potentiallySuperseded=true` + later event covers same window + later `creationTime` → later wins.
3. Randomized start: apply rand offset to startTime once per MRID; cache result.
4. Primacy: lower number wins (program primacy 1 beats 5 beats 10).
5. Default fallback: no active event in highest-priority program → use `DefaultDERControl`.
6. **Fail closed** (audit: malform-empty-program / malform-huge-activepower): a cycle that
   resolves *no* control (empty/missing programs) or a *malformed* one (an `OpModXxxLimW`
   that decodes non-finite or > `maxPlausibleLimitW` = 1 GW) does NOT drop an adopted
   control. `Evaluate` re-serves the last-known-good control (with `ActiveControl.Held=true`)
   until its own `ValidUntil` expires; a malformed control is never adopted nor stored as
   last-known-good. Only an actually-expired (or never-set) last-known-good yields `nil`.

## Walk-cadence pacing (TASK-071, §12, AD-014)
`run/pollrate.go`'s `effectiveInterval` paces `Discovery.Loop`'s per-cycle
wait: `poll_rate_mode: "honor"` (product default, `cmd/northbound/config.go`)
throttles the walk to the MAX of every advertised class pollRate seen in the
last successful tree (DeviceCapability/Time/each DERProgram's
DERControlList) — floored at the operator's `discovery_interval_s`, capped
at `min(MaxInterval, 15 min)` (the 15-min clock-resync max-staleness bound
is unconditional in every mode). `poll_rate_mode: "override"` (BENCH
default — `configs/northbound.json` ships it explicitly) ignores the tree
entirely and reproduces the pre-TASK-071 fixed cadence byte-for-byte —
required because gridsim's DERControlList pollRate (60s) would otherwise
push Mayhem's adoption-latency-sensitive scenarios (`expired-control`,
`conflicting-primacy`, `clock-jump-forward`) well past their expected
few-second adoption window. The walk itself is still one atomic whole-tree
`discovery.Walker.Discover` call every cycle (unchanged) — this paces WHEN
that call happens, not what it fetches; per-class independent scheduling
and conditional-GET (gridsim has none) are backlogged, see AD-014.

## MirrorUsagePoint telemetry flow
1. `POST /mup` → 201 + Location header (e.g. `/mup/0`). Save location.
2. `POST /mup/{n}` with `MirrorMeterReading` XML → 204. Post per measurement update.
MUP XML must include `xmlns="urn:ieee:std:2030.5:ns"` on root element.

## DNS-SD
`dnssd.Browse(ctx)` returns `[]Server{Host, Port, DCAPPath}`.
Works Pi-to-Pi on a switch (mDNS multicast). Times out cleanly in WSL2 — use `--server` flag there.
