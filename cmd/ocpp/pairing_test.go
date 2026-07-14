// WP-13 (D10): pairing-gate tests — both stacks driven through the REAL
// forwarder/bridge methods (fake conns, no live WebSocket handshake), the
// decision flow end-to-end (approve → persisted → next Boot Accepted; deny →
// Rejected; restart re-seed; malformed rejected), and the pinned per-message
// policy for Pending stations (StatusNotification recorded on the pending
// surface; TransactionEvent/MeterValues dropped). Config-resolution tests
// (gated product default, open bench default) ride along at the bottom.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	core16 "github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	types16 "github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
	ocpp2 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/availability"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/meter"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/provisioning"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/remotecontrol"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/smartcharging"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/transactions"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
)

// fakeSender201 records CSMS-initiated 2.0.1 sends through the
// centralSystem201 seam — the 2.0.1 sibling of fakeSender16
// (bridge16_test.go), answering callbacks asynchronously like the real
// library.
type fakeSender201 struct {
	mu          sync.Mutex
	sp          int
	clears      []clearCall201
	trig        []remotecontrol.MessageTrigger
	clearStatus smartcharging.ClearChargingProfileStatus // "" ⇒ Accepted
	cbErr       error
	callErr     error
}

type clearCall201 struct {
	stationID string
	req       *smartcharging.ClearChargingProfileRequest
}

func (f *fakeSender201) SetChargingProfile(stationID string, cb func(*smartcharging.SetChargingProfileResponse, error), _ int, _ *types.ChargingProfile, _ ...func(*smartcharging.SetChargingProfileRequest)) error {
	f.mu.Lock()
	f.sp++
	f.mu.Unlock()
	go cb(smartcharging.NewSetChargingProfileResponse(smartcharging.ChargingProfileStatusAccepted), nil)
	return nil
}

func (f *fakeSender201) ClearChargingProfile(stationID string, cb func(*smartcharging.ClearChargingProfileResponse, error), props ...func(*smartcharging.ClearChargingProfileRequest)) error {
	req := smartcharging.NewClearChargingProfileRequest()
	for _, p := range props {
		p(req)
	}
	f.mu.Lock()
	status := f.clearStatus
	if status == "" {
		status = smartcharging.ClearChargingProfileStatusAccepted
	}
	cbErr := f.cbErr
	callErr := f.callErr
	if callErr == nil {
		f.clears = append(f.clears, clearCall201{stationID: stationID, req: req})
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
		cb(smartcharging.NewClearChargingProfileResponse(status), nil)
	}()
	return nil
}

func (f *fakeSender201) TriggerMessage(_ string, cb func(*remotecontrol.TriggerMessageResponse, error), requestedMessage remotecontrol.MessageTrigger, _ ...func(*remotecontrol.TriggerMessageRequest)) error {
	f.mu.Lock()
	f.trig = append(f.trig, requestedMessage)
	f.mu.Unlock()
	go cb(remotecontrol.NewTriggerMessageResponse(remotecontrol.TriggerMessageStatusAccepted), nil)
	return nil
}

func (f *fakeSender201) trigCalls() []remotecontrol.MessageTrigger {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]remotecontrol.MessageTrigger(nil), f.trig...)
}

func (f *fakeSender201) clearCalls() []clearCall201 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]clearCall201(nil), f.clears...)
}

// newGatedBridge builds a bridge whose pairing gate runs in the given mode,
// with "cs-conf" configured (implicitly allowlisted) and an allowlist at
// path ("" ⇒ RAM-only).
func newGatedBridge(t *testing.T, mode, path string) (*mqttBridge, *metrics.Registry) {
	t.Helper()
	mc := &fakeMQTTClient{}
	reg := metrics.New()
	gauge := reg.Gauge("lexa_ocpp_pending_stations")
	csms := ocpp2.NewCSMS(nil, nil)
	stations := []StationConfig{{ID: "cs-conf", MaxCurrentA: 32, VoltageV: 230}}
	bridge := newMQTTBridge(mc, csms, stations, gauge)
	bridge.setStationConfig("cs-conf", 32, 230)
	bridge.gate = newPairingGate(mode, []string{"cs-conf"}, path, reg.Counter("lexa_ocpp_pairing_dropped_total"))
	return bridge, reg
}

