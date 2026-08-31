// Mainnet Blend price decode: Reflector feeds + oracle-aggregators.
//
// On mainnet a Blend pool's PoolConfig.oracle names an oracle-aggregator — a
// SEP-40 VIEW contract (blend-capital/oracle-aggregator) that never writes a
// price on-ledger. The only on-ledger price writes live in the Reflector feed
// contracts the aggregator reads via cross-contract calls. A storage-write fold
// therefore decodes three things:
//
//  1. the feed's per-round price entries (two storage protocols exist inside a
//     mainnet replay window — the feed contract was upgraded live):
//     protocol 1: one temporary entry per (asset, round), key
//     u128(hi = round_ts_ms, lo = asset_index), value bare i128;
//     protocol 2: one temporary entry per round, key u64(round_ts_ms), value
//     {mask: Bytes, prices: Vec<i128>} — sparse: prices[k] belongs to
//     the k-th set bit of mask (bit i%8 of byte i/8 = asset index i);
//  2. the feed's instance storage (rewritten every round): the ordered asset
//     list, decimals, last_timestamp, and a small cache of recent rounds;
//  3. the aggregator's instance storage: Base / BaseAssets / Assets / Oracles /
//     Decimals / MaxAge — the asset->feed map its lastprice math uses.
//
// Formats verified against real mainnet ledger meta (see
// testdata/reflector_mainnet.json) and the published contract sources
// (reflector-network/reflector-contract oracle/src/{prices,mapping}.rs,
// blend-capital/oracle-aggregator src/price_data.rs @ b97e0a8).
//
// Each ledger, resolveAggregatorPrices replays the aggregator's own get_price
// against the carried feed rounds — newest round within MaxAge of the ledger
// close, max_dev deviation guard against the next older available round,
// feed->aggregator decimal rescale, base/BaseAssets hardcoded to 1 — and
// synthesizes the result into the same oracleBuilder representation the
// testnet mock writes, so resolveOraclePrices and everything downstream are
// untouched.
//
// Divergence note: the two mainnet aggregators are different builds with the
// same storage layout. This decoder mirrors blend-capital/oracle-aggregator's
// get_price. yieldblox/oracle-aggregator (@ 2307a9b) differs only at boundary
// regimes: it refuses any price once the feed's newest round is older than 2x
// resolution (vs MaxAge here), and rejects a deviation exactly equal to
// max_dev (vs accepting it here). Both implementations agree in the normal
// regime of a feed publishing every round.
package blend

