# Southbound Stack (Modbus / SunSpec)

Pure Go — zero cgo. Implements the `Device` interface, polled by the `registry` and
driven by the `cmd/modbus` service (which publishes measurements / applies controls
over MQTT). `cmd/modbus` also withholds a decoded `W` that exceeds the configured
nameplate (`DeviceConfig.MaxW × 1.2`) before publishing — a corrupted SunSpec scale
factor decodes ~10× over, and the hub must not optimise against it (audit GS-1/MTR-1:
solar-bad-scale). This is a service-layer plausibility guard, NOT a register-map change,
so it does **not** require the southbound lockstep with the harness repo.

## Package map
```
device/    Device interface: ApplyControl, ReadMeasurements, Status, Close.
           Measurements and DeviceStatus types. Only package knowing both CSIP and hardware shapes.
modbus/    Transport wrapping simonvetter/modbus.
           URL selects layer: tcp://host:502 | rtu:///dev/ttyUSB0 | rtuovertcp://host:502
sunspec/   Scan (model discovery, reads IDs only — no data burst).
           Reader: ReadModel(id) / WriteModel(id, offset, values), 0-based offsets within named block.
           scale.go: ApplyScaleSigned/Unsigned, RawFromScaleSigned/Unsigned. 0x8000 → NaN.
           layout.go + derlayout.go: declarative point tables for the IEEE 1547-2018
           DER models 701-714 (exact spec order/type). der1547.go: typed parse/encode.
derbase/   Shared DER device logic + CSIP DERControlBase → SunSpec write mapping.
inverter/  Inverter implements Device. Reads Model 103 (or 101/102 fallback), nameplate from 121, controls via 123.
battery/   BatteryDevice implements Device. Model 103 AC + 802 Li-Ion battery state.
meter/     MeterDevice for bi-directional smart meter. Model 201 (single-phase AC).
registry/  Registry: fan-out ApplyControl, background poll, MeasurementUpdate channel.
sim/       Animated Modbus TCP servers. NewSolarServer / NewBatteryServer / NewMeterServer.
```

## SunSpec wire format
Header at address 40000 (0-based): `0x5375 0x6E53` ("SunS")
Block layout: `[ModelID uint16][Length uint16][Length × data uint16]`
End sentinel: ModelID = `0xFFFF`

## Key model offsets (0-based within block data section)
| Model | Field | Offset |
|-------|-------|--------|
| 103 (inverter AC) | W, W_SF | 12, 13 |
| 103 | Hz, Hz_SF | 14, 15 |
| 103 | PhVphA, V_SF | 8, 11 |
| 103 | St (operating state) | 36 |
| 121 (nameplate) | WMax, WMax_SF | 0, 22 |
| 123 (controls) | WMaxLimPct, _Ena, Conn, _SF | 0, 4, 16, 20 |
| 201 (meter) | W, W_SF | 16, 20 |
| 802 (Li-Ion) | SoC, SoH, DoD, ChaSt | 10, 11, 12, 16 |

## IEEE 1547-2018 DER models (701-714)
Defined declaratively in `sunspec/derlayout.go` as ordered point tables (`L701`,
`L704`, `L705Hdr`/`L705Crv`, …) — **do not** hand-number offsets; add/edit the
`Field` list and offsets/lengths follow. A `View` over a register slice gives
typed, scale-factor-aware, sentinel-safe access (`Float`, `Enum`, `SetFloat`, …).
Curve/trip/port repeating groups use the `CurveOffset*` / `SubCurveOffset*` /
`PortOffset714` helpers. PF points carry the power factor directly (engineering
value = raw × 10^SF); no ×100.

**CSIP → SunSpec mapping** lives in `derbase.ApplyControl`: limits
(Exp/Max/GenLimW) → 704 WMaxLimPct%; setpoints (FixedW) → 704 WSet; PF
inject/absorb → 704 PFWInj/PFWAbs sync groups; FixedVar → 704 VarSet; energize →
703 ES. Curve writers run the §3.1.2 adopt handshake (write staging curve →
AdptCrvReq=2 → poll AdptCrvRslt → Ena=1).

## Simulator API summary
All sims expose HTTP + WebSocket via `internal/simapi/`:
- Ports: modsim 5020/6020 · batsim 5021/6021 · metersim 5022/6022 · evsim —/6024
- `GET /state` → typed JSON · `POST /inject {"W_W":3000}` · `POST /control {"cmd":"pause","speed":5}` · `GET /registers` · `GET /ws` (2 s push)
- CORS wildcard enabled (legacy — the web dashboard proxies same-origin and does not need it).

## Tests
Inverter, battery, meter packages test against an in-process simonvetter Modbus server — no hardware required.
`go test ./internal/southbound/...`
