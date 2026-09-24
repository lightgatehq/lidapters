// Package contracts holds the Blend-domain state and vocabulary types: the
// typed ledger-state slices the adapter's DecodeState decodes contract_data
// into, and the position/activity enums Blend emits. The protocol-agnostic seam
// types (ProtocolAdapter, TransformInput/Output, RawEventEnvelope, config
// records) live in the module's bindings package.
package contracts

import "time"

type PositionType string

const (
	PositionTypeSupply     PositionType = "supply"
	PositionTypeCollateral PositionType = "collateral"
	PositionTypeLiability  PositionType = "liability"
	PositionTypeBackstop   PositionType = "backstop"
)

type ActivityType string

const (
	ActivityTypeDeposit      ActivityType = "deposit"
	ActivityTypeWithdraw     ActivityType = "withdraw"
	ActivityTypeBorrow       ActivityType = "borrow"
	ActivityTypeRepay        ActivityType = "repay"
	ActivityTypeLiquidation  ActivityType = "liquidation"
	ActivityTypeClaimRewards ActivityType = "claim_rewards"
	ActivityTypeFlashLoan    ActivityType = "flash_loan"
	ActivityTypeBadDebt      ActivityType = "bad_debt"
	ActivityTypeStatusChange ActivityType = "contract_status_change"
)

// Exact blend-contracts-v2 pool event names, carried verbatim as activity
// types. A consumer that stores activities in an activity_type enum must carry
// these exact values — the two vocabularies must stay identical, or a consumer
// that normalizes unknown types coerces the row to contract_status_change and
// the insert fails the lifecycle_synthetic_identity CHECK on the stored
// activity. The v2 events withdraw, borrow, repay and flash_loan share their
// spelling with the legacy constants above.
const (
	ActivityTypeSupply                ActivityType = "supply"
	ActivityTypeSupplyCollateral      ActivityType = "supply_collateral"
	ActivityTypeWithdrawCollateral    ActivityType = "withdraw_collateral"
	ActivityTypeClaim                 ActivityType = "claim"
	ActivityTypeNewAuction            ActivityType = "new_auction"
	ActivityTypeFillAuction           ActivityType = "fill_auction"
	ActivityTypeDeleteAuction         ActivityType = "delete_auction"
	ActivityTypeSetStatus             ActivityType = "set_status"
	ActivityTypeSetReserve            ActivityType = "set_reserve"
	ActivityTypeQueueSetReserve       ActivityType = "queue_set_reserve"
	ActivityTypeCancelSetReserve      ActivityType = "cancel_set_reserve"
	ActivityTypeUpdatePool            ActivityType = "update_pool"
	ActivityTypeSetAdmin              ActivityType = "set_admin"
	ActivityTypeGulp                  ActivityType = "gulp"
	ActivityTypeGulpEmissions         ActivityType = "gulp_emissions"
	ActivityTypeReserveEmissionUpdate ActivityType = "reserve_emission_update"
	ActivityTypeDefaultedDebt         ActivityType = "defaulted_debt"
)

type PoolState struct {
	ContractID       string
	BackstopContract string
	OracleContract   string
	WasmHash         string
	PoolStatus       string
	BackstopTakeRate string
	// Pool instance-storage identity: the on-chain display Name (the authoritative
	// value a registry-sourced pool_name diverges from), the pool Admin address,
	// and the pool-level BLND token address. Empty when absent on-chain.
	Name      string
	Admin     string
	BLNDToken string
	// PoolConfig's remaining fields: max effective positions per user (u32) and
	// the minimum collateral (i128, oracle units) to open any liability. Raw
	// strings, empty when absent — never a fabricated "0".
	MaxPositionsRaw  string
	MinCollateralRaw string
	// PoolEmissions is the pool's PoolEmis entry: the per-reserve-token BLND
	// emission share split (res_token_id -> share, 7-dp fraction of 1e7).
	// Sorted by ReserveTokenID; nil when the entry is absent on-chain.
	PoolEmissions []PoolEmissionEntry
	Reserves      []ReserveState
	// Backstop pool-level balance: the totals from this pool's PoolBalance entry
	// in the backstop contract (total LP shares/tokens deposited against the
	// pool, and the aggregate shares currently queued for withdrawal). These ride
	// on PoolState rather than only on BackstopPosition so they round-trip via
	// prior.Pools independent of whether the pool currently has any individual
	// backstop depositors.
	BackstopSharesRaw    string
	BackstopTokensRaw    string
	BackstopQ4WSharesRaw string
	// Backstop pool-level emission accrual: the backstop contract's
	// BEmisData(pool) entry (BackstopEmissionData {expiration, eps, index,
	// last_time}), riding on PoolState for the same carry reason as the balance
	// fields above. Empty string means the entry (or that field) is absent
	// on-chain — emissions not activated — never a fabricated "0".
	BackstopEmisEPSRaw        string
	BackstopEmisExpirationRaw string
	BackstopEmisIndexRaw      string
	BackstopEmisLastTimeRaw   string
}

