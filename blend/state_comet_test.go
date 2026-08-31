package blend

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/lightgatehq/lidapters/blend/contracts"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// Comet (BToken) fold tests: the pinned persistent-entry layout
// (CometDEX/comet-contracts-v1 @ ef4cbfad — AllTokenVec Vec<Address>,
// AllRecordData Map<Address, Record{balance,weight,scalar,index}>, TotalShares
// i128), the backstop valuation join by exact token ID, and the
// affected-holder dirty set (V1-09, lidapters#31).

// cometFixture names every contract/account in the scenario by its seed.
type cometFixture struct {
	poolID     string // contract seed 1
	blndID     string // contract seed 2 (pool reserve AND backstop BLND token)
	oracleID   string // contract seed 3
	backstopID string // contract seed 4
	userA      string // account seed 5
	cometID    string // contract seed 6 (the BToken / Comet LP)
	usdcID     string // contract seed 7 (pool reserve AND backstop USDC token)
	userB      string // account seed 8
}

func newCometFixture(t *testing.T) cometFixture {
	t.Helper()
	return cometFixture{
		poolID:     validContractString(t, 1),
		blndID:     validContractString(t, 2),
		oracleID:   validContractString(t, 3),
		backstopID: validContractString(t, 4),
		userA:      validAccountString(t, 5),
		cometID:    validContractString(t, 6),
		usdcID:     validContractString(t, 7),
		userB:      validAccountString(t, 8),
	}
}

func (f cometFixture) register(adapter *Adapter) {
	adapter.RegisterContracts(f.poolID, f.backstopID, f.oracleID)
	adapter.RegisterCometContracts(f.cometID)
}

func u128Val(lo uint64) xdr.ScVal {
	raw := xdr.UInt128Parts{Hi: 0, Lo: xdr.Uint64(lo)}
	return xdr.ScVal{Type: xdr.ScValTypeScvU128, U128: &raw}
}

func cometRecordVal(t *testing.T, balance int64, index uint32) xdr.ScVal {
	t.Helper()
	return mapVal(t, map[string]xdr.ScVal{
		"balance": i128Val(balance),
		"weight":  i128Val(5_000_000),
		"scalar":  i128Val(10_000_000),
		"index":   u32Val(index),
	})
}

// cometRecordMapVal builds AllRecordData in the given entry order — the fold
// must be indifferent to it.
func cometRecordMapVal(t *testing.T, entries ...xdr.ScMapEntry) xdr.ScVal {
	t.Helper()
	raw := xdr.ScMap(entries)
	ptr := &raw
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &ptr}
}

