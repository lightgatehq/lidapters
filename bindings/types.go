// Package bindings holds the protocol-agnostic seam between the relay's
// projector edge and a protocol adapter: the ProtocolAdapter interface, the
// raw-input envelopes it consumes (RawEventEnvelope, ContractDataChange), the
// gold output rows it emits (TransformOutput and friends), and the
// config-persistence inversion-of-control types (config.go).
//
// One deliberate transitional coupling: LedgerState is the seam's state
// carrier (it is named in the ProtocolAdapter signatures), but its original
// slices are Blend-shaped, so this package imports blend/contracts for those
// member types and enums. Genericization happens additively, one slice family
// at a time, as real protocols demand it: the AMM* slices came with the first
// AMM protocol, the Vault* slices with the first CDP-style vault protocol
// (FXDAO). Each family sits beside the older slices so existing checkpoint
// JSON remains decodable.
package bindings

import (
	"time"

	"github.com/lightgatehq/lidapters/blend/contracts"
)

type RawEventEnvelope struct {
	SchemaVersion string
	MessageType   string
	Subject       string
	LedgerSeq     int64
	TxHash        string
	EventIndex    int
	ContractID    string
	Topic         string
	RawEvent      []byte
	SourceName    string
	CloseTime     time.Time
	Metadata      map[string]string
}

type LedgerState struct {
	Pools     []contracts.PoolState
	Users     []contracts.UserReservePosition
	Backstops []contracts.BackstopPosition
	// PendingUserPositions carries raw decode scratch across ledgers so the
	// decoder can stay a stateless pure reducer: anything that must survive from
	// one ledger to the next rides in this returned value rather than on the
	// decoder, which is what keeps repeated runs byte-identical. See
	// contracts.PendingUserPosition. Carried state only — never emitted to gold.
	PendingUserPositions []contracts.PendingUserPosition
	// Oracles carries each price oracle's decoded asset->index map, decimals and
	// per-index raw prices across ledgers. The oracle's instance entry (which
	// holds the asset list + decimals) is written once at deploy, not on each
	// set_price, and a price entry only appears in the ledger it changes — so
	// without carrying this, any ledger after the deploy would rebuild an empty
	// oracle and reserves would lose their price (the index map would be empty
	// and price-only ledgers would map nothing). It is the oracle analog of
	// PendingUserPositions. Carried state only — never emitted to gold.
	Oracles []contracts.OracleState
	// PriceFeeds carries each registered Reflector price feed's decoded asset
	// list and recent rounds, and OracleAggregators carries each Blend
	// oracle-aggregator's instance configuration. Mainnet pools name aggregator
	// VIEW contracts as their oracle — contracts that never write prices; the
	// real writes are the feeds' rounds, and the aggregator config is what maps
	// a pool reserve onto them. Both are carried state only — never emitted to
	// gold; each ledger the fold re-synthesizes the aggregator's prices from
	// them into the same oracle representation the resolveOraclePrices path
	// already consumes.
	PriceFeeds        []contracts.PriceFeedState
	OracleAggregators []contracts.OracleAggregatorState
	// Assets carries each registered token contract's decoded human-readable
	// identity (SAC AssetInfo or SEP-41 METADATA). Like the oracle instance, a
	// token's identity entry is written once at deploy and never re-emitted, so
	// without carrying this, any ledger after the deploy would lose the decoded
	// symbol/name/decimals. Carried state only — never emitted to gold; it feeds
	// Reserve.Metadata / Activity.AssetSymbol in the transform instead.
	Assets []contracts.AssetMetadata
	// Auctions carries each live auction's decoded state (the pool's
	// Auction(AuctionKey) temporary entries). An auction entry only appears in
	// the ledger it changes, so without carrying it here every later ledger
	// would forget the auction while it is still live on-chain. Removed the
	// ledger the entry goes not-live (filled/deleted/TTL-lapsed — temporary
	// storage, so not-live means gone).
	Auctions []contracts.AuctionState
	// UserEmissions carries each user's per-reserve-token emission accrual
	// (the pool's UserEmis(UserReserveKey) entries). Same carry requirement as
	// Auctions: the entry is only written when the user's accrual checkpoints,
	// so it must ride here to survive to the next ledger.
	UserEmissions []contracts.UserEmissionState
	// QueuedReserves carries each pending ResInit(Address) entry (the
	// time-locked reserve-parameter change). Same carry requirement as
	// Auctions: the entry is only written when queued/cancelled/executed.
	QueuedReserves []contracts.QueuedReserveState
	// BackstopInstances carries each backstop contract's decoded identity
	// (instance addresses + reward zone + drop list). Written at deploy and
	// rarely re-emitted, so it must ride here — the backstop analog of the
	// Oracles carry.
	BackstopInstances []contracts.BackstopInstanceState
	// AMMPools and AMMPositions are the protocol-neutral state carrier for
	// share-based and concentrated-liquidity AMMs. They intentionally sit beside
	// the legacy Blend slices so existing checkpoints remain JSON-compatible.
	AMMPools     []AMMPoolState
	AMMPositions []AMMPositionState
	AMMAssets    []AMMAssetMetadata
	// Vaults and VaultsInfo are the protocol-neutral state carrier for
	// CDP-style vault protocols (collateral/debt positions keyed by
	// (account, denomination) on one vaults contract; FXDAO is the first).
	// Like the AMM slices, they intentionally sit beside the legacy Blend
	// slices so existing checkpoints remain JSON-compatible.
	Vaults     []VaultState
	VaultsInfo []VaultsInfoState
}

