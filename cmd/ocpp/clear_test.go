// WP-13 (B3): ClearChargingProfile release-path tests — the shell's
// release dispatch (sentinel / at-rated → ApplyClear, below-rated → Apply,
// Rejected = L11 error with retry, trivial post-release convergence) and the
// bridge's per-proto Clear send on both stacks (criteria shape, Accepted and
// Unknown as success, transport error as failure, disconnected no-op).
package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	smartcharging16 "github.com/lorenzodonini/ocpp-go/ocpp1.6/smartcharging"
	types16 "github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
	ocpp2 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/smartcharging"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
)

// newReleaseShell builds an active shell with a known 32 A rating.
func newReleaseShell(mreg *metrics.Registry, drv profileDriver) *evseShell {
	s := newEVSEShell("cs-001", mreg, modeActive, drv)
	s.ratedMaxA = 32
	return s
}

// TestEVSEShell_SentinelReleasesViaClear: a RestoreCurrentA desired doc goes
// through ApplyClear, never Apply.
func TestEVSEShell_SentinelReleasesViaClear(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingProfileDriver{}
	s := newReleaseShell(mreg, drv)
	t0 := time.Now()
	s.setDesired(evseDoc(bus.RestoreCurrentA, 0, 1, t0), t0)
	if drv.cleared != 1 {
		t.Fatalf("sentinel doc must ClearChargingProfile once, cleared=%d", drv.cleared)
	}
	if len(drv.applied) != 0 {
		t.Fatalf("a release must never re-set a numeric limit, applied=%v", drv.applied)
	}
}

// TestEVSEShell_AtRatedReleasesViaClear: a desired current AT the station's
// rated maximum (the optimizer's "charge at full rate") is a release too.
func TestEVSEShell_AtRatedReleasesViaClear(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingProfileDriver{}
	s := newReleaseShell(mreg, drv)
	t0 := time.Now()
	s.setDesired(evseDoc(32, 0, 1, t0), t0)
	if drv.cleared != 1 || len(drv.applied) != 0 {
		t.Fatalf("at-rated doc must release via Clear, cleared=%d applied=%v", drv.cleared, drv.applied)
	}
}

// TestEVSEShell_BelowRatedStillSets: a real throttle (below rated) keeps the
// SetChargingProfile path; 0 A suspend included.
func TestEVSEShell_BelowRatedStillSets(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingProfileDriver{}
	s := newReleaseShell(mreg, drv)
	t0 := time.Now()
	s.setDesired(evseDoc(16, 0, 1, t0), t0)
	s.setDesired(evseDoc(0, 0, 2, t0.Add(time.Second)), t0.Add(time.Second))
	if drv.cleared != 0 {
		t.Fatalf("below-rated limits must never Clear, cleared=%d", drv.cleared)
	}
	if len(drv.applied) != 2 || drv.applied[0] != 16 || drv.applied[1] != 0 {
		t.Fatalf("throttle then suspend must Apply 16 then 0, applied=%v", drv.applied)
	}
}

// TestEVSEShell_UnknownRatingOnlySentinelReleases: with ratedMaxA unset (0),
// the rated-value mapping is disabled — a 32 A doc Applies — but the
// explicit sentinel still releases.
func TestEVSEShell_UnknownRatingOnlySentinelReleases(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingProfileDriver{}
	s := newEVSEShell("cs-001", mreg, modeActive, drv) // ratedMaxA left 0
	t0 := time.Now()
	s.setDesired(evseDoc(32, 0, 1, t0), t0)
	if len(drv.applied) != 1 || drv.cleared != 0 {
		t.Fatalf("unknown rating: 32 A must Apply, applied=%v cleared=%d", drv.applied, drv.cleared)
	}
	s.setDesired(evseDoc(bus.RestoreCurrentA, 0, 2, t0.Add(time.Second)), t0.Add(time.Second))
	if drv.cleared != 1 {
		t.Fatalf("unknown rating: the sentinel must still Clear, cleared=%d", drv.cleared)
	}
}

// TestEVSEShell_ReleaseConverges: after a successful Clear, the very next
// plausible under-limit sample converges (release has no measurable target —
// the one-sided rule makes any real current compliant) with no further
// writes.
func TestEVSEShell_ReleaseConverges(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingProfileDriver{}
	s := newReleaseShell(mreg, drv)
	t0 := time.Now()
	s.setDesired(evseDoc(bus.RestoreCurrentA, 0, 1, t0), t0) // Clear #1
	s.observe(28, true, true, t0.Add(time.Second))           // EV charging freely
	if drv.calls != 1 {
		t.Fatalf("post-release sample must converge without another write, calls=%d", drv.calls)
	}
	if !strings.Contains(mreg.Format(), "lexa_ocpp_shadow_matches_total 1") {
		t.Errorf("post-release sample must be a match, got:\n%s", mreg.Format())
	}
}

