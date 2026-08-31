// Blend contract_data -> typed LedgerState decode lives here, in the protocol
// adapter rather than the relay core. Keeping decode in the adapter is what
// makes the protocol self-contained: event decode, state decode, and transform
// all live in one package.
//
// DecodeState is a stateless PURE reducer — (prior, changes, ledgerSeq) -> next.
// The Adapter retains no per-ledger scratch; every carry-over threads through
// *bindings.LedgerState (PendingUserPositions carries the one piece of builder
// state that does not otherwise round-trip). Because it keeps no hidden state,
// folding the same input twice yields byte-identical output, and it cannot leak
// map-iteration order or wall-clock reads across ledgers.
package blend

import (
	"encoding/json"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/lightgatehq/lidapters/blend/contracts"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// DecodeState folds Blend contract_data changes into typed ledger state. It is
// a pure reducer: it rebuilds a fresh in-memory mirror from prior, applies
// changes, and returns the freshly built LedgerState. No DB / network / clock /
// random / map-order; deterministic and run-twice byte-identical.
func (a *Adapter) DecodeState(prior *bindings.LedgerState, changes []bindings.ContractDataChange, ledgerSeq int64) (*bindings.LedgerState, error) {
	return a.DecodeStateAt(prior, changes, ledgerSeq, time.Time{})
}

// DecodeStateAt is DecodeState with the folding ledger's close time threaded
// in. The close time comes from the same close-meta as the changes, so it is
// fold input, not a clock — purity holds. It gates the oracle-aggregators'
// MaxAge staleness window (state_reflector.go); a zero closeTime (the plain
// DecodeState path) falls back to each feed's newest round timestamp as the
// reference "now", which prices the freshest round but cannot observe a feed
// that stopped publishing.
//
// The fold itself is delegated to the strategy selected at New
// (Config.StateMode): paranoid runs the reference reducer below verbatim;
// incremental produces byte-identical output from a carried mirror. See
// state_strategy.go for the two-mode contract.
func (a *Adapter) DecodeStateAt(prior *bindings.LedgerState, changes []bindings.ContractDataChange, ledgerSeq int64, closeTime time.Time) (*bindings.LedgerState, error) {
	next, _, dirty, dirtyBackstops, diagnostics, temporary := a.state.decodeState(prior, changes, ledgerSeq, closeTime)
	a.lastDirty = dirty
	a.lastDirtyBackstops = dirtyBackstops
	a.lastDiagnostics = diagnostics
	a.lastTemporaryChanges = temporary
	return next, nil
}

// OwnsContract reports whether contractID belongs to Blend. Ownership is the
// runtime-discovered pool/backstop/oracle set fed in via RegisterContracts, plus
// the registered token-contract set fed in via RegisterAssetContracts; both are
// config-like (not per-ledger scratch), so neither breaks DecodeState purity.
func (a *Adapter) OwnsContract(contractID string) bool {
	if contractID == "" {
		return false
	}
	if _, ok := a.contracts[contractID]; ok {
		return true
	}
	if _, ok := a.feeds[contractID]; ok {
		return true
	}
	if _, ok := a.comets[contractID]; ok {
		return true
	}
	_, ok := a.assets[contractID]
	return ok
}

// RegisterContracts adds discovered contract IDs to the owned set so
// OwnsContract returns true for them. Idempotent; ignores blank IDs. Called by
// the relay's projector edge as it discovers pools (it is NOT called from the
// pure DecodeState path).
func (a *Adapter) RegisterContracts(ids ...string) {
	if a.contracts == nil {
		a.contracts = map[string]struct{}{}
	}
	for _, id := range ids {
		if id != "" {
			a.contracts[id] = struct{}{}
		}
	}
}

// RegisterAssetContracts adds token-contract IDs (a pool's reserve assets) to
// the registered asset set. Idempotent; ignores blank IDs. Called by the
// relay's projector edge as a pool's reserve list reveals its reserve assets
// (it is NOT called from the pure DecodeState path). A registered asset's
// contract_data is folded on the SAC/SEP-41 decode path in apply(), ahead of
// and instead of the generic pool-instance branch — see the assets field
// comment on Adapter.
func (a *Adapter) RegisterAssetContracts(ids ...string) {
	if a.assets == nil {
		a.assets = map[string]struct{}{}
	}
	for _, id := range ids {
		if id != "" {
			a.assets[id] = struct{}{}
		}
	}
}

// RegisterPriceFeeds adds Reflector price-feed contract IDs (the feeds backing
// the pools' oracle-aggregators) to the registered feed set. Idempotent;
// ignores blank IDs. Called by the relay's projector edge from static config
// (it is NOT called from the pure DecodeState path): feeds must be owned from
// the first folded ledger, or the projector filters their round writes out
// before decode and no aggregator can ever resolve a price.
func (a *Adapter) RegisterPriceFeeds(ids ...string) {
	if a.feeds == nil {
		a.feeds = map[string]struct{}{}
	}
	for _, id := range ids {
		if id != "" {
			a.feeds[id] = struct{}{}
		}
	}
}

// RegisterCometContracts adds Comet (BToken) LP contract IDs to the registered
// Comet set. Idempotent; ignores blank IDs. Called by the relay's projector
// edge from static config (it is NOT called from the pure DecodeState path): a
// Comet must be owned from the first folded ledger, or the projector filters
// its reserve/supply writes out before decode and the backstop's LP valuation
// inputs can never fold. The set is deliberately distinct from RegisterContracts:
// Comet instance/persistent data routes only to the Comet reducer
// (state_comet.go), never to the Blend pool decoder (D-03).
func (a *Adapter) RegisterCometContracts(ids ...string) {
	if a.comets == nil {
		a.comets = map[string]struct{}{}
	}
	for _, id := range ids {
		if id != "" {
			a.comets[id] = struct{}{}
		}
	}
}

type typedStateDelta struct {
	LedgerSeq  int64
	EntityType string
	EntityKey  string
	Live       bool
	StateJSON  json.RawMessage
}

type blendStateBuilder struct {
	pools         map[string]*poolBuilder
	pendingPos    map[string]pendingUserPositions
	backstopPools map[string]backstopPoolBalance
	backstopUsers map[string]backstopUserBalance
	// backstopEmis holds each pool's BEmisData entry from the backstop contract,
	// keyed by pool contract ID. Kept beside backstopPools (not merged into it)
	// so a PoolBalance write can never clobber emission fields and vice versa;
	// merged onto PoolState at build time and reconstructed from it on reload,
	// the same round-trip rule as backstopPools.
	backstopEmis map[string]backstopEmisData
	// auctions holds each live Auction(AuctionKey) entry, keyed by
	// typedAuctionEntityKey(pool, user, auctType). Temporary storage on-chain:
	// any not-live change removes the entry.
	auctions map[string]contracts.AuctionState
	// userEmis holds each UserEmis(UserReserveKey) entry, keyed by
	// typedUserEmisEntityKey(user, pool, resTokenID).
	userEmis map[string]contracts.UserEmissionState
	// queuedReserves holds each pending ResInit(Address) entry, keyed by
	// typedReserveEntityKey(pool, asset). Deliberately NOT folded into
	// pool.reserves: a queue can target a brand-new asset, and folding it in
	// would fabricate a phantom live reserve.
	queuedReserves map[string]contracts.QueuedReserveState
	// backstopInstances holds each backstop contract's decoded identity
	// (instance addresses + RZ/DropList), keyed by contract ID.
	backstopInstances map[string]*contracts.BackstopInstanceState
	// comets holds each registered Comet (BToken) contract's folded LP state,
	// keyed by contract ID. Populated only for contracts in ownedComets;
	// carried across ledgers via LedgerState.AMMPools (PoolType "comet").
	comets      map[string]*cometPoolBuilder
	oracles     map[string]*oracleBuilder
	feeds       map[string]*feedBuilder
	aggregators map[string]*aggregatorBuilder
	assets      map[string]contracts.AssetMetadata
	// owned is the adapter's owned-contract set, threaded in so the reducer can
	// tell an oracle's contract_data apart from a pool's. It is read-only config,
	// not per-ledger scratch, so it does not break the run-twice purity guarantee.
	owned map[string]struct{}
	// ownedAssets is the adapter's registered token-contract set, threaded in the
	// same read-only-config way as owned, so the reducer can route a registered
	// asset's contract_data onto the SAC/SEP-41 decode path ahead of the generic
	// pool-instance branch.
	ownedAssets map[string]struct{}
	// ownedFeeds is the adapter's registered Reflector price-feed set, threaded
	// in the same read-only-config way, so a feed's contract_data is always
	// routed onto the Reflector decode path (state_reflector.go) and never
	// mistaken for a pool or a mock oracle.
	ownedFeeds map[string]struct{}
	// ownedComets is the adapter's registered Comet (BToken) contract set,
	// threaded in the same read-only-config way, so a Comet's contract_data is
	// always routed onto the Comet decode path (state_comet.go) and never
	// mistaken for a Blend pool, oracle, or asset.
	ownedComets map[string]struct{}
	// protocol is the adapter's configured protocol ID, threaded onto emitted
	// AMMPoolState rows. Read-only config, like the owned sets.
	protocol string
	deltas   []typedStateDelta
	// ledgerSeq is the ledger currently being folded, threaded onto the builder
	// so the position-skip diagnostics emitted during build carry it. Set once
	// per decode, before apply runs.
	ledgerSeq int64
	// diagnostics collects this fold's skipped-leg records (see
	// positionSkipSink). Paranoid starts from a fresh builder each ledger; the
	// incremental strategy resets it in normalizeCarry beside deltas. Both
	// strategies sort it with sortDecodeDiagnostics before returning it.
	diagnostics []bindings.DecodeDiagnostic
	// dirtyUsers collects the identity of every user-positions entry apply
	// touches (live write, archive, or explicit delete), keyed like pendingPos.
	// Always allocated (both strategies populate it identically): the
	// incremental strategy's snapshot also consults it to recompute exactly the
	// touched users' cached position blocks instead of rebuilding all of them,
	// and decodeBlendState/incrementalStrategy.decodeState both fold it (plus
	// the pool-reserve-remap union, see markPoolRemapDirty) into the
	// bindings.DirtyPosition set DecodeState/DecodeStateAt exposes via
	// Adapter.LastDirtyPositions.
	dirtyUsers map[string]userIdentity
	// dirtyTemporary collects the identity of every auction/queued-reserve
	// entry apply touches (live write or not-live removal), keyed by the
	// entity key (typedAuctionEntityKey / typedReserveEntityKey). It is the
	// per-ledger scratch behind Adapter.LastTemporaryStateChanges: like
	// dirtyUsers it is always allocated, the incremental strategy resets it in
	// normalizeCarry beside deltas/diagnostics, and both strategies expose the
	// same sorted transition set via finalizeTemporaryStateChanges. The
	// identity comes from the changed ledger key, so a removal is reportable
	// even when the fold never observed the entry's create.
	dirtyTemporary map[string]bindings.TemporaryStateChange
	// dirtyBackstops collects the identity of every backstop (address, pool)
	// pair whose valuation inputs this ledger's changes invalidated — the
	// holder's own balance/emission write, its pool's PoolBalance, a linked
	// Comet write, or a price change for its BLND/USDC legs. Per-ledger scratch
	// like dirtyUsers; finalized into the bindings.DirtyBackstop set exposed
	// via Adapter.LastDirtyBackstops (D-10).
	dirtyBackstops map[string]backstopIdentity
	// changedFeeds / changedPriceAssets are this ledger's price-invalidation
	// signals, recorded during apply and consumed by the backstop dirty
	// finalize: a feed round write (aggregator-synthesized prices move) and a
	// mock-oracle price write/delete for a resolvable asset.
	changedFeeds       map[string]struct{}
	changedPriceAssets map[string]struct{}
}

// backstopIdentity is the (address, pool) pair behind a backstopUsers
// composite key — the dirtyBackstops analog of userIdentity.
type backstopIdentity struct {
	address string
	pool    string
}

// userIdentity is the (address, pool) pair behind a pendingPos composite key —
// what the incremental strategy needs to maintain its sorted user order when
// an entry is deleted and the pendingPos value is no longer there to consult.
type userIdentity struct {
	address string
	pool    string
}

// oracleBuilder accumulates the parts of a Blend price oracle that a fold needs
// to resolve a reserve's USD price: the oracle's asset->index map and the raw
// per-index price, both decoded from the oracle's own contract_data. Price
// entries are keyed by the asset's index, so the index->asset map (which the
// oracle keeps in its instance storage) is what ties a stored price back to a
// pool reserve.
type oracleBuilder struct {
	decimals     int32
	assetToIndex map[string]int64
	priceByIndex map[int64]string
	// Instance facets beyond the asset list (base/res/admin) plus the oracle's
	// top-level `timestamp` freshness entry — see contracts.OracleState.
	baseKey          string
	resolutionRaw    string
	admin            string
	lastTimestampRaw string
	// synthesized marks an oracle whose map and prices were derived this ledger
	// from an oracle-aggregator's config plus registered feed rounds
	// (resolveAggregatorPrices) rather than decoded from the oracle's own
	// writes. Synthesized oracles are recomputed every ledger from the carried
	// aggregator + feed state, so they are excluded from the LedgerState.Oracles
	// carry — carrying them too would create a second source of truth.
	synthesized bool
}

type poolBuilder struct {
	state          contracts.PoolState
	reserves       map[string]*reserveBuilder
	reserveList    []string
	reserveByIndex map[int32]string
	// ambiguousByIndex holds each reserve index claimed by two or more
	// KNOWN-index reserves, mapped to the sorted claiming asset IDs. Such an
	// index is deliberately absent from reserveByIndex: resolving either way
	// would misattribute. positionsFromMap reports skipped legs against it as
	// duplicate_reserve_index diagnostics.
	ambiguousByIndex map[int32][]string
	// unknownIndexAssets is the sorted asset IDs of the pool's reserves whose
	// index is not known (ResData materialized the reserve; its ResConfig never
	// folded). They are the candidates reported on an unmapped_reserve_index
	// diagnostic: the skipped leg's true owner is one of them, but which one is
	// unknowable until its ResConfig arrives.
	unknownIndexAssets []string
}

type reserveBuilder struct {
	state contracts.ReserveState
}

type pendingUserPositions struct {
	poolContract string
	user         string
	positions    xdr.ScVal
	valueXDR     string
	// archived / archivedLedgerSeq mirror contracts.PendingUserPosition.Archived
	// / ArchivedLedgerSeq — see applyDelete's Positions case for how they're set
	// (TTL lapse / eviction) and apply's Positions case for how a fresh live
	// write clears them.
	archived          bool
	archivedLedgerSeq int64
}

type backstopPoolBalance struct {
	poolContract string
	sharesRaw    string
	tokensRaw    string
	q4wRaw       string
}

type backstopUserBalance struct {
	poolContract string
	user         string
	sharesRaw    string
	q4w          []contracts.Q4WEntry
	// emisIndexRaw / emisAccruedRaw mirror the backstop's UEmisData(pool, user)
	// entry — a SIBLING of the UserBalance entry, so either can exist without
	// the other and neither's write/delete may clobber the other's fields.
	emisIndexRaw   string
	emisAccruedRaw string
}

// backstopEmisData mirrors the backstop's BEmisData(pool) entry
// (BackstopEmissionData {expiration, eps, index, last_time}); every field is
// independently optional-on-chain and stays "" when absent.
type backstopEmisData struct {
	poolContract  string
	epsRaw        string
	expirationRaw string
	indexRaw      string
	lastTimeRaw   string
}

func (d backstopEmisData) empty() bool {
	return d.epsRaw == "" && d.expirationRaw == "" && d.indexRaw == "" && d.lastTimeRaw == ""
}

func newBlendStateBuilder() *blendStateBuilder {
	return &blendStateBuilder{
		pools:              map[string]*poolBuilder{},
		pendingPos:         map[string]pendingUserPositions{},
		backstopPools:      map[string]backstopPoolBalance{},
		backstopUsers:      map[string]backstopUserBalance{},
		backstopEmis:       map[string]backstopEmisData{},
		auctions:           map[string]contracts.AuctionState{},
		userEmis:           map[string]contracts.UserEmissionState{},
		queuedReserves:     map[string]contracts.QueuedReserveState{},
		backstopInstances:  map[string]*contracts.BackstopInstanceState{},
		comets:             map[string]*cometPoolBuilder{},
		oracles:            map[string]*oracleBuilder{},
		feeds:              map[string]*feedBuilder{},
		aggregators:        map[string]*aggregatorBuilder{},
		assets:             map[string]contracts.AssetMetadata{},
		dirtyUsers:         map[string]userIdentity{},
		dirtyTemporary:     map[string]bindings.TemporaryStateChange{},
		dirtyBackstops:     map[string]backstopIdentity{},
		changedFeeds:       map[string]struct{}{},
		changedPriceAssets: map[string]struct{}{},
	}
}

// decodeBlendState is the pure-reducer core shared by DecodeState (which returns
// only the LedgerState) and the in-package tests (which assert the sorted
// Deltas). It rebuilds the mirror from prior, folds changes, and returns the
// built state, the silver-debug deltas, the ledger's dirty-positions set (see
// markPoolRemapDirty and bindings.DirtyPosition), the affected-backstop set
// (see finalizeDirtyBackstops and bindings.DirtyBackstop), the ledger's
// skipped-leg diagnostics (see positionSkipSink and
// bindings.DecodeDiagnostic), and the ledger's auction/queued-reserve
// transition set (see finalizeTemporaryStateChanges and
// bindings.TemporaryStateChange).
func (a *Adapter) decodeBlendState(prior *bindings.LedgerState, changes []bindings.ContractDataChange, ledgerSeq int64, closeTime time.Time) (bindings.LedgerState, []typedStateDelta, []bindings.DirtyPosition, []bindings.DirtyBackstop, []bindings.DecodeDiagnostic, []bindings.TemporaryStateChange) {
	b := newBlendStateBuilder()
	b.owned = a.contracts
	b.ownedAssets = a.assets
	b.ownedFeeds = a.feeds
	b.ownedComets = a.comets
	b.protocol = a.cfg.Protocol
	b.ledgerSeq = ledgerSeq
	if prior != nil {
		b.loadPrior(prior)
	}
	for _, change := range changes {
		b.apply(change, ledgerSeq)
	}
	// A reserve-index remap (or a pool appearing/disappearing) changes what
	// every one of its pending users resolves to, even though their own
	// Positions entry did not change this ledger — union them into the dirty
	// set exactly as the incremental strategy's cache invalidation already
	// must (state_incremental.go's snapshot), so the two strategies expose an
	// identical dirty set regardless of which one is active.
	markPoolRemapDirty(b.dirtyUsers, reserveIndexSnapshot(prior), b)
	dirty := finalizeDirtyPositions(b.dirtyUsers, b.pendingPos)
	// Feed writes recorded during apply move aggregator-synthesized prices;
	// route the assets they serve through the backstop-invalidation join before
	// finalizing the affected-holder set.
	b.propagateFeedPriceDirty()
	for assetID := range b.changedPriceAssets {
		b.markPriceAssetDirty(assetID)
	}
	dirtyBackstops := finalizeDirtyBackstops(b.dirtyBackstops, b.backstopUsers)
	temporary := finalizeTemporaryStateChanges(b.dirtyTemporary, b.auctions, b.queuedReserves)

	// The deltas are appended from map-range iteration over the builder maps
	// (appendPoolReserves / appendPoolUsers / appendBackstopUsersForPool), and Go
	// map order is randomized, so two runs over the same ledgers would otherwise
	// emit them in different orders. Sort by a stable total-order key before emit
	// so the output is byte-identical run to run. The skipped-leg diagnostics
	// collected during build are sorted for the same reason.
	next := b.build(closeTime)
	sortTypedStateDeltas(b.deltas)
	sortDecodeDiagnostics(b.diagnostics)

	return next, b.deltas, dirty, dirtyBackstops, b.diagnostics, temporary
}

// reserveIndexSnapshot derives each pool's reserveByIndex mapping (known,
// unique ReserveIndex -> AssetID) as of a LedgerState, the same shape
// loadPrior would reconstruct. nil (no prior) yields an empty snapshot: every
// pool the fold produces this ledger is then "new", which is correct — a
// first-ledger pending entry can only exist if it was also created this
// ledger, and that is already in dirtyUsers directly.
func reserveIndexSnapshot(state *bindings.LedgerState) map[string]map[int32]string {
	if state == nil {
		return nil
	}
	out := make(map[string]map[int32]string, len(state.Pools))
	for _, pool := range state.Pools {
		out[pool.ContractID] = publishedReserveIndexes(pool.Reserves)
	}
	return out
}

// markPoolRemapDirty unions into dirty every pendingPos entry belonging to a
// pool whose reserveByIndex mapping changed this ledger — including a pool
// that appeared or disappeared — between priorIndexes and the builder's
// current pools. A remap invalidates every such user's resolved position
// exactly as the incremental strategy's own cache does (state_incremental.go
// tracks the identical comparison via its persistent s.poolIndexes).
func markPoolRemapDirty(dirty map[string]userIdentity, priorIndexes map[string]map[int32]string, b *blendStateBuilder) {
	if dirty == nil {
		return
	}
	changed := map[string]struct{}{}
	seen := make(map[string]struct{}, len(b.pools))
	for id, pool := range b.pools {
		seen[id] = struct{}{}
		previous, tracked := priorIndexes[id]
		if !tracked || !reserveIndexEqual(previous, pool.reserveByIndex) {
			changed[id] = struct{}{}
		}
	}
	for id := range priorIndexes {
		if _, ok := seen[id]; !ok {
			changed[id] = struct{}{}
		}
	}
	if len(changed) == 0 {
		return
	}
	for composite, pending := range b.pendingPos {
		if _, ok := changed[pending.poolContract]; ok {
			dirty[composite] = userIdentity{address: pending.user, pool: pending.poolContract}
		}
	}
}

// finalizeDirtyPositions turns the builder's raw dirty set into the exposed
// bindings.DirtyPosition list, sorted by (address, pool) for byte-stable
// output. Kind is derived post-hoc from whether the entry still has positions
// after the fold (pendingPos presence) rather than tracked incrementally: a
// key present in pendingPos is an Upsert (still has positions — including an
// archived one, see Change 1), a key absent is a Removal (a genuine on-chain
// delete purged it — TTL lapse/eviction never removes the pendingPos entry).
func finalizeDirtyPositions(dirty map[string]userIdentity, pendingPos map[string]pendingUserPositions) []bindings.DirtyPosition {
	if len(dirty) == 0 {
		return nil
	}
	out := make([]bindings.DirtyPosition, 0, len(dirty))
	for composite, identity := range dirty {
		kind := bindings.DirtyRemoval
		if _, ok := pendingPos[composite]; ok {
			kind = bindings.DirtyUpsert
		}
		out = append(out, bindings.DirtyPosition{Address: identity.address, PoolContractID: identity.pool, Kind: kind})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Address != out[j].Address {
			return out[i].Address < out[j].Address
		}
		return out[i].PoolContractID < out[j].PoolContractID
	})
	return out
}

