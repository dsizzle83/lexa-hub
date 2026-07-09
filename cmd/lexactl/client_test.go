package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_AttachesBearerToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "s3cr3t")
	if _, err := c.get(context.Background(), "/status"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if want := "Bearer s3cr3t"; gotAuth != want {
		t.Errorf("Authorization header = %q, want %q", gotAuth, want)
	}
}

func TestClient_NoTokenMeansNoAuthHeader(t *testing.T) {
	var gotAuth string
	sawHeader := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, sawHeader = r.Header.Get("Authorization"), r.Header.Get("Authorization") != ""
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "")
	if _, err := c.get(context.Background(), "/status"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if sawHeader {
		t.Errorf("expected no Authorization header, got %q", gotAuth)
	}
}

func TestClient_PostEncodesJSONBody(t *testing.T) {
	var gotBody map[string]any
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "")
	resp, err := c.post(context.Background(), "/intent", map[string]any{"kind": "mode"})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	if !bytes.Contains(resp.Body, []byte(`"ok":true`)) {
		t.Errorf("body = %s, want to contain ok:true", resp.Body)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotBody["kind"] != "mode" {
		t.Errorf("decoded body = %v, want kind=mode", gotBody)
	}
}

func TestClient_GetSendsNoBody(t *testing.T) {
	var contentLength int64 = -1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentLength = r.ContentLength
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "")
	if _, err := c.get(context.Background(), "/status"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if contentLength > 0 {
		t.Errorf("GET Content-Length = %d, want 0 or unset", contentLength)
	}
}

func TestClient_TransportErrorIsReported(t *testing.T) {
	c := newClient("http://127.0.0.1:1", "", "", nil) // nothing listens on port 1
	if _, err := c.get(context.Background(), "/status"); err == nil {
		t.Fatal("expected a connection error, got nil")
	}
}

func TestWriteRaw_AddsNewlineOnlyWhenMissing(t *testing.T) {
	var buf bytes.Buffer
	writeRaw(&buf, []byte(`{"a":1}`+"\n"))
	if got := buf.String(); got != `{"a":1}`+"\n" {
		t.Errorf("got %q, want single trailing newline preserved", got)
	}

	buf.Reset()
	writeRaw(&buf, []byte(`{"a":1}`))
	if got := buf.String(); got != `{"a":1}`+"\n" {
		t.Errorf("got %q, want a newline appended", got)
	}
}