import (
	"math/big"
	"sort"
	"time"

	"github.com/lightgatehq/lidapters/blend/contracts"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// maxCarriedRounds bounds the per-feed round carry. The aggregator's math can
// reach at most MaxAge/resolution rounds back from the newest round for the
// price walk plus the same again for the deviation guard's older price (900/300
// = 3 + 3 with the mainnet settings); 8 keeps headroom without carrying the
// feed's whole day-scale retention.
const maxCarriedRounds = 8

// feedBuilder mirrors one registered Reflector feed: the canonical-key ->
// index asset map from its instance, the newest round timestamp, and the
// carried rounds' raw prices (round ts ms -> asset index -> raw price).
type feedBuilder struct {
	decimals     int32
	lastRoundMs  int64
	assetToIndex map[string]int64
	rounds       map[int64]map[int64]string
}

// aggregatorBuilder mirrors one oracle-aggregator's instance configuration.
type aggregatorBuilder struct {
	decimals   int32
	maxAgeS    int64
	baseKey    string
	baseAssets []string
	feeds      map[int64]contracts.AggregatorFeedRef
	assets     map[string]contracts.AggregatorAssetConfig
}

func (b *blendStateBuilder) ensureFeed(feedID string) *feedBuilder {
	feed, ok := b.feeds[feedID]
	if !ok {
		feed = &feedBuilder{
			assetToIndex: map[string]int64{},
			rounds:       map[int64]map[int64]string{},
		}
		b.feeds[feedID] = feed
	}
	return feed
}

// canonicalAssetKey flattens the SEP-40 Asset enum into a stable string key:
// Asset::Stellar(addr) -> "stellar:<C...>", Asset::Other(sym) -> "other:<SYM>".
// The aggregator's per-asset config and the feed's asset list both speak this
// enum, and the two mainnet feeds use one variant each (DEX: Stellar addresses,
// CEX: tickers), so the canonical key is what joins them.
func canonicalAssetKey(v xdr.ScVal) (string, bool) {
	variant, args, ok := scVariant(v)
	if !ok {
		return "", false
	}
	switch variant {
	case "Stellar":
		addr, ok := variantAddress(args)
		if !ok {
			return "", false
		}
		return "stellar:" + addr, true
	case "Other":
		if len(args) == 0 {
			return "", false
		}
		sym, ok := scSymbol(args[0])
		if !ok {
			return "", false
		}
		return "other:" + sym, true
	default:
		return "", false
	}
}

// scString extracts an ScvString value. The Reflector feed instance keys its
// storage with ScString entries ("assets", "last_timestamp", ...), unlike the
// Symbol keys everywhere else in Blend's storage.
func scString(v xdr.ScVal) (string, bool) {
	s, ok := v.GetStr()
	if !ok {
		return "", false
	}
	return string(s), true
}

// stellarAssetID returns the contract address of a "stellar:<C...>" canonical
// key — the only kind that can name a pool reserve.
func stellarAssetID(key string) (string, bool) {
	const prefix = "stellar:"
	if len(key) > len(prefix) && key[:len(prefix)] == prefix {
		return key[len(prefix):], true
	}
	return "", false
}

// --- feed decode -------------------------------------------------------------

// applyFeedChange folds one live contract_data change of a registered feed.
func (b *blendStateBuilder) applyFeedChange(feedID string, key, value xdr.ScVal) {
	switch key.Type {
	case xdr.ScValTypeScvLedgerKeyContractInstance:
		b.applyFeedInstance(feedID, value)
	case xdr.ScValTypeScvU64:
		// Protocol 2: one batched entry per round.
		ts, ok := scInt64(key)
		if !ok || ts <= 0 {
			return
		}
		feed := b.ensureFeed(feedID)
		if prices, ok := decodeFeedRoundBatch(value); ok {
			feed.setRound(ts, prices)
		}
	case xdr.ScValTypeScvU128:
		// Protocol 1: one entry per (asset, round) — key hi = round ts ms,
		// key lo = asset index (reflector-contract format_price_key_v1).
		parts, ok := key.GetU128()
		if !ok {
			return
		}
		ts := int64(parts.Hi)
		index := int64(parts.Lo)
		if ts <= 0 || index < 0 {
			return
		}
		priceRaw, ok := scIntString(value)
		if !ok {
			return
		}
		feed := b.ensureFeed(feedID)
		round := feed.rounds[ts]
		if round == nil {
			round = map[int64]string{}
			feed.rounds[ts] = round
		}
		round[index] = priceRaw
		if ts > feed.lastRoundMs {
			feed.lastRoundMs = ts
		}
	}
}

// applyFeedDelete drops what a not-live change of a registered feed governed.
func (b *blendStateBuilder) applyFeedDelete(feedID string, key xdr.ScVal) {
	feed := b.feeds[feedID]
	if feed == nil {
		return
	}
	switch key.Type {
	case xdr.ScValTypeScvLedgerKeyContractInstance:
		delete(b.feeds, feedID)
	case xdr.ScValTypeScvU64:
		if ts, ok := scInt64(key); ok {
			delete(feed.rounds, ts)
		}
	case xdr.ScValTypeScvU128:
		if parts, ok := key.GetU128(); ok {
			if round := feed.rounds[int64(parts.Hi)]; round != nil {
				delete(round, int64(parts.Lo))
				if len(round) == 0 {
					delete(feed.rounds, int64(parts.Hi))
				}
			}
		}
	}
}

// applyFeedInstance decodes the feed's instance storage. Reflector rewrites the
// instance on every round (last_timestamp lives there), so the asset list is
// re-read continuously and a fold starting mid-stream is configured within one
// round of its floor. The instance keys are ScString (not Symbol). The protocol
// 2 instance also carries a cache of the most recent rounds — decoded like the
// round entries themselves, which primes the deviation guard's older price
// immediately after a restart or floor start.
func (b *blendStateBuilder) applyFeedInstance(feedID string, value xdr.ScVal) {
	instance, ok := value.GetInstance()
	if !ok || instance.Storage == nil {
		return
	}
	feed := b.ensureFeed(feedID)
	for _, entry := range []xdr.ScMapEntry(*instance.Storage) {
		name, ok := scString(entry.Key)
		if !ok {
			continue
		}
		switch name {
		case "assets":
			items, ok := scVec(entry.Val)
			if !ok {
				continue
			}
			assetToIndex := map[string]int64{}
			for i, item := range items {
				if key, ok := canonicalAssetKey(item); ok {
					assetToIndex[key] = int64(i)
				}
			}
			feed.assetToIndex = assetToIndex
		case "decimals":
			if decimals, ok := scInt32(entry.Val); ok {
				feed.decimals = decimals
			}
		case "last_timestamp":
			if ts, ok := scInt64(entry.Val); ok && ts > feed.lastRoundMs {
				feed.lastRoundMs = ts
			}
		case "cache":
			items, ok := scVec(entry.Val)
			if !ok {
				continue
			}
			for _, item := range items {
				pair, ok := scVec(item)
				if !ok || len(pair) != 2 {
					continue
				}
				ts, ok := scInt64(pair[0])
				if !ok || ts <= 0 {
					continue
				}
				if prices, ok := decodeFeedRoundBatch(pair[1]); ok {
					feed.setRound(ts, prices)
				}
			}
		}
	}
}

// decodeFeedRoundBatch decodes a protocol-2 round value {mask, prices}: the
// sparse prices vec paired with the asset-index bitmask (bit i%8 of byte i/8 =
// asset index i; prices[k] belongs to the k-th set bit — reflector-contract
// mapping.rs resolve_period_update_mask_position + prices.rs
// extract_single_update_record_price).
func decodeFeedRoundBatch(value xdr.ScVal) (map[int64]string, bool) {
	fields := scMapFields(value)
	if fields == nil {
		return nil, false
	}
	mask, ok := scValBytes(fields["mask"])
	if !ok {
		return nil, false
	}
	items, ok := scVec(fields["prices"])
	if !ok {
		return nil, false
	}
	prices := map[int64]string{}
	rank := 0
	for byteIdx, m := range mask {
		for bit := 0; bit < 8; bit++ {
			if m&(1<<bit) == 0 {
				continue
			}
			if rank >= len(items) {
				return prices, true
			}
			if priceRaw, ok := scIntString(items[rank]); ok {
				prices[int64(byteIdx*8+bit)] = priceRaw
			}
			rank++
		}
	}
	return prices, true
}

// setRound records one round's sparse prices, merging over any partial decode
// of the same round (a protocol-2 batch and the instance cache carry identical
// values for the same timestamp, so the merge is idempotent).
func (f *feedBuilder) setRound(ts int64, prices map[int64]string) {
	round := f.rounds[ts]
	if round == nil {
		round = map[int64]string{}
		f.rounds[ts] = round
	}
	for index, priceRaw := range prices {
		round[index] = priceRaw
	}
	if ts > f.lastRoundMs {
		f.lastRoundMs = ts
	}
}

// positivePrice returns the asset's raw price in the given round when present
// and > 0 — mirroring the contract, where only positive prices set the
// round/history bits a reader ever sees.
func (f *feedBuilder) positivePrice(ts, index int64) (string, bool) {
	round := f.rounds[ts]
	if round == nil {
		return "", false
	}
	priceRaw, ok := round[index]
	if !ok || !isPositiveIntString(priceRaw) {
		return "", false
	}
	return priceRaw, true
}

// trimRounds keeps only the newest maxCarriedRounds rounds so the carry stays
// bounded regardless of how long the fold runs.
func (f *feedBuilder) trimRounds() {
	if len(f.rounds) <= maxCarriedRounds {
		return
	}
	stamps := make([]int64, 0, len(f.rounds))
	for ts := range f.rounds {
		stamps = append(stamps, ts)
	}
	sort.Slice(stamps, func(i, j int) bool { return stamps[i] > stamps[j] })
	for _, ts := range stamps[maxCarriedRounds:] {
		delete(f.rounds, ts)
	}
}

// --- aggregator decode ---------------------------------------------------------

// applyAggregatorInstance decodes an oracle-aggregator's contract instance. Its
// entire configuration lives in the instance's Symbol-keyed storage
// (blend-capital/oracle-aggregator storage.rs): Base and MaxAge from the deploy
// transaction, then Oracles / Assets / BaseAssets assembled by admin calls that
// rewrite the instance. It returns true only when the storage carries both Base
// and MaxAge — the deploy-time shape — so a pool's or mock oracle's instance
// still falls through to its own handling. Each write replaces the whole
// carried config: the instance IS the config.
func (b *blendStateBuilder) applyAggregatorInstance(aggregatorID string, value xdr.ScVal) bool {
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
	baseKey, hasBase := canonicalAssetKey(storage["Base"])
	maxAge, hasMaxAge := scInt64(storage["MaxAge"])
	if !hasBase || !hasMaxAge {
		return false
	}
	decimals, ok := scInt32(storage["Decimals"])
	if !ok {
		return false
	}

	agg := &aggregatorBuilder{
		decimals: decimals,
		maxAgeS:  maxAge,
		baseKey:  baseKey,
		feeds:    map[int64]contracts.AggregatorFeedRef{},
		assets:   map[string]contracts.AggregatorAssetConfig{},
	}

	if items, ok := scVec(storage["BaseAssets"]); ok {
		for _, item := range items {
			if key, ok := canonicalAssetKey(item); ok {
				agg.baseAssets = append(agg.baseAssets, key)
			}
		}
		sort.Strings(agg.baseAssets)
	}

	if items, ok := scVec(storage["Oracles"]); ok {
		for _, item := range items {
			fields := scMapFields(item)
			feedID, ok := fieldAddress(fields, "address")
			if !ok {
				continue
			}
			index, ok := scInt64(fields["index"])
			if !ok {
				continue
			}
			feedDecimals, _ := fieldInt32(fields, "decimals")
			resolution, _ := scInt64(fields["resolution"])
			agg.feeds[index] = contracts.AggregatorFeedRef{
				Index:       index,
				ContractID:  feedID,
				Decimals:    feedDecimals,
				ResolutionS: resolution,
			}
		}
	}

	if raw, ok := storage["Assets"].GetMap(); ok && raw != nil {
		for _, entry := range []xdr.ScMapEntry(*raw) {
			assetKey, ok := canonicalAssetKey(entry.Key)
			if !ok {
				continue
			}
			assetID, ok := stellarAssetID(assetKey)
			if !ok {
				// Only a Stellar token contract can name a pool reserve; an
				// Other-keyed entry has nothing to resolve against.
				continue
			}
			fields := scMapFields(entry.Val)
			feedAssetKey, ok := canonicalAssetKey(fields["asset"])
			if !ok {
				continue
			}
			oracleIndex, ok := scInt64(fields["oracle_index"])
			if !ok {
				continue
			}
			maxDev, _ := scInt64(fields["max_dev"])
			agg.assets[assetID] = contracts.AggregatorAssetConfig{
				AssetID:      assetID,
				FeedAssetKey: feedAssetKey,
				OracleIndex:  oracleIndex,
				MaxDev:       maxDev,
			}
		}
	}

	b.aggregators[aggregatorID] = agg
	return true
}

// --- price resolution ----------------------------------------------------------

// resolveAggregatorPrices synthesizes each aggregator's per-asset prices into an
// oracleBuilder under the aggregator's own contract ID — the ID a pool's
// PoolConfig.oracle names — so resolveOraclePrices threads them onto reserves
// exactly as it does the testnet mock's written prices. Every asset the
// aggregator serves gets an index mapping; an asset whose price does not
// resolve (stale, deviant, missing) gets NO price at that index, which
// resolveOraclePrices turns into a cleared reserve price rather than a stale
// carry — the fold analog of the aggregator returning None.
func (b *blendStateBuilder) resolveAggregatorPrices(closeTime time.Time) {
	aggregatorIDs := make([]string, 0, len(b.aggregators))
	for id := range b.aggregators {
		aggregatorIDs = append(aggregatorIDs, id)
	}
	sort.Strings(aggregatorIDs)

	for _, aggregatorID := range aggregatorIDs {
		agg := b.aggregators[aggregatorID]

		// Collect every pool-addressable asset the aggregator answers for:
		// the feed-mapped assets plus the constant-1 assets (Base itself and
		// the BaseAssets list). Sorted, so the synthetic indices and the
		// run-twice output are deterministic.
		constants := map[string]struct{}{}
		if assetID, ok := stellarAssetID(agg.baseKey); ok {
			constants[assetID] = struct{}{}
		}
		for _, key := range agg.baseAssets {
			if assetID, ok := stellarAssetID(key); ok {
				constants[assetID] = struct{}{}
			}
		}
		assetIDs := make([]string, 0, len(agg.assets)+len(constants))
		for assetID := range agg.assets {
			assetIDs = append(assetIDs, assetID)
		}
		for assetID := range constants {
			if _, mapped := agg.assets[assetID]; !mapped {
				assetIDs = append(assetIDs, assetID)
			}
		}
		sort.Strings(assetIDs)

		oracle := b.ensureOracle(aggregatorID)
		oracle.synthesized = true
		oracle.decimals = agg.decimals
		oracle.assetToIndex = map[string]int64{}
		oracle.priceByIndex = map[int64]string{}

		one := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(agg.decimals)), nil).String()
		for i, assetID := range assetIDs {
			index := int64(i)
			oracle.assetToIndex[assetID] = index
			if _, isConst := constants[assetID]; isConst {
				// Base / BaseAssets: price 1.0 at the aggregator's decimals, no
				// feed lookup at all — the contract's own hardcoded branch.
				oracle.priceByIndex[index] = one
				continue
			}
			cfg := agg.assets[assetID]
			if priceRaw, ok := b.resolveFeedPrice(agg, cfg, closeTime); ok {
				oracle.priceByIndex[index] = priceRaw
			}
		}
	}
}

