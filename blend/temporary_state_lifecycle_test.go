// Temporary-state lifecycle tests (V1-04 D-05/D-06): the fold exposes
// per-ledger auction/queued-reserve transitions from the changed ledger keys
// (TemporaryStateChangesProvider), and ProjectTemporaryStateChanges turns
// them into active/inactive lifecycle rows — active with the full payload,
// inactive with stable identity only, never a fabricated outcome. Both fold
// strategies must expose the identical sorted transition set (parity).
package blend

import (
	"reflect"
	"testing"
	"time"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/stellar/go-stellar-sdk/xdr"
)

func auctionValueVal(t *testing.T, block uint32) xdr.ScVal {
	t.Helper()
	lotEntries := xdr.ScMap{{Key: contractAddressVal(t, 2), Val: i128Val(500)}}
	bidEntries := xdr.ScMap{{Key: contractAddressVal(t, 3), Val: i128Val(250)}}
	lotPtr, bidPtr := &lotEntries, &bidEntries
	return mapVal(t, map[string]xdr.ScVal{
		"bid":   {Type: xdr.ScValTypeScvMap, Map: &bidPtr},
		"lot":   {Type: xdr.ScValTypeScvMap, Map: &lotPtr},
		"block": u32Val(block),
	})
}

func queuedReserveValueVal(t *testing.T) xdr.ScVal {
	t.Helper()
	return mapVal(t, map[string]xdr.ScVal{
		"new_config":  fullQueuedConfigVal(t),
		"unlock_time": u64Val(1_800_000_000),
	})
}

func TestTemporaryStateChanges_AuctionCreateUpdateRemoveRestore(t *testing.T) {
	t.Parallel()

	adapter := newTestAdapter(t)
	poolID := validContractString(t, 1)
	user := validAccountString(t, 5)
	key := auctionKeyVal(t, accountAddressVal(t, 5), 0)

	wantUpsert := []bindings.TemporaryStateChange{{
		Kind:           bindings.TemporaryAuction,
		Action:         bindings.DirtyUpsert,
		PoolContractID: poolID,
		UserAddress:    user,
		AuctionType:    0,
	}}
	wantRemoval := []bindings.TemporaryStateChange{{
		Kind:           bindings.TemporaryAuction,
		Action:         bindings.DirtyRemoval,
		PoolContractID: poolID,
		UserAddress:    user,
		AuctionType:    0,
	}}

	// Create: one upsert, identity from the changed key.
	state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		stateChange(t, poolID, key, auctionValueVal(t, 100)),
	}, 100)
	if err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if got := adapter.LastTemporaryStateChanges(); !reflect.DeepEqual(got, wantUpsert) {
		t.Fatalf("create changes = %+v, want %+v", got, wantUpsert)
	}

	// Quiet ledger: the auction carries, but nothing fired.
	state, err = adapter.DecodeState(state, nil, 101)
	if err != nil {
		t.Fatalf("decode carry: %v", err)
	}
	if got := adapter.LastTemporaryStateChanges(); len(got) != 0 {
		t.Fatalf("quiet ledger changes = %+v, want none", got)
	}

	// Update (dutch decay re-checkpoint): upsert again.
	state, err = adapter.DecodeState(state, []bindings.ContractDataChange{
		stateChange(t, poolID, key, auctionValueVal(t, 102)),
	}, 102)
	if err != nil {
		t.Fatalf("decode update: %v", err)
	}
	if got := adapter.LastTemporaryStateChanges(); !reflect.DeepEqual(got, wantUpsert) {
		t.Fatalf("update changes = %+v, want %+v", got, wantUpsert)
	}

	// Filled/deleted: removal with identity only.
	state, err = adapter.DecodeState(state, []bindings.ContractDataChange{
		stateChange(t, poolID, key, auctionValueVal(t, 102), withLive(false), withNoValue(), withChangeType("Removed")),
	}, 103)
	if err != nil {
		t.Fatalf("decode removal: %v", err)
	}
	if got := adapter.LastTemporaryStateChanges(); !reflect.DeepEqual(got, wantRemoval) {
		t.Fatalf("removal changes = %+v, want %+v", got, wantRemoval)
	}

	// Restore (a fresh auction for the same user/type): upsert again.
	state, err = adapter.DecodeState(state, []bindings.ContractDataChange{
		stateChange(t, poolID, key, auctionValueVal(t, 104)),
	}, 104)
	if err != nil {
		t.Fatalf("decode restore: %v", err)
	}
	if got := adapter.LastTemporaryStateChanges(); !reflect.DeepEqual(got, wantUpsert) {
		t.Fatalf("restore changes = %+v, want %+v", got, wantUpsert)
	}
}

