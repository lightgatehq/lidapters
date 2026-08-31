// Decode-gap closure tests (lidapters#9): auction state, emission-accrual
// structs (pool EmisData v2 merged shape, UserEmis, BEmisData, UEmisData), and
// the ResData backstop_credit/last_time cheap wins. Every group carries
// adversarial cases — missing keys, wrong ScVal types, extreme i128 values,
// truncated maps, unknown extra keys — pinning that each decode fails safe
// (absent/skip, never fabricated zeros or panics).
package blend

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/lightgatehq/lidapters/blend/contracts"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// i128 extremes: max = 2^127-1, min = -2^127.
const (
	i128MaxString = "170141183460469231731687303715884105727"
	i128MinString = "-170141183460469231731687303715884105728"
)

func i128MaxVal() xdr.ScVal {
	raw := xdr.Int128Parts{Hi: xdr.Int64(^uint64(0) >> 1), Lo: xdr.Uint64(^uint64(0))}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &raw}
}

func i128MinVal() xdr.ScVal {
	raw := xdr.Int128Parts{Hi: xdr.Int64(-1 << 63), Lo: 0}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &raw}
}

func auctionKeyVal(t *testing.T, user xdr.ScVal, auctType uint32) xdr.ScVal {
	t.Helper()
	return variantVal(t, "Auction", mapVal(t, map[string]xdr.ScVal{
		"user":      user,
		"auct_type": u32Val(auctType),
	}))
}

func userEmisKeyVal(t *testing.T, user xdr.ScVal, reserveID uint32) xdr.ScVal {
	t.Helper()
	return variantVal(t, "UserEmis", mapVal(t, map[string]xdr.ScVal{
		"user":       user,
		"reserve_id": u32Val(reserveID),
	}))
}

// --- auction state -----------------------------------------------------------

func TestDecodeState_AuctionRoundTrip(t *testing.T) {
	t.Parallel()

	adapter := newTestAdapter(t)
	poolID := validContractString(t, 1)
	user := validAccountString(t, 5)
	assetA := validContractString(t, 2)
	assetB := validContractString(t, 3)

	auctionValue := mapVal(t, map[string]xdr.ScVal{
		"bid": mapVal(t, map[string]xdr.ScVal{}),
		"lot": mapVal(t, map[string]xdr.ScVal{}),
	})
	// Rebuild with address-keyed maps: lot has an extreme i128 amount, bid a
	// negative extreme, so 128-bit round-tripping is pinned exactly.
	lotEntries := xdr.ScMap{
		{Key: contractAddressVal(t, 2), Val: i128MaxVal()},
		{Key: contractAddressVal(t, 3), Val: i128Val(500)},
	}
	bidEntries := xdr.ScMap{
		{Key: contractAddressVal(t, 2), Val: i128MinVal()},
	}
	lotPtr, bidPtr := &lotEntries, &bidEntries
	auctionValue = mapVal(t, map[string]xdr.ScVal{
		"bid":   {Type: xdr.ScValTypeScvMap, Map: &bidPtr},
		"lot":   {Type: xdr.ScValTypeScvMap, Map: &lotPtr},
		"block": u32Val(62986500),
	})

	changes := []bindings.ContractDataChange{
		stateChange(t, poolID, auctionKeyVal(t, accountAddressVal(t, 5), 0), auctionValue),
	}
	state, err := adapter.DecodeState(nil, changes, 100)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	wantLot := []contracts.AuctionEntry{{AssetID: assetA, AmountRaw: i128MaxString}, {AssetID: assetB, AmountRaw: "500"}}
	if assetB < assetA {
		wantLot = []contracts.AuctionEntry{{AssetID: assetB, AmountRaw: "500"}, {AssetID: assetA, AmountRaw: i128MaxString}}
	}
	want := []contracts.AuctionState{{
		PoolContractID: poolID,
		UserAddress:    user,
		AuctionType:    0,
		Block:          62986500,
		Lot:            wantLot,
		Bid:            []contracts.AuctionEntry{{AssetID: assetA, AmountRaw: i128MinString}},
	}}
	if !reflect.DeepEqual(state.Auctions, want) {
		t.Fatalf("auction state = %+v, want %+v", state.Auctions, want)
	}

	// Carry: a ledger with no changes must not forget the live auction.
	carried, err := adapter.DecodeState(state, nil, 101)
	if err != nil {
		t.Fatalf("decode carry: %v", err)
	}
	if !reflect.DeepEqual(carried.Auctions, want) {
		t.Fatalf("carried auction state = %+v, want %+v", carried.Auctions, want)
	}

	// Explicit on-chain removal (auction filled/deleted) drops it.
	removed, err := adapter.DecodeState(carried, []bindings.ContractDataChange{
		stateChange(t, poolID, auctionKeyVal(t, accountAddressVal(t, 5), 0), auctionValue,
			withLive(false), withNoValue(), withChangeType("Removed")),
	}, 102)
	if err != nil {
		t.Fatalf("decode removal: %v", err)
	}
	if len(removed.Auctions) != 0 {
		t.Fatalf("expected auction removed, got %+v", removed.Auctions)
	}
}