// finalizeTemporaryStateChanges turns the builder's raw temporary-state dirty
// set into the exposed bindings.TemporaryStateChange list, sorted by (kind,
// pool, user, auction type, asset) for byte-stable output. Action is derived
// post-hoc from whether the identity is still live after the fold (auctions /
// queuedReserves presence) rather than tracked incrementally — the same rule
// finalizeDirtyPositions uses, so a create-then-remove inside one ledger
// reports the final outcome (removal) and a remove-then-restore reports the
// upsert. A removal's identity comes from the dirty record itself, never from
// a prior state slice: a bounded replay that first observes an entry at its
// removal still reports the transition.
func finalizeTemporaryStateChanges(dirty map[string]bindings.TemporaryStateChange, auctions map[string]contracts.AuctionState, queuedReserves map[string]contracts.QueuedReserveState) []bindings.TemporaryStateChange {
	if len(dirty) == 0 {
		return nil
	}
	out := make([]bindings.TemporaryStateChange, 0, len(dirty))
	for entityKey, change := range dirty {
		action := bindings.DirtyRemoval
		switch change.Kind {
		case bindings.TemporaryAuction:
			if _, ok := auctions[entityKey]; ok {
				action = bindings.DirtyUpsert
			}
		case bindings.TemporaryQueuedReserve:
			if _, ok := queuedReserves[entityKey]; ok {
				action = bindings.DirtyUpsert
			}
		}
		change.Action = action
		out = append(out, change)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].PoolContractID != out[j].PoolContractID {
			return out[i].PoolContractID < out[j].PoolContractID
		}
		if out[i].UserAddress != out[j].UserAddress {
			return out[i].UserAddress < out[j].UserAddress
		}
		if out[i].AuctionType != out[j].AuctionType {
			return out[i].AuctionType < out[j].AuctionType
		}
		return out[i].AssetID < out[j].AssetID
	})
	return out
}

// sortTypedStateDeltas sorts the per-ledger silver-debug deltas by their stable
// total-order key. Shared by both fold strategies so their delta streams stay
// comparable entry for entry.
func sortTypedStateDeltas(deltas []typedStateDelta) {
	sort.SliceStable(deltas, func(i, j int) bool {
		di, dj := deltas[i], deltas[j]
		if di.EntityType != dj.EntityType {
			return di.EntityType < dj.EntityType
		}
		if di.EntityKey != dj.EntityKey {
			return di.EntityKey < dj.EntityKey
		}
		if di.LedgerSeq != dj.LedgerSeq {
			return di.LedgerSeq < dj.LedgerSeq
		}
		if di.Live != dj.Live {
			return !di.Live && dj.Live
		}
		return string(di.StateJSON) < string(dj.StateJSON)
	})
}

// loadPrior reconstructs the mirror from the prior LedgerState so the reducer
// keeps no state of its own between ledgers. Pools/reserves (including each
// pool's backstop pool-level balance) come from prior.Pools, per-user backstop
// balances from prior.Backstops, and raw user-position blobs from
// prior.PendingUserPositions.
func (b *blendStateBuilder) loadPrior(prior *bindings.LedgerState) {
	for _, pool := range prior.Pools {
		pb := ensurePool(b.pools, pool.ContractID)
		pb.state = pool
		for _, reserve := range pool.Reserves {
			pb.reserves[reserve.AssetID] = &reserveBuilder{state: reserve}
		}
		finalizePoolReserves(pb)
		// Restored from the pool itself (not from prior.Backstops) so a pool's
		// backstop total round-trips even in a ledger where it currently has zero
		// individual backstop depositors.
		if pool.BackstopSharesRaw != "" || pool.BackstopTokensRaw != "" || pool.BackstopQ4WSharesRaw != "" {
			b.backstopPools[pool.ContractID] = backstopPoolBalance{
				poolContract: pool.ContractID,
				sharesRaw:    pool.BackstopSharesRaw,
				tokensRaw:    pool.BackstopTokensRaw,
				q4wRaw:       pool.BackstopQ4WSharesRaw,
			}
		}
		// Backstop pool-level emission accrual round-trips on the pool the same
		// way the balance does — restored whenever any field is present.
		emis := backstopEmisData{
			poolContract:  pool.ContractID,
			epsRaw:        pool.BackstopEmisEPSRaw,
			expirationRaw: pool.BackstopEmisExpirationRaw,
			indexRaw:      pool.BackstopEmisIndexRaw,
			lastTimeRaw:   pool.BackstopEmisLastTimeRaw,
		}
		if !emis.empty() {
			b.backstopEmis[pool.ContractID] = emis
		}
	}
	for _, auction := range prior.Auctions {
		// A live auction only appears in the ledger it changes; restore it so a
		// later ledger still knows about it. Removed on the not-live change.
		b.auctions[typedAuctionEntityKey(auction.PoolContractID, auction.UserAddress, auction.AuctionType)] = auction
	}
	for _, emission := range prior.UserEmissions {
		// Same carry requirement as auctions: the entry is only written when the
		// user's accrual checkpoints.
		b.userEmis[typedUserEmisEntityKey(emission.Address, emission.PoolContractID, emission.ReserveTokenID)] = emission
	}
	for _, pending := range prior.PendingUserPositions {
		value, ok := decodeScValBase64(pending.PositionsXDR)
		if !ok {
			continue
		}
		b.pendingPos[typedUserEntityKey(pending.Address, pending.PoolContractID)] = pendingUserPositions{
			poolContract:      pending.PoolContractID,
			user:              pending.Address,
			positions:         value,
			valueXDR:          pending.PositionsXDR,
			archived:          pending.Archived,
			archivedLedgerSeq: pending.ArchivedLedgerSeq,
		}
	}
	for _, backstop := range prior.Backstops {
		b.backstopUsers[typedBackstopEntityKey(backstop.Address, backstop.PoolContractID)] = backstopUserBalance{
			poolContract:   backstop.PoolContractID,
			user:           backstop.Address,
			sharesRaw:      backstop.UserSharesRaw,
			q4w:            backstop.Q4W,
			emisIndexRaw:   backstop.EmisIndexRaw,
			emisAccruedRaw: backstop.UnclaimedEmissionsRaw,
		}
	}
	for _, oracle := range prior.Oracles {
		// The oracle instance is written at deploy, and a price entry only appears
		// in the ledger it changes, so the asset->index map and the still-live
		// prices must be restored here for a price-only ledger to resolve anything.
		ob := b.ensureOracle(oracle.ContractID)
		ob.decimals = oracle.Decimals
		ob.baseKey = oracle.BaseKey
		ob.resolutionRaw = oracle.ResolutionRaw
		ob.admin = oracle.Admin
		ob.lastTimestampRaw = oracle.LastTimestampRaw
		for _, asset := range oracle.Assets {
			ob.assetToIndex[asset.AssetID] = asset.Index
		}
		for _, price := range oracle.Prices {
			ob.priceByIndex[price.Index] = price.PriceRaw
		}
	}
	for _, queued := range prior.QueuedReserves {
		// A queued reserve init is only written when queued/cancelled/executed,
		// so it must be restored to survive to the next ledger.
		b.queuedReserves[typedReserveEntityKey(queued.PoolContractID, queued.AssetID)] = queued
	}
	for _, instance := range prior.BackstopInstances {
		// The backstop instance is written at deploy and rarely re-emitted —
		// same carry requirement as the oracle instance.
		restored := instance
		b.backstopInstances[instance.ContractID] = &restored
	}
	for _, asset := range prior.Assets {
		// A token's AssetInfo/METADATA instance is written once at deploy and never
		// re-emitted, so it must be restored here for it to survive past the ledger
		// it was decoded on.
		b.assets[asset.ContractID] = asset
	}
	// A feed round only appears in the ledger it was published, and an
	// aggregator's instance config is assembled once and rarely touched — both
	// must be restored for any later ledger to synthesize prices (the Reflector
	// analog of the Oracles carry above).
	for _, feed := range prior.PriceFeeds {
		b.feeds[feed.ContractID] = feedBuilderFromState(feed)
	}
	for _, agg := range prior.OracleAggregators {
		b.aggregators[agg.ContractID] = aggregatorBuilderFromState(agg)
	}
	for _, pool := range prior.AMMPools {
		// Comet facets are written once at deploy (AllTokenVec) and only in the
		// ledger they change (AllRecordData, TotalShares) — same carry
		// requirement as the oracle instance. Only registered Comet contracts
		// are restored: anything else on AMMPools belongs to another adapter.
		if _, ok := b.ownedComets[pool.ContractID]; ok {
			b.restoreComet(pool)
		}
	}
}

