#!/bin/bash
# Stages a rotated client cert/key for lexa-northbound (TASK-073 /
# §10.5 / §8.6 / RSK-07) WITHOUT restarting the service and without any
# gap in control enforcement: the new material is written to a STAGING
# path (never overwriting the live path until the rotation is PROVEN
# good), a sentinel file is written that lexa-northbound's
# RotationController (cmd/northbound/rotate.go) polls for every few
# seconds, the fetcher-swap outcome is confirmed via certstatus + the
# journal, and only THEN is the staged material promoted onto the live
# path — so a future process restart also picks up the rotated cert,
# but a FAILED rotation never leaves the live path holding untested
# material.
#
# See docs/CERT_ROTATION_RUNBOOK.md for the full procedure, the
# reconnect-churn soak this mechanism is deferred-validated against, and
# the "re-enrollment vs rotation" distinction this script enforces below.
#
# LFDI SAFETY (task step 4 / RSK-07 "common mistakes"): this script
# refuses to write the sentinel at all if the new cert's derived LFDI
# does not match the LIVE cert's. A CN/key change for what is supposed
# to be the SAME device is RE-ENROLLMENT (gridsim's EndDevice
# registration would 403 the walk), not rotation — see the runbook.
# RotationController independently re-checks this on the Go side too
# (defense in depth), so a sentinel written by anything OTHER than this
# script is still refused if it fails the same check.
#
# Usage:
#   bash scripts/rotate-cert.sh <pi-ip> <new-cert.pem> <new-key.pem> [ssh-user]
# Example:
#   bash scripts/rotate-cert.sh 69.0.0.1 \
#     ../csip-tls-test/certs/client-staging/client-cert.pem \
#     ../csip-tls-test/certs/client-staging/client-key.pem dmitri
#
# Prerequisites:
#   - <new-cert.pem>/<new-key.pem> were issued FOR THE SAME DEVICE this
#     hub is currently enrolled as — csip-tls-test's
#     `make gen-client-cert CN=<current-CN>` (same CN as the live cert).
#     This script compares LFDI, not CN, so it catches an identity
#     change regardless of how it happened (see the runbook's caveat on
#     gen-client-cert.sh minting a fresh key on every call).
#   - lexa-northbound on the target Pi is running a build that includes
#     RotationController (this task) — an older binary silently ignores
#     the sentinel forever; this script's poll will time out and say so.
#   - passwordless sudo on the Pi (per csip-tls-test's bench convention).
#
# Abort path: if this script fails or times out at ANY step, the live
# cert AND lexa-northbound's in-memory session are left EXACTLY as they
# were — nothing is promoted to the live path until a commit is
# confirmed via certstatus. The staging directory and any leftover
# sentinel/outcome files are cleaned up before exit. Safe to re-run.
set -euo pipefail

PI="${1:?usage: rotate-cert.sh <pi-ip> <new-cert.pem> <new-key.pem> [ssh-user]}"
NEWCERT="${2:?missing new-cert.pem path}"
NEWKEY="${3:?missing new-key.pem path}"
SSHUSER="${4:-dmitri}"

[[ -f "$NEWCERT" ]] || { echo "error: $NEWCERT not found" >&2; exit 1; }
[[ -f "$NEWKEY" ]] || { echo "error: $NEWKEY not found" >&2; exit 1; }

REMOTE_CERTS=/etc/lexa/certs
REMOTE_STAGE=/etc/lexa/certs/rotate-staging
SENTINEL=/etc/lexa/certs/rotate.request
POLL_INTERVAL=5
POLL_TIMEOUT=120 # seconds — a healthy rotation commits in well under this

lfdi_of_der() {
  # Leftmost 160 bits of SHA-256 over the DER-encoded cert, uppercase hex —
  # must match internal/northbound/identity.FromCertificate exactly.
  openssl dgst -sha256 -binary | head -c 20 | xxd -p -c 20 | tr '[:lower:]' '[:upper:]'
}

echo "== TASK-073 cert rotation: $PI =="

echo "-- Deriving LFDI of the LIVE cert on $PI"
LIVE_LFDI="$(ssh "$SSHUSER@$PI" "openssl x509 -in $REMOTE_CERTS/client.pem -outform DER" | lfdi_of_der)"
echo "   live LFDI:   $LIVE_LFDI"

echo "-- Deriving LFDI of the staged (new) cert: $NEWCERT"
NEW_LFDI="$(openssl x509 -in "$NEWCERT" -outform DER | lfdi_of_der)"
echo "   staged LFDI: $NEW_LFDI"

if [[ "$LIVE_LFDI" != "$NEW_LFDI" ]]; then
  cat >&2 <<EOF
REFUSING: staged cert's LFDI does not match the live device identity.
  live:   $LIVE_LFDI
  staged: $NEW_LFDI
This is RE-ENROLLMENT (a different device identity), not rotation — see
docs/CERT_ROTATION_RUNBOOK.md "Re-enrollment vs rotation". Nothing was
touched on $PI; aborting.
EOF
  exit 1
fi

echo "-- Staging new cert/key on $PI (0600, owner lexa) — the LIVE path is not touched yet"
ssh "$SSHUSER@$PI" "sudo install -d -m 750 -o lexa -g lexa $REMOTE_STAGE"
scp -q "$NEWCERT" "$SSHUSER@$PI:/tmp/rotate-client-cert.pem"
scp -q "$NEWKEY" "$SSHUSER@$PI:/tmp/rotate-client-key.pem"
ssh "$SSHUSER@$PI" "
  sudo install -m 644 -o lexa -g lexa /tmp/rotate-client-cert.pem $REMOTE_STAGE/client.pem &&
  sudo install -m 600 -o lexa -g lexa /tmp/rotate-client-key.pem  $REMOTE_STAGE/client-key.pem &&
  rm -f /tmp/rotate-client-cert.pem /tmp/rotate-client-key.pem
