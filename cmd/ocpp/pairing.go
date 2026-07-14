// WP-13 (architecture D10, NORMATIVE): the OCPP pairing gate.
//
// pairingGate is the single, stack-neutral owner of "may this station become
// plant?" — BOTH listeners (the 2.0.1 forwarders in main.go and the 1.6
// forwarders in bridge16.go) consult the same instance, so the gate can never
// fork per protocol. Verdicts:
//
//   - open mode (bench default): every station is allowed — byte-identical to
//     the pre-WP-13 behavior, including the Unit 6.1 pending surface for
//     unconfigured stations.
//   - gated mode (product default): configured stations[] are implicitly
//     allowlisted; a persisted installer approval allows; a persisted deny
//     answers Boot with Rejected; everything else is Pending — surfaced on
//     the retained lexa/ocpp/pending doc, never promoted to a stationState
//     (no lexa/evse/{station}/state, no plant), transactions/meter values
//     dropped with an edge log, StatusNotification recorded on the pending
//     surface only (D10's per-message policy, pinned by pairing_test.go).
//
// Decisions arrive as bus.PairingDecision edges on lexa/ocpp/pairing
// (lexa-api's POST /devices/evse/{id}/pairing is the only writer) and are
// persisted to allowlist_path (0600, atomic tmp+rename+fsync — the same
// technique as internal/northbound/responses.Store.Compact) so a restart
// re-seeds them (crash-only, AD-011). A missing/unprovisioned state
// directory degrades to RAM-only decisions with a loud error pointing at
// provisioning (V1RC finding D) — never a refusal to start, and never a
// silent gap.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"lexa-hub/internal/bus"
	"lexa-hub/internal/metrics"
)

// bootVerdict is the gate's protocol-neutral BootNotification answer; each
// stack's forwarder maps it onto its own RegistrationStatus vocabulary
// (provisioning.RegistrationStatus* for 2.0.1, core16.RegistrationStatus*
// for 1.6 — both define exactly Accepted/Pending/Rejected).
type bootVerdict int

const (
	bootAccept bootVerdict = iota
	bootPending
	bootReject
)

// pairingGate holds the pairing posture and the persisted allowlist. All
// methods are nil-receiver-safe and behave as OPEN mode on a nil gate, so
// bridge code paths (and pre-WP-13 tests that never construct one) keep
// their exact prior behavior without nil checks at every call site.
type pairingGate struct {
	mu         sync.Mutex
	mode       string // PairingGated | PairingOpen
	configured map[string]bool
	approved   map[string]bool
	denied     map[string]bool
	path       string // "" ⇒ persistence disabled (tests)

	// dropLogged dedupes the per-station drop log line (edge-triggered — a
	// pending station streaming MeterValues every 10 s must not fill the
	// journal; TASK-009 budget). Cleared for a station when its pairing
	// status changes, so post-decision behavior re-logs once if still
	// dropping.
	dropLogged map[string]bool

	// dropped counts every message dropped by the gate
	// (lexa_ocpp_pairing_dropped_total); nil-safe.
	dropped *metrics.Counter
}

// allowlistFile is the on-disk JSON shape at allowlist_path. Arrays are
// sorted for a deterministic file (diff-able across deploys).
type allowlistFile struct {
	Approved []string `json:"approved"`
	Denied   []string `json:"denied"`
}

// newPairingGate builds the gate and re-seeds persisted decisions from path
// (crash-only restart re-seed). A missing file is a clean empty start; an
// unreadable/corrupt file logs an ERROR and starts empty — the fail-CLOSED
// direction for gated mode (previously-approved stations pend again until
// re-approved; nothing unknown is silently admitted). A missing state
// directory is reported once, loudly, with the provisioning fix (V1RC
// finding D), and the gate runs RAM-only.
func newPairingGate(mode string, configuredIDs []string, path string, dropped *metrics.Counter) *pairingGate {
	g := &pairingGate{
		mode:       mode,
		configured: make(map[string]bool, len(configuredIDs)),
		approved:   make(map[string]bool),
		denied:     make(map[string]bool),
		path:       path,
		dropLogged: make(map[string]bool),
		dropped:    dropped,
	}
	for _, id := range configuredIDs {
		g.configured[id] = true
	}
	if path != "" {
		if dir := filepath.Dir(path); dirMissing(dir) {
			log.Printf("lexa-ocpp: ERROR pairing allowlist dir %s does not exist — approvals will NOT survive a restart until it is provisioned (StateDirectory=lexa on lexa-ocpp.service, or deploy-hub-pi.sh's install -d /var/lib/lexa; V1RC finding D)", dir)
		}
		if err := g.load(); err != nil {
			log.Printf("lexa-ocpp: ERROR pairing allowlist %s unreadable (%v) — starting with an EMPTY allowlist (fail-closed: previously approved stations pend again until re-approved)", path, err)
		}
	}
	return g
}

func dirMissing(dir string) bool {
	_, err := os.Stat(dir)
	return errors.Is(err, os.ErrNotExist)
}

