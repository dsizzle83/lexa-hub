package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// factoryResetScript resolves scripts/factory-reset.sh relative to this test
// file, independent of the test's working directory.
func factoryResetScript(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// this file: <repo>/cmd/provision/factoryreset_test.go
	repo := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	p := filepath.Join(repo, "scripts", "factory-reset.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("factory-reset.sh not found at %s: %v", p, err)
	}
	return p
}

func TestFactoryReset_SyntaxCheck(t *testing.T) {
	script := factoryResetScript(t)
	out, err := exec.Command("sh", "-n", script).CombinedOutput()
	if err != nil {
		t.Fatalf("sh -n %s failed: %v\n%s", script, err, out)
	}
}

// TestFactoryReset_SandboxedPreservesIdentityAndPoP runs the real script
// against a temp sandbox (paths + service control overridden by env) and
// asserts the B4 acceptance: commissioned marker removed, re-provision window
// cleared, but /etc/lexa/identity AND /etc/lexa/provision-pop preserved.
func TestFactoryReset_SandboxedPreservesIdentityAndPoP(t *testing.T) {
	script := factoryResetScript(t)
	sb := t.TempDir()

	etc := filepath.Join(sb, "etc", "lexa")
	varLexa := filepath.Join(sb, "var", "lib", "lexa")
	run := filepath.Join(sb, "run", "lexa")
	factory := filepath.Join(sb, "factory")
	for _, d := range []string{
		filepath.Join(etc, "identity"),
		filepath.Join(etc, "certs"),
		filepath.Join(varLexa, "journal"),
		run,
		factory,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	mustWrite := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(filepath.Join(etc, "identity", "serial"), "LX93-IDENTITY\n")
	mustWrite(filepath.Join(etc, "provision-pop"), "MFG-POP-SECRET\n")
	mustWrite(filepath.Join(etc, "commissioned"), "")
	mustWrite(filepath.Join(etc, "api.json"), `{"stale":1}`)
	mustWrite(filepath.Join(etc, "certs", "client.pem"), "junk")
	mustWrite(filepath.Join(run, "provision-window"), "9999999999\n")
	mustWrite(filepath.Join(factory, "api.json"), `{"factory":1}`)

	cmd := exec.Command("sh", script, "--yes")
	cmd.Env = append(os.Environ(),
		"LEXA_ETC="+etc,
		"LEXA_VAR="+varLexa,
		"LEXA_RUN="+run,
		"LEXA_FACTORY_DIR="+factory,
		"LEXA_SKIP_SERVICES=1",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("factory-reset run failed: %v\n%s", err, out)
	}

	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }

	if exists(filepath.Join(etc, "commissioned")) {
		t.Error("commissioned marker must be removed")
	}
	if exists(filepath.Join(run, "provision-window")) {
		t.Error("re-provision window must be cleared")
	}
	if !exists(filepath.Join(etc, "identity", "serial")) {
		t.Error("identity must be preserved")
	}
	// The load-bearing B4 assertion: the manufacturing PoP survives.
	popData, err := os.ReadFile(filepath.Join(etc, "provision-pop"))
	if err != nil || string(popData) != "MFG-POP-SECRET\n" {
		t.Errorf("provision-pop must be preserved untouched, got %q err=%v", popData, err)
	}
	// Factory config installed over the wiped one.
	apiData, err := os.ReadFile(filepath.Join(etc, "api.json"))
	if err != nil || string(apiData) != `{"factory":1}` {
		t.Errorf("factory api.json must be installed, got %q err=%v", apiData, err)
	}
	// CSIP certs wiped.
	if exists(filepath.Join(etc, "certs", "client.pem")) {
		t.Error("CSIP certs must be wiped")
	}
}
