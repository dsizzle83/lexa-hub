package main

// diag.go implements DEVICE_ROADMAP.md §2.8: the "diag" cloud command.
// Unlike the seven intent kinds (downlink.go), diag is NOT an intent — it
// never publishes anything onto the local bus and never reaches the hub.
// The cloud command service asks for an on-box diagnostic bundle
// (journal + snapshot + redacted config), cloudlink builds it locally,
// bounded in size, and PUTs it to a presigned HTTPS upload URL the cloud
// minted (homeowner consent is enforced cloud-side, before that URL is ever
// handed to a device — this file only ever sees an already-authorized
// request).
//
// Redaction is defense in depth for a bundle that is about to leave the
// device over the WAN: any file whose NAME looks like a credential (pass/
// secret/key material) is excluded outright, never even opened for
// inclusion; every JSON config file that IS included has every key matching
// pass|secret|token walked and replaced with a fixed placeholder before
// re-marshaling, recursively, since a credential can appear nested (e.g. a
// per-device config block). journal/snapshot content is included verbatim
// (modulo the filename exclusion) — the redaction rule is JSON-config-only,
// per spec, since journal lines are structured audit records with no
// credential-shaped fields to begin with (see internal/journal/schema.go).

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	diagKind = "diag"

	diagIncludeJournal  = "journal"
	diagIncludeSnapshot = "snapshot"
	diagIncludeConfig   = "config"

	// diagMaxBytes bounds the COMPRESSED tar.gz output written to disk
	// (§2.8: "≤32MiB output, stop+error beyond"), checked as bytes actually
	// written to the temp file (post-gzip), not the uncompressed input —
	// build() aborts (removing the partial temp file) the instant this is
	// crossed, rather than silently truncating a bundle mid-file.
	diagMaxBytes = 32 << 20

	// diagUploadTimeout bounds the presigned-URL PUT (§2.8: "30s timeout").
	diagUploadTimeout = 30 * time.Second

	// diagMinGap is the §2.8 "1 per 5min" floor between diag runs (tracked
	// from the START of the previous run, matching diagLimiter.tryStart's
	// doc).
	diagMinGap = 5 * time.Minute
)

// validDiagIncludes is the §2.8 include vocabulary: a diag request's
// "include" list must be a subset of these three section names.
var validDiagIncludes = map[string]bool{
	diagIncludeJournal:  true,
	diagIncludeSnapshot: true,
	diagIncludeConfig:   true,
}

// diagRequest is the "diag" kind's body shape (§2.8):
// {"upload_url":"https://...presigned","include":["journal","snapshot","config"]}.
type diagRequest struct {
	UploadURL string   `json:"upload_url"`
	Include   []string `json:"include"`
}

// validateDiagRequest checks the request-shape rules from §2.8: upload_url
// must be a well-formed https URL (a presigned URL is always https; a plain
// http URL is rejected outright, never dialed), and every include entry
// must be one of the three known bundle sections. Returns ("", "") when the
// request is valid, or a short machine-checkable reason plus a human detail
// otherwise. A pure function — no I/O, no rate limiting — so it is testable
// standalone (the "http-url reject" test case) without building a bundle.
func validateDiagRequest(req diagRequest) (reason, detail string) {
	u, err := url.Parse(req.UploadURL)
	if err != nil || u.Scheme != "https" {
		return "https-required", fmt.Sprintf("upload_url must be https, got %q", req.UploadURL)
	}
	for _, inc := range req.Include {
		if !validDiagIncludes[inc] {
			return "bad-include", fmt.Sprintf("unknown include %q (want subset of journal/snapshot/config)", inc)
		}
	}
	return "", ""
}

// diagPaths are the real on-box locations a diag bundle draws from — struct
// fields (not Config JSON keys) so tests can point them at a temp dir
// without touching cloudlink.json's schema (§2.8: "paths injectable for
// tests (cfg or struct fields defaulting to the real dirs)").
type diagPaths struct {
	JournalDir  string // /var/lib/lexa/journal/**  (every service's journal, not just cloudlink's own)
	SnapshotDir string // /var/lib/lexa/snapshot/**
	ConfigGlob  string // /etc/lexa/*.json (non-recursive, per spec's literal glob)
}

func defaultDiagPaths() diagPaths {
	return diagPaths{
		JournalDir:  "/var/lib/lexa/journal",
		SnapshotDir: "/var/lib/lexa/snapshot",
		ConfigGlob:  "/etc/lexa/*.json",
	}
}

