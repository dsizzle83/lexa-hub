package discovery

// paginate.go — IEEE 2030.5 list pagination for the discovery walker
// (audit docs/QA_COMPLETENESS_AUDIT.md P1-1).
//
// A 2030.5 list resource carries `all` (the total number of entries the server
// holds) and `results` (the number in THIS response). A spec-compliant server
// is free to page a large list: a single GET returns only the first page
// (`results` < `all`) and the client must request the rest with the `s` (start
// offset) and `l` (limit) query parameters until it has collected every entry.
// Before this file the walker did a single GET of each list and used only what
// came back — so against a paging server it would silently enforce only the
// first page's controls/programs/devices and drop everything past it (the exact
// "utility returns all=40, results=10 → hub obeys only the first 10" field
// failure P1-1 names).
//
// fetchPagedList adds a bounded, fail-closed paging loop. It is deliberately
// conservative:
//
//   - Single-page lists (every non-paginating server, including the bench
//     gridsim's default, and any server that omits `all`) take a fast path that
//     is byte-identical to a bare fetch — zero behaviour change off the paging
//     path.
//   - A mid-pagination fetch error propagates as an error (never a silently
//     truncated list): the caller's existing fail-closed discipline
//     (internal/northbound rule 6 / run.RunOnce) then holds the last complete
//     tree, so an adopted control is never dropped for a half-assembled one.
//   - A server that lies about `all` and ignores the offset (the
//     malform-pagination "all=999 with no real next page" bait,
//     sim/gridsim/malform.go) can never spin the loop: entries are de-duplicated
//     by href, and a page that adds nothing new ends the loop and uses what was
//     assembled — the same harmless outcome the old single-GET walker had.

import (
	"context"
	"fmt"
	"strings"
)

// maxListPages bounds how many pages fetchPagedList retrieves for one list
// before giving up — a fail-closed backstop against a server that advertises an
// ever-growing `all` while serving genuinely-new hrefs forever. It is never the
// normal terminator (that is "collected `all`" or "a page added nothing new");
// 256 pages is far beyond any real DER deployment's list, so tripping it means
// a pathological server and the walk fails closed (holds last-known-good).
const maxListPages = 256

// defaultPageLimit is the `l` hint the walker sends when the first page did not
// reveal the server's own page size (`results` == 0 while `all` > 0 — a
// malformed page). The server may serve fewer; the walker pages by offset
// regardless, so this is only a hint.
const defaultPageLimit = 255

// pagedList is the per-type glue fetchPagedList needs to page one kind of 2030.5
// list without knowing its concrete Go type: read the list-level `all`/`results`
// attributes and this page's entries, identify an entry (by href, for the
// ignored-offset guard), and install the fully-assembled slice back onto the
// first page's list object.
type pagedList[L any, E any] struct {
	fetch   func(ctx context.Context, path string) (*L, error) // fetch one page
	all     func(*L) uint32                                    // list "all" attr (total across all pages)
	results func(*L) uint32                                    // list "results" attr (this page's count)
	entries func(*L) []E                                       // this page's entries
	href    func(E) string                                     // an entry's href (dedupe / ignored-offset key)
	install func(l *L, e []E)                                  // replace entries + refresh all/results to len(e)
}

// fetchPagedList fetches every page of a 2030.5 list whose first page is at
// path, following `?s=<offset>&l=<limit>` until every advertised entry has been
// collected, then installs the concatenated entries onto the first page's list
// object (with `all` and `results` refreshed to the assembled count so the
// returned document is internally consistent) and returns it. See this file's
// package doc for the fail-closed contract.
func fetchPagedList[L any, E any](ctx context.Context, path string, p pagedList[L, E]) (*L, error) {
	first, err := p.fetch(ctx, path)
	if err != nil {
		return nil, err
	}
	all := p.all(first)
	acc := p.entries(first)

	// Fast path: the server put every entry on the first page (`results` >=
	// `all`, or it omitted `all`). Identical to a bare fetch — the returned
	// object is the untouched first page.
	if uint32(len(acc)) >= all {
		return first, nil
	}

	// Seed the seen-set with the first page's hrefs. A server that ignores `s`
	// (the lying-`all` malform) re-serves the same entries on the next request;
	// spotting a page that adds no NEW href is what stops the loop instead of
	// letting it run to the page guard. Empty hrefs are never deduped (a spec
	// list entry is an addressable resource with a unique href, but a server
	// that omits them must not have its real entries collapsed to one) — the
	// page guard is the backstop if such a server also lies about `all`.
	seen := make(map[string]struct{}, len(acc))
	for i := range acc {
		if h := p.href(acc[i]); h != "" {
			seen[h] = struct{}{}
		}
	}

	// Ask for the server's own page size back (its first-page `results`) as the
	// `l` hint; fall back to a default only if the first page was malformed
	// (`results` == 0 yet `all` > 0).
	limit := p.results(first)
	if limit == 0 {
		limit = defaultPageLimit
	}

	for pages := 1; uint32(len(acc)) < all; pages++ {
		if pages >= maxListPages {
			return nil, fmt.Errorf("discovery: paged list %s exceeded the %d-page guard (all=%d, assembled=%d) — refusing a runaway list", path, maxListPages, all, len(acc))
		}
		next, err := p.fetch(ctx, pagedPath(path, uint32(len(acc)), limit))
		if err != nil {
			// Fail closed: a transport error mid-pagination is a walk error, not
			// a short list. The caller holds last-known-good.
			return nil, fmt.Errorf("discovery: paged list %s at offset %d: %w", path, len(acc), err)
		}
		added := 0
		for _, e := range p.entries(next) {
			if h := p.href(e); h != "" {
				if _, dup := seen[h]; dup {
					continue
				}
				seen[h] = struct{}{}
			}
			acc = append(acc, e)
			added++
		}
		// An empty page, or one that repeats hrefs we already hold (server
		// ignored the offset), means there is no more real content. Stop and use
		// what we assembled — this is what makes the lying-`all` malform
		// harmless instead of an infinite loop.
		if added == 0 {
			break
		}
	}

	p.install(first, acc)
	return first, nil
}

// pagedPath appends the s (start offset) and l (limit) pagination query
// parameters to a list href, preserving any query string the href already
// carries. IEEE 2030.5 also defines `a` (after-key) paging; this walker uses the
// offset form (s/l), which the mainstream utility servers and the bench gridsim
// serve.
func pagedPath(base string, start, limit uint32) string {
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return fmt.Sprintf("%s%ss=%d&l=%d", base, sep, start, limit)
}
