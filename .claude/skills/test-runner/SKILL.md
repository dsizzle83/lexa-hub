---
name: test-runner
description: Run the right tests for the most recently changed code and report results concisely. Use after any code change in this repo, before claiming work is done.
---

# Test runner

## Procedure

1. **Default**: `make test` (= `go test -race ./internal/...`). Race detection is on for a
   reason — concurrency bugs in the audit were only catchable this way.
   - The Makefile auto-wires the amd64 wolfSSL sysroot (`~/.local/wolfssl-amd64`) into
     CGO flags; if that sysroot is missing and cgo packages fail to build, fall back to
     `go test -race $(go list ./internal/... | grep -vE 'wolfssl|tlsclient')` and say so.

2. **Build check** for `cmd/` changes: `make build` (all six binaries).
   For deploy-bound changes: `make build-arm64` (needs the arm64 sysroot —
   `make wolfssl-arm64` first if /tmp was wiped).

3. **Integration / conformance** tests live in the harness repo
   (`~/projects/csip-tls-test`: `make test-fast`, `go test ./tests/`,
   `scripts/run-conformance.sh`). Run them there when changing `internal/northbound`
   or the shared `lexa-proto` packages (`sunspec`, `ocppserver` — lockstep via
   `proto.pin`, enforced by `check-proto-pin.sh`).

4. **Report format**
   - All pass: "All N tests pass." One line. Done.
   - Per failure: test name, file:line, exact error. Nothing else.
   - On failure: read the failing test + exercised code, identify root cause, propose the
     minimal fix. No drive-by refactors.

## Do not
- Re-run a test that just passed.
- Deploy or restart bench services as part of "testing" — that's the hub-deploy skill,
  and only on request.
- Change the shared `lexa-proto` packages (`sunspec`, `ocppserver`) without planning the
  matching csip-tls-test change (both repos must repin `proto.pin` in lockstep).
