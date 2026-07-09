package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TASK-014 (W7, AD-008): /status and /logs require a bearer token once
// api_token_file is configured; an unset token file preserves the historical
// open behavior so the bench doesn't flag-day.

func okHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func TestRequireBearer_EmptyTokenIsOpen(t *testing.T) {
	h := requireBearer("", okHandler)
	req := httptest.NewRequest(http.MethodGet, "/status", nil) // no Authorization header at all
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty token: got %d, want 200 (auth disabled)", rec.Code)
	}
}

func TestRequireBearer_MissingHeaderDenied(t *testing.T) {
	h := requireBearer("s3cret", okHandler)
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing header: got %d, want 401", rec.Code)
	}
}

func TestRequireBearer_WrongTokenDenied(t *testing.T) {
	h := requireBearer("s3cret", okHandler)
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", rec.Code)
	}
}

func TestRequireBearer_MalformedHeaderDenied(t *testing.T) {
	h := requireBearer("s3cret", okHandler)
	for _, hdr := range []string{"s3cret", "Bearer", "bearer s3cret", "Bearer s3cret ", "Basic s3cret"} {
		req := httptest.NewRequest(http.MethodGet, "/status", nil)
		req.Header.Set("Authorization", hdr)
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("header %q: got %d, want 401", hdr, rec.Code)
		}
	}
}

func TestRequireBearer_CorrectTokenAllowed(t *testing.T) {
	h := requireBearer("s3cret", okHandler)
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("correct token: got %d, want 200", rec.Code)
	}
}

// /healthz is never wrapped in main.go — this test pins that healthzHandler
// itself has no auth-awareness, so a future refactor can't accidentally route
// it through requireBearer without a visible diff here.
func TestHealthzHandlerAlwaysOpen(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	healthzHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz: got %d, want 200 unauthenticated", rec.Code)
	}
}

// requireBearerStrict (DEVICE_ROADMAP.md §4.2) is requireBearer's
// write-route counterpart: an empty token must NEVER open the gate, unlike
// requireBearer's staged-rollout escape hatch for reads.
func TestRequireBearerStrict_Table(t *testing.T) {
	cases := []struct {
		name  string
		token string
		hdr   string
		want  int
	}{
		{"empty token, no header, always 401", "", "", http.StatusUnauthorized},
		{"empty token, WITH a header, still 401", "", "Bearer anything", http.StatusUnauthorized},
		{"configured token, missing header", "s3cret", "", http.StatusUnauthorized},
		{"configured token, wrong value", "s3cret", "Bearer wrong", http.StatusUnauthorized},
		{"configured token, malformed header", "s3cret", "s3cret", http.StatusUnauthorized},
		{"configured token, correct value", "s3cret", "Bearer s3cret", http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := requireBearerStrict(c.token, okHandler)
			req := httptest.NewRequest(http.MethodPost, "/intent", nil)
			if c.hdr != "" {
				req.Header.Set("Authorization", c.hdr)
			}
			rec := httptest.NewRecorder()
			h(rec, req)
			if rec.Code != c.want {
				t.Errorf("token=%q hdr=%q: got %d, want %d", c.token, c.hdr, rec.Code, c.want)
			}
		})
	}
}

func TestLoadAPIToken_UnsetFileMeansDisabled(t *testing.T) {
	cfg := &Config{}
	tok, err := cfg.LoadAPIToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "" {
		t.Fatalf("unset api_token_file: got token %q, want empty", tok)
	}
}

func TestLoadAPIToken_ReadsAndTrims(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api.token")
	if err := os.WriteFile(path, []byte("  abc123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{APITokenFile: path}
	tok, err := cfg.LoadAPIToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "abc123" {
		t.Fatalf("got token %q, want %q", tok, "abc123")
	}
}

func TestLoadAPIToken_ConfiguredButMissingFileErrors(t *testing.T) {
	cfg := &Config{APITokenFile: filepath.Join(t.TempDir(), "does-not-exist.token")}
	if _, err := cfg.LoadAPIToken(); err == nil {
		t.Fatal("expected error for configured-but-missing token file, got nil")
	}
}

func TestLoadAPIToken_ConfiguredButEmptyFileErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api.token")
	if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{APITokenFile: path}
	if _, err := cfg.LoadAPIToken(); err == nil {
		t.Fatal("expected error for configured-but-empty token file, got nil")
	}
}