// build assembles the typed LedgerState from the mirror, sorting every slice so
// the output is byte-identical when the same input is folded twice.
func (b *blendStateBuilder) build(closeTime time.Time) bindings.LedgerState {
	// Synthesize each oracle-aggregator's per-asset prices from the carried
	// aggregator config + registered feed rounds FIRST, so the existing
	// resolveOraclePrices pass below sees them exactly as if the oracle had
	// written per-index prices itself (the testnet-mock representation).
	b.resolveAggregatorPrices(closeTime)
	// Thread decoded oracle prices onto their reserves before the slices are
	// finalized and sorted, so the price rides on the already-deterministic
	// reserve ordering (finalizePoolReserves + sortLedgerState) and the run-twice
	// output stays byte-identical.
	b.resolveOraclePrices()

	pools := make([]contracts.PoolState, 0, len(b.pools))
	users := make([]contracts.UserReservePosition, 0)
	pending := make([]contracts.PendingUserPosition, 0, len(b.pendingPos))
	backstops := make([]contracts.BackstopPosition, 0, len(b.backstopUsers))

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
	for composite, p := range b.pendingPos {
		// Raw blob round-trips regardless of whether the pool is known yet, so a
		// position decoded before its pool appears is not lost.
		pending = append(pending, pendingOut(p))
		pool := b.pools[p.poolContract]
		if pool == nil {
			continue
		}
		finalizePoolReserves(pool)
		// Skipped-leg diagnostics are recorded only for the (address, pool)
		// pairs this ledger's fold actually touched (the dirty set, including
		// the pool-remap union). That keeps the records scoped to the fold —
		// and identical across the two strategies, whose per-ledger position
		// recomputation covers exactly the dirty pairs.
		var sink *positionSkipSink
		if _, touched := b.dirtyUsers[composite]; touched {
			sink = &positionSkipSink{ledgerSeq: b.ledgerSeq, out: &b.diagnostics}
		}
		users = append(users, buildUserPositionsForPending(pool, p, sink)...)
	}
	for _, userBalance := range b.backstopUsers {
		backstops = append(backstops, b.backstopPosition(userBalance))
	}

	sortLedgerState(pools, users, backstops)
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].Address != pending[j].Address {
			return pending[i].Address < pending[j].Address
		}
		return pending[i].PoolContractID < pending[j].PoolContractID
	})

	return bindings.LedgerState{
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
	}
}

// buildQueuedReserves snapshots the carried ResInit entries, sorted by
// (pool, asset) so the run-twice output stays byte-identical.
func (b *blendStateBuilder) buildQueuedReserves() []contracts.QueuedReserveState {
	if len(b.queuedReserves) == 0 {
		return nil
	}
	queued := make([]contracts.QueuedReserveState, 0, len(b.queuedReserves))
	for _, entry := range b.queuedReserves {
		queued = append(queued, entry)
	}
	sort.Slice(queued, func(i, j int) bool {
		if queued[i].PoolContractID != queued[j].PoolContractID {
			return queued[i].PoolContractID < queued[j].PoolContractID
		}
		return queued[i].AssetID < queued[j].AssetID
	})
	return queued
}

// buildBackstopInstances snapshots the carried backstop identities, sorted by
// contract ID so the run-twice output stays byte-identical.
func (b *blendStateBuilder) buildBackstopInstances() []contracts.BackstopInstanceState {
	if len(b.backstopInstances) == 0 {
		return nil
	}
	instances := make([]contracts.BackstopInstanceState, 0, len(b.backstopInstances))
	for _, instance := range b.backstopInstances {
		instances = append(instances, *instance)
	}
	sort.Slice(instances, func(i, j int) bool { return instances[i].ContractID < instances[j].ContractID })
	return instances
}

// mergeBackstopEmis threads a pool's carried BEmisData onto its PoolState —
// the emission twin of the backstopPools merge above it in build/snapshot.
// When no entry is carried the fields are cleared, so a deleted BEmisData
// entry cannot linger on the pool from a prior ledger's merge.
func mergeBackstopEmis(pool *poolBuilder, backstopEmis map[string]backstopEmisData) {
	emis := backstopEmis[pool.state.ContractID]
	pool.state.BackstopEmisEPSRaw = emis.epsRaw
	pool.state.BackstopEmisExpirationRaw = emis.expirationRaw
	pool.state.BackstopEmisIndexRaw = emis.indexRaw
	pool.state.BackstopEmisLastTimeRaw = emis.lastTimeRaw
}

// buildAuctions snapshots the carried auction entries, sorted by
// (pool, user, type) so the run-twice output stays byte-identical.
func (b *blendStateBuilder) buildAuctions() []contracts.AuctionState {
	if len(b.auctions) == 0 {
		return nil
	}
	auctions := make([]contracts.AuctionState, 0, len(b.auctions))
	for _, auction := range b.auctions {
		auctions = append(auctions, auction)
	}
	sort.Slice(auctions, func(i, j int) bool {
		if auctions[i].PoolContractID != auctions[j].PoolContractID {
			return auctions[i].PoolContractID < auctions[j].PoolContractID
		}
		if auctions[i].UserAddress != auctions[j].UserAddress {
			return auctions[i].UserAddress < auctions[j].UserAddress
		}
		return auctions[i].AuctionType < auctions[j].AuctionType
	})
	return auctions
}

// buildUserEmissions snapshots the carried per-user emission entries, sorted
// by (address, pool, reserve token) so the run-twice output stays
// byte-identical.
func (b *blendStateBuilder) buildUserEmissions() []contracts.UserEmissionState {
	if len(b.userEmis) == 0 {
		return nil
	}
	emissions := make([]contracts.UserEmissionState, 0, len(b.userEmis))
	for _, emission := range b.userEmis {
		emissions = append(emissions, emission)
	}
	sort.Slice(emissions, func(i, j int) bool {
		if emissions[i].Address != emissions[j].Address {
			return emissions[i].Address < emissions[j].Address
		}
		if emissions[i].PoolContractID != emissions[j].PoolContractID {
			return emissions[i].PoolContractID < emissions[j].PoolContractID
		}
		return emissions[i].ReserveTokenID < emissions[j].ReserveTokenID
	})
	return emissions
}

// buildAssets snapshots each registered asset contract's carried decode state
// (symbol, name, decimals) into the returned LedgerState so the next ledger's
// loadPrior can restore it — the instance entry is written once at deploy and
// never re-emitted, the same carry requirement as Oracles. Sorted by contract
// ID so the run-twice output stays byte-identical.
func (b *blendStateBuilder) buildAssets() []contracts.AssetMetadata {
	if len(b.assets) == 0 {
		return nil
	}
	assets := make([]contracts.AssetMetadata, 0, len(b.assets))
	for _, meta := range b.assets {
		assets = append(assets, meta)
	}
	sort.Slice(assets, func(i, j int) bool { return assets[i].ContractID < assets[j].ContractID })
	return assets
}

// buildOracles snapshots each oracle's carried decode state (decimals,
// asset->index map, per-index prices) into the returned LedgerState so the next
// ledger's loadPrior can restore it. Every slice is sorted by a stable key so
// the run-twice output stays byte-identical.
func (b *blendStateBuilder) buildOracles() []contracts.OracleState {
	if len(b.oracles) == 0 {
		return nil
	}
	oracles := make([]contracts.OracleState, 0, len(b.oracles))
	for contractID, oracle := range b.oracles {
		if oracle.synthesized {
			// Derived every ledger from the carried aggregator + feed state —
			// carrying the derived copy too would be a second source of truth.
			continue
		}
		assets := make([]contracts.OracleAssetIndex, 0, len(oracle.assetToIndex))
		for assetID, index := range oracle.assetToIndex {
			assets = append(assets, contracts.OracleAssetIndex{AssetID: assetID, Index: index})
		}
		sort.Slice(assets, func(i, j int) bool {
			if assets[i].Index != assets[j].Index {
				return assets[i].Index < assets[j].Index
			}
			return assets[i].AssetID < assets[j].AssetID
		})
		prices := make([]contracts.OracleIndexPrice, 0, len(oracle.priceByIndex))
		for index, priceRaw := range oracle.priceByIndex {
			prices = append(prices, contracts.OracleIndexPrice{Index: index, PriceRaw: priceRaw})
		}
		sort.Slice(prices, func(i, j int) bool { return prices[i].Index < prices[j].Index })
		oracles = append(oracles, contracts.OracleState{
			ContractID:       contractID,
			Decimals:         oracle.decimals,
			Assets:           assets,
			Prices:           prices,
			BaseKey:          oracle.baseKey,
			ResolutionRaw:    oracle.resolutionRaw,
			Admin:            oracle.admin,
			LastTimestampRaw: oracle.lastTimestampRaw,
		})
	}
	if len(oracles) == 0 {
		// All builders were synthesized — keep the no-oracles shape identical to
		// the no-builders shape so run-twice output stays byte-identical.
		return nil
	}
	sort.Slice(oracles, func(i, j int) bool { return oracles[i].ContractID < oracles[j].ContractID })
	return oracles
}

