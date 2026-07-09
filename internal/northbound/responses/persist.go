package responses

// WS-4.2 (docs/refactor/HANDOFF.md §8, csip-tls-test repo): TASK-041's
// acknowledged northbound half. Tracker's posted/alerted maps were RAM-only,
// so a lexa-northbound restart between the hub's compliance-alert edge
// (non-retained MQTT) and the outbound CannotComply POST could lose a
// Response the utility is owed, and a restart mid-event-lifecycle could
// re-POST a duplicate Received/Started/... for an event already
// acknowledged. This file adds a small self-compacting NDJSON append-log
// (Store) plus a pure replay reader (LoadState) so Tracker can survive a
// restart without either gap — see tracker.go's New/set/AlertCannotComply/
// ClearAlerts call sites for how it's wired in.
//
// Format choice (over reusing internal/journal.Writer directly): journal's
// rotation model drops whole rotated files by BYTE SIZE, with no concept of
// "this specific entry is still live" — a live non-terminal posted[] entry
// or a not-yet-superseded alerted[] key could silently roll off the back of
// MaxFiles during a long-running process, which is the wrong compaction
// semantic for "reconstruct current state," not "keep N days of forensic
// history" (journal's actual job — TASK-040's dispatch/breach/
// cannot_comply_posted events, which this file does NOT duplicate: that
// journal's cannot_comply_posted event, already wired in tracker.go, is a
// point-in-time forensic record and is orthogonal to the durable dedupe
// state this file exists to give restart survivability). journal also has
// no Load/replay-to-current-state API — reader.go is a forensic
// scan/range reader, not a state-reconstruction one. A dedicated, much
// smaller log-compacted store (append transitions; periodically rewrite to
// just the current live state, same atomic tmp+rename+fsync technique as
// cmd/hub/snapshot.go) fits this narrower job better and stays tiny: the
// live state is bounded by in-flight DERControl events + open breach
// episodes, not by process uptime.
import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// stateSchemaV is this file's own schema version (independent of
// internal/journal's SchemaV and cmd/hub/snapshot.go's hubSnapshotV — a
// different file, a different format). Bump only on a breaking change to
// stateEntry's shape.
const stateSchemaV = 1

// DefaultCompactBytes bounds the append-log between self-compactions. The
// live state here is tiny (in-flight events + open breach episodes, a
// handful of entries in normal operation) — 16 KiB comfortably holds
// hundreds of transition lines before a compaction fires, so this default
// needs no tuning on the bench.
const DefaultCompactBytes = 16 << 10 // 16 KiB

const (
	opPosted  = "posted"  // one Tracker.posted[mrid]=status transition
	opAlerted = "alerted" // a CONFIRMED (POST succeeded) CannotComply for mrid(+episodeID)
	opClear   = "clear"   // Tracker.ClearAlerts(): breach episode ended, re-arm
	opCompact = "compact" // full checkpoint superseding every prior line
)

// stateEntry is one NDJSON line. Only the fields relevant to Op are
// populated; the rest stay zero/omitted.
type stateEntry struct {
	V         int    `json:"v"`
	Ts        int64  `json:"ts"`
	Op        string `json:"op"`
	MRID      string `json:"mrid,omitempty"`
	Status    uint8  `json:"status,omitempty"`     // opPosted
	EpisodeID string `json:"episode_id,omitempty"` // opAlerted

	Posted  map[string]uint8 `json:"posted,omitempty"`  // opCompact only
	Alerted []string         `json:"alerted,omitempty"` // opCompact only
}

// ErrStateCorrupt is returned by LoadState when a state file exists, is
// non-empty, and contains not one successfully-parsed entry (as opposed to
// the ordinary crash-recovery case of a single torn trailing line, which is
// tolerated silently — same discipline as internal/journal's resumeSeq).
var ErrStateCorrupt = errors.New("responses: state file has no valid entries")

// ErrStateVersion is returned by LoadState for a v field this binary does
// not understand.
var ErrStateVersion = errors.New("responses: unsupported state schema version")

// State is Tracker's reconstructed posted/alerted maps, as read back by
// LoadState and consumed by New's initial parameter.
type State struct {
	Posted  map[string]uint8
	Alerted map[string]bool
}

