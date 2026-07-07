package constraint

import (
	"fmt"
	"math"
	"sort"
)

// Interval is a resolved admissible bound on one axis. NaN marks an unbounded
// side (matching Demand). A pinned point has Min==Max.
type Interval struct {
	Min, Max float64
}

// Desired is the arbitrated outcome for one device: the resolved admissible
// interval per axis, the resolved connect state, and any conflicts recorded
// while resolving. The Stack converts this into orchestrator commands.
type Desired struct {
	Device    string
	Bounds    map[Axis]Interval
	Connect   *bool
	Conflicts []Conflict
}

// Bound returns the resolved interval for an axis and whether any demand set it.
func (d Desired) Bound(axis Axis) (Interval, bool) {
	iv, ok := d.Bounds[axis]
	return iv, ok
}

// Conflict records a same-tier contention the arbiter resolved by taking the
// most-restrictive demand. It is surfaced in the plan's decision log so an
// operator can see WHICH constraints disagreed — the cascade could not, because
// a later rule silently overwrote an earlier one.
type Conflict struct {
	Device  string
	Axis    Axis
	Tier    Tier
	Sources [2]string // the two contending constraint names
	Reason  string
}

// Resolve arbitrates a tick's demands into per-device Desired state.
//
// Semantics (AD-007):
//   - Demands are grouped per (Device, Axis) and processed SAFETY→ECONOMICS,
//     ties broken by Source name — fully deterministic, no map-iteration order
//     leaks into the result.
//   - Each demand INTERSECTS the running interval: newLo=max(lo,Min),
//     newHi=min(hi,Max). Intersection can only shrink the interval, so a lower
//     tier can never widen what a higher tier set — the narrowing-only invariant
//     is structural, not merely tested (economics can't relax a compliance cap).
//   - An empty intersection (the incoming demand cannot be satisfied together
//     with the accumulated interval) is a same-tier conflict: the MOST
//     RESTRICTIVE demand wins (lowest ceiling), the interval collapses to it,
//     and a Conflict is recorded.
//   - Connect: false (disconnect) is the safe value and always wins; a tier that
//     holds both a true and a false demand is a recorded conflict.
func Resolve(demands []Demand) map[string]Desired {
	// Bucket by device then axis, preserving nothing about input order.
	byDevice := map[string]map[Axis][]Demand{}
	deviceOrder := []string{}
	for _, d := range demands {
		if _, ok := byDevice[d.Device]; !ok {
			byDevice[d.Device] = map[Axis][]Demand{}
			deviceOrder = append(deviceOrder, d.Device)
		}
		byDevice[d.Device][d.Axis] = append(byDevice[d.Device][d.Axis], d)
	}
	sort.Strings(deviceOrder)

	out := make(map[string]Desired, len(byDevice))
	for _, dev := range deviceOrder {
		des := Desired{Device: dev, Bounds: map[Axis]Interval{}}
		axes := byDevice[dev]
		for _, axis := range axisOrder {
			group := axes[axis]
			if len(group) == 0 {
				continue
			}
			if axis == AxisConnect {
				connect, conflicts := resolveConnect(dev, group)
				des.Connect = connect
				des.Conflicts = append(des.Conflicts, conflicts...)
				continue
			}
			iv, conflicts := resolveInterval(dev, axis, group)
			des.Bounds[axis] = iv
			des.Conflicts = append(des.Conflicts, conflicts...)
		}
		out[dev] = des
	}
	return out
}

// sortDemands orders a group deterministically: SAFETY first, then by Source.
func sortDemands(group []Demand) {
	sort.SliceStable(group, func(i, j int) bool {
		if group[i].Tier != group[j].Tier {
			return group[i].Tier < group[j].Tier
		}
		return group[i].Source < group[j].Source
	})
}

// resolveInterval intersects an axis group with STRICT tier priority (AD-007,
// TASK-063). It is a two-level fold:
//
//   - WITHIN a tier: demands intersect; an empty intersection is a same-tier
//     conflict resolved to the most-restrictive (lowest) ceiling — the legacy
//     "keep the tighter of two caps" reconciliation. This is unchanged from the
//     058 skeleton, so same-tier arbitration (two compliance caps on one axis)
//     is byte-identical.
//
//   - ACROSS tiers: tiers fold SAFETY→COMPLIANCE→ECONOMICS. A lower tier may only
//     NARROW the interval a higher tier has already fixed. When a lower tier's
//     admissible interval does NOT intersect the accumulated higher-tier interval,
//     the HIGHER tier wins outright (its interval is kept) and a cross-tier
//     conflict is recorded — the lower tier is fully clamped, never allowed to
//     move a bound.
//
// The cross-tier rule is what makes "economics can never violate a compliance or
// safety bound" STRUCTURAL rather than conventional (058's narrowing property):
// an economics PointDemand outside a compliance bound is clamped to the bound,
// not resolved by a global min that could let a lower-tier point (e.g. a charge
// setpoint more negative than a compliance discharge point) silently override the
// higher tier. See TestResolve_EconomicsPointClampedByCompliance*.
func resolveInterval(device string, axis Axis, group []Demand) (Interval, []Conflict) {
	sortDemands(group) // SAFETY first, then by Source — deterministic tier order

	var conflicts []Conflict
	accLo, accHi := math.Inf(-1), math.Inf(1)
	accSet := false // whether any higher tier has fixed the interval yet

	for _, tg := range groupByTier(group) {
		tierLo, tierHi, tierConflicts := intersectSameTier(device, axis, tg.demands)
		conflicts = append(conflicts, tierConflicts...)

		if !accSet {
			accLo, accHi, accSet = tierLo, tierHi, true
			continue
		}

		newLo := math.Max(accLo, tierLo)
		newHi := math.Min(accHi, tierHi)
		if newLo <= newHi {
			accLo, accHi = newLo, newHi
			continue
		}

		// Lower tier is infeasible inside the higher-tier interval: the higher
		// tier wins outright, the lower tier is clamped. Record the seam.
		conflicts = append(conflicts, Conflict{
			Device:  device,
			Axis:    axis,
			Tier:    tg.tier,
			Sources: [2]string{tg.demands[0].Source, tg.demands[len(tg.demands)-1].Source},
			Reason: fmt.Sprintf("%s: %s demand [%s] outside the admissible bound set by a higher tier [%s]; clamped to the higher bound",
				axis, tg.tier, fmtInterval(tierLo, tierHi), fmtInterval(accLo, accHi)),
		})
	}

	return Interval{Min: infToNaN(accLo), Max: infToNaN(accHi)}, conflicts
}