func (b *blendStateBuilder) apply(change bindings.ContractDataChange, ledgerSeq int64) {
	key, ok := decodeScValBase64(change.KeyXDR)
	if !ok {
		return
	}

	// An entry is live only if the change says so AND its TTL has not lapsed.
	// Live=false covers eviction (the relay extract sets it from the close meta's
	// evicted-key set, which is reported separately from the change stream);
	// LiveUntilLedgerSeq < ledgerSeq covers TTL expiry. Either makes the entry
	// not-live, so we apply it as a delete — otherwise evicted or expired state
	// would read as live forever.
	live := change.Live && change.ValueXDR != nil
	if change.LiveUntilLedgerSeq != nil && int64(*change.LiveUntilLedgerSeq) < ledgerSeq {
		live = false
	}
	if !live {
		b.applyDelete(change, key, ledgerSeq)
		return
	}

	value, ok := decodeScValBase64(*change.ValueXDR)
	if !ok {
		return
	}

	// A registered Reflector price feed's contract_data is always decoded on the
	// Reflector path — per-round price entries (protocol 1: u128(ts<<64|index)
	// keys; protocol 2: u64(ts) batch keys) and the feed instance (asset list,
	// decimals, last_timestamp, round cache). Routed by the registered set, ahead
	// of everything: a feed instance carries a wasm executable and would
	// otherwise be misdecoded as a phantom pool by the wasm-hash sniff below.
	if _, isFeed := b.ownedFeeds[change.ContractID]; isFeed {
		b.applyFeedChange(change.ContractID, key, value)
		b.changedFeeds[change.ContractID] = struct{}{}
		return
	}

	// A registered Comet (BToken) contract's contract_data is always decoded on
	// the Comet path (state_comet.go), ahead of every Blend branch below: its
	// instance carries a wasm executable and would otherwise be misdecoded as a
	// phantom pool, and its persistent DataKey enum variants must never reach
	// the Blend pool key switch.
	if _, isComet := b.ownedComets[change.ContractID]; isComet {
		b.applyCometChange(change, key, value, ledgerSeq)
		return
	}

	// A registered price oracle stores two kinds of entries we care about: its
	// contract instance (whose storage carries the ordered asset list and the
	// shared price decimals) and one temporary entry per asset holding the raw
	// price, keyed by the asset's index. Decode those here, ahead of the generic
	// contract-instance handling, so an oracle is never mistaken for a pool.
	// A mainnet oracle-aggregator's instance (Base/Decimals/MaxAge + the
	// asset->feed map) is recognized the same way — its storage shape is
	// distinct from both the mock oracle's and a pool's, but like them it
	// carries a wasm executable and must not fall through to the pool sniff.
	if _, owned := b.owned[change.ContractID]; owned {
		if b.applyOracleInstance(change.ContractID, value) {
			return
		}
		if b.applyAggregatorInstance(change.ContractID, value) {
			return
		}
		if isOraclePriceKey(key) {
			b.applyOraclePrice(change.ContractID, key, value)
			return
		}
	}

	// A registered token contract's contract_data is always decoded on the
	// SAC/SEP-41 asset path, and never falls through to the generic pool-instance
	// branch below — even on a decode miss or an unrecognized key/value shape (a
	// balance/allowance entry, an unsupported layout, ...). A SAC's instance
	// carries no wasm executable and would fall through harmlessly, but a
	// wasm-backed SEP-41 token's instance DOES carry one and would otherwise be
	// misdecoded as a phantom pool by the wasm-hash sniff below.
	if _, isAsset := b.ownedAssets[change.ContractID]; isAsset {
		b.applyAssetInstance(change.ContractID, value)
		return
	}

	// The backstop contract's instance (BToken/Emitter/PoolFact/... symbol
	// keys) is recognized ahead of the generic pool-instance sniff below, the
	// same way an oracle's or aggregator's instance is — it carries a wasm
	// executable and would otherwise be misdecoded as a phantom pool.
	if b.applyBackstopInstance(change.ContractID, value) {
		b.addDelta(ledgerSeq, "backstop_instance", change.ContractID, true, b.backstopInstances[change.ContractID])
		return
	}

	if wasmHash, ok := contractInstanceWasmHash(value); ok {
		pool := ensurePool(b.pools, change.ContractID)
		pool.state.WasmHash = wasmHash
		if instance, ok := value.GetInstance(); ok {
			applyPoolInstanceStorage(pool, instance)
		}
		b.addDelta(ledgerSeq, "pool", change.ContractID, true, pool.state)
		return
	}

	if sym, ok := scSymbol(key); ok {
		// Keys that must not fabricate a phantom pool entry: the backstop's
		// top-level RZ/DropList lists and the mock oracle's `timestamp`
		// freshness entry live on contracts that are not pools, so they are
		// absorbed before ensurePool. No Blend pool has these symbol keys.
		switch sym {
		case "RZ":
			if zone, ok := decodeAddressList(value); ok {
				b.ensureBackstopInstance(change.ContractID).RewardZone = zone
				b.addDelta(ledgerSeq, "backstop_instance", change.ContractID, true, b.backstopInstances[change.ContractID])
			}
			return
		case "DropList":
			if list, ok := decodeDropList(value); ok {
				b.ensureBackstopInstance(change.ContractID).DropList = list
				b.addDelta(ledgerSeq, "backstop_instance", change.ContractID, true, b.backstopInstances[change.ContractID])
			}
			return
		case "timestamp":
			// Only meaningful on a contract already decoded as an oracle — a
			// bare timestamp write for an unknown contract sets nothing (and,
			// deliberately, fabricates no oracle either).
			if oracle := b.oracles[change.ContractID]; oracle != nil {
				if ts, ok := scIntString(value); ok {
					oracle.lastTimestampRaw = ts
				}
			}
			return
		}
		pool := ensurePool(b.pools, change.ContractID)
		switch sym {
		case "Config":
			if !isPoolConfig(value) {
				return
			}
			applyPoolConfig(pool, value)
			b.addDelta(ledgerSeq, "pool", change.ContractID, true, pool.state)
		case "Backstop":
			if address, ok := scAddress(value); ok {
				pool.state.BackstopContract = address
				b.addDelta(ledgerSeq, "pool", change.ContractID, true, pool.state)
			}
		case "ResList":
			pool.reserveList = nil
			applyReserveList(pool, value)
			finalizePoolReserves(pool)
			b.addDelta(ledgerSeq, "pool", change.ContractID, true, pool.state)
			b.appendPoolReserves(change.ContractID, ledgerSeq)
			b.appendPoolUsers(change.ContractID, ledgerSeq)
		case "PoolEmis":
			// v2 PoolEmis is Map<u32 res_token_id, u64 share>. A malformed
			// value (v1 PoolEmissionConfig or garbage) decodes to nothing and
			// keeps the carried split rather than wiping it.
			if entries, ok := decodePoolEmissions(value); ok {
				pool.state.PoolEmissions = entries
				b.addDelta(ledgerSeq, "pool", change.ContractID, true, pool.state)
			}
		}
		return
	}

	variant, args, ok := scVariant(key)
	if !ok {
		return
	}
	switch variant {
	case "ResConfig":
		asset, ok := variantAddress(args)
		if !ok {
			return
		}
		pool := ensurePool(b.pools, change.ContractID)
		reserve := ensureReserve(pool, asset)
		applyReserveConfig(reserve, value)
		// A live ResConfig write un-archives the reserve — see applyDelete's
		// ResConfig/ResData case for how Archived gets set on TTL lapse/eviction.
		reserve.state.Archived = false
		reserve.state.ArchivedLedgerSeq = 0
		finalizePoolReserves(pool)
		b.addDelta(ledgerSeq, "reserve", typedReserveEntityKey(change.ContractID, asset), true, pool.reserves[asset].state)
		b.appendPoolUsers(change.ContractID, ledgerSeq)
	case "ResData":
		asset, ok := variantAddress(args)
		if !ok {
			return
		}
		pool := ensurePool(b.pools, change.ContractID)
		reserve := ensureReserve(pool, asset)
		applyReserveData(reserve, value)
		reserve.state.Archived = false
		reserve.state.ArchivedLedgerSeq = 0
		finalizePoolReserves(pool)
		b.addDelta(ledgerSeq, "reserve", typedReserveEntityKey(change.ContractID, asset), true, pool.reserves[asset].state)
	case "EmisConfig":
		// res_token_id = reserve_index*2 + side (0 = borrow/d-token, 1 =
		// supply/b-token). The contract only lets EmisConfig be set for a
		// res_token_id whose reserve already exists (set_pool_emissions panics
		// otherwise), so ResList/ResConfig for this index is guaranteed to have
		// already folded — an unresolved index is dropped defensively.
		resTokenID, ok := variantU32(args)
		if !ok {
			return
		}
		pool := ensurePool(b.pools, change.ContractID)
		asset, ok := pool.reserveByIndex[int32(resTokenID/2)]
		if !ok {
			return
		}
		applyReserveEmisConfig(ensureReserve(pool, asset), resTokenID%2, value)
		finalizePoolReserves(pool)
		b.addDelta(ledgerSeq, "reserve", typedReserveEntityKey(change.ContractID, asset), true, pool.reserves[asset].state)
	case "EmisData":
		resTokenID, ok := variantU32(args)
		if !ok {
			return
		}
		pool := ensurePool(b.pools, change.ContractID)
		asset, ok := pool.reserveByIndex[int32(resTokenID/2)]
		if !ok {
			return
		}
		applyReserveEmisData(ensureReserve(pool, asset), resTokenID%2, value)
		finalizePoolReserves(pool)
		b.addDelta(ledgerSeq, "reserve", typedReserveEntityKey(change.ContractID, asset), true, pool.reserves[asset].state)
	case "Positions":
		user, ok := variantAddress(args)
		if !ok {
			return
		}
		pending := pendingUserPositions{poolContract: change.ContractID, user: user, positions: value, valueXDR: *change.ValueXDR}
		b.pendingPos[typedUserEntityKey(user, change.ContractID)] = pending
		if b.dirtyUsers != nil {
			b.dirtyUsers[typedUserEntityKey(user, change.ContractID)] = userIdentity{address: user, pool: change.ContractID}
		}
		b.appendUserPositions(pending, ledgerSeq)
	case "PoolBalance":
		poolID, ok := variantAddress(args)
		if !ok {
			return
		}
		balance := decodeBackstopPoolBalance(poolID, value)
		b.backstopPools[poolID] = balance
		b.addDelta(ledgerSeq, "backstop_pool", poolID, true, typedBackstopPool(balance))
		b.appendBackstopUsersForPool(poolID, ledgerSeq)
		// The shares<->LP conversion moved for every holder of this pool.
		b.markBackstopPoolDirty(poolID)
	case "UserBalance":
		poolID, user, ok := backstopPoolUser(args)
		if !ok {
			return
		}
		balance := decodeBackstopUserBalance(poolID, user, value)
		// UEmisData is a sibling entry — a UserBalance write must not clobber
		// carried emission fields.
		if existing, ok := b.backstopUsers[typedBackstopEntityKey(user, poolID)]; ok {
			balance.emisIndexRaw = existing.emisIndexRaw
			balance.emisAccruedRaw = existing.emisAccruedRaw
		}
		b.backstopUsers[typedBackstopEntityKey(user, poolID)] = balance
		b.markBackstopDirty(user, poolID)
		b.addDelta(ledgerSeq, "backstop_position", typedBackstopEntityKey(user, poolID), true, b.backstopPosition(balance))
	case "BEmisData":
		poolID, ok := variantAddress(args)
		if !ok {
			return
		}
		emis, ok := decodeBackstopEmisData(poolID, value)
		if !ok {
			// Malformed live write: keep whatever was carried rather than
			// half-clearing it — absent decode is a skip, never a wipe.
			return
		}
		b.backstopEmis[poolID] = emis
		b.addDelta(ledgerSeq, "backstop_emission", poolID, true, typedBackstopEmis(emis))
	case "UEmisData":
		poolID, user, ok := backstopPoolUser(args)
		if !ok {
			return
		}
		indexRaw, accruedRaw, ok := decodeUserEmissionData(value)
		if !ok {
			return
		}
		entityKey := typedBackstopEntityKey(user, poolID)
		balance, exists := b.backstopUsers[entityKey]
		if !exists {
			// Accrual can outlive (or precede) a UserBalance entry — carry it on
			// an otherwise-empty balance row rather than dropping it.
			balance = backstopUserBalance{poolContract: poolID, user: user}
		}
		balance.emisIndexRaw = indexRaw
		balance.emisAccruedRaw = accruedRaw
		b.backstopUsers[entityKey] = balance
		b.markBackstopDirty(user, poolID)
		b.addDelta(ledgerSeq, "backstop_position", entityKey, true, b.backstopPosition(balance))
	case "Auction":
		user, auctType, ok := auctionKeyParts(args)
		if !ok {
			return
		}
		auction, ok := decodeAuctionState(change.ContractID, user, auctType, value)
		if !ok {
			// Malformed AuctionData (missing block, non-map lot/bid, undecodable
			// entry): skip the write entirely — a partial auction would
			// under-report a liquidation's lot or bid, which is worse than
			// keeping the carried state absent/stale until the next good write.
			return
		}
		entityKey := typedAuctionEntityKey(change.ContractID, user, auctType)
		b.auctions[entityKey] = auction
		b.dirtyTemporary[entityKey] = bindings.TemporaryStateChange{
			Kind:           bindings.TemporaryAuction,
			PoolContractID: change.ContractID,
			UserAddress:    user,
			AuctionType:    auctType,
		}
		b.addDelta(ledgerSeq, "auction", entityKey, true, auction)
	case "UserEmis":
		user, resTokenID, ok := userReserveKeyParts(args)
		if !ok {
			return
		}
		indexRaw, accruedRaw, ok := decodeUserEmissionData(value)
		if !ok {
			return
		}
		emission := contracts.UserEmissionState{
			Address:        user,
			PoolContractID: change.ContractID,
			ReserveTokenID: resTokenID,
			IndexRaw:       indexRaw,
			AccruedRaw:     accruedRaw,
		}
		entityKey := typedUserEmisEntityKey(user, change.ContractID, resTokenID)
		b.userEmis[entityKey] = emission
		b.addDelta(ledgerSeq, "user_emission", entityKey, true, emission)
	case "ResInit":
		asset, ok := variantAddress(args)
		if !ok {
			return
		}
		queued, ok := decodeQueuedReserve(change.ContractID, asset, value)
		if !ok {
			// Malformed QueuedReserveInit: skip whole — a queued change with an
			// unknown unlock time or config shape must not surface half-decoded.
			return
		}
		entityKey := typedReserveEntityKey(change.ContractID, asset)
		b.queuedReserves[entityKey] = queued
		b.dirtyTemporary[entityKey] = bindings.TemporaryStateChange{
			Kind:           bindings.TemporaryQueuedReserve,
			PoolContractID: change.ContractID,
			AssetID:        asset,
		}
		b.addDelta(ledgerSeq, "queued_reserve", entityKey, true, queued)
	}
}

// isExplicitOnChainDelete reports whether a not-live change is a genuine
// on-chain removal (the contract itself cleared the entry — CAP-23
// LEDGER_ENTRY_REMOVED) rather than a TTL lapse or a CAP-0062 network-level
// eviction. The relay extract (relay.lightgate.xyz/internal/relay/state)
// threads the underlying xdr.LedgerEntryChangeType through as ChangeType via
// its .String() form, so a real removal's value ends in "Removed"
// (LedgerEntryChangeTypeLedgerEntryRemoved / test fixtures' short "Removed");
// TTL lapse leaves ChangeType at whatever the entry's last live change was
// (Created/Updated/Restored, forced not-live by Live/LiveUntilLedgerSeq
// instead), and a synthesized eviction is tagged "evicted". Both of the
// latter are restorable — the holder still owns the entry — so Change 1 (see
// applyDelete's Positions and ResConfig/ResData cases) archives them instead
// of purging. Only a genuine removal purges, matching prior behavior exactly.
func isExplicitOnChainDelete(changeType string) bool {
	return strings.HasSuffix(changeType, "Removed")
}

