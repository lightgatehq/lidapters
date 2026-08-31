// Comet LP (BToken) state folding: the deterministic, fold-owned decode of the
// Comet pool contract behind each Blend backstop (lidapters#31, V1-09 D-02/D-03).
//
// The layout is pinned to CometDEX/comet-contracts-v1 @ ef4cbfad0a35202ad267c14d163d2f362995a8d3
// (contracts/src/c_pool/storage_types.rs): three SEPARATE persistent entries
// keyed by the DataKey enum's unit variants —
//
//	DataKey::AllTokenVec   -> Vec<Address>                  (token identities)
//	DataKey::AllRecordData -> Map<Address, Record>          (Record{balance,weight,scalar,index})
//	DataKey::TotalShares   -> i128                          (LP supply denominator)
//
// A unit enum variant serializes as a one-element Vec[Symbol], so these keys
// decode through the same scVariant helper as the Blend pool's own keys. A
// registered Comet's contract_data is routed ONLY here (see apply/applyDelete
// in state.go): it must never fall through to the Blend pool decoder, and Blend
// pool state must never decode as Comet — ownership is by configured contract
// address, never by key shape.
//
// Absent-not-zero (D-09): a key that has never folded leaves its facet absent
// ("" / nil), a real stored zero stays "0", and a malformed write is rejected
// whole (the carried state survives) rather than half-decoded. Component
// identity is matched by token address only — AllTokenVec order and
// AllRecordData map order are decoded but never used for semantic matching, so
// a reordered token vector yields byte-identical economic output.
package blend