// tierGroup is one tier's slice of an axis group, in the sorted (ascending-tier)
// order sortDemands produced.
type tierGroup struct {
	tier    Tier
	demands []Demand
}

// groupByTier splits a demand group (already tier-sorted) into contiguous
// per-tier runs, preserving order so the fold visits SAFETY before COMPLIANCE
// before ECONOMICS.
func groupByTier(group []Demand) []tierGroup {
	var out []tierGroup
	for _, d := range group {
		if n := len(out); n > 0 && out[n-1].tier == d.Tier {
			out[n-1].demands = append(out[n-1].demands, d)
			continue
		}
		out = append(out, tierGroup{tier: d.Tier, demands: []Demand{d}})
	}
	return out
}

// intersectSameTier intersects demands that share one tier. An empty intersection
// collapses to the most-restrictive (lowest) ceiling and records a same-tier
// conflict — the 058 skeleton's within-tier semantics, kept bit-identical so
// same-tier arbitration does not shift under TASK-063.
func intersectSameTier(device string, axis Axis, group []Demand) (float64, float64, []Conflict) {
	lo, hi := math.Inf(-1), math.Inf(1)
	tighteningSource := ""
	var conflicts []Conflict

	for _, d := range group {
		dmin := d.Min
		if math.IsNaN(dmin) {
			dmin = math.Inf(-1)
		}
		dmax := d.Max
		if math.IsNaN(dmax) {
			dmax = math.Inf(1)
		}

		newLo := math.Max(lo, dmin)
		newHi := math.Min(hi, dmax)

		if newLo <= newHi {
			lo, hi = newLo, newHi
			if dmax < math.Inf(1) && (tighteningSource == "" || dmax <= hi) {
				tighteningSource = d.Source
			}
			continue
		}

		prev := tighteningSource
		hi = math.Min(hi, dmax)
		lo = math.Min(lo, hi)
		tighteningSource = d.Source
		conflicts = append(conflicts, Conflict{
			Device:  device,
			Axis:    axis,
			Tier:    d.Tier,
			Sources: [2]string{prev, d.Source},
			Reason: fmt.Sprintf("%s: %s bounds do not intersect; collapsed to most-restrictive ceiling %.0f",
				axis, d.Tier, hi),
		})
	}
	return lo, hi, conflicts
}

// fmtInterval renders an interval for a conflict reason, mapping ±Inf to "∞".
func fmtInterval(lo, hi float64) string {
	l, h := "-∞", "∞"
	if !math.IsInf(lo, 0) {
		l = fmt.Sprintf("%.0f", lo)
	}
	if !math.IsInf(hi, 0) {
		h = fmt.Sprintf("%.0f", hi)
	}
	return l + "," + h
}

// resolveConnect resolves the connect axis: false wins; a tier holding both a
// true and a false demand is a recorded conflict.
func resolveConnect(device string, group []Demand) (*bool, []Conflict) {
	sortDemands(group)

	anyFalse := false
	anyTrue := false
	var falseSrc, trueSrc string
	perTier := map[Tier][2]string{} // tier → {trueSrc, falseSrc}
	var conflicts []Conflict

	for _, d := range group {
		if d.Connect == nil {
			continue
		}
		pair := perTier[d.Tier]
		if *d.Connect {
			anyTrue = true
			if trueSrc == "" {
				trueSrc = d.Source
			}
			if pair[0] == "" {
				pair[0] = d.Source
			}
		} else {
			anyFalse = true
			if falseSrc == "" {
				falseSrc = d.Source
			}
			if pair[1] == "" {
				pair[1] = d.Source
			}
		}
		perTier[d.Tier] = pair
	}

	// Record same-tier true/false contentions in deterministic tier order.
	tiers := make([]Tier, 0, len(perTier))
	for t := range perTier {
		tiers = append(tiers, t)
	}
	sort.Slice(tiers, func(i, j int) bool { return tiers[i] < tiers[j] })
	for _, t := range tiers {
		pair := perTier[t]
		if pair[0] != "" && pair[1] != "" {
			conflicts = append(conflicts, Conflict{
				Device:  device,
				Axis:    AxisConnect,
				Tier:    t,
				Sources: [2]string{pair[0], pair[1]},
				Reason:  fmt.Sprintf("connect: %s wants connect, %s wants disconnect; disconnect wins", pair[0], pair[1]),
			})
		}
	}

	switch {
	case anyFalse:
		v := false
		return &v, conflicts
	case anyTrue:
		v := true
		return &v, conflicts
	default:
		return nil, conflicts
	}
}

// infToNaN maps ±Inf sentinels back to NaN (the Demand/Interval "unbounded"
// marker) so a fully-unbounded side reads as NaN, not ±Inf.
func infToNaN(v float64) float64 {
	if math.IsInf(v, 0) {
		return math.NaN()
	}
	return v
}