// cometDeployChanges folds the full pinned state at one ledger: pool with both
// token reserves, oracle with both prices, backstop instance, pool/user
// balances, and the three Comet entries.
func (f cometFixture) deployChanges(t *testing.T) []bindings.ContractDataChange {
	t.Helper()
	blndAddr := contractAddressVal(t, 2)
	usdcAddr := contractAddressVal(t, 7)
	return []bindings.ContractDataChange{
		// Pool: oracle + backstop wiring, two reserves (BLND index 0, USDC index 1).
		stateChange(t, f.poolID, symbolVal(t, "Config"), mapVal(t, map[string]xdr.ScVal{
			"oracle":     contractAddressVal(t, 3),
			"bstop_rate": u32Val(1_000_000),
			"status":     u32Val(1),
		})),
		stateChange(t, f.poolID, symbolVal(t, "Backstop"), contractAddressVal(t, 4)),
		stateChange(t, f.poolID, symbolVal(t, "ResList"), vecVal(blndAddr, usdcAddr)),
		stateChange(t, f.poolID, variantVal(t, "ResConfig", blndAddr), mapVal(t, map[string]xdr.ScVal{
			"index":      u32Val(0),
			"decimals":   u32Val(7),
			"c_factor":   u32Val(8_000_000),
			"l_factor":   u32Val(9_000_000),
			"reactivity": u32Val(20_000),
			"enabled":    boolVal(true),
		})),
		stateChange(t, f.poolID, variantVal(t, "ResData", blndAddr), mapVal(t, map[string]xdr.ScVal{
			"d_rate":   i128Val(1_000_000_000_000),
			"b_rate":   i128Val(1_000_000_000_000),
			"b_supply": i128Val(1_000_000),
			"d_supply": i128Val(200_000),
		})),
		stateChange(t, f.poolID, variantVal(t, "ResConfig", usdcAddr), mapVal(t, map[string]xdr.ScVal{
			"index":      u32Val(1),
			"decimals":   u32Val(7),
			"c_factor":   u32Val(8_500_000),
			"l_factor":   u32Val(9_500_000),
			"reactivity": u32Val(20_000),
			"enabled":    boolVal(true),
		})),
		stateChange(t, f.poolID, variantVal(t, "ResData", usdcAddr), mapVal(t, map[string]xdr.ScVal{
			"d_rate":   i128Val(1_000_000_000_000),
			"b_rate":   i128Val(1_000_000_000_000),
			"b_supply": i128Val(2_000_000),
			"d_supply": i128Val(100_000),
		})),
		// Mock oracle: instance (assets in index order, 14 decimals) + one price
		// entry per index. BLND = 0.05 USD, USDC = 0.9999999 USD at 14dp.
		stateChange(t, f.oracleID, xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance}, instanceVal(xdr.ScMap{
			{Key: symbolVal(t, "assets"), Val: vecVal(
				variantVal(t, "Stellar", blndAddr),
				variantVal(t, "Stellar", usdcAddr),
			)},
			{Key: symbolVal(t, "decimals"), Val: u32Val(14)},
		}, 9)),
		stateChange(t, f.oracleID, u128Val(0), u128Val(5_000_000_000_000)),
		stateChange(t, f.oracleID, u128Val(1), u128Val(99_999_990_000_000)),
		// Backstop instance: BToken is the Comet contract; BLND/USDC identities.
		stateChange(t, f.backstopID, xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance}, instanceVal(xdr.ScMap{
			{Key: symbolVal(t, "BToken"), Val: contractAddressVal(t, 6)},
			{Key: symbolVal(t, "BLNDTkn"), Val: blndAddr},
			{Key: symbolVal(t, "USDCTkn"), Val: usdcAddr},
		}, 10)),
		stateChange(t, f.backstopID, variantVal(t, "PoolBalance", contractAddressVal(t, 1)), mapVal(t, map[string]xdr.ScVal{
			"shares": i128Val(10_000_000_000),
			"tokens": i128Val(40_000_000_000),
		})),
		stateChange(t, f.backstopID, variantVal(t, "UserBalance", mapVal(t, map[string]xdr.ScVal{
			"pool": contractAddressVal(t, 1),
			"user": accountAddressVal(t, 5),
		})), mapVal(t, map[string]xdr.ScVal{
			"shares": i128Val(5_000_000_000),
		})),
		stateChange(t, f.backstopID, variantVal(t, "UserBalance", mapVal(t, map[string]xdr.ScVal{
			"pool": contractAddressVal(t, 1),
			"user": accountAddressVal(t, 8),
		})), mapVal(t, map[string]xdr.ScVal{
			"shares": i128Val(1_000_000_000),
		})),
		// Comet: the three pinned persistent entries.
		stateChange(t, f.cometID, variantVal(t, "AllTokenVec"), vecVal(blndAddr, usdcAddr)),
		stateChange(t, f.cometID, variantVal(t, "AllRecordData"), cometRecordMapVal(t,
			xdr.ScMapEntry{Key: blndAddr, Val: cometRecordVal(t, 80_000_000_000_000, 0)},
			xdr.ScMapEntry{Key: usdcAddr, Val: cometRecordVal(t, 4_000_000_000_000, 1)},
		)),
		stateChange(t, f.cometID, variantVal(t, "TotalShares"), i128Val(100_000_000_000)),
	}
}

func foldCometDeploy(t *testing.T, adapter *Adapter, f cometFixture) *bindings.LedgerState {
	t.Helper()
	state, err := adapter.DecodeState(nil, f.deployChanges(t), 100)
	if err != nil {
		t.Fatalf("fold deploy ledger: %v", err)
	}
	return state
}

func cometPoolState(t *testing.T, state *bindings.LedgerState, cometID string) bindings.AMMPoolState {
	t.Helper()
	for _, pool := range state.AMMPools {
		if pool.ContractID == cometID {
			return pool
		}
	}
	t.Fatalf("no AMMPoolState for comet %s", cometID)
	return bindings.AMMPoolState{}
}

func backstopFor(t *testing.T, state *bindings.LedgerState, address, poolID string) contracts.BackstopPosition {
	t.Helper()
	for _, position := range state.Backstops {
		if position.Address == address && position.PoolContractID == poolID {
			return position
		}
	}
	t.Fatalf("no backstop position for %s in %s", address, poolID)
	panic("unreachable")
}

func TestCometFold_PinnedKeysDecode(t *testing.T) {
	t.Parallel()

	f := newCometFixture(t)
	adapter := newTestAdapter(t)
	f.register(adapter)
	state := foldCometDeploy(t, adapter, f)

	pool := cometPoolState(t, state, f.cometID)
	if pool.PoolType != "comet" {
		t.Fatalf("expected pool type comet, got %q", pool.PoolType)
	}
	if pool.Protocol != "blend" {
		t.Fatalf("expected protocol blend, got %q", pool.Protocol)
	}
	if pool.TotalSharesRaw != "100000000000" {
		t.Fatalf("expected total shares 100000000000, got %q", pool.TotalSharesRaw)
	}
	// Tokens sorted by address; reserves joined by address, not vec position.
	if len(pool.Tokens) != 2 {
		t.Fatalf("expected 2 tokens, got %+v", pool.Tokens)
	}
	reserves := map[string]string{}
	for _, token := range pool.Tokens {
		reserves[token.AssetID] = token.ReserveRaw
	}
	if reserves[f.blndID] != "80000000000000" {
		t.Fatalf("expected BLND reserve 80000000000000, got %q", reserves[f.blndID])
	}
	if reserves[f.usdcID] != "4000000000000" {
		t.Fatalf("expected USDC reserve 4000000000000, got %q", reserves[f.usdcID])
	}

	position := backstopFor(t, state, f.userA, f.poolID)
	if position.LPTokenSupplyRaw != "100000000000" {
		t.Fatalf("expected LP supply 100000000000, got %q", position.LPTokenSupplyRaw)
	}
	if position.LPBLNDReserveRaw != "80000000000000" {
		t.Fatalf("expected BLND reserve, got %q", position.LPBLNDReserveRaw)
	}
	if position.LPUSDCReserveRaw != "4000000000000" {
		t.Fatalf("expected USDC reserve, got %q", position.LPUSDCReserveRaw)
	}
	// Price bindings from the folded pool reserves: 0.05 and 0.9999999 at 14dp.
	if position.BLNDPriceUSD != "0.05" {
		t.Fatalf("expected BLND price 0.05, got %q", position.BLNDPriceUSD)
	}
	if position.USDCPriceUSD != "0.9999999" {
		t.Fatalf("expected USDC price 0.9999999, got %q", position.USDCPriceUSD)
	}
}

