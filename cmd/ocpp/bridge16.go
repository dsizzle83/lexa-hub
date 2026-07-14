// WP-12: OCPP 1.6J forwarders for the dual-stack CSMS.
//
// lexa-ocpp is 2.0.1-native; this file is the 1.6J compatibility mode. The
// second listener (ocppserver16.Server on cfg.Port16, wired by main.go) feeds
// the SAME mqttBridge/stationState the 2.0.1 forwarders feed — one state map,
// station tagged with its proto at boot/connect (architecture §5(c)):
//
//   - BootNotification        → pending-station upsert (vendor/model) + proto
//     tag; response auto-Accepted/60 s heartbeat, mirroring the 2.0.1
//     provisioningForwarder. The pairing gate is deliberately NOT built here
//     (WP-13/D10): both stacks keep today's auto-accept behavior.
//   - StatusNotification      → connector map, with the 1.6 fine-grained
//     ChargePointStatus values folded into the 2.0.1 status vocabulary
//     (mapStatus16) so lexa/evse/{station}/state carries one vocabulary.
//   - Start/StopTransaction   → the TransactionEvent-shaped session lifecycle
//     (OCPP-1 invariant: the transaction lifecycle is the carrier, never bare
//     MeterValues). StartTransaction is Started (transactions counter, CSMS-
//     assigned tx ID); StopTransaction is Ended (zeroes current so site power
//     drops immediately and the 0 A suspend case converges).
//   - MeterValues             → applySamples16Locked, reusing the SAME
//     plausibility gate (implausibleCurrent, L11 reject-keep-last-good) and
//     measurand handling as the 2.0.1 fold. 1.6 differences handled here:
//     SampledValue.Value is a STRING (parsed, non-finite rejected); there is
//     no UnitOfMeasure multiplier — energy scaling is the Unit field ("Wh"
//     default, "kWh" ×1000); a blank measurand defaults to
//     Energy.Active.Import.Register per the 1.6 spec.
//
// CSMS-initiated sends dispatch off stationState.proto in bridge.Apply
// (main.go): protoOCPP16 lands in apply16 below — SetChargingProfile with a
// TxDefaultProfile, chargingRateUnit=A, single period, 10 s bound,
// delivered-but-Rejected = error — the 2.0.1 contract (ledger L11) verbatim.
// Reassert-on-reconnect works identically: onConnect16 signals the station's
// reconciler shell exactly like the 2.0.1 onConnect (markReconnected), and
// re-triggers a StatusNotification (the 1.6 TriggerMessage analog of the
// 2.0.1 path).
package main

import (
	"fmt"
	"log"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	ocpp16 "github.com/lorenzodonini/ocpp-go/ocpp1.6"
	core16 "github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	remotetrigger16 "github.com/lorenzodonini/ocpp-go/ocpp1.6/remotetrigger"
	smartcharging16 "github.com/lorenzodonini/ocpp-go/ocpp1.6/smartcharging"
	types16 "github.com/lorenzodonini/ocpp-go/ocpp1.6/types"

	"lexa-proto/ocppserver16"
)

// centralSystem16 is the slice of ocpp16.CentralSystem the bridge needs for
// CSMS-initiated 1.6 sends (bridge.cs16). Narrowed to an interface so
// bridge16_test.go can record SetChargingProfile/TriggerMessage calls without
// a live WebSocket server — mirroring how the 2.0.1 tests drive the named
// handler methods directly. Production always wires srv.CentralSystem()
// (wireOCPP16 below).
type centralSystem16 interface {
	SetChargingProfile(clientID string, callback func(*smartcharging16.SetChargingProfileConfirmation, error), connectorID int, chargingProfile *types16.ChargingProfile, props ...func(request *smartcharging16.SetChargingProfileRequest)) error
	// ClearChargingProfile (WP-13, B3): the release path. Satisfied by the
	// full ocpp16.CentralSystem the vendored ocppserver16.Server.
	// CentralSystem() already exposes — no vendored-package change needed.
	ClearChargingProfile(clientID string, callback func(*smartcharging16.ClearChargingProfileConfirmation, error), props ...func(request *smartcharging16.ClearChargingProfileRequest)) error
	TriggerMessage(clientID string, callback func(*remotetrigger16.TriggerMessageConfirmation, error), requestedMessage remotetrigger16.MessageTrigger, props ...func(request *remotetrigger16.TriggerMessageRequest)) error
}

