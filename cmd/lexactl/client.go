package main

// client.go is the ONLY place in this program that speaks HTTP to lexa-api.
// Every subcommand routes through client's methods — there is no second path
// into the hub (DEVICE_ROADMAP.md §7: "nothing bypasses the intent
// journal"). Kept deliberately dumb: it does not interpret response bodies
// beyond status code + raw bytes — each subcommand decodes what it needs
// into its own local struct (the cmd/healthcheck "hand-copied decode
// structs, package-main import wall" pattern — see that package's 1.5
// review note — applied here too, so this stdlib-only CLI never imports
// internal/bus or cmd/api).
import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// clientTimeout bounds every single HTTP round trip this program makes
// (connect + TLS handshake + request + response). A CLI invocation must
// never hang indefinitely against a dark or wedged lexa-api.
const clientTimeout = 10 * time.Second

// client wraps an *http.Client with the base URL and bearer token every
// route needs. certFile is carried alongside purely so `lexactl fingerprint`
// (which needs no network at all) fits the same "subcommand funcs take
// (*client, args, stdout)" signature as every other subcommand.
type client struct {
	http     *http.Client
	base     string // e.g. "https://127.0.0.1:9100" — no trailing slash
	token    string
	certFile string
}

// newClient builds a client ready to use. tlsConfig is nil for a plain
// http:// base (see trust.go's resolveTrust) — http.Transport's zero
// TLSClientConfig is the correct "use Go's normal defaults" value in that
// case, and is simply never consulted for a non-TLS dial.
func newClient(base, token, certFile string, tlsConfig *tls.Config) *client {
	return &client{
		http: &http.Client{
			Timeout:   clientTimeout,
			Transport: &http.Transport{TLSClientConfig: tlsConfig},
		},
		base:     strings.TrimRight(base, "/"),
		token:    token,
		certFile: certFile,
	}
}

// apiResponse is one HTTP round trip's outcome: status code + raw body.
// Subcommands decode Body themselves (or print it verbatim for -json).
type apiResponse struct {
	Status int
	Body   []byte
}

// request issues method against base+path. body, if non-nil, is JSON-encoded
// as the request body (json.RawMessage passes through verbatim — see
// intent_report.go's postIntent, which relies on that for the `intent`
// escape hatch). The bearer token, if set, rides on every request — read
// routes tolerate its absence (the staged-rollout empty-token-open default,
// mirrored from cmd/api/auth.go's requireBearer); write routes enforce it
// server-side (requireBearerStrict) and simply 401 when it's missing or
// wrong, which is a response this function reports like any other, not an
// error it need special-case.
func (c *client) request(ctx context.Context, method, path string, body any) (apiResponse, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return apiResponse{}, fmt.Errorf("encode request body: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return apiResponse{}, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return apiResponse{}, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return apiResponse{}, fmt.Errorf("%s %s: read response body: %w", method, path, err)
	}
	return apiResponse{Status: resp.StatusCode, Body: b}, nil
}

func (c *client) get(ctx context.Context, path string) (apiResponse, error) {
	return c.request(ctx, http.MethodGet, path, nil)
}

func (c *client) post(ctx context.Context, path string, body any) (apiResponse, error) {
	return c.request(ctx, http.MethodPost, path, body)
}

// writeRaw prints body to stdout exactly as received (the -json / escape-
// hatch "verbatim" contract), adding a trailing newline only if body doesn't
// already end with one — every lexa-api handler in this codebase (writeJSON,
// http.Error, json.Encoder) already terminates its body with "\n", so this
// is a defensive backstop, not the common case.
func writeRaw(stdout io.Writer, body []byte) {
	stdout.Write(body) //nolint:errcheck // best-effort CLI output
	if len(body) == 0 || body[len(body)-1] != '\n' {
		fmt.Fprintln(stdout)
	}
}