func TestTemporaryStateChanges_QueuedReserveCreateReplaceRemoveRestore(t *testing.T) {
	t.Parallel()

	adapter := newTestAdapter(t)
	poolID := validContractString(t, 1)
	asset := validContractString(t, 2)
	key := resInitKeyVal(t, contractAddressVal(t, 2))

	wantUpsert := []bindings.TemporaryStateChange{{
		Kind:           bindings.TemporaryQueuedReserve,
		Action:         bindings.DirtyUpsert,
		PoolContractID: poolID,
		AssetID:        asset,
	}}
	wantRemoval := []bindings.TemporaryStateChange{{
		Kind:           bindings.TemporaryQueuedReserve,
		Action:         bindings.DirtyRemoval,
		PoolContractID: poolID,
		AssetID:        asset,
	}}

	state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		stateChange(t, poolID, key, queuedReserveValueVal(t)),
	}, 100)
	if err != nil {
		t.Fatalf("decode queue: %v", err)
	}
	if got := adapter.LastTemporaryStateChanges(); !reflect.DeepEqual(got, wantUpsert) {
		t.Fatalf("queue changes = %+v, want %+v", got, wantUpsert)
	}

	// Replace (queue_set_reserve overwrites the pending change): upsert.
	state, err = adapter.DecodeState(state, []bindings.ContractDataChange{
		stateChange(t, poolID, key, queuedReserveValueVal(t)),
	}, 101)
	if err != nil {
		t.Fatalf("decode replace: %v", err)
	}
	if got := adapter.LastTemporaryStateChanges(); !reflect.DeepEqual(got, wantUpsert) {
		t.Fatalf("replace changes = %+v, want %+v", got, wantUpsert)
	}

	// Executed/cancelled: removal.
	state, err = adapter.DecodeState(state, []bindings.ContractDataChange{
		stateChange(t, poolID, key, i128Val(0), withLive(false), withNoValue(), withChangeType("Removed")),
	}, 102)
	if err != nil {
		t.Fatalf("decode removal: %v", err)
	}
	if got := adapter.LastTemporaryStateChanges(); !reflect.DeepEqual(got, wantRemoval) {
		t.Fatalf("removal changes = %+v, want %+v", got, wantRemoval)
	}

	// Re-queued after the removal: upsert.
	_, err = adapter.DecodeState(state, []bindings.ContractDataChange{
		stateChange(t, poolID, key, queuedReserveValueVal(t)),
	}, 103)
	if err != nil {
		t.Fatalf("decode restore: %v", err)
	}
	if got := adapter.LastTemporaryStateChanges(); !reflect.DeepEqual(got, wantUpsert) {
		t.Fatalf("restore changes = %+v, want %+v", got, wantUpsert)
	}
}

