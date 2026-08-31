// Decode-gap closure tests, batch 2 (lidapters#9 follow-up): ResInit queued
// reserve changes, the v2 PoolEmis split, pool instance identity + PoolConfig
// remainder, backstop instance identity (addresses, RZ, DropList) and the
// oracle instance facets + timestamp freshness entry. Same adversarial
// standard as batch 1: every decode is refuted with missing keys, wrong ScVal
// types and extremes, proving absent/skip — never fabricated zeros or panics.
package blend

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/lightgatehq/lidapters/blend/contracts"
	"github.com/stellar/go-stellar-sdk/xdr"
)

func instanceVal(storage xdr.ScMap, wasmSeed byte) xdr.ScVal {
	var wasm xdr.Hash
	wasm[31] = wasmSeed
	return xdr.ScVal{
		Type: xdr.ScValTypeScvContractInstance,
		Instance: &xdr.ScContractInstance{
			Executable: xdr.ContractExecutable{
				Type:     xdr.ContractExecutableTypeContractExecutableWasm,
				WasmHash: &wasm,
			},
			Storage: &storage,
		},
	}
}

func resInitKeyVal(t *testing.T, asset xdr.ScVal) xdr.ScVal {
	t.Helper()
	return variantVal(t, "ResInit", asset)
}

func fullQueuedConfigVal(t *testing.T) xdr.ScVal {
	t.Helper()
	return mapVal(t, map[string]xdr.ScVal{
		"index":      u32Val(2),
		"decimals":   u32Val(7),
		"c_factor":   u32Val(7_500_000),
		"l_factor":   u32Val(9_000_000),
		"util":       u32Val(6_000_000),
		"max_util":   u32Val(9_500_000),
		"r_base":     u32Val(5_000),
		"r_one":      u32Val(300_000),
		"r_two":      u32Val(1_000_000),
		"r_three":    u32Val(10_000_000),
		"reactivity": u32Val(20_000),
		"supply_cap": i128MaxVal(),
		"enabled":    boolVal(true),
	})
}

// --- ResInit (queued reserve changes) ----------------------------------------

func TestDecodeState_QueuedReserveRoundTrip(t *testing.T) {
	t.Parallel()

	adapter := newTestAdapter(t)
	poolID := validContractString(t, 1)
	asset := validContractString(t, 2)

	state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		stateChange(t, poolID, resInitKeyVal(t, contractAddressVal(t, 2)), mapVal(t, map[string]xdr.ScVal{
			"new_config":  fullQueuedConfigVal(t),
			"unlock_time": u64Val(1_800_000_000),
		})),
	}, 100)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []contracts.QueuedReserveState{{
		PoolContractID: poolID,
		AssetID:        asset,
		UnlockTimeRaw:  "1800000000",
		NewConfig: contracts.QueuedReserveConfig{
			IndexRaw: "2", DecimalsRaw: "7", CFactorRaw: "7500000", LFactorRaw: "9000000",
			UtilRaw: "6000000", MaxUtilRaw: "9500000", RBaseRaw: "5000", ROneRaw: "300000",
			RTwoRaw: "1000000", RThreeRaw: "10000000", ReactivityRaw: "20000",
			SupplyCapRaw: i128MaxString, Enabled: "true",
		},
	}}
	if !reflect.DeepEqual(state.QueuedReserves, want) {
		t.Fatalf("queued reserves = %+v, want %+v", state.QueuedReserves, want)
	}
	// A queue for a brand-new asset must NOT fabricate a pool or a live
	// reserve — it takes effect only when set_reserve executes.
	if len(state.Pools) != 0 {
		t.Fatalf("ResInit fabricated a pool: %+v", state.Pools)
	}

	carried, err := adapter.DecodeState(state, nil, 101)
	if err != nil {
		t.Fatalf("decode carry: %v", err)
	}
	if !reflect.DeepEqual(carried.QueuedReserves, want) {
		t.Fatalf("carried queued reserves = %+v", carried.QueuedReserves)
	}

	// set_reserve executed / cancel_set_reserve: the entry is removed.
	resolved, err := adapter.DecodeState(carried, []bindings.ContractDataChange{
		stateChange(t, poolID, resInitKeyVal(t, contractAddressVal(t, 2)), i128Val(0),
			withLive(false), withNoValue(), withChangeType("Removed")),
	}, 102)
	if err != nil {
		t.Fatalf("decode removal: %v", err)
	}
	if len(resolved.QueuedReserves) != 0 {
		t.Fatalf("expected queued reserve removed, got %+v", resolved.QueuedReserves)
	}
}