type ReserveState struct {
	ReserveIndex int32
	// ReserveIndexKnown marks whether ReserveIndex came from a successfully
	// decoded ResConfig.index (or config hydration of such a record). Zero is a
	// valid reserve index, so validity can never be inferred from the value: a
	// reserve materialized by ResData alone keeps the zero default, and only
	// this bit distinguishes "configured at index 0" from "index never seen".
	// Only a known index participates in index-based resolution, and only when
	// it is unique within the pool.
	ReserveIndexKnown bool
	AssetID           string
	AssetDecimals     int32
	BRateRaw          string
	DRateRaw          string
	BSupplyRaw        string
	DSupplyRaw        string
	PoolBalanceRaw    string
	CFactorRaw        string
	LFactorRaw        string
	UtilTargetRaw     string
	MaxUtilRaw        string
	RBaseRaw          string
	ROneRaw           string
	RTwoRaw           string
	RThreeRaw         string
	RateModifierRaw   string
	SupplyCapRaw      string
	OraclePriceRaw    string
	OracleDecimals    int32
	// Enabled is ResConfig's per-reserve "can act right now" flag — distinct from
	// the pool-level status. ReactivityRaw is the IR-curve reactivity constant
	// (u32, 7-dp), governing how fast ir_mod moves.
	Enabled       bool
	ReactivityRaw string
	// Normalized APR fractions, not percentages. Empty means unavailable.
	SupplyEmissionsAPR string
	BorrowEmissionsAPR string
	// Per-side (supply/b-token, borrow/d-token) emission config + accrual data,
	// decoded from the pool's EmisConfig/EmisData(res_token_id) storage, where
	// res_token_id = ReserveIndex*2 + (1 for supply, 0 for borrow). An empty
	// EPSRaw means no active emission config exists for that side — absence,
	// never a fabricated "0". Expiration/LastTime are raw unix-second strings.
	SupplyEmisEPSRaw        string
	SupplyEmisExpirationRaw string
	SupplyEmisIndexRaw      string
	SupplyEmisLastTimeRaw   string
	BorrowEmisEPSRaw        string
	BorrowEmisExpirationRaw string
	BorrowEmisIndexRaw      string
	BorrowEmisLastTimeRaw   string
	// BackstopCreditRaw is ResData's backstop_credit — underlying accrued to the
	// backstop (protocol take on debt interest) but not yet gulped. It directly
	// reduces true available liquidity. LastTimeRaw is ResData's last_time — the
	// unix second the reserve's rates last accrued, letting a consumer reproduce
	// interest to now deterministically. Both empty when absent on-chain.
	BackstopCreditRaw string
	LastTimeRaw       string
	// Archived marks a reserve whose ResConfig/ResData entry went not-live via TTL
	// lapse or network-level eviction rather than an explicit on-chain delete. The
	// reserve (and its reserveByIndex slot) is kept rather than dropped, so a pool
	// user's position still resolves against it — dropping it here would silently
	// zero every holder's row for this asset. Cleared back to false the next time
	// a live ResConfig/ResData write for this reserve is decoded.
	// ArchivedLedgerSeq is the ledger the lapse was observed on, zero when not
	// archived.
	Archived          bool
	ArchivedLedgerSeq int64
}

