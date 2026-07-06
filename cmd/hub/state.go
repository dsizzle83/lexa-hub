package main

import (
	"log/slog"
	"math"
	"sync"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/orchestrator"
	"lexa-hub/internal/utilitytime"
	model "lexa-proto/csipmodel"
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

	// meterFrozenAfter is the window after which a meter that keeps publishing
	// the same W value is treated as a frozen sensor and excluded from the
	// optimizer's grid reading. The optimizer falls back to its computed power
	// balance, which is safer than acting on a sensor stuck at a stale reading.
	// A typical meter publishes every ~2–3 s; 30 s = ~12 consecutive identical
	// readings. A noise gate of 10 W prevents false triggers from ADC jitter.
	meterFrozenAfter  = 30 * time.Second
	meterFrozenNoiseW = 10.0

	// solarMovingWindow is how recently an inverter's W must have changed for the
	// world to be considered "moving". A frozen meter is only excluded while the
	// world is demonstrably changing — this prevents false positives when the grid
	// is genuinely steady (meter stable because load is stable, not because it's stuck).
	solarMovingWindow = 20 * time.Second
)

// expiryConfirmWindowS is the wall-clock window (seconds, server time) an
// active CSIP control must read as past its ValidUntil before it is dropped
// — riding out a non-monotonic clock step / lurch (a transient forward jump
// past ValidUntil) while still clearing a genuinely-expired control whose
// publisher has died within a few ticks. utilitytime.DebouncedExpiry
// (AD-004, TASK-036) is the policy; this is denominated in seconds per 05 §5
// ("thresholds in wall-clock seconds, scaled to ticks at the edge") rather
// than the old tick-counted expiryConfirmTicks=3 constant.
//
// 9 s reproduces today's QA-validated FAST behavior bit-for-bit (3 ticks ×
// the 3 s FAST engine interval — see confirmTicksFor). At the STOCK 15 s
// interval this floors to 2 ticks = 30 s, not the legacy 3 ticks = 45 s: a
// deliberate wall-clock-denomination correction (AD-004, TASK-036, 05 §5),
// not a bug — tick-counting this threshold meant FAST and STOCK debounced
// for different real-world durations. STOCK's control-expiry release
// latency improves 45 s → 30 s; see
// docs/refactor/02_ARCHITECTURE_DECISIONS.md AD-004 in csip-tls-test.
const expiryConfirmWindowS = 9.0

// confirmTicksFor scales expiryConfirmWindowS to the number of consecutive
// engine ticks that represents at the given engine cadence, floored at 2 so
// a single-tick clock excursion can never drop a still-valid control (the
// floor the legacy constant also implicitly provided at every cadence <=
// 4.5 s). Mirrors the optimizer's scaleTicks pattern
// (internal/orchestrator/optimizer.go) on the economic-tick side.
//
// engineInterval <= 0 is defensive only (cmd/hub/config.go's loadConfig
// always defaults EngineIntervalS to 15 before any reader is constructed);
// it falls back to that same 15 s STOCK default rather than dividing by zero.
func confirmTicksFor(engineInterval time.Duration) int {
	if engineInterval <= 0 {
		engineInterval = 15 * time.Second
	}
	n := int(math.Ceil(expiryConfirmWindowS / engineInterval.Seconds()))
	if n < 2 {
		n = 2
	}
	return n
}

// localStepLogAction is localStepEdge's verdict for the current tick: log
// nothing, log a forward step, log a backward step, or log that a
// previously-logged step condition has cleared.
type localStepLogAction int

const (
	localStepLogNone localStepLogAction = iota
	localStepLogForward
	localStepLogBackward
	localStepLogCleared
)