func TestDecodeState_AuctionTTLLapseRemoves(t *testing.T) {
	t.Parallel()

	adapter := newTestAdapter(t)
	poolID := validContractString(t, 1)
	auctionValue := mapVal(t, map[string]xdr.ScVal{
		"bid":   mapVal(t, map[string]xdr.ScVal{}),
		"lot":   mapVal(t, map[string]xdr.ScVal{}),
		"block": u32Val(10),
	})
	state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		stateChange(t, poolID, auctionKeyVal(t, accountAddressVal(t, 5), 2), auctionValue),
	}, 100)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(state.Auctions) != 1 {
		t.Fatalf("expected 1 auction, got %d", len(state.Auctions))
	}

	// TTL lapse (Live=false, ChangeType NOT Removed): auctions are temporary
	// storage, so unlike Positions/ResConfig this must remove, never archive.
	lapsed, err := adapter.DecodeState(state, []bindings.ContractDataChange{
		stateChange(t, poolID, auctionKeyVal(t, accountAddressVal(t, 5), 2), auctionValue,
			withLive(false), withChangeType("Updated")),
	}, 101)
	if err != nil {
		t.Fatalf("decode lapse: %v", err)
	}
	if len(lapsed.Auctions) != 0 {
		t.Fatalf("expected TTL-lapsed auction removed, got %+v", lapsed.Auctions)
	}
}

// TestDecodeState_AuctionMalformedFailsSafe refutes the auction decode with
// every malformed shape: each must leave the carried state untouched (absent
// or the previous good value), never a partial or zero-filled auction.
func TestDecodeState_AuctionMalformedFailsSafe(t *testing.T) {
	t.Parallel()

	poolID := validContractString(t, 1)
	goodValue := mapVal(t, map[string]xdr.ScVal{
		"bid":   mapVal(t, map[string]xdr.ScVal{}),
		"lot":   mapVal(t, map[string]xdr.ScVal{}),
		"block": u32Val(42),
	})

	badLotEntries := xdr.ScMap{{Key: u32Val(7), Val: i128Val(1)}} // non-address key
	badLotPtr := &badLotEntries
	badAmountEntries := xdr.ScMap{{Key: contractAddressVal(t, 2), Val: boolVal(true)}} // non-numeric amount
	badAmountPtr := &badAmountEntries

	cases := []struct {
		name  string
		key   xdr.ScVal
		value xdr.ScVal
	}{
		{"value_not_a_map", auctionKeyVal(t, accountAddressVal(t, 5), 0), i128Val(1)},
		{"missing_block", auctionKeyVal(t, accountAddressVal(t, 5), 0), mapVal(t, map[string]xdr.ScVal{
			"bid": mapVal(t, map[string]xdr.ScVal{}),
			"lot": mapVal(t, map[string]xdr.ScVal{}),
		})},
		{"missing_lot", auctionKeyVal(t, accountAddressVal(t, 5), 0), mapVal(t, map[string]xdr.ScVal{
			"bid":   mapVal(t, map[string]xdr.ScVal{}),
			"block": u32Val(1),
		})},
		{"lot_entry_non_address_key", auctionKeyVal(t, accountAddressVal(t, 5), 0), mapVal(t, map[string]xdr.ScVal{
			"bid":   mapVal(t, map[string]xdr.ScVal{}),
			"lot":   {Type: xdr.ScValTypeScvMap, Map: &badLotPtr},
			"block": u32Val(1),
		})},
		{"lot_entry_non_numeric_amount", auctionKeyVal(t, accountAddressVal(t, 5), 0), mapVal(t, map[string]xdr.ScVal{
			"bid":   mapVal(t, map[string]xdr.ScVal{}),
			"lot":   {Type: xdr.ScValTypeScvMap, Map: &badAmountPtr},
			"block": u32Val(1),
		})},
		{"key_missing_user", variantVal(t, "Auction", mapVal(t, map[string]xdr.ScVal{
			"auct_type": u32Val(0),
		})), goodValue},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter := newTestAdapter(t)
			state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
				stateChange(t, poolID, tc.key, tc.value),
			}, 100)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(state.Auctions) != 0 {
				t.Fatalf("malformed %s produced an auction: %+v", tc.name, state.Auctions)
			}
		})
	}

	// A malformed live write over a carried good auction keeps the good one.
	adapter := newTestAdapter(t)
	prior, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		stateChange(t, poolID, auctionKeyVal(t, accountAddressVal(t, 5), 0), goodValue),
	}, 100)
	if err != nil {
		t.Fatalf("decode good: %v", err)
	}
	next, err := adapter.DecodeState(prior, []bindings.ContractDataChange{
		stateChange(t, poolID, auctionKeyVal(t, accountAddressVal(t, 5), 0), i128Val(9)),
	}, 101)
	if err != nil {
		t.Fatalf("decode malformed over good: %v", err)
	}
	if len(next.Auctions) != 1 || next.Auctions[0].Block != 42 {
		t.Fatalf("malformed live write clobbered carried auction: %+v", next.Auctions)
	}
}

// TestDecodeState_AuctionUnknownExtraKeysIgnored pins forward compatibility: a
// future AuctionData with extra fields still decodes the known ones.
func TestDecodeState_AuctionUnknownExtraKeysIgnored(t *testing.T) {
	t.Parallel()

	adapter := newTestAdapter(t)
	poolID := validContractString(t, 1)
	state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		stateChange(t, poolID, auctionKeyVal(t, accountAddressVal(t, 5), 1), mapVal(t, map[string]xdr.ScVal{
			"bid":         mapVal(t, map[string]xdr.ScVal{}),
			"lot":         mapVal(t, map[string]xdr.ScVal{}),
			"block":       u32Val(9),
			"future_knob": boolVal(true),
		})),
	}, 100)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(state.Auctions) != 1 || state.Auctions[0].Block != 9 || state.Auctions[0].AuctionType != 1 {
		t.Fatalf("unexpected auction decode with extra keys: %+v", state.Auctions)
	}
}

