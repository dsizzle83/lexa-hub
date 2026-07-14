package derreport

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
	"lexa-hub/internal/northbound/egress"
	"lexa-hub/internal/utilitytime"
)

// fakePutter records PUTs and returns scripted results per path — the
// discovery.Fetcher-style seam test (WP-4 spec: fake fetcher, no TLS).
type fakePutter struct {
	puts []fakePut
	// errByPath returns the error for the NEXT put on a path each time it is
	// consulted; nil entry (or absent) means success.
	errByPath map[string][]error
}

type fakePut struct {
	path        string
	body        string
	contentType string
}

func (f *fakePutter) Put(path string, body []byte, contentType string) ([]byte, error) {
	f.puts = append(f.puts, fakePut{path: path, body: string(body), contentType: contentType})
	if q := f.errByPath[path]; len(q) > 0 {
		err := q[0]
		f.errByPath[path] = q[1:]
		return nil, err
	}
	return nil, nil
}

func (f *fakePutter) pathsPut() []string {
	var out []string
	for _, p := range f.puts {
		out = append(out, p.path)
	}
	return out
}

func statusErr(path string, code int) error {
	// tlsclient putResult's pinned format ("PUT %s: status %d").
	return fmt.Errorf("PUT %s: status %d", path, code)
}

func newTestManager(f *fakePutter) *Manager {
	reg := metrics.New()
	return New(f, utilitytime.New(utilitytime.Config{}),
		reg.Counter("lexa_nb_derreport_puts_total"),
		reg.Counter("lexa_nb_derreport_errors_total"))
}

func testReport(hash string) bus.DERSiteReport {
	soc := 57.5
	storage := uint8(2)
	wAvail := 8000.0
	dur := uint32(3600)
	return bus.DERSiteReport{
		Envelope:             bus.Envelope{V: bus.DERSiteReportV},
		DERType:              bus.DERTypeStorage,
		ModesSupported:       0x01800007,
		RtgMaxW:              18000,
		RtgMaxChargeRateW:    7500,
		RtgMaxDischargeRateW: 8000,
		RtgMaxWh:             16000,
		SetMaxW:              18000,
		SetMaxChargeRateW:    7500,
		SetMaxDischargeRateW: 8000,
		SetMaxWh:             16000,
		Status: bus.DERSiteStatus{
			SocPct:           &soc,
			GenConnectStatus: 1,
			OperationalMode:  1,
			StorageMode:      &storage,
			AlarmBits:        bus.DERAlarmOverFrequency,
			ReadingTs:        1752480000,
		},
		Avail: &bus.DERSiteAvailability{
			EstimatedWAvailW:      &wAvail,
			AvailabilityDurationS: &dur,
		},
		ContentHash: hash,
		Ts:          1752480001,
	}
}

const (
	capHref   = "/edev/1/der/1/dercap"
	setHref   = "/edev/1/der/1/derg"
	statHref  = "/edev/1/der/1/ders"
	availHref = "/edev/1/der/1/dera"
)

// TestStartupCadence pins the startup sequence (G29+G30): a retained dersite
// doc arrives before the first walk (no hrefs — nothing PUT), then the walk
// delivers hrefs and ONE OnWalk PUTs status+availability AND
// capability+settings.
func TestStartupCadence(t *testing.T) {
	f := &fakePutter{}
	m := newTestManager(f)

	m.HandleDERSite(bus.TopicHubDERSite, testReport("hash-1"))
	if len(f.puts) != 0 {
		t.Fatalf("PUT before any walk discovered hrefs: %v", f.pathsPut())
	}

	m.OnWalk(capHref, setHref, statHref, availHref)
	want := map[string]bool{capHref: true, setHref: true, statHref: true, availHref: true}
	if len(f.puts) != 4 {
		t.Fatalf("startup walk PUTs = %v, want all four resources", f.pathsPut())
	}
	for _, p := range f.puts {
		if !want[p.path] {
			t.Errorf("unexpected PUT path %s", p.path)
		}
		if p.contentType != contentTypeSepXML {
			t.Errorf("content type %s, want %s", p.contentType, contentTypeSepXML)
		}
	}
}