func boot201(t *testing.T, b *mqttBridge, id string) provisioning.RegistrationStatus {
	t.Helper()
	prov := &provisioningForwarder{bridge: b}
	resp, err := prov.OnBootNotification(id, &provisioning.BootNotificationRequest{
		Reason:          "PowerUp",
		ChargingStation: provisioning.ChargingStationType{Model: "Fast50", VendorName: "AcmeEV"},
	})
	if err != nil {
		t.Fatalf("OnBootNotification(%s): %v", id, err)
	}
	return resp.Status
}

func boot16(t *testing.T, b *mqttBridge, id string) core16.RegistrationStatus {
	t.Helper()
	f := &forwarder16{bridge: b}
	resp, err := f.OnBootNotification(id, &core16.BootNotificationRequest{
		ChargePointModel: "Wall16", ChargePointVendor: "AcmeEV",
	})
	if err != nil {
		t.Fatalf("OnBootNotification16(%s): %v", id, err)
	}
	return resp.Status
}

func hasStation(b *mqttBridge, id string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.stations[id]
	return ok
}

// TestPairingGate_UnknownBootPending_201: gated mode, unknown 2.0.1 station —
// connect is not adopted (no stationState, no plant), Boot answers Pending,
// and the station is surfaced on the pending gauge/doc.
func TestPairingGate_UnknownBootPending_201(t *testing.T) {
	b, reg := newGatedBridge(t, PairingGated, "")

	b.onConnect(newFakeCSConn("cs-unknown", "10.0.0.7:9000"))
	if hasStation(b, "cs-unknown") {
		t.Fatal("gated unknown station must NOT be promoted to a stationState at connect")
	}
	if got := boot201(t, b, "cs-unknown"); got != provisioning.RegistrationStatusPending {
		t.Fatalf("unknown Boot status = %q, want Pending", got)
	}
	if hasStation(b, "cs-unknown") {
		t.Fatal("a Pending Boot must NOT create a stationState")
	}
	if !strings.Contains(reg.Format(), "lexa_ocpp_pending_stations 1") {
		t.Errorf("unknown station must be surfaced pending, got:\n%s", reg.Format())
	}
}

// TestPairingGate_UnknownBootPending_16 mirrors the 2.0.1 test on the 1.6
// stack — the SAME gate, no fork.
func TestPairingGate_UnknownBootPending_16(t *testing.T) {
	b, reg := newGatedBridge(t, PairingGated, "")

	b.onConnect16(fakeCPConn{id: "cp-unknown", addr: newFakeCSConn("cp-unknown", "10.0.0.8:9000").addr})
	if hasStation(b, "cp-unknown") {
		t.Fatal("gated unknown 1.6 station must NOT be promoted at connect")
	}
	if got := boot16(t, b, "cp-unknown"); got != core16.RegistrationStatusPending {
		t.Fatalf("unknown 1.6 Boot status = %q, want Pending", got)
	}
	if hasStation(b, "cp-unknown") {
		t.Fatal("a Pending 1.6 Boot must NOT create a stationState")
	}
	if !strings.Contains(reg.Format(), "lexa_ocpp_pending_stations 1") {
		t.Errorf("unknown 1.6 station must be surfaced pending, got:\n%s", reg.Format())
	}
}

// TestPairingGate_ConfiguredPreApproved: configured stations[] are implicitly
// allowlisted in gated mode — Accepted on both stacks, tracked as plant,
// never pending.
func TestPairingGate_ConfiguredPreApproved(t *testing.T) {
	b, reg := newGatedBridge(t, PairingGated, "")

	b.onConnect(newFakeCSConn("cs-conf", "10.0.0.9:9000"))
	if got := boot201(t, b, "cs-conf"); got != provisioning.RegistrationStatusAccepted {
		t.Fatalf("configured Boot status = %q, want Accepted", got)
	}
	if !hasStation(b, "cs-conf") {
		t.Fatal("configured station must be tracked as plant")
	}
	if got := boot16(t, b, "cs-conf"); got != core16.RegistrationStatusAccepted {
		t.Fatalf("configured 1.6 Boot status = %q, want Accepted", got)
	}
	if !strings.Contains(reg.Format(), "lexa_ocpp_pending_stations 0") {
		t.Errorf("configured station must never be pending, got:\n%s", reg.Format())
	}
}

