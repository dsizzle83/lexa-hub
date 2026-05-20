# Southbound Stack (Modbus / SunSpec)

Pure Go — zero cgo. Implements `Device` interface consumed by `internal/bridge/`.

## Package map
```
device/    Device interface: ApplyControl, ReadMeasurements, Status, Close.
           Measurements and DeviceStatus types. Only package knowing both CSIP and hardware shapes.
modbus/    Transport wrapping simonvetter/modbus.
           URL selects layer: tcp://host:502 | rtu:///dev/ttyUSB0 | rtuovertcp://host:502
sunspec/   Scan (model discovery, reads IDs only — no data burst).
           Reader: ReadModel(id) / WriteModel(id, offset, values), 0-based offsets within named block.
           scale.go: ApplyScaleSigned/Unsigned, RawFromScaleSigned/Unsigned. 0x8000 → NaN.
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
| 201 (meter) | W, W_SF | 12, 28 |
| 802 (Li-Ion) | SoC, SoH, DoD, ChaSt | 10, 11, 12, 16 |

## Simulator API summary
All sims expose HTTP + WebSocket via `internal/simapi/`:
- Ports: modsim 5020/6020 · batsim 5021/6021 · metersim 5022/6022 · evsim —/6024
- `GET /state` → typed JSON · `POST /inject {"W_W":3000}` · `POST /control {"cmd":"pause","speed":5}` · `GET /registers` · `GET /ws` (2 s push)
- CORS wildcard enabled (for Python GUI).

## Tests
Inverter, battery, meter packages test against an in-process simonvetter Modbus server — no hardware required.
`go test ./internal/southbound/...`