// load re-seeds approved/denied from g.path. Missing file ⇒ empty, nil.
func (g *pairingGate) load() error {
	data, err := os.ReadFile(g.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var f allowlistFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	for _, id := range f.Approved {
		g.approved[id] = true
	}
	for _, id := range f.Denied {
		g.denied[id] = true
	}
	return nil
}

// persistLocked atomically rewrites the allowlist file (0600, tmp+rename+
// fsync — the responses.Store.Compact discipline). MkdirAll tolerates an
// already-provisioned /var/lib/lexa as a no-op; on an unprovisioned host it
// fails and the error names the provisioning fix. Caller must hold g.mu.
func (g *pairingGate) persistLocked() error {
	if g.path == "" {
		return nil
	}
	f := allowlistFile{Approved: sortedKeys(g.approved), Denied: sortedKeys(g.denied)}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(g.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s (provision /var/lib/lexa — StateDirectory=lexa or deploy-hub-pi.sh install -d; V1RC finding D): %w", dir, err)
	}
	tmp := g.path + ".tmp"
	tf, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := tf.Write(data); err != nil {
		tf.Close()
		os.Remove(tmp)
		return err
	}
	if err := tf.Sync(); err != nil {
		tf.Close()
		os.Remove(tmp)
		return err
	}
	if err := tf.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, g.path); err != nil {
		os.Remove(tmp)
		return err
	}
	// Best-effort directory fsync so the rename itself is durable (same
	// crash-consistency posture as the responses store).
	if df, err := os.Open(dir); err == nil {
		_ = df.Sync()
		df.Close()
	}
	return nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// allowed reports whether stationID may fold into plant state (stationState
// creation, measurements, transactions). Open mode (or a nil gate) allows
// everything — pre-WP-13 behavior.
func (g *pairingGate) allowed(stationID string) bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.mode != PairingGated {
		return true
	}
	return g.configured[stationID] || g.approved[stationID]
}

// verdict is the BootNotification answer for stationID (see bootVerdict).
func (g *pairingGate) verdict(stationID string) bootVerdict {
	if g == nil {
		return bootAccept
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.mode != PairingGated || g.configured[stationID] || g.approved[stationID] {
		return bootAccept
	}
	if g.denied[stationID] {
		return bootReject
	}
	return bootPending
}

// decided reports whether stationID carries a persisted approve/deny — the
// set the pending surface must never (re-)track (main.go seeds
// pendingStations.neverTrack from this at startup, gated mode only).
func (g *pairingGate) decidedStations() []string {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	ids := make(map[string]bool, len(g.approved)+len(g.denied))
	for id := range g.approved {
		ids[id] = true
	}
	for id := range g.denied {
		ids[id] = true
	}
	return sortedKeys(ids)
}

// permitOrLogDrop is the per-message gate for transaction/meter traffic: true
// ⇒ fold as usual. false ⇒ the caller must drop the fold; the drop has been
// counted and edge-logged (once per station until its pairing status
// changes — never per message, TASK-009 flash budget).
func (g *pairingGate) permitOrLogDrop(stationID, what string) bool {
	if g.allowed(stationID) {
		return true
	}
	g.dropped.Inc()
	g.mu.Lock()
	first := !g.dropLogged[stationID]
	g.dropLogged[stationID] = true
	g.mu.Unlock()
	if first {
		log.Printf("[ocpp] pairing gate: dropping %s from non-approved station %s (edge log — further drops from this station are counted, not logged; approve via POST /devices/evse/%s/pairing)", what, stationID, stationID)
	}
	return false
}

// errDecisionIgnored marks a decision applyDecision accepted syntactically
// but deliberately did not act on (configured station).
var errDecisionIgnored = errors.New("decision ignored")

// applyDecision validates and applies one installer decision, persisting the
// result. Returns nil when the decision took effect (idempotent re-delivery
// of the same decision also returns nil — QoS 1 replays are harmless), and a
// non-nil error for a malformed decision (unknown action, empty station ID)
// or a configured station (implicitly allowlisted — there is nothing to
// approve and denying it would contradict the operator's own config; fix
// stations[] instead). A persist failure does NOT undo the in-RAM decision
// (tolerate-and-alarm: the station works until restart) — it is logged here,
// loudly, once per failure.
func (g *pairingGate) applyDecision(d bus.PairingDecision) error {
	if g == nil {
		return errors.New("pairing gate not constructed")
	}
	if d.StationID == "" {
		return errors.New("empty station_id")
	}
	if d.Action != bus.PairingActionApprove && d.Action != bus.PairingActionDeny {
		return fmt.Errorf("unknown action %q (want approve|deny)", d.Action)
	}
	g.mu.Lock()
	if g.configured[d.StationID] {
		g.mu.Unlock()
		return fmt.Errorf("station %q is in stations[] (implicitly allowlisted): %w", d.StationID, errDecisionIgnored)
	}
	if d.Action == bus.PairingActionApprove {
		g.approved[d.StationID] = true
		delete(g.denied, d.StationID)
	} else {
		g.denied[d.StationID] = true
		delete(g.approved, d.StationID)
	}
	delete(g.dropLogged, d.StationID) // status changed ⇒ next drop (if any) re-logs once
	perr := g.persistLocked()
	g.mu.Unlock()
	if perr != nil {
		log.Printf("lexa-ocpp: ERROR pairing allowlist persist failed (%v) — %s of %s holds in RAM only and will NOT survive a restart", perr, d.Action, d.StationID)
	}
	return nil
}
