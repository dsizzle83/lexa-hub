package main

// rotate.go implements TASK-073 (§10.5/§8.6/RSK-07): the trigger side of a
// staged certificate-rotation procedure for lexa-northbound's three
// independent wolfSSL fetchers (discovery, response, flow-reservation —
// see main.go's mustFetcher calls). docs/CERT_ROTATION_RUNBOOK.md is the
// operator procedure (scripts/rotate-cert.sh) this controller is the other
// half of.
//
// Trigger: a sentinel file (default /etc/lexa/certs/rotate.request,
// cfg.CertRotateSentinel) written by the operator script, containing the
// path to a STAGED new cert/key — NOT the live paths northbound.json
// points ClientCertPath/ClientKeyPath at. This matters: only after a
// rotation fully commits does the operator script promote the staged
// files onto the live paths (see the runbook), so a failed rotation
// attempt never leaves the live path — the one a future process restart
// depends on — holding untested cert material.
//
// Ordering discipline (task's "common mistakes to avoid"): each of the
// three fetchers is rotated ONE AT A TIME via WolfSSLFetcher.Reload's own
// probe-then-commit swap (internal/tlsclient/fetcher.go) — never
// concurrently. Reload's internal mutex already makes each individual swap
// safe no matter which goroutine calls it (this controller's own, an owned
// goroutine independent of the discovery walk loop and the MQTT
// subscription goroutines — 05 §4, the same shape as certmon.go's
// Monitor); rotating one fetcher at a time is about keeping a PARTIAL
// failure easy to reason about (each fetcher's rotation outcome is
// reported independently), not about additional safety the mutex doesn't
// already provide.
//
// LFDI defense-in-depth: the primary refusal gate is
// scripts/rotate-cert.sh, which compares the staged cert's derived LFDI
// against the live one and refuses to even write the sentinel on a
// mismatch (that is re-enrollment, not rotation — a different device
// identity, which gridsim's EndDevice registration does not recognize).
// This controller re-checks independently, so a sentinel written by
// anything else is still refused here before any Reload is attempted.
import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"lexa-hub/internal/metrics"
	"lexa-hub/internal/tlsclient"
)

// defaultCertRotatePollInterval is how often RotationController checks for
// a pending sentinel file, absent an explicit interval (Run's parameter
// mirrors run.Discovery.Loop / Monitor.Run's shape: production passes a
// real interval, tests inject a short one).
const defaultCertRotatePollInterval = 5 * time.Second

// certRotateProbePath is the resource every rotated fetcher's Reload probes
// with a GET before committing — DeviceCapability, the CSIP entry point,
// requires no prior walk state and is always available (TASK-073's
// "performs a probe walk (DeviceCapability GET)").
const certRotateProbePath = "/dcap"

// defaultCertRotateSentinel is where RotationController looks for a
// rotation request absent an explicit Config.CertRotateSentinel.
const defaultCertRotateSentinel = "/etc/lexa/certs/rotate.request"

// rotateRequest is the sentinel file's JSON schema, written by
// scripts/rotate-cert.sh. ClientCert/ClientKey point at the STAGED new
// material (see this file's package doc) — never the live paths.
type rotateRequest struct {
	ClientCert  string `json:"client_cert"`
	ClientKey   string `json:"client_key"`
	CACert      string `json:"ca_cert,omitempty"` // empty ⇒ reuse the configured CA path
	RequestedAt string `json:"requested_at,omitempty"`
}

// reloader is the subset of *tlsclient.WolfSSLFetcher RotationController
// needs, narrowed to an interface at the point of consumption (05 §2) so
// the sentinel-handling/ordering logic in checkOnce is unit-testable
// (rotate_test.go) without a live wolfSSL session — the real Reload
// mechanics (Free ordering) are proven separately by
// internal/tlsclient/reload_test.go + reload_integration_test.go.
type reloader interface {
	Reload(cfg tlsclient.Config, probePath string) error
}

