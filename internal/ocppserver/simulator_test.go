package ocppserver_test

import (
	"fmt"
	"net"
	"testing"
	"time"

	ocpp2 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/availability"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/provisioning"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"

	"lexa-hub/internal/ocppserver"
)

// freePort returns an unused TCP port on localhost.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// noopCSHandler satisfies the provisioning.ChargingStationHandler and
// availability.ChargingStationHandler interfaces; the simulator never receives
// CSMS-initiated requests in this test.
type noopCSHandler struct{}

func (noopCSHandler) OnGetBaseReport(r *provisioning.GetBaseReportRequest) (*provisioning.GetBaseReportResponse, error) {
	return &provisioning.GetBaseReportResponse{Status: types.GenericDeviceModelStatusAccepted}, nil
}
func (noopCSHandler) OnGetReport(r *provisioning.GetReportRequest) (*provisioning.GetReportResponse, error) {
	return &provisioning.GetReportResponse{Status: types.GenericDeviceModelStatusAccepted}, nil
}
func (noopCSHandler) OnGetVariables(r *provisioning.GetVariablesRequest) (*provisioning.GetVariablesResponse, error) {
	return &provisioning.GetVariablesResponse{}, nil
}
func (noopCSHandler) OnReset(r *provisioning.ResetRequest) (*provisioning.ResetResponse, error) {
	return &provisioning.ResetResponse{Status: provisioning.ResetStatusAccepted}, nil
}
func (noopCSHandler) OnSetNetworkProfile(r *provisioning.SetNetworkProfileRequest) (*provisioning.SetNetworkProfileResponse, error) {
	return &provisioning.SetNetworkProfileResponse{Status: provisioning.SetNetworkProfileStatusAccepted}, nil
}
func (noopCSHandler) OnSetVariables(r *provisioning.SetVariablesRequest) (*provisioning.SetVariablesResponse, error) {
	return &provisioning.SetVariablesResponse{}, nil
}
func (noopCSHandler) OnChangeAvailability(r *availability.ChangeAvailabilityRequest) (*availability.ChangeAvailabilityResponse, error) {
	return &availability.ChangeAvailabilityResponse{Status: availability.ChangeAvailabilityStatusAccepted}, nil
}

// TestSimulator_BootAndHeartbeat starts an in-process CSMS (plain WebSocket,
// no TLS) and simulates a charging station connecting, booting, and sending a
// heartbeat.
func TestSimulator_BootAndHeartbeat(t *testing.T) {
	port := freePort(t)

	srv := ocppserver.New(ocppserver.Config{Port: port}) // no TLS, no basic auth
	go srv.Start()

	// Wait until the TCP port is actually accepting connections.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 50*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Create a charging station using the plain WebSocket client.
	const csID = "test-cs-001"
	cs := ocpp2.NewChargingStation(csID, nil, nil)

	noop := noopCSHandler{}
	cs.SetProvisioningHandler(noop)
	cs.SetAvailabilityHandler(noop)

	// ocppj.Client.Start appends "/{id}" to the URL automatically, so we pass
	// only the base path.  The actual dial URL becomes ws://host:port/ocpp/cs-id,
	// which matches the server's "/ocpp/{id}" route template.
	csmsURL := fmt.Sprintf("ws://127.0.0.1:%d/ocpp", port)
	if err := cs.Start(csmsURL); err != nil {
		t.Fatalf("charging station Start: %v", err)
	}
	defer cs.Stop()

	// ── BootNotification ────────────────────────────────────────────────────
	bootResp, err := cs.BootNotification(
		provisioning.BootReasonPowerUp, "TestModel", "TestVendor",
	)
	if err != nil {
		t.Fatalf("BootNotification: %v", err)
	}
	if bootResp.Status != provisioning.RegistrationStatusAccepted {
		t.Errorf("BootNotification status = %s, want Accepted", bootResp.Status)
	}
	if bootResp.Interval != 60 {
		t.Errorf("BootNotification interval = %d, want 60", bootResp.Interval)
	}

	// ── Heartbeat ────────────────────────────────────────────────────────────
	hbResp, err := cs.Heartbeat()
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if hbResp.CurrentTime.IsZero() {
		t.Error("Heartbeat response has zero CurrentTime")
	}
	// Server time should be within a few seconds of local time.
	drift := time.Since(hbResp.CurrentTime.Time).Abs()
	if drift > 5*time.Second {
		t.Errorf("Heartbeat time drift too large: %v", drift)
	}

	t.Logf("BootNotification: status=%s interval=%ds", bootResp.Status, bootResp.Interval)
	t.Logf("Heartbeat:        serverTime=%s drift=%v", hbResp.CurrentTime.FormatTimestamp(), drift)

	srv.Stop()
}