func TestTransform_AuctionOutput(t *testing.T) {
	t.Parallel()

	adapter, err := New(Config{V2WasmHashes: map[string]struct{}{"known-v2": {}}})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	user := validAccountString(t, 5)
	asset := validContractString(t, 2)
	out, err := adapter.Transform(bindings.TransformInput{
		LedgerSeq: 200,
		CloseTime: time.Unix(2000, 0).UTC(),
		State: &bindings.LedgerState{
			Auctions: []contracts.AuctionState{
				{PoolContractID: "CPOOL", UserAddress: user, AuctionType: 0, Block: 7,
					Lot: []contracts.AuctionEntry{{AssetID: asset, AmountRaw: "123"}},
					Bid: []contracts.AuctionEntry{{AssetID: asset, AmountRaw: "456"}}},
				{PoolContractID: "CPOOL", UserAddress: user, AuctionType: 7, Block: 8},
			},
		},
	})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if len(out.Auctions) != 2 {
		t.Fatalf("expected 2 auction rows, got %d", len(out.Auctions))
	}
	first := out.Auctions[0]
	if first.AuctionType != "user_liquidation" || first.Block != 7 || first.UserAddress != user {
		t.Fatalf("unexpected auction row: %+v", first)
	}
	if len(first.Lot) != 1 || first.Lot[0].AmountRaw != "123" || len(first.Bid) != 1 || first.Bid[0].AmountRaw != "456" {
		t.Fatalf("lot/bid not surfaced: %+v", first)
	}
	// An out-of-range auction type stays visible as its number, never coerced.
	if out.Auctions[1].AuctionType != "7" {
		t.Fatalf("unknown auction type label = %q, want \"7\"", out.Auctions[1].AuctionType)
	}
}

// --- pool per-user emission accrual (UserEmis) -------------------------------

func TestDecodeState_UserEmisRoundTrip(t *testing.T) {
	t.Parallel()

	adapter := newTestAdapter(t)
	poolID := validContractString(t, 1)
	user := validAccountString(t, 5)

	state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		stateChange(t, poolID, userEmisKeyVal(t, accountAddressVal(t, 5), 3), mapVal(t, map[string]xdr.ScVal{
			"index":   i128Val(1_000_000),
			"accrued": i128MaxVal(),
		})),
	}, 100)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []contracts.UserEmissionState{{
		Address:        user,
		PoolContractID: poolID,
		ReserveTokenID: 3,
		IndexRaw:       "1000000",
		AccruedRaw:     i128MaxString,
	}}
	if !reflect.DeepEqual(state.UserEmissions, want) {
		t.Fatalf("user emissions = %+v, want %+v", state.UserEmissions, want)
	}

	carried, err := adapter.DecodeState(state, nil, 101)
	if err != nil {
		t.Fatalf("decode carry: %v", err)
	}
	if !reflect.DeepEqual(carried.UserEmissions, want) {
		t.Fatalf("carried user emissions = %+v, want %+v", carried.UserEmissions, want)
	}

	removed, err := adapter.DecodeState(carried, []bindings.ContractDataChange{
		stateChange(t, poolID, userEmisKeyVal(t, accountAddressVal(t, 5), 3), i128Val(0),
			withLive(false), withNoValue(), withChangeType("Removed")),
	}, 102)
	if err != nil {
		t.Fatalf("decode removal: %v", err)
	}
	if len(removed.UserEmissions) != 0 {
		t.Fatalf("expected user emission removed, got %+v", removed.UserEmissions)
	}
}

// TestDecodeState_UserEmisMalformedFailsSafe: UserEmissionData requires both
// index and accrued — a value missing either (or shaped wrong) is skipped
// whole, never half-written or zero-filled.
func TestDecodeState_UserEmisMalformedFailsSafe(t *testing.T) {
	t.Parallel()

	poolID := validContractString(t, 1)
	cases := []struct {
		name  string
		key   xdr.ScVal
		value xdr.ScVal
	}{
		{"missing_accrued", userEmisKeyVal(t, accountAddressVal(t, 5), 3), mapVal(t, map[string]xdr.ScVal{
			"index": i128Val(1),
		})},
		{"missing_index", userEmisKeyVal(t, accountAddressVal(t, 5), 3), mapVal(t, map[string]xdr.ScVal{
			"accrued": i128Val(1),
		})},
		{"value_not_a_map", userEmisKeyVal(t, accountAddressVal(t, 5), 3), i128Val(1)},
		{"wrong_field_types", userEmisKeyVal(t, accountAddressVal(t, 5), 3), mapVal(t, map[string]xdr.ScVal{
			"index":   boolVal(true),
			"accrued": i128Val(1),
		})},
		{"key_missing_reserve_id", variantVal(t, "UserEmis", mapVal(t, map[string]xdr.ScVal{
			"user": accountAddressVal(t, 5),
		})), mapVal(t, map[string]xdr.ScVal{"index": i128Val(1), "accrued": i128Val(2)})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter := newTestAdapter(t)
			state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
				stateChange(t, poolID, tc.key, tc.value),
			}, 100)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(state.UserEmissions) != 0 {
				t.Fatalf("malformed %s produced a user emission: %+v", tc.name, state.UserEmissions)
			}
		})
	}
}

