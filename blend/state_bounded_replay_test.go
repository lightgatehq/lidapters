package blend

// Regression tests for the bounded-replay reserve-index defect (lidapters#33):
// a reserve materialized by a ResData write alone keeps Go's zero-value
// ReserveIndex (0), wins reserveByIndex[0] over the real index-0 reserve, and
// positionsFromMap then attributes another reserve's position legs to the
// wrong asset while silently dropping legs whose true index has no mapping.
// A pinned-start bounded replay without seeded config is the exposed path.
//
// The corrected contract: reserve-index validity is explicit
// (ReserveState.ReserveIndexKnown, set only by a decoded ResConfig.index or by
// config hydration), only known and unique indexes resolve, and every skipped
// non-zero leg surfaces as a structured per-ledger DecodeDiagnostic instead of
// a wrong-but-plausible row.

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// resDataChange is the in-window ResData write a bounded replay sees without
// its governance-time ResConfig.
func resDataChange(t *testing.T, poolID string, asset xdr.ScVal) bindings.ContractDataChange {
	t.Helper()
	return stateChange(t, poolID, variantVal(t, "ResData", asset), mapVal(t, map[string]xdr.ScVal{
		"d_rate":   i128Val(962409238681),
		"b_rate":   i128Val(1_000_000_000),
		"b_supply": i128Val(5000),
		"d_supply": i128Val(1200),
	}))
}

func resConfigChange(t *testing.T, poolID string, asset xdr.ScVal, index uint32) bindings.ContractDataChange {
	t.Helper()
	return stateChange(t, poolID, variantVal(t, "ResConfig", asset), mapVal(t, map[string]xdr.ScVal{
		"index":    u32Val(index),
		"decimals": u32Val(7),
		"enabled":  boolVal(true),
	}))
}

// witnessPositionsChange is the two-reserve Positions shape of the #33
// witness: bucket 0 is the XLM legs, bucket 1 the USDC legs.
func witnessPositionsChange(t *testing.T, poolID string, user xdr.ScVal) bindings.ContractDataChange {
	t.Helper()
	return stateChange(t, poolID, variantVal(t, "Positions", user), mapVal(t, map[string]xdr.ScVal{
		"collateral":  intMapVal(t, map[uint32]xdr.ScVal{0: i128Val(210346315861), 1: i128Val(16523965334)}),
		"liabilities": intMapVal(t, map[uint32]xdr.ScVal{0: i128Val(14746315917), 1: i128Val(12665205938)}),
	}))
}

// TestReserveIndexKnown_ResDataBeforeConfigSkipsAndDiagnoses is the core #33
// repro: a bounded replay where only USDC's ResData has folded (no ResConfig,
// no ResList, no config seed) and a user's Positions entry carries the real
// two-reserve buckets. Neither bucket may resolve: index 0 has no configured
// reserve behind it and USDC's true index is unknown, so both legs are skipped
// — never attributed to USDC through the zero-value default — and every skip
// is diagnosed.
func TestReserveIndexKnown_ResDataBeforeConfigSkipsAndDiagnoses(t *testing.T) {
	t.Parallel()
	adapter := newTestAdapter(t)
	poolID := validContractString(t, 1)
	usdcAsset := contractAddressVal(t, 7)
	usdcID := scValAddress(usdcAsset)
	user := accountAddressVal(t, 5)
	userID := scValAddress(user)

	state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		resDataChange(t, poolID, usdcAsset),
		witnessPositionsChange(t, poolID, user),
	}, 100)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(state.Users) != 0 {
		t.Fatalf("unmapped position legs resolved anyway: %+v", state.Users)
	}

	// Every skipped non-zero leg surfaces exactly once, carrying the ledger,
	// pool, user, position type, raw index, raw amount, and the candidate —
	// the one unknown-index reserve the leg could belong to.
	diags := adapter.LastDecodeDiagnostics()
	type wantDiag struct {
		positionType string
		index        int32
		amount       string
	}
	want := []wantDiag{
		{"collateral", 0, "210346315861"},
		{"collateral", 1, "16523965334"},
		{"liability", 0, "14746315917"},
		{"liability", 1, "12665205938"},
	}
	if len(diags) != len(want) {
		t.Fatalf("diagnostics = %+v, want %d skipped-leg records", diags, len(want))
	}
	for i, w := range want {
		d := diags[i]
		if d.Code != bindings.DecodeDiagnosticUnmappedReserveIndex {
			t.Errorf("diags[%d].Code = %q, want %q", i, d.Code, bindings.DecodeDiagnosticUnmappedReserveIndex)
		}
		if d.LedgerSeq != 100 || d.PoolContractID != poolID || d.Address != userID {
			t.Errorf("diags[%d] identity = {ledger %d, pool %q, address %q}, want {100, %q, %q}",
				i, d.LedgerSeq, d.PoolContractID, d.Address, poolID, userID)
		}
		if d.PositionType != w.positionType || d.ReserveIndex != w.index || d.AmountRaw != w.amount {
			t.Errorf("diags[%d] = {%s, index %d, amount %q}, want {%s, index %d, amount %q}",
				i, d.PositionType, d.ReserveIndex, d.AmountRaw, w.positionType, w.index, w.amount)
		}
		if len(d.CandidateAssetIDs) != 1 || d.CandidateAssetIDs[0] != usdcID {
			t.Errorf("diags[%d].CandidateAssetIDs = %v, want [%s] (the ResData-only reserve)", i, d.CandidateAssetIDs, usdcID)
		}
	}
}

