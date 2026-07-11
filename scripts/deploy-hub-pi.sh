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
# TLS-on-by-default (bring-up fix, 2026-07-11): the repo's dev/bench example
# configs/api.json now ships "tls": true, matching configs/factory/api.json —
# the companion app is HTTPS-only (cert-pinned) and simply cannot reach a
# plain-http hub at all. lexa-api auto-generates its own self-signed cert on
# first boot (cmd/api/tlscert.go), so flipping this needed no cert staging.
# This script also asserts, right after installing /etc/lexa/api.json below,
# that its "tls" key isn't explicitly false — a LOUD (non-fatal) warning if
# it is, so a hand-edited or stale config can't silently re-introduce the
# app-breaking plain-http hub.
#
# Extension campaign catch-up (units 4.5/2.1/1.5/1.6, 2026-07-10): lexa-hub's
# canonical install set grew to 6 services + lexa-healthcheck + lexa-migrate
# + lexa-cloudlink + lexactl (see the Makefile's `install`/`install-services`
# targets), plus two new units — lexa-commission.path/.service (unit 4.5's
# privilege-free commissioning restart trigger, scripts/lexa-commission-apply)
# — this script now stages and installs all of it: the four extra binaries,
# lexa-cloudlink/lexa-migrate/lexa-commission.path/.service unit files, the
# commission-apply script, and configs/cloudlink.json (installed no-overwrite,
# unlike the six services' configs — see the remote block). lexa-migrate now
# runs once, directly, BEFORE the service restart loop (aborts the deploy
# loudly on nonzero — a botched schema migration must not be masked by
# restarting services against it); lexa-healthcheck runs once, AFTER, as an
# advisory-only check (deploys here are operator-attended, so a failure
# prints loudly but never rolls anything back — there is no A/B slot to fall
# back to on this Pi, unlike Mender's ArtifactCommit gate on the SOM).
#
# Usage:   bash scripts/deploy-hub-pi.sh <pi-ip> [ssh-user] [--bench-insecure] [--enable-ocpp-sp2]
# Example: bash scripts/deploy-hub-pi.sh 69.0.0.14 pi
#          bash scripts/deploy-hub-pi.sh 69.0.0.1 dmitri --enable-ocpp-sp2
#          bash scripts/deploy-hub-pi.sh 69.0.0.1 dmitri --bench-insecure   # pre-WS-1 permissive bench mode
#
# Prereqs (already done if you're following the bench bring-up):
#   - make build-arm64 (bin/arm64/lexa-* present — incl. northbound/telemetry,
#     healthcheck, migrate, cloudlink — plus bin/arm64/lexactl, which doesn't
#     match the lexa-* name pattern)
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
#   - asserts, right after installing /etc/lexa/api.json, that its "tls" key
#     isn't explicitly false — printing a LOUD non-fatal warning if it is,
#     since the companion app is HTTPS-only and cannot reach a plain-http
#     hub at all (TLS-on is the default in both configs/api.json and
#     configs/factory/api.json now).
#   - generates per-service MQTT broker passwords (idempotent) under
#     /etc/lexa/mqtt/<svc>.pass and always patches them into each service's
#     mqtt_user/mqtt_pass_file (TASK-013 / W7 / AD-008) — independent of
#     --bench-insecure, every service always holds valid credentials.
#     allow_anonymous is false and password_file/acl_file are enforced BY
#     DEFAULT; --bench-insecure flips allow_anonymous back to true (today's
#     pre-WS-1 behavior) for the air-gapped LAN.
#   - ALSO provisions a lexa-cloudlink broker user + /etc/lexa/mqtt/
#     cloudlink.pass (TASK-082, unit 1.3 — same idempotent generation as the
#     six services above) so systemd/mosquitto-lexa.acl's lexa-cloudlink
#     stanza is backed by real credentials from day one.
#   - installs /etc/lexa/cloudlink.json from configs/cloudlink.json (unit
#     2.1+, now shipping) ONLY IF ABSENT — unlike the six services' configs
#     (always overwritten, then immediately re-patched below), cloudlink.json
#     carries operator-set commissioning state (enabled, endpoint,
#     serial_file, cert paths) this script never re-patches, so clobbering it
#     on every redeploy would erase a device's cloud enrollment. Once
#     present (fresh or pre-existing), mqtt_user/mqtt_pass_file are patched
#     into it the same way as the six services.
#   - runs /usr/local/sbin/lexa-migrate once, directly, BEFORE restarting any
#     service (unit 1.6) — aborts the whole deploy loudly on nonzero exit.
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
#     lexa-northbound, lexa-telemetry, lexa-cloudlink → lexa-hub
#   - enables (but does not start/restart) lexa-migrate and
#     lexa-commission.path — lexa-migrate already ran directly above, and
#     lexa-commission.path is a passive watcher a reboot picks up on its
#     own; matches the Makefile's install-services (enable-only) vs start
#     (restart-now) split for these same two units. lexa-commission.service
#     is never enabled — it's trigger-activated by the .path unit only.
#   - runs /usr/local/sbin/lexa-healthcheck -budget 60s once, after the
#     restarts, as an advisory-only check (unit 1.5): a failure PRINTS
#     LOUDLY but never aborts or rolls back — deploys here are
#     operator-attended, and there's no A/B slot to fall back to.
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