func TestTransform_UserEmissionResolvesAssetAndSide(t *testing.T) {
	t.Parallel()

	adapter, err := New(Config{V2WasmHashes: map[string]struct{}{"known-v2": {}}})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	user := validAccountString(t, 5)
	asset := validContractString(t, 2)
	out, err := adapter.Transform(bindings.TransformInput{
		LedgerSeq: 300,
		CloseTime: time.Unix(3000, 0).UTC(),
		State: &bindings.LedgerState{
			Pools: []contracts.PoolState{{
				ContractID: "CPOOL",
				WasmHash:   "known-v2",
				Reserves:   []contracts.ReserveState{{ReserveIndex: 1, ReserveIndexKnown: true, AssetID: asset}},
			}},
			UserEmissions: []contracts.UserEmissionState{
				// res_token_id 3 = reserve index 1, supply side.
				{Address: user, PoolContractID: "CPOOL", ReserveTokenID: 3, IndexRaw: "10", AccruedRaw: "999"},
				// res_token_id 2 = reserve index 1, borrow side.
				{Address: user, PoolContractID: "CPOOL", ReserveTokenID: 2, IndexRaw: "11", AccruedRaw: "1"},
				// Unknown pool: the asset must stay "" (never guessed), the row
				// still surfaces with its raw reserve token id.
				{Address: user, PoolContractID: "CUNKNOWNPOOL", ReserveTokenID: 4, IndexRaw: "12", AccruedRaw: "2"},
			},
		},
	})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if len(out.UserEmissions) != 3 {
		t.Fatalf("expected 3 user emission rows, got %d", len(out.UserEmissions))
	}
	supply := out.UserEmissions[0]
	if supply.Side != "supply" || supply.AssetID != asset || supply.AccruedRaw != "999" {
		t.Fatalf("supply-side row = %+v", supply)
	}
	borrow := out.UserEmissions[1]
	if borrow.Side != "borrow" || borrow.AssetID != asset {
		t.Fatalf("borrow-side row = %+v", borrow)
	}
	unresolved := out.UserEmissions[2]
	if unresolved.AssetID != "" || unresolved.ReserveTokenID != 4 {
		t.Fatalf("unresolved row must keep AssetID empty, got %+v", unresolved)
	}
}

// --- backstop emission accrual (BEmisData / UEmisData) ------------------------

func TestDecodeState_BackstopEmisRoundTrip(t *testing.T) {
	t.Parallel()

	adapter := newTestAdapter(t)
	poolID := validContractString(t, 1)
	backstopID := validContractString(t, 4)

	changes := []bindings.ContractDataChange{
		stateChange(t, poolID, symbolVal(t, "Config"), mapVal(t, map[string]xdr.ScVal{
			"oracle":     contractAddressVal(t, 3),
			"bstop_rate": u32Val(1_000_000),
			"status":     u32Val(1),
		})),
		stateChange(t, backstopID, variantVal(t, "BEmisData", contractAddressVal(t, 1)), mapVal(t, map[string]xdr.ScVal{
			"expiration": u64Val(1_800_000_000),
			"eps":        u64Val(5_000),
			"index":      i128Val(777),
			"last_time":  u64Val(1_700_000_000),
		})),
	}
	state, err := adapter.DecodeState(nil, changes, 100)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(state.Pools) != 1 {
		t.Fatalf("expected 1 pool, got %d", len(state.Pools))
	}
	pool := state.Pools[0]
	if pool.BackstopEmisEPSRaw != "5000" || pool.BackstopEmisExpirationRaw != "1800000000" ||
		pool.BackstopEmisIndexRaw != "777" || pool.BackstopEmisLastTimeRaw != "1700000000" {
		t.Fatalf("backstop emission fields = %+v", pool)
	}

	// Round-trips on the pool across an empty ledger.
	carried, err := adapter.DecodeState(state, nil, 101)
	if err != nil {
		t.Fatalf("decode carry: %v", err)
	}
	if carried.Pools[0].BackstopEmisEPSRaw != "5000" {
		t.Fatalf("carried backstop emission lost: %+v", carried.Pools[0])
	}

	// A malformed live write keeps the carried values (skip, never wipe).
	afterBad, err := adapter.DecodeState(carried, []bindings.ContractDataChange{
		stateChange(t, backstopID, variantVal(t, "BEmisData", contractAddressVal(t, 1)), i128Val(3)),
	}, 102)
	if err != nil {
		t.Fatalf("decode malformed: %v", err)
	}
	if afterBad.Pools[0].BackstopEmisEPSRaw != "5000" {
		t.Fatalf("malformed BEmisData write clobbered carried fields: %+v", afterBad.Pools[0])
	}

	// Deletion clears all four — no stale phantom emission.
	deleted, err := adapter.DecodeState(afterBad, []bindings.ContractDataChange{
		stateChange(t, backstopID, variantVal(t, "BEmisData", contractAddressVal(t, 1)), i128Val(0),
			withLive(false), withNoValue(), withChangeType("Removed")),
	}, 103)
	if err != nil {
		t.Fatalf("decode delete: %v", err)
	}
	pool = deleted.Pools[0]
	if pool.BackstopEmisEPSRaw != "" || pool.BackstopEmisExpirationRaw != "" ||
		pool.BackstopEmisIndexRaw != "" || pool.BackstopEmisLastTimeRaw != "" {
		t.Fatalf("deleted BEmisData left residue: %+v", pool)
	}
}

