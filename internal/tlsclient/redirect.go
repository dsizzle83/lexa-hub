package tlsclient

// 301/302 redirect following for the WolfSSLFetcher verbs (WP-3/D3,
// ERR-001). CSIP uses 302 for rediscovery — a resource moved under a new
// path on the SAME head-end — never for cross-host bouncing, so the rules
// here are deliberately fail-closed rather than RFC-general:
//
//   - same-host only: a path-only Location is taken as-is; an absolute URL
//     must name the exact configured server (host:port) or the redirect is
//     an error, never silently followed;
//   - never scheme-downgrade: everything this client sends rides the
//     wolfSSL mTLS session, so an http:// Location is refused outright;
//   - bounded: at most Config.RedirectMax hops per logical request; a
//     server still redirecting past the bound is an error, not a loop.
//     RedirectMax == 0 disables following entirely (the 30x surfaces as a
//     status error — pre-WP-3 behavior);
//   - verb-preserving: the re-issued request keeps its verb and body. This
//     client is not a browser — 2030.5's 301/302 means "resource moved",
//     not the historical method-rewriting form redirect.
//
// Like planReload (fetcher.go), the loop is a package-level function over a
// narrow closure so redirect_test.go exercises it with fakes in the plain
// `go test` run — no wolfSSL session involved.

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// maxRedirectLocation caps the Location value followRedirects will act on.
// The header block a Location arrives in is already bounded
// (maxResponseHeader, 64 KiB — client.go), so this is defense-in-depth with
// a much tighter bound: a real CSIP Location is a short resource href, and
// a kilobytes-long one is a hostile server, not a big deployment.
const maxRedirectLocation = 2048

// redirectsTotal is the monotonic count of 301/302 hops followRedirects has
// actually followed (a validated Location, about to be re-issued) — for
// lexa_nb_redirects_total (architecture.md §8, bench round 2 gap: named
// there but never wired up). internal/tlsclient is a leaf package that must
// stay decoupled from any metrics implementation (see internal/metrics'
// package doc and the CLAUDE.md dependency-posture rationale), so this is a
// package-level atomic with an exported accessor — the same shape
// discovery.IgnoredContentTotal uses for the analogous WP-8 counter — rather
// than importing internal/metrics or threading a callback through Config
// (three independent fetchers — discovery/response/flow-reservation, see
// cmd/northbound/main.go — would each need wiring; a single process-wide
// total needs none). cmd/northbound's metrics Collect callback scrapes it
// via RedirectsTotal, mirroring IgnoredContentTotal's wiring exactly.
var redirectsTotal uint64

// RedirectsTotal returns the total number of redirect hops followRedirects
// has followed across every WolfSSLFetcher in this process, for a metrics
// Collect callback to snapshot into lexa_nb_redirects_total.
func RedirectsTotal() uint64 {
	return atomic.LoadUint64(&redirectsTotal)
}

// isRedirectStatus reports whether code is one of the two redirect statuses
// followRedirects acts on. 303/307/308 are deliberately NOT followed: the
// pinned CSIP server family sends 301/302 only (D3), and anything else
// keeps failing closed as an unexpected status.
func isRedirectStatus(code int) bool {
	return code == 301 || code == 302
}

// resolveRedirectLocation validates a 301/302 Location value against the
// fail-closed D3 rules above and returns the path to re-issue against.
// serverAddr is the configured host:port (Config.ServerAddr). Anything that
// is not a plain path on the configured server — empty, oversized,
// CR/LF/space-bearing (the same injection guard every request path passes),
// scheme-relative (//host/...), http:// (downgrade), a different host, or
// an https URL with no path — is an error.
func resolveRedirectLocation(location, serverAddr string) (string, error) {
	if location == "" {
		return "", fmt.Errorf("redirect Location missing")
	}
	if len(location) > maxRedirectLocation {
		return "", fmt.Errorf("redirect Location too long: %d bytes (max %d)", len(location), maxRedirectLocation)
	}
	// The re-issued path would be rejected again by the Client verb's own
	// validateRequestParam, but checking here names the redirect as the
	// source instead of a generic "invalid path" a call layer later.
	if err := validateRequestParam(location, "redirect Location"); err != nil {
		return "", err
	}

	if strings.HasPrefix(location, "/") {
		// "//host/path" is a scheme-relative URL — a cross-host escape
		// disguised as a path. Refuse it before treating "/..." as path-only.
		if strings.HasPrefix(location, "//") {
			return "", fmt.Errorf("redirect Location %q: scheme-relative URL refused (same-host only)", location)
		}
		return location, nil
	}

	// Absolute URL: https only (never downgrade below the mTLS transport),
	// and the host:port must be exactly the configured server.
	const (
		httpPrefix  = "http://"
		httpsPrefix = "https://"
	)
	if len(location) >= len(httpPrefix) && strings.EqualFold(location[:len(httpPrefix)], httpPrefix) {
		return "", fmt.Errorf("redirect Location %q: scheme downgrade to http refused", location)
	}
	if len(location) < len(httpsPrefix) || !strings.EqualFold(location[:len(httpsPrefix)], httpsPrefix) {
		return "", fmt.Errorf("redirect Location %q: not a path or https URL on the configured server", location)
	}
	rest := location[len(httpsPrefix):]
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return "", fmt.Errorf("redirect Location %q: no path component", location)
	}
	host, path := rest[:slash], rest[slash:]
	if !strings.EqualFold(host, serverAddr) {
		return "", fmt.Errorf("redirect Location %q: host %q does not match configured server %q (same-host only)", location, host, serverAddr)
	}
	return path, nil
}

// followRedirects issues one request via issue and, while the response is a
// 301/302 and hops remain within redirectMax, resolves the Location and
// re-issues against the new path. It is the shared redirect driver for the
// fetcher's Get/Post/Put verbs — issue closes over the verb-specific
// primitive (doGet/doPost/doPut), so each hop gets that primitive's full
// redial-once and ConnClose handling. verb appears only in error text.
//
// redirectMax <= 0 returns the first response untouched, redirect or not —
// the caller's status check then fails it exactly as before this existed.
// A Location that violates resolveRedirectLocation's rules, or a server
// still redirecting after redirectMax follows, is an error.
func followRedirects(verb, path string, redirectMax int, serverAddr string, issue func(string) (*HTTPResponse, error)) (*HTTPResponse, error) {
	resp, err := issue(path)
	if err != nil {
		return nil, err
	}
	if redirectMax <= 0 {
		return resp, nil
	}
	cur := path
	for hops := 0; isRedirectStatus(resp.StatusCode); hops++ {
		if hops >= redirectMax {
			return nil, fmt.Errorf("%s %s: redirect limit %d exceeded (last Location %q)", verb, path, redirectMax, resp.Location)
		}
		next, rerr := resolveRedirectLocation(resp.Location, serverAddr)
		if rerr != nil {
			return nil, fmt.Errorf("%s %s: status %d: %w", verb, cur, resp.StatusCode, rerr)
		}
		atomic.AddUint64(&redirectsTotal, 1)
		cur = next
		resp, err = issue(cur)
		if err != nil {
			return nil, err
		}
	}
	return resp, nil
}
