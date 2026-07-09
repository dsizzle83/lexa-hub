#!/bin/bash
# Deploys the full lexa-hub service set to a Raspberry Pi (interim hub while
# the ConnectCore dev kit is unavailable).
#
# WS-1 (V1.0 punch list, security fail-closed by default, 2026-07-09):
# api-token auth and the MQTT broker ACL are now the DEFAULT — a plain
# invocation with no flags provisions and enables both. The former
# --enable-api-auth / --enable-mqtt-acl opt-in flags are kept as accepted
# no-op aliases (today's default already does what they used to turn on) so
# existing call sites don't break. Use --bench-insecure to restore the
# pre-WS-1 permissive behavior (anonymous MQTT, unauthenticated lexa-api) on
# the air-gapped 69.0.0.x LAN. --enable-ocpp-sp2 is UNCHANGED (still opt-in:
# it needs a staged CSMS cert + the same-session evsim lockstep below) —
# its plain/off path now explicitly sets ocpp.json's "bench": true so
# lexa-ocpp's new fail-closed startup gate (cmd/ocpp/config.go) doesn't
# block a plain bench deploy.
#
# Usage:   bash scripts/deploy-hub-pi.sh <pi-ip> [ssh-user] [--bench-insecure] [--enable-ocpp-sp2]
# Example: bash scripts/deploy-hub-pi.sh 69.0.0.14 pi
#          bash scripts/deploy-hub-pi.sh 69.0.0.1 dmitri --enable-ocpp-sp2
#          bash scripts/deploy-hub-pi.sh 69.0.0.1 dmitri --bench-insecure   # pre-WS-1 permissive bench mode
#
# Prereqs (already done if you're following the bench bring-up):
#   - make build-arm64 (bin/arm64/lexa-* present, incl. northbound/telemetry)
#   - client certs staged in ../csip-tls-test/certs/client-staging/
#   - key-based SSH to the Pi with passwordless sudo (or run with -t and type it)
#   - for --enable-ocpp-sp2: ../csip-tls-test/certs/ev-server-cert.pem +
#     certs/vault/ev-server-key.pem must exist — run
#     `bash scripts/gen-ev-cert.sh <hub-ip>` in csip-tls-test first (TASK-074)
#
# What it does on the Pi:
#   - installs distro mosquitto + the lexa localhost listener conf
#   - creates the lexa system user, /etc/lexa{,/certs}
#   - installs binaries to /usr/local/sbin, configs, systemd units
#   - generates /etc/lexa/api.token (idempotent) for lexa-api's bearer-token
#     auth (TASK-014 / AD-008 / WS-1) and wires it into api.json's
#     api_token_file BY DEFAULT — a plain deploy now starts with auth on.
#     --bench-insecure leaves api_token_file unset (today's pre-WS-1
#     behavior) and sets api.json's "bench": true so lexa-api's fail-closed
#     startup gate on its LAN-reachable listen_addr doesn't block it.
#   - generates per-service MQTT broker passwords (idempotent) under
#     /etc/lexa/mqtt/<svc>.pass and always patches them into each service's
#     mqtt_user/mqtt_pass_file (TASK-013 / W7 / AD-008) — independent of
#     --bench-insecure, every service always holds valid credentials.
#     allow_anonymous is false and password_file/acl_file are enforced BY
#     DEFAULT; --bench-insecure flips allow_anonymous back to true (today's
#     pre-WS-1 behavior) for the air-gapped LAN.
#   - stages the OCPP CSMS TLS cert/key + generates an idempotent Basic Auth
#     secret (/etc/lexa/ocpp-auth.pass, 0600 lexa:lexa) and wires
#     cert_path/key_path/basic_auth_user/basic_auth_pass into ocpp.json —
#     ONLY when --enable-ocpp-sp2 is passed (TASK-074 / AD-008 / 09 Security
#     hard gate; unchanged by WS-1, still opt-in). LOCKSTEP: evsim must flip
#     to wss:// in the SAME session — see csip-tls-test's
#     `scripts/update-sim-pis.sh --enable-ocpp-sp2`, or lexa-ocpp starts
#     requiring TLS+auth while evsim still dials plain ws://, which blinds
#     every EV Mayhem scenario instantly.
#   - enables + starts: mosquitto → lexa-modbus, lexa-ocpp, lexa-api,
#     lexa-northbound, lexa-telemetry → lexa-hub
set -euo pipefail