// TestDecodeState_BackstopEmisPartialFieldsStayAbsent: each
// BackstopEmissionData field is independently optional — the two present set,
// the two absent stay "", never zero-filled.
func TestDecodeState_BackstopEmisPartialFieldsStayAbsent(t *testing.T) {
	t.Parallel()

	adapter := newTestAdapter(t)
	poolID := validContractString(t, 1)
	backstopID := validContractString(t, 4)
	state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		stateChange(t, poolID, symbolVal(t, "Config"), mapVal(t, map[string]xdr.ScVal{
			"oracle":     contractAddressVal(t, 3),
			"bstop_rate": u32Val(1_000_000),
			"status":     u32Val(1),
		})),
		stateChange(t, backstopID, variantVal(t, "BEmisData", contractAddressVal(t, 1)), mapVal(t, map[string]xdr.ScVal{
			"index":     i128Val(1),
			"last_time": u64Val(1_700_000_000),
		})),
	}, 100)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	pool := state.Pools[0]
	if pool.BackstopEmisIndexRaw != "1" || pool.BackstopEmisLastTimeRaw != "1700000000" {
		t.Fatalf("present fields not decoded: %+v", pool)
	}
	if pool.BackstopEmisEPSRaw != "" || pool.BackstopEmisExpirationRaw != "" {
		t.Fatalf("absent fields fabricated: eps=%q expiration=%q", pool.BackstopEmisEPSRaw, pool.BackstopEmisExpirationRaw)
	}
}

func TestDecodeState_BackstopUserEmisLifecycle(t *testing.T) {
	t.Parallel()

	adapter := newTestAdapter(t)
	backstopID := validContractString(t, 4)
	user := validAccountString(t, 5)
	poolUserKey := func() xdr.ScVal {
		return variantVal(t, "UEmisData", mapVal(t, map[string]xdr.ScVal{
			"pool": contractAddressVal(t, 1),
			"user": accountAddressVal(t, 5),
		}))
	}
	userBalanceKey := func() xdr.ScVal {
		return variantVal(t, "UserBalance", mapVal(t, map[string]xdr.ScVal{
			"pool": contractAddressVal(t, 1),
			"user": accountAddressVal(t, 5),
		}))
	}

	// UEmisData BEFORE any UserBalance: the accrual must not be dropped.
	state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		stateChange(t, backstopID, poolUserKey(), mapVal(t, map[string]xdr.ScVal{
			"index":   i128Val(42),
			"accrued": i128Val(9_999),
		})),
	}, 100)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(state.Backstops) != 1 {
		t.Fatalf("expected accrual-only backstop row, got %d", len(state.Backstops))
	}
	row := state.Backstops[0]
	if row.Address != user || row.UnclaimedEmissionsRaw != "9999" || row.EmisIndexRaw != "42" {
		t.Fatalf("accrual-only row = %+v", row)
	}
	if row.UserSharesRaw != "" {
		t.Fatalf("accrual-only row fabricated shares: %q", row.UserSharesRaw)
	}

	// A UserBalance write must not clobber the carried emission fields.
	state, err = adapter.DecodeState(state, []bindings.ContractDataChange{
		stateChange(t, backstopID, userBalanceKey(), mapVal(t, map[string]xdr.ScVal{
			"shares": i128Val(400),
		})),
	}, 101)
	if err != nil {
		t.Fatalf("decode balance: %v", err)
	}
	row = state.Backstops[0]
	if row.UserSharesRaw != "400" || row.UnclaimedEmissionsRaw != "9999" || row.EmisIndexRaw != "42" {
		t.Fatalf("UserBalance write clobbered emission fields: %+v", row)
	}

	// A UserBalance delete keeps the accrual (unclaimed emissions survive a
	// full withdrawal on-chain).
	state, err = adapter.DecodeState(state, []bindings.ContractDataChange{
		stateChange(t, backstopID, userBalanceKey(), i128Val(0),
			withLive(false), withNoValue(), withChangeType("Removed")),
	}, 102)
	if err != nil {
		t.Fatalf("decode balance delete: %v", err)
	}
	if len(state.Backstops) != 1 {
		t.Fatalf("balance delete dropped the accrual row: %+v", state.Backstops)
	}
	row = state.Backstops[0]
	if row.UserSharesRaw != "" || row.UnclaimedEmissionsRaw != "9999" {
		t.Fatalf("post-balance-delete row = %+v", row)
	}

	// Deleting the accrual too removes the row entirely.
	state, err = adapter.DecodeState(state, []bindings.ContractDataChange{
		stateChange(t, backstopID, poolUserKey(), i128Val(0),
			withLive(false), withNoValue(), withChangeType("Removed")),
	}, 103)
	if err != nil {
		t.Fatalf("decode emis delete: %v", err)
	}
	if len(state.Backstops) != 0 {
		t.Fatalf("expected row removed once balance and accrual are both gone, got %+v", state.Backstops)
	}
}

// --- v2 merged ReserveEmissionData (EmisData carries eps/expiration) ---------