// LoadState reads and replays path (an NDJSON append-log written by
// *Store), applying each entry in file order, and returns the resulting
// State. A missing file returns os.ErrNotExist unwrapped (os.Open's own
// sentinel), matching cmd/hub/snapshot.go's loadHubSnapshot convention, so
// callers can special-case "no state yet" without a fault-level log. Every
// other error (corrupt file, unsupported version) is returned typed and the
// caller must start empty — this store is dedupe-hint state, never a
// source of truth worth blocking startup over (mirrors AD-011 crash-only:
// a fault here must never be fatal).
func LoadState(path string) (State, error) {
	empty := State{Posted: map[string]uint8{}, Alerted: map[string]bool{}}

	f, err := os.Open(path)
	if err != nil {
		return empty, err
	}
	defer f.Close()

	st := State{Posted: map[string]uint8{}, Alerted: map[string]bool{}}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)

	parsedAny := false
	corrupted := 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e stateEntry
		if err := json.Unmarshal(line, &e); err != nil {
			corrupted++
			continue // tolerate a torn tail line from a prior crash mid-write
		}
		if e.V != stateSchemaV {
			return empty, fmt.Errorf("%w: got %d want %d", ErrStateVersion, e.V, stateSchemaV)
		}
		applyEntry(&st, e)
		parsedAny = true
	}
	if err := sc.Err(); err != nil {
		return st, fmt.Errorf("responses: scan %s: %w", path, err)
	}
	if !parsedAny && corrupted > 0 {
		return empty, fmt.Errorf("%w: %s", ErrStateCorrupt, path)
	}
	return st, nil
}

// applyEntry folds one log entry into st, in file order.
//
// A opPosted entry with a TERMINAL status (Completed/Cancelled/Superseded)
// PRUNES mrid from st.Posted rather than storing it: this is the "entries
// for expired events get pruned" requirement, and terminalResponse is the
// exact event-lifecycle signal Tracker already has for it (tracker.go's
// pass-1/pass-2 logic gates every further transition on
// !terminalResponse(last) — a terminal event never transitions again, so a
// restart has nothing left to protect by remembering it; see
// TASK-041's original design note, "snapshot only what a restart needs:
// non-terminal statuses + alerted set").
func applyEntry(st *State, e stateEntry) {
	switch e.Op {
	case opPosted:
		if terminalResponse(e.Status) {
			delete(st.Posted, e.MRID)
		} else {
			st.Posted[e.MRID] = e.Status
		}
	case opAlerted:
		st.Alerted[e.MRID] = true
		if e.EpisodeID != "" {
			st.Alerted[e.EpisodeID] = true
		}
	case opClear:
		st.Alerted = map[string]bool{}
	case opCompact:
		posted := make(map[string]uint8, len(e.Posted))
		for k, v := range e.Posted {
			if !terminalResponse(v) { // belt-and-suspenders: Store never
				posted[k] = v // *writes* a terminal entry into a compact line
			}
		}
		alerted := make(map[string]bool, len(e.Alerted))
		for _, k := range e.Alerted {
			alerted[k] = true
		}
		st.Posted = posted
		st.Alerted = alerted
	}
}

// Store is the write side of response-state persistence: it appends one
// NDJSON line per Tracker mutation to Path, fsyncing every write (this
// data is small and rare — state CHANGES only, never per-poll-cycle — so
// internal/journal's batched-fsync budgeting is unneeded machinery here;
// see this file's package doc). Once the file exceeds CompactBytes,
// Compact atomically rewrites it (tmp+rename+fsync, same technique as
// cmd/hub/snapshot.go's saveHubSnapshot) down to a single checkpoint line
// of the current live state, bounding disk usage by live-event count
// rather than process uptime.
//
// Nil-safe: every method on a nil *Store is a no-op (matches
// metrics.Counter/journal.Writer's nil-receiver convention throughout this
// codebase), so a caller that fails to open the store can pass nil through
// unchanged and Tracker runs exactly as it did before WS-4.2 — RAM-only.
type Store struct {
	Path         string
	CompactBytes int64 // <=0 -> DefaultCompactBytes

	mu      sync.Mutex
	f       *os.File
	size    int64
	failing bool // edge-triggered error state, mirrors journal.Writer's fail()/recovered()
}

// OpenStore creates Path's directory (0755) if needed and opens (or
// creates, 0644) it for append.
func OpenStore(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("responses: empty state path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("responses: mkdir %s: %w", dir, err)
	}
	s := &Store{Path: path}
	if err := s.reopenLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) compactBytes() int64 {
	if s.CompactBytes <= 0 {
		return DefaultCompactBytes
	}
	return s.CompactBytes
}

// AppendPosted durably records a Tracker.posted[mrid]=status transition.
// Called unconditionally from set() — matching set()'s own unconditional
// in-memory record (tracker.go: "a failed POST here was already silently
// swallowed before this task, and stays that way").
func (s *Store) AppendPosted(mrid string, status uint8) {
	s.append(stateEntry{Op: opPosted, MRID: mrid, Status: status})
}

