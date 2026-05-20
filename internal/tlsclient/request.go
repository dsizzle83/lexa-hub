package tlsclient

import "fmt"

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
