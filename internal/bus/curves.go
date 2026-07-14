package bus

// CurveSet bus contract (WP-8, standards-buildout C1 / architecture §2.3 +
// D6 preamble): lexa-northbound resolves the active DER control's
// curve-linked modes (DERControlBase opMod*Link hrefs → DERCurve resources,
// fetched during the discovery walk) and publishes the resolved content
// RETAINED on TopicCSIPCurves as one CurveSet. The hub (WP-9's adv-doc
// author) consumes it alongside ActiveControl, correlating the two docs by
// ActiveControl.CurveSetID == CurveSet.SetID.
//
// Content-addressed identity: SetID is CurveSetContentHash over the
// canonicalized entries (below) — pure curve CONTENT, not resource identity
// — so a server that reissues byte-identical curve shapes under new resource
// MRIDs/hrefs does not force downstream re-adoption, and a reconciler can
// recompute the same hash from a device READBACK (which has no MRID to
// offer) to verify adoption (D6's "content hashes let both sides skip no-op
// re-adoptions").

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

// Curve-linked mode names carried in CurveSetEntry.Mode — the same
// vocabulary DERScheduleSlot's curve JSON keys already use, so the two docs
// never disagree on what a mode is called.
const (
	CurveModeVoltVar                = "volt_var"
	CurveModeFreqWatt               = "freq_watt"
	CurveModeWattPF                 = "watt_pf"
	CurveModeVoltWatt               = "volt_watt"
	CurveModeHFRTMayTrip            = "hfrt_may_trip"
	CurveModeHFRTMustTrip           = "hfrt_must_trip"
	CurveModeHVRTMayTrip            = "hvrt_may_trip"
	CurveModeHVRTMomentaryCessation = "hvrt_momentary_cessation"
	CurveModeHVRTMustTrip           = "hvrt_must_trip"
	CurveModeLFRTMayTrip            = "lfrt_may_trip"
	CurveModeLFRTMustTrip           = "lfrt_must_trip"
	CurveModeLVRTMayTrip            = "lvrt_may_trip"
	CurveModeLVRTMomentaryCessation = "lvrt_momentary_cessation"
	CurveModeLVRTMustTrip           = "lvrt_must_trip"
)

// CurveSet is the retained curve-content document on TopicCSIPCurves
// (northbound → hub, QoS 1, WP-8). One entry per curve-linked mode the
// active control carries a RESOLVABLE curve for ("per-mode presence": a mode
// absent from Curves is not commanded, or its href did not resolve — the
// latter is alarmed at the walker, never silent). An empty set (SetID "",
// no entries) is the explicit "the active control links no curves" state,
// published so a superseded curve set does not linger retained.
type CurveSet struct {
	Envelope

	// SetID is CurveSetContentHash(Curves): "" for an empty set, else the
	// hex SHA-256 of the canonicalized entries. ActiveControl.CurveSetID on
	// the matching lexa/csip/control publish carries the same value.
	SetID string `json:"set_id,omitempty"`

	// MRID is the DERControl/DefaultDERControl the curves accompany (the
	// same value as the matching ActiveControl.MRID). Identity metadata —
	// deliberately NOT part of the content hash.
	MRID string `json:"mrid,omitempty"`

	// Program is the DERProgram href the curves were resolved from.
	// Identity metadata — deliberately NOT part of the content hash.
	Program string `json:"program,omitempty"`

	// Curves holds one entry per present curve-linked mode, sorted by Mode
	// (the publisher's canonical order — also what the hash sorts by).
	Curves []CurveSetEntry `json:"curves,omitempty"`

	Ts int64 `json:"ts"` // Unix seconds of the publish
}

// CurveSetEntry is one resolved DERCurve bound to the curve-linked mode that
// referenced it. Points are the raw 2030.5 int32 breakpoints with the axis
// power-of-ten multipliers carried alongside (never pre-applied: applying
// 10^mult can overflow float precision, and the reconciler writes the raw
// register values anyway). ≤ 10 points per the 2030.5 DERCurve bound — the
// scheduler's plausibility gate rejects the whole control otherwise, and the
// publisher additionally refuses to emit an entry that fails the same gate.
type CurveSetEntry struct {
	Mode      string       `json:"mode"`                 // CurveMode* vocabulary above
	MRID      string       `json:"mrid,omitempty"`       // the DERCurve resource's mRID (metadata, not hashed)
	CurveType uint16       `json:"curve_type"`           // csipmodel CurveType* (Table 19)
	XMult     int8         `json:"x_mult,omitempty"`     // x-axis power-of-ten multiplier
	YMult     int8         `json:"y_mult,omitempty"`     // y-axis power-of-ten multiplier
	YRefType  uint8        `json:"y_ref_type,omitempty"` // Table 19 DERUnitRefType, when the server sent one (0 = N/A/absent)
	Points    []CurvePoint `json:"points"`               // ordered (x, y) breakpoints, 1..10
}

// ContentHash returns CurveSetContentHash over the set's entries.
func (cs CurveSet) ContentHash() string {
	return CurveSetContentHash(cs.Curves)
}

// CurveSetContentHash is the canonical content hash for a curve set (WP-8,
// pinned by TestCurveSetContentHash_Pinned). Canonicalization:
//
//   - entries are sorted by Mode (each mode appears at most once, so the
//     sort is total) — field/slice order in the caller's hands never moves
//     the hash;
//   - each entry contributes the line
//     "mode|curveType|xMult|yMult|yRefType|x1,y1;x2,y2;...;\n"
//     (decimal integers, the exact delimiters shown) to a single SHA-256;
//   - the hash is the lowercase hex digest;
//   - zero entries hash to "" — the same "no curves" sentinel
//     ActiveControl.CurveSetID uses (architecture §2.2).
//
// Deliberately EXCLUDED: entry MRID, set MRID/Program/Ts — those are
// resource identity/metadata, and the hash is content-addressed (see the
// package comment). Any change to this canonicalization is a CurveSetV
// version bump: both publisher and every consumer must agree on it.
func CurveSetContentHash(entries []CurveSetEntry) string {
	if len(entries) == 0 {
		return ""
	}
	sorted := make([]CurveSetEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Mode < sorted[j].Mode })

	h := sha256.New()
	for _, e := range sorted {
		fmt.Fprintf(h, "%s|%d|%d|%d|%d|", e.Mode, e.CurveType, e.XMult, e.YMult, e.YRefType)
		for _, p := range e.Points {
			fmt.Fprintf(h, "%d,%d;", p.X, p.Y)
		}
		h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}
