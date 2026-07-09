package main

// configwrite.go implements DEVICE_ROADMAP.md §4.5: POST /config/{service},
// the commissioning config-write path. This is the single write path a
// commissioning wizard has onto every other lexa-* service's on-disk
// config, plus lexa-api's own bearer secret — the most security-sensitive
// local HTTP surface this service exposes. Every request goes through the
// same fixed order, and a rejection at any stage means nothing at any later
// stage happens:
//
//  1. gate   — refuse outright unless the unit is uncommissioned (§9)
//  2. read   — bounded body read (256 KiB for a JSON config, 128 B for the
//              api-secret raw-token case)
//  3. decode — must be a JSON object (api-secret: plain text, not JSON)
//  4. validate — schema check (required/enum/path-allowlist) against the
//              embedded configs/schema/<service>.json allowlist
//  5. write  — sha256(before) → staged file (0640, fsynced) → rename →
//              sha256(after)
//  6. journal — config_write{service, actor, before_sha256, after_sha256}
//  7. restart — request via the privilege-free commissioning trigger (unit
//              4.5): append the bare service name to
//              /var/lib/lexa/commission/restart.request (tmp+rename) and
//              poll restart.result for this service's entry, up to
//              restartTimeout. A root-owned systemd .path unit
//              (systemd/lexa-commission.path -> lexa-commission.service ->
//              scripts/lexa-commission-apply) does the actual `systemctl
//              restart` from a closed allowlist — lexa-api itself never
//              escalates privilege (NoNewPrivileges=yes,
//              systemd/lexa-api.service; see that unit's comment block).
//              This replaces a sudo-based restart (systemd/sudoers.d-
//              lexa-api, DELETED) that NoNewPrivileges blocked by design.
//              The api-secret case now ALSO requests a restart through the
//              SAME file (of "api" itself, uniquely legal here since the
//              actual restart runs in an external process) but never
//              polls for the outcome — see handleAPISecretWrite.
//
// Callers wrap this handler in requireBearerStrict (main.go): the bearer
// token is required on this route even while the unit is uncommissioned —
// it is the per-unit label secret, not a "commissioning implies trusted"
// bypass.
import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"lexa-hub/configs/schema"
	"lexa-hub/internal/journal"
	"lexa-hub/internal/metrics"
)

const (
	// configWriteMaxBodyBytes bounds a full-JSON-config POST body (§4.5
	// point 1: "body = full JSON config ≤256KiB").
	configWriteMaxBodyBytes = 256 << 10
	// apiSecretMaxBodyBytes bounds the api-secret route's raw-token body
	// (§4.5 point 1: "a raw new secret (plain text ≤128B)").
	apiSecretMaxBodyBytes = 128
)

// configWriteDir is the directory holding the six lexa-*.json config files —
// a var (not const), matching mdns.go's commissionedMarkerPath convention,
// so tests can point it at a temp directory instead of the real /etc/lexa.
var configWriteDir = "/etc/lexa"

// serviceUnit maps a POST /config/{service} path segment to the systemd unit
// name §4.5 point 5 restarts on a successful write. Deliberately excludes
// "api"/"api-secret" — lexa-api restarting itself would kill the very HTTP
// response reporting the restart; see handleAPISecretWrite, which never
// execs a restart at all.
var serviceUnit = map[string]string{
	"hub":        "lexa-hub",
	"northbound": "lexa-northbound",
	"modbus":     "lexa-modbus",
	"ocpp":       "lexa-ocpp",
	"telemetry":  "lexa-telemetry",
	"cloudlink":  "lexa-cloudlink",
}

