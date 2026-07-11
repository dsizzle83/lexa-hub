# ADR-0002: BLE commissioning protocol — "LEXA Provision v1"

Status: **proposed** (contract shared with `lexa-hub`; hub-side implementation
is net-new — see H.* units in ROADMAP.md) · Date: 2026-07-10

## Context

A factory-fresh hub has no network identity a phone can reach: today it gets
online via Ethernet DHCP or a hand-built `nmcli` profile (`lexa-hub/DEVKIT.md`).
There is **no existing BLE, SoftAP, or WiFi-credential mechanism anywhere in
the hub codebase** (verified 2026-07-10; the only mention is DEVKIT.md calling
the Yocto image "BT provisioning-ready", i.e. kernel support only).

We want: phone commissions the hub over BLE — authenticates using the setup
code printed on the device label, delivers WiFi credentials, and receives the
connection material (IP, API cert fingerprint, bearer token) needed to talk to
the hub's existing HTTPS API (`HUB_API.md`). This turns the API's
trust-on-first-use fingerprint pinning into **trust-on-commissioning**: the
fingerprint arrives over an authenticated, encrypted BLE session instead of
being blindly accepted on first HTTPS contact.

Roles: **hub = GATT peripheral** (new `lexa-provision` service, BlueZ D-Bus,
in the lexa-hub repo), **app = GATT central** (this repo).

## Label QR format

Printed on the unit (alongside the existing serial / API-secret label):

```
LEXA:v1;serial=<serial>;pop=<8+ char setup code>
```

`pop` (proof of possession) is a per-unit random code, manufacturing-
provisioned to `/etc/lexa/provision-pop` on the device. Possession of the QR
(physical access) is the root of trust.

## Advertising

- Advertise **only** when uncommissioned (no `/etc/lexa/commissioned` marker)
  **or** during an explicit re-provision window (physical button hold ≥5 s, or
  `lexactl provision --window 10m`). Otherwise the radio stays silent.
- Local name `LEXA-<last 6 of serial>`; advertisement includes the service UUID.
- After 3 failed PoP handshakes: stop advertising for 5 minutes (throttle
  brute force; PoP entropy is the real defence).

## GATT layout

Base UUID note: `4c455841` = ASCII "LEXA".

| UUID | Name | Ops | Payload |
|---|---|---|---|
| `4c455841-0001-4870-b2d7-c36a53c1c9a1` | service | — | — |
| `4c455841-0002-4870-b2d7-c36a53c1c9a1` | `info` | read | plaintext JSON `{"v":1,"serial":"...","fw":"...","commissioned":false,"sec":["sec1"]}` |
| `4c455841-0003-4870-b2d7-c36a53c1c9a1` | `session` | write / indicate | handshake frames (below) |
| `4c455841-0004-4870-b2d7-c36a53c1c9a1` | `wifi` | write / indicate | encrypted: scan command + results |
| `4c455841-0005-4870-b2d7-c36a53c1c9a1` | `config` | write | encrypted: join credentials |
| `4c455841-0006-4870-b2d7-c36a53c1c9a1` | `status` | indicate | encrypted: state machine + handoff |

## Framing (all characteristics except `info`)

App requests MTU ≥ 247 (falls back gracefully). Messages are JSON documents
chunked into GATT writes/indications:

```
byte 0   flags: bit0 = FIN (last chunk), bit1 = ENC (ciphertext)
byte 1   seq  (0..255, per message)
byte 2+  payload chunk
```

Reassembled message limit: 8 KiB. Encrypted messages are
`AES-128-GCM(nonce, plaintext)` with the 16-byte tag appended; the whole
ciphertext is chunked as above with ENC set.

Nonce (12 bytes): `direction (1B: 0x01 app→hub, 0x02 hub→app) ‖ 0x000000 ‖
counter (8B big-endian)`, counter per direction starting at 0, incremented per
message. **Nonces are implicit — never transmitted.** The receiver decrypts
strictly under its next expected counter for that direction, so any replayed,
reordered, or tampered message fails GCM authentication; that failure
permanently aborts the session. (Conformance vectors:
`packages/lexa_core/test/ble/sec1_test_vectors.json` — the Go implementation
must reproduce them exactly.)

## Handshake (`sec1`) on `session`

