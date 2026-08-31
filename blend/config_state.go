package blend

// Blend's side of the config-persistence inversion of control. The adapter owns
// its low-frequency config: it declares the storage schema (ConfigSchema), emits
// this ledger's config changes as opaque records (ConfigRecords), and rebuilds the
// seed LedgerState from persisted records on cold start (HydrateConfig). The relay
// is a generic host that stores and returns records without decoding them.
//
// Config vs data split (persist ONLY the low-frequency half):
//   - oracle: decimals + asset->index map (NOT the per-index prices).
//   - pool:   oracle/backstop refs, status, take rate, wasm hash (NOT reserves).
//   - reserve: ResConfig — index, decimals, factors, rate-curve, caps (NOT ResData:
//     the b/d rate accumulators and supplies, which re-fold from bronze).
//
// All three methods are pure: ConfigRecords/HydrateConfig read only their inputs,
// so the fold stays run-twice byte-identical.

import (
	"encoding/json"
	"sort"
	"strconv"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/lightgatehq/lidapters/blend/contracts"
	"github.com/stellar/go-stellar-sdk/xdr"
)

const (
	kindOracle      = "blend.oracle"
	kindOraclePrice = "blend.oracle_price"
	kindPool        = "blend.pool"
	kindReserve     = "blend.reserve"

	tableOracle      = "blend_oracle_config"
	tableOraclePrice = "blend_oracle_price"
	tablePool        = "blend_pool_config"
	tableReserve     = "blend_reserve_config"

	// reserveKeySep joins the pool id and asset id into the reserve's opaque
	// entity_key; priceKeySep joins the oracle id and asset index into the price's
	// entity_key. The host treats the whole string as opaque; hydration recovers the
	// bindings from the payload, not by splitting these keys.
	reserveKeySep = "|"
	priceKeySep   = "|"
)

// factorScaleExpr is the 7-decimal fixed-point divisor Blend stores its factors and
// rate-curve params at. Analytics generated columns divide the raw payload value by
// it so a query reads 0.95 rather than 9500000. It is approximate-for-analytics; the
// exact raw string stays in payload.
const factorScaleExpr = "1e7"

// --- schema manifest --------------------------------------------------------

