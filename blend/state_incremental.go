// The incremental state-fold strategy: a persistent builder mirror that lives
// across ledgers, plus caches for the two O(total state) products the paranoid
// path re-derives every ledger (the per-user position blocks and their global
// sort order). Per-ledger work drops from O(total state) to O(changes) plus
// the unavoidable O(state) copy that materializes the output slices.
//
// Byte-identity with paranoid is the invariant everything here serves. Two
// mechanisms carry it:
//
//  1. normalizeCarry replays, at the start of every ledger, exactly the lossy
//     parts of paranoid's build->loadPrior round-trip, so the carried mirror is
//     indistinguishable from a freshly reloaded one when apply runs.
//
//  2. snapshot assembles the output with the same statements build uses for
//     every component that is cheap (pools, backstops, oracles, feeds,
//     aggregators, assets), and replaces only the two expensive products with
//     cache-maintained equivalents whose ordering provably matches the
//     paranoid sort (all sort keys are distinct, so the unstable global sort
//     has exactly one possible result: sorted (address, pool) blocks, each
//     internally sorted by (asset, position type)).
//
// The parity suite in state_parity_test.go enforces the invariant in CI.
//
// Statefulness contract: the adapter carries fold state between calls, so an
// incremental adapter must not be shared by concurrent folds, and the
// LedgerState it returns must be treated as immutable — it is the baseline the
// next call builds on. decodeState is still a pure function of (prior content,
// changes): the carried mirror is only trusted when prior IS the strategy's own
// last output (pointer identity); any other prior — first ledger, checkpoint
// restore, caller-side rewind — reseeds the mirror from it, paranoid-style.
package blend

import (
	"sort"
	"time"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/lightgatehq/lidapters/blend/contracts"
)

// userEntry is the cached view of one pendingPos entry: its identity, its
// ready-to-emit PendingUserPosition, and its computed position block —
// pool-resolved typed positions ordered exactly as paranoid's global users
// sort orders them within one (address, pool).
type userEntry struct {
	address   string
	pool      string
	composite string // typedUserEntityKey(address, pool), the pendingPos key
	out       contracts.PendingUserPosition
	block     []contracts.UserReservePosition
}

type incrementalStrategy struct {
	adapter *Adapter
	// mirror is the persistent builder. Between ledgers it holds exactly what a
	// paranoid loadPrior of the last output would reconstruct (normalizeCarry
	// maintains that equivalence at each ledger start).
	mirror *blendStateBuilder
	// lastOut is the LedgerState returned by the previous decodeState call. The
	// carried mirror is only valid as a continuation of that exact state.
	lastOut *bindings.LedgerState

	// keys is every pendingPos entry, sorted by (address, pool) — the order the
	// paranoid sort gives both the PendingUserPositions and (blockwise) the
	// Users output. index resolves a composite key to its entry.
	keys  []*userEntry
	index map[string]*userEntry
	// usersLen is the summed block length, maintained so the Users slice is
	// allocated exactly once per snapshot.
	usersLen int
	// poolIndexes is each pool's reserveByIndex as of the last snapshot. A
	// user's block depends on its pool ONLY through reserveByIndex (positions
	// resolve reserve index -> asset through it), so a changed mapping — and
	// nothing else about the pool — invalidates the pool's cached blocks. Pool
	// appearance and disappearance are detected here too.
	poolIndexes map[string]map[int32]string
	// dirty is the set of pendingPos keys touched since the last snapshot. It is
	// the same map the mirror's apply writes through (blendStateBuilder.dirtyUsers).
	dirty map[string]userIdentity

	// bisect pins individual incremental players back to their paranoid
	// equivalent. Test-only (same-package); never reachable from public config.
	// It exists so a parity divergence can be bisected to a single player: pin
	// one, rerun parity, repeat.
	bisect struct {
		// reloadEachLedger discards the carried mirror and reseeds from prior on
		// every ledger — the prior-load player pinned to paranoid.
		reloadEachLedger bool
		// rebuildSnapshot ignores the block/order caches and assembles
		// pending/users by full rebuild + sort — the snapshot player pinned to
		// paranoid. Cache maintenance still runs, so the caches themselves stay
		// exercisable underneath a pinned assembly.
		rebuildSnapshot bool
	}
}