// AppendAlerted durably records a CONFIRMED CannotComply POST for mrid
// (and, when present, its episode ID). Callers must only call this AFTER
// postResponse has reported success — see AlertCannotComply's doc for why
// persisting the pre-POST "recorded" mark would reintroduce exactly the
// bug this file exists to close (a crash between the mark and the POST
// would durably swallow a CannotComply the utility never actually got).
func (s *Store) AppendAlerted(mrid, episodeID string) {
	s.append(stateEntry{Op: opAlerted, MRID: mrid, EpisodeID: episodeID})
}

// AppendClear durably records ClearAlerts: the breach episode ended, so a
// future breach must re-alert.
func (s *Store) AppendClear() {
	s.append(stateEntry{Op: opClear})
}

func (s *Store) append(e stateEntry) {
	if s == nil {
		return
	}
	e.V = stateSchemaV
	e.Ts = time.Now().Unix()
	b, err := json.Marshal(e)
	if err != nil {
		s.fail(fmt.Errorf("responses: marshal state entry: %w", err))
		return
	}
	b = append(b, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		if err := s.reopenLocked(); err != nil {
			s.fail(fmt.Errorf("responses: reopen state file: %w", err))
			return
		}
	}
	n, werr := s.f.Write(b)
	s.size += int64(n)
	if werr != nil {
		s.fail(fmt.Errorf("responses: write state entry: %w", werr))
		return
	}
	if err := s.f.Sync(); err != nil {
		s.fail(fmt.Errorf("responses: fsync state file: %w", err))
		return
	}
	s.recovered()
}

// reopenLocked (re)opens s.Path for append and syncs s.size to its current
// on-disk length. Must be called with s.mu held, except from OpenStore.
func (s *Store) reopenLocked() error {
	f, err := os.OpenFile(s.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("responses: open %s: %w", s.Path, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("responses: stat %s: %w", s.Path, err)
	}
	s.f = f
	s.size = info.Size()
	return nil
}

// NeedsCompact reports whether the append log has grown past its compact
// threshold.
func (s *Store) NeedsCompact() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size > s.compactBytes()
}

// Compact atomically rewrites the state file to a single opCompact line
// representing exactly the (already terminal-pruned) live state in posted
// and the keys of confirmedAlerted. Safe to call directly (e.g. from a
// test) as well as via Tracker's automatic NeedsCompact-triggered path.
func (s *Store) Compact(posted map[string]uint8, confirmedAlerted map[string]bool) error {
	if s == nil {
		return nil
	}
	live := make(map[string]uint8, len(posted))
	for k, v := range posted {
		if !terminalResponse(v) {
			live[k] = v
		}
	}
	keys := make([]string, 0, len(confirmedAlerted))
	for k := range confirmedAlerted {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic file content

	e := stateEntry{V: stateSchemaV, Ts: time.Now().Unix(), Op: opCompact, Posted: live, Alerted: keys}
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("responses: marshal compact entry: %w", err)
	}
	b = append(b, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()

	tmp := s.Path + ".tmp"
	tf, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("responses: create tmp %s: %w", tmp, err)
	}
	if _, err := tf.Write(b); err != nil {
		tf.Close()
		os.Remove(tmp)
		return fmt.Errorf("responses: write tmp %s: %w", tmp, err)
	}
	if err := tf.Sync(); err != nil {
		tf.Close()
		os.Remove(tmp)
		return fmt.Errorf("responses: fsync tmp %s: %w", tmp, err)
	}
	if err := tf.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("responses: close tmp %s: %w", tmp, err)
	}

	// Close the currently-open append fd BEFORE the rename: the next
	// append() lazily reopens (reopenLocked) against the new file rather
	// than continuing to append to the old inode a rename would leave it
	// pointing at — same self-heal shape as internal/journal's
	// rotateIfNeeded closing before reopening.
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
	}
	if err := os.Rename(tmp, s.Path); err != nil {
		return fmt.Errorf("responses: rename %s -> %s: %w", tmp, s.Path, err)
	}
	s.size = int64(len(b))
	return nil
}

// Close closes the underlying file handle. Safe to call on a nil *Store.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

func (s *Store) fail(err error) {
	if !s.failing {
		s.failing = true
		slog.Error("lexa-northbound: response-state persistence failing", "path", s.Path, "err", err)
	}
}

func (s *Store) recovered() {
	if s.failing {
		s.failing = false
		slog.Info("lexa-northbound: response-state persistence recovered", "path", s.Path)
	}
}