func TestCometFold_RejectsWrongLayout(t *testing.T) {
	t.Parallel()

	f := newCometFixture(t)
	adapter := newTestAdapter(t)
	f.register(adapter)
	state := foldCometDeploy(t, adapter, f)

	blndAddr := contractAddressVal(t, 2)
	bad := []bindings.ContractDataChange{
		// TotalShares as a map: wrong type — carried value survives.
		stateChange(t, f.cometID, variantVal(t, "TotalShares"), mapVal(t, map[string]xdr.ScVal{"x": i128Val(1)})),
		// AllTokenVec with a non-address element: whole write rejected.
		stateChange(t, f.cometID, variantVal(t, "AllTokenVec"), vecVal(blndAddr, u32Val(7))),
		// A record missing the weight field: whole map write rejected.
		stateChange(t, f.cometID, variantVal(t, "AllRecordData"), cometRecordMapVal(t,
			xdr.ScMapEntry{Key: blndAddr, Val: mapVal(t, map[string]xdr.ScVal{
				"balance": i128Val(1),
				"scalar":  i128Val(1),
				"index":   u32Val(0),
			})},
		)),
	}
	next, err := adapter.DecodeState(state, bad, 101)
	if err != nil {
		t.Fatalf("fold bad-layout ledger: %v", err)
	}
	before, _ := json.Marshal(state.AMMPools)
	after, _ := json.Marshal(next.AMMPools)
	if !bytes.Equal(before, after) {
		t.Fatalf("malformed Comet writes changed folded state:\nbefore=%s\nafter=%s", before, after)
	}
}

func TestCometFold_TokenOrderIrrelevant(t *testing.T) {
	t.Parallel()

	f := newCometFixture(t)
	blndAddr := contractAddressVal(t, 2)
	usdcAddr := contractAddressVal(t, 7)

	fold := func(t *testing.T, tokenVec []xdr.ScVal, recordEntries ...xdr.ScMapEntry) *bindings.LedgerState {
		t.Helper()
		adapter := newTestAdapter(t)
		f.register(adapter)
		changes := f.deployChanges(t)
		// Replace the two Comet identity writes with the reordered variants.
		filtered := changes[:0]
		for _, change := range changes {
			key, _ := decodeScValBase64(change.KeyXDR)
			if variant, _, ok := scVariant(key); ok && (variant == "AllTokenVec" || variant == "AllRecordData") && change.ContractID == f.cometID {
				continue
			}
			filtered = append(filtered, change)
		}
		filtered = append(filtered,
			stateChange(t, f.cometID, variantVal(t, "AllTokenVec"), vecVal(tokenVec...)),
			stateChange(t, f.cometID, variantVal(t, "AllRecordData"), cometRecordMapVal(t, recordEntries...)),
		)
		state, err := adapter.DecodeState(nil, filtered, 100)
		if err != nil {
			t.Fatalf("fold: %v", err)
		}
		return state
	}

	canonical := fold(t, []xdr.ScVal{blndAddr, usdcAddr},
		xdr.ScMapEntry{Key: blndAddr, Val: cometRecordVal(t, 80_000_000_000_000, 0)},
		xdr.ScMapEntry{Key: usdcAddr, Val: cometRecordVal(t, 4_000_000_000_000, 1)},
	)
	reordered := fold(t, []xdr.ScVal{usdcAddr, blndAddr},
		xdr.ScMapEntry{Key: usdcAddr, Val: cometRecordVal(t, 4_000_000_000_000, 1)},
		xdr.ScMapEntry{Key: blndAddr, Val: cometRecordVal(t, 80_000_000_000_000, 0)},
	)

	a, _ := json.Marshal(canonical)
	b, _ := json.Marshal(reordered)
	if !bytes.Equal(a, b) {
		t.Fatalf("token/record order changed economic output:\ncanonical=%s\nreordered=%s", a, b)
	}
}

