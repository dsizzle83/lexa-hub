#!/bin/bash
# Deploys the full lexa-hub service set to a Raspberry Pi (interim hub while
# the ConnectCore dev kit is unavailable).
#
# Usage:   bash scripts/deploy-hub-pi.sh <pi-ip> [ssh-user] [--enable-api-auth] [--enable-mqtt-acl]
# Example: bash scripts/deploy-hub-pi.sh 69.0.0.14 pi
#          bash scripts/deploy-hub-pi.sh 69.0.0.1 dmitri --enable-api-auth --enable-mqtt-acl
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
#   - generates per-service MQTT broker passwords (idempotent) under
#     /etc/lexa/mqtt/<svc>.pass and always patches them into each service's
#     mqtt_user/mqtt_pass_file (TASK-013 / W7 / AD-008) — this always runs, so
#     a plain deploy leaves every service holding valid credentials while the
#     broker still accepts anonymous connections. allow_anonymous only flips
#     to false, and the broker only starts enforcing password_file/acl_file,
#     when --enable-mqtt-acl is passed — staged rollout, mirrors
#     --enable-api-auth. Re-run with --enable-mqtt-acl only after confirming
#     (journal evidence) every service reconnected using its username.
#   - enables + starts: mosquitto → lexa-modbus, lexa-ocpp, lexa-api,
#     lexa-northbound, lexa-telemetry → lexa-hub
set -euo pipefail

PI="${1:?usage: deploy-hub-pi.sh <pi-ip> [ssh-user] [--enable-api-auth] [--enable-mqtt-acl]}"
shift
SSHUSER="pi"
ENABLE_API_AUTH=0
ENABLE_MQTT_ACL=0
for arg in "$@"; do
  case "$arg" in
    --enable-api-auth) ENABLE_API_AUTH=1 ;;
    --enable-mqtt-acl) ENABLE_MQTT_ACL=1 ;;
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
scp -q "$HERE"/systemd/lexa-*.service "$HERE"/systemd/mosquitto-lexa.conf "$HERE"/systemd/mosquitto-lexa.acl "$HERE"/systemd/journald-lexa.conf "$SSHUSER@$PI:/tmp/lexa-deploy/systemd/"
scp -q "$STAGE/ca-cert.pem"     "$SSHUSER@$PI:/tmp/lexa-deploy/certs/ca.pem"
scp -q "$STAGE/client-cert.pem" "$SSHUSER@$PI:/tmp/lexa-deploy/certs/client.pem"
scp -q "$STAGE/client-key.pem"  "$SSHUSER@$PI:/tmp/lexa-deploy/certs/client-key.pem"

echo "── Installing on the Pi (sudo)"
ssh "$SSHUSER@$PI" "sudo bash -s -- $ENABLE_API_AUTH $ENABLE_MQTT_ACL" <<'REMOTE'
set -euo pipefail
D=/tmp/lexa-deploy
ENABLE_API_AUTH="${1:-0}"
ENABLE_MQTT_ACL="${2:-0}"
SERVICES="modbus ocpp api northbound telemetry hub"
PASSWD_FILE=/etc/mosquitto/lexa-passwd
ACL_FILE=/etc/mosquitto/lexa-acl

# MQTT broker: distro package + lexa localhost listener. The repo's
# mosquitto-lexa.conf is written for the dev kit's standalone broker; the
# distro's main mosquitto.conf already sets persistence and log_dest, and
# mosquitto 2.x treats re-specifying them in conf.d as fatal duplicates —
# so install a slimmed drop-in with only the listener, queue bounds, and (once
# flipped) the credential/ACL directives.
if ! command -v mosquitto >/dev/null; then
  apt-get update -qq && apt-get install -y -qq mosquitto
fi

# lexa system user + directories (created before the credential block below,
# which writes per-service pass-files under /etc/lexa/mqtt owned by lexa).
id lexa >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin lexa
install -d -m 755 /etc/lexa
install -d -m 750 -o lexa -g lexa /etc/lexa/certs
install -d -m 750 -o lexa -g lexa /etc/lexa/mqtt

# Per-service MQTT broker credentials (TASK-013 / W7 / AD-008). This always
# runs, independent of --enable-mqtt-acl: every service gets a real password
# and mosquitto broker user on every deploy, so flipping allow_anonymous off
# later is just a config flip, never a race against credential generation.
# Idempotent: an existing pass-file's password is reused (mosquitto_passwd
# -b upserts the entry either way), so re-deploys don't rotate secrets or
# bounce every service's session for no reason.
install -m 644 $D/systemd/mosquitto-lexa.acl "$ACL_FILE"
for svc in $SERVICES; do
  passfile="/etc/lexa/mqtt/lexa-$svc.pass"
  if [[ ! -s "$passfile" ]]; then
    ( umask 077 && openssl rand -hex 16 > "$passfile" )
    chown lexa:lexa "$passfile"
    chmod 600 "$passfile"
  fi
  pass="$(cat "$passfile")"
  if [[ -s "$PASSWD_FILE" ]]; then
    mosquitto_passwd -b "$PASSWD_FILE" "lexa-$svc" "$pass"
  else
    mosquitto_passwd -b -c "$PASSWD_FILE" "lexa-$svc" "$pass"
  fi
