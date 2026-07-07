package constraint

// BatterySafetySession is the BatterySafetyConstraint's typed inter-tick state.
// It consolidates the three remaining scattered guard fields DefaultOptimizer
// kept for battery protection (optimizer.go:152-179) into one owner, completing
// the W1/R4 "one owner per concept" state model:
//
//	drainTicks    — ports battDrainTicks (optimizer.go:152-156). Per battery,
//	                consecutive ticks the pack MEASURED discharging at/below its
//	                SOC reserve. No rule ever commands a discharge below reserve,
//	                so a sustained count is a device inverting/ignoring its
//	                setpoint (audit: battery-wrong-sign). Reset to 0 on any
//	                compliant tick; the map entry is PRUNED when the device
//	                disappears or reads a NaN SOC — matching the legacy
//	                delete(o.battDrainTicks, name) at optimizer.go:1529.
//	wrongDirTicks — ports battWrongDirTicks (optimizer.go:158-162). Per battery,
//	                consecutive ticks the hub commanded charge (negative setpoint)
//	                but the pack measured discharging, regardless of SOC — catches
//	                a sign-flipped pack the SOC-reserve check misses at high SOC
//	                (audit: battery-wrong-sign). Reset/prune identical to
//	                drainTicks (optimizer.go:1530).
//	lastCmdW      — ports lastBattCmd (optimizer.go:173-179). Last battery
//	                setpoint (W; <0 = charge) the ECONOMIC tick committed, recorded
//	                via RecordCommands. The fast protection path (EvaluateFast /
//	                legacy EvaluateSafety) reads it BETWEEN economic ticks — when
//	                no fresh plan exists — to infer whether a measured discharge
//	                contradicts a commanded charge. Written and read on the ONE
//	                engine control goroutine (engine.go run() serialises tick() and
//	                safetyTick()), so it needs no lock — a mutex here would hide a
//	                future contract violation instead of tripping -race (TASK-062
//	                common-mistakes). Like legacy lastBattCmd it is NEVER pruned:
//	                it is only ever overwritten by a later command for the same
//	                device (optimizer.go:383-386 writes, never deletes).
//
// Reset domains: drainTicks/wrongDirTicks are debounce counters reset by their
// OWN compliant-tick logic and pruned on device disappearance — they do NOT
// follow a CSIP cap-value cadence the way the compliance sessions
// (Export/Import/Gen) do, because battery safety is a measured-effect protection
// with no CSIP limit to track. That is why it is a SAFETY-tier session, not a
// compliance one (see BatterySafetyConstraint doc + 02 AD-007).
type BatterySafetySession struct {
	drainTicks    map[string]int
	wrongDirTicks map[string]int
	lastCmdW      map[string]float64
}

// newBatterySafetySession returns an empty session with all three maps
// allocated, mirroring NewDefaultOptimizer (optimizer.go:243-245).
func newBatterySafetySession() BatterySafetySession {
	return BatterySafetySession{
		drainTicks:    make(map[string]int),
		wrongDirTicks: make(map[string]int),
		lastCmdW:      make(map[string]float64),
	}
}

// prune drops the debounce-counter entries for a device that has disappeared or
// reads a NaN SOC — the pack is offline/unmeasurable, so its consecutive-tick
// history is meaningless and must not leak (the registries sync.Map lesson,
// §11). Ports the paired delete()s at optimizer.go:1529-1530. lastCmdW is NOT
// pruned here, exactly as legacy leaves lastBattCmd in place across a transient
// disconnect so the command is still known when the pack returns.
func (s *BatterySafetySession) prune(name string) {
	delete(s.drainTicks, name)
	delete(s.wrongDirTicks, name)
}

// chargeCommanded reports whether the last committed economic command for this
// pack was a charge (negative setpoint). Ports the lastBattCmd leg of
// chargeCommandedFor (optimizer.go:1481-1485) — the fast path's only source of
// commanded intent, since it runs with no fresh plan. Absent = not commanded to
// charge (false), matching the map-miss return.
func (s *BatterySafetySession) chargeCommanded(name string) bool {
	sp, ok := s.lastCmdW[name]
	return ok && sp < 0
}