type UserReservePosition struct {
	Address        string
	PoolContractID string
	AssetID        string
	PositionType   PositionType
	BTokensRaw     string
	DTokensRaw     string
	// Archived mirrors the owning PendingUserPosition's archival state (see
	// PendingUserPosition.Archived): the user's Positions entry lapsed via TTL
	// or eviction rather than an explicit on-chain delete, so this row is a
	// dormant-but-restorable holding, not a live one. ArchivedLedgerSeq is the
	// ledger the lapse was observed on, zero when not archived.
	Archived          bool
	ArchivedLedgerSeq int64
}

// PoolEmissionEntry is one reserve token's share of the pool's BLND emission
// split (the PoolEmis Map<u32, u64> entry). ShareRaw is a 7-dp fraction of
// 1e7 (15% = "1500000").
type PoolEmissionEntry struct {
	ReserveTokenID int32
	ShareRaw       string
}

// QueuedReserveState is one pending, time-locked reserve-parameter change:
// the pool's ResInit(Address) entry (QueuedReserveInit {new_config,
// unlock_time}) — the "params about to change" signal, previously entirely
// undecoded. It is deliberately NOT merged into the pool's live reserves: the
// queued config takes effect only when set_reserve executes after
// unlock_time. NewConfig carries the queued ReserveConfig verbatim as raw
// strings ("" = field absent on-chain, Enabled "true"/"false"/"" likewise).
type QueuedReserveState struct {
	PoolContractID string
	AssetID        string
	UnlockTimeRaw  string
	NewConfig      QueuedReserveConfig
}

// QueuedReserveConfig mirrors the on-chain ReserveConfig inside a
// QueuedReserveInit, every field raw and independently optional.
type QueuedReserveConfig struct {
	IndexRaw      string
	DecimalsRaw   string
	CFactorRaw    string
	LFactorRaw    string
	UtilRaw       string
	MaxUtilRaw    string
	RBaseRaw      string
	ROneRaw       string
	RTwoRaw       string
	RThreeRaw     string
	ReactivityRaw string
	SupplyCapRaw  string
	Enabled       string // "true" / "false" / "" when absent
}

// BackstopInstanceState is the backstop contract's decoded identity: the
// instance-storage addresses (BToken — the Comet LP anchoring share
// valuation — BLNDTkn, USDCTkn, Emitter, PoolFact) plus the top-level RZ
// (reward-zone membership: which pools earn emissions at all) and DropList
// persistent entries. Carried in LedgerState because the instance is written
// at deploy and rarely re-emitted — the same carry requirement as
// OracleState. Empty/nil fields mean absent on-chain.
type BackstopInstanceState struct {
	ContractID    string
	BackstopToken string // BToken: the Comet LP (backstop token) address
	BLNDToken     string
	USDCToken     string
	Emitter       string
	PoolFactory   string
	RewardZone    []string
	DropList      []DropListEntry
}

// DropListEntry is one (Address, i128) pair of the backstop's DropList.
type DropListEntry struct {
	Address   string
	AmountRaw string
}

// AuctionState is one live auction's decoded on-chain state: the pool's
// Auction(AuctionKey{user, auct_type}) temporary entry (AuctionData {bid, lot,
// block}). AuctionType follows the contract's enum: 0 = user_liquidation,
// 1 = bad_debt, 2 = interest. Lot and Bid are the full per-asset maps, sorted
// by asset for deterministic output; their unit depends on the auction type
// (see the contract's AuctionData docs — e.g. a user liquidation's lot is
// bTokens and bid is dTokens). The entry is temporary storage: when it goes
// not-live on-chain (filled, deleted, or TTL-lapsed) the auction is gone.
type AuctionState struct {
	PoolContractID string
	UserAddress    string
	AuctionType    int32
	Block          int64
	Lot            []AuctionEntry
	Bid            []AuctionEntry
}

// AuctionEntry is one asset's amount within an auction's lot or bid map.
type AuctionEntry struct {
	AssetID   string
	AmountRaw string
}

