package main

// diag_test.go: unit 2.5/§2.8 — the diag bundle command. Covers the include
// matrix, filename exclusion + JSON key redaction, the 32MiB size-bound
// abort, https-only URL validation, upload via httptest.NewTLSServer, the
// 1-per-5min rate limit under a fake clock, and end-to-end through
// downlink.process (journal request+outcome, never a bus publish).

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lexa-hub/internal/journal"
)

// -----------------------------------------------------------------------------
// validation.
// -----------------------------------------------------------------------------

func TestValidateDiagRequest(t *testing.T) {
	cases := []struct {
		name       string
		req        diagRequest
		wantReason string
	}{
		{"https ok", diagRequest{UploadURL: "https://bucket.s3.example/presigned?sig=x", Include: []string{"journal"}}, ""},
		{"all includes ok", diagRequest{UploadURL: "https://x.example/y", Include: []string{"journal", "snapshot", "config"}}, ""},
		{"empty include ok (empty bundle)", diagRequest{UploadURL: "https://x.example/y"}, ""},
		{"http rejected", diagRequest{UploadURL: "http://x.example/y", Include: []string{"journal"}}, "https-required"},
		{"empty url rejected", diagRequest{UploadURL: ""}, "https-required"},
		{"garbage url rejected", diagRequest{UploadURL: "::::not a url"}, "https-required"},
		{"ftp rejected", diagRequest{UploadURL: "ftp://x.example/y"}, "https-required"},
		{"unknown include rejected", diagRequest{UploadURL: "https://x.example/y", Include: []string{"journal", "shadow"}}, "bad-include"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, _ := validateDiagRequest(tc.req)
			if reason != tc.wantReason {
				t.Errorf("validateDiagRequest = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// redaction rules.
// -----------------------------------------------------------------------------

func TestIsSensitiveFilename(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"cloudlink.pass", true},
		{"mqtt_pass_file", true},
		{"PASSWD", true}, // case-insensitive
		{"api-secret", true},
		{"secrets.json", true},
		{"server.key", true},
		{"client-key.pem", true},
		{"cloud-key.pem", true},
		{"ca.pem", false},
		{"client.pem", false}, // cert, not key material
		{"hub.json", false},
		{"monkey.json", false}, // contains "key" but neither .key nor key*.pem
		{"journal.ndjson", false},
	}
	for _, tc := range cases {
		if got := isSensitiveFilename(tc.name); got != tc.want {
			t.Errorf("isSensitiveFilename(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRedactJSONBytes(t *testing.T) {
	in := []byte(`{
		"mqtt_broker": "tcp://localhost:1883",
		"mqtt_pass_file": "/etc/lexa/mqtt/hub.pass",
		"api_token_file": "/etc/lexa/api-secret",
		"Password": "hunter2",
		"nested": {"basic_auth_pass": "x", "port": 8883, "SECRET_SEED": 42},
		"list": [{"token": "abc"}, {"plain": "keep"}],
		"plain": "keep-me"
	}`)
	out, err := redactJSONBytes(in)
	if err != nil {
		t.Fatalf("redactJSONBytes: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("re-parse redacted: %v", err)
	}
	// Keys matching (pass|secret|token), any case, any nesting → placeholder.
	if m["mqtt_pass_file"] != redactedPlaceholder {
		t.Errorf("mqtt_pass_file = %v, want redacted", m["mqtt_pass_file"])
	}
	if m["api_token_file"] != redactedPlaceholder {
		t.Errorf("api_token_file = %v, want redacted", m["api_token_file"])
	}
	if m["Password"] != redactedPlaceholder {
		t.Errorf("Password = %v, want redacted (case-insensitive)", m["Password"])
	}
	nested := m["nested"].(map[string]any)
	if nested["basic_auth_pass"] != redactedPlaceholder || nested["SECRET_SEED"] != redactedPlaceholder {
		t.Errorf("nested secrets not redacted: %v", nested)
	}
	if nested["port"] != float64(8883) {
		t.Errorf("non-secret nested value changed: %v", nested["port"])
	}
	list := m["list"].([]any)
	if list[0].(map[string]any)["token"] != redactedPlaceholder {
		t.Errorf("token inside array not redacted: %v", list[0])
	}
	if list[1].(map[string]any)["plain"] != "keep" || m["plain"] != "keep-me" || m["mqtt_broker"] != "tcp://localhost:1883" {
		t.Error("non-secret values were altered")
	}

	if _, err := redactJSONBytes([]byte(`{not json`)); err == nil {
		t.Error("unparsable JSON must return an error (caller skips+notes)")
	}
}

// -----------------------------------------------------------------------------
// build: include matrix + exclusion + redaction inside the tar.
// -----------------------------------------------------------------------------

// newDiagFixture lays out a realistic on-disk tree and returns the builder.
func newDiagFixture(t *testing.T) (*diagBuilder, diagPaths) {
	t.Helper()
	root := t.TempDir()
	paths := diagPaths{
		JournalDir:  filepath.Join(root, "journal"),
		SnapshotDir: filepath.Join(root, "snapshot"),
		ConfigGlob:  filepath.Join(root, "etc", "*.json"),
	}
	mustWrite := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("journal/cloudlink/journal.ndjson", `{"v":1,"type":"intent_received"}`)
	mustWrite("journal/hub/journal.ndjson", `{"v":1,"type":"dispatch"}`)
	mustWrite("journal/hub/rotate.pass", "MUST NOT APPEAR") // sensitive name inside journal tree
	mustWrite("snapshot/state.json", `{"soc":55}`)
	mustWrite("etc/hub.json", `{"mqtt_pass_file":"/etc/lexa/mqtt/hub.pass","engine_interval_s":15}`)
	mustWrite("etc/secrets.json", `{"k":"v"}`)     // excluded by NAME despite matching the glob
	mustWrite("etc/broken.json", `{not json`)      // unparsable → skipped+noted
	mustWrite("etc/api-secret", "token")           // not *.json → never globbed anyway
	mustWrite("etc/client-key.pem", "PRIVATE KEY") // not *.json → never globbed anyway
	return newDiagBuilder(paths), paths
}

// readTarGz returns name → contents for every entry in the bundle at path.
func readTarGz(t *testing.T, path string) map[string][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar read %s: %v", hdr.Name, err)
		}
		out[hdr.Name] = data
	}
	return out
}

func TestDiagBuild_IncludeMatrix(t *testing.T) {
	cases := []struct {
		name        string
		include     []string
		wantEntries []string
	}{
		{"journal only", []string{"journal"},
			[]string{"journal/cloudlink/journal.ndjson", "journal/hub/journal.ndjson"}},
		{"snapshot only", []string{"snapshot"},
			[]string{"snapshot/state.json"}},
		{"config only", []string{"config"},
			[]string{"config/hub.json", "config/broken-is-skipped-sentinel"}}, // broken.json handled below
		{"all three", []string{"journal", "snapshot", "config"},
			[]string{"journal/cloudlink/journal.ndjson", "journal/hub/journal.ndjson", "snapshot/state.json", "config/hub.json"}},
		{"empty include -> empty bundle", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := newDiagFixture(t)
			path, n, skipped, err := b.build(tc.include)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			defer os.Remove(path)
			entries := readTarGz(t, path)

			want := map[string]bool{}
			for _, e := range tc.wantEntries {
				if e == "config/broken-is-skipped-sentinel" {
					continue // marker, not a real expectation
				}
				want[e] = true
			}
			for e := range want {
				if _, ok := entries[e]; !ok {
					t.Errorf("bundle missing %s (has: %v)", e, keys(entries))
				}
			}
			for e := range entries {
				if !want[e] {
					t.Errorf("bundle has unexpected entry %s", e)
				}
			}
			if n != len(want) {
				t.Errorf("build reported %d files, want %d", n, len(want))
			}

			// The sensitive-name journal file must never appear regardless of set.
			for e := range entries {
				if strings.Contains(e, "rotate.pass") || strings.Contains(e, "secrets.json") ||
					strings.Contains(e, "api-secret") || strings.Contains(e, "key.pem") {
					t.Errorf("SENSITIVE file leaked into bundle: %s", e)
				}
			}

			if hasInclude(tc.include, "journal") {
				assertSkippedContains(t, skipped, "rotate.pass")
			}
			if hasInclude(tc.include, "config") {
				assertSkippedContains(t, skipped, "secrets.json")
				assertSkippedContains(t, skipped, "broken.json")
			}
		})
	}
}

func hasInclude(include []string, s string) bool {
	for _, i := range include {
		if i == s {
			return true
		}
	}
	return false
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func assertSkippedContains(t *testing.T, skipped []string, needle string) {
	t.Helper()
	for _, s := range skipped {
		if strings.Contains(s, needle) {
			return
		}
	}
	t.Errorf("skipped list %v does not note %s", skipped, needle)
}

func TestDiagBuild_ConfigRedactedInsideTar(t *testing.T) {
	b, _ := newDiagFixture(t)
	path, _, _, err := b.build([]string{"config"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer os.Remove(path)
	entries := readTarGz(t, path)
	data, ok := entries["config/hub.json"]
	if !ok {
		t.Fatalf("config/hub.json missing from bundle: %v", keys(entries))
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("bundled config not valid JSON: %v", err)
	}
	if m["mqtt_pass_file"] != redactedPlaceholder {
		t.Errorf("mqtt_pass_file in bundle = %v, want redacted", m["mqtt_pass_file"])
	}
	if m["engine_interval_s"] != float64(15) {
		t.Errorf("non-secret key altered: %v", m["engine_interval_s"])
	}
	if bytes.Contains(data, []byte("hub.pass")) {
		t.Error("raw secret value survived redaction")
	}
	// Journal content, by contrast, rides verbatim (no JSON re-marshal).
	path2, _, _, err := b.build([]string{"journal"})
	if err != nil {
		t.Fatalf("build journal: %v", err)
	}
	defer os.Remove(path2)
	j := readTarGz(t, path2)
	if string(j["journal/cloudlink/journal.ndjson"]) != `{"v":1,"type":"intent_received"}` {
		t.Error("journal content not verbatim")
	}
}

func TestDiagBuild_MissingSectionDirIsEmptyNotError(t *testing.T) {
	b := newDiagBuilder(diagPaths{
		JournalDir:  filepath.Join(t.TempDir(), "does-not-exist"),
		SnapshotDir: filepath.Join(t.TempDir(), "also-missing"),
		ConfigGlob:  filepath.Join(t.TempDir(), "*.json"),
	})
	path, n, _, err := b.build([]string{"journal", "snapshot", "config"})
	if err != nil {
		t.Fatalf("build with missing dirs errored: %v", err)
	}
	defer os.Remove(path)
	if n != 0 {
		t.Errorf("included %d files from nonexistent dirs", n)
	}
}

func TestDiagBuild_SizeBoundAbort(t *testing.T) {
	root := t.TempDir()
	jdir := filepath.Join(root, "journal")
	if err := os.MkdirAll(jdir, 0o755); err != nil {
		t.Fatal(err)
	}
	// One 40MiB incompressible file: gzip output > 32MiB bound.
	big := make([]byte, 40<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jdir, "huge.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}

	before := countDiagTemps(t)
	b := newDiagBuilder(diagPaths{JournalDir: jdir, SnapshotDir: root, ConfigGlob: filepath.Join(root, "*.json")})
	_, _, _, err := b.build([]string{"journal"})
	if err == nil {
		t.Fatal("build succeeded past the 32MiB bound, want stop+error")
	}
	if !errors.Is(err, errDiagBundleTooLarge) {
		t.Errorf("err = %v, want errDiagBundleTooLarge", err)
	}
	if after := countDiagTemps(t); after != before {
		t.Errorf("aborted build leaked a temp file (%d -> %d)", before, after)
	}
}

// countDiagTemps counts lexa-diag-* files in the OS temp dir.
func countDiagTemps(t *testing.T) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "lexa-diag-*"))
	if err != nil {
		t.Fatal(err)
	}
	return len(matches)
}

// -----------------------------------------------------------------------------
// upload.
// -----------------------------------------------------------------------------

func TestDiagUpload_PUTToPresignedURL(t *testing.T) {
	var gotMethod string
	var gotLen int64
	var gotBody []byte
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotLen = r.ContentLength
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	f := filepath.Join(t.TempDir(), "bundle.tar.gz")
	content := []byte("fake-bundle-bytes")
	if err := os.WriteFile(f, content, 0o600); err != nil {
		t.Fatal(err)
	}

	b := newDiagBuilder(defaultDiagPaths())
	b.client = ts.Client() // trusts the httptest self-signed cert
	if err := b.upload(ts.URL, f); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %s, want PUT", gotMethod)
	}
	if gotLen != int64(len(content)) {
		t.Errorf("Content-Length = %d, want %d (presigned PUTs are signed over an exact byte count)", gotLen, len(content))
	}
	if !bytes.Equal(gotBody, content) {
		t.Error("uploaded body does not match the bundle file")
	}
}

func TestDiagUpload_Non2xxIsError(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden) // expired presigned URL
	}))
	defer ts.Close()

	f := filepath.Join(t.TempDir(), "b.tar.gz")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := newDiagBuilder(defaultDiagPaths())
	b.client = ts.Client()
	if err := b.upload(ts.URL, f); err == nil {
		t.Error("403 upload returned nil error")
	}
}

