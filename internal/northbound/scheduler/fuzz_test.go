package scheduler

// Go-native fuzz targets for the IEEE 2030.5 XML unmarshal surface
// (TASK-048). These live in package scheduler — not a standalone
// internal/northbound/model package, because that package no longer exists:
// TASK-023 merged the 2030.5 data model into the shared lexa-proto/csipmodel
// module (both lexa-hub and csip-tls-test import it as `model
// "lexa-proto/csipmodel"`). scheduler is the consumer that owns the
// downstream plausibility gate (plausibleControl/plausibleLimit,
// maxPlausibleLimitW below) the task asks these targets to exercise, so the
// fuzz functions live next to the gate they drive rather than in the
// (now-gone) model package or in lexa-proto itself (which has no CI to run
// them — see csip-tls-test's sibling copy for the mirrored decode-only
// target and the AD-003(f) note on why it does NOT also duplicate this
// gate-driving logic).
//
// Empirical note on the "namespace-or-zero-value" hazard (CLAUDE.md
// invariant, architecture review §10.3): every current csipmodel root type
// carries an explicit `xml:"urn:ieee:std:2030.5:ns <Name>"` XMLName tag
// (resources.go's own package doc warns never to omit one). Verified against
// go1.26's encoding/xml: unmarshalling a document whose root element's
// namespace does not match that tag returns a NON-NIL error ("expected
// element <X> in name space ... but have ..."), not a silent nil-error
// zero-value struct — encoding/xml's namespace check is enforced at every
// level that carries a namespace-qualified tag, including nested DERControl
// elements inside a DERControlList. The walker's fetchAndParse already
// returns early on any xml.Unmarshal error and never hands a partially- or
// non-decoded dest to the scheduler, so that path is safe today by
// construction. The hazard would resurface only if a *future* csipmodel
// root type were added without the namespace-qualified tag (silently
// accepting any/no namespace, not zero-valuing it) — assertRootMatches below
// is the regression tripwire for exactly that mistake, and
// TestNamespaceStrippedDERControlListIsZeroValueAndNonAdoptable pins today's
// correct (error-returning, zero-value) behavior with a literal
// reflect.DeepEqual assertion per the task's acceptance criteria ("test
// assertion, not prose").
//
// Run locally (nightly CI runs the same three at 15m each; see the `fuzz`
// Makefile target and .github/workflows/ci.yml):
//
//	go test -fuzz=FuzzUnmarshalDeviceCapability -fuzztime=15m ./internal/northbound/scheduler/
//	go test -fuzz=FuzzUnmarshalTime             -fuzztime=15m ./internal/northbound/scheduler/
//	go test -fuzz=FuzzUnmarshalDERControlList   -fuzztime=15m ./internal/northbound/scheduler/

import (
	"bytes"
	"encoding/xml"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	model "lexa-proto/csipmodel"
)

// sharedSeedsDir is the repo-root shared 2030.5 XML corpus, committed
// identically to csip-tls-test in the same TASK-048 session (05 §11
// lockstep). Genuine csipmodel-marshaled documents (built from the same
// field values gridsim's server.go seeds its /derp/0/* resources with) plus
// csip-tls-test's actual gridsim golden DeviceCapability fixture
// (sim/tlsserver/testdata/golden/dcap.xml) — not hand-typed guesses at the
// wire format.
const sharedSeedsDir = "../../../testdata/fuzz/shared-2030_5"

func sharedXMLSeeds(f *testing.F) [][]byte {
	f.Helper()
	entries, err := os.ReadDir(sharedSeedsDir)
	if err != nil {
		return nil
	}
	var out [][]byte
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".xml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(sharedSeedsDir, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, data)
	}
	return out
}

// stripNamespace removes the mandatory 2030.5 xmlns declaration from a
// genuine seed document — the "root element missing xmlns" hazard input.
func stripNamespace(doc []byte) []byte {
	return bytes.Replace(doc, []byte(` xmlns="`+model.XMLNamespace+`"`), nil, 1)
}