// VaultState is one on-chain vault: a single (account, denomination) pair on
// one vaults contract. All quantity fields are raw decimal strings (u128-
// safe); an absent field is "" — never a fabricated zero.
type VaultState struct {
	Protocol     string
	ContractID   string
	Account      string
	Denomination string
	// IndexRaw is the vault's on-chain sort index (collateralization order).
	IndexRaw      string
	CollateralRaw string
	DebtRaw       string
	// HadVault is sticky lifecycle state, the vault analog of
	// AMMPositionState.HadShares: true once the fold has observed the vault
	// live. It distinguishes "vault that closed" (Closed && HadVault, which
	// deserves a terminal closed row) from "vault never seen live".
	HadVault bool
	// Closed marks a genuine on-chain deletion of the vault entry this ledger
	// or earlier. A TTL lapse / archival is NOT a close: archived entries
	// restore with their bytes intact and restore-then-write is the normal
	// wake-up sequence for long-idle vaults.
	Closed bool
	// Restored is true when the vault's most recent write was a
	// LedgerEntryRestored change — the raw change variant is preserved so
	// downstream consumers can tell a restore-write from a plain update.
	// A restored entry is LIVE.
	Restored bool
}

// VaultsInfoState is one denomination's aggregate vault bookkeeping from the
// vaults contract instance (per-denomination totals and rate parameters).
type VaultsInfoState struct {
	Protocol           string
	ContractID         string
	Denomination       string
	TotalVaultsRaw     string
	TotalDebtRaw       string
	TotalColRaw        string
	MinColRateRaw      string
	MinDebtCreationRaw string
	OpeningColRateRaw  string
}

type AMMAssetMetadata struct {
	ContractID string
	Symbol     string
	Name       string
	Decimals   int32
}

type AMMTokenReserve struct {
	AssetID    string
	Decimals   int32
	ReserveRaw string
}

type AMMPoolState struct {
	Protocol               string
	RouterContract         string
	PoolHash               string
	ContractID             string
	WasmHash               string
	PoolType               string // constant_product | stable | concentrated
	Tokens                 []AMMTokenReserve
	TotalSharesRaw         string
	FeeFractionRaw         string
	ProtocolFeeFractionRaw string
	AmplificationRaw       string
	TickSpacing            int32
	SqrtPriceX96           string
	CurrentTick            int32
	ActiveLiquidityRaw     string
	FeeGrowthGlobal0X128   string
	FeeGrowthGlobal1X128   string
	// Aquarius checkpointed AQUA rewards (PoolRewardConfig/PoolRewardData and
	// WorkingSupply instance keys). RewardTpsRaw is the emission rate per
	// second; RewardAccumulatedRaw is the pool's cumulative accrued reward
	// total at RewardLastTimeRaw; accrual between checkpoints advances it by
	// tps * elapsed, capped at RewardExpiredAtRaw. WorkingSupplyRaw is the
	// pool's total reward weight (sum of working balances).
	RewardTpsRaw         string
	RewardExpiredAtRaw   string
	RewardAccumulatedRaw string
	RewardLastTimeRaw    string
	WorkingSupplyRaw     string
	RewardTokenID        string
}

