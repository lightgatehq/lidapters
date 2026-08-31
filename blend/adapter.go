package blend

import (
	"fmt"
	"sort"
	"time"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/lightgatehq/lidapters/blend/contracts"
	"github.com/stellar/go-stellar-sdk/strkey"
)

// Adapter satisfies bindings.ProtocolAdapter: event decode (Transform),
// state decode (DecodeState, in state.go), and ownership (OwnsContract).
var _ bindings.ProtocolAdapter = (*Adapter)(nil)

// Adapter also owns its low-frequency config across process restarts: it declares
// the storage schema, emits config records, and rehydrates the seed state. See
// config_state.go.
var _ bindings.ConfigStateful = (*Adapter)(nil)

// Adapter decodes time-dependent oracle state (the aggregators' MaxAge
// staleness window) when the host supplies the ledger close time. See
// state_reflector.go.
var _ bindings.CloseTimeStateDecoder = (*Adapter)(nil)

// Adapter exposes the per-ledger dirty-positions set from its most recent
// DecodeState/DecodeStateAt call, so a consumer doing per-ledger emission can
// project only the touched (address, pool) pairs instead of every user in
// state. See bindings.DirtyPositionsProvider and Adapter.ProjectPositions
// (math.go).
var _ bindings.DirtyPositionsProvider = (*Adapter)(nil)

// Adapter exposes the per-ledger affected-backstop set from its most recent
// DecodeState/DecodeStateAt call, so a consumer doing dirty-mode emission can
// re-emit exactly the invalidated backstop positions (O(affected holders))
// instead of stripping them wholesale. See bindings.DirtyBackstopsProvider.
var _ bindings.DirtyBackstopsProvider = (*Adapter)(nil)

// Adapter exposes the skipped-leg diagnostics from its most recent
// DecodeState/DecodeStateAt call, so a consumer can log and count the position
// legs the fold could not attribute instead of discovering corrupted rows
// after the fact. See bindings.DecodeDiagnosticsProvider.
var _ bindings.DecodeDiagnosticsProvider = (*Adapter)(nil)

// Adapter exposes the auction/queued-reserve transition set from its most
// recent DecodeState/DecodeStateAt call, so a consumer persists per-ledger
// temporary-state lifecycle rows from the changed ledger keys instead of
// diffing complete previous/current state slices. See
// bindings.TemporaryStateChangesProvider and
// Adapter.ProjectTemporaryStateChanges (math.go).
var _ bindings.TemporaryStateChangesProvider = (*Adapter)(nil)

type Adapter struct {
	cfg Config
	// contracts is the owned contract-ID set OwnsContract checks. It is
	// config-like ownership, not per-ledger scratch, so it does not affect the
	// DecodeState purity guarantee. Seeded empty; the relay projector feeds
	// discovered pools via RegisterContracts.
	contracts map[string]struct{}
	// assets is the registered token-contract set: reserve assets the relay edge
	// feeds via RegisterAssetContracts once a pool's reserve list reveals them.
	// It is checked ahead of the generic pool-instance branch in the reducer so a
	// registered asset's instance entry is always decoded on the SAC/SEP-41 path
	// and never mistaken for a pool — critical for a wasm-backed SEP-41 token,
	// which would otherwise pass the pool branch's wasm-hash sniff. Same
	// config-like status as contracts; does not affect DecodeState purity.
	assets map[string]struct{}
	// feeds is the registered Reflector price-feed set the relay edge fills via
	// RegisterPriceFeeds. A feed's contract_data (per-round price entries plus
	// the instance's asset list) is routed onto the Reflector decode path ahead
	// of every other branch — a feed instance carries a wasm executable and
	// would otherwise be misdecoded as a phantom pool. Same config-like status
	// as contracts; does not affect DecodeState purity.
	feeds map[string]struct{}
	// comets is the registered Comet (BToken) LP contract set the relay edge
	// fills via RegisterCometContracts. A Comet's contract_data (the pool's
	// AllTokenVec/AllRecordData/TotalShares persistent entries) is routed onto
	// the Comet decode path (state_comet.go) ahead of every Blend branch.
	// Deliberately distinct from contracts: Comet state can never decode as
	// Blend pool state and Blend state never as Comet (D-03). Same config-like
	// status as contracts; does not affect DecodeState purity.
	comets map[string]struct{}
	// state is the state-fold strategy DecodeState delegates to, selected once
	// at New from Config.StateMode and swapped as a whole class — paranoid (the
	// stateless reference reducer) or incremental (the persistent-builder
	// optimization). See state_strategy.go for the contract between the two.
	state stateStrategy
	// lastDirty is the dirty-positions set from the most recent
	// DecodeState/DecodeStateAt call, overwritten by the next one. See
	// LastDirtyPositions / bindings.DirtyPositionsProvider.
	lastDirty []bindings.DirtyPosition
	// lastDirtyBackstops is the affected-backstop set from the most recent
	// DecodeState/DecodeStateAt call, overwritten by the next one. See
	// LastDirtyBackstops / bindings.DirtyBackstopsProvider.
	lastDirtyBackstops []bindings.DirtyBackstop
	// lastDiagnostics is the skipped-leg diagnostic set from the most recent
	// DecodeState/DecodeStateAt call, overwritten by the next one. See
	// LastDecodeDiagnostics / bindings.DecodeDiagnosticsProvider.
	lastDiagnostics []bindings.DecodeDiagnostic
	// lastTemporaryChanges is the auction/queued-reserve transition set from
	// the most recent DecodeState/DecodeStateAt call, overwritten by the next
	// one. See LastTemporaryStateChanges /
	// bindings.TemporaryStateChangesProvider.
	lastTemporaryChanges []bindings.TemporaryStateChange
}