func TestDecodeState_QueuedReserveMalformedFailsSafe(t *testing.T) {
	t.Parallel()

	poolID := validContractString(t, 1)
	cases := []struct {
		name  string
		key   xdr.ScVal
		value xdr.ScVal
	}{
		{"missing_unlock_time", resInitKeyVal(t, contractAddressVal(t, 2)), mapVal(t, map[string]xdr.ScVal{
			"new_config": fullQueuedConfigVal(t),
		})},
		{"missing_new_config", resInitKeyVal(t, contractAddressVal(t, 2)), mapVal(t, map[string]xdr.ScVal{
			"unlock_time": u64Val(1_800_000_000),
		})},
		{"new_config_not_a_map", resInitKeyVal(t, contractAddressVal(t, 2)), mapVal(t, map[string]xdr.ScVal{
			"new_config":  i128Val(1),
			"unlock_time": u64Val(1_800_000_000),
		})},
		{"value_not_a_map", resInitKeyVal(t, contractAddressVal(t, 2)), i128Val(1)},
		{"key_missing_asset", variantVal(t, "ResInit"), mapVal(t, map[string]xdr.ScVal{
			"new_config":  fullQueuedConfigVal(t),
			"unlock_time": u64Val(1_800_000_000),
		})},
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
			if len(state.QueuedReserves) != 0 {
				t.Fatalf("malformed %s produced a queued reserve: %+v", tc.name, state.QueuedReserves)
			}
		})
	}
}

