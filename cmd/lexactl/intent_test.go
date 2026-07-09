package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCmdIntent_MissingJSONFlagIsUsageError(t *testing.T) {
	var buf bytes.Buffer
	code := cmdIntent(&client{}, []string{"tariff"}, &buf)
	if code != 2 {
		t.Errorf("exit code = %d, want 2; output: %s", code, buf.String())
	}
}

func TestCmdIntent_NoKindIsUsageError(t *testing.T) {
	var buf bytes.Buffer
	code := cmdIntent(&client{}, nil, &buf)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestCmdIntent_InvalidJSONBody(t *testing.T) {
	var buf bytes.Buffer
	code := cmdIntent(&client{}, []string{"tariff", "-json", "{not valid json"}, &buf)
	if code != 1 {
		t.Errorf("exit code = %d, want 1; output: %s", code, buf.String())
	}
}

func TestCmdIntent_PrintsVerbatimAndMapsOutcome(t *testing.T) {
	tests := []struct {
		name       string
		httpStatus int
		respBody   string
		wantExit   int
	}{
		{"applied", http.StatusOK, `{"id":"1","kind":"tariff","outcome":"applied"}` + "\n", 0},
		{"rejected", http.StatusOK, `{"id":"1","kind":"tariff","outcome":"rejected","detail":"bad rate"}` + "\n", 1},
		{"pending", http.StatusAccepted, `{"id":"1","outcome":"pending"}` + "\n", 0},
		{"unknown kind (plain text)", http.StatusBadRequest, "unknown intent kind \"bogus\"\n", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotKind string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Kind string          `json:"kind"`
					Body json.RawMessage `json:"body"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				gotKind = req.Kind
				w.WriteHeader(tt.httpStatus)
				_, _ = w.Write([]byte(tt.respBody))
			}))
			defer srv.Close()

			var buf bytes.Buffer
			code := cmdIntent(newTestClient(t, srv, ""), []string{"tariff", "-json", `{"currency":"USD"}`}, &buf)
			if code != tt.wantExit {
				t.Errorf("exit code = %d, want %d; output: %s", code, tt.wantExit, buf.String())
			}
			if gotKind != "tariff" {
				t.Errorf("server saw kind=%q, want tariff", gotKind)
			}
			if buf.String() != tt.respBody {
				t.Errorf("output = %q, want verbatim %q", buf.String(), tt.respBody)
			}
		})
	}
}

func TestCmdIntent_BodyPassesThroughByteForByte(t *testing.T) {
	// The user-supplied --json's CONTENT (keys, values, key order) must
	// reach the server unaltered — the escape hatch promises verbatim
	// passthrough of what the caller asked for, never a re-derived body.
	// encoding/json.RawMessage legitimately compacts insignificant
	// whitespace when embedding the value into the outer {kind,body}
	// envelope (its MarshalJSON output is run through json's own compact()
	// as part of any larger Marshal call) — that's a lossless, standard-
	// library-level transform, not a lexactl bug, so the expected value
	// here is the COMPACTED form of body, not its exact byte sequence.
	const body = `{  "mode" :   "gateway"  }`
	const wantCompact = `{"mode":"gateway"}`
	var gotRaw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Body json.RawMessage `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotRaw = strings.TrimSpace(string(req.Body))
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "1", "outcome": "applied"})
	}))
	defer srv.Close()

	var buf bytes.Buffer
	cmdIntent(newTestClient(t, srv, ""), []string{"mode", "-json", body}, &buf)
	if gotRaw != wantCompact {
		t.Errorf("server received body %q, want %q (compacted, content-preserved)", gotRaw, wantCompact)
	}
}
