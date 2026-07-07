// TASK-071 (§12): honor the CSIP server's advertised pollRate attributes
// instead of re-walking the entire resource tree on a fixed fast cadence
// regardless of what the server asked for. §12's finding: a utility
// head-end is free to rate-limit or blacklist a walker that ignores its
// advertised poll rates.
//
// Scope (narrowed from TASK-071's original per-class pollsched design, see
// the task file's status header): lexa-northbound's walk is one atomic
// discovery.Walker.Discover call that fetches the WHOLE tree every cycle —
// TASK-068 didn't change that, and this task doesn't either. So there is no
// per-class fetch scheduling here (that remains backlogged, see 10_BACKLOG
// — it also needs gridsim conditional-GET support that does not exist
// today, verified: `grep -rn "If-Modified\|ETag\|304" sim/gridsim
// sim/tlsserver` → 0 hits). What this file DOES do: pick the interval
// between whole-tree walks so no individually-advertised class pollRate is
// fetched more often than the server asked for, with an explicit,
// config-driven escape hatch (PollRateOverride) that keeps today's fixed
// cadence — required because the bench's Mayhem scenarios post controls
// and expect adoption within seconds, far faster than gridsim's
// DERControlList pollRate (60s) would allow in honor mode.
package run

import (
	"time"

	"lexa-hub/internal/northbound/discovery"
)

// PollRateMode selects how Discovery paces its walk cadence against
// server-advertised pollRate attributes.
type PollRateMode string

const (
	// PollRateHonor throttles the walk interval to the slowest cadence that
	// still respects every individually-advertised class pollRate seen in
	// the last successful walk (DeviceCapability, Time, and each discovered
	// DERProgram's DERControlList) — see effectiveInterval. This is the
	// PRODUCT default (cmd/northbound/config.go's loadConfig defaults an
	// absent/empty poll_rate_mode to this).
	PollRateHonor PollRateMode = "honor"

	// PollRateOverride ignores advertised pollRate entirely and walks at
	// the fixed configured interval every cycle — byte-identical to
	// pre-TASK-071 behavior. This is the BENCH default
	// (configs/northbound.json ships "poll_rate_mode": "override"
	// explicitly, and deploy-hub-pi.sh reinstalls that file whole on every
	// deploy, so a redeploy always reinstates it — see that script's
	// `install -m 644 $D/configs/*.json /etc/lexa/` line).
	//
	// Any Mode value other than PollRateHonor (including the Go zero value
	// "") is treated as PollRateOverride by effectiveInterval — a defensive
	// default distinct from (and deliberately opposite of) loadConfig's own
	// JSON-level default. A caller that builds a Discovery/PollRateConfig
	// directly (tests, or code that bypasses config loading) gets
	// today's-unchanged behavior unless it explicitly asks to honor
	// pollRate; only the JSON config-loading layer defaults the OTHER way,
	// toward spec-polite behavior, because that is the one path real
	// deployments actually go through.
	PollRateOverride PollRateMode = "override"
)

const (
	// pollRateMaxInterval is the absolute ceiling PollRateHonor clamps any
	// advertised pollRate to, guarding against a hostile or malformed
	// pollRate (e.g. 4e9) freezing discovery — 05 §3 plausibility
	// discipline, called out explicitly in TASK-071's "Common mistakes to
	// avoid".
	pollRateMaxInterval = 24 * time.Hour

	// clockMaxStaleness is a SEPARATE, tighter ceiling that applies in every
	// mode (TASK-071 "Things that must NOT change": clock resync
	// max-staleness ≤15 min regardless of poll_rate_mode). Every walk
	// resyncs the clock (run.go RunOnce feeds tree.ClockOffset to
	// utilitytime.Clock), so the walk interval doubles as the clock resync
	// interval; this bound exists independently of whatever pollRate a
	// server advertises on DeviceCapability or Time and is never relaxed by
	// pollRateMaxInterval or a caller-supplied PollRateConfig.MaxInterval.
	// It happens to equal gridsim's own /tm pollRate (900s) so it never
	// actually bites against gridsim, but it is enforced unconditionally,
	// not because of that coincidence.
	clockMaxStaleness = 15 * time.Minute
)