// LastDirtyPositions returns the (address, pool) pairs whose position changed
// on the most recent DecodeState/DecodeStateAt call, and whether each change
// was an upsert or a tombstone removal. See bindings.DirtyPositionsProvider.
func (a *Adapter) LastDirtyPositions() []bindings.DirtyPosition {
	return a.lastDirty
}

// LastDirtyBackstops returns the (address, pool) backstop pairs whose
// valuation inputs the most recent DecodeState/DecodeStateAt call invalidated
// — holder balance/emission writes, pool PoolBalance writes, linked Comet
// reserve/supply writes, or BLND/USDC price changes — and whether each pair
// still has a balance after the fold. See bindings.DirtyBackstopsProvider.
func (a *Adapter) LastDirtyBackstops() []bindings.DirtyBackstop {
	return a.lastDirtyBackstops
}

// LastDecodeDiagnostics returns the non-zero position legs the most recent
// DecodeState/DecodeStateAt call skipped because their reserve index was
// unmapped or ambiguous, sorted by (code, pool, address, position type,
// reserve index, amount, candidates). See bindings.DecodeDiagnosticsProvider.
func (a *Adapter) LastDecodeDiagnostics() []bindings.DecodeDiagnostic {
	return a.lastDiagnostics
}

// LastTemporaryStateChanges returns the auction/queued-reserve identities the
// most recent DecodeState/DecodeStateAt call created/updated or removed,
// sorted by (kind, pool, user, auction type, asset). See
// bindings.TemporaryStateChangesProvider.
func (a *Adapter) LastTemporaryStateChanges() []bindings.TemporaryStateChange {
	return a.lastTemporaryChanges
}

func New(cfg Config) (*Adapter, error) {
	merged := DefaultConfig()
	if cfg.AdapterID != "" {
		merged.AdapterID = cfg.AdapterID
	}
	if cfg.Protocol != "" {
		merged.Protocol = cfg.Protocol
	}
	if cfg.V2Scalar != "" {
		merged.V2Scalar = cfg.V2Scalar
	}
	merged.AllowUnknownV2 = cfg.AllowUnknownV2
	merged.StateMode = cfg.StateMode
	merged.V2WasmHashes = map[string]struct{}{}
	for hash := range cfg.V2WasmHashes {
		merged.V2WasmHashes[hash] = struct{}{}
	}

	if merged.AdapterID == "" {
		return nil, fmt.Errorf("adapter id is required")
	}
	if merged.Protocol == "" {
		return nil, fmt.Errorf("protocol is required")
	}
	if merged.V2Scalar == "" {
		return nil, fmt.Errorf("v2 scalar is required")
	}
	a := &Adapter{cfg: merged, contracts: map[string]struct{}{}}
	switch cfg.StateMode {
	case "", StateModeParanoid:
		a.state = &paranoidStrategy{adapter: a}
	case StateModeIncremental:
		a.state = newIncrementalStrategy(a)
	default:
		return nil, fmt.Errorf("unknown state mode %q", cfg.StateMode)
	}
	return a, nil
}

