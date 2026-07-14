package logevent

import (
	"encoding/xml"
	"errors"
	"strings"
	"testing"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/utilitytime"
	model "lexa-proto/csipmodel"
)

// fakePoster records every LogEvent POST, decoding the XML body back so
// tests can assert exact wire content, and pops one scripted error per call
// (nil = success with a 201-Created-style Location header) — the same
// fake-Poster shape responses/tracker_test.go uses.
type fakePoster struct {
	paths  []string
	events []model.LogEvent
	errs   []error // popped per call; empty/exhausted = success
}

func (f *fakePoster) Post(path string, body []byte, contentType string) ([]byte, string, error) {
	f.paths = append(f.paths, path)
	var ev model.LogEvent
	_ = xml.Unmarshal(body, &ev)
	f.events = append(f.events, ev)
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return nil, "", err
		}
	}
	return nil, "/edev/0/log/1", nil
}

func msg(key string) bus.LogEventMsg {
	return bus.LogEventMsg{
		Envelope:     bus.Envelope{V: bus.LogEventV},
		Device:       "inv0",
		FunctionSet:  bus.LogEventFunctionSetDER,
		LogEventCode: bus.LogEventDEROverVoltage,
		Alarm:        true,
		LogEventID:   3,
		CreatedTs:    1750000000,
		DedupeKey:    key,
	}
}

// harness builds a Manager over fp with real counters, returning the
// registry so tests assert counts via its text exposition (metrics.Counter
// has no exported read — Format is the scrape surface anyway).
func harness(fp *fakePoster) (*Manager, *utilitytime.Clock, *metrics.Registry) {
	reg := metrics.New()
	clk := utilitytime.New(utilitytime.Config{})
	m := New(fp, clk,
		reg.Counter("lexa_nb_logevents_posted_total"),
		reg.Counter("lexa_nb_logevents_dropped_total"))
	return m, clk, reg
}

func wantMetric(t *testing.T, reg *metrics.Registry, line string) {
	t.Helper()
	if !strings.Contains(reg.Format(), line) {
		t.Fatalf("metrics missing %q:\n%s", line, reg.Format())
	}
}

// TestPost_Success: happy path — one POST to the discovered
// LogEventListLink, correct wire fields (functionSet 11, CSIP profile,
// standard PEN, code, id), createdDateTime shifted to server time by the
// walk's clock offset, posted counter incremented.
func TestPost_Success(t *testing.T) {
	fp := &fakePoster{}
	m, clk, reg := harness(fp)
	clk.SetOffset(120) // server 2 min ahead of local
	m.SetPath("/edev/0/log")

	m.HandleLogEvent(bus.TopicHubLogEvent, msg("k1"))

	if len(fp.events) != 1 || fp.paths[0] != "/edev/0/log" {
		t.Fatalf("POST calls = %v", fp.paths)
	}
	ev := fp.events[0]
	if ev.FunctionSet != 11 || ev.LogEventCode != bus.LogEventDEROverVoltage ||
		ev.LogEventID != 3 || ev.ProfileID != profileIDCSIP || ev.LogEventPEN != logEventPENStandard {
		t.Fatalf("posted LogEvent = %+v", ev)
	}
	if ev.CreatedDateTime != 1750000000+120 {
		t.Fatalf("createdDateTime = %d, want local+offset %d", ev.CreatedDateTime, 1750000000+120)
	}
	wantMetric(t, reg, "lexa_nb_logevents_posted_total 1")
	wantMetric(t, reg, "lexa_nb_logevents_dropped_total 0")
}

// TestPost_NoOffsetUsesLocalTime: before the first successful walk, the
// clock has no offset — the local timestamp posts as-is.
func TestPost_NoOffsetUsesLocalTime(t *testing.T) {
	fp := &fakePoster{}
	m, _, _ := harness(fp)
	m.SetPath("/edev/0/log")
	m.HandleLogEvent(bus.TopicHubLogEvent, msg("k1"))
	if fp.events[0].CreatedDateTime != 1750000000 {
		t.Fatalf("createdDateTime = %d, want local ts", fp.events[0].CreatedDateTime)
	}
}

// TestPost_RetryOnceThenSuccess: a first-attempt error (stale keepalive
// session, transient) retries exactly once; the second attempt's success
// counts as posted, not dropped.
func TestPost_RetryOnceThenSuccess(t *testing.T) {
	fp := &fakePoster{errs: []error{errors.New("session reset")}}
	m, _, reg := harness(fp)
	m.SetPath("/edev/0/log")

	m.HandleLogEvent(bus.TopicHubLogEvent, msg("k1"))

	if len(fp.paths) != 2 {
		t.Fatalf("POST attempts = %d, want 2 (retry once)", len(fp.paths))
	}
	wantMetric(t, reg, "lexa_nb_logevents_posted_total 1")
	wantMetric(t, reg, "lexa_nb_logevents_dropped_total 0")
}