func TestCometFold_TTLExpiryGoesAbsent(t *testing.T) {
	t.Parallel()

	f := newCometFixture(t)
	adapter := newTestAdapter(t)
	f.register(adapter)
	state := foldCometDeploy(t, adapter, f)

	expired := uint32(100) // LiveUntilLedgerSeq < folding ledger 101
	next, err := adapter.DecodeState(state, []bindings.ContractDataChange{
		stateChange(t, f.cometID, variantVal(t, "TotalShares"), i128Val(100_000_000_000), withLiveUntil(&expired)),
	}, 101)
	if err != nil {
		t.Fatalf("fold ttl ledger: %v", err)
	}
	position := backstopFor(t, next, f.userA, f.poolID)
	if position.LPTokenSupplyRaw != "" {
		t.Fatalf("expired TotalShares must go absent, got %q", position.LPTokenSupplyRaw)
	}
	// The surviving facets are untouched.
	if position.LPBLNDReserveRaw != "80000000000000" {
		t.Fatalf("record facet must survive, got %q", position.LPBLNDReserveRaw)
	}
}

func TestCometFold_ZeroSupplyPresentButUnvalued(t *testing.T) {
	t.Parallel()

	f := newCometFixture(t)
	// AllowUnknownV2 so the test pool's unset wasm resolves in Transform.
	adapter, err := New(Config{AllowUnknownV2: true})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	f.register(adapter)

	changes := f.deployChanges(t)
	filtered := changes[:0]
	for _, change := range changes {
		key, _ := decodeScValBase64(change.KeyXDR)
		if variant, _, ok := scVariant(key); ok && variant == "TotalShares" && change.ContractID == f.cometID {
			continue
		}
		filtered = append(filtered, change)
	}
	filtered = append(filtered, stateChange(t, f.cometID, variantVal(t, "TotalShares"), i128Val(0)))
	state, err := adapter.DecodeState(nil, filtered, 100)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}

	position := backstopFor(t, state, f.userA, f.poolID)
	if position.LPTokenSupplyRaw != "0" {
		t.Fatalf("a real stored zero supply stays \"0\", got %q", position.LPTokenSupplyRaw)
	}

	out, err := adapter.Transform(bindings.TransformInput{LedgerSeq: 100, CloseTime: time.Unix(1000, 0).UTC(), State: state})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	var row *bindings.Position
	for i := range out.Positions {
		if out.Positions[i].PositionType == contracts.PositionTypeBackstop && out.Positions[i].Address == f.userA {
			row = &out.Positions[i]
			break
		}
	}
	if row == nil {
		t.Fatal("expected a backstop position row")
	}
	if row.USDValue != "" {
		t.Fatalf("zero denominator must yield absent USD, got %q", row.USDValue)
	}
	if row.Metadata["lp_denominator_zero"] != "true" {
		t.Fatalf("expected lp_denominator_zero marker, metadata %+v", row.Metadata)
	}
	if _, ok := row.Metadata["blnd_component"]; ok {
		t.Fatalf("zero denominator must not fabricate components: %+v", row.Metadata)
	}
}

func TestCometFold_MissingLegsStayAbsent(t *testing.T) {
	t.Parallel()

	f := newCometFixture(t)

	t.Run("missing_record_leg", func(t *testing.T) {
		adapter := newTestAdapter(t)
		f.register(adapter)
		changes := f.deployChanges(t)
		// AllRecordData carries only the BLND record.
		blndAddr := contractAddressVal(t, 2)
		filtered := changes[:0]
		for _, change := range changes {
			key, _ := decodeScValBase64(change.KeyXDR)
			if variant, _, ok := scVariant(key); ok && variant == "AllRecordData" && change.ContractID == f.cometID {
				continue
			}
			filtered = append(filtered, change)
		}
		filtered = append(filtered, stateChange(t, f.cometID, variantVal(t, "AllRecordData"), cometRecordMapVal(t,
			xdr.ScMapEntry{Key: blndAddr, Val: cometRecordVal(t, 80_000_000_000_000, 0)},
		)))
		state, err := adapter.DecodeState(nil, filtered, 100)
		if err != nil {
			t.Fatalf("fold: %v", err)
		}
		position := backstopFor(t, state, f.userA, f.poolID)
		if position.LPBLNDReserveRaw != "80000000000000" {
			t.Fatalf("BLND leg must fold, got %q", position.LPBLNDReserveRaw)
		}
		if position.LPUSDCReserveRaw != "" {
			t.Fatalf("missing USDC record must stay absent, got %q", position.LPUSDCReserveRaw)
		}
	})

	t.Run("missing_price_leg", func(t *testing.T) {
		adapter := newTestAdapter(t)
		f.register(adapter)
		changes := f.deployChanges(t)
		// Drop the USDC price entry (index 1).
		filtered := changes[:0]
		for _, change := range changes {
			if change.ContractID == f.oracleID {
				key, _ := decodeScValBase64(change.KeyXDR)
				if key.Type == xdr.ScValTypeScvU128 {
					if parts, ok := key.GetU128(); ok && parts.Lo == 1 {
						continue
					}
				}
			}
			filtered = append(filtered, change)
		}
		state, err := adapter.DecodeState(nil, filtered, 100)
		if err != nil {
			t.Fatalf("fold: %v", err)
		}
		position := backstopFor(t, state, f.userA, f.poolID)
		if position.BLNDPriceUSD != "0.05" {
			t.Fatalf("BLND price must bind, got %q", position.BLNDPriceUSD)
		}
		if position.USDCPriceUSD != "" {
			t.Fatalf("missing USDC price must stay absent (never $1), got %q", position.USDCPriceUSD)
		}
	})

	t.Run("no_comet_registered", func(t *testing.T) {
		adapter := newTestAdapter(t)
		adapter.RegisterContracts(f.poolID, f.backstopID, f.oracleID)
		// The Comet contract is NOT registered: its writes never fold.
		state, err := adapter.DecodeState(nil, f.deployChanges(t), 100)
		if err != nil {
			t.Fatalf("fold: %v", err)
		}
		position := backstopFor(t, state, f.userA, f.poolID)
		if position.LPTokenSupplyRaw != "" || position.LPBLNDReserveRaw != "" || position.LPUSDCReserveRaw != "" {
			t.Fatalf("unregistered Comet must leave LP inputs absent: %+v", position)
		}
		if len(state.AMMPools) != 0 {
			t.Fatalf("unregistered Comet must not enter AMMPools: %+v", state.AMMPools)
		}
	})
}