// isSensitiveFilename reports whether name (matched against its basename by
// every caller) is excluded from a diag bundle ENTIRELY — never opened, never
// listed — per §2.8's four patterns: *pass*, *secret*, *.key, *key*.pem.
// Matching is case-insensitive and substring-based (deliberately generous:
// over-excluding a borderline filename is free; under-excluding a real
// credential is not).
func isSensitiveFilename(name string) bool {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "pass"):
		return true
	case strings.Contains(lower, "secret"):
		return true
	case strings.HasSuffix(lower, ".key"):
		return true
	case strings.Contains(lower, "key") && strings.HasSuffix(lower, ".pem"):
		return true
	default:
		return false
	}
}

// redactedPlaceholder replaces the VALUE of any JSON key matching
// redactKeyRe, regardless of the original value's type.
const redactedPlaceholder = "«redacted»"

var redactKeyRe = regexp.MustCompile(`(?i)(pass|secret|token)`)

// redactJSON walks a json.Unmarshal(..., &any)-shaped value recursively,
// replacing the value of every object key matching redactKeyRe. Arrays are
// walked element-wise; scalars pass through unchanged.
func redactJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if redactKeyRe.MatchString(k) {
				out[k] = redactedPlaceholder
			} else {
				out[k] = redactJSON(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = redactJSON(val)
		}
		return out
	default:
		return v
	}
}

// redactJSONBytes parses data as a generic JSON document, redacts it
// (redactJSON), and re-marshals. json.Marshal on a map[string]any sorts keys
// lexicographically, so output is deterministic even though map iteration
// order is not. Returns an error — unchanged — if data does not parse as
// JSON at all; the caller's contract for that case is "skipped+noted", not a
// build-aborting failure.
func redactJSONBytes(data []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return json.Marshal(redactJSON(v))
}

// errDiagBundleTooLarge is returned by countingWriter.Write once the §2.8
// size bound is crossed, propagating up through gzip/tar's Write calls to
// build()'s WalkDir callback, which treats it as fatal (unlike a per-file
// skip).
var errDiagBundleTooLarge = fmt.Errorf("diag bundle exceeds %d byte bound", diagMaxBytes)

// countingWriter wraps the temp file and counts bytes actually written
// post-compression (build()'s gzip.Writer writes through this), returning
// errDiagBundleTooLarge the instant the running total exceeds the bound —
// "stop+error beyond", not a silent truncation.
type countingWriter struct {
	w       *os.File
	written int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if c.written > diagMaxBytes {
		return 0, errDiagBundleTooLarge
	}
	n, err := c.w.Write(p)
	c.written += int64(n)
	if err != nil {
		return n, err
	}
	if c.written > diagMaxBytes {
		return n, errDiagBundleTooLarge
	}
	return n, nil
}

// diagLimiter enforces §2.8's "1 concurrent, 1 per 5min" rate limit on the
// build+upload phase (request-shape validation happens before tryStart, so a
// malformed request never consumes the limiter's budget).
type diagLimiter struct {
	mu      sync.Mutex
	running bool
	lastRun time.Time
}

// tryStart attempts to begin a diag run at now, returning ok=false and a
// human reason if another run is in flight or the last one started less
// than diagMinGap ago. On success the caller MUST call finish() (typically
// via defer) once the run completes, success or failure.
func (l *diagLimiter) tryStart(now time.Time) (ok bool, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running {
		return false, "a diag bundle is already in progress"
	}
	if !l.lastRun.IsZero() && now.Sub(l.lastRun) < diagMinGap {
		return false, fmt.Sprintf("diag rate-limited: last run %s ago (min gap %s)", now.Sub(l.lastRun), diagMinGap)
	}
	l.running = true
	l.lastRun = now
	return true, ""
}

func (l *diagLimiter) finish() {
	l.mu.Lock()
	l.running = false
	l.mu.Unlock()
}

// diagBuilder builds and uploads a diag bundle. paths/client are both
// injectable for tests.
type diagBuilder struct {
	paths  diagPaths
	client *http.Client
}

func newDiagBuilder(paths diagPaths) *diagBuilder {
	return &diagBuilder{paths: paths, client: &http.Client{Timeout: diagUploadTimeout}}
}

