package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestEvalPlanHeartbeat(t *testing.T) {
	mk := func(state string) *statusPayload {
		sp := &statusPayload{}
		sp.PlanHeartbeat.State = state
		sp.PlanHeartbeat.AgeS = 1.5
		return sp
	}

	tests := []struct {
		name         string
		sp           *statusPayload
		commissioned bool
		want         Status
	}{
		{"ok always passes, commissioned", mk("ok"), true, StatusPass},
		{"ok always passes, uncommissioned", mk("ok"), false, StatusPass},
		{"never + uncommissioned passes (idle by design)", mk("never"), false, StatusPass},
		{"never + commissioned fails", mk("never"), true, StatusFail},
		{"stalled fails regardless", mk("stalled"), false, StatusFail},
		{"stalled fails, commissioned", mk("stalled"), true, StatusFail},
		{"unknown state fails", mk("bogus"), false, StatusFail},
		{"empty state fails", mk(""), true, StatusFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalPlanHeartbeat(tt.sp, tt.commissioned)
			if got.Status != tt.want {
				t.Errorf("evalPlanHeartbeat(state=%q, commissioned=%v) = %v, want %v",
					tt.sp.PlanHeartbeat.State, tt.commissioned, got.Status, tt.want)
			}
		})
	}
}

func TestIsCommissioned(t *testing.T) {
	dir := t.TempDir()
	if isCommissioned(dir) {
		t.Fatalf("isCommissioned = true on a fresh temp dir, want false")
	}
	if err := os.WriteFile(filepath.Join(dir, "commissioned"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !isCommissioned(dir) {
		t.Fatalf("isCommissioned = false after creating the marker, want true")
	}
}

// statusServer spins up an httptest server serving a fixed statusPayload
// at /status, optionally requiring an exact bearer token.
func statusServer(t *testing.T, sp statusPayload, requireToken string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		if requireToken != "" && r.Header.Get("Authorization") != "Bearer "+requireToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sp)
	}))
}

func TestCheckPlanHeartbeat_Gather_Uncommissioned(t *testing.T) {
	sp := statusPayload{}
	sp.PlanHeartbeat.State = "never"
	srv := statusServer(t, sp, "")
	defer srv.Close()
	host, port := splitTestAddr(t, srv.URL)

	dir := t.TempDir()
	writeAPIJSON(t, dir, host+":"+port)
	// no `commissioned` marker created

	env := &Environment{ConfigDir: dir, HTTPClient: newProbeHTTPClient(), APIScheme: "http"}
	res := checkPlanHeartbeat(context.Background(), env)
	if res.Status != StatusPass {
		t.Fatalf("checkPlanHeartbeat = %+v, want PASS (never + uncommissioned)", res)
	}
}

func TestCheckPlanHeartbeat_Gather_CommissionedNeverFails(t *testing.T) {
	sp := statusPayload{}
	sp.PlanHeartbeat.State = "never"
	srv := statusServer(t, sp, "")
	defer srv.Close()
	host, port := splitTestAddr(t, srv.URL)

	dir := t.TempDir()
	writeAPIJSON(t, dir, host+":"+port)
	if err := os.WriteFile(filepath.Join(dir, "commissioned"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	env := &Environment{ConfigDir: dir, HTTPClient: newProbeHTTPClient(), APIScheme: "http"}
	res := checkPlanHeartbeat(context.Background(), env)
	if res.Status != StatusFail {
		t.Fatalf("checkPlanHeartbeat = %+v, want FAIL (never + commissioned)", res)
	}
}

func TestCheckPlanHeartbeat_BearerTokenRead(t *testing.T) {
	sp := statusPayload{}
	sp.PlanHeartbeat.State = "ok"
	srv := statusServer(t, sp, "topsecret")
	defer srv.Close()
	host, port := splitTestAddr(t, srv.URL)

	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "api-token")
	if err := os.WriteFile(tokenFile, []byte("topsecret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"listen_addr":%q,"api_token_file":%q}`, host+":"+port, tokenFile)
	if err := os.WriteFile(filepath.Join(dir, "api.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	env := &Environment{ConfigDir: dir, HTTPClient: newProbeHTTPClient(), APIScheme: "http"}
	res := checkPlanHeartbeat(context.Background(), env)
	if res.Status != StatusPass {
		t.Fatalf("checkPlanHeartbeat with correct bearer token = %+v, want PASS", res)
	}
}

func TestCheckPlanHeartbeat_MissingTokenRejected(t *testing.T) {
	sp := statusPayload{}
	sp.PlanHeartbeat.State = "ok"
	srv := statusServer(t, sp, "topsecret")
	defer srv.Close()
	host, port := splitTestAddr(t, srv.URL)

	dir := t.TempDir()
	// api.json has NO api_token_file configured, but the server requires
	// one — checkPlanHeartbeat must surface the resulting 401 as FAIL, not
	// silently pass.
	writeAPIJSON(t, dir, host+":"+port)

	env := &Environment{ConfigDir: dir, HTTPClient: newProbeHTTPClient(), APIScheme: "http"}
	res := checkPlanHeartbeat(context.Background(), env)
	if res.Status != StatusFail {
		t.Fatalf("checkPlanHeartbeat without required token = %+v, want FAIL", res)
	}
}
