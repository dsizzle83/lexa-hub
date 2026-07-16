# Southbound Stack (Modbus / SunSpec)

Pure Go — zero cgo. Implements the `Device` interface, polled by the `registry` and
driven by the `cmd/modbus` service (which publishes measurements / applies controls
over MQTT). `cmd/modbus` also withholds a decoded `W` that exceeds the configured
nameplate (`DeviceConfig.MaxW × 1.2`) before publishing — a corrupted SunSpec scale
factor decodes ~10× over, and the hub must not optimise against it (audit GS-1/MTR-1:
solar-bad-scale). This is a service-layer plausibility guard, NOT a register-map change,
so it does **not** require the southbound lockstep with the harness repo.

**GS-1/MTR-1 register-wrap invariant — regression-swept by TASK-053**: the
int16 watt-field wrap class is now a standing generative property test, not
just an audit finding. The shared codec's own contract (round-trip,
wrap/clamp, sentinel) is swept in `lexa-proto/sunspec/scale_sweep_test.go`;
this repo runs the identical contract against its own vendored copy via
`internal/southbound/sunspecsweep/`, and the two watt→ActivePower encoders
(`cmd/hub/state.go:wattsToActivePower`, `cmd/modbus/main.go:activePowerFromWatts`)
are swept 0..1e9 W and cross-checked against each other in
`cmd/hub/state_test.go` / `cmd/modbus/control_test.go`.

## Package map
```
device/    Device interface: ApplyControl, ReadMeasurements, Status, Close.
           Measurements and DeviceStatus types. Only package knowing both CSIP and hardware shapes.
inverter/  Inverter implements Device. Reads Model 103 (or 101/102 fallback), nameplate from 121, controls via 123.
battery/   BatteryDevice implements Device. Model 103 AC + 802 Li-Ion battery state.
meter/     MeterDevice for bi-directional smart meter. Model 201 (single-phase AC).
registry/  Registry: fan-out ApplyControl, background poll, MeasurementUpdate channel.
sunspecsweep/  Runs the shared codec's scale/wrap/sentinel contract test against this
           repo's vendored lexa-proto/sunspec copy (GS-1/MTR-1 sweep, TASK-053 — see above).
```

The Modbus transport (`lexa-proto/modbus`), the SunSpec codec — Scan/Reader,
`scale.go`, `layout.go`/`derlayout.go`, `der1547.go` (`lexa-proto/sunspec`) — and the
shared DER device logic + CSIP DERControlBase → SunSpec write mapping
(`lexa-proto/derbase`) moved out of this package into the shared **lexa-proto** module
(TASK-023); they're imported here and vendored under `vendor/lexa-proto/`. The
wire-format / offset reference below still documents that shared codec accurately — only
the source now lives in lexa-proto.

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
Defined declaratively in `lexa-proto/sunspec/derlayout.go` as ordered point tables (`L701`,
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

## Simulators
The device simulators (animated Modbus TCP servers) and their HTTP + WebSocket
`internal/simapi/` live in the bench repo `~/projects/csip-tls-test`, not here — this
repo is the product (see the top-level CLAUDE.md).

## Tests
Inverter, battery, meter packages test against an in-process simonvetter Modbus server — no hardware required.
`go test ./internal/southbound/...`
