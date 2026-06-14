# OCPP 2.0.1 CSMS (internal/ocppserver/)

## Purpose
Central System Management System for EV chargers, consumed by `cmd/ocpp` (lexa-ocpp service, :8887) which bridges EVState onto MQTT. Pure Go — intentionally decoupled from wolfSSL. Do NOT wire wolfSSL here; this uses Go's `crypto/tls`, not CSIP mTLS.

**Lockstep**: a copy of this package lives in `csip-tls-test/internal/ocppserver` (used by the harness's tests). Protocol-level changes must land in both copies.

## Security Profile 2
TLS over WebSocket + HTTP Basic Auth (credential checked per-connection).
Cert pair: `certs/ev-server-cert.pem` / `certs/ev-server-key.pem`.
Configure via `cfg.BasicAuthUser` / `cfg.BasicAuthPass`.

## Implemented message handlers
| Message | Behaviour |
|---------|-----------|
| BootNotification | → Accepted + server time |
| Heartbeat | → CurrentTime |
| StatusNotification | → Accepted; updates connector status map |
| Authorize | → Accepted (no real IdToken check yet) |
| TransactionEvent | Started/Updated/Ended lifecycle — owns session state + energy_Wh (bare MeterValues kept only for backward compat) |
| SetChargingProfile | stores limit_A from first ChargingSchedulePeriod |
| TriggerMessage | re-sends current status for all connectors |

## EVState
```go
connected   bool
connectors  map[int]string          // connector_id → status
session     {active, connector_id, start_time, energy_Wh}
last_profile {connector_id, limit_A}
last_heartbeat string
```
In this repo EVState is published by `cmd/ocpp` to MQTT `lexa/evse/{station}/state`
(inspect via `mosquitto_sub` or `lexa-api` `/status` on :9100). The HTTP inject API on
port 6024 belongs to **evsim** in the harness repo — drive the *station* there to provoke
CSMS behaviour here.

## Adding new OCPP handlers
Implement the relevant interface method on `csHandler`, register via `cs.SetXxxHandler()`.
Use the lorenzodonini/ocpp-go type aliases: `ocpp2` package prefix.
Field names follow the library: `IDToken` (not `IdToken`), `ID` on struct embeds, etc.
