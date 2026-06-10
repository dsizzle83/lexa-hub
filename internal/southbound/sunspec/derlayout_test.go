package sunspec

import "testing"

// TestLayoutLengths pins the total register count of each fixed model and the
// header of each curve model, computed by hand from the spec tables. A failure
// here means a point was transcribed with the wrong width or dropped.
func TestLayoutLengths(t *testing.T) {
	cases := []struct {
		name string
		got  int
		want int
	}{
		{"701", L701.Len(), 137},
		{"702", L702.Len(), 50},
		{"703", L703.Len(), 17},
		{"704", L704.Len(), 65},
		{"705Hdr", L705Hdr.Len(), 13},
		{"705Crv", L705Crv.Len(), 10},
		{"706Hdr", L706Hdr.Len(), 13},
		{"706Crv", L706Crv.Len(), 5},
		{"707Hdr", L707Hdr.Len(), 7},
		{"709Hdr", L709Hdr.Len(), 7},
		{"711Hdr", L711Hdr.Len(), 12},
		{"711Ctl", L711Ctl.Len(), 10},
		{"712Hdr", L712Hdr.Len(), 12},
		{"712Crv", L712Crv.Len(), 4},
		{"713", L713.Len(), 7},
		{"714Hdr", L714Hdr.Len(), 18},
		{"714Prt", L714Prt.Len(), 25},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s length = %d, want %d", c.name, c.got, c.want)
		}
	}
}

// TestKeyOffsets pins offsets of points that sit after multi-register fields,
// where an off-by-one in a uint32/uint64 width would shift everything.
func TestKeyOffsets(t *testing.T) {
	cases := []struct {
		name string
		got  int
		want int
	}{
		{"701.St", L701.Offset("St"), 1},
		{"701.W", L701.Offset("W"), 8},      // after ACType,St,InvSt,ConnSt,Alrm(2),DERMode(2)
		{"701.Hz", L701.Offset("Hz"), 15},   // after W,VA,Var,PF,A,LLV,LNV
		{"701.TotWhInj", L701.Offset("TotWhInj"), 17},
		{"701.WL1", L701.Offset("WL1"), 39}, // after Hz(2)+4×uint64(16)+6×temp
		{"702.CtrlModes", L702.Offset("CtrlModes"), 21},
		{"702.WMax", L702.Offset("WMax"), 24},
		{"703.ESHzHi", L703.Offset("ESHzHi"), 3},
		{"703.ESDlyTms", L703.Offset("ESDlyTms"), 7}, // ES,ESVHi,ESVLo,ESHzHi(2),ESHzLo(2)
		{"704.WSet", L704.Offset("WSet"), 22},
		{"704.VarSet", L704.Offset("VarSet"), 36},
		{"704.PFWInj_PF", L704.Offset("PFWInj_PF"), 57},
		{"713.Sta", L713.Offset("Sta"), 4},
		{"714.DCWhInj", L714Hdr.Offset("DCWhInj"), 5},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s offset = %d, want %d", c.name, c.got, c.want)
		}
	}
}

// TestCurveStride validates the repeating-group offset helpers for NPt=5.
func TestCurveStride(t *testing.T) {
	if got := CurveOffset705(1, 4); got != 31 {
		t.Errorf("CurveOffset705(1,4) = %d, want 31", got)
	}
	if got := PointOffset705(1, 0, 4); got != 41 {
		t.Errorf("PointOffset705(1,0,4) = %d, want 41", got)
	}
	if got := tripVSetSize(5); got != 49 {
		t.Errorf("tripVSetSize(5) = %d, want 49", got)
	}
	if got := tripHzSetSize(5); got != 64 {
		t.Errorf("tripHzSetSize(5) = %d, want 64", got)
	}
	// Curve-set 0: MustTrip ActPt at 8, MayTrip at 8+16=24, MomCess at 40.
	if got := SubCurveOffset707(0, SubMayTrip, 5); got != L707Hdr.Len()+1+16 {
		t.Errorf("SubCurveOffset707 MayTrip = %d, want %d", got, L707Hdr.Len()+1+16)
	}
	if got := CtlOffset711(1); got != 22 {
		t.Errorf("CtlOffset711(1) = %d, want 22", got)
	}
	if got := PortOffset714(1); got != 43 {
		t.Errorf("PortOffset714(1) = %d, want 43", got)
	}
}