// -----------------------------------------------------------------------------
// end-to-end through downlink.process: dispatch, journal, rate limit.
// -----------------------------------------------------------------------------

func TestProcessDiag_EndToEnd(t *testing.T) {
	var uploads int
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		uploads++
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	dir := t.TempDir()
	jw, err := journal.Open(journal.Config{Dir: dir, FlushEvery: 1})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	defer jw.Close()

	fx := newTestDownlink(t, jw)
	b, _ := newDiagFixture(t)
	b.client = ts.Client()
	fx.dl.diag = b

	payload := cmdPayload(t, 1, "diag", map[string]any{
		"upload_url": ts.URL,
		"include":    []string{"journal", "config"},
	})

	kind, reason := fx.dl.process(payload)
	if kind != "diag" || reason != "" {
		t.Fatalf("process(diag) = (%q, %q), want (diag, \"\")", kind, reason)
	}
	if uploads != 1 {
		t.Fatalf("uploads = %d, want 1", uploads)
	}
	// Diag NEVER publishes to the local bus and never touches intent counters.
	if len(fx.fc.pubs()) != 0 {
		t.Errorf("diag published %d bus message(s), want 0 — diag is not an intent", len(fx.fc.pubs()))
	}
	if got := metricValue(t, fx.reg, "lexa_cloudlink_intents_forwarded_total"); got != 0 {
		t.Errorf("intents_forwarded_total = %v, want 0 for diag", got)
	}

	// Journal carries the request AND the outcome.
	events := clJournalEvents(t, dir)
	if len(events[journal.TypeIntentReceived]) != 1 {
		t.Errorf("intent_received events = %d, want 1", len(events[journal.TypeIntentReceived]))
	}
	if len(events[journal.TypeIntentApplied]) != 1 {
		t.Fatalf("intent_applied events = %d, want 1", len(events[journal.TypeIntentApplied]))
	}
	var applied journal.IntentApplied
	if err := json.Unmarshal(events[journal.TypeIntentApplied][0].Data, &applied); err != nil {
		t.Fatal(err)
	}
	if applied.Kind != "diag" || applied.Outcome != "applied" {
		t.Errorf("outcome journal = %+v", applied)
	}

	// Immediate second request → rate-limited (1 per 5min), no new upload.
	if _, reason := fx.dl.process(payload); reason != "rate-limited" {
		t.Errorf("second diag reason = %q, want rate-limited", reason)
	}
	if uploads != 1 {
		t.Errorf("uploads = %d after rate-limited request, want still 1", uploads)
	}

	// 5 minutes later → allowed again.
	fx.now = fx.now.Add(diagMinGap + time.Second)
	if _, reason := fx.dl.process(payload); reason != "" {
		t.Errorf("post-gap diag reason = %q, want allowed", reason)
	}
	if uploads != 2 {
		t.Errorf("uploads = %d, want 2", uploads)
	}
}

