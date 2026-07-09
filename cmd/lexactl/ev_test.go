package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseDeparture(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		input   string
		want    int64 // 0 when wantErr
		wantErr bool
	}{
		{"RFC3339 in the future", "2026-07-09T18:00:00Z", now.Add(6 * time.Hour).Unix(), false},
		{"RFC3339 in the past", "2026-07-09T06:00:00Z", 0, true},
		{"RFC3339 exactly now", now.Format(time.RFC3339), 0, true},
		{"+duration hours", "+2h", now.Add(2 * time.Hour).Unix(), false},
		{"+duration minutes", "+90m", now.Add(90 * time.Minute).Unix(), false},
		{"+duration zero", "+0h", 0, true},
		{"+duration negative", "+-2h", 0, true},
		{"+duration malformed", "+banana", 0, true},
		{"empty string", "", 0, true},
		{"garbage (neither form)", "next tuesday", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDeparture(tt.input, now)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseDeparture(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("parseDeparture(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestCmdEVGoal_UsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"missing target-kwh", []string{"--departure", "+2h"}},
		{"missing departure", []string{"--target-kwh", "10"}},
		{"extra positional arg", []string{"--target-kwh", "10", "--departure", "+2h", "extra"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			code := cmdEVGoal(&client{}, tt.args, &buf)
			if code != 2 {
				t.Errorf("exit code = %d, want 2; output: %s", code, buf.String())
			}
		})
	}
}

func TestCmdEVGoal_ValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"negative target-kwh", []string{"--target-kwh", "-5", "--departure", "+2h"}},
		{"past departure", []string{"--target-kwh", "10", "--departure", "2000-01-01T00:00:00Z"}},
		{"negative initial-kwh", []string{"--target-kwh", "10", "--departure", "+2h", "--initial-kwh", "-1"}},
		{"malformed departure", []string{"--target-kwh", "10", "--departure", "not-a-time"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			code := cmdEVGoal(&client{}, tt.args, &buf)
			if code != 1 {
				t.Errorf("exit code = %d, want 1; output: %s", code, buf.String())
			}
		})
	}
}

func TestCmdEVGoal_Valid(t *testing.T) {
	var gotBody struct {
		TargetSocKwh  float64 `json:"target_soc_kwh"`
		DepartureUnix int64   `json:"departure_unix"`
		InitialSocKwh float64 `json:"initial_soc_kwh"`
		StationID     string  `json:"station_id"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Kind string          `json:"kind"`
			Body json.RawMessage `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Kind != "evgoal" {
			t.Errorf("kind = %q, want evgoal", req.Kind)
		}
		_ = json.Unmarshal(req.Body, &gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "1", "outcome": "applied"})
	}))
	defer srv.Close()

	var buf bytes.Buffer
	code := cmdEVGoal(newTestClient(t, srv, ""),
		[]string{"--target-kwh", "40", "--departure", "+2h", "--initial-kwh", "5", "--station", "evse1"}, &buf)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output: %s", code, buf.String())
	}
	if gotBody.TargetSocKwh != 40 {
		t.Errorf("target_soc_kwh = %v, want 40", gotBody.TargetSocKwh)
	}
	if gotBody.InitialSocKwh != 5 {
		t.Errorf("initial_soc_kwh = %v, want 5", gotBody.InitialSocKwh)
	}
	if gotBody.StationID != "evse1" {
		t.Errorf("station_id = %v, want evse1", gotBody.StationID)
	}
	wantMin := time.Now().Add(90 * time.Minute).Unix()
	wantMax := time.Now().Add(150 * time.Minute).Unix()
	if gotBody.DepartureUnix < wantMin || gotBody.DepartureUnix > wantMax {
		t.Errorf("departure_unix = %d, want roughly now+2h", gotBody.DepartureUnix)
	}
}

func TestCmdEVGoal_InitialKwhOmittedWhenNotSet(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Body json.RawMessage `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.Unmarshal(req.Body, &gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "1", "outcome": "applied"})
	}))
	defer srv.Close()

	var buf bytes.Buffer
	cmdEVGoal(newTestClient(t, srv, ""), []string{"--target-kwh", "40", "--departure", "+2h"}, &buf)
	if _, present := gotBody["initial_soc_kwh"]; present {
		t.Errorf("initial_soc_kwh should be omitted when --initial-kwh wasn't passed, got %v", gotBody)
	}
}

func TestCmdEVChargeNow_TTLRequired(t *testing.T) {
	var buf bytes.Buffer
	code := cmdEVChargeNow(&client{}, nil, &buf)
	if code != 2 {
		t.Errorf("exit code = %d, want 2 (usage: --ttl required); output: %s", code, buf.String())
	}
}

func TestCmdEVChargeNow_InvalidTTL(t *testing.T) {
	tests := []string{"not-a-duration", "0s", "-5m"}
	for _, ttl := range tests {
		t.Run(ttl, func(t *testing.T) {
			var buf bytes.Buffer
			code := cmdEVChargeNow(&client{}, []string{"--ttl", ttl}, &buf)
			if code != 1 {
				t.Errorf("exit code = %d, want 1; output: %s", code, buf.String())
			}
		})
	}
}

func TestCmdEVChargeNow_Valid(t *testing.T) {
	var gotBody struct {
		TTLS int `json:"ttl_s"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Kind string          `json:"kind"`
			Body json.RawMessage `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Kind != "chargenow" {
			t.Errorf("kind = %q, want chargenow", req.Kind)
		}
		_ = json.Unmarshal(req.Body, &gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "1", "outcome": "applied"})
	}))
	defer srv.Close()

	var buf bytes.Buffer
	code := cmdEVChargeNow(newTestClient(t, srv, ""), []string{"--ttl", "90m"}, &buf)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output: %s", code, buf.String())
	}
	if gotBody.TTLS != 5400 {
		t.Errorf("ttl_s = %d, want 5400", gotBody.TTLS)
	}
}

func TestCmdEVChargeNow_RejectedOutcome(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "1", "outcome": "rejected", "detail": "no station configured"})
	}))
	defer srv.Close()

	var buf bytes.Buffer
	code := cmdEVChargeNow(newTestClient(t, srv, ""), []string{"--ttl", "5m"}, &buf)
	if code != 1 {
		t.Errorf("exit code = %d, want 1; output: %s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "no station configured") {
		t.Errorf("expected detail in output, got:\n%s", buf.String())
	}
}