// localStepEdge is TASK-037's local-clock-step logging policy as a pure
// decision function, deliberately factored out of ReadSystemState so it is
// unit-testable in isolation: given whether the reader was already in a
// "stepped" state as of the previous tick, this tick's utilitytime.Clock
// LocalStep() result (stepped, drift), it returns the new stepped state to
// remember plus what (if anything) to log this tick.
//
// Policy (AD-004 extension, 02_ARCHITECTURE_DECISIONS.md in csip-tls-test):
// forward steps re-anchor silently from the enforcement side (r.utclk already
// re-anchors on every onCSIPControl arrival, independent of this function) and
// get a plain transition log; backward steps get the identical anchored
// correctness plus a louder alarm log, since a backward RTC/NTP correction is
// the more operationally surprising direction (log wall-clock times can
// appear to regress). Either way the log fires exactly once per transition —
// edge-triggered like noteStaleness — not once per tick for the duration of
// the step.
func localStepEdge(prevStepped, stepped bool, drift int64) (newStepped bool, action localStepLogAction) {
	switch {
	case stepped && !prevStepped:
		if drift >= 0 {
			return true, localStepLogForward
		}
		return true, localStepLogBackward
	case !stepped && prevStepped:
		return false, localStepLogCleared
	default:
		return prevStepped, localStepLogNone
	}
}

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

	// utclk anchors utility (server) time to a monotonic instant every time a
	// fresh bus.ActiveControl arrives (onCSIPControl), so ServerNow() between
	// arrivals is immune to a LOCAL wall-clock step (NTP correction at
	// commissioning, RTC drift fix-up) — TASK-037/GAP-04. This is a distinct
	// concern from clockOffset above: clockOffset is the raw *server-vs-local*
	// offset (unaffected by this change, still fed to the optimizer via a
	// derived value below); utclk instead protects against the *local* clock
	// itself moving between messages. See internal/utilitytime's package doc
	// for the full design writeup and AD-004 (csip-tls-test docs/refactor) for
	// the decision record.
	utclk *utilitytime.Clock
	// clockStepped edge-triggers the local-step transition log (like
	// noteStaleness): true once a LocalStep() call has classified the local
	// clock as currently stepped, so the log fires once per transition, not
	// once per tick for the duration of the step.
	clockStepped bool

	// lastCSIPMRID/lastCSIPChangedAt back lexa_hub_control_adoption_age_seconds
	// (TASK-044): the topic is retained and lexa-northbound republishes it on
	// every discovery cycle even when nothing changed (60 s default), so
	// "time since the last message" would just track the discovery interval,
	// not what the metric is actually for — how long the CURRENTLY-ADOPTED
	// control has been in force. Updated only in onCSIPControl when the
	// resolved control's identity actually changes (MRID differs, or the
	// source flips to/from "none"/"default" with no MRID).
	lastCSIPMRID      string
	lastCSIPChangedAt time.Time
	// expiry debounces lastCSIP's expiry: a transient (non-monotonic) clock
	// excursion past ValidUntil must not drop a still-valid control, but a
	// SUSTAINED expiry (confirmed for expiry.Confirm consecutive ticks) must.
	// utilitytime.DebouncedExpiry (AD-004, TASK-036) generalizes the old
	// csipExpiredTicks/expiryConfirmTicks pair; see confirmTicksFor for how
	// Confirm is derived from the engine cadence.
	expiry utilitytime.DebouncedExpiry

	// stale tracks which measurement sources are currently stale, so staleness
	// is surfaced edge-triggered (one log on going stale, one on recovery)
	// instead of being silently absorbed by the computed-balance fallback.
	stale map[string]bool

	// Device configuration for role/capacity lookup
	devices   []DeviceConfig
	devByName map[string]*DeviceConfig
}

type measSnapshot struct {
	W          float64 // NaN if not received
	V          float64
	Hz         float64
	at         time.Time // receive time of the last message; zero = never received
	wChangedAt time.Time // when W last changed by more than meterFrozenNoiseW
}

func (s measSnapshot) fresh(now time.Time) bool {
	return !s.at.IsZero() && now.Sub(s.at) <= measStaleAfter
}

// frozenW returns true when messages are still arriving (fresh) but the W value
// has not changed by more than meterFrozenNoiseW for meterFrozenAfter. This
// detects a sensor that is stuck at a stale reading without going silent.
func (s measSnapshot) frozenW(now time.Time) bool {
	return s.fresh(now) && !s.wChangedAt.IsZero() && now.Sub(s.wChangedAt) > meterFrozenAfter
}

// evseSnapshot pairs the last EVSE state with its receive time.
type evseSnapshot struct {
	bus.EVSEState
	at time.Time
}

func (s evseSnapshot) fresh(now time.Time) bool {
	return now.Sub(s.at) <= evseStaleAfter
}