// resolveFeedPrice replays blend-capital/oracle-aggregator get_price
// (src/price_data.rs @ b97e0a8) against the carried rounds of the asset's
// mapped feed:
//
//	oldest = now - MaxAge
//	walk t from the feed's last round down one resolution per step while
//	  t >= oldest, until the asset has a positive price at t
//	deviation guard (0 < max_dev < 100): find the next older available price
//	  within MaxAge/resolution further steps; reject when none exists or when
//	  |price - old| > old * max_dev / 100
//	rescale feed decimals -> aggregator decimals
//
// "now" is the folding ledger's close time — the same value the contract's
// e.ledger().timestamp() yields when lastprice is called at that ledger. A
// zero closeTime (the legacy DecodeState path) anchors on the feed's newest
// round instead: the freshest price still resolves, but a silent feed cannot
// be aged out without a clock reference.
func (b *blendStateBuilder) resolveFeedPrice(agg *aggregatorBuilder, cfg contracts.AggregatorAssetConfig, closeTime time.Time) (string, bool) {
	feedRef, ok := agg.feeds[cfg.OracleIndex]
	if !ok || feedRef.ResolutionS <= 0 {
		return "", false
	}
	feed := b.feeds[feedRef.ContractID]
	if feed == nil || feed.lastRoundMs <= 0 {
		return "", false
	}
	index, ok := feed.assetToIndex[cfg.FeedAssetKey]
	if !ok {
		return "", false
	}

	nowS := feed.lastRoundMs / 1000
	if !closeTime.IsZero() {
		nowS = closeTime.Unix()
	}
	oldestS := nowS - agg.maxAgeS
	resolutionMs := feedRef.ResolutionS * 1000

	var foundRaw string
	foundTs := int64(0)
	for t := feed.lastRoundMs; t > 0 && t/1000 >= oldestS; t -= resolutionMs {
		if priceRaw, ok := feed.positivePrice(t, index); ok {
			foundRaw, foundTs = priceRaw, t
			break
		}
	}
	if foundTs == 0 {
		return "", false
	}

	if cfg.MaxDev > 0 && cfg.MaxDev < 100 {
		var oldRaw string
		maxSteps := agg.maxAgeS / feedRef.ResolutionS
		t := foundTs - resolutionMs
		for step := int64(0); step < maxSteps && t > 0; step++ {
			if priceRaw, ok := feed.positivePrice(t, index); ok {
				oldRaw = priceRaw
				break
			}
			t -= resolutionMs
		}
		if oldRaw == "" {
			// No older price to validate against — the contract refuses to
			// serve rather than skip the guard.
			return "", false
		}
		price, ok1 := new(big.Int).SetString(foundRaw, 10)
		old, ok2 := new(big.Int).SetString(oldRaw, 10)
		if !ok1 || !ok2 {
			return "", false
		}
		diff := new(big.Int).Abs(new(big.Int).Sub(price, old))
		limit := new(big.Int).Div(new(big.Int).Mul(old, big.NewInt(cfg.MaxDev)), big.NewInt(100))
		if diff.Cmp(limit) > 0 {
			return "", false
		}
	}

	return rescalePrice(foundRaw, feedRef.Decimals, agg.decimals)
}

