package main

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
)

// recordingPublish is the pendingStations publish seam fake: it records
// every doc handed to it and can be told to fail once (to exercise the
// log-and-continue error path in pendingStations.doPublish).
type recordingPublish struct {
	mu    sync.Mutex
	docs  []bus.PendingStations
	failN int // number of leading calls that should return an error
}

func (r *recordingPublish) fn(doc bus.PendingStations) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failN > 0 {
		r.failN--
		return errors.New("publish failed")
	}
	r.docs = append(r.docs, doc)
	return nil
}

func (r *recordingPublish) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.docs)
}

func (r *recordingPublish) last() bus.PendingStations {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.docs[len(r.docs)-1]
}

// TestPendingStations_EmptyDocAtStartup: publishStartup unconditionally
// publishes the (empty) current doc, bypassing the rate limiter, so a fresh
// process clears any stale retained lexa/ocpp/pending left from a prior run
// even before anything has connected.
func TestPendingStations_EmptyDocAtStartup(t *testing.T) {
	rec := &recordingPublish{}
	p := newPendingStations(nil, rec.fn, nil)
	t0 := time.Now()

	p.publishStartup(t0)

	if rec.count() != 1 {
		t.Fatalf("publishStartup: got %d publishes, want 1", rec.count())
	}
	doc := rec.last()
	if len(doc.Stations) != 0 {
		t.Errorf("startup doc has %d stations, want 0 (empty set)", len(doc.Stations))
	}
	if doc.Ts != t0.Unix() {
		t.Errorf("startup doc Ts = %d, want %d", doc.Ts, t0.Unix())
	}
}

// TestPendingStations_UpsertThenBootNotificationUpdates: an initial upsert
// (as onConnect would call, with RemoteAddr but no vendor/model yet) is
// later updated by a BootNotification-shaped upsert (vendor/model known, no
// RemoteAddr available) — the two calls must compose into ONE entry, not a
// duplicate, with FirstSeen preserved from the first sighting and RemoteAddr
// preserved across the second (blank fields never overwrite a populated one).
func TestPendingStations_UpsertThenBootNotificationUpdates(t *testing.T) {
	rec := &recordingPublish{}
	p := newPendingStations(nil, rec.fn, nil)
	t0 := time.Now()

	p.upsert("cs-099", "", "", "10.0.0.5:5000", t0)                   // onConnect-shaped
	p.upsert("cs-099", "AcmeEV", "Fast50", "", t0.Add(2*time.Second)) // BootNotification-shaped

	doc := rec.last()
	if len(doc.Stations) != 1 {
		t.Fatalf("got %d entries, want exactly 1 (update, not duplicate)", len(doc.Stations))
	}
	s := doc.Stations[0]
	if s.StationID != "cs-099" || s.Vendor != "AcmeEV" || s.ModelName != "Fast50" {
		t.Errorf("entry = %+v, want vendor/model filled in by the second (BootNotification) call", s)
	}
	if s.RemoteAddr != "10.0.0.5:5000" {
		t.Errorf("RemoteAddr = %q, want the first call's value preserved", s.RemoteAddr)
	}
	if s.FirstSeenTs != t0.Unix() {
		t.Errorf("FirstSeenTs = %d, want %d (the FIRST sighting, unchanged by the update)", s.FirstSeenTs, t0.Unix())
	}
}

// TestPendingStations_PublishRateLimit drives the component with a fake
// clock and asserts publishes are suppressed inside pendingPublishMinInterval
// and resume once it elapses (the rewalkRateLimit-style policy).
func TestPendingStations_PublishRateLimit(t *testing.T) {
	rec := &recordingPublish{}
	p := newPendingStations(nil, rec.fn, nil)
	t0 := time.Now()

	p.upsert("cs-A", "", "", "", t0) // first-ever call: always publishes
	if rec.count() != 1 {
		t.Fatalf("first upsert: got %d publishes, want 1", rec.count())
	}

	p.upsert("cs-B", "", "", "", t0.Add(500*time.Millisecond)) // within 1s: suppressed
	if rec.count() != 1 {
		t.Fatalf("upsert within rate-limit window: got %d publishes, want still 1", rec.count())
	}

	p.upsert("cs-C", "", "", "", t0.Add(1500*time.Millisecond)) // past 1s: resumes
	if rec.count() != 2 {
		t.Fatalf("upsert past rate-limit window: got %d publishes, want 2", rec.count())
	}
	// The resumed publish reflects ALL accumulated entries (A, B, C), not
	// just the one that triggered it — upsert always mutates the map first.
	if len(rec.last().Stations) != 3 {
		t.Errorf("resumed publish has %d stations, want 3 (A, B, C all accumulated)", len(rec.last().Stations))
	}
}

