package constraint

import (
	"math"

	"lexa-hub/internal/orchestrator"
)

// GatewayEVSEPolicy configures CSIPPassthrough's EV-charging behavior in
// gateway mode (DEVICE_ROADMAP §3.5; ECOSYSTEM_ROADMAP §13/§14). A pure CSIP
// gateway has no cost-optimal EV allocation opinion of its own — the operator
// picks a blanket policy instead: let EVs charge whenever plugged in, or only
// overnight. This is the documented, user-visible consequence of gateway mode
// ECOSYSTEM_ROADMAP §13 calls out ("a pure CSIP gateway has no opinion about
// EVs; don't let the mode switch silently strand smart charging").
//
// WindowStartHH/WindowEndHH are read as LOCAL-PROCESS-ZONE hours [0,24) — the
// same GAP-05 provenance the TOU cost model and planner already depend on
// (internal/orchestrator/costmodel.go's IsPeakHour, planner.go:606-651,
// CLAUDE.md's "SOM zone must match the tariff zone" note): the hub's
// configured timezone must equal the site's zone at commissioning, and this
// policy reads the hour straight off Input.State.Timestamp with no zone
// conversion of its own — it is only correct when that deployment invariant
// holds. A window may wrap past midnight (WindowStartHH > WindowEndHH, e.g.
// the 23→7 default).
type GatewayEVSEPolicy struct {
	// Mode is "scheduled" (honour the window below) or "full" (FullCurrentA at
	// every hour). Any other value — including empty before WithDefaults — is
	// treated as "scheduled" defensively: an EV charging window is the more
	// conservative failure mode than silently going full-power.
	Mode string `json:"mode"`
	// WindowStartHH is the local hour [0,23] the scheduled window begins
	// (inclusive), matching bus.TariffPeriod's StartHH convention.
	WindowStartHH int `json:"window_start_hh"`
	// WindowEndHH is the local hour [0,23] the scheduled window ends
	// (exclusive), matching bus.TariffPeriod's EndHH convention.
	// WindowStartHH > WindowEndHH wraps past midnight (the default, 23→7).
	WindowEndHH int `json:"window_end_hh"`
	// FullCurrentA is the ceiling (A) offered inside the window — or at every
	// hour, in "full" mode.
	FullCurrentA float64 `json:"full_current_a"`
}

// WithDefaults returns a copy with zero/absent fields filled to hub.json §10's
// documented defaults (scheduled, 23:00→07:00, 32 A). This follows the same
// zero-sentinel convention plantmodel.go's WithDefaults functions use (e.g.
// InverterPlant, BatteryPlant): a legitimate all-zero policy is
// indistinguishable from an absent one. That is accepted plant-model debt
// reused here rather than invented anew — a real "start the window at
// midnight" config cannot be expressed as WindowStartHH:0; it must be
// approximated (e.g. WindowStartHH:24 is not valid either, so this is a real,
// if minor, limitation carried over from the existing convention).
func (p GatewayEVSEPolicy) WithDefaults() GatewayEVSEPolicy {
	if p.Mode == "" {
		p.Mode = "scheduled"
	}
	if p.WindowStartHH == 0 {
		p.WindowStartHH = 23
	}
	if p.WindowEndHH == 0 {
		p.WindowEndHH = 7
	}
	if p.FullCurrentA == 0 {
		p.FullCurrentA = 32
	}
	return p
}

// inWindow reports whether hour (local, [0,24)) falls in [start, end),
// wrapping past midnight when start > end (e.g. 23→7 admits hours
// 23,0,1,...,6). A zero-width window (start == end) is nonsensical
// configuration and is treated as "never" — the conservative failure mode
// for an unattended charger (never silently full-power it).
func inWindow(hour, start, end int) bool {
	if start == end {
		return false
	}
	if start < end {
		return hour >= start && hour < end
	}
	return hour >= start || hour < end
}

