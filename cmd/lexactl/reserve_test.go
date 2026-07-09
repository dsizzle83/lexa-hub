package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCmdReserveSet_UsageErrors(t *testing.T) {
	tests := [][]string{nil, {"set"}, {"set", "10", "extra"}}
	for _, args := range tests {
		var buf bytes.Buffer
		code := dispatchReserve(&client{}, args, &buf)
		if code != 2 {
			t.Errorf("dispatchReserve(%v): exit code = %d, want 2; output: %s", args, code, buf.String())
		}
	}
}

func TestCmdReserveSet_ValidationErrors(t *testing.T) {
	tests := []string{"not-a-number", "-5", "150"}
	for _, pct := range tests {
		t.Run(pct, func(t *testing.T) {
			var buf bytes.Buffer
			code := dispatchReserve(&client{}, []string{"set", pct}, &buf)
			if code != 1 {
				t.Errorf("exit code = %d, want 1; output: %s", code, buf.String())
			}
		})
	}
}

func TestCmdReserveSet_Valid(t *testing.T) {
	var gotBody struct {
		ReservePct float64 `json:"reserve_pct"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Kind string          `json:"kind"`
			Body json.RawMessage `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Kind != "reserve" {
			t.Errorf("kind = %q, want reserve", req.Kind)
		}
		_ = json.Unmarshal(req.Body, &gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "1", "outcome": "clamped", "detail": "raised to floor 20"})
	}))
	defer srv.Close()

	var buf bytes.Buffer
	code := dispatchReserve(newTestClient(t, srv, ""), []string{"set", "35.5"}, &buf)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output: %s", code, buf.String())
	}
	if gotBody.ReservePct != 35.5 {
		t.Errorf("reserve_pct = %v, want 35.5", gotBody.ReservePct)
	}
}