// configSchema is the hand-rolled allowlist shape read from
// configs/schema/<service>.json (schema.FS). No jsonschema dependency, by
// design (task brief): three plain rule kinds cover everything this task's
// six configs need.
type configSchema struct {
	// Required top-level keys that must be present in the posted document.
	Required []string `json:"required,omitempty"`

	// Enums maps a dotted key path (e.g. "log_level", "mode") to its allowed
	// string values. The value at that path may be either a plain string
	// (checked directly) or a JSON object (every value within it is checked
	// the same way — this is how modbus.json's "reconciler" map, keyed by
	// device class, and ocpp.json's scalar "reconciler" string share one
	// enum rule). An empty string value (the pervasive "disabled/anonymous"
	// sentinel this codebase uses throughout — see CLAUDE.md's mqtt_user/
	// reconciler defaults) is always accepted regardless of the enum list.
	Enums map[string][]string `json:"enums,omitempty"`

	// PathKeys maps a dotted key path to a required path prefix. An empty
	// prefix string means the generic allowlist applies: the value (after
	// path.Clean) must be under /etc/lexa/ or /var/lib/lexa/. A non-empty
	// prefix (e.g. "/etc/lexa/mqtt/" for mqtt_pass_file) must match exactly.
	// An empty string VALUE (path not configured) is always accepted — the
	// same disabled/anonymous sentinel as Enums.
	PathKeys map[string]string `json:"path_keys,omitempty"`
}

// configSchemas holds every service's schema, decoded once from the
// embedded configs/schema/*.json files at package init. A missing or
// malformed embedded schema is a build-time-catchable programmer error
// (the files are compiled into the binary, so if one is wrong every build
// using it is wrong) — panicking here fails loud immediately rather than
// discovering it on the first commissioning request in the field.
var configSchemas = loadConfigSchemas()

func loadConfigSchemas() map[string]configSchema {
	out := make(map[string]configSchema, len(serviceUnit))
	for svc := range serviceUnit {
		b, err := schema.FS.ReadFile(svc + ".json")
		if err != nil {
			panic(fmt.Sprintf("configwrite: embedded schema for service %q missing: %v", svc, err))
		}
		var s configSchema
		if err := json.Unmarshal(b, &s); err != nil {
			panic(fmt.Sprintf("configwrite: embedded schema for service %q is invalid JSON: %v", svc, err))
		}
		out[svc] = s
	}
	return out
}

// configWriteResp is POST /config/{service}'s JSON response shape (both the
// full-config and api-secret cases).
type configWriteResp struct {
	Written   bool   `json:"written"`
	Restarted bool   `json:"restarted"`
	Detail    string `json:"detail,omitempty"`
}

// restartRunner executes the restart step and reports its outcome. A
// function value (not the file-write-and-poll sequence called inline in
// the handler) so tests can substitute a scripted double
// (success/failure/timeout) without ever touching the real
// /var/lib/lexa/commission files or a live lexa-commission-apply — see
// configwrite_test.go's recordingRestartRunner.
type restartRunner func(unit string) (ok bool, detail string)

// commissionDir locates the commissioning restart trigger's request/result
// files (unit 4.5) — a var (not const), matching configWriteDir's
// convention, so tests can point it at a temp directory instead of the
// real /var/lib/lexa/commission. restartTimeout/restartPollInterval bound
// how long defaultRestartRunner polls restart.result before reporting a
// timeout — also vars, so a test can shrink both instead of waiting out a
// real 15s bound. Production always runs with the defaults set here.
var (
	commissionDir       = "/var/lib/lexa/commission"
	restartTimeout      = 15 * time.Second
	restartPollInterval = 500 * time.Millisecond
)

// commissionRequestFile/commissionResultFile are the two file BASE NAMES
// (joined onto commissionDir) the protocol uses — see
// scripts/lexa-commission-apply's header for the full protocol writeup
// shared between that script and this file.
const (
	commissionRequestFile = "restart.request"
	commissionResultFile  = "restart.result"
)

// commissionWriteMu serializes read-modify-write access to the request
// file across concurrent HTTP handler goroutines within this ONE lexa-api
// process (net/http runs each request in its own goroutine, and two
// commissioning config writes landing back-to-back would otherwise race
// on the read-append-rename below). It says nothing about the root
// oneshot on the other end of the file, which only ever reads-then-removes
// it — no lock needed there since PathChanged= events serialize its runs.
var commissionWriteMu sync.Mutex

// commissionResult is restart.result's JSON shape, written by
// scripts/lexa-commission-apply and read here: {"ts": <unix-seconds>,
// "results": {"<svc>": "ok"|"failed: <detail>"}}. Results is keyed by the
// SAME bare service name (hub|northbound|modbus|ocpp|telemetry|cloudlink|
// api) the request file uses.
type commissionResult struct {
	TS      int64             `json:"ts"`
	Results map[string]string `json:"results"`
}