// rescalePrice converts a raw integer price between decimal scales the way the
// aggregator's normalize_price does: floor-division shrinking, multiplication
// widening. Feeds store 14 decimals, the aggregators report 7 — miss this and
// every valuation is off by 10^7.
func rescalePrice(raw string, fromDecimals, toDecimals int32) (string, bool) {
	price, ok := new(big.Int).SetString(raw, 10)
	if !ok || price.Sign() <= 0 {
		return "", false
	}
	switch {
	case fromDecimals > toDecimals:
		scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(fromDecimals-toDecimals)), nil)
		return new(big.Int).Quo(price, scale).String(), true
	case fromDecimals < toDecimals:
		scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(toDecimals-fromDecimals)), nil)
		return new(big.Int).Mul(price, scale).String(), true
	default:
		return price.String(), true
	}
}

// --- carry (LedgerState round-trip) ---------------------------------------------

func feedBuilderFromState(state contracts.PriceFeedState) *feedBuilder {
	feed := &feedBuilder{
		decimals:     state.Decimals,
		lastRoundMs:  state.LastRoundMs,
		assetToIndex: map[string]int64{},
		rounds:       map[int64]map[int64]string{},
	}
	for _, asset := range state.Assets {
		feed.assetToIndex[asset.AssetKey] = asset.Index
	}
	for _, round := range state.Rounds {
		prices := map[int64]string{}
		for _, price := range round.Prices {
			prices[price.Index] = price.PriceRaw
		}
		feed.rounds[round.TimestampMs] = prices
	}
	return feed
}