// TestReserveIndexKnown_RealZeroResolves is the control: a reserve whose
// ResConfig decoded index 0 is legitimate, and positions against it must keep
// resolving. Index zero is a valid index — validity may never be inferred
// from ReserveIndex != 0.
func TestReserveIndexKnown_RealZeroResolves(t *testing.T) {
	t.Parallel()
	adapter := newTestAdapter(t)
	poolID := validContractString(t, 1)
	xlmAsset := contractAddressVal(t, 2)
	usdcAsset := contractAddressVal(t, 7)
	xlmID := scValAddress(xlmAsset)
	usdcID := scValAddress(usdcAsset)

	state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		resConfigChange(t, poolID, xlmAsset, 0),
		resConfigChange(t, poolID, usdcAsset, 1),
		stateChange(t, poolID, variantVal(t, "Positions", accountAddressVal(t, 5)), mapVal(t, map[string]xdr.ScVal{
			"collateral":  intMapVal(t, map[uint32]xdr.ScVal{0: i128Val(300), 1: i128Val(100)}),
			"liabilities": intMapVal(t, map[uint32]xdr.ScVal{1: i128Val(40)}),
		})),
	}, 100)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]string{
		xlmID + "|collateral":  "300",
		usdcID + "|collateral": "100",
		usdcID + "|liability":  "40",
	}
	got := map[string]string{}
	for _, u := range state.Users {
		amount := u.BTokensRaw
		if u.PositionType == "liability" {
			amount = u.DTokensRaw
		}
		got[u.AssetID+"|"+string(u.PositionType)] = amount
	}
	for key, amount := range want {
		if got[key] != amount {
			t.Errorf("position %s = %q, want %q (all legs: %+v)", key, got[key], amount, state.Users)
		}
	}
	if len(got) != len(want) {
		t.Errorf("resolved %d legs, want %d: %+v", len(got), len(want), state.Users)
	}
	if diags := adapter.LastDecodeDiagnostics(); len(diags) != 0 {
		t.Errorf("diagnostics = %+v, want none — every index is configured and unique", diags)
	}
}

// TestReserveIndexKnown_DuplicateKnownIndexesAreAmbiguous pins the second
// unsafe case: two reserves whose ResConfigs claim the SAME index. Choosing a
// deterministic winner would still misattribute, so the index resolves to
// nothing, the affected legs are skipped, and the diagnostic names both
// claiming assets — while a third, uniquely configured reserve keeps
// resolving.
func TestReserveIndexKnown_DuplicateKnownIndexesAreAmbiguous(t *testing.T) {
	t.Parallel()
	adapter := newTestAdapter(t)
	poolID := validContractString(t, 1)
	assetA := contractAddressVal(t, 2)
	assetB := contractAddressVal(t, 7)
	assetC := contractAddressVal(t, 9)
	assetAID := scValAddress(assetA)
	assetBID := scValAddress(assetB)
	assetCID := scValAddress(assetC)
	user := accountAddressVal(t, 5)

	state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		// A remap in flight: the index-0 claim is duplicated.
		resConfigChange(t, poolID, assetA, 0),
		resConfigChange(t, poolID, assetB, 0),
		resConfigChange(t, poolID, assetC, 1),
		stateChange(t, poolID, variantVal(t, "Positions", user), mapVal(t, map[string]xdr.ScVal{
			"collateral": intMapVal(t, map[uint32]xdr.ScVal{0: i128Val(700), 1: i128Val(300)}),
		})),
	}, 100)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Bucket 1 still resolves to the uniquely configured reserve; bucket 0 is
	// ambiguous and emits nothing.
	if len(state.Users) != 1 || state.Users[0].AssetID != assetCID || state.Users[0].BTokensRaw != "300" {
		t.Fatalf("users = %+v, want only the unambiguous bucket-1 leg (asset %s, 300)", state.Users, assetCID)
	}

	diags := adapter.LastDecodeDiagnostics()
	if len(diags) != 1 {
		t.Fatalf("diagnostics = %+v, want exactly one duplicate record", diags)
	}
	d := diags[0]
	if d.Code != bindings.DecodeDiagnosticDuplicateReserveIndex {
		t.Errorf("code = %q, want %q", d.Code, bindings.DecodeDiagnosticDuplicateReserveIndex)
	}
	wantCandidates := []string{assetAID, assetBID}
	if assetAID > assetBID {
		wantCandidates[0], wantCandidates[1] = wantCandidates[1], wantCandidates[0]
	}
	if fmt.Sprint(d.CandidateAssetIDs) != fmt.Sprint(wantCandidates) {
		t.Errorf("candidates = %v, want the sorted claiming assets %v", d.CandidateAssetIDs, wantCandidates)
	}
	if d.PositionType != "collateral" || d.ReserveIndex != 0 || d.AmountRaw != "700" {
		t.Errorf("diagnostic = {%s, index %d, amount %q}, want {collateral, 0, 700}",
			d.PositionType, d.ReserveIndex, d.AmountRaw)
	}
}

