package main

import (
	"math"
	"sync"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/northbound/model"
	"lexa-hub/internal/orchestrator"
)

// MQTTSystemReader implements orchestrator.SystemReader by maintaining a
// snapshot of all device state populated via MQTT subscriptions.
type MQTTSystemReader struct {
	mu sync.RWMutex

	// Per-device last measurement (device name → value)
	lastMeas map[string]measSnapshot

	// Per-device last battery metrics
	lastBattMet map[string]bus.BattMetrics

	// Per-station EVSE state (station ID → last state)
	lastEVSE map[string]bus.EVSEState

	// Last resolved CSIP active control from lexa-csip
	lastCSIP     *bus.ActiveControl
	clockOffset  int64

	// Device configuration for role/capacity lookup
	devices  []DeviceConfig
	devByName map[string]*DeviceConfig
}

type measSnapshot struct {
	W  float64 // NaN if not received
	V  float64
	Hz float64
}

func newMQTTSystemReader(devices []DeviceConfig) *MQTTSystemReader {
	r := &MQTTSystemReader{
		lastMeas:    make(map[string]measSnapshot),
		lastBattMet: make(map[string]bus.BattMetrics),
		lastEVSE:    make(map[string]bus.EVSEState),
		devices:     devices,
		devByName:   make(map[string]*DeviceConfig),
	}
	for i := range devices {
		d := &devices[i]
		r.devByName[d.Name] = d
		r.lastMeas[d.Name] = measSnapshot{W: math.NaN(), V: math.NaN(), Hz: math.NaN()}
	}
	return r
}

func (r *MQTTSystemReader) onMeasurement(_ string, msg bus.Measurement) {
	r.mu.Lock()
	snap := r.lastMeas[msg.Device]
	if msg.W != nil {
		snap.W = *msg.W
	}
	if msg.V != nil {
		snap.V = *msg.V
	}
	if msg.Hz != nil {
		snap.Hz = *msg.Hz
	}
	r.lastMeas[msg.Device] = snap
	r.mu.Unlock()
}

func (r *MQTTSystemReader) onBattMetrics(_ string, msg bus.BattMetrics) {
	r.mu.Lock()
	r.lastBattMet[msg.Device] = msg
	r.mu.Unlock()
}

func (r *MQTTSystemReader) onCSIPControl(_ string, msg bus.ActiveControl) {
	r.mu.Lock()
	r.lastCSIP = &msg
	r.clockOffset = msg.ClockOffset
	r.mu.Unlock()
}

func (r *MQTTSystemReader) onEVSEState(_ string, msg bus.EVSEState) {
	r.mu.Lock()
	r.lastEVSE[msg.StationID] = msg
	r.mu.Unlock()
}

// ReadSystemState implements orchestrator.SystemReader.
func (r *MQTTSystemReader) ReadSystemState() (orchestrator.SystemState, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	state := orchestrator.SystemState{
		Timestamp: time.Now(),
		Grid:      orchestrator.NewGridState(),
	}

	for _, dc := range r.devices {
		snap := r.lastMeas[dc.Name]

		switch dc.Role {
		case "battery":
			b := orchestrator.NewBatteryState(dc.Name)
			if !math.IsNaN(snap.W) {
				b.PowerW = snap.W
				b.Connected = true
				b.Energized = true
			}
			b.MaxChargeW = dc.MaxW
			b.MaxDischargeW = dc.MaxW
			if bm, ok := r.lastBattMet[dc.Name]; ok {
				if bm.SOC != nil {
					b.SOC = *bm.SOC
				}
				if bm.SOH != nil {
					b.SOH = *bm.SOH
				}
				if bm.CapacityWh != nil {
					b.CapacityWh = *bm.CapacityWh
				}
				if bm.MaxChargeW > 0 {
					b.MaxChargeW = bm.MaxChargeW
				}
				if bm.MaxDischargeW > 0 {
					b.MaxDischargeW = bm.MaxDischargeW
				}
			}
			state.Batteries = append(state.Batteries, b)

		case "inverter":
			sol := orchestrator.SolarState{
				Name:      dc.Name,
				MaxW:      dc.MaxW,
				Connected: !math.IsNaN(snap.W),
				Energized: !math.IsNaN(snap.W),
			}
			if !math.IsNaN(snap.W) {
				sol.PowerW = math.Max(0, snap.W)
			}
			state.Solar = append(state.Solar, sol)

		case "meter":
			if !math.IsNaN(snap.W) {
				if math.IsNaN(state.Grid.NetW) {
					state.Grid.NetW = snap.W
				} else {
					state.Grid.NetW += snap.W
				}
			}
			if !math.IsNaN(snap.Hz) {
				state.Grid.FrequencyHz = snap.Hz
			}
			if !math.IsNaN(snap.V) {
				state.Grid.VoltageV = snap.V
			}
		}
	}

	if r.lastCSIP != nil {
		state.CSIPControl = busToCSIPControl(r.lastCSIP)
		state.ClockOffset = r.clockOffset
	}

	for _, evse := range r.lastEVSE {
		state.EVSEs = append(state.EVSEs, busToEVSEState(evse))
	}

	return state, nil
}

// busToCSIPControl converts a bus.ActiveControl to an orchestrator.CSIPControlState.
func busToCSIPControl(msg *bus.ActiveControl) *orchestrator.CSIPControlState {
	if msg == nil || msg.Source == "none" || msg.Source == "" {
		return nil
	}
	cs := &orchestrator.CSIPControlState{
		Source:     msg.Source,
		MRID:       msg.MRID,
		ValidUntil: msg.ValidUntil,
	}
	cs.Base.OpModConnect = msg.Connect
	if msg.ExpLimW != nil {
		v := int16(*msg.ExpLimW)
		cs.Base.OpModExpLimW = &model.ActivePower{Value: v}
	}
	if msg.ImpLimW != nil {
		v := int16(*msg.ImpLimW)
		cs.Base.OpModImpLimW = &model.ActivePower{Value: v}
	}
	if msg.MaxLimW != nil {
		v := int16(*msg.MaxLimW)
		cs.Base.OpModMaxLimW = &model.ActivePower{Value: v}
	}
	if msg.FixedW != nil {
		v := int16(*msg.FixedW)
		cs.Base.OpModFixedW = &model.ActivePower{Value: v}
	}
	return cs
}

// busToEVSEState converts a bus.EVSEState to an orchestrator.EVSEState.
func busToEVSEState(msg bus.EVSEState) orchestrator.EVSEState {
	soc := math.NaN()
	if msg.SOC != nil {
		soc = *msg.SOC
	}
	return orchestrator.EVSEState{
		StationID:     msg.StationID,
		ConnectorID:   msg.ConnectorID,
		Connected:     msg.Connected,
		SessionActive: msg.SessionActive,
		CurrentA:      msg.CurrentA,
		MaxCurrentA:   msg.MaxCurrentA,
		VoltageV:      msg.VoltageV,
		PowerW:        msg.PowerW,
		Status:        msg.Status,
		SOC:           soc,
		EnergyWh:      msg.EnergyWh,
	}
}