// requestRestart appends bare (a bare service name, e.g. "hub" — NOT
// "lexa-hub") to commissionDir/restart.request via tmp+rename, creating
// the directory first if needed (§4.5 point 1: StateDirectory=lexa makes
// /var/lib/lexa writable by this process; the "commission" subdirectory
// itself is this function's job to create, not systemd's — the root
// lexa-commission.service that later reads it never needs to create
// anything, since it only ever runs once this directory already holds a
// request). A name already pending is left alone rather than duplicated —
// harmless either way since scripts/lexa-commission-apply's allowlist step
// processes each line independently, but a duplicate line is wasted work
// for no benefit.
func requestRestart(bare string) error {
	commissionWriteMu.Lock()
	defer commissionWriteMu.Unlock()

	if err := os.MkdirAll(commissionDir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", commissionDir, err)
	}
	reqPath := filepath.Join(commissionDir, commissionRequestFile)
	existing, err := os.ReadFile(reqPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", reqPath, err)
	}
	lines := strings.Fields(string(existing))
	for _, name := range lines {
		if name == bare {
			return nil // already pending
		}
	}
	lines = append(lines, bare)
	data := []byte(strings.Join(lines, "\n") + "\n")
	return writeStagedThenRename(reqPath, data, 0o640)
}

// readRestartResult looks up bare's entry in commissionDir/restart.result.
// ok is false when the file is absent, unparsable, or has no entry for
// bare yet — all three mean "nothing to report yet, keep polling" to
// defaultRestartRunner's caller, not an error: the root oneshot may not
// have run at all yet, may still be mid-run, or may be processing a
// DIFFERENT service's line first (unit 4.5's api-last ordering,
// scripts/lexa-commission-apply).
func readRestartResult(bare string) (status string, ts int64, ok bool) {
	data, err := os.ReadFile(filepath.Join(commissionDir, commissionResultFile))
	if err != nil {
		return "", 0, false
	}
	var res commissionResult
	if err := json.Unmarshal(data, &res); err != nil {
		return "", 0, false
	}
	status, present := res.Results[bare]
	return status, res.TS, present
}

// defaultRestartRunner requests a restart via the privilege-free
// commissioning trigger (unit 4.5): lexa-api runs unprivileged
// (NoNewPrivileges=yes, systemd/lexa-api.service) and can no longer sudo
// into systemctl — that path is deleted (systemd/sudoers.d-lexa-api, see
// its header for why it existed and this comment for why it's gone).
// Instead this appends unit's bare name to restart.request and polls
// restart.result for that name's entry, up to restartTimeout
// (restartPollInterval between reads), returning the SAME (ok, detail)
// shape the old sudo-exec implementation did — every caller
// (configWriteHandler's `run(unit)`) is unchanged. A restart failure or
// timeout is reported honestly in the response but is NEVER a write
// failure — the config is already committed to disk by the time this
// runs (see configWriteHandler: write, then journal, then restart, in
// that order).
//
// reqAt (captured BEFORE the request is written, not after) guards
// against reading a STALE result left over from an earlier restart of the
// same service: scripts/lexa-commission-apply always removes the request
// file after writing a fresh result, but this process has no way to tell
// "the file I see now is fresh" from "the file I see now is yesterday's"
// except by timestamp — a result whose ts predates this call's own
// request can only be leftover from a PRIOR cycle, so it is treated as
// not-yet-answered and polling continues rather than returning a false
// positive.
func defaultRestartRunner(unit string) (bool, string) {
	bare := strings.TrimPrefix(unit, "lexa-")
	reqAt := time.Now().Unix()

	if err := requestRestart(bare); err != nil {
		return false, fmt.Sprintf("could not request restart of %s: %v", unit, err)
	}

	deadline := time.Now().Add(restartTimeout)
	for {
		if status, ts, ok := readRestartResult(bare); ok && ts >= reqAt {
			if status == "ok" {
				return true, fmt.Sprintf("restarted %s", unit)
			}
			return false, fmt.Sprintf("restart of %s %s", unit, status)
		}
		if time.Now().After(deadline) {
			return false, "restart requested; result pending — poll /status or retry"
		}
		time.Sleep(restartPollInterval)
	}
}

