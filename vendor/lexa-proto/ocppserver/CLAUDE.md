# OCPP 2.0.1 CSMS (ocppserver/)

## Purpose
Central System Management System for EV chargers. Pure Go — intentionally
decoupled from wolfSSL. Do NOT wire wolfSSL here; this uses Go's `crypto/tls`,
not CSIP mTLS.

**Shared module (TASK-022):** this package used to be forked verbatim into
`lexa-hub/internal/ocppserver` (the production CSMS, consumed by `cmd/ocpp` —
the lexa-ocpp service, :8887, which bridges EVState onto MQTT) and
`csip-tls-test/internal/ocppserver` (the bench's copy, embedded in gridsim's
`sim/server` and exercised by `sim/evsim`'s test harness). Both consumers now
import `lexa-proto/ocppserver` directly — there is exactly one copy. Tested
end-to-end against `sim/evsim` in `simulator_test.go`.

## Security Profile 2
TLS over WebSocket + HTTP Basic Auth (credential checked per-connection).
Basic Auth comparison must stay `subtle.ConstantTimeCompare` (audit OCPP-3).

## OCPP-1 invariant — do not violate
Charging sessions are carried by `TransactionEvent` Started/Updated/Ended
lifecycles, never bare MeterValues. Consumers may also observe legacy bare
MeterValues during a transaction (some stations send both), but the
transaction lifecycle itself must always be driven by TransactionEvent, not
inferred from MeterValues alone.

## Handlers
| Handler | Behavior |
|---|---|
| OnGetBaseReport / OnGetReport | provisioning no-ops, status Accepted |
| SetChargingProfile | stores limit_A from first ChargingSchedulePeriod |
| TriggerMessage | re-sends current status for all connectors |

## EVState (exposed via API)
```go
connected   bool
connectors  map[int]string          // connector_id -> status
last_meter  {connector_id, current_A, energy_Wh}
last_profile {connector_id, limit_A}
last_heartbeat string
```
In lexa-hub, EVState is published by `cmd/ocpp` to MQTT
`lexa/evse/{station}/state` (inspect via `mosquitto_sub` or `lexa-api`
`/status` on :9100).

## Driving it in tests / on the bench
Port 6024 is **evsim's simapi sidecar** (the charging *station* sim, in
csip-tls-test), not part of this package. To provoke CSMS behaviour, inject
into evsim:
```json
POST http://<ev-pi>:6024/inject
{"status":"Faulted","connector_id":1}        // station sends StatusNotification
{"action":"start_session","connector_id":1}  // station starts a TransactionEvent lifecycle
{"action":"stop_session"}                    // station ends the transaction
```

## Adding new OCPP handlers
Implement the relevant interface method on `csHandler`, register via
`cs.SetXxxHandler()`.
