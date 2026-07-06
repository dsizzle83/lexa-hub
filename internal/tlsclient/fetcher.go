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

// postResult validates a POST's HTTPResponse and extracts (body, Location).
// Shared by Post and PostContext so the two never drift on what counts as a
// successful POST.
func postResult(path string, resp *HTTPResponse) ([]byte, string, error) {
	if resp.StatusCode != 201 && resp.StatusCode != 204 {
		return nil, "", fmt.Errorf("POST %s: status %d", path, resp.StatusCode)
	}
	return resp.Body, resp.Location, nil
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

	resp, err := f.doPost(path, body, contentType)
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

	resp, err := f.doPost(path, body, contentType)
	if err != nil {
		return nil, "", err
	}
	return postResult(path, resp)
}

// GetStatus performs a GET and returns the raw HTTP status code without
// enforcing that it must be 200. Used by conformance tests that need to
// verify the server correctly returns 404, 405, etc. Not part of the
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