// TestPairingGate_OpenModeUnchanged: open mode preserves the pre-WP-13
// behavior byte-for-byte — unknown stations are auto-adopted (tracked,
// Accepted) AND still surfaced on the Unit 6.1 pending doc.
func TestPairingGate_OpenModeUnchanged(t *testing.T) {
	b, reg := newGatedBridge(t, PairingOpen, "")

	b.onConnect(newFakeCSConn("cs-unknown", "10.0.0.10:9000"))
	if !hasStation(b, "cs-unknown") {
		t.Fatal("open mode must keep auto-adopting unknown stations (tracked-but-uncontrolled)")
	}
	if got := boot201(t, b, "cs-unknown"); got != provisioning.RegistrationStatusAccepted {
		t.Fatalf("open-mode Boot status = %q, want Accepted", got)
	}
	if got := boot16(t, b, "cp-unknown16"); got != core16.RegistrationStatusAccepted {
		t.Fatalf("open-mode 1.6 Boot status = %q, want Accepted", got)
	}
	// The gauge reflects the last PUBLISHED doc; the second upsert falls
	// inside pendingPublishMinInterval, so assert the first entry only (the
	// Unit 6.1 surface itself, not the rate limiter, is under test here).
	if !strings.Contains(reg.Format(), "lexa_ocpp_pending_stations 1") {
		t.Errorf("open mode keeps the Unit 6.1 pending surface, got:\n%s", reg.Format())
	}
}

// TestPairingGate_NilGateIsOpen: a bridge with no gate at all (pre-WP-13
// construction, every older test) behaves as open mode.
func TestPairingGate_NilGateIsOpen(t *testing.T) {
	mc := &fakeMQTTClient{}
	reg := metrics.New()
	csms := ocpp2.NewCSMS(nil, nil)
	b := newMQTTBridge(mc, csms, nil, reg.Gauge("lexa_ocpp_pending_stations"))

	b.onConnect(newFakeCSConn("cs-any", "10.0.0.11:9000"))
	if !hasStation(b, "cs-any") {
		t.Fatal("nil gate must auto-adopt (open-mode behavior)")
	}
	if got := boot201(t, b, "cs-any"); got != provisioning.RegistrationStatusAccepted {
		t.Fatalf("nil-gate Boot status = %q, want Accepted", got)
	}
}

// TestPairingGate_ApproveFlow: approve → persisted allowlist (0600) → next
// Boot Accepted + stationState created + resolved off the pending surface +
// a TriggerMessage(BootNotification) nudge on both stacks.
func TestPairingGate_ApproveFlow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ocpp-allowlist.json")
	b, reg := newGatedBridge(t, PairingGated, path)
	send201 := &fakeSender201{}
	send16 := &fakeSender16{}
	b.cs201 = send201
	b.cs16 = send16

	b.onConnect(newFakeCSConn("cs-new", "10.0.0.12:9000"))
	if got := boot201(t, b, "cs-new"); got != provisioning.RegistrationStatusPending {
		t.Fatalf("pre-approval Boot = %q, want Pending", got)
	}

	b.handlePairingDecision(bus.PairingDecision{StationID: "cs-new", Action: bus.PairingActionApprove, Actor: "local-api"}, time.Now())

	// Persisted, 0600, decodes back.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("allowlist not persisted: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("allowlist mode = %v, want 0600", fi.Mode().Perm())
	}
	var f allowlistFile
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("allowlist decode: %v", err)
	}
	if len(f.Approved) != 1 || f.Approved[0] != "cs-new" || len(f.Denied) != 0 {
		t.Errorf("allowlist = %+v, want approved [cs-new]", f)
	}

	// Next Boot: Accepted, station promoted to plant.
	if got := boot201(t, b, "cs-new"); got != provisioning.RegistrationStatusAccepted {
		t.Fatalf("post-approval Boot = %q, want Accepted", got)
	}
	if !hasStation(b, "cs-new") {
		t.Fatal("post-approval Boot must create the stationState (the just-approved promotion path)")
	}

	// Resolved off the pending surface.
	if !strings.Contains(reg.Format(), "lexa_ocpp_pending_stations 0") {
		t.Errorf("approved station must leave the pending surface, got:\n%s", reg.Format())
	}

	// Nudge fired on both stacks (async goroutine — poll briefly).
	deadline := time.Now().Add(2 * time.Second)
	for {
		t201, t16 := send201.trigCalls(), send16.trigCalls()
		if len(t201) == 1 && t201[0] == remotecontrol.MessageTriggerBootNotification &&
			len(t16) == 1 && string(t16[0]) == core16.BootNotificationFeatureName {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("approve must nudge BootNotification on both stacks; got 2.0.1=%v 1.6=%v", t201, t16)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestPairingGate_DenyFlow: deny → persisted → Boot answers Rejected on both
// stacks, no plant promotion, and the station leaves (and stays off) the
// pending surface.
func TestPairingGate_DenyFlow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ocpp-allowlist.json")
	b, reg := newGatedBridge(t, PairingGated, path)
	b.cs201 = &fakeSender201{}

	b.onConnect(newFakeCSConn("cs-bad", "10.0.0.13:9000"))
	b.handlePairingDecision(bus.PairingDecision{StationID: "cs-bad", Action: bus.PairingActionDeny}, time.Now())

	if got := boot201(t, b, "cs-bad"); got != provisioning.RegistrationStatusRejected {
		t.Fatalf("post-deny Boot = %q, want Rejected", got)
	}
	if got := boot16(t, b, "cs-bad"); got != core16.RegistrationStatusRejected {
		t.Fatalf("post-deny 1.6 Boot = %q, want Rejected", got)
	}
	if hasStation(b, "cs-bad") {
		t.Fatal("a denied station must never be promoted")
	}
	// Off the pending surface — the Boot upserts above must not re-add it.
	if !strings.Contains(reg.Format(), "lexa_ocpp_pending_stations 0") {
		t.Errorf("denied station must leave (and stay off) the pending surface, got:\n%s", reg.Format())
	}

	var f allowlistFile
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("allowlist decode: %v", err)
	}
	if len(f.Denied) != 1 || f.Denied[0] != "cs-bad" {
		t.Errorf("allowlist = %+v, want denied [cs-bad]", f)
	}
}