// wireOCPP16 attaches the 1.6J listener to the shared bridge: consumer
// callbacks via the ocppserver16 Handlers seam, connect/disconnect handlers
// (overriding the package's log-only defaults), and the send seam
// (bridge.cs16). Must be called before srv.Start() AND before the 2.0.1
// listener starts — SetHandlers is unsynchronized by contract, and cs16 is
// read lock-free by Apply.
func wireOCPP16(b *mqttBridge, srv *ocppserver16.Server) {
	f := &forwarder16{bridge: b}
	srv.SetHandlers(ocppserver16.Handlers{
		OnBootNotification:   f.OnBootNotification,
		OnStatusNotification: f.OnStatusNotification,
		OnStartTransaction:   f.OnStartTransaction,
		OnStopTransaction:    f.OnStopTransaction,
		OnMeterValues:        f.OnMeterValues,
	})
	cs := srv.CentralSystem()
	b.cs16 = cs
	cs.SetNewChargePointHandler(b.onConnect16)
	cs.SetChargePointDisconnectedHandler(b.onDisconnect16)
}

// getOrCreate16Locked is getOrCreateLocked for a station heard on the 1.6J
// listener: identical create/pending semantics, plus (re)stamping the proto
// tag. Every 1.6 entry point routes through this so bridge.Apply dispatches
// the 1.6 profile shape even when a StatusNotification or MeterValues — not
// a connect — is the first thing this bridge hears of a station (the same
// defensive symmetry as the 2.0.1 "OnStatusNotification twin"). Caller must
// hold mu for writing.
func (b *mqttBridge) getOrCreate16Locked(id string) (*stationState, bool) {
	s, created := b.getOrCreateLocked(id)
	s.proto = protoOCPP16
	return s, created
}

// onConnect16 mirrors onConnect for a 1.6 charge point: track + tag the
// station, surface unconfigured ones as pending, signal the reconciler shell
// (reassert-on-reconnect — the SAME markReconnected path as 2.0.1, so a 1.6
// charger that drops and returns gets its standing limit re-sent immediately),
// publish state, and re-trigger a StatusNotification.
func (b *mqttBridge) onConnect16(cp ocpp16.ChargePointConnection) {
	// WP-13/D10: the SAME pairing gate as the 2.0.1 onConnect — a
	// non-approved station is surfaced pending, never promoted to plant.
	if !b.gate.allowed(cp.ID()) {
		log.Printf("[ocpp16] connected (pairing-gated, not adopted): %s addr=%s", cp.ID(), cp.RemoteAddr())
		b.pending.upsert(cp.ID(), "", "", cp.RemoteAddr().String(), time.Now())
		return
	}
	b.mu.Lock()
	s, created := b.getOrCreate16Locked(cp.ID())
	s.connected = true
	b.mu.Unlock()
	log.Printf("[ocpp16] connected: %s addr=%s", cp.ID(), cp.RemoteAddr())
	if created {
		b.pending.upsert(cp.ID(), "", "", cp.RemoteAddr().String(), time.Now())
	}
	if sh, ok := b.shells[cp.ID()]; ok {
		sh.markReconnected()
	}
	b.publishAll(cp.ID())
	go b.triggerStatusNotification16(cp.ID())
}

// onDisconnect16 mirrors onDisconnect.
func (b *mqttBridge) onDisconnect16(cp ocpp16.ChargePointConnection) {
	b.mu.Lock()
	if s, ok := b.stations[cp.ID()]; ok {
		s.connected = false
	}
	b.mu.Unlock()
	log.Printf("[ocpp16] disconnected: %s", cp.ID())
	b.publishAll(cp.ID())
}

