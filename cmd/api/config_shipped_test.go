package main

import "testing"

// TestLoadConfig_ShippedAPITLSEnabled pins the TLS-on-by-default bring-up fix
// (2026-07-11): the repo's dev/bench example configs/api.json used to ship
// "tls": false, and scripts/deploy-hub-pi.sh installs that file verbatim over
// /etc/lexa/api.json on every deploy — so every plain deploy re-served plain
// HTTP, which the companion app (HTTPS-only, cert-pinned) simply cannot reach
// at all. configs/api.json now ships "tls": true, matching
// configs/factory/api.json. This test loads the real shipped config through
// the real loadConfig (not a hand-written fixture) so a future accidental
// re-flip back to "tls": false fails CI instead of a bench.
func TestLoadConfig_ShippedAPITLSEnabled(t *testing.T) {
	cfg, err := loadConfig("../../configs/api.json")
	if err != nil {
		t.Fatalf("loadConfig(configs/api.json): %v", err)
	}
	if !cfg.TLSEnabled() {
		t.Fatal("TLSEnabled() = false for the shipped configs/api.json, want true (TLS-on-by-default, 2026-07-11 bring-up fix)")
	}
}
