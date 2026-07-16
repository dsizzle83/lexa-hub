# LEXA Hub — Dev Kit Runbook (Digi ConnectCore 93 DVK)

How to (re)deploy and run the LEXA hub on the Digi ConnectCore 93 Development Kit,
and how to recover the network configuration after a reboot or reflash.

- **Device:** Digi International ConnectCore 93 DVK (i.MX 93, ARM64)
- **Hostname:** `ccimx93-dvk`
- **OS:** Digi Embedded Yocto 5.0-r3 (scarthgap), glibc 2.39, kernel 6.6.52-dey
- **Login:** `ssh root@69.0.0.2` (no password configured for root over the demo LAN);
  also on WiFi via DHCP (resolves as `ccimx93-dvk.local`)

## Custom DEY image build (2026-07-07 — flash pending)

A custom headless DEY 5.0 image + kernel for this board is built and verified
on the desktop (BT provisioning-ready, netem enabled, no display/camera/audio,
eth0 69.0.0.2 static baked in, mosquitto/tc/sudo/python3/bash/tzdata included
— once flashed, the sudo shim, hand-built mosquitto, and NM profile below
become unnecessary):

- Yocto layer (git): `~/projects/meta-lexa` · project: `~/workspace/ccimx93-dvk`
  · DEY install: `~/dey/dey-5.0` (scarthgap; pinned snapshot
  `pinned-manifest-20260707.xml`)
- Rebuild: `cd ~/workspace/ccimx93-dvk && source ./dey-setup-environment &&
  bitbake dey-image-lexa` → `tmp/deploy/images/ccimx93-dvk/`
- Host gotchas (Ubuntu 24.04): `kernel.apparmor_restrict_unprivileged_userns=0`
  required (`/etc/sysctl.d/60-yocto-userns.conf`); `hg` removed from bitbake
  HOSTTOOLS (setuptools-scm probe times out under load); Mali GPU + display-PHY
  kernel symbols must stay disabled with DRM off (build breaks otherwise);
  `ETH0_STATIC_GATEWAY` has a baked Digi default — meta-lexa ships a
  gateway-free `nm.eth0.static` with never-default=true instead.

## 2026-07-07: dev kit is the live hub again (fresh image)

The board came back with a **fresh Yocto image** (nothing from the previous
install survived) and the hub was migrated onto it from hub-pi 69.0.0.1, whose
lexa services are now stopped+disabled (standby). Differences from the old
install, all live now:

- **`/usr/bin/sudo` is a shim** (there is no sudo on Yocto): it strips sudo's
  flags and execs the command as root. This is what lets unmodified bench
  scripts and the Mayhem engine (`sudo -n true` probes, `sudo bash -s`,
  `sudo date -s`, `sudo systemctl restart …`) work against `root@69.0.0.2`.
  The dashboard is started with `LEXA_SSH_USER=root` (csip-tls-test
  `bench-up.sh`).
- **mosquitto 2.0.20** cross-built no-TLS (`WITH_TLS=no WITH_CJSON=no
  WITH_LIB_CPP=no`, aarch64-linux-gnu-gcc) at `/usr/local/sbin/mosquitto`,
  running with `allow_anonymous true` and **no passwd/ACL** (no
  mosquitto_passwd on Yocto; services still send credentials, broker accepts
  them). `log_dest stderr` → journal, persistence in `/var/lib/mosquitto/`.
  **`user root` is REQUIRED in mosquitto.conf**: without it the broker tries
  to drop privileges to the nonexistent `mosquitto` user, falls back to
  `nobody`, and every persistence save fails with Permission denied — no
  `mosquitto.db` is ever written and the Mayhem `power-cut-retained-rollback`
  broker-snapshot scenario goes INCONCLUSIVE (found+fixed 2026-07-07).
  **`mosquitto_sub`/`mosquitto_pub` must be installed at `/usr/bin`** (same
  cross-build, `make -C client` with `WITH_SHARED_LIBRARIES=no
  WITH_STATIC_LIBRARIES=yes`): the same Mayhem scenario verifies the retained
  control by running `mosquitto_sub` ON the hub over SSH — the Pi got these
  from the distro mosquitto-clients package, Yocto has nothing.
- **mqttproxy** (QA fault proxy) installed and enabled: localhost:1882 →
  broker :1883, control API on `69.0.0.2:11882`; all `/etc/lexa/*.json` point
  `mqtt_broker` at `tcp://localhost:1882` (copied from the Pi's live configs,
  `metrics_addr` rewritten to 69.0.0.2).