// TestPairingGate_RestartReseeds: a fresh gate over the same allowlist file
// re-derives approve/deny (crash-only re-seed) — and an approve of a
// previously denied station flips it.
func TestPairingGate_RestartReseeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ocpp-allowlist.json")
	g1 := newPairingGate(PairingGated, nil, path, nil)
	if err := g1.applyDecision(bus.PairingDecision{StationID: "cs-a", Action: bus.PairingActionApprove}); err != nil {
		t.Fatal(err)
	}
	if err := g1.applyDecision(bus.PairingDecision{StationID: "cs-b", Action: bus.PairingActionDeny}); err != nil {
		t.Fatal(err)
	}

	g2 := newPairingGate(PairingGated, nil, path, nil)
	if got := g2.verdict("cs-a"); got != bootAccept {
		t.Errorf("re-seeded verdict(cs-a) = %v, want accept", got)
	}
	if got := g2.verdict("cs-b"); got != bootReject {
		t.Errorf("re-seeded verdict(cs-b) = %v, want reject", got)
	}
	if got := g2.verdict("cs-c"); got != bootPending {
		t.Errorf("verdict(cs-c) = %v, want pending", got)
	}

	// approve-after-deny flips, persists, and re-seeds flipped.
	if err := g2.applyDecision(bus.PairingDecision{StationID: "cs-b", Action: bus.PairingActionApprove}); err != nil {
		t.Fatal(err)
	}
	g3 := newPairingGate(PairingGated, nil, path, nil)
	if got := g3.verdict("cs-b"); got != bootAccept {
		t.Errorf("approve-after-deny must re-seed as accept, got %v", got)
	}
}

// TestPairingGate_MalformedDecisionRejected: unknown actions, empty station
// IDs, and decisions about configured stations are rejected without changing
// gate state.
func TestPairingGate_MalformedDecisionRejected(t *testing.T) {
	g := newPairingGate(PairingGated, []string{"cs-conf"}, "", nil)
	cases := []bus.PairingDecision{
		{StationID: "cs-x", Action: "maybe"},
		{StationID: "cs-x", Action: ""},
		{StationID: "", Action: bus.PairingActionApprove},
		{StationID: "cs-conf", Action: bus.PairingActionDeny}, // configured: implicitly allowlisted
	}
	for _, d := range cases {
		if err := g.applyDecision(d); err == nil {
			t.Errorf("applyDecision(%+v) succeeded, want rejection", d)
		}
	}
	if got := g.verdict("cs-x"); got != bootPending {
		t.Errorf("verdict(cs-x) after rejected decisions = %v, want pending (unchanged)", got)
	}
	if got := g.verdict("cs-conf"); got != bootAccept {
		t.Errorf("verdict(cs-conf) = %v, want accept (configured must stay allowlisted)", got)
	}
}

