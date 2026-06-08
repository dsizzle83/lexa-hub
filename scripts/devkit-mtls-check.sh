#!/bin/bash
# devkit-mtls-check.sh — air-gapped end-to-end CSIP check for the DUT.
#
# Runs the real lexa-northbound service (the device under test) against the
# csip-tls-test grid-sim server over mutual TLS, with the client and server
# certificates living in SEPARATE repos:
#
#   lexa-hub/certs/      ca.pem + client.pem + client-key.pem   (no server cert)
#   csip-tls-test/certs/ ca-cert.pem + server-cert.pem + key    (no client cert)
#
# Neither side holds the other's leaf certificate; each presents its own during
# the handshake and verifies the peer against the shared CA. The script proves
# the negotiated cipher is ECDHE-ECDSA-AES128-CCM-8 on the wire and shows the
# DUT walking the resource tree and POSTing CORE-022 responses.
#
# Usage:  scripts/devkit-mtls-check.sh [path-to-csip-tls-test]
set -uo pipefail

DUT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SRV_ROOT="${1:-$(cd "$DUT_ROOT/../csip-tls-test" 2>/dev/null && pwd)}"
[[ -d "$SRV_ROOT" ]] || { echo "csip-tls-test not found; pass its path as arg 1"; exit 1; }

# wolfSSL (cgo) for both the DUT and the sim server.
: "${WOLFSSL_PREFIX:=$HOME/.local/wolfssl-amd64}"
[[ -f "$WOLFSSL_PREFIX/include/wolfssl/options.h" ]] || {
  echo "wolfSSL not found at $WOLFSSL_PREFIX (set WOLFSSL_PREFIX). See csip-tls-test/sim_gridsim.txt STEP 1."; exit 1; }
export CGO_ENABLED=1
export CGO_CFLAGS="-I$WOLFSSL_PREFIX/include"
export CGO_LDFLAGS="-L$WOLFSSL_PREFIX/lib -lwolfssl -lm"

CLI_CERTS="$DUT_ROOT/certs"          # client side
SRV_CERTS="$SRV_ROOT/certs"          # server side
for f in "$CLI_CERTS/ca.pem" "$CLI_CERTS/client.pem" "$CLI_CERTS/client-key.pem"; do
  [[ -f "$f" ]] || { echo "missing client cert: $f"; exit 1; }
done
for f in "$SRV_CERTS/ca-cert.pem" "$SRV_CERTS/server-cert.pem" "$SRV_CERTS/vault/server-key.pem"; do
  [[ -f "$f" ]] || { echo "missing server cert: $f"; exit 1; }
done

# Air-gap assertions.
ls "$CLI_CERTS" | grep -qi 'server' && { echo "AIR-GAP FAIL: client repo holds a server cert"; exit 1; }
find "$SRV_CERTS" -name 'client-*.pem' | grep -q . && { echo "AIR-GAP FAIL: server repo holds a client cert"; exit 1; }
echo "✓ air gap: client repo has no server cert; server repo has no client cert; shared CA"

echo "Building sim server (csip-tls-test) and lexa-northbound (DUT)…"
( cd "$SRV_ROOT" && go build -o bin/server ./sim/server ) || exit 1
( cd "$DUT_ROOT" && go build -o bin/lexa-northbound ./cmd/northbound ) || exit 1

# Broker: reuse one already on :1883, else start a throwaway.
MOSQ=""
if ! (exec 3<>/dev/tcp/127.0.0.1/1883) 2>/dev/null; then
  printf 'listener 1883 127.0.0.1\nallow_anonymous true\n' > /tmp/devkit-mosq.conf
  mosquitto -c /tmp/devkit-mosq.conf >/tmp/devkit-mosq.log 2>&1 & MOSQ=$!
  sleep 1
fi

# Server with SERVER-side certs only.
"$SRV_ROOT/bin/server" -listen 127.0.0.1:11111 -admin 127.0.0.1:11112 -ocpp-port 8889 \
  -ca "$SRV_CERTS/ca-cert.pem" -cert "$SRV_CERTS/server-cert.pem" -key "$SRV_CERTS/vault/server-key.pem" \
  >/tmp/devkit-gridsim.log 2>&1 & SRV=$!
sleep 1.5

PCAP=""
if command -v dumpcap >/dev/null; then
  PCAP=$(mktemp --suffix=.pcapng)
  dumpcap -i lo -f 'tcp port 11111' -w "$PCAP" >/dev/null 2>&1 & CAP=$!
  sleep 1
fi

# DUT config pointing at the sim server with CLIENT-side certs only.
cat > /tmp/devkit-nb.json <<JSON
{ "mqtt_broker": "tcp://127.0.0.1:1883", "mqtt_client_id": "lexa-northbound-devkit",
  "server": "127.0.0.1:11111",
  "ca_cert": "$CLI_CERTS/ca.pem", "client_cert": "$CLI_CERTS/client.pem", "client_key": "$CLI_CERTS/client-key.pem",
  "lfdi": "", "discovery_interval_s": 5, "response_set_path": "/rsps/0/r" }
JSON

echo; echo "Running DUT for 8s…"
timeout 8 "$DUT_ROOT/bin/lexa-northbound" -config /tmp/devkit-nb.json >/tmp/devkit-nb.log 2>&1

[[ -n "$PCAP" ]] && { kill -INT ${CAP:-0} 2>/dev/null; wait ${CAP:-0} 2>/dev/null; }
kill $SRV 2>/dev/null
[[ -n "$MOSQ" ]] && kill "$MOSQ" 2>/dev/null

echo; echo "── DUT discovery + response posts ──"
grep -E "LFDI=|discovery OK|response posted" /tmp/devkit-nb.log
echo "── server-side handshake + cert-derived identity ──"
grep -E "handshake|client identity" /tmp/devkit-gridsim.log | head -2
if [[ -n "$PCAP" ]]; then
  CLASSIC=$(mktemp --suffix=.pcap); tcpdump -r "$PCAP" -w "$CLASSIC" 2>/dev/null
  if python3 - "$CLASSIC" <<'PY'
import sys; sys.exit(0 if open(sys.argv[1],'rb').read().count(b'\xc0\xae')>=2 else 1)
PY
  then echo "✓ ECDHE-ECDSA-AES128-CCM-8 (0xC0AE) negotiated on the wire"
  else echo "✗ cipher 0xC0AE not observed"; fi
  rm -f "$PCAP" "$CLASSIC"
fi