func newIncrementalStrategy(a *Adapter) *incrementalStrategy {
	return &incrementalStrategy{adapter: a}
}

func (s *incrementalStrategy) decodeState(prior *bindings.LedgerState, changes []bindings.ContractDataChange, ledgerSeq int64, closeTime time.Time) (*bindings.LedgerState, []typedStateDelta, []bindings.DirtyPosition, []bindings.DirtyBackstop, []bindings.DecodeDiagnostic, []bindings.TemporaryStateChange) {
	if s.mirror == nil || prior != s.lastOut || s.bisect.reloadEachLedger {
		// Not a continuation of our own last output: rebuild the mirror from
		// prior exactly as paranoid would. One O(total state) ledger, then the
		// carry takes over.
		s.reseed(prior)
	} else {
		s.normalizeCarry()
	}
	s.mirror.ledgerSeq = ledgerSeq
	for _, change := range changes {
		s.mirror.apply(change, ledgerSeq)
	}
	sortTypedStateDeltas(s.mirror.deltas)
	out, dirty, dirtyBackstops := s.snapshot(closeTime)
	sortDecodeDiagnostics(s.mirror.diagnostics)
	temporary := finalizeTemporaryStateChanges(s.mirror.dirtyTemporary, s.mirror.auctions, s.mirror.queuedReserves)
	s.lastOut = out
	return out, s.mirror.deltas, dirty, dirtyBackstops, s.mirror.diagnostics, temporary
}

// reseed rebuilds the mirror from prior via the same loadPrior the paranoid
// path runs, then rebuilds the caches from it in one pass.
func (s *incrementalStrategy) reseed(prior *bindings.LedgerState) {
	b := newBlendStateBuilder()
	s.mirror = b
	s.refreshOwned()
	s.dirty = map[string]userIdentity{}
	b.dirtyUsers = s.dirty
	if prior != nil {
		b.loadPrior(prior)
	}

	s.keys = make([]*userEntry, 0, len(b.pendingPos))
	s.index = make(map[string]*userEntry, len(b.pendingPos))
	s.usersLen = 0
	for composite, pending := range b.pendingPos {
		entry := &userEntry{
			address:   pending.user,
			pool:      pending.poolContract,
			composite: composite,
			out:       pendingOut(pending),
			block:     s.computeBlock(pending, nil),
		}
		s.keys = append(s.keys, entry)
		s.index[composite] = entry
		s.usersLen += len(entry.block)
	}
	sort.Slice(s.keys, func(i, j int) bool { return userEntryLess(s.keys[i], s.keys[j]) })

	s.poolIndexes = make(map[string]map[int32]string, len(b.pools))
	for id, pool := range b.pools {
		s.poolIndexes[id] = copyReserveIndex(pool.reserveByIndex)
	}
}

// refreshOwned re-reads the adapter's ownership sets, exactly as the paranoid
// path does at the top of every decode. Register* may allocate a fresh map when
// called for the first time (mid-fold discovery), so a reference taken at
// reseed alone could go stale.
func (s *incrementalStrategy) refreshOwned() {
	s.mirror.owned = s.adapter.contracts
	s.mirror.ownedAssets = s.adapter.assets
	s.mirror.ownedFeeds = s.adapter.feeds
	s.mirror.ownedComets = s.adapter.comets
	s.mirror.protocol = s.adapter.cfg.Protocol
}

