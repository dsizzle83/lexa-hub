package tlsclient

import (
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

// Get satisfies discovery.Fetcher. Returns the response body on 200 with
// Content-Type application/sep+xml; any other status/type is an error.
func (f *WolfSSLFetcher) Get(path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	resp, err := f.doGet(path)
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

// Post performs a single HTTP POST over the persistent wolfSSL session.
// Returns the response body and Location header (for 201 Created).
// Accepts 201 and 204; any other status is an error.
func (f *WolfSSLFetcher) Post(path string, body []byte, contentType string) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	resp, err := f.doPost(path, body, contentType)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != 201 && resp.StatusCode != 204 {
		return nil, "", fmt.Errorf("POST %s: status %d", path, resp.StatusCode)
	}
	return resp.Body, resp.Location, nil
}

// GetStatus performs a GET and returns the raw HTTP status code without
// enforcing that it must be 200. Used by conformance tests that need to
// verify the server correctly returns 404, 405, etc.
func (f *WolfSSLFetcher) GetStatus(path string) (int, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	resp, err := f.doGet(path)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, resp.Body, nil
}
