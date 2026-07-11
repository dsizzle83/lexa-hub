#!/bin/sh
# scripts/factory-reset.sh — return this LEXA hub to factory/uncommissioned
# state (Unit 1.6, docs/DEVICE_ROADMAP.md §9). POSIX sh, no bashisms: this
# runs on the Digi i.MX93 Yocto (DEY) image, and DEVKIT.md documents this
# repo's actual bring-up needing a sudo SHIM and a hand-built mosquitto on a
# fresh image — there is no guarantee bash (or GNU coreutils) is present on
# a minimal factory rootfs slot, even though today's dev-kit bring-up
# happens to have both.
#
# What this does, IN ORDER:
#   1. stop the seven lexa-* services (best-effort per service — a
#      not-yet-installed unit, e.g. lexa-cloudlink before TASK-085 lands,
#      is logged and skipped, never fatal)
#   2. remove every /etc/lexa/*.json and replace them with the baked
#      factory profiles from /usr/share/lexa/factory/ (see
#      configs/factory/README.md for exactly what ships in each one, and
#      that same file's "Install path" section for what meta-lexa still
#      needs to do to populate that directory)
#   3. wipe /var/lib/lexa/{journal,spool,snapshot,api} (whole directories —
#      each service's own Open()/MkdirAll recreates the one it needs on its
#      next write; see internal/journal's JournalConfig doc) and
#      /var/lib/lexa/site.json
#   4. wipe /etc/lexa/certs/* (contents; the directory itself survives, so
#      systemd ReadOnlyPaths=/etc/lexa/certs on lexa-northbound/telemetry
#      keeps pointing at something that exists)
#   5. remove /etc/lexa/commissioned (the uncommissioned-mode marker) AND
#      clear /run/lexa/provision-window (any open BLE re-provision window),
#      returning the unit to provisioning-ready state — lexa-provision will
#      advertise again (ADR-0002 unit B4)
#   6. restart the seven services against the now-factory config
#
# /etc/lexa/identity/ AND /etc/lexa/provision-pop are never touched by ANY
# step above — both are preserved by omission, not by a special-cased
# "restore" step (step 5 makes the provision-pop preservation explicit with a
# log line, the way the identity note below does). See the certs step for why
# identity survives a reset but the CSIP cert does not, and step 5 for why the
# per-unit manufacturing PoP is treated like identity: it is label-bound (the
# LEXA:v1 QR printed on the device carries this exact code), so a reset that
# invalidated it would brick the printed label and make the unit
# un-recommissionable from its own sticker.
#
# Refuses to run without --yes (this is destructive and irreversible for
# everything except identity/). Every action is logged via `logger` (to the
# journal, same channel every other lexa-* operational script uses) so a
# factory reset always leaves a forensic trail — this is a "wipe the
# device" button, it should never run invisibly.
#
# KNOWN GAP (full writeup in configs/factory/README.md's "Known gaps"):
# lexa-northbound and lexa-telemetry both load their CA/client cert/key
# files EAGERLY at process start, independent of whether "server" is
# configured. Step 4 below wiping /etc/lexa/certs/* means step 6's restart
# of those two services will very likely crash-loop until systemd's
# StartLimitBurst trips them to "failed" (the V1RC FINDING A signature),
# UNTIL commissioning provisions new certs AND something runs
# `systemctl reset-failed lexa-northbound lexa-telemetry` afterward. This
# script does not paper over that with baked/generated placeholder certs
# (no guaranteed openssl on this image, and an identical placeholder cert
# baked into every unit would be its own smell) — it is called out here
# loudly instead, exactly like the rest of this codebase prefers a loud
# known gap over a silent papered-over one.
#
# Also NOT done here, and worth knowing: this script does not rotate
# /etc/lexa/api-secret. A factory reset for resale/relocation leaves the
# PREVIOUS owner's bearer token valid against the reset device unless
# something else regenerates that file — it is not identity, and it is not
# in DEVICE_ROADMAP.md §9's wipe list, so it is left alone here. Flagged as
# an open question for whoever owns the resale/relocation story.

set -eu

# Paths are env-overridable (defaults are the real device paths, so a normal
# invocation is unchanged). This lets the B4 test-suite point the whole reset at
# a temp sandbox and assert on its effects WITHOUT wiping the real /etc/lexa.
# LEXA_SKIP_SERVICES=1 skips the systemctl stop/start steps for the same reason
# (a test box must not stop live units); it is never set on a real device.
FACTORY_DIR="${LEXA_FACTORY_DIR:-/usr/share/lexa/factory}"
ETC_LEXA="${LEXA_ETC:-/etc/lexa}"
VAR_LEXA="${LEXA_VAR:-/var/lib/lexa}"
RUN_LEXA="${LEXA_RUN:-/run/lexa}"
SKIP_SERVICES="${LEXA_SKIP_SERVICES:-}"
SERVICES="lexa-hub lexa-northbound lexa-modbus lexa-ocpp lexa-telemetry lexa-api lexa-cloudlink"

log() {
  # $1 = message. logger may not exist on every minimal image; never let a
  # missing/failing logger abort a destructive script that's already
  # committed to running.
  logger -t lexa-factory-reset -- "$1" 2>/dev/null || true
  echo "lexa-factory-reset: $1"
}

YES=""
for arg in "$@"; do
  case "$arg" in
    --yes) YES="1" ;;
  esac
done