// normalizeCarry replays the lossy parts of the paranoid build->loadPrior
// round-trip on the live mirror, so this ledger's apply starts from exactly
// the mirror a fresh reload of lastOut would produce:
//
//   - deltas are per-ledger scratch — reset; the skipped-leg diagnostics are
//     the same per-ledger scratch — reset; the temporary-state dirty set is
//     the same per-ledger scratch — reset;
//   - synthesized oracles are excluded from the Oracles carry (buildOracles
//     skips them; they are re-derived every ledger from aggregator + feed
//     state) — dropped;
//   - backstopPools is not carried in its own right: loadPrior reconstructs it
//     from each pool's own Backstop*Raw fields, so an entry for an unknown pool
//     or with all-empty fields does not survive a ledger boundary — rebuilt by
//     the same rule.
//
// Everything else in the mirror round-trips losslessly (verified by the parity
// suite), so it is carried as-is.
func (s *incrementalStrategy) normalizeCarry() {
	b := s.mirror
	s.refreshOwned()
	b.deltas = nil
	b.diagnostics = nil
	b.dirtyTemporary = map[string]bindings.TemporaryStateChange{}
	// Per-ledger dirty/price-invalidation scratch, reset exactly as a fresh
	// paranoid builder starts empty: the fold's affected-holder set reflects
	// only this ledger's changes.
	b.dirtyBackstops = map[string]backstopIdentity{}
	b.changedFeeds = map[string]struct{}{}
	b.changedPriceAssets = map[string]struct{}{}
	for id, oracle := range b.oracles {
		if oracle.synthesized {
			delete(b.oracles, id)
		}
	}
	b.backstopPools = map[string]backstopPoolBalance{}
	for id, pool := range b.pools {
		if pool.state.BackstopSharesRaw != "" || pool.state.BackstopTokensRaw != "" || pool.state.BackstopQ4WSharesRaw != "" {
			b.backstopPools[id] = backstopPoolBalance{
				poolContract: id,
				sharesRaw:    pool.state.BackstopSharesRaw,
				tokensRaw:    pool.state.BackstopTokensRaw,
				q4wRaw:       pool.state.BackstopQ4WSharesRaw,
			}
		}
	}
	// backstopEmis follows the identical round-trip rule as backstopPools: it is
	// not carried in its own right — loadPrior reconstructs it from each pool's
	// BackstopEmis*Raw fields, so an entry for an unknown pool or with all-empty
	// fields does not survive a ledger boundary.
	b.backstopEmis = map[string]backstopEmisData{}
	for id, pool := range b.pools {
		emis := backstopEmisData{
			poolContract:  id,
			epsRaw:        pool.state.BackstopEmisEPSRaw,
			expirationRaw: pool.state.BackstopEmisExpirationRaw,
			indexRaw:      pool.state.BackstopEmisIndexRaw,
			lastTimeRaw:   pool.state.BackstopEmisLastTimeRaw,
		}
		if !emis.empty() {
			b.backstopEmis[id] = emis
		}
	}
}

