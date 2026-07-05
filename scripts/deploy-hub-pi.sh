#!/bin/bash
# Deploys the full lexa-hub service set to a Raspberry Pi (interim hub while
# the ConnectCore dev kit is unavailable).
#
# Usage:   bash scripts/deploy-hub-pi.sh <pi-ip> [ssh-user] [--enable-api-auth]
# Example: bash scripts/deploy-hub-pi.sh 69.0.0.14 pi
#          bash scripts/deploy-hub-pi.sh 69.0.0.1 dmitri --enable-api-auth
#
# Prereqs (already done if you're following the bench bring-up):
#   - make build-arm64 (bin/arm64/lexa-* present, incl. northbound/telemetry)
#   - client certs staged in ../csip-tls-test/certs/client-staging/
#   - key-based SSH to the Pi with passwordless sudo (or run with -t and type it)
#
# What it does on the Pi:
#   - installs distro mosquitto + the lexa localhost listener conf
#   - creates the lexa system user, /etc/lexa{,/certs}
#   - installs binaries to /usr/local/sbin, configs, systemd units
#   - generates /etc/lexa/api.token (idempotent) for lexa-api's bearer-token
#     auth (TASK-014 / AD-008); the token is only wired into api.json's
#     api_token_file when --enable-api-auth is passed — staged rollout, so a
#     plain deploy never flips auth on before the dashboard/metersim have the
#     token distributed to them (see docs/BENCH.md)
#   - enables + starts: mosquitto → lexa-modbus, lexa-ocpp, lexa-api,
#     lexa-northbound, lexa-telemetry → lexa-hub
set -euo pipefail

PI="${1:?usage: deploy-hub-pi.sh <pi-ip> [ssh-user] [--enable-api-auth]}"
shift
SSHUSER="pi"
ENABLE_API_AUTH=0
for arg in "$@"; do
  case "$arg" in
    --enable-api-auth) ENABLE_API_AUTH=1 ;;
    *) SSHUSER="$arg" ;;
  esac
done
HERE="$(cd "$(dirname "$0")/.." && pwd)"
CSIP="$(cd "$HERE/../csip-tls-test" && pwd)"
STAGE="$CSIP/certs/client-staging"

for f in lexa-hub lexa-modbus lexa-ocpp lexa-api lexa-northbound lexa-telemetry; do
  [[ -x "$HERE/bin/arm64/$f" ]] || { echo "missing $HERE/bin/arm64/$f — run make build-arm64"; exit 1; }
done
for f in ca-cert.pem client-cert.pem client-key.pem; do
  [[ -f "$STAGE/$f" ]] || { echo "missing $STAGE/$f — run gen-client-cert.sh"; exit 1; }
done

