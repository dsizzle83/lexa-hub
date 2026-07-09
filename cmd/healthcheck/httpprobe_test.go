package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// splitTestAddr pulls host/port out of an httptest.Server's URL (always
// "http://127.0.0.1:PORT" or "https://127.0.0.1:PORT").
func splitTestAddr(t *testing.T, rawURL string) (host, port string) {
	t.Helper()
	u := strings.TrimPrefix(strings.TrimPrefix(rawURL, "https://"), "http://")
	parts := strings.SplitN(u, ":", 2)
	if len(parts) != 2 {
		t.Fatalf("could not split host:port out of %q", rawURL)
	}
	return parts[0], parts[1]
}

func TestProbeGET_HTTPSFirst(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	host, port := splitTestAddr(t, srv.URL)

	client := newProbeHTTPClient()
	res, err := probeGET(context.Background(), client, "", host, port, "/healthz", nil)
	if err != nil {
		t.Fatalf("probeGET: %v", err)
	}
	if res.Scheme != "https" {
		t.Errorf("scheme = %q, want https", res.Scheme)
	}
	if res.StatusCode != 200 {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
}

func TestProbeGET_FallsBackToHTTP(t *testing.T) {
	// Plain http server: an https attempt against it will fail the TLS
	// handshake (it's not even an HTTPS listener), so probeGET must fall
	// back to http and still succeed — the "pre-TLS units still pass"
	// contract.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	host, port := splitTestAddr(t, srv.URL)

	client := newProbeHTTPClient()
	res, err := probeGET(context.Background(), client, "", host, port, "/healthz", nil)
	if err != nil {
		t.Fatalf("probeGET: %v", err)
	}
	if res.Scheme != "http" {
		t.Errorf("scheme = %q, want http (fallback)", res.Scheme)
	}
	if res.StatusCode != 200 {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
}

func TestProbeGET_ForcedSchemeSkipsFallback(t *testing.T) {
	// A plain http server, forced to "https" — must fail, not silently
	// fall back, since forceScheme is meant to pin exactly one path.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	host, port := splitTestAddr(t, srv.URL)

	client := newProbeHTTPClient()
	_, err := probeGET(context.Background(), client, "https", host, port, "/healthz", nil)
	if err == nil {
		t.Fatalf("probeGET with forceScheme=https against a plain http server should fail, got no error")
	}
}

func TestProbeGET_BothUnreachable(t *testing.T) {
	client := newProbeHTTPClient()
	// Port 1 is never a listening loopback service in any test environment.
	_, err := probeGET(context.Background(), client, "", "127.0.0.1", "1", "/healthz", nil)
	if err == nil {
		t.Fatalf("probeGET against an unreachable host:port should fail")
	}
}

func TestProbeGET_HeadersSent(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer srv.Close()
	host, port := splitTestAddr(t, srv.URL)

	client := newProbeHTTPClient()
	_, err := probeGET(context.Background(), client, "http", host, port, "/status", map[string]string{
		"Authorization": "Bearer sekrit",
	})
	if err != nil {
		t.Fatalf("probeGET: %v", err)
	}
	if gotAuth != "Bearer sekrit" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer sekrit")
	}
}
