package main

// version.go surfaces the hub⇄app HTTP contract version (Workstream C,
// internal/apicontract) on the wire. The header is the transport-level signal
// the companion app reads to detect a major-version mismatch; the additive
// "contract_version" JSON field in GET /status and GET /site, and the
// "contract=" mDNS TXT record (mdns.go), are the other two advertised
// surfaces. All three read the single apicontract.Version constant so they can
// never disagree.
import (
	"net/http"
	"strconv"

	"lexa-hub/internal/apicontract"
)

// contractVersionHeader is the response header carrying apicontract.Version.
const contractVersionHeader = "X-Lexa-Contract-Version"

// withContractVersion wraps the API mux so EVERY response — including /healthz
// and /metrics — carries the X-Lexa-Contract-Version header. It is a pure
// header stamp with no auth or body effect (unlike requireBearer*, which wrap
// individual routes): the version must be observable on any route the app
// touches, without the app first having to authenticate. Set once from the
// apicontract.Version constant at construction, not per request.
func withContractVersion(next http.Handler) http.Handler {
	v := strconv.Itoa(apicontract.Version)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(contractVersionHeader, v)
		next.ServeHTTP(w, r)
	})
}