echo "── Copying artifacts to $SSHUSER@$PI:/tmp/lexa-deploy/"
ssh "$SSHUSER@$PI" 'rm -rf /tmp/lexa-deploy && mkdir -p /tmp/lexa-deploy/{bin,configs,systemd,certs}'
scp -q "$HERE"/bin/arm64/lexa-* "$SSHUSER@$PI:/tmp/lexa-deploy/bin/"
scp -q "$HERE"/configs/*.json "$SSHUSER@$PI:/tmp/lexa-deploy/configs/"
scp -q "$HERE"/systemd/lexa-*.service "$HERE"/systemd/mosquitto-lexa.conf "$HERE"/systemd/journald-lexa.conf "$SSHUSER@$PI:/tmp/lexa-deploy/systemd/"
scp -q "$STAGE/ca-cert.pem"     "$SSHUSER@$PI:/tmp/lexa-deploy/certs/ca.pem"
scp -q "$STAGE/client-cert.pem" "$SSHUSER@$PI:/tmp/lexa-deploy/certs/client.pem"
scp -q "$STAGE/client-key.pem"  "$SSHUSER@$PI:/tmp/lexa-deploy/certs/client-key.pem"

echo "── Installing on the Pi (sudo)"
ssh "$SSHUSER@$PI" "sudo bash -s -- $ENABLE_API_AUTH" <<'REMOTE'
set -euo pipefail
D=/tmp/lexa-deploy
ENABLE_API_AUTH="${1:-0}"

# MQTT broker: distro package + lexa localhost listener. The repo's
# mosquitto-lexa.conf is written for the dev kit's standalone broker; the
# distro's main mosquitto.conf already sets persistence and log_dest, and
# mosquitto 2.x treats re-specifying them in conf.d as fatal duplicates —
# so install a slimmed drop-in with only the listener and queue bounds.
if ! command -v mosquitto >/dev/null; then
  apt-get update -qq && apt-get install -y -qq mosquitto
fi
cat > /etc/mosquitto/conf.d/lexa.conf <<'CONF'
# LEXA hub listener (installed by deploy-hub-pi.sh).
# localhost-only, so anonymous access stays acceptable; see
# systemd/mosquitto-lexa.conf in lexa-hub for the rationale and ACL notes.
listener 1883 localhost
allow_anonymous true
max_inflight_messages 20
max_queued_messages 1000
CONF
systemctl enable mosquitto >/dev/null
systemctl reset-failed mosquitto 2>/dev/null || true
systemctl restart mosquitto

# lexa system user + directories.
id lexa >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin lexa
install -d -m 755 /etc/lexa
install -d -m 750 -o lexa -g lexa /etc/lexa/certs

# Binaries, configs, certs.
install -m 755 $D/bin/lexa-* /usr/local/sbin/
install -m 644 $D/configs/*.json /etc/lexa/
install -m 640 -o lexa -g lexa $D/certs/ca.pem $D/certs/client.pem /etc/lexa/certs/
install -m 600 -o lexa -g lexa $D/certs/client-key.pem /etc/lexa/certs/

# lexa-api bearer-token auth (TASK-014 / AD-008). The config install above
# just overwrote /etc/lexa/api.json from the repo's example (api_token_file
# ""), so any enabling of auth has to happen AFTER install, patched in like
# hub-replay-tune.sh patches timing — never re-declared wholesale.
TOKEN_FILE=/etc/lexa/api.token
if [[ ! -s "$TOKEN_FILE" ]]; then
  ( umask 077 && openssl rand -hex 32 > "$TOKEN_FILE" )
  chown lexa:lexa "$TOKEN_FILE"
  chmod 600 "$TOKEN_FILE"
  echo "  generated $TOKEN_FILE (0600 lexa:lexa)"
else
  echo "  $TOKEN_FILE already present — left untouched"
fi
if [[ "$ENABLE_API_AUTH" == "1" ]]; then
  python3 - <<PY
import json
path = "/etc/lexa/api.json"
with open(path) as f:
    cfg = json.load(f)
cfg["api_token_file"] = "$TOKEN_FILE"
with open(path, "w") as f:
    json.dump(cfg, f, indent=2)
    f.write("\n")
PY
  echo "  api_token_file set → /etc/lexa/api.json; lexa-api will require the bearer token once restarted"
else
  echo "  api_token_file left unset — auth still OFF; re-run with --enable-api-auth once the dashboard and metersim carry $TOKEN_FILE's contents"
fi

# Units: the repo ships its own mosquitto.service for the dev kit; on a Pi we
# use the distro's, so install only the lexa-* units.
install -m 644 $D/systemd/lexa-*.service /etc/systemd/system/
systemctl daemon-reload

# journald size/retention budget (TASK-009, review §11 flash wear / RSK-14):
# a conf.d drop-in, not an in-place edit of journald.conf, so this stays
# reproducible from the repo on every deploy. Restarting systemd-journald
# picks up SystemMaxUse/SystemKeepFree/MaxRetentionSec immediately; per-unit
# LogRateLimit* lives in each lexa-*.service above instead.
install -d -m 755 /etc/systemd/journald.conf.d
install -m 644 $D/systemd/journald-lexa.conf /etc/systemd/journald.conf.d/lexa.conf
systemctl restart systemd-journald

for s in lexa-modbus lexa-ocpp lexa-api lexa-northbound lexa-telemetry lexa-hub; do
  systemctl enable "$s" >/dev/null
  systemctl restart "$s"
done
sleep 3
echo "── Service status:"
for s in mosquitto lexa-modbus lexa-ocpp lexa-api lexa-northbound lexa-telemetry lexa-hub; do
  printf '  %-18s %s\n' "$s" "$(systemctl is-active $s)"
done
rm -rf $D
REMOTE

echo
echo "── Done. Verify from the desktop:"
if [[ "$ENABLE_API_AUTH" == "1" ]]; then
  echo "   curl -s http://$PI:9100/status                                        # → 401 (auth on)"
  echo "   curl -s -H \"Authorization: Bearer \$(ssh $SSHUSER@$PI sudo cat /etc/lexa/api.token)\" http://$PI:9100/status | python3 -m json.tool | head"
  echo "   curl -s http://$PI:9100/healthz                                        # → 200 (never authenticated)"
  echo "   Then restart the dashboard/metersim with -hub-token-file pointing at a copy of that token (see docs/BENCH.md)."
else
  echo "   curl http://$PI:9100/status | python3 -m json.tool | head              # auth still off"
  echo "   Then restart the dashboard with -hub http://$PI:9100"
  echo "   When the dashboard/metersim carry the token (scripts/update-sim-pis.sh, bench-up.sh), re-run with --enable-api-auth."
fi
