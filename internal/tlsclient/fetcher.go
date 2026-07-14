package tlsclient

import (
	"context"
	"fmt"
	"sync"
)

// WolfSSLFetcher implements discovery.Fetcher using wolfSSL mTLS.
//
// The wolfSSL context (cert loading and cipher config) is created once
// in NewWolfSSLFetcher and reused for the lifetime of the fetcher.
//
// Persistent connection: the fetcher keeps a single TLS session alive
// across multiple Get/Post calls, redialing automatically when the
// server closes the connection or on any I/O error. A mutex serializes
// requests so the session is never used concurrently.
type WolfSSLFetcher struct {
	client *Client
	mu     sync.Mutex
}

// NewWolfSSLFetcher allocates a wolfSSL context configured for CSIP mTLS.
// Call Free when the fetcher is no longer needed.
func NewWolfSSLFetcher(cfg Config) (*WolfSSLFetcher, error) {
	c, err := New(cfg)
	if err != nil {
		return nil, err
	}
	return &WolfSSLFetcher{client: c}, nil
}

// Free releases the underlying wolfSSL context. After Free, the
// WolfSSLFetcher must not be used.
func (f *WolfSSLFetcher) Free() {
	f.client.Free()
}

// reloadable is the subset of *Client's lifecycle planReload's state
// machine drives: dial the new session and probe it with a raw GET. It is
// satisfied structurally by *Client (New's return type) — no changes to
// Client were needed. Defined here, at the point of consumption (05 §2), so
// planReload's ordering can be exercised in reload_test.go with a fake, in
// a plain `go test` run with no wolfSSL sysroot/cgo involved. The real
// wolfSSL Free-ordering this seam relies on (Close → FreeSSL → FreeCtx,
// internally inside Client.Free) is proven against a live in-process
// wolfSSL server by the `integration`-tagged tests in
// reload_integration_test.go (`go test -tags=integration`, desktop amd64
// sysroot — TASK-073/§8.6/RSK-07).
type reloadable interface {
	Dial() error
	Get(path string) ([]byte, error)
	Free()
}

// planReload drives the probe-then-commit ordering TASK-073/RSK-07 exists
// to get right: dial newClient (the freshly built, not-yet-trusted session
// for the rotated cert) and probe it with one GET to probePath — in
// production, DeviceCapability ("/dcap"), the same resource the discovery
// walker fetches first, so a successful, parsed, 200-status probe is a real
// proof the new cert is accepted end-to-end (TLS handshake AND the CSIP
// application layer — a wrong-CA cert fails at Dial; a right-CA cert for
// the wrong/unregistered device can still complete the handshake and only
// 403 at the HTTP layer, which raw success alone would miss).
//
// On any failure (dial, transport, parse, or non-200 status), newClient is
// torn down completely here (Free — which internally does
// Close-then-FreeCtx, matching the required Close → FreeSSL → FreeCtx
// order) and the error is returned; the caller's existing session is
// untouched and remains fully functional — a failed rotation attempt costs
// nothing (RSK-07 "probe failure leaves old path fully functional").
//
// On success, planReload returns nil and newClient is left connected and
// untouched (still holding its probed session) for the caller to install
// as the active client and to free the OLD one — ownership transfer, never
// concurrent use, which is the entire point of doing this as probe-THEN-
// commit rather than swap-then-verify.
func planReload(newClient reloadable, probePath string) error {
	if err := newClient.Dial(); err != nil {
		newClient.Free()
		return fmt.Errorf("reload: dial new session: %w", err)
	}
	raw, err := newClient.Get(probePath)
	if err != nil {
		newClient.Free()
		return fmt.Errorf("reload: probe %s failed on new session: %w", probePath, err)
	}
	resp, err := parseHTTPResponse(raw)
	if err != nil {
		newClient.Free()
		return fmt.Errorf("reload: probe %s: parse response: %w", probePath, err)
	}
	if resp.StatusCode != 200 {
		newClient.Free()
		return fmt.Errorf("reload: probe %s: status %d, want 200", probePath, resp.StatusCode)
	}
	return nil
}

