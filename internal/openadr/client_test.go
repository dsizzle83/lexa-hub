package openadr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

// TestProgramsPagination: /programs served in skip/limit pages is drained to
// completion (3 pages: 2+2+1 at limit 2), with the right skip sequence.
func TestProgramsPagination(t *testing.T) {
	all := []Program{
		{ID: "p1"}, {ID: "p2"}, {ID: "p3"}, {ID: "p4"}, {ID: "p5"},
	}
	var mu sync.Mutex
	var skips []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/programs" {
			http.NotFound(w, r)
			return
		}
		skip, _ := strconv.Atoi(r.URL.Query().Get("skip"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		mu.Lock()
		skips = append(skips, skip)
		mu.Unlock()
		end := skip + limit
		if skip > len(all) {
			skip = len(all)
		}
		if end > len(all) {
			end = len(all)
		}
		_ = json.NewEncoder(w).Encode(all[skip:end])
	}))
	defer srv.Close()

	c := &Client{Base: srv.URL, HTTP: srv.Client(), PageLimit: 2}
	got, err := c.Programs(context.Background())
	if err != nil {
		t.Fatalf("Programs: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d programs, want 5", len(got))
	}
	for i, p := range got {
		if p.ID != all[i].ID {
			t.Errorf("programs[%d].ID = %q, want %q", i, p.ID, all[i].ID)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	want := []int{0, 2, 4}
	if len(skips) != len(want) {
		t.Fatalf("skip sequence %v, want %v", skips, want)
	}
	for i := range want {
		if skips[i] != want[i] {
			t.Fatalf("skip sequence %v, want %v", skips, want)
		}
	}
}

// TestEventsQueryCarriesProgramID pins the /events?programID= filter.
func TestEventsQueryCarriesProgramID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("programID"); got != "prog-9" {
			t.Errorf("programID = %q, want prog-9", got)
		}
		_ = json.NewEncoder(w).Encode([]Event{{ID: "e1", ProgramID: "prog-9"}})
	}))
	defer srv.Close()

	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	evs, err := c.Events(context.Background(), "prog-9")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(evs) != 1 || evs[0].ID != "e1" {
		t.Fatalf("events = %+v", evs)
	}
}

// TestPaginationRunawayCap: a VTN that always returns a full page trips the
// maxPages bound instead of looping forever.
func TestPaginationRunawayCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]Program{{ID: "same"}, {ID: "same2"}})
	}))
	defer srv.Close()

	c := &Client{Base: srv.URL, HTTP: srv.Client(), PageLimit: 2}
	if _, err := c.Programs(context.Background()); err == nil {
		t.Fatal("runaway pagination not capped")
	}
}

// TestEnsureVenFindsExisting: GET-first — a ven already registered under
// this venName is adopted without any POST (the restart-idempotence
// contract documented on Client.EnsureVen).
func TestEnsureVenFindsExisting(t *testing.T) {
	var posted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/vens":
			if got := r.URL.Query().Get("venName"); got != "lexa-hub" {
				t.Errorf("venName filter = %q", got)
			}
			_ = json.NewEncoder(w).Encode([]Ven{{ID: "ven-42", VenName: "lexa-hub"}})
		case r.Method == http.MethodPost && r.URL.Path == "/vens":
			posted = true
			http.Error(w, "conflict", http.StatusConflict)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	id, err := c.EnsureVen(context.Background(), "lexa-hub")
	if err != nil {
		t.Fatalf("EnsureVen: %v", err)
	}
	if id != "ven-42" {
		t.Fatalf("venID = %q, want ven-42", id)
	}
	if posted {
		t.Fatal("POST /vens fired despite an existing ven")
	}
}

// TestEnsureVenCreates: an empty GET result leads to exactly one POST /vens
// whose response ID is adopted.
func TestEnsureVenCreates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/vens":
			_ = json.NewEncoder(w).Encode([]Ven{})
		case r.Method == http.MethodPost && r.URL.Path == "/vens":
			var v Ven
			_ = json.NewDecoder(r.Body).Decode(&v)
			if v.VenName != "lexa-hub" {
				t.Errorf("POST venName = %q", v.VenName)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(Ven{ID: "ven-new", VenName: v.VenName})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	id, err := c.EnsureVen(context.Background(), "lexa-hub")
	if err != nil {
		t.Fatalf("EnsureVen: %v", err)
	}
	if id != "ven-new" {
		t.Fatalf("venID = %q, want ven-new", id)
	}
}

// TestPostReportShape pins the wire shape of a POSTed report: path, method,
// content type, and the JSON body's key fields (programID/eventID/
// clientName/payload descriptor/resource intervals).
func TestPostReportShape(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/reports" {
			t.Errorf("got %s %s, want POST /reports", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode report body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"r1"}`)
	}))
	defer srv.Close()

	kwh := 1.25
	rep := Report{
		ProgramID:  "prog-1",
		EventID:    "evt-1",
		ClientName: "lexa-hub",
		PayloadDescriptors: []PayloadDescriptor{{
			ObjectType: "REPORT_PAYLOAD_DESCRIPTOR", PayloadType: "USAGE",
			ReadingType: "DIRECT_READ", Units: "KWH",
		}},
		Resources: []ReportResource{{
			ResourceName:   "meter-0",
			IntervalPeriod: &IntervalPeriod{Start: "2026-07-14T08:00:00Z", Duration: "PT60S"},
			Intervals: []Interval{{
				ID:       0,
				Payloads: []ValuesMap{{Type: "USAGE", Values: []any{kwh}}},
			}},
		}},
	}
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	if err := c.PostReport(context.Background(), rep); err != nil {
		t.Fatalf("PostReport: %v", err)
	}

	if body["objectType"] != "REPORT" {
		t.Errorf("objectType = %v, want REPORT (defaulted)", body["objectType"])
	}
	if body["programID"] != "prog-1" || body["eventID"] != "evt-1" || body["clientName"] != "lexa-hub" {
		t.Errorf("report identity fields wrong: %v", body)
	}
	res := body["resources"].([]any)[0].(map[string]any)
	if res["resourceName"] != "meter-0" {
		t.Errorf("resourceName = %v", res["resourceName"])
	}
	payload := res["intervals"].([]any)[0].(map[string]any)["payloads"].([]any)[0].(map[string]any)
	if payload["type"] != "USAGE" {
		t.Errorf("payload type = %v", payload["type"])
	}
	if v := payload["values"].([]any)[0].(float64); v != kwh {
		t.Errorf("payload value = %v, want %v", v, kwh)
	}
}