for f in lexa-hub lexa-modbus lexa-ocpp lexa-api lexa-northbound lexa-telemetry lexa-cloudlink lexa-healthcheck lexa-migrate; do
  [[ -x "$HERE/bin/arm64/$f" ]] || { echo "missing $HERE/bin/arm64/$f — run make build-arm64"; exit 1; }
done
# lexactl doesn't fit the lexa-* binary name pattern (it's the operator CLI,
# unit 7.1) so it needs its own existence check and, further down, its own
# scp/install lines — the "bin/arm64/lexa-*" globs used everywhere else in
# this script never match it.
[[ -x "$HERE/bin/arm64/lexactl" ]] || { echo "missing $HERE/bin/arm64/lexactl — run make build-arm64"; exit 1; }
for f in ca-cert.pem client-cert.pem client-key.pem; do
  [[ -f "$STAGE/$f" ]] || { echo "missing $STAGE/$f — run gen-client-cert.sh"; exit 1; }
done
if [[ "$ENABLE_OCPP_SP2" == "1" ]]; then
  [[ -f "$CSIP/certs/ev-server-cert.pem" ]] || { echo "missing $CSIP/certs/ev-server-cert.pem — run: bash scripts/gen-ev-cert.sh $PI (in csip-tls-test)"; exit 1; }
  [[ -f "$CSIP/certs/vault/ev-server-key.pem" ]] || { echo "missing $CSIP/certs/vault/ev-server-key.pem — run: bash scripts/gen-ev-cert.sh $PI (in csip-tls-test)"; exit 1; }
fi

