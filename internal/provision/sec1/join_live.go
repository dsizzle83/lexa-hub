package sec1

import (
	"context"

	"lexa-hub/internal/provision/frame"
)

// JoinRunner drives a real (non-scripted) join. The peripheral calls it in its
// own goroutine when a JoinLive behavior handles a join request, passing the
// decoded request and an emit callback. The runner must call emit for each
// state transition — zero or more StateJoining, then exactly one terminal
// StateJoined/StateFailed — then return. ctx is cancelled when the join is
// superseded (a retry within the session, or a fresh handshake); a well-behaved
// runner stops promptly on ctx.Done(). emit encrypts, frames, and delivers each
// StateMessage on the status characteristic via the peripheral's AsyncSend
// transport, so it is safe to call from the runner's goroutine.
type JoinRunner func(ctx context.Context, req Join, emit func(StateMessage))

// JoinLive is a JoinBehavior backed by a real join driver (unit B3's
// NetworkManager join). Unlike the scripted JoinSucceeds/JoinFails/JoinHangs,
// its states arrive over time and stream ASYNCHRONOUSLY on the status
// characteristic via the peripheral's AsyncSend transport: the triggering
// config write is answered immediately (empty synchronous response), and the
// join runs in the background.
type JoinLive struct{ Run JoinRunner }

func (JoinLive) isJoinBehavior() {}

// emitStatusAsync encrypts, frames, and delivers one join StateMessage on the
// status characteristic OUTSIDE the HandleChunk return path — the streaming
// path a JoinLive runner uses. It is safe to call from the runner's goroutine:
// it takes the peripheral's send mutex (shared with HandleChunk) so session
// encryption counters stay single-threaded, and it drops the message if the
// session is gone or no AsyncSend transport is wired. A message from a
// superseded session is encrypted under a now-dead key, so a stale central can
// never decode it — cheap, cryptographically-inert cleanup.
func (p *Peripheral) emitStatusAsync(sm StateMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.session == nil || !p.confirmed || p.asyncSend == nil {
		return
	}
	if sm.State == StateJoined && sm.Handoff != nil {
		sm.Handoff = p.withSerial(*sm.Handoff)
	}
	payload, err := sm.Encode()
	if err != nil {
		p.LastError = err
		return
	}
	ct, err := p.session.Encrypt(payload)
	if err != nil {
		p.LastError = err
		return
	}
	chunks, err := frame.Chunk(ct, p.attPayloadSize, true)
	if err != nil {
		p.LastError = err
		return
	}
	for _, c := range chunks {
		p.asyncSend(Outbound{UUID: UUIDStatus, Chunk: c})
	}
}

// cancelJoin cancels any in-flight JoinLive runner (a retry or a fresh handshake
// supersedes the previous join). Must be called with p.mu held.
func (p *Peripheral) cancelJoin() {
	if p.joinCancel != nil {
		p.joinCancel()
		p.joinCancel = nil
	}
}
