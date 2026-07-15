package discovery

// pagination_test.go — walker list-pagination tests (audit P1-1).
//
// Proves the paging loop (paginate.go, wired into walker.go's four list
// fetchers) reassembles a list a spec-compliant server serves across multiple
// pages, that a single-page fetch would silently miss the tail (the bug the fix
// closes), and that a server lying about `all` / ignoring the offset can never
// spin the loop.

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/url"
	"strconv"
	"testing"

	model "lexa-proto/csipmodel"
)

// ───────────────────────────────────────────────────────────────────────
// pagingFetcher — a Fetcher that serves registered lists with real s/l
// pagination, so the walker's paging loop must reassemble them. Any path not
// registered as a paged list falls through to an inner mockFetcher (whole
// resource, query ignored).
// ───────────────────────────────────────────────────────────────────────

type pagingFetcher struct {
	inner    *mockFetcher
	paged    map[string]interface{} // basePath → the FULL list object
	pageSize int
	getCalls []string

	// Fault knobs. ignoreOffset makes every paged page start at 0 regardless of
	// s (a server that ignores the offset); overrideAll stamps a fake `all` on
	// every page (the lying-`all` bait). Together they model malform-pagination.
	ignoreOffset bool
	overrideAll  uint32
}

func newPagingFetcher(pageSize int) *pagingFetcher {
	return &pagingFetcher{inner: newMockFetcher(), paged: map[string]interface{}{}, pageSize: pageSize}
}

// servePaged registers a full list object to be served with s/l pagination.
func (f *pagingFetcher) servePaged(path string, list interface{}) { f.paged[path] = list }

// serve registers a whole (non-paged) resource on the inner fetcher.
func (f *pagingFetcher) serve(path string, resource interface{}) { f.inner.serve(path, resource) }

func (f *pagingFetcher) Get(ctx context.Context, rawPath string) ([]byte, error) {
	f.getCalls = append(f.getCalls, rawPath)
	base, start, limit := splitPaged(rawPath)
	full, ok := f.paged[base]
	if !ok {
		return f.inner.Get(ctx, base)
	}
	if f.ignoreOffset {
		start = 0
	}
	if limit == 0 || int(limit) > f.pageSize {
		limit = uint32(f.pageSize)
	}
	page, err := slicePaged(full, start, limit, f.overrideAll)
	if err != nil {
		return nil, err
	}
	return xml.Marshal(page)
}

// splitPaged separates a request path into its base and the s/l query values.
func splitPaged(rawPath string) (base string, start, limit uint32) {
	u, err := url.Parse(rawPath)
	if err != nil {
		return rawPath, 0, 0
	}
	q := u.Query()
	if v, err := strconv.ParseUint(q.Get("s"), 10, 32); err == nil {
		start = uint32(v)
	}
	if v, err := strconv.ParseUint(q.Get("l"), 10, 32); err == nil {
		limit = uint32(v)
	}
	return u.Path, start, limit
}

// slicePaged returns a copy of a list resource holding only entries
// [start, start+limit), with `all` set to the full count (or overrideAll when
// nonzero) and `results` to the slice count.
func slicePaged(full interface{}, start, limit, overrideAll uint32) (interface{}, error) {
	sub := func(n uint32) (lo, hi uint32) {
		lo = start
		if lo > n {
			lo = n
		}
		hi = lo + limit
		if hi > n {
			hi = n
		}
		return lo, hi
	}
	allOf := func(realCount uint32) uint32 {
		if overrideAll > 0 {
			return overrideAll
		}
		return realCount
	}
	switch l := full.(type) {
	case *model.EndDeviceList:
		lo, hi := sub(uint32(len(l.EndDevice)))
		c := *l
		c.EndDevice = l.EndDevice[lo:hi]
		c.All, c.Results = allOf(uint32(len(l.EndDevice))), hi-lo
		return &c, nil
	case *model.DERProgramList:
		lo, hi := sub(uint32(len(l.DERProgram)))
		c := *l
		c.DERProgram = l.DERProgram[lo:hi]
		c.All, c.Results = allOf(uint32(len(l.DERProgram))), hi-lo
		return &c, nil
	case *model.ExtendedDERControlList:
		lo, hi := sub(uint32(len(l.DERControl)))
		c := *l
		c.DERControl = l.DERControl[lo:hi]
		c.All, c.Results = allOf(uint32(len(l.DERControl))), hi-lo
		return &c, nil
	case *model.MirrorUsagePointList:
		lo, hi := sub(uint32(len(l.MirrorUsagePoint)))
		c := *l
		c.MirrorUsagePoint = l.MirrorUsagePoint[lo:hi]
		c.All, c.Results = allOf(uint32(len(l.MirrorUsagePoint))), hi-lo
		return &c, nil
	}
	return nil, fmt.Errorf("slicePaged: unsupported list type %T", full)
}