// snapshot assembles the next LedgerState. Component for component it mirrors
// build() in state.go — same statements, same order — except pending/users,
// which come from the caches. Any edit to build() must be mirrored here; the
// parity suite fails loudly when the two drift.
func (s *incrementalStrategy) snapshot(closeTime time.Time) (*bindings.LedgerState, []bindings.DirtyPosition, []bindings.DirtyBackstop) {
	b := s.mirror
	b.resolveAggregatorPrices(closeTime)
	b.resolveOraclePrices()

	pools := make([]contracts.PoolState, 0, len(b.pools))
	for _, pool := range b.pools {
		finalizePoolReserves(pool)
		if balance, ok := b.backstopPools[pool.state.ContractID]; ok {
			pool.state.BackstopSharesRaw = balance.sharesRaw
			pool.state.BackstopTokensRaw = balance.tokensRaw
			pool.state.BackstopQ4WSharesRaw = balance.q4wRaw
		}
		mergeBackstopEmis(pool, b.backstopEmis)
		pools = append(pools, pool.state)
	}
	sort.Slice(pools, func(i, j int) bool { return pools[i].ContractID < pools[j].ContractID })

	// A pool whose reserveByIndex mapping changed this ledger (including a pool
	// that appeared or disappeared) invalidates every cached block of its users
	// — the paranoid analog is that build re-resolves every user against the
	// fresh mapping unconditionally.
	changedPools := map[string]struct{}{}
	for id, pool := range b.pools {
		previous, tracked := s.poolIndexes[id]
		if !tracked || !reserveIndexEqual(previous, pool.reserveByIndex) {
			changedPools[id] = struct{}{}
			s.poolIndexes[id] = copyReserveIndex(pool.reserveByIndex)
		}
	}
	for id := range s.poolIndexes {
		if _, ok := b.pools[id]; !ok {
			changedPools[id] = struct{}{}
			delete(s.poolIndexes, id)
		}
	}
	if len(changedPools) > 0 {
		// Cached entries only: an entry added or deleted THIS ledger is already
		// in the dirty set via the apply hook and will be (re)computed against
		// the fresh mapping anyway.
		for _, entry := range s.keys {
			if _, ok := changedPools[entry.pool]; ok {
				s.dirty[entry.composite] = userIdentity{address: entry.address, pool: entry.pool}
			}
		}
	}

	// Captured before the cache-update loop clears entries out of pendingPos
	// bookkeeping below (removeKey/delete don't touch b.pendingPos itself, but
	// s.dirty is cleared at the end of this pass either way) — same derivation
	// paranoid's decodeBlendState uses (finalizeDirtyPositions), so the two
	// strategies expose an identical set for the same ledger.
	dirty := finalizeDirtyPositions(s.dirty, b.pendingPos)

	// The affected-backstop analog of the line above: same derivation paranoid's
	// decodeBlendState runs (feed-price propagation, then finalize against the
	// post-apply balances), so the two strategies expose an identical set.
	b.propagateFeedPriceDirty()
	for assetID := range b.changedPriceAssets {
		b.markPriceAssetDirty(assetID)
	}
	dirtyBackstops := finalizeDirtyBackstops(b.dirtyBackstops, b.backstopUsers)

	for composite, identity := range s.dirty {
		pending, live := b.pendingPos[composite]
		entry, present := s.index[composite]
		// Every entry in this loop is a pair this ledger touched, so its block
		// recomputation is exactly where the fold's skipped-leg diagnostics are
		// recorded — the incremental analog of paranoid's build() running the
		// sink for the dirty pairs. The carried-cache rebuilds (reseed,
		// rebuildPendingUsers) pass a nil sink: they are not this fold's output
		// computation.
		sink := &positionSkipSink{ledgerSeq: b.ledgerSeq, out: &b.diagnostics}
		switch {
		case live && present:
			entry.out = pendingOut(pending)
			s.usersLen -= len(entry.block)
			entry.block = s.computeBlock(pending, sink)
			s.usersLen += len(entry.block)
		case live && !present:
			entry = &userEntry{
				address:   identity.address,
				pool:      identity.pool,
				composite: composite,
				out:       pendingOut(pending),
				block:     s.computeBlock(pending, sink),
			}
			s.insertKey(entry)
			s.index[composite] = entry
			s.usersLen += len(entry.block)
		case !live && present:
			s.removeKey(entry)
			delete(s.index, composite)
			s.usersLen -= len(entry.block)
		}
	}
	clear(s.dirty)

	var pending []contracts.PendingUserPosition
	var users []contracts.UserReservePosition
	if s.bisect.rebuildSnapshot {
		pending, users = s.rebuildPendingUsers()
	} else {
		pending = make([]contracts.PendingUserPosition, 0, len(s.keys))
		users = make([]contracts.UserReservePosition, 0, s.usersLen)
		for _, entry := range s.keys {
			pending = append(pending, entry.out)
			users = append(users, entry.block...)
		}
	}

	backstops := make([]contracts.BackstopPosition, 0, len(b.backstopUsers))
	for _, userBalance := range b.backstopUsers {
		backstops = append(backstops, b.backstopPosition(userBalance))
	}
	sort.Slice(backstops, func(i, j int) bool {
		if backstops[i].Address != backstops[j].Address {
			return backstops[i].Address < backstops[j].Address
		}
		return backstops[i].PoolContractID < backstops[j].PoolContractID
	})

	return &bindings.LedgerState{
		Pools:                pools,
		Users:                users,
		Backstops:            backstops,
		PendingUserPositions: pending,
		Oracles:              b.buildOracles(),
		PriceFeeds:           b.buildPriceFeeds(),
		OracleAggregators:    b.buildOracleAggregators(),
		Assets:               b.buildAssets(),
		Auctions:             b.buildAuctions(),
		UserEmissions:        b.buildUserEmissions(),
		QueuedReserves:       b.buildQueuedReserves(),
		BackstopInstances:    b.buildBackstopInstances(),
		AMMPools:             b.buildAMMPools(),
	}, dirty, dirtyBackstops
}