func TestCometDirtyBackstops(t *testing.T) {
	t.Parallel()

	f := newCometFixture(t)

	newFoldedAdapter := func(t *testing.T) (*Adapter, *bindings.LedgerState) {
		t.Helper()
		adapter := newTestAdapter(t)
		f.register(adapter)
		state := foldCometDeploy(t, adapter, f)
		if dirty := adapter.LastDirtyBackstops(); len(dirty) != 2 {
			t.Fatalf("deploy ledger must dirty both backstop holders, got %+v", dirty)
		}
		return adapter, state
	}

	t.Run("holder_balance_marks_only_that_holder", func(t *testing.T) {
		adapter, state := newFoldedAdapter(t)
		_, err := adapter.DecodeState(state, []bindings.ContractDataChange{
			stateChange(t, f.backstopID, variantVal(t, "UserBalance", mapVal(t, map[string]xdr.ScVal{
				"pool": contractAddressVal(t, 1),
				"user": accountAddressVal(t, 5),
			})), mapVal(t, map[string]xdr.ScVal{"shares": i128Val(6_000_000_000)})),
		}, 101)
		if err != nil {
			t.Fatalf("fold: %v", err)
		}
		dirty := adapter.LastDirtyBackstops()
		if len(dirty) != 1 || dirty[0].Address != f.userA || dirty[0].PoolContractID != f.poolID || dirty[0].Kind != bindings.DirtyUpsert {
			t.Fatalf("expected only userA dirty, got %+v", dirty)
		}
		if lending := adapter.LastDirtyPositions(); len(lending) != 0 {
			t.Fatalf("a backstop write must not dirty lending positions, got %+v", lending)
		}
	})

	t.Run("pool_balance_marks_all_pool_holders", func(t *testing.T) {
		adapter, state := newFoldedAdapter(t)
		_, err := adapter.DecodeState(state, []bindings.ContractDataChange{
			stateChange(t, f.backstopID, variantVal(t, "PoolBalance", contractAddressVal(t, 1)), mapVal(t, map[string]xdr.ScVal{
				"shares": i128Val(10_000_000_000),
				"tokens": i128Val(41_000_000_000),
			})),
		}, 101)
		if err != nil {
			t.Fatalf("fold: %v", err)
		}
		if dirty := adapter.LastDirtyBackstops(); len(dirty) != 2 {
			t.Fatalf("PoolBalance must dirty both holders, got %+v", dirty)
		}
	})

	t.Run("comet_write_marks_linked_holders_only", func(t *testing.T) {
		adapter, state := newFoldedAdapter(t)
		_, err := adapter.DecodeState(state, []bindings.ContractDataChange{
			stateChange(t, f.cometID, variantVal(t, "TotalShares"), i128Val(101_000_000_000)),
		}, 101)
		if err != nil {
			t.Fatalf("fold: %v", err)
		}
		if dirty := adapter.LastDirtyBackstops(); len(dirty) != 2 {
			t.Fatalf("a linked Comet write must dirty both holders, got %+v", dirty)
		}
		if lending := adapter.LastDirtyPositions(); len(lending) != 0 {
			t.Fatalf("a Comet write must not dirty lending positions, got %+v", lending)
		}
	})

	t.Run("unrelated_comet_marks_nothing", func(t *testing.T) {
		adapter, state := newFoldedAdapter(t)
		other := validContractString(t, 42)
		adapter.RegisterCometContracts(other)
		_, err := adapter.DecodeState(state, []bindings.ContractDataChange{
			stateChange(t, other, variantVal(t, "TotalShares"), i128Val(1)),
		}, 101)
		if err != nil {
			t.Fatalf("fold: %v", err)
		}
		if dirty := adapter.LastDirtyBackstops(); len(dirty) != 0 {
			t.Fatalf("an unlinked Comet write must dirty nothing, got %+v", dirty)
		}
	})

	t.Run("blnd_price_tick_marks_linked_holders", func(t *testing.T) {
		adapter, state := newFoldedAdapter(t)
		_, err := adapter.DecodeState(state, []bindings.ContractDataChange{
			stateChange(t, f.oracleID, u128Val(0), u128Val(600_000_000_000)),
		}, 101)
		if err != nil {
			t.Fatalf("fold: %v", err)
		}
		if dirty := adapter.LastDirtyBackstops(); len(dirty) != 2 {
			t.Fatalf("a BLND price tick must dirty both holders, got %+v", dirty)
		}
		if lending := adapter.LastDirtyPositions(); len(lending) != 0 {
			t.Fatalf("a price tick must not dirty lending positions, got %+v", lending)
		}
	})

	t.Run("lending_write_marks_no_backstop", func(t *testing.T) {
		adapter, state := newFoldedAdapter(t)
		_, err := adapter.DecodeState(state, []bindings.ContractDataChange{
			stateChange(t, f.poolID, variantVal(t, "Positions", accountAddressVal(t, 5)), mapVal(t, map[string]xdr.ScVal{
				"supply": intMapVal(t, map[uint32]xdr.ScVal{0: i128Val(700)}),
			})),
		}, 101)
		if err != nil {
			t.Fatalf("fold: %v", err)
		}
		if dirty := adapter.LastDirtyBackstops(); len(dirty) != 0 {
			t.Fatalf("a lending Positions write must not dirty backstops, got %+v", dirty)
		}
		if lending := adapter.LastDirtyPositions(); len(lending) != 1 {
			t.Fatalf("expected the lending pair dirty, got %+v", lending)
		}
	})

	t.Run("user_balance_removal_reports_removal", func(t *testing.T) {
		adapter, state := newFoldedAdapter(t)
		_, err := adapter.DecodeState(state, []bindings.ContractDataChange{
			stateChange(t, f.backstopID, variantVal(t, "UserBalance", mapVal(t, map[string]xdr.ScVal{
				"pool": contractAddressVal(t, 1),
				"user": accountAddressVal(t, 5),
			})), mapVal(t, map[string]xdr.ScVal{}), withNoValue(), withChangeType("LedgerEntryRemoved")),
		}, 101)
		if err != nil {
			t.Fatalf("fold: %v", err)
		}
		dirty := adapter.LastDirtyBackstops()
		if len(dirty) != 1 || dirty[0].Kind != bindings.DirtyRemoval {
			t.Fatalf("expected one Removal, got %+v", dirty)
		}
	})
}

