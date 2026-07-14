package openadr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// tokenEndpoint is a configurable httptest OAuth2 token endpoint.
type tokenEndpoint struct {
	calls    atomic.Int64
	fail     atomic.Bool
	token    atomic.Value // string
	lastForm atomic.Value // url-decoded form snapshot as map
}

func newTokenEndpoint(tok string) *tokenEndpoint {
	te := &tokenEndpoint{}
	te.token.Store(tok)
	return te
}

func (te *tokenEndpoint) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		te.calls.Add(1)
		if te.fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("token POST content-type = %q", ct)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("token POST form parse: %v", err)
		}
		te.lastForm.Store(map[string]string{
			"grant_type":    r.PostFormValue("grant_type"),
			"client_id":     r.PostFormValue("client_id"),
			"client_secret": r.PostFormValue("client_secret"),
		})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": te.token.Load().(string),
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	}
}

// TestTokenObtainAndCache: the first Token() call fetches (correct
// client-credentials form POST), subsequent calls inside the TTL reuse the
// cached token without touching the endpoint, and the refresh hook fires
// exactly once.
func TestTokenObtainAndCache(t *testing.T) {
	te := newTokenEndpoint("tok-A")
	srv := httptest.NewServer(te.handler(t))
	defer srv.Close()

	var refreshes atomic.Int64
	ts := NewTokenSource(srv.Client(), srv.URL, "ven-1", "s3cret", func() { refreshes.Add(1) })

	tok, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "tok-A" {
		t.Fatalf("token = %q, want tok-A", tok)
	}
	form := te.lastForm.Load().(map[string]string)
	if form["grant_type"] != "client_credentials" || form["client_id"] != "ven-1" || form["client_secret"] != "s3cret" {
		t.Fatalf("bad token form: %v", form)
	}
	if !ts.Healthy() {
		t.Fatal("Healthy() = false after successful fetch")
	}
	// Cached: no second endpoint hit.
	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatalf("cached Token: %v", err)
	}
	if n := te.calls.Load(); n != 1 {
		t.Fatalf("token endpoint hit %d times, want 1 (cache)", n)
	}
	if n := refreshes.Load(); n != 1 {
		t.Fatalf("refresh hook fired %d times, want 1", n)
	}
}

// TestClientRefreshesOn401Once: a VTN that 401s the first bearer forces one
// token invalidate+refresh and a single retry carrying the NEW token; the
// request then succeeds.
func TestClientRefreshesOn401Once(t *testing.T) {
	te := newTokenEndpoint("tok-A")
	tokSrv := httptest.NewServer(te.handler(t))
	defer tokSrv.Close()

	var apiCalls atomic.Int64
	var mu sync.Mutex
	var sawTokens []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls.Add(1)
		auth := r.Header.Get("Authorization")
		mu.Lock()
		sawTokens = append(sawTokens, auth)
		mu.Unlock()
		if auth != "Bearer tok-B" {
			// Reject tok-A (revoked mid-lifetime); accept tok-B.
			te.token.Store("tok-B")
			http.Error(w, "expired", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer api.Close()

	ts := NewTokenSource(api.Client(), tokSrv.URL, "ven-1", "s3cret", nil)
	c := &Client{Base: api.URL, HTTP: api.Client(), Tokens: ts}

	if _, err := c.Programs(context.Background()); err != nil {
		t.Fatalf("Programs after 401-refresh: %v", err)
	}
	if n := apiCalls.Load(); n != 2 {
		t.Fatalf("API hit %d times, want 2 (401 then retry)", n)
	}
	mu.Lock()
	seq := append([]string(nil), sawTokens...)
	mu.Unlock()
	if len(seq) != 2 || seq[0] != "Bearer tok-A" || seq[1] != "Bearer tok-B" {
		t.Fatalf("token sequence = %v, want [Bearer tok-A, Bearer tok-B]", seq)
	}
	if n := te.calls.Load(); n != 2 {
		t.Fatalf("token endpoint hit %d times, want 2 (initial + refresh)", n)
	}
}

// TestTokenBackoffAfterFailure: a failed token fetch opens a backoff window
// during which Token() errors WITHOUT touching the endpoint; once the window
// elapses (clock seam) the next call fetches again.
func TestTokenBackoffAfterFailure(t *testing.T) {
	te := newTokenEndpoint("tok-A")
	te.fail.Store(true)
	srv := httptest.NewServer(te.handler(t))
	defer srv.Close()

	now := time.Unix(1752480000, 0)
	ts := NewTokenSource(srv.Client(), srv.URL, "ven-1", "s3cret", nil)
	ts.now = func() time.Time { return now }

	if _, err := ts.Token(context.Background()); err == nil {
		t.Fatal("Token succeeded against a failing endpoint")
	}
	if n := te.calls.Load(); n != 1 {
		t.Fatalf("endpoint hit %d times, want 1", n)
	}
	// Inside the backoff window: error, no network.
	if _, err := ts.Token(context.Background()); err == nil {
		t.Fatal("Token succeeded inside backoff window")
	}
	if n := te.calls.Load(); n != 1 {
		t.Fatalf("endpoint hit %d times inside backoff, want still 1", n)
	}
	// Past the window (first backoff = tokenBackoffBase): fetches again,
	// and the endpoint now works.
	te.fail.Store(false)
	now = now.Add(tokenBackoffBase + time.Second)
	tok, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("Token after backoff elapsed: %v", err)
	}
	if tok != "tok-A" {
		t.Fatalf("token = %q, want tok-A", tok)
	}
	if n := te.calls.Load(); n != 2 {
		t.Fatalf("endpoint hit %d times, want 2", n)
	}
}
