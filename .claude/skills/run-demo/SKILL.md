---
name: run-demo
description: Bring up the LEXA hub's side of the bench demo, verify the full chain (gridsim ← hub ← sims), and know where the rest lives. Use for "run the demo", "start the demo", "is the demo up", or post-reboot recovery.
---

# Run the demo (hub side)

The demo chain: gridsim on the desktop (69.0.0.20) ←mTLS← **this hub** (hub-pi
`dhpi4`, 69.0.0.1) ←Modbus/OCPP→ device sims on the Pis, visualized at
**http://69.0.0.20:8080**. Topology: `../csip-tls-test/docs/BENCH.md`.

Division of labour:
- **Desktop gridsim + dashboard, device sims** → harness repo (`~/projects/csip-tls-test`,
  its `run-demo` skill has the exact `systemd-run` commands). The desktop pieces are
  transient units that die on desktop reboot — that is the usual "demo is broken" cause,
  not the hub.
- **This repo** owns the six lexa services on hub-pi (root units — they survive reboots).

## 1. Hub health

```bash
ssh dmitri@69.0.0.1 'sudo systemctl is-active mosquitto lexa-modbus lexa-northbound lexa-telemetry lexa-ocpp lexa-api lexa-hub'
curl -s --max-time 3 http://69.0.0.1:9100/status | head -c 300
```
All seven `active` + a JSON status with device readings ⇒ hub side is demo-ready.

Restart order if multiple services are down (dependencies flow downward):
```bash
ssh dmitri@69.0.0.1 'sudo systemctl restart mosquitto && \
  sudo systemctl restart lexa-modbus lexa-ocpp lexa-api && \
  sudo systemctl restart lexa-northbound lexa-telemetry && \
  sudo systemctl restart lexa-hub'
```

## 2. Confirm the hub is in the chain

1. Northbound walking gridsim: `ssh dmitri@69.0.0.1 'sudo journalctl -u lexa-northbound -n 10'`
   shows periodic walk cycles, no TLS errors. (Desktop-side proof:
   `journalctl --user -u csip-gridsim` shows `GET ... peer=<LFDI>`.)
2. Controls on the bus: `ssh dmitri@69.0.0.1 'timeout 5 mosquitto_sub -t lexa/csip/control -v'`
   returns the retained control immediately.
3. Measurements ticking: `ssh dmitri@69.0.0.1 'timeout 12 mosquitto_sub -t "lexa/measurements/+" -v'`
   shows updates within ~10 s.
4. EV connected: `lexa/evse/+/state` shows the station, or check the dashboard EV card.

If 1 fails with TLS errors → gridsim is down on the desktop (transient unit — see the
harness repo's run-demo skill) or certs changed. If 3 is silent → lexa-modbus can't
reach the sim Pis (check `ping 69.0.0.10` from the hub, then the sims' user units).

## 3. Run it

Open **http://69.0.0.20:8080** → Scenarios tab → run any of the five scenarios; each
stages the sims, fires a grid event, and asserts the PCC outcome (PASS/FAIL shown live).
Watch the hub react: `sudo journalctl -u lexa-hub -f` shows the optimizer plan; the
Logs tab on the dashboard merges all backends.

## Rules
- Don't restart hub services mid-demo (the retained `lexa/csip/control` makes restarts
  *mostly* seamless, but the dashboard KPIs blip) — flag it first.
- Never deploy a SunSpec register-map change to only one side during demo prep
  (MTR-4 lockstep: hub + metersim together).