// TestPairingGate_CorruptAllowlistFailsClosed: an unparsable allowlist file
// starts the gate EMPTY (fail-closed: previously approved stations pend
// again) rather than crashing or silently admitting anyone.
func TestPairingGate_CorruptAllowlistFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ocpp-allowlist.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := newPairingGate(PairingGated, nil, path, nil)
	if got := g.verdict("cs-a"); got != bootPending {
		t.Errorf("corrupt allowlist must yield an empty (all-pending) gate, verdict = %v", got)
	}
}

// TestPairingGate_PersistFailureHoldsInRAM: an unwritable allowlist path
// (missing state dir — the V1RC finding D case) does not reject the
// decision: it takes effect in RAM (tolerate-and-alarm), it just won't
// survive a restart.
func TestPairingGate_PersistFailureHoldsInRAM(t *testing.T) {
	// A path whose parent is a FILE cannot be MkdirAll'd — a portable stand-in
	// for the unprovisioned /var/lib/lexa permission failure.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := newPairingGate(PairingGated, nil, filepath.Join(blocker, "ocpp-allowlist.json"), nil)
	if err := g.applyDecision(bus.PairingDecision{StationID: "cs-a", Action: bus.PairingActionApprove}); err != nil {
		t.Fatalf("a persist failure must not reject the decision: %v", err)
	}
	if got := g.verdict("cs-a"); got != bootAccept {
		t.Errorf("verdict(cs-a) = %v, want accept (RAM-only approval)", got)
	}
}

// TestPairingGate_PendingMessagePolicy_201 pins D10's per-message policy on
// the 2.0.1 stack for a Pending station: StatusNotification is recorded on
// the pending surface only (no connector fold, no plant), MeterValues and
// TransactionEvent are dropped (counted; transactions never counted; no
// stationState ever appears).
func TestPairingGate_PendingMessagePolicy_201(t *testing.T) {
	b, reg := newGatedBridge(t, PairingGated, "")
	txCtr := reg.Counter("lexa_ocpp_transactions_total")
	b.transactionsTotal = txCtr

	b.onConnect(newFakeCSConn("cs-pend", "10.0.0.14:9000"))

	avail := &availForwarder{bridge: b}
	statusReq := availability.NewStatusNotificationRequest(types.NewDateTime(time.Now()), availability.ConnectorStatusAvailable, 1, 1)
	if _, err := avail.OnStatusNotification("cs-pend", statusReq); err != nil {
		t.Fatal(err)
	}
	if hasStation(b, "cs-pend") {
		t.Fatal("StatusNotification from a Pending station must not fold into plant state")
	}
	if !strings.Contains(reg.Format(), "lexa_ocpp_pending_stations 1") {
		t.Errorf("StatusNotification must be recorded on the pending surface, got:\n%s", reg.Format())
	}

	mf := &meterForwarder{bridge: b}
	if _, err := mf.OnMeterValues("cs-pend", &meter.MeterValuesRequest{EvseID: 1}); err != nil {
		t.Fatal(err)
	}
	tf := &txForwarder{bridge: b}
	if _, err := tf.OnTransactionEvent("cs-pend", &transactions.TransactionEventRequest{
		EventType: transactions.TransactionEventStarted,
	}); err != nil {
		t.Fatal(err)
	}
	if hasStation(b, "cs-pend") {
		t.Fatal("MeterValues/TransactionEvent from a Pending station must not create plant state")
	}
	got := reg.Format()
	if !strings.Contains(got, "lexa_ocpp_pairing_dropped_total 2") {
		t.Errorf("both drops must be counted, got:\n%s", got)
	}
	if !strings.Contains(got, "lexa_ocpp_transactions_total 0") {
		t.Errorf("a Pending station's transaction must not count, got:\n%s", got)
	}
}

