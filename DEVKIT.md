# LEXA Hub — Dev Kit Runbook (Digi ConnectCore 93 DVK)

How to (re)deploy and run the LEXA hub on the Digi ConnectCore 93 Development Kit,
and how to recover the network configuration after a reboot or reflash.

- **Device:** Digi International ConnectCore 93 DVK (i.MX 93, ARM64)
- **Hostname:** `ccimx93-dvk`
- **OS:** Digi Embedded Yocto 5.0-r3 (scarthgap), glibc 2.39
- **Login:** `ssh root@69.0.0.2` (no password configured for root over the demo LAN)

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
curl http://69.0.0.2:9100/status
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
3. `curl http://69.0.0.2:9100/status` → live device readings
4. Northbound mTLS: `journalctl -u lexa-northbound -n 20` → `discovery OK`

After rebooting the **desktop** (gridsim/dashboard host), nothing on the dev kit
is affected, but to bring the upstream side back:

1. Re-run `make wolfssl-arm64` only if you need to rebuild (the `/tmp` sysroot is gone)
2. Restart gridsim and the dashboard — see `~/projects/csip-tls-test/sim_gridsim.txt`
   and `sim_dashboard.txt`

---

## Related docs

- `~/projects/csip-tls-test/sim_gridsim.txt` — IEEE 2030.5 gridsim server on the desktop
- `~/projects/csip-tls-test/sim_dashboard.txt` — web dashboard (`cmd/dashboard`)
- `~/projects/csip-tls-test/sim_{solar,battery,meter,ev}.txt` — per-Pi simulator setup
- `CLAUDE.md` — architecture, MQTT topic map, build basics