func (b *blendStateBuilder) applyDelete(change bindings.ContractDataChange, key xdr.ScVal, ledgerSeq int64) {
	if _, isFeed := b.ownedFeeds[change.ContractID]; isFeed {
		// Feed rounds are temporary storage with a ~day-scale TTL; by the time a
		// round's delete arrives it is far outside any aggregator's staleness
		// window and long since trimmed from the bounded carry, so dropping it is
		// normally a no-op. Absorbed here regardless so a feed's deletes never
		// fall through to the pool delete logic.
		b.applyFeedDelete(change.ContractID, key)
		b.changedFeeds[change.ContractID] = struct{}{}
		return
	}
	if _, isComet := b.ownedComets[change.ContractID]; isComet {
		// A registered Comet's not-live changes go absent on the Comet facet they
		// name (state_comet.go), never to the Blend pool delete logic.
		b.applyCometDelete(change, key, ledgerSeq)
		return
	}
	if _, isAsset := b.ownedAssets[change.ContractID]; isAsset {
		// Any change on a registered asset contract is fully absorbed here — it never
		// falls through to the pool delete logic below. Only the instance entry
		// itself going not-live (evicted or TTL-lapsed) clears the decoded identity;
		// a registered asset also emits Balance/Allowance persistent-storage deletes
		// that carry no bearing on its symbol/name/decimals and must not wipe them.
		if key.Type == xdr.ScValTypeScvLedgerKeyContractInstance {
			delete(b.assets, change.ContractID)
		}
		return
	}
	if _, owned := b.owned[change.ContractID]; owned && isOraclePriceKey(key) {
		// A price entry is temporary storage: once it is evicted or its TTL lapses
		// the price is gone on-chain, so drop it here too. This is the storage-level
		// form of the contract's reject-stale-price rule — a price that is no longer
		// live leaves the reserve without one, surfaced downstream as unavailable.
		if index, ok := scInt64(key); ok {
			if oracle := b.oracles[change.ContractID]; oracle != nil {
				delete(oracle.priceByIndex, index)
				b.markOraclePriceAssetDirty(oracle, index)
			}
		}
		return
	}
	if key.Type == xdr.ScValTypeScvLedgerKeyContractInstance {
		// An oracle-aggregator's instance going not-live takes its configuration
		// with it — prices stop resolving rather than freezing on the last config.
		// A backstop instance going not-live clears its decoded identity the
		// same way. For every other contract an instance delete stays the no-op
		// it was.
		delete(b.aggregators, change.ContractID)
		if _, ok := b.backstopInstances[change.ContractID]; ok {
			delete(b.backstopInstances, change.ContractID)
			b.addDelta(ledgerSeq, "backstop_instance", change.ContractID, false, nil)
		}
		return
	}
	if sym, ok := scSymbol(key); ok {
		switch sym {
		case "Config", "ResList":
			delete(b.pools, change.ContractID)
			b.addDelta(ledgerSeq, "pool", change.ContractID, false, nil)
		case "Backstop":
			if pool := b.pools[change.ContractID]; pool != nil {
				pool.state.BackstopContract = ""
				b.addDelta(ledgerSeq, "pool", change.ContractID, true, pool.state)
			}
		case "PoolEmis":
			if pool := b.pools[change.ContractID]; pool != nil {
				pool.state.PoolEmissions = nil
				b.addDelta(ledgerSeq, "pool", change.ContractID, true, pool.state)
			}
		case "RZ":
			if instance := b.backstopInstances[change.ContractID]; instance != nil {
				instance.RewardZone = nil
				b.addDelta(ledgerSeq, "backstop_instance", change.ContractID, true, instance)
			}
		case "DropList":
			if instance := b.backstopInstances[change.ContractID]; instance != nil {
				instance.DropList = nil
				b.addDelta(ledgerSeq, "backstop_instance", change.ContractID, true, instance)
			}
		case "timestamp":
			if oracle := b.oracles[change.ContractID]; oracle != nil {
				oracle.lastTimestampRaw = ""
			}
		}
		return
	}
	variant, args, ok := scVariant(key)
	if !ok {
		return
	}
	switch variant {
	case "ResConfig", "ResData":
		asset, ok := variantAddress(args)
		if !ok {
			return
		}
		pool := b.pools[change.ContractID]
		if pool == nil {
			return
		}
		if isExplicitOnChainDelete(change.ChangeType) {
			delete(pool.reserves, asset)
			// reserveByIndex is fully rebuilt below by finalizePoolReserves, so the
			// reserve ordering stays index-stable (the prior code deleted index 0
			// unconditionally here — a bug; dropped).
			finalizePoolReserves(pool)
			b.addDelta(ledgerSeq, "reserve", typedReserveEntityKey(change.ContractID, asset), false, nil)
			b.appendPoolUsers(change.ContractID, ledgerSeq)
			return
		}
		// TTL lapse or network eviction (not a real on-chain delete): archive
		// rather than drop. Deleting the reserve here would erase its
		// reserveByIndex slot and silently zero every holder's position in this
		// asset (positionsFromMap resolves an index only through that map) —
		// the exact bug this fixes. If the change still carries a value (TTL
		// lapse: the entry's data survives, only its liveness lapsed) apply it
		// first so the archived reserve reflects its last known state.
		reserve := ensureReserve(pool, asset)
		if change.ValueXDR != nil {
			if value, ok := decodeScValBase64(*change.ValueXDR); ok {
				if variant == "ResConfig" {
					applyReserveConfig(reserve, value)
				} else {
					applyReserveData(reserve, value)
				}
			}
		}
		reserve.state.Archived = true
		reserve.state.ArchivedLedgerSeq = ledgerSeq
		finalizePoolReserves(pool)
		b.addDelta(ledgerSeq, "reserve", typedReserveEntityKey(change.ContractID, asset), true, pool.reserves[asset].state)
		b.appendPoolUsers(change.ContractID, ledgerSeq)
	case "EmisConfig", "EmisData":
		// Unlike ResConfig/ResData, losing an emission entry (TTL lapse or
		// eviction) does not remove the reserve itself — it only clears that
		// side's emission fields, so the reserve's core lending config/data
		// survives. The delta still reports the reserve as live with its (now
		// emission-cleared) state.
		resTokenID, ok := variantU32(args)
		if !ok {
			return
		}
		pool := b.pools[change.ContractID]
		if pool == nil {
			return
		}
		asset, ok := pool.reserveByIndex[int32(resTokenID/2)]
		if !ok {
			return
		}
		reserve := pool.reserves[asset]
		if reserve == nil {
			return
		}
		if variant == "EmisConfig" {
			clearReserveEmisConfig(reserve, resTokenID%2)
		} else {
			clearReserveEmisData(reserve, resTokenID%2)
		}
		finalizePoolReserves(pool)
		b.addDelta(ledgerSeq, "reserve", typedReserveEntityKey(change.ContractID, asset), true, pool.reserves[asset].state)
	case "Positions":
		user, ok := variantAddress(args)
		if !ok {
			return
		}
		entityKey := typedUserEntityKey(user, change.ContractID)
		if b.dirtyUsers != nil {
			b.dirtyUsers[entityKey] = userIdentity{address: user, pool: change.ContractID}
		}
		if isExplicitOnChainDelete(change.ChangeType) {
			delete(b.pendingPos, entityKey)
			b.addDelta(ledgerSeq, "user_positions", entityKey, false, nil)
			return
		}
		// TTL lapse or network eviction (not a real on-chain delete): archive
		// rather than purge. On mainnet this is the ≥321 dormant-but-live
		// holders the old code silently dropped from Users/PendingUserPositions
		// — their entry archived, still owned, restorable. If the change still
		// carries a value (TTL lapse leaves ValueXDR populated; only eviction
		// nils it) that is the last known blob and replaces the carried one;
		// otherwise the prior blob is kept as-is.
		existing, hadPrior := b.pendingPos[entityKey]
		archived := pendingUserPositions{
			poolContract:      change.ContractID,
			user:              user,
			archived:          true,
			archivedLedgerSeq: ledgerSeq,
		}
		switch {
		case change.ValueXDR != nil:
			if value, ok := decodeScValBase64(*change.ValueXDR); ok {
				archived.positions = value
				archived.valueXDR = *change.ValueXDR
			} else if hadPrior {
				archived.positions, archived.valueXDR = existing.positions, existing.valueXDR
			}
		case hadPrior:
			archived.positions, archived.valueXDR = existing.positions, existing.valueXDR
		default:
			// Nothing to archive: no prior entry and no value on this change.
			return
		}
		b.pendingPos[entityKey] = archived
		b.appendUserPositions(archived, ledgerSeq)
	case "PoolBalance":
		poolID, ok := variantAddress(args)
		if !ok {
			return
		}
		delete(b.backstopPools, poolID)
		b.addDelta(ledgerSeq, "backstop_pool", poolID, false, nil)
		b.appendBackstopUsersForPool(poolID, ledgerSeq)
		// Every holder's shares<->LP conversion lost its denominator.
		b.markBackstopPoolDirty(poolID)
	case "UserBalance":
		poolID, user, ok := backstopPoolUser(args)
		if !ok {
			return
		}
		entityKey := typedBackstopEntityKey(user, poolID)
		b.markBackstopDirty(user, poolID)
		// The sibling UEmisData entry survives a UserBalance delete on-chain
		// (accrued-but-unclaimed emissions persist after a full withdrawal), so
		// only the balance half is cleared; the row is dropped when nothing
		// remains.
		if balance, exists := b.backstopUsers[entityKey]; exists && (balance.emisIndexRaw != "" || balance.emisAccruedRaw != "") {
			balance.sharesRaw = ""
			balance.q4w = nil
			b.backstopUsers[entityKey] = balance
			b.addDelta(ledgerSeq, "backstop_position", entityKey, true, b.backstopPosition(balance))
			return
		}
		delete(b.backstopUsers, entityKey)
		b.addDelta(ledgerSeq, "backstop_position", entityKey, false, nil)
	case "BEmisData":
		poolID, ok := variantAddress(args)
		if !ok {
			return
		}
		delete(b.backstopEmis, poolID)
		b.addDelta(ledgerSeq, "backstop_emission", poolID, false, nil)
	case "UEmisData":
		poolID, user, ok := backstopPoolUser(args)
		if !ok {
			return
		}
		entityKey := typedBackstopEntityKey(user, poolID)
		balance, exists := b.backstopUsers[entityKey]
		if !exists {
			return
		}
		b.markBackstopDirty(user, poolID)
		balance.emisIndexRaw = ""
		balance.emisAccruedRaw = ""
		if balance.sharesRaw == "" && len(balance.q4w) == 0 {
			// The entry only existed for the emission accrual — drop it.
			delete(b.backstopUsers, entityKey)
			b.addDelta(ledgerSeq, "backstop_position", entityKey, false, nil)
			return
		}
		b.backstopUsers[entityKey] = balance
		b.addDelta(ledgerSeq, "backstop_position", entityKey, true, b.backstopPosition(balance))
	case "Auction":
		user, auctType, ok := auctionKeyParts(args)
		if !ok {
			return
		}
		// Auctions are temporary storage: not-live (filled, deleted, or
		// TTL-lapsed) means the auction is gone on-chain — remove, never archive.
		entityKey := typedAuctionEntityKey(change.ContractID, user, auctType)
		delete(b.auctions, entityKey)
		b.dirtyTemporary[entityKey] = bindings.TemporaryStateChange{
			Kind:           bindings.TemporaryAuction,
			PoolContractID: change.ContractID,
			UserAddress:    user,
			AuctionType:    auctType,
		}
		b.addDelta(ledgerSeq, "auction", entityKey, false, nil)
	case "UserEmis":
		user, resTokenID, ok := userReserveKeyParts(args)
		if !ok {
			return
		}
		// Absent-not-zero: a not-live accrual entry leaves the user's emission
		// state absent (it re-folds from bronze on restore), never zero-filled.
		entityKey := typedUserEmisEntityKey(user, change.ContractID, resTokenID)
		delete(b.userEmis, entityKey)
		b.addDelta(ledgerSeq, "user_emission", entityKey, false, nil)
	case "ResInit":
		// The queue resolved (set_reserve executed, cancel_set_reserve, or the
		// entry lapsed) — the pending change is gone either way.
		asset, ok := variantAddress(args)
		if !ok {
			return
		}
		entityKey := typedReserveEntityKey(change.ContractID, asset)
		delete(b.queuedReserves, entityKey)
		b.dirtyTemporary[entityKey] = bindings.TemporaryStateChange{
			Kind:           bindings.TemporaryQueuedReserve,
			PoolContractID: change.ContractID,
			AssetID:        asset,
		}
		b.addDelta(ledgerSeq, "queued_reserve", entityKey, false, nil)
	}
}

func (b *blendStateBuilder) appendPoolReserves(poolID string, ledgerSeq int64) {
	pool := b.pools[poolID]
	if pool == nil {
		return
	}
	for asset, reserve := range pool.reserves {
		if reserve.state.AssetID == "" {
			reserve.state.AssetID = asset
		}
		b.addDelta(ledgerSeq, "reserve", typedReserveEntityKey(poolID, asset), true, reserve.state)
	}
}

func (b *blendStateBuilder) appendPoolUsers(poolID string, ledgerSeq int64) {
	for _, pending := range b.pendingPos {
		if pending.poolContract == poolID {
			b.appendUserPositions(pending, ledgerSeq)
		}
	}
}

func (b *blendStateBuilder) appendBackstopUsersForPool(poolID string, ledgerSeq int64) {
	for _, balance := range b.backstopUsers {
		if balance.poolContract != poolID {
			continue
		}
		b.addDelta(ledgerSeq, "backstop_position", typedBackstopEntityKey(balance.user, poolID), true, b.backstopPosition(balance))
	}
}

func (b *blendStateBuilder) appendUserPositions(pending pendingUserPositions, ledgerSeq int64) {
	pool := b.pools[pending.poolContract]
	if pool == nil {
		return
	}
	finalizePoolReserves(pool)
	// Silver-debug deltas recompute positions for their own payload only — no
	// diagnostics sink; the fold's skipped-leg records come from build().
	positions := buildUserPositionsForPending(pool, pending, nil)
	b.addDelta(ledgerSeq, "user_positions", typedUserEntityKey(pending.user, pending.poolContract), true, positions)
}

func buildUserPositionsForPending(pool *poolBuilder, pending pendingUserPositions, sink *positionSkipSink) []contracts.UserReservePosition {
	fields := scMapFields(pending.positions)
	out := make([]contracts.UserReservePosition, 0)
	out = append(out, positionsFromMap(pool, pending, fields["supply"], contracts.PositionTypeSupply, sink)...)
	out = append(out, positionsFromMap(pool, pending, fields["collateral"], contracts.PositionTypeCollateral, sink)...)
	out = append(out, positionsFromMap(pool, pending, fields["liabilities"], contracts.PositionTypeLiability, sink)...)
	return out
}

func (b *blendStateBuilder) backstopPosition(userBalance backstopUserBalance) contracts.BackstopPosition {
	poolBalance := b.backstopPools[userBalance.poolContract]

	// Comet LP valuation inputs (V1-09): the folded Comet pool behind this
	// backstop's BToken, joined by exact token ID (never vector position), plus
	// each leg's ledger-pinned price binding (tokenPriceUSD — the folded pool
	// reserve whose asset IS that token, first in ascending pool order). Every
	// facet stays "" while its identity is incomplete: no Comet, no record, no
	// supply, or no folded price source is explicit absence, never a fabricated
	// zero and never a hardcoded $1 (D-09).
	lpSupply := ""
	blndReserve := ""
	usdcReserve := ""
	blndPrice := ""
	usdcPrice := ""
	if instance := b.backstopInstanceForPool(userBalance.poolContract); instance != nil {
		if comet := b.cometForBackstopInstance(instance); comet != nil {
			lpSupply = comet.lpSupplyRaw()
			blndReserve = comet.reserveOf(instance.BLNDToken)
			usdcReserve = comet.reserveOf(instance.USDCToken)
		}
		blndPrice = b.tokenPriceUSD(instance.BLNDToken)
		usdcPrice = b.tokenPriceUSD(instance.USDCToken)
	}

	return contracts.BackstopPosition{
		Address:        userBalance.user,
		PoolContractID: userBalance.poolContract,
		UserSharesRaw:  userBalance.sharesRaw,
		PoolSharesRaw:  poolBalance.sharesRaw,
		PoolTokensRaw:  poolBalance.tokensRaw,
		Q4W:            userBalance.q4w,
		// The UEmisData sibling entry, when present: the user's checkpointed
		// unclaimed backstop BLND and last accrued index. Absent stays "".
		UnclaimedEmissionsRaw: userBalance.emisAccruedRaw,
		EmisIndexRaw:          userBalance.emisIndexRaw,
		BLNDDecimals:          7,
		USDCDecimals:          7,
		LPTokenSupplyRaw:      lpSupply,
		LPBLNDReserveRaw:      blndReserve,
		LPUSDCReserveRaw:      usdcReserve,
		BLNDPriceUSD:          blndPrice,
		USDCPriceUSD:          usdcPrice,
		BackstopInterestAPY:   "",
		BackstopEmissionsAPY:  "",
	}
}

// typedBackstopPoolDelta is the silver-debug delta payload for a backstop pool
// balance (exported fields so addDelta can JSON-marshal it deterministically).
type typedBackstopPoolDelta struct {
	PoolContractID string
	SharesRaw      string
	TokensRaw      string
	Q4WRaw         string
}

func typedBackstopPool(balance backstopPoolBalance) typedBackstopPoolDelta {
	return typedBackstopPoolDelta{
		PoolContractID: balance.poolContract,
		SharesRaw:      balance.sharesRaw,
		TokensRaw:      balance.tokensRaw,
		Q4WRaw:         balance.q4wRaw,
	}
}

// typedBackstopEmisDelta is the silver-debug delta payload for a pool's
// BEmisData entry (exported fields so addDelta can JSON-marshal it).
type typedBackstopEmisDelta struct {
	PoolContractID string
	EPSRaw         string
	ExpirationRaw  string
	IndexRaw       string
	LastTimeRaw    string
}

func typedBackstopEmis(emis backstopEmisData) typedBackstopEmisDelta {
	return typedBackstopEmisDelta{
		PoolContractID: emis.poolContract,
		EPSRaw:         emis.epsRaw,
		ExpirationRaw:  emis.expirationRaw,
		IndexRaw:       emis.indexRaw,
		LastTimeRaw:    emis.lastTimeRaw,
	}
}

func (b *blendStateBuilder) addDelta(ledgerSeq int64, entityType, entityKey string, live bool, state any) {
	var raw json.RawMessage
	if state != nil {
		if encoded, err := json.Marshal(state); err == nil {
			raw = encoded
		}
	}
	b.deltas = append(b.deltas, typedStateDelta{
		LedgerSeq:  ledgerSeq,
		EntityType: entityType,
		EntityKey:  entityKey,
		Live:       live,
		StateJSON:  raw,
	})
}

func typedReserveEntityKey(poolID, assetID string) string  { return poolID + "|" + assetID }
func typedUserEntityKey(address, poolID string) string     { return address + "|" + poolID }
func typedBackstopEntityKey(address, poolID string) string { return address + "|" + poolID }

func typedAuctionEntityKey(poolID, user string, auctType int32) string {
	return poolID + "|" + user + "|" + strconv.FormatInt(int64(auctType), 10)
}

func typedUserEmisEntityKey(address, poolID string, resTokenID int32) string {
	return address + "|" + poolID + "|" + strconv.FormatInt(int64(resTokenID), 10)
}

func sortLedgerState(pools []contracts.PoolState, users []contracts.UserReservePosition, backstops []contracts.BackstopPosition) {
	sort.Slice(pools, func(i, j int) bool { return pools[i].ContractID < pools[j].ContractID })
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
	sort.Slice(backstops, func(i, j int) bool {
		if backstops[i].Address != backstops[j].Address {
			return backstops[i].Address < backstops[j].Address
		}
		return backstops[i].PoolContractID < backstops[j].PoolContractID
	})
}

func ensurePool(pools map[string]*poolBuilder, contractID string) *poolBuilder {
	pool, ok := pools[contractID]
	if !ok {
		pool = &poolBuilder{
			state: contracts.PoolState{
				ContractID: contractID,
				PoolStatus: "unknown",
			},
			reserves:       map[string]*reserveBuilder{},
			reserveByIndex: map[int32]string{},
		}
		pools[contractID] = pool
	}
	return pool
}

func ensureReserve(pool *poolBuilder, assetID string) *reserveBuilder {
	reserve, ok := pool.reserves[assetID]
	if !ok {
		reserve = &reserveBuilder{state: contracts.ReserveState{AssetID: assetID}}
		pool.reserves[assetID] = reserve
	}
	return reserve
}

func isPoolConfig(value xdr.ScVal) bool {
	fields := scMapFields(value)
	if fields == nil {
		return false
	}
	_, hasOracle := fieldAddress(fields, "oracle")
	_, hasBackstopTake := fieldIntString(fields, "bstop_rate")
	_, hasStatus := fieldInt32(fields, "status")
	return hasOracle && hasBackstopTake && hasStatus
}

