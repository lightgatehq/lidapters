package blend

import (
	"bytes"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/lightgatehq/lidapters/blend/contracts"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// TestConfigRecords_EmitsLowFrequencyConfigOnly proves the adapter emits one
// config record per config entity present at the deploy/floor ledger — the oracle
// (asset->index map + decimals, NOT prices), the pool (refs/status/take-rate) and
// one reserve per ResConfig (factors/curve/caps, NOT ResData) — and nothing for
// the data half.
func TestConfigRecords_EmitsLowFrequencyConfigOnly(t *testing.T) {
	t.Parallel()

	layout := loadOracleLayout(t)
	adapter := newOracleCarryAdapter(t, layout)
	adapter.RegisterContracts(layout.PoolContract)

	changes := oracleSceneChanges(t, layout)
	next, err := adapter.DecodeState(nil, changes, layout.LedgerSeq)
	if err != nil {
		t.Fatalf("decode floor: %v", err)
	}

	records := adapter.ConfigRecords(next, ownedChanges(adapter, changes), layout.LedgerSeq)

	byKind := map[string]int{}
	for _, r := range records {
		byKind[r.Kind]++
		if r.Removed || len(r.Payload) == 0 {
			t.Fatalf("floor record %s/%s should be a live upsert, got removed=%v payload=%q", r.Kind, r.EntityKey, r.Removed, r.Payload)
		}
		if r.Ledger != uint32(layout.LedgerSeq) {
			t.Fatalf("record %s stamped ledger %d, want %d", r.EntityKey, r.Ledger, layout.LedgerSeq)
		}
	}
	if byKind[kindOracle] != 1 {
		t.Fatalf("want exactly 1 oracle record, got %d", byKind[kindOracle])
	}
	if byKind[kindPool] != 1 {
		t.Fatalf("want exactly 1 pool record, got %d", byKind[kindPool])
	}
	if byKind[kindReserve] != 4 {
		t.Fatalf("want 4 reserve records (one per ResConfig), got %d", byKind[kindReserve])
	}

	// Oracle payload carries the map + decimals but NOT prices (prices are data).
	var oracle oracleConfigBody
	mustUnmarshalKind(t, records, kindOracle, &oracle)
	if oracle.Decimals == 0 || len(oracle.Assets) != 4 {
		t.Fatalf("oracle payload wrong: decimals=%d assets=%d", oracle.Decimals, len(oracle.Assets))
	}
	if bytes.Contains(payloadOf(t, records, kindOracle), []byte("price")) {
		t.Fatalf("oracle payload must not carry prices: %s", payloadOf(t, records, kindOracle))
	}

	// Reserve payload carries ResConfig (c_factor) but NOT ResData (b_rate).
	reservePayload := payloadOf(t, records, kindReserve)
	if !bytes.Contains(reservePayload, []byte("c_factor")) {
		t.Fatalf("reserve payload missing c_factor: %s", reservePayload)
	}
	if bytes.Contains(reservePayload, []byte("b_rate")) || bytes.Contains(reservePayload, []byte("b_supply")) {
		t.Fatalf("reserve payload must not carry ResData: %s", reservePayload)
	}
}

// TestConfigRecords_Deterministic pins that the emitted records are a pure,
// run-twice byte-identical function of the fold inputs.
func TestConfigRecords_Deterministic(t *testing.T) {
	t.Parallel()
	layout := loadOracleLayout(t)
	adapter := newOracleCarryAdapter(t, layout)
	adapter.RegisterContracts(layout.PoolContract)
	changes := oracleSceneChanges(t, layout)
	next, err := adapter.DecodeState(nil, changes, layout.LedgerSeq)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	a := adapter.ConfigRecords(next, ownedChanges(adapter, changes), layout.LedgerSeq)
	b := adapter.ConfigRecords(next, ownedChanges(adapter, changes), layout.LedgerSeq)
	if !bytes.Equal(mustJSON(t, a), mustJSON(t, b)) {
		t.Fatalf("ConfigRecords not run-twice identical:\n a=%s\n b=%s", mustJSON(t, a), mustJSON(t, b))
	}
}

// TestHydrateConfig_RoundTripsConfigOnly proves the persisted records rebuild a
// seed LedgerState with the COMPLETE oracle (map + prices), pool config and reserve
// config, and no reserve DATA / positions (those re-fold from bronze). The oracle's
// prices are reloaded, not self-healed, so a restart has no null-HF window.
func TestHydrateConfig_RoundTripsConfigOnly(t *testing.T) {
	t.Parallel()
	layout := loadOracleLayout(t)
	adapter := newOracleCarryAdapter(t, layout)
	adapter.RegisterContracts(layout.PoolContract)

	changes := oracleSceneChanges(t, layout)
	next, err := adapter.DecodeState(nil, changes, layout.LedgerSeq)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	records := adapter.ConfigRecords(next, ownedChanges(adapter, changes), layout.LedgerSeq)

	seed, err := adapter.HydrateConfig(records)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if len(seed.Oracles) != 1 || len(seed.Oracles[0].Assets) != 4 {
		t.Fatalf("hydrated oracle wrong: %#v", seed.Oracles)
	}
	// The oracle is persisted completely: the map AND the prices are reloaded, so a
	// restart has no null-HF window (prices are present, not awaiting a set_price).
	if len(seed.Oracles[0].Prices) != 4 {
		t.Fatalf("hydrated oracle must carry all 4 reloaded prices, got %d", len(seed.Oracles[0].Prices))
	}
	if len(seed.Pools) != 1 || seed.Pools[0].OracleContract != layout.OracleContract {
		t.Fatalf("hydrated pool wrong: %#v", seed.Pools)
	}
	if len(seed.Pools[0].Reserves) != 4 {
		t.Fatalf("hydrated pool must carry 4 reserve configs, got %d", len(seed.Pools[0].Reserves))
	}
	for _, r := range seed.Pools[0].Reserves {
		if r.CFactorRaw == "" {
			t.Fatalf("hydrated reserve %s missing config c_factor", r.AssetID)
		}
		if r.BRateRaw != "" || r.BSupplyRaw != "" {
			t.Fatalf("hydrated reserve %s must carry no ResData, got b_rate=%q b_supply=%q", r.AssetID, r.BRateRaw, r.BSupplyRaw)
		}
	}
	if len(seed.Users) != 0 || len(seed.PendingUserPositions) != 0 {
		t.Fatalf("config-only seed must carry no positions, got users=%d pending=%d", len(seed.Users), len(seed.PendingUserPositions))
	}

	// Hydration is deterministic.
	again, _ := adapter.HydrateConfig(records)
	if !bytes.Equal(mustJSON(t, seed), mustJSON(t, again)) {
		t.Fatal("HydrateConfig not run-twice identical")
	}
}

// TestEmitGuard_ConfigOnlySeedWritesNoValuedRows is the no-null-overwrite proof
// (ResData axis) at the adapter tier: transforming a config-only seed (reserves
// with config but no folded ResData, no positions) emits ZERO reserves and ZERO
// summaries, so a restart cannot overwrite good gold with zero-valued rows before
// the data re-folds from bronze. The price axis (map present, price missing) is
// covered by the relay's TestRelayRestart_PriceUnavailableSuppressesSummary.
func TestEmitGuard_ConfigOnlySeedWritesNoValuedRows(t *testing.T) {
	t.Parallel()
	layout := loadOracleLayout(t)
	adapter := newOracleCarryAdapter(t, layout)
	adapter.RegisterContracts(layout.PoolContract)

	changes := oracleSceneChanges(t, layout)
	next, err := adapter.DecodeState(nil, changes, layout.LedgerSeq)
	if err != nil {
		t.Fatalf("decode floor: %v", err)
	}
	// Sanity: the full floor state DOES value the cross-asset wallet.
	floorOut, err := adapter.Transform(bindings.TransformInput{LedgerSeq: layout.LedgerSeq, State: next})
	if err != nil {
		t.Fatalf("transform floor: %v", err)
	}
	if len(floorOut.Reserves) == 0 || len(floorOut.Summaries) == 0 {
		t.Fatalf("floor must value the scene: reserves=%d summaries=%d", len(floorOut.Reserves), len(floorOut.Summaries))
	}

	seed, err := adapter.HydrateConfig(adapter.ConfigRecords(next, ownedChanges(adapter, changes), layout.LedgerSeq))
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	seedOut, err := adapter.Transform(bindings.TransformInput{LedgerSeq: layout.LedgerSeq + 1, State: seed})
	if err != nil {
		t.Fatalf("transform seed: %v", err)
	}
	if len(seedOut.Reserves) != 0 {
		t.Fatalf("config-only seed must emit no reserve rows (emit-guard), got %d", len(seedOut.Reserves))
	}
	if len(seedOut.Summaries) != 0 {
		t.Fatalf("config-only seed must emit no summary rows, got %d", len(seedOut.Summaries))
	}
}

// TestEmitGuard_SuppressesDataIncompleteSummary proves the no-null-overwrite
// property extends to per-account summaries on the ResData axis: after a
// config-only reload a cross-asset account whose one reserve's ResData has not
// folded yet is NOT emitted with an incomplete (single-leg) health factor — its
// summary is withheld so the account's good gold is preserved (stale-but-safe),
// while the reserve that
// does have data is still emitted.
func TestEmitGuard_SuppressesDataIncompleteSummary(t *testing.T) {
	t.Parallel()
	layout := loadOracleLayout(t)
	adapter := newOracleCarryAdapter(t, layout)
	adapter.RegisterContracts(layout.PoolContract)

	full := oracleSceneChanges(t, layout)
	fullState, err := adapter.DecodeState(nil, full, layout.LedgerSeq)
	if err != nil {
		t.Fatalf("decode full: %v", err)
	}
	fullOut, err := adapter.Transform(bindings.TransformInput{LedgerSeq: layout.LedgerSeq, State: fullState})
	if err != nil {
		t.Fatalf("transform full: %v", err)
	}
	if len(fullOut.Summaries) == 0 {
		t.Fatal("baseline: the cross-asset account should have a summary when all reserves have data")
	}

	// Drop the USDC ResData: the account's USDC liability leg then references a
	// config-only reserve (no folded data), so its summary must be suppressed.
	usdc := assetIDByCode(layout, "USDC")
	partial := dropResDataFor(t, full, usdc)
	partialState, err := adapter.DecodeState(nil, partial, layout.LedgerSeq)
	if err != nil {
		t.Fatalf("decode partial: %v", err)
	}
	partialOut, err := adapter.Transform(bindings.TransformInput{LedgerSeq: layout.LedgerSeq, State: partialState})
	if err != nil {
		t.Fatalf("transform partial: %v", err)
	}
	if len(partialOut.Summaries) != 0 {
		t.Fatalf("a data-incomplete account summary must be suppressed, got %d (hf=%q)",
			len(partialOut.Summaries), partialOut.Summaries[0].HealthFactor)
	}
	// The reserve that DOES have data (wBTC) is still emitted; only the config-only
	// USDC reserve is skipped.
	sawWBTC, sawUSDC := false, false
	for _, r := range partialOut.Reserves {
		switch r.AssetID {
		case assetIDByCode(layout, "wBTC"):
			sawWBTC = true
		case usdc:
			sawUSDC = true
		}
	}
	if !sawWBTC {
		t.Fatal("wBTC reserve (data present) must still be emitted")
	}
	if sawUSDC {
		t.Fatal("USDC reserve (config-only, data absent) must be skipped, not emitted at zero")
	}
}

// dropResDataFor returns a copy of changes with the ResData entry for assetID
// removed, leaving that reserve config-only (ResConfig present, ResData absent).
func dropResDataFor(t *testing.T, changes []bindings.ContractDataChange, assetID string) []bindings.ContractDataChange {
	t.Helper()
	out := make([]bindings.ContractDataChange, 0, len(changes))
	for _, c := range changes {
		if key, ok := decodeScValBase64(c.KeyXDR); ok {
			if variant, args, ok := scVariant(key); ok && variant == "ResData" {
				if a, ok := variantAddress(args); ok && a == assetID {
					continue
				}
			}
		}
		out = append(out, c)
	}
	return out
}

// TestConfigRecords_EmisConfigOnReserveRecord proves EmisConfig (relay#26) rides
// the SAME kindReserve config record as ResConfig — not a tombstone, and not a
// separate record — so a restart reloads the reserve's active emission config
// alongside its factors/curve, closing the same null-window class of bug the
// oracle price / enabled+reactivity persistence already closed. EmisData
// (index/last_time) is deliberately NOT asserted here — it is accrual data and
// re-folds from bronze, same split as ResData.
func TestConfigRecords_EmisConfigOnReserveRecord(t *testing.T) {
	t.Parallel()

	adapter := newTestAdapter(t)
	poolID := validContractString(t, 21)
	adapter.RegisterContracts(poolID)

	changes := []bindings.ContractDataChange{
		stateChange(t, poolID, symbolVal(t, "Config"), mapVal(t, map[string]xdr.ScVal{
			"oracle":     contractAddressVal(t, 23),
			"bstop_rate": u32Val(1_000_000),
			"status":     u32Val(1),
		})),
		stateChange(t, poolID, symbolVal(t, "ResList"), vecVal(contractAddressVal(t, 22))),
		stateChange(t, poolID, variantVal(t, "ResConfig", contractAddressVal(t, 22)), mapVal(t, map[string]xdr.ScVal{
			"index":    u32Val(0),
			"decimals": u32Val(7),
			"c_factor": u32Val(8_000_000),
			"l_factor": u32Val(9_000_000),
		})),
		// res_token_id = reserve_index*2 + side = 0*2 + 1 (supply side) = 1.
		stateChange(t, poolID, variantVal(t, "EmisConfig", u32Val(1)), mapVal(t, map[string]xdr.ScVal{
			"eps":        u64Val(2_000_000),
			"expiration": u64Val(1_900_000_000),
		})),
	}

	next, err := adapter.DecodeState(nil, changes, 500)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	records := adapter.ConfigRecords(next, ownedChanges(adapter, changes), 500)

	var reserveRecords []bindings.ConfigRecord
	for _, r := range records {
		if r.Kind == kindReserve {
			reserveRecords = append(reserveRecords, r)
		}
	}
	if len(reserveRecords) != 1 {
		t.Fatalf("expected 1 reserve config record, got %d", len(reserveRecords))
	}
	if reserveRecords[0].Removed {
		t.Fatal("reserve config record must not be a tombstone from an EmisConfig change")
	}
	var body reserveConfigBody
	if err := json.Unmarshal(reserveRecords[0].Payload, &body); err != nil {
		t.Fatalf("unmarshal reserve payload: %v", err)
	}
	if body.SupplyEmisEPS != "2000000" {
		t.Fatalf("expected supply_emis_eps=2000000, got %q", body.SupplyEmisEPS)
	}
	if body.SupplyEmisExpiration != "1900000000" {
		t.Fatalf("expected supply_emis_expiration=1900000000, got %q", body.SupplyEmisExpiration)
	}

	seed, err := adapter.HydrateConfig(records)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if len(seed.Pools) != 1 || len(seed.Pools[0].Reserves) != 1 {
		t.Fatalf("expected 1 pool with 1 reserve in hydrated seed, got pools=%d", len(seed.Pools))
	}
	hydrated := seed.Pools[0].Reserves[0]
	if hydrated.SupplyEmisEPSRaw != "2000000" {
		t.Fatalf("expected hydrated SupplyEmisEPSRaw=2000000, got %q", hydrated.SupplyEmisEPSRaw)
	}
	if hydrated.SupplyEmisExpirationRaw != "1900000000" {
		t.Fatalf("expected hydrated SupplyEmisExpirationRaw=1900000000, got %q", hydrated.SupplyEmisExpirationRaw)
	}
}

// --- helpers ---------------------------------------------------------------

// ownedChanges filters a change set to the adapter's owned contracts, mirroring
// what the relay projector hands to ConfigRecords.
func ownedChanges(a *Adapter, changes []bindings.ContractDataChange) []bindings.ContractDataChange {
	out := make([]bindings.ContractDataChange, 0, len(changes))
	for _, c := range changes {
		if a.OwnsContract(c.ContractID) {
			out = append(out, c)
		}
	}
	return out
}

func payloadOf(t *testing.T, records []bindings.ConfigRecord, kind string) []byte {
	t.Helper()
	for _, r := range records {
		if r.Kind == kind {
			return r.Payload
		}
	}
	t.Fatalf("no record of kind %s", kind)
	return nil
}

func mustUnmarshalKind(t *testing.T, records []bindings.ConfigRecord, kind string, dst any) {
	t.Helper()
	if err := json.Unmarshal(payloadOf(t, records, kind), dst); err != nil {
		t.Fatalf("unmarshal %s: %v", kind, err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestHydrateConfig_LegacyReservePayloadMarksIndexKnown pins that hydrating a
// blend.reserve record — including a pre-release payload with no
// index-validity field — marks the reserve's index known, legitimate index
// zero included. The record exists only because a ResConfig was decoded, so
// the known bit is provenance, not payload.
func TestHydrateConfig_LegacyReservePayloadMarksIndexKnown(t *testing.T) {
	t.Parallel()
	adapter := newTestAdapter(t)
	poolID := validContractString(t, 1)
	assetZero := validContractString(t, 2)
	assetOne := validContractString(t, 7)

	// Hand-written pre-release payloads: no index-known field anywhere.
	legacy := func(assetID string, index int) []byte {
		return []byte(`{"pool_id":"` + poolID + `","asset_id":"` + assetID + `","index":` + strconv.Itoa(index) + `,"decimals":7,"c_factor":"8000000","l_factor":"9000000","util_target":"","max_util":"","r_base":"","r_one":"","r_two":"","r_three":"","supply_cap":"","reactivity":"","enabled":true,"supply_emis_eps":"","supply_emis_expiration":"","borrow_emis_eps":"","borrow_emis_expiration":""}`)
	}
	seed, err := adapter.HydrateConfig([]bindings.ConfigRecord{
		{Kind: kindPool, EntityKey: poolID, Payload: []byte(`{"oracle":"","backstop":"","status":"active","take_rate":"","wasm_hash":""}`)},
		{Kind: kindReserve, EntityKey: poolID + reserveKeySep + assetZero, Payload: legacy(assetZero, 0)},
		{Kind: kindReserve, EntityKey: poolID + reserveKeySep + assetOne, Payload: legacy(assetOne, 1)},
	})
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if len(seed.Pools) != 1 || len(seed.Pools[0].Reserves) != 2 {
		t.Fatalf("seed = %+v, want 1 pool with 2 reserves", seed.Pools)
	}
	for _, r := range seed.Pools[0].Reserves {
		if !r.ReserveIndexKnown {
			t.Errorf("hydrated reserve %s (index %d) not marked known — a decoded config record implies a known index", r.AssetID, r.ReserveIndex)
		}
	}
	if _, ok := reserveByIndex(seed.Pools[0], 0); !ok {
		t.Errorf("legitimate index zero did not resolve after hydration")
	}
}

// TestReserveByIndex_RequiresKnownUniqueIndex pins the resolution rule the
// config-record and user-emission paths share with the fold: only a known,
// unique index resolves. An unknown index (never configured) and a duplicate
// known index are both unresolved — never a guessed winner.
func TestReserveByIndex_RequiresKnownUniqueIndex(t *testing.T) {
	t.Parallel()
	pool := contracts.PoolState{Reserves: []contracts.ReserveState{
		{AssetID: "A", ReserveIndex: 0, ReserveIndexKnown: true},
		{AssetID: "B", ReserveIndex: 1, ReserveIndexKnown: true},
		{AssetID: "C", ReserveIndex: 1, ReserveIndexKnown: true}, // duplicate of B
		{AssetID: "D"}, // ResData-only: index 0 default, not known
	}}

	if r, ok := reserveByIndex(pool, 0); !ok || r.AssetID != "A" {
		t.Errorf("index 0 = (%s, %v), want the legitimate configured reserve A", r.AssetID, ok)
	}
	if _, ok := reserveByIndex(pool, 1); ok {
		t.Errorf("duplicate known index 1 resolved, want ambiguous")
	}
	if _, ok := reserveByIndex(pool, 2); ok {
		t.Errorf("unmapped index 2 resolved, want unresolved")
	}
}
