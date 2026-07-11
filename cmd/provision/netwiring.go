package main

import (
	"context"
	"log/slog"
	"time"

	"lexa-hub/internal/provision/netmgr"
	"lexa-hub/internal/provision/sec1"
)

// This file wires the real NetworkManager client (unit B3) into the sec1
// peripheral seams B2 stubbed: ScanFunc (a live scan) and JoinBehavior (a live,
// streaming join). It is deliberately thin — the NetworkManager D-Bus work lives
// in internal/provision/netmgr; here we only bound each operation and translate
// netmgr's JoinUpdate into ADR-0002 status states.

// scanCallback builds the sec1 ScanFunc that answers a WiFi scan request with a
// live NetworkManager scan, bounded by scanTimeout.
func scanCallback(nm *netmgr.Client, scanTimeout time.Duration) func() ([]sec1.WifiAp, error) {
	return func() ([]sec1.WifiAp, error) {
		ctx, cancel := context.WithTimeout(context.Background(), scanTimeout)
		defer cancel()
		aps, err := nm.Scan(ctx)
		if err != nil {
			slog.Warn("wifi scan failed", "err", err)
			return nil, err
		}
		slog.Info("wifi scan complete", "aps", len(aps))
		return aps, nil
	}
}

// liveJoin builds the sec1.JoinRunner that drives a real NetworkManager join and
// streams its progress as ADR-0002 status states.
func liveJoin(nm *netmgr.Client, port int) sec1.JoinRunner {
	return func(ctx context.Context, req sec1.Join, emit func(sec1.StateMessage)) {
		jr := netmgr.JoinRequest{SSID: req.SSID, PSK: req.PSK}
		slog.Info("wifi join starting", "ssid", req.SSID)
		err := nm.Join(ctx, jr, func(u netmgr.JoinUpdate) {
			emit(updateToState(u, port))
		})
		// A cancelled join (superseded by a retry/handshake) is not an error to
		// report; the superseding attempt streams its own states.
		if err != nil && ctx.Err() == nil {
			slog.Warn("wifi join ended with error", "ssid", req.SSID, "err", err)
		}
	}
}

// updateToState translates one netmgr.JoinUpdate into the sec1 StateMessage the
// hub streams on the status characteristic. The joined handoff carries {ip,
// port}; the serial is filled by the peripheral (withSerial) and api_cert_fp +
// token are filled by B4's handoffRunner (handoff.go), which decorates this
// runner's emit. updateToState itself leaves that pair empty — the secret fill
// is deliberately a separate layer, so this netmgr→state translation stays
// pure and independently testable.
func updateToState(u netmgr.JoinUpdate, port int) sec1.StateMessage {
	switch u.State {
	case sec1.StateJoined:
		return sec1.StateMessage{
			State: sec1.StateJoined,
			Handoff: &sec1.HandoffInfo{
				IP:   u.IP,
				Port: port,
			},
		}
	case sec1.StateFailed:
		r := u.Reason
		return sec1.StateMessage{State: sec1.StateFailed, Reason: &r}
	default:
		return sec1.StateMessage{State: sec1.StateJoining}
	}
}