- **netem gap:** the DEY kernel has `CONFIG_NET_SCH_NETEM` unset and ships no
  `tc` — the three `netem-*` Mayhem scenarios are INCONCLUSIVE against this
  hub until the kernel is rebuilt with netem + iproute2-tc is added.
- Binaries deployed are the exact `bin/arm64/` set the Pi was running
  (byte-identical, 2026-07-06 build). Units are the repo ones with
  `User=lexa`/`Group=lexa` stripped (services run as root — see gotcha below).
- Serial console **input works on this board** (see updated section below).

---

## Network topology (flat 69.0.0.x/24 wired LAN)

| Host | IP | Role |
|---|---|---|
| Desktop (dev machine) | `69.0.0.20` (enp1s0) | builds binaries, runs gridsim + web dashboard |
| **Dev kit (this device)** | `69.0.0.2` (eth0) | runs the LEXA hub services |
| solar-pi | `69.0.0.10` | modsim (Modbus 5020, simapi 6020) |
| battery-pi | `69.0.0.11` | batsim (Modbus 5021, simapi 6021) |
| meter-pi | `69.0.0.12` | metersim (Modbus 5022, simapi 6022) |
| ev-pi | `69.0.0.14` | evsim (OCPP → dev-kit :8887, simapi 6024) |

The dev kit has **two** active interfaces:

- **eth0** — static `69.0.0.2/24`, **no gateway**. Carries all hub traffic on the
  demo LAN (Modbus to the Pis, mTLS to gridsim at 69.0.0.20, OCPP from ev-pi).
- **wlan0** — DHCP (e.g. `192.168.0.x`). Provides the **default route / internet**
  (NTP, package pulls). Leave it on DHCP; its subnet won't collide with 69.0.0.x.

> The default route intentionally lives on wlan0. eth0 has no gateway because the
> 69.0.0.x LAN is isolated and self-contained.

---

## Static IP configuration (the important part)

The static IP is managed by **NetworkManager** as a saved connection profile named
`eth0-static`. It is **persistent across reboot** — the profile is stored on the
root filesystem at:

```
/etc/NetworkManager/system-connections/eth0-static.nmconnection
```

### Reboot safety: the boot race we fixed

The factory image ships a stock `eth0` profile set to **DHCP** with `autoconnect=yes`.
Our `eth0-static` profile also has `autoconnect=yes`. With both at the same
autoconnect-priority, NetworkManager picks one **non-deterministically** at boot —
so the dev kit could come up on DHCP and **lose 69.0.0.2**.

This was fixed by making the static profile win deterministically:

```bash
nmcli con modify eth0-static connection.autoconnect-priority 100
nmcli con modify eth0        connection.autoconnect no
```

These settings are saved to disk and **persist across reboot**. If you ever see the
dev kit come up on the wrong address, re-run those two commands.

### Verify after a reboot

```bash
ip -br addr show eth0
# expect: eth0  UP  69.0.0.2/24 ...

nmcli -f connection.id,connection.autoconnect,connection.autoconnect-priority \
      con show eth0-static
# expect: eth0-static / yes / 100
```

### Recreate the static IP from scratch (after a reflash / factory reset)

If the `eth0-static` profile is gone (new SD card, reflashed image), recreate it:

```bash
# Create the static profile on eth0 (no gateway — internet comes via wlan0)
nmcli con add type ethernet con-name eth0-static ifname eth0 \
    ipv4.method manual ipv4.addresses 69.0.0.2/24

# Make it win on boot, and stop the stock DHCP profile from competing
nmcli con modify eth0-static connection.autoconnect yes \
    connection.autoconnect-priority 100
nmcli con modify eth0 connection.autoconnect no

# Bring it up now
nmcli con up eth0-static
```

> Do NOT add `ipv4.gateway` on eth0. The default route must stay on wlan0.

If you run this while connected over the eth0 SSH session, `nmcli con up` briefly
re-applies the interface — your session should survive since the address is
unchanged, but if it drops, reconnect to `69.0.0.2`.

---

## What lives on the dev kit (and persists across reboot)

Everything below is on the root filesystem (`/dev/root`, persistent) and the
systemd units are enabled, so **a plain reboot brings the whole hub back
automatically** — no redeploy needed.

