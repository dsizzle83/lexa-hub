package main

import (
	"context"
	"time"
)

// SummaryV is the JSON summary's schema version — bump only on a breaking
// change to Summary's own shape (mirrors the bus.Envelope/journal.Event
// "append-only" habit used elsewhere in this codebase).
const SummaryV = 1

// Attempt is one full pass over every Check.
type Attempt struct {
	N      int      `json:"n"`
	Checks []Result `json:"checks"`
	Pass   bool     `json:"pass"`
}

// Summary is the stable JSON schema printed to stdout (spec: "machine-
// readable JSON summary to stdout").
type Summary struct {
	V         int      `json:"v"`
	Ts        string   `json:"ts"`
	BudgetS   float64  `json:"budget_s"`
	Commit    bool     `json:"commit"`
	Attempts  int      `json:"attempts"`
	DurationS float64  `json:"duration_s"`
	Pass      bool     `json:"pass"`
	Checks    []Result `json:"checks"` // the FINAL attempt's per-check results
}

// Clock abstracts wall-clock reads/waits so the -commit retry loop is
// deterministically testable: budget exhaustion must not cost a real 120s
// in the test suite (run_test.go supplies a fake whose Sleep advances a
// synthetic clock instead of actually waiting).
type Clock struct {
	Now   func() time.Time
	Sleep func(d time.Duration)
}

// RealClock is the on-device Clock: real time.Now/time.Sleep.
func RealClock() Clock {
	return Clock{Now: time.Now, Sleep: time.Sleep}
}

// RunOptions configures one Run invocation.
type RunOptions struct {
	Budget   time.Duration
	Commit   bool
	Interval time.Duration // retry interval in -commit mode; Run defaults this to 5s if zero
	Clock    Clock

	// OnAttempt, if non-nil, is called synchronously right after each
	// attempt completes (before any retry sleep) — main.go uses this to
	// stream stderr lines in real time across a long -commit retry loop
	// instead of only reporting the final attempt once everything is over.
	OnAttempt func(Attempt)
}

// runOnce runs every check with its own bounded sub-context (derived from
// ctx, which itself carries the overall -budget deadline set by Run) and
// collects one Attempt.
func runOnce(ctx context.Context, env *Environment, checks []Check, n int) Attempt {
	att := Attempt{N: n, Pass: true}
	for _, c := range checks {
		cctx, cancel := context.WithTimeout(ctx, c.Timeout)
		res := c.Run(cctx, env)
		cancel()
		att.Checks = append(att.Checks, res)
		if res.Status == StatusFail {
			att.Pass = false
		}
	}
	return att
}

// Run executes the check set once, or — in -commit mode — retries the
// full set every opts.Interval until it passes or opts.Budget elapses
// (spec: "-commit mode retries the full set until pass or budget
// exhausted"). The overall wall-clock budget bounds both modes: ctx itself
// carries a real context.WithTimeout(opts.Budget) deadline, from which
// every check's own per-check sub-context (Check.Timeout) is derived, so
// even a single ad-hoc (non -commit) run cannot hang past the budget.
//
// SKIP never counts as failure: Attempt.Pass is false only when at least
// one check reports FAIL (see runOnce) — a device with no EV stations
// configured, for example, must not be held hostage by an unrelated
// modbus SKIP.
func Run(ctx context.Context, env *Environment, checks []Check, opts RunOptions) Summary {
	if opts.Interval <= 0 {
		opts.Interval = 5 * time.Second
	}
	if opts.Clock.Now == nil || opts.Clock.Sleep == nil {
		opts.Clock = RealClock()
	}

	ctx, cancel := context.WithTimeout(ctx, opts.Budget)
	defer cancel()

	start := opts.Clock.Now()
	n := 0
	var last Attempt
	for {
		n++
		last = runOnce(ctx, env, checks, n)
		if opts.OnAttempt != nil {
			opts.OnAttempt(last)
		}
		if last.Pass || !opts.Commit {
			break
		}
		if elapsed := opts.Clock.Now().Sub(start); elapsed >= opts.Budget {
			break
		}
		if ctx.Err() != nil {
			break
		}
		opts.Clock.Sleep(opts.Interval)
	}

	return Summary{
		V:         SummaryV,
		Ts:        opts.Clock.Now().UTC().Format(time.RFC3339),
		BudgetS:   opts.Budget.Seconds(),
		Commit:    opts.Commit,
		Attempts:  n,
		DurationS: opts.Clock.Now().Sub(start).Seconds(),
		Pass:      last.Pass,
		Checks:    last.Checks,
	}
}
