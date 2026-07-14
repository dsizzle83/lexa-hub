package openadr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeVTN is a minimal httptest VTN: programs + per-program events,
// mutable between polls.
type fakeVTN struct {
	mu       sync.Mutex
	programs []Program
	events   map[string][]Event // programID → events
	failAll  bool
}

func (f *fakeVTN) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failAll {
			http.Error(w, "vtn down", http.StatusInternalServerError)
			return
		}
		switch r.URL.Path {
		case "/programs":
			_ = json.NewEncoder(w).Encode(f.programs)
		case "/events":
			_ = json.NewEncoder(w).Encode(f.events[r.URL.Query().Get("programID")])
		default:
			http.NotFound(w, r)
		}
	}
}

// TestVENPollOnceEndToEnd: programs fetched, filtered by program_ids, events
// reconciled, prices/limits translated, counts populated.
func TestVENPollOnceEndToEnd(t *testing.T) {
	vtn := &fakeVTN{
		programs: []Program{
			{ID: "prog-1", ProgramName: "cp-tariff", PayloadDescriptors: []PayloadDescriptor{
				{PayloadType: "PRICE", Units: "KWH", Currency: "USD"},
			}},
			{ID: "prog-other", ProgramName: "ignored"},
		},
		events: map[string][]Event{
			"prog-1": {{
				ID: "evt-1", ProgramID: "prog-1",
				IntervalPeriod: &IntervalPeriod{Start: "2026-07-14T08:00:00Z", Duration: "PT1H"},
				Intervals:      []Interval{{ID: 0, Payloads: []ValuesMap{{Type: "PRICE", Values: []any{0.25}}}}},
			}},
			"prog-other": {{ID: "evt-x", ProgramID: "prog-other"}},
		},
	}
	srv := httptest.NewServer(vtn.handler())
	defer srv.Close()

	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	ven := New(c, []string{"prog-1"}, "lexa-hub")
	ven.SetNow(func() time.Time { return now14 })

	res, err := ven.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if res.ProgramCount != 1 {
		t.Fatalf("ProgramCount = %d, want 1 (filtered)", res.ProgramCount)
	}
	if len(res.Diff.Added) != 1 || res.Diff.Added[0] != "evt-1" {
		t.Fatalf("Diff = %+v", res.Diff)
	}
	if res.ActiveEvents != 1 {
		t.Fatalf("ActiveEvents = %d, want 1", res.ActiveEvents)
	}
	if len(res.Prices.Series) != 1 || res.Prices.Series[0].Currency != "USD" {
		t.Fatalf("prices = %+v (program-level descriptor must apply)", res.Prices.Series)
	}
	if res.Prices.Series[0].Intervals[0].Value != 0.25 {
		t.Fatalf("price = %v", res.Prices.Series[0].Intervals[0].Value)
	}

	// Second poll, unchanged VTN: no lifecycle transitions.
	res, err = ven.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("second PollOnce: %v", err)
	}
	if !res.Diff.Empty() {
		t.Fatalf("unchanged VTN produced diff %+v", res.Diff)
	}

	// VTN deletes the event: diff reports it and the doc series drains.
	vtn.mu.Lock()
	vtn.events["prog-1"] = nil
	vtn.mu.Unlock()
	res, err = ven.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("third PollOnce: %v", err)
	}
	if len(res.Diff.Deleted) != 1 || res.Diff.Deleted[0] != "evt-1" {
		t.Fatalf("Diff after delete = %+v", res.Diff)
	}
	if len(res.Prices.Series) != 0 {
		t.Fatalf("deleted event still priced: %+v", res.Prices.Series)
	}
}

// TestVENPollFailureLeavesStoreUnchanged: a failing VTN mid-lifecycle must
// NOT read as "all events deleted" — the store (and hence the retained bus
// docs, which cmd/openadr only republishes from successful polls) holds
// last-known-good.
func TestVENPollFailureLeavesStoreUnchanged(t *testing.T) {
	vtn := &fakeVTN{
		programs: []Program{{ID: "prog-1"}},
		events: map[string][]Event{
			"prog-1": {{
				ID: "evt-1", ProgramID: "prog-1",
				IntervalPeriod: &IntervalPeriod{Start: "2026-07-14T08:00:00Z", Duration: "PT1H"},
				Intervals:      []Interval{{ID: 0}},
			}},
		},
	}
	srv := httptest.NewServer(vtn.handler())
	defer srv.Close()

	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	ven := New(c, nil, "lexa-hub")
	ven.SetNow(func() time.Time { return now14 })

	if _, err := ven.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if ven.Store().Len() != 1 {
		t.Fatalf("store len = %d", ven.Store().Len())
	}

	vtn.mu.Lock()
	vtn.failAll = true
	vtn.mu.Unlock()
	if _, err := ven.PollOnce(context.Background()); err == nil {
		t.Fatal("PollOnce succeeded against a failing VTN")
	}
	if ven.Store().Len() != 1 {
		t.Fatalf("store drained by a failed poll: len = %d, want 1 (fail-closed)", ven.Store().Len())
	}
}
