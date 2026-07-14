// Untagged (runs in the plain `go test ./...` suite, like reload_test.go):
// followRedirects is a package-level function over a narrow issue closure,
// so the WP-3/D3 redirect rules — bounded hops, same-host only, never
// scheme-downgrade, 0-disables — get fast, wolfSSL-free coverage here with
// scripted fakes. The wiring (Get/Post/Put routing through the driver) is
// one line per verb in fetcher.go.
package tlsclient

import (
	"errors"
	"strings"
	"testing"
)

const testServerAddr = "69.0.0.20:11111"

// scriptedIssuer returns responses in order, recording every path it was
// asked to issue against. Issuing past the script's end fails the test.
type scriptedIssuer struct {
	t     *testing.T
	resps []*HTTPResponse
	errs  []error // parallel to resps; nil entry = no error
	paths []string
}

func (s *scriptedIssuer) issue(path string) (*HTTPResponse, error) {
	i := len(s.paths)
	s.paths = append(s.paths, path)
	if i >= len(s.resps) {
		s.t.Fatalf("issue called %d times, scripted for %d (paths so far: %v)", i+1, len(s.resps), s.paths)
	}
	if s.errs != nil && s.errs[i] != nil {
		return nil, s.errs[i]
	}
	return s.resps[i], nil
}

func redirectResp(status int, location string) *HTTPResponse {
	return &HTTPResponse{StatusCode: status, Location: location}
}

func okResp() *HTTPResponse {
	return &HTTPResponse{StatusCode: 200, ContentType: "application/sep+xml", Body: []byte("<DCAP/>")}
}

// === followRedirects: the driver ==========================================

func TestFollowRedirects_SingleHop(t *testing.T) {
	for _, status := range []int{301, 302} {
		s := &scriptedIssuer{t: t, resps: []*HTTPResponse{redirectResp(status, "/dcap-v2"), okResp()}}

		resp, err := followRedirects("GET", "/dcap", 3, testServerAddr, s.issue)
		if err != nil {
			t.Fatalf("status %d: followRedirects: %v", status, err)
		}
		if resp.StatusCode != 200 {
			t.Errorf("status %d: final StatusCode = %d, want 200", status, resp.StatusCode)
		}
		wantPaths := []string{"/dcap", "/dcap-v2"}
		if len(s.paths) != len(wantPaths) || s.paths[0] != wantPaths[0] || s.paths[1] != wantPaths[1] {
			t.Errorf("status %d: issued paths = %v, want %v", status, s.paths, wantPaths)
		}
	}
}

