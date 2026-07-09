package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
)

// newProbeHTTPClient returns an *http.Client suitable ONLY for loopback
// liveness probes. InsecureSkipVerify is deliberate and narrowly scoped:
// this client never dials anything but 127.0.0.1/localhost, and every call
// site exists to answer "is something listening and responding correctly
// on this port right now", never "is this the box I think it is". CSIP
// identity verification (wolfSSL, ECDHE-ECDSA-AES128-CCM-8, LFDI) is a
// completely separate client (internal/tlsclient) this tool never touches
// or substitutes for.
func newProbeHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // loopback liveness probe only, see doc above
		},
	}
}

// probeResult is what a scheme-fallback GET returns on success.
type probeResult struct {
	Scheme     string // "https" | "http" — whichever scheme actually answered
	StatusCode int
	Body       []byte
}

// probeGET issues a GET to host:port+path, trying https first and falling
// back to http on ANY error (connection refused, TLS handshake failure,
// timeout) — the "pre-TLS units still pass" contract lexa-api's HTTPS
// rollout (TASK-088, landing in a parallel unit) needs: this tool must work
// identically whether or not a given box's api.json has flipped its `tls`
// key on yet.
//
// forceScheme, if non-empty, skips the fallback and only tries that one
// scheme — wired to -api-scheme for an operator who already knows which
// scheme is live, and used by tests that want to pin one path without
// exercising the fallback.
func probeGET(ctx context.Context, client *http.Client, forceScheme, host, port, path string, headers map[string]string) (probeResult, error) {
	schemes := []string{"https", "http"}
	if forceScheme != "" {
		schemes = []string{forceScheme}
	}

	var lastErr error
	for _, scheme := range schemes {
		url := fmt.Sprintf("%s://%s:%s%s", scheme, host, port, path)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			lastErr = err
			continue
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return probeResult{Scheme: scheme, StatusCode: resp.StatusCode, Body: body}, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no scheme attempted")
	}
	return probeResult{}, lastErr
}
