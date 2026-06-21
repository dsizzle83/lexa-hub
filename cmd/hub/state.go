package main

import (
	"log"
	"math"
	"sync"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/northbound/model"
	"lexa-hub/internal/orchestrator"
)

// Staleness windows: a snapshot older than this is treated as if the device
// were disconnected, so the optimizer never acts on frozen data after a
// publisher (lexa-modbus, lexa-ocpp) dies or the bus drops.
const (
	// measStaleAfter covers Modbus-polled devices (battery/solar/meter),
	// which publish every few seconds; 60 s ≈ four missed engine ticks.
	measStaleAfter = 60 * time.Second

	// evseStaleAfter covers OCPP stations.  State republishes on every
	// MeterValues (~10 s) during an active session, but only on status
	// changes while idle — the longer window avoids flapping an idle-but-
	// connected station, while a silent active session still expires.
	evseStaleAfter = 90 * time.Second

	// expiryConfirmTicks is how many consecutive ticks an active CSIP control
	// must read as past its ValidUntil (in server time) before it is dropped.
	// It rides out a non-monotonic clock step / lurch (a transient forward jump
	// past ValidUntil) while still clearing a genuinely-expired control whose
	// publisher has died within a few ticks.
	expiryConfirmTicks = 3
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
	lastEVSE map[string]evseSnapshot

	// Last resolved CSIP active control from lexa-csip
	lastCSIP    *bus.ActiveControl
	clockOffset int64
	// csipExpiredTicks counts consecutive ticks lastCSIP has been past its
	// validity window, so a transient (non-monotonic) clock excursion does not
	// drop a still-valid control. Reset whenever it is back inside the window.
	csipExpiredTicks int

	// stale tracks which measurement sources are currently stale, so staleness
	// is surfaced edge-triggered (one log on going stale, one on recovery)
	// instead of being silently absorbed by the computed-balance fallback.
	stale map[string]bool

	// Device configuration for role/capacity lookup
	devices   []DeviceConfig
	devByName map[string]*DeviceConfig
}

type measSnapshot struct {
	W  float64 // NaN if not received
	V  float64
	Hz float64
	at time.Time // receive time of the last message; zero = never received
}

func (s measSnapshot) fresh(now time.Time) bool {
	return !s.at.IsZero() && now.Sub(s.at) <= measStaleAfter
}

// evseSnapshot pairs the last EVSE state with its receive time.
type evseSnapshot struct {
	bus.EVSEState
	at time.Time
}

func (s evseSnapshot) fresh(now time.Time) bool {
	return now.Sub(s.at) <= evseStaleAfter
}