| Path | Contents |
|---|---|
| `/usr/local/sbin/mosquitto` | MQTT broker (cross-built, no-TLS) |
| `/usr/local/sbin/lexa-{hub,modbus,ocpp,api,northbound,telemetry}` | the six hub services |
| `/etc/lexa/{hub,modbus,ocpp,api,northbound,telemetry}.json` | service configs |
| `/etc/lexa/certs/{ca.pem,client.pem,client-key.pem}` | mTLS certs for northbound/telemetry |
| `/etc/mosquitto/mosquitto.conf` | broker config (listener 1883 localhost) |
| `/var/lib/mosquitto/` | retained-message persistence |
| `/etc/systemd/system/{mosquitto,lexa-*}.service` | unit files (all enabled) |

Services and their dependency order: `mosquitto` → `lexa-modbus`, `lexa-ocpp`,
`lexa-api`, `lexa-northbound`, `lexa-telemetry` → `lexa-hub`.

### Enabling OCPP Security Profile 2 (wss:// + Basic Auth)

By default `lexa-ocpp` listens on plain `ws://:8887` with no auth (dev only).
To enable TLS + Basic Auth on the EV charger link:

1. On the desktop, in `csip-tls-test/`, issue the CSMS cert (signed by the
   demo CA, SAN includes the dev kit's LAN IP):
   `bash scripts/gen-ev-cert.sh 69.0.0.2`
2. Copy `certs/ev-server-cert.pem` and `certs/vault/ev-server-key.pem` to the
   dev kit as `/etc/lexa/certs/ev-server-{cert,key}.pem` (key mode 600).
3. In `/etc/lexa/ocpp.json` set `"cert_path"`, `"key_path"`, and
   `"basic_auth_user"` / `"basic_auth_pass"`, then
   `systemctl restart lexa-ocpp`.
4. Point evsim at the secure endpoint (on ev-pi):
   `evsim -csms wss://69.0.0.2:8887/ocpp -tls-ca certs/ca-cert.pem \
          -auth-user <user> -auth-pass <pass>`

---

## Service operations

```bash
ssh root@69.0.0.2

# Status of everything
for s in mosquitto lexa-modbus lexa-hub lexa-ocpp lexa-api lexa-northbound lexa-telemetry; do
  printf "%-18s %s\n" "$s" "$(systemctl is-active $s)"
done

# Start / stop the whole stack
systemctl start  mosquitto lexa-modbus lexa-ocpp lexa-api lexa-northbound lexa-telemetry lexa-hub
systemctl stop   lexa-hub lexa-telemetry lexa-northbound lexa-api lexa-ocpp lexa-modbus

# Logs
journalctl -f -u lexa-hub -u lexa-modbus -u lexa-northbound

# Confirm the API answers (from the desktop or the device)
# lexa-api serves HTTPS on :9100 with a self-signed leaf (configs/api.json tls:true) — use -k
curl -sk https://69.0.0.2:9100/status
```

---

## Build & deploy from the desktop

Binaries are cross-compiled on the desktop (`69.0.0.20`) and pushed over SCP.
The four pure-Go services need only the Go toolchain; `lexa-northbound` and
`lexa-telemetry` are CGo + wolfSSL.

### One-time desktop setup

```bash
sudo snap install go --classic                                   # Go toolchain
sudo apt-get install -y gcc-aarch64-linux-gnu cmake automake autoconf libtool
cd ~/projects/lexa-hub
make wolfssl-arm64        # cross-builds wolfSSL into /tmp/wolfssl-arm64-sysroot
```

> `make wolfssl-arm64` installs into `/tmp`, which is wiped on **desktop** reboot.
> Re-run it after a desktop reboot, or set a persistent `WOLFSSL_SYSROOT` (see Makefile).
> This does **not** affect the dev kit — the wolfSSL library is statically linked
> into the deployed binaries.

### Build all six ARM64 binaries

```bash
cd ~/projects/lexa-hub
make build-arm64          # → bin/arm64/lexa-*
```

### Deploy to the dev kit

```bash
cd ~/projects/lexa-hub

# Binaries
scp bin/arm64/lexa-* root@69.0.0.2:/usr/local/sbin/

# Configs (edit these on the device if they already exist — scp overwrites)
scp configs/*.json root@69.0.0.2:/etc/lexa/

# Certs (note the rename to match the config paths)
scp ~/projects/csip-tls-test/certs/ca-cert.pem                    root@69.0.0.2:/etc/lexa/certs/ca.pem
scp ~/projects/csip-tls-test/certs/client-staging/client-cert.pem root@69.0.0.2:/etc/lexa/certs/client.pem
scp ~/projects/csip-tls-test/certs/client-staging/client-key.pem  root@69.0.0.2:/etc/lexa/certs/client-key.pem

# Systemd units
scp systemd/*.service root@69.0.0.2:/etc/systemd/system/

# Apply
ssh root@69.0.0.2 'systemctl daemon-reload && systemctl restart \
    mosquitto lexa-modbus lexa-ocpp lexa-api lexa-northbound lexa-telemetry lexa-hub'
```

### Gotcha: systemd units reference a `lexa` user that does not exist on Yocto

The shipped unit files contain `User=lexa` / `Group=lexa`. The Digi Yocto image
has no `lexa` user, so the services fail to start until those lines are removed
(they then run as root). After copying units to a fresh device:

```bash
ssh root@69.0.0.2 'sed -i "/^User=lexa/d; /^Group=lexa/d" /etc/systemd/system/lexa-*.service \
    && chmod 600 /etc/lexa/certs/client-key.pem \
    && systemctl daemon-reload'
```

The unit files also use `StartLimitIntervalSec` in `[Service]`; on this systemd
version that key belongs in `[Unit]` and is harmlessly ignored where it is — not
a problem, just a log warning.

---

## Reboot recovery checklist

After rebooting the **dev kit**:

1. `ip -br addr show eth0` → `69.0.0.2/24` (static IP persisted)
2. `for s in mosquitto lexa-hub lexa-modbus lexa-ocpp lexa-api lexa-northbound lexa-telemetry; do systemctl is-active $s; done` → all `active`
3. `curl -sk https://69.0.0.2:9100/status` → live device readings (HTTPS, self-signed leaf)
4. Northbound mTLS: `journalctl -u lexa-northbound -n 20` → `discovery OK`

After rebooting the **desktop** (gridsim/dashboard host), nothing on the dev kit
is affected, but to bring the upstream side back:

1. Re-run `make wolfssl-arm64` only if you need to rebuild (the `/tmp` sysroot is gone)
2. Restart gridsim and the dashboard — see `~/projects/csip-tls-test/sim_gridsim.txt`
   and `sim_dashboard.txt`

---

## Serial console

**Update 2026-07-07:** on the current (reflashed/replaced) board, serial input
**works** — commands typed/written to `/dev/ttyACM0` at 115200 reach a root
shell and execute (verified with a marker command from the desktop). The
diagnosis below is kept as history for the earlier board/bridge and for the
fastboot-without-console procedure, which remains useful. ModemManager on the
host is still a nuisance — keep it disabled when using the console.

### Historical fault (earlier board): input dead (host→board), output works

**Symptom:** over the USB serial console (`/dev/ttyACM0`, picocom `-b 115200`) you
see all boot output and the Linux login prompt, but **nothing you type reaches the
board** — no echo, and you cannot interrupt U-Boot autoboot.

**This is a hardware-level fault in the DVK's onboard USB-serial console bridge,
not a software/picocom/terminal problem.** How it was proven (so nobody re-chases
the wrong things):

- The console bridge is a **Cypress USB-Serial `04b4:0002`** (single channel).
- Board→host works (you see output); the USB link is fine both directions.
- The board's console UART is `ttyLP5` (`/proc/tty/driver/fsl-lpuart`, port 5,
  `mmio 0x425A0010`). Its receive counter is **`rx:0`** and never moves.
