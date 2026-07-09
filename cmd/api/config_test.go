package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// WS-1 (V1.0 punch list, security fail-closed by default): loadConfig must
// refuse a non-loopback listen_addr with no api_token_file configured unless
// bench:true is set. Loopback binds and bench-mode binds are unaffected.

func writeTempAPIConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &probe); err != nil {
		t.Fatalf("test fixture is not valid JSON: %v", err)
	}
	return path
}

func TestLoadConfig_DefaultListenAddrIsLoopback(t *testing.T) {
	path := writeTempAPIConfig(t, `{"mqtt_broker": "tcp://localhost:1883"}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:9100" {
		t.Fatalf("ListenAddr = %q, want the WS-1 loopback default %q", cfg.ListenAddr, "127.0.0.1:9100")
	}
}

func TestLoadConfig_NonLoopback_NoToken_NoBench_Fails(t *testing.T) {
	path := writeTempAPIConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"listen_addr": ":9100"
	}`)
	_, err := loadConfig(path)
	if err == nil {
		t.Fatal("loadConfig succeeded binding non-loopback with no api_token_file and no bench profile; want a fail-closed error")
	}
}

func TestLoadConfig_NonLoopback_WithToken_Succeeds(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "api.token")
	if err := os.WriteFile(tokenPath, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeTempAPIConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"listen_addr": ":9100",
		"api_token_file": "`+tokenPath+`"
	}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig with api_token_file configured failed: %v", err)
	}
	if cfg.APITokenFile == "" {
		t.Fatal("APITokenFile not carried through")
	}
}

func TestLoadConfig_Loopback_NoToken_Succeeds(t *testing.T) {
	path := writeTempAPIConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"listen_addr": "127.0.0.1:9100"
	}`)
	if _, err := loadConfig(path); err != nil {
		t.Fatalf("loadConfig with loopback listen_addr + no token failed: %v", err)
	}
}

func TestLoadConfig_DefaultLoopback_NoToken_Succeeds(t *testing.T) {
	// listen_addr entirely absent ⇒ defaults to the loopback bind above.
	path := writeTempAPIConfig(t, `{"mqtt_broker": "tcp://localhost:1883"}`)
	if _, err := loadConfig(path); err != nil {
		t.Fatalf("loadConfig with default listen_addr + no token failed: %v", err)
	}
}

func TestLoadConfig_NonLoopback_NoToken_BenchTrue_Succeeds(t *testing.T) {
	path := writeTempAPIConfig(t, `{
		"mqtt_broker": "tcp://localhost:1883",
		"listen_addr": ":9100",
		"bench": true
	}`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig with bench:true failed: %v", err)
	}
	if !cfg.Bench {
		t.Fatal("cfg.Bench = false, want true")
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:9100", true},
		{"localhost:9100", true},
		{"[::1]:9100", true},
		{":9100", false},
		{"0.0.0.0:9100", false},
		{"69.0.0.1:9100", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isLoopbackAddr(c.addr); got != c.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}