// TestReserveIndexKnown_LaterConfigRemapReemitsUsers pins the recovery path: a
// bounded replay first skips and diagnoses the unmapped legs, and when the
// missing ResConfig writes later fold, the remap dirties every user of the
// pool so the corrected legs are emitted without waiting for each user's
// Positions entry to change.
func TestReserveIndexKnown_LaterConfigRemapReemitsUsers(t *testing.T) {
	t.Parallel()
	adapter := newTestAdapter(t)
	poolID := validContractString(t, 1)
	xlmAsset := contractAddressVal(t, 2)
	usdcAsset := contractAddressVal(t, 7)
	xlmID := scValAddress(xlmAsset)
	usdcID := scValAddress(usdcAsset)
	user := accountAddressVal(t, 5)
	userID := scValAddress(user)

	// Ledger 100: the ResData-only window — both buckets unmapped.
	prior, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		resDataChange(t, poolID, usdcAsset),
		witnessPositionsChange(t, poolID, user),
	}, 100)
	if err != nil {
		t.Fatalf("decode ledger 100: %v", err)
	}
	if len(prior.Users) != 0 {
		t.Fatalf("ledger 100 users = %+v, want none", prior.Users)
	}
	if len(adapter.LastDecodeDiagnostics()) != 4 {
		t.Fatalf("ledger 100 diagnostics = %+v, want 4", adapter.LastDecodeDiagnostics())
	}

	// Ledger 101: the governance-time ResConfig writes fold (XLM at index 0,
	// USDC at index 1). No Positions entry changes, yet the user must be
	// re-emitted with the corrected attribution.
	state, err := adapter.DecodeState(prior, []bindings.ContractDataChange{
		resConfigChange(t, poolID, xlmAsset, 0),
		resConfigChange(t, poolID, usdcAsset, 1),
	}, 101)
	if err != nil {
		t.Fatalf("decode ledger 101: %v", err)
	}

	want := map[string]string{
		xlmID + "|collateral":  "210346315861",
		usdcID + "|collateral": "16523965334",
		xlmID + "|liability":   "14746315917",
		usdcID + "|liability":  "12665205938",
	}
	got := map[string]string{}
	for _, u := range state.Users {
		amount := u.BTokensRaw
		if u.PositionType == "liability" {
			amount = u.DTokensRaw
		}
		got[u.AssetID+"|"+string(u.PositionType)] = amount
	}
	for key, amount := range want {
		if got[key] != amount {
			t.Errorf("position %s = %q, want %q (all legs: %+v)", key, got[key], amount, state.Users)
		}
	}
	if len(got) != len(want) {
		t.Errorf("resolved %d legs after remap, want %d: %+v", len(got), len(want), state.Users)
	}

	dirty := adapter.LastDirtyPositions()
	if len(dirty) != 1 || dirty[0].Address != userID || dirty[0].PoolContractID != poolID || dirty[0].Kind != bindings.DirtyUpsert {
		t.Errorf("dirty = %+v, want the pool's one user re-emitted by the remap", dirty)
	}
	if diags := adapter.LastDecodeDiagnostics(); len(diags) != 0 {
		t.Errorf("ledger 101 diagnostics = %+v, want none — every index now resolves", diags)
	}
}