// wrongNamespace substitutes an unrelated namespace URI for the mandatory
// 2030.5 one throughout a genuine seed document.
func wrongNamespace(doc []byte) []byte {
	return bytes.Replace(doc, []byte(model.XMLNamespace), []byte("urn:evil:not-2030.5"), -1)
}

// assertRootMatches fails t if a successfully-decoded root element's XMLName
// doesn't carry the mandatory 2030.5 namespace + expected local name. See
// the package doc comment above for why err==nil with a mismatched
// namespace should be unreachable today, and why this assertion is the
// regression tripwire for that hazard resurfacing.
func assertRootMatches(t *testing.T, got xml.Name, wantLocal string) {
	t.Helper()
	if got.Space != model.XMLNamespace || got.Local != wantLocal {
		t.Fatalf("decoded with no error but wrong root name/namespace: got %+v, want space=%q local=%q — "+
			"this is the namespace-or-zero-value hazard (CLAUDE.md invariant) reproducing",
			got, model.XMLNamespace, wantLocal)
	}
}

func FuzzUnmarshalDeviceCapability(f *testing.F) {
	for _, seed := range sharedXMLSeeds(f) {
		f.Add(seed)
		f.Add(stripNamespace(seed))
		f.Add(wrongNamespace(seed))
	}
	f.Add([]byte(`<DeviceCapability href="/dcap"></DeviceCapability>`)) // no xmlns at all
	f.Add([]byte(``))                                                   // empty input
	f.Add([]byte(`not xml at all`))                                     // garbage, not XML

	f.Fuzz(func(t *testing.T, data []byte) {
		var dest model.DeviceCapability
		if err := xml.Unmarshal(data, &dest); err != nil {
			// Matches walker.fetchAndParse: on any error the caller returns
			// immediately and never touches dest. Nothing more to check.
			return
		}
		assertRootMatches(t, dest.XMLName, "DeviceCapability")
	})
}

func FuzzUnmarshalTime(f *testing.F) {
	for _, seed := range sharedXMLSeeds(f) {
		f.Add(seed)
		f.Add(stripNamespace(seed))
		f.Add(wrongNamespace(seed))
	}
	f.Add([]byte(`<Time href="/tm"><currentTime>-99999999999999</currentTime></Time>`)) // no xmlns, huge negative time
	f.Add([]byte(``))

	f.Fuzz(func(t *testing.T, data []byte) {
		var dest model.Time
		if err := xml.Unmarshal(data, &dest); err != nil {
			return
		}
		assertRootMatches(t, dest.XMLName, "Time")

		// Residual-risk note (deliberately NOT asserted — no gate exists to
		// call, per the task's "common mistakes to avoid": do not invent
		// assertions for fields that no plausibility gate covers). Time.
		// CurrentTime is an unbounded int64; walker.go's
		// `tree.ClockOffset = tm.CurrentTime - time.Now().Unix()` applies no
		// bound at all, and that offset feeds every scheduler.Evaluate
		// serverNow computation downstream. A garbage-but-namespace-valid
		// CurrentTime (this fuzz target's high-value target per the task
		// background) decodes cleanly and is adopted ungated. This is
		// exactly the "decoded, implausible, but not rejected by any gate"
		// class the task asks to report rather than fix here — see the
		// PR description / TASK-048 findings list.
	})
}