// TestPairingGate_PendingMessagePolicy_16 pins the same policy on the 1.6
// stack: StartTransaction answers Invalid (nothing folds, nothing counts),
// MeterValues and StopTransaction are dropped.
func TestPairingGate_PendingMessagePolicy_16(t *testing.T) {
	b, reg := newGatedBridge(t, PairingGated, "")
	b.transactionsTotal = reg.Counter("lexa_ocpp_transactions_total")

	f := &forwarder16{bridge: b}
	conf, err := f.OnStartTransaction("cp-pend", &core16.StartTransactionRequest{ConnectorId: 1, IdTag: "TAG-1", MeterStart: 100})
	if err != nil {
		t.Fatal(err)
	}
	if conf.IdTagInfo.Status != types16.AuthorizationStatusInvalid {
		t.Fatalf("StartTransaction from a Pending 1.6 station = %q, want Invalid", conf.IdTagInfo.Status)
	}
	if _, err := f.OnMeterValues("cp-pend", &core16.MeterValuesRequest{ConnectorId: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.OnStopTransaction("cp-pend", &core16.StopTransactionRequest{TransactionId: 1, MeterStop: 200}); err != nil {
		t.Fatal(err)
	}
	if hasStation(b, "cp-pend") {
		t.Fatal("1.6 transaction/meter traffic from a Pending station must not create plant state")
	}
	got := reg.Format()
	if !strings.Contains(got, "lexa_ocpp_transactions_total 0") {
		t.Errorf("a Pending 1.6 station's transaction must not count, got:\n%s", got)
	}
	if !strings.Contains(got, "lexa_ocpp_pairing_dropped_total 3") {
		t.Errorf("all three 1.6 drops must be counted, got:\n%s", got)
	}

	// 1.6 StatusNotification: recorded on the pending surface, not folded.
	if _, err := f.OnStatusNotification("cp-pend", &core16.StatusNotificationRequest{ConnectorId: 1, Status: core16.ChargePointStatusAvailable}); err != nil {
		t.Fatal(err)
	}
	if hasStation(b, "cp-pend") {
		t.Fatal("1.6 StatusNotification from a Pending station must not fold")
	}
	if !strings.Contains(reg.Format(), "lexa_ocpp_pending_stations 1") {
		t.Errorf("1.6 StatusNotification must be recorded pending, got:\n%s", reg.Format())
	}
}

// ── pairing_mode config resolution (WP-13) ───────────────────────────────────

func TestLoadConfig_PairingMode_ProductDefaultGated(t *testing.T) {
	path := writeTempConfig(t, `{
		"reconciler": "active",
		"cert_path": "/c.pem", "key_path": "/k.pem",
		"basic_auth_user": "u", "basic_auth_pass": "p",
		"stations": [{"id": "cs-001"}]
	}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PairingMode != PairingGated {
		t.Errorf("product pairing_mode default = %q, want gated (fail-closed)", cfg.PairingMode)
	}
	if cfg.AllowlistPath != defaultAllowlistPath {
		t.Errorf("allowlist_path default = %q, want %q", cfg.AllowlistPath, defaultAllowlistPath)
	}
}

func TestLoadConfig_PairingMode_BenchDefaultOpen(t *testing.T) {
	path := writeTempConfig(t, `{"reconciler": "active", "bench": true}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PairingMode != PairingOpen {
		t.Errorf("bench pairing_mode default = %q, want open (preserve every bench flow)", cfg.PairingMode)
	}
}

func TestLoadConfig_PairingMode_BenchEnvDefaultOpen(t *testing.T) {
	t.Setenv("OCPP_PROFILE", "bench")
	path := writeTempConfig(t, `{"reconciler": "active"}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PairingMode != PairingOpen {
		t.Errorf("OCPP_PROFILE=bench pairing_mode default = %q, want open", cfg.PairingMode)
	}
}

func TestLoadConfig_PairingMode_ExplicitWinsOverBench(t *testing.T) {
	path := writeTempConfig(t, `{"reconciler": "active", "bench": true, "pairing_mode": "gated"}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PairingMode != PairingGated {
		t.Errorf("explicit pairing_mode under bench = %q, want gated (explicit always wins)", cfg.PairingMode)
	}
}

func TestLoadConfig_PairingMode_UnknownValueFails(t *testing.T) {
	path := writeTempConfig(t, `{"reconciler": "active", "bench": true, "pairing_mode": "sometimes"}`)
	if _, err := loadConfig(path); err == nil {
		t.Fatal("unknown pairing_mode must fail loud")
	}
}
