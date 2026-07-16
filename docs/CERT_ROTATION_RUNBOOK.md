# Certificate rotation runbook (TASK-073, §10.5 / §8.6 / RSK-07)

This is the operator procedure for rotating lexa-northbound's client
certificate WITHOUT a control-enforcement gap and without restarting the
service. It is the write-up the review's §8.6/RSK-07 hard gate asks for:
*"Rotation procedure without control interruption, exercised on bench incl.
reconnect-churn soak."* The soak evidence is a separate, deferred artifact —
see [Reconnect-churn soak (deferred)](#reconnect-churn-soak-deferred) below
and csip-tls-test's `docs/CERT_ROTATION_SOAK_RUNBOOK.md`.

## Mechanism, in one paragraph

lexa-northbound holds three independent wolfSSL sessions (discovery,
response, flow-reservation — `cmd/northbound/main.go`'s `mustFetcher`
calls). Each is a `*tlsclient.WolfSSLFetcher`, which now has a `Reload(cfg,
probePath)` method (`internal/tlsclient/fetcher.go`): it builds a brand-new
wolfSSL context from the new cert/key, dials it, and probes it with a real
`GET /dcap` — proving the new cert is accepted end-to-end (TLS handshake
**and** the CSIP application layer) — all BEFORE touching the fetcher's live
session. Only on a successful, 200-status probe does it take the fetcher's
own mutex, swap the pointer, and free the OLD session (`Close` → `FreeSSL` →
`FreeCtx`, the invariant order). A failed probe tears the new (untested)
session down and leaves the old one completely untouched — a failed
rotation attempt costs nothing. `cmd/northbound/rotate.go`'s
`RotationController` is the trigger: it polls a sentinel file every ~5s
(`cfg.cert_rotate_poll_interval_s`) and, on finding a request, rotates the
three fetchers in sequence (never concurrently) and republishes certstatus
(TASK-072) once all three commit.

## Trigger: sentinel file, not SIGHUP

We chose a **watched sentinel file** over SIGHUP:

- Deploy-friendly — no signal delivery path to get right over SSH/systemd,
  no risk of a signal arriving before the process has finished startup.
- Carries data (the staged cert/key paths) without a side-channel — SIGHUP
  alone can't tell the process WHERE the new material lives.
- Self-documenting on disk: the outcome suffix (`.done-<ts>` /
  `.failed-<ts>` / `.rejected-<ts>`) the controller renames the sentinel to
  is itself the audit trail of every rotation attempt.

Default path: `/etc/lexa/certs/rotate.request` (`cfg.cert_rotate_sentinel`
in `northbound.json`). Schema:

```json
{
  "client_cert": "/etc/lexa/certs/rotate-staging/client.pem",
  "client_key": "/etc/lexa/certs/rotate-staging/client-key.pem",
  "requested_at": "2026-07-06T12:00:00Z"
}
```

**`client_cert`/`client_key` point at the STAGED new material, never the
live `/etc/lexa/certs/{client,client-key}.pem` paths.** This is the
load-bearing design choice: only after `RotationController` proves the
staged cert works (probe succeeds on all three fetchers) does
`scripts/rotate-cert.sh` promote it onto the live path. If the live path
were written first and the rotation then failed, a future process restart
would come up on the SAME cert the running process just decided doesn't
work — this ordering means a failed rotation never risks that.

## Operator procedure (`scripts/rotate-cert.sh`)

```
bash scripts/rotate-cert.sh <pi-ip> <new-cert.pem> <new-key.pem> [ssh-user]
```

Steps the script performs, matching the mechanism above:

1. Derives the LFDI of the LIVE cert on the target and of the candidate new
   cert (locally), and **refuses to proceed at all** if they don't match —
   see [Re-enrollment vs rotation](#re-enrollment-vs-rotation) below.
2. Stages the new cert (0644) and key (0600), owner `lexa:lexa`, under
   `/etc/lexa/certs/rotate-staging/` — the live path is untouched.
3. Records the current `client_not_after` from the retained
   `lexa/northbound/certstatus` MQTT topic (TASK-072), as the "before" value.
4. Writes the sentinel pointing at the staged files.
5. Polls (every 5s, up to 120s) for the sentinel to be renamed
   `.done-<ts>` (committed), `.failed-<ts>` (a fetcher's probe failed —
   nothing was promoted), or `.rejected-<ts>` (the Go-side LFDI/format check
   refused it independently of the script's own check).
6. On `.done`: confirms `certstatus`'s `client_not_after` actually changed,
   and greps the journal for `cert rotation committed on all three
   fetchers`.
7. Only THEN promotes the staged cert/key onto the live path
   (`/etc/lexa/certs/{client,client-key}.pem`), after archiving the
   previous pair under `/etc/lexa/certs/archive/client[-key]-<ts>.pem`.
8. On any failure/timeout/refusal: cleans up the staging directory and any
   leftover sentinel/outcome files, and exits non-zero. **The live path and
   the running process's active sessions are never touched on this path.**

### Abort path

If the script fails at any point before step 7, nothing has changed on the
live path or in the running process beyond whichever of the three fetchers
already reloaded successfully (a genuinely partial rotation — see below).
Re-running the script is safe: it re-derives LFDI, re-stages, and writes a
fresh sentinel.

### A note on partial rotations

`RotationController` rotates the three fetchers **one at a time** and
continues through all three even if one fails — so a `.failed-<ts>` outcome
can mean "0 of 3 committed" or "2 of 3 committed, 1 still on the previous
cert." Both are safe (each fetcher's session is independent and still uses
a cert that hasn't expired), but they are NOT equivalent to a clean
rotation. Check the journal (`fetcher=` field on each `cert rotation:
fetcher committed` / `fetcher reload failed` line) to see exactly which
fetcher(s) are on which cert before re-running. `scripts/rotate-cert.sh`
does not promote the staged cert to the live path on ANY outcome other than
a full `.done` — a partial rotation is not silently treated as good enough.

## Control-enforcement continuity during the reconnect window

**Claim:** rotating a fetcher's session never interrupts DER control
enforcement, because the discovery fetcher's Reload only ever runs BETWEEN
walks (its own mutex serializes it against any in-flight `Get`/`Post`), and
the scheduler's fail-closed hold (`internal/northbound/scheduler/scheduler.go`
`failClosed`, TASK-035/037/042) already covers every gap between two
successful walks — a WAN outage, a wedged server, or a rotation's brief
reconnect window look identical to it: the retained `ActiveControl` stays
published, and lexa-hub keeps enforcing it locally until either the next
successful walk resolves something new, or the control's own `ValidUntil`
expires — at which point the hub degrades to the last-known DefaultDERControl
if the expiring event carried one (still enforcing, never dropping to
unconstrained), and only releases control entirely if no fallback was present
(`cmd/hub/state.go`'s DefaultFallback branch).

**This must be VERIFIED, not assumed, on the bench** (acceptance criteria):
run a long export-cap DERControl through the dashboard/gridsim, rotate the
cert mid-cap, and confirm the meter shows continuous compliance with no
excursion during the reconnect window. A rotation that completes in one
`Reload` cycle (sub-second probe + swap, per fetcher) is many orders of
magnitude shorter than any realistic control's `ValidUntil` horizon — but
confirm this holds for the SHORTEST `ValidUntil` your fleet actually issues,
not just the demo scenario's.

## Re-enrollment vs rotation

The LFDI (`internal/northbound/identity.FromCertificate`) is the leftmost
160 bits of SHA-256 **over the full DER-encoded certificate** — not just the
public key or the Subject CN. This has a sharp consequence worth stating
plainly: **any re-issued certificate — even for the exact same device, same
CN, same private key — has a different LFDI than the one it replaces**,
because a real X.509 re-issuance always changes at least the serial number
and validity window, which changes the DER, which changes the hash.

What this script's (and `RotationController`'s) LFDI check actually
guarantees is narrower than "cryptographic proof of continuous device
identity": it guarantees the operator is rotating onto a cert they already
told the check to expect (by comparing against the LIVE cert at rotation
time, not some earlier baseline). It reliably catches the mistake the task
is most worried about — staging the WRONG device's cert, a typo'd CN, a
cert from the wrong CA/environment — because those look nothing like the
live cert's hash. It does **not**, by itself, distinguish "a legitimate
reissue for this device" from "a plausible-looking cert for a different
device" in the abstract; that distinction currently rests on the operator
having generated the new cert correctly (`make gen-client-cert
CN=<current-CN>` in csip-tls-test) and the CA controlling who gets a cert
issued at all.

Two consequences to plan around:

1. **`csip-tls-test/scripts/gen-client-cert.sh` mints a fresh ECDSA
   keypair on every invocation.** There is currently no "reissue a cert
   around the SAME existing key" path. Until one exists, every reissue via
   that script changes the LFDI this codebase computes, by definition —
   meaning the bench single-rotation drill (task step 5) demonstrates the
   MECHANISM (fetcher swap, zero enforcement gap, certstatus update)
   against a cert that the LFDI check is told, out of band, to treat as
   "the same device" — not a claim that the byte-level identity is
   unchanged. The runbook's LFDI check is exactly this: an operator-facing
   sanity gate against gross mistakes, run at rotation time against the
   CURRENT live cert, not a cryptographic non-repudiation proof across
   reissues.
2. **A genuine CN change, or a cert signed by a different CA, is always
   re-enrollment** — gridsim's `EndDevice` registration is keyed by LFDI,
   so the very next walk after such a swap 403s (or worse, is treated as an
   unrecognized device) until the utility re-registers the new identity.
   That is the commissioning/re-enrollment flow (backlog item, not this
   task) — never paper over an LFDI-mismatch refusal by disabling the
   check; investigate why the staged cert doesn't match first.

## Reconnect-churn soak (deferred)

TASK-073's other acceptance criterion — **24h churn soak, N rotations/hour,
watching for segfault/fd-leak/watchdog fire** — is BENCH TIME, not something
a single implementation session can execute (it needs the bench dedicated
for a day, outside any QA campaign window). Per this program's soak-gating
convention (see csip-tls-test `docs/refactor/00_MASTER_INDEX.md`'s P5
residual-soak entries), this is **code-complete, soak-deferred**:

- The mechanism (`Reload`, `RotationController`) is implemented, unit- and
  integration-tested (see below), and exercised once on the bench via the
  single-rotation drill (acceptance criterion 1).
- The soak driver script and its exact procedure/pass-fail criteria are
  written up precisely and ready to run:
  `csip-tls-test/scripts/cert-churn-soak.sh` +
  `csip-tls-test/docs/CERT_ROTATION_SOAK_RUNBOOK.md`.
- RSK-07 in `docs/refactor/08_RISK_REGISTER.md` is marked
  "mitigation implemented, soak pending" rather than "Open" or "Mitigated" —
  see that file's diff.

Run the soak in its own dedicated bench session (not layered onto a QA
campaign), then attach its evidence (CSV + summary) here and flip RSK-07 to
fully mitigated.

## Testing this task added

- `internal/tlsclient/reload_test.go` — unit tests for `planReload`'s
  probe-then-commit ordering against a fake `reloadable` (dial failure,
  probe transport failure, probe non-200/malformed response, and the
  success path's "never Free the session that just succeeded" assertion).
  No cgo/wolfSSL needed; runs in the plain `go test ./...` suite.
- `internal/tlsclient/reload_integration_test.go` — `//go:build integration`
  tests against a REAL, self-contained wolfSSL client+server pair (built
  directly on `internal/wolfssl`, with cert material generated at test
  time): 5 back-to-back reload rounds with no crash/hang (a miniature churn
  smoke test), plus the "same CA, wrong device — TLS handshake succeeds,
  CSIP layer 403s" probe-rejection case, proving the old session stays
  fully functional. Run via `go test -race -tags=integration
  ./internal/tlsclient/...` on the desktop amd64 wolfSSL sysroot
  (`~/.local/wolfssl-amd64`).
  - **Note for reviewers:** this package's PRE-EXISTING integration tests
    (`client_test.go`, `fetcher_test.go`) reference helper functions
    (`startInProcessServer`, `goodClientConfig`) that are not defined
    anywhere in the tree, and their `testdata/certs/` has no private keys
    (`*-key.pem` is gitignored repo-wide and none were ever committed) — so
    those two files cannot currently build or run in a fresh checkout,
    independent of this task. `reload_integration_test.go` does not depend
    on either gap; it is fully self-contained. Fixing that pre-existing gap
    is out of TASK-073's scope but worth its own follow-up.
- `cmd/northbound/rotate_test.go` — unit tests for `RotationController`'s
  sentinel-handling state machine against a fake `reloader`: no-sentinel
  no-op, happy path (all three rotate, `onCommit` fires, sentinel consumed
  exactly once), LFDI-mismatch refusal (acceptance criterion), malformed
  JSON, missing fields, unreadable staged cert, and a partial-failure case
  (one fetcher's `Reload` fails — the other two still commit, `onCommit`
  does NOT fire, sentinel is marked `.failed`, not `.done`).

Run: `go test -race ./internal/... ./cmd/...` (lexa-hub); `go test -race
-tags=integration ./internal/tlsclient/...` (desktop, cgo).

## Fetcher rotation seam (for future maintainers)

`internal/tlsclient.WolfSSLFetcher.Reload(cfg, probePath) error` is the
seam: build-dial-probe a new session, and only on success swap+free the
old one under the fetcher's existing mutex (the same one `Get`/`Post`/
`GetStatus` already take — no NEW locking scheme was introduced). Any
future northbound TLS-session owner (a fourth fetcher, a different
resource kind) gets rotation for free by calling `Reload` the same way;
nothing about the swap depends on which of the three current call sites
invokes it.