func aggregatorBuilderFromState(state contracts.OracleAggregatorState) *aggregatorBuilder {
	agg := &aggregatorBuilder{
		decimals:   state.Decimals,
		maxAgeS:    state.MaxAgeS,
		baseKey:    state.BaseKey,
		baseAssets: append([]string(nil), state.BaseAssets...),
		feeds:      map[int64]contracts.AggregatorFeedRef{},
		assets:     map[string]contracts.AggregatorAssetConfig{},
	}
	for _, feed := range state.Feeds {
		agg.feeds[feed.Index] = feed
	}
	for _, asset := range state.Assets {
		agg.assets[asset.AssetID] = asset
	}
	return agg
}

// buildPriceFeeds snapshots the carried feed state, trimming each feed's rounds
// to the bounded carry. Every slice is sorted so run-twice output stays
// byte-identical.
func (b *blendStateBuilder) buildPriceFeeds() []contracts.PriceFeedState {
	if len(b.feeds) == 0 {
		return nil
	}
	feeds := make([]contracts.PriceFeedState, 0, len(b.feeds))
	for feedID, feed := range b.feeds {
		feed.trimRounds()
		state := contracts.PriceFeedState{
			ContractID:  feedID,
			Decimals:    feed.decimals,
			LastRoundMs: feed.lastRoundMs,
		}
		state.Assets = make([]contracts.FeedAssetIndex, 0, len(feed.assetToIndex))
		for assetKey, index := range feed.assetToIndex {
			state.Assets = append(state.Assets, contracts.FeedAssetIndex{AssetKey: assetKey, Index: index})
		}
		sort.Slice(state.Assets, func(i, j int) bool {
			if state.Assets[i].Index != state.Assets[j].Index {
				return state.Assets[i].Index < state.Assets[j].Index
			}
			return state.Assets[i].AssetKey < state.Assets[j].AssetKey
		})
		state.Rounds = make([]contracts.FeedRound, 0, len(feed.rounds))
		for ts, prices := range feed.rounds {
			round := contracts.FeedRound{TimestampMs: ts}
			round.Prices = make([]contracts.FeedRoundPrice, 0, len(prices))
			for index, priceRaw := range prices {
				round.Prices = append(round.Prices, contracts.FeedRoundPrice{Index: index, PriceRaw: priceRaw})
			}
			sort.Slice(round.Prices, func(i, j int) bool { return round.Prices[i].Index < round.Prices[j].Index })
			state.Rounds = append(state.Rounds, round)
		}
		sort.Slice(state.Rounds, func(i, j int) bool { return state.Rounds[i].TimestampMs < state.Rounds[j].TimestampMs })
		feeds = append(feeds, state)
	}
	sort.Slice(feeds, func(i, j int) bool { return feeds[i].ContractID < feeds[j].ContractID })
	return feeds
}