// Reload atomically replaces f's underlying wolfSSL session with one built
// from newCfg (new ClientCertPath/ClientKeyPath — a staged cert rotation;
// ServerAddr/CACertPath are normally unchanged) — TASK-073/§10.5. It never
// frees the old session while ANY Get/Post/GetStatus call could be
// in-flight on it: Reload takes the SAME f.mu those methods take, so the
// runtime effect is identical to calling Reload "between" two ordinary
// requests, from any goroutine — the mutex is what makes the swap safe, not
// which goroutine happens to call it (the rotation trigger in
// cmd/northbound/rotate.go runs on its own goroutine, distinct from the
// discovery walk loop).
//
// The new session is fully built, dialed, and probed with a GET to
// probePath (pass "/dcap" for all three fetchers — DeviceCapability is
// always the entry point and requires no prior walk state) BEFORE f.mu is
// ever taken — see planReload. Only once the new session has proven itself
// does Reload take the lock, swap the pointer, and free the old one
// (Close → FreeSSL → FreeCtx, inside Client.Free) — by which point no
// Get/Post/GetStatus call can still be referencing it (they either
// completed before this critical section, or are blocked on f.mu and will
// observe the new client once they proceed).
//
// Failure returns a non-nil error; f is left exactly as it was (still
// using its previous cert) and the caller should alarm and retry per its
// own policy (see cmd/northbound/rotate.go).
func (f *WolfSSLFetcher) Reload(newCfg Config, probePath string) error {
	newClient, err := New(newCfg)
	if err != nil {
		return fmt.Errorf("reload: build new client: %w", err)
	}
	if err := planReload(newClient, probePath); err != nil {
		return err
	}

	f.mu.Lock()
	old := f.client
	f.client = newClient
	f.mu.Unlock()

	// Ownership transfer complete: f.client now points at newClient, so any
	// Get/Post/GetStatus that acquires f.mu from here on uses the new
	// session exclusively. old is reachable only through this local
	// variable — no in-flight call could still be holding it, because every
	// caller of doGet/doPost/GetStatus holds f.mu for its entire request,
	// and this critical section already completed the swap before
	// releasing that same mutex. Free it now: Close (Shutdown + FreeSSL)
	// then FreeCtx, in that order, inside Client.Free.
	old.Free()
	return nil
}

// ensureDialed dials if not currently connected. Must be called with f.mu held.
func (f *WolfSSLFetcher) ensureDialed() error {
	if f.client.ssl != nil {
		return nil
	}
	return f.client.Dial()
}

// doGet executes one GET, reconnecting once on failure. f.mu must be held.
func (f *WolfSSLFetcher) doGet(path string) (*HTTPResponse, error) {
	if err := f.ensureDialed(); err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	raw, err := f.client.Get(path)
	if err != nil {
		// Stale connection — close and retry once.
		f.client.Close()
		if err2 := f.client.Dial(); err2 != nil {
			return nil, fmt.Errorf("redial: %w", err2)
		}
		raw, err = f.client.Get(path)
		if err != nil {
			f.client.Close()
			return nil, err
		}
	}
	resp, err := parseHTTPResponse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse response from %s: %w", path, err)
	}
	// Server sent Connection: close — preemptively close so the next
	// call triggers a fresh dial rather than writing on a dead socket.
	if resp.ConnClose {
		f.client.Close()
	}
	return resp, nil
}

// doPost executes one POST, reconnecting once on failure. f.mu must be held.
func (f *WolfSSLFetcher) doPost(path string, body []byte, contentType string) (*HTTPResponse, error) {
	if err := f.ensureDialed(); err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	raw, err := f.client.Post(path, body, contentType)
	if err != nil {
		f.client.Close()
		if err2 := f.client.Dial(); err2 != nil {
			return nil, fmt.Errorf("redial: %w", err2)
		}
		raw, err = f.client.Post(path, body, contentType)
		if err != nil {
			f.client.Close()
			return nil, err
		}
	}
	resp, err := parseHTTPResponse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse response from %s: %w", path, err)
	}
	if resp.ConnClose {
		f.client.Close()
	}
	return resp, nil
}