// ───────────────────────────────────────────────────────────────────────
// Tree builders
// ───────────────────────────────────────────────────────────────────────

// buildPaginatedTree serves a CSIP tree whose EndDeviceList (3 devices, self
// last), DERProgramList (4 programs), and program-0 DERControlList (5 controls)
// are ALL served with pagination, so a correct walk must page every one.
func buildPaginatedTree(f *pagingFetcher) {
	boolTrue := true

	f.serve("/dcap", &model.DeviceCapability{
		Resource:                 model.Resource{Href: "/dcap"},
		TimeLink:                 &model.Link{Href: "/tm"},
		EndDeviceListLink:        &model.ListLink{Link: model.Link{Href: "/edev"}, All: 3},
		MirrorUsagePointListLink: &model.ListLink{Link: model.Link{Href: "/mup"}, All: 0},
	})
	f.serve("/tm", &model.Time{Resource: model.Resource{Href: "/tm"}, CurrentTime: 1700000000})

	// EndDeviceList: 3 devices, self (testLFDI) is index 2 — with pageSize 1 the
	// walker must fetch 3 pages to find itself.
	f.servePaged("/edev", &model.EndDeviceList{
		Resource: model.Resource{Href: "/edev"}, All: 3, Results: 3,
		EndDevice: []model.EndDevice{
			{Resource: model.Resource{Href: "/edev/0"}, LFDI: "1111111111111111111111111111111111111111", SFDI: 111111111},
			{Resource: model.Resource{Href: "/edev/1"}, LFDI: "2222222222222222222222222222222222222222", SFDI: 222222222},
			{Resource: model.Resource{Href: "/edev/2"}, LFDI: testLFDI, SFDI: testSFDI, Enabled: &boolTrue,
				FunctionSetAssignmentsListLink: &model.ListLink{Link: model.Link{Href: "/edev/2/fsa"}, All: 1}},
		},
	})

	f.serve("/edev/2/fsa", &model.FunctionSetAssignmentsList{
		Resource: model.Resource{Href: "/edev/2/fsa"}, All: 1, Results: 1,
		FunctionSetAssignments: []model.FunctionSetAssignments{{
			Resource:           model.Resource{Href: "/edev/2/fsa/0"},
			MRID:               "FSA1",
			DERProgramListLink: &model.ListLink{Link: model.Link{Href: "/edev/2/fsa/0/derp"}, All: 4},
		}},
	})

	// DERProgramList: 4 programs — with pageSize 2 the walker must fetch 2 pages.
	progs := make([]model.DERProgram, 4)
	primacy := []uint8{1, 5, 10, 15}
	for i := range progs {
		progs[i] = model.DERProgram{
			Resource:              model.Resource{Href: fmt.Sprintf("/derp/%d", i)},
			MRID:                  fmt.Sprintf("PROG-%d", i),
			Primacy:               primacy[i],
			DefaultDERControlLink: &model.Link{Href: fmt.Sprintf("/derp/%d/dderc", i)},
			DERControlListLink:    &model.ListLink{Link: model.Link{Href: fmt.Sprintf("/derp/%d/derc", i)}, All: 1},
		}
		f.serve(fmt.Sprintf("/derp/%d/dderc", i), &model.ExtendedDefaultDERControl{
			Resource: model.Resource{Href: fmt.Sprintf("/derp/%d/dderc", i)}, MRID: fmt.Sprintf("DD-%d", i),
			DERControlBase: model.ExtendedDERControlBase{OpModExpLimW: &model.ActivePower{Value: 5000}},
		})
	}
	f.servePaged("/edev/2/fsa/0/derp", &model.DERProgramList{
		Resource: model.Resource{Href: "/edev/2/fsa/0/derp"}, All: 4, Results: 4, DERProgram: progs,
	})

	// Program 0 DERControlList: 5 controls — with pageSize 2 the walker must
	// fetch 3 pages to assemble them all.
	ctrls := make([]model.ExtendedDERControl, 5)
	for i := range ctrls {
		ctrls[i] = model.ExtendedDERControl{
			Resource:     model.Resource{Href: fmt.Sprintf("/derp/0/derc/%d", i)},
			MRID:         fmt.Sprintf("CTRL-%d", i),
			CreationTime: int64(1700000000 + i),
			EventStatus:  &model.EventStatus{CurrentStatus: 0, DateTime: 1700000000},
			Interval:     model.DateTimeInterval{Duration: 3600, Start: 1700003600},
			DERControlBase: model.ExtendedDERControlBase{
				OpModExpLimW: &model.ActivePower{Value: int16(1000 + i*500)},
			},
		}
	}
	f.servePaged("/derp/0/derc", &model.ExtendedDERControlList{
		Resource: model.Resource{Href: "/derp/0/derc"}, All: 5, Results: 5, DERControl: ctrls,
	})
	// Programs 1-3 serve a single control each (single-page, not registered paged).
	for i := 1; i < 4; i++ {
		f.serve(fmt.Sprintf("/derp/%d/derc", i), &model.ExtendedDERControlList{
			Resource: model.Resource{Href: fmt.Sprintf("/derp/%d/derc", i)}, All: 1, Results: 1,
			DERControl: []model.ExtendedDERControl{{
				Resource: model.Resource{Href: fmt.Sprintf("/derp/%d/derc/0", i)}, MRID: fmt.Sprintf("PC-%d", i),
				EventStatus: &model.EventStatus{CurrentStatus: 0, DateTime: 1700000000},
				Interval:    model.DateTimeInterval{Duration: 3600, Start: 1700003600},
			}},
		})
	}

	f.servePaged("/mup", &model.MirrorUsagePointList{Resource: model.Resource{Href: "/mup"}, All: 0, Results: 0})
}