// UserEmissionState is one user's per-reserve-token emission accrual: the
// pool's UserEmis(UserReserveKey{user, reserve_id}) entry (UserEmissionData
// {index, accrued}). ReserveTokenID is the contract's res_token_id
// (reserve_index*2 + side; side 1 = supply/b-token, 0 = borrow/d-token), kept
// raw so the entry survives even when the reserve list is not yet known;
// asset/side resolution happens at transform time. AccruedRaw is the user's
// checkpointed unclaimed BLND for that reserve token. Both fields are decoded
// verbatim; an absent entry is absent from the slice, never zero-filled.
type UserEmissionState struct {
	Address        string
	PoolContractID string
	ReserveTokenID int32
	IndexRaw       string
	AccruedRaw     string
}

type Q4WEntry struct {
	SharesRaw string
	UnlockAt  time.Time
}

type BackstopPosition struct {
	Address        string
	PoolContractID string
	UserSharesRaw  string
	PoolSharesRaw  string
	PoolTokensRaw  string
	Q4W            []Q4WEntry
	// UnclaimedEmissionsRaw is the backstop contract's UEmisData(pool, user)
	// accrued value — the user's checkpointed, not-yet-claimed backstop BLND.
	// EmisIndexRaw is the same entry's index (the user's last accrued emission
	// index, 14 decimals). Both empty when the entry is absent on-chain —
	// emissions not activated or nothing accrued — never a fabricated "0".
	UnclaimedEmissionsRaw string
	EmisIndexRaw          string
	LPTokenSupplyRaw      string
	LPBLNDReserveRaw      string
	LPUSDCReserveRaw      string
	BLNDDecimals          int32
	USDCDecimals          int32
	BLNDPriceUSD          string
	USDCPriceUSD          string
	BackstopInterestAPY   string
	BackstopEmissionsAPY  string
}

// OracleState is one price oracle's carried decode state: the shared price
// decimals, the asset->index map decoded from its instance storage, and the
// latest raw price per index. It rides in LedgerState so the decoder stays a
// stateless pure reducer — see bindings.LedgerState.Oracles.
type OracleState struct {
	ContractID string
	Decimals   int32
	Assets     []OracleAssetIndex
	Prices     []OracleIndexPrice
	// Instance-storage facets beyond the asset list: the quote asset every
	// price is denominated in (canonical SEP-40 key,
	// "stellar:<C...>" / "other:<SYM>"), the update cadence in seconds, and the
	// oracle admin. LastTimestampRaw is the oracle's top-level `timestamp`
	// entry — the last-price-update unix time, the price-freshness signal.
	// All empty when absent on-chain.
	BaseKey          string
	ResolutionRaw    string
	Admin            string
	LastTimestampRaw string
}

// OracleAssetIndex binds one asset to its index in the oracle's asset list. The
// oracle keys each price by this index, so this map is what ties a stored price
// back to a pool reserve.
type OracleAssetIndex struct {
	AssetID string
	Index   int64
}

// OracleIndexPrice is one asset's latest raw oracle price, keyed by the asset's
// index. The raw i128 price is resolved to a reserve at build time once the
// asset list is known.
type OracleIndexPrice struct {
	Index    int64
	PriceRaw string
}

// PriceFeedState is one registered Reflector price feed's carried decode state:
// the ordered asset list from the feed's instance storage, the feed's shared
// price decimals, the newest round timestamp, and the most recent rounds' raw
// prices. Reflector writes one round per resolution period (temporary entries;
// protocol 1 keyed u128(ts_ms<<64|asset_index) with a bare i128 value, protocol
// 2 keyed u64(ts_ms) with a sparse {mask, prices} batch), so the rounds an
// aggregator's staleness window can still reach must ride in LedgerState for a
// price-less ledger to resolve anything — the feed analog of OracleState.
type PriceFeedState struct {
	ContractID  string
	Decimals    int32
	LastRoundMs int64
	Assets      []FeedAssetIndex
	Rounds      []FeedRound
}

// FeedAssetIndex binds one feed asset to its index in the feed's ordered asset
// list. AssetKey is the canonical form of the SEP-40 Asset enum:
// "stellar:<contract-id>" for Asset::Stellar, "other:<symbol>" for Asset::Other.
type FeedAssetIndex struct {
	AssetKey string
	Index    int64
}