// configWriteHandler serves POST /config/{service} (DEVICE_ROADMAP.md §4.5).
// apiSecretPath is the file the "api-secret" service case rotates — the same
// path lexa-api's own Config.APITokenFile names (main.go wires cfg.APITokenFile
// through unchanged). jw may be nil (defensive; cmd/api's own Journal config
// block is never optional in main.go's wiring, but a nil-tolerant journal
// call here means a test constructing this handler directly doesn't need
// one). writesCtr/rejectsCtr are nil-receiver-safe *metrics.Counter (see
// internal/metrics's doc) so tests can pass nil too.
func configWriteHandler(apiSecretPath string, jw *journal.Writer, run restartRunner, writesCtr, rejectsCtr *metrics.Counter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		service := strings.TrimPrefix(r.URL.Path, "/config/")
		if service == "" || strings.Contains(service, "/") {
			rejectsCtr.Inc()
			http.Error(w, "missing or malformed service name", http.StatusBadRequest)
			return
		}

		// Gate (§9): checked FIRST, before reading or parsing anything the
		// caller sent — a commissioned unit refuses every write outright and
		// must never spend effort decoding/validating a body it's going to
		// 403 regardless. The roadmap's "or a cloud-armed commissioning
		// window" re-open is EXPLICITLY out of scope here (v2; no such armed
		// doc exists anywhere in this repo yet — lexa-cloudlink's downlink,
		// §2.6/TASK-086, is the only plausible future source of one).
		if isCommissioned() {
			rejectsCtr.Inc()
			slog.Warn("lexa-api: config write refused", "route", "/config", "service", service, "reason", "commissioned")
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "unit is commissioned; config writes are locked (a cloud-armed re-open window is a v2 feature, not implemented in this build)",
			})
			return
		}

		if service == "api-secret" {
			handleAPISecretWrite(w, r, apiSecretPath, jw, writesCtr, rejectsCtr)
			return
		}

		unit, ok := serviceUnit[service]
		if !ok {
			rejectsCtr.Inc()
			http.Error(w, fmt.Sprintf("unknown service %q", service), http.StatusBadRequest)
			return
		}
		sch, ok := configSchemas[service]
		if !ok {
			// Unreachable in practice — serviceUnit and configSchemas are
			// built from the same key set at init — but fail closed rather
			// than commit an unvalidated config if they ever drift apart.
			rejectsCtr.Inc()
			slog.Error("lexa-api: config write: no schema registered", "route", "/config", "service", service)
			http.Error(w, fmt.Sprintf("no schema registered for service %q", service), http.StatusInternalServerError)
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, configWriteMaxBodyBytes))
		if err != nil {
			rejectsCtr.Inc()
			var mbErr *http.MaxBytesError
			if errors.As(err, &mbErr) {
				http.Error(w, "request body too large (max 256 KiB)", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}

		// Decode to a generic map (never a typed Config struct): this is
		// the round-trip requirement (§4.5 point 3c) — unknown keys must be
		// PRESERVED verbatim, and the only way to guarantee that with
		// encoding/json is to never decode into anything narrower than
		// map[string]any.
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			rejectsCtr.Inc()
			http.Error(w, "body must be a JSON object: "+err.Error(), http.StatusBadRequest)
			return
		}
		if doc == nil {
			// A literal `null` body decodes into a nil map without error —
			// reject explicitly rather than "successfully" writing an empty
			// config out from under a service.
			rejectsCtr.Inc()
			http.Error(w, "body must be a non-null JSON object", http.StatusBadRequest)
			return
		}

		if err := validateConfigBody(doc, sch); err != nil {
			rejectsCtr.Inc()
			slog.Warn("lexa-api: config write rejected", "route", "/config", "service", service, "reason", "schema", "err", err)
			http.Error(w, "schema validation failed: "+err.Error(), http.StatusBadRequest)
			return
		}

		// Re-encode from the SAME map that was validated — never the raw
		// client bytes (which were only ever validated, not sanitized) and
		// never a narrower typed struct (which would silently drop unknown
		// keys). json.Marshal of a map[string]any sorts keys, so this is
		// deterministic but not byte-identical to whatever key order the
		// client sent — the same tradeoff cmd/lexa-migrate/migrate.go's
		// writeStaged documents and accepts.
		reencoded, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			rejectsCtr.Inc()
			http.Error(w, "internal re-encode failure: "+err.Error(), http.StatusInternalServerError)
			return
		}
		reencoded = append(reencoded, '\n')

		targetPath := filepath.Join(configWriteDir, service+".json")
		beforeSHA, err := sha256OfFileOrEmpty(targetPath)
		if err != nil {
			rejectsCtr.Inc()
			http.Error(w, "read existing config: "+err.Error(), http.StatusInternalServerError)
			return
		}

		if err := writeStagedThenRename(targetPath, reencoded, 0o640); err != nil {
			rejectsCtr.Inc()
			slog.Error("lexa-api: config write failed", "route", "/config", "service", service, "err", err)
			http.Error(w, "write failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		afterSHA := sha256Hex(reencoded)

		const actor = "local-api" // token identity is all lexa-api has to attribute a write to; see this task's report
		if jw != nil {
			if ev, everr := journal.NewConfigWriteEvent("api", journal.NewConfigWrite(service, actor, beforeSHA, afterSHA)); everr == nil {
				_ = jw.Append(ev)
			}
		}
		writesCtr.Inc()

		resp := configWriteResp{Written: true}
		ok2, detail := run(unit)
		resp.Restarted = ok2
		resp.Detail = detail

		slog.Info("lexa-api: config write", "route", "/config", "service", service, "actor", actor,
			"before_sha256", beforeSHA, "after_sha256", afterSHA, "restarted", resp.Restarted)
		writeJSON(w, http.StatusOK, resp)
	}
}