func TestFollowRedirects_AbsoluteSameHost(t *testing.T) {
	s := &scriptedIssuer{t: t, resps: []*HTTPResponse{
		redirectResp(302, "https://"+testServerAddr+"/edev-v2"),
		okResp(),
	}}

	resp, err := followRedirects("GET", "/edev", 3, testServerAddr, s.issue)
	if err != nil {
		t.Fatalf("followRedirects: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("final StatusCode = %d, want 200", resp.StatusCode)
	}
	if s.paths[1] != "/edev-v2" {
		t.Errorf("re-issued path = %q, want /edev-v2 (host stripped)", s.paths[1])
	}
}

func TestFollowRedirects_HopLimitExceeded(t *testing.T) {
	const max = 3
	// initial + max follows, every one a redirect: the driver must error
	// out rather than issue a (max+2)th request.
	resps := make([]*HTTPResponse, max+1)
	for i := range resps {
		resps[i] = redirectResp(302, "/loop")
	}
	s := &scriptedIssuer{t: t, resps: resps}

	_, err := followRedirects("GET", "/dcap", max, testServerAddr, s.issue)
	if err == nil {
		t.Fatal("expected redirect-limit error, got nil")
	}
	if !strings.Contains(err.Error(), "redirect limit 3 exceeded") {
		t.Errorf("error = %v, want it to mention the redirect limit", err)
	}
	if len(s.paths) != max+1 {
		t.Errorf("issued %d requests, want exactly %d (initial + %d follows)", len(s.paths), max+1, max)
	}
}

func TestFollowRedirects_CrossHostRefused(t *testing.T) {
	s := &scriptedIssuer{t: t, resps: []*HTTPResponse{
		redirectResp(302, "https://evil.example:11111/dcap"),
	}}

	_, err := followRedirects("GET", "/dcap", 3, testServerAddr, s.issue)
	if err == nil {
		t.Fatal("expected cross-host redirect to be refused, got nil error")
	}
	if !strings.Contains(err.Error(), "same-host only") {
		t.Errorf("error = %v, want it to mention same-host only", err)
	}
	if len(s.paths) != 1 {
		t.Errorf("issued %d requests, want 1 — a refused Location must never be re-issued", len(s.paths))
	}
}

func TestFollowRedirects_SchemeDowngradeRefused(t *testing.T) {
	s := &scriptedIssuer{t: t, resps: []*HTTPResponse{
		redirectResp(301, "http://"+testServerAddr+"/dcap"),
	}}

	_, err := followRedirects("GET", "/dcap", 3, testServerAddr, s.issue)
	if err == nil {
		t.Fatal("expected scheme-downgrade redirect to be refused, got nil error")
	}
	if !strings.Contains(err.Error(), "downgrade") {
		t.Errorf("error = %v, want it to mention the downgrade", err)
	}
	if len(s.paths) != 1 {
		t.Errorf("issued %d requests, want 1", len(s.paths))
	}
}

func TestFollowRedirects_ZeroDisables(t *testing.T) {
	for _, max := range []int{0, -1} {
		s := &scriptedIssuer{t: t, resps: []*HTTPResponse{redirectResp(302, "/dcap-v2")}}

		resp, err := followRedirects("GET", "/dcap", max, testServerAddr, s.issue)
		if err != nil {
			t.Fatalf("max %d: followRedirects: %v (disabled following must pass the 30x through, not error)", max, err)
		}
		if resp.StatusCode != 302 {
			t.Errorf("max %d: StatusCode = %d, want the raw 302 back", max, resp.StatusCode)
		}
		if len(s.paths) != 1 {
			t.Errorf("max %d: issued %d requests, want exactly 1", max, len(s.paths))
		}
	}
}

func TestFollowRedirects_NonRedirectPassthrough(t *testing.T) {
	// 200 and the not-followed 3xx family: exactly one issue, response
	// returned untouched (the caller's status check owns rejecting 303+).
	for _, status := range []int{200, 204, 303, 307, 308, 404} {
		s := &scriptedIssuer{t: t, resps: []*HTTPResponse{{StatusCode: status, Location: "/elsewhere"}}}

		resp, err := followRedirects("GET", "/dcap", 3, testServerAddr, s.issue)
		if err != nil {
			t.Fatalf("status %d: followRedirects: %v", status, err)
		}
		if resp.StatusCode != status {
			t.Errorf("StatusCode = %d, want %d untouched", resp.StatusCode, status)
		}
		if len(s.paths) != 1 {
			t.Errorf("status %d: issued %d requests, want 1", status, len(s.paths))
		}
	}
}

func TestFollowRedirects_IssueErrorPropagates(t *testing.T) {
	wantErr := errors.New("connection reset")
	s := &scriptedIssuer{t: t,
		resps: []*HTTPResponse{redirectResp(302, "/dcap-v2"), nil},
		errs:  []error{nil, wantErr},
	}

	_, err := followRedirects("GET", "/dcap", 3, testServerAddr, s.issue)
	if !errors.Is(err, wantErr) {
		t.Errorf("error = %v, want the hop's transport error to propagate", err)
	}
}

// === resolveRedirectLocation: the fail-closed rules =======================

func TestResolveRedirectLocation(t *testing.T) {
	cases := []struct {
		name     string
		location string
		wantPath string // "" means an error is expected
		wantErr  string // substring the error must contain
	}{
		{"path only", "/dcap-v2", "/dcap-v2", ""},
		{"path with query", "/derp/0/derc?l=10", "/derp/0/derc?l=10", ""},
		{"absolute same host", "https://" + testServerAddr + "/dcap-v2", "/dcap-v2", ""},
		{"absolute same host case-insensitive scheme", "HTTPS://" + testServerAddr + "/dcap-v2", "/dcap-v2", ""},
		{"root path", "https://" + testServerAddr + "/", "/", ""},

		{"empty", "", "", "Location missing"},
		{"cross host", "https://evil.example:11111/dcap", "", "same-host only"},
		{"cross port", "https://69.0.0.20:443/dcap", "", "same-host only"},
		{"scheme downgrade", "http://" + testServerAddr + "/dcap", "", "downgrade"},
		{"scheme relative", "//evil.example/dcap", "", "scheme-relative"},
		{"no path component", "https://" + testServerAddr, "", "no path component"},
		{"other scheme", "ftp://" + testServerAddr + "/dcap", "", "not a path or https URL"},
		{"relative path", "dcap-v2", "", "not a path or https URL"},
		{"CR injection", "/dcap\r\nX-Evil: 1", "", "invalid redirect Location"},
		{"LF injection", "/dcap\nX-Evil: 1", "", "invalid redirect Location"},
		{"space smuggling", "/dcap HTTP/1.1", "", "invalid redirect Location"},
		{"overlong", "/" + strings.Repeat("a", maxRedirectLocation), "", "too long"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveRedirectLocation(tc.location, testServerAddr)
			if tc.wantPath != "" {
				if err != nil {
					t.Fatalf("resolveRedirectLocation(%q): %v", tc.location, err)
				}
				if got != tc.wantPath {
					t.Errorf("path = %q, want %q", got, tc.wantPath)
				}
				return
			}
			if err == nil {
				t.Fatalf("resolveRedirectLocation(%q) = %q, want error", tc.location, got)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestResolveRedirectLocation_LengthCapBoundary pins the cap's off-by-one:
// exactly maxRedirectLocation bytes is still accepted, one more is not.
func TestResolveRedirectLocation_LengthCapBoundary(t *testing.T) {
	atCap := "/" + strings.Repeat("a", maxRedirectLocation-1)
	if _, err := resolveRedirectLocation(atCap, testServerAddr); err != nil {
		t.Errorf("Location of exactly %d bytes rejected: %v", maxRedirectLocation, err)
	}
	overCap := "/" + strings.Repeat("a", maxRedirectLocation)
	if _, err := resolveRedirectLocation(overCap, testServerAddr); err == nil {
		t.Errorf("Location of %d bytes accepted, want error", maxRedirectLocation+1)
	}
}
