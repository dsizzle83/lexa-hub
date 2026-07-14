# OCPP 1.6J CSMS (ocppserver16/)

## Purpose
Central System for OCPP 1.6J EV chargers — the compatibility-mode sibling of
`ocppserver/` (OCPP 2.0.1, which stays the product-native protocol). Pure Go —
intentionally decoupled from wolfSSL. Do NOT wire wolfSSL here; this uses Go's
`crypto/tls`, not CSIP mTLS.

**Scope: Core + SmartCharging profiles ONLY.** Firmware, LocalAuthList,
Reservation, RemoteTrigger and the 1.6 security extensions are out of scope —
the LEXA hub needs boot/status/transaction/meter ingestion plus
Set/ClearChargingProfile actuation, nothing else. Widen deliberately, not by
accretion.

**Shared-bridge intent (WP-12):** `lexa-hub/cmd/ocpp` runs this as a SECOND
listener (`port_16`, default off; :8886 vs 2.0.1's :8887 — ocpp-go gives each
version its own `ws.WsServer`, there is no shared-listener subprotocol
dispatch) whose forwarders feed the SAME `mqttBridge` state map as the 2.0.1
handlers: Boot/Start-StopTransaction map onto the TransactionEvent-shaped
station lifecycle, MeterValues fold into the same plausibility-gated metering,
StatusNotification onto the connector map. This package therefore stays a thin
protocol shim — session/state logic belongs in the consumer's bridge, not here.

## Security Profile 2 analog
TLS over WebSocket + HTTP Basic Auth (credential checked per-connection),
same `Config` shape as `ocppserver.Config` so one config block can populate
both listeners. Basic Auth comparison must stay `subtle.ConstantTimeCompare`
(audit OCPP-3).

## Session-lifecycle invariant (OCPP-1 analog)
In 1.6, charging sessions are carried by `StartTransaction`/`StopTransaction`
(the CSMS assigns the transaction ID in the StartTransaction confirmation),
with `MeterValues` referencing the transaction in between — the 1.6 analog of
2.0.1's TransactionEvent Started/Updated/Ended rule. Consumers must drive
session lifecycle from Start/StopTransaction, never infer it from bare
MeterValues.

## Handler seam
`New(cfg)` installs a default handler: minimal accepted response for every
Core message (Boot → Accepted/60 s heartbeat, Authorize → Accepted,
StartTransaction → Accepted + package-assigned sequential tx ID, etc.).
Consumers install callbacks via `Server.SetHandlers(Handlers{...})` — only
the five bridge-relevant messages (Boot / StatusNotification /
StartTransaction / StopTransaction / MeterValues) are seam-covered; a nil
callback keeps the default. **SetHandlers must be called before Start** (no
synchronization on the handler fields).

CSMS-initiated sends (SetChargingProfile, ClearChargingProfile,
TriggerMessage, RemoteStart/Stop) go through `Server.CentralSystem()` —
mirroring `ocppserver.Server.CSMS()`. Note ocpp-go's 1.6 send API is
callback-async (`SetChargingProfile(id, callback, ...)`), unlike some 2.0.1
paths; the consumer's bridge owns timeout/rejected-as-error handling (L11).