// ConfigSchema declares one table per config kind plus the STORED generated
// columns that expose the jsonb payload to SQL analytics. The host renders the DDL
// from this and imports none of these Blend field names — they live here.
func (a *Adapter) ConfigSchema() []bindings.ConfigTableSchema {
	factor := func(name, jsonKey string) bindings.ConfigGeneratedColumn {
		return bindings.ConfigGeneratedColumn{
			Name: name, SQLType: "numeric",
			Expr: "NULLIF(payload->>'" + jsonKey + "','')::numeric / " + factorScaleExpr,
		}
	}
	return []bindings.ConfigTableSchema{
		{
			Kind:  kindOracle,
			Table: tableOracle,
			Generated: []bindings.ConfigGeneratedColumn{
				{Name: "decimals", SQLType: "int", Expr: "NULLIF(payload->>'decimals','')::int"},
				{Name: "asset_count", SQLType: "int", Expr: "CASE WHEN jsonb_typeof(payload->'assets') = 'array' THEN jsonb_array_length(payload->'assets') ELSE 0 END"},
			},
			Indexes: []bindings.ConfigIndex{
				{Name: "idx_blend_oracle_config_decimals", Columns: []string{"decimals"}},
			},
			// Per-asset unnest of the oracle's asset->index map for analytics.
			Views: []bindings.ConfigView{{
				Name: "blend_oracle_asset",
				Body: "SELECT entity_key AS oracle_id, ledger, removed,\n" +
					"       (asset->>'asset_id') AS asset_id,\n" +
					"       NULLIF(asset->>'index','')::int AS asset_index\n" +
					"FROM blend_oracle_config\n" +
					"CROSS JOIN LATERAL jsonb_array_elements(\n" +
					"  CASE WHEN jsonb_typeof(payload->'assets')='array' THEN payload->'assets' ELSE '[]'::jsonb END\n" +
					") AS asset",
			}},
		},
		{
			// The oracle's raw index->price, one row per (oracle, asset_index) change.
			// Persisting prices (not just the map) is what removes the null-HF window
			// on restart: the reload stitches the latest price per index onto the map.
			Kind:  kindOraclePrice,
			Table: tableOraclePrice,
			Generated: []bindings.ConfigGeneratedColumn{
				{Name: "oracle_id", SQLType: "text", Expr: "payload->>'oracle_id'"},
				{Name: "asset_index", SQLType: "int", Expr: "NULLIF(payload->>'asset_index','')::int"},
				{Name: "price_raw", SQLType: "numeric", Expr: "NULLIF(payload->>'price_raw','')::numeric"},
			},
			Indexes: []bindings.ConfigIndex{
				{Name: "idx_blend_oracle_price_oracle", Columns: []string{"oracle_id"}},
				{Name: "idx_blend_oracle_price_index", Columns: []string{"oracle_id", "asset_index"}},
			},
			// Price history scaled by the oracle's decimals (a cross-table join, so it
			// lives in a view rather than a generated column).
			Views: []bindings.ConfigView{{
				Name: "blend_oracle_price_scaled",
				Body: "SELECT p.oracle_id, p.asset_index, p.ledger, p.removed, p.price_raw,\n" +
					"       CASE WHEN o.decimals IS NULL THEN NULL\n" +
					"            ELSE p.price_raw / power(10::numeric, o.decimals) END AS price_scaled\n" +
					"FROM blend_oracle_price p\n" +
					"LEFT JOIN LATERAL (\n" +
					"    SELECT decimals FROM blend_oracle_config c\n" +
					"    WHERE c.entity_key = p.oracle_id AND c.ledger <= p.ledger AND NOT c.removed\n" +
					"    ORDER BY c.ledger DESC LIMIT 1\n" +
					") o ON true",
			}},
		},
		{
			Kind:  kindPool,
			Table: tablePool,
			Generated: []bindings.ConfigGeneratedColumn{
				{Name: "status", SQLType: "text", Expr: "payload->>'status'"},
				{Name: "backstop_take_rate", SQLType: "numeric", Expr: "NULLIF(payload->>'take_rate','')::numeric / " + factorScaleExpr},
				{Name: "oracle_ref", SQLType: "text", Expr: "payload->>'oracle'"},
				{Name: "backstop_ref", SQLType: "text", Expr: "payload->>'backstop'"},
			},
			Indexes: []bindings.ConfigIndex{
				{Name: "idx_blend_pool_config_status", Columns: []string{"status"}},
				{Name: "idx_blend_pool_config_oracle_ref", Columns: []string{"oracle_ref"}},
			},
		},
		{
			Kind:  kindReserve,
			Table: tableReserve,
			Generated: []bindings.ConfigGeneratedColumn{
				{Name: "pool_id", SQLType: "text", Expr: "payload->>'pool_id'"},
				{Name: "asset_id", SQLType: "text", Expr: "payload->>'asset_id'"},
				{Name: "reserve_index", SQLType: "int", Expr: "NULLIF(payload->>'index','')::int"},
				factor("c_factor", "c_factor"),
				factor("l_factor", "l_factor"),
				factor("util_target", "util_target"),
				factor("max_util", "max_util"),
				factor("r_base", "r_base"),
				factor("r_one", "r_one"),
				factor("r_two", "r_two"),
				factor("r_three", "r_three"),
				{Name: "supply_cap", SQLType: "numeric", Expr: "NULLIF(payload->>'supply_cap','')::numeric"},
				factor("reactivity", "reactivity"),
				{Name: "enabled", SQLType: "boolean", Expr: "NULLIF(payload->>'enabled','')::boolean"},
				// Emission config (eps/expiration), not emission data (index/last_time
				// re-folds from bronze, same split as ResConfig vs ResData above). Raw
				// token-amount-per-second, so plain numeric like supply_cap (no /1e7).
				{Name: "supply_emis_eps", SQLType: "numeric", Expr: "NULLIF(payload->>'supply_emis_eps','')::numeric"},
				{Name: "supply_emis_expiration", SQLType: "bigint", Expr: "NULLIF(payload->>'supply_emis_expiration','')::bigint"},
				{Name: "borrow_emis_eps", SQLType: "numeric", Expr: "NULLIF(payload->>'borrow_emis_eps','')::numeric"},
				{Name: "borrow_emis_expiration", SQLType: "bigint", Expr: "NULLIF(payload->>'borrow_emis_expiration','')::bigint"},
			},
			Indexes: []bindings.ConfigIndex{
				{Name: "idx_blend_reserve_config_pool", Columns: []string{"pool_id"}},
				{Name: "idx_blend_reserve_config_asset", Columns: []string{"asset_id"}},
				{Name: "idx_blend_reserve_config_enabled", Columns: []string{"enabled"}},
			},
		},
	}
}

// --- payload bodies (canonical JSON; the adapter owns the shape) ------------