type AMMPositionState struct {
	Address                  string
	PoolContractID           string
	SharesRaw                string
	TickLower                int32
	TickUpper                int32
	LiquidityRaw             string
	SqrtPriceLowerX96        string
	SqrtPriceUpperX96        string
	Principal0Raw            string
	Principal1Raw            string
	PendingFee0Raw           string
	PendingFee1Raw           string
	RewardTokenID            string
	PendingRewardRaw         string
	WeightedLiquidityRaw     string
	RewardCheckpointEligible bool
	// HadShares is sticky lifecycle state: it flips true the first time the
	// fold observes a nonzero share balance for this position and never flips
	// back. It distinguishes "position that closed" (HadShares && shares==0,
	// which deserves component tombstones) from "position that never existed"
	// (shares==0 without HadShares, which must stay silent).
	HadShares bool
	// WorkingBalanceRaw is the pool's checkpointed reward weight for this user
	// (the Aquarius WorkingBalance entry). It is NOT necessarily the raw share
	// count: ICE boost scales it within [0.4x, 1.0x] of the deposit. Reward
	// accrual uses this weight; LP principal decomposition uses SharesRaw.
	WorkingBalanceRaw string
	// RewardPoolAccumulatedRaw is the user checkpoint's copy of the pool's
	// accumulated reward total (UserRewardData.pool_accumulated) — the point
	// from which the user's unclaimed accrual is measured. PendingRewardRaw
	// is the checkpointed to_claim from the same entry.
	RewardPoolAccumulatedRaw string
}

// ContractDataChange is the shared vocabulary between the relay's
// protocol-agnostic projector edge (which extracts these from raw ledger meta)
// and a protocol adapter's DecodeState (which folds them into typed state). It
// is the contract_data delta for one ledger entry. The silver-only hash/JSON
// debug fields the relay's prior struct carried are dropped here, since their
// only consumer (a debug writer) is gone.
type ContractDataChange struct {
	ContractID string
	KeyXDR     string  // base64 ScVal
	ValueXDR   *string // base64 ScVal; nil when removed/evicted
	Durability string
	ChangeType string // Created/Updated/Restored/Removed
	Live       bool
	// LiveUntilLedgerSeq is the TTL-liveness signal: the ledger up to which this
	// entry is live. The relay extract populates it from the close-meta TtlEntry
	// fold; DecodeState treats *LiveUntilLedgerSeq < ledgerSeq as expired. nil
	// means no TTL signal was attached. On Soroban an entry's data and its TTL
	// are separate ledger entries, so without carrying the TTL here expired state
	// would read as live forever.
	LiveUntilLedgerSeq *uint32
	LastModifiedLedger uint32
}

// ProtocolAdapter is the seam the relay's protocol-agnostic projector consumes
// and each protocol adapter implements. It folds the decode half into the older
// ID/Protocol/Transform interface so a protocol is fully self-contained: event
// decode, state decode, and transform all live in the adapter rather than being
// split between the adapter and the relay core.
type ProtocolAdapter interface {
	ID() string
	Protocol() string

	// OwnsContract reports whether a contract_data change / event for contractID
	// belongs to this protocol. It subsumes the relay router contract-match +
	// protocol classification, which happens consumer-side, inside the adapter.
	OwnsContract(contractID string) bool

	// DecodeState folds this protocol's contract_data changes into typed state.
	//
	// Decode is adapter-owned: keeping protocol decode in the adapter (rather
	// than in the relay core) is what makes each protocol self-contained — event
	// decode, state decode, and transform in one place.
	//
	// It is a PURE reducer — (prior, changes, ledgerSeq) -> next, with no
	// DB/network/clock/random: folding the same input twice yields
	// byte-identical output, and all carry-over threads through *LedgerState
	// (PendingUserPositions carries the one piece of raw scratch that does not
	// otherwise round-trip).
	//
	// An adapter MAY internally carry a mirror of its own last returned state as
	// a performance optimization (blend's incremental state mode does), provided
	// the functional contract is preserved: the carried mirror is only trusted
	// when prior IS the adapter's own previous return value, any other prior
	// reseeds from it, and the returned state is treated by both sides as
	// immutable. Such an adapter is not shareable across concurrent folds; the
	// default blend mode (paranoid) remains fully stateless.
	DecodeState(prior *LedgerState, changes []ContractDataChange, ledgerSeq int64) (*LedgerState, error)

	// Transform folds events + typed state into gold. Pure; unchanged by the fold.
	Transform(input TransformInput) (*TransformOutput, error)
}

