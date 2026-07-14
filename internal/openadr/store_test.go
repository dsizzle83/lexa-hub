package openadr

import (
	"reflect"
	"testing"
	"time"
)

func ev(id, name string) Event {
	return Event{
		ID: id, ProgramID: "prog-1", EventName: name,
		IntervalPeriod: &IntervalPeriod{Start: "2026-07-14T08:00:00Z", Duration: "PT15M"},
		Intervals:      []Interval{{ID: 0, Payloads: []ValuesMap{{Type: "PRICE", Values: []any{0.1}}}}},
	}
}

// TestReconcileAddUpdateDelete exercises the poll-based lifecycle: first
// poll adds, a content change updates, disappearance deletes, and an
// unchanged event produces no transition at all.
func TestReconcileAddUpdateDelete(t *testing.T) {
	s := NewEventStore()

	d := s.Reconcile([]Event{ev("e1", "a"), ev("e2", "b")})
	if !reflect.DeepEqual(d.Added, []string{"e1", "e2"}) || len(d.Updated) != 0 || len(d.Deleted) != 0 {
		t.Fatalf("first poll diff = %+v", d)
	}

	// Unchanged content (even after a round of decode) — no transitions.
	d = s.Reconcile([]Event{ev("e1", "a"), ev("e2", "b")})
	if !d.Empty() {
		t.Fatalf("unchanged poll produced diff %+v", d)
	}

	// e1 content changes; e2 vanishes (implied cancel); e3 appears.
	e1v2 := ev("e1", "a")
	e1v2.Intervals[0].Payloads[0].Values = []any{0.2}
	d = s.Reconcile([]Event{e1v2, ev("e3", "c")})
	if !reflect.DeepEqual(d.Updated, []string{"e1"}) {
		t.Errorf("Updated = %v, want [e1]", d.Updated)
	}
	if !reflect.DeepEqual(d.Deleted, []string{"e2"}) {
		t.Errorf("Deleted = %v, want [e2]", d.Deleted)
	}
	if !reflect.DeepEqual(d.Added, []string{"e3"}) {
		t.Errorf("Added = %v, want [e3]", d.Added)
	}
	if s.Len() != 2 {
		t.Fatalf("store len = %d, want 2", s.Len())
	}
}

// TestRandomizeStartAssignedOnceAndKept: a randomizeStart event gets one
// offset within the bound at first sight, and a content UPDATE keeps it
// (randomization is applied once per event, never re-rolled).
func TestRandomizeStartAssignedOnceAndKept(t *testing.T) {
	s := NewEventStore()
	calls := 0
	s.SetRandFn(func(max time.Duration) time.Duration {
		calls++
		if max != 5*time.Minute {
			t.Errorf("randomizeStart bound = %v, want 5m", max)
		}
		return 90 * time.Second
	})

	e := ev("e1", "a")
	e.IntervalPeriod.RandomizeStart = "PT5M"
	s.Reconcile([]Event{e})
	inst := s.Instances()
	if len(inst) != 1 || inst[0].RandOffset != 90*time.Second {
		t.Fatalf("instances = %+v, want one with 90s offset", inst)
	}

	// Content update: offset survives, randFn not re-consulted.
	e2 := e
	e2.EventName = "renamed"
	d := s.Reconcile([]Event{e2})
	if !reflect.DeepEqual(d.Updated, []string{"e1"}) {
		t.Fatalf("diff = %+v", d)
	}
	inst = s.Instances()
	if inst[0].RandOffset != 90*time.Second {
		t.Fatalf("offset after update = %v, want 90s (kept)", inst[0].RandOffset)
	}
	if calls != 1 {
		t.Fatalf("randFn called %d times, want 1", calls)
	}
}