// TestCometCheckpoint_RestartMatchesReplay is the restart gate: restore the
// checkpoint JSON at ledger N, apply a wallet-only write at N+1 with no Comet
// write, and match uninterrupted replay byte for byte — AMMPools, Backstops,
// and the wallet's valuation.
func TestCometCheckpoint_RestartMatchesReplay(t *testing.T) {
	t.Parallel()

	f := newCometFixture(t)
	walletOnly := func(t *testing.T) []bindings.ContractDataChange {
		t.Helper()
		return []bindings.ContractDataChange{
			stateChange(t, f.poolID, variantVal(t, "Positions", accountAddressVal(t, 5)), mapVal(t, map[string]xdr.ScVal{
				"supply": intMapVal(t, map[uint32]xdr.ScVal{0: i128Val(700)}),
			})),
		}
	}

	// Uninterrupted: deploy at 100, wallet-only at 101.
	uninterrupted := newTestAdapter(t)
	f.register(uninterrupted)
	deployed := foldCometDeploy(t, uninterrupted, f)
	finalReplay, err := uninterrupted.DecodeState(deployed, walletOnly(t), 101)
	if err != nil {
		t.Fatalf("replay fold 101: %v", err)
	}

	// Restart: checkpoint JSON at 100 restored into a fresh adapter, same 101.
	raw, err := json.Marshal(deployed)
	if err != nil {
		t.Fatalf("marshal checkpoint: %v", err)
	}
	var restored bindings.LedgerState
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatalf("unmarshal checkpoint: %v", err)
	}
	restarted := newTestAdapter(t)
	f.register(restarted)
	finalRestart, err := restarted.DecodeState(&restored, walletOnly(t), 101)
	if err != nil {
		t.Fatalf("restart fold 101: %v", err)
	}

	a, _ := json.Marshal(finalReplay)
	b, _ := json.Marshal(finalRestart)
	if !bytes.Equal(a, b) {
		t.Fatalf("restart diverged from replay:\nreplay=%s\nrestart=%s", a, b)
	}

	// The known wallet's valuation matches across the restart boundary too.
	position := backstopFor(t, finalRestart, f.userA, f.poolID)
	if position.LPTokenSupplyRaw != "100000000000" || position.BLNDPriceUSD != "0.05" {
		t.Fatalf("restart lost Comet valuation inputs: %+v", position)
	}
}