// currentLimitA resolves this tick's EVSE current ceiling from the policy and
// the tick's local hour. hour is supplied by the caller from
// Input.State.Timestamp — this method itself does no clock I/O, keeping it a
// pure function like the rest of the package (doc.go's "No I/O" rule).
func (p GatewayEVSEPolicy) currentLimitA(hour int) float64 {
	if p.Mode == "full" {
		return p.FullCurrentA
	}
	if inWindow(hour, p.WindowStartHH, p.WindowEndHH) {
		return p.FullCurrentA
	}
	return 0
}

// Source constants for CSIPPassthrough's demands — one per CSIP field/policy
// branch mirrored, so a divergence trace or arbiter conflict names WHICH
// mapping produced a demand (the same diagnostic role Demand.Source plays for
// every other constraint in this package).
const (
	sourceCSIPMaxLim  = "csip-maxlim"  // OpModMaxLimW → solar ceiling
	sourceCSIPRestore = "csip-restore" // no active MaxLimW → explicit restore
	sourceCSIPFixedW  = "csip-fixedw"  // OpModFixedW → battery setpoint
	sourceCSIPIdle    = "csip-idle"    // no active FixedW → explicit 0 W idle
	sourceCSIPConnect = "csip-connect" // OpModConnect → connect/disconnect
	sourceGatewayEVSE = "gateway-evse" // GatewayEVSEPolicy → EVSE current ceiling
)

// CSIPPassthrough occupies the TierEconomics slot in gateway mode
// (DEVICE_ROADMAP §3.5; ECOSYSTEM_ROADMAP §14): it expresses the utility's
// ACTIVE CSIP control — or, absent one, the site's restore/idle/policy
// defaults — as demands, with NO economic opinion of its own. The
// compliance-tier constraints (Export/GenLimit/ImportLimit) run ABOVE it in
// the Stack and narrow these demands to the site's ExpLimW/ImpLimW/MaxLimW
// envelopes exactly as they do today in shadow (TASK-060/061) — this
// constraint never allocates a site envelope across devices itself; it only
// forwards the utility's per-axis instruction (or its absence) so the
// compliance tier has something to narrow. "Economics propose, constraints
// dispose" (doc.go) holds here exactly as it does for EconomicsConstraint,
// the constraint this one REPLACES in gateway mode: every demand below is a
// TierEconomics proposal, never a higher tier, so it can never widen a
// compliance/safety bound (AD-007, structural via the arbiter).
//
// Emitted demands (all TierEconomics):
//
//   - AxisSolarCeilingW: one ceiling per inverter (Plant.Inverters) — the
//     CSIP OpModMaxLimW value when an active control carries one, else an
//     EXPLICIT restore ceiling (Max=NaN, "no curtailment"). Restore is a
//     WRITE, never an absence — the solar restore-is-a-write rule
//     generalises to gateway mode (bus/desired.go's RestoreCeilingW doc;
//     ECOSYSTEM_ROADMAP §14's "never by absence").
//
//   - AxisBatterySetpointW: one point per battery (Plant.Batteries) — the
//     CSIP OpModFixedW value, forwarded VERBATIM as the setpoint (sign
//     convention below), when active, else an explicit 0 W idle point. A
//     pure gateway has no autonomous dispatch of its own: this is the
//     documented, user-visible consequence of gateway mode ECOSYSTEM_ROADMAP
//     §13 names for EVs, generalised here to batteries.
//
//   - AxisConnect: a connect/disconnect demand for EVERY device across all
//     three plant maps (Inverters, Batteries, EVSEs), but ONLY when the
//     active control carries an explicit OpModConnect — mirroring
//     optimizer.go's cease-to-energize rule (optimizer.go:395-439) as an
//     explicit demand instead of an early return. No control, or a control
//     silent on Connect, emits NO connect demand at all: reasserting connect
//     on reconnect is the RECONCILERS' territory (their retained-desired-doc
//     reassert, internal/reconcile), not this constraint's to re-decide
//     every tick.
//
//   - AxisEVSECurrentA: one ceiling per configured station (Plant.EVSEs,
//     keyed by station only — the bare device key implies connector 0,
//     stack.go's parseEVSEDevice) from the GatewayEVSEPolicy: "full" mode
//     offers FullCurrentA at every hour; "scheduled" mode offers
//     FullCurrentA inside the configured window and 0 A (suspend) outside
//     it. This is evaluated EVERY tick regardless of CSIP state — a CSIP
//     Connect=false still suspends charging via the connect demand above,
//     and the compliance tier's export/import backstops narrow this ceiling
//     exactly as they narrow any other economics proposal.
//
// Evaluate NEVER returns a ComplianceBreach: passthrough only forwards
// instructions, it never diagnoses non-convergence — that stays the
// compliance constraints' job, unaffected by which constraint occupies the
// economics slot.
//
// Known Stack-wiring gap (documented, not fixed here — out of this unit's
// scope; the house convention is naming a Stack limitation in the emitting
// constraint's doc rather than reaching into stack.go, see export.go's
// EV-emission-deferred note for precedent): stack.go's emitCommands maps
// AxisConnect only onto BatteryCommand.Connect (constraint.go:59-62) — an
// AxisConnect demand keyed to an inverter or EVSE device name is therefore
// currently inert once past arbitration (no SolarCommand/EVSECommand carries
// a Connect field yet). Wiring or actuating this constraint (a later unit)
// needs that Stack gap closed, or must accept that gateway-mode
// cease-to-energize for solar/EVSE stays legacy-owned in the interim (the
// fallback ECOSYSTEM_ROADMAP §14 already names: "a passthrough-only gateway
// … weeks, not months, but document its […] gap prominently"). This unit
// only builds and unit-tests the constraint in isolation — nothing
// constructs or calls it outside passthrough_test.go.
type CSIPPassthrough struct {
	// policy is resolved (WithDefaults applied) at construction time, like
	// EconomicsConstraint's constructor arguments — not re-defaulted on every
	// Evaluate.
	policy GatewayEVSEPolicy
}

