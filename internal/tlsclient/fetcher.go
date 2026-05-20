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