// buildOracleAggregators snapshots the carried aggregator configs, sorted for
// byte-identical run-twice output.
func (b *blendStateBuilder) buildOracleAggregators() []contracts.OracleAggregatorState {
	if len(b.aggregators) == 0 {
		return nil
	}
	aggregators := make([]contracts.OracleAggregatorState, 0, len(b.aggregators))
	for aggregatorID, agg := range b.aggregators {
		state := contracts.OracleAggregatorState{
			ContractID: aggregatorID,
			Decimals:   agg.decimals,
			MaxAgeS:    agg.maxAgeS,
			BaseKey:    agg.baseKey,
			BaseAssets: append([]string(nil), agg.baseAssets...),
		}
		state.Feeds = make([]contracts.AggregatorFeedRef, 0, len(agg.feeds))
		for _, feed := range agg.feeds {
			state.Feeds = append(state.Feeds, feed)
		}
		sort.Slice(state.Feeds, func(i, j int) bool { return state.Feeds[i].Index < state.Feeds[j].Index })
		state.Assets = make([]contracts.AggregatorAssetConfig, 0, len(agg.assets))
		for _, asset := range agg.assets {
			state.Assets = append(state.Assets, asset)
		}
		sort.Slice(state.Assets, func(i, j int) bool { return state.Assets[i].AssetID < state.Assets[j].AssetID })
		aggregators = append(aggregators, state)
	}
	sort.Slice(aggregators, func(i, j int) bool { return aggregators[i].ContractID < aggregators[j].ContractID })
	return aggregators
}