// rebuildPendingUsers is the paranoid assembly of pending/users (build()'s
// pendingPos loop plus the global sorts), for the bisect knob only.
func (s *incrementalStrategy) rebuildPendingUsers() ([]contracts.PendingUserPosition, []contracts.UserReservePosition) {
	b := s.mirror
	pending := make([]contracts.PendingUserPosition, 0, len(b.pendingPos))
	users := make([]contracts.UserReservePosition, 0)
	for _, p := range b.pendingPos {
		pending = append(pending, pendingOut(p))
		pool := b.pools[p.poolContract]
		if pool == nil {
			continue
		}
		finalizePoolReserves(pool)
		users = append(users, buildUserPositionsForPending(pool, p, nil)...)
	}
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].Address != pending[j].Address {
			return pending[i].Address < pending[j].Address
		}
		return pending[i].PoolContractID < pending[j].PoolContractID
	})
	sort.Slice(users, func(i, j int) bool {
		if users[i].Address != users[j].Address {
			return users[i].Address < users[j].Address
		}
		if users[i].PoolContractID != users[j].PoolContractID {
			return users[i].PoolContractID < users[j].PoolContractID
		}
		if users[i].AssetID != users[j].AssetID {
			return users[i].AssetID < users[j].AssetID
		}
		return users[i].PositionType < users[j].PositionType
	})
	return pending, users
}

// computeBlock resolves one pending entry's positions against its pool —
// buildUserPositionsForPending, then the block ordered by (asset, position
// type). Concatenating such blocks in (address, pool) key order reproduces the
// paranoid global users sort exactly: the full sort key (address, pool, asset,
// type) is distinct per element, so the unstable sort has a single possible
// result, and address/pool are constant within a block. The sink is non-nil
// only when the recomputation IS this ledger's dirty-pair update (see
// snapshot); cache rebuilds pass nil.
func (s *incrementalStrategy) computeBlock(pending pendingUserPositions, sink *positionSkipSink) []contracts.UserReservePosition {
	pool := s.mirror.pools[pending.poolContract]
	if pool == nil {
		return nil
	}
	block := buildUserPositionsForPending(pool, pending, sink)
	sort.Slice(block, func(i, j int) bool {
		if block[i].AssetID != block[j].AssetID {
			return block[i].AssetID < block[j].AssetID
		}
		return block[i].PositionType < block[j].PositionType
	})
	return block
}

// userPositions implements dirtyUserPositions (state_strategy.go): an O(1)
// lookup of one user's cached, already-resolved position block, keyed the
// same way as pendingPos. Adapter.ProjectPositions (math.go) uses this to
// make single-user projection O(dirty users) instead of scanning the whole
// output LedgerState.Users slice.
func (s *incrementalStrategy) userPositions(address, pool string) []contracts.UserReservePosition {
	entry, ok := s.index[typedUserEntityKey(address, pool)]
	if !ok {
		return nil
	}
	return entry.block
}

func pendingOut(p pendingUserPositions) contracts.PendingUserPosition {
	return contracts.PendingUserPosition{
		Address:           p.user,
		PoolContractID:    p.poolContract,
		PositionsXDR:      p.valueXDR,
		Archived:          p.archived,
		ArchivedLedgerSeq: p.archivedLedgerSeq,
	}
}

// userEntryLess orders entries by (address, pool) — the same comparator the
// paranoid sort applies to PendingUserPositions and, blockwise, to Users.
func userEntryLess(a, b *userEntry) bool {
	if a.address != b.address {
		return a.address < b.address
	}
	return a.pool < b.pool
}

func (s *incrementalStrategy) insertKey(entry *userEntry) {
	at := sort.Search(len(s.keys), func(i int) bool { return !userEntryLess(s.keys[i], entry) })
	s.keys = append(s.keys, nil)
	copy(s.keys[at+1:], s.keys[at:])
	s.keys[at] = entry
}

func (s *incrementalStrategy) removeKey(entry *userEntry) {
	at := sort.Search(len(s.keys), func(i int) bool { return !userEntryLess(s.keys[i], entry) })
	for at < len(s.keys) && s.keys[at] != entry {
		at++
	}
	if at < len(s.keys) {
		s.keys = append(s.keys[:at], s.keys[at+1:]...)
	}
}

func copyReserveIndex(m map[int32]string) map[int32]string {
	out := make(map[int32]string, len(m))
	for index, assetID := range m {
		out[index] = assetID
	}
	return out
}

func reserveIndexEqual(a, b map[int32]string) bool {
	if len(a) != len(b) {
		return false
	}
	for index, assetID := range a {
		if other, ok := b[index]; !ok || other != assetID {
			return false
		}
	}
	return true
}