type oracleConfigBody struct {
	Decimals int32             `json:"decimals"`
	Assets   []oracleAssetBody `json:"assets"`
	// Exactly one of Aggregator / Feed is set when the blend.oracle record
	// describes a mainnet oracle-aggregator or a Reflector price feed rather
	// than a price-writing oracle. The host stores the payload opaquely, so the
	// extension needs no new kind or table; hydration routes on which section is
	// present. For these records Assets carries the entity's own asset map
	// (feed asset list / aggregator asset->synthetic-index map), keeping the
	// blend_oracle_asset analytics view meaningful for them too.
	Aggregator *aggregatorConfigBody `json:"aggregator,omitempty"`
	Feed       *feedConfigBody       `json:"feed,omitempty"`
}

// aggregatorConfigBody persists an oracle-aggregator's carried configuration —
// without it, a restarted fold would re-price only when the aggregator's
// instance is next rewritten, which on mainnet is effectively never.
type aggregatorConfigBody struct {
	MaxAgeS    int64                 `json:"max_age_s"`
	BaseKey    string                `json:"base_key"`
	BaseAssets []string              `json:"base_assets,omitempty"`
	Feeds      []aggregatorFeedBody  `json:"feeds,omitempty"`
	AssetsCfg  []aggregatorAssetBody `json:"assets_cfg,omitempty"`
}

type aggregatorFeedBody struct {
	Index       int64  `json:"index"`
	ContractID  string `json:"contract_id"`
	Decimals    int32  `json:"decimals"`
	ResolutionS int64  `json:"resolution_s"`
}

type aggregatorAssetBody struct {
	AssetID      string `json:"asset_id"`
	FeedAssetKey string `json:"feed_asset_key"`
	OracleIndex  int64  `json:"oracle_index"`
	MaxDev       int64  `json:"max_dev"`
}

// feedConfigBody persists a Reflector feed's carried state including its
// bounded recent rounds, so a restarted fold prices immediately instead of
// waiting out the feed's next publication.
type feedConfigBody struct {
	LastRoundMs int64           `json:"last_round_ms"`
	Rounds      []feedRoundBody `json:"rounds,omitempty"`
}

type feedRoundBody struct {
	TimestampMs int64                `json:"timestamp_ms"`
	Prices      []feedRoundPriceBody `json:"prices"`
}

type feedRoundPriceBody struct {
	Index    int64  `json:"index"`
	PriceRaw string `json:"price_raw"`
}

type oracleAssetBody struct {
	AssetID string `json:"asset_id"`
	Index   int64  `json:"index"`
}

type oraclePriceBody struct {
	OracleID   string `json:"oracle_id"`
	AssetIndex int64  `json:"asset_index"`
	PriceRaw   string `json:"price_raw"`
}

type poolConfigBody struct {
	Oracle   string `json:"oracle"`
	Backstop string `json:"backstop"`
	Status   string `json:"status"`
	TakeRate string `json:"take_rate"`
	WasmHash string `json:"wasm_hash"`
}

type reserveConfigBody struct {
	PoolID     string `json:"pool_id"`
	AssetID    string `json:"asset_id"`
	Index      int32  `json:"index"`
	Decimals   int32  `json:"decimals"`
	CFactor    string `json:"c_factor"`
	LFactor    string `json:"l_factor"`
	UtilTarget string `json:"util_target"`
	MaxUtil    string `json:"max_util"`
	RBase      string `json:"r_base"`
	ROne       string `json:"r_one"`
	RTwo       string `json:"r_two"`
	RThree     string `json:"r_three"`
	SupplyCap  string `json:"supply_cap"`
	Reactivity string `json:"reactivity"`
	Enabled    bool   `json:"enabled"`
	// Emission config only (eps/expiration) — emission DATA (index/last_time)
	// re-folds from bronze, same split as ResData above.
	SupplyEmisEPS        string `json:"supply_emis_eps"`
	SupplyEmisExpiration string `json:"supply_emis_expiration"`
	BorrowEmisEPS        string `json:"borrow_emis_eps"`
	BorrowEmisExpiration string `json:"borrow_emis_expiration"`
}

// --- record emission (chain-signal, pure) -----------------------------------

