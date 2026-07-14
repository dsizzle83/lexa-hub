// WP-12: fixtures for the OCPP 1.6J forwarders (bridge16.go) — each mapped
// message driven through the real forwarder16/bridge methods against the
// SAME stationState the 2.0.1 forwarders feed, mirroring bridge_test.go's
// style (fake conns, named handler methods, no live WebSocket handshake).
package main

import (
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	ocpp16 "github.com/lorenzodonini/ocpp-go/ocpp1.6"
	core16 "github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	remotetrigger16 "github.com/lorenzodonini/ocpp-go/ocpp1.6/remotetrigger"
	smartcharging16 "github.com/lorenzodonini/ocpp-go/ocpp1.6/smartcharging"
	types16 "github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
	ocpp2 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1"

	"lexa-hub/internal/metrics"
)

// fakeCPConn is a minimal ocpp16.ChargePointConnection stand-in — the 1.6
// sibling of fakeCSConn (bridge_test.go), used to drive onConnect16 /
// onDisconnect16 directly.
type fakeCPConn struct {
	id   string
	addr net.Addr
}

func (f fakeCPConn) ID() string                               { return f.id }
func (f fakeCPConn) RemoteAddr() net.Addr                     { return f.addr }
func (f fakeCPConn) TLSConnectionState() *tls.ConnectionState { return nil }

func newFakeCPConn(id, addr string) fakeCPConn {
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		panic(err)
	}
	return fakeCPConn{id: id, addr: tcpAddr}
}

// fakeSender16 records CSMS-initiated 1.6 sends through the centralSystem16
// seam and answers the SetChargingProfile callback with a configurable
// status/error (default Accepted), asynchronously like the real library.
type fakeSender16 struct {
	mu      sync.Mutex
	sp      []spCall16
	trig    []remotetrigger16.MessageTrigger
	status  smartcharging16.ChargingProfileStatus // "" ⇒ Accepted
	cbErr   error                                 // handed to the callback (transport failure)
	callErr error                                 // returned from SetChargingProfile itself
}

type spCall16 struct {
	stationID   string
	connectorID int
	profile     *types16.ChargingProfile
}

func (f *fakeSender16) SetChargingProfile(stationID string, cb func(*smartcharging16.SetChargingProfileConfirmation, error), connectorID int, profile *types16.ChargingProfile, _ ...func(*smartcharging16.SetChargingProfileRequest)) error {
	f.mu.Lock()
	status := f.status
	if status == "" {
		status = smartcharging16.ChargingProfileStatusAccepted
	}
	cbErr := f.cbErr
	callErr := f.callErr
	if callErr == nil {
		f.sp = append(f.sp, spCall16{stationID, connectorID, profile})
	}
	f.mu.Unlock()
	if callErr != nil {
		return callErr
	}
	go func() {
		if cbErr != nil {
			cb(nil, cbErr)
			return
		}
		cb(smartcharging16.NewSetChargingProfileConfirmation(status), nil)
	}()
	return nil
}

func (f *fakeSender16) TriggerMessage(_ string, cb func(*remotetrigger16.TriggerMessageConfirmation, error), requestedMessage remotetrigger16.MessageTrigger, _ ...func(*remotetrigger16.TriggerMessageRequest)) error {
	f.mu.Lock()
	f.trig = append(f.trig, requestedMessage)
	f.mu.Unlock()
	go cb(remotetrigger16.NewTriggerMessageConfirmation(remotetrigger16.TriggerMessageStatusAccepted), nil)
	return nil
}

func (f *fakeSender16) spCalls() []spCall16 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]spCall16(nil), f.sp...)
}

func (f *fakeSender16) trigCalls() []remotetrigger16.MessageTrigger {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]remotetrigger16.MessageTrigger(nil), f.trig...)
}