import (
	"sort"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/lightgatehq/lidapters/blend/contracts"
	"github.com/shopspring/decimal"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// cometPoolType is the AMMPoolState.PoolType marker for a folded Comet pool, so
// generic AMM state carries Comet data without being confusable with an
// Aquarius constant_product/stable/concentrated pool (D-03).
const cometPoolType = "comet"

// cometPoolBuilder accumulates one registered Comet contract's decoded facets.
// Each facet tracks its own known-ness: the fold can observe TotalShares before
// AllRecordData (or never), and each must round-trip independently.
type cometPoolBuilder struct {
	contractID string
	wasmHash   string
	// tokens is the decoded AllTokenVec (order preserved from the wire, never
	// matched on). tokenKnown distinguishes "vec folded, empty" from "vec never
	// observed".
	tokens     []string
	tokenKnown bool
	// records is the decoded AllRecordData, keyed by token address. Record
	// weight/scalar/index are validated present on decode (a wrong or partial
	// layout rejects the write) but only balance feeds valuation; the index is
	// not carried into output state (no V1-09 consumer — see PR notes).
	records      map[string]cometRecord
	recordsKnown bool
	// totalSharesRaw is TotalShares verbatim; "" when never observed, "0" for a
	// real stored zero (which cannot value anything downstream).
	totalSharesRaw   string
	totalSharesKnown bool
}

type cometRecord struct {
	balanceRaw string
	index      int32
}

// reserveOf returns the token's folded Comet reserve (Record.balance), "" when
// the record map never folded or the token is absent from it. A present zero
// balance stays "0" — an observed zero, not an absence.
func (c *cometPoolBuilder) reserveOf(assetID string) string {
	if assetID == "" || !c.recordsKnown {
		return ""
	}
	record, ok := c.records[assetID]
	if !ok {
		return ""
	}
	return record.balanceRaw
}

// lpSupplyRaw returns the folded LP supply denominator, "" when TotalShares
// never folded. A real stored zero stays "0" — present, but unusable as a
// denominator downstream (D-09: absent derived output, not silent zero).
func (c *cometPoolBuilder) lpSupplyRaw() string {
	if !c.totalSharesKnown {
		return ""
	}
	return c.totalSharesRaw
}

func (b *blendStateBuilder) ensureComet(contractID string) *cometPoolBuilder {
	comet, ok := b.comets[contractID]
	if !ok {
		comet = &cometPoolBuilder{
			contractID: contractID,
			records:    map[string]cometRecord{},
		}
		b.comets[contractID] = comet
	}
	return comet
}

// applyCometChange folds one live contract_data change on a registered Comet
// contract. Routed from apply() ahead of every Blend branch, so a Comet
// instance (which carries a wasm executable) can never be misdecoded as a
// phantom Blend pool.
func (b *blendStateBuilder) applyCometChange(change bindings.ContractDataChange, key, value xdr.ScVal, ledgerSeq int64) {
	if key.Type == xdr.ScValTypeScvLedgerKeyContractInstance {
		comet := b.ensureComet(change.ContractID)
		if wasmHash, ok := contractInstanceWasmHash(value); ok {
			comet.wasmHash = wasmHash
		}
		b.markCometDirty(change.ContractID)
		return
	}
	variant, args, ok := scVariant(key)
	if !ok || len(args) != 0 {
		// Not a Comet DataKey unit variant (LP-token Balance/Allowance entries,
		// etc.): absorbed, never decoded as Blend state.
		return
	}
	switch variant {
	case "AllTokenVec":
		tokens, ok := decodeCometTokenVec(value)
		if !ok {
			return
		}
		comet := b.ensureComet(change.ContractID)
		comet.tokens = tokens
		comet.tokenKnown = true
	case "AllRecordData":
		records, ok := decodeCometRecordData(value)
		if !ok {
			return
		}
		comet := b.ensureComet(change.ContractID)
		comet.records = records
		comet.recordsKnown = true
	case "TotalShares":
		totalShares, ok := scIntString(value)
		if !ok {
			return
		}
		comet := b.ensureComet(change.ContractID)
		comet.totalSharesRaw = totalShares
		comet.totalSharesKnown = true
	default:
		// Factory/Controller/SwapFee/TokenShare/PublicSwap/Finalize/Freeze and
		// unknown variants carry no valuation input.
		return
	}
	b.markCometDirty(change.ContractID)
}

// applyCometDelete handles a not-live change on a registered Comet contract:
// the facet behind the lapsed/removed key goes absent (never zero), everything
// else is kept. Persistent Comet entries have ~31-day TTLs on-chain, so TTL
// lapse is a real hazard this must survive honestly.
func (b *blendStateBuilder) applyCometDelete(change bindings.ContractDataChange, key xdr.ScVal, ledgerSeq int64) {
	comet := b.comets[change.ContractID]
	if comet == nil {
		return
	}
	if key.Type == xdr.ScValTypeScvLedgerKeyContractInstance {
		comet.wasmHash = ""
		return
	}
	variant, args, ok := scVariant(key)
	if !ok || len(args) != 0 {
		return
	}
	switch variant {
	case "AllTokenVec":
		comet.tokens = nil
		comet.tokenKnown = false
	case "AllRecordData":
		comet.records = map[string]cometRecord{}
		comet.recordsKnown = false
	case "TotalShares":
		comet.totalSharesRaw = ""
		comet.totalSharesKnown = false
	default:
		return
	}
	b.markCometDirty(change.ContractID)
}

// decodeCometTokenVec decodes AllTokenVec's Vec<Address>. Any non-address
// element rejects the whole write — a half-decoded identity set is worse than
// the carried one.
func decodeCometTokenVec(value xdr.ScVal) ([]string, bool) {
	vec, ok := scVec(value)
	if !ok {
		return nil, false
	}
	tokens := make([]string, 0, len(vec))
	for _, item := range vec {
		address, ok := scAddress(item)
		if !ok {
			return nil, false
		}
		tokens = append(tokens, address)
	}
	return tokens, true
}

// decodeCometRecordData decodes AllRecordData's Map<Address, Record>. Every
// entry must carry the pinned Record layout (balance i128, weight, scalar,
// index u32); one malformed entry rejects the whole write.
func decodeCometRecordData(value xdr.ScVal) (map[string]cometRecord, bool) {
	raw, ok := value.GetMap()
	if !ok || raw == nil {
		return nil, false
	}
	records := make(map[string]cometRecord, len(*raw))
	for _, entry := range *raw {
		address, ok := scAddress(entry.Key)
		if !ok {
			return nil, false
		}
		fields := scMapFields(entry.Val)
		if fields == nil {
			return nil, false
		}
		balance, ok := fieldIntString(fields, "balance")
		if !ok {
			return nil, false
		}
		if _, ok := fieldIntString(fields, "weight"); !ok {
			return nil, false
		}
		if _, ok := fieldIntString(fields, "scalar"); !ok {
			return nil, false
		}
		index, ok := fieldInt32(fields, "index")
		if !ok {
			return nil, false
		}
		records[address] = cometRecord{balanceRaw: balance, index: index}
	}
	return records, true
}

// restoreComet rebuilds one cometPoolBuilder from its AMMPoolState carrier —
// the loadPrior half of the checkpoint round-trip. Only pools whose contract is
// a registered Comet are restored; anything else on AMMPools belongs to another
// adapter's state and is left alone.
func (b *blendStateBuilder) restoreComet(state bindings.AMMPoolState) {
	comet := b.ensureComet(state.ContractID)
	comet.wasmHash = state.WasmHash
	if state.Tokens != nil {
		comet.tokenKnown = true
		comet.tokens = make([]string, 0, len(state.Tokens))
		for _, token := range state.Tokens {
			comet.tokens = append(comet.tokens, token.AssetID)
			if token.ReserveRaw != "" {
				comet.records[token.AssetID] = cometRecord{balanceRaw: token.ReserveRaw}
				comet.recordsKnown = true
			}
		}
		// A folded vec with a folded-but-empty record map round-trips as tokens
		// with empty reserves; mark the map known only when the carrier proves
		// it (a non-empty reserve). The empty-map/empty-vec case is
		// economically identical — no reserve can value — so the distinction is
		// safe to collapse here.
		if len(state.Tokens) == 0 {
			comet.recordsKnown = true
		}
	}
	if state.TotalSharesRaw != "" {
		comet.totalSharesRaw = state.TotalSharesRaw
		comet.totalSharesKnown = true
	}
}

// buildAMMPools snapshots the folded Comet pools into the protocol-neutral
// carrier, sorted by contract ID with tokens sorted by address so the run-twice
// and cross-strategy output is byte-identical. Records whose address fell out
// of the token vec (an unbind observed between the two writes) still emit —
// dropping them would lose a reserve the fold observed.
func (b *blendStateBuilder) buildAMMPools() []bindings.AMMPoolState {
	if len(b.comets) == 0 {
		return nil
	}
	pools := make([]bindings.AMMPoolState, 0, len(b.comets))
	for _, comet := range b.comets {
		pool := bindings.AMMPoolState{
			Protocol:   b.protocol,
			ContractID: comet.contractID,
			WasmHash:   comet.wasmHash,
			PoolType:   cometPoolType,
		}
		if comet.tokenKnown || comet.recordsKnown {
			pool.Tokens = make([]bindings.AMMTokenReserve, 0, len(comet.tokens)+len(comet.records))
			seen := make(map[string]struct{}, len(comet.tokens)+len(comet.records))
			for _, token := range comet.tokens {
				seen[token] = struct{}{}
				pool.Tokens = append(pool.Tokens, bindings.AMMTokenReserve{
					AssetID:    token,
					ReserveRaw: comet.records[token].balanceRaw,
				})
			}
			recordExtras := make([]string, 0)
			for address := range comet.records {
				if _, ok := seen[address]; !ok {
					recordExtras = append(recordExtras, address)
				}
			}
			sort.Strings(recordExtras)
			for _, address := range recordExtras {
				pool.Tokens = append(pool.Tokens, bindings.AMMTokenReserve{
					AssetID:    address,
					ReserveRaw: comet.records[address].balanceRaw,
				})
			}
			sort.Slice(pool.Tokens, func(i, j int) bool { return pool.Tokens[i].AssetID < pool.Tokens[j].AssetID })
		}
		if comet.totalSharesKnown {
			pool.TotalSharesRaw = comet.totalSharesRaw
		}
		pools = append(pools, pool)
	}
	sort.Slice(pools, func(i, j int) bool { return pools[i].ContractID < pools[j].ContractID })
	return pools
}

// --- backstop linkage ---------------------------------------------------------

// backstopInstanceForPool resolves a pool's backstop contract to its decoded
// instance (the BToken/BLND/USDC identity carrier), nil when either half has
// not folded.
func (b *blendStateBuilder) backstopInstanceForPool(poolContract string) *contracts.BackstopInstanceState {
	pool := b.pools[poolContract]
	if pool == nil || pool.state.BackstopContract == "" {
		return nil
	}
	return b.backstopInstances[pool.state.BackstopContract]
}

// cometForBackstopInstance resolves the folded Comet pool behind a backstop
// instance's BToken, nil when the token is unregistered or unfoldable.
func (b *blendStateBuilder) cometForBackstopInstance(instance *contracts.BackstopInstanceState) *cometPoolBuilder {
	if instance == nil || instance.BackstopToken == "" {
		return nil
	}
	return b.comets[instance.BackstopToken]
}

// tokenPriceUSD binds one token contract ID to its folded USD price: the first
// pool reserve (in ascending pool-contract order, so the binding is
// deterministic) whose asset IS that token and whose oracle resolved a positive
// price this ledger. resolveOraclePrices/resolveAggregatorPrices have already
// run when build/snapshot calls this, so the price is ledger-pinned to the same
// committed LedgerState.Ledger as every other valuation input. There is no
// fallback: an unbound token (no folded reserve, no positive price) returns ""
// and every derived USD stays absent — never a hardcoded $1, never a stale or
// cross-ledger substitute (D-09).
func (b *blendStateBuilder) tokenPriceUSD(assetID string) string {
	if assetID == "" {
		return ""
	}
	poolIDs := make([]string, 0, len(b.pools))
	for poolID := range b.pools {
		poolIDs = append(poolIDs, poolID)
	}
	sort.Strings(poolIDs)
	for _, poolID := range poolIDs {
		reserve, ok := b.pools[poolID].reserves[assetID]
		if !ok || !isPositiveIntString(reserve.state.OraclePriceRaw) {
			continue
		}
		price := parseDecimalOrZero(reserve.state.OraclePriceRaw)
		return numString(price.Div(decimal.New(1, reserve.state.OracleDecimals)))
	}
	return ""
}

// --- affected-holder dirty set (D-10) -----------------------------------------

// markBackstopDirty records one (address, pool) backstop pair as invalidated
// this ledger. Kind is derived at finalize time from post-fold presence, the
// same rule finalizeDirtyPositions applies to lending pairs.
func (b *blendStateBuilder) markBackstopDirty(address, poolContract string) {
	if b.dirtyBackstops == nil {
		return
	}
	b.dirtyBackstops[typedBackstopEntityKey(address, poolContract)] = backstopIdentity{address: address, pool: poolContract}
}

// markBackstopPoolDirty invalidates every holder of one pool's backstop: a
// PoolBalance write moves the shares<->LP conversion for all of them.
func (b *blendStateBuilder) markBackstopPoolDirty(poolContract string) {
	for _, balance := range b.backstopUsers {
		if balance.poolContract == poolContract {
			b.markBackstopDirty(balance.user, poolContract)
		}
	}
}

// markCometDirty invalidates every backstop holder linked to one Comet: the
// pools whose backstop instance names this contract as its BToken. A Comet
// reserve/supply write legitimately touches every linked holder (D-10); it
// must not touch any lending position.
func (b *blendStateBuilder) markCometDirty(cometContract string) {
	for _, pool := range b.pools {
		instance := b.backstopInstanceForPool(pool.state.ContractID)
		if instance == nil || instance.BackstopToken != cometContract {
			continue
		}
		b.markBackstopPoolDirty(pool.state.ContractID)
	}
}

// markPriceAssetDirty invalidates every backstop holder whose BLND or USDC leg
// is the price-affected asset. Only the pools linked through a decoded backstop
// instance are touched; a price tick for any other asset marks nothing.
func (b *blendStateBuilder) markPriceAssetDirty(assetID string) {
	if assetID == "" {
		return
	}
	for _, instance := range b.backstopInstances {
		if instance.BLNDToken != assetID && instance.USDCToken != assetID {
			continue
		}
		for _, pool := range b.pools {
			if pool.state.BackstopContract == instance.ContractID {
				b.markBackstopPoolDirty(pool.state.ContractID)
			}
		}
	}
}

// propagateFeedPriceDirty maps this ledger's changed feeds through each
// aggregator's asset->feed wiring and invalidates the backstop holders priced
// by them. Aggregator prices are synthesized at build time, so the apply pass
// sees the feed write, not the price; this is the fold-time join between the
// two.
func (b *blendStateBuilder) propagateFeedPriceDirty() {
	if len(b.changedFeeds) == 0 {
		return
	}
	affected := map[string]struct{}{}
	for _, agg := range b.aggregators {
		for assetID, cfg := range agg.assets {
			feedRef, ok := agg.feeds[cfg.OracleIndex]
			if !ok {
				continue
			}
			if _, changed := b.changedFeeds[feedRef.ContractID]; changed {
				affected[assetID] = struct{}{}
			}
		}
	}
	for assetID := range affected {
		b.markPriceAssetDirty(assetID)
	}
}

// finalizeDirtyBackstops turns the builder's raw backstop dirty set into the
// exposed bindings.DirtyBackstop list, sorted by (address, pool) for
// byte-stable output. Kind mirrors finalizeDirtyPositions: present after the
// fold is an Upsert, purged is a Removal.
func finalizeDirtyBackstops(dirty map[string]backstopIdentity, balances map[string]backstopUserBalance) []bindings.DirtyBackstop {
	if len(dirty) == 0 {
		return nil
	}
	out := make([]bindings.DirtyBackstop, 0, len(dirty))
	for composite, identity := range dirty {
		kind := bindings.DirtyRemoval
		if _, ok := balances[composite]; ok {
			kind = bindings.DirtyUpsert
		}
		out = append(out, bindings.DirtyBackstop{Address: identity.address, PoolContractID: identity.pool, Kind: kind})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Address != out[j].Address {
			return out[i].Address < out[j].Address
		}
		return out[i].PoolContractID < out[j].PoolContractID
	})
	return out
}