// DirtyKind classifies one entry of a per-ledger dirty-positions set: whether
// the (address, pool) pair still has positions after the fold (Upsert) or its
// Positions entry was explicitly removed on-chain this ledger (Removal). A
// TTL lapse or network eviction is reported as Upsert, not Removal — Change 1
// (see blend/state.go's applyDelete) archives rather than deletes those, so
// the entry still has positions (flagged Archived); only a real
// LedgerEntryRemoved change is a Removal.
type DirtyKind string

const (
	DirtyUpsert  DirtyKind = "upsert"
	DirtyRemoval DirtyKind = "removal"
)

// DirtyPosition is one (address, pool) pair whose position changed on the
// ledger just folded, plus the kind of change. See DirtyPositionsProvider.
type DirtyPosition struct {
	Address        string
	PoolContractID string
	Kind           DirtyKind
}

// DirtyPositionsProvider is an additive capability an adapter MAY implement
// alongside ProtocolAdapter: after a DecodeState/DecodeStateAt call, it
// reports exactly which (address, pool) position pairs that ledger's changes
// touched, and whether each was an upsert or a tombstone removal. It is the
// per-ledger analog of re-scanning the whole LedgerState.Users slice for
// diffs: a consumer computing per-ledger emission projects ONLY the dirty
// pairs (see a protocol-specific single-user projection helper, e.g. blend's
// Adapter.ProjectPositions) instead of every user in state, dropping the cost
// from O(all users) to O(dirty users).
//
// LastDirtyPositions reflects the most recent DecodeState/DecodeStateAt call
// on that adapter instance and is overwritten by the next one — same
// single-fold-at-a-time contract as the incremental state mode (see
// ProtocolAdapter.DecodeState's doc on carried mirrors): do not share one
// adapter across concurrent folds and read this immediately after folding,
// before the next ledger.
type DirtyPositionsProvider interface {
	LastDirtyPositions() []DirtyPosition
}

// Required DecodeDiagnostic reason codes. An unmapped index has no configured
// reserve behind it (the cold-start/bounded-replay case: the reserve's
// ResConfig never folded, so its true index is unknown); a duplicate index has
// two or more configured reserves claiming it, so resolving either way would
// misattribute.
const (
	DecodeDiagnosticUnmappedReserveIndex  = "unmapped_reserve_index"
	DecodeDiagnosticDuplicateReserveIndex = "duplicate_reserve_index"
)

// DirtyBackstop is one (address, pool) backstop position whose valuation
// inputs changed on the ledger just folded: the holder's own UserBalance or
// its sibling UEmisData, the pool's PoolBalance (every holder's shares<->LP
// conversion moves), a Comet LP reserve/supply write on the pool's BToken
// contract, or a price change for the backstop's BLND/USDC legs. It is the
// backstop analog of DirtyPosition, exposed separately because a lending
// reserve tick must never fan out to backstop holders and a Comet/price tick
// must never fan out to unrelated lending positions — a consumer doing
// per-ledger emission projects ONLY these pairs (O(affected holders)) instead
// of re-emitting every backstop position in state.
type DirtyBackstop struct {
	Address        string
	PoolContractID string
	Kind           DirtyKind
}

// DirtyBackstopsProvider is an additive capability an adapter MAY implement
// alongside ProtocolAdapter: after a DecodeState/DecodeStateAt call, it
// reports exactly which (address, pool) backstop pairs that ledger's changes
// invalidated, sorted by (address, pool). Same single-fold-at-a-time contract
// as DirtyPositionsProvider: the set reflects the most recent fold, is
// overwritten by the next one, and must be read immediately after folding,
// before the next ledger.
type DirtyBackstopsProvider interface {
	LastDirtyBackstops() []DirtyBackstop
}