func TestTransform_QueuedReserveOutput(t *testing.T) {
	t.Parallel()

	adapter, err := New(Config{V2WasmHashes: map[string]struct{}{"known-v2": {}}})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	out, err := adapter.Transform(bindings.TransformInput{
		LedgerSeq: 500,
		CloseTime: time.Unix(5000, 0).UTC(),
		State: &bindings.LedgerState{
			QueuedReserves: []contracts.QueuedReserveState{{
				PoolContractID: "CPOOL",
				AssetID:        "CASSET",
				UnlockTimeRaw:  "1800000000",
				// Partial config: only the fields present on-chain surface.
				NewConfig: contracts.QueuedReserveConfig{CFactorRaw: "7500000", Enabled: "false"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if len(out.QueuedReserves) != 1 {
		t.Fatalf("expected 1 queued reserve row, got %d", len(out.QueuedReserves))
	}
	row := out.QueuedReserves[0]
	if row.UnlockTimeRaw != "1800000000" || !row.UnlockTime.Equal(time.Unix(1_800_000_000, 0).UTC()) {
		t.Fatalf("unlock time = %q / %s", row.UnlockTimeRaw, row.UnlockTime)
	}
	if !reflect.DeepEqual(row.NewConfig, map[string]string{"c_factor": "7500000", "enabled": "false"}) {
		t.Fatalf("new config surfaced absent fields: %+v", row.NewConfig)
	}
}

// --- PoolEmis (per-reserve emission split) -----------------------------------

func TestDecodeState_PoolEmisRoundTrip(t *testing.T) {
	t.Parallel()

	adapter := newTestAdapter(t)
	poolID := validContractString(t, 1)
	configChange := stateChange(t, poolID, symbolVal(t, "Config"), mapVal(t, map[string]xdr.ScVal{
		"oracle":     contractAddressVal(t, 3),
		"bstop_rate": u32Val(1_000_000),
		"status":     u32Val(1),
	}))

	state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		configChange,
		stateChange(t, poolID, symbolVal(t, "PoolEmis"), intMapVal(t, map[uint32]xdr.ScVal{
			3: u64Val(8_500_000),
			2: u64Val(1_500_000),
		})),
	}, 100)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []contracts.PoolEmissionEntry{
		{ReserveTokenID: 2, ShareRaw: "1500000"},
		{ReserveTokenID: 3, ShareRaw: "8500000"},
	}
	if !reflect.DeepEqual(state.Pools[0].PoolEmissions, want) {
		t.Fatalf("pool emissions = %+v, want sorted %+v", state.Pools[0].PoolEmissions, want)
	}

	// A malformed live write (v1 PoolEmissionConfig shape — symbol keys, not
	// u32) keeps the carried split rather than wiping it.
	afterBad, err := adapter.DecodeState(state, []bindings.ContractDataChange{
		stateChange(t, poolID, symbolVal(t, "PoolEmis"), mapVal(t, map[string]xdr.ScVal{
			"config":    u64Val(1),
			"last_time": u64Val(2),
		})),
	}, 101)
	if err != nil {
		t.Fatalf("decode malformed: %v", err)
	}
	if !reflect.DeepEqual(afterBad.Pools[0].PoolEmissions, want) {
		t.Fatalf("malformed PoolEmis clobbered carried split: %+v", afterBad.Pools[0].PoolEmissions)
	}

	// Deletion clears the split.
	deleted, err := adapter.DecodeState(afterBad, []bindings.ContractDataChange{
		stateChange(t, poolID, symbolVal(t, "PoolEmis"), i128Val(0),
			withLive(false), withNoValue(), withChangeType("Removed")),
	}, 102)
	if err != nil {
		t.Fatalf("decode delete: %v", err)
	}
	if deleted.Pools[0].PoolEmissions != nil {
		t.Fatalf("deleted PoolEmis left residue: %+v", deleted.Pools[0].PoolEmissions)
	}
}

func TestTransform_PoolEmissionsInContractMetadata(t *testing.T) {
	t.Parallel()

	adapter, err := New(Config{V2WasmHashes: map[string]struct{}{"known-v2": {}}})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	out, err := adapter.Transform(bindings.TransformInput{
		LedgerSeq: 600,
		CloseTime: time.Unix(6000, 0).UTC(),
		State: &bindings.LedgerState{
			Pools: []contracts.PoolState{{
				ContractID:    "CPOOL",
				WasmHash:      "known-v2",
				PoolEmissions: []contracts.PoolEmissionEntry{{ReserveTokenID: 3, ShareRaw: "8500000"}},
			}},
		},
	})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if len(out.Contracts) != 1 {
		t.Fatalf("expected 1 contract row, got %d", len(out.Contracts))
	}
	var split map[string]string
	if err := json.Unmarshal([]byte(out.Contracts[0].Metadata["pool_emissions"]), &split); err != nil {
		t.Fatalf("pool_emissions not JSON: %q", out.Contracts[0].Metadata["pool_emissions"])
	}
	if split["3"] != "8500000" {
		t.Fatalf("pool_emissions = %v", split)
	}
}

// --- pool instance identity + PoolConfig remainder ----------------------------

func TestDecodeState_PoolInstanceIdentity(t *testing.T) {
	t.Parallel()

	adapter, err := New(Config{AllowUnknownV2: true})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	poolID := validContractString(t, 1)
	adminID := validAccountString(t, 6)
	blndID := validContractString(t, 7)

	storage := xdr.ScMap{
		{Key: symbolVal(t, "Config"), Val: mapVal(t, map[string]xdr.ScVal{
			"oracle":         contractAddressVal(t, 3),
			"bstop_rate":     u32Val(1_000_000),
			"status":         u32Val(1),
			"max_positions":  u32Val(4),
			"min_collateral": i128Val(10_000_000),
		})},
		{Key: symbolVal(t, "Name"), Val: stringVal(t, "Fixed XLM-USDC")},
		{Key: symbolVal(t, "Admin"), Val: accountAddressVal(t, 6)},
		{Key: symbolVal(t, "BLNDTkn"), Val: contractAddressVal(t, 7)},
	}
	instanceKey := xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance}
	state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		stateChange(t, poolID, instanceKey, instanceVal(storage, 7)),
	}, 100)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	pool := state.Pools[0]
	if pool.Name != "Fixed XLM-USDC" {
		t.Fatalf("Name = %q", pool.Name)
	}
	if pool.Admin != adminID || pool.BLNDToken != blndID {
		t.Fatalf("Admin/BLNDToken = %q / %q", pool.Admin, pool.BLNDToken)
	}
	if pool.MaxPositionsRaw != "4" || pool.MinCollateralRaw != "10000000" {
		t.Fatalf("max_positions/min_collateral = %q / %q", pool.MaxPositionsRaw, pool.MinCollateralRaw)
	}
}

func TestApplyPoolConfig_MissingRemainderStaysAbsent(t *testing.T) {
	t.Parallel()

	pool := ensurePool(map[string]*poolBuilder{}, "CPOOL")
	applyPoolConfig(pool, mapVal(t, map[string]xdr.ScVal{
		"oracle":     contractAddressVal(t, 3),
		"bstop_rate": u32Val(1_000_000),
		"status":     u32Val(1),
	}))
	if pool.state.MaxPositionsRaw != "" || pool.state.MinCollateralRaw != "" {
		t.Fatalf("absent PoolConfig fields fabricated: %q / %q",
			pool.state.MaxPositionsRaw, pool.state.MinCollateralRaw)
	}

	// Extreme: min_collateral is an i128 — the negative extreme round-trips.
	extreme := ensurePool(map[string]*poolBuilder{}, "CPOOL2")
	applyPoolConfig(extreme, mapVal(t, map[string]xdr.ScVal{
		"min_collateral": i128MinVal(),
	}))
	if extreme.state.MinCollateralRaw != i128MinString {
		t.Fatalf("min_collateral extreme = %q", extreme.state.MinCollateralRaw)
	}
}

func TestTransform_PoolIdentityInContractMetadata(t *testing.T) {
	t.Parallel()

	adapter, err := New(Config{V2WasmHashes: map[string]struct{}{"known-v2": {}}})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	out, err := adapter.Transform(bindings.TransformInput{
		LedgerSeq: 700,
		CloseTime: time.Unix(7000, 0).UTC(),
		State: &bindings.LedgerState{
			Pools: []contracts.PoolState{
				{ContractID: "CPOOL", WasmHash: "known-v2", Name: "Fixed", Admin: "GADMIN",
					BLNDToken: "CBLND", MaxPositionsRaw: "4", MinCollateralRaw: "10000000"},
				{ContractID: "CPOOL2", WasmHash: "known-v2"},
			},
		},
	})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if len(out.Contracts) != 2 {
		t.Fatalf("expected 2 contract rows, got %d", len(out.Contracts))
	}
	meta := out.Contracts[0].Metadata
	for key, want := range map[string]string{
		"pool_name": "Fixed", "pool_admin": "GADMIN", "blnd_token": "CBLND",
		"max_positions": "4", "min_collateral": "10000000",
	} {
		if meta[key] != want {
			t.Errorf("metadata[%s] = %q, want %q", key, meta[key], want)
		}
	}
	// Absent facets stay absent from metadata — never emitted as "".
	bare := out.Contracts[1].Metadata
	for _, key := range []string{"pool_name", "pool_admin", "blnd_token", "max_positions", "min_collateral", "pool_emissions"} {
		if _, present := bare[key]; present {
			t.Errorf("absent facet %s surfaced on bare pool: %q", key, bare[key])
		}
	}
}

// --- backstop instance identity -----------------------------------------------

func backstopInstanceStorage(t *testing.T) xdr.ScMap {
	t.Helper()
	return xdr.ScMap{
		{Key: symbolVal(t, "BToken"), Val: contractAddressVal(t, 10)},
		{Key: symbolVal(t, "BLNDTkn"), Val: contractAddressVal(t, 11)},
		{Key: symbolVal(t, "USDCTkn"), Val: contractAddressVal(t, 12)},
		{Key: symbolVal(t, "Emitter"), Val: contractAddressVal(t, 13)},
		{Key: symbolVal(t, "PoolFact"), Val: contractAddressVal(t, 14)},
	}
}

func TestDecodeState_BackstopInstanceIdentity(t *testing.T) {
	t.Parallel()

	adapter := newTestAdapter(t)
	backstopID := validContractString(t, 4)
	instanceKey := xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance}

	state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		stateChange(t, backstopID, instanceKey, instanceVal(backstopInstanceStorage(t), 9)),
		stateChange(t, backstopID, symbolVal(t, "RZ"), vecVal(contractAddressVal(t, 1), contractAddressVal(t, 2))),
		stateChange(t, backstopID, symbolVal(t, "DropList"), vecVal(
			vecVal(accountAddressVal(t, 5), i128MaxVal()),
		)),
	}, 100)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(state.BackstopInstances) != 1 {
		t.Fatalf("expected 1 backstop instance, got %d", len(state.BackstopInstances))
	}
	got := state.BackstopInstances[0]
	if got.BackstopToken != validContractString(t, 10) || got.Emitter != validContractString(t, 13) ||
		got.PoolFactory != validContractString(t, 14) || got.BLNDToken != validContractString(t, 11) ||
		got.USDCToken != validContractString(t, 12) {
		t.Fatalf("backstop instance = %+v", got)
	}
	if !reflect.DeepEqual(got.RewardZone, []string{validContractString(t, 1), validContractString(t, 2)}) {
		t.Fatalf("reward zone = %+v", got.RewardZone)
	}
	if len(got.DropList) != 1 || got.DropList[0].AmountRaw != i128MaxString {
		t.Fatalf("drop list = %+v", got.DropList)
	}
	// The backstop instance must NOT be misdecoded as a phantom pool anymore.
	if len(state.Pools) != 0 {
		t.Fatalf("backstop instance fabricated a pool: %+v", state.Pools)
	}

	// Carry, then instance delete clears the identity.
	carried, err := adapter.DecodeState(state, nil, 101)
	if err != nil {
		t.Fatalf("decode carry: %v", err)
	}
	if len(carried.BackstopInstances) != 1 {
		t.Fatalf("carried backstop instance lost: %+v", carried.BackstopInstances)
	}
	deleted, err := adapter.DecodeState(carried, []bindings.ContractDataChange{
		stateChange(t, backstopID, instanceKey, i128Val(0),
			withLive(false), withNoValue(), withChangeType("Removed")),
	}, 102)
	if err != nil {
		t.Fatalf("decode delete: %v", err)
	}
	if len(deleted.BackstopInstances) != 0 {
		t.Fatalf("deleted backstop instance left residue: %+v", deleted.BackstopInstances)
	}
}

