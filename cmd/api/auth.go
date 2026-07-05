package main

import (
	"crypto/subtle"
	"net/http"
)

// requireBearer wraps next so requests must carry `Authorization: Bearer
// <token>` to reach it. An empty token disables the check entirely (returns
// next unwrapped) — this is the staged-rollout escape hatch (TASK-014 /
// AD-008): the bench runs with api_token_file unset until every consumer
// (dashboard proxy, logmux, Mayhem/replay drivers, metersim) presents the
// token, so /status and /logs must keep working exactly as before until then.
//
// Comparison is constant-time (crypto/subtle) — this guards a bench secret,
// not a high-value credential, but timing side-channels are free to close.
// Neither the presented nor the expected header value is ever logged.
func requireBearer(token string, next http.HandlerFunc) http.HandlerFunc {
	if token == "" {
		return next
	}
	want := "Bearer " + token
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