// newMQTTSystemReader constructs a reader for the given device set. engineInterval
// is the engine's configured tick cadence (cfg.EngineInterval()) — it sizes the
// CSIP expiry debounce (expiry.Confirm) so the debounce means the same
// wall-clock seconds regardless of cadence (see confirmTicksFor, AD-004/TASK-036).
func newMQTTSystemReader(devices []DeviceConfig, engineInterval time.Duration) *MQTTSystemReader {
	r := &MQTTSystemReader{
		lastMeas:    make(map[string]measSnapshot),
		lastBattMet: make(map[string]bus.BattMetrics),
		lastEVSE:    make(map[string]evseSnapshot),
		devices:     devices,
		devByName:   make(map[string]*DeviceConfig),
		stale:       make(map[string]bool),
		expiry:      utilitytime.DebouncedExpiry{Confirm: confirmTicksFor(engineInterval)},
		utclk:       utilitytime.New(utilitytime.Config{}),
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
	now := time.Now()
	if msg.W != nil {
		newW := *msg.W
		if math.IsNaN(snap.W) || math.Abs(newW-snap.W) > meterFrozenNoiseW {
			snap.wChangedAt = now
		}
		snap.W = newW
	}
	if msg.VoltageV != nil {
		snap.V = *msg.VoltageV
	}
	if msg.Hz != nil {
		snap.Hz = *msg.Hz
	}
	snap.at = now
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
	// Anchor utility time at this message's arrival (TASK-037). msg.Ts is
	// stamped by lexa-northbound with time.Now().Unix() at publish
	// (cmd/northbound/main.go's toActiveControl); msg.Ts+msg.ClockOffset is
	// therefore server time AT PUBLISH. This is only valid because
	// lexa-northbound and lexa-hub share the same host clock (the hub Pi/SOM
	// — see CLAUDE.md's bench topology) and MQTT localhost latency is
	// negligible (≪ 1 s) — a split-host deployment would have to re-derive
	// this anchor from its own local receipt time instead.
	r.utclk.Anchor(msg.Ts + msg.ClockOffset)
	if msg.MRID != r.lastCSIPMRID {
		r.lastCSIPMRID = msg.MRID
		r.lastCSIPChangedAt = time.Now()
	}
	r.mu.Unlock()
}

// ControlAdoptionAge returns how long the currently-adopted CSIP control has
// been in force, as of now (lexa_hub_control_adoption_age_seconds, TASK-044).
// Returns 0 before any control has ever been resolved (never — Source
// "none" still carries a stable empty MRID, so the very first message
// already sets lastCSIPChangedAt); the zero-value case only matters before
// lexa-northbound has published anything at all, which is startup.
func (r *MQTTSystemReader) ControlAdoptionAge(now time.Time) time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.lastCSIPChangedAt.IsZero() {
		return 0
	}
	return now.Sub(r.lastCSIPChangedAt)
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
		// TASK-045: migrated to slog. "STALE" kept intact in the message text
		// (grep-verified: no csip-tls-test caller quotes it today, but the
		// phrase is this fault class's name throughout the QA docs).
		slog.Warn("[hub] measurement source STALE — optimizer now running on estimated values for it",
			"device", name, "since", now.Sub(snap.at).Round(time.Second).String())
	case !stale && r.stale[name]:
		r.stale[name] = false
		slog.Info("[hub] measurement source recovered (fresh again)", "device", name)
	}
}