// RotationController watches a sentinel file for a staged cert-rotation
// request and, on finding one, rotates all three northbound fetchers
// (discovery, response, flow-reservation) in sequence via probe-then-commit
// Reload — refusing, without attempting any Reload, if the staged cert's
// derived LFDI does not match this process's own identity.
type RotationController struct {
	sentinelPath string
	baseCfg      tlsclient.Config // ServerAddr/CACertPath/timeouts template; Client*Path overridden per request
	ownLFDI      string

	discovery reloader
	response  reloader
	flowres   reloader

	// onCommit runs once after a FULLY successful rotation (all three
	// fetchers) — main.go wires this to certMon.CheckOnce (TASK-072), so
	// certstatus reflects the new cert's NotAfter immediately rather than
	// waiting up to 24h for the next scheduled check.
	onCommit func()

	// deriveLFDI computes a candidate cert file's LFDI — a seam so
	// rotate_test.go can inject a fake without touching real PEM parsing
	// paths (lfdiFromCert, defined in main.go, is the production value).
	deriveLFDI func(certPath string) (string, error)

	rotations *metrics.Counter // lexa_nb_cert_rotations_total (per fetcher committed)
	failures  *metrics.Counter // lexa_nb_cert_rotation_failures_total (per fetcher reload error)
	refusals  *metrics.Counter // lexa_nb_cert_rotation_refusals_total (malformed/LFDI-mismatch sentinels)

	now func() time.Time
}

// NewRotationController constructs a controller. baseCfg supplies
// ServerAddr/CACertPath/DialTimeout/ReadTimeout for the rotated fetchers —
// only ClientCertPath/ClientKeyPath (and, rarely, CACertPath) vary per
// request. ownLFDI is this process's current identity (cfg.LFDI, or
// derived from the live client cert at startup — see main.go); reg may be
// nil (tests exercising checkOnce without a metrics registry).
func NewRotationController(sentinelPath string, baseCfg tlsclient.Config, ownLFDI string, discovery, response, flowres reloader, onCommit func(), reg *metrics.Registry) *RotationController {
	if sentinelPath == "" {
		sentinelPath = defaultCertRotateSentinel
	}
	rc := &RotationController{
		sentinelPath: sentinelPath,
		baseCfg:      baseCfg,
		ownLFDI:      ownLFDI,
		discovery:    discovery,
		response:     response,
		flowres:      flowres,
		onCommit:     onCommit,
		deriveLFDI:   lfdiFromCert,
		now:          time.Now,
	}
	if reg != nil {
		rc.rotations = reg.Counter("lexa_nb_cert_rotations_total")
		rc.failures = reg.Counter("lexa_nb_cert_rotation_failures_total")
		rc.refusals = reg.Counter("lexa_nb_cert_rotation_refusals_total")
	}
	return rc
}

// Run polls the sentinel path every interval until ctx is cancelled.
// Callers run this in its own goroutine — an owned goroutine with its own
// ticker and its own ctx.Done() shutdown path (05 §4), sharing no state
// with the discovery walk loop or certMon. Mirrors Monitor.Run's shape.
func (rc *RotationController) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultCertRotatePollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rc.checkOnce()
		}
	}
}