// triggerStatusNotification16 is the 1.6 TriggerMessage analog of
// triggerStatusNotification: in 1.6 the requested message is the feature
// name itself (remotetrigger16.MessageTrigger's valid values are feature
// names), so StatusNotification is requested by name.
func (b *mqttBridge) triggerStatusNotification16(stationID string) {
	time.Sleep(500 * time.Millisecond)
	_ = b.cs16.TriggerMessage(
		stationID,
		func(_ *remotetrigger16.TriggerMessageConfirmation, _ error) {},
		remotetrigger16.MessageTrigger(core16.StatusNotificationFeatureName),
	)
}

// apply16 is the 1.6 half of bridge.Apply's per-proto dispatch: a
// TxDefaultProfile capping connectorID at limitA, chargingRateUnit=A, one
// schedule period — the same profile shape as the 2.0.1 path, in 1.6 types.
// The contract is the 2.0.1 contract verbatim (ledger L11): the call is
// bounded at 10 s, and a delivered-but-REJECTED (or NotSupported) profile is
// an error, not success — the charger kept its previous limit. The caller
// (Apply) has already handled the disconnected no-op and the evseID 0→1
// mapping, so both stacks share those exactly.
func (b *mqttBridge) apply16(stationID string, connectorID int, limitA float64) error {
	period := types16.NewChargingSchedulePeriod(0, limitA)
	schedule := types16.NewChargingSchedule(types16.ChargingRateUnitAmperes, period)
	profile := types16.NewChargingProfile(
		1, 0,
		types16.ChargingProfilePurposeTxDefaultProfile,
		types16.ChargingProfileKindAbsolute,
		schedule,
	)
	type spResult struct {
		status smartcharging16.ChargingProfileStatus
		err    error
	}
	resCh := make(chan spResult, 1)
	callErr := b.cs16.SetChargingProfile(
		stationID,
		func(resp *smartcharging16.SetChargingProfileConfirmation, err error) {
			r := spResult{err: err}
			if resp != nil {
				r.status = resp.Status
			}
			resCh <- r
		},
		connectorID, profile,
	)
	if callErr != nil {
		return fmt.Errorf("SetChargingProfile(1.6) %s connector=%d call failed: %w", stationID, connectorID, callErr)
	}
	t := time.NewTimer(10 * time.Second)
	defer t.Stop()
	select {
	case r := <-resCh:
		if r.err != nil {
			return fmt.Errorf("SetChargingProfile(1.6) %s connector=%d failed: %w", stationID, connectorID, r.err)
		}
		if r.status != smartcharging16.ChargingProfileStatusAccepted {
			return fmt.Errorf("SetChargingProfile(1.6) %s connector=%d rejected: status=%q", stationID, connectorID, r.status)
		}
		return nil
	case <-t.C:
		return fmt.Errorf("SetChargingProfile(1.6) %s connector=%d timed out after 10s", stationID, connectorID)
	}
}

// applyClear16 is the 1.6 half of bridge.ApplyClear's per-proto dispatch
// (WP-13, B3): remove the TxDefaultProfile Apply installed on connectorID.
// Same contract as the 2.0.1 path (see ApplyClear's doc): 10 s bound,
// Accepted OR Unknown (nothing to clear ⇒ already released) is success, any
// other status / transport error / timeout is an error, L11-style. The
// caller has already handled the disconnected no-op and connectorID 0→1.
func (b *mqttBridge) applyClear16(stationID string, connectorID int) error {
	type ccpResult struct {
		status smartcharging16.ClearChargingProfileStatus
		err    error
	}
	resCh := make(chan ccpResult, 1)
	callErr := b.cs16.ClearChargingProfile(
		stationID,
		func(resp *smartcharging16.ClearChargingProfileConfirmation, err error) {
			r := ccpResult{err: err}
			if resp != nil {
				r.status = resp.Status
			}
			resCh <- r
		},
		func(req *smartcharging16.ClearChargingProfileRequest) {
			// Clear exactly what apply16 installs: the TxDefaultProfile on
			// this connector — never a blanket clear.
			id := connectorID
			req.ConnectorId = &id
			req.ChargingProfilePurpose = types16.ChargingProfilePurposeTxDefaultProfile
		},
	)
	if callErr != nil {
		return fmt.Errorf("ClearChargingProfile(1.6) %s connector=%d call failed: %w", stationID, connectorID, callErr)
	}
	t := time.NewTimer(10 * time.Second)
	defer t.Stop()
	select {
	case r := <-resCh:
		if r.err != nil {
			return fmt.Errorf("ClearChargingProfile(1.6) %s connector=%d failed: %w", stationID, connectorID, r.err)
		}
		if r.status != smartcharging16.ClearChargingProfileStatusAccepted &&
			r.status != smartcharging16.ClearChargingProfileStatusUnknown {
			return fmt.Errorf("ClearChargingProfile(1.6) %s connector=%d rejected: status=%q", stationID, connectorID, r.status)
		}
		return nil
	case <-t.C:
		return fmt.Errorf("ClearChargingProfile(1.6) %s connector=%d timed out after 10s", stationID, connectorID)
	}
}

