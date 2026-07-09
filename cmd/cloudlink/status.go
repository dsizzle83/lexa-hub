package main

// status.go holds two things: the seams unit 2.2 (spool) and unit 2.3
// (cloud session) plug into, and the retained CloudlinkStatus publisher
// this unit (2.1) fully owns.

import (
	"context"
	"log/slog"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/mqttutil"
)

// cloudSession is the seam unit 2.3 (cmd/cloudlink/cloud.go:
// docs/DEVICE_ROADMAP.md §2.1 — a paho client over crypto/tls, mTLS to
// cfg.Endpoint, its own reconnect loop) plugs into. statusPublisher only
// needs to know whether the cloud link is up right now; 2.3 is expected to
// grow a concrete implementation with whatever Publish/Subscribe/Close
// surface the batcher (2.2) and downlink (2.4) need on top of it —
// Connected() bool is the only method this unit's statusPublisher calls.
type cloudSession interface {
	Connected() bool
}

// stubCloudSession is the nil-safe placeholder cloudSession this unit wires
// into main(): the real cloud session does not exist yet (2.3), and — just
// as important — THIS unit must never attempt a WAN connection regardless
// of cfg.Enabled (see main.go's package doc), so "always disconnected" is
// not merely a placeholder, it is also the one correct answer for the
// enabled:false local-only configuration this unit ships by default.
type stubCloudSession struct{}

func (stubCloudSession) Connected() bool { return false }

// spoolStats is the seam unit 2.2 (internal/spool — already vendored under
// internal/spool/ as a leaf package, not yet wired into any service) is
// expected to satisfy. That package's *spool.Spool already exposes
// Bytes() int64 and OldestTs() int64 with exactly these signatures (see
// internal/spool/spool.go), so 2.2 should be able to hand a live
// *spool.Spool straight into statusPublisher with no adapter type needed.
type spoolStats interface {
	Bytes() int64
	OldestTs() int64
}

// stubSpoolStats is the zero-value placeholder wired in until 2.2 lands:
// no spool exists yet in this unit, so both figures report empty.
type stubSpoolStats struct{}

func (stubSpoolStats) Bytes() int64    { return 0 }
func (stubSpoolStats) OldestTs() int64 { return 0 }

// buildStatus constructs the CloudlinkStatus message published to
// bus.TopicCloudlinkStatus. It is factored out of statusPublisher as a pure
// function of (cfg, session, spool, now) so its JSON wire shape — including
// the "v":1 envelope stamp — is unit-testable without a live MQTT client or
// a real cloud session/spool (status_test.go).
//
// LastUplinkTs and CertDaysLeft are left at their zero value (all
// omitempty on the wire, per bus.CloudlinkStatus's json tags): no uplink
// has ever happened and no cert monitor runs in this unit — 2.2's batcher
// and 2.7's cert monitor are the respective future owners of those fields.
func buildStatus(cfg *Config, session cloudSession, sp spoolStats, now time.Time) bus.CloudlinkStatus {
	st := bus.CloudlinkStatus{
		Envelope:      bus.Envelope{V: bus.CloudlinkStatusV},
		Connected:     session.Connected(),
		SpoolBytes:    sp.Bytes(),
		SpoolOldestTs: sp.OldestTs(),
		Ts:            now.Unix(),
	}
	// Endpoint only when Enabled (spec item 4): an enabled:false box has
	// nothing configured to report, and surfacing a would-be endpoint that
	// is never dialed would mislead anything folding this into a
	// dashboard's /status view.
	if cfg.Enabled {
		st.Endpoint = cfg.Endpoint
	}
	return st
}

// statusPublisher publishes the retained CloudlinkStatus once immediately
// (spec: "once at startup") and again every cfg.HealthInterval() thereafter,
// until ctx is canceled. At the 900s default this is journald/flash-
// negligible (CLAUDE.md's Metrics section makes the same point about the
// plan heartbeat's 5s ticker, a much faster cadence), so no additional rate
// limiting is needed beyond the ticker itself.
//
// lastUplink supplies CloudlinkStatus.LastUplinkTs — the batcher's atomic when
// the cloud link is enabled, else a constant 0. It is overlaid AFTER buildStatus
// so buildStatus's pure (cfg, session, sp, now) signature stays exactly as unit
// 2.1 pinned it (status_test.go); the batcher, not buildStatus, owns that field.
//
// certDaysLeft is the same overlay pattern for CloudlinkStatus.CertDaysLeft
// (§2.7): cloudCertMon's CloudDaysLeft when the cloud link is enabled, else
// main.go's constant-0 default (no cert monitor runs disabled — mirroring
// lastUplink's disabled-path shape exactly). nil is also tolerated (tests)
// and leaves the field at its omitempty zero.
func statusPublisher(ctx context.Context, mc mqtt.Client, cfg *Config, session cloudSession, sp spoolStats, lastUplink func() int64, certDaysLeft func() int, m *cloudlinkMetrics) {
	publish := func() {
		st := buildStatus(cfg, session, sp, time.Now())
		if lastUplink != nil {
			st.LastUplinkTs = lastUplink()
		}
		if certDaysLeft != nil {
			st.CertDaysLeft = certDaysLeft()
		}
		if err := mqttutil.PublishJSONRetained(mc, bus.TopicCloudlinkStatus, st); err != nil {
			slog.Warn("lexa-cloudlink: publish status failed", "err", err)
		}
		m.connected.Set(boolToGauge(st.Connected))
		m.spoolBytes.Set(float64(st.SpoolBytes))
	}
	publish()

	t := time.NewTicker(cfg.HealthInterval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			publish()
		}
	}
}

func boolToGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
