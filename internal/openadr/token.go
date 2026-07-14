package openadr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Token-fetch backoff bounds: after a failed token POST, no re-attempt is
// made for tokenBackoffBase × 2^(failures-1), capped at tokenBackoffMax —
// "refresh on 401 once then backoff": one immediate refresh per request
// cycle (client.go's 401 retry), then this source refuses to hammer the
// token endpoint until the window elapses.
const (
	tokenBackoffBase = 5 * time.Second
	tokenBackoffMax  = 5 * time.Minute

	// tokenExpirySlack is subtracted from the endpoint's expires_in so a
	// token is refreshed shortly BEFORE the VTN would start 401ing it.
	tokenExpirySlack = 30 * time.Second

	// tokenDefaultTTL is assumed when the endpoint omits expires_in.
	tokenDefaultTTL = 5 * time.Minute
)

// TokenSource implements the OAuth2 client-credentials flow (3.1's only
// VEN auth model) with a hand-rolled form POST — deliberately no oauth2
// library (repo dependency posture; the flow is one request).
//
// Concurrency: safe for concurrent use, though the only production caller
// is cmd/openadr's single poll goroutine.
type TokenSource struct {
	httpc        *http.Client
	tokenURL     string
	clientID     string
	clientSecret string

	// onRefresh, when non-nil, is called after every SUCCESSFUL token fetch
	// (lexa_openadr_token_refresh_total's hook — a function value, not a
	// metrics import, same decoupling as mqttutil.Instrumentation).
	onRefresh func()

	// now is the clock seam for tests; time.Now in production.
	now func() time.Time

	mu       sync.Mutex
	token    string
	expiry   time.Time
	failures int
	nextTry  time.Time
	lastErr  error
}

// NewTokenSource constructs a TokenSource. onRefresh may be nil.
func NewTokenSource(httpc *http.Client, tokenURL, clientID, clientSecret string, onRefresh func()) *TokenSource {
	return &TokenSource{
		httpc:        httpc,
		tokenURL:     tokenURL,
		clientID:     clientID,
		clientSecret: clientSecret,
		onRefresh:    onRefresh,
		now:          time.Now,
	}
}

// Token returns a currently-valid bearer token, fetching a new one from the
// token endpoint when the cached one is absent/expired/invalidated. During a
// failure-backoff window it returns the last fetch error immediately without
// touching the network.
func (t *TokenSource) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if t.token != "" && now.Before(t.expiry) {
		return t.token, nil
	}
	if now.Before(t.nextTry) {
		return "", fmt.Errorf("openadr: token fetch backing off until %s after %d failure(s): %w",
			t.nextTry.Format(time.RFC3339), t.failures, t.lastErr)
	}
	tok, ttl, err := t.fetch(ctx)
	if err != nil {
		t.failures++
		backoff := tokenBackoffBase << (t.failures - 1)
		if backoff > tokenBackoffMax || backoff <= 0 {
			backoff = tokenBackoffMax
		}
		t.nextTry = now.Add(backoff)
		t.lastErr = err
		return "", err
	}
	t.failures = 0
	t.nextTry = time.Time{}
	t.lastErr = nil
	t.token = tok
	t.expiry = now.Add(ttl)
	if t.onRefresh != nil {
		t.onRefresh()
	}
	return t.token, nil
}

// Invalidate drops the cached token if it is still the one the caller was
// rejected with (a 401 mid-lifetime — revoked or clock-skewed), so the next
// Token call fetches fresh. A token already replaced is left alone.
func (t *TokenSource) Invalidate(tok string) {
	t.mu.Lock()
	if t.token == tok {
		t.token = ""
		t.expiry = time.Time{}
	}
	t.mu.Unlock()
}

// Healthy reports whether the source currently holds an unexpired token —
// the bus.OpenADRStatus TokenOK input. A source that has never fetched
// reports false until the first successful poll obtains one.
func (t *TokenSource) Healthy() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.token != "" && t.now().Before(t.expiry)
}

// fetch performs the client-credentials form POST. Called with t.mu held
// (the poll loop is single-flight; holding the lock across the bounded HTTP
// round trip is deliberate simplicity, not contention).
func (t *TokenSource) fetch(ctx context.Context) (token string, ttl time.Duration, err error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", t.clientID)
	form.Set("client_secret", t.clientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("openadr: token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.httpc.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("openadr: token POST %s: %w", t.tokenURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, fmt.Errorf("openadr: token response read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("openadr: token POST %s: status %d: %s", t.tokenURL, resp.StatusCode, truncate(body, 200))
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", 0, fmt.Errorf("openadr: token response decode: %w", err)
	}
	if tr.AccessToken == "" {
		return "", 0, fmt.Errorf("openadr: token response missing access_token")
	}
	ttl = tokenDefaultTTL
	if tr.ExpiresIn > 0 {
		ttl = time.Duration(tr.ExpiresIn) * time.Second
	}
	ttl -= tokenExpirySlack
	if ttl < tokenExpirySlack {
		ttl = tokenExpirySlack
	}
	return tr.AccessToken, ttl, nil
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}
