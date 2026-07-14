package run

import (
	"testing"

	"lexa-hub/internal/northbound/discovery"
	model "lexa-proto/csipmodel"
)

// fakeDERReportSink captures OnWalk calls (run.DERReportSink).
type fakeDERReportSink struct {
	calls [][4]string
}

func (f *fakeDERReportSink) OnWalk(capHref, setHref, statHref, availHref string) {
	f.calls = append(f.calls, [4]string{capHref, setHref, statHref, availHref})
}

// TestDERHrefsFromTree pins WP-4's href extraction: the GFEMS single DER
// entry's sub-resource links are reused from the walker's observations
// (never hardcoded), missing links yield "", and an absent/empty DERList
// yields all-empty without panicking.
func TestDERHrefsFromTree(t *testing.T) {
	link := func(h string) *model.Link { return &model.Link{Href: h} }

	full := &discovery.ResourceTree{DERList: &model.DERList{DER: []model.DER{{
		DERCapabilityLink:   link("/edev/1/der/1/dercap"),
		DERSettingsLink:     link("/edev/1/der/1/derg"),
		DERStatusLink:       link("/edev/1/der/1/ders"),
		DERAvailabilityLink: link("/edev/1/der/1/dera"),
	}}}}
	c, s, st, av := derHrefsFromTree(full)
	if c != "/edev/1/der/1/dercap" || s != "/edev/1/der/1/derg" ||
		st != "/edev/1/der/1/ders" || av != "/edev/1/der/1/dera" {
		t.Fatalf("full tree hrefs = %q %q %q %q", c, s, st, av)
	}

	partial := &discovery.ResourceTree{DERList: &model.DERList{DER: []model.DER{{
		DERStatusLink: link("/edev/1/der/1/ders"),
	}}}}
	c, s, st, av = derHrefsFromTree(partial)
	if c != "" || s != "" || av != "" || st != "/edev/1/der/1/ders" {
		t.Fatalf("partial tree hrefs = %q %q %q %q", c, s, st, av)
	}

	// GFEMS profile: the FIRST DER entry is authoritative (UTIL-002's
	// per-DER mechanics are out of scope).
	multi := &discovery.ResourceTree{DERList: &model.DERList{DER: []model.DER{
		{DERStatusLink: link("/der/first")},
		{DERStatusLink: link("/der/second")},
	}}}
	if _, _, st, _ := derHrefsFromTree(multi); st != "/der/first" {
		t.Fatalf("multi-DER tree picked %q, want the first entry", st)
	}

	for name, tree := range map[string]*discovery.ResourceTree{
		"nil tree":      nil,
		"no DERList":    {},
		"empty DERList": {DERList: &model.DERList{}},
	} {
		c, s, st, av := derHrefsFromTree(tree)
		if c != "" || s != "" || st != "" || av != "" {
			t.Errorf("%s: hrefs = %q %q %q %q, want all empty", name, c, s, st, av)
		}
	}
}

// TestSetDERReporterWiring pins the additive-setter shape (mirrors
// SetLogEventSink): a wired sink is stored on the Discovery; nil stays a
// safe no-op default (RunOnce's nil guard — same contract as logEvents).
func TestSetDERReporterWiring(t *testing.T) {
	d := New(&fakeNBClient{}, nil, "LFDI", nil, nil, nil, nil, Metrics{}, PollRateConfig{})
	if d.derReport != nil {
		t.Fatal("derReport non-nil before wiring")
	}
	sink := &fakeDERReportSink{}
	d.SetDERReporter(sink)
	if d.derReport != DERReportSink(sink) {
		t.Fatal("SetDERReporter did not store the sink")
	}

	// The per-walk trigger shape RunOnce uses: extraction + OnWalk.
	tree := &discovery.ResourceTree{DERList: &model.DERList{DER: []model.DER{{
		DERCapabilityLink: &model.Link{Href: "/edev/1/der/1/dercap"},
	}}}}
	capHref, setHref, statHref, availHref := derHrefsFromTree(tree)
	d.derReport.OnWalk(capHref, setHref, statHref, availHref)
	if len(sink.calls) != 1 || sink.calls[0] != [4]string{"/edev/1/der/1/dercap", "", "", ""} {
		t.Fatalf("OnWalk calls = %v", sink.calls)
	}
}