// FeedRound is one Reflector publication round: the normalized round timestamp
// (milliseconds, on the feed's resolution grid) and the raw feed-decimal price
// per asset index present in that round.
type FeedRound struct {
	TimestampMs int64
	Prices      []FeedRoundPrice
}

// FeedRoundPrice is one asset's raw price within a round, keyed by the asset's
// index in the feed's asset list.
type FeedRoundPrice struct {
	Index    int64
	PriceRaw string
}

// OracleAggregatorState is one Blend oracle-aggregator's carried configuration,
// decoded from its instance storage (Admin/Base/BaseAssets/Assets/Oracles/
// Decimals/MaxAge — blend-capital/oracle-aggregator storage.rs). The aggregator
// itself never writes prices; at decode time its per-asset prices are
// synthesized from the registered feeds' rounds using exactly this
// configuration. The config is assembled across several admin transactions
// after deploy, then rarely touched, so it must ride in LedgerState — the same
// carry requirement as OracleState.
type OracleAggregatorState struct {
	ContractID string
	Decimals   int32
	MaxAgeS    int64
	// BaseKey is the canonical asset key of the aggregator's Base — the unit its
	// answers are quoted in ("other:USD" fiat for Fixed, "stellar:<USDC id>" for
	// YieldBlox). Carried, not converted: downstream valuations are denominated
	// exactly as the pool's own solvency math sees them.
	BaseKey string
	// BaseAssets are canonical asset keys hardcoded to price 1.0 at the
	// aggregator's decimals, with no feed lookup at all.
	BaseAssets []string
	Feeds      []AggregatorFeedRef
	Assets     []AggregatorAssetConfig
}

// AggregatorFeedRef is one entry of the aggregator's Oracles vec: the Reflector
// feed answering for the assets whose OracleIndex points here, plus the feed
// decimals and resolution the aggregator's own price math uses.
type AggregatorFeedRef struct {
	Index       int64
	ContractID  string
	Decimals    int32
	ResolutionS int64
}

// AggregatorAssetConfig maps one pool-side asset (a Stellar token contract) to
// its feed-side identity: the canonical feed asset key, the feed to ask
// (OracleIndex into Feeds), and the max_dev deviation guard (percent, 0/100 =
// disabled) the aggregator applies before serving a price.
type AggregatorAssetConfig struct {
	AssetID      string
	FeedAssetKey string
	OracleIndex  int64
	MaxDev       int64
}

// AssetMetadata is one registered token contract's decoded human-readable
// identity — a Stellar Asset Contract's AssetInfo instance entry or a SEP-41
// token's METADATA instance entry, decoded to {symbol, name, decimals}. It
// rides in LedgerState so the decoder stays a stateless pure reducer, the same
// carry requirement as OracleState: the instance is written once at deploy and
// not re-emitted on later ledgers.
type AssetMetadata struct {
	ContractID string
	Symbol     string
	Name       string
	Decimals   int32
}

// PendingUserPosition retains a Blend user's raw, not-yet-resolved positions
// ScVal so the decoder can stay a stateless pure reducer: user positions are
// keyed by reserve index in the raw blob and only resolve to assets against a
// pool's reserve map when the state is built, so the blob must survive the
// prior->next round-trip to be re-resolved when reserves change. It is the one
// piece of builder scratch the typed slices above cannot represent, so it is
// carried here explicitly rather than hidden behind an opaque handle.
type PendingUserPosition struct {
	Address        string
	PoolContractID string
	PositionsXDR   string // base64 ScVal of the user's positions map
	// Archived marks a Positions entry that went not-live via TTL lapse or
	// network-level eviction rather than an explicit on-chain delete (a real
	// LedgerEntryRemoved change). Soroban archival is restorable — the holder
	// still owns the position, it has just fallen off the live footprint — so
	// the entry is kept (with its last known PositionsXDR) instead of purged.
	// An explicit delete still clears the entry exactly as before. Cleared
	// back to false the next time a live Positions write for this user is decoded.
	// ArchivedLedgerSeq is the ledger the lapse was observed on, zero when not
	// archived. Additive field: a snapshot from before this existed decodes
	// with Archived=false, matching prior behavior for every entry it already
	// carried (none of which could have been archived under the old code).
	Archived          bool
	ArchivedLedgerSeq int64
}
