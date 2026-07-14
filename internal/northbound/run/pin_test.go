package run

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"testing"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/northbound/egress"
	"lexa-hub/internal/northbound/flowres"
	"lexa-hub/internal/northbound/responses"
	"lexa-hub/internal/northbound/scheduler"
	"lexa-hub/internal/utilitytime"
	model "lexa-proto/csipmodel"
)

// ───────────────────────────────────────────────────────────────────────
// WP-7 (D4) fixtures: a mutable map-backed Fetcher serving a minimal but
// complete CSIP tree (this package's compact analogue of the discovery
// package's unexported mockFetcher), and a Poster double counting POSTs.
// ───────────────────────────────────────────────────────────────────────

const pinTestLFDI = "AB12CD34EF56789012345678901234567890ABCD"

type mapFetcher struct {
	responses map[string]interface{}
}

func (m *mapFetcher) serve(path string, r interface{}) { m.responses[path] = r }
func (m *mapFetcher) remove(path string)               { delete(m.responses, path) }

func (m *mapFetcher) Get(_ context.Context, path string) ([]byte, error) {
	r, ok := m.responses[path]
	if !ok {
		return nil, fmt.Errorf("404: no resource at %s", path)
	}
	return xml.Marshal(r)
}

// countingPoster satisfies both responses.Poster and flowres.Poster,
// recording every server POST so tests can assert egress suspension.
type countingPoster struct{ calls int }

func (p *countingPoster) Post(string, []byte, string) ([]byte, string, error) {
	p.calls++
	return nil, "", nil
}

// pinTestTree serves a minimal walkable tree: dcap → tm → edev(self, with
// RegistrationLink → /reg PIN 111115) → fsa → derp with ONE active event
// (eventMRID) whose window comfortably covers serverNow (~/tm CurrentTime).
func pinTestTree(m *mapFetcher, eventMRID string) {
	m.serve("/dcap", &model.DeviceCapability{
		Resource:          model.Resource{Href: "/dcap"},
		TimeLink:          &model.Link{Href: "/tm"},
		EndDeviceListLink: &model.ListLink{Link: model.Link{Href: "/edev"}, All: 1},
	})
	m.serve("/tm", &model.Time{Resource: model.Resource{Href: "/tm"}, CurrentTime: 1700000000})
	m.serve("/edev", &model.EndDeviceList{
		Resource: model.Resource{Href: "/edev"}, All: 1, Results: 1,
		EndDevice: []model.EndDevice{{
			Resource:                       model.Resource{Href: "/edev/0"},
			LFDI:                           pinTestLFDI,
			FunctionSetAssignmentsListLink: &model.ListLink{Link: model.Link{Href: "/fsa"}, All: 1},
			RegistrationLink:               &model.Link{Href: "/reg"},
		}},
	})
	m.serve("/reg", &model.Registration{Resource: model.Resource{Href: "/reg"}, PIN: 111115})
	m.serve("/fsa", &model.FunctionSetAssignmentsList{
		Resource: model.Resource{Href: "/fsa"}, All: 1, Results: 1,
		FunctionSetAssignments: []model.FunctionSetAssignments{{
			Resource:           model.Resource{Href: "/fsa/0"},
			DERProgramListLink: &model.ListLink{Link: model.Link{Href: "/derp"}, All: 1},
		}},
	})
	m.serve("/derp", &model.DERProgramList{
		Resource: model.Resource{Href: "/derp"}, All: 1, Results: 1,
		DERProgram: []model.DERProgram{{
			Resource:           model.Resource{Href: "/derp/0"},
			MRID:               "P1",
			Primacy:            1,
			DERControlListLink: &model.ListLink{Link: model.Link{Href: "/derc"}, All: 1},
		}},
	})
	m.serve("/derc", &model.ExtendedDERControlList{
		Resource: model.Resource{Href: "/derc"}, All: 1, Results: 1,
		DERControl: []model.ExtendedDERControl{{
			Resource:     model.Resource{Href: "/derc/0"},
			MRID:         eventMRID,
			CreationTime: 1699990000,
			// Window [1699990000, 1700090000) — covers serverNow (~/tm's
			// CurrentTime plus a few real seconds of test runtime).
			Interval: model.DateTimeInterval{Start: 1699990000, Duration: 100000},
			DERControlBase: model.ExtendedDERControlBase{
				OpModExpLimW: &model.ActivePower{Multiplier: 0, Value: 5000},
			},
		}},
	})
}

// pinHarness bundles one wired Discovery plus every double the WP-7 tests
// assert against.
type pinHarness struct {
	fc       *fakeNBClient
	fetcher  *mapFetcher
	poster   *countingPoster
	gate     *egress.Gate
	verifier *PinVerifier
	tracker  *responses.Tracker
	disc     *Discovery
}