"

abort_cleanup() {
  echo "-- Cleaning up staging + any leftover sentinel/outcome files" >&2
  ssh "$SSHUSER@$PI" "sudo rm -rf $REMOTE_STAGE; sudo rm -f ${SENTINEL} ${SENTINEL}.*-*" || true
}

echo "-- Recording pre-rotation certstatus (client_not_after)"
BEFORE_NOTAFTER="$(ssh "$SSHUSER@$PI" "mosquitto_sub -h localhost -t lexa/northbound/certstatus -C 1 -W 3" 2>/dev/null \
  | python3 -c 'import json,sys; print(json.load(sys.stdin).get("client_not_after",""))' 2>/dev/null || true)"
echo "   before: client_not_after=$BEFORE_NOTAFTER"

echo "-- Writing sentinel — RotationController polls every ~5s (cfg.cert_rotate_poll_interval_s)"
REQUESTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
ssh "$SSHUSER@$PI" "cat > /tmp/rotate.request.json <<JSON
{\"client_cert\": \"$REMOTE_STAGE/client.pem\", \"client_key\": \"$REMOTE_STAGE/client-key.pem\", \"requested_at\": \"$REQUESTED_AT\"}
JSON
sudo install -m 644 -o lexa -g lexa /tmp/rotate.request.json $SENTINEL
rm -f /tmp/rotate.request.json"

echo "-- Waiting up to ${POLL_TIMEOUT}s for the rotation to commit (or refuse/fail)"
ELAPSED=0
OUTCOME=""
while (( ELAPSED < POLL_TIMEOUT )); do
  sleep "$POLL_INTERVAL"
  ELAPSED=$((ELAPSED + POLL_INTERVAL))
  if ssh "$SSHUSER@$PI" "compgen -G '${SENTINEL}.done-*'" >/dev/null 2>&1; then OUTCOME="done"; break; fi
  if ssh "$SSHUSER@$PI" "compgen -G '${SENTINEL}.failed-*'" >/dev/null 2>&1; then OUTCOME="failed"; break; fi
  if ssh "$SSHUSER@$PI" "compgen -G '${SENTINEL}.rejected-*'" >/dev/null 2>&1; then OUTCOME="rejected"; break; fi
  echo "   ...still waiting (${ELAPSED}s elapsed)"
done

if [[ "$OUTCOME" != "done" ]]; then
  cat >&2 <<EOF
ABORT: rotation did not commit within ${POLL_TIMEOUT}s (outcome: ${OUTCOME:-timeout}).
The live cert path was never touched; lexa-northbound is still using its
previous session on every fetcher that did not commit. Diagnose with:
  ssh $SSHUSER@$PI journalctl -u lexa-northbound --since '10 minutes ago' | grep 'cert rotation'
EOF
  abort_cleanup
  exit 1
fi

echo "-- Verifying certstatus reflects the new cert"
AFTER_NOTAFTER="$(ssh "$SSHUSER@$PI" "mosquitto_sub -h localhost -t lexa/northbound/certstatus -C 1 -W 3" 2>/dev/null \
  | python3 -c 'import json,sys; print(json.load(sys.stdin).get("client_not_after",""))' 2>/dev/null || true)"
echo "   after:  client_not_after=$AFTER_NOTAFTER"
if [[ -z "$AFTER_NOTAFTER" || "$AFTER_NOTAFTER" == "$BEFORE_NOTAFTER" ]]; then
  echo "WARNING: certstatus NotAfter did not change ($BEFORE_NOTAFTER -> $AFTER_NOTAFTER)." >&2
  echo "The sentinel reported 'done' but certstatus looks stale — investigate before trusting this rotation, and before promoting the staged cert." >&2
  exit 1
fi

echo "-- Verifying a fresh walk + rotation-commit line in the journal"
if ! ssh "$SSHUSER@$PI" "journalctl -u lexa-northbound --since '5 minutes ago' | grep -q 'cert rotation committed on all three fetchers'"; then
  echo "WARNING: expected journal line not found — the rotation may still be fine (certstatus already confirmed the NotAfter change), but inspect the journal manually before relying on this evidence for a soak/release gate." >&2
fi

echo "-- Promoting staged cert onto the live path (archiving the previous one first)"
TS="$(date -u +%Y%m%dT%H%M%SZ)"
ssh "$SSHUSER@$PI" "
  sudo install -d -m 750 -o lexa -g lexa $REMOTE_CERTS/archive &&
  sudo cp $REMOTE_CERTS/client.pem     $REMOTE_CERTS/archive/client-$TS.pem &&
  sudo cp $REMOTE_CERTS/client-key.pem $REMOTE_CERTS/archive/client-key-$TS.pem &&
  sudo chmod 600 $REMOTE_CERTS/archive/client-key-$TS.pem &&
  sudo install -m 640 -o lexa -g lexa $REMOTE_STAGE/client.pem     $REMOTE_CERTS/client.pem &&
  sudo install -m 600 -o lexa -g lexa $REMOTE_STAGE/client-key.pem $REMOTE_CERTS/client-key.pem &&
  sudo rm -rf $REMOTE_STAGE &&
  sudo rm -f ${SENTINEL}.done-*
"

echo "DONE: rotation committed on all three fetchers; live path promoted."
echo "      Previous cert archived: $REMOTE_CERTS/archive/client-$TS.pem"
