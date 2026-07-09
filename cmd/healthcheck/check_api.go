package main

import (
	"context"
	"fmt"
)

// checkAPI probes GET /healthz on the configured lexa-api listen_addr,
// loopback-only (127.0.0.1), trying https then falling back to http (see
// httpprobe.go's probeGET). /healthz is documented as "always open"
// regardless of api_token_file, so no Authorization header is sent here.
func checkAPI(ctx context.Context, env *Environment) Result {
	cfg, err := loadAPIConfig(env.ConfigDir)
	if err != nil {
		return fail("api", err.Error())
	}
	host, port, err := apiHostPort(cfg.ListenAddr)
	if err != nil {
		return fail("api", err.Error())
	}
	res, err := probeGET(ctx, env.HTTPClient, env.APIScheme, host, port, "/healthz", nil)
	if err != nil {
		return fail("api", fmt.Sprintf("GET %s:%s/healthz: %v", host, port, err))
	}
	if res.StatusCode != 200 {
		return fail("api", fmt.Sprintf("GET %s://%s:%s/healthz: HTTP %d", res.Scheme, host, port, res.StatusCode))
	}
	return pass("api", fmt.Sprintf("%s://%s:%s/healthz 200", res.Scheme, host, port))
}