func TestDecodeState_EmisDataV2MergedStruct(t *testing.T) {
	t.Parallel()

	adapter := newTestAdapter(t)
	poolID := validContractString(t, 1)
	changes := []bindings.ContractDataChange{
		stateChange(t, poolID, symbolVal(t, "ResList"), vecVal(contractAddressVal(t, 2))),
		stateChange(t, poolID, variantVal(t, "ResConfig", contractAddressVal(t, 2)), mapVal(t, map[string]xdr.ScVal{
			"index":    u32Val(1),
			"decimals": u32Val(7),
		})),
		// v2: no EmisConfig entry exists — EmisData alone carries all four
		// ReserveEmissionData fields. The old decode dropped eps/expiration
		// here, so on the v2 deploy an activated emission never surfaced.
		stateChange(t, poolID, variantVal(t, "EmisData", u32Val(3)), mapVal(t, map[string]xdr.ScVal{
			"expiration": u64Val(1_800_000_000),
			"eps":        u64Val(2_500),
			"index":      i128Val(123),
			"last_time":  u64Val(1_700_000_000),
		})),
	}
	state, err := adapter.DecodeState(nil, changes, 100)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	reserve := state.Pools[0].Reserves[0]
	if reserve.SupplyEmisEPSRaw != "2500" || reserve.SupplyEmisExpirationRaw != "1800000000" {
		t.Fatalf("v2 EmisData eps/expiration dropped: eps=%q expiration=%q", reserve.SupplyEmisEPSRaw, reserve.SupplyEmisExpirationRaw)
	}
	if reserve.SupplyEmisIndexRaw != "123" || reserve.SupplyEmisLastTimeRaw != "1700000000" {
		t.Fatalf("v2 EmisData index/last_time = %q/%q", reserve.SupplyEmisIndexRaw, reserve.SupplyEmisLastTimeRaw)
	}
	// Borrow side untouched — absent, never zero-filled.
	if reserve.BorrowEmisEPSRaw != "" || reserve.BorrowEmisIndexRaw != "" {
		t.Fatalf("borrow side fabricated: %+v", reserve)
	}
}

// TestDecodeState_EmisDataV1ShapeStillPartial pins the v1 split regression: an
// EmisData with only {index, last_time} must not invent eps/expiration.
func TestDecodeState_EmisDataV1ShapeStillPartial(t *testing.T) {
	t.Parallel()

	reserve := &reserveBuilder{}
	applyReserveEmisData(reserve, 1, mapVal(t, map[string]xdr.ScVal{
		"index":     i128Val(5),
		"last_time": u64Val(1_700_000_000),
	}))
	if reserve.state.SupplyEmisEPSRaw != "" || reserve.state.SupplyEmisExpirationRaw != "" {
		t.Fatalf("v1-shaped EmisData fabricated eps/expiration: %q/%q",
			reserve.state.SupplyEmisEPSRaw, reserve.state.SupplyEmisExpirationRaw)
	}
	if reserve.state.SupplyEmisIndexRaw != "5" {
		t.Fatalf("index not decoded: %q", reserve.state.SupplyEmisIndexRaw)
	}
}

// TestClearReserveEmisData_ClearsAllFour: with v2's merged struct, an EmisData
// entry going not-live takes eps/expiration with it — leaving them set would
// fabricate an active emission that no longer exists on-chain.
func TestClearReserveEmisData_ClearsAllFour(t *testing.T) {
	t.Parallel()

	reserve := &reserveBuilder{state: contracts.ReserveState{
		SupplyEmisEPSRaw:        "2500",
		SupplyEmisExpirationRaw: "1800000000",
		SupplyEmisIndexRaw:      "123",
		SupplyEmisLastTimeRaw:   "1700000000",
		BorrowEmisEPSRaw:        "9",
	}}
	clearReserveEmisData(reserve, 1)
	if reserve.state.SupplyEmisEPSRaw != "" || reserve.state.SupplyEmisExpirationRaw != "" ||
		reserve.state.SupplyEmisIndexRaw != "" || reserve.state.SupplyEmisLastTimeRaw != "" {
		t.Fatalf("EmisData clear left residue: %+v", reserve.state)
	}
	if reserve.state.BorrowEmisEPSRaw != "9" {
		t.Fatalf("other side clobbered: %+v", reserve.state)
	}
}

func TestTransform_ReserveEmissionCarriesIndexAndLastTime(t *testing.T) {
	t.Parallel()

	adapter, err := New(Config{V2WasmHashes: map[string]struct{}{"known-v2": {}}})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	out, err := adapter.Transform(bindings.TransformInput{
		LedgerSeq: 400,
		CloseTime: time.Unix(4000, 0).UTC(),
		State: &bindings.LedgerState{
			Pools: []contracts.PoolState{{
				ContractID: "CPOOL",
				WasmHash:   "known-v2",
				Reserves: []contracts.ReserveState{{
					ReserveIndex:            1,
					ReserveIndexKnown:       true,
					AssetID:                 "CASSET",
					AssetDecimals:           7,
					BRateRaw:                "1000000000000",
					DRateRaw:                "1000000000000",
					BSupplyRaw:              "1000",
					DSupplyRaw:              "0",
					SupplyEmisEPSRaw:        "2500",
					SupplyEmisExpirationRaw: "1800000000",
					SupplyEmisIndexRaw:      "123",
					SupplyEmisLastTimeRaw:   "1700000000",
					// Borrow side: accrual only (index/last_time), no eps — the
					// row must still surface so relay#26 sees the checkpoint.
					BorrowEmisIndexRaw:    "9",
					BorrowEmisLastTimeRaw: "1700000001",
				}},
			}},
		},
	})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if len(out.ReserveEmissions) != 2 {
		t.Fatalf("expected 2 emission rows, got %d: %+v", len(out.ReserveEmissions), out.ReserveEmissions)
	}
	supply := out.ReserveEmissions[0]
	if supply.Side != "supply" || supply.EPSRaw != "2500" || supply.IndexRaw != "123" || supply.LastTimeRaw != "1700000000" {
		t.Fatalf("supply emission row = %+v", supply)
	}
	borrow := out.ReserveEmissions[1]
	if borrow.Side != "borrow" || borrow.EPSRaw != "" || borrow.IndexRaw != "9" {
		t.Fatalf("borrow emission row = %+v", borrow)
	}
}