// TestPost_RetryOnceThenDrop: two failures = drop-and-count, never a third
// attempt (crash-only, no spool). A LATER QoS 1 redelivery of the same key
// genuinely retries — only a CONFIRMED post is recorded as seen.
func TestPost_RetryOnceThenDrop(t *testing.T) {
	fp := &fakePoster{errs: []error{errors.New("down"), errors.New("down")}}
	m, _, reg := harness(fp)
	m.SetPath("/edev/0/log")

	m.HandleLogEvent(bus.TopicHubLogEvent, msg("k1"))
	if len(fp.paths) != 2 {
		t.Fatalf("POST attempts = %d, want exactly 2", len(fp.paths))
	}
	wantMetric(t, reg, "lexa_nb_logevents_posted_total 0")
	wantMetric(t, reg, "lexa_nb_logevents_dropped_total 1")

	// Redelivery after the drop: the key was never confirmed, so it retries
	// (and now succeeds — errs exhausted).
	m.HandleLogEvent(bus.TopicHubLogEvent, msg("k1"))
	if len(fp.paths) != 3 {
		t.Fatalf("POST attempts after redelivery = %d, want 3", len(fp.paths))
	}
	wantMetric(t, reg, "lexa_nb_logevents_posted_total 1")
}

// TestPost_DedupeOnKey: QoS 1 at-least-once redelivery of a CONFIRMED post
// is idempotent.
func TestPost_DedupeOnKey(t *testing.T) {
	fp := &fakePoster{}
	m, _, reg := harness(fp)
	m.SetPath("/edev/0/log")

	m.HandleLogEvent(bus.TopicHubLogEvent, msg("k1"))
	m.HandleLogEvent(bus.TopicHubLogEvent, msg("k1"))
	if len(fp.paths) != 1 {
		t.Fatalf("POST attempts = %d, want 1 (dedupe)", len(fp.paths))
	}
	wantMetric(t, reg, "lexa_nb_logevents_posted_total 1")

	// A DIFFERENT key still posts.
	m.HandleLogEvent(bus.TopicHubLogEvent, msg("k2"))
	if len(fp.paths) != 2 {
		t.Fatalf("POST attempts = %d, want 2", len(fp.paths))
	}
}

// TestPost_NoPathDrops: before any walk has discovered a LogEventListLink
// (or when the server offers none), events drop-and-count — never spool.
func TestPost_NoPathDrops(t *testing.T) {
	fp := &fakePoster{}
	m, _, reg := harness(fp)

	m.HandleLogEvent(bus.TopicHubLogEvent, msg("k1"))
	if len(fp.paths) != 0 {
		t.Fatalf("POSTed with no path: %v", fp.paths)
	}
	wantMetric(t, reg, "lexa_nb_logevents_dropped_total 1")
}

// TestPost_EgressSuspended: the WP-7 D4 seam — while suspended, events drop
// (counted), and un-suspending restores posting.
func TestPost_EgressSuspended(t *testing.T) {
	fp := &fakePoster{}
	m, _, reg := harness(fp)
	m.SetPath("/edev/0/log")

	m.SetEgressSuspended(true)
	m.HandleLogEvent(bus.TopicHubLogEvent, msg("k1"))
	if len(fp.paths) != 0 {
		t.Fatalf("POSTed while suspended: %v", fp.paths)
	}
	wantMetric(t, reg, "lexa_nb_logevents_dropped_total 1")

	m.SetEgressSuspended(false)
	m.HandleLogEvent(bus.TopicHubLogEvent, msg("k2"))
	if len(fp.paths) != 1 {
		t.Fatalf("POST after un-suspend = %v, want 1 call", fp.paths)
	}
}

// TestPost_RejectsOutsideTable14: a message outside the Table 14 vocabulary
// (wrong function set, or a code past 21) is dropped without a POST — the
// standard's code space is never extended from here.
func TestPost_RejectsOutsideTable14(t *testing.T) {
	fp := &fakePoster{}
	m, _, reg := harness(fp)
	m.SetPath("/edev/0/log")

	bad := msg("k1")
	bad.FunctionSet = 5
	m.HandleLogEvent(bus.TopicHubLogEvent, bad)

	bad2 := msg("k2")
	bad2.LogEventCode = 22
	m.HandleLogEvent(bus.TopicHubLogEvent, bad2)

	if len(fp.paths) != 0 {
		t.Fatalf("POSTed out-of-vocabulary events: %v", fp.paths)
	}
	wantMetric(t, reg, "lexa_nb_logevents_dropped_total 2")
}
