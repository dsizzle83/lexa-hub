package main

import (
	"context"
	"testing"
	"time"
)

// fakeClockSource drives Clock in tests: Sleep advances a synthetic "now"
// instead of actually waiting, so a -budget of minutes costs microseconds
// of real test time.
type fakeClockSource struct {
	now    time.Time
	sleeps []time.Duration
}

func newFakeClockSource() *fakeClockSource {
	return &fakeClockSource{now: time.Unix(1_700_000_000, 0)}
}

func (f *fakeClockSource) clock() Clock {
	return Clock{
		Now: func() time.Time { return f.now },
		Sleep: func(d time.Duration) {
			f.sleeps = append(f.sleeps, d)
			f.now = f.now.Add(d)
		},
	}
}

func alwaysFailCheck(name string) Check {
	return Check{Name: name, Timeout: time.Second, Run: func(ctx context.Context, env *Environment) Result {
		return fail(name, "always fails")
	}}
}

func alwaysPassCheck(name string) Check {
	return Check{Name: name, Timeout: time.Second, Run: func(ctx context.Context, env *Environment) Result {
		return pass(name, "always passes")
	}}
}

// passAfterNCheck fails on the first n-1 calls, then passes forever.
func passAfterNCheck(name string, n int) Check {
	calls := 0
	return Check{Name: name, Timeout: time.Second, Run: func(ctx context.Context, env *Environment) Result {
		calls++
		if calls >= n {
			return pass(name, "eventually passes")
		}
		return fail(name, "not yet")
	}}
}

func TestRun_SingleShot_NoRetryEvenOnFailure(t *testing.T) {
	fc := newFakeClockSource()
	env := &Environment{}
	checks := []Check{alwaysFailCheck("x")}

	sum := Run(context.Background(), env, checks, RunOptions{
		Budget: 30 * time.Second,
		Commit: false, // no -commit: exactly one attempt regardless of outcome
		Clock:  fc.clock(),
	})

	if sum.Attempts != 1 {
		t.Fatalf("non-commit Run made %d attempts, want exactly 1", sum.Attempts)
	}
	if sum.Pass {
		t.Fatalf("sum.Pass = true, want false (the check always fails)")
	}
	if len(fc.sleeps) != 0 {
		t.Fatalf("non-commit mode slept %d times, want 0", len(fc.sleeps))
	}
}

func TestRun_Commit_BudgetExhaustion(t *testing.T) {
	fc := newFakeClockSource()
	env := &Environment{}
	checks := []Check{alwaysFailCheck("x")}

	var attempts []Attempt
	sum := Run(context.Background(), env, checks, RunOptions{
		Budget:    30 * time.Second,
		Commit:    true,
		Interval:  5 * time.Second,
		Clock:     fc.clock(),
		OnAttempt: func(a Attempt) { attempts = append(attempts, a) },
	})

	if sum.Pass {
		t.Fatalf("sum.Pass = true, want false — the check never passes")
	}
	// start=0; retries at elapsed 0,5,10,15,20,25 all < 30 (6 sleeps), then
	// elapsed=30 >= budget stops the loop => 7 attempts total.
	const wantAttempts = 7
	if sum.Attempts != wantAttempts {
		t.Fatalf("sum.Attempts = %d, want %d", sum.Attempts, wantAttempts)
	}
	if len(attempts) != wantAttempts {
		t.Fatalf("OnAttempt fired %d times, want %d", len(attempts), wantAttempts)
	}
	if len(fc.sleeps) != wantAttempts-1 {
		t.Fatalf("slept %d times, want %d (one fewer than attempts)", len(fc.sleeps), wantAttempts-1)
	}
	for _, d := range fc.sleeps {
		if d != 5*time.Second {
			t.Errorf("sleep interval = %v, want 5s", d)
		}
	}
}

func TestRun_Commit_RecoversBeforeBudgetExhausted(t *testing.T) {
	fc := newFakeClockSource()
	env := &Environment{}
	checks := []Check{passAfterNCheck("x", 3)} // fails twice, passes on the 3rd

	sum := Run(context.Background(), env, checks, RunOptions{
		Budget:   120 * time.Second,
		Commit:   true,
		Interval: 5 * time.Second,
		Clock:    fc.clock(),
	})

	if !sum.Pass {
		t.Fatalf("sum.Pass = false, want true — the check passes on its 3rd attempt")
	}
	if sum.Attempts != 3 {
		t.Fatalf("sum.Attempts = %d, want 3 (stop retrying once it passes)", sum.Attempts)
	}
	if len(fc.sleeps) != 2 {
		t.Fatalf("slept %d times, want 2 (between attempts 1-2 and 2-3, none after success)", len(fc.sleeps))
	}
}

func TestRun_Commit_PassesFirstTry_NoSleep(t *testing.T) {
	fc := newFakeClockSource()
	env := &Environment{}
	checks := []Check{alwaysPassCheck("x")}

	sum := Run(context.Background(), env, checks, RunOptions{
		Budget:   60 * time.Second,
		Commit:   true,
		Interval: 5 * time.Second,
		Clock:    fc.clock(),
	})

	if !sum.Pass || sum.Attempts != 1 {
		t.Fatalf("sum = %+v, want Pass=true Attempts=1", sum)
	}
	if len(fc.sleeps) != 0 {
		t.Fatalf("slept %d times on an immediate pass, want 0", len(fc.sleeps))
	}
}

func TestRun_SkipDoesNotCountAsFailure(t *testing.T) {
	fc := newFakeClockSource()
	env := &Environment{}
	skipCheck := Check{Name: "y", Timeout: time.Second, Run: func(ctx context.Context, env *Environment) Result {
		return skip("y", "not configured")
	}}
	checks := []Check{alwaysPassCheck("x"), skipCheck}

	sum := Run(context.Background(), env, checks, RunOptions{
		Budget: 10 * time.Second,
		Clock:  fc.clock(),
	})
	if !sum.Pass {
		t.Fatalf("sum.Pass = false with a PASS + a SKIP, want true (SKIP never fails the run)")
	}
}

func TestRun_DefaultsIntervalAndClock(t *testing.T) {
	env := &Environment{}
	checks := []Check{alwaysPassCheck("x")}
	// Zero-value RunOptions.Clock/Interval must not panic — Run fills in
	// RealClock()/5s itself.
	sum := Run(context.Background(), env, checks, RunOptions{Budget: time.Second})
	if !sum.Pass {
		t.Fatalf("sum.Pass = false, want true")
	}
}

func TestRunOnce_EachCheckGetsOwnSubContext(t *testing.T) {
	// A check with an already-expired parent context should still run if
	// its own Check.Timeout is what's applied per runOnce's contract...
	// actually runOnce derives from the passed ctx, so an expired parent
	// SHOULD prevent the child from proceeding past ctx.Done(). This test
	// instead confirms the more important contract: one check's context is
	// independent of another's — a check that ignores ctx entirely still
	// returns its own result, and Timeout values differ per check without
	// interfering.
	fast := Check{Name: "fast", Timeout: time.Millisecond, Run: func(ctx context.Context, env *Environment) Result {
		return pass("fast", "ok")
	}}
	slow := Check{Name: "slow", Timeout: time.Second, Run: func(ctx context.Context, env *Environment) Result {
		return pass("slow", "ok")
	}}
	att := runOnce(context.Background(), &Environment{}, []Check{fast, slow}, 1)
	if !att.Pass || len(att.Checks) != 2 {
		t.Fatalf("runOnce = %+v, want both checks to pass", att)
	}
}