// compile-time proof CSIPPassthrough satisfies the Constraint interface.
var _ Constraint = (*CSIPPassthrough)(nil)

// NewCSIPPassthrough builds the passthrough constraint from the gateway EVSE
// policy (hub.json's "gateway" block).
func NewCSIPPassthrough(policy GatewayEVSEPolicy) *CSIPPassthrough {
	return &CSIPPassthrough{policy: policy.WithDefaults()}
}

// Name is the stable identity. This constraint carries no Session state (see
// the type doc — like EconomicsConstraint, the other TierEconomics
// constraint, it accepts a *Session only to satisfy the Constraint
// interface), so Name is not used to key any inter-tick state here; each
// demand below stamps its OWN Source constant instead of this Name, so a
// trace can tell exactly which CSIP field/policy branch produced it.
func (c *CSIPPassthrough) Name() string { return "csip-passthrough" }

// Tier places CSIPPassthrough in the economics band: it PROPOSES the
// utility's instruction (or a default), and every higher tier still narrows
// it (AD-007) — exactly the slot the real EconomicsConstraint occupies in
// optimizer mode. Compliance-tier constraints (Export/GenLimit/ImportLimit)
// narrow everything this constraint emits; TierSafety narrows both.
func (c *CSIPPassthrough) Tier() Tier { return TierEconomics }