// build tars+gzips the requested include sections into a new temp file,
// applying filename exclusion (every section) and JSON key redaction
// (config section only — see this file's package doc). Returns the temp
// file path (caller must os.Remove it once done), the count of files
// actually included, a human-readable list of skipped paths (excluded or
// unparsable/unreadable — never fatal on their own), and an error only for
// a genuine build failure: an I/O fault writing the tar itself, or the
// diagMaxBytes size bound tripping ("stop+error beyond" — the partial temp
// file is removed before returning).
func (b *diagBuilder) build(include []string) (path string, n int, skipped []string, err error) {
	tmp, err := os.CreateTemp("", "lexa-diag-*.tar.gz")
	if err != nil {
		return "", 0, nil, err
	}
	tmpPath := tmp.Name()
	abort := func(err error) (string, int, []string, error) {
		tmp.Close()
		os.Remove(tmpPath)
		return "", 0, nil, err
	}

	cw := &countingWriter{w: tmp}
	gz := gzip.NewWriter(cw)
	tw := tar.NewWriter(gz)

	set := make(map[string]bool, len(include))
	for _, s := range include {
		set[s] = true
	}

	addVerbatim := func(root, relBase string) error {
		return filepath.WalkDir(root, func(p string, de fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if os.IsNotExist(walkErr) {
					return nil // section dir doesn't exist yet — nothing to include, not an error
				}
				return walkErr
			}
			if de.IsDir() {
				return nil
			}
			if isSensitiveFilename(de.Name()) {
				skipped = append(skipped, p+" (excluded: sensitive filename)")
				return nil
			}
			data, rerr := os.ReadFile(p)
			if rerr != nil {
				skipped = append(skipped, p+" (unreadable: "+rerr.Error()+")")
				return nil
			}
			rel, relErr := filepath.Rel(root, p)
			if relErr != nil {
				rel = de.Name()
			}
			if werr := writeTarEntry(tw, filepath.ToSlash(filepath.Join(relBase, rel)), data); werr != nil {
				return werr
			}
			n++
			return nil
		})
	}

	addConfig := func() error {
		matches, _ := filepath.Glob(b.paths.ConfigGlob)
		for _, p := range matches {
			base := filepath.Base(p)
			if isSensitiveFilename(base) {
				skipped = append(skipped, p+" (excluded: sensitive filename)")
				continue
			}
			data, rerr := os.ReadFile(p)
			if rerr != nil {
				skipped = append(skipped, p+" (unreadable: "+rerr.Error()+")")
				continue
			}
			red, rerr := redactJSONBytes(data)
			if rerr != nil {
				skipped = append(skipped, p+" (unparsable JSON, skipped)")
				continue
			}
			if werr := writeTarEntry(tw, "config/"+base, red); werr != nil {
				return werr
			}
			n++
		}
		return nil
	}

	var buildErr error
	if set[diagIncludeJournal] {
		buildErr = addVerbatim(b.paths.JournalDir, diagIncludeJournal)
	}
	if buildErr == nil && set[diagIncludeSnapshot] {
		buildErr = addVerbatim(b.paths.SnapshotDir, diagIncludeSnapshot)
	}
	if buildErr == nil && set[diagIncludeConfig] {
		buildErr = addConfig()
	}

	if buildErr != nil {
		return abort(buildErr)
	}
	if err := tw.Close(); err != nil {
		return abort(err)
	}
	if err := gz.Close(); err != nil {
		return abort(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", 0, nil, err
	}
	return tmpPath, n, skipped, nil
}

// writeTarEntry writes one regular-file entry (mode 0600 — a diagnostic
// bundle carries no reason to preserve execute bits, and never should for a
// config file) with name and contents data.
func writeTarEntry(tw *tar.Writer, name string, data []byte) error {
	hdr := &tar.Header{
		Name:    name,
		Mode:    0o600,
		Size:    int64(len(data)),
		ModTime: time.Now(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// upload PUTs the file at path to uploadURL, bounded by b.client's Timeout
// (30s, §2.8). Content-Length is set explicitly (fi.Size()) so the request
// carries a real Content-Length rather than falling back to chunked
// transfer encoding — required by most S3-style presigned PUT URLs, which
// are signed over an exact byte count.
func (b *diagBuilder) upload(uploadURL, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPut, uploadURL, f)
	if err != nil {
		return err
	}
	req.ContentLength = fi.Size()
	req.Header.Set("Content-Type", "application/gzip")

	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upload: unexpected status %s", resp.Status)
	}
	return nil
}
