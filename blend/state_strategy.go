// The state-fold strategy seam: one object owns the whole per-ledger fold
// chain — prior-load, change apply, snapshot/build — and is selected once at
// New (Config.StateMode). Strategies swap as whole classes, never as
// per-component flags threaded through calls.
//
// The two-mode contract:
//
//   - paranoid (default) is TODAY'S code path, delegated verbatim: a stateless
//     pure reducer that reconstructs the full builder mirror from the prior
//     LedgerState every ledger (loadPrior) and reassembles + re-sorts the full
//     typed state (build). O(total state) per ledger. It keeps no cross-call
//     state whatsoever, which is why it survives as the reference oracle: any
//     other strategy is correct exactly insofar as it matches paranoid
//     byte for byte.
//
//   - incremental carries the builder mirror across ledgers and re-derives only
//     what a ledger's changes touched, at O(changes) fold cost instead of
//     O(total state). Its output MUST be byte-identical to paranoid's — same
//     slice contents, same ordering, same serialized checksums — on every
//     ledger; the parity suite in state_parity_test.go enforces this in CI and
//     is the permanent gate for any change to either side.
//
// Paranoid is not legacy. It stays the default, it defines the semantics, and
// every incremental optimization is validated against it. Consumers opt into
// incremental deliberately (the relay config-selects it per deployment).
package blend

import (
	"time"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/lightgatehq/lidapters/blend/contracts"
)

// stateStrategy folds one ledger's owned contract_data changes into the next
// typed LedgerState (plus the in-package silver-debug deltas, the exposed
// dirty-positions set — see bindings.DirtyPosition — the fold's skipped-leg
// diagnostics — see bindings.DecodeDiagnostic — and the fold's
// auction/queued-reserve transition set — see bindings.TemporaryStateChange).
// Implementations own the entire chain; DecodeState/DecodeStateAt delegate
// here blindly.
type stateStrategy interface {
	decodeState(prior *bindings.LedgerState, changes []bindings.ContractDataChange, ledgerSeq int64, closeTime time.Time) (*bindings.LedgerState, []typedStateDelta, []bindings.DirtyPosition, []bindings.DirtyBackstop, []bindings.DecodeDiagnostic, []bindings.TemporaryStateChange)
}

// dirtyUserPositions is an optional capability a stateStrategy MAY implement:
// answer one user's resolved position rows in O(1) rather than requiring a
// caller to scan the whole output LedgerState.Users slice. The incremental
// strategy already maintains exactly this index (s.index, keyed the same way
// as pendingPos) to serve its own cached-block invalidation, so exposing it
// costs nothing extra. Adapter.ProjectPositions (math.go) uses this when the
// active strategy implements it, which is what makes a per-ledger dirty-set
// projection genuinely O(dirty users) instead of O(all users) — paranoid does
// not implement it (it caches nothing) and ProjectPositions falls back to a
// full scan for that mode, which is no worse than paranoid's existing
// O(total state) per-ledger cost.
type dirtyUserPositions interface {
	userPositions(address, pool string) []contracts.UserReservePosition
}

// paranoidStrategy delegates to the reference reducer (decodeBlendState in
// state.go) untouched: fresh mirror from prior, apply, full build. Stateless.
type paranoidStrategy struct {
	adapter *Adapter
}

func (s *paranoidStrategy) decodeState(prior *bindings.LedgerState, changes []bindings.ContractDataChange, ledgerSeq int64, closeTime time.Time) (*bindings.LedgerState, []typedStateDelta, []bindings.DirtyPosition, []bindings.DirtyBackstop, []bindings.DecodeDiagnostic, []bindings.TemporaryStateChange) {
	next, deltas, dirty, dirtyBackstops, diagnostics, temporary := s.adapter.decodeBlendState(prior, changes, ledgerSeq, closeTime)
	return &next, deltas, dirty, dirtyBackstops, diagnostics, temporary
}