func TestDecodeState_BackstopListsMalformedFailSafe(t *testing.T) {
	t.Parallel()

	adapter := newTestAdapter(t)
	backstopID := validContractString(t, 4)
	instanceKey := xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance}
	prior, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		stateChange(t, backstopID, instanceKey, instanceVal(backstopInstanceStorage(t), 9)),
		stateChange(t, backstopID, symbolVal(t, "RZ"), vecVal(contractAddressVal(t, 1))),
	}, 100)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	// A reward zone containing a non-address, and a drop list with a
	// malformed pair, are rejected whole — the carried lists stay put.
	next, err := adapter.DecodeState(prior, []bindings.ContractDataChange{
		stateChange(t, backstopID, symbolVal(t, "RZ"), vecVal(contractAddressVal(t, 2), u32Val(9))),
		stateChange(t, backstopID, symbolVal(t, "DropList"), vecVal(vecVal(accountAddressVal(t, 5)))),
	}, 101)
	if err != nil {
		t.Fatalf("decode malformed: %v", err)
	}
	got := next.BackstopInstances[0]
	if !reflect.DeepEqual(got.RewardZone, []string{validContractString(t, 1)}) {
		t.Fatalf("malformed RZ clobbered carried zone: %+v", got.RewardZone)
	}
	if got.DropList != nil {
		t.Fatalf("malformed drop list surfaced: %+v", got.DropList)
	}
}