// TestTemporaryStateChanges_RemovalWithoutPriorStillCarriesIdentity is the
// bounded-replay contract: a replay that first observes an auction or queued
// reserve at its REMOVAL ledger (its create is before the replay floor) still
// reports the transition, because the identity comes from the removed ledger
// key — never from comparing full previous/current state slices.
func TestTemporaryStateChanges_RemovalWithoutPriorStillCarriesIdentity(t *testing.T) {
	t.Parallel()

	adapter := newTestAdapter(t)
	poolID := validContractString(t, 1)
	user := validAccountString(t, 5)
	asset := validContractString(t, 2)

	state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		stateChange(t, poolID, auctionKeyVal(t, accountAddressVal(t, 5), 1), auctionValueVal(t, 99),
			withLive(false), withNoValue(), withChangeType("Removed")),
		stateChange(t, poolID, resInitKeyVal(t, contractAddressVal(t, 2)), i128Val(0),
			withLive(false), withNoValue(), withChangeType("Removed")),
	}, 100)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(state.Auctions) != 0 || len(state.QueuedReserves) != 0 {
		t.Fatalf("removal fabricated state: auctions=%+v queued=%+v", state.Auctions, state.QueuedReserves)
	}
	want := []bindings.TemporaryStateChange{
		{
			Kind:           bindings.TemporaryAuction,
			Action:         bindings.DirtyRemoval,
			PoolContractID: poolID,
			UserAddress:    user,
			AuctionType:    1,
		},
		{
			Kind:           bindings.TemporaryQueuedReserve,
			Action:         bindings.DirtyRemoval,
			PoolContractID: poolID,
			AssetID:        asset,
		},
	}
	if got := adapter.LastTemporaryStateChanges(); !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
}

func TestProjectTemporaryStateChanges_InactivePayloadIsAbsent(t *testing.T) {
	t.Parallel()

	adapter := newTestAdapter(t)
	poolID := validContractString(t, 1)
	user := validAccountString(t, 5)
	asset := validContractString(t, 2)
	closeTime := time.Unix(1_700_000_000, 0).UTC()

	auctionKey := auctionKeyVal(t, accountAddressVal(t, 5), 0)
	resInitKey := resInitKeyVal(t, contractAddressVal(t, 2))

	// Create both, project the upserts: full payload, Active=true.
	state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		stateChange(t, poolID, auctionKey, auctionValueVal(t, 100)),
		stateChange(t, poolID, resInitKey, queuedReserveValueVal(t)),
	}, 100)
	if err != nil {
		t.Fatalf("decode create: %v", err)
	}
	projected := adapter.ProjectTemporaryStateChanges(state, adapter.LastTemporaryStateChanges(), 100, closeTime)
	if len(projected.AuctionLifecycle) != 1 || len(projected.QueuedReserveLifecycle) != 1 {
		t.Fatalf("active projection = %+v / %+v, want one of each", projected.AuctionLifecycle, projected.QueuedReserveLifecycle)
	}
	activeAuction := projected.AuctionLifecycle[0]
	if !activeAuction.Active || activeAuction.Block != 100 || len(activeAuction.Lot) != 1 || len(activeAuction.Bid) != 1 {
		t.Fatalf("active auction row = %+v, want payload present", activeAuction)
	}
	activeQueued := projected.QueuedReserveLifecycle[0]
	if !activeQueued.Active || activeQueued.UnlockTimeRaw != "1800000000" || len(activeQueued.NewConfig) == 0 {
		t.Fatalf("active queued row = %+v, want payload present", activeQueued)
	}

	// Remove both, project the removals: stable identity only, Active=false,
	// no payload — never a fabricated terminal outcome.
	state, err = adapter.DecodeState(state, []bindings.ContractDataChange{
		stateChange(t, poolID, auctionKey, auctionValueVal(t, 100), withLive(false), withNoValue(), withChangeType("Removed")),
		stateChange(t, poolID, resInitKey, i128Val(0), withLive(false), withNoValue(), withChangeType("Removed")),
	}, 101)
	if err != nil {
		t.Fatalf("decode removal: %v", err)
	}
	projected = adapter.ProjectTemporaryStateChanges(state, adapter.LastTemporaryStateChanges(), 101, closeTime)
	if len(projected.AuctionLifecycle) != 1 || len(projected.QueuedReserveLifecycle) != 1 {
		t.Fatalf("inactive projection = %+v / %+v, want one of each", projected.AuctionLifecycle, projected.QueuedReserveLifecycle)
	}
	inactiveAuction := projected.AuctionLifecycle[0]
	if inactiveAuction.Active {
		t.Fatal("inactive auction row marked active")
	}
	if inactiveAuction.ID != activeAuction.ID {
		t.Fatalf("inactive auction ID %q != active ID %q — identity must be stable across the lifecycle", inactiveAuction.ID, activeAuction.ID)
	}
	if inactiveAuction.UserAddress != user || inactiveAuction.ContractID != poolID || inactiveAuction.AuctionType != "user_liquidation" {
		t.Fatalf("inactive auction identity = %+v", inactiveAuction)
	}
	if inactiveAuction.Block != 0 || inactiveAuction.Lot != nil || inactiveAuction.Bid != nil {
		t.Fatalf("inactive auction payload = %+v, want absent", inactiveAuction)
	}
	if inactiveAuction.LedgerSeq != 101 || !inactiveAuction.Timestamp.Equal(closeTime) {
		t.Fatalf("inactive auction ledger/timestamp = %d/%v", inactiveAuction.LedgerSeq, inactiveAuction.Timestamp)
	}
	inactiveQueued := projected.QueuedReserveLifecycle[0]
	if inactiveQueued.Active {
		t.Fatal("inactive queued row marked active")
	}
	if inactiveQueued.ID != activeQueued.ID {
		t.Fatalf("inactive queued ID %q != active ID %q", inactiveQueued.ID, activeQueued.ID)
	}
	if inactiveQueued.AssetID != asset || inactiveQueued.ContractID != poolID {
		t.Fatalf("inactive queued identity = %+v", inactiveQueued)
	}
	if inactiveQueued.UnlockTimeRaw != "" || !inactiveQueued.UnlockTime.IsZero() || inactiveQueued.NewConfig != nil {
		t.Fatalf("inactive queued payload = %+v, want absent", inactiveQueued)
	}
}