// ───────────────────────────────────────────────────────────────────────
// Tests
// ───────────────────────────────────────────────────────────────────────

// TestWalker_AssemblesAllPages proves the walker collects every entry of a
// paginated EndDeviceList, DERProgramList, and DERControlList — not just page 1.
func TestWalker_AssemblesAllPages(t *testing.T) {
	f := newPagingFetcher(2) // 2 entries per page
	buildPaginatedTree(f)

	w := NewWalker(f, testLFDI)
	tree, err := w.Discover(context.Background(), "/dcap")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	// /edev paged at pageSize 2, self (testLFDI) is the 3rd device → the walker
	// only finds itself if it pages past page 1.
	if tree.SelfDevice == nil {
		t.Fatal("self device not found — walker failed to page the EndDeviceList to reach itself")
	}
	if got := len(tree.EndDeviceList.EndDevice); got != 3 {
		t.Errorf("EndDeviceList: assembled %d devices, want 3 (all pages)", got)
	}
	if got := len(tree.Programs); got != 4 {
		t.Errorf("DERProgramList: assembled %d programs, want 4 (all pages)", got)
	}
	// Program 0's DERControlList had 5 controls across 3 pages.
	var prog0 *ProgramState
	for i := range tree.Programs {
		if tree.Programs[i].Program.MRID == "PROG-0" {
			prog0 = &tree.Programs[i]
		}
	}
	if prog0 == nil {
		t.Fatal("PROG-0 missing from assembled programs")
	}
	if prog0.Controls == nil || len(prog0.Controls.DERControl) != 5 {
		t.Fatalf("PROG-0 DERControlList: assembled %v controls, want 5 (all pages)",
			ctrlCount(prog0))
	}
	// The reassembled list must be self-consistent: all == results == 5.
	if prog0.ExtendedControls.All != 5 || prog0.ExtendedControls.Results != 5 {
		t.Errorf("reassembled list all/results = %d/%d, want 5/5",
			prog0.ExtendedControls.All, prog0.ExtendedControls.Results)
	}
}

func ctrlCount(ps *ProgramState) int {
	if ps.Controls == nil {
		return 0
	}
	return len(ps.Controls.DERControl)
}