// TestReserveIndexKnown_HydratesLegacyConfigIndexZero pins the relay's
// seedConfigState resume path: a blend.reserve record exists only because a
// ResConfig was decoded, so hydrating one — including a pre-release record
// whose payload has no index-validity field — marks the index known, and an
// index-0 reserve from such a record resolves positions exactly like a folded
// ResConfig.
func TestReserveIndexKnown_HydratesLegacyConfigIndexZero(t *testing.T) {
	t.Parallel()
	adapter := newTestAdapter(t)
	poolID := validContractString(t, 1)
	xlmAsset := contractAddressVal(t, 2)
	xlmID := scValAddress(xlmAsset)
	user := accountAddressVal(t, 5)

	// The legacy payload shape: no ReserveIndexKnown field exists in the JSON
	// (the field predates the fix), and the index is the legitimate zero.
	legacyPayload := []byte(`{"pool_id":"` + poolID + `","asset_id":"` + xlmID + `","index":0,"decimals":7,"c_factor":"8000000","l_factor":"9000000","util_target":"","max_util":"","r_base":"","r_one":"","r_two":"","r_three":"","supply_cap":"","reactivity":"","enabled":true,"supply_emis_eps":"","supply_emis_expiration":"","borrow_emis_eps":"","borrow_emis_expiration":""}`)
	seed, err := adapter.HydrateConfig([]bindings.ConfigRecord{
		{Kind: kindPool, EntityKey: poolID, Payload: []byte(`{"oracle":"","backstop":"","status":"active","take_rate":"","wasm_hash":""}`)},
		{Kind: kindReserve, EntityKey: poolID + reserveKeySep + xlmID, Payload: legacyPayload},
	})
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if len(seed.Pools) != 1 || len(seed.Pools[0].Reserves) != 1 {
		t.Fatalf("seed = %+v, want 1 pool with 1 reserve", seed.Pools)
	}
	hydrated := seed.Pools[0].Reserves[0]
	if !hydrated.ReserveIndexKnown || hydrated.ReserveIndex != 0 {
		t.Fatalf("hydrated reserve = {index %d, known %v}, want {0, true} — a decoded config record implies a known index",
			hydrated.ReserveIndex, hydrated.ReserveIndexKnown)
	}

	// The seeded index-0 reserve resolves positions on the very first fold.
	state, err := adapter.DecodeState(seed, []bindings.ContractDataChange{
		stateChange(t, poolID, variantVal(t, "Positions", user), mapVal(t, map[string]xdr.ScVal{
			"collateral": intMapVal(t, map[uint32]xdr.ScVal{0: i128Val(300)}),
		})),
	}, 100)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(state.Users) != 1 || state.Users[0].AssetID != xlmID || state.Users[0].BTokensRaw != "300" {
		t.Fatalf("users = %+v, want the index-0 leg resolved to %s", state.Users, xlmID)
	}
	if diags := adapter.LastDecodeDiagnostics(); len(diags) != 0 {
		t.Errorf("diagnostics = %+v, want none", diags)
	}
}

// TestReserveIndexDiagnostics_DeterministicOrder pins the exposed diagnostic
// set's stable total order and run-twice byte identity: it is collected from
// map-iteration folds, so without the sort the same ledger could serialize
// differently run to run.
func TestReserveIndexDiagnostics_DeterministicOrder(t *testing.T) {
	t.Parallel()
	pool1 := validContractString(t, 1)
	pool2 := validContractString(t, 9)
	usdcAsset := contractAddressVal(t, 7)

	changes := []bindings.ContractDataChange{
		resDataChange(t, pool1, usdcAsset),
		resDataChange(t, pool2, usdcAsset),
		witnessPositionsChange(t, pool1, accountAddressVal(t, 5)),
		witnessPositionsChange(t, pool1, accountAddressVal(t, 6)),
		witnessPositionsChange(t, pool2, accountAddressVal(t, 5)),
	}

	adapter := newTestAdapter(t)
	if _, err := adapter.DecodeState(nil, changes, 100); err != nil {
		t.Fatalf("decode: %v", err)
	}
	diags := adapter.LastDecodeDiagnostics()
	if len(diags) != 12 {
		t.Fatalf("diagnostics = %d records, want 12 (3 users x 4 legs)", len(diags))
	}

	less := func(a, b bindings.DecodeDiagnostic) bool {
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		if a.PoolContractID != b.PoolContractID {
			return a.PoolContractID < b.PoolContractID
		}
		if a.Address != b.Address {
			return a.Address < b.Address
		}
		if a.PositionType != b.PositionType {
			return a.PositionType < b.PositionType
		}
		if a.ReserveIndex != b.ReserveIndex {
			return a.ReserveIndex < b.ReserveIndex
		}
		return a.AmountRaw < b.AmountRaw
	}
	for i := 1; i < len(diags); i++ {
		if less(diags[i], diags[i-1]) {
			t.Fatalf("diagnostics not sorted at index %d: %+v before %+v", i, diags[i-1], diags[i])
		}
	}

	second := newTestAdapter(t)
	if _, err := second.DecodeState(nil, changes, 100); err != nil {
		t.Fatalf("decode second: %v", err)
	}
	a, b := mustJSON(t, adapter.LastDecodeDiagnostics()), mustJSON(t, second.LastDecodeDiagnostics())
	if !bytes.Equal(a, b) {
		t.Fatalf("diagnostics not run-twice identical:\nfirst=%s\nsecond=%s", a, b)
	}
}