// ConfigRecords emits this ledger's config changes. It classifies the owned
// contract-data keys to find which config entities changed (pool Config/ResList,
// reserve ResConfig, oracle instance), then reads each dirty entity's current
// config from the freshly folded next state and serializes it. A removed config
// key yields a tombstone. Prices, ResData, positions and backstop balances are
// data, not config, and are never emitted here.
func (a *Adapter) ConfigRecords(next *bindings.LedgerState, changes []bindings.ContractDataChange, ledgerSeq int64) []bindings.ConfigRecord {
	if next == nil {
		next = &bindings.LedgerState{}
	}
	oracleByID := make(map[string]contracts.OracleState, len(next.Oracles))
	for _, o := range next.Oracles {
		oracleByID[o.ContractID] = o
	}
	poolByID := make(map[string]contracts.PoolState, len(next.Pools))
	for _, p := range next.Pools {
		poolByID[p.ContractID] = p
	}
	aggregatorByID := make(map[string]contracts.OracleAggregatorState, len(next.OracleAggregators))
	for _, a := range next.OracleAggregators {
		aggregatorByID[a.ContractID] = a
	}
	feedByID := make(map[string]contracts.PriceFeedState, len(next.PriceFeeds))
	for _, f := range next.PriceFeeds {
		feedByID[f.ContractID] = f
	}

	// dirty sets: value is true when the config key was removed this ledger.
	dirtyOracle := map[string]bool{}
	dirtyPool := map[string]bool{}
	dirtyAggregator := map[string]bool{}
	dirtyFeed := map[string]bool{}
	type reserveRef struct{ pool, asset string }
	dirtyReserve := map[reserveRef]bool{}
	type priceRef struct {
		oracle string
		index  int64
	}
	dirtyPrice := map[priceRef]bool{}

	for _, ch := range changes {
		key, ok := decodeScValBase64(ch.KeyXDR)
		if !ok {
			continue
		}
		removed := !configChangeLive(ch, ledgerSeq)
		if key.Type == xdr.ScValTypeScvLedgerKeyContractInstance {
			// A contract instance is config for whichever entity it belongs to. Use
			// the folded next state to tell an oracle instance (asset map) from a
			// pool instance (wasm hash); a removed instance that is gone from next is
			// left to the Config/ResList removal path (real decommission signal).
			// A Reflector feed's instance is rewritten every round and an
			// oracle-aggregator's on every admin config call — both are config for
			// the restart reload (the feed record also carries its recent rounds, so
			// a reload prices without waiting out the next publication).
			if _, isFeed := feedByID[ch.ContractID]; isFeed {
				dirtyFeed[ch.ContractID] = removed
			} else if _, isAggregator := aggregatorByID[ch.ContractID]; isAggregator {
				dirtyAggregator[ch.ContractID] = removed
			} else if _, isOracle := oracleByID[ch.ContractID]; isOracle {
				dirtyOracle[ch.ContractID] = removed
			} else if _, isPool := poolByID[ch.ContractID]; isPool {
				dirtyPool[ch.ContractID] = removed
			}
			continue
		}
		if isOraclePriceKey(key) {
			// A set_price entry (u128 key = asset index) on an owned oracle. Persist
			// the raw price so a restart reloads it — no waiting for the next
			// set_price. The oracle must be known (in next) to tie the index to a map.
			if _, isOracle := oracleByID[ch.ContractID]; isOracle {
				if index, ok := scInt64(key); ok && index >= 0 {
					dirtyPrice[priceRef{ch.ContractID, index}] = removed
				}
			}
			continue
		}
		if sym, ok := scSymbol(key); ok {
			switch sym {
			case "Config", "ResList":
				dirtyPool[ch.ContractID] = removed
			}
			continue
		}
		if variant, args, ok := scVariant(key); ok {
			switch variant {
			case "ResConfig":
				if asset, ok := variantAddress(args); ok {
					dirtyReserve[reserveRef{ch.ContractID, asset}] = removed
				}
			case "EmisConfig":
				// EmisConfig is config (persisted); its sibling EmisData is accrual
				// data and re-folds from bronze, so it is never classified here. An
				// EmisConfig change only ever re-marshals the reserve's current
				// state (which by now reflects this ledger's decode) — it never
				// tombstones the reserve record, even when the EmisConfig entry
				// itself was evicted/expired, because the reserve's core
				// (ResConfig-derived) config is unaffected by an emission change.
				if resTokenID, ok := variantU32(args); ok {
					if pool, ok := poolByID[ch.ContractID]; ok {
						if reserve, ok := reserveByIndex(pool, int32(resTokenID/2)); ok {
							dirtyReserve[reserveRef{ch.ContractID, reserve.AssetID}] = false
						}
					}
				}
			}
		}
	}

	records := make([]bindings.ConfigRecord, 0, len(dirtyOracle)+len(dirtyPool)+len(dirtyReserve)+len(dirtyAggregator)+len(dirtyFeed))
	seq := uint32(ledgerSeq)

	for id, removed := range dirtyAggregator {
		if removed {
			records = append(records, tombstone(kindOracle, id, seq))
			continue
		}
		a, ok := aggregatorByID[id]
		if !ok {
			continue
		}
		records = append(records, bindings.ConfigRecord{Kind: kindOracle, EntityKey: id, Ledger: seq, Payload: marshalAggregatorBody(a)})
	}
	for id, removed := range dirtyFeed {
		if removed {
			records = append(records, tombstone(kindOracle, id, seq))
			continue
		}
		f, ok := feedByID[id]
		if !ok {
			continue
		}
		records = append(records, bindings.ConfigRecord{Kind: kindOracle, EntityKey: id, Ledger: seq, Payload: marshalFeedBody(f)})
	}
	for id, removed := range dirtyOracle {
		if removed {
			records = append(records, tombstone(kindOracle, id, seq))
			continue
		}
		o, ok := oracleByID[id]
		if !ok {
			continue
		}
		records = append(records, bindings.ConfigRecord{Kind: kindOracle, EntityKey: id, Ledger: seq, Payload: marshalOracleBody(o)})
	}
	for id, removed := range dirtyPool {
		if removed {
			records = append(records, tombstone(kindPool, id, seq))
			continue
		}
		p, ok := poolByID[id]
		if !ok {
			continue
		}
		records = append(records, bindings.ConfigRecord{Kind: kindPool, EntityKey: id, Ledger: seq, Payload: marshalPoolBody(p)})
	}
	for ref, removed := range dirtyReserve {
		entityKey := ref.pool + reserveKeySep + ref.asset
		if removed {
			records = append(records, tombstone(kindReserve, entityKey, seq))
			continue
		}
		pool, ok := poolByID[ref.pool]
		if !ok {
			continue
		}
		reserve, ok := reserveByAsset(pool, ref.asset)
		if !ok {
			continue
		}
		records = append(records, bindings.ConfigRecord{Kind: kindReserve, EntityKey: entityKey, Ledger: seq, Payload: marshalReserveBody(ref.pool, reserve)})
	}
	for ref, removed := range dirtyPrice {
		entityKey := ref.oracle + priceKeySep + strconv.FormatInt(ref.index, 10)
		if removed {
			records = append(records, tombstone(kindOraclePrice, entityKey, seq))
			continue
		}
		oracle, ok := oracleByID[ref.oracle]
		if !ok {
			continue
		}
		priceRaw, ok := oraclePriceByIndex(oracle, ref.index)
		if !ok {
			// The oracle carries no live price for this index (evicted / not decoded)
			// — no upsert; the reserve is left without a price rather than seeded stale.
			continue
		}
		records = append(records, bindings.ConfigRecord{Kind: kindOraclePrice, EntityKey: entityKey, Ledger: seq, Payload: mustMarshal(oraclePriceBody{OracleID: ref.oracle, AssetIndex: ref.index, PriceRaw: priceRaw})})
	}

	// Deterministic order so a run-twice comparison of the emitted records is stable
	// (the host writes them keyed, so order does not affect storage, but pinning it
	// keeps the seam trivially testable).
	sort.Slice(records, func(i, j int) bool {
		if records[i].Kind != records[j].Kind {
			return records[i].Kind < records[j].Kind
		}
		return records[i].EntityKey < records[j].EntityKey
	})
	return records
}