func (a *Adapter) ID() string {
	return a.cfg.AdapterID
}

func (a *Adapter) Protocol() string {
	return a.cfg.Protocol
}

func (a *Adapter) Transform(input bindings.TransformInput) (*bindings.TransformOutput, error) {
	out := &bindings.TransformOutput{
		LedgerSeq:  input.LedgerSeq,
		Activities: make([]bindings.Activity, 0, len(input.Events)),
		Positions:  make([]bindings.Position, 0, 32),
		Summaries:  make([]bindings.PositionSummary, 0, 32),
		Reserves:   make([]bindings.Reserve, 0, 16),
		Contracts:  make([]bindings.Contract, 0, 8),
		Quarantine: make([]bindings.QuarantineEvent, 0, 8),
	}

	for _, evt := range input.Events {
		decoded := decodeEvent(evt)
		if !decoded.isBlend {
			continue
		}
		if decoded.activityType == "" {
			out.Quarantine = append(out.Quarantine, bindings.QuarantineEvent{
				ID:         stableID(a.cfg.AdapterID, fmt.Sprintf("%d", evt.LedgerSeq), evt.TxHash, fmt.Sprintf("%d", evt.EventIndex), "unknown"),
				AdapterID:  a.cfg.AdapterID,
				LedgerSeq:  evt.LedgerSeq,
				TxHash:     evt.TxHash,
				EventIndex: evt.EventIndex,
				ContractID: evt.ContractID,
				Reason:     decoded.reason,
				RawEvent:   evt.RawEvent,
				Metadata:   decoded.metadata,
			})
			continue
		}
		if contractScopedActivity(decoded.activityType) && decoded.address == "" {
			decoded.address = evt.ContractID
		}
		if reason := activityIdentityFailure(decoded, evt); reason != "" {
			out.Quarantine = append(out.Quarantine, bindings.QuarantineEvent{
				ID:         stableID(a.cfg.AdapterID, fmt.Sprintf("%d", evt.LedgerSeq), evt.TxHash, fmt.Sprintf("%d", evt.EventIndex), reason),
				AdapterID:  a.cfg.AdapterID,
				LedgerSeq:  evt.LedgerSeq,
				TxHash:     evt.TxHash,
				EventIndex: evt.EventIndex,
				ContractID: evt.ContractID,
				Reason:     reason,
				RawEvent:   evt.RawEvent,
				Metadata:   decoded.metadata,
			})
			continue
		}
		txHash := evt.TxHash
		eventIndex := evt.EventIndex
		if decoded.activityType == contracts.ActivityTypeStatusChange {
			// Gold's lifecycle_synthetic_identity constraint keys a status change
			// as a per-ledger contract fact, not a per-event one:
			// tx_hash = status:<contract>:<ledger>, event_index = 0. The raw
			// event's tx hash and index would violate the constraint, so emit the
			// synthetic identity (and derive the stable ID from it too, so it stays
			// deterministic regardless of which raw event carried the change).
			txHash = statusChangeTxHash(evt.ContractID, evt.LedgerSeq)
			eventIndex = 0
		}
		out.Activities = append(out.Activities, bindings.Activity{
			ID:           stableID(a.cfg.Protocol, fmt.Sprintf("%d", evt.LedgerSeq), txHash, fmt.Sprintf("%d", eventIndex), string(decoded.activityType)),
			LedgerSeq:    evt.LedgerSeq,
			TxHash:       txHash,
			EventIndex:   eventIndex,
			ContractID:   evt.ContractID,
			Address:      decoded.address,
			Protocol:     a.cfg.Protocol,
			ActivityType: decoded.activityType,
			AssetID:      decoded.assetID,
			AmountRaw:    decoded.amountRaw,
			ShareAmount:  decoded.shareRaw,
			ShareType:    shareTypeForEvent(decoded.eventName, decoded.activityType),
			Counterparty: decoded.counterparty,
			Direction:    decoded.direction,
			Timestamp:    evt.CloseTime,
			Metadata:     decoded.metadata,
		})
	}

	if err := a.computeState(input, out); err != nil {
		return nil, err
	}

	// Emit tombstones for positions that disappeared since the prior ledger.
	// The adapter is stateless across ledgers, so the relay passes the prior
	// ledger's gold Position output via TransformInput.PriorPositions. Any
	// position ID present in PriorPositions but absent from the current output
	// is a leg that went to zero or was evicted — emit a tombstone so the relay
	// can insert an is_deleted=TRUE row at this ledger.
	emitTombstones(input, out)

	return out, nil
}