func TestTransform_BackstopInstanceContractRow(t *testing.T) {
	t.Parallel()

	adapter, err := New(Config{V2WasmHashes: map[string]struct{}{"known-v2": {}}})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	out, err := adapter.Transform(bindings.TransformInput{
		LedgerSeq: 800,
		CloseTime: time.Unix(8000, 0).UTC(),
		State: &bindings.LedgerState{
			BackstopInstances: []contracts.BackstopInstanceState{{
				ContractID:    "CBACKSTOP",
				BackstopToken: "CCOMETLP",
				Emitter:       "CEMITTER",
				RewardZone:    []string{"CPOOL"},
				DropList:      []contracts.DropListEntry{{Address: "GDROPPED", AmountRaw: "5"}},
			}},
		},
	})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if len(out.Contracts) != 1 {
		t.Fatalf("expected 1 contract row, got %d", len(out.Contracts))
	}
	row := out.Contracts[0]
	if row.ContractType != "backstop" || row.Address != "CBACKSTOP" {
		t.Fatalf("backstop contract row = %+v", row)
	}
	if row.Metadata["backstop_token"] != "CCOMETLP" || row.Metadata["emitter"] != "CEMITTER" {
		t.Fatalf("metadata = %+v", row.Metadata)
	}
	if _, present := row.Metadata["usdc_token"]; present {
		t.Fatalf("absent address surfaced: %+v", row.Metadata)
	}
	if row.Metadata["reward_zone"] != `["CPOOL"]` {
		t.Fatalf("reward_zone = %q", row.Metadata["reward_zone"])
	}
	if row.Metadata["drop_list"] != `{"GDROPPED":"5"}` {
		t.Fatalf("drop_list = %q", row.Metadata["drop_list"])
	}
}