// configChangeLive mirrors the live decision the state fold applies to a change:
// live requires a present value AND a TTL that has not lapsed. Eviction or TTL
// expiry of a config key is a removal (tombstone).
func configChangeLive(ch bindings.ContractDataChange, ledgerSeq int64) bool {
	if !ch.Live || ch.ValueXDR == nil {
		return false
	}
	if ch.LiveUntilLedgerSeq != nil && int64(*ch.LiveUntilLedgerSeq) < ledgerSeq {
		return false
	}
	return true
}

func tombstone(kind, entityKey string, ledger uint32) bindings.ConfigRecord {
	return bindings.ConfigRecord{Kind: kind, EntityKey: entityKey, Ledger: ledger, Removed: true}
}

func reserveByAsset(pool contracts.PoolState, assetID string) (contracts.ReserveState, bool) {
	for _, r := range pool.Reserves {
		if r.AssetID == assetID {
			return r, true
		}
	}
	return contracts.ReserveState{}, false
}

// reserveByIndex resolves a reserve by its index under the same known-unique
// rule the fold's reserveByIndex applies: only a reserve whose index came from
// a decoded ResConfig participates, and only when it is the single claimant —
// an unknown or duplicate index is unresolved, never a guessed winner.
func reserveByIndex(pool contracts.PoolState, index int32) (contracts.ReserveState, bool) {
	var found contracts.ReserveState
	claimants := 0
	for _, r := range pool.Reserves {
		if r.ReserveIndexKnown && r.ReserveIndex == index {
			found = r
			claimants++
		}
	}
	if claimants != 1 {
		return contracts.ReserveState{}, false
	}
	return found, true
}

func oraclePriceByIndex(oracle contracts.OracleState, index int64) (string, bool) {
	for _, p := range oracle.Prices {
		if p.Index == index {
			return p.PriceRaw, true
		}
	}
	return "", false
}

