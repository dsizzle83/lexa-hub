---
name: mqtt-debug
description: Trace data and control flow across the MQTT bus — which service publishes what, subscription recipes, and the end-to-end checklist for "control didn't reach the device" / "no measurements" / stale-state bugs.
---

# MQTT bus debugging

Broker: mosquitto on the hub, localhost:1883 (services) — from the bench it's
`ssh dmitri@69.0.0.1` first, then mosquitto_sub locally (the listener is localhost-only).

## Watch everything
```bash
mosquitto_sub -h localhost -t 'lexa/#' -v
```

## Per-flow recipes (topic → publisher → consumers)
| Watch | Tells you |
|---|---|
| `lexa/measurements/+` | lexa-modbus is polling devices (feeds hub + telemetry) |
| `lexa/battery/+/metrics` | battery detail (SoC etc.) reaching the optimizer |
| `lexa/csip/control` | northbound walker delivering DER controls (**retained**, QoS 1) |
| `lexa/evse/+/state` | lexa-ocpp seeing the charging station |
| `lexa/control/battery/+`, `lexa/control/solar/+` | optimizer issuing device commands |
| `lexa/evse/+/command` | optimizer issuing EV commands |

## End-to-end trace: "grid event didn't change device behaviour"
Follow the chain; the first silent link is the broken one.
1. gridsim has the event? `curl -s http://69.0.0.20:11112/...` admin API / dashboard Grid tab.
2. Walker picked it up? `mosquitto_sub -t lexa/csip/control -v` (retained — you get the
   last value immediately; check its timestamp/content, stale retained payloads mislead).
3. Optimizer reacted? `journalctl -u lexa-hub` + `mosquitto_sub -t 'lexa/control/#' -v`.
4. Modbus write landed? `journalctl -u lexa-modbus`, then read back the register via the
   sim's simapi `GET /registers` (ports 6020/6021/6022 — see BENCH.md).
5. Meter balance moved? `curl -s http://69.0.0.12:6022/state`.

## Known traps
- **Stale retained control**: `lexa/csip/control` is retained by design; after bench
  resets, a subscriber can act on yesterday's control. Clear with
  `mosquitto_pub -t lexa/csip/control -r -n` (publishes empty retained = delete).
- **NaN**: a NaN that sneaks into a JSON message fails unmarshal at every subscriber —
  bus types use `*float64` (nil = absent) for a reason.
- **Clock offset**: events firing at the "wrong" time usually means a missing
  `+ ClockOffset` somewhere, not a bus problem.
- Message shapes + topic constants: `internal/bus/`. Never match topics by string literal.
