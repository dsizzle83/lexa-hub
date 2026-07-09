package main

import "encoding/json"

// decodeJSON is a one-line wrapper so every subcommand's decode call site
// reads the same way; kept in its own file since it's used across every
// cmd_*.go.
func decodeJSON(body []byte, v any) error {
	return json.Unmarshal(body, v)
}