// TestWalker_SinglePageFetchMissesTail confirms the FIX is load-bearing: the
// underlying single-page fetch helper (what the walker used before P1-1) returns
// only the first page, so without the paging loop the walker would silently
// enforce a truncated list.
func TestWalker_SinglePageFetchMissesTail(t *testing.T) {
	f := newPagingFetcher(2)
	buildPaginatedTree(f)
	w := NewWalker(f, testLFDI)

	// Single-page (pre-fix behaviour): first page only.
	single, err := w.fetchExtendedDERControlList(context.Background(), "/derp/0/derc")
	if err != nil {
		t.Fatalf("single-page fetch: %v", err)
	}
	if len(single.DERControl) != 2 {
		t.Fatalf("single-page fetch returned %d controls, want 2 (one page) — test tree not paginating", len(single.DERControl))
	}

	// Paged (post-fix behaviour): every page.
	paged, err := w.fetchExtendedDERControlListPaged(context.Background(), "/derp/0/derc")
	if err != nil {
		t.Fatalf("paged fetch: %v", err)
	}
	if len(paged.DERControl) != 5 {
		t.Fatalf("paged fetch returned %d controls, want 5 (all pages)", len(paged.DERControl))
	}
	if len(paged.DERControl) <= len(single.DERControl) {
		t.Errorf("paging assembled no more than one page (%d <= %d) — the fix is not exercised",
			len(paged.DERControl), len(single.DERControl))
	}
}

// TestFetchPagedList_LyingAllTerminates proves a server that advertises a huge
// `all` but ignores the offset (re-serving the same page — the
// malform-pagination bait) cannot spin the loop: paging stops as soon as a page
// adds no new href, and the real entries are returned unharmed.
func TestFetchPagedList_LyingAllTerminates(t *testing.T) {
	f := newPagingFetcher(5) // page size >= the 3 real entries: page 0 carries them all
	f.ignoreOffset = true    // every page re-serves from offset 0
	f.overrideAll = 999      // …while claiming 999 entries exist
	f.servePaged("/edev/2/fsa/0/derp", &model.DERProgramList{
		Resource: model.Resource{Href: "/derp"}, All: 999, Results: 3,
		DERProgram: []model.DERProgram{
			{Resource: model.Resource{Href: "/derp/0"}, MRID: "P0"},
			{Resource: model.Resource{Href: "/derp/1"}, MRID: "P1"},
			{Resource: model.Resource{Href: "/derp/2"}, MRID: "P2"},
		},
	})

	w := NewWalker(f, testLFDI)
	got, err := w.fetchDERProgramListPaged(context.Background(), "/edev/2/fsa/0/derp")
	if err != nil {
		t.Fatalf("paged fetch on a lying-all server should not error, got: %v", err)
	}
	if len(got.DERProgram) != 3 {
		t.Fatalf("lying-all server: assembled %d programs, want the 3 real ones (no loop, no over-fetch)", len(got.DERProgram))
	}
	// A handful of fetches at most — never anywhere near the 256-page guard.
	if len(f.getCalls) > 4 {
		t.Errorf("made %d fetches against a lying-all server (%v) — the ignored-offset guard did not stop the loop promptly", len(f.getCalls), f.getCalls)
	}
}

// TestFetchPagedList_MidPaginationErrorFailsClosed proves a transport error on a
// later page fails the whole fetch (so the caller holds last-known-good) rather
// than returning a silently-truncated list.
func TestFetchPagedList_MidPaginationErrorFailsClosed(t *testing.T) {
	fe := &pageErrFetcher{failFromOffset: 2, full: []model.DERProgram{
		{Resource: model.Resource{Href: "/derp/0"}, MRID: "P0"},
		{Resource: model.Resource{Href: "/derp/1"}, MRID: "P1"},
		{Resource: model.Resource{Href: "/derp/2"}, MRID: "P2"},
		{Resource: model.Resource{Href: "/derp/3"}, MRID: "P3"},
	}, pageSize: 2, all: 4}

	w := NewWalker(fe, testLFDI)
	_, err := w.fetchDERProgramListPaged(context.Background(), "/derp")
	if err == nil {
		t.Fatal("a mid-pagination fetch error must propagate (fail closed), not return a truncated list")
	}
}

// pageErrFetcher serves DERProgramList pages but returns an error once the
// requested offset reaches failFromOffset.
type pageErrFetcher struct {
	full           []model.DERProgram
	pageSize       int
	all            uint32
	failFromOffset uint32
}

func (p *pageErrFetcher) Get(ctx context.Context, rawPath string) ([]byte, error) {
	_, start, _ := splitPaged(rawPath)
	if start >= p.failFromOffset {
		return nil, fmt.Errorf("simulated transport error at offset %d", start)
	}
	hi := start + uint32(p.pageSize)
	if hi > uint32(len(p.full)) {
		hi = uint32(len(p.full))
	}
	return xml.Marshal(&model.DERProgramList{
		Resource: model.Resource{Href: "/derp"}, All: p.all, Results: hi - start,
		DERProgram: p.full[start:hi],
	})
}