// --- oracle instance facets + freshness ---------------------------------------

func oracleInstanceStorage(t *testing.T) xdr.ScMap {
	t.Helper()
	return xdr.ScMap{
		{Key: symbolVal(t, "admin"), Val: accountAddressVal(t, 6)},
		{Key: symbolVal(t, "assets"), Val: vecVal(
			vecVal(symbolVal(t, "Stellar"), contractAddressVal(t, 2)),
		)},
		{Key: symbolVal(t, "base"), Val: vecVal(symbolVal(t, "Other"), symbolVal(t, "USD"))},
		{Key: symbolVal(t, "decimals"), Val: u32Val(14)},
		{Key: symbolVal(t, "res"), Val: u32Val(300)},
	}
}

func TestDecodeState_OracleInstanceFacetsAndTimestamp(t *testing.T) {
	t.Parallel()

	adapter := newTestAdapter(t)
	oracleID := validContractString(t, 3)
	adapter.RegisterContracts(oracleID)
	instanceKey := xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance}

	state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		stateChange(t, oracleID, instanceKey, instanceVal(oracleInstanceStorage(t), 5)),
		stateChange(t, oracleID, symbolVal(t, "timestamp"), u64Val(1_752_000_000)),
	}, 100)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(state.Oracles) != 1 {
		t.Fatalf("expected 1 oracle, got %d", len(state.Oracles))
	}
	oracle := state.Oracles[0]
	if oracle.BaseKey != "other:USD" {
		t.Fatalf("base key = %q", oracle.BaseKey)
	}
	if oracle.ResolutionRaw != "300" || oracle.Admin != validAccountString(t, 6) {
		t.Fatalf("res/admin = %q / %q", oracle.ResolutionRaw, oracle.Admin)
	}
	if oracle.LastTimestampRaw != "1752000000" {
		t.Fatalf("timestamp = %q", oracle.LastTimestampRaw)
	}

	// All four facets survive the carry.
	carried, err := adapter.DecodeState(state, nil, 101)
	if err != nil {
		t.Fatalf("decode carry: %v", err)
	}
	if carried.Oracles[0].LastTimestampRaw != "1752000000" || carried.Oracles[0].BaseKey != "other:USD" {
		t.Fatalf("carried oracle facets lost: %+v", carried.Oracles[0])
	}
}

