package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCmdModeGet_Known(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mode": "gateway", "since": int64(1000), "actor": "root", "intent_id": "abc123",
		})
	}))
	defer srv.Close()

	var buf bytes.Buffer
	code := dispatchMode(newTestClient(t, srv, ""), []string{"get"}, &buf)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output: %s", code, buf.String())
	}
	out := buf.String()
	for _, want := range []string{"mode: gateway", "actor: root", "intent: abc123"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q; got:\n%s", want, out)
		}
	}
}

func TestCmdModeGet_UnknownIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unknown"})
	}))
	defer srv.Close()

	var buf bytes.Buffer
	code := dispatchMode(newTestClient(t, srv, ""), []string{"get"}, &buf)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (documented steady state); output: %s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "unknown") {
		t.Errorf("expected 'unknown' in output, got:\n%s", buf.String())
	}
}

func TestCmdModeGet_OtherHTTPErrorIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	code := dispatchMode(newTestClient(t, srv, ""), []string{"get"}, &buf)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

func TestCmdModeSet_UsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"no args", nil},
		{"invalid mode", []string{"set", "sideways"}},
		{"too many args", []string{"set", "optimizer", "extra"}},
		{"unknown subcommand", []string{"frobnicate"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			code := dispatchMode(&client{}, tt.args, &buf)
			if code != 2 {
				t.Errorf("exit code = %d, want 2; output: %s", code, buf.String())
			}
		})
	}
}

// TestCmdModeSet_OutcomeExitCodeMapping pins the outcome->exit-code table
// the unit brief calls for.
func TestCmdModeSet_OutcomeExitCodeMapping(t *testing.T) {
	tests := []struct {
		outcome    string
		httpStatus int
		wantExit   int
	}{
		{"applied", http.StatusOK, 0},
		{"clamped", http.StatusOK, 0},
		{"duplicate", http.StatusOK, 0},
		{"rejected", http.StatusOK, 1},
		{"expired", http.StatusOK, 1},
		{"pending", http.StatusAccepted, 0},
	}
	for _, tt := range tests {
		t.Run(tt.outcome, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Kind string `json:"kind"`
					Body struct {
						Mode string `json:"mode"`
					} `json:"body"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				if req.Kind != "mode" {
					t.Errorf("kind = %q, want mode", req.Kind)
				}
				if req.Body.Mode != "gateway" {
					t.Errorf("body.mode = %q, want gateway", req.Body.Mode)
				}
				w.WriteHeader(tt.httpStatus)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "xyz", "kind": "mode", "outcome": tt.outcome,
				})
			}))
			defer srv.Close()

			var buf bytes.Buffer
			code := cmdModeSet(newTestClient(t, srv, ""), []string{"gateway"}, &buf)
			if code != tt.wantExit {
				t.Errorf("exit code = %d, want %d; output: %s", code, tt.wantExit, buf.String())
			}
			if !strings.Contains(buf.String(), "outcome: "+tt.outcome) {
				t.Errorf("output missing outcome line, got:\n%s", buf.String())
			}
		})
	}
}

func TestCmdModeSet_HTTPErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	code := cmdModeSet(newTestClient(t, srv, ""), []string{"optimizer"}, &buf)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

func TestCmdModeSet_JSONPassthrough(t *testing.T) {
	const raw = `{"id":"x","kind":"mode","outcome":"applied"}` + "\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(raw))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	code := cmdModeSet(newTestClient(t, srv, ""), []string{"gateway", "-json"}, &buf)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if buf.String() != raw {
		t.Errorf("output = %q, want raw passthrough %q", buf.String(), raw)
	}
}