// ── 1.6 handler forwarders ───────────────────────────────────────────────────

// forwarder16 carries the five seam-covered 1.6 Core messages onto the shared
// bridge (registered via ocppserver16.Handlers in wireOCPP16).
type forwarder16 struct{ bridge *mqttBridge }

// OnBootNotification mirrors the 2.0.1 provisioningForwarder: capture
// vendor/model for the pending-station surface, answer with the SAME pairing
// gate's verdict (WP-13/D10 — one gate, both stacks: Accepted for
// configured/approved stations and everything in open mode, Pending for an
// unknown station in gated mode, Rejected for a persisted deny), and — only
// when accepted — ensure the stationState exists with its proto tag stamped
// (WP-12: "set at boot/connect"; a gated station must never be promoted).
func (f *forwarder16) OnBootNotification(cpID string, req *core16.BootNotificationRequest) (*core16.BootNotificationConfirmation, error) {
	f.bridge.pending.upsert(cpID, req.ChargePointVendor, req.ChargePointModel, "", time.Now())
	status := core16.RegistrationStatusAccepted
	switch f.bridge.gate.verdict(cpID) {
	case bootPending:
		status = core16.RegistrationStatusPending
	case bootReject:
		status = core16.RegistrationStatusRejected
	default:
		f.bridge.mu.Lock()
		s, created := f.bridge.getOrCreate16Locked(cpID)
		if created {
			// Just-approved case: onConnect16 ran while still gated, so no
			// stationState exists yet. The boot arrived on a live socket by
			// construction, so connected=true is truthful. A normal boot
			// (station already tracked) leaves state byte-identical.
			s.connected = true
		}
		f.bridge.mu.Unlock()
	}
	log.Printf("[ocpp16] BootNotification cs=%s model=%s vendor=%s status=%s",
		cpID, req.ChargePointModel, req.ChargePointVendor, status)
	return core16.NewBootNotificationConfirmation(
		types16.NewDateTime(time.Now()),
		60, // heartbeat interval in seconds — same as the 2.0.1 forwarder
		status,
	), nil
}

// mapStatus16 folds the 1.6 fine-grained ChargePointStatus into the 2.0.1
// status vocabulary the bridge (and lexa/evse/{station}/state) already
// carries: everything from "an EV is attached" through "session winding
// down" is what 2.0.1 collapses into Occupied — which is also what keeps
// EVSEState.SessionActive (status == Occupied, publishAll) meaning the same
// thing on both stacks. Reserved passes through verbatim: it is a valid
// 2.0.1 ConnectorStatus too (the 2.0.1 forwarder stores it untranslated), so
// the published vocabulary stays within the 2.0.1 set either way.
func mapStatus16(st core16.ChargePointStatus) connectorStatus {
	switch st {
	case core16.ChargePointStatusAvailable:
		return statusAvailable
	case core16.ChargePointStatusPreparing, core16.ChargePointStatusCharging,
		core16.ChargePointStatusSuspendedEV, core16.ChargePointStatusSuspendedEVSE,
		core16.ChargePointStatusFinishing:
		return statusOccupied
	case core16.ChargePointStatusFaulted:
		return statusFaulted
	case core16.ChargePointStatusUnavailable:
		return statusUnavailable
	default:
		return connectorStatus(st)
	}
}