// TestEVSEShell_RejectedClearIsFailure: a rejected/failed Clear is a write
// FAILURE (L11) — counted and logged, never a false success. It is NOT
// divergence-retried (a charger holding its OLD, more-restrictive limit is
// under the release target, which the one-sided rule deliberately treats as
// compliant — the fail-SAFE direction); the retry vector is
// reassert-on-reconnect (and any later desired change), pinned here via
// markReconnected.
func TestEVSEShell_RejectedClearIsFailure(t *testing.T) {
	mreg := metrics.New()
	drv := &recordingProfileDriver{err: errors.New(`rejected: status="Nonconforming"`)}
	s := newReleaseShell(mreg, drv)
	t0 := time.Now()
	s.setDesired(evseDoc(bus.RestoreCurrentA, 0, 1, t0), t0) // attempt #1 → fails
	// Reconnect reasserts the standing release — attempt #2, also fails.
	s.markReconnected()
	s.observe(10, true, true, t0.Add(time.Second))
	if drv.calls != 2 {
		t.Fatalf("reconnect must reassert the standing release, calls=%d", drv.calls)
	}
	if drv.cleared != 0 {
		t.Fatalf("a rejected Clear must never count as done, cleared=%d", drv.cleared)
	}
	if !strings.Contains(mreg.Format(), "lexa_ocpp_reconcile_write_failures_total 2") {
		t.Errorf("both failed Clears must be counted, got:\n%s", mreg.Format())
	}
}

// connect201 marks a station connected on the 2.0.1 stack for send tests.
func connect201(t *testing.T, b *mqttBridge, id string) {
	t.Helper()
	b.onConnect(newFakeCSConn(id, "10.0.0.20:9000"))
	if !hasStation(b, id) {
		t.Fatalf("station %s not tracked after connect", id)
	}
}

// TestBridgeApplyClear_201: the 2.0.1 Clear send — criteria pins the
// TxDefaultProfile purpose and the (0→1 mapped) EVSE id, Accepted is
// success.
func TestBridgeApplyClear_201(t *testing.T) {
	mc := &fakeMQTTClient{}
	reg := metrics.New()
	csms := ocpp2.NewCSMS(nil, nil)
	b := newMQTTBridge(mc, csms, []StationConfig{{ID: "cs-001", MaxCurrentA: 32, VoltageV: 230}}, reg.Gauge("lexa_ocpp_pending_stations"))
	b.setStationConfig("cs-001", 32, 230)
	fake := &fakeSender201{}
	b.cs201 = fake
	connect201(t, b, "cs-001")

	if err := b.ApplyClear("cs-001", 0); err != nil {
		t.Fatalf("ApplyClear: %v", err)
	}
	clears := fake.clearCalls()
	if len(clears) != 1 {
		t.Fatalf("expected 1 ClearChargingProfile, got %d", len(clears))
	}
	crit := clears[0].req.ChargingProfileCriteria
	if crit == nil || crit.EvseID == nil || *crit.EvseID != 1 {
		t.Errorf("criteria must target evse 1 (0→1 mapping), got %+v", crit)
	}
	if crit != nil && crit.ChargingProfilePurpose != types.ChargingProfilePurposeTxDefaultProfile {
		t.Errorf("criteria purpose = %q, want TxDefaultProfile (clear exactly what Apply installs)", crit.ChargingProfilePurpose)
	}
}

// TestBridgeApplyClear_201_UnknownIsSuccess: Unknown (nothing matched — the
// charger already carries no profile) IS the released state; treating it as
// an error would retry a Clear forever.
func TestBridgeApplyClear_201_UnknownIsSuccess(t *testing.T) {
	mc := &fakeMQTTClient{}
	reg := metrics.New()
	b := newMQTTBridge(mc, ocpp2.NewCSMS(nil, nil), []StationConfig{{ID: "cs-001"}}, reg.Gauge("lexa_ocpp_pending_stations"))
	b.setStationConfig("cs-001", 32, 230)
	fake := &fakeSender201{clearStatus: smartcharging.ClearChargingProfileStatusUnknown}
	b.cs201 = fake
	connect201(t, b, "cs-001")

	if err := b.ApplyClear("cs-001", 1); err != nil {
		t.Fatalf("Unknown must be success (already released), got %v", err)
	}
}

