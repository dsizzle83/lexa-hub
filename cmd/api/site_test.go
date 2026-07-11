package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lexa-hub/internal/buildinfo"
)

// TestSiteHandler_BasicFields pins GET /site's always-present fields and the
// deliberate omission of tariff_zone (DEVICE_ROADMAP.md §4.3: "NOT available
// to api — omit").
func TestSiteHandler_BasicFields(t *testing.T) {
	h := siteHandler("SN-TEST-1", filepath.Join(t.TempDir(), "does-not-exist.json"))
	req := httptest.NewRequest(http.MethodGet, "/site", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "tariff_zone") {
		t.Errorf("/site response contains tariff_zone, want it entirely omitted: %s", rec.Body.String())
	}
	var got siteResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Serial != "SN-TEST-1" {
		t.Errorf("Serial = %q, want %q", got.Serial, "SN-TEST-1")
	}
	if got.FW == "" {
		t.Error("FW is empty, want a placeholder version string")
	}
	if got.TZ == "" {
		t.Error("TZ is empty, want time.Local.String()")
	}
	if got.SiteCache != nil {
		t.Errorf("SiteCache = %s, want nil (no cache file present)", got.SiteCache)
	}
}

// TestSiteHandler_FWReflectsBuildinfoVersion pins that /site.fw is a live
// read of internal/buildinfo.Version (GAP-5) — not a hardcoded placeholder
// — so a real -ldflags -X stamp actually reaches this response.
func TestSiteHandler_FWReflectsBuildinfoVersion(t *testing.T) {
	orig := buildinfo.Version
	defer func() { buildinfo.Version = orig }()
	buildinfo.Version = "1.2.3-test"

	h := siteHandler("SN-TEST-FW", filepath.Join(t.TempDir(), "absent.json"))
	req := httptest.NewRequest(http.MethodGet, "/site", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	var got siteResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.FW != "1.2.3-test" {
		t.Errorf("FW = %q, want %q", got.FW, "1.2.3-test")
	}
}

// TestSiteHandler_SiteCachePassthrough pins the raw-JSON passthrough of an
// on-disk site cache file, present vs. absent.
func TestSiteHandler_SiteCachePassthrough(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "site.json")
	cacheContent := `{"address":"123 Main St","install_date":"2026-01-01"}`
	if err := os.WriteFile(cachePath, []byte(cacheContent), 0o644); err != nil {
		t.Fatal(err)
	}

	h := siteHandler("SN-TEST-2", cachePath)
	req := httptest.NewRequest(http.MethodGet, "/site", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	cache, ok := got["site_cache"].(map[string]any)
	if !ok {
		t.Fatalf("site_cache missing or wrong type: %+v", got)
	}
	if cache["address"] != "123 Main St" {
		t.Errorf("site_cache.address = %v, want %q", cache["address"], "123 Main St")
	}
}

// TestSiteHandler_InvalidCacheFileOmitted pins that a present-but-corrupt
// cache file is omitted (not a fabricated/garbled field), not a 500.
func TestSiteHandler_InvalidCacheFileOmitted(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "site.json")
	if err := os.WriteFile(cachePath, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := siteHandler("SN-TEST-3", cachePath)
	req := httptest.NewRequest(http.MethodGet, "/site", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even with a corrupt cache file", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "site_cache") {
		t.Errorf("response contains site_cache for a non-JSON cache file, want omitted: %s", rec.Body.String())
	}
}

// TestSiteHandler_DefaultCacheFile pins that an empty siteCacheFile argument
// falls back to defaultSiteCacheFile rather than panicking/erroring.
func TestSiteHandler_DefaultCacheFile(t *testing.T) {
	h := siteHandler("SN-TEST-4", "")
	req := httptest.NewRequest(http.MethodGet, "/site", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// TestSiteHandler_CORSPreflight pins the shared OPTIONS convention.
func TestSiteHandler_CORSPreflight(t *testing.T) {
	h := siteHandler("SN-TEST-5", filepath.Join(t.TempDir(), "absent.json"))
	req := httptest.NewRequest(http.MethodOptions, "/site", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS: status = %d, want 204", rec.Code)
	}
}