// DecodeDiagnostic is one non-zero position leg the fold skipped because its
// reserve index could not be resolved to exactly one configured reserve.
// Skipping is financially safer than guessing (a wrong-but-plausible row is
// strictly worse than a missing-and-counted one in a position store downstream
// health-factor math consumes), but a silent skip is not acceptable: each
// record names the ledger, pool, user, position type, raw reserve index, raw
// amount, and the sorted candidate assets — the unknown-index reserves for an
// unmapped index, the claiming reserves for a duplicate.
type DecodeDiagnostic struct {
	Code              string
	LedgerSeq         int64
	PoolContractID    string
	Address           string
	PositionType      string
	ReserveIndex      int32
	AmountRaw         string
	CandidateAssetIDs []string // sorted; empty when no candidate exists
}

// DecodeDiagnosticsProvider is an additive capability an adapter MAY implement
// alongside ProtocolAdapter: after a DecodeState/DecodeStateAt call, it
// reports the position legs that fold skipped as unresolvable, sorted by
// (code, pool, address, position type, reserve index, amount, candidates).
// Same single-fold-at-a-time contract as DirtyPositionsProvider: the set
// reflects the most recent fold, is overwritten by the next one, and must be
// read immediately after folding, before the next ledger.
type DecodeDiagnosticsProvider interface {
	LastDecodeDiagnostics() []DecodeDiagnostic
}

// TemporaryStateKind names the temporary-storage entity families whose
// per-ledger transitions the fold exposes (see TemporaryStateChange): pool
// auctions (Auction(AuctionKey) entries) and queued reserves (ResInit
// entries). Both are temporary storage on-chain: the entry only appears in
// the ledger it changes, and any not-live change removes it.
type TemporaryStateKind string

const (
	TemporaryAuction       TemporaryStateKind = "auction"
	TemporaryQueuedReserve TemporaryStateKind = "queued_reserve"
)

// TemporaryStateChange is one auction or queued-reserve identity whose
// on-chain entry the ledger just folded created/updated (DirtyUpsert) or
// removed (DirtyRemoval). The identity comes from the changed ledger key, so
// a removal is reportable even when the replay never observed the entry's
// create (the bounded-replay case) — no comparison of complete previous and
// current state slices is involved. For auctions the identity is
// (PoolContractID, UserAddress, AuctionType); for queued reserves it is
// (PoolContractID, AssetID). The fields of the other kind are zero.
type TemporaryStateChange struct {
	Kind           TemporaryStateKind
	Action         DirtyKind
	PoolContractID string
	UserAddress    string // auction only
	AuctionType    int32  // auction only
	AssetID        string // queued reserve only
}

// TemporaryStateChangesProvider is an additive capability an adapter MAY
// implement alongside ProtocolAdapter: after a DecodeState/DecodeStateAt
// call, it reports exactly which auction/queued-reserve identities that
// ledger's changes touched, sorted by (kind, pool, user, auction type,
// asset). Same single-fold-at-a-time contract as DirtyPositionsProvider: the
// set reflects the most recent fold, is overwritten by the next one, and must
// be read immediately after folding, before the next ledger.
type TemporaryStateChangesProvider interface {
	LastTemporaryStateChanges() []TemporaryStateChange
}

// AuctionLifecycle is one per-ledger auction transition surfaced to gold:
// Active=true carries the live auction verbatim (create, update, or restore
// observed); Active=false carries stable identity only (the entry went
// not-live — filled, deleted, or TTL-lapsed; the adapter cannot distinguish
// the business outcome from a lone removed storage entry, so it does not
// invent one). Additive beside the established full-state Auctions slice.
type AuctionLifecycle struct {
	Auction
	Active bool
}

// QueuedReserveLifecycle is the queued-reserve twin of AuctionLifecycle:
// Active=true carries the pending change verbatim; Active=false carries
// stable identity only (executed, cancelled, or lapsed — indistinguishable
// from a lone removed entry). Additive beside the full-state QueuedReserves
// slice.
type QueuedReserveLifecycle struct {
	QueuedReserve
	Active bool
}