func applyPoolConfig(pool *poolBuilder, value xdr.ScVal) {
	fields := scMapFields(value)
	if oracle, ok := fieldAddress(fields, "oracle"); ok {
		pool.state.OracleContract = oracle
	}
	if bstopRate, ok := fieldIntString(fields, "bstop_rate"); ok {
		pool.state.BackstopTakeRate = bstopRate
	}
	if statusRaw, ok := fieldInt32(fields, "status"); ok {
		pool.state.PoolStatus = blendPoolStatus(statusRaw)
	}
	if maxPositions, ok := fieldIntString(fields, "max_positions"); ok {
		pool.state.MaxPositionsRaw = maxPositions
	}
	if minCollateral, ok := fieldIntString(fields, "min_collateral"); ok {
		pool.state.MinCollateralRaw = minCollateral
	}
}

// applyPoolInstanceStorage folds a Blend pool's instance-storage map. A pool's
// PoolConfig (oracle, backstop take rate, status) and its backstop address live
// INSIDE the contract instance's storage, keyed by the symbols "Config" and
// "Backstop" — they are not emitted as top-level contract_data entries the way
// "ResList" is. They must be read from the instance here or the pool's oracle
// link is never populated, and every reserve's USD value and health factor
// surface as unavailable. The instance is written at deploy and carried across
// ledgers via loadPrior, so a single Set is enough.
func applyPoolInstanceStorage(pool *poolBuilder, instance xdr.ScContractInstance) {
	if instance.Storage == nil {
		return
	}
	for _, entry := range []xdr.ScMapEntry(*instance.Storage) {
		name, ok := scSymbol(entry.Key)
		if !ok {
			continue
		}
		switch name {
		case "Config":
			if isPoolConfig(entry.Val) {
				applyPoolConfig(pool, entry.Val)
			}
		case "Backstop":
			if address, ok := scAddress(entry.Val); ok {
				pool.state.BackstopContract = address
			}
		case "Name":
			// The on-chain display name — the authoritative value the API's
			// registry-sourced pool_name can diverge from.
			if poolName := scValSymbol(entry.Val); poolName != "" {
				pool.state.Name = poolName
			}
		case "Admin":
			if address, ok := scAddress(entry.Val); ok {
				pool.state.Admin = address
			}
		case "BLNDTkn":
			if address, ok := scAddress(entry.Val); ok {
				pool.state.BLNDToken = address
			}
		}
	}
}

func applyReserveList(pool *poolBuilder, value xdr.ScVal) {
	items, ok := scVec(value)
	if !ok {
		return
	}
	seen := map[string]struct{}{}
	for _, item := range items {
		assetID, ok := scAddress(item)
		if !ok {
			continue
		}
		if _, exists := seen[assetID]; exists {
			continue
		}
		seen[assetID] = struct{}{}
		pool.reserveList = append(pool.reserveList, assetID)
		ensureReserve(pool, assetID)
	}
}

func applyReserveConfig(reserve *reserveBuilder, value xdr.ScVal) {
	fields := scMapFields(value)
	if index, ok := fieldInt32(fields, "index"); ok {
		reserve.state.ReserveIndex = index
		// The decoded ResConfig.index is the ONLY authority on index validity:
		// it — and nothing else — marks the reserve's index as known. A reserve
		// materialized by ResData alone keeps the zero-value default with
		// ReserveIndexKnown=false and never enters index-based resolution.
		reserve.state.ReserveIndexKnown = true
	}
	if decimals, ok := fieldInt32(fields, "decimals"); ok {
		reserve.state.AssetDecimals = decimals
	}
	if cFactor, ok := fieldIntString(fields, "c_factor"); ok {
		reserve.state.CFactorRaw = cFactor
	}
	if lFactor, ok := fieldIntString(fields, "l_factor"); ok {
		reserve.state.LFactorRaw = lFactor
	}
	if util, ok := fieldIntString(fields, "util"); ok {
		reserve.state.UtilTargetRaw = util
	}
	if maxUtil, ok := fieldIntString(fields, "max_util"); ok {
		reserve.state.MaxUtilRaw = maxUtil
	}
	if rBase, ok := fieldIntString(fields, "r_base"); ok {
		reserve.state.RBaseRaw = rBase
	}
	if rOne, ok := fieldIntString(fields, "r_one"); ok {
		reserve.state.ROneRaw = rOne
	}
	if rTwo, ok := fieldIntString(fields, "r_two"); ok {
		reserve.state.RTwoRaw = rTwo
	}
	if rThree, ok := fieldIntString(fields, "r_three"); ok {
		reserve.state.RThreeRaw = rThree
	}
	if supplyCap, ok := fieldIntString(fields, "supply_cap"); ok {
		reserve.state.SupplyCapRaw = supplyCap
	}
	if reactivity, ok := fieldIntString(fields, "reactivity"); ok {
		reserve.state.ReactivityRaw = reactivity
	}
	if enabled, ok := fieldBool(fields, "enabled"); ok {
		reserve.state.Enabled = enabled
	}
}

func applyReserveData(reserve *reserveBuilder, value xdr.ScVal) {
	fields := scMapFields(value)
	if dRate, ok := fieldIntString(fields, "d_rate"); ok {
		reserve.state.DRateRaw = dRate
	}
	if bRate, ok := fieldIntString(fields, "b_rate"); ok {
		reserve.state.BRateRaw = bRate
	}
	if irMod, ok := fieldIntString(fields, "ir_mod"); ok {
		reserve.state.RateModifierRaw = irMod
	}
	if bSupply, ok := fieldIntString(fields, "b_supply"); ok {
		reserve.state.BSupplyRaw = bSupply
	}
	if dSupply, ok := fieldIntString(fields, "d_supply"); ok {
		reserve.state.DSupplyRaw = dSupply
	}
	if backstopCredit, ok := fieldIntString(fields, "backstop_credit"); ok {
		// PoolBalanceRaw keeps carrying backstop_credit for compatibility with
		// existing consumers (surfaced as pool_balance_raw metadata); the field
		// also lands under its own on-chain name.
		reserve.state.PoolBalanceRaw = backstopCredit
		reserve.state.BackstopCreditRaw = backstopCredit
	}
	if lastTime, ok := fieldIntString(fields, "last_time"); ok {
		reserve.state.LastTimeRaw = lastTime
	}
}

// applyReserveEmisConfig decodes one side's ReserveEmissionsConfig {expiration,
// eps} onto the reserve. side is res_token_id % 2: 1 = supply/b-token, 0 =
// borrow/d-token (matches Blend's pool contract convention).
func applyReserveEmisConfig(reserve *reserveBuilder, side uint32, value xdr.ScVal) {
	fields := scMapFields(value)
	eps, epsOK := fieldIntString(fields, "eps")
	expiration, expOK := fieldIntString(fields, "expiration")
	if side == 1 {
		if epsOK {
			reserve.state.SupplyEmisEPSRaw = eps
		}
		if expOK {
			reserve.state.SupplyEmisExpirationRaw = expiration
		}
	} else {
		if epsOK {
			reserve.state.BorrowEmisEPSRaw = eps
		}
		if expOK {
			reserve.state.BorrowEmisExpirationRaw = expiration
		}
	}
}

// applyReserveEmisData decodes one side's emission-data entry onto the
// reserve. side follows the same convention as applyReserveEmisConfig.
// blend-contracts-v2 merged the v1 config/data split into one struct —
// EmisData(u32) now holds ReserveEmissionData {expiration, eps, index,
// last_time} — so eps/expiration are read here too when present. Every field
// stays independently optional: a v1-shaped {index, last_time} entry sets
// only those two, and an absent field is never zero-filled.
func applyReserveEmisData(reserve *reserveBuilder, side uint32, value xdr.ScVal) {
	fields := scMapFields(value)
	index, indexOK := fieldIntString(fields, "index")
	lastTime, lastOK := fieldIntString(fields, "last_time")
	eps, epsOK := fieldIntString(fields, "eps")
	expiration, expOK := fieldIntString(fields, "expiration")
	if side == 1 {
		if indexOK {
			reserve.state.SupplyEmisIndexRaw = index
		}
		if lastOK {
			reserve.state.SupplyEmisLastTimeRaw = lastTime
		}
		if epsOK {
			reserve.state.SupplyEmisEPSRaw = eps
		}
		if expOK {
			reserve.state.SupplyEmisExpirationRaw = expiration
		}
	} else {
		if indexOK {
			reserve.state.BorrowEmisIndexRaw = index
		}
		if lastOK {
			reserve.state.BorrowEmisLastTimeRaw = lastTime
		}
		if epsOK {
			reserve.state.BorrowEmisEPSRaw = eps
		}
		if expOK {
			reserve.state.BorrowEmisExpirationRaw = expiration
		}
	}
}

// clearReserveEmisConfig / clearReserveEmisData drop one side's emission
// fields back to absent (empty string) when its storage entry is evicted or
// TTL-lapses — the reserve itself is untouched, only that side's emission
// data goes away.
func clearReserveEmisConfig(reserve *reserveBuilder, side uint32) {
	if side == 1 {
		reserve.state.SupplyEmisEPSRaw = ""
		reserve.state.SupplyEmisExpirationRaw = ""
	} else {
		reserve.state.BorrowEmisEPSRaw = ""
		reserve.state.BorrowEmisExpirationRaw = ""
	}
}

// clearReserveEmisData clears all four emission fields: on the v2 deploy the
// EmisData entry is the sole source of eps/expiration too (ReserveEmissionData
// merged the v1 split), so leaving them set after the entry goes not-live
// would fabricate an active emission that no longer exists on-chain. Under a
// v1-style split the (persisted) EmisConfig entry re-folds eps/expiration on
// its next write.
func clearReserveEmisData(reserve *reserveBuilder, side uint32) {
	if side == 1 {
		reserve.state.SupplyEmisIndexRaw = ""
		reserve.state.SupplyEmisLastTimeRaw = ""
		reserve.state.SupplyEmisEPSRaw = ""
		reserve.state.SupplyEmisExpirationRaw = ""
	} else {
		reserve.state.BorrowEmisIndexRaw = ""
		reserve.state.BorrowEmisLastTimeRaw = ""
		reserve.state.BorrowEmisEPSRaw = ""
		reserve.state.BorrowEmisExpirationRaw = ""
	}
}

func (b *blendStateBuilder) ensureOracle(oracleID string) *oracleBuilder {
	oracle, ok := b.oracles[oracleID]
	if !ok {
		oracle = &oracleBuilder{
			assetToIndex: map[string]int64{},
			priceByIndex: map[int64]string{},
		}
		b.oracles[oracleID] = oracle
	}
	return oracle
}

// isOraclePriceKey reports whether a contract_data key is a price entry's key.
// The oracle keys each asset's price by the asset's index as a u128; no pool or
// backstop entry uses a bare u128 key, so this is an unambiguous discriminator.
func isOraclePriceKey(key xdr.ScVal) bool {
	return key.Type == xdr.ScValTypeScvU128
}

// applyOracleInstance decodes a price oracle's contract instance. Its storage
// holds the ordered asset list (asset -> index) and the shared price decimals.
// It returns true only when the instance actually looks like an oracle (it
// carries both an asset list and a decimals entry), so a pool's contract
// instance still falls through to the pool-wasm handling.
func (b *blendStateBuilder) applyOracleInstance(oracleID string, value xdr.ScVal) bool {
	instance, ok := value.GetInstance()
	if !ok || instance.Storage == nil {
		return false
	}
	storage := map[string]xdr.ScVal{}
	for _, entry := range []xdr.ScMapEntry(*instance.Storage) {
		name, ok := scSymbol(entry.Key)
		if !ok {
			continue
		}
		storage[name] = entry.Val
	}
	assetsVal, hasAssets := storage["assets"]
	decimals, hasDecimals := scInt32(storage["decimals"])
	if !hasAssets || !hasDecimals {
		return false
	}
	items, ok := scVec(assetsVal)
	if !ok {
		return false
	}
	oracle := b.ensureOracle(oracleID)
	oracle.decimals = decimals
	// Instance facets beyond the asset list (audit section 4): the quote asset
	// (base), the update cadence (res, seconds) and the admin. Each optional —
	// absent keys leave the field empty, never guessed.
	if baseKey, ok := canonicalAssetKey(storage["base"]); ok {
		oracle.baseKey = baseKey
	}
	if resolution, ok := scIntString(storage["res"]); ok {
		oracle.resolutionRaw = resolution
	}
	if admin, ok := scAddress(storage["admin"]); ok {
		oracle.admin = admin
	}
	assetToIndex := map[string]int64{}
	for i, item := range items {
		// Each asset is the SEP-40 Asset::Stellar(address) enum, encoded as a vec
		// of [symbol, address]; only Stellar assets carry a contract-address price.
		_, args, ok := scVariant(item)
		if !ok {
			continue
		}
		asset, ok := variantAddress(args)
		if !ok {
			continue
		}
		assetToIndex[asset] = int64(i)
	}
	oracle.assetToIndex = assetToIndex
	return true
}

// applyOraclePrice records one asset's raw oracle price. The entry is keyed by
// the asset's index (a u128) and holds the price as an i128; resolving it back to
// a reserve happens at build time, once the asset list is known.
func (b *blendStateBuilder) applyOraclePrice(oracleID string, key, value xdr.ScVal) {
	index, ok := scInt64(key)
	if !ok || index < 0 {
		return
	}
	priceRaw, ok := scIntString(value)
	if !ok {
		return
	}
	oracle := b.ensureOracle(oracleID)
	oracle.priceByIndex[index] = priceRaw
	b.markOraclePriceAssetDirty(oracle, index)
}

// markOraclePriceAssetDirty records the asset behind an oracle price index as
// price-affected this ledger (the backstop dirty finalize joins it to BLND/USDC
// legs). An index the oracle's asset map does not resolve marks nothing — an
// unknown binding is explicit absence, never a guessed join.
func (b *blendStateBuilder) markOraclePriceAssetDirty(oracle *oracleBuilder, index int64) {
	for assetID, assetIndex := range oracle.assetToIndex {
		if assetIndex == index {
			b.changedPriceAssets[assetID] = struct{}{}
			return
		}
	}
}

// applyAssetInstance decodes a registered token contract's contract-instance
// entry into human-readable {symbol, name, decimals}. Like a Blend pool's
// Config/Backstop (applyPoolInstanceStorage), a token's AssetInfo/METADATA live
// INSIDE the instance's storage map, not as top-level contract_data entries —
// so this reads instance.Storage the same way. It recognizes two layouts, keyed
// within that map: a Stellar Asset Contract's AssetInfo (key
// vec[Symbol("AssetInfo")], value Native/AlphaNum4/AlphaNum12) and a SEP-41
// token's METADATA (bare key Symbol("METADATA"), value {decimal, name,
// symbol}). A change whose value is not instance-shaped (a registered asset
// contract also emits Balance/Allowance persistent entries) or whose storage
// matches neither layout decodes to nothing: an absent symbol stays absent, it
// is never guessed.
func (b *blendStateBuilder) applyAssetInstance(contractID string, value xdr.ScVal) {
	instance, ok := value.GetInstance()
	if !ok || instance.Storage == nil {
		return
	}
	for _, entry := range []xdr.ScMapEntry(*instance.Storage) {
		if variant, args, ok := scVariant(entry.Key); ok && variant == "AssetInfo" && len(args) == 0 {
			if meta, ok := decodeSACAssetInfo(contractID, entry.Val); ok {
				b.assets[contractID] = meta
			}
			continue
		}
		if sym, ok := scSymbol(entry.Key); ok && sym == "METADATA" {
			if meta, ok := decodeSEP41Metadata(contractID, entry.Val); ok {
				b.assets[contractID] = meta
			}
			continue
		}
	}
}