// handleAPISecretWrite implements the "api-secret" case (§4.5 point 1): the
// body is NOT a JSON config — it IS the new bearer token, plain text, ≤128
// bytes — written straight to apiSecretPath (lexa-api's own
// Config.APITokenFile).
//
// Unlike every other service, "api" IS a legal restart-trigger target
// under unit 4.5 — scripts/lexa-commission-apply runs it LAST and
// documents why: restarting lexa-api happens in an entirely SEPARATE root
// process, not inside this handler's own goroutine, so — unlike the old
// sudo-exec design this replaced — it no longer kills the very HTTP
// response reporting the restart. This handler still never POLLS for that
// restart's outcome the way configWriteHandler's defaultRestartRunner does
// for the other six services, though: polling here would mean blocking
// this goroutine until either a result appears or lexa-api itself is
// killed by the restart it is waiting on — the second outcome is the
// LIKELY one, and it would simply drop the connection mid-poll with no
// response ever written, which is strictly worse than an honest,
// immediate {restarted:false, "connection will drop"}. So the request is
// fired (best-effort — see the error log below) and this handler returns
// immediately; a commissioning wizard is expected to poll for lexa-api
// coming back on its own (e.g. GET /healthz) rather than wait on this
// response for it.
//
// Live-reload vs restart-required, documented (TESTS spec explicitly allows
// either — this ships restart-required): main.go loads the bearer token
// ONCE at startup (Config.LoadAPIToken) and closes over that string in every
// requireBearer/requireBearerStrict-wrapped handler for the rest of the
// process's life. This write commits the NEW secret to disk immediately
// (0600, matching the file's manufacturing-provisioned permissions) but does
// NOT take effect for authentication until lexa-api itself restarts — which
// is exactly the restart this handler now requests via the SAME
// commissioning-trigger file the generic config-write path uses (unit 4.5),
// instead of asking the operator to do it by hand. A live in-process reload
// would need either a background re-read of the token file on every request
// (defeating much of the point of comparing a fixed constant-time secret) or
// a SIGHUP-style signal wired through main()'s wrapper closures — a
// materially bigger change than this unit's bounded scope over
// cmd/api/main.go.
// configwrite_test.go's TestAPISecretRotation_OldTokenStillWorksUntilRestart
// pins this contract explicitly so it can't silently drift into looking like
// a bug.
func handleAPISecretWrite(w http.ResponseWriter, r *http.Request, apiSecretPath string, jw *journal.Writer, writesCtr, rejectsCtr *metrics.Counter) {
	if apiSecretPath == "" {
		rejectsCtr.Inc()
		http.Error(w, "api_token_file is not configured; nowhere to write the rotated secret", http.StatusInternalServerError)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, apiSecretMaxBodyBytes))
	if err != nil {
		rejectsCtr.Inc()
		var mbErr *http.MaxBytesError
		if errors.As(err, &mbErr) {
			http.Error(w, "secret body too large (max 128 bytes)", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	secret := strings.TrimSpace(string(body))
	if secret == "" {
		rejectsCtr.Inc()
		http.Error(w, "secret body must not be empty", http.StatusBadRequest)
		return
	}

	beforeSHA, err := sha256OfFileOrEmpty(apiSecretPath)
	if err != nil {
		rejectsCtr.Inc()
		http.Error(w, "read existing secret: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := writeStagedThenRename(apiSecretPath, []byte(secret+"\n"), 0o600); err != nil {
		rejectsCtr.Inc()
		slog.Error("lexa-api: api-secret rotation failed", "route", "/config", "service", "api-secret", "err", err)
		http.Error(w, "write failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	afterSHA := sha256Hex([]byte(secret + "\n"))

	const actor = "local-api"
	if jw != nil {
		if ev, everr := journal.NewConfigWriteEvent("api", journal.NewConfigWrite("api-secret", actor, beforeSHA, afterSHA)); everr == nil {
			_ = jw.Append(ev)
		}
	}
	writesCtr.Inc()

	// Fire the self-restart request (best-effort: a failure here means the
	// new secret is already safely on disk, just not yet requested to take
	// effect — logged loudly rather than failing this already-succeeded
	// write). See this function's doc comment for why this never polls for
	// the outcome the way the generic configWriteHandler path does.
	if err := requestRestart("api"); err != nil {
		slog.Error("lexa-api: could not request self-restart via commissioning trigger", "route", "/config", "service", "api-secret", "err", err)
	}

	slog.Info("lexa-api: config write", "route", "/config", "service", "api-secret", "actor", actor,
		"before_sha256", beforeSHA, "after_sha256", afterSHA, "restarted", false)
	writeJSON(w, http.StatusOK, configWriteResp{
		Written:   true,
		Restarted: false,
		Detail:    "restart requested via commissioning trigger; connection will drop",
	})
}

// validateConfigBody runs every rule in sch against doc, collecting ALL
// violations (not just the first) into one joined error — a commissioning
// wizard benefits from seeing every problem in one round trip rather than
// fixing them one at a time.
func validateConfigBody(doc map[string]any, sch configSchema) error {
	var errs []string

	for _, req := range sch.Required {
		if _, ok := doc[req]; !ok {
			errs = append(errs, fmt.Sprintf("missing required key %q", req))
		}
	}

	for dotted, allowed := range sch.Enums {
		v, ok := lookupPath(doc, dotted)
		if !ok {
			continue // optional key, absent — fine unless also Required (checked above)
		}
		switch val := v.(type) {
		case string:
			if val != "" && !containsStr(allowed, val) {
				errs = append(errs, fmt.Sprintf("%s: %q is not one of %v", dotted, val, allowed))
			}
		case map[string]any:
			for k, vv := range val {
				sv, ok := vv.(string)
				if !ok {
					errs = append(errs, fmt.Sprintf("%s.%s: expected a string, got %T", dotted, k, vv))
					continue
				}
				if sv != "" && !containsStr(allowed, sv) {
					errs = append(errs, fmt.Sprintf("%s.%s: %q is not one of %v", dotted, k, sv, allowed))
				}
			}
		default:
			errs = append(errs, fmt.Sprintf("%s: expected a string or an object of strings, got %T", dotted, v))
		}
	}

	for dotted, prefix := range sch.PathKeys {
		v, ok := lookupPath(doc, dotted)
		if !ok {
			continue // optional key, absent
		}
		sv, ok := v.(string)
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: expected a string path, got %T", dotted, v))
			continue
		}
		if err := validatePathValue(sv, prefix); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", dotted, err))
		}
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// lookupPath walks doc via dotted (e.g. "journal.dir"), returning the value
// found and whether every step of the path existed as an object. A partial
// path through a non-object value (e.g. "log_level.dir" where log_level is
// a string) reports not-found rather than panicking or erroring — schema
// authors are trusted; a malformed dotted key in a schema file itself would
// just silently never match, which loadConfigSchemas's init-time decode
// already guards isn't the case for keys this task actually wrote.
func lookupPath(doc map[string]any, dotted string) (any, bool) {
	var cur any = doc
	for _, part := range strings.Split(dotted, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok := m[part]
		if !ok {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

// validatePathValue enforces §4.5 point 3b's path constraint: value, once
// path.Clean'd, must be under requiredPrefix (an exact required prefix, e.g.
// "/etc/lexa/mqtt/") or — when requiredPrefix is empty — under the generic
// allowlist (/etc/lexa/ or /var/lib/lexa/). An empty value is always
// accepted: it is the pervasive "disabled/anonymous" sentinel this
// codebase's configs already use throughout (empty mqtt_pass_file ⇒
// anonymous MQTT, empty cert_path ⇒ plain-WS bench mode, etc.) — treating it
// as a violation would reject every factory/bench config that intentionally
// leaves an optional path field unset.
//
// The explicit ".." containment check below is defense-in-depth alongside
// the prefix check, not a replacement for it: path.Clean fully resolves ".."
// in an ABSOLUTE path (so "/etc/lexa/../../etc/passwd" cleans straight to
// "/etc/passwd", already caught by the prefix check with no residual ".."
// to find), but a RELATIVE value like "../../etc/passwd" retains its
// leading ".." components after Clean — both cases are rejected here, the
// first by the prefix check, the second by either check.
func validatePathValue(value, requiredPrefix string) error {
	if value == "" {
		return nil
	}
	cleaned := path.Clean(value)
	if strings.Contains(cleaned, "..") {
		return fmt.Errorf("path %q escapes its base directory", value)
	}
	if requiredPrefix != "" {
		if !strings.HasPrefix(cleaned, requiredPrefix) {
			return fmt.Errorf("path %q must be under %s", value, requiredPrefix)
		}
		return nil
	}
	if strings.HasPrefix(cleaned, "/etc/lexa/") || strings.HasPrefix(cleaned, "/var/lib/lexa/") {
		return nil
	}
	return fmt.Errorf("path %q must be under /etc/lexa/ or /var/lib/lexa/", value)
}

func containsStr(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// writeStagedThenRename implements §4.5 point 4's staged-write discipline —
// the same write→fsync→rename shape internal/journal's rotateFiles and
// cmd/lexa-migrate/migrate.go's writeStaged both already use: path is NEVER
// opened for an in-place write, so a crash mid-write can never leave it
// torn. A crash between the fsync'd write and the rename leaves
// path+".staged" behind; unlike lexa-migrate's recoverStaged, nothing here
// reads a leftover .staged back on the next request — it is simply
// overwritten (open/create/truncate) by this same function the next time
// this path is written, and a request that never comes back to this path
// leaves inert, never-executed bytes on disk, not a live hazard.
func writeStagedThenRename(targetPath string, data []byte, mode os.FileMode) error {
	stagedPath := targetPath + ".staged"
	f, err := os.OpenFile(stagedPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("open %s: %w", stagedPath, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", stagedPath, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("fsync %s: %w", stagedPath, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", stagedPath, err)
	}
	// OpenFile's mode is masked by the process umask; chmod explicitly so
	// the file that's about to be renamed into place has EXACTLY the
	// requested mode (0640 for a config, 0600 for the api-secret), matching
	// cmd/lexa-migrate/migrate.go's writeFileLike's same reasoning.
	if err := os.Chmod(stagedPath, mode); err != nil {
		return fmt.Errorf("chmod %s: %w", stagedPath, err)
	}
	if err := os.Rename(stagedPath, targetPath); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", stagedPath, targetPath, err)
	}
	return nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// sha256OfFileOrEmpty hashes the current content of path, or sha256("") if
// the file does not exist yet (the "before" state for a service's very
// first commissioning write).
func sha256OfFileOrEmpty(targetPath string) (string, error) {
	data, err := os.ReadFile(targetPath)
	if errors.Is(err, os.ErrNotExist) {
		return sha256Hex(nil), nil
	}
	if err != nil {
		return "", err
	}
	return sha256Hex(data), nil
}
