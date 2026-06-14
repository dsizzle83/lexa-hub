---
name: hub-deploy
description: Cross-compile and deploy the six lexa services to the bench hub, restart, and verify. Use for "deploy the hub", "push to the Pi", "update the hub", or post-deploy health checks.
---

# Hub deploy

Topology source of truth: `~/projects/csip-tls-test/docs/BENCH.md`. Currently the hub is
**hub-pi `dhpi4` at 69.0.0.1** (SSH `dmitri@`, passwordless sudo, root systemd units).
The ConnectCore dev kit (69.0.0.2) is offline — its runbook is `DEVKIT.md`.

## Build + deploy

```bash
make wolfssl-arm64        # only if /tmp/wolfssl-arm64-sysroot is missing (wiped on reboot)
make build-arm64          # all six binaries → bin/arm64/
bash scripts/deploy-hub-pi.sh 69.0.0.1 dmitri
```
The script installs mosquitto + binaries + configs + units and starts everything in
dependency order. Client certs are staged from `../csip-tls-test/certs/client-staging/`.

**Lockstep warning**: if this deploy includes a SunSpec register-map change
(`internal/southbound/sunspec`), the sims in csip-tls-test must be redeployed in the same
session (audit MTR-4) — otherwise hub and metersim read garbage from each other.

## Verify

```bash
ssh dmitri@69.0.0.1 'sudo systemctl is-active mosquitto lexa-modbus lexa-northbound lexa-telemetry lexa-ocpp lexa-api lexa-hub'
curl -s http://69.0.0.1:9100/status        # lexa-api: link state, device readings, EV state
ssh dmitri@69.0.0.1 'sudo journalctl -u lexa-hub -n 20 --no-pager'   # optimizer ticking?
```
A healthy bench also shows the meter balance closing on the dashboard (69.0.0.20:8080).
Report each service as active/failed with journal evidence for failures.

## Config changes
Configs live on-device in `/etc/lexa/*.json` (the deploy script does NOT overwrite them —
`make install-configs` semantics). To change config: edit on device, then
`sudo systemctl restart <service>`. Mirror any keeper changes back into `configs/` here.

## Never
- `pkill -f` over SSH (can kill your own session) — use systemctl.
- Restart services mid-demo without flagging it.
- Copy private keys outside the established script flow.