// TestPendingStations_24hPrune: an entry not refreshed within
// pendingStationTTL is dropped the next time anything triggers a publish.
func TestPendingStations_24hPrune(t *testing.T) {
	rec := &recordingPublish{}
	p := newPendingStations(nil, rec.fn, nil)
	t0 := time.Now()

	p.upsert("cs-stale", "", "", "", t0)
	if len(rec.last().Stations) != 1 {
		t.Fatalf("setup: expected cs-stale present, got %+v", rec.last())
	}

	// 25h later a different station connects — past the rate limit AND past
	// the TTL for cs-stale, which is pruned during THIS publish.
	p.upsert("cs-fresh", "", "", "", t0.Add(25*time.Hour))

	doc := rec.last()
	if len(doc.Stations) != 1 || doc.Stations[0].StationID != "cs-fresh" {
		t.Fatalf("after 25h, doc = %+v, want only cs-fresh (cs-stale pruned)", doc.Stations)
	}
}

// TestPendingStations_DropOnConfigured: a station ID present in cfg.Stations
// at construction is never tracked as pending, regardless of how many times
// it "connects" — this is the "approval = config-write + restart" contract:
// a fresh process's configured set already excludes it.
func TestPendingStations_DropOnConfigured(t *testing.T) {
	rec := &recordingPublish{}
	p := newPendingStations([]string{"cs-002"}, rec.fn, nil)
	t0 := time.Now()

	p.upsert("cs-002", "Acme", "Model-X", "10.0.0.9:1234", t0)

	// upsert on an already-configured ID is a silent no-op: no entry, no
	// publish attempt at all (not even an empty one) from this call.
	if rec.count() != 0 {
		t.Fatalf("upsert of a configured station published %d times, want 0", rec.count())
	}
}

// TestPendingStations_DocShapeStampsV1 marshals a doc built by
// buildDocLocked and checks it carries exactly the "v":1 envelope key
// (bus.PendingStationsV), the schema Unit 1.1/1.2 defined in
// internal/bus/scan.go.
func TestPendingStations_DocShapeStampsV1(t *testing.T) {
	rec := &recordingPublish{}
	p := newPendingStations(nil, rec.fn, nil)
	t0 := time.Now()

	p.upsert("cs-1", "AcmeEV", "Fast50", "10.0.0.5:5000", t0)

	b := marshalPendingDoc(t, rec.last())
	if strings.Count(b, `"v":`) != 1 {
		t.Fatalf("doc JSON has %d \"v\" keys, want exactly 1: %s", strings.Count(b, `"v":`), b)
	}
	if !strings.Contains(b, `"v":1`) {
		t.Errorf("doc JSON missing \"v\":1, got %s", b)
	}
}

func marshalPendingDoc(t *testing.T, doc bus.PendingStations) string {
	t.Helper()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestPendingStations_PublishErrorDoesNotPanic: doPublish's log-and-continue
// error handling must not panic, and a later successful publish still goes
// through normally (the rate limiter tracks lastPublish regardless of the
// publish func's own success/failure — matching mqttutil's "late/dropped is
// harmless because it's re-issued" convention elsewhere in this codebase).
func TestPendingStations_PublishErrorDoesNotPanic(t *testing.T) {
	rec := &recordingPublish{failN: 1}
	p := newPendingStations(nil, rec.fn, nil)
	t0 := time.Now()

	p.upsert("cs-1", "", "", "", t0) // publish attempt fails (failN consumed)
	if rec.count() != 0 {
		t.Fatalf("failed publish recorded a doc anyway, count=%d", rec.count())
	}

	p.upsert("cs-2", "", "", "", t0.Add(2*time.Second)) // past rate limit: succeeds
	if rec.count() != 1 {
		t.Fatalf("subsequent publish did not go through, count=%d", rec.count())
	}
}

// TestPendingStations_GaugeTracksCount pins that the metrics gauge (when
// present) reflects the pruned entry count computed by buildDocLocked.
func TestPendingStations_GaugeTracksCount(t *testing.T) {
	rec := &recordingPublish{}
	reg := metrics.New()
	gauge := reg.Gauge("lexa_ocpp_pending_stations")
	p := newPendingStations(nil, rec.fn, gauge)
	t0 := time.Now()

	p.upsert("cs-1", "", "", "", t0)
	p.upsert("cs-2", "", "", "", t0.Add(2*time.Second))

	if got := reg.Format(); !strings.Contains(got, "lexa_ocpp_pending_stations 2") {
		t.Errorf("gauge did not reach 2, got:\n%s", got)
	}
}