// Evaluate maps the active CSIP control (or its absence) plus the gateway
// EVSE policy onto this tick's demands. It is pure: no time.Now(), no I/O
// (doc.go) — the EVSE schedule reads Input.State.Timestamp, never the wall
// clock, so it stays deterministic for tests and any future shadow diffing.
func (c *CSIPPassthrough) Evaluate(in Input, s *Session) ([]Demand, *orchestrator.ComplianceBreach) {
	// ctl is nil whenever there is no active CSIP program. cmd/hub's
	// busToCSIPControl already folds a bus ActiveControl with Source=="" or
	// Source=="none" to a literal nil *CSIPControlState before it ever reaches
	// SystemState (cmd/hub/state.go) — so a nil check is the ONLY guard
	// needed here; there is no live "non-nil but sourceless" control state to
	// additionally check for, unlike a raw bus message.
	ctl := in.State.CSIPControl

	var demands []Demand

	// ── Solar: explicit ceiling from OpModMaxLimW, or an explicit restore ──
	for name := range in.Plant.Inverters {
		if ctl != nil && ctl.Base.OpModMaxLimW != nil {
			demands = append(demands, CeilingDemand(name, AxisSolarCeilingW, apW(ctl.Base.OpModMaxLimW), TierEconomics, sourceCSIPMaxLim))
		} else {
			demands = append(demands, CeilingDemand(name, AxisSolarCeilingW, math.NaN(), TierEconomics, sourceCSIPRestore))
		}
	}

	// ── Battery: explicit point from OpModFixedW, or an explicit idle point ──
	for name := range in.Plant.Batteries {
		if ctl != nil && ctl.Base.OpModFixedW != nil {
			// Sign convention matched to optimizer.go:582-589
			// (applyFixedDispatchRule) and its constraint-package twin,
			// economics.go:236-240 (EconomicsConstraint.applyFixedDispatch):
			// both read targetW := apW(cc.Base.OpModFixedW) and use it
			// DIRECTLY as a discharge/export target (added to the battery's
			// PowerW, never negated), because BatteryCommand.SetpointW shares
			// BatteryState.PowerW's "+discharge (export), −charge (import)"
			// convention (model.go:50-51; also derbase.go's
			// SetActivePowerWatts doc: "signed: + discharge/export, − charge/
			// import"). A passthrough does no solar/home-load netting (that
			// arithmetic is what makes Rule 2 an ECONOMIC rule, not merely a
			// forward) — it forwards the CSIP target to the setpoint
			// verbatim.
			demands = append(demands, PointDemand(name, AxisBatterySetpointW, apW(ctl.Base.OpModFixedW), TierEconomics, sourceCSIPFixedW))
		} else {
			// A pure gateway has no autonomous dispatch: idle at 0 W unless
			// the utility commands otherwise. Documented, user-visible
			// consequence of gateway mode (ECOSYSTEM_ROADMAP §13's "a pure
			// CSIP gateway has no opinion" generalised from EVs to
			// batteries).
			demands = append(demands, PointDemand(name, AxisBatterySetpointW, 0, TierEconomics, sourceCSIPIdle))
		}
	}

	// ── Connect: only when CSIP is explicit about it; never invented ──
	if ctl != nil && ctl.Base.OpModConnect != nil {
		connect := *ctl.Base.OpModConnect
		for name := range in.Plant.Inverters {
			demands = append(demands, ConnectDemand(name, connect, TierEconomics, sourceCSIPConnect))
		}
		for name := range in.Plant.Batteries {
			demands = append(demands, ConnectDemand(name, connect, TierEconomics, sourceCSIPConnect))
		}
		for name := range in.Plant.EVSEs {
			demands = append(demands, ConnectDemand(name, connect, TierEconomics, sourceCSIPConnect))
		}
	}
	// ctl == nil, or a control silent on Connect: no connect demand at all —
	// restore-connect-on-reconnect is the reconcilers' reassert territory
	// (see the type doc).

	// ── EVSE: policy-driven ceiling, independent of CSIP (narrowed above it
	// only by the connect demand and, downstream, the compliance tier) ──
	hour := in.State.Timestamp.Hour() // NEVER time.Now() — see the Evaluate doc.
	for name := range in.Plant.EVSEs {
		limitA := c.policy.currentLimitA(hour)
		demands = append(demands, CeilingDemand(name, AxisEVSECurrentA, limitA, TierEconomics, sourceGatewayEVSE))
	}

	return demands, nil
}