// doPut executes one PUT, reconnecting once on failure. f.mu must be held.
func (f *WolfSSLFetcher) doPut(path string, body []byte, contentType string) (*HTTPResponse, error) {
	if err := f.ensureDialed(); err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	raw, err := f.client.Put(path, body, contentType)
	if err != nil {
		f.client.Close()
		if err2 := f.client.Dial(); err2 != nil {
			return nil, fmt.Errorf("redial: %w", err2)
		}
		raw, err = f.client.Put(path, body, contentType)
		if err != nil {
			f.client.Close()
			return nil, err
		}
	}
	resp, err := parseHTTPResponse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse response from %s: %w", path, err)
	}
	if resp.ConnClose {
		f.client.Close()
	}
	return resp, nil
}

// Get satisfies discovery.Fetcher. Returns the response body on 200 with
// Content-Type application/sep+xml; any other status/type is an error.
//
// 301/302 redirects are followed within Config.RedirectMax hops before the
// status check runs (WP-3/D3, ERR-001 — same-host only, never
// scheme-downgrade; see redirect.go). RedirectMax 0 (the zero value)
// disables following and a 30x fails the status check exactly as before.
//
// Cancellation contract (TASK-070, R5): ctx is checked once, after
// acquiring the session mutex and before dialing or writing any bytes —
// so a canceled ctx never starts a new request. There is no way to
// interrupt a request already in flight: wolfSSL performs a blocking
// read(2)/write(2) directly on the duplicated fd (client.go's
// SO_RCVTIMEO/SNDTIMEO, set at Dial time), which Go's netpoller-based
// context plumbing cannot reach without closing the fd out from under
// wolfSSL mid-read — that is RSK-07 segfault territory and deliberately
// not attempted here. In practice this means cancellation is honored
// *between* requests, and any single in-flight request still returns
// within its configured ReadTimeout regardless of ctx. Callers that walk
// many resources (internal/northbound/discovery.Walker) get abort
// granularity of "between fetches," which is the documented, honest
// contract — not "instantly."
func (f *WolfSSLFetcher) Get(ctx context.Context, path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	resp, err := followRedirects("GET", path, f.client.cfg.RedirectMax, f.client.cfg.ServerAddr, f.doGet)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: status %d", path, resp.StatusCode)
	}
	if resp.ContentType != "application/sep+xml" {
		return nil, fmt.Errorf("GET %s: Content-Type %q, want application/sep+xml (GEN.003)", path, resp.ContentType)
	}
	return resp.Body, nil
}

// postResult validates a POST's HTTPResponse and extracts (body, Location).
// Shared by Post and PostContext so the two never drift on what counts as a
// successful POST.
func postResult(path string, resp *HTTPResponse) ([]byte, string, error) {
	if resp.StatusCode != 201 && resp.StatusCode != 204 {
		return nil, "", fmt.Errorf("POST %s: status %d", path, resp.StatusCode)
	}
	return resp.Body, resp.Location, nil
}

// doPostFollowing runs doPost through the shared redirect driver
// (followRedirects, redirect.go) — one helper so Post and PostContext can no
// more drift on redirect behavior than postResult lets them drift on status
// handling. Each hop re-issues the SAME body/contentType (D3:
// verb-preserving). f.mu must be held.
func (f *WolfSSLFetcher) doPostFollowing(path string, body []byte, contentType string) (*HTTPResponse, error) {
	return followRedirects("POST", path, f.client.cfg.RedirectMax, f.client.cfg.ServerAddr,
		func(p string) (*HTTPResponse, error) { return f.doPost(p, body, contentType) })
}

// Post performs a single HTTP POST over the persistent wolfSSL session.
// Returns the response body and Location header (for 201 Created).
// Accepts 201 and 204; any other status is an error.
//
// No ctx parameter: Post's callers (internal/northbound/responses.Tracker,
// internal/northbound/flowres.Manager) are driven by MQTT subscription
// callbacks and the walk cycle's own fail-closed bookkeeping, not by the
// service-shutdown ctx this task threads through the discovery walk — see
// PostContext for the ctx-aware sibling lexa-telemetry's POST loop uses.
func (f *WolfSSLFetcher) Post(path string, body []byte, contentType string) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	resp, err := f.doPostFollowing(path, body, contentType)
	if err != nil {
		return nil, "", err
	}
	return postResult(path, resp)
}