if [ -z "$YES" ]; then
  echo "factory-reset.sh: refusing to run without --yes" >&2
  echo "  this wipes /etc/lexa/*.json, /etc/lexa/certs/*, and all of /var/lib/lexa" >&2
  echo "  (journal, spool, snapshot, api cert, site.json). Only /etc/lexa/identity/" >&2
  echo "  and /etc/lexa/provision-pop survive. Re-run as: $0 --yes" >&2
  exit 1
fi

log "factory reset starting (--yes acknowledged)"

if [ -n "$SKIP_SERVICES" ]; then
  log "step 1/6: skipping service stop (LEXA_SKIP_SERVICES set)"
else
  log "step 1/6: stopping services: $SERVICES"
  for svc in $SERVICES; do
    if timeout 15 systemctl stop "$svc" >/dev/null 2>&1; then
      log "stopped $svc"
    else
      log "WARN: could not stop $svc (not installed, already stopped, or systemctl unresponsive) -- continuing"
    fi
  done
fi

log "step 2/6: removing existing /etc/lexa/*.json"
for f in "$ETC_LEXA"/*.json; do
  if [ -e "$f" ]; then
    rm -f -- "$f"
    log "removed $f"
  fi
done

if [ -d "$FACTORY_DIR" ]; then
  installed=0
  for f in "$FACTORY_DIR"/*.json; do
    if [ -e "$f" ]; then
      dest="$ETC_LEXA/$(basename "$f")"
      cp -- "$f" "$dest"
      chmod 640 "$dest"
      log "installed $dest (from $f)"
      installed=$((installed + 1))
    fi
  done
  if [ "$installed" -eq 0 ]; then
    log "WARN: $FACTORY_DIR exists but contains no *.json files -- /etc/lexa/*.json left EMPTY"
  fi
else
  log "WARN: $FACTORY_DIR not present -- /etc/lexa/*.json left EMPTY. meta-lexa must populate this directory at image build time (see configs/factory/README.md 'Install path')"
fi

log "step 3/6: wiping /var/lib/lexa state (journal, spool, snapshot, api, site.json)"
for d in journal spool snapshot api; do
  target="$VAR_LEXA/$d"
  if [ -d "$target" ]; then
    rm -rf -- "$target"
    log "removed $target (each service recreates its own subdirectory on next write)"
  fi
done
if [ -e "$VAR_LEXA/site.json" ]; then
  rm -f -- "$VAR_LEXA/site.json"
  log "removed $VAR_LEXA/site.json"
fi

log "step 4/6: wiping /etc/lexa/certs/* -- the CSIP LFDI mTLS cert is a PER-SITE enrollment artifact, not device identity (this codebase's LFDI hashes the FULL DER cert -- CLAUDE.md's cert-rotation section -- so even an identical CN/key reissued at a new site produces a different LFDI). A factory reset un-enrolls the device from its current site, so this MUST go; re-enrollment (commissioning) provisions a fresh one. See this script's header for the known startup-crash-loop consequence."
if [ -d "$ETC_LEXA/certs" ]; then
  rm -rf -- "${ETC_LEXA:?}/certs"
  mkdir -p -- "$ETC_LEXA/certs"
  chmod 750 "$ETC_LEXA/certs"
  log "cleared $ETC_LEXA/certs"
fi

log "step 5/6: restoring provisioning-ready state (remove commissioned marker + clear re-provision window)"
rm -f -- "$ETC_LEXA/commissioned"
log "removed $ETC_LEXA/commissioned marker"
# Clear any open BLE re-provision window so advertising state is governed
# purely by the (now absent) commissioned marker, not a stale window file left
# over from before the reset (ADR-0002 unit B4). Missing file is fine.
rm -f -- "$RUN_LEXA/provision-window"
log "cleared $RUN_LEXA/provision-window (re-provision window, if any)"

if [ -d "$ETC_LEXA/identity" ]; then
  log "preserved $ETC_LEXA/identity/ untouched (device identity survives every reset)"
else
  log "note: $ETC_LEXA/identity did not exist -- nothing to preserve"
fi

# Preserve the per-unit manufacturing PoP (ADR-0002 unit B4). It is treated
# EXACTLY like identity: it is provisioned once at manufacturing and printed
# into the device's LEXA:v1 label QR (LEXA:v1;serial=...;pop=<this value>), so
# it must survive a factory reset or the printed label would no longer
# commission the unit. Like identity/, it is preserved by OMISSION (no step
# above touches /etc/lexa/provision-pop — step 2 wipes only *.json), and this
# log line makes that preservation explicit + auditable.
if [ -f "$ETC_LEXA/provision-pop" ]; then
  log "preserved $ETC_LEXA/provision-pop untouched (per-unit manufacturing PoP, label-bound -- survives every reset like identity)"
else
  log "note: $ETC_LEXA/provision-pop did not exist -- nothing to preserve (dev-kit default PoP in effect)"
fi

if [ -n "$SKIP_SERVICES" ]; then
  log "step 6/6: skipping service restart (LEXA_SKIP_SERVICES set)"
else
  log "step 6/6: restarting services: $SERVICES"
  for svc in $SERVICES; do
    if timeout 15 systemctl start "$svc" >/dev/null 2>&1; then
      log "started $svc"
    else
      log "WARN: could not start $svc (not installed, failed to start, or systemctl unresponsive -- check: systemctl status $svc)"
    fi
  done
fi

log "factory reset complete"
