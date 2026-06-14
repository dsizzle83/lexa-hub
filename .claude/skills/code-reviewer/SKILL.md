---
name: code-reviewer
description: Review recently changed code against LEXA hub invariants — MQTT bus contracts, wolfSSL/CSIP rules, SunSpec register handling, service isolation. Use before commits and when asked to review.
---

# Code reviewer

Read the diff, then check against these rules. Report only real issues — no style nits on unchanged code.

## Blockers (must fix before merge)
- [ ] wolfSSL cipher changed from `ECDHE-ECDSA-AES128-CCM-8`.
- [ ] wolfSSL/cgo introduced outside lexa-northbound and lexa-telemetry (the other four services must stay `CGO_ENABLED=0` cross-compilable).
- [ ] `wolfssl.Init()` called more than once per process, or called anywhere but `main()`.
- [ ] `math.NaN()` (or any non-finite float) can reach a JSON bus message — use `*float64`, nil = absent.
- [ ] MQTT topic written as a string literal instead of an `internal/bus` constant, or QoS/retain deviates from the topic map in CLAUDE.md (`lexa/csip/control` MUST stay retained, QoS 1).
- [ ] `scheduler.Evaluate()` called with `time.Now().Unix()` instead of `+ ClockOffset`.
- [ ] XML root element missing `xmlns="urn:ieee:std:2030.5:ns"`.
- [ ] `internal/southbound/sunspec` register constants or `internal/ocppserver` protocol behaviour changed without the matching csip-tls-test change (lockstep — MTR-4).
- [ ] Watt/register value raw-cast to int16/uint16 instead of scaled into the SunSpec multiplier.
- [ ] Charging session modeled as bare `MeterValues` instead of `TransactionEvent` lifecycle.
- [ ] Map/slice shared across goroutines accessed without the lock (suggest `go test -race`).
- [ ] Credential compare that isn't `subtle.ConstantTimeCompare`.
- [ ] Private key written or logged anywhere.

## Warnings (should fix)
- [ ] SunSpec scale factor 0x8000 not mapped to NaN.
- [ ] Hand-numbered offsets for DER models 701–714 instead of editing the `derlayout.go` point tables.
- [ ] Error silenced with `_` on Modbus I/O, wolfSSL I/O, MQTT publish, or XML unmarshal.
- [ ] Service subscribes to a topic it doesn't own per the topic map (coupling creep between services).
- [ ] Goroutine started without a stop mechanism; `time.Sleep` in non-test code.
- [ ] New exported identifier without a doc comment.

## Output format
`BLOCKER|WARNING|SUGGESTION  file:line  description` — one per line.
If nothing found: "No issues found."