func marshalOracleBody(o contracts.OracleState) []byte {
	body := oracleConfigBody{Decimals: o.Decimals}
	body.Assets = make([]oracleAssetBody, 0, len(o.Assets))
	for _, a := range o.Assets {
		body.Assets = append(body.Assets, oracleAssetBody{AssetID: a.AssetID, Index: a.Index})
	}
	sort.Slice(body.Assets, func(i, j int) bool {
		if body.Assets[i].Index != body.Assets[j].Index {
			return body.Assets[i].Index < body.Assets[j].Index
		}
		return body.Assets[i].AssetID < body.Assets[j].AssetID
	})
	return mustMarshal(body)
}

// marshalAggregatorBody serializes an oracle-aggregator's carried config as an
// extended blend.oracle payload. The top-level assets list mirrors the
// synthetic asset->index map resolveAggregatorPrices derives (sorted asset IDs,
// index = position), so the blend_oracle_asset view reads the same mapping the
// fold prices with.
func marshalAggregatorBody(a contracts.OracleAggregatorState) []byte {
	body := oracleConfigBody{
		Decimals: a.Decimals,
		Aggregator: &aggregatorConfigBody{
			MaxAgeS:    a.MaxAgeS,
			BaseKey:    a.BaseKey,
			BaseAssets: append([]string(nil), a.BaseAssets...),
		},
	}
	assetIDs := make([]string, 0, len(a.Assets))
	for _, asset := range a.Assets {
		assetIDs = append(assetIDs, asset.AssetID)
		body.Aggregator.AssetsCfg = append(body.Aggregator.AssetsCfg, aggregatorAssetBody{
			AssetID:      asset.AssetID,
			FeedAssetKey: asset.FeedAssetKey,
			OracleIndex:  asset.OracleIndex,
			MaxDev:       asset.MaxDev,
		})
	}
	sort.Strings(assetIDs)
	for i, assetID := range assetIDs {
		body.Assets = append(body.Assets, oracleAssetBody{AssetID: assetID, Index: int64(i)})
	}
	sort.Slice(body.Aggregator.AssetsCfg, func(i, j int) bool {
		return body.Aggregator.AssetsCfg[i].AssetID < body.Aggregator.AssetsCfg[j].AssetID
	})
	for _, feed := range a.Feeds {
		body.Aggregator.Feeds = append(body.Aggregator.Feeds, aggregatorFeedBody{
			Index:       feed.Index,
			ContractID:  feed.ContractID,
			Decimals:    feed.Decimals,
			ResolutionS: feed.ResolutionS,
		})
	}
	sort.Slice(body.Aggregator.Feeds, func(i, j int) bool {
		return body.Aggregator.Feeds[i].Index < body.Aggregator.Feeds[j].Index
	})
	return mustMarshal(body)
}

// marshalFeedBody serializes a Reflector feed's carried state — asset list plus
// the bounded recent rounds — as an extended blend.oracle payload.
func marshalFeedBody(f contracts.PriceFeedState) []byte {
	body := oracleConfigBody{
		Decimals: f.Decimals,
		Feed:     &feedConfigBody{LastRoundMs: f.LastRoundMs},
	}
	for _, asset := range f.Assets {
		body.Assets = append(body.Assets, oracleAssetBody{AssetID: asset.AssetKey, Index: asset.Index})
	}
	sort.Slice(body.Assets, func(i, j int) bool {
		if body.Assets[i].Index != body.Assets[j].Index {
			return body.Assets[i].Index < body.Assets[j].Index
		}
		return body.Assets[i].AssetID < body.Assets[j].AssetID
	})
	for _, round := range f.Rounds {
		rb := feedRoundBody{TimestampMs: round.TimestampMs}
		for _, price := range round.Prices {
			rb.Prices = append(rb.Prices, feedRoundPriceBody{Index: price.Index, PriceRaw: price.PriceRaw})
		}
		body.Feed.Rounds = append(body.Feed.Rounds, rb)
	}
	sort.Slice(body.Feed.Rounds, func(i, j int) bool {
		return body.Feed.Rounds[i].TimestampMs < body.Feed.Rounds[j].TimestampMs
	})
	return mustMarshal(body)
}

func marshalPoolBody(p contracts.PoolState) []byte {
	return mustMarshal(poolConfigBody{
		Oracle:   p.OracleContract,
		Backstop: p.BackstopContract,
		Status:   p.PoolStatus,
		TakeRate: p.BackstopTakeRate,
		WasmHash: p.WasmHash,
	})
}