// TestPollRateCadence pins the per-walk (pollRate-paced) cadence: every walk
// re-PUTs status+availability; capability+settings only when the content
// hash changed since their last clean PUT.
func TestPollRateCadence(t *testing.T) {
	f := &fakePutter{}
	m := newTestManager(f)
	m.HandleDERSite(bus.TopicHubDERSite, testReport("hash-1"))

	m.OnWalk(capHref, setHref, statHref, availHref) // startup: 4 PUTs
	f.puts = nil

	m.OnWalk(capHref, setHref, statHref, availHref) // unchanged content
	if got := f.pathsPut(); len(got) != 2 || got[0] != statHref || got[1] != availHref {
		t.Fatalf("second walk PUTs = %v, want status+availability only", got)
	}

	// On-change (G29): a new ContentHash re-PUTs capability+settings from
	// the MQTT path immediately, without waiting for a walk.
	f.puts = nil
	m.HandleDERSite(bus.TopicHubDERSite, testReport("hash-2"))
	if got := f.pathsPut(); len(got) != 2 || got[0] != capHref || got[1] != setHref {
		t.Fatalf("on-change PUTs = %v, want capability+settings only", got)
	}

	// A heartbeat republish (same hash) triggers nothing.
	f.puts = nil
	m.HandleDERSite(bus.TopicHubDERSite, testReport("hash-2"))
	if len(f.puts) != 0 {
		t.Fatalf("unchanged-hash redelivery PUT something: %v", f.pathsPut())
	}
}

// TestPutBodiesXMLNSAndValues pins the PUT bodies: xmlns-carrying roots
// (csipmodel XMLName round-trips, WP-1) and the key value conversions
// (ActivePower scaling, SoC percent×100, modes/type passthrough).
func TestPutBodiesXMLNSAndValues(t *testing.T) {
	f := &fakePutter{}
	m := newTestManager(f)
	m.HandleDERSite(bus.TopicHubDERSite, testReport("hash-1"))
	m.OnWalk(capHref, setHref, statHref, availHref)

	byPath := map[string]string{}
	for _, p := range f.puts {
		byPath[p.path] = p.body
	}

	capBody := byPath[capHref]
	if !strings.Contains(capBody, `<DERCapability xmlns="urn:ieee:std:2030.5:ns">`) {
		t.Errorf("DERCapability missing xmlns root: %s", capBody)
	}
	if !strings.Contains(capBody, "<type>83</type>") {
		t.Errorf("DERCapability type not carried: %s", capBody)
	}
	if !strings.Contains(capBody, fmt.Sprintf("<modesSupported>%d</modesSupported>", 0x01800007)) {
		t.Errorf("DERCapability modesSupported not carried: %s", capBody)
	}
	// 18000 W fits int16 → multiplier 0, value verbatim.
	if !strings.Contains(capBody, "<rtgMaxW><multiplier>0</multiplier><value>18000</value></rtgMaxW>") {
		t.Errorf("rtgMaxW ActivePower encoding wrong: %s", capBody)
	}
	// No VA/Var data on the report → no fabricated elements (G27).
	if strings.Contains(capBody, "rtgMaxVA") || strings.Contains(capBody, "rtgMaxVar") {
		t.Errorf("fabricated VA/Var rating in body: %s", capBody)
	}

	setBody := byPath[setHref]
	if !strings.Contains(setBody, `<DERSettings xmlns="urn:ieee:std:2030.5:ns">`) {
		t.Errorf("DERSettings missing xmlns root: %s", setBody)
	}
	if !strings.Contains(setBody, "<setMaxW><multiplier>0</multiplier><value>18000</value></setMaxW>") {
		t.Errorf("setMaxW not carried: %s", setBody)
	}

	statBody := byPath[statHref]
	if !strings.Contains(statBody, `<DERStatus xmlns="urn:ieee:std:2030.5:ns">`) {
		t.Errorf("DERStatus missing xmlns root: %s", statBody)
	}
	// SoC 57.5% → 5750 (percent × 100).
	if !strings.Contains(statBody, "<value>5750</value>") {
		t.Errorf("stateOfChargeStatus not percent×100: %s", statBody)
	}
	if !strings.Contains(statBody, "genConnectStatus") || !strings.Contains(statBody, "storageModeStatus") {
		t.Errorf("status blocks missing: %s", statBody)
	}

	availBody := byPath[availHref]
	if !strings.Contains(availBody, `<DERAvailability xmlns="urn:ieee:std:2030.5:ns">`) {
		t.Errorf("DERAvailability missing xmlns root: %s", availBody)
	}
	if !strings.Contains(availBody, "<estimatedWAvail><multiplier>0</multiplier><value>8000</value></estimatedWAvail>") {
		t.Errorf("estimatedWAvail not carried: %s", availBody)
	}
	if !strings.Contains(availBody, "<availabilityDuration>3600</availabilityDuration>") {
		t.Errorf("availabilityDuration not carried: %s", availBody)
	}
}

