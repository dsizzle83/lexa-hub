package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun_NoArgsIsUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(nil, &out, &errOut)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestRun_UnknownCommandIsUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"frobnicate"}, &out, &errOut)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "frobnicate") {
		t.Errorf("expected the unknown command name in stderr, got:\n%s", errOut.String())
	}
}

func TestRun_HelpIsUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"-h"}, &out, &errOut)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "Trust model") {
		t.Errorf("expected the trust-model doc in -h output, got:\n%s", errOut.String())
	}
}

func TestRun_BadAddrSchemeIsAPIError(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"-addr", "ftp://127.0.0.1:9100", "status"}, &out, &errOut)
	if code != 1 {
		t.Errorf("exit code = %d, want 1; output: %s", code, out.String())
	}
}

// TestRun_FingerprintDispatchesBeforeTokenAndTrustResolution pins that
// `lexactl fingerprint` short-circuits ahead of -token-file/-addr handling
// (main.go's run(): "fingerprint... resolve it before anything network- or
// trust-related"). We can't point defaultCertFile (a const) at a fixture,
// so this asserts the NEGATIVE: with a garbage -addr and a nonexistent
// -token-file that would each fail their own resolution step, the reported
// error is about the cert file (fingerprint's own concern), never about
// the addr scheme or the token file — proving those steps were skipped.
func TestRun_FingerprintDispatchesBeforeTokenAndTrustResolution(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{
		"-addr", "not-even-a-url",
		"-token-file", "/definitely/not/a/real/token/file/either",
		"fingerprint",
	}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (defaultCertFile won't exist in a test env); stdout: %s", code, out.String())
	}
	got := out.String()
	if strings.Contains(got, "-addr") || strings.Contains(got, "token file") {
		t.Errorf("expected an error about the cert file only (addr/token resolution should be skipped), got:\n%s", got)
	}
}

func TestRun_EndToEndStatusThroughPlainHTTP(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mode":           "optimizer",
			"plan_heartbeat": map[string]any{"state": "ok", "age_s": 1.0},
			"devices":        map[string]any{},
		})
	}))
	defer srv.Close()

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("s3cr3t\n"), 0o600); err != nil {
		t.Fatalf("write token fixture: %v", err)
	}

	var out, errOut bytes.Buffer
	code := run([]string{"-addr", srv.URL, "-token-file", tokenFile, "status"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stdout: %s stderr: %s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "mode: optimizer") {
		t.Errorf("expected status summary, got:\n%s", out.String())
	}
	if gotAuth != "Bearer s3cr3t" {
		t.Errorf("Authorization = %q, want Bearer s3cr3t (token file should be read and attached)", gotAuth)
	}
}

func TestRun_MissingTokenFileIsNotFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"devices": map[string]any{}})
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := run([]string{"-addr", srv.URL, "-token-file", "/definitely/not/a/real/token/file", "status"}, &out, &errOut)
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (missing token file is not fatal); stdout: %s", code, out.String())
	}
}

func TestRun_EmptyTokenFileIsFatal(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "empty-token")
	if err := os.WriteFile(tokenFile, nil, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var out, errOut bytes.Buffer
	code := run([]string{"-addr", "http://127.0.0.1:1", "-token-file", tokenFile, "status"}, &out, &errOut)
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (empty token file must fail loud); stdout: %s", code, out.String())
	}
}

func TestLoadToken(t *testing.T) {
	t.Run("missing file returns empty, no error", func(t *testing.T) {
		tok, err := loadToken("/definitely/not/a/real/path")
		if err != nil {
			t.Fatalf("loadToken: %v", err)
		}
		if tok != "" {
			t.Errorf("token = %q, want empty", tok)
		}
	})

	t.Run("present file is trimmed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(path, []byte("  s3cr3t\n"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		tok, err := loadToken(path)
		if err != nil {
			t.Fatalf("loadToken: %v", err)
		}
		if tok != "s3cr3t" {
			t.Errorf("token = %q, want s3cr3t", tok)
		}
	})

	t.Run("present-but-empty file is an error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		if _, err := loadToken(path); err == nil {
			t.Fatal("expected an error for a present-but-blank token file")
		}
	})
}