// OnStatusNotification mirrors the 2.0.1 availForwarder, through mapStatus16.
func (f *forwarder16) OnStatusNotification(cpID string, req *core16.StatusNotificationRequest) (*core16.StatusNotificationConfirmation, error) {
	// WP-13/D10: recorded on the pending surface only, never folded — the
	// same policy as the 2.0.1 availForwarder.
	if !f.bridge.gate.allowed(cpID) {
		f.bridge.pending.upsert(cpID, "", "", "", time.Now())
		return core16.NewStatusNotificationConfirmation(), nil
	}
	status := mapStatus16(req.Status)
	f.bridge.mu.Lock()
	s, created := f.bridge.getOrCreate16Locked(cpID)
	s.connectors[req.ConnectorId] = &connState{connectorID: req.ConnectorId, status: status}
	f.bridge.mu.Unlock()
	if created {
		f.bridge.pending.upsert(cpID, "", "", "", time.Now())
	}
	log.Printf("[ocpp16] StatusNotification cs=%s connector=%d status=%s (raw=%s errorCode=%s)",
		cpID, req.ConnectorId, status, req.Status, req.ErrorCode)
	f.bridge.publishAll(cpID)
	return core16.NewStatusNotificationConfirmation(), nil
}

// applySamples16Locked folds 1.6 sampled values into the station state — the
// 1.6 sibling of applySamplesLocked, same measurands, same L11 plausibility
// gate. 1.6 differences (see the file doc): Value is a string (a non-numeric
// or non-finite value is rejected keep-last-good — the decode layer's
// NaN-never-enters-state posture, which 2.0.1 gets for free from typed JSON
// floats); energy scaling is the Unit field ("Wh" default, "kWh" ×1000)
// rather than a power-of-ten multiplier; a blank measurand is
// Energy.Active.Import.Register per the 1.6 spec default.
// Caller must hold bridge.mu for writing.
func applySamples16Locked(s *stationState, meterValues []types16.MeterValue) {
	for _, mv := range meterValues {
		for _, sv := range mv.SampledValue {
			v, err := strconv.ParseFloat(strings.TrimSpace(sv.Value), 64)
			if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
				log.Printf("[ocpp16] REJECT non-numeric MeterValues value %q (measurand %q) on %s — keeping last good",
					sv.Value, sv.Measurand, s.id)
				continue
			}
			switch sv.Measurand {
			case types16.MeasurandCurrentImport:
				if implausibleCurrent(v, s.maxCurrentA) {
					log.Printf("[ocpp16] REJECT implausible MeterValues current %.1fA on %s (station max %.1fA) — keeping last good %.1fA",
						v, s.id, s.maxCurrentA, s.currentA)
					continue
				}
				s.currentA = v
			case types16.MeasurandSoC:
				s.soc = v
			case "", types16.MeasurandEnergyActiveImportRegister:
				if sv.Unit == types16.UnitOfMeasureKWh {
					v *= 1000
				}
				s.energyWh = v
			case types16.MeasurandVoltage:
				if v > 0 {
					s.voltageV = v
				}
			}
		}
	}
}

// OnMeterValues mirrors the 2.0.1 meterForwarder: fold under mu, feed the
// reconciler shell outside mu (post-L11, so by construction plausible), log
// at Debug (steady-state, per the TASK-045 demotion table), publish.
func (f *forwarder16) OnMeterValues(cpID string, req *core16.MeterValuesRequest) (*core16.MeterValuesConfirmation, error) {
	// WP-13/D10: dropped (edge log + counter) from a non-approved station.
	if !f.bridge.gate.permitOrLogDrop(cpID, "MeterValues(1.6)") {
		return core16.NewMeterValuesConfirmation(), nil
	}
	f.bridge.mu.Lock()
	s, _ := f.bridge.getOrCreate16Locked(cpID)
	applySamples16Locked(s, req.MeterValue)
	currentA, soc, energyWh := s.currentA, s.soc, s.energyWh
	connected, maxA := s.connected, s.maxCurrentA
	f.bridge.mu.Unlock()
	f.bridge.observeShell(cpID, currentA, maxA, connected)
	slog.Debug("[ocpp16] MeterValues", "cs", cpID, "connector", req.ConnectorId,
		"current_a", currentA, "soc_pct", soc, "energy_wh", energyWh)
	f.bridge.publishAll(cpID)
	return core16.NewMeterValuesConfirmation(), nil
}