// CloseTimeStateDecoder is the close-time-aware variant of DecodeState. The
// ledger close time comes from the same close-meta the changes were extracted
// from — it is fold INPUT, not a clock read, so purity holds: (prior, changes,
// ledgerSeq, closeTime) -> next is still a deterministic function of bronze.
// An adapter implements it when some decode semantics depend on ledger time —
// the Blend oracle-aggregators' MaxAge staleness window is the motivating case
// (a price older than MaxAge at the folding ledger's close must resolve to
// nothing, exactly as the aggregator's own lastprice would). Hosts that have
// the close time (the relay projector decodes it from the raw meta anyway)
// prefer this over DecodeState when the adapter provides it.
type CloseTimeStateDecoder interface {
	DecodeStateAt(prior *LedgerState, changes []ContractDataChange, ledgerSeq int64, closeTime time.Time) (*LedgerState, error)
}

type TransformInput struct {
	LedgerSeq int64
	CloseTime time.Time
	Events    []RawEventEnvelope
	State     *LedgerState
	// PriorPositions carries the previous ledger's gold Position output,
	// enabling the adapter to diff and emit tombstones for legs that
	// disappeared (went to zero or were evicted). May be nil on the first
	// ledger or when the relay does not support tombstone emission.
	PriorPositions []Position
	// PriorSummaries carries the previous ledger's gold PositionSummary
	// output for the same diff-and-tombstone purpose. May be nil.
	PriorSummaries []PositionSummary
}

// PositionTombstone marks a position leg that should be marked as deleted
// in Gold at this ledger. The relay inserts a tombstone row with
// is_deleted=TRUE at LedgerSeq so the current-view (filtered by NOT
// is_deleted) no longer shows the phantom last nonzero value.
type PositionTombstone struct {
	Address      string
	Protocol     string
	ContractID   string
	AssetID      string
	PositionType string
	LedgerSeq    int64
}

// SummaryTombstone marks a per-account summary that should be marked as
// deleted in Gold at this ledger. Emitted only when the address has no
// remaining Blend reserve or backstop positions of any kind.
type SummaryTombstone struct {
	Address   string
	Protocol  string
	LedgerSeq int64
}

type Activity struct {
	ID           string
	LedgerSeq    int64
	TxHash       string
	EventIndex   int
	ContractID   string
	Address      string
	Protocol     string
	ActivityType contracts.ActivityType
	AssetID      string
	AmountRaw    string
	ShareAmount  string
	ShareType    string
	AssetSymbol  string
	Counterparty string
	USDValue     string
	Direction    string
	Timestamp    time.Time
	Metadata     map[string]string
}

type BackstopQueueEntry struct {
	Amount   string
	UnlockAt time.Time
}

type Position struct {
	ID           string
	Address      string
	Protocol     string
	ContractID   string
	AssetID      string
	PositionType contracts.PositionType
	ShareAmount  string
	AssetAmount  string
	USDValue     string
	APY          string
	LedgerSeq    int64
	Timestamp    time.Time
	Metadata     map[string]string
	Q4WEntries   []BackstopQueueEntry
}

type PositionSummary struct {
	ID                     string
	Address                string
	Protocol               string
	HealthFactor           string
	BorrowLimitPct         string
	BorrowCapUSD           string
	DepositedUSD           string
	BorrowedUSD            string
	EffectiveCollateralUSD string
	EffectiveLiabilityUSD  string
	NetAPY                 string
	NetAPYWeightUSD        string
	LiquidationPrice       string
	LedgerSeq              int64
	Timestamp              time.Time
	Metadata               map[string]string
	StructuredMetadata     map[string]any
}

type Reserve struct {
	ID             string
	Protocol       string
	ContractID     string
	AssetID        string
	TotalSupplied  string
	TotalBorrowed  string
	Utilization    string
	SupplyAPY      string
	BorrowAPY      string
	SupplyCap      string
	BorrowCap      string
	CFactor        string
	LFactor        string
	OracleContract string
	LedgerSeq      int64
	Timestamp      time.Time
	Metadata       map[string]string
}