X25519 ECDH, PoP-bound, ephemeral keys per session:

1. app → hub `{"op":"hello","pub":"<b64 X25519 pubkey>"}`
2. hub → app `{"op":"hello","pub":"<b64>","challenge":"<b64 8B random>"}`
3. Both derive `K = HKDF-SHA256(ikm = X25519(shared), salt = UTF8(pop), info = "lexa-prov-v1", len = 16)`
4. app → hub (encrypted) `{"op":"confirm","challenge":"<b64 same 8B>"}`
   — proves the app derived K, i.e. knows the PoP.
5. hub → app (encrypted) `{"op":"ok"}` — or plaintext `{"op":"err","code":"pop_mismatch"}`.

An attacker without the PoP can complete ECDH but cannot produce a valid
`confirm`, and MITM of the ECDH fails the same way. `sec0` (plaintext, bench
builds only) may be listed in `info.sec` on dev images; production images
compile it out.

Plaintext frames are acceptable **only** during the handshake on `session`
(the hub `hello` and `err` replies). After `ok`, both sides MUST reject any
plaintext frame on any characteristic as a protocol error — downgrade
attempts abort the session.

## WiFi scan (`wifi`, optional step)

- app → hub `{"op":"scan"}`
- hub → app `{"op":"scan_result","aps":[{"ssid":"...","rssi":-52,"sec":"wpa2"},...]}`
  (hub queries NetworkManager; deduped, sorted by RSSI, top 20)

## Join (`config` → `status`)

- app → hub `{"op":"join","ssid":"...","psk":"..."}` (`psk` omitted for open networks)
- hub creates/activates a NetworkManager profile and streams state on `status`:

```
{"op":"state","state":"joining"}
{"op":"state","state":"joined","ip":"192.168.1.42","port":9100,
 "api_cert_fp":"<64 hex sha256>","token":"<bearer token>","serial":"<serial>"}
-- or --
{"op":"state","state":"failed","reason":"auth_failed"}
```

`reason ∈ {not_found, auth_failed, dhcp_timeout, timeout, internal}`.
On failure the app may retry `join` within the same session.

The handoff payload is the point of the whole protocol: `api_cert_fp` is read
the same way `lexa-api` computes it (SHA-256 of the leaf DER, lowercase hex,
`lexa-hub/cmd/api/tlscert.go:263`) and `token` is the current content of
`/etc/lexa/api-secret` — delivered over the PoP-authenticated channel so the
homeowner never types it.

- app → hub `{"op":"done"}` — written **encrypted on the `config`
  characteristic** — hub stops advertising and closes the session.
  Note: `done` does **not** write the commissioned marker; that happens at the
  end of the full commissioning wizard via the existing
  `POST /config/{service}` path (HUB_API.md).

## App-side completion

The app stores a **hub profile** `{serial, host, port, api_cert_fp, token}` in
secure storage, connects to `https://<ip>:9100/status` with the pinned
fingerprint, and verifies the reported serial matches the QR. Later IP changes
are re-resolved via mDNS (`_lexa-hub._tcp`, TXT `serial=`) — units 1.16.

## Hub-side counterpart (tracked in lexa-hub, H.* units here)

New `cmd/provision` service: BlueZ GATT server over D-Bus, NetworkManager
`AddAndActivateConnection` over D-Bus, reads `/etc/lexa/identity` +
`provision-pop` + `api-secret` + the API cert file, gated on the commissioned
marker, systemd unit with `NoNewPrivileges` where possible. Go crypto is
stdlib/`x/crypto` (curve25519, hkdf); Dart side uses `package:cryptography`.

## Alternatives considered

- **Improv WiFi (open BLE standard)**: no session encryption; credentials go
  over the air in plaintext relying on proximity — unacceptable for a
  grid-connected DER controller, and it has no channel to hand back
  fingerprint + token.
- **SoftAP commissioning**: phone joins hub's temporary AP. Clunky UX on
  modern Android/iOS (auto-switchback, captive portal heuristics), and the
  i.MX93 WiFi radio can't reliably do AP+STA concurrently during join.
- **Wired/mDNS-only TOFU (status quo of hub roadmap)**: requires the hub to
  already be online, which is exactly the bootstrap problem; blind TOFU also
  weakens the pinning story.
