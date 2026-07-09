package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func writeAPIJSON(t *testing.T, dir, listenAddr string) {
	t.Helper()
	body := fmt.Sprintf(`{"listen_addr":%q}`, listenAddr)
	if err := os.WriteFile(filepath.Join(dir, "api.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckAPI_Pass_HTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(200)
		fmt.Fprintln(w, "ok")
	}))
	defer srv.Close()
	host, port := splitTestAddr(t, srv.URL)

	dir := t.TempDir()
	writeAPIJSON(t, dir, host+":"+port)

	env := &Environment{ConfigDir: dir, HTTPClient: newProbeHTTPClient(), APIScheme: "http"}
	res := checkAPI(context.Background(), env)
	if res.Status != StatusPass {
		t.Fatalf("checkAPI = %+v, want PASS", res)
	}
}

func TestCheckAPI_Pass_HTTPSThenFallback(t *testing.T) {
	// No -api-scheme override: probeGET must try https (fails, plain http
	// server) then fall back to http and still PASS.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	host, port := splitTestAddr(t, srv.URL)

	dir := t.TempDir()
	writeAPIJSON(t, dir, host+":"+port)

	env := &Environment{ConfigDir: dir, HTTPClient: newProbeHTTPClient(), APIScheme: ""}
	res := checkAPI(context.Background(), env)
	if res.Status != StatusPass {
		t.Fatalf("checkAPI = %+v, want PASS via http fallback", res)
	}
}

func TestCheckAPI_Fail_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()
	host, port := splitTestAddr(t, srv.URL)

	dir := t.TempDir()
	writeAPIJSON(t, dir, host+":"+port)

	env := &Environment{ConfigDir: dir, HTTPClient: newProbeHTTPClient(), APIScheme: "http"}
	res := checkAPI(context.Background(), env)
	if res.Status != StatusFail {
		t.Fatalf("checkAPI = %+v, want FAIL on 503", res)
	}
}

func TestCheckAPI_Fail_Unreachable(t *testing.T) {
	dir := t.TempDir()
	writeAPIJSON(t, dir, "127.0.0.1:1") // nothing listens on port 1

	env := &Environment{ConfigDir: dir, HTTPClient: newProbeHTTPClient(), APIScheme: "http"}
	res := checkAPI(context.Background(), env)
	if res.Status != StatusFail {
		t.Fatalf("checkAPI = %+v, want FAIL when unreachable", res)
	}
}

func TestCheckAPI_DefaultsWhenConfigMissing(t *testing.T) {
	dir := t.TempDir() // no api.json at all
	env := &Environment{ConfigDir: dir, HTTPClient: newProbeHTTPClient(), APIScheme: "http"}
	// Expect a FAIL (nothing listening on the default :9100 in the test
	// sandbox), but importantly no error about a missing config file —
	// loadAPIConfig must default cleanly.
	res := checkAPI(context.Background(), env)
	if res.Status != StatusFail {
		t.Fatalf("checkAPI = %+v, want FAIL (unreachable default port), not a config error", res)
	}
}

func TestApiHostPort(t *testing.T) {
	tests := []struct {
		in       string
		wantHost string
		wantPort string
		wantErr  bool
	}{
		{":9100", "127.0.0.1", "9100", false},
		{"0.0.0.0:9100", "127.0.0.1", "9100", false},
		{"127.0.0.1:9100", "127.0.0.1", "9100", false},
		{"192.168.1.5:9100", "192.168.1.5", "9100", false},
		{"not-a-valid-addr", "", "", true},
	}
	for _, tt := range tests {
		host, port, err := apiHostPort(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("apiHostPort(%q) = (%q,%q,nil), want error", tt.in, host, port)
			}
			continue
		}
		if err != nil {
			t.Fatalf("apiHostPort(%q): unexpected error %v", tt.in, err)
		}
		if host != tt.wantHost || port != tt.wantPort {
			t.Errorf("apiHostPort(%q) = (%q,%q), want (%q,%q)", tt.in, host, port, tt.wantHost, tt.wantPort)
		}
	}
}