// TestBridgeApplyClear_201_TransportErrorFails: a transport-level callback
// error is an L11-style failure.
func TestBridgeApplyClear_201_TransportErrorFails(t *testing.T) {
	mc := &fakeMQTTClient{}
	reg := metrics.New()
	b := newMQTTBridge(mc, ocpp2.NewCSMS(nil, nil), []StationConfig{{ID: "cs-001"}}, reg.Gauge("lexa_ocpp_pending_stations"))
	b.setStationConfig("cs-001", 32, 230)
	fake := &fakeSender201{cbErr: errors.New("socket closed")}
	b.cs201 = fake
	connect201(t, b, "cs-001")

	if err := b.ApplyClear("cs-001", 1); err == nil {
		t.Fatal("a failed Clear must be an error, not a silent success")
	}
}

// TestBridgeApplyClear_DisconnectedNoOp: same contract as Apply — a
// disconnected station is a silent no-op (the reconnect reassert re-issues
// the standing desired doc, which re-derives the release).
func TestBridgeApplyClear_DisconnectedNoOp(t *testing.T) {
	mc := &fakeMQTTClient{}
	reg := metrics.New()
	b := newMQTTBridge(mc, ocpp2.NewCSMS(nil, nil), []StationConfig{{ID: "cs-001"}}, reg.Gauge("lexa_ocpp_pending_stations"))
	b.setStationConfig("cs-001", 32, 230)
	fake := &fakeSender201{}
	b.cs201 = fake

	if err := b.ApplyClear("cs-001", 1); err != nil {
		t.Fatalf("disconnected ApplyClear must be a silent no-op, got %v", err)
	}
	if len(fake.clearCalls()) != 0 {
		t.Fatal("no Clear may be sent to a disconnected station")
	}
}

// TestBridgeApplyClear_16: a 1.6-tagged station dispatches the 1.6 Clear
// shape — connector + TxDefaultProfile purpose — under the same contract.
func TestBridgeApplyClear_16(t *testing.T) {
	b, fake, _ := newBridge16ForTest(t, []StationConfig{{ID: "cp-001", MaxCurrentA: 32, VoltageV: 230}})
	b.onConnect16(fakeCPConn{id: "cp-001", addr: newFakeCSConn("cp-001", "10.0.0.21:9000").addr})

	if err := b.ApplyClear("cp-001", 0); err != nil {
		t.Fatalf("ApplyClear(1.6): %v", err)
	}
	clears := fake.clearCalls()
	if len(clears) != 1 {
		t.Fatalf("expected 1 ClearChargingProfile(1.6), got %d", len(clears))
	}
	req := clears[0].req
	if req.ConnectorId == nil || *req.ConnectorId != 1 {
		t.Errorf("1.6 criteria must target connector 1 (0→1 mapping), got %+v", req)
	}
	if req.ChargingProfilePurpose != types16.ChargingProfilePurposeTxDefaultProfile {
		t.Errorf("1.6 criteria purpose = %q, want TxDefaultProfile", req.ChargingProfilePurpose)
	}
}

// TestBridgeApplyClear_16_UnknownIsSuccess mirrors the 2.0.1 Unknown rule on
// the 1.6 stack.
func TestBridgeApplyClear_16_UnknownIsSuccess(t *testing.T) {
	b, fake, _ := newBridge16ForTest(t, []StationConfig{{ID: "cp-001"}})
	fake.clearStatus = smartcharging16.ClearChargingProfileStatusUnknown
	b.onConnect16(fakeCPConn{id: "cp-001", addr: newFakeCSConn("cp-001", "10.0.0.22:9000").addr})

	if err := b.ApplyClear("cp-001", 1); err != nil {
		t.Fatalf("1.6 Unknown must be success, got %v", err)
	}
}

// TestBridgeApplyClear_16_TransportErrorFails mirrors the 2.0.1 failure rule.
func TestBridgeApplyClear_16_TransportErrorFails(t *testing.T) {
	b, fake, _ := newBridge16ForTest(t, []StationConfig{{ID: "cp-001"}})
	fake.cbErr = errors.New("socket closed")
	b.onConnect16(fakeCPConn{id: "cp-001", addr: newFakeCSConn("cp-001", "10.0.0.23:9000").addr})

	if err := b.ApplyClear("cp-001", 1); err == nil {
		t.Fatal("a failed 1.6 Clear must be an error")
	}
}