func newPinHarness(t *testing.T, expectedPIN uint32) *pinHarness {
	t.Helper()
	h := &pinHarness{
		fc:      &fakeNBClient{},
		fetcher: &mapFetcher{responses: map[string]interface{}{}},
		poster:  &countingPoster{},
		gate:    &egress.Gate{},
	}
	clk := utilitytime.New(utilitytime.Config{})
	sched := scheduler.New()
	h.tracker = responses.New(h.poster, pinTestLFDI, "/rsps/0/r", clk, nil, nil, nil, responses.State{})
	h.tracker.SetEgressGate(h.gate)
	frm := flowres.New(h.poster, pinTestLFDI)
	frm.SetEgressGate(h.gate)
	h.disc = New(h.fc, h.fetcher, pinTestLFDI, sched, clk, h.tracker, frm, Metrics{}, PollRateConfig{})
	if expectedPIN != 0 {
		h.verifier = NewPinVerifier(expectedPIN, h.gate, nil, nil)
		h.disc.SetPinVerifier(h.verifier)
	}
	return h
}

// lastControl decodes the most recent retained publish on lexa/csip/control.
func (h *pinHarness) lastControl(t *testing.T) *bus.ActiveControl {
	t.Helper()
	var last *bus.ActiveControl
	for _, p := range h.fc.publishes {
		if p.topic != bus.TopicCSIPControl {
			continue
		}
		var ac bus.ActiveControl
		if err := json.Unmarshal(p.payload, &ac); err != nil {
			t.Fatalf("decode published control: %v", err)
		}
		last = &ac
	}
	return last
}

// TestRunOnce_PinFreeze_HoldsLKGBlocksAdoptionSuspendsEgressSelfHeals is the
// WP-7 failclosed-style acceptance test for the full D4 posture across three
// walks: verified → frozen (mismatch) → healed.
func TestRunOnce_PinFreeze_HoldsLKGBlocksAdoptionSuspendsEgressSelfHeals(t *testing.T) {
	h := newPinHarness(t, 111115)
	pinTestTree(h.fetcher, "E1")
	ctx := context.Background()

	// Walk 1 — PIN matches: normal adoption.
	h.disc.RunOnce(ctx)
	if got := h.lastControl(t); got == nil || got.Source != "event" || got.MRID != "E1" {
		t.Fatalf("walk 1 published control = %+v, want event E1", got)
	}
	if ok := h.verifier.PinOK(); ok == nil || !*ok {
		t.Fatalf("PinOK after matching walk = %v, want true", ok)
	}
	if h.gate.Suspended() {
		t.Fatal("gate suspended after a matching walk")
	}
	postsAfterWalk1 := h.poster.calls
	if postsAfterWalk1 == 0 {
		t.Fatal("walk 1 posted no Responses; expected Received+Started for E1")
	}

	// Walk 2 — server PIN changes (mismatch) AND the server serves a NEW
	// event E2: the freeze must hold E1 (LKG), adopt nothing new, and post
	// nothing.
	h.fetcher.serve("/reg", &model.Registration{Resource: model.Resource{Href: "/reg"}, PIN: 999999})
	pinTestTree2 := func() { // swap only the control list
		h.fetcher.serve("/derc", &model.ExtendedDERControlList{
			Resource: model.Resource{Href: "/derc"}, All: 1, Results: 1,
			DERControl: []model.ExtendedDERControl{{
				Resource:     model.Resource{Href: "/derc/0"},
				MRID:         "E2",
				CreationTime: 1699995000,
				Interval:     model.DateTimeInterval{Start: 1699990000, Duration: 100000},
				DERControlBase: model.ExtendedDERControlBase{
					OpModExpLimW: &model.ActivePower{Multiplier: 0, Value: 0},
				},
			}},
		})
	}
	pinTestTree2()
	h.disc.RunOnce(ctx)

	got := h.lastControl(t)
	if got == nil || got.MRID != "E1" || got.Source != "event" {
		t.Fatalf("frozen walk published control = %+v, want HELD E1 (no new adoption)", got)
	}
	if ok := h.verifier.PinOK(); ok == nil || *ok {
		t.Fatalf("PinOK during freeze = %v, want false", ok)
	}
	if !h.gate.Suspended() {
		t.Fatal("egress gate not suspended during PIN freeze")
	}
	if h.poster.calls != postsAfterWalk1 {
		t.Fatalf("frozen walk posted %d new Responses, want 0 (tracker.Update must be skipped)",
			h.poster.calls-postsAfterWalk1)
	}

	// Async CannotComply during the freeze: the tracker's gate backstop must
	// suppress the POST (mrid deliberately ≠ E1 so E1's own end-of-event
	// code stays clean for the heal assertion below).
	h.tracker.AlertCannotComply("OTHER", "OTHER@1#1")
	if h.poster.calls != postsAfterWalk1 {
		t.Fatalf("CannotComply POSTed during egress freeze (%d new posts), want suppressed",
			h.poster.calls-postsAfterWalk1)
	}

	// Walk 3 — server fixes the PIN: self-heal. E2 adopted, egress resumed
	// (E2's Received/Started post), gate open, pin_ok true.
	h.fetcher.serve("/reg", &model.Registration{Resource: model.Resource{Href: "/reg"}, PIN: 111115})
	h.disc.RunOnce(ctx)

	if got := h.lastControl(t); got == nil || got.MRID != "E2" || got.Source != "event" {
		t.Fatalf("healed walk published control = %+v, want freshly-adopted E2", got)
	}
	if ok := h.verifier.PinOK(); ok == nil || !*ok {
		t.Fatalf("PinOK after heal = %v, want true", ok)
	}
	if h.gate.Suspended() {
		t.Fatal("egress gate still suspended after heal")
	}
	if h.poster.calls <= postsAfterWalk1 {
		t.Fatal("no Responses posted after heal; expected E2 lifecycle posts (egress resumed)")
	}
}