// decodeSACAssetInfo decodes a Stellar Asset Contract's AssetInfo instance
// value, matching soroban-env-host's stellar_asset_contract AssetInfo enum:
// Native, AlphaNum4{asset_code, issuer}, AlphaNum12{asset_code, issuer}. A
// SAC's decimals is always 7 — the classic Stellar asset convention the
// contract enforces, not a value stored on-chain.
func decodeSACAssetInfo(contractID string, value xdr.ScVal) (contracts.AssetMetadata, bool) {
	variant, args, ok := scVariant(value)
	if !ok {
		return contracts.AssetMetadata{}, false
	}
	switch variant {
	case "Native":
		return contracts.AssetMetadata{ContractID: contractID, Symbol: "native", Name: "native", Decimals: 7}, true
	case "AlphaNum4", "AlphaNum12":
		if len(args) == 0 {
			return contracts.AssetMetadata{}, false
		}
		fields := scMapFields(args[0])
		code, ok := scSymbol(fields["asset_code"])
		if !ok || code == "" {
			return contracts.AssetMetadata{}, false
		}
		issuer, ok := scValBytes(fields["issuer"])
		if !ok {
			return contracts.AssetMetadata{}, false
		}
		issuerAddr, err := strkey.Encode(strkey.VersionByteAccountID, issuer)
		if err != nil {
			return contracts.AssetMetadata{}, false
		}
		return contracts.AssetMetadata{
			ContractID: contractID,
			Symbol:     code,
			Name:       code + ":" + issuerAddr,
			Decimals:   7,
		}, true
	default:
		return contracts.AssetMetadata{}, false
	}
}

// decodeSEP41Metadata decodes a custom SEP-41 token's METADATA instance value
// (soroban-token-sdk TokenMetadata: {decimal, name, symbol}) into
// {symbol, name, decimals}.
func decodeSEP41Metadata(contractID string, value xdr.ScVal) (contracts.AssetMetadata, bool) {
	fields := scMapFields(value)
	if fields == nil {
		return contracts.AssetMetadata{}, false
	}
	decimals, ok := fieldInt32(fields, "decimal")
	if !ok {
		return contracts.AssetMetadata{}, false
	}
	name, ok := scSymbol(fields["name"])
	if !ok {
		return contracts.AssetMetadata{}, false
	}
	symbol, ok := scSymbol(fields["symbol"])
	if !ok {
		return contracts.AssetMetadata{}, false
	}
	return contracts.AssetMetadata{ContractID: contractID, Symbol: symbol, Name: name, Decimals: decimals}, true
}

// resolveOraclePrices threads each oracle's decoded prices onto the reserves
// that reference it, matching a reserve to its price by the asset's index in its
// pool's oracle. The oracle's asset->index map and prices are carried across
// ledgers (see LedgerState.Oracles), so this is the single source of a reserve's
// price every ledger: a price set on an earlier ledger is still in the carried
// priceByIndex and re-applied here. When the oracle knows the reserve's index
// but holds no positive price for it — never set, evicted, TTL-lapsed, or
// non-positive (the contract rejects price <= 0 on-chain) — the reserve's price
// is cleared so the stale value cannot linger and the downstream USD value and
// health factor surface as unavailable. An asset the oracle does not list is
// left untouched.
func (b *blendStateBuilder) resolveOraclePrices() {
	for _, pool := range b.pools {
		oracle := b.oracles[pool.state.OracleContract]
		if oracle == nil {
			continue
		}
		for assetID, reserve := range pool.reserves {
			index, ok := oracle.assetToIndex[assetID]
			if !ok {
				continue
			}
			priceRaw, ok := oracle.priceByIndex[index]
			if ok && isPositiveIntString(priceRaw) {
				reserve.state.OraclePriceRaw = priceRaw
				reserve.state.OracleDecimals = oracle.decimals
			} else {
				reserve.state.OraclePriceRaw = ""
				reserve.state.OracleDecimals = 0
			}
		}
	}
}

// isPositiveIntString reports whether a base-10 integer string is greater than
// zero. The price is widened through big.Int so a 128-bit value can never
// overflow the comparison.
func isPositiveIntString(s string) bool {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return false
	}
	return n.Sign() > 0
}

// finalizePoolReserves rebuilds reserveByIndex from scratch and sorts the pool's
// reserves by (ReserveIndex, AssetID) so reserve ordering is index-stable and
// identical run to run.
func finalizePoolReserves(pool *poolBuilder) {
	for assetID, reserve := range pool.reserves {
		if reserve.state.AssetID == "" {
			reserve.state.AssetID = assetID
		}
	}
	reserves := make([]contracts.ReserveState, 0, len(pool.reserves))
	for _, reserve := range pool.reserves {
		reserves = append(reserves, reserve.state)
	}
	sort.Slice(reserves, func(i, j int) bool {
		if reserves[i].ReserveIndex != reserves[j].ReserveIndex {
			return reserves[i].ReserveIndex < reserves[j].ReserveIndex
		}
		return reserves[i].AssetID < reserves[j].AssetID
	})
	pool.state.Reserves = reserves

	// reserveByIndex is derived from the just-sorted slice, not a second raw
	// range over pool.reserves, so the published mapping is reproducible run to
	// run (surfaced by markPoolRemapDirty's prior-vs-current comparison flaking
	// in the parity suite when it was built from Go's randomized map iteration).
	//
	// Only KNOWN, UNIQUE indexes are published. An unknown index (a reserve
	// materialized by ResData alone, still at its zero-value default) must never
	// masquerade as the real index 0 — that collision misattributed every
	// position leg resolved through it (lidapters#33). A duplicate known index
	// is equally unsafe: choosing a deterministic winner still misattributes, so
	// the index is left unresolved and its claiming assets are recorded as
	// ambiguity candidates instead.
	knownCount := map[int32]int{}
	for _, reserve := range reserves {
		if reserve.ReserveIndexKnown {
			knownCount[reserve.ReserveIndex]++
		}
	}
	pool.reserveByIndex = map[int32]string{}
	pool.ambiguousByIndex = map[int32][]string{}
	pool.unknownIndexAssets = nil
	for _, reserve := range reserves {
		if !reserve.ReserveIndexKnown {
			pool.unknownIndexAssets = append(pool.unknownIndexAssets, reserve.AssetID)
			continue
		}
		if knownCount[reserve.ReserveIndex] == 1 {
			pool.reserveByIndex[reserve.ReserveIndex] = reserve.AssetID
		} else {
			pool.ambiguousByIndex[reserve.ReserveIndex] = append(pool.ambiguousByIndex[reserve.ReserveIndex], reserve.AssetID)
		}
	}
	sort.Strings(pool.unknownIndexAssets)
}

// publishedReserveIndexes derives a pool's reserveByIndex mapping (known,
// unique ReserveIndex -> AssetID) from its typed reserve slice — the same rule
// finalizePoolReserves applies to the builder mirror, so a prior LedgerState
// and a rebuilt builder compare equal when their effective mappings match.
func publishedReserveIndexes(reserves []contracts.ReserveState) map[int32]string {
	knownCount := map[int32]int{}
	for _, reserve := range reserves {
		if reserve.ReserveIndexKnown {
			knownCount[reserve.ReserveIndex]++
		}
	}
	out := make(map[int32]string, len(knownCount))
	for _, reserve := range reserves {
		if reserve.ReserveIndexKnown && knownCount[reserve.ReserveIndex] == 1 {
			out[reserve.ReserveIndex] = reserve.AssetID
		}
	}
	return out
}

// positionSkipSink collects one bindings.DecodeDiagnostic per non-zero
// position leg positionsFromMap skips as unresolvable. It is nil on every path
// that recomputes positions for a purpose other than this ledger's folded
// output (silver-debug deltas, the incremental strategy's carried-cache
// rebuilds), so diagnostics are recorded exactly once per skipped leg per fold
// and only for the (address, pool) pairs the ledger actually touched.
type positionSkipSink struct {
	ledgerSeq int64
	out       *[]bindings.DecodeDiagnostic
}

// skip records the skipped leg. A duplicate known index reports the claiming
// assets; anything else is an unmapped index and reports the pool's
// unknown-index reserves as the candidates (the leg's true owner is one of
// them, but which is unknowable until its ResConfig folds).
func (s *positionSkipSink) skip(pool *poolBuilder, pending pendingUserPositions, kind contracts.PositionType, index int32, amount string) {
	diag := bindings.DecodeDiagnostic{
		LedgerSeq:      s.ledgerSeq,
		PoolContractID: pending.poolContract,
		Address:        pending.user,
		PositionType:   string(kind),
		ReserveIndex:   index,
		AmountRaw:      amount,
	}
	if candidates, duplicate := pool.ambiguousByIndex[index]; duplicate {
		diag.Code = bindings.DecodeDiagnosticDuplicateReserveIndex
		diag.CandidateAssetIDs = append([]string(nil), candidates...)
	} else {
		diag.Code = bindings.DecodeDiagnosticUnmappedReserveIndex
		diag.CandidateAssetIDs = append([]string(nil), pool.unknownIndexAssets...)
	}
	*s.out = append(*s.out, diag)
}

func positionsFromMap(pool *poolBuilder, pending pendingUserPositions, value xdr.ScVal, kind contracts.PositionType, sink *positionSkipSink) []contracts.UserReservePosition {
	raw, ok := value.GetMap()
	if !ok || raw == nil {
		return nil
	}
	out := make([]contracts.UserReservePosition, 0, len(*raw))
	for _, entry := range *raw {
		index, ok := scInt32(entry.Key)
		if !ok {
			continue
		}
		amount, ok := scIntString(entry.Val)
		if !ok || amount == "0" {
			continue
		}
		// Resolve only through the published known-unique mapping. An
		// unresolvable non-zero leg is skipped — never guessed through the
		// zero-value default or a duplicate — and surfaced as a diagnostic so a
		// bounded replay that cannot attribute it is visibly incomplete.
		assetID, resolved := pool.reserveByIndex[index]
		if !resolved {
			if sink != nil {
				sink.skip(pool, pending, kind, index, amount)
			}
			continue
		}
		pos := contracts.UserReservePosition{
			Address:           pending.user,
			PoolContractID:    pending.poolContract,
			AssetID:           assetID,
			PositionType:      kind,
			Archived:          pending.archived,
			ArchivedLedgerSeq: pending.archivedLedgerSeq,
		}
		if kind == contracts.PositionTypeLiability {
			pos.DTokensRaw = amount
		} else {
			pos.BTokensRaw = amount
		}
		out = append(out, pos)
	}
	return out
}

// sortDecodeDiagnostics orders a fold's diagnostics by their stable
// total-order key so the exposed set is byte-identical run to run regardless
// of the map-iteration order the legs were collected in.
func sortDecodeDiagnostics(diags []bindings.DecodeDiagnostic) {
	sort.SliceStable(diags, func(i, j int) bool {
		di, dj := diags[i], diags[j]
		if di.Code != dj.Code {
			return di.Code < dj.Code
		}
		if di.PoolContractID != dj.PoolContractID {
			return di.PoolContractID < dj.PoolContractID
		}
		if di.Address != dj.Address {
			return di.Address < dj.Address
		}
		if di.PositionType != dj.PositionType {
			return di.PositionType < dj.PositionType
		}
		if di.ReserveIndex != dj.ReserveIndex {
			return di.ReserveIndex < dj.ReserveIndex
		}
		if di.AmountRaw != dj.AmountRaw {
			return di.AmountRaw < dj.AmountRaw
		}
		return strings.Join(di.CandidateAssetIDs, "|") < strings.Join(dj.CandidateAssetIDs, "|")
	})
}

func decodeBackstopPoolBalance(poolID string, value xdr.ScVal) backstopPoolBalance {
	fields := scMapFields(value)
	balance := backstopPoolBalance{poolContract: poolID}
	if shares, ok := fieldIntString(fields, "shares"); ok {
		balance.sharesRaw = shares
	}
	if tokens, ok := fieldIntString(fields, "tokens"); ok {
		balance.tokensRaw = tokens
	}
	if q4w, ok := fieldIntString(fields, "q4w"); ok {
		balance.q4wRaw = q4w
	}
	return balance
}

func decodeBackstopUserBalance(poolID, user string, value xdr.ScVal) backstopUserBalance {
	fields := scMapFields(value)
	balance := backstopUserBalance{poolContract: poolID, user: user}
	if shares, ok := fieldIntString(fields, "shares"); ok {
		balance.sharesRaw = shares
	}
	items, ok := scVec(fields["q4w"])
	if !ok {
		return balance
	}
	for _, item := range items {
		qFields := scMapFields(item)
		amount, amountOK := fieldIntString(qFields, "amount")
		exp, expOK := fieldIntString(qFields, "exp")
		if !amountOK || !expOK {
			continue
		}
		expUnix, ok := scStringInt64(exp)
		if !ok {
			continue
		}
		balance.q4w = append(balance.q4w, contracts.Q4WEntry{
			SharesRaw: amount,
			UnlockAt:  time.Unix(expUnix, 0).UTC(),
		})
	}
	return balance
}

// ensureBackstopInstance returns (creating if needed) the carried identity
// entry for a backstop contract.
func (b *blendStateBuilder) ensureBackstopInstance(contractID string) *contracts.BackstopInstanceState {
	instance, ok := b.backstopInstances[contractID]
	if !ok {
		instance = &contracts.BackstopInstanceState{ContractID: contractID}
		b.backstopInstances[contractID] = instance
	}
	return instance
}

// applyBackstopInstance decodes a backstop contract's instance-storage
// addresses (BToken, BLNDTkn, USDCTkn, Emitter, PoolFact). It returns true
// only when the instance actually looks like a backstop (it carries a BToken
// entry — no pool, oracle or aggregator instance has one), so every other
// contract instance still falls through to its own handling. Each address is
// independently optional; RZ/DropList arrive as top-level entries and are
// merged onto the same carried identity elsewhere.
func (b *blendStateBuilder) applyBackstopInstance(contractID string, value xdr.ScVal) bool {
	instance, ok := value.GetInstance()
	if !ok || instance.Storage == nil {
		return false
	}
	storage := map[string]xdr.ScVal{}
	for _, entry := range []xdr.ScMapEntry(*instance.Storage) {
		if name, ok := scSymbol(entry.Key); ok {
			storage[name] = entry.Val
		}
	}
	if _, isBackstop := storage["BToken"]; !isBackstop {
		return false
	}
	state := b.ensureBackstopInstance(contractID)
	if address, ok := scAddress(storage["BToken"]); ok {
		state.BackstopToken = address
	}
	if address, ok := scAddress(storage["BLNDTkn"]); ok {
		state.BLNDToken = address
	}
	if address, ok := scAddress(storage["USDCTkn"]); ok {
		state.USDCToken = address
	}
	if address, ok := scAddress(storage["Emitter"]); ok {
		state.Emitter = address
	}
	if address, ok := scAddress(storage["PoolFact"]); ok {
		state.PoolFactory = address
	}
	return true
}

// decodeAddressList decodes a Vec<Address> (the backstop's RZ reward zone).
// Any non-address element rejects the whole list so a partial membership set
// is never surfaced.
func decodeAddressList(value xdr.ScVal) ([]string, bool) {
	items, ok := scVec(value)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		address, ok := scAddress(item)
		if !ok {
			return nil, false
		}
		out = append(out, address)
	}
	return out, true
}

// decodeDropList decodes the backstop's DropList Vec<(Address, i128)>. Any
// malformed pair rejects the whole list.
func decodeDropList(value xdr.ScVal) ([]contracts.DropListEntry, bool) {
	items, ok := scVec(value)
	if !ok {
		return nil, false
	}
	out := make([]contracts.DropListEntry, 0, len(items))
	for _, item := range items {
		pair, ok := scVec(item)
		if !ok || len(pair) < 2 {
			return nil, false
		}
		address, ok := scAddress(pair[0])
		if !ok {
			return nil, false
		}
		amount, ok := scIntString(pair[1])
		if !ok {
			return nil, false
		}
		out = append(out, contracts.DropListEntry{Address: address, AmountRaw: amount})
	}
	return out, true
}

