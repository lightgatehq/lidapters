// Package bindings holds the protocol-agnostic seam between the relay's
// projector edge and a protocol adapter: the ProtocolAdapter interface, the
// raw-input envelopes it consumes (RawEventEnvelope, ContractDataChange), the
// gold output rows it emits (TransformOutput and friends), and the
// config-persistence inversion-of-control types (config.go).
//
// One deliberate transitional coupling: LedgerState is the seam's state
// carrier (it is named in the ProtocolAdapter signatures), but its slices are
// still Blend-shaped, so this package imports blend/contracts for the member
// types and enums. Genericizing LedgerState is deferred until a second real
// protocol needs its own state shape.
package bindings

import (
	"time"

	"github.com/daccred/lidapters/blend/contracts"
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

// FullStateDecoder is an additive capability an adapter MAY implement
// alongside CloseTimeStateDecoder: an explicit full-build variant that always
// materializes the complete Users/PendingUserPositions slices on the returned
// state, regardless of the adapter's active fold strategy.
//
// It exists because DecodeStateAt's default cost changed (see issue #67,
// relay.lightgate.xyz): an adapter carrying a persistent per-ledger mirror
// (blend's incremental state mode) may skip materializing those two slices on
// an ordinary DecodeStateAt call, since a per-ledger consumer following the
// dirty-positions set (DirtyPositionsProvider) never reads them. A host that
// needs the complete slices — writing a fold checkpoint, cold-start hydration,
// or any other full-state read-back — calls DecodeStateFullAt instead, at
// exactly the boundaries where that cost is unavoidable, rather than paying it
// on every ledger.
type FullStateDecoder interface {
	DecodeStateFullAt(prior *LedgerState, changes []ContractDataChange, ledgerSeq int64, closeTime time.Time) (*LedgerState, error)
}

type TransformInput struct {
	LedgerSeq int64
	CloseTime time.Time
	Events    []RawEventEnvelope
	State     *LedgerState
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
// slice, not emitted with a zero EPS.
type ReserveEmission struct {
	ID         string
	Protocol   string
	ContractID string
	AssetID    string
	Side       string // "supply" | "borrow"
	EPSRaw     string
	Expiration time.Time
	APY        string
	LedgerSeq  int64
	Timestamp  time.Time
	Metadata   map[string]string
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
	Backstops        []Backstop
	Contracts        []Contract
	Quarantine       []QuarantineEvent
}