// TestRunOnce_PinFreeze_RegistrationFetchFailureFreezes covers the
// fetch-failure-when-required half of D4: a server that stops serving the
// Registration resource (404) while registration_pin is configured freezes
// exactly like a mismatch — never fail-open.
func TestRunOnce_PinFreeze_RegistrationFetchFailureFreezes(t *testing.T) {
	h := newPinHarness(t, 111115)
	pinTestTree(h.fetcher, "E1")
	ctx := context.Background()

	h.disc.RunOnce(ctx) // adopt E1 normally
	posts := h.poster.calls

	h.fetcher.remove("/reg") // Registration now 404s
	h.disc.RunOnce(ctx)

	if ok := h.verifier.PinOK(); ok == nil || *ok {
		t.Fatalf("PinOK after Registration fetch failure = %v, want false (freeze)", ok)
	}
	if !h.gate.Suspended() {
		t.Fatal("gate not suspended on Registration fetch failure")
	}
	if got := h.lastControl(t); got == nil || got.MRID != "E1" {
		t.Fatalf("published control after fetch-failure freeze = %+v, want held E1", got)
	}
	if h.poster.calls != posts {
		t.Fatal("Responses posted during fetch-failure freeze, want none")
	}
}

// TestRunOnce_NoVerifierIsPreWP7Behavior pins the registration_pin=0 default:
// with no verifier wired, a server whose Registration would mismatch is never
// consulted — adoption and egress run exactly as before WP-7, and PinOK stays
// nil (check disabled).
func TestRunOnce_NoVerifierIsPreWP7Behavior(t *testing.T) {
	h := newPinHarness(t, 0) // no verifier
	pinTestTree(h.fetcher, "E1")
	// A PIN nobody expects: must not matter.
	h.fetcher.serve("/reg", &model.Registration{Resource: model.Resource{Href: "/reg"}, PIN: 424242})

	h.disc.RunOnce(context.Background())

	if got := h.lastControl(t); got == nil || got.MRID != "E1" || got.Source != "event" {
		t.Fatalf("published control = %+v, want event E1 (check disabled)", got)
	}
	if h.gate.Suspended() {
		t.Fatal("gate suspended with no verifier wired")
	}
	if h.poster.calls == 0 {
		t.Fatal("no Responses posted with no verifier wired; expected normal lifecycle posts")
	}
	var nilVerifier *PinVerifier
	if ok := nilVerifier.PinOK(); ok != nil {
		t.Fatalf("nil verifier PinOK() = %v, want nil (check disabled)", ok)
	}
}

// TestPinVerifier_TransitionEdges pins the edge discipline: onChange fires
// on every verdict TRANSITION and never on a repeat verdict, and PinOK
// reports nil until the first verdict.
func TestPinVerifier_TransitionEdges(t *testing.T) {
	changes := 0
	v := NewPinVerifier(111115, nil, nil, func() { changes++ })

	if ok := v.PinOK(); ok != nil {
		t.Fatalf("PinOK before any check = %v, want nil (no verdict yet)", ok)
	}

	v.transition(pinVerified, nil)
	if changes != 1 {
		t.Fatalf("onChange after unchecked→verified = %d, want 1", changes)
	}
	v.transition(pinVerified, nil) // repeat verdict — no edge
	if changes != 1 {
		t.Fatalf("onChange after verified→verified = %d, want 1 (no repeat edge)", changes)
	}
	v.transition(pinFrozen, fmt.Errorf("mismatch"))
	if changes != 2 {
		t.Fatalf("onChange after verified→frozen = %d, want 2", changes)
	}
	v.transition(pinFrozen, fmt.Errorf("mismatch again")) // still frozen — no edge
	if changes != 2 {
		t.Fatalf("onChange after frozen→frozen = %d, want 2 (no repeat edge)", changes)
	}
	v.transition(pinVerified, nil)
	if changes != 3 {
		t.Fatalf("onChange after frozen→verified = %d, want 3", changes)
	}
	if ok := v.PinOK(); ok == nil || !*ok {
		t.Fatalf("PinOK after heal = %v, want true", ok)
	}
}