// newBridge16ForTest builds a bridge with a fake MQTT client, a real (never
// started) 2.0.1 CSMS — exactly like bridge_test.go — plus the 1.6 send fake.
func newBridge16ForTest(t *testing.T, stations []StationConfig) (*mqttBridge, *fakeSender16, *metrics.Registry) {
	t.Helper()
	mc := &fakeMQTTClient{}
	reg := metrics.New()
	gauge := reg.Gauge("lexa_ocpp_pending_stations")
	csms := ocpp2.NewCSMS(nil, nil)
	bridge := newMQTTBridge(mc, csms, stations, gauge)
	for _, sc := range stations {
		bridge.setStationConfig(sc.ID, sc.MaxCurrentA, sc.VoltageV)
	}
	fake := &fakeSender16{}
	bridge.cs16 = fake
	return bridge, fake, reg
}

func stationSnapshot(t *testing.T, b *mqttBridge, id string) stationState {
	t.Helper()
	b.mu.RLock()
	defer b.mu.RUnlock()
	s, ok := b.stations[id]
	if !ok {
		t.Fatalf("station %s not tracked", id)
	}
	return *s
}

// TestBridge16_UnconfiguredStationBecomesPending mirrors the 2.0.1 test: a
// 1.6 charge point whose ID is not in cfg.Stations must surface as pending
// (same pendingStations component, same gauge).
func TestBridge16_UnconfiguredStationBecomesPending(t *testing.T) {
	bridge, _, reg := newBridge16ForTest(t, []StationConfig{{ID: "cs-configured"}})

	bridge.onConnect16(newFakeCPConn("cp-unknown", "10.0.0.7:9000"))

	if got := reg.Format(); !strings.Contains(got, "lexa_ocpp_pending_stations 1") {
		t.Errorf("expected lexa_ocpp_pending_stations=1, got:\n%s", got)
	}
}

// TestBridge16_ConnectTagsProtoAndTracks: a 1.6 connect must tag the station
// protoOCPP16 (dispatch routing) and mark it connected; a configured station
// must never appear pending.
func TestBridge16_ConnectTagsProtoAndTracks(t *testing.T) {
	bridge, _, reg := newBridge16ForTest(t, []StationConfig{{ID: "cp-001", MaxCurrentA: 32, VoltageV: 230}})

	bridge.onConnect16(newFakeCPConn("cp-001", "10.0.0.8:9000"))

	s := stationSnapshot(t, bridge, "cp-001")
	if s.proto != protoOCPP16 {
		t.Errorf("proto = %q after 1.6 connect, want %q", s.proto, protoOCPP16)
	}
	if !s.connected {
		t.Error("station not marked connected after onConnect16")
	}
	if got := reg.Format(); !strings.Contains(got, "lexa_ocpp_pending_stations 0") {
		t.Errorf("configured station must never be pending, got:\n%s", got)
	}

	bridge.onDisconnect16(newFakeCPConn("cp-001", "10.0.0.8:9000"))
	if s := stationSnapshot(t, bridge, "cp-001"); s.connected {
		t.Error("station still marked connected after onDisconnect16")
	}
}

// TestBridge16_BootNotificationAcceptedFillsVendorModel: the 1.6 boot
// forwarder must answer Accepted with the same 60 s heartbeat interval as
// the 2.0.1 provisioning forwarder (no pairing gate — WP-13), stamp the
// proto tag, and feed vendor/model to the pending surface.
func TestBridge16_BootNotificationAcceptedFillsVendorModel(t *testing.T) {
	bridge, _, reg := newBridge16ForTest(t, nil)
	f := &forwarder16{bridge: bridge}

	resp, err := f.OnBootNotification("cp-boot", &core16.BootNotificationRequest{
		ChargePointModel:  "Slow22",
		ChargePointVendor: "AcmeEV",
	})
	if err != nil {
		t.Fatalf("OnBootNotification returned error: %v", err)
	}
	if resp.Status != core16.RegistrationStatusAccepted {
		t.Errorf("boot status = %q, want Accepted (auto-accept until WP-13)", resp.Status)
	}
	if resp.Interval != 60 {
		t.Errorf("heartbeat interval = %d, want 60 (2.0.1 parity)", resp.Interval)
	}
	if s := stationSnapshot(t, bridge, "cp-boot"); s.proto != protoOCPP16 {
		t.Errorf("proto = %q after 1.6 boot, want %q", s.proto, protoOCPP16)
	}
	if got := reg.Format(); !strings.Contains(got, "lexa_ocpp_pending_stations 1") {
		t.Errorf("unconfigured booting station must be pending, got:\n%s", got)
	}
}