func FuzzUnmarshalDERControlList(f *testing.F) {
	for _, seed := range sharedXMLSeeds(f) {
		f.Add(seed)
		f.Add(stripNamespace(seed))
		f.Add(wrongNamespace(seed))
	}
	// Hand-mutants beyond namespace games: implausible ActivePower values the
	// wire format can legally carry (int16 value, int8 multiplier) that must
	// never pass plausibleControl (audit: malform-huge-activepower).
	f.Add([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<DERControlList xmlns="` + model.XMLNamespace + `" href="/derp/0/derc" all="1" results="1">
  <DERControl xmlns="` + model.XMLNamespace + `" href="/derp/0/derc/0">
    <mRID>DERC-HUGE</mRID>
    <interval><duration>60</duration><start>1751500000</start></interval>
    <DERControlBase>
      <opModExpLimW><multiplier>9</multiplier><value>32767</value></opModExpLimW>
    </DERControlBase>
  </DERControl>
</DERControlList>`))
	f.Add([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<DERControlList xmlns="` + model.XMLNamespace + `" href="/derp/0/derc" all="1" results="1">
  <DERControl xmlns="` + model.XMLNamespace + `" href="/derp/0/derc/0">
    <mRID>DERC-INF</mRID>
    <interval><duration>60</duration><start>1751500000</start></interval>
    <DERControlBase>
      <opModMaxLimW><multiplier>127</multiplier><value>32767</value></opModMaxLimW>
    </DERControlBase>
  </DERControl>
</DERControlList>`)) // multiplier at int8 max: value*10^127 overflows to +Inf
	f.Add([]byte(`<?xml version="1.0" encoding="UTF-8"?><DERControlList xmlns="` + model.XMLNamespace + `" href="/derp/0/derc" all="0" results="0"></DERControlList>`)) // empty list
	f.Add([]byte(``))

	f.Fuzz(func(t *testing.T, data []byte) {
		var dest model.DERControlList
		if err := xml.Unmarshal(data, &dest); err != nil {
			return
		}
		assertRootMatches(t, dest.XMLName, "DERControlList")

		// Property (acceptance criterion): no decoded control with any
		// gated |limit| > maxPlausibleLimitW ever passes plausibleControl.
		for i := range dest.DERControl {
			c := &dest.DERControl[i]
			ac := &ActiveControl{Base: c.DERControlBase}
			ok := plausibleControl(ac)
			for _, ap := range []*model.ActivePower{
				c.DERControlBase.OpModExpLimW,
				c.DERControlBase.OpModMaxLimW,
				c.DERControlBase.OpModImpLimW,
				c.DERControlBase.OpModFixedW,
			} {
				if ap == nil {
					continue
				}
				w := float64(ap.Value) * math.Pow10(int(ap.Multiplier))
				implausible := math.IsNaN(w) || math.IsInf(w, 0) || math.Abs(w) > maxPlausibleLimitW
				if implausible && ok {
					t.Fatalf("plausibleControl accepted an implausible limit: value=%d multiplier=%d -> %vW (mrid=%s)",
						ap.Value, ap.Multiplier, w, c.MRID)
				}
			}
		}
	})
}

// TestNamespaceStrippedDERControlListIsZeroValueAndNonAdoptable pins the
// acceptance criterion "namespace-stripped seeds provably yield zero-values
// and are provably non-adoptable as controls (test assertion, not prose)"
// against a real seed document, outside the property-fuzzing loop above so
// the specific claim has its own named, always-run test.
func TestNamespaceStrippedDERControlListIsZeroValueAndNonAdoptable(t *testing.T) {
	seed, err := os.ReadFile(filepath.Join(sharedSeedsDir, "dercontrollist.xml"))
	if err != nil {
		t.Fatalf("read shared seed: %v", err)
	}
	stripped := stripNamespace(seed)
	if bytes.Equal(stripped, seed) {
		t.Fatalf("stripNamespace was a no-op — seed fixture or helper changed shape")
	}

	var dest model.DERControlList
	err = xml.Unmarshal(stripped, &dest)
	if err == nil {
		t.Fatalf("expected a decode error for a namespace-stripped root element; got nil " +
			"(namespace enforcement regressed — see package doc comment)")
	}

	var zero model.DERControlList
	if !reflect.DeepEqual(dest, zero) {
		t.Fatalf("namespace-stripped root did not yield a zero-value struct: %+v", dest)
	}

	// Non-adoptable: production code (walker.fetchAndParse) returns on this
	// same error before ever calling Evaluate/plausibleControl. Independently
	// of that: a zero-value DERControlList has no DERControl entries at all,
	// so there is nothing a scheduler could adopt even if it were reached.
	if len(dest.DERControl) != 0 {
		t.Fatalf("zero-value DERControlList unexpectedly carries %d DERControl entries", len(dest.DERControl))
	}
}
