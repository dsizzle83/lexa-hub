#!/bin/bash
# TASK-005: pinned govulncheck run + triage-allowlist gate (D7).
#
# Installs a PINNED govulncheck (never @latest — scanner/DB behavior must not
# change under us without a deliberate version bump), scans the whole module
# in JSON mode, and fails only on REACHABLE findings that aren't allowlisted.
#
# "Reachable" here means govulncheck's own top tier: code you actually call.
# govulncheck's JSON finding.trace is NEVER empty (it always carries at least
# one module/version frame for "modules you require" — the weakest tier), so
# a naive "trace non-empty" check would fail the build on every stale
# transitive dependency and drown the signal in the ~2019 x/crypto / ~2020
# x/net noise both repos carry. A finding is only reachable if at least one
# trace frame carries a "function" key (i.e. govulncheck found a real call
# path from this module's code to the vulnerable symbol) — that's the same
# distinction `govulncheck` (no -format) prints as "N vulnerabilities from
# your code" vs "M vulnerabilities in modules you require, but your code
# doesn't appear to call".
#
# Must run with the same cgo env as the repo's wolfSSL CI job (CGO_CFLAGS /
# CGO_LDFLAGS pointing at the wolfSSL sysroot) — without it, cgo packages
# (internal/wolfssl, internal/tlsclient and friends) fail to load and
# govulncheck silently scans a smaller module than you think. This script
# sanity-checks that at least one SBOM module was reported and fails loudly
# if not, rather than passing on a shrunken scan.
#
# Usage: scripts/ci/govulncheck.sh   (run from anywhere; cd's to repo root)
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$HERE/../.." && pwd)"
cd "$REPO_ROOT"

# Pin deliberately; bump only after checking `go list -m golang.org/x/vuln@latest`
# and re-running the triage in docs/refactor/VULN_BASELINE_*.md.
GOVULNCHECK_VERSION="v1.5.0"

ALLOWLIST_FILE="$HERE/vuln-allowlist.txt"

GOBIN_DIR="$(go env GOPATH)/bin"

echo "== installing govulncheck ${GOVULNCHECK_VERSION} (pinned) =="
GOBIN="$GOBIN_DIR" go install "golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}"
GOVULNCHECK="$GOBIN_DIR/govulncheck"

RAW_JSON="$(mktemp)"
trap 'rm -f "$RAW_JSON"' EXIT

echo "== running govulncheck -format json ./... =="
"$GOVULNCHECK" -format json ./... > "$RAW_JSON"

# Guard against a silently-shrunken scan (missing cgo env is the usual cause).
SBOM_MODULES="$(jq -s '[.[] | select(.SBOM) | .SBOM.modules | length] | add // 0' "$RAW_JSON")"
echo "SBOM module count: ${SBOM_MODULES}"
if [ "${SBOM_MODULES}" -lt 1 ]; then
  echo "ERROR: govulncheck reported 0 SBOM modules — cgo packages likely failed" >&2
  echo "        to load. Check CGO_CFLAGS/CGO_LDFLAGS point at the wolfSSL" >&2
  echo "        sysroot (see Makefile / ci.yml wolfSSL cache steps)." >&2
  exit 1
fi

# Unique (osv, module) pairs where at least one trace frame is a real call
# frame (has "function"). Excludes the "imported but uncalled" / "required
# but not imported" tiers, which are informational only.
REACHABLE_TSV="$(
  jq -r '
    select(.finding) | .finding |
    select([.trace[]? | has("function")] | any) |
    [.osv, (.trace[0].module // "unknown")] | @tsv
  ' "$RAW_JSON" | sort -u
)"

# Everything else, for the informational summary only (never gates the build).
NONREACHABLE_COUNT="$(
  jq -r '
    select(.finding) | .finding |
    select([.trace[]? | has("function")] | any | not) |
    .osv
  ' "$RAW_JSON" | sort -u | wc -l
)"

declare -A ALLOW_REASON=()
if [ -f "$ALLOWLIST_FILE" ]; then
  while IFS= read -r line; do
    line="${line%%$'\r'}"
    [[ -z "${line// /}" ]] && continue
    [[ "${line#"${line%%[![:space:]]*}"}" == \#* ]] && continue
    id="${line%%[[:space:]]*}"
    reason="${line#"$id"}"
    reason="${reason#"${reason%%[![:space:]]*}"}"
    ALLOW_REASON["$id"]="$reason"
  done < "$ALLOWLIST_FILE"
fi

FAIL=0
echo
printf '%-15s %-45s %-12s %s\n' "OSV ID" "MODULE" "ALLOWLISTED" "REASON"
printf '%-15s %-45s %-12s %s\n' "---------------" "---------------------------------------------" "------------" "------"

if [ -z "$REACHABLE_TSV" ]; then
  echo "(no reachable findings)"
else
  while IFS=$'\t' read -r osv module; do
    [ -z "$osv" ] && continue
    if [ "${ALLOW_REASON[$osv]+set}" = "set" ]; then
      printf '%-15s %-45s %-12s %s\n' "$osv" "$module" "yes" "${ALLOW_REASON[$osv]}"
    else
      printf '%-15s %-45s %-12s %s\n' "$osv" "$module" "NO" "UNALLOWLISTED — build fails"
      FAIL=1
    fi
  done <<< "$REACHABLE_TSV"
fi

echo
echo "Informational: ${NONREACHABLE_COUNT} additional finding(s) in modules"
echo "required/imported but not reachable (not gated; see VULN_BASELINE doc)."

if [ "$FAIL" -ne 0 ]; then
  echo
  echo "govulncheck: reachable, un-allowlisted finding(s) present (see table above)." >&2
  exit 1
fi

echo
echo "govulncheck: OK (no un-allowlisted reachable findings)."