// TestBridge16_StatusNotificationMapsStatusVocabulary pins mapStatus16: the
// 1.6 fine-grained statuses fold into the 2.0.1 vocabulary the bridge (and
// lexa/evse/{station}/state) already carries, so SessionActive means the
// same thing on both stacks.
func TestBridge16_StatusNotificationMapsStatusVocabulary(t *testing.T) {
	cases := []struct {
		raw  core16.ChargePointStatus
		want connectorStatus
	}{
		{core16.ChargePointStatusAvailable, statusAvailable},
		{core16.ChargePointStatusPreparing, statusOccupied},
		{core16.ChargePointStatusCharging, statusOccupied},
		{core16.ChargePointStatusSuspendedEV, statusOccupied},
		{core16.ChargePointStatusSuspendedEVSE, statusOccupied},
		{core16.ChargePointStatusFinishing, statusOccupied},
		{core16.ChargePointStatusFaulted, statusFaulted},
		{core16.ChargePointStatusUnavailable, statusUnavailable},
		{core16.ChargePointStatusReserved, connectorStatus("Reserved")}, // 2.0.1 pass-through parity
	}
	bridge, _, _ := newBridge16ForTest(t, []StationConfig{{ID: "cp-001"}})
	f := &forwarder16{bridge: bridge}

	for _, tc := range cases {
		if _, err := f.OnStatusNotification("cp-001", &core16.StatusNotificationRequest{
			ConnectorId: 1,
			Status:      tc.raw,
			ErrorCode:   core16.NoError,
		}); err != nil {
			t.Fatalf("OnStatusNotification(%s): %v", tc.raw, err)
		}
		bridge.mu.RLock()
		got := bridge.stations["cp-001"].connectors[1].status
		bridge.mu.RUnlock()
		if got != tc.want {
			t.Errorf("status %q mapped to %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func mv16(samples ...types16.SampledValue) []types16.MeterValue {
	return []types16.MeterValue{{SampledValue: samples}}
}

// TestBridge16_MeterValuesFoldsMeasurands: the four bridge measurands fold
// into the SAME stationState fields as 2.0.1, including the 1.6-specific
// handling — string values, kWh unit scaling, and the spec's blank-measurand
// default (Energy.Active.Import.Register).
func TestBridge16_MeterValuesFoldsMeasurands(t *testing.T) {
	bridge, _, _ := newBridge16ForTest(t, []StationConfig{{ID: "cp-001", MaxCurrentA: 32, VoltageV: 230}})
	f := &forwarder16{bridge: bridge}

	if _, err := f.OnMeterValues("cp-001", &core16.MeterValuesRequest{
		ConnectorId: 1,
		MeterValue: mv16(
			types16.SampledValue{Value: "16", Measurand: types16.MeasurandCurrentImport},
			types16.SampledValue{Value: "55.5", Measurand: types16.MeasurandSoC},
			types16.SampledValue{Value: "1234", Measurand: types16.MeasurandEnergyActiveImportRegister},
			types16.SampledValue{Value: "231", Measurand: types16.MeasurandVoltage},
		),
	}); err != nil {
		t.Fatalf("OnMeterValues: %v", err)
	}
	s := stationSnapshot(t, bridge, "cp-001")
	if s.currentA != 16 || s.soc != 55.5 || s.energyWh != 1234 || s.voltageV != 231 {
		t.Errorf("folded state = currentA %v soc %v energyWh %v voltageV %v, want 16/55.5/1234/231",
			s.currentA, s.soc, s.energyWh, s.voltageV)
	}
	if s.proto != protoOCPP16 {
		t.Errorf("MeterValues must stamp proto %q, got %q", protoOCPP16, s.proto)
	}

	// kWh unit scales ×1000; a blank measurand is the 1.6 default register.
	if _, err := f.OnMeterValues("cp-001", &core16.MeterValuesRequest{
		ConnectorId: 1,
		MeterValue:  mv16(types16.SampledValue{Value: "1.5", Measurand: types16.MeasurandEnergyActiveImportRegister, Unit: types16.UnitOfMeasureKWh}),
	}); err != nil {
		t.Fatalf("OnMeterValues (kWh): %v", err)
	}
	if s := stationSnapshot(t, bridge, "cp-001"); s.energyWh != 1500 {
		t.Errorf("kWh sample folded to %v Wh, want 1500", s.energyWh)
	}
	if _, err := f.OnMeterValues("cp-001", &core16.MeterValuesRequest{
		ConnectorId: 1,
		MeterValue:  mv16(types16.SampledValue{Value: "2000"}), // no measurand ⇒ energy register
	}); err != nil {
		t.Fatalf("OnMeterValues (blank measurand): %v", err)
	}
	if s := stationSnapshot(t, bridge, "cp-001"); s.energyWh != 2000 {
		t.Errorf("blank-measurand sample folded to %v Wh, want 2000 (1.6 default register)", s.energyWh)
	}
}

// TestBridge16_MeterValuesPlausibilityGateKeepsLastGood: the SAME L11 gate as
// 2.0.1 (current beyond rating×1.25 rejected keep-last-good), plus the
// 1.6-only string hazards: non-numeric and non-finite values are rejected the
// same way.
func TestBridge16_MeterValuesPlausibilityGateKeepsLastGood(t *testing.T) {
	bridge, _, _ := newBridge16ForTest(t, []StationConfig{{ID: "cp-001", MaxCurrentA: 32, VoltageV: 230}})
	f := &forwarder16{bridge: bridge}

	feed := func(val string) {
		t.Helper()
		if _, err := f.OnMeterValues("cp-001", &core16.MeterValuesRequest{
			ConnectorId: 1,
			MeterValue:  mv16(types16.SampledValue{Value: val, Measurand: types16.MeasurandCurrentImport}),
		}); err != nil {
			t.Fatalf("OnMeterValues(%q): %v", val, err)
		}
	}

	feed("16")
	if s := stationSnapshot(t, bridge, "cp-001"); s.currentA != 16 {
		t.Fatalf("valid sample not folded: currentA=%v, want 16", s.currentA)
	}
	feed("6000") // mA-as-A wrong units, 32 A station — must keep last good
	feed("abc")  // non-numeric string
	feed("NaN")  // parses, but non-finite must never enter state
	feed("+Inf")
	if s := stationSnapshot(t, bridge, "cp-001"); s.currentA != 16 {
		t.Errorf("implausible/non-finite sample ingested: currentA=%v, want last-good 16", s.currentA)
	}
	feed("10")
	if s := stationSnapshot(t, bridge, "cp-001"); s.currentA != 10 {
		t.Errorf("recovery sample not folded: currentA=%v, want 10", s.currentA)
	}
}

// TestBridge16_StartStopTransactionLifecycle: 1.6 Start/StopTransaction maps
// onto the TransactionEvent-shaped session lifecycle — Started counts the
// transaction and assigns a CSMS-side tx ID; Ended (StopTransaction) folds
// the closing register and ZEROES current so site power drops immediately.
func TestBridge16_StartStopTransactionLifecycle(t *testing.T) {
	bridge, _, reg := newBridge16ForTest(t, []StationConfig{{ID: "cp-001", MaxCurrentA: 32, VoltageV: 230}})
	bridge.transactionsTotal = reg.Counter("lexa_ocpp_transactions_total")
	f := &forwarder16{bridge: bridge}

	start, err := f.OnStartTransaction("cp-001", &core16.StartTransactionRequest{
		ConnectorId: 1, IdTag: "tag-1", MeterStart: 1200,
	})
	if err != nil {
		t.Fatalf("OnStartTransaction: %v", err)
	}
	if start.TransactionId <= 0 {
		t.Errorf("CSMS must assign a positive transaction ID, got %d", start.TransactionId)
	}
	if start.IdTagInfo == nil || start.IdTagInfo.Status != types16.AuthorizationStatusAccepted {
		t.Errorf("StartTransaction idTagInfo = %+v, want Accepted", start.IdTagInfo)
	}
	if s := stationSnapshot(t, bridge, "cp-001"); s.energyWh != 1200 {
		t.Errorf("MeterStart not folded as energy register: energyWh=%v, want 1200", s.energyWh)
	}
	if !strings.Contains(reg.Format(), "lexa_ocpp_transactions_total 1") {
		t.Errorf("StartTransaction must count lexa_ocpp_transactions_total, got:\n%s", reg.Format())
	}

	// A second session gets a distinct, increasing ID.
	start2, err := f.OnStartTransaction("cp-001", &core16.StartTransactionRequest{
		ConnectorId: 1, IdTag: "tag-2", MeterStart: 2000,
	})
	if err != nil {
		t.Fatalf("OnStartTransaction #2: %v", err)
	}
	if start2.TransactionId <= start.TransactionId {
		t.Errorf("tx IDs must increase: first=%d second=%d", start.TransactionId, start2.TransactionId)
	}

	// Mid-session current, then StopTransaction: current zeroed, register
	// taken from MeterStop, final TransactionData samples folded first.
	if _, err := f.OnMeterValues("cp-001", &core16.MeterValuesRequest{
		ConnectorId: 1,
		MeterValue:  mv16(types16.SampledValue{Value: "14", Measurand: types16.MeasurandCurrentImport}),
	}); err != nil {
		t.Fatalf("OnMeterValues: %v", err)
	}
	if _, err := f.OnStopTransaction("cp-001", &core16.StopTransactionRequest{
		TransactionId: start2.TransactionId,
		MeterStop:     5400,
		Reason:        core16.ReasonLocal,
		TransactionData: mv16(
			types16.SampledValue{Value: "77", Measurand: types16.MeasurandSoC},
		),
	}); err != nil {
		t.Fatalf("OnStopTransaction: %v", err)
	}
	s := stationSnapshot(t, bridge, "cp-001")
	if s.currentA != 0 {
		t.Errorf("StopTransaction must zero current (TransactionEvent Ended analog), got %v", s.currentA)
	}
	if s.energyWh != 5400 {
		t.Errorf("MeterStop not folded: energyWh=%v, want 5400", s.energyWh)
	}
	if s.soc != 77 {
		t.Errorf("final TransactionData not folded: soc=%v, want 77", s.soc)
	}
}

// TestBridge16_StopTransactionDrivesSuspendConvergence: a commanded 0 A
// suspend converges when StopTransaction forces measured current to 0 —
// through the SAME observeShell path as 2.0.1's TransactionEvent Ended.
func TestBridge16_StopTransactionDrivesSuspendConvergence(t *testing.T) {
	bridge, _, _ := newBridge16ForTest(t, []StationConfig{{ID: "cs-001", MaxCurrentA: 32, VoltageV: 230}})
	reg := metrics.New()
	drv := &recordingProfileDriver{}
	sh := newEVSEShell("cs-001", reg, modeActive, drv)
	bridge.shells = map[string]*evseShell{"cs-001": sh}
	f := &forwarder16{bridge: bridge}

	// Mid-session connected state, set directly rather than via onConnect16:
	// a fresh connect would ALSO arm the reconnect-reassert (its own test
	// below), which would obscure the pure suspend-convergence assertion.
	bridge.mu.Lock()
	s, _ := bridge.getOrCreate16Locked("cs-001")
	s.connected = true
	bridge.mu.Unlock()

	t0 := time.Now()
	sh.setDesired(evseDoc(0, 0, 1, t0), t0) // suspend, applied #1
	if len(drv.applied) != 1 {
		t.Fatalf("suspend doc should apply once, applies=%d", len(drv.applied))
	}
	// StopTransaction zeroes current; the shell must see 0 A and converge —
	// no corrective write.
	if _, err := f.OnStopTransaction("cs-001", &core16.StopTransactionRequest{
		TransactionId: 1, MeterStop: 100,
	}); err != nil {
		t.Fatalf("OnStopTransaction: %v", err)
	}
	if len(drv.applied) != 1 {
		t.Fatalf("0 A after StopTransaction must converge the suspend, applies=%d", len(drv.applied))
	}
	if !strings.Contains(reg.Format(), "lexa_ocpp_shadow_matches_total 1") {
		t.Errorf("suspend convergence must count as a match, got:\n%s", reg.Format())
	}
}

// TestBridge16_ReconnectReassertsStandingLimit: requirement 5 — a 1.6 charge
// point that drops and reconnects gets its standing limit re-sent through
// the IDENTICAL markReconnected → observe path as 2.0.1.
func TestBridge16_ReconnectReassertsStandingLimit(t *testing.T) {
	bridge, _, _ := newBridge16ForTest(t, []StationConfig{{ID: "cs-001", MaxCurrentA: 32, VoltageV: 230}})
	reg := metrics.New()
	drv := &recordingProfileDriver{}
	sh := newEVSEShell("cs-001", reg, modeActive, drv)
	bridge.shells = map[string]*evseShell{"cs-001": sh}
	f := &forwarder16{bridge: bridge}

	t0 := time.Now()
	sh.setDesired(evseDoc(10, 0, 1, t0), t0) // applied #1
	bridge.onConnect16(newFakeCPConn("cs-001", "10.0.0.9:9000"))
	// Next metered sample consumes the reconnect signal → reassert #2.
	if _, err := f.OnMeterValues("cs-001", &core16.MeterValuesRequest{
		ConnectorId: 1,
		MeterValue:  mv16(types16.SampledValue{Value: "10", Measurand: types16.MeasurandCurrentImport}),
	}); err != nil {
		t.Fatalf("OnMeterValues: %v", err)
	}
	if len(drv.applied) != 2 {
		t.Fatalf("1.6 reconnect must reassert the standing limit, applies=%d", len(drv.applied))
	}
}

// TestBridge_ApplyDispatchesPerProto: the proto tag routes bridge.Apply — a
// 1.6-tagged station takes the 1.6 SetChargingProfile shape
// (TxDefaultProfile, chargingRateUnit=A, single period, evseID 0→1); a
// 2.0.1-tagged station never touches the 1.6 sender; a disconnected 1.6
// station is the same silent no-op as 2.0.1.
func TestBridge_ApplyDispatchesPerProto(t *testing.T) {
	bridge, fake, _ := newBridge16ForTest(t, []StationConfig{
		{ID: "cp-16", MaxCurrentA: 32, VoltageV: 230},
		{ID: "cs-201", MaxCurrentA: 32, VoltageV: 230},
	})

	// 1.6 station: connect tags it, Apply routes to the 1.6 sender.
	bridge.onConnect16(newFakeCPConn("cp-16", "10.0.0.11:9000"))
	if err := bridge.Apply("cp-16", 0, 12); err != nil {
		t.Fatalf("Apply on 1.6 station: %v", err)
	}
	calls := fake.spCalls()
	if len(calls) != 1 {
		t.Fatalf("expected exactly one 1.6 SetChargingProfile, got %d", len(calls))
	}
	call := calls[0]
	if call.stationID != "cp-16" {
		t.Errorf("stationID = %q, want cp-16", call.stationID)
	}
	if call.connectorID != 1 {
		t.Errorf("connectorID = %d, want 1 (evseID 0→1, 2.0.1 contract mirrored)", call.connectorID)
	}
	p := call.profile
	if p.ChargingProfilePurpose != types16.ChargingProfilePurposeTxDefaultProfile {
		t.Errorf("purpose = %q, want TxDefaultProfile", p.ChargingProfilePurpose)
	}
	if p.ChargingProfileKind != types16.ChargingProfileKindAbsolute {
		t.Errorf("kind = %q, want Absolute", p.ChargingProfileKind)
	}
	if p.ChargingSchedule == nil || p.ChargingSchedule.ChargingRateUnit != types16.ChargingRateUnitAmperes {
		t.Fatalf("schedule = %+v, want chargingRateUnit=A", p.ChargingSchedule)
	}
	if n := len(p.ChargingSchedule.ChargingSchedulePeriod); n != 1 {
		t.Fatalf("schedule periods = %d, want 1", n)
	}
	if lim := p.ChargingSchedule.ChargingSchedulePeriod[0].Limit; lim != 12 {
		t.Errorf("period limit = %v, want 12", lim)
	}

	// 2.0.1 station: Apply must go down the 2.0.1 path (which errors here —
	// the test CSMS is never started) and must NOT touch the 1.6 sender.
	bridge.onConnect(newFakeCSConn("cs-201", "10.0.0.12:9000"))
	err := bridge.Apply("cs-201", 0, 12)
	if err == nil {
		t.Fatal("Apply on a 2.0.1 station against a never-started CSMS should error (proves 2.0.1 routing)")
	}
	if !strings.Contains(err.Error(), "SetChargingProfile cs-201") {
		t.Errorf("2.0.1-path error shape changed: %v", err)
	}
	if len(fake.spCalls()) != 1 {
		t.Errorf("2.0.1 Apply leaked into the 1.6 sender: %d calls", len(fake.spCalls()))
	}

	// Disconnected 1.6 station: silent no-op, no send.
	bridge.onDisconnect16(newFakeCPConn("cp-16", "10.0.0.11:9000"))
	if err := bridge.Apply("cp-16", 0, 8); err != nil {
		t.Fatalf("Apply on disconnected 1.6 station must be a silent no-op, got %v", err)
	}
	if len(fake.spCalls()) != 1 {
		t.Errorf("disconnected 1.6 station still got a SetChargingProfile: %d calls", len(fake.spCalls()))
	}
}

// TestBridge16_ApplyRejectedIsError: delivered-but-Rejected = error, L11
// semantics verbatim (the charger kept its previous limit — never a success).
func TestBridge16_ApplyRejectedIsError(t *testing.T) {
	bridge, fake, _ := newBridge16ForTest(t, []StationConfig{{ID: "cp-16", MaxCurrentA: 32, VoltageV: 230}})
	bridge.onConnect16(newFakeCPConn("cp-16", "10.0.0.13:9000"))

	fake.mu.Lock()
	fake.status = smartcharging16.ChargingProfileStatusRejected
	fake.mu.Unlock()
	err := bridge.Apply("cp-16", 0, 12)
	if err == nil {
		t.Fatal("rejected 1.6 profile must be an error, not success (L11)")
	}
	if !strings.Contains(err.Error(), "rejected") || !strings.Contains(err.Error(), "Rejected") {
		t.Errorf("error should surface the rejected status, got: %v", err)
	}

	// A transport error surfaces too.
	fake.mu.Lock()
	fake.status = ""
	fake.cbErr = errors.New("boom")
	fake.mu.Unlock()
	if err := bridge.Apply("cp-16", 0, 12); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("transport error must surface, got: %v", err)
	}
}

// TestBridge16_TriggerStatusNotification: the 1.6 TriggerMessage analog
// requests StatusNotification by feature name (1.6's MessageTrigger values
// are feature names).
func TestBridge16_TriggerStatusNotification(t *testing.T) {
	bridge, fake, _ := newBridge16ForTest(t, []StationConfig{{ID: "cp-16"}})

	bridge.triggerStatusNotification16("cp-16")

	trig := fake.trigCalls()
	if len(trig) != 1 {
		t.Fatalf("expected one TriggerMessage, got %d", len(trig))
	}
	if got, want := string(trig[0]), core16.StatusNotificationFeatureName; got != want {
		t.Errorf("requested message = %q, want %q", got, want)
	}
}

// Interface conformance pin: the production wiring (wireOCPP16) hands
// srv.CentralSystem() to bridge.cs16 — this line fails to compile if the
// narrowed centralSystem16 seam ever drifts from ocpp-go's real interface.
var _ centralSystem16 = (ocpp16.CentralSystem)(nil)