// TestTemporaryStateChanges_DeterministicOrder pins the exposed sort:
// (kind, pool, user, auction type, asset), independent of change order in the
// fold input.
func TestTemporaryStateChanges_DeterministicOrder(t *testing.T) {
	t.Parallel()

	adapter := newTestAdapter(t)
	poolA := validContractString(t, 1)
	poolB := validContractString(t, 10)

	changes := []bindings.ContractDataChange{
		stateChange(t, poolB, resInitKeyVal(t, contractAddressVal(t, 2)), queuedReserveValueVal(t)),
		stateChange(t, poolA, auctionKeyVal(t, accountAddressVal(t, 6), 2), auctionValueVal(t, 100)),
		stateChange(t, poolA, auctionKeyVal(t, accountAddressVal(t, 5), 0), auctionValueVal(t, 100)),
		stateChange(t, poolA, resInitKeyVal(t, contractAddressVal(t, 2)), queuedReserveValueVal(t)),
		stateChange(t, poolB, auctionKeyVal(t, accountAddressVal(t, 5), 0), auctionValueVal(t, 100)),
	}
	if _, err := adapter.DecodeState(nil, changes, 100); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := adapter.LastTemporaryStateChanges()
	if len(got) != 5 {
		t.Fatalf("changes = %+v, want 5", got)
	}
	for i := 1; i < len(got); i++ {
		a, b := got[i-1], got[i]
		less := a.Kind != b.Kind && a.Kind < b.Kind ||
			a.Kind == b.Kind && (a.PoolContractID != b.PoolContractID && a.PoolContractID < b.PoolContractID ||
				a.PoolContractID == b.PoolContractID && (a.UserAddress != b.UserAddress && a.UserAddress < b.UserAddress ||
					a.UserAddress == b.UserAddress && (a.AuctionType != b.AuctionType && a.AuctionType < b.AuctionType ||
						a.AuctionType == b.AuctionType && a.AssetID < b.AssetID)))
		if !less {
			t.Fatalf("changes not sorted at %d: %+v then %+v", i, a, b)
		}
	}

	// Run twice: identical serialization.
	second := newTestAdapter(t)
	if _, err := second.DecodeState(nil, changes, 100); err != nil {
		t.Fatalf("decode second: %v", err)
	}
	a, b := mustJSON(t, adapter.LastTemporaryStateChanges()), mustJSON(t, second.LastTemporaryStateChanges())
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("run-twice mismatch:\nfirst=%s\nsecond=%s", a, b)
	}
}