PI="${1:?usage: deploy-hub-pi.sh <pi-ip> [ssh-user] [--bench-insecure] [--enable-ocpp-sp2]}"
shift
SSHUSER="pi"
BENCH_INSECURE=0
ENABLE_OCPP_SP2=0
for arg in "$@"; do
  case "$arg" in
    --bench-insecure) BENCH_INSECURE=1 ;;
    --enable-api-auth|--enable-mqtt-acl) echo "note: $arg is now the default behavior (WS-1); no-op" >&2 ;;
    --enable-ocpp-sp2) ENABLE_OCPP_SP2=1 ;;
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
if [[ "$ENABLE_OCPP_SP2" == "1" ]]; then
  [[ -f "$CSIP/certs/ev-server-cert.pem" ]] || { echo "missing $CSIP/certs/ev-server-cert.pem — run: bash scripts/gen-ev-cert.sh $PI (in csip-tls-test)"; exit 1; }
  [[ -f "$CSIP/certs/vault/ev-server-key.pem" ]] || { echo "missing $CSIP/certs/vault/ev-server-key.pem — run: bash scripts/gen-ev-cert.sh $PI (in csip-tls-test)"; exit 1; }
fi

echo "── Copying artifacts to $SSHUSER@$PI:/tmp/lexa-deploy/"
ssh "$SSHUSER@$PI" 'rm -rf /tmp/lexa-deploy && mkdir -p /tmp/lexa-deploy/{bin,configs,systemd,certs}'
scp -q "$HERE"/bin/arm64/lexa-* "$SSHUSER@$PI:/tmp/lexa-deploy/bin/"
scp -q "$HERE"/configs/*.json "$SSHUSER@$PI:/tmp/lexa-deploy/configs/"
scp -q "$HERE"/systemd/lexa-*.service "$HERE"/systemd/mosquitto-lexa.conf "$HERE"/systemd/mosquitto-lexa.acl "$HERE"/systemd/journald-lexa.conf "$SSHUSER@$PI:/tmp/lexa-deploy/systemd/"
scp -q "$STAGE/ca-cert.pem"     "$SSHUSER@$PI:/tmp/lexa-deploy/certs/ca.pem"
scp -q "$STAGE/client-cert.pem" "$SSHUSER@$PI:/tmp/lexa-deploy/certs/client.pem"
scp -q "$STAGE/client-key.pem"  "$SSHUSER@$PI:/tmp/lexa-deploy/certs/client-key.pem"
if [[ "$ENABLE_OCPP_SP2" == "1" ]]; then
  scp -q "$CSIP/certs/ev-server-cert.pem"      "$SSHUSER@$PI:/tmp/lexa-deploy/certs/ocpp-cert.pem"
  scp -q "$CSIP/certs/vault/ev-server-key.pem" "$SSHUSER@$PI:/tmp/lexa-deploy/certs/ocpp-key.pem"
fi

echo "── Installing on the Pi (sudo)"
ssh "$SSHUSER@$PI" "sudo bash -s -- $BENCH_INSECURE $ENABLE_OCPP_SP2" <<'REMOTE'
set -euo pipefail
D=/tmp/lexa-deploy
BENCH_INSECURE="${1:-0}"
ENABLE_OCPP_SP2="${2:-0}"
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
# runs, independent of --bench-insecure: every service gets a real password
# and mosquitto broker user on every deploy, so flipping allow_anonymous off
# (the WS-1 default below) is never a race against credential generation.
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

if [[ "$BENCH_INSECURE" == "1" ]]; then
  cat > /etc/mosquitto/conf.d/lexa.conf <<'CONF'
# LEXA hub listener (installed by deploy-hub-pi.sh).
# localhost-only. Anonymous access is ON (--bench-insecure, pre-WS-1
# behavior): every service already carries broker credentials
# (/etc/lexa/mqtt/<svc>.pass, patched into mqtt_user/mqtt_pass_file below),
# but the broker isn't checking them. Re-run without --bench-insecure to
# flip allow_anonymous off and install password_file/acl_file.
listener 1883 localhost
allow_anonymous true
max_inflight_messages 20
max_queued_messages 1000
CONF
else
  cat > /etc/mosquitto/conf.d/lexa.conf <<'CONF'
# LEXA hub listener (installed by deploy-hub-pi.sh).
# localhost-only; credentials + ACL are defense-in-depth behind that, not a
# LAN opening (TASK-013 / W7 / WS-1 — this is now the DEFAULT). See
# systemd/mosquitto-lexa.conf in lexa-hub for the rationale and
# systemd/mosquitto-lexa.acl for the topic matrix. --bench-insecure restores
# allow_anonymous true.
listener 1883 localhost
allow_anonymous false
password_file /etc/mosquitto/lexa-passwd
acl_file /etc/mosquitto/lexa-acl
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
# mqtt_pass_file both ""). Always populated, independent of --bench-insecure
# — see the credential block above (step 6 of TASK-013).
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

# lexa-api bearer-token auth (TASK-014 / AD-008 / WS-1). The config install
# above just overwrote /etc/lexa/api.json from the repo's example
# (api_token_file "", listen_addr "127.0.0.1:9100" — WS-1's loopback-only
# product default), so patching happens AFTER install, like
# hub-replay-tune.sh patches timing — never re-declared wholesale. This
# script also always overrides listen_addr to ":9100" (LAN-reachable) here: the
# bench's dashboard/metersim on other 69.0.0.x hosts need to reach lexa-api,
# same bench-vs-product framing as MetricsAddr (cmd/hub/config.go).
TOKEN_FILE=/etc/lexa/api.token
if [[ ! -s "$TOKEN_FILE" ]]; then
  ( umask 077 && openssl rand -hex 32 > "$TOKEN_FILE" )
  chown lexa:lexa "$TOKEN_FILE"
  chmod 600 "$TOKEN_FILE"
  echo "  generated $TOKEN_FILE (0600 lexa:lexa)"
else
  echo "  $TOKEN_FILE already present — left untouched"
fi
if [[ "$BENCH_INSECURE" == "1" ]]; then
  # --bench-insecure: leave api_token_file unset (pre-WS-1 open behavior),
  # but the LAN-reachable listen_addr below needs "bench": true or lexa-api's
  # fail-closed startup gate refuses to bind non-loopback with no token.
  python3 - <<PY
import json
path = "/etc/lexa/api.json"
with open(path) as f:
    cfg = json.load(f)
cfg["listen_addr"] = ":9100"
cfg["api_token_file"] = ""
cfg["bench"] = True
with open(path, "w") as f:
    json.dump(cfg, f, indent=2)
    f.write("\n")
PY
  echo "  --bench-insecure: api_token_file left unset, bench:true set → /etc/lexa/api.json; auth OFF"
else
  python3 - <<PY
import json
path = "/etc/lexa/api.json"
with open(path) as f:
    cfg = json.load(f)
cfg["listen_addr"] = ":9100"
cfg["api_token_file"] = "$TOKEN_FILE"
cfg["bench"] = False
with open(path, "w") as f:
    json.dump(cfg, f, indent=2)
    f.write("\n")
PY
  echo "  api_token_file set → /etc/lexa/api.json; lexa-api will require the bearer token once restarted"
fi

# OCPP Security Profile 2 (TASK-074 / AD-008 / 09 Security hard gate: "OCPP
# security profile ≥2; ws:// disabled in product config"). Same staged-rollout
# shape as api-token/mqtt-acl above: the config install just overwrote
# ocpp.json from the repo's example (cert_path/key_path/basic_auth_user/
# basic_auth_pass all ""), so enabling has to happen AFTER install, patched
# in — never re-declared wholesale. Only runs when --enable-ocpp-sp2 is
# passed; a plain deploy leaves lexa-ocpp on ws://, no auth (bench-only
# fallback, never the product default).
#
# LOCKSTEP WARNING: once this patches ocpp.json and lexa-ocpp restarts below,
# any evsim still dialing ws:// is instantly rejected (TLS handshake failure)
# — re-run csip-tls-test's `scripts/update-sim-pis.sh <hub-ip> <ssh-user>
# --enable-ocpp-sp2` in THIS SAME SESSION (05 §11 same-session rule, same
# class as MTR-4).
OCPP_AUTH_USER="evse-bench"
if [[ "$ENABLE_OCPP_SP2" == "1" ]]; then
  install -m 644 -o lexa -g lexa "$D/certs/ocpp-cert.pem" /etc/lexa/certs/ocpp-cert.pem
  install -m 600 -o lexa -g lexa "$D/certs/ocpp-key.pem"  /etc/lexa/certs/ocpp-key.pem

  OCPP_AUTH_PASS_FILE=/etc/lexa/ocpp-auth.pass
  if [[ ! -s "$OCPP_AUTH_PASS_FILE" ]]; then
    ( umask 077 && openssl rand -hex 16 > "$OCPP_AUTH_PASS_FILE" )
    chown lexa:lexa "$OCPP_AUTH_PASS_FILE"
    chmod 600 "$OCPP_AUTH_PASS_FILE"
    echo "  generated $OCPP_AUTH_PASS_FILE (0600 lexa:lexa)"
  else
    echo "  $OCPP_AUTH_PASS_FILE already present — left untouched (secret is stable across redeploys)"
  fi
  OCPP_AUTH_PASS="$(cat "$OCPP_AUTH_PASS_FILE")"

  python3 - "$OCPP_AUTH_USER" "$OCPP_AUTH_PASS" <<'PY'
import json, sys
auth_user, auth_pass = sys.argv[1], sys.argv[2]
path = "/etc/lexa/ocpp.json"
with open(path) as f:
    cfg = json.load(f)
cfg["cert_path"] = "/etc/lexa/certs/ocpp-cert.pem"
cfg["key_path"] = "/etc/lexa/certs/ocpp-key.pem"
cfg["basic_auth_user"] = auth_user
cfg["basic_auth_pass"] = auth_pass
with open(path, "w") as f:
    json.dump(cfg, f, indent=2)
    f.write("\n")
PY
  echo "  cert_path/key_path/basic_auth_user/basic_auth_pass set → /etc/lexa/ocpp.json; lexa-ocpp will require TLS+auth once restarted"
else
  # WS-1: the shipped configs/ocpp.json now ships SP2 fields populated with
  # placeholder/deploy-provisioned values (product-default template) so a
  # config that's never been through this script fails closed. A plain
  # bench deploy (no --enable-ocpp-sp2) blanks those placeholders back out
  # and sets "bench": true so lexa-ocpp's fail-closed startup gate
  # (cmd/ocpp/config.go) lets it start on plaintext ws://, no auth — same
  # runtime behavior the bench has always had.
  python3 - <<'PY'
import json
path = "/etc/lexa/ocpp.json"
with open(path) as f:
    cfg = json.load(f)
cfg["cert_path"] = ""
cfg["key_path"] = ""
cfg["basic_auth_user"] = ""
cfg["basic_auth_pass"] = ""
cfg["bench"] = True
with open(path, "w") as f:
    json.dump(cfg, f, indent=2)
    f.write("\n")
PY
  echo "  bench:true set, SP2 fields cleared → /etc/lexa/ocpp.json (ws://, no auth, bench-only per docs/BENCH.md)"
  echo "  OCPP Security Profile 2 left OFF (ws://, no auth) — re-run with --enable-ocpp-sp2 once"
  echo "  csip-tls-test/certs/ev-server-cert.pem + certs/vault/ev-server-key.pem exist (gen-ev-cert.sh)"
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
if [[ "$BENCH_INSECURE" == "1" ]]; then
  echo "   curl http://$PI:9100/status | python3 -m json.tool | head              # auth off (--bench-insecure)"
  echo "   Then restart the dashboard with -hub http://$PI:9100"
  echo "   Re-run WITHOUT --bench-insecure once the dashboard/metersim carry the token (scripts/update-sim-pis.sh, bench-up.sh)."
else
  echo "   curl -s http://$PI:9100/status                                        # → 401 (auth on, WS-1 default)"
  echo "   curl -s -H \"Authorization: Bearer \$(ssh $SSHUSER@$PI sudo cat /etc/lexa/api.token)\" http://$PI:9100/status | python3 -m json.tool | head"
  echo "   curl -s http://$PI:9100/healthz                                        # → 200 (never authenticated)"
  echo "   Then restart the dashboard/metersim with -hub-token-file pointing at a copy of that token (see docs/BENCH.md)."
fi
echo
if [[ "$BENCH_INSECURE" == "1" ]]; then
  echo "── MQTT broker: allow_anonymous true (--bench-insecure) — every service already carries"
  echo "   credentials (/etc/lexa/mqtt/lexa-<svc>.pass). Confirm each connected with its username:"
  echo "   ssh $SSHUSER@$PI sudo journalctl -u lexa-modbus -n 20 --no-pager | grep 'broker user='"
  echo "   Re-run WITHOUT --bench-insecure to flip allow_anonymous off and install password_file/acl_file."
else
  echo "── MQTT broker: allow_anonymous false, password_file + acl_file live (WS-1 default)."
  echo "   ssh $SSHUSER@$PI sudo journalctl -u mosquitto -n 50 --no-pager | grep -i 'not authorised'   # want: no output"
  echo "   ssh $SSHUSER@$PI mosquitto_pub -h localhost -t lexa/control/battery/battery-0 -m '{}'        # want: rejected (no creds)"
fi
echo
if [[ "$ENABLE_OCPP_SP2" == "1" ]]; then
  echo "── OCPP Security Profile 2: TLS + Basic Auth live on :8887."
  echo "   ssh $SSHUSER@$PI sudo journalctl -u lexa-ocpp -n 20 --no-pager | grep 'TLS enabled'   # want: 1 line"
  echo "   LOCKSTEP — do this in THE SAME SESSION or every EV Mayhem scenario goes BLIND:"
  echo "   bash ~/projects/csip-tls-test/scripts/update-sim-pis.sh $PI $SSHUSER --enable-ocpp-sp2"
  echo "   Negative-auth check (from the desktop, needs a WS client — see docs/BENCH.md OCPP SP2 runbook"
  echo "   for the wscat form; unit-level equivalent: 'go test ./cmd/ocpp/... -run TestOCPPSecurityProfile2_BasicAuth')."
else
  echo "── OCPP: still ws://, no auth (bench-only fallback, staged rollout) — re-run with --enable-ocpp-sp2"
  echo "   once ../csip-tls-test/certs/ev-server-cert.pem is provisioned (gen-ev-cert.sh $PI)."
fi
