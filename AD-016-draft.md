<!--
Coordinator note (not part of the AD text): apply this as a new section in
csip-tls-test's docs/refactor/02_ARCHITECTURE_DECISIONS.md, inserted after
AD-014 (ends "## Superseded / rejected log" at the file's current tail) and
before that "Superseded / rejected log" table — i.e. append it as the new
last AD, immediately above the "---" that precedes the table. Everything
between the "===AD-016 BEGIN===" and "===AD-016 END===" markers below is the
literal section to paste in (markers themselves excluded).

This branch (lexa-hub task/ws4a-restart-safe) implements the northbound
persistence half this AD documents:
  - b2f82b7 feat(hub): snapshot default-on (WS-4.1)
  - 55f8c44 feat(northbound): persist responseTracker posted/alerted (WS-4.2)

Also worth a small follow-up edit while this AD lands: csip-tls-test's
docs/refactor/10_BACKLOG.md:180 currently carries the line

  "TASK-041 northbound snapshot half: persist responseTracker.alerted/posted
  so a NB restart mid-episode does not re-post CannotComply begin (hub half
  done in 041)."

— that's the mis-filed entry HANDOFF §8 WS-4.2 points at. It's resolved by
55f8c44 and should be deleted from 10_BACKLOG.md (not moved — the AD text
below is the permanent record) as part of applying this patch.
-->

===AD-016 BEGIN===
## AD-016 ✅ NB responseTracker persistence is inside the Restart-safety gate (TASK-041's northbound half)

**Problem.** TASK-041 (AD-005) shipped the hub-side breach-episode snapshot
but explicitly left `internal/northbound/responses` (`responseTracker`'s
`posted`/`alerted` maps) RAM-only, and that gap was mis-filed as backlog
(`10_BACKLOG.md:180`) rather than tracked against the restart-safety release
gate. The 2026-07-07 independent audit (HANDOFF §8 WS-4.2) re-raised it as a
regulatory-record defect, not a nice-to-have: the hub's compliance-alert
edge (`bus.ComplianceAlert`, non-retained MQTT) fires exactly once per
breach episode onset. A `lexa-northbound` restart landing between that edge
and the outbound CannotComply POST silently drops a Response the utility is
contractually owed — and, independently, a restart mid-event-lifecycle with
no persisted `posted` map re-POSTs a duplicate Received/Started/... for an
event the service already acknowledged. Both are protocol-record defects,
not internal bookkeeping: CORE-022/023's Response state machine and the
CannotComply extension are the utility-facing compliance audit trail this
program exists to keep honest across a restart (`09_RELEASE_CHECKLIST.md`'s
"Restart safety" ◆ gate is exactly this class of guarantee for the hub side
already; the audit's read — now this AD's decision — is that the northbound
half is the same guarantee, not a separate, deferrable concern).

**Decision.** NB `responseTracker` persistence is INSIDE the ◆ "Restart
safety" gate in `09_RELEASE_CHECKLIST.md`, on equal footing with the hub-side
breach snapshot (AD-005/TASK-041) and the WS-2 desired-doc staleness fix — a
lost CannotComply is a regulatory-record defect, and the release checklist
must not go green while it is possible. `10_BACKLOG.md:180`'s entry is
removed (resolved, not deferred).

**Implemented by:** `lexa-hub` `task/ws4a-restart-safe`
(`b2f82b7`/`55f8c44`), same branch that flipped the hub snapshot's shipped
default to `true` (WS-4.1) once its write-only-soak precondition
(2026-07-08 8-cycle `hub-restart-mid-cap` campaign, snapshot OFF, PASS) was
met. `internal/northbound/responses/persist.go` adds a small self-compacting
NDJSON append-log (`Store`/`LoadState`), deliberately NOT
`internal/journal.Writer` reused as-is: journal's byte-size rotation has no
"this entry is still live" concept — a non-terminal `posted[]` entry could
roll off the back of `MaxFiles` during a long-running process, which is the
wrong compaction semantic for "reconstruct current dedupe state" versus
journal's actual job of bounded forensic history — and journal exposes no
replay-to-current-state `Load` API. The new store's compaction instead
rewrites to exactly the live state (terminal-status `posted[]` entries
pruned using `terminalResponse`, the event-lifecycle signal the tracker
already had) past a size threshold, atomically (tmp+rename+fsync, the same
technique AD-005's hub snapshot already uses). `AlertCannotComply` persists
its `alerted` mark only after `postResponse` confirms success — deliberately
lagging the in-memory mark, which still gates same-process retries unchanged
— so a crash between "alert received" and "POST confirmed" leaves nothing
durable and a redelivered alert genuinely retries after restart instead of
being swallowed by a "recorded but never sent" ghost dedupe entry. Path is a
new `northbound.json` key (`response_state_path`), defaulting to
`/var/lib/lexa/journal/northbound/response-state.ndjson` — a sibling of the
journal dir, created independently of whether `"journal"` is configured, so
no bench config change is required; the literal `"off"` opts out, matching
`metrics_addr`'s convention. Load failure (missing or corrupt) is a WARN,
never fatal (AD-011) — starts empty, matching the pre-AD-016 behavior.

**Known residual (flagged, deliberately not fixed by this AD):**
`responseTracker.activeMRID` (which event is currently "active" for the
Started/Completed pass) is not persisted — only `posted`/`alerted` are, per
the original TASK-041 scope this AD closes. A restart at ANY point still
re-fires one Started POST for whichever event was active at that moment;
this is pre-existing (true before AD-005/TASK-041 too) and independent of
this AD's fix. Left for a follow-up if bench evidence shows it is more than
cosmetic protocol noise.

**Gate.** `09_RELEASE_CHECKLIST.md`'s Restart-safety ◆ box requires
`hub-restart-mid-cap` PLUS a new `northbound-restart-mid-breach` scenario
(kill NB between the hub's alert edge and the POST) to post exactly one
CannotComply per episode, campaigned both snapshot-on and snapshot-off
(HANDOFF §8 WS-4's gate note) — control-plane-adjacent per 05 §12, batched
at the wave boundary with WS-2, not run by this branch (unit-gated only:
`go test ./... -count=1` + `go test -race ./internal/northbound/... -count=1`
green on `lexa-hub`).

**Alternatives considered.** Reuse `internal/journal.Writer` unmodified —
rejected, wrong compaction semantic (above). A single atomic JSON blob
(mirroring `cmd/hub/snapshot.go` exactly, full-rewrite on every change) —
viable and simpler, but the task brief's write pattern (append per
transition, compact on a size boundary) maps more directly onto an NDJSON
log with periodic compaction, and the two designs converge on the same
worst-case disk/fsync behavior at this data's scale (a handful of live
entries) — not reconsidered further absent a concrete reason to prefer one.
Persisting `activeMRID` too, closing the residual above in the same
patch — descoped: the branch's launch instructions scoped WS-4 item 2 to
"the posted/alerted maps," and closing `activeMRID` cleanly needs its own
acceptance criteria (what "restart mid-active-event" should even POST) that
this branch did not have direction to define.
===AD-016 END===