- Writing bytes straight to `/dev/ttyACM0` from the host (bypassing picocom) — in
  **every** DTR/RTS combination — leaves `ttyLP5` `rx` at exactly 0.
- The board side is correctly configured to receive: `stty -F /dev/ttyLP5 -a` shows
  `cread` (receiver on), `-crtscts -ixon -ixoff` (no flow control); pinmux shows
  both pads claimed by `425a0000.serial`. So bytes simply never arrive at the SoC
  RX pin.
- Host side is clean: `ModemManager` stopped, nothing else holding the port.

Conclusion: the bridge's transmit-toward-SoC path (or its trace to UART5 RX) is
dead. Output-only console. Not fixable from software on host or board.

### Things that are NOT the cause (already ruled out)
picocom flags, terminal/line-discipline state, `SIGKILL` leftovers, hardware/software
flow control, DTR/RTS gating, ModemManager (it *was* probing the port — a real
nuisance, keep it disabled — but stopping it did not restore input), board UART
config, and pin muxing.

### How to get a working serial console again
1. **External USB-TTL adapter** (FTDI / CP2102 / CH340) wired directly to the
   console UART header on the DVK (TX↔RX, RX↔TX, GND↔GND), bypassing the dead
   Cypress bridge. This is the reliable fix.
2. Sanity-check on a **different host PC** to confirm the bridge (not this PC) is
   at fault — expected result: still output-only.