// TestCometCheckpoint_PreCometVintageLoads is the upgrade gate: a checkpoint
// serialized BEFORE the Comet fold existed (no AMMPools, backstop LP fields and
// price bindings all "") must restore into a Comet-registered adapter, and the
// first ledger carrying the Comet writes must land the full valuation — byte
// for byte what an uninterrupted post-upgrade fold produces. The vintage is
// simulated by folding the same deploy without Comet registration and blanking
// the two price-binding fields; the pre-Comet reducer's output differs from
// that only in those fields (it never populated them).
func TestCometCheckpoint_PreCometVintageLoads(t *testing.T) {
	t.Parallel()

	f := newCometFixture(t)
	blndAddr := contractAddressVal(t, 2)
	usdcAddr := contractAddressVal(t, 7)
	cometWrites := func(t *testing.T) []bindings.ContractDataChange {
		t.Helper()
		return []bindings.ContractDataChange{
			stateChange(t, f.cometID, variantVal(t, "AllTokenVec"), vecVal(blndAddr, usdcAddr)),
			stateChange(t, f.cometID, variantVal(t, "AllRecordData"), cometRecordMapVal(t,
				xdr.ScMapEntry{Key: blndAddr, Val: cometRecordVal(t, 80_000_000_000_000, 0)},
				xdr.ScMapEntry{Key: usdcAddr, Val: cometRecordVal(t, 4_000_000_000_000, 1)},
			)),
			stateChange(t, f.cometID, variantVal(t, "TotalShares"), i128Val(100_000_000_000)),
		}
	}
	// The deploy ledger minus the Comet writes: what a pre-Comet relay folded.
	preCometChanges := func(t *testing.T) []bindings.ContractDataChange {
		t.Helper()
		changes := f.deployChanges(t)
		filtered := changes[:0]
		for _, change := range changes {
			if change.ContractID == f.cometID {
				continue
			}
			filtered = append(filtered, change)
		}
		return filtered
	}

	// Vintage checkpoint: pre-Comet registration, then blank the price bindings
	// the old reducer never populated.
	vintage := newTestAdapter(t)
	vintage.RegisterContracts(f.poolID, f.backstopID, f.oracleID)
	vintageState, err := vintage.DecodeState(nil, preCometChanges(t), 100)
	if err != nil {
		t.Fatalf("vintage fold: %v", err)
	}
	for i := range vintageState.Backstops {
		vintageState.Backstops[i].BLNDPriceUSD = ""
		vintageState.Backstops[i].USDCPriceUSD = ""
	}
	raw, err := json.Marshal(vintageState)
	if err != nil {
		t.Fatalf("marshal vintage checkpoint: %v", err)
	}

	// Restore into the upgraded, Comet-registered adapter; fold the Comet writes.
	var restored bindings.LedgerState
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatalf("unmarshal vintage checkpoint: %v", err)
	}
	upgraded := newTestAdapter(t)
	f.register(upgraded)
	fromVintage, err := upgraded.DecodeState(&restored, cometWrites(t), 101)
	if err != nil {
		t.Fatalf("fold on vintage checkpoint: %v", err)
	}

	// Uninterrupted post-upgrade run over the same ledgers.
	fresh := newTestAdapter(t)
	f.register(fresh)
	freshDeployed, err := fresh.DecodeState(nil, preCometChanges(t), 100)
	if err != nil {
		t.Fatalf("fresh fold 100: %v", err)
	}
	fromFresh, err := fresh.DecodeState(freshDeployed, cometWrites(t), 101)
	if err != nil {
		t.Fatalf("fresh fold 101: %v", err)
	}

	a, _ := json.Marshal(fromVintage)
	b, _ := json.Marshal(fromFresh)
	if !bytes.Equal(a, b) {
		t.Fatalf("vintage checkpoint diverged from uninterrupted fold:\nvintage=%s\nfresh=%s", a, b)
	}
	position := backstopFor(t, fromVintage, f.userA, f.poolID)
	if position.LPTokenSupplyRaw != "100000000000" || position.LPBLNDReserveRaw != "80000000000000" ||
		position.BLNDPriceUSD != "0.05" || position.USDCPriceUSD != "0.9999999" {
		t.Fatalf("vintage checkpoint did not fold to full valuation: %+v", position)
	}
}

// TestCometFold_PoolBackstopValued covers the pool-level aggregate row
// (bindings.Backstop): component amounts and USD from the folded Comet state.
func TestCometFold_PoolBackstopValued(t *testing.T) {
	t.Parallel()

	f := newCometFixture(t)
	adapter, err := New(Config{AllowUnknownV2: true})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	f.register(adapter)
	state := foldCometDeploy(t, adapter, f)

	out, err := adapter.Transform(bindings.TransformInput{LedgerSeq: 100, CloseTime: time.Unix(1000, 0).UTC(), State: state})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if len(out.Backstops) != 1 {
		t.Fatalf("expected one pool backstop row, got %+v", out.Backstops)
	}
	row := out.Backstops[0]
	// poolTokens 40000000000 over supply 100000000000: 0.4 of each reserve.
	if row.BLNDAmountRaw != "32000000000000" {
		t.Fatalf("expected BLND component 32000000000000, got %q", row.BLNDAmountRaw)
	}
	if row.USDCAmountRaw != "1600000000000" {
		t.Fatalf("expected USDC component 1600000000000, got %q", row.USDCAmountRaw)
	}
	// 3.2e6 * 0.05 + 160000 * 0.9999999 = 160000 + 159999.984 = 319999.984
	if row.USDValue != "319999.984" {
		t.Fatalf("expected USD 319999.984, got %q", row.USDValue)
	}
}