// --- ResData cheap wins (backstop_credit, last_time) --------------------------

func TestApplyReserveData_BackstopCreditAndLastTime(t *testing.T) {
	t.Parallel()

	reserve := &reserveBuilder{}
	applyReserveData(reserve, mapVal(t, map[string]xdr.ScVal{
		"d_rate":          i128Val(1_000_000),
		"b_rate":          i128Val(1_000_000),
		"b_supply":        i128Val(100),
		"d_supply":        i128Val(20),
		"backstop_credit": i128MaxVal(),
		"last_time":       u64Val(1_752_000_000),
	}))
	if reserve.state.BackstopCreditRaw != i128MaxString {
		t.Fatalf("BackstopCreditRaw = %q, want %s", reserve.state.BackstopCreditRaw, i128MaxString)
	}
	// PoolBalanceRaw keeps carrying backstop_credit for existing consumers.
	if reserve.state.PoolBalanceRaw != i128MaxString {
		t.Fatalf("PoolBalanceRaw compat broken: %q", reserve.state.PoolBalanceRaw)
	}
	if reserve.state.LastTimeRaw != "1752000000" {
		t.Fatalf("LastTimeRaw = %q, want 1752000000", reserve.state.LastTimeRaw)
	}

	// Absent keys stay absent.
	bare := &reserveBuilder{}
	applyReserveData(bare, mapVal(t, map[string]xdr.ScVal{
		"d_rate": i128Val(1),
	}))
	if bare.state.BackstopCreditRaw != "" || bare.state.LastTimeRaw != "" {
		t.Fatalf("absent ResData fields fabricated: credit=%q last_time=%q",
			bare.state.BackstopCreditRaw, bare.state.LastTimeRaw)
	}
}

// --- regression + determinism gates ------------------------------------------

// TestDecodeState_PreexistingDecodeUnchanged is the byte-level regression pin:
// folding the pre-existing representative change set (no new key kinds beyond
// what main already decoded) must produce exactly the same values in every
// previously-decoded field, and the new fields must stay at their absent zero
// values except BackstopCreditRaw/LastTimeRaw, which decode from the same
// ResData map main already parsed.
func TestDecodeState_PreexistingDecodeUnchanged(t *testing.T) {
	t.Parallel()

	adapter := newTestAdapter(t)
	state, err := adapter.DecodeState(nil, representativeChanges(t), 123)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(state.Pools) != 1 || len(state.Pools[0].Reserves) != 1 {
		t.Fatalf("unexpected state shape: %+v", state.Pools)
	}
	pool := state.Pools[0]
	reserve := pool.Reserves[0]

	// Previously-decoded fields, pinned value for value.
	checks := map[string]struct{ got, want string }{
		"BackstopContract":    {pool.BackstopContract, validContractString(t, 4)},
		"BackstopTakeRate":    {pool.BackstopTakeRate, "1000000"},
		"PoolStatus":          {pool.PoolStatus, "active"},
		"DRateRaw":            {reserve.DRateRaw, "1000000"},
		"BRateRaw":            {reserve.BRateRaw, "1000000"},
		"BSupplyRaw":          {reserve.BSupplyRaw, "100"},
		"DSupplyRaw":          {reserve.DSupplyRaw, "20"},
		"PoolBalanceRaw":      {reserve.PoolBalanceRaw, "42"},
		"ReactivityRaw":       {reserve.ReactivityRaw, "20000"},
		"SupplyEmisEPSRaw":    {reserve.SupplyEmisEPSRaw, "1000000"},
		"SupplyEmisIndexRaw":  {reserve.SupplyEmisIndexRaw, "0"},
		"BackstopSharesRaw":   {pool.BackstopSharesRaw, "2000"},
		"BackstopTokensRaw":   {pool.BackstopTokensRaw, "5000"},
		"NewBackstopCredit":   {reserve.BackstopCreditRaw, "42"},
		"NewLastTime":         {reserve.LastTimeRaw, ""},
		"NewBackstopEmisEPS":  {pool.BackstopEmisEPSRaw, ""},
		"NewBackstopEmisIdx":  {pool.BackstopEmisIndexRaw, ""},
		"NewBackstopEmisExp":  {pool.BackstopEmisExpirationRaw, ""},
		"NewBackstopEmisLast": {pool.BackstopEmisLastTimeRaw, ""},
		"NewPoolName":         {pool.Name, ""},
		"NewPoolAdmin":        {pool.Admin, ""},
		"NewPoolBLNDToken":    {pool.BLNDToken, ""},
		"NewMaxPositions":     {pool.MaxPositionsRaw, ""},
		"NewMinCollateral":    {pool.MinCollateralRaw, ""},
	}
	for name, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", name, c.got, c.want)
		}
	}
	if len(state.Auctions) != 0 || len(state.UserEmissions) != 0 ||
		len(state.QueuedReserves) != 0 || len(state.BackstopInstances) != 0 {
		t.Fatalf("new slices fabricated entries: auctions=%d userEmissions=%d queued=%d instances=%d",
			len(state.Auctions), len(state.UserEmissions), len(state.QueuedReserves), len(state.BackstopInstances))
	}
	if pool.PoolEmissions != nil {
		t.Fatalf("pool emissions fabricated: %+v", pool.PoolEmissions)
	}
	if len(state.Backstops) != 1 || state.Backstops[0].UnclaimedEmissionsRaw != "" || state.Backstops[0].EmisIndexRaw != "" {
		t.Fatalf("backstop row fabricated emission fields: %+v", state.Backstops)
	}
}