// Backstop is the pool-level twin of Reserve: the aggregate backstop capital
// protecting one pool (every depositor's shares/tokens summed, plus the
// aggregate queued-for-withdrawal fraction), not a single user's deposit — a
// user's own backstop position already rides on Position with
// PositionType=backstop. USDValue follows the same nullable-until-priced rule
// as Reserve/Position: empty until LP-token pricing (BLND/USDC component +
// oracle price) is wired, never a fabricated zero.
type Backstop struct {
	ID               string
	Protocol         string
	ContractID       string // the pool this backstop protects
	BackstopContract string
	SharesRaw        string
	LPTokensRaw      string
	Q4WSharesRaw     string
	Q4WPct           string
	BLNDAmountRaw    string
	USDCAmountRaw    string
	USDValue         string
	LedgerSeq        int64
	Timestamp        time.Time
	Metadata         map[string]string
}

// ReserveEmission is one side (supply/b-token or borrow/d-token) of a
// reserve's emission config, keyed like Reserve plus Side. APY is "" when it
// cannot be derived (e.g. no emitted-token price feed) — never a fabricated
// value. A side with no active emission config is simply absent from the
// slice, not emitted with a zero EPS. IndexRaw / LastTimeRaw are the side's
// accrual checkpoint (ReserveEmissionData index, 14 decimals, and last_time
// unix seconds); "" when the accrual half is absent on-chain.
type ReserveEmission struct {
	ID          string
	Protocol    string
	ContractID  string
	AssetID     string
	Side        string // "supply" | "borrow"
	EPSRaw      string
	Expiration  time.Time
	APY         string
	IndexRaw    string
	LastTimeRaw string
	LedgerSeq   int64
	Timestamp   time.Time
	Metadata    map[string]string
}

// Auction is one live auction surfaced to gold: structured liquidation /
// bad-debt / interest auction state, previously visible only as a coarse
// activity. AuctionType is the label form of the contract's enum
// (user_liquidation | bad_debt | interest). Lot and Bid carry the full
// per-asset maps; their unit depends on the auction type (a user
// liquidation's lot is bTokens, bid is dTokens).
type Auction struct {
	ID          string
	Protocol    string
	ContractID  string // the pool running the auction
	UserAddress string // the address whose assets are being auctioned
	AuctionType string
	Block       int64
	Lot         []AuctionAmount
	Bid         []AuctionAmount
	LedgerSeq   int64
	Timestamp   time.Time
	Metadata    map[string]string
}

// AuctionAmount is one asset's amount within an auction's lot or bid.
type AuctionAmount struct {
	AssetID   string
	AmountRaw string
}

// QueuedReserve is one pending, time-locked reserve-parameter change
// surfaced to gold: the pool's ResInit entry — a "params about to change"
// signal. NewConfig carries the queued ReserveConfig fields verbatim as raw
// strings, only the ones present on-chain.
type QueuedReserve struct {
	ID            string
	Protocol      string
	ContractID    string // the pool
	AssetID       string
	UnlockTimeRaw string // unix seconds, raw
	UnlockTime    time.Time
	NewConfig     map[string]string
	LedgerSeq     int64
	Timestamp     time.Time
	Metadata      map[string]string
}

// UserEmission is one user's per-reserve-token emission accrual surfaced to
// gold: the checkpointed unclaimed BLND (AccruedRaw) plus the user's last
// accrued index (IndexRaw, 14 decimals) for one reserve side. AssetID and
// Side are resolved from the pool's reserve list when known; AssetID is ""
// when the reserve index cannot be resolved yet (the raw ReserveTokenID is
// always present). An absent on-chain entry is absent here — never
// zero-filled.
type UserEmission struct {
	ID             string
	Protocol       string
	ContractID     string // the pool
	Address        string
	AssetID        string
	ReserveTokenID int32
	Side           string // "supply" | "borrow"
	IndexRaw       string
	AccruedRaw     string
	LedgerSeq      int64
	Timestamp      time.Time
	Metadata       map[string]string
}

type Contract struct {
	ID              string
	Address         string
	Protocol        string
	ContractType    string
	Status          string
	WasmHash        string
	FirstSeenLedger int64
	LastSeenLedger  int64
	Metadata        map[string]string
}

type QuarantineEvent struct {
	ID         string
	AdapterID  string
	LedgerSeq  int64
	TxHash     string
	EventIndex int
	ContractID string
	Reason     string
	RawEvent   []byte
	Metadata   map[string]string
}