func marshalReserveBody(poolID string, r contracts.ReserveState) []byte {
	return mustMarshal(reserveConfigBody{
		PoolID:     poolID,
		AssetID:    r.AssetID,
		Index:      r.ReserveIndex,
		Decimals:   r.AssetDecimals,
		CFactor:    r.CFactorRaw,
		LFactor:    r.LFactorRaw,
		UtilTarget: r.UtilTargetRaw,
		MaxUtil:    r.MaxUtilRaw,
		RBase:      r.RBaseRaw,
		ROne:       r.ROneRaw,
		RTwo:       r.RTwoRaw,
		RThree:     r.RThreeRaw,
		SupplyCap:  r.SupplyCapRaw,
		Reactivity: r.ReactivityRaw,
		Enabled:    r.Enabled,

		SupplyEmisEPS:        r.SupplyEmisEPSRaw,
		SupplyEmisExpiration: r.SupplyEmisExpirationRaw,
		BorrowEmisEPS:        r.BorrowEmisEPSRaw,
		BorrowEmisExpiration: r.BorrowEmisExpirationRaw,
	})
}

// mustMarshal serializes a config body. The bodies are plain structs of scalars
// and a sorted slice, so json.Marshal is deterministic and cannot error.
func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

// --- hydration (pure) -------------------------------------------------------

// HydrateConfig rebuilds the seed LedgerState from the latest-per-entity config
// records the host loaded (tombstones already excluded). The result carries pool
// config (with reserve config attached) and the oracle asset->index map with
// decimals; it carries NO prices, NO ResData and NO positions — those re-fold from
// bronze after the restart. Reserve records whose pool was not loaded are dropped.
func (a *Adapter) HydrateConfig(records []bindings.ConfigRecord) (*bindings.LedgerState, error) {
	pools := map[string]*contracts.PoolState{}
	reservesByPool := map[string][]contracts.ReserveState{}
	oracles := []contracts.OracleState{}
	aggregators := []contracts.OracleAggregatorState{}
	feeds := []contracts.PriceFeedState{}
	// Prices are a separate facet keyed by (oracle, index); stitch them onto their
	// oracle's map after both facets are read, so the reload reconstructs a COMPLETE
	// OracleState{Decimals, Assets, Prices} and the first post-restart ledger has no
	// null-HF window.
	pricesByOracle := map[string][]contracts.OracleIndexPrice{}

	for _, rec := range records {
		if rec.Removed {
			continue
		}
		switch rec.Kind {
		case kindOracle:
			var body oracleConfigBody
			if err := json.Unmarshal(rec.Payload, &body); err != nil {
				return nil, err
			}
			// Extended payloads route to their carried-state slice; a plain body
			// stays a price-writing oracle's map, exactly as before.
			if body.Aggregator != nil {
				aggregators = append(aggregators, hydrateAggregator(rec.EntityKey, body))
				continue
			}
			if body.Feed != nil {
				feeds = append(feeds, hydrateFeed(rec.EntityKey, body))
				continue
			}
			oracle := contracts.OracleState{ContractID: rec.EntityKey, Decimals: body.Decimals}
			for _, asset := range body.Assets {
				oracle.Assets = append(oracle.Assets, contracts.OracleAssetIndex{AssetID: asset.AssetID, Index: asset.Index})
			}
			oracles = append(oracles, oracle)
		case kindOraclePrice:
			var body oraclePriceBody
			if err := json.Unmarshal(rec.Payload, &body); err != nil {
				return nil, err
			}
			pricesByOracle[body.OracleID] = append(pricesByOracle[body.OracleID], contracts.OracleIndexPrice{Index: body.AssetIndex, PriceRaw: body.PriceRaw})
		case kindPool:
			var body poolConfigBody
			if err := json.Unmarshal(rec.Payload, &body); err != nil {
				return nil, err
			}
			pools[rec.EntityKey] = &contracts.PoolState{
				ContractID:       rec.EntityKey,
				OracleContract:   body.Oracle,
				BackstopContract: body.Backstop,
				PoolStatus:       body.Status,
				BackstopTakeRate: body.TakeRate,
				WasmHash:         body.WasmHash,
			}
		case kindReserve:
			var body reserveConfigBody
			if err := json.Unmarshal(rec.Payload, &body); err != nil {
				return nil, err
			}
			reservesByPool[body.PoolID] = append(reservesByPool[body.PoolID], contracts.ReserveState{
				ReserveIndex: body.Index,
				// A blend.reserve record exists only because a ResConfig — index
				// included — was decoded, so hydration implies a known index.
				// This holds for records written before ReserveIndexKnown
				// existed: their payload has no boolean for it, and interpreting
				// the missing field as unknown would invalidate every
				// pre-existing config row.
				ReserveIndexKnown: true,
				AssetID:           body.AssetID,
				AssetDecimals:     body.Decimals,
				CFactorRaw:        body.CFactor,
				LFactorRaw:        body.LFactor,
				UtilTargetRaw:     body.UtilTarget,
				MaxUtilRaw:        body.MaxUtil,
				RBaseRaw:          body.RBase,
				ROneRaw:           body.ROne,
				RTwoRaw:           body.RTwo,
				RThreeRaw:         body.RThree,
				SupplyCapRaw:      body.SupplyCap,
				ReactivityRaw:     body.Reactivity,
				Enabled:           body.Enabled,

				SupplyEmisEPSRaw:        body.SupplyEmisEPS,
				SupplyEmisExpirationRaw: body.SupplyEmisExpiration,
				BorrowEmisEPSRaw:        body.BorrowEmisEPS,
				BorrowEmisExpirationRaw: body.BorrowEmisExpiration,
			})
		}
	}

	state := &bindings.LedgerState{}
	poolIDs := make([]string, 0, len(pools))
	for id := range pools {
		poolIDs = append(poolIDs, id)
	}
	sort.Strings(poolIDs)
	for _, id := range poolIDs {
		pool := pools[id]
		reserves := reservesByPool[id]
		sort.Slice(reserves, func(i, j int) bool {
			if reserves[i].ReserveIndex != reserves[j].ReserveIndex {
				return reserves[i].ReserveIndex < reserves[j].ReserveIndex
			}
			return reserves[i].AssetID < reserves[j].AssetID
		})
		pool.Reserves = reserves
		state.Pools = append(state.Pools, *pool)
	}
	for i := range oracles {
		prices := pricesByOracle[oracles[i].ContractID]
		sort.Slice(prices, func(a, b int) bool { return prices[a].Index < prices[b].Index })
		oracles[i].Prices = prices
	}
	sort.Slice(oracles, func(i, j int) bool { return oracles[i].ContractID < oracles[j].ContractID })
	state.Oracles = oracles
	if len(aggregators) > 0 {
		sort.Slice(aggregators, func(i, j int) bool { return aggregators[i].ContractID < aggregators[j].ContractID })
		state.OracleAggregators = aggregators
	}
	if len(feeds) > 0 {
		sort.Slice(feeds, func(i, j int) bool { return feeds[i].ContractID < feeds[j].ContractID })
		state.PriceFeeds = feeds
	}
	return state, nil
}