// decodeGapChanges is representativeChanges plus every new key kind — the
// change set the determinism and strategy-parity gates below fold.
func decodeGapChanges(t *testing.T) []bindings.ContractDataChange {
	t.Helper()
	poolID := validContractString(t, 1)
	backstopID := validContractString(t, 4)
	changes := representativeChanges(t)
	return append(changes,
		stateChange(t, poolID, auctionKeyVal(t, accountAddressVal(t, 5), 0), mapVal(t, map[string]xdr.ScVal{
			"bid":   mapVal(t, map[string]xdr.ScVal{}),
			"lot":   mapVal(t, map[string]xdr.ScVal{}),
			"block": u32Val(77),
		})),
		stateChange(t, poolID, userEmisKeyVal(t, accountAddressVal(t, 5), 3), mapVal(t, map[string]xdr.ScVal{
			"index":   i128Val(10),
			"accrued": i128Val(20),
		})),
		stateChange(t, backstopID, variantVal(t, "BEmisData", contractAddressVal(t, 1)), mapVal(t, map[string]xdr.ScVal{
			"expiration": u64Val(1_800_000_000),
			"eps":        u64Val(5_000),
			"index":      i128Val(777),
			"last_time":  u64Val(1_700_000_000),
		})),
		stateChange(t, backstopID, variantVal(t, "UEmisData", mapVal(t, map[string]xdr.ScVal{
			"pool": contractAddressVal(t, 1),
			"user": accountAddressVal(t, 5),
		})), mapVal(t, map[string]xdr.ScVal{
			"index":   i128Val(1),
			"accrued": i128Val(2),
		})),
		stateChange(t, poolID, resInitKeyVal(t, contractAddressVal(t, 6)), mapVal(t, map[string]xdr.ScVal{
			"new_config":  fullQueuedConfigVal(t),
			"unlock_time": u64Val(1_800_000_000),
		})),
		stateChange(t, poolID, symbolVal(t, "PoolEmis"), intMapVal(t, map[uint32]xdr.ScVal{
			2: u64Val(1_500_000),
			3: u64Val(8_500_000),
		})),
		stateChange(t, backstopID, symbolVal(t, "RZ"), vecVal(contractAddressVal(t, 1))),
		stateChange(t, backstopID, symbolVal(t, "DropList"), vecVal(vecVal(accountAddressVal(t, 5), i128Val(9)))),
		stateChange(t, backstopID, xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
			instanceVal(backstopInstanceStorage(t), 9)),
	)
}

func TestDecodeState_NewEntitiesRunTwiceByteIdentical(t *testing.T) {
	t.Parallel()

	adapter := newTestAdapter(t)
	changes := decodeGapChanges(t)
	first, err := adapter.DecodeState(nil, changes, 500)
	if err != nil {
		t.Fatalf("decode first: %v", err)
	}
	second, err := adapter.DecodeState(nil, changes, 500)
	if err != nil {
		t.Fatalf("decode second: %v", err)
	}
	if len(first.Auctions) == 0 || len(first.UserEmissions) == 0 {
		t.Fatalf("expected non-trivial new-entity state: auctions=%d userEmissions=%d",
			len(first.Auctions), len(first.UserEmissions))
	}
	b1, _ := json.Marshal(first)
	b2, _ := json.Marshal(second)
	if !bytes.Equal(b1, b2) {
		t.Fatalf("run-twice output not byte-identical:\nfirst=%s\nsecond=%s", b1, b2)
	}
}

// TestDecodeState_NewEntitiesStrategyParity folds the same ledgers through the
// paranoid and incremental strategies and requires byte-identical output —
// including a second, carried ledger, so the incremental mirror's
// normalizeCarry round-trip of the new state is exercised too.
func TestDecodeState_NewEntitiesStrategyParity(t *testing.T) {
	t.Parallel()

	paranoid := newTestAdapter(t)
	incremental, err := New(Config{StateMode: StateModeIncremental})
	if err != nil {
		t.Fatalf("new incremental adapter: %v", err)
	}

	changes := decodeGapChanges(t)
	p1, err := paranoid.DecodeState(nil, changes, 500)
	if err != nil {
		t.Fatalf("paranoid ledger 1: %v", err)
	}
	i1, err := incremental.DecodeState(nil, changes, 500)
	if err != nil {
		t.Fatalf("incremental ledger 1: %v", err)
	}
	pb1, _ := json.Marshal(p1)
	ib1, _ := json.Marshal(i1)
	if !bytes.Equal(pb1, ib1) {
		t.Fatalf("strategy outputs diverge on ledger 1:\nparanoid=%s\nincremental=%s", pb1, ib1)
	}

	// Ledger 2: no changes — pure carry through both strategies.
	p2, err := paranoid.DecodeState(p1, nil, 501)
	if err != nil {
		t.Fatalf("paranoid ledger 2: %v", err)
	}
	i2, err := incremental.DecodeState(i1, nil, 501)
	if err != nil {
		t.Fatalf("incremental ledger 2: %v", err)
	}
	pb2, _ := json.Marshal(p2)
	ib2, _ := json.Marshal(i2)
	if !bytes.Equal(pb2, ib2) {
		t.Fatalf("strategy outputs diverge on carried ledger 2:\nparanoid=%s\nincremental=%s", pb2, ib2)
	}
}