// ProjectPositions projects ONLY the given dirty (address, pool) pairs' rows
// out of state — the per-ledger-emission analog of Transform's full
// computeState pass, for a consumer that wants just the Positions/Summaries
// Change 1's Positions dirty set touched this ledger (bindings.DirtyPosition,
// Adapter.LastDirtyPositions). It reuses computeState verbatim (no duplicated
// valuation logic): it assembles a filtered LedgerState whose Users slice
// holds only the dirty pairs' rows and calls computeState on it exactly as
// Transform does. state.Pools/Oracles/Assets are left as-is (a reserve still
// needs its pool's full context to value; pool/reserve normalization is
// O(pools), which does not grow with user count).
//
// When the adapter's active state-fold strategy is incremental, gathering the
// dirty rows is an O(1)-per-pair lookup into its own position cache
// (dirtyUserPositions — see state_strategy.go), so the whole call is
// O(dirty users), not O(all users): the defect this closes is a consumer
// projecting all ~11.5k mainnet users on every event ledger. Paranoid mode
// caches nothing, so this falls back to a single scan of state.Users — no
// worse than paranoid's existing O(total state) per-ledger cost.
//
// Backstop deposits are out of scope: Change 2's dirty set tracks Positions
// entries only, so the returned TransformOutput carries no Backstops-derived
// rows. The result's Reserves/Contracts/ReserveEmissions still cover every
// pool (computeState always emits those from state.Pools) and its
// Activities/Quarantine are always empty (no events are fed in — this is a
// state-only re-projection, not event replay). A caller doing per-ledger
// position emission should read only Positions and Summaries from the result.
func (a *Adapter) ProjectPositions(state *bindings.LedgerState, dirty []bindings.DirtyPosition, ledgerSeq int64, closeTime time.Time) (*bindings.TransformOutput, error) {
	out := &bindings.TransformOutput{
		LedgerSeq:  ledgerSeq,
		Positions:  make([]bindings.Position, 0, len(dirty)),
		Summaries:  make([]bindings.PositionSummary, 0, len(dirty)),
		Reserves:   make([]bindings.Reserve, 0, 16),
		Contracts:  make([]bindings.Contract, 0, 8),
		Quarantine: make([]bindings.QuarantineEvent, 0, 4),
	}
	if state == nil || len(dirty) == 0 {
		return out, nil
	}

	users := a.dirtyPositionRows(state, dirty)
	filtered := *state
	filtered.Users = users
	filtered.Backstops = nil

	if err := a.computeState(bindings.TransformInput{LedgerSeq: ledgerSeq, CloseTime: closeTime, State: &filtered}, out); err != nil {
		return nil, err
	}
	return out, nil
}