echo "── Copying artifacts to $SSHUSER@$PI:/tmp/lexa-deploy/"
ssh "$SSHUSER@$PI" 'rm -rf /tmp/lexa-deploy && mkdir -p /tmp/lexa-deploy/{bin,configs,systemd,certs}'
# bin/arm64/lexa-* already sweeps up lexa-cloudlink/lexa-healthcheck/
# lexa-migrate (built under those exact names by build-arm64); lexactl and
# scripts/lexa-commission-apply don't match that glob (no "lexa-" prefix /
# not a build-arm64 artifact) so they're staged explicitly. commission-apply
# rides into the same "bin" tmp dir since it lands in /usr/local/sbin too.
scp -q "$HERE"/bin/arm64/lexa-* "$SSHUSER@$PI:/tmp/lexa-deploy/bin/"
scp -q "$HERE/bin/arm64/lexactl" "$SSHUSER@$PI:/tmp/lexa-deploy/bin/lexactl"
scp -q "$HERE/scripts/lexa-commission-apply" "$SSHUSER@$PI:/tmp/lexa-deploy/bin/lexa-commission-apply"
scp -q "$HERE"/configs/*.json "$SSHUSER@$PI:/tmp/lexa-deploy/configs/"
scp -q "$HERE"/systemd/lexa-*.service "$HERE"/systemd/lexa-commission.path "$HERE"/systemd/mosquitto-lexa.conf "$HERE"/systemd/mosquitto-lexa.acl "$HERE"/systemd/journald-lexa.conf "$SSHUSER@$PI:/tmp/lexa-deploy/systemd/"
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
# lexa-cloudlink broker credential (TASK-082, unit 1.3 — pre-provisioning
# only; the cloudlink service ships in the standard set as of the 2026-07-10 merge). Mirrors the
# loop above exactly (idempotent pass-file, 0600 lexa:lexa, mosquitto_passwd
# upsert) but is deliberately NOT folded into the $SERVICES loop: that loop
# also drives the mqtt_user/mqtt_pass_file config-patch further below, which
# assumes /etc/lexa/<svc>.json already exists (installed from configs/*.json
# just above) — /etc/lexa/cloudlink.json won't exist until unit 2.1+ actually
# ships it, and under set -euo pipefail a missing file there would abort the
# whole deploy. The pass-file name (cloudlink.pass, not lexa-cloudlink.pass)
# matches configs/cloudlink.json's shipped mqtt_pass_file value verbatim
# (docs/DEVICE_ROADMAP.md §2.2) even though it breaks the lexa-<svc>.pass
# pattern above — the username itself (lexa-cloudlink) still follows house
# convention.
CLOUDLINK_PASSFILE=/etc/lexa/mqtt/cloudlink.pass
if [[ ! -s "$CLOUDLINK_PASSFILE" ]]; then
  ( umask 077 && openssl rand -hex 16 > "$CLOUDLINK_PASSFILE" )
  chown lexa:lexa "$CLOUDLINK_PASSFILE"
  chmod 600 "$CLOUDLINK_PASSFILE"
fi
cloudlink_pass="$(cat "$CLOUDLINK_PASSFILE")"
if [[ -s "$PASSWD_FILE" ]]; then
  mosquitto_passwd -b "$PASSWD_FILE" lexa-cloudlink "$cloudlink_pass"
else
  mosquitto_passwd -b -c "$PASSWD_FILE" lexa-cloudlink "$cloudlink_pass"
fi
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
# $D/bin/lexa-* already sweeps up lexa-cloudlink/lexa-healthcheck/
# lexa-migrate/lexa-commission-apply (staged under those names above);
# lexactl needs its own line, same reason as the scp step.
install -m 755 $D/bin/lexa-* /usr/local/sbin/
install -m 755 $D/bin/lexactl /usr/local/sbin/lexactl
# The six services' configs are always overwritten from the repo's example
# (mqtt_user/mqtt_pass_file etc. get patched back in immediately below) —
# unchanged behavior. cloudlink.json is handled separately further down:
# no-overwrite, since it can carry operator-set commissioning state this
# script never re-patches.
install -m 644 $D/configs/hub.json $D/configs/northbound.json $D/configs/modbus.json $D/configs/ocpp.json $D/configs/telemetry.json $D/configs/api.json /etc/lexa/
install -m 640 -o lexa -g lexa $D/certs/ca.pem $D/certs/client.pem /etc/lexa/certs/
install -m 600 -o lexa -g lexa $D/certs/client-key.pem /etc/lexa/certs/

# TLS-on-by-default guardrail (bring-up fix, 2026-07-11): configs/api.json now
# ships "tls": true (matching configs/factory/api.json) so a plain deploy
# serves HTTPS — the companion app is HTTPS-only (cert-pinned TLS) and cannot
# reach a plain-http hub at all. This does NOT fail the deploy (an operator
# may deliberately want a one-off plain-http local debug session) — it just
# makes a plain-http deploy impossible to miss in the log. Checked right after
# the config install above so this reflects exactly what just landed on disk,
# before any later step in this script touches api.json again.
if python3 - <<'PY'
import json, sys
with open("/etc/lexa/api.json") as f:
    cfg = json.load(f)
sys.exit(0 if cfg.get("tls") is False else 1)
PY
then
  echo "############################################################"
  echo "# WARNING: /etc/lexa/api.json has \"tls\": false"
  echo "# lexa-api will serve PLAIN HTTP, not HTTPS, once restarted."
  echo "# The companion app is HTTPS-only (TLS cert pinning) and CANNOT"
  echo "# connect to a plain-http hub — this deploy will be unreachable"
  echo "# from the app until tls is set back to true."
  echo "# Fix: set \"tls\": true in configs/api.json and redeploy, or"
  echo "# hand-edit /etc/lexa/api.json's \"tls\" key on this Pi and"
  echo "# restart lexa-api."
  echo "############################################################"
fi

# lexa-cloudlink config (unit 2.1+, now shipping). Install ONLY IF ABSENT —
# mirrors the Makefile's install-configs no-overwrite discipline, but scoped
# to just this one file: unlike the six configs above (blindly overwritten
# every deploy, then immediately re-patched with mqtt creds/listen_addr/etc,
# so nothing meaningful from a prior deploy survives that anyway),
# cloudlink.json carries operator-set commissioning state (enabled,
# endpoint, serial_file, cert paths) this script never re-patches —
# clobbering it on every redeploy would erase a device's cloud enrollment.
if [[ ! -f /etc/lexa/cloudlink.json ]]; then
  install -m 644 $D/configs/cloudlink.json /etc/lexa/cloudlink.json
  echo "  installed /etc/lexa/cloudlink.json (fresh; configs/cloudlink.json template — enabled:false, no cloud identity yet)"
else
  echo "  /etc/lexa/cloudlink.json already present — left untouched (no-overwrite; may carry commissioning state)"
fi

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

# lexa-cloudlink mqtt_user/mqtt_pass_file (TASK-082, unit 1.3; cloudlink.json
# itself now installs, no-overwrite, right above — unit 2.1+). Still guarded
# on the config existing — belt-and-braces, same reasoning as the
# PathExists= fallback in systemd/lexa-commission.path — even though the
# install-if-absent block above now means /etc/lexa/cloudlink.json always
# exists by this point; costs nothing and protects against a future
# reordering. Patches exactly like the loop above patches the six services,
# just with the cloudlink.pass filename set by the credential block earlier
# (matches configs/cloudlink.json's shipped mqtt_pass_file value,
# docs/DEVICE_ROADMAP.md §2.2).
if [[ -f /etc/lexa/cloudlink.json ]]; then
  python3 - <<'PY'
import json
path = "/etc/lexa/cloudlink.json"
with open(path) as f:
    cfg = json.load(f)
cfg["mqtt_user"] = "lexa-cloudlink"
cfg["mqtt_pass_file"] = "/etc/lexa/mqtt/cloudlink.pass"
with open(path, "w") as f:
    json.dump(cfg, f, indent=2)
    f.write("\n")
PY
  echo "  mqtt_user/mqtt_pass_file patched into /etc/lexa/cloudlink.json"
else
  echo "  /etc/lexa/cloudlink.json not present — skipped cloudlink mqtt cred patch (unexpected: install-if-absent should have created it)"
fi

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
# use the distro's, so install only the lexa-* units (this glob also picks
# up lexa-cloudlink.service/lexa-migrate.service/lexa-commission.service,
# all staged above alongside the original six). lexa-commission.path is a
# .path unit, not a .service, so it needs its own line.
install -m 644 $D/systemd/lexa-*.service /etc/systemd/system/
install -m 644 $D/systemd/lexa-commission.path /etc/systemd/system/
systemctl daemon-reload

# journald size/retention budget (TASK-009, review §11 flash wear / RSK-14):
# a conf.d drop-in, not an in-place edit of journald.conf, so this stays
# reproducible from the repo on every deploy. Restarting systemd-journald
# picks up SystemMaxUse/SystemKeepFree/MaxRetentionSec immediately; per-unit
# LogRateLimit* lives in each lexa-*.service above instead.
install -d -m 755 /etc/systemd/journald.conf.d
install -m 644 $D/systemd/journald-lexa.conf /etc/systemd/journald.conf.d/lexa.conf
systemctl restart systemd-journald

# lexa-migrate + lexa-commission.path: enabled (so they take effect on every
# future boot) but never started/restarted here. lexa-migrate runs
# directly, once, right below instead — a `systemctl start` here would just
# duplicate that (harmless — an already-current schema is a no-op — but
# redundant). lexa-commission.path is a passive file watcher a reboot picks
# up on its own; mirrors the Makefile's install-services (enable-only) vs
# start (restart-now) split for these same two units. lexa-commission.service
# is deliberately never enabled — it's trigger-activated by the .path unit's
# PathChanged=/PathExists= only (see its own [Unit] comment).
systemctl enable lexa-migrate lexa-commission.path >/dev/null

# lexa-migrate (unit 1.6): runs ONCE, directly, BEFORE any service below is
# restarted — a config a service is about to reload against must already be
# at the schema version that service's code expects. A refused migration
# (e.g. a config whose schema_version is newer than this binary's registry
# understands — cmd/lexa-migrate/main.go) aborts the WHOLE deploy loudly
# here rather than silently restarting services against a config
# lexa-migrate itself couldn't bring up to date.
echo "── Running lexa-migrate (config schema migration)"
if ! /usr/local/sbin/lexa-migrate -config-dir /etc/lexa; then
  echo "############################################################"
  echo "# lexa-migrate FAILED — aborting deploy BEFORE restarting any"
  echo "# service. See the lexa-migrate output above for which config"
  echo "# file/step was refused; /etc/lexa/*.json.pre-v<N> backups are"
  echo "# the recovery path (cmd/lexa-migrate/README.md)."
  echo "############################################################"
  exit 1
fi

for s in lexa-modbus lexa-ocpp lexa-api lexa-northbound lexa-telemetry lexa-hub lexa-cloudlink; do
  systemctl enable "$s" >/dev/null
  systemctl restart "$s"
done
sleep 3
echo "── Service status:"
for s in mosquitto lexa-modbus lexa-ocpp lexa-api lexa-northbound lexa-telemetry lexa-hub lexa-cloudlink; do
  printf '  %-18s %s\n' "$s" "$(systemctl is-active $s)"
done

# lexa-healthcheck (unit 1.5): advisory-only, run AFTER the restarts above.
# Deploys via this script are operator-attended — unlike Mender's
# ArtifactCommit gate on the SOM, there is no A/B slot to fall back to here
# — so a failure PRINTS LOUDLY but never aborts this (already-finished)
# deploy or undoes anything already restarted. It's a "look before you
# trust it" signal for the operator, not a gate.
echo "── Running lexa-healthcheck -budget 60s (advisory; does not affect deploy result)"
if /usr/local/sbin/lexa-healthcheck -budget 60s; then
  echo "  lexa-healthcheck: PASS"
else
  echo "  ############################################################"
  echo "  # lexa-healthcheck FAILED — see the JSON summary above for"
  echo "  # which check(s) failed. This deploy is NOT rolled back;"
  echo "  # investigate before trusting this hub."
  echo "  ############################################################"
fi

rm -rf $D
REMOTE

echo
echo "── Done. Verify from the desktop (TLS is ON by default now — lexa-api"
echo "   serves HTTPS via its self-signed cert; curl needs -k to skip local"
echo "   verification, same as any client until it does real TOFU pinning):"
if [[ "$BENCH_INSECURE" == "1" ]]; then
  echo "   curl -k https://$PI:9100/status | python3 -m json.tool | head          # auth off (--bench-insecure)"
  echo "   Re-run WITHOUT --bench-insecure once the dashboard/metersim carry the token (scripts/update-sim-pis.sh, bench-up.sh)."
else
  echo "   curl -k -s https://$PI:9100/status                                    # → 401 (auth on, WS-1 default)"
  echo "   curl -k -s -H \"Authorization: Bearer \$(ssh $SSHUSER@$PI sudo cat /etc/lexa/api.token)\" https://$PI:9100/status | python3 -m json.tool | head"
  echo "   curl -k -s https://$PI:9100/healthz                                   # → 200 (never authenticated)"
  echo "   Then restart the dashboard/metersim with -hub-token-file pointing at a copy of that token (see docs/BENCH.md)."
fi
echo "── The companion app REQUIRES this: it is HTTPS-only (cert-pinned) and"
echo "   cannot reach a plain-http hub at all — that's why tls:true is now the"
echo "   default in both configs/api.json and configs/factory/api.json."
echo "   The bench tooling in csip-tls-test was migrated in tandem (WS-B):"
echo "   the dashboard reverse-proxy/drivers/log follower, metersim's -hub-api,"
echo "   prometheus-bench.yml's :9100 scrape, and update-sim-pis.sh all now dial"
echo "   https with skip-verify (self-signed leaf) — so a normal paired"
echo "   hub+sims deploy keeps working. A hand-set \"tls\": false on this Pi is"
echo "   the only path that still needs plain http (the WARNING above flags it)."
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
echo
echo "── lexa-cloudlink / lexa-migrate / lexa-commission:"
echo "   ssh $SSHUSER@$PI sudo systemctl is-active lexa-cloudlink                    # want: active (enabled:false in cloudlink.json ⇒ idle, no cloud session)"
echo "   ssh $SSHUSER@$PI sudo journalctl -u lexa-migrate --no-pager | tail -5       # this deploy's migration run"
echo "   ssh $SSHUSER@$PI sudo systemctl is-enabled lexa-commission.path            # want: enabled (watches /var/lib/lexa/commission/restart.request)"
