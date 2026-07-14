# A7 Bench Verification Sweep (WP-16)

Companion: `CSIP_AUS_CHECKLIST.md`, `architecture.md` D3/§0 item 3, `work-packages.md` WP-3/WP-16,
`digests/csip-conformance-tests.md`, `../csip-tls-test/docs/BENCH.md`, `../csip-tls-test/CLAUDE.md`.
Every procedure below is written to run **when the bench frees** (per WP-16's scope line);
several depend on bench-repo (`csip-tls-test`) work that does not exist yet — each such
dependency is called out explicitly rather than glossed over, since silently assuming a fixture
or endpoint exists is exactly the kind of gap this sweep exists to catch.

## COMM-004 D–G reject-path pcap procedure

### What must be asserted (wolfSSL verify posture)

COMM-004 is 7 standalone sub-tests against a DUT that is the CSIP **client** (lexa-northbound)
connecting to a CSIP **server** presenting different certificate chains
(`digests/csip-conformance-tests.md:20`):

- **Accept** (context for D–G, not re-verified here): chain length 2 (SERCA→Device, 004A),
  length 3 (SERCA→MICA→Device, 004B), length 4 (SERCA→MCA→MICA→Device, 004C) — the client
  completes the mTLS handshake and its GET DeviceCapability returns 200.
- **Reject** (this section's scope, 004D–G): invalid MICA Extended-Key-Usage-critical (004D),
  invalid MICA Name non-critical (004E), invalid MICA Policy-Mapping non-critical (004F),
  self-signed device cert (004G) — the client **refuses the connection**. The conformance digest's
  generic pass bar is "TLS Alert; TCP disconnect or HTTP 403 acceptable"
  (`digests/csip-conformance-tests.md:20`), but for **this** codebase only the transport-level
  outcome is reachable: lexa-northbound's client never gets far enough to send an HTTP request
  over a rejected handshake, so "403" cannot occur here — the pass criterion for THIS client is
  **`WolfSSLFetcher`/`Client.Dial` returns a non-nil error and no HTTP bytes appear on the wire
  past the TLS handshake** (visible in the pcap as a TLS Alert record, or a bare TCP FIN/RST with
  no Finished message).

**Code posture found during this sweep** (grep, both `internal/wolfssl` and `internal/tlsclient`):
there is **no `wolfSSL_CTX_set_verify`/`set_verify_depth` call anywhere in the client path**.
`internal/tlsclient/client.go:59-79` (`New`) configures the client CTX via `SetCipherList` →
`LoadVerifyLocations` → `UseCertFile` → `UseKeyFile` only; `wolfssl.RequireClientCert`
(`internal/wolfssl/wolfssl.go:143-150`) is the only `set_verify` call in the package and it is
used **server-side only** (never called from `tlsclient`). Chain-depth acceptance up to 4 and
chain rejection on the D–G malformations both rely entirely on **wolfSSL's own compiled-in
default verify behavior** — this repo neither pins nor asserts either bound in code, and no unit
test exercises it (`internal/tlsclient`'s suite — `client_test.go`, `fetcher_test.go`,
`reload_integration_test.go`, `put_test.go`, `redirect_test.go` — runs against `sim/tlsserver`'s
single-CA depth-1 fixture and never varies chain depth or injects a malformed extension). **The
pcap capture below is the only artifact that will ever empirically confirm this posture** — until
it runs, "chains to depth 4 accepted, bad-MICA/self-signed rejected" is an assumption inherited
from wolfSSL's defaults, not a verified property of this codebase.

### Prerequisites not yet built (read before scheduling bench time)

1. **Test-PKI fixtures for 004B–G do not exist.** `csip-tls-test/scripts/gen-test-certs.sh`
   generates exactly one depth-1 chain (a single test CA signing one server cert and one client
   cert, `certs/ca-cert.pem`/`server-cert.pem`/`client-cert.pem`) plus one "wrong-CA" negative
   fixture (`certs/wrong-ca-cert.pem` + `wrong-ca-client-cert.pem`, signed by an unrelated CA —
   useful for a client-cert-rejection test, not for COMM-004's server-chain scenarios). There is
   **no intermediate/MICA cert, no depth-3/4 chain, and no deliberately-invalid X.509 extension
   cert** (MICA Extended-Key-Usage-critical / Name non-critical / Policy-Mapping non-critical) **and
   no self-signed device leaf** anywhere in `csip-tls-test/certs/`. All seven scenario fixtures
   must be generated as bench-repo work before any capture can run.
2. **gridsim's TLS server cannot present a multi-certificate chain today — a real code gap, not
   just a missing fixture.** gridsim (`csip-tls-test/sim/server/main.go`, the `bin/server`
   binary — flags `-ca`/`-cert`/`-key` at lines 23-25) delegates TLS setup to
   `sim/tlsserver.New` (`sim/server/main.go:46-51`), which calls
   `wolfssl.UseCertFile(ctx, cfg.ServerCertPath)` (`csip-tls-test/sim/tlsserver/server.go:67`).
   `UseCertFile` (`csip-tls-test/internal/wolfssl/wolfssl.go:102`, mirrored verbatim in this
   repo's own `internal/wolfssl/wolfssl.go:100-108`) wraps `wolfSSL_CTX_use_certificate_file` —
   the **single-leaf-certificate** loader, not `wolfSSL_CTX_use_certificate_chain_file` (the
   multi-cert-PEM loader). Neither `wolfssl.go` in either repo defines a chain-file wrapper
   (`grep -n chain` on both returns nothing). **Concretely: 004A (depth 2, root signs leaf
   directly) and 004G (self-signed leaf, no chain) are testable against gridsim TODAY once
   fixtures exist — both are single-certificate scenarios.** **004B/C/D/E/F all require an
   intermediate cert in the presented chain and CANNOT be exercised against gridsim as it stands
   today** — `csip-tls-test/internal/wolfssl` needs a `UseCertificateChainFile` wrapper (binding
   `wolfSSL_CTX_use_certificate_chain_file`) and `sim/tlsserver.New` needs to call it instead of
   `UseCertFile`, before any chain-bearing scenario can even be attempted on the wire. This is a
   **paired csip-tls-test work item**, the same class of gap as the gridsim PUT/LogEvent endpoint
   gap already flagged in `00_PROGRESS.md`.
3. **No existing lexa-hub or csip-tls-test test harness drives a chain-depth scenario.** The most
   representative driver is `cmd/northbound` itself (the actual production wolfSSL client code
   path) pointed at a throwaway gridsim instance via its own `server_addr`/`ca_cert` config —
   there is no lighter-weight harness purpose-built for this in either repo.

### Runnable checklist

Once items 1–2 above are closed (fixtures generated, gridsim's chain-file wrapper lands):

```bash
# --- 1. Generate the 7 scenario fixtures (illustrative shape — reconcile the exact
#        X.509 extension malformations for 004D/E/F against SunSpec's own Test PKI
#        hierarchy documentation before treating these as authoritative; this repo's
#        standards library does not include that hierarchy doc either) ---
cd ~/projects/csip-tls-test
WORK=$(mktemp -d); CERTS=certs/comm004; mkdir -p "$CERTS"

# SERCA root (pathlen 2: allows SERCA -> {MCA|MICA} -> MICA -> Device, i.e. depth 4)
openssl ecparam -name prime256v1 -genkey -noout -out $WORK/serca-key.pem
openssl req -x509 -new -key $WORK/serca-key.pem -days 3650 -out $CERTS/serca-cert.pem \
  -subj "/CN=COMM-004 Test SERCA" -sha256 \
  -addext "basicConstraints=critical,CA:TRUE,pathlen:2" \
  -addext "keyUsage=critical,keyCertSign,cRLSign"

# 004A — depth 2: SERCA signs the device leaf directly
openssl ecparam -name prime256v1 -genkey -noout -out $CERTS/004a-device-key.pem
openssl req -new -key $CERTS/004a-device-key.pem -out $WORK/004a.csr -subj "/CN=comm004-device-a"
openssl x509 -req -in $WORK/004a.csr -CA $CERTS/serca-cert.pem -CAkey $WORK/serca-key.pem \
  -CAcreateserial -days 365 -sha256 -out $CERTS/004a-device-cert.pem \
  -extfile <(echo -e "basicConstraints=critical,CA:FALSE\nextendedKeyUsage=serverAuth")

# MICA intermediate (signed by SERCA, pathlen 0) — reused by 004B and as the base for 004D/E/F
openssl ecparam -name prime256v1 -genkey -noout -out $WORK/mica-key.pem
openssl req -new -key $WORK/mica-key.pem -out $WORK/mica.csr -subj "/CN=COMM-004 Test MICA"
openssl x509 -req -in $WORK/mica.csr -CA $CERTS/serca-cert.pem -CAkey $WORK/serca-key.pem \
  -CAcreateserial -days 1825 -sha256 -out $WORK/mica-cert.pem \
  -extfile <(echo -e "basicConstraints=critical,CA:TRUE,pathlen:0\nkeyUsage=critical,keyCertSign,cRLSign")

# 004B — depth 3: SERCA -> MICA -> device (concatenate leaf + MICA into one chain PEM)
openssl ecparam -name prime256v1 -genkey -noout -out $CERTS/004b-device-key.pem
openssl req -new -key $CERTS/004b-device-key.pem -out $WORK/004b.csr -subj "/CN=comm004-device-b"
openssl x509 -req -in $WORK/004b.csr -CA $WORK/mica-cert.pem -CAkey $WORK/mica-key.pem \
  -CAcreateserial -days 365 -sha256 -out $WORK/004b-leaf.pem \
  -extfile <(echo -e "basicConstraints=critical,CA:FALSE\nextendedKeyUsage=serverAuth")
cat $WORK/004b-leaf.pem $WORK/mica-cert.pem > $CERTS/004b-device-chain.pem

# 004C — depth 4: SERCA -> MCA -> MICA -> device (MCA is a second, distinct intermediate)
openssl ecparam -name prime256v1 -genkey -noout -out $WORK/mca-key.pem
openssl req -new -key $WORK/mca-key.pem -out $WORK/mca.csr -subj "/CN=COMM-004 Test MCA"
openssl x509 -req -in $WORK/mca.csr -CA $CERTS/serca-cert.pem -CAkey $WORK/serca-key.pem \
  -CAcreateserial -days 1825 -sha256 -out $WORK/mca-cert.pem \
  -extfile <(echo -e "basicConstraints=critical,CA:TRUE,pathlen:1\nkeyUsage=critical,keyCertSign,cRLSign")
openssl ecparam -name prime256v1 -genkey -noout -out $WORK/mica2-key.pem
openssl req -new -key $WORK/mica2-key.pem -out $WORK/mica2.csr -subj "/CN=COMM-004 Test MICA (under MCA)"
openssl x509 -req -in $WORK/mica2.csr -CA $WORK/mca-cert.pem -CAkey $WORK/mca-key.pem \
  -CAcreateserial -days 1825 -sha256 -out $WORK/mica2-cert.pem \
  -extfile <(echo -e "basicConstraints=critical,CA:TRUE,pathlen:0\nkeyUsage=critical,keyCertSign,cRLSign")
openssl ecparam -name prime256v1 -genkey -noout -out $CERTS/004c-device-key.pem
openssl req -new -key $CERTS/004c-device-key.pem -out $WORK/004c.csr -subj "/CN=comm004-device-c"
openssl x509 -req -in $WORK/004c.csr -CA $WORK/mica2-cert.pem -CAkey $WORK/mica2-key.pem \
  -CAcreateserial -days 365 -sha256 -out $WORK/004c-leaf.pem \
  -extfile <(echo -e "basicConstraints=critical,CA:FALSE\nextendedKeyUsage=serverAuth")
cat $WORK/004c-leaf.pem $WORK/mica2-cert.pem $WORK/mca-cert.pem > $CERTS/004c-device-chain.pem

# 004D/E/F — reissue a MICA (same shape as the 004B intermediate) with one deliberately
# invalid extension each (VERIFY the exact malformation against SunSpec's Test PKI hierarchy
# before treating as authoritative — illustrative shapes only):
#   004D: Extended Key Usage marked critical on the CA cert (should not restrict CA usage)
#   004E: Name Constraints marked non-critical (RFC 5280 requires critical when present)
#   004F: Policy Mappings marked non-critical (RFC 5280 requires critical when present)
# then chain a device leaf under each exactly as 004B did, producing
# 004d-device-chain.pem / 004e-device-chain.pem / 004f-device-chain.pem.

# 004G — self-signed device leaf, no SERCA involvement at all
openssl ecparam -name prime256v1 -genkey -noout -out $CERTS/004g-device-key.pem
openssl req -x509 -new -key $CERTS/004g-device-key.pem -days 365 -sha256 \
  -out $CERTS/004g-device-cert.pem -subj "/CN=comm004-device-g-selfsigned"

# --- 2. For each of the 7 scenarios, point a throwaway gridsim instance at the
#        scenario's chain/key and the REAL client-trusted CA (serca-cert.pem) ---
bin/server -listen 0.0.0.0:11115 -admin 0.0.0.0:11116 \
  -ca certs/comm004/serca-cert.pem \
  -cert certs/comm004/004d-device-chain.pem -key certs/comm004/004d-device-key.pem &

# --- 3. Capture on the desktop (69.0.0.20, where gridsim's mTLS listener lives) — filter
#        by the throwaway port so the capture isolates this scenario's handshake. Confirm
#        the CURRENT hub host before running: this repo's own CLAUDE.md ("Current bench
#        deployment") and ../csip-tls-test/docs/BENCH.md have disagreed on hub-pi (69.0.0.1)
#        vs the ConnectCore dev kit (69.0.0.2) before (BENCH.md's header is dated 2026-07-07;
#        re-check it live) — `tcpdump -i any` sidesteps the ambiguity by not filtering on host.
sudo tcpdump -i any -w comm004-004D.pcapng port 11115

# --- 4. Drive the scenario with the REAL production client — point a spare/test
#        cmd/northbound config at the throwaway port and CA, then watch the handshake fail.
#        (northbound.json: "server_addr": "69.0.0.20:11115", "ca_cert": ".../serca-cert.pem",
#        client_cert/client_key unchanged — this scenario tests SERVER chain verification,
#        not the client's own cert.)
./bin/lexa-northbound -config /tmp/comm004-004D-northbound.json
# Expected: startup log shows a TLS/dial error, never a successful discovery walk.
# Stop tcpdump, inspect: Wireshark/tshark should show a TLS Alert record (or bare
# FIN/RST) immediately after the server's Certificate message — no Finished record,
# no HTTP GET on the wire.
tshark -r comm004-004D.pcapng -Y "tls.alert_message or tcp.flags.reset==1"

# --- 5. Repeat steps 2-4 for 004E/F/G (reject) and — for the accept-path context
#        record, not this section's primary scope — 004A/B/C, confirming a completed
#        Finished handshake + 200 on GET DeviceCapability instead.
```

Archive all 7 `.pcapng` files under the cert-campaign's evidence bundle (see "Cert-campaign
mechanics" below) — COMM-004 is the one test in the whole matrix that additionally requires a
packet-level artifact (`digests/csip-conformance-tests.md:137`).

## ERR-001 redirect bench check

**Config**: `redirect_max` (`cmd/northbound/config.go:92`, default 3 via
`defaultRedirectMax`, `cmd/northbound/config.go:150`; `RedirectMaxValue()`,
`cmd/northbound/config.go:156-161`, resolves the 0-disables convention). Code-side following is
implemented and unit-tested (`internal/tlsclient/redirect.go`, `redirect_test.go`) —
`00_PROGRESS.md`'s WP-3 row states this explicitly: "closes ERR-001 code half". This section is
the bench (pcap/observation) half.

**What to observe**: on GET `/dcap`, the Test Server returns 301/302 + `Location`; the client
must follow to the new URI over the SAME TLS session's transport rules (same host, same/upgraded
scheme, bounded by `redirect_max`) and complete the walk. Per `resolveRedirectLocation`
(`internal/tlsclient/redirect.go:52-97`), also confirm on the wire:
- an absolute `Location` naming a **different host** is refused (no second connection attempt
  appears in the pcap);
- an `http://` `Location` is refused (never a scheme-downgrade dial);
- a redirect count exceeding `redirect_max` produces an error, not an (N+1)th request.

**Prerequisite not yet built**: gridsim has no way to serve a 301/302 today. Its only
`Location`-header response is `POST /mup` → 201 + `Location` (resource creation, unrelated to
redirect-following — `csip-tls-test/sim/gridsim/server.go:325,362`). Its fault-injection surface
(`POST /admin/malform`, `csip-tls-test/sim/gridsim/malform.go:58-85`) supports a fixed kind set
(`malformKinds`, `malform.go:44-55`: pagination, negative/huge price, bad price multiplier, empty
curve list) with no redirect kind. **A new `csip-tls-test` bench work item** — a
`malform.go`-shaped redirect-injection mode (e.g. `POST /admin/malform {"kind":"redirect-dcap"}`
making the next `/dcap` GET return 301/302 + a configurable `Location`) — is needed before this
check can run; until then this bench check is queued, not executable.

**Once built**: `sudo tcpdump -i any -w err001.pcapng port 11111` on the desktop while
lexa-northbound performs a walk against gridsim armed with the redirect mode; inspect for the
initial 301/302 response followed by a second GET to the new path over the same TLS session, and
a subsequent successful walk (Time/EndDeviceList/etc. all still resolve).

## SunS 3-base probe

**Closed at lib level (WP-1).** `vendor/lexa-proto/sunspec/scanner.go:20` defines
`probeBases = []uint16{SunSpecBase, 0, 50000}` (40000 first, then 0, then 50000, per SunSpec
§6.2's spec-permitted alternates), and `ScanProbe` (`scanner.go:39-66`) iterates them in order,
returning the matched base. No further bench verification needed for the probe mechanism itself
— the base-selection logic is exercised by `lexa-proto`'s own scanner unit tests
(sunspecsweep fixtures per `work-packages.md` WP-1's test list); a live-hardware confirmation
that a real device actually responds at a non-40000 base is opportunistic (report it if a bench
device is ever provisioned at 0 or 50000) but not a gating requirement.

## Cert-campaign mechanics

From `digests/csip-conformance-tests.md:133-137` — the SunSpec certification process, not
LEXA-specific, restated here so a future cert campaign has the checklist in one place:

- **Certification body**: a SunSpec Authorized Test Laboratory runs the required tests for the
  claimed profile and submits a Test Results Report (TRR) to SunSpec; SunSpec grants the
  "SunSpec Certified" mark on a pass. Summary results publish publicly; detailed logs stay
  private (`csip-conformance-tests.md:133`).
- **Configuration freeze** (`csip-conformance-tests.md:134`): no software or hardware change
  during the campaign. A DER Client config may not change **at all** once testing starts; any
  change outside what a specific test specifies restarts the whole process. Practical
  implication for this repo: every flag this build-out ships flag-off
  (`port_16`, `pairing_mode`, `advanced_der`, `enforce_aus_limits`, `reconciler.adv`,
  `legacy_cannotcomply_code`, `registration_pin`, `der_report`) must be pinned to its FROZEN
  campaign value in the exact binary/config that goes to the lab — decide and record that value
  before the freeze starts, not during.
- **TRR contents** (CSV key/value, `csip-conformance-tests.md:135`): cert type/number, company
  identity, test lab + supervising engineer, **software name/version/checksum**, OS/version,
  operating environment (Cloud vs Hardware Device), hardware manufacturer/model, **PICS URL**,
  test date/description/comments, then one `Test <ID>` row per test valued PASS/FAIL/NOT
  SUPPORTED. Certification requires PASS on every test the claimed profile marks mandatory
  (`digests/csip-conformance-tests.md:125`'s DER Client matrix).
- **Detailed Test Logs** (`csip-conformance-tests.md:136`): JSON, every HTTP(S) message in
  unencrypted form, sub-second timestamps, for every test — this is a byproduct of running the
  lab's own harness against the DUT, not something this repo generates itself.
- **COMM-004's extra deliverable**: Libpcap/PcapNg trace of the TLS handshake for each of the 7
  certificate scenarios (`csip-conformance-tests.md:137`) — the procedure above is this repo's
  pre-lab dry run of exactly that artifact.
- **PICS URL**: a per-product capability declaration (Protocol Implementation Conformance
  Statement) hosted alongside the TRR; per-test NOT SUPPORTED entries are the escape hatch for
  optional features the DUT doesn't implement (`csip-conformance-tests.md:129`). This repo does
  not have a PICS document yet — drafting one (declaring, e.g., which DERControl modes are
  live per `standards-coverage-audit.md`'s coverage table) is separate pre-cert work, not
  covered by this sweep.
- **Dry-run before lab**: every procedure in this file (COMM-004 pcaps, ERR-001 redirect, the
  bench validation queue below) should be run to a clean PASS against gridsim/the bench
  **before** booking lab time — the configuration freeze means a fix discovered lab-side restarts
  the whole paid campaign (`csip-conformance-tests.md:134`; risk R1 in `risks.md` makes the same
  point for GFEMS aggregation specifically: "dry-run vs gridsim replica of the lab topology
  BEFORE booking lab time").

## Bench validation queue

Consolidated, ordered list — merges `00_PROGRESS.md`'s bench-deferred queue (lines 36-40) with
this file's own COMM-004/ERR-001 findings and the two named gridsim work items called out in the
WP-16 brief. Earlier items are prerequisites or cheaper/faster; later items need more bench setup
or a paired csip-tls-test change.

1. **PIN mismatch drill** — arm `registration_pin` against a Test Server whose `Registration.pIN`
   deliberately mismatches (or 404s); confirm D4's fail-closed posture (held control, suspended
   egress, `lexa_nb_pin_mismatch` gauge, `pin_ok` in `/status`) end-to-end
   (`internal/northbound/run/pin.go`).
2. **COMM-004 D–G pcaps** (this file, above) — blocked on: (a) the 7 scenario cert fixtures
   (none exist), (b) a `UseCertificateChainFile` wrapper in `csip-tls-test/internal/wolfssl` +
   `sim/tlsserver.New` switching to it (blocks 004B/C/D/E/F specifically; 004A/G are
   fixture-only). Paired csip-tls-test work item.
3. **ERR-001 redirect bench check** (this file, above) — blocked on: a redirect-injection
   `/admin/malform` kind in gridsim (does not exist). Paired csip-tls-test work item.
4. **dersite/PUT against gridsim** — gridsim needs PUT + LogEvent endpoints (`00_PROGRESS.md:37`:
   "gridsim needs PUT/LogEvent endpoints — csip-tls-test work"); blocks bench validation of WP-4's
   DER* reporting and WP-6's LogEvent poster against a real (simulated) CSIP-AUS-shaped server.
   Paired csip-tls-test work item.
5. **CannotComply code-flip pairing** — `00_PROGRESS.md:32-34`: WP-7's default-flip of
   CannotComply codes (0xF0 → IEEE 2030.5 Table 27, D5) needs gridsim's own expectation update in
   the SAME session, or bench configs must set `legacy_cannotcomply_code=true` in the meantime
   (`configs/northbound.json` currently ships `legacy_cannotcomply_code: true` — confirmed still
   the case in-repo — i.e. the pairing has NOT happened yet and the bench is running the legacy
   code path). Paired csip-tls-test work item.
6. **AUS shadow week** — run `constraint_shadow` against real or simulated CSIP-AUS envelope
   traffic (`opModGenLimW`/`opModLoadLimW`) for ≥1 week per `architecture.md` §6's gate, comparing
   `internal/orchestrator/auslimits.go`'s cascade against
   `internal/orchestrator/constraint/{genlimaus,loadlimaus}.go`'s shadow mirror; zero-diff
   required before any `enforce_aus_limits` default-flip proposal (see `CSIP_AUS_CHECKLIST.md`).
7. **adv-shell shadow soak** — `reconciler.adv` shadow→active bench soak (curve/PF/energize
   execution, WP-10) against real inverter hardware; R8 in `risks.md` flags this as unvalidatable
   in sim alone ("adopt handshake passes in sim, real inverter ignores/mangles curves").
8. **1.6 evsim** — bring up an OCPP 1.6J-speaking evsim instance and confirm `port_16`'s dual-stack
   bridge (WP-12) reaches the same `stationState`/reconcile path as the 2.0.1 stack, byte-identical
   2.0.1 behavior preserved.
9. **lexa-openadr deploy wiring** — `00_PROGRESS.md:38-40`: the WP-15 service half shipped without
   `deploy-hub-pi.sh` changes; the script still needs to provision
   `/etc/lexa/mqtt/openadr.pass`, patch `openadr.json` creds, install/enable the systemd unit, and
   provision a `client_secret_file`, before lexa-openadr can run on the actual bench hub.

Every item above is either explicitly bench-deferred in `00_PROGRESS.md` or a gap this sweep
found while trying to write a runnable procedure for WP-16 — none are asserted PASS anywhere in
this repo today.