3. Keep `ModemManager` disabled on any Linux host used for the console:
   `sudo systemctl disable --now ModemManager` (it hijacks USB-serial devices).

---

## Getting into fastboot / U-Boot WITHOUT the serial console

Because the console can't accept keystrokes, you can't tap a key to stop U-Boot
autoboot. Instead, a **one-shot fastboot trigger** has been installed in the U-Boot
environment (settable from Linux over SSH — no console needed). It is **armed and
verified safe** (a normal-boot reboot test passed).

### How it works
`bootcmd` was wrapped so it enters fastboot only when a flag is set, then clears
the flag before launching fastboot (so a power-cycle ALWAYS returns to normal Linux
— it can never strand the board):

```
bootcmd = if test "${enter_fastboot}" = "1"; then \
              setenv enter_fastboot 0; saveenv; \
              echo ">>> LEXA one-shot: entering fastboot"; \
              run bootcmd_mfg;            # = fastboot auto  (Digi's built-in)
          fi; \
          run bsp_bootcmd                 # = normal boot (original bootcmd)
```
Backup of the original is in `bootcmd_orig_lexa` (= `run bsp_bootcmd`).

### To enter fastboot
1. **Connect a USB cable from the host PC to the board's fastboot/UDC port** —
   that is i.MX93 **USB1** (`4c100000.usb` / `ci_hdrc.0`). This is REQUIRED: that
   port currently reads `state=not attached`, and `fastboot auto` blocks waiting
   for a host, so without the cable the board just sits in fastboot until you
   power-cycle it (which then returns it to normal Linux).
2. Trigger it over SSH:
   ```
   ssh root@69.0.0.2 'fw_setenv enter_fastboot 1 && reboot'
   ```
3. On the host, talk to it (fastboot is installed at `~/.local/platform-tools/`):
   ```
   ~/.local/platform-tools/fastboot devices
   ~/.local/platform-tools/fastboot getvar product
   ```
4. When done, return to normal Linux:
   ```
   ~/.local/platform-tools/fastboot reboot        # or just power-cycle
   ```

`fastboot_dev=mmc0`, so `fastboot flash <partition> <image>` targets the eMMC.

### To remove the one-shot wrapper entirely
```
ssh root@69.0.0.2 'fw_setenv bootcmd "run bsp_bootcmd"'
```

### If U-Boot itself ever needs recovery (env hosed, won't boot)
Use NXP's **UUU (mfgtools)** over the i.MX93 boot-ROM serial-download USB — it does
not use the console UART or a working U-Boot at all. Set the DVK boot switch to
serial-download mode and run `uuu`. This is the ultimate fallback.

---

## Related docs

- `~/projects/csip-tls-test/sim_gridsim.txt` — IEEE 2030.5 gridsim server on the desktop
- `~/projects/csip-tls-test/sim_dashboard.txt` — web dashboard (`cmd/dashboard`)
- `~/projects/csip-tls-test/sim_{solar,battery,meter,ev}.txt` — per-Pi simulator setup
- `CLAUDE.md` — architecture, MQTT topic map, build basics

## Deploy gotchas found 2026-07-15 (bench round 2)

- **Stop services before scp'ing binaries**: writing over a running binary fails
  with `ETXTBSY` — `systemctl stop 'lexa-*'` on the kit first, then copy, then start.
- **ocpp.json must be patched after a manual copy**: the repo example ships SP2
  placeholder values (cert/auth paths that are not real secrets). The Pi deploy
  script patches `bench:true` + blanks the SP2 fields on a plain deploy; the manual
  dev-kit procedure must do the same by hand or lexa-ocpp starts in a silently
  broken SP2 mode. Same for api.json if desktop access is needed: `bench:true`
  (the WS-1 gate otherwise refuses a non-loopback bind without a token — correct).
- Fresh-image note: retained MQTT state can carry a stale `mode=gateway` — check
  `lexa/hub/mode` and publish an optimizer-mode intent if the hub was migrated.
