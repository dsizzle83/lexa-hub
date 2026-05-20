# OCPP 2.0.1 CSMS (internal/ocppserver/)

## Purpose
Central System Management System for EV chargers. Pure Go — intentionally decoupled from wolfSSL. Do NOT wire wolfSSL here; this uses Go's `crypto/tls`, not CSIP mTLS.

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
| TransactionEvent | stores session, accumulates energy_Wh |
| SetChargingProfile | stores limit_A from first ChargingSchedulePeriod |
| TriggerMessage | re-sends current status for all connectors |

## EVState (exposed via API)
```go
connected   bool
connectors  map[int]string          // connector_id → status
session     {active, connector_id, start_time, energy_Wh}
last_profile {connector_id, limit_A}
last_heartbeat string
```

## Inject via HTTP API (port 6024)
```json
{"status":"Faulted","connector_id":1}       // force connector status
{"action":"start_session","connector_id":1}  // simulate plug-in
{"action":"stop_session"}                    // simulate unplug
```

## Adding new OCPP handlers
Implement the relevant interface method on `csHandler`, register via `cs.SetXxxHandler()`.
Use the lorenzodonini/ocpp-go type aliases: `ocpp2` package prefix.
Field names follow the library: `IDToken` (not `IdToken`), `ID` on struct embeds, etc.
