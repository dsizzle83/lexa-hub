package main

// site.go implements DEVICE_ROADMAP.md §4.3's GET /site route: identity and
// environment facts a commissioning wizard or dashboard needs before it has
// talked to the hub bus at all.
import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"time"

	"lexa-hub/internal/apicontract"
	"lexa-hub/internal/buildinfo"
)

// defaultSiteCacheFile is Config.SiteCacheFile's default (DEVICE_ROADMAP.md
// §10): the on-disk cache of whatever site metadata the cloud has pushed
// down (address, tariff zone, plan details, ...). lexa-api has no cloud
// session of its own (that's lexa-cloudlink's job, TASK-085+) — this file is
// a passive passthrough of whatever cache file exists, nothing more.
const defaultSiteCacheFile = "/var/lib/lexa/site.json"

// siteResp is GET /site's JSON shape.
//
// tariff_zone is deliberately OMITTED from this shape (not present, not even
// as an empty string): DEVICE_ROADMAP.md §4.3 asks for it, but lexa-api has
// no source for it today — the tariff intent/CSIP schedule live on
// lexa-hub, and GAP-05's zone-must-match-tariff invariant is enforced by SOM
// provisioning (CLAUDE.md's timezone deployment requirement), not by
// anything this process can read. Guessing a value here would be worse than
// omitting it; see this task's report for the same note.
type siteResp struct {
	Serial       string          `json:"serial"`
	FW           string          `json:"fw"`
	Commissioned bool            `json:"commissioned"`
	TZ           string          `json:"tz"`
	SiteCache    json.RawMessage `json:"site_cache,omitempty"`
	// ContractVersion is the hub⇄app HTTP contract version (apicontract.Version,
	// Workstream C) — additive, same value as /status's field, the
	// X-Lexa-Contract-Version header, and the mDNS "contract=" TXT record.
	ContractVersion int `json:"contract_version"`
}

// siteHandler serves GET /site. serial is the same value main.go resolves
// for the TLS cert CN and the mDNS TXT record (resolveSerial), so all three
// surfaces always agree.
func siteHandler(serial, siteCacheFile string) http.HandlerFunc {
	if siteCacheFile == "" {
		siteCacheFile = defaultSiteCacheFile
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		resp := siteResp{
			Serial:          serial,
			FW:              buildinfo.Version,
			Commissioned:    isCommissioned(),
			TZ:              time.Local.String(),
			ContractVersion: apicontract.Version,
		}
		if data, err := os.ReadFile(siteCacheFile); err == nil {
			if json.Valid(data) {
				resp.SiteCache = json.RawMessage(data)
			} else {
				slog.Warn("lexa-api: site cache file is not valid JSON, omitting from /site", "path", siteCacheFile)
			}
		}
		// A missing/unreadable site cache file is the expected steady state
		// on an uncommissioned or cloudlink-less unit — no log line for that
		// case; only a present-but-corrupt file is worth a WARN.

		writeJSON(w, http.StatusOK, resp)
	}
}