func TestDecodeState_OracleTimestampUnknownContractFabricatesNothing(t *testing.T) {
	t.Parallel()

	adapter := newTestAdapter(t)
	state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		stateChange(t, validContractString(t, 9), symbolVal(t, "timestamp"), u64Val(1_752_000_000)),
	}, 100)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(state.Oracles) != 0 || len(state.Pools) != 0 {
		t.Fatalf("bare timestamp write fabricated state: oracles=%d pools=%d",
			len(state.Oracles), len(state.Pools))
	}
}

func TestDecodeState_OracleFacetsWrongTypesStayAbsent(t *testing.T) {
	t.Parallel()

	adapter := newTestAdapter(t)
	oracleID := validContractString(t, 3)
	adapter.RegisterContracts(oracleID)
	storage := xdr.ScMap{
		{Key: symbolVal(t, "admin"), Val: u32Val(1)},
		{Key: symbolVal(t, "assets"), Val: vecVal(vecVal(symbolVal(t, "Stellar"), contractAddressVal(t, 2)))},
		{Key: symbolVal(t, "base"), Val: u32Val(7)},
		{Key: symbolVal(t, "decimals"), Val: u32Val(14)},
		{Key: symbolVal(t, "res"), Val: boolVal(true)},
	}
	instanceKey := xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance}
	state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		stateChange(t, oracleID, instanceKey, instanceVal(storage, 5)),
	}, 100)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	oracle := state.Oracles[0]
	if oracle.BaseKey != "" || oracle.ResolutionRaw != "" || oracle.Admin != "" {
		t.Fatalf("wrong-typed facets fabricated: %+v", oracle)
	}
	if oracle.Decimals != 14 || len(oracle.Assets) != 1 {
		t.Fatalf("core oracle decode disturbed: %+v", oracle)
	}
}

func TestTransform_ReserveCarriesOracleFreshness(t *testing.T) {
	t.Parallel()

	adapter, err := New(Config{V2WasmHashes: map[string]struct{}{"known-v2": {}}})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	reserve := contracts.ReserveState{
		AssetID: "CASSET", AssetDecimals: 7,
		BRateRaw: "1000000000000", DRateRaw: "1000000000000",
		BSupplyRaw: "1000", DSupplyRaw: "0",
	}
	out, err := adapter.Transform(bindings.TransformInput{
		LedgerSeq: 900,
		CloseTime: time.Unix(9000, 0).UTC(),
		State: &bindings.LedgerState{
			Pools: []contracts.PoolState{{
				ContractID: "CPOOL", WasmHash: "known-v2", OracleContract: "CORACLE",
				Reserves: []contracts.ReserveState{reserve},
			}},
			Oracles: []contracts.OracleState{{
				ContractID: "CORACLE", Decimals: 14,
				LastTimestampRaw: "1752000000", ResolutionRaw: "300",
			}},
		},
	})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if len(out.Reserves) != 1 {
		t.Fatalf("expected 1 reserve, got %d", len(out.Reserves))
	}
	meta := out.Reserves[0].Metadata
	if meta["oracle_timestamp"] != "1752000000" || meta["oracle_resolution"] != "300" {
		t.Fatalf("freshness metadata = ts %q res %q", meta["oracle_timestamp"], meta["oracle_resolution"])
	}

	// Absent freshness stays absent — a price with no timestamp must stay
	// visibly timestamp-less, never fabricated.
	bare, err := adapter.Transform(bindings.TransformInput{
		LedgerSeq: 901,
		CloseTime: time.Unix(9010, 0).UTC(),
		State: &bindings.LedgerState{
			Pools: []contracts.PoolState{{
				ContractID: "CPOOL", WasmHash: "known-v2", OracleContract: "CORACLE",
				Reserves: []contracts.ReserveState{reserve},
			}},
		},
	})
	if err != nil {
		t.Fatalf("transform bare: %v", err)
	}
	bareMeta := bare.Reserves[0].Metadata
	for _, key := range []string{"oracle_timestamp", "oracle_resolution"} {
		if _, present := bareMeta[key]; present {
			t.Fatalf("absent freshness fabricated: %q=%q", key, bareMeta[key])
		}
	}
}