// checkOnce looks for a pending sentinel file and, if present, processes it
// exactly once: parse, LFDI-check, rotate each fetcher in turn, then rename
// the sentinel away with an outcome suffix (done/failed/rejected) plus a
// timestamp — the sentinel's mere PRESENCE at sentinelPath is the trigger,
// so consuming it (rather than leaving it in place) is what prevents
// reprocessing the same request on the next tick. A missing sentinel is the
// steady-state (no-op, not logged); any other read error is logged but
// otherwise also a no-op — a transient stat failure should not be treated
// as a malformed request.
func (rc *RotationController) checkOnce() {
	data, err := os.ReadFile(rc.sentinelPath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("lexa-northbound: cert rotation: read sentinel", "path", rc.sentinelPath, "err", err)
		}
		return
	}

	var req rotateRequest
	if err := json.Unmarshal(data, &req); err != nil {
		slog.Error("lexa-northbound: cert rotation: malformed sentinel JSON, refusing", "path", rc.sentinelPath, "err", err)
		rc.refusals.Inc()
		rc.consume("rejected")
		return
	}
	if req.ClientCert == "" || req.ClientKey == "" {
		slog.Error("lexa-northbound: cert rotation: sentinel missing client_cert/client_key, refusing", "path", rc.sentinelPath)
		rc.refusals.Inc()
		rc.consume("rejected")
		return
	}

	// LFDI defense-in-depth (05 §12 / task's "common mistakes"): a new CN
	// for what is supposed to be the SAME device is re-enrollment, not
	// rotation — gridsim's EndDevice registration is keyed by LFDI, so a
	// mismatched cert would 403 on every subsequent walk, not silently
	// misbehave. scripts/rotate-cert.sh already refuses this before ever
	// writing the sentinel; this is the second, independent check.
	newLFDI, err := rc.deriveLFDI(req.ClientCert)
	if err != nil {
		slog.Error("lexa-northbound: cert rotation: derive LFDI of staged cert, refusing", "path", req.ClientCert, "err", err)
		rc.refusals.Inc()
		rc.consume("rejected")
		return
	}
	if rc.ownLFDI != "" && newLFDI != rc.ownLFDI {
		slog.Error("lexa-northbound: cert rotation: staged cert LFDI does not match this device's identity — refusing (this is re-enrollment, not rotation)",
			"own_lfdi", rc.ownLFDI, "staged_lfdi", newLFDI)
		rc.refusals.Inc()
		rc.consume("rejected")
		return
	}

	newCfg := rc.baseCfg
	newCfg.ClientCertPath = req.ClientCert
	newCfg.ClientKeyPath = req.ClientKey
	if req.CACert != "" {
		newCfg.CACertPath = req.CACert
	}

	slog.Info("lexa-northbound: cert rotation requested — rotating fetchers off-tick, one at a time", "lfdi", newLFDI)

	steps := []struct {
		name string
		f    reloader
	}{
		{"discovery", rc.discovery},
		{"response", rc.response},
		{"flow-reservation", rc.flowres},
	}

	var failed []string
	for _, s := range steps {
		// Each fetcher swaps at its OWN safe point (task's "common
		// mistakes": never rotate all three concurrently mid-walk).
		// Continuing through every fetcher even after one fails — rather
		// than aborting the loop — leaves the other two rotated: a
		// partial outcome is fully reported (failed lists exactly which
		// fetcher(s) still hold the previous cert) rather than silently
		// leaving fetchers in different, unlogged states.
		if err := s.f.Reload(newCfg, certRotateProbePath); err != nil {
			slog.Error("lexa-northbound: cert rotation: fetcher reload failed — keeping previous cert on this fetcher",
				"fetcher", s.name, "err", err)
			failed = append(failed, s.name)
			rc.failures.Inc()
			continue
		}
		rc.rotations.Inc()
		slog.Info("lexa-northbound: cert rotation: fetcher committed", "fetcher", s.name)
	}

	if len(failed) > 0 {
		slog.Error("lexa-northbound: cert rotation: PARTIAL — some fetchers still on the previous cert; fix the underlying issue and re-stage + re-write the sentinel to retry",
			"failed", failed)
		rc.consume("failed")
		return
	}

	slog.Info("lexa-northbound: cert rotation committed on all three fetchers", "lfdi", newLFDI)
	if rc.onCommit != nil {
		rc.onCommit()
	}
	rc.consume("done")
}

// consume renames the sentinel file away with an outcome suffix and Unix
// timestamp, so it is processed exactly once and the outcome stays visible
// on disk for the operator script to poll for (docs/CERT_ROTATION_RUNBOOK.md
// step: "wait for certstatus ... verify a fresh walk in the journal").
func (rc *RotationController) consume(outcome string) {
	dst := fmt.Sprintf("%s.%s-%d", rc.sentinelPath, outcome, rc.now().Unix())
	if err := os.Rename(rc.sentinelPath, dst); err != nil {
		slog.Error("lexa-northbound: cert rotation: consume sentinel", "path", rc.sentinelPath, "dst", dst, "err", err)
	}
}