type TransformOutput struct {
	LedgerSeq        int64
	Activities       []Activity
	Positions        []Position
	Summaries        []PositionSummary
	Reserves         []Reserve
	ReserveEmissions []ReserveEmission
	UserEmissions    []UserEmission
	Auctions         []Auction
	QueuedReserves   []QueuedReserve
	// AuctionLifecycle / QueuedReserveLifecycle carry the per-ledger
	// transition rows projected from a TemporaryStateChange set (see
	// TemporaryStateChangesProvider and the adapter's
	// ProjectTemporaryStateChanges): active rows with payload, inactive rows
	// with stable identity only. They are additive — the established
	// full-state Auctions/QueuedReserves slices above keep their meaning.
	AuctionLifecycle       []AuctionLifecycle
	QueuedReserveLifecycle []QueuedReserveLifecycle
	Backstops              []Backstop
	Contracts              []Contract
	Quarantine             []QuarantineEvent
	AMMPools               []AMMPool
	AMMComponents          []AMMPositionComponent
	AMMRewards             []AMMReward
	Vaults                 []Vault
	PositionTombstones     []PositionTombstone
	SummaryTombstones      []SummaryTombstone
}

// Vault is one vault snapshot row for gold: the (address, denomination) pair
// on one vaults contract, with the raw on-chain amounts. Status is 'active'
// (entry live or archived-but-owned on-chain) or 'closed' (the entry was
// genuinely deleted this ledger or earlier) — the closed row IS the vault's
// tombstone: current-state views project the latest row per identity, so a
// terminal 'closed' snapshot retires the vault without a second write path.
// Quantity fields are "" when absent on-chain (a deleted vault's terminal row
// carries no amounts); USD valuations stay "" unless a confirmed price source
// exists — honest-null, never fabricated.
type Vault struct {
	ID            string
	Address       string
	Protocol      string
	ContractID    string
	Denomination  string
	VaultIndexRaw string
	CollateralRaw string
	DebtRaw       string
	// RatioRaw is the contract's own deposit-ratio math
	// (currency_rate * collateral / debt, raw scale); "" when the oracle rate
	// needed to compute it is unavailable.
	RatioRaw      string
	CollateralUSD string
	DebtUSD       string
	Status        string // "active" | "closed"
	LedgerSeq     int64
	Timestamp     time.Time
	Metadata      map[string]string
}

type AMMPool struct {
	Protocol               string
	RouterContract         string
	PoolHash               string
	ContractID             string
	PoolType               string
	WasmHash               string
	Tokens                 []AMMTokenReserve
	TotalSharesRaw         string
	FeeFractionRaw         string
	ProtocolFeeFractionRaw string
	AmplificationRaw       string
	TickSpacing            int32
	SqrtPriceX96           string
	CurrentTick            int32
	ActiveLiquidityRaw     string
	LedgerSeq              int64
	Timestamp              time.Time
	Metadata               map[string]string
}

type AMMPositionComponent struct {
	ID              string
	PositionGroupID string
	Address         string
	Protocol        string
	PoolContractID  string
	ComponentKind   string // lp_principal | range_principal | unclaimed_fee
	AssetID         string
	AmountRaw       string
	ShareAmountRaw  string
	TickLower       *int32
	TickUpper       *int32
	USDValue        string
	APR             string
	LedgerSeq       int64
	Timestamp       time.Time
	Metadata        map[string]string
}

type AMMReward struct {
	ID              string
	PositionGroupID string
	Address         string
	Protocol        string
	PoolContractID  string
	RewardContract  string
	RewardTokenID   string
	RewardKind      string // aqua | incentive
	AmountRaw       string
	USDValue        string
	LedgerSeq       int64
	Timestamp       time.Time
	Metadata        map[string]string
}

// StateStats lets the relay report health without inspecting protocol-specific
// state slices.
type StateStats struct{ Pools, Users, Backstops int }

type StateReporter interface{ StateStats(*LedgerState) StateStats }

// AssetRegistrar lets the relay register token contracts discovered in prior
// folded state without knowing a protocol's state layout.
type AssetRegistrar interface {
	RegisterAssetContracts(ids ...string)
	StateAssetContracts(*LedgerState) []string
}
