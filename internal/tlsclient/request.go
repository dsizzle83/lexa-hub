package tlsclient

import (
	"fmt"
	"strings"
)

// validateRequestParam rejects strings that contain CR, LF, or space.
// These characters would allow a hostile server to inject additional HTTP
// headers or smuggle a second request via attacker-controlled path/host values.
func validateRequestParam(s, name string) error {
	if strings.ContainsAny(s, "\r\n ") {
		return fmt.Errorf("invalid %s: contains CR, LF, or space", name)
	}
	return nil
}

// buildGetRequest constructs an HTTP/1.1 GET request for the given path.
// The Host header is required by HTTP/1.1 and 2030.5 conformance test
// harnesses verify it; we use the configured server address as the
// Host value, stripping the port if it's present (the spec wants the
// hostname only when no port-significance is intended, but accepts the
// host:port form too — we use host:port for clarity).
func buildGetRequest(path, host string) []byte {
	return []byte(fmt.Sprintf(
		"GET %s HTTP/1.1\r\n"+
			"Host: %s\r\n"+
			"Accept: application/sep+xml\r\n"+
			"Connection: keep-alive\r\n"+
			"\r\n",
		path, host))
}

// buildPostRequest constructs an HTTP/1.1 POST request with body.
func buildPostRequest(path, host string, body []byte, contentType string) []byte {
	header := fmt.Sprintf(
		"POST %s HTTP/1.1\r\n"+
			"Host: %s\r\n"+
			"Content-Type: %s\r\n"+
			"Content-Length: %d\r\n"+
			"Connection: keep-alive\r\n"+
			"\r\n",
		path, host, contentType, len(body))
	return append([]byte(header), body...)
}

// buildPutRequest constructs an HTTP/1.1 PUT request with body — the same
// shape as buildPostRequest with only the verb changed (WP-3/D3: DER*
// reporting PUTs a full resource representation, Content-Length framed).
// Like the other builders this is a pure formatter: the CRLF/space
// injection guard (validateRequestParam) is applied by the Client verb
// methods to the path before this ever runs, and the host is the
// configured ServerAddr, never attacker-controlled.
func buildPutRequest(path, host string, body []byte, contentType string) []byte {
	header := fmt.Sprintf(
		"PUT %s HTTP/1.1\r\n"+
			"Host: %s\r\n"+
			"Content-Type: %s\r\n"+
			"Content-Length: %d\r\n"+
			"Connection: keep-alive\r\n"+
			"\r\n",
		path, host, contentType, len(body))
	return append([]byte(header), body...)
}
