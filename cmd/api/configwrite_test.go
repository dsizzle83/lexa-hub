package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"lexa-hub/internal/journal"
	"lexa-hub/internal/metrics"
)

// --- test helpers -----------------------------------------------------

// useTempConfigWriteDir points configWriteDir at a fresh temp directory for
// the duration of the test (configWriteDir is a var exactly for this,
// mirroring mdns.go's commissionedMarkerPath convention).
func useTempConfigWriteDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	orig := configWriteDir
	configWriteDir = dir
	t.Cleanup(func() { configWriteDir = orig })
	return dir
}

// useTempCommissionDir points commissionDir (unit 4.5's restart-request/
// result file directory) at a fresh temp directory for the duration of the
// test — mirrors useTempConfigWriteDir's convention exactly.
func useTempCommissionDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	orig := commissionDir
	commissionDir = dir
	t.Cleanup(func() { commissionDir = orig })
	return dir
}

// writeCommissionResult writes commissionDir/restart.result directly (as
// scripts/lexa-commission-apply would, in production) so a test can hand
// defaultRestartRunner's poll loop a scripted result without a real script
// or systemctl anywhere nearby.
func writeCommissionResult(t *testing.T, ts int64, results map[string]string) {
	t.Helper()
	body, err := json.Marshal(commissionResult{TS: ts, Results: results})
	if err != nil {
		t.Fatalf("marshal commissionResult: %v", err)
	}
	if err := os.WriteFile(filepath.Join(commissionDir, commissionResultFile), body, 0o640); err != nil {
		t.Fatalf("write restart.result: %v", err)
	}
}

// useUncommissioned points commissionedMarkerPath at a path that does not
// exist — the gate-open state.
func useUncommissioned(t *testing.T) {
	t.Helper()
	orig := commissionedMarkerPath
	commissionedMarkerPath = filepath.Join(t.TempDir(), "commissioned")
	t.Cleanup(func() { commissionedMarkerPath = orig })
}

// useCommissioned points commissionedMarkerPath at a path that DOES exist —
// the gate-closed state.
func useCommissioned(t *testing.T) {
	t.Helper()
	orig := commissionedMarkerPath
	dir := t.TempDir()
	commissionedMarkerPath = filepath.Join(dir, "commissioned")
	if err := os.WriteFile(commissionedMarkerPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { commissionedMarkerPath = orig })
}

// noopRestartRunner never actually execs anything — the default double for
// tests that don't care about the restart outcome, but DO fail loudly if
// invoked with a service this test never expected to be restarted (e.g.
// api-secret).
func noopRestartRunner(unit string) (bool, string) { return true, "restarted " + unit }

// recordingRestartRunner records every unit it was asked to restart and
// returns a scripted (ok, detail) pair — the "restart exec faked via a
// runner seam" test double the task brief calls for.
type recordingRestartRunner struct {
	ok     bool
	detail string
	calls  []string
}

func (r *recordingRestartRunner) run(unit string) (bool, string) {
	r.calls = append(r.calls, unit)
	return r.ok, r.detail
}

// newTestJournal opens a real journal.Writer rooted at a temp dir, for tests
// that assert on the NDJSON it wrote back (journal.Scan).
func newTestJournal(t *testing.T) (*journal.Writer, string) {
	t.Helper()
	dir := t.TempDir()
	jw, err := journal.Open(journal.Config{Dir: dir})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	t.Cleanup(func() { jw.Close() })
	return jw, dir
}

// readConfigWriteEvents scans dir's default-named journal file for every
// config_write event, decoding each Data payload into a journal.ConfigWrite.
func readConfigWriteEvents(t *testing.T, dir string) []journal.ConfigWrite {
	t.Helper()
	var out []journal.ConfigWrite
	_, err := journal.Scan(dir, journal.DefaultName, func(e journal.Event) error {
		if e.Type != journal.TypeConfigWrite {
			return nil
		}
		var p journal.ConfigWrite
		if err := json.Unmarshal(e.Data, &p); err != nil {
			return err
		}
		out = append(out, p)
		return nil
	})
	if err != nil {
		t.Fatalf("journal.Scan: %v", err)
	}
	return out
}

// counterValue extracts a counter's exposed value from the registry's
// Prometheus text-exposition Format() output — internal/metrics.Counter's
// underlying accessor is unexported (see cmd/northbound/rotate_test.go's
// identical helper), so this package reads it the same way a real scrape
// target would.
func counterValue(t *testing.T, reg *metrics.Registry, name string) string {
	t.Helper()
	for _, line := range strings.Split(reg.Format(), "\n") {
		if strings.HasPrefix(line, name+" ") {
			return strings.TrimPrefix(line, name+" ")
		}
	}
	return "<absent>"
}

// loadRealConfigFixture reads this repo's own configs/<service>.json — a
// realistic, currently-shipping example config — and decodes it into a
// generic map, exactly the shape configWriteHandler validates against.
func loadRealConfigFixture(t *testing.T, service string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "configs", service+".json"))
	if err != nil {
		t.Fatalf("read fixture configs/%s.json: %v", service, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse fixture configs/%s.json: %v", service, err)
	}
	return doc
}

func marshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func postConfig(t *testing.T, h http.HandlerFunc, service string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/config/"+service, strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// --- gate ---------------------------------------------------------------

// TestConfigWriteHandler_GateCommissionedRejects403 pins §9's fail-closed
// gate: a commissioned unit refuses EVERY config write, before even looking
// at the body.
func TestConfigWriteHandler_GateCommissionedRejects403(t *testing.T) {
	useCommissioned(t)
	dir := useTempConfigWriteDir(t)
	rr := &recordingRestartRunner{ok: true, detail: "restarted"}
	h := configWriteHandler("", nil, rr.run, nil, nil)

	rec := postConfig(t, h, "hub", []byte(`{"mqtt_broker":"tcp://localhost:1883"}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if len(rr.calls) != 0 {
		t.Fatalf("restart runner called %v, want no calls when gate refuses", rr.calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "hub.json")); !os.IsNotExist(err) {
		t.Fatalf("hub.json exists after a gate-refused write: %v", err)
	}
}

// TestConfigWriteHandler_GateAbsentAllowsWrite is the full happy path:
// uncommissioned + a valid body must 200, write the file, journal the
// event, and restart the unit.
func TestConfigWriteHandler_GateAbsentAllowsWrite(t *testing.T) {
	useUncommissioned(t)
	dir := useTempConfigWriteDir(t)
	jw, jdir := newTestJournal(t)
	rr := &recordingRestartRunner{ok: true, detail: "restarted lexa-hub"}
	reg := metrics.New()
	writes := reg.Counter("writes_total")
	rejects := reg.Counter("rejects_total")
	h := configWriteHandler("", jw, rr.run, writes, rejects)

	doc := loadRealConfigFixture(t, "hub")
	rec := postConfig(t, h, "hub", marshal(t, doc))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp configWriteResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Written {
		t.Error("Written = false, want true")
	}
	if !resp.Restarted {
		t.Errorf("Restarted = false, want true: detail=%q", resp.Detail)
	}
	if len(rr.calls) != 1 || rr.calls[0] != "lexa-hub" {
		t.Fatalf("restart calls = %v, want exactly [lexa-hub]", rr.calls)
	}

	written, err := os.ReadFile(filepath.Join(dir, "hub.json"))
	if err != nil {
		t.Fatalf("read written hub.json: %v", err)
	}
	var gotDoc map[string]any
	if err := json.Unmarshal(written, &gotDoc); err != nil {
		t.Fatalf("written hub.json is not valid JSON: %v", err)
	}
	if !reflect.DeepEqual(gotDoc, doc) {
		t.Fatalf("written config does not match posted body:\ngot:  %v\nwant: %v", gotDoc, doc)
	}

	jw.Close() // flush before Scan reads it back
	events := readConfigWriteEvents(t, jdir)
	if len(events) != 1 {
		t.Fatalf("journal has %d config_write events, want 1", len(events))
	}
	ev := events[0]
	if ev.Service != "hub" || ev.Actor != "local-api" {
		t.Errorf("journaled ConfigWrite = %+v, want service=hub actor=local-api", ev)
	}
	wantAfter := sha256Hex(marshalIndentNL(t, doc))
	if ev.AfterSHA != wantAfter {
		t.Errorf("AfterSHA = %q, want %q (sha256 of the exact bytes committed)", ev.AfterSHA, wantAfter)
	}
	wantBefore := sha256Hex(nil) // hub.json did not exist before this write
	if ev.BeforeSHA != wantBefore {
		t.Errorf("BeforeSHA = %q, want %q (sha256(\"\") — file did not exist)", ev.BeforeSHA, wantBefore)
	}

	if counterValue(t, reg, "writes_total") != "1" {
		t.Errorf("writes_total = %s, want 1", counterValue(t, reg, "writes_total"))
	}
	if counterValue(t, reg, "rejects_total") != "0" {
		t.Errorf("rejects_total = %s, want 0", counterValue(t, reg, "rejects_total"))
	}
}

// marshalIndentNL mirrors configWriteHandler's own re-encode step
// (json.MarshalIndent + trailing newline) so a test can compute the exact
// "after" bytes the handler commits, for an independent sha256 comparison.
func marshalIndentNL(t *testing.T, doc map[string]any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	return append(b, '\n')
}

// TestConfigWriteHandler_SecondWriteUsesPriorContentAsBeforeSHA pins that a
// SECOND write's BeforeSHA is the sha256 of whatever the FIRST write
// committed, not sha256("") again.
func TestConfigWriteHandler_SecondWriteUsesPriorContentAsBeforeSHA(t *testing.T) {
	useUncommissioned(t)
	useTempConfigWriteDir(t)
	jw, jdir := newTestJournal(t)
	h := configWriteHandler("", jw, noopRestartRunner, nil, nil)

	first := map[string]any{"mqtt_broker": "tcp://localhost:1883", "log_level": "info"}
	rec := postConfig(t, h, "hub", marshal(t, first))
	if rec.Code != http.StatusOK {
		t.Fatalf("first write status = %d: %s", rec.Code, rec.Body.String())
	}
	firstBytes := marshalIndentNL(t, first)

	second := map[string]any{"mqtt_broker": "tcp://localhost:1883", "log_level": "warn"}
	rec = postConfig(t, h, "hub", marshal(t, second))
	if rec.Code != http.StatusOK {
		t.Fatalf("second write status = %d: %s", rec.Code, rec.Body.String())
	}

	jw.Close()
	events := readConfigWriteEvents(t, jdir)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[1].BeforeSHA != sha256Hex(firstBytes) {
		t.Errorf("second write's BeforeSHA = %q, want sha256 of the first write's committed bytes %q",
			events[1].BeforeSHA, sha256Hex(firstBytes))
	}
}

// --- strict auth wiring ---------------------------------------------------

// TestConfigWriteHandler_WiredWithRequireBearerStrict pins that main.go's
// intended composition (requireBearerStrict wrapping configWriteHandler)
// fails closed with no token configured, same as /intent and POST /scan.
func TestConfigWriteHandler_WiredWithRequireBearerStrict(t *testing.T) {
	useUncommissioned(t)
	useTempConfigWriteDir(t)
	h := requireBearerStrict("", configWriteHandler("", nil, noopRestartRunner, nil, nil))

	rec := postConfig(t, h, "hub", []byte(`{"mqtt_broker":"tcp://localhost:1883"}`))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (no token configured must fail closed)", rec.Code)
	}
}

func TestConfigWriteHandler_WiredWithRequireBearerStrict_CorrectTokenPasses(t *testing.T) {
	useUncommissioned(t)
	useTempConfigWriteDir(t)
	h := requireBearerStrict("s3cret", configWriteHandler("", nil, noopRestartRunner, nil, nil))

	req := httptest.NewRequest(http.MethodPost, "/config/hub", strings.NewReader(`{"mqtt_broker":"tcp://localhost:1883"}`))
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// --- method/path dispatch -------------------------------------------------

func TestConfigWriteHandler_MethodAndPathDispatch(t *testing.T) {
	useUncommissioned(t)
	useTempConfigWriteDir(t)
	h := configWriteHandler("", nil, noopRestartRunner, nil, nil)

	t.Run("OPTIONS is a CORS preflight", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/config/hub", nil)
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rec.Code)
		}
	})

	t.Run("GET is method not allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/config/hub", nil)
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", rec.Code)
		}
	})

	t.Run("missing service name", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/config/", strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("extra path segment rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/config/hub/extra", strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("unknown service name", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/config/bogus", strings.NewReader(`{"mqtt_broker":"x"}`))
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
}

// --- schema rejection table ------------------------------------------------

// TestConfigWriteHandler_SchemaRejections covers every rejection class the
// task brief names: bad enum, an absolute path escape, a relative ".."
// traversal, a path-specific-prefix violation (mqtt_pass_file outside
// /etc/lexa/mqtt/), a non-object body, and a missing required key. Every
// case must leave NO config file on disk (staged-write never committed).
func TestConfigWriteHandler_SchemaRejections(t *testing.T) {
	cases := []struct {
		name    string
		service string
		body    string
	}{
		{
			name:    "bad enum (modbus reconciler)",
			service: "modbus",
			body:    `{"mqtt_broker":"tcp://localhost:1883","reconciler":{"battery":"bogus-mode"}}`,
		},
		{
			name:    "bad enum (scalar log_level)",
			service: "hub",
			body:    `{"mqtt_broker":"tcp://localhost:1883","log_level":"shout"}`,
		},
		{
			name:    "path escape to /etc/passwd",
			service: "modbus",
			body:    `{"mqtt_broker":"tcp://localhost:1883","mqtt_pass_file":"/etc/passwd"}`,
		},
		{
			name:    "relative .. traversal",
			service: "modbus",
			body:    `{"mqtt_broker":"tcp://localhost:1883","mqtt_pass_file":"../../etc/passwd"}`,
		},
		{
			name:    "mqtt_pass_file outside /etc/lexa/mqtt/ specifically",
			service: "modbus",
			body:    `{"mqtt_broker":"tcp://localhost:1883","mqtt_pass_file":"/etc/lexa/other/hub.pass"}`,
		},
		{
			name:    "non-object body (array)",
			service: "hub",
			body:    `[1,2,3]`,
		},
		{
			name:    "non-object body (string)",
			service: "hub",
			body:    `"just a string"`,
		},
		{
			name:    "non-object body (null)",
			service: "hub",
			body:    `null`,
		},
		{
			name:    "missing required mqtt_broker",
			service: "hub",
			body:    `{"log_level":"info"}`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			useUncommissioned(t)
			dir := useTempConfigWriteDir(t)
			rr := &recordingRestartRunner{ok: true, detail: "restarted"}
			reg := metrics.New()
			rejects := reg.Counter("rejects_total")
			h := configWriteHandler("", nil, rr.run, nil, rejects)

			rec := postConfig(t, h, c.service, []byte(c.body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if len(rr.calls) != 0 {
				t.Errorf("restart called %v, want no calls on a rejected write", rr.calls)
			}
			if _, err := os.Stat(filepath.Join(dir, c.service+".json")); !os.IsNotExist(err) {
				t.Errorf("%s.json exists after a rejected write", c.service)
			}
			if _, err := os.Stat(filepath.Join(dir, c.service+".json.staged")); !os.IsNotExist(err) {
				t.Errorf("%s.json.staged leaked after a rejected write", c.service)
			}
			if counterValue(t, reg, "rejects_total") != "1" {
				t.Errorf("rejects_total = %s, want 1", counterValue(t, reg, "rejects_total"))
			}
		})
	}
}

// TestConfigWriteHandler_OversizeBodyRejected pins the 256 KiB body cap.
func TestConfigWriteHandler_OversizeBodyRejected(t *testing.T) {
	useUncommissioned(t)
	dir := useTempConfigWriteDir(t)
	h := configWriteHandler("", nil, noopRestartRunner, nil, nil)

	huge := strings.Repeat("a", configWriteMaxBodyBytes+1024)
	body := `{"mqtt_broker":"` + huge + `"}`
	rec := postConfig(t, h, "hub", []byte(body))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "hub.json")); !os.IsNotExist(err) {
		t.Error("hub.json exists after an oversize-body rejection")
	}
}

// TestConfigWriteHandler_ValidPathValues pins that every real path value
// this repo's shipping fixtures actually use passes validation — a schema
// that's too strict would brick legitimate deploys.
func TestConfigWriteHandler_ValidPathValues(t *testing.T) {
	for _, svc := range []string{"hub", "northbound", "modbus", "ocpp", "telemetry", "cloudlink"} {
		t.Run(svc, func(t *testing.T) {
			useUncommissioned(t)
			useTempConfigWriteDir(t)
			h := configWriteHandler("", nil, noopRestartRunner, nil, nil)
			doc := loadRealConfigFixture(t, svc)
			rec := postConfig(t, h, svc, marshal(t, doc))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 for the repo's own shipping %s.json: %s", rec.Code, svc, rec.Body.String())
			}
		})
	}
}

// --- unknown-keys-preserved round trip -------------------------------------

// TestConfigWriteHandler_UnknownKeysPreserved pins §4.5 point 3c: a future
// key this schema doesn't know about yet must survive the write byte-for-byte
// (semantically — via re-decode, since key order/number formatting are not
// preserved, exactly like cmd/lexa-migrate/migrate.go's writeStaged already
// documents and accepts).
func TestConfigWriteHandler_UnknownKeysPreserved(t *testing.T) {
	useUncommissioned(t)
	dir := useTempConfigWriteDir(t)
	h := configWriteHandler("", nil, noopRestartRunner, nil, nil)

	doc := loadRealConfigFixture(t, "hub")
	doc["totally_future_key"] = "keep-me-please"
	doc["future_nested"] = map[string]any{"a": 1.0, "b": []any{"x", "y"}}

	rec := postConfig(t, h, "hub", marshal(t, doc))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	written, err := os.ReadFile(filepath.Join(dir, "hub.json"))
	if err != nil {
		t.Fatalf("read written hub.json: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(written, &got); err != nil {
		t.Fatalf("written hub.json invalid JSON: %v", err)
	}
	if got["totally_future_key"] != "keep-me-please" {
		t.Errorf("totally_future_key = %v, want it preserved", got["totally_future_key"])
	}
	if !reflect.DeepEqual(got, doc) {
		t.Fatalf("round trip lost or altered data:\ngot:  %v\nwant: %v", got, doc)
	}
}

// --- staged-rename atomicity ------------------------------------------------

// TestConfigWriteHandler_LeftoverStagedFileOverwrittenCleanly pins that a
// stale <service>.json.staged left behind by some earlier crash never leaks
// into the committed file — the next successful write's O_TRUNC clobbers it
// before the rename.
func TestConfigWriteHandler_LeftoverStagedFileOverwrittenCleanly(t *testing.T) {
	useUncommissioned(t)
	dir := useTempConfigWriteDir(t)
	h := configWriteHandler("", nil, noopRestartRunner, nil, nil)

	stagedPath := filepath.Join(dir, "hub.json.staged")
	if err := os.WriteFile(stagedPath, []byte("GARBAGE-FROM-A-CRASHED-PRIOR-WRITE"), 0o640); err != nil {
		t.Fatal(err)
	}

	doc := map[string]any{"mqtt_broker": "tcp://localhost:1883", "log_level": "info"}
	rec := postConfig(t, h, "hub", marshal(t, doc))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	written, err := os.ReadFile(filepath.Join(dir, "hub.json"))
	if err != nil {
		t.Fatalf("read hub.json: %v", err)
	}
	if strings.Contains(string(written), "GARBAGE") {
		t.Fatalf("committed hub.json contains the stale .staged garbage: %s", written)
	}
	var got map[string]any
	if err := json.Unmarshal(written, &got); err != nil {
		t.Fatalf("committed hub.json invalid JSON: %v", err)
	}
	if got["log_level"] != "info" {
		t.Errorf("committed content = %v, want the new write's content", got)
	}
	if _, err := os.Stat(stagedPath); !os.IsNotExist(err) {
		t.Error(".staged file left behind after a successful write")
	}
}

// TestConfigWriteHandler_RejectedWriteLeavesExistingFileUntouched pins that a
// schema-rejected write never disturbs a pre-existing, previously-committed
// config for that service.
func TestConfigWriteHandler_RejectedWriteLeavesExistingFileUntouched(t *testing.T) {
	useUncommissioned(t)
	dir := useTempConfigWriteDir(t)
	h := configWriteHandler("", nil, noopRestartRunner, nil, nil)

	good := map[string]any{"mqtt_broker": "tcp://localhost:1883", "log_level": "info"}
	rec := postConfig(t, h, "hub", marshal(t, good))
	if rec.Code != http.StatusOK {
		t.Fatalf("seed write failed: %d: %s", rec.Code, rec.Body.String())
	}
	before, err := os.ReadFile(filepath.Join(dir, "hub.json"))
	if err != nil {
		t.Fatal(err)
	}

	rec = postConfig(t, h, "hub", []byte(`{"mqtt_broker":"tcp://localhost:1883","log_level":"way-too-loud"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad write status = %d, want 400", rec.Code)
	}

	after, err := os.ReadFile(filepath.Join(dir, "hub.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("existing hub.json was modified by a rejected write:\nbefore: %s\nafter:  %s", before, after)
	}
}

// --- restart runner seam (success/fail/timeout) ----------------------------

func TestConfigWriteHandler_RestartOutcomes(t *testing.T) {
	cases := []struct {
		name        string
		ok          bool
		detail      string
		wantRestart bool
	}{
		{"success", true, "restarted lexa-hub", true},
		{"failure", false, "restart failed: exit status 1: unit not found", false},
		{"timeout", false, "restart of lexa-hub timed out after 15s", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			useUncommissioned(t)
			useTempConfigWriteDir(t)
			rr := &recordingRestartRunner{ok: c.ok, detail: c.detail}
			h := configWriteHandler("", nil, rr.run, nil, nil)

			rec := postConfig(t, h, "hub", []byte(`{"mqtt_broker":"tcp://localhost:1883"}`))
			// A restart failure is NEVER a write failure — the response is
			// always 200 with an honest restarted/detail pair.
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (restart outcome must not affect write status): %s", rec.Code, rec.Body.String())
			}
			var resp configWriteResp
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if !resp.Written {
				t.Error("Written = false, want true regardless of restart outcome")
			}
			if resp.Restarted != c.wantRestart {
				t.Errorf("Restarted = %v, want %v", resp.Restarted, c.wantRestart)
			}
			if resp.Detail != c.detail {
				t.Errorf("Detail = %q, want %q", resp.Detail, c.detail)
			}
		})
	}
}

// TestDefaultRestartRunner_FileProtocol exercises the REAL
// defaultRestartRunner (not the restartRunner function-value seam) against
// the unit 4.5 file protocol — commissionDir pointed at a temp directory,
// restartTimeout/restartPollInterval shrunk so the test runs fast. This
// replaces the old TestDefaultRestartRunner_ExecSeam (which exercised a
// sudo/systemctl subprocess directly): defaultRestartRunner no longer execs
// anything itself — it writes a request file and polls a result file that
// scripts/lexa-commission-apply would write in production, so these tests
// write that result file directly instead.
func TestDefaultRestartRunner_FileProtocol(t *testing.T) {
	origTimeout, origPoll := restartTimeout, restartPollInterval
	defer func() { restartTimeout, restartPollInterval = origTimeout, origPoll }()
	restartPollInterval = 10 * time.Millisecond

	t.Run("success", func(t *testing.T) {
		dir := useTempCommissionDir(t)
		restartTimeout = 2 * time.Second

		go func() {
			time.Sleep(20 * time.Millisecond)
			writeCommissionResult(t, time.Now().Unix()+1, map[string]string{"hub": "ok"})
		}()

		ok, detail := defaultRestartRunner("lexa-hub")
		if !ok {
			t.Fatalf("ok = false, want true: %s", detail)
		}
		if detail != "restarted lexa-hub" {
			t.Errorf("detail = %q, want %q", detail, "restarted lexa-hub")
		}

		reqBody, err := os.ReadFile(filepath.Join(dir, commissionRequestFile))
		if err != nil {
			t.Fatalf("read request file: %v", err)
		}
		if strings.TrimSpace(string(reqBody)) != "hub" {
			t.Errorf("request file = %q, want bare name %q", reqBody, "hub")
		}
	})

	t.Run("failure", func(t *testing.T) {
		useTempCommissionDir(t)
		restartTimeout = 2 * time.Second

		go func() {
			time.Sleep(20 * time.Millisecond)
			writeCommissionResult(t, time.Now().Unix()+1, map[string]string{"hub": "failed: exit 1: unit not found"})
		}()

		ok, detail := defaultRestartRunner("lexa-hub")
		if ok {
			t.Fatal("ok = true, want false for a failed result")
		}
		if !strings.Contains(detail, "unit not found") {
			t.Errorf("detail = %q, want it to include the script's failure detail", detail)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		useTempCommissionDir(t)
		restartTimeout = 60 * time.Millisecond
		// No result ever written — the root oneshot never ran (or hasn't
		// gotten to it yet); the poll must give up honestly.
		ok, detail := defaultRestartRunner("lexa-hub")
		if ok {
			t.Fatal("ok = true, want false on timeout")
		}
		if detail != "restart requested; result pending — poll /status or retry" {
			t.Errorf("detail = %q, want the pending-result message", detail)
		}
	})

	// STALE RESULTS MUST NEVER BE READ AS FRESH: a result file left over
	// from an EARLIER restart of the same service (ts before this call's
	// own request) must be ignored — proven by observing the runner is
	// STILL polling well after a stale entry already exists, and only
	// returns once a result with a ts at/after the request is written.
	t.Run("stale result from a prior cycle is ignored until a fresh one appears", func(t *testing.T) {
		dir := useTempCommissionDir(t)
		restartTimeout = 2 * time.Second
		writeCommissionResult(t, time.Now().Add(-time.Hour).Unix(), map[string]string{"hub": "ok"})

		type outcome struct {
			ok     bool
			detail string
		}
		resultCh := make(chan outcome, 1)
		go func() {
			ok, detail := defaultRestartRunner("lexa-hub")
			resultCh <- outcome{ok, detail}
		}()

		select {
		case r := <-resultCh:
			t.Fatalf("returned early (%+v) based on a stale result — request file: %s", r, mustReadFile(t, filepath.Join(dir, commissionRequestFile)))
		case <-time.After(80 * time.Millisecond):
			// expected: still polling past the stale entry
		}

		writeCommissionResult(t, time.Now().Unix()+1, map[string]string{"hub": "ok"})

		select {
		case r := <-resultCh:
			if !r.ok {
				t.Fatalf("ok = false after a fresh result, want true: %s", r.detail)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("did not pick up the fresh result before restartTimeout")
		}
	})
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return "<absent>"
	}
	return string(b)
}

// TestRequestRestart_ProtocolShape pins the request-file format itself:
// bare names (no "lexa-" prefix), one per line, appended across calls, and
// deduplicated for a name already pending.
func TestRequestRestart_ProtocolShape(t *testing.T) {
	dir := useTempCommissionDir(t)

	if err := requestRestart("hub"); err != nil {
		t.Fatalf("requestRestart(hub): %v", err)
	}
	got := mustReadFile(t, filepath.Join(dir, commissionRequestFile))
	if strings.TrimSpace(got) != "hub" {
		t.Fatalf("after one request, file = %q, want %q", got, "hub")
	}

	if err := requestRestart("northbound"); err != nil {
		t.Fatalf("requestRestart(northbound): %v", err)
	}
	got = mustReadFile(t, filepath.Join(dir, commissionRequestFile))
	fields := strings.Fields(got)
	if len(fields) != 2 || fields[0] != "hub" || fields[1] != "northbound" {
		t.Fatalf("after two requests, file = %q, want lines [hub northbound]", got)
	}

	// Re-requesting "hub" while it is still pending must not duplicate it.
	if err := requestRestart("hub"); err != nil {
		t.Fatalf("requestRestart(hub) again: %v", err)
	}
	got = mustReadFile(t, filepath.Join(dir, commissionRequestFile))
	fields = strings.Fields(got)
	if len(fields) != 2 {
		t.Fatalf("after a duplicate request, file = %q, want still exactly [hub northbound]", got)
	}
}

// --- api-secret rotation ----------------------------------------------------

// TestAPISecretRotation_WritesFileWithRestrictiveModeAndNoRestart pins the
// api-secret branch's whole shape: 0600 mode, the injected restart runner
// seam is NEVER called (api-secret doesn't use it — unchanged from before
// unit 4.5), and the specific "connection will drop" detail message. Unit
// 4.5 changed WHAT actually requests the restart (a commission-trigger
// file write, asserted below) and the detail text, but not the {Written:
// true, Restarted:false} shape or the "never call the injected runner"
// invariant.
func TestAPISecretRotation_WritesFileWithRestrictiveModeAndNoRestart(t *testing.T) {
	useUncommissioned(t)
	commissionDirTmp := useTempCommissionDir(t)
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "api-secret")
	if err := os.WriteFile(secretPath, []byte("old-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	calledRestart := false
	rr := func(unit string) (bool, string) { calledRestart = true; return true, "should never happen" }
	h := configWriteHandler(secretPath, nil, rr, nil, nil)

	rec := postConfig(t, h, "api-secret", []byte("new-secret-value\n"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if calledRestart {
		t.Fatal("the injected restart runner was called for api-secret — that seam is only for the generic six-service path")
	}
	var resp configWriteResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Written || resp.Restarted {
		t.Fatalf("resp = %+v, want {Written:true Restarted:false}", resp)
	}
	if resp.Detail != "restart requested via commissioning trigger; connection will drop" {
		t.Errorf("Detail = %q, want the commissioning-trigger message", resp.Detail)
	}

	info, err := os.Stat(secretPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("secret file mode = %v, want 0600", info.Mode().Perm())
	}
	got, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "new-secret-value" {
		t.Errorf("secret file content = %q, want %q", strings.TrimSpace(string(got)), "new-secret-value")
	}

	// The self-restart DOES go through the same commissioning-trigger file
	// the generic six-service path uses — this is the new part unit 4.5
	// adds: "api" is a legal bare name in restart.request even though it's
	// never a legal serviceUnit key.
	reqBody := mustReadFile(t, filepath.Join(commissionDirTmp, commissionRequestFile))
	if strings.TrimSpace(reqBody) != "api" {
		t.Errorf("commission request file = %q, want bare name %q", reqBody, "api")
	}
}

// TestAPISecretRotation_OldTokenStillWorksUntilRestart pins the DOCUMENTED
// restart-required semantics (configwrite.go's handleAPISecretWrite doc):
// main.go captures the bearer token ONCE at startup, so rotating the
// on-disk secret does not invalidate the OLD in-memory token for this
// process's lifetime — only a restart (picking up LoadAPIToken's fresh
// read) does. This is a deliberate, tested contract, not an oversight.
func TestAPISecretRotation_OldTokenStillWorksUntilRestart(t *testing.T) {
	useUncommissioned(t)
	useTempConfigWriteDir(t)
	useTempCommissionDir(t) // the api-secret rotation below now also requests a self-restart (unit 4.5)
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "api-secret")
	oldToken := "old-token-from-startup"
	if err := os.WriteFile(secretPath, []byte(oldToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// main.go's actual wiring: requireBearerStrict closes over the token
	// LOADED AT STARTUP, not a live re-read.
	wrapped := requireBearerStrict(oldToken, configWriteHandler(secretPath, nil, noopRestartRunner, nil, nil))

	// Rotate the secret on disk using the OLD token to authenticate (still
	// valid — this process hasn't restarted).
	req := httptest.NewRequest(http.MethodPost, "/config/api-secret", strings.NewReader("brand-new-secret\n"))
	req.Header.Set("Authorization", "Bearer "+oldToken)
	rec := httptest.NewRecorder()
	wrapped(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotation status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// The file on disk now holds the NEW secret...
	got, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "brand-new-secret" {
		t.Fatalf("secret file = %q, want the newly rotated value", strings.TrimSpace(string(got)))
	}

	// ...but THIS PROCESS (the still-running wrapped handler, standing in
	// for lexa-api before its next restart) still authenticates with the
	// OLD token, because main.go's requireBearerStrict never re-reads the
	// file. This is the restart-required contract, not a bug.
	req2 := httptest.NewRequest(http.MethodPost, "/config/hub", strings.NewReader(`{"mqtt_broker":"tcp://localhost:1883"}`))
	req2.Header.Set("Authorization", "Bearer "+oldToken)
	rec2 := httptest.NewRecorder()
	wrapped2 := requireBearerStrict(oldToken, configWriteHandler(secretPath, nil, noopRestartRunner, nil, nil))
	wrapped2(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("old token after rotation: status = %d, want 200 (restart-required semantics — old token must still work)", rec2.Code)
	}

	// A brand new process reading the file fresh (simulating the restart
	// the operator is asked to perform) WOULD pick up the new secret —
	// proven by simply reading it back above; LoadAPIToken's own behavior
	// (auth_test.go) already pins that a fresh read returns the current
	// file content trimmed.
}

// TestAPISecretRotation_NotConfiguredFailsClosed pins that with no
// api_token_file configured, the api-secret route 500s rather than writing
// to an empty/undefined path — defensive, since main.go's actual wiring
// makes this unreachable in practice (requireBearerStrict("", ...) always
// 401s before this handler ever runs when APITokenFile is unset).
func TestAPISecretRotation_NotConfiguredFailsClosed(t *testing.T) {
	useUncommissioned(t)
	h := configWriteHandler("", nil, noopRestartRunner, nil, nil)
	rec := postConfig(t, h, "api-secret", []byte("some-secret"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestAPISecretRotation_OversizeBodyRejected(t *testing.T) {
	useUncommissioned(t)
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "api-secret")
	h := configWriteHandler(secretPath, nil, noopRestartRunner, nil, nil)

	rec := postConfig(t, h, "api-secret", []byte(strings.Repeat("a", apiSecretMaxBodyBytes+10)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestAPISecretRotation_EmptyBodyRejected(t *testing.T) {
	useUncommissioned(t)
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "api-secret")
	h := configWriteHandler(secretPath, nil, noopRestartRunner, nil, nil)

	rec := postConfig(t, h, "api-secret", []byte("   \n"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a blank secret", rec.Code)
	}
}

// TestAPISecretRotation_Journaled pins that the api-secret path also
// journals a config_write event (service="api-secret") with correct shas.
func TestAPISecretRotation_Journaled(t *testing.T) {
	useUncommissioned(t)
	useTempCommissionDir(t) // the api-secret rotation now also requests a self-restart (unit 4.5)
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "api-secret")
	if err := os.WriteFile(secretPath, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	jw, jdir := newTestJournal(t)
	h := configWriteHandler(secretPath, jw, noopRestartRunner, nil, nil)

	rec := postConfig(t, h, "api-secret", []byte("new-value"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	jw.Close()

	events := readConfigWriteEvents(t, jdir)
	if len(events) != 1 {
		t.Fatalf("got %d config_write events, want 1", len(events))
	}
	ev := events[0]
	if ev.Service != "api-secret" {
		t.Errorf("Service = %q, want api-secret", ev.Service)
	}
	if ev.BeforeSHA != sha256Hex([]byte("old\n")) {
		t.Errorf("BeforeSHA = %q, want sha256 of the old file content", ev.BeforeSHA)
	}
	if ev.AfterSHA != sha256Hex([]byte("new-value\n")) {
		t.Errorf("AfterSHA = %q, want sha256 of the new (trimmed+newline) content", ev.AfterSHA)
	}
}

// --- path/enum validator unit tests -----------------------------------------

func TestValidatePathValue(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		prefix  string
		wantErr bool
	}{
		{"empty is always allowed", "", "/etc/lexa/mqtt/", false},
		{"generic allowlist /etc/lexa/", "/etc/lexa/certs/ca.pem", "", false},
		{"generic allowlist /var/lib/lexa/", "/var/lib/lexa/journal/hub", "", false},
		{"generic allowlist rejects /etc/passwd", "/etc/passwd", "", true},
		{"specific prefix satisfied", "/etc/lexa/mqtt/hub.pass", "/etc/lexa/mqtt/", false},
		{"specific prefix violated (generic-only path)", "/etc/lexa/other/hub.pass", "/etc/lexa/mqtt/", true},
		{"relative .. traversal", "../../etc/passwd", "", true},
		{"absolute .. resolves then fails prefix", "/etc/lexa/../../etc/passwd", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validatePathValue(c.value, c.prefix)
			if c.wantErr && err == nil {
				t.Errorf("validatePathValue(%q, %q) = nil, want an error", c.value, c.prefix)
			}
			if !c.wantErr && err != nil {
				t.Errorf("validatePathValue(%q, %q) = %v, want nil", c.value, c.prefix, err)
			}
		})
	}
}

func TestLookupPath(t *testing.T) {
	doc := map[string]any{
		"top": "value",
		"nested": map[string]any{
			"inner": "deep",
		},
	}
	if v, ok := lookupPath(doc, "top"); !ok || v != "value" {
		t.Errorf("top = %v, %v, want value, true", v, ok)
	}
	if v, ok := lookupPath(doc, "nested.inner"); !ok || v != "deep" {
		t.Errorf("nested.inner = %v, %v, want deep, true", v, ok)
	}
	if _, ok := lookupPath(doc, "nested.missing"); ok {
		t.Error("nested.missing: ok = true, want false")
	}
	if _, ok := lookupPath(doc, "top.cant-descend-into-a-string"); ok {
		t.Error("descending into a string leaf: ok = true, want false")
	}
	if _, ok := lookupPath(doc, "entirely.missing.path"); ok {
		t.Error("entirely.missing.path: ok = true, want false")
	}
}

// TestConfigSchemasLoadForEverySix pins that every one of the six
// serviceUnit keys has a corresponding embedded schema — a drift between
// the two maps is a fail-closed panic at package init (loadConfigSchemas),
// so simply reaching this test at all already proves it, but assert the
// key set explicitly too for a clearer failure message.
func TestConfigSchemasLoadForEverySix(t *testing.T) {
	for svc := range serviceUnit {
		if _, ok := configSchemas[svc]; !ok {
			t.Errorf("no configSchemas entry for service %q", svc)
		}
	}
	if len(configSchemas) != len(serviceUnit) {
		t.Errorf("configSchemas has %d entries, serviceUnit has %d", len(configSchemas), len(serviceUnit))
	}
}

// --- scripts/lexa-commission-apply smoke test (unit 4.5) -------------------
//
// These tests shell the REAL script (not a Go reimplementation of its
// logic) against a fake systemctl/logger on PATH and temp request/result/
// commissioned-marker paths, via the script's own
// LEXA_COMMISSION_REQUEST_FILE/LEXA_COMMISSION_RESULT_FILE/
// LEXA_COMMISSIONED_MARKER env overrides — the closest this package can
// get to proving the shell side of unit 4.5's protocol matches the Go
// side (commissionRequestFile/commissionResultFile/commissionResult
// above) without a live systemd or root.

// commissionApplyScriptPath resolves scripts/lexa-commission-apply's real
// path relative to this package's test working directory (cmd/api).
func commissionApplyScriptPath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "scripts", "lexa-commission-apply"))
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("scripts/lexa-commission-apply not found at %s: %v", path, err)
	}
	return path
}

// TestLexaCommissionApplyScript_ShSyntaxOK is the "sh -n" syntax check the
// task's VERIFY step calls for, pinned as a real go test so CI catches a
// syntax regression here the same way it catches a Go compile error
// anywhere else in this repo.
func TestLexaCommissionApplyScript_ShSyntaxOK(t *testing.T) {
	script := commissionApplyScriptPath(t)
	out, err := exec.Command("sh", "-n", script).CombinedOutput()
	if err != nil {
		t.Fatalf("sh -n %s: %v: %s", script, err, out)
	}
}

// writeFakeExecutable writes a POSIX sh script to dir/name with the given
// body appended after a shebang, mode 0755.
func writeFakeExecutable(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
}

// fakeSystemctlScript is a scripted double for `systemctl restart <unit>`:
// "lexa-northbound" fails (simulating a real systemctl error), every
// other "restart <unit>" succeeds, and every invocation (successful or
// not) is appended to $FAKE_SYSTEMCTL_LOG when set — so a test can assert
// exactly which units were restarted, in what order, without a real
// systemd anywhere nearby.
const fakeSystemctlScript = `
if [ -n "${FAKE_SYSTEMCTL_LOG:-}" ]; then
  echo "$*" >> "$FAKE_SYSTEMCTL_LOG"
fi
case "$1 $2" in
  "restart lexa-northbound")
    echo "Unit lexa-northbound.service not found." >&2
    exit 1
    ;;
  "restart "*)
    exit 0
    ;;
  *)
    echo "fake systemctl: unsupported invocation: $*" >&2
    exit 2
    ;;
esac
`

// TestLexaCommissionApplyScript_EndToEnd exercises the full protocol: a
// mixed request (a service that succeeds, one that fails, one not in the
// allowlist, and "api" itself submitted FIRST in the file to prove the
// script reorders it to run LAST), the commissioned-unit refusal gate,
// and the missing-request-file no-op.
func TestLexaCommissionApplyScript_EndToEnd(t *testing.T) {
	script := commissionApplyScriptPath(t)

	t.Run("mixed request: ok, failed, not-in-allowlist, api-last", func(t *testing.T) {
		fakeBin := t.TempDir()
		callsLog := filepath.Join(t.TempDir(), "systemctl-calls.log")
		writeFakeExecutable(t, fakeBin, "logger", "exit 0\n")
		writeFakeExecutable(t, fakeBin, "systemctl", fakeSystemctlScript)

		commissionDir := t.TempDir()
		reqPath := filepath.Join(commissionDir, commissionRequestFile)
		resultPath := filepath.Join(commissionDir, commissionResultFile)
		markerPath := filepath.Join(t.TempDir(), "commissioned") // absent -> uncommissioned

		// "api" listed FIRST on purpose: the script must still process it
		// LAST regardless of input order (see the script's header,
		// "ORDERING").
		if err := os.WriteFile(reqPath, []byte("api\nhub\nnorthbound\ntotally-bogus\n"), 0o640); err != nil {
			t.Fatal(err)
		}

		cmd := exec.Command("sh", script)
		cmd.Env = append(os.Environ(),
			"PATH="+fakeBin+":"+os.Getenv("PATH"),
			"LEXA_COMMISSION_REQUEST_FILE="+reqPath,
			"LEXA_COMMISSION_RESULT_FILE="+resultPath,
			"LEXA_COMMISSIONED_MARKER="+markerPath,
			"FAKE_SYSTEMCTL_LOG="+callsLog,
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("script failed: %v: %s", err, out)
		}

		if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
			t.Errorf("request file still exists after processing: %v", err)
		}

		resultBody, err := os.ReadFile(resultPath)
		if err != nil {
			t.Fatalf("read result file: %v", err)
		}
		var res commissionResult
		if err := json.Unmarshal(resultBody, &res); err != nil {
			t.Fatalf("result file is not valid JSON: %v\n%s", err, resultBody)
		}
		if res.TS <= 0 {
			t.Errorf("ts = %d, want a positive unix timestamp", res.TS)
		}
		want := map[string]string{
			"hub":           "ok",
			"api":           "ok",
			"totally-bogus": "failed: not in allowlist",
		}
		for name, wantStatus := range want {
			if got := res.Results[name]; got != wantStatus {
				t.Errorf("results[%q] = %q, want %q", name, got, wantStatus)
			}
		}
		if got := res.Results["northbound"]; !strings.Contains(got, "failed:") || !strings.Contains(got, "not found") {
			t.Errorf("results[northbound] = %q, want a failed: ... detail mentioning the fake systemctl's stderr", got)
		}

		callsRaw, err := os.ReadFile(callsLog)
		if err != nil {
			t.Fatalf("read fake systemctl calls log: %v", err)
		}
		calls := strings.Split(strings.TrimSpace(string(callsRaw)), "\n")
		if len(calls) != 3 {
			t.Fatalf("systemctl called %d times %v, want exactly 3 (hub, northbound, api — never the not-allowlisted name)", len(calls), calls)
		}
		if calls[len(calls)-1] != "restart lexa-api" {
			t.Errorf("last systemctl call = %q, want %q (api must be restarted LAST)", calls[len(calls)-1], "restart lexa-api")
		}
	})

	t.Run("commissioned unit refuses and never touches systemctl", func(t *testing.T) {
		fakeBin := t.TempDir()
		callsLog := filepath.Join(t.TempDir(), "systemctl-calls.log")
		writeFakeExecutable(t, fakeBin, "logger", "exit 0\n")
		writeFakeExecutable(t, fakeBin, "systemctl", fakeSystemctlScript)

		commissionDir := t.TempDir()
		reqPath := filepath.Join(commissionDir, commissionRequestFile)
		resultPath := filepath.Join(commissionDir, commissionResultFile)
		markerPath := filepath.Join(t.TempDir(), "commissioned")
		if err := os.WriteFile(markerPath, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(reqPath, []byte("hub\n"), 0o640); err != nil {
			t.Fatal(err)
		}

		cmd := exec.Command("sh", script)
		cmd.Env = append(os.Environ(),
			"PATH="+fakeBin+":"+os.Getenv("PATH"),
			"LEXA_COMMISSION_REQUEST_FILE="+reqPath,
			"LEXA_COMMISSION_RESULT_FILE="+resultPath,
			"LEXA_COMMISSIONED_MARKER="+markerPath,
			"FAKE_SYSTEMCTL_LOG="+callsLog,
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("script failed: %v: %s", err, out)
		}

		if _, err := os.Stat(reqPath); err != nil {
			t.Errorf("request file was removed/modified on a commissioned unit: %v", err)
		}
		if _, err := os.Stat(resultPath); !os.IsNotExist(err) {
			t.Errorf("result file was written on a commissioned unit: %v", err)
		}
		if _, err := os.Stat(callsLog); !os.IsNotExist(err) {
			t.Errorf("systemctl was invoked on a commissioned unit (calls log exists)")
		}
	})

	t.Run("missing request file exits cleanly", func(t *testing.T) {
		commissionDir := t.TempDir()
		reqPath := filepath.Join(commissionDir, commissionRequestFile) // never created
		resultPath := filepath.Join(commissionDir, commissionResultFile)
		markerPath := filepath.Join(t.TempDir(), "commissioned")

		cmd := exec.Command("sh", script)
		cmd.Env = append(os.Environ(),
			"LEXA_COMMISSION_REQUEST_FILE="+reqPath,
			"LEXA_COMMISSION_RESULT_FILE="+resultPath,
			"LEXA_COMMISSIONED_MARKER="+markerPath,
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("script failed on a missing request file: %v: %s", err, out)
		}
		if _, err := os.Stat(resultPath); !os.IsNotExist(err) {
			t.Errorf("result file was written despite no request ever existing: %v", err)
		}
	})
}
