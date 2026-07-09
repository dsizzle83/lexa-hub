package main

import (
	"bytes"
	"net/http/httptest"
	"os"
	"testing"
)

func TestFingerprintFromCertFile(t *testing.T) {
	srv := httptest.NewTLSServer(nil)
	defer srv.Close()
	der := srv.Certificate().Raw
	want := fingerprintOfDER(der)

	path := writeCertPEM(t, der)
	got, err := fingerprintFromCertFile(path)
	if err != nil {
		t.Fatalf("fingerprintFromCertFile: %v", err)
	}
	if got != want {
		t.Errorf("fingerprint = %s, want %s", got, want)
	}
	if len(got) != 64 {
		t.Errorf("fingerprint length = %d, want 64 (32 bytes hex)", len(got))
	}
}

func TestFingerprintFromCertFile_MissingFile(t *testing.T) {
	if _, err := fingerprintFromCertFile("/definitely/not/a/real/path.pem"); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestFingerprintFromCertFile_NotPEM(t *testing.T) {
	path := t.TempDir() + "/not-a-cert.pem"
	if err := os.WriteFile(path, []byte("this is not PEM data"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := fingerprintFromCertFile(path); err == nil {
		t.Fatal("expected an error for non-PEM content")
	}
}

func TestCmdFingerprint_Success(t *testing.T) {
	srv := httptest.NewTLSServer(nil)
	defer srv.Close()
	path := writeCertPEM(t, srv.Certificate().Raw)
	want := fingerprintOfDER(srv.Certificate().Raw)

	var buf bytes.Buffer
	c := &client{certFile: path}
	code := cmdFingerprint(c, nil, &buf)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output: %s", code, buf.String())
	}
	if got := buf.String(); got != want+"\n" {
		t.Errorf("output = %q, want %q", got, want+"\n")
	}
}

func TestCmdFingerprint_MissingCertFile(t *testing.T) {
	var buf bytes.Buffer
	c := &client{certFile: "/definitely/not/a/real/path.pem"}
	code := cmdFingerprint(c, nil, &buf)
	if code != 1 {
		t.Errorf("exit code = %d, want 1; output: %s", code, buf.String())
	}
}

func TestCmdFingerprint_UsageError(t *testing.T) {
	var buf bytes.Buffer
	code := cmdFingerprint(&client{}, []string{"extra-arg"}, &buf)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}