// decodePoolEmissions decodes the v2 PoolEmis Map<u32 res_token_id, u64
// share>. Entries are sorted by reserve token ID; any malformed entry rejects
// the whole map (a partial split would misattribute emissions). A v1-shaped
// PoolEmissionConfig ({config, last_time} symbol map) has no u32 keys and is
// rejected the same way.
func decodePoolEmissions(value xdr.ScVal) ([]contracts.PoolEmissionEntry, bool) {
	raw, ok := value.GetMap()
	if !ok || raw == nil {
		return nil, false
	}
	entries := make([]contracts.PoolEmissionEntry, 0, len(*raw))
	for _, entry := range *raw {
		resTokenID, ok := scInt32(entry.Key)
		if !ok || resTokenID < 0 {
			return nil, false
		}
		share, ok := scIntString(entry.Val)
		if !ok {
			return nil, false
		}
		entries = append(entries, contracts.PoolEmissionEntry{ReserveTokenID: resTokenID, ShareRaw: share})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ReserveTokenID < entries[j].ReserveTokenID })
	return entries, true
}

// decodeQueuedReserve decodes a ResInit(Address) value (QueuedReserveInit
// {new_config, unlock_time}). unlock_time and a map-shaped new_config are
// required — a queued change without either is malformed and skipped whole;
// the individual config fields stay independently optional.
func decodeQueuedReserve(poolID, asset string, value xdr.ScVal) (contracts.QueuedReserveState, bool) {
	fields := scMapFields(value)
	unlockTime, unlockOK := fieldIntString(fields, "unlock_time")
	config := scMapFields(fields["new_config"])
	if !unlockOK || config == nil {
		return contracts.QueuedReserveState{}, false
	}
	queued := contracts.QueuedReserveState{
		PoolContractID: poolID,
		AssetID:        asset,
		UnlockTimeRaw:  unlockTime,
	}
	set := func(dst *string, name string) {
		if v, ok := fieldIntString(config, name); ok {
			*dst = v
		}
	}
	set(&queued.NewConfig.IndexRaw, "index")
	set(&queued.NewConfig.DecimalsRaw, "decimals")
	set(&queued.NewConfig.CFactorRaw, "c_factor")
	set(&queued.NewConfig.LFactorRaw, "l_factor")
	set(&queued.NewConfig.UtilRaw, "util")
	set(&queued.NewConfig.MaxUtilRaw, "max_util")
	set(&queued.NewConfig.RBaseRaw, "r_base")
	set(&queued.NewConfig.ROneRaw, "r_one")
	set(&queued.NewConfig.RTwoRaw, "r_two")
	set(&queued.NewConfig.RThreeRaw, "r_three")
	set(&queued.NewConfig.ReactivityRaw, "reactivity")
	set(&queued.NewConfig.SupplyCapRaw, "supply_cap")
	if enabled, ok := fieldBool(config, "enabled"); ok {
		queued.NewConfig.Enabled = boolString(enabled)
	}
	return queued, true
}

// decodeBackstopEmisData decodes a BEmisData(pool) value (BackstopEmissionData
// {expiration, eps, index, last_time}). Each field is independently optional:
// only the fields present on-chain are set, absent ones stay "". A value that
// yields no field at all (wrong ScVal shape) reports !ok so the caller keeps
// the carried entry instead of wiping it.
func decodeBackstopEmisData(poolID string, value xdr.ScVal) (backstopEmisData, bool) {
	fields := scMapFields(value)
	emis := backstopEmisData{poolContract: poolID}
	if eps, ok := fieldIntString(fields, "eps"); ok {
		emis.epsRaw = eps
	}
	if expiration, ok := fieldIntString(fields, "expiration"); ok {
		emis.expirationRaw = expiration
	}
	if index, ok := fieldIntString(fields, "index"); ok {
		emis.indexRaw = index
	}
	if lastTime, ok := fieldIntString(fields, "last_time"); ok {
		emis.lastTimeRaw = lastTime
	}
	return emis, !emis.empty()
}

// decodeUserEmissionData decodes a UserEmissionData {index, accrued} value
// (shared by the pool's UserEmis and the backstop's UEmisData). Both fields
// are required — the contract always writes both, so a value missing either
// is malformed and skipped whole rather than half-written.
func decodeUserEmissionData(value xdr.ScVal) (indexRaw, accruedRaw string, ok bool) {
	fields := scMapFields(value)
	indexRaw, indexOK := fieldIntString(fields, "index")
	accruedRaw, accruedOK := fieldIntString(fields, "accrued")
	if !indexOK || !accruedOK {
		return "", "", false
	}
	return indexRaw, accruedRaw, true
}

// decodeAuctionState decodes an Auction(AuctionKey) value (AuctionData {bid,
// lot, block}). All three fields are required and every lot/bid entry must
// decode — a partial auction would under-report a liquidation's lot or bid,
// so any malformed piece rejects the whole value (the caller then keeps the
// carried state untouched). Lot/bid may legitimately be empty maps.
func decodeAuctionState(poolID, user string, auctType int32, value xdr.ScVal) (contracts.AuctionState, bool) {
	fields := scMapFields(value)
	if fields == nil {
		return contracts.AuctionState{}, false
	}
	blockVal, hasBlock := fields["block"]
	block, blockOK := scInt64(blockVal)
	lot, lotOK := decodeAuctionEntries(fields["lot"])
	bid, bidOK := decodeAuctionEntries(fields["bid"])
	if !hasBlock || !blockOK || !lotOK || !bidOK {
		return contracts.AuctionState{}, false
	}
	return contracts.AuctionState{
		PoolContractID: poolID,
		UserAddress:    user,
		AuctionType:    auctType,
		Block:          block,
		Lot:            lot,
		Bid:            bid,
	}, true
}

// decodeAuctionEntries decodes one lot/bid Map<Address, i128> into entries
// sorted by asset. Any entry that fails to decode rejects the whole map.
func decodeAuctionEntries(value xdr.ScVal) ([]contracts.AuctionEntry, bool) {
	raw, ok := value.GetMap()
	if !ok || raw == nil {
		return nil, false
	}
	entries := make([]contracts.AuctionEntry, 0, len(*raw))
	for _, entry := range *raw {
		asset, ok := scAddress(entry.Key)
		if !ok {
			return nil, false
		}
		amount, ok := scIntString(entry.Val)
		if !ok {
			return nil, false
		}
		entries = append(entries, contracts.AuctionEntry{AssetID: asset, AmountRaw: amount})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].AssetID < entries[j].AssetID })
	return entries, true
}

// auctionKeyParts reads an AuctionKey variant payload ({user, auct_type} as a
// struct map, or the positional two-arg form some encoders emit).
func auctionKeyParts(args []xdr.ScVal) (string, int32, bool) {
	if len(args) == 1 {
		fields := scMapFields(args[0])
		user, userOK := fieldAddress(fields, "user")
		auctType, typeOK := fieldInt32(fields, "auct_type")
		return user, auctType, userOK && typeOK
	}
	if len(args) >= 2 {
		user, userOK := scAddress(args[0])
		auctType, typeOK := scInt32(args[1])
		if !userOK || !typeOK {
			// Tolerate either argument order — the key is a struct on-chain, so
			// positional encoders may emit (type, user) as well as (user, type).
			user, userOK = scAddress(args[1])
			auctType, typeOK = scInt32(args[0])
		}
		return user, auctType, userOK && typeOK
	}
	return "", 0, false
}

// userReserveKeyParts reads a UserReserveKey variant payload ({user,
// reserve_id} as a struct map, or the positional two-arg form).
func userReserveKeyParts(args []xdr.ScVal) (string, int32, bool) {
	if len(args) == 1 {
		fields := scMapFields(args[0])
		user, userOK := fieldAddress(fields, "user")
		resTokenID, idOK := fieldInt32(fields, "reserve_id")
		return user, resTokenID, userOK && idOK
	}
	if len(args) >= 2 {
		user, userOK := scAddress(args[0])
		resTokenID, idOK := scInt32(args[1])
		if !userOK || !idOK {
			user, userOK = scAddress(args[1])
			resTokenID, idOK = scInt32(args[0])
		}
		return user, resTokenID, userOK && idOK
	}
	return "", 0, false
}

func variantAddress(args []xdr.ScVal) (string, bool) {
	if len(args) == 0 {
		return "", false
	}
	return scAddress(args[0])
}

// variantU32 reads a variant's sole u32 argument — the shape EmisConfig(u32)
// and EmisData(u32) use to key by res_token_id.
func variantU32(args []xdr.ScVal) (uint32, bool) {
	if len(args) == 0 {
		return 0, false
	}
	v, ok := scInt32(args[0])
	if !ok || v < 0 {
		return 0, false
	}
	return uint32(v), true
}

func backstopPoolUser(args []xdr.ScVal) (string, string, bool) {
	if len(args) == 1 {
		fields := scMapFields(args[0])
		poolID, poolOK := fieldAddress(fields, "pool")
		user, userOK := fieldAddress(fields, "user")
		return poolID, user, poolOK && userOK
	}
	if len(args) >= 2 {
		poolID, poolOK := scAddress(args[0])
		user, userOK := scAddress(args[1])
		return poolID, user, poolOK && userOK
	}
	return "", "", false
}

func contractInstanceWasmHash(value xdr.ScVal) (string, bool) {
	instance, ok := value.GetInstance()
	if !ok {
		return "", false
	}
	wasmHash, ok := instance.Executable.GetWasmHash()
	if !ok {
		return "", false
	}
	return xdr.Hash(wasmHash).HexString(), true
}

func blendPoolStatus(status int32) string {
	switch status {
	case 0:
		return "admin_active"
	case 1:
		return "active"
	case 2:
		return "admin_on_ice"
	case 3:
		return "on_ice"
	case 4:
		return "admin_frozen"
	case 5:
		return "frozen"
	case 6:
		return "setup"
	default:
		return "unknown"
	}
}

func scStringInt64(raw string) (int64, bool) {
	var value int64
	for _, ch := range raw {
		if ch < '0' || ch > '9' {
			return 0, false
		}
		value = value*10 + int64(ch-'0')
	}
	return value, true
}

// --- scval helpers (state-decode flavor) -----------------------------------
// These complement the event-decode helpers in decode.go (the canonical scval
// home for this adapter); the (string, bool) shapes below thinly wrap decode.go
// (scValAddress / scValSymbol / int128ToString / uint128ToString) so there is a
// single decode implementation, not the relay's duplicated copies.

func scSymbol(v xdr.ScVal) (string, bool) {
	s := scValSymbol(v)
	return s, s != ""
}

func scAddress(v xdr.ScVal) (string, bool) {
	s := scValAddress(v)
	return s, s != ""
}

// scValBytes extracts raw bytes from an ScvBytes value — used for a SAC
// AlphaNum4/12 AssetInfo's issuer field, which is a BytesN<32> (the issuer
// account's raw Ed25519 public key), not an ScAddress.
func scValBytes(v xdr.ScVal) ([]byte, bool) {
	b, ok := v.GetBytes()
	if !ok {
		return nil, false
	}
	return []byte(b), true
}

func scInt64(v xdr.ScVal) (int64, bool) {
	switch v.Type {
	case xdr.ScValTypeScvU32:
		value, ok := v.GetU32()
		return int64(value), ok
	case xdr.ScValTypeScvI32:
		value, ok := v.GetI32()
		return int64(value), ok
	case xdr.ScValTypeScvU64:
		value, ok := v.GetU64()
		if !ok || uint64(value) > uint64(^uint64(0)>>1) {
			return 0, false
		}
		return int64(value), true
	case xdr.ScValTypeScvI64:
		value, ok := v.GetI64()
		return int64(value), ok
	case xdr.ScValTypeScvU128, xdr.ScValTypeScvI128:
		value, ok := scIntString(v)
		if !ok {
			return 0, false
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func scInt32(v xdr.ScVal) (int32, bool) {
	value, ok := scInt64(v)
	if !ok || value < -2147483648 || value > 2147483647 {
		return 0, false
	}
	return int32(value), true
}

func scIntString(v xdr.ScVal) (string, bool) {
	switch v.Type {
	case xdr.ScValTypeScvU32:
		value, ok := v.GetU32()
		if !ok {
			return "", false
		}
		return strconv.FormatUint(uint64(value), 10), true
	case xdr.ScValTypeScvI32:
		value, ok := v.GetI32()
		if !ok {
			return "", false
		}
		return strconv.FormatInt(int64(value), 10), true
	case xdr.ScValTypeScvU64, xdr.ScValTypeScvTimepoint, xdr.ScValTypeScvDuration:
		var value uint64
		var ok bool
		if v.Type == xdr.ScValTypeScvTimepoint {
			raw, found := v.GetTimepoint()
			value, ok = uint64(raw), found
		} else if v.Type == xdr.ScValTypeScvDuration {
			raw, found := v.GetDuration()
			value, ok = uint64(raw), found
		} else {
			raw, found := v.GetU64()
			value, ok = uint64(raw), found
		}
		if !ok {
			return "", false
		}
		return strconv.FormatUint(value, 10), true
	case xdr.ScValTypeScvI64:
		value, ok := v.GetI64()
		if !ok {
			return "", false
		}
		return strconv.FormatInt(int64(value), 10), true
	case xdr.ScValTypeScvU128:
		value, ok := v.GetU128()
		if !ok {
			return "", false
		}
		return uint128ToString(value), true
	case xdr.ScValTypeScvI128:
		value, ok := v.GetI128()
		if !ok {
			return "", false
		}
		return int128ToString(value), true
	default:
		return "", false
	}
}

func scMapFields(v xdr.ScVal) map[string]xdr.ScVal {
	raw, ok := v.GetMap()
	if !ok || raw == nil {
		return nil
	}
	out := map[string]xdr.ScVal{}
	for _, entry := range *raw {
		name, ok := scSymbol(entry.Key)
		if !ok {
			continue
		}
		out[name] = entry.Val
	}
	return out
}

func scVariant(v xdr.ScVal) (string, []xdr.ScVal, bool) {
	vec, ok := v.GetVec()
	if !ok || vec == nil || len(*vec) == 0 {
		return "", nil, false
	}
	name, ok := scSymbol((*vec)[0])
	if !ok {
		return "", nil, false
	}
	return name, []xdr.ScVal((*vec)[1:]), true
}

func scVec(v xdr.ScVal) ([]xdr.ScVal, bool) {
	vec, ok := v.GetVec()
	if !ok || vec == nil {
		return nil, false
	}
	return []xdr.ScVal(*vec), true
}

func fieldAddress(fields map[string]xdr.ScVal, name string) (string, bool) {
	if fields == nil {
		return "", false
	}
	return scAddress(fields[name])
}

func fieldIntString(fields map[string]xdr.ScVal, name string) (string, bool) {
	if fields == nil {
		return "", false
	}
	return scIntString(fields[name])
}

func fieldInt32(fields map[string]xdr.ScVal, name string) (int32, bool) {
	if fields == nil {
		return 0, false
	}
	return scInt32(fields[name])
}

func fieldBool(fields map[string]xdr.ScVal, name string) (bool, bool) {
	if fields == nil {
		return false, false
	}
	v, present := fields[name]
	if !present || v.Type != xdr.ScValTypeScvBool || v.B == nil {
		return false, false
	}
	return *v.B, true
}
