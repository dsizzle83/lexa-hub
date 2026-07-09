# Mender state scripts — OTA commit/rollback integration

This directory is the **payload half** of `lexa-hub`'s OTA gate
(DEVICE_ROADMAP.md §8.1, TASK-098 / Unit 1.5). The other half — Mender
itself, meta-mender's U-Boot/imx-boot A/B integration for the i.MX93, and
the actual bitbake recipe that installs these two files onto the image —
lives in the `meta-lexa` Yocto layer (`~/projects/meta-lexa`,
`~/workspace/ccimx93-dvk`, see `DEVKIT.md`), **not in this repo**. Nothing
here talks to Mender's client, the deployment server, or the bootloader
directly; these are plain POSIX shell scripts that Mender's client invokes
at fixed points in the update lifecycle and whose exit code it acts on.

## The two scripts

| Script | Mender state | When it runs | Job |
|---|---|---|---|
| `ArtifactInstall_Leave_00_note` | `ArtifactInstall_Leave` | Right after the new artifact is written to the **inactive** A/B slot, still before that slot is ever booted | Journal breadcrumb only — no health logic |
| `ArtifactCommit_Enter_00_lexa-health` | `ArtifactCommit_Enter` | After the new slot has **booted and become active**, before Mender commits it as the permanent default | Runs `lexa-healthcheck -commit -budget 120s` (`cmd/healthcheck` in this repo) and lets its exit code decide commit vs. rollback |

Mender's state-script naming convention is
`<State>_<Action>_<NNNN>_<name>` — the `00` here just means "first (and
only) script in this state" for this layer; a future layer could add
`ArtifactCommit_Enter_01_something-else` alongside it without conflict.

**The commit/rollback contract lives entirely in
`ArtifactCommit_Enter_00_lexa-health`'s exit code**: non-zero → Mender
does not commit → the bootloader falls back to the previous slot on the
next boot automatically (no separate rollback step, no operator action).
Zero → the new slot becomes permanent. See that script's header comment
for the full reasoning; see `cmd/healthcheck/main.go`'s package doc and
`docs/HEALTHCHECK.md` for what each of the seven checks verifies.

## Install path (meta-lexa's job, not this repo's)

Mender supports two ways to ship state scripts; either is legitimate for
`meta-lexa` to choose, and neither requires changing anything in this
directory:

1. **Rootfs-embedded** (typical for a Yocto/BSP-integrated layer like this
   one): a bitbake recipe in `meta-lexa` installs these two files straight
   into the rootfs image at `/etc/mender/scripts/` (the directory the
   Mender client scans on every state transition). This is the simpler
   option when the scripts change at the same cadence as the rest of the
   `dey-image-lexa` image, which is the case here.
2. **Artifact-embedded**: the scripts travel *inside* the `.mender`
   artifact itself (under `scripts/` in the artifact's payload), installed
   by `mender-artifact` at build time via `--script`. This lets the
   scripts version alongside a specific release rather than the base
   image — worth it later if `lexa-healthcheck`'s gate logic needs to
   evolve independently of image rebuilds, but not needed for the current
   one-image-one-gate setup.

Either way, **the file must be world-readable, owned appropriately, and
executable** (`chmod +x`) — Mender refuses to run a state script that
isn't. `git` does not preserve the execute bit reliably across all clone
methods, so `meta-lexa`'s recipe (or the artifact-embedding step) must set
it explicitly rather than assuming the bit survived from this repo.

## i.MX93 A/B note

The actual A/B slot mechanics — U-Boot/imx-boot environment variables,
`mender-client`'s partition bookkeeping, the boot-count/upgrade-available
dance that decides which slot U-Boot hands off to — are entirely
`meta-mender` + `meta-lexa`'s integration surface for the ConnectCore i.MX93
(DVK today; the production SOM is the same family). None of it is
reproducible or testable from this repo: `lexa-healthcheck` and these two
scripts are deliberately bootloader-agnostic — they only assume "Mender
will run ArtifactCommit_Enter after boot and roll back on non-zero exit",
which is true regardless of which A/B mechanism underlies it.

## Testing without a real Mender client

Both scripts are plain, side-effect-light shell (the commit gate's only
side effect beyond logging is running `lexa-healthcheck`, which itself
does no writes — every check in cmd/healthcheck is a read/probe). To dry
run the actual gate on a booted device or in a bench chroot:

```sh
sh -n scripts/mender/ArtifactCommit_Enter_00_lexa-health   # syntax check only
LEXA_HEALTHCHECK=./bin/arm64/lexa-healthcheck \
LEXA_HEALTHCHECK_BUDGET=10s \
  sh scripts/mender/ArtifactCommit_Enter_00_lexa-health; echo "exit=$?"
```

The `ota-broken-service-rollback` Mayhem scenario (bench-deferred, see
`docs/extension/00_PROGRESS.md`'s validation queue) is the actual
end-to-end proof: deliberately break one lexa-* unit on a freshly-booted
slot and confirm Mender rolls back via this exact path.

## POSIX sh discipline

Both scripts are `#!/bin/sh`, not `#!/bin/bash`: DEY's Yocto image is
BusyBox-adjacent (see `DEVKIT.md`'s "no sudo, no mosquitto_passwd on
Yocto" gotchas for the general shape of what's missing) and Mender's own
client environment does not guarantee bash is present or is `/bin/sh`.
Neither script uses arrays, `[[ ]]`, `local`, process substitution, or any
other bashism — only `${VAR:-default}` parameter expansion, `[ ]` test,
and straight-line command execution, all POSIX-portable. Verify with
`sh -n <script>` (also run in CI-adjacent local verification — see the
task's VERIFY step) before changing either file.