// ReadSafetyState implements orchestrator.SafetyReader: a cheap, side-effect-free
// snapshot of just the batteries (power + SOC + connectivity) for the fast
// protection loop. It deliberately does NOT run CSIP-control expiry, meter-freeze,
// or EVSE-staleness logic — those are per-economic-tick concerns whose
// tick-denominated state polling at the fast cadence would perturb. Takes only a
// read lock so it never contends with the economic ReadSystemState write lock
// beyond RWMutex fairness.
func (r *MQTTSystemReader) ReadSafetyState() (orchestrator.SystemState, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := time.Now()
	state := orchestrator.SystemState{
		Timestamp: now,
		Grid:      orchestrator.NewGridState(),
	}
	for _, dc := range r.devices {
		if dc.Role != "battery" {
			continue
		}
		snap := r.lastMeas[dc.Name]
		b := orchestrator.NewBatteryState(dc.Name)
		if snap.fresh(now) && !math.IsNaN(snap.W) {
			b.PowerW = snap.W
			b.Connected = true
			b.Energized = true
		}
		b.MaxChargeW = dc.MaxW
		b.MaxDischargeW = dc.MaxW
		if bm, ok := r.lastBattMet[dc.Name]; ok && bm.SOC != nil {
			b.SOC = *bm.SOC
		}
		state.Batteries = append(state.Batteries, b)
	}
	return state, nil
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

	// Pre-scan: does the world appear to be moving? Frozen-meter detection requires
	// at least one inverter to have changed its W recently; otherwise a legitimately
	// steady grid (stable load, zero wind) would produce a false positive.
	worldMoving := false
	for _, dc := range r.devices {
		if dc.Role == "inverter" {
			s := r.lastMeas[dc.Name]
			if !s.wChangedAt.IsZero() && now.Sub(s.wChangedAt) < solarMovingWindow {
				worldMoving = true
				break
			}
		}
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
			//
			// A frozen meter (same W for meterFrozenAfter while still publishing)
			// is also excluded — but ONLY while the world is demonstrably moving
			// (worldMoving=true). A legitimately stable grid looks identical to a
			// frozen sensor without this cross-source gate.
			frozenKey := dc.Name + ":frozen"
			frozen := worldMoving && snap.frozenW(now)
			if !snap.fresh(now) || frozen {
				if frozen && !r.stale[frozenKey] {
					r.stale[frozenKey] = true
					// TASK-045: migrated to slog. "frozen" kept intact (INV-STALE
					// vocabulary; grep-verified unquoted by csip-tls-test today).
					slog.Warn("[hub] meter W value frozen while grid is moving; excluding from grid reading",
						"device", dc.Name, "w", snap.W, "frozen_for", now.Sub(snap.wChangedAt).Round(time.Second).String())
				}
				continue
			}
			if r.stale[frozenKey] {
				r.stale[frozenKey] = false
				slog.Info("[hub] meter W value moving again; restoring to grid reading",
					"device", dc.Name, "w", snap.W)
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
	// expiry to PERSIST for the confirm window before dropping
	// (utilitytime.DebouncedExpiry, AD-004/TASK-036 — see expiryConfirmWindowS),
	// and keep enforcing the control in the meantime (a cap is conservative, so
	// holding it across a transient clock jump is the safe choice).
	//
	// serverNow now reads from r.utclk (TASK-037/AD-004 extension): monotonic-
	// anchored at every onCSIPControl arrival, so a LOCAL wall-clock step
	// (distinct from the server-side excursion this debounce already handles)
	// cannot move it between arrivals either. Before any control has ever
	// arrived, r.utclk is unanchored and ServerNow() degrades to the local
	// clock (offset 0) — identical to the pre-TASK-037 zero-value behavior.
	// Expired/r.expiry.Observe are unchanged: a false Observe (no control, or
	// a ValidUntil=0 control that never expires) resets the counter, matching
	// the old "else { r.csipExpiredTicks = 0 }" branch exactly.
	serverNow := r.utclk.ServerNow()
	expired := r.lastCSIP != nil && utilitytime.Expired(r.lastCSIP.ValidUntil, serverNow)
	if r.expiry.Observe(expired) {
		// TASK-045: migrated to slog (control expiry edge).
		slog.Info("[hub] CSIP control expired; dropping",
			"mrid", r.lastCSIP.MRID, "source", r.lastCSIP.Source,
			"valid_until", r.lastCSIP.ValidUntil, "server_now", serverNow,
			"confirm_ticks", r.expiry.Confirm)
		r.lastCSIP = nil
	}
	if r.lastCSIP != nil {
		state.CSIPControl = busToCSIPControl(r.lastCSIP)
	}

	// TASK-037: edge-triggered local (wall-clock) step detection/logging, like
	// noteStaleness above. This is purely observability — utility-time
	// evaluation above is already monotonic-anchored and freshness windows
	// (measStaleAfter/evseStaleAfter/meterFrozenAfter, all time.Now()+Sub)
	// were already immune before this task; nothing here changes behavior.
	// The decision of WHETHER/WHAT to log is factored into localStepEdge (a
	// pure function) so the "log fires exactly once per step" edge-trigger
	// claim is unit-testable without needing to fake a genuine OS-level
	// wall/monotonic desync through r.utclk itself (see internal/utilitytime's
	// package doc for why that can't be done through the public time.Time API).
	drift, stepped := r.utclk.LocalStep()
	var action localStepLogAction
	r.clockStepped, action = localStepEdge(r.clockStepped, stepped, drift)
	switch action {
	case localStepLogForward:
		slog.Info("[hub] local clock step detected — utility-time evaluation is monotonic-anchored; freshness unaffected",
			"drift_s", drift, "direction", "forward")
	case localStepLogBackward:
		// Backward local steps get the same anchored correctness, plus an
		// alarm: a backward RTC/NTP correction is the more operationally
		// surprising direction (log wall-times can appear to regress), worth
		// a louder signal even though enforcement stays correct.
		slog.Warn("[hub] local clock step detected — utility-time evaluation is monotonic-anchored; freshness unaffected",
			"drift_s", drift, "direction", "backward")
	case localStepLogCleared:
		slog.Info("[hub] local clock step condition cleared", "drift_s", drift)
	}
	// state.ClockOffset carries a DERIVED offset (r.utclk.ServerNow() minus
	// this tick's local now), not the raw r.clockOffset — so the optimizer's
	// existing utilitytime.ServerNowAt(state.Timestamp, state.ClockOffset)
	// call (internal/orchestrator/optimizer.go, TOU/IsPeakHour) reconstructs
	// the SAME anchored serverNow without the orchestrator touching a Clock
	// or reading a wall clock of its own (AD-004: orchestrator stays I/O-free).
	// Under a stable local clock this is bit-identical to r.clockOffset (both
	// equal server-minus-local); it only diverges from the raw offset during
	// the monotonic holdover between control arrivals under a local step —
	// which is exactly the case this task closes.
	state.ClockOffset = serverNow - now.Unix()

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