// TestNotOfferedSkip pins the 404/405-tolerated-once-then-skip rule: the
// first 404 on a resource logs+counts and latches skip; later cadences never
// re-PUT it; an href CHANGE clears the latch.
func TestNotOfferedSkip(t *testing.T) {
	f := &fakePutter{errByPath: map[string][]error{
		availHref: {statusErr(availHref, 404)},
		setHref:   {statusErr(setHref, 405)},
	}}
	m := newTestManager(f)
	m.HandleDERSite(bus.TopicHubDERSite, testReport("hash-1"))
	m.OnWalk(capHref, setHref, statHref, availHref)

	f.puts = nil
	m.OnWalk(capHref, setHref, statHref, availHref)
	for _, p := range f.puts {
		if p.path == availHref {
			t.Fatal("404'd availability re-PUT after skip latch")
		}
	}
	// Settings 405'd — but capability succeeded and settings is now settled
	// (server refuses it), so the hash recorded and no cap/settings re-PUT
	// happens on an unchanged walk.
	for _, p := range f.puts {
		if p.path == setHref || p.path == capHref {
			t.Fatalf("skipped/settled resource re-PUT: %v", f.pathsPut())
		}
	}

	// A changed href resets the latch: the resource gets a fresh attempt.
	f.puts = nil
	m.OnWalk(capHref, setHref, statHref, "/edev/1/der/1/dera-v2")
	found := false
	for _, p := range f.puts {
		if p.path == "/edev/1/der/1/dera-v2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("changed availability href not retried: %v", f.pathsPut())
	}
}

// TestTransientErrorRetriesOnceThenNextCadence pins the retry semantics: a
// transport-level failure retries exactly once within the same attempt; a
// still-failing resource does NOT latch skip and is retried at the next
// cadence; capability/settings whose PUT never succeeded keep the content
// hash unrecorded so the next walk retries them.
func TestTransientErrorRetriesOnceThenNextCadence(t *testing.T) {
	transient := errors.New("write: broken pipe")
	f := &fakePutter{errByPath: map[string][]error{
		capHref: {transient, transient}, // first attempt + its one retry both fail
	}}
	m := newTestManager(f)
	m.HandleDERSite(bus.TopicHubDERSite, testReport("hash-1"))
	m.OnWalk(capHref, setHref, statHref, availHref)

	var capPuts int
	for _, p := range f.puts {
		if p.path == capHref {
			capPuts++
		}
	}
	if capPuts != 2 {
		t.Fatalf("transient failure PUT %d times, want 2 (one retry)", capPuts)
	}

	// Next walk: capability retried (hash never recorded), and succeeds now.
	f.puts = nil
	m.OnWalk(capHref, setHref, statHref, availHref)
	capPuts = 0
	for _, p := range f.puts {
		if p.path == capHref {
			capPuts++
		}
	}
	if capPuts != 1 {
		t.Fatalf("failed capability not retried next walk: %v", f.pathsPut())
	}

	// Now settled: a further unchanged walk leaves cap/settings alone.
	f.puts = nil
	m.OnWalk(capHref, setHref, statHref, availHref)
	for _, p := range f.puts {
		if p.path == capHref || p.path == setHref {
			t.Fatalf("settled cap/settings re-PUT on unchanged walk: %v", f.pathsPut())
		}
	}
}

// TestStatusErrorDoesNotRetryImmediately pins that an HTTP-status failure
// (the server answered) is not retried within the same attempt — only
// transport errors get the in-attempt retry.
func TestStatusErrorDoesNotRetryImmediately(t *testing.T) {
	f := &fakePutter{errByPath: map[string][]error{
		statHref: {statusErr(statHref, 500)},
	}}
	m := newTestManager(f)
	m.HandleDERSite(bus.TopicHubDERSite, testReport("hash-1"))
	m.OnWalk(capHref, setHref, statHref, availHref)

	var statPuts int
	for _, p := range f.puts {
		if p.path == statHref {
			statPuts++
		}
	}
	if statPuts != 1 {
		t.Fatalf("status-500 PUT %d times, want 1 (no in-attempt retry)", statPuts)
	}

	// A 500 is transient server-side: NOT latched as skip — next walk retries.
	f.puts = nil
	m.OnWalk(capHref, setHref, statHref, availHref)
	statPuts = 0
	for _, p := range f.puts {
		if p.path == statHref {
			statPuts++
		}
	}
	if statPuts != 1 {
		t.Fatalf("status-500 resource not retried next walk: %v", f.pathsPut())
	}
}