// hydrateAggregator rebuilds an oracle-aggregator's carried config from its
// persisted record.
func hydrateAggregator(contractID string, body oracleConfigBody) contracts.OracleAggregatorState {
	agg := contracts.OracleAggregatorState{
		ContractID: contractID,
		Decimals:   body.Decimals,
		MaxAgeS:    body.Aggregator.MaxAgeS,
		BaseKey:    body.Aggregator.BaseKey,
		BaseAssets: append([]string(nil), body.Aggregator.BaseAssets...),
	}
	for _, feed := range body.Aggregator.Feeds {
		agg.Feeds = append(agg.Feeds, contracts.AggregatorFeedRef{
			Index:       feed.Index,
			ContractID:  feed.ContractID,
			Decimals:    feed.Decimals,
			ResolutionS: feed.ResolutionS,
		})
	}
	for _, asset := range body.Aggregator.AssetsCfg {
		agg.Assets = append(agg.Assets, contracts.AggregatorAssetConfig{
			AssetID:      asset.AssetID,
			FeedAssetKey: asset.FeedAssetKey,
			OracleIndex:  asset.OracleIndex,
			MaxDev:       asset.MaxDev,
		})
	}
	return agg
}

// hydrateFeed rebuilds a Reflector feed's carried state — including the
// bounded recent rounds, so the first post-restart ledger prices immediately —
// from its persisted record.
func hydrateFeed(contractID string, body oracleConfigBody) contracts.PriceFeedState {
	feed := contracts.PriceFeedState{
		ContractID:  contractID,
		Decimals:    body.Decimals,
		LastRoundMs: body.Feed.LastRoundMs,
	}
	for _, asset := range body.Assets {
		feed.Assets = append(feed.Assets, contracts.FeedAssetIndex{AssetKey: asset.AssetID, Index: asset.Index})
	}
	for _, round := range body.Feed.Rounds {
		fr := contracts.FeedRound{TimestampMs: round.TimestampMs}
		for _, price := range round.Prices {
			fr.Prices = append(fr.Prices, contracts.FeedRoundPrice{Index: price.Index, PriceRaw: price.PriceRaw})
		}
		feed.Rounds = append(feed.Rounds, fr)
	}
	return feed
}