// OnStartTransaction is the 1.6 analog of TransactionEvent Started: count the
// transaction, assign the CSMS-side transaction ID (the Central System owns
// IDs in 1.6), and fold MeterStart — the Wh energy register at session start,
// i.e. the same quantity as an Energy.Active.Import.Register sample — into
// the station state. Session lifecycle is driven from here, never inferred
// from bare MeterValues (OCPP-1 invariant, ocppserver16 CLAUDE.md).
func (f *forwarder16) OnStartTransaction(cpID string, req *core16.StartTransactionRequest) (*core16.StartTransactionConfirmation, error) {
	// WP-13/D10: no transactions from a non-approved station. 1.6 requires a
	// confirmation with an IdTagInfo and a transaction ID — answer Invalid
	// (the 1.6 analog of D10's "Authorize → Invalid where used") with a real
	// CSMS-assigned ID so a nonconforming charger's later StopTransaction
	// still references something well-formed; nothing folds, nothing counts.
	if !f.bridge.gate.permitOrLogDrop(cpID, "StartTransaction(1.6)") {
		txID := int(atomic.AddInt32(&f.bridge.nextTxID16, 1))
		return core16.NewStartTransactionConfirmation(
			types16.NewIdTagInfo(types16.AuthorizationStatusInvalid), txID), nil
	}
	f.bridge.transactionsTotal.Inc() // lexa_ocpp_transactions_total (TASK-044)
	txID := int(atomic.AddInt32(&f.bridge.nextTxID16, 1))
	f.bridge.mu.Lock()
	s, _ := f.bridge.getOrCreate16Locked(cpID)
	s.energyWh = float64(req.MeterStart)
	currentA, soc, energyWh := s.currentA, s.soc, s.energyWh
	connected, maxA := s.connected, s.maxCurrentA
	f.bridge.mu.Unlock()
	f.bridge.observeShell(cpID, currentA, maxA, connected)
	// Started/Ended are real session lifecycle edges — Info, like the 2.0.1
	// TransactionEvent forwarder (TASK-045).
	slog.Info("[ocpp16] StartTransaction", "cs", cpID, "connector", req.ConnectorId,
		"id_tag", req.IdTag, "tx", txID, "meter_start_wh", req.MeterStart,
		"current_a", currentA, "soc_pct", soc, "energy_wh", energyWh)
	f.bridge.publishAll(cpID)
	return core16.NewStartTransactionConfirmation(
		types16.NewIdTagInfo(types16.AuthorizationStatusAccepted), txID), nil
}

// OnStopTransaction is the 1.6 analog of TransactionEvent Ended: fold the
// final TransactionData samples, take MeterStop as the closing energy
// register, and ZERO the current so site power drops immediately instead of
// holding the last sample — which is also what lets a commanded 0 A suspend
// converge through observeShell (the same forced-measured-0 contract as
// TransactionEvent Ended, CLAUDE.md's EVSE reconciler section).
func (f *forwarder16) OnStopTransaction(cpID string, req *core16.StopTransactionRequest) (*core16.StopTransactionConfirmation, error) {
	// WP-13/D10: dropped (edge log + counter) from a non-approved station.
	if !f.bridge.gate.permitOrLogDrop(cpID, "StopTransaction(1.6)") {
		return core16.NewStopTransactionConfirmation(), nil
	}
	f.bridge.mu.Lock()
	s, _ := f.bridge.getOrCreate16Locked(cpID)
	applySamples16Locked(s, req.TransactionData)
	s.energyWh = float64(req.MeterStop)
	s.currentA = 0
	currentA, soc, energyWh := s.currentA, s.soc, s.energyWh
	connected, maxA := s.connected, s.maxCurrentA
	f.bridge.mu.Unlock()
	f.bridge.observeShell(cpID, currentA, maxA, connected)
	slog.Info("[ocpp16] StopTransaction", "cs", cpID, "tx", req.TransactionId,
		"meter_stop_wh", req.MeterStop, "reason", req.Reason,
		"current_a", currentA, "soc_pct", soc, "energy_wh", energyWh)
	f.bridge.publishAll(cpID)
	return core16.NewStopTransactionConfirmation(), nil
}