func TestProcessDiag_HTTPURLRejectedBeforeAnything(t *testing.T) {
	dir := t.TempDir()
	jw, err := journal.Open(journal.Config{Dir: dir, FlushEvery: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer jw.Close()

	fx := newTestDownlink(t, jw)
	payload := cmdPayload(t, 1, "diag", map[string]any{
		"upload_url": "http://attacker.example/exfil", // plain http
		"include":    []string{"config"},
	})
	kind, reason := fx.dl.process(payload)
	if kind != "diag" || reason != "https-required" {
		t.Fatalf("process = (%q, %q), want (diag, https-required)", kind, reason)
	}
	// Validation precedes the limiter AND the journal: nothing recorded,
	// no budget consumed.
	if events := clJournalEvents(t, dir); len(events) != 0 {
		t.Errorf("rejected diag journaled %d event type(s), want 0", len(events))
	}
	// The limiter was never charged: a valid request right after still runs.
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	b, _ := newDiagFixture(t)
	b.client = ts.Client()
	fx.dl.diag = b
	if _, reason := fx.dl.process(cmdPayload(t, 1, "diag", map[string]any{"upload_url": ts.URL})); reason != "" {
		t.Errorf("valid diag after a rejected one = %q, want allowed (invalid requests must not charge the limiter)", reason)
	}
}

func TestProcessDiag_UploadFailureJournaledAsRejected(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer ts.Close()

	dir := t.TempDir()
	jw, err := journal.Open(journal.Config{Dir: dir, FlushEvery: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer jw.Close()

	fx := newTestDownlink(t, jw)
	b, _ := newDiagFixture(t)
	b.client = ts.Client()
	fx.dl.diag = b

	_, reason := fx.dl.process(cmdPayload(t, 1, "diag", map[string]any{"upload_url": ts.URL, "include": []string{"config"}}))
	if reason != "upload-failed" {
		t.Fatalf("reason = %q, want upload-failed", reason)
	}
	events := clJournalEvents(t, dir)
	if len(events[journal.TypeIntentReceived]) != 1 || len(events[journal.TypeIntentRejected]) != 1 {
		t.Errorf("journal = received:%d rejected:%d, want 1/1 (request AND outcome recorded)",
			len(events[journal.TypeIntentReceived]), len(events[journal.TypeIntentRejected]))
	}
}