// ProjectBackstopPositions projects ONLY the given dirty (address, pool)
// backstop pairs' rows out of state — the per-ledger-emission analog of
// ProjectPositions for the affected-backstop set (bindings.DirtyBackstop,
// Adapter.LastDirtyBackstops, V1-09 D-10). It reuses computeState verbatim (no
// duplicated valuation logic): a filtered LedgerState whose Backstops slice
// holds only the dirty pairs' rows and whose Users slice is empty.
//
// A caller doing per-ledger backstop emission must read ONLY Positions from
// the result: a PositionSummary computed from a backstop-only state carries
// just the backstop leg of each address, which is a wrong row for every
// holder with lending positions — summaries stay with the lending projection
// (ProjectPositions) and the fold-time whole-state pass. Reserves/Contracts
// still cover every pool (computeState always emits those from state.Pools)
// and Activities/Quarantine are always empty (state-only re-projection).
// Removal pairs have no row in state.Backstops and project nothing; the
// caller synthesizes their closed tombstone from the DirtyBackstop identity.
func (a *Adapter) ProjectBackstopPositions(state *bindings.LedgerState, dirty []bindings.DirtyBackstop, ledgerSeq int64, closeTime time.Time) (*bindings.TransformOutput, error) {
	out := &bindings.TransformOutput{
		LedgerSeq:  ledgerSeq,
		Positions:  make([]bindings.Position, 0, len(dirty)),
		Summaries:  make([]bindings.PositionSummary, 0),
		Reserves:   make([]bindings.Reserve, 0, 16),
		Contracts:  make([]bindings.Contract, 0, 8),
		Quarantine: make([]bindings.QuarantineEvent, 0, 4),
	}
	if state == nil || len(dirty) == 0 {
		return out, nil
	}

	want := make(map[string]struct{}, len(dirty))
	for _, d := range dirty {
		want[dirtyPairKey(d.Address, d.PoolContractID)] = struct{}{}
	}
	backstops := make([]contracts.BackstopPosition, 0, len(dirty))
	for _, b := range state.Backstops {
		if _, ok := want[dirtyPairKey(b.Address, b.PoolContractID)]; ok {
			backstops = append(backstops, b)
		}
	}
	filtered := *state
	filtered.Users = nil
	filtered.Backstops = backstops

	if err := a.computeState(bindings.TransformInput{LedgerSeq: ledgerSeq, CloseTime: closeTime, State: &filtered}, out); err != nil {
		return nil, err
	}
	// A summary computed from a backstop-only state carries just the backstop
	// leg of each address — a wrong row for any holder with lending positions.
	// Drop them so a caller cannot merge a partial-vintage total by mistake;
	// summaries stay with the lending projection and the whole-state pass.
	out.Summaries = nil
	return out, nil
}

// pairs. When the active strategy implements dirtyUserPositions (incremental
// mode), each pair is an O(1) cache lookup; otherwise it falls back to a
// single O(all users) scan of state.Users, matching a pair to its rows.
func (a *Adapter) dirtyPositionRows(state *bindings.LedgerState, dirty []bindings.DirtyPosition) []contracts.UserReservePosition {
	if lookup, ok := a.state.(dirtyUserPositions); ok {
		out := make([]contracts.UserReservePosition, 0, len(dirty))
		for _, d := range dirty {
			out = append(out, lookup.userPositions(d.Address, d.PoolContractID)...)
		}
		return out
	}

	want := make(map[string]struct{}, len(dirty))
	for _, d := range dirty {
		want[dirtyPairKey(d.Address, d.PoolContractID)] = struct{}{}
	}
	out := make([]contracts.UserReservePosition, 0, len(dirty))
	for _, u := range state.Users {
		if _, ok := want[dirtyPairKey(u.Address, u.PoolContractID)]; ok {
			out = append(out, u)
		}
	}
	return out
}

func dirtyPairKey(address, poolContractID string) string {
	return address + "|" + poolContractID
}

// statusChangeTxHash builds the synthetic transaction hash gold expects for a
// contract_status_change activity. It MUST match relay migration 001's
// lifecycle_synthetic_identity CHECK exactly:
//
//	tx_hash = 'status:' || contract || ':' || ledger
//
// where ledger is the integer column rendered as text (no zero-padding).
func statusChangeTxHash(contractID string, ledgerSeq int64) string {
	return fmt.Sprintf("status:%s:%d", contractID, ledgerSeq)
}

