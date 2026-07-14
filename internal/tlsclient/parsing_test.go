package tlsclient

import (
	"strings"
	"testing"
)

// === Request building tests ===============================================

func TestBuildGetRequest_Format(t *testing.T) {
	req := string(buildGetRequest("/dcap", "192.168.0.188:11111"))

	for _, want := range []string{
		"GET /dcap HTTP/1.1\r\n",
		"Host: 192.168.0.188:11111\r\n",
		"Accept: application/sep+xml\r\n",
		"Connection: keep-alive\r\n",
		"\r\n\r\n", // header terminator
	} {
		if !strings.Contains(req, want) {
			t.Errorf("request missing %q\nfull request:\n%s", want, req)
		}
	}
}

func TestBuildGetRequest_DifferentPaths(t *testing.T) {
	cases := []string{"/dcap", "/edev", "/tm", "/edev/0/derp"}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			req := string(buildGetRequest(path, "host:11111"))
			wantLine := "GET " + path + " HTTP/1.1\r\n"
			if !strings.HasPrefix(req, wantLine) {
				t.Errorf("request line wrong:\nwant prefix: %q\ngot: %q", wantLine, req)
			}
		})
	}
}

// === Response parsing tests ===============================================

func TestParseHTTPResponse_Happy(t *testing.T) {
	raw := []byte(
		"HTTP/1.1 200 OK\r\n" +
			"Content-Type: application/sep+xml\r\n" +
			"Content-Length: 13\r\n" +
			"Connection: close\r\n" +
			"\r\n" +
			"<DCAP/>hello")

	resp, err := parseHTTPResponse(raw)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if resp.ContentType != "application/sep+xml" {
		t.Errorf("ContentType = %q, want application/sep+xml", resp.ContentType)
	}
	if string(resp.Body) != "<DCAP/>hello" {
		t.Errorf("Body = %q, want <DCAP/>hello", resp.Body)
	}
}

func TestParseHTTPResponse_ContentTypeWithCharset(t *testing.T) {
	// Some servers append charset; we should strip it.
	raw := []byte(
		"HTTP/1.1 200 OK\r\n" +
			"Content-Type: application/sep+xml; charset=utf-8\r\n" +
			"\r\n" +
			"body")

	resp, err := parseHTTPResponse(raw)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if resp.ContentType != "application/sep+xml" {
		t.Errorf("ContentType = %q, want application/sep+xml (stripped)", resp.ContentType)
	}
}

func TestParseHTTPResponse_ErrorStatus(t *testing.T) {
	raw := []byte("HTTP/1.1 404 Not Found\r\n\r\nnope")
	resp, err := parseHTTPResponse(raw)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if resp.StatusCode != 404 {
		t.Errorf("StatusCode = %d, want 404", resp.StatusCode)
	}
}

// TestParseHTTPResponse_RedirectLocation pins that a 301/302's Location
// header lands on HTTPResponse.Location — the field followRedirects
// (redirect.go, WP-3/D3) acts on. Location was previously only exercised
// via 201 Created; a parser regression that made it 201-specific would
// break redirect following while every other test stayed green.
func TestParseHTTPResponse_RedirectLocation(t *testing.T) {
	cases := []struct {
		status   string
		wantCode int
	}{
		{"301 Moved Permanently", 301},
		{"302 Found", 302},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			raw := []byte(
				"HTTP/1.1 " + tc.status + "\r\n" +
					"Location: /dcap-v2\r\n" +
					"Content-Length: 0\r\n" +
					"\r\n")
			resp, err := parseHTTPResponse(raw)
			if err != nil {
				t.Fatalf("parse failed: %v", err)
			}
			if resp.StatusCode != tc.wantCode {
				t.Errorf("StatusCode = %d, want %d", resp.StatusCode, tc.wantCode)
			}
			if resp.Location != "/dcap-v2" {
				t.Errorf("Location = %q, want /dcap-v2", resp.Location)
			}
		})
	}
}

func TestParseHTTPResponse_Malformed(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"no separator", "HTTP/1.1 200 OK\r\nContent-Type: x\r\n"},
		{"no status code", "HTTP/1.1\r\n\r\n"},
		{"non-numeric status", "HTTP/1.1 ABC OK\r\n\r\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseHTTPResponse([]byte(tc.raw))
			if err == nil {
				t.Errorf("expected error for %q, got nil", tc.raw)
			}
		})
	}
}

// === DCAP XML parsing tests ===============================================

// TestDCAPParse_Canonical exercises the XML unmarshaling against the
// exact byte representation our server produces. This is the
// client-side complement to the server's DCAP golden file test:
// together they prove that what the server emits, the client can
// parse, with no human in the loop.
func TestDCAPParse_Canonical(t *testing.T) {
	xmlBytes := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<DeviceCapability xmlns="urn:ieee:std:2030.5:ns" href="/dcap">
  <EndDeviceListLink href="/edev" all="0"/>
  <MirrorUsagePointListLink href="/mup" all="0"/>
  <SelfDeviceLink href="/sdev"/>
  <TimeLink href="/tm"/>
</DeviceCapability>`)

	// We can't call FetchDCAP because it's tied to the network. We
	// reach into the parsing logic by simulating an HTTP response and
	// parsing only the body. This is fine because parseHTTPResponse
	// is itself unit-tested above.
	resp, err := parseHTTPResponse(append([]byte(
		"HTTP/1.1 200 OK\r\n"+
			"Content-Type: application/sep+xml\r\n\r\n"), xmlBytes...))
	if err != nil {
		t.Fatalf("parse HTTP: %v", err)
	}

	var dcap DeviceCapability
	if err := xmlUnmarshal(resp.Body, &dcap); err != nil {
		t.Fatalf("parse XML: %v", err)
	}

	if dcap.Href != "/dcap" {
		t.Errorf("DCAP.Href = %q, want /dcap", dcap.Href)
	}
	if dcap.EndDeviceListLink == nil {
		t.Fatal("EndDeviceListLink is nil")
	}
	if dcap.EndDeviceListLink.Href != "/edev" {
		t.Errorf("EndDeviceListLink.Href = %q, want /edev", dcap.EndDeviceListLink.Href)
	}
	if dcap.TimeLink == nil || dcap.TimeLink.Href != "/tm" {
		t.Errorf("TimeLink wrong: %+v", dcap.TimeLink)
	}
	if dcap.SelfDeviceLink == nil || dcap.SelfDeviceLink.Href != "/sdev" {
		t.Errorf("SelfDeviceLink wrong: %+v", dcap.SelfDeviceLink)
	}
}

// xmlUnmarshal is a tiny indirection so the test doesn't need to
// import encoding/xml directly. The production code's FetchDCAP
// uses encoding/xml.Unmarshal — same thing.
func xmlUnmarshal(data []byte, v interface{}) error {
	return xmlUnmarshalImpl(data, v)
}
