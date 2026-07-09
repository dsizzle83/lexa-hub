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

// requireBearerStrict is requireBearer's write-route counterpart
// (DEVICE_ROADMAP.md §4.2): unlike requireBearer, an EMPTY token does NOT
// open the gate. requireBearer's empty-token escape hatch exists for the
// staged bearer-token ROLLOUT on read routes (/status, /logs) — it must
// never apply to a route that changes state. A write route wrapped in
// requireBearerStrict with no api_token_file configured returns 401
// unconditionally: "no token configured" must fail closed for writes, not
// fail open the way it deliberately does for reads.
//
// No route wires this in yet — unit 4.3 (POST /intent, /scan,
// /config/{service}) will — but it is exported package-internally and
// tested now so its behavior is pinned ahead of any caller.
func requireBearerStrict(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		want := "Bearer " + token
		got := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