// TestIncrementalParity_CometBackstop extends the two-strategy byte-parity
// gate to the Comet fold: full Comet state, a token reorder, a partial
// same-ledger update, a TTL expiry, and a missing record leg must all produce
// byte-identical state and identical dirty sets across paranoid and
// incremental.
func TestIncrementalParity_CometBackstop(t *testing.T) {
	t.Parallel()

	f := newCometFixture(t)
	run := newParityRun(t, func(a *Adapter) { f.register(a) }, nil)

	expired := uint32(100)
	blndAddr := contractAddressVal(t, 2)
	usdcAddr := contractAddressVal(t, 7)
	run.fold(
		parityLedger{seq: 100, close: time.Unix(1000, 0).UTC(), changes: f.deployChanges(t)},
		// Token reorder: identity unchanged, economic output byte-identical.
		parityLedger{seq: 101, close: time.Unix(1005, 0).UTC(), changes: []bindings.ContractDataChange{
			stateChange(t, f.cometID, variantVal(t, "AllTokenVec"), vecVal(usdcAddr, blndAddr)),
		}},
		// Partial same-facet update: one record leg moves.
		parityLedger{seq: 102, close: time.Unix(1010, 0).UTC(), changes: []bindings.ContractDataChange{
			stateChange(t, f.cometID, variantVal(t, "AllRecordData"), cometRecordMapVal(t,
				xdr.ScMapEntry{Key: usdcAddr, Val: cometRecordVal(t, 4_100_000_000_000, 1)},
				xdr.ScMapEntry{Key: blndAddr, Val: cometRecordVal(t, 80_000_000_000_000, 0)},
			)),
		}},
		// TTL expiry of TotalShares: the facet goes absent on both strategies.
		parityLedger{seq: 103, close: time.Unix(1015, 0).UTC(), changes: []bindings.ContractDataChange{
			stateChange(t, f.cometID, variantVal(t, "TotalShares"), i128Val(100_000_000_000), withLiveUntil(&expired)),
		}},
		// Restore the supply; a wallet-only write touches nothing Comet.
		parityLedger{seq: 104, close: time.Unix(1020, 0).UTC(), changes: []bindings.ContractDataChange{
			stateChange(t, f.cometID, variantVal(t, "TotalShares"), i128Val(100_500_000_000)),
			stateChange(t, f.backstopID, variantVal(t, "UserBalance", mapVal(t, map[string]xdr.ScVal{
				"pool": contractAddressVal(t, 1),
				"user": accountAddressVal(t, 5),
			})), mapVal(t, map[string]xdr.ScVal{"shares": i128Val(5_500_000_000)})),
		}},
	)
}

// TestProjectBackstopPositions covers the affected-holder projector: only the
// dirty pairs' backstop rows come back, summaries are never emitted from a
// backstop-only state, and an unknown pair projects nothing.
func TestProjectBackstopPositions(t *testing.T) {
	t.Parallel()

	f := newCometFixture(t)
	adapter, err := New(Config{AllowUnknownV2: true})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	f.register(adapter)
	state := foldCometDeploy(t, adapter, f)

	dirty := []bindings.DirtyBackstop{
		{Address: f.userA, PoolContractID: f.poolID, Kind: bindings.DirtyUpsert},
	}
	out, err := adapter.ProjectBackstopPositions(state, dirty, 100, time.Unix(1000, 0).UTC())
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if len(out.Positions) != 1 {
		t.Fatalf("expected exactly one backstop row, got %+v", out.Positions)
	}
	row := out.Positions[0]
	if row.Address != f.userA || row.PositionType != contracts.PositionTypeBackstop {
		t.Fatalf("unexpected row: %+v", row)
	}
	// 5000000000/10000000000 shares -> 20000000000 LP -> components 0.2 of each
	// reserve; USD = 3.2e6*0.05*... see the neutral vectors.
	if row.USDValue == "" {
		t.Fatalf("expected valued USD for the folded wallet, metadata %+v", row.Metadata)
	}
	if out.Summaries != nil {
		t.Fatalf("a backstop-only projection must never emit summaries, got %+v", out.Summaries)
	}

	out, err = adapter.ProjectBackstopPositions(state, []bindings.DirtyBackstop{
		{Address: f.userA, PoolContractID: f.poolID, Kind: bindings.DirtyUpsert},
		{Address: f.userB, PoolContractID: f.poolID, Kind: bindings.DirtyUpsert},
	}, 100, time.Unix(1000, 0).UTC())
	if err != nil {
		t.Fatalf("project two: %v", err)
	}
	if len(out.Positions) != 2 {
		t.Fatalf("expected both holders, got %+v", out.Positions)
	}

	// A removal pair (no row in state) projects nothing — the caller builds its
	// tombstone from the DirtyBackstop identity.
	out, err = adapter.ProjectBackstopPositions(state, []bindings.DirtyBackstop{
		{Address: "GNOSUCH", PoolContractID: f.poolID, Kind: bindings.DirtyRemoval},
	}, 100, time.Unix(1000, 0).UTC())
	if err != nil {
		t.Fatalf("project removal: %v", err)
	}
	if len(out.Positions) != 0 {
		t.Fatalf("expected no rows for an absent pair, got %+v", out.Positions)
	}
}