func activityIdentityFailure(decoded decodedEvent, evt bindings.RawEventEnvelope) string {
	if decoded.address == "" {
		return "missing_activity_address"
	}
	if !strkey.IsValidContractAddress(evt.ContractID) {
		return "invalid_activity_contract"
	}
	if decoded.assetID != "" && !strkey.IsValidContractAddress(decoded.assetID) {
		return "invalid_activity_asset"
	}
	if decoded.activityType == contracts.ActivityTypeStatusChange {
		if decoded.address != evt.ContractID || !strkey.IsValidContractAddress(decoded.address) {
			return "invalid_activity_address"
		}
		return ""
	}
	// Soroban contracts can be Blend users (vaults/strategies routinely supply
	// on behalf of themselves), so both account and contract StrKeys are valid
	// activity actors.
	if !strkey.IsValidEd25519PublicKey(decoded.address) && !strkey.IsValidContractAddress(decoded.address) {
		return "invalid_activity_address"
	}
	return ""
}

// emitTombstones diffs the prior ledger's gold output against the current
// output and emits PositionTombstones and SummaryTombstones for entities that
// disappeared. A position leg disappears when it went to zero (the on-chain
// blob still exists but the leg's amount reached zero and was filtered by
// positionsFromMap) or when the entire Positions entry was evicted/removed
// (applyDelete deleted the blob so build() no longer iterates it).
//
// The relay passes PriorPositions via TransformInput; we build a set of
// current position IDs and any prior ID not in that set is a tombstone.
//
// Summary tombstones are emitted only when an address had a prior summary
// and has no positions at all in the current output (fully closed).
func emitTombstones(input bindings.TransformInput, out *bindings.TransformOutput) {
	if len(input.PriorPositions) == 0 && len(input.PriorSummaries) == 0 {
		return
	}

	// Build a set of current position identity keys: address|protocol|contract|asset|type
	currentPosKeys := make(map[string]struct{}, len(out.Positions))
	for _, p := range out.Positions {
		key := positionIdentityKey(p.Address, p.Protocol, p.ContractID, p.AssetID, string(p.PositionType))
		currentPosKeys[key] = struct{}{}
	}

	// Diff: any prior position ID not in current set → tombstone
	for _, pp := range input.PriorPositions {
		key := positionIdentityKey(pp.Address, pp.Protocol, pp.ContractID, pp.AssetID, string(pp.PositionType))
		if _, exists := currentPosKeys[key]; !exists {
			out.PositionTombstones = append(out.PositionTombstones, bindings.PositionTombstone{
				Address:      pp.Address,
				Protocol:     pp.Protocol,
				ContractID:   pp.ContractID,
				AssetID:      pp.AssetID,
				PositionType: string(pp.PositionType),
				LedgerSeq:    input.LedgerSeq,
			})
		}
	}

	// Sort tombstones for deterministic output
	sort.Slice(out.PositionTombstones, func(i, j int) bool {
		a, b := out.PositionTombstones[i], out.PositionTombstones[j]
		return a.Address < b.Address ||
			(a.Address == b.Address && a.ContractID < b.ContractID) ||
			(a.Address == b.Address && a.ContractID == b.ContractID && a.AssetID < b.AssetID) ||
			(a.Address == b.Address && a.ContractID == b.ContractID && a.AssetID == b.AssetID && a.PositionType < b.PositionType)
	})

	// Summary tombstones: an address has a prior summary but no positions now
	if len(input.PriorSummaries) > 0 {
		// Collect addresses that still have positions in the current output
		addressesWithPositions := make(map[string]bool)
		for _, p := range out.Positions {
			addressesWithPositions[p.Address] = true
		}

		// Also check which addresses are in current summaries
		currentSummaryAddrs := make(map[string]bool)
		for _, s := range out.Summaries {
			currentSummaryAddrs[s.Address] = true
		}

		for _, ps := range input.PriorSummaries {
			if !addressesWithPositions[ps.Address] && !currentSummaryAddrs[ps.Address] {
				out.SummaryTombstones = append(out.SummaryTombstones, bindings.SummaryTombstone{
					Address:   ps.Address,
					Protocol:  ps.Protocol,
					LedgerSeq: input.LedgerSeq,
				})
			}
		}

		sort.Slice(out.SummaryTombstones, func(i, j int) bool {
			a, b := out.SummaryTombstones[i], out.SummaryTombstones[j]
			return a.Address < b.Address || (a.Address == b.Address && a.Protocol < b.Protocol)
		})
	}
}

func positionIdentityKey(address, protocol, contractID, assetID, posType string) string {
	return address + "|" + protocol + "|" + contractID + "|" + assetID + "|" + posType
}
