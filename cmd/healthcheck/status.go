package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// apiConfig is healthcheck's OWN minimal, read-only view of api.json's
// schema. cmd/api's Config (cmd/api/config.go) lives in a `package main` we
// cannot import — this is an intentional, documented duplication of
// exactly the two fields this tool needs. Keep the json tags in sync by
// hand if those field names ever change in cmd/api/config.go.
type apiConfig struct {
	ListenAddr   string `json:"listen_addr"`
	APITokenFile string `json:"api_token_file"`
}

// defaultAPIListenAddr mirrors cmd/api/config.go's loadConfig default.
const defaultAPIListenAddr = ":9100"

// loadAPIConfig reads <configDir>/api.json. A MISSING file falls back to
// the documented product default (":9100", no token) — consistent with
// cmd/api/config.go's own defaulting, and necessary so this tool still
// works before install-configs has run. A file that EXISTS but fails to
// parse is a real misconfiguration and is reported as an error (fail loud,
// matching the house convention in cmd/api/config.go's LoadAPIToken doc)
// rather than silently falling back.
func loadAPIConfig(configDir string) (apiConfig, error) {
	cfg := apiConfig{ListenAddr: defaultAPIListenAddr}
	data, err := os.ReadFile(filepath.Join(configDir, "api.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read api.json: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return apiConfig{}, fmt.Errorf("parse api.json: %w", err)
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = defaultAPIListenAddr
	}
	return cfg, nil
}

// apiHostPort splits a listen_addr like ":9100" / "0.0.0.0:9100" /
// "127.0.0.1:9100" into a dial target for a LOOPBACK liveness probe. An
// empty or 0.0.0.0 host means "bound on every interface", so 127.0.0.1 is
// always the correct probe target regardless of what the config says
// (spec: "GET https://127.0.0.1:9100/healthz").
func apiHostPort(listenAddr string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(listenAddr)
	if err != nil {
		return "", "", fmt.Errorf("parse listen_addr %q: %w", listenAddr, err)
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	return host, port, nil
}

// loadAPIToken reads the bearer token file api.json points at, mirroring
// cmd/api/config.go's Config.LoadAPIToken semantics exactly: unset ⇒
// ("", nil) (no auth — today's default, and every factory/pre-rollout
// box); set-but-unreadable-or-empty ⇒ error (a real misconfiguration, not
// silently open and not silently sending a garbage token forever).
func loadAPIToken(tokenFile string) (string, error) {
	if tokenFile == "" {
		return "", nil
	}
	data, err := os.ReadFile(tokenFile)
	if err != nil {
		return "", fmt.Errorf("read api_token_file %s: %w", tokenFile, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("api_token_file %s is configured but empty", tokenFile)
	}
	return token, nil
}

// statusPayload is healthcheck's decode target for GET /status — a subset
// of cmd/api/handlers.go's statusResp, limited to the fields the plan/
// northbound/modbus checks need. Same duplication caveat as apiConfig.
type statusPayload struct {
	CSIPPrograms  int `json:"csip_programs"`
	PlanHeartbeat struct {
		State string  `json:"state"`
		AgeS  float64 `json:"age_s"`
	} `json:"plan_heartbeat"`
	Devices map[string]struct {
		Connected bool `json:"connected"`
	} `json:"devices"`
	StaleSources []string `json:"stale_sources"`
}

// fetchStatus GETs /status from the local lexa-api (scheme fallback per
// probeGET) and decodes it into statusPayload. token, if non-empty, is
// sent as "Authorization: Bearer <token>" (api.json's api_token_file, when
// configured — /status enforces it when set, unlike /healthz).
func fetchStatus(ctx context.Context, env *Environment, host, port, token string) (*statusPayload, string, error) {
	var headers map[string]string
	if token != "" {
		headers = map[string]string{"Authorization": "Bearer " + token}
	}
	res, err := probeGET(ctx, env.HTTPClient, env.APIScheme, host, port, "/status", headers)
	if err != nil {
		return nil, "", fmt.Errorf("GET /status: %w", err)
	}
	if res.StatusCode != 200 {
		return nil, res.Scheme, fmt.Errorf("GET /status: HTTP %d", res.StatusCode)
	}
	var sp statusPayload
	if err := json.Unmarshal(res.Body, &sp); err != nil {
		return nil, res.Scheme, fmt.Errorf("decode /status: %w", err)
	}
	return &sp, res.Scheme, nil
}
