package openadr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	// defaultPageLimit is the skip/limit page size used for the paged GETs
	// (3.1 VTNs MUST support skip/limit pagination).
	defaultPageLimit = 50
	// maxPages bounds pagination against a hostile/looping VTN — same
	// fail-closed cap discipline as tlsclient's header/body caps.
	maxPages = 100
	// maxBodyBytes caps any single response body read (per-resource lists
	// are page-limited, so 4 MiB is generous).
	maxBodyBytes = 4 << 20
)

// Client is a minimal OpenADR 3.1 REST client over stdlib net/http. Base is
// the VTN base URL (scheme://host[:port][/prefix], no trailing slash
// required); Tokens is nil for an unauthenticated VTN (3.1 allows plain GETs
// for public tariff programs — a VEN with no client_id configured).
type Client struct {
	Base   string
	HTTP   *http.Client
	Tokens *TokenSource
	// PageLimit overrides defaultPageLimit when > 0 (tests use small pages).
	PageLimit int
}

func (c *Client) pageLimit() int {
	if c.PageLimit > 0 {
		return c.PageLimit
	}
	return defaultPageLimit
}

// httpError is a non-2xx VTN response, exported enough (Status field) for
// callers to special-case 403/404/405 (e.g. EnsureVen's "VTN has no writable
// /vens" tolerance).
type httpError struct {
	Method string
	URL    string
	Status int
	Body   string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("openadr: %s %s: status %d: %s", e.Method, e.URL, e.Status, e.Body)
}

// roundTrip performs one authenticated request with the "refresh on 401
// once" rule: a 401 invalidates the cached token and retries exactly once
// with a fresh one; a second 401 is returned to the caller (whose poll-cycle
// cadence — plus TokenSource's own fetch backoff — is the back-off).
func (c *Client) roundTrip(ctx context.Context, method, path string, q url.Values, body any) ([]byte, error) {
	u := strings.TrimRight(c.Base, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("openadr: marshal %T: %w", body, err)
		}
	}
	for attempt := 0; ; attempt++ {
		var tok string
		if c.Tokens != nil {
			var err error
			tok, err = c.Tokens.Token(ctx)
			if err != nil {
				return nil, err
			}
		}
		var rdr io.Reader
		if payload != nil {
			rdr = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rdr)
		if err != nil {
			return nil, fmt.Errorf("openadr: build %s %s: %w", method, u, err)
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, fmt.Errorf("openadr: %s %s: %w", method, u, err)
		}
		respBody, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		resp.Body.Close()
		if rerr != nil {
			return nil, fmt.Errorf("openadr: %s %s: read body: %w", method, u, rerr)
		}
		if resp.StatusCode == http.StatusUnauthorized && c.Tokens != nil && attempt == 0 {
			c.Tokens.Invalidate(tok)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return nil, &httpError{Method: method, URL: u, Status: resp.StatusCode, Body: truncate(respBody, 200)}
		}
		return respBody, nil
	}
}

// getPaged fetches path with skip/limit pagination until a short page,
// decoding each page as a JSON array of T.
func getPaged[T any](ctx context.Context, c *Client, path string, q url.Values) ([]T, error) {
	limit := c.pageLimit()
	var out []T
	skip := 0
	for page := 0; page < maxPages; page++ {
		qq := url.Values{}
		for k, v := range q {
			qq[k] = v
		}
		qq.Set("skip", strconv.Itoa(skip))
		qq.Set("limit", strconv.Itoa(limit))
		body, err := c.roundTrip(ctx, http.MethodGet, path, qq, nil)
		if err != nil {
			return nil, err
		}
		var items []T
		if err := json.Unmarshal(body, &items); err != nil {
			return nil, fmt.Errorf("openadr: decode GET %s page %d: %w", path, page, err)
		}
		out = append(out, items...)
		if len(items) < limit {
			return out, nil
		}
		skip += limit
	}
	return nil, fmt.Errorf("openadr: GET %s: more than %d pages — refusing (runaway pagination)", path, maxPages)
}

// Programs GETs the full paged /programs list.
func (c *Client) Programs(ctx context.Context) ([]Program, error) {
	return getPaged[Program](ctx, c, "/programs", nil)
}

// Events GETs the full paged /events list for one program.
func (c *Client) Events(ctx context.Context, programID string) ([]Event, error) {
	q := url.Values{}
	q.Set("programID", programID)
	return getPaged[Event](ctx, c, "/events", q)
}

// PostReport POSTs one report to /reports (201 expected; any 2xx accepted).
func (c *Client) PostReport(ctx context.Context, r Report) error {
	if r.ObjectType == "" {
		r.ObjectType = "REPORT"
	}
	_, err := c.roundTrip(ctx, http.MethodPost, "/reports", nil, r)
	return err
}

// EnsureVen implements 3.1 ven-object registration idempotently:
// GET /vens?venName=X first (venName is unique per VTN); only when nothing
// matches does it POST /vens. Returns the venID.
//
// Restart behavior (documented per WP-15): the venID is held in MEMORY only.
// A restarted process re-runs the GET, finds the ven the previous life
// created, and adopts the same ID — no disk persistence needed, because
// venName uniqueness makes registration idempotent. A VTN that would create
// a duplicate on a blind POST is guarded against by the GET-first order.
func (c *Client) EnsureVen(ctx context.Context, venName string) (string, error) {
	q := url.Values{}
	q.Set("venName", venName)
	body, err := c.roundTrip(ctx, http.MethodGet, "/vens", q, nil)
	if err == nil {
		var vens []Ven
		if jerr := json.Unmarshal(body, &vens); jerr == nil {
			for _, v := range vens {
				if v.VenName == venName && v.ID != "" {
					return v.ID, nil
				}
			}
		}
	} else if he, ok := err.(*httpError); !ok || he.Status != http.StatusNotFound {
		// Transport errors and non-404 statuses propagate (403 = scope
		// denied, caller decides whether to skip permanently). A 404 just
		// means "no such ven yet" on VTNs that 404 an empty filter result.
		return "", err
	}
	created, err := c.roundTrip(ctx, http.MethodPost, "/vens", nil, Ven{VenName: venName})
	if err != nil {
		return "", err
	}
	var v Ven
	if err := json.Unmarshal(created, &v); err != nil {
		return "", fmt.Errorf("openadr: decode POST /vens response: %w", err)
	}
	return v.ID, nil
}

// DiscoverTokenURL resolves the VTN's OAuth2 token endpoint via the 3.1
// GET /auth/server resource. The response shape varies across
// implementations, so this is deliberately tolerant: it decodes a generic
// object and takes the first string value under a known key. A configured
// token_url always wins over discovery (cmd/openadr wiring); the final
// fallback is base+"/auth/token", the spec's optional built-in endpoint.
func DiscoverTokenURL(ctx context.Context, httpc *http.Client, base string) (string, error) {
	u := strings.TrimRight(base, "/") + "/auth/server"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return "", fmt.Errorf("openadr: GET %s: %w", u, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", &httpError{Method: http.MethodGet, URL: u, Status: resp.StatusCode, Body: truncate(body, 200)}
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return "", fmt.Errorf("openadr: decode %s: %w", u, err)
	}
	for _, key := range []string{"token_url", "tokenUrl", "token_endpoint", "authServer", "authorizationServer", "url"} {
		if s, ok := m[key].(string); ok && s != "" {
			return s, nil
		}
	}
	return "", fmt.Errorf("openadr: %s response carries no recognizable token endpoint", u)
}