func newMQTTSystemReader(devices []DeviceConfig) *MQTTSystemReader {
	r := &MQTTSystemReader{
		lastMeas:    make(map[string]measSnapshot),
		lastBattMet: make(map[string]bus.BattMetrics),
		lastEVSE:    make(map[string]evseSnapshot),
		devices:     devices,
		devByName:   make(map[string]*DeviceConfig),
		stale:       make(map[string]bool),
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
	snap.at = time.Now()
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
	r.lastEVSE[msg.StationID] = evseSnapshot{EVSEState: msg, at: time.Now()}
	r.mu.Unlock()
}

// noteStaleness edge-triggers a warning when a measurement source goes stale
// (its publisher died or the bus dropped) and a notice when it recovers. The
// optimizer already fails safe on stale data — this makes that condition
// visible instead of silent. A source that has never reported is not warned
// about (that's startup, not a transition). Caller holds r.mu.
//
// Note: this catches a source that stops UPDATING. It cannot catch a sensor
// that keeps answering with a frozen value (no time-based signal distinguishes
// that from a genuinely steady reading); detecting that safely needs value-churn
// analysis that would false-positive against noise-free sims and is left to the
// telemetry layer.
func (r *MQTTSystemReader) noteStaleness(name string, snap measSnapshot, now time.Time) {
	if snap.at.IsZero() {
		return // never received — not a stale transition
	}
	stale := now.Sub(snap.at) > measStaleAfter
	switch {
	case stale && !r.stale[name]:
		r.stale[name] = true
		log.Printf("[hub] measurement source %q STALE — no update for %s; optimizer now running on estimated values for it",
			name, now.Sub(snap.at).Round(time.Second))
	case !stale && r.stale[name]:
		r.stale[name] = false
		log.Printf("[hub] measurement source %q recovered (fresh again)", name)
	}
}

// ReadSystemState implements orchestrator.SystemReader.
//
// Takes the write lock (not RLock) because it may clear an expired CSIP
// control; call frequency is one engine tick plus occasional replans, so the
// extra contention is negligible.
func (r *MQTTSystemReader) ReadSystemState() (orchestrator.SystemState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	state := orchestrator.SystemState{
		Timestamp: now,
		Grid:      orchestrator.NewGridState(),
	}

	for _, dc := range r.devices {
		snap := r.lastMeas[dc.Name]
		r.noteStaleness(dc.Name, snap, now)

		switch dc.Role {
		case "battery":
			b := orchestrator.NewBatteryState(dc.Name)
			if snap.fresh(now) && !math.IsNaN(snap.W) {
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
				if bm.MaxChargeW != nil && *bm.MaxChargeW > 0 {
					b.MaxChargeW = *bm.MaxChargeW
				}
				if bm.MaxDischargeW != nil && *bm.MaxDischargeW > 0 {
					b.MaxDischargeW = *bm.MaxDischargeW
				}
			}
			state.Batteries = append(state.Batteries, b)

		case "inverter":
			connected := snap.fresh(now) && !math.IsNaN(snap.W)
			sol := orchestrator.SolarState{
				Name:      dc.Name,
				MaxW:      dc.MaxW,
				Connected: connected,
				Energized: connected,
			}
			if connected {
				sol.PowerW = math.Max(0, snap.W)
			}
			state.Solar = append(state.Solar, sol)

		case "meter":
			// A stale meter contributes nothing: Grid.NetW stays NaN, which
			// makes the optimizer fall back to its computed power balance
			// instead of "verifying" limits against a frozen reading.
			if !snap.fresh(now) {
				continue
			}
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

	// Expire a control whose validity window has passed in SERVER time — the
	// topic is retained, so if lexa-northbound dies after publishing, nothing
	// else would clear it (and a stale OpModFixedW would keep dispatching).
	//
	// But the server clock (now+clockOffset) is not guaranteed monotonic: an NTP
	// step or a flapping grid-server clock can momentarily push server-now past
	// ValidUntil. Dropping the control on that single excursion would stop
	// enforcing a cap that is still valid once the clock settles — and the
	// retained control would not come back on the clock's return. So require the
	// expiry to PERSIST for expiryConfirmTicks consecutive ticks before dropping,
	// and keep enforcing the control in the meantime (a cap is conservative, so
	// holding it across a transient clock jump is the safe choice).
	if r.lastCSIP != nil && r.lastCSIP.ValidUntil > 0 &&
		now.Unix()+r.clockOffset >= r.lastCSIP.ValidUntil {
		r.csipExpiredTicks++
		if r.csipExpiredTicks >= expiryConfirmTicks {
			log.Printf("[hub] CSIP control %s (source=%s) expired at %d (server-now %d) for %d ticks; dropping",
				r.lastCSIP.MRID, r.lastCSIP.Source, r.lastCSIP.ValidUntil, now.Unix()+r.clockOffset, r.csipExpiredTicks)
			r.lastCSIP = nil
			r.csipExpiredTicks = 0
		}
	} else {
		r.csipExpiredTicks = 0
	}
	if r.lastCSIP != nil {
		state.CSIPControl = busToCSIPControl(r.lastCSIP)
	}
	state.ClockOffset = r.clockOffset

	for _, snap := range r.lastEVSE {
		es := busToEVSEState(snap.EVSEState)
		if !snap.fresh(now) {
			// lexa-ocpp (or the charger) has gone silent: drop the phantom
			// session so the optimizer stops budgeting power for a charger
			// it can no longer command.
			es.Connected = false
			es.SessionActive = false
		}
		state.EVSEs = append(state.EVSEs, es)
	}

	return state, nil
}

// wattsToActivePower encodes w into an IEEE 2030.5 ActivePower, scaling the
// multiplier up until the value fits in int16.  A bare int16 conversion is
// implementation-defined for out-of-range floats, silently corrupting any
// limit ≥ 32.768 kW.  Precision loss is bounded by half the final scale step
// (e.g. ±5 W at multiplier 1), negligible for grid limits.
func wattsToActivePower(w float64) *model.ActivePower {
	mult := int8(0)
	for (w > math.MaxInt16 || w < math.MinInt16) && mult < 9 {
		w /= 10
		mult++
	}
	return &model.ActivePower{Value: int16(math.Round(w)), Multiplier: mult}
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
		cs.Base.OpModExpLimW = wattsToActivePower(*msg.ExpLimW)
	}
	if msg.ImpLimW != nil {
		cs.Base.OpModImpLimW = wattsToActivePower(*msg.ImpLimW)
	}
	if msg.MaxLimW != nil {
		cs.Base.OpModMaxLimW = wattsToActivePower(*msg.MaxLimW)
	}
	if msg.FixedW != nil {
		cs.Base.OpModFixedW = wattsToActivePower(*msg.FixedW)
	}
	return cs
}

// busToEVSEState converts a bus.EVSEState to an orchestrator.EVSEState.
func busToEVSEState(msg bus.EVSEState) orchestrator.EVSEState {
	soc := math.NaN()
	if msg.SOC != nil {
		soc = *msg.SOC
	}
	deref := func(p *float64) float64 {
		if p == nil {
			return 0
		}
		return *p
	}
	return orchestrator.EVSEState{
		StationID:     msg.StationID,
		ConnectorID:   msg.ConnectorID,
		Connected:     msg.Connected,
		SessionActive: msg.SessionActive,
		CurrentA:      deref(msg.CurrentA),
		MaxCurrentA:   deref(msg.MaxCurrentA),
		VoltageV:      deref(msg.VoltageV),
		PowerW:        deref(msg.PowerW),
		Status:        msg.Status,
		SOC:           soc,
		EnergyWh:      deref(msg.EnergyWh),
	}
}