done
# CORRECTED 2026-07-05 (TASK-006 bench validation): the 06931cc change to
# root:root 0600 was based on a false premise. strace of a fresh
# `systemctl restart mosquitto` on the hub Pi shows this package's
# mosquitto (2.0.21, Debian) calls setgid(106)/setuid(103) — dropping to
# the mosquitto:mosquitto user — BEFORE it ever opens password_file or
# acl_file, on every start (fresh or SIGHUP reload alike), regardless of
# whether a config `user` directive is present. root:root 0600 therefore
# makes the file unreadable unconditionally: mosquitto.service went into
# a restart-loop / "Unable to open pwfile" the moment this ran. Restoring
# root:mosquitto 0640 (the pre-06931cc mode) so the dropped-privilege
# process can always read it via group permission, on any restart path.
chown root:mosquitto "$PASSWD_FILE" "$ACL_FILE" 2>/dev/null || true
chmod 640 "$PASSWD_FILE" "$ACL_FILE"

if [[ "$ENABLE_MQTT_ACL" == "1" ]]; then
  cat > /etc/mosquitto/conf.d/lexa.conf <<'CONF'
# LEXA hub listener (installed by deploy-hub-pi.sh).
# localhost-only; credentials + ACL are defense-in-depth behind that, not a
# LAN opening (TASK-013 / W7). See systemd/mosquitto-lexa.conf in lexa-hub
# for the rationale and systemd/mosquitto-lexa.acl for the topic matrix.
listener 1883 localhost
allow_anonymous false
password_file /etc/mosquitto/lexa-passwd
acl_file /etc/mosquitto/lexa-acl
max_inflight_messages 20
max_queued_messages 1000
CONF
else
  cat > /etc/mosquitto/conf.d/lexa.conf <<'CONF'
# LEXA hub listener (installed by deploy-hub-pi.sh).
# localhost-only. Anonymous access is still ON (staged rollout, TASK-013 /
# W7): every service already carries broker credentials
# (/etc/lexa/mqtt/<svc>.pass, patched into mqtt_user/mqtt_pass_file below),
# but the broker isn't checking them yet. Re-run with --enable-mqtt-acl once
# journal evidence shows every service connected using its username.
listener 1883 localhost
allow_anonymous true
max_inflight_messages 20
max_queued_messages 1000
CONF
fi
systemctl enable mosquitto >/dev/null
systemctl reset-failed mosquitto 2>/dev/null || true
systemctl restart mosquitto

# Binaries, configs, certs.
install -m 755 $D/bin/lexa-* /usr/local/sbin/
install -m 644 $D/configs/*.json /etc/lexa/
install -m 640 -o lexa -g lexa $D/certs/ca.pem $D/certs/client.pem /etc/lexa/certs/
install -m 600 -o lexa -g lexa $D/certs/client-key.pem /etc/lexa/certs/

# Patch each service's mqtt_user/mqtt_pass_file (the config install above just
# overwrote /etc/lexa/*.json from the repo's example, mqtt_user/
# mqtt_pass_file both ""). Always populated, independent of --enable-mqtt-acl
# — see the credential block above; this is the "configs already carry
# credentials while the broker still allows anonymous" half of the staged
# rollout (step 6 of TASK-013).
for svc in $SERVICES; do
  python3 - "$svc" <<'PY'
import json, sys
svc = sys.argv[1]
path = f"/etc/lexa/{svc}.json"
with open(path) as f:
    cfg = json.load(f)
cfg["mqtt_user"] = f"lexa-{svc}"
cfg["mqtt_pass_file"] = f"/etc/lexa/mqtt/lexa-{svc}.pass"
with open(path, "w") as f:
    json.dump(cfg, f, indent=2)
    f.write("\n")
PY
done
echo "  mqtt_user/mqtt_pass_file patched into every /etc/lexa/*.json"

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
echo
if [[ "$ENABLE_MQTT_ACL" == "1" ]]; then
  echo "── MQTT broker: allow_anonymous false, password_file + acl_file live."
  echo "   ssh $SSHUSER@$PI sudo journalctl -u mosquitto -n 50 --no-pager | grep -i 'not authorised'   # want: no output"
  echo "   ssh $SSHUSER@$PI mosquitto_pub -h localhost -t lexa/control/battery/battery-0 -m '{}'        # want: rejected (no creds)"
else
  echo "── MQTT broker: still allow_anonymous true (staged rollout) — every service already carries"
  echo "   credentials (/etc/lexa/mqtt/lexa-<svc>.pass). Confirm each connected with its username:"
  echo "   ssh $SSHUSER@$PI sudo journalctl -u lexa-modbus -n 20 --no-pager | grep 'broker user='"
  echo "   Then re-run with --enable-mqtt-acl to flip allow_anonymous off and install password_file/acl_file."
fi