// PostContext is Post with the same between-requests cancellation contract
// documented on Get: ctx is checked once, after acquiring the session mutex
// and before dialing or writing, so a canceled ctx never starts a new POST.
// It does not interrupt a POST already in flight (see Get's doc comment for
// why). Added alongside Post — rather than changing Post's signature — so
// the two other production Poster consumers (responses.Tracker,
// flowres.Manager) are untouched by this task (TASK-070's blast radius is
// lexa-northbound's walker + lexa-telemetry only); lexa-telemetry's MUP POST
// loop is the one caller.
func (f *WolfSSLFetcher) PostContext(ctx context.Context, path string, body []byte, contentType string) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	resp, err := f.doPostFollowing(path, body, contentType)
	if err != nil {
		return nil, "", err
	}
	return postResult(path, resp)
}

// putResult validates a PUT's HTTPResponse and extracts the body. Shared by
// Put and PutContext so the two never drift on what counts as a successful
// PUT. Success is 200, 201, or 204 (WP-3/D3): a PUT writes a full resource
// representation to a path the caller already knows, so unlike postResult
// there is no Location to hand back.
func putResult(path string, resp *HTTPResponse) ([]byte, error) {
	if resp.StatusCode != 200 && resp.StatusCode != 201 && resp.StatusCode != 204 {
		return nil, fmt.Errorf("PUT %s: status %d", path, resp.StatusCode)
	}
	return resp.Body, nil
}

// doPutFollowing is doPostFollowing's PUT sibling: doPut through the shared
// redirect driver, same body/contentType on every hop. f.mu must be held.
func (f *WolfSSLFetcher) doPutFollowing(path string, body []byte, contentType string) (*HTTPResponse, error) {
	return followRedirects("PUT", path, f.client.cfg.RedirectMax, f.client.cfg.ServerAddr,
		func(p string) (*HTTPResponse, error) { return f.doPut(p, body, contentType) })
}

// Put performs a single HTTP PUT over the persistent wolfSSL session and
// returns the response body. Accepts 200, 201, and 204; any other status is
// an error (WP-3/D3 — the DER* reporting verb, no Location dependency).
//
// Put takes the SAME f.mu as Get/Post/GetStatus, so DER* PUTs riding the
// discovery fetcher serialize with the walk and — critically — cert
// rotation via Reload keeps working for free: Reload swaps the session
// under this mutex, so a Put observes either the old or the new session,
// never a torn one. 301/302 redirects are followed within
// Config.RedirectMax hops, same rules as Get (redirect.go).
//
// No ctx parameter, mirroring Post: see PostContext's doc for the split.
func (f *WolfSSLFetcher) Put(path string, body []byte, contentType string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	resp, err := f.doPutFollowing(path, body, contentType)
	if err != nil {
		return nil, err
	}
	return putResult(path, resp)
}

// PutContext is Put with the same between-requests cancellation contract
// documented on Get: ctx is checked once, after acquiring the session mutex
// and before dialing or writing — a canceled ctx never starts a new PUT,
// and does not interrupt one already in flight (nor its redirect hops,
// which are part of the same logical request).
func (f *WolfSSLFetcher) PutContext(ctx context.Context, path string, body []byte, contentType string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	resp, err := f.doPutFollowing(path, body, contentType)
	if err != nil {
		return nil, err
	}
	return putResult(path, resp)
}

// GetStatus performs a GET and returns the raw HTTP status code without
// enforcing that it must be 200. Used by conformance tests that need to
// verify the server correctly returns 404, 405, etc. — which is also why it
// deliberately does NOT follow 301/302 (WP-3): its whole contract is
// observing the raw status, and a conformance check asserting a redirect
// must see the 30x itself, not the post-redirect result. Not part of the
// discovery.Fetcher interface and out of TASK-070's scope — no production
// caller in this repo holds a ctx at its call site today.
func (f *WolfSSLFetcher) GetStatus(path string) (int, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	resp, err := f.doGet(path)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, resp.Body, nil
}
