package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolveTrust_PlainHTTP(t *testing.T) {
	tc, err := resolveTrust("http://127.0.0.1:9100", false, "", "/nonexistent")
	if err != nil {
		t.Fatalf("resolveTrust: %v", err)
	}
	if tc.tlsConfig != nil {
		t.Errorf("tlsConfig = %+v, want nil for a plain http:// addr", tc.tlsConfig)
	}
	if tc.source != "none" {
		t.Errorf("source = %q, want %q", tc.source, "none")
	}
}

func TestResolveTrust_BadScheme(t *testing.T) {
	if _, err := resolveTrust("ftp://127.0.0.1:9100", false, "", ""); err == nil {
		t.Fatal("expected an error for a non-http(s) scheme")
	}
}

func TestResolveTrust_Insecure(t *testing.T) {
	tc, err := resolveTrust("https://127.0.0.1:9100", true, "", "/nonexistent")
	if err != nil {
		t.Fatalf("resolveTrust: %v", err)
	}
	if tc.tlsConfig == nil || !tc.tlsConfig.InsecureSkipVerify {
		t.Fatalf("tlsConfig = %+v, want InsecureSkipVerify", tc.tlsConfig)
	}
	if tc.tlsConfig.VerifyPeerCertificate != nil {
		t.Error("insecure mode should not install a VerifyPeerCertificate callback")
	}
}

func TestResolveTrust_BadFingerprintFormat(t *testing.T) {
	// Deliberately excludes "" — an empty -fingerprint is the "no flag
	// given" case, which falls through to the local-cert-file default and
	// is covered by TestResolveTrust_DefaultCertFileMissing instead.
	cases := []string{"not-hex-and-wrong-length", "abcd", strings.Repeat("zz", 32)}
	for _, fp := range cases {
		if _, err := resolveTrust("https://127.0.0.1:9100", false, fp, "/nonexistent"); err == nil {
			t.Errorf("resolveTrust(fingerprint=%q): expected error, got nil", fp)
		}
	}
}

func TestResolveTrust_DefaultCertFileMissing(t *testing.T) {
	if _, err := resolveTrust("https://127.0.0.1:9100", false, "", "/definitely/not/a/real/path/cert.pem"); err == nil {
		t.Fatal("expected an error when the local cert file is unavailable and no override was given")
	}
}

// TestPinning_CorrectFingerprintConnects_WrongFingerprintRefuses drives an
// actual TLS handshake against an httptest.NewTLSServer (a real, ephemeral
// self-signed cert) through the VerifyPeerCertificate path pinnedTLSConfig
// installs — this is the "correct pin connects / wrong pin refuses" test
// the unit brief calls for.
func TestPinning_CorrectFingerprintConnects_WrongFingerprintRefuses(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	correctFP := fingerprintOfDER(srv.Certificate().Raw)
	wrongFP := strings.Repeat("0", len(correctFP))
	if wrongFP == correctFP { // pathological but cheap to guard
		wrongFP = strings.Repeat("f", len(correctFP))
	}

	t.Run("correct pin connects", func(t *testing.T) {
		tc, err := resolveTrust(srv.URL, false, correctFP, "/nonexistent")
		if err != nil {
			t.Fatalf("resolveTrust: %v", err)
		}
		c := newClient(srv.URL, "", "", tc.tlsConfig)
		resp, err := c.get(context.Background(), "/")
		if err != nil {
			t.Fatalf("get with correct pin: %v", err)
		}
		if resp.Status != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.Status)
		}
	})

	t.Run("wrong pin refuses", func(t *testing.T) {
		tc, err := resolveTrust(srv.URL, false, wrongFP, "/nonexistent")
		if err != nil {
			t.Fatalf("resolveTrust: %v", err)
		}
		c := newClient(srv.URL, "", "", tc.tlsConfig)
		if _, err := c.get(context.Background(), "/"); err == nil {
			t.Fatal("expected a TLS handshake failure with a mismatched pin, got nil error")
		}
	})

	t.Run("insecure connects despite mismatched-in-spirit pin", func(t *testing.T) {
		tc, err := resolveTrust(srv.URL, true, "", "/nonexistent")
		if err != nil {
			t.Fatalf("resolveTrust: %v", err)
		}
		c := newClient(srv.URL, "", "", tc.tlsConfig)
		resp, err := c.get(context.Background(), "/")
		if err != nil {
			t.Fatalf("get with -insecure: %v", err)
		}
		if resp.Status != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.Status)
		}
	})

	t.Run("default local-cert-file pin connects when the file matches", func(t *testing.T) {
		certPath := writeCertPEM(t, srv.Certificate().Raw)
		tc, err := resolveTrust(srv.URL, false, "", certPath)
		if err != nil {
			t.Fatalf("resolveTrust: %v", err)
		}
		if tc.source != "local-cert-file" {
			t.Errorf("source = %q, want local-cert-file", tc.source)
		}
		c := newClient(srv.URL, "", certPath, tc.tlsConfig)
		resp, err := c.get(context.Background(), "/")
		if err != nil {
			t.Fatalf("get with local-cert-file pin: %v", err)
		}
		if resp.Status != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.Status)
		}
	})

	t.Run("default with a non-matching local cert file refuses", func(t *testing.T) {
		// A genuinely DIFFERENT cert from srv's — two bare
		// httptest.NewTLSServer instances would share the same hardcoded
		// default test cert and defeat this case, so otherSrv presents its
		// own freshly generated leaf (testhelpers_test.go's genSelfSignedCert
		// / newTLSServerWithCert).
		otherSrv := newTLSServerWithCert(t, genSelfSignedCert(t, "other-unit"))
		defer otherSrv.Close()

		// Pin the FIRST server's cert but connect to the SECOND — a stand-in
		// for "the box's cert changed since the file was captured".
		certPath := writeCertPEM(t, srv.Certificate().Raw)
		tc, err := resolveTrust(otherSrv.URL, false, "", certPath)
		if err != nil {
			t.Fatalf("resolveTrust: %v", err)
		}
		c := newClient(otherSrv.URL, "", certPath, tc.tlsConfig)
		if _, err := c.get(context.Background(), "/"); err == nil {
			t.Fatal("expected a TLS handshake failure against a non-matching server, got nil error")
		}
	})
}

func TestValidateFingerprint(t *testing.T) {
	tests := []struct {
		name    string
		fp      string
		wantErr bool
	}{
		{"valid lowercase hex sha256", strings.Repeat("ab", 32), false},
		{"too short", "abcd", true},
		{"not hex", strings.Repeat("zz", 32), true},
		{"uppercase gets rejected by length-preserving hex check", strings.ToUpper(strings.Repeat("ab", 32)), false}, // hex.DecodeString accepts uppercase
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateFingerprint(tt.fp)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateFingerprint(%q) error = %v, wantErr %v", tt.fp, err, tt.wantErr)
			}
		})
	}
}
