# Journal forensics — reading the TASK-040 event journal

*TASK-040 wires `internal/journal` (TASK-039) into the running system:
control adoptions/releases, dispatches, breach episodes, and CannotComply
POSTs are now durably recorded, with an episode ID that lines up the journal
against `journalctl` for incident diagnosis. This is the one-page runbook
for reading it.*

## File locations

Config-gated per service (`"journal"` block in `/etc/lexa/{hub,northbound}.json`
— see `configs/hub.json`/`configs/northbound.json` for the shipped example).
No block ⇒ no journal, no files, every emit call site is a no-op.

The two services **must not share a directory**: each `journal.Writer` keeps
its own in-memory `size`/`seq` state and is only safe for concurrent use from
multiple goroutines *within one process* — two processes pointed at the same
`dir`+`name` would race independently on rotation and corrupt each other's
output. The shipped example configs give each its own subdirectory:

```
/var/lib/lexa/journal/hub/journal.ndjson            (+ .1 .. .4 rotated)
/var/lib/lexa/journal/northbound/journal.ndjson      (+ .1 .. .4 rotated)
```

Default rotation: 1 MiB active file × 4 rotated = 5 MiB resident cap per
service, regardless of input rate (see `internal/journal/journal.go`'s
package doc for the write-budget derivation — this sits comfortably inside
the existing journald flash budget in `docs/FLASH_BUDGET.md`, not stacked on
top of it).

## Event vocabulary

One NDJSON line per event, oldest-to-newest across `name.4 .. name.1` then
the active `name` file. Envelope: `v`, `ts` (local wall clock), `srv_t`
(utility/server time, when known), `seq` (per-writer monotonic), `type`,
`svc` (`"hub"` | `"northbound"`), `data` (the typed payload).

| `type` | Emitted by | When |
|---|---|---|
| `service_start` | both | once per process start |
| `control_adopted` | hub (`cmd/hub/state.go` `onCSIPControl`) | a CSIP control's resolved identity/limits change (never on an unchanged retained republish) |
| `control_released` | hub (`onCSIPControl` + `ReadSystemState`'s expiry-drop branch) | `reason`: `cleared` (source → none), `replaced` (mRID switch, no intervening clear), or `expired` (aged past `ValidUntil` with no replacement message) |
| `dispatch` | hub (`cmd/hub/desired.go`, all three actuators) | a desired-state doc is actually published (post content-dedupe — a repeat tick with unchanged intent journals nothing) |
| `breach_begin` / `breach_end` | hub (`cmd/hub/breach.go`'s `breachEpisodes` component) | the same edges that publish a `bus.ComplianceAlert`: one begin per episode onset/mRID-switch, one end when all evidence sources clear |
| `cannot_comply_posted` | northbound (`cmd/northbound/main.go`'s `responseTracker.alertCannotComply`) | after a successful (not merely attempted) CannotComply Response POST |

## `jq` one-liners

```bash
# Every breach onset, most recent first
jq -r 'select(.type=="breach_begin") | "\(.ts) \(.data.episode_id) mrid=\(.data.mrid) shortfall_w=\(.data.shortfall_w)"' \
  /var/lib/lexa/journal/hub/journal.ndjson | tail -20

# Full lifecycle for one episode ID (begin -> CannotComply -> end)
EPISODE='DERC-SP-002@1751500000#3'
jq -r --arg ep "$EPISODE" 'select(.data.episode_id == $ep)' \
  /var/lib/lexa/journal/hub/journal.ndjson /var/lib/lexa/journal/hub/journal.ndjson.1
jq -r --arg ep "$EPISODE" 'select(.data.episode_id == $ep)' \
  /var/lib/lexa/journal/northbound/journal.ndjson

# All dispatches to one device today
jq -r --arg dev "battery-0" 'select(.type=="dispatch" and .data.device == $dev)' \
  /var/lib/lexa/journal/hub/journal.ndjson

# Write-volume spot check (should be KBs after a normal QA run, not MBs —
# see "Common mistakes to avoid" in TASK-040: an ungated per-tick emit is a
# regression, not a feature)
wc -l /var/lib/lexa/journal/hub/journal.ndjson
du -h /var/lib/lexa/journal/*/journal.ndjson*
```

## Correlating with `journalctl`

The episode ID minted at `breach_begin` (`cmd/hub/breach.go`'s
`fmt.Sprintf("%s@%d#%d", mrid, issuedAt, counter)`) is stamped onto the
outgoing `bus.ComplianceAlert.EpisodeID` and is **already** included in the
hub's own structured log line for the onset
(`cmd/hub/main.go`'s `emitAlerts` closure):

```
lexa-hub: COMPLIANCE BREACH limit_type=... limit_w=... measured_w=... shortfall_w=... reason=... mrid=... episode=<episode-id>
```

So the same ID that appears in the journal's `breach_begin`/`breach_end`/
`cannot_comply_posted` records also appears in `journalctl` (the *log*, not
this event journal) at the moment of onset:

```bash
# Full journald timeline for one episode, hub side
journalctl -u lexa-hub | grep '<episode-id>'

# Northbound's CannotComply POST log line for the same episode
journalctl -u lexa-northbound | grep '<episode-id>'
```

For an episode reported purely from reconciler non-convergence (no meter
breach), the same episode ID is on the "compliance breach cleared" line too,
and on every `bus.ComplianceAlert` publish in between — so grepping either
`journalctl` or `mosquitto_sub -t lexa/csip/compliance/alert` by the ID gives
the same timeline as the two event-journal `jq` queries above.

## What this journal is *not*

It does not replace retained MQTT (still the bus recovery mechanism, AD-013)
and it deliberately has no per-tick "everything is fine" event (05 §9) — it
is a transition-only audit trail: adoptions, dispatches, and breach/response
lifecycle edges only. A steady-state plant with no faults and no control
churn should show a nearly-empty journal.