// PollRateConfig is Discovery's TASK-071 pacing configuration.
type PollRateConfig struct {
	// Mode selects PollRateHonor vs PollRateOverride. See those constants'
	// docs for the zero-value behavior.
	Mode PollRateMode

	// MaxInterval caps PollRateHonor's stretch beyond whatever the server
	// advertises. Zero (the common case) uses pollRateMaxInterval. Always
	// further clamped to clockMaxStaleness — see that constant's doc.
	MaxInterval time.Duration
}

// effectiveInterval computes the walk interval to use for the NEXT cycle.
// base is the operator-configured cadence (cmd/northbound's
// discovery_interval_s — the same knob scripts/hub-replay-tune.sh already
// tunes fast=5s/stock=20s): it is PollRateOverride's literal interval every
// cycle, AND PollRateHonor's floor (a pollRate more aggressive than the
// operator's own configured interval is clamped UP to it, never down —
// honoring a server does not mean out-polling the operator's own patience).
//
// tree is the just-completed walk's resource tree, or nil if the walk
// failed or hasn't run yet — in either case the base interval is used
// (advertised pollRate is only known from a successful walk; retrying
// promptly on failure is more useful than guessing at a stale or absent
// rate).
//
// PollRateHonor's rate itself is the MAX of every individually-advertised
// class pollRate found in tree (DeviceCapability, Time, each DERProgram's
// DERControlList) — not the min, and not just DeviceCapability's. A single
// walk interval fetches every one of those classes together, so the only
// choice that never re-fetches ANY class more often than it specifically
// asked for is the slowest of them; fetching a class less often than its
// own pollRate allows is compliant (pollRate is a floor on re-fetch
// interval, not a target), so this never under-serves the server's
// courtesy request on any class, only over-serves the more patient ones.
// (Zero/absent PollRate values — the XML omitempty case — are treated as
// "not advertised" and excluded from the max, matching the spec's own
// "if omitted, default 900s" guidance loosely: falling through to the
// operator's base interval, which is normally well under 900s anyway.)
func effectiveInterval(tree *discovery.ResourceTree, base time.Duration, cfg PollRateConfig) time.Duration {
	if base <= 0 {
		base = time.Second // defensive; callers should never pass <=0
	}
	if cfg.Mode != PollRateHonor || tree == nil {
		return base
	}

	advertisedS := advertisedPollSeconds(tree)
	if advertisedS <= 0 {
		return base
	}

	maxI := cfg.MaxInterval
	if maxI <= 0 || maxI > pollRateMaxInterval {
		maxI = pollRateMaxInterval
	}
	if maxI > clockMaxStaleness {
		maxI = clockMaxStaleness
	}

	want := time.Duration(advertisedS) * time.Second
	switch {
	case want < base:
		return base
	case want > maxI:
		return maxI
	default:
		return want
	}
}

// advertisedPollSeconds returns the maximum PollRate (in seconds) advertised
// across DeviceCapability, Time, and every discovered DERProgram's
// DERControlList in tree. Zero (omitted-on-the-wire) values are ignored.
// Returns 0 if nothing in the tree advertised a pollRate at all.
func advertisedPollSeconds(tree *discovery.ResourceTree) uint32 {
	var max uint32
	upd := func(v uint32) {
		if v > max {
			max = v
		}
	}
	if tree.DeviceCapability != nil {
		upd(tree.DeviceCapability.PollRate)
	}
	if tree.Time != nil {
		upd(tree.Time.PollRate)
	}
	for _, p := range tree.Programs {
		if p.Controls != nil {
			upd(p.Controls.PollRate)
		}
	}
	return max
}