// TestEgressGateSuspension pins the WP-7/D4 hook: a suspended gate blocks
// every PUT from both the walk path and the MQTT on-change path, and
// resuming restores them (with the un-PUT content still pending — the hash
// was never recorded).
func TestEgressGateSuspension(t *testing.T) {
	f := &fakePutter{}
	m := newTestManager(f)
	gate := &egress.Gate{}
	m.SetEgressGate(gate)
	gate.Suspend("pin mismatch")

	m.HandleDERSite(bus.TopicHubDERSite, testReport("hash-1"))
	m.OnWalk(capHref, setHref, statHref, availHref)
	if len(f.puts) != 0 {
		t.Fatalf("PUTs escaped a suspended gate: %v", f.pathsPut())
	}

	gate.Resume()
	m.OnWalk(capHref, setHref, statHref, availHref)
	if len(f.puts) != 4 {
		t.Fatalf("resume did not restore PUTs: %v", f.pathsPut())
	}
}

// TestNoReportNoPut pins that a walk before any dersite doc arrives PUTs
// nothing — there is nothing truthful to report yet.
func TestNoReportNoPut(t *testing.T) {
	f := &fakePutter{}
	m := newTestManager(f)
	m.OnWalk(capHref, setHref, statHref, availHref)
	if len(f.puts) != 0 {
		t.Fatalf("PUT with no report: %v", f.pathsPut())
	}
}

// TestMissingAvailBlockSkipsAvailabilityPut pins G27 pass-through: a report
// with no availability block PUTs no DERAvailability at all.
func TestMissingAvailBlockSkipsAvailabilityPut(t *testing.T) {
	f := &fakePutter{}
	m := newTestManager(f)
	rep := testReport("hash-1")
	rep.Avail = nil
	m.HandleDERSite(bus.TopicHubDERSite, rep)
	m.OnWalk(capHref, setHref, statHref, availHref)
	for _, p := range f.puts {
		if p.path == availHref {
			t.Fatalf("DERAvailability fabricated from a nil avail block: %v", f.pathsPut())
		}
	}
}

// TestAPWattsScaling pins the ActivePower encoding: values past int16 scale
// the multiplier up instead of overflowing (the wattsToActivePower contract
// this helper deliberately copies from cmd/hub/state.go).
func TestAPWattsScaling(t *testing.T) {
	cases := []struct {
		w        float64
		wantVal  int16
		wantMult int8
	}{
		{18000, 18000, 0},
		{40000, 4000, 1},    // > int16 max: one decade up
		{1250000, 12500, 2}, // 1.25 MW: two decades up
		{0, 0, 0},
	}
	for _, tc := range cases {
		ap := apWatts(tc.w)
		if ap.Value != tc.wantVal || ap.Multiplier != tc.wantMult {
			t.Errorf("apWatts(%v) = {%d,%d}, want {%d,%d}",
				tc.w, ap.Value, ap.Multiplier, tc.wantVal, tc.wantMult)
		}
	}
}

// TestPutStatusCode pins the error-format parse against tlsclient
// putResult's pinned shape, and the transport-error fallthrough.
func TestPutStatusCode(t *testing.T) {
	if got := putStatusCode(statusErr("/x", 404)); got != 404 {
		t.Errorf("putStatusCode(404 err) = %d", got)
	}
	if got := putStatusCode(statusErr("/x", 405)); got != 405 {
		t.Errorf("putStatusCode(405 err) = %d", got)
	}
	if got := putStatusCode(errors.New("write: broken pipe")); got != 0 {
		t.Errorf("putStatusCode(transport err) = %d, want 0", got)
	}
	if got := putStatusCode(nil); got != 0 {
		t.Errorf("putStatusCode(nil) = %d, want 0", got)
	}
}
