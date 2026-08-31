package blend

// Demonstrates Change 2's cost claim: Adapter.ProjectPositions on a single
// dirty (address, pool) pair costs O(dirty users), not O(all users) — the
// defect this closes is a per-ledger emission consumer running the full
// Transform (O(all users), ~11.5k on mainnet) on every event ledger. Compare:
//
//	go test ./blend/ -run '^$' -bench 'ProjectPositionsSingleUser|TransformAllUsers' -benchtime 2s
//
// ProjectPositionsSingleUser's time should stay flat as totalUsers grows (the
// incremental strategy's O(1) userPositions lookup, state_incremental.go);
// TransformAllUsers' should grow with it — the two curves are the point.

import (
	"fmt"
	"testing"
	"time"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// benchAssetAddressVal builds the same contract ScVal shape contractAddressVal
// (adapter_behavior_test.go) does, for a *testing.B — seed byte 0 in the
// high-order position, matching benchContract's encoding so the ScVal and the
// string form (used as the pool/asset ContractID) name the same contract.
func benchAssetAddressVal(b *testing.B, seed byte) xdr.ScVal {
	b.Helper()
	var hash xdr.Hash
	hash[0] = seed
	contractID := xdr.ContractId(hash)
	address, err := xdr.NewScAddress(xdr.ScAddressTypeScAddressTypeContract, contractID)
	if err != nil {
		b.Fatalf("contract address: %v", err)
	}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &address}
}

func benchSymbolVal(b *testing.B, raw string) xdr.ScVal {
	b.Helper()
	sym := xdr.ScSymbol(raw)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
}

func benchU32Val(value uint32) xdr.ScVal {
	raw := xdr.Uint32(value)
	return xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &raw}
}

func benchI128Val(value int64) xdr.ScVal {
	raw := xdr.Int128Parts{Hi: 0, Lo: xdr.Uint64(value)}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &raw}
}

func benchVecVal(items ...xdr.ScVal) xdr.ScVal {
	vec := xdr.ScVec(items)
	ptr := &vec
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &ptr}
}

func benchVariantVal(b *testing.B, name string, args ...xdr.ScVal) xdr.ScVal {
	b.Helper()
	return benchVecVal(append([]xdr.ScVal{benchSymbolVal(b, name)}, args...)...)
}

func benchMapVal(b *testing.B, fields map[string]xdr.ScVal) xdr.ScVal {
	b.Helper()
	entries := make(xdr.ScMap, 0, len(fields))
	for name, val := range fields {
		sym := xdr.ScSymbol(name)
		entries = append(entries, xdr.ScMapEntry{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}, Val: val})
	}
	ptr := &entries
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &ptr}
}

func benchStateChange(b *testing.B, contractID string, key, value xdr.ScVal) bindings.ContractDataChange {
	b.Helper()
	keyXDR, err := xdr.MarshalBase64(key)
	if err != nil {
		b.Fatalf("marshal key: %v", err)
	}
	valueXDR, err := xdr.MarshalBase64(value)
	if err != nil {
		b.Fatalf("marshal value: %v", err)
	}
	return bindings.ContractDataChange{
		ContractID: contractID,
		KeyXDR:     keyXDR,
		ValueXDR:   &valueXDR,
		Durability: "persistent",
		Live:       true,
	}
}

func benchResListChange(b *testing.B, poolID string, assetSeed byte) bindings.ContractDataChange {
	b.Helper()
	return benchStateChange(b, poolID, benchSymbolVal(b, "ResList"), benchVecVal(benchAssetAddressVal(b, assetSeed)))
}

func benchResConfigChange(b *testing.B, poolID string, assetSeed byte) bindings.ContractDataChange {
	b.Helper()
	key := benchVariantVal(b, "ResConfig", benchAssetAddressVal(b, assetSeed))
	value := benchMapVal(b, map[string]xdr.ScVal{
		"index":      benchU32Val(1),
		"decimals":   benchU32Val(7),
		"c_factor":   benchU32Val(9_000_000),
		"l_factor":   benchU32Val(9_000_000),
		"reactivity": benchU32Val(20_000),
	})
	return benchStateChange(b, poolID, key, value)
}

func benchResDataChange(b *testing.B, poolID string, assetSeed byte) bindings.ContractDataChange {
	b.Helper()
	key := benchVariantVal(b, "ResData", benchAssetAddressVal(b, assetSeed))
	value := benchMapVal(b, map[string]xdr.ScVal{
		"d_rate":   benchI128Val(10_000_000),
		"b_rate":   benchI128Val(10_000_000),
		"b_supply": benchI128Val(1_000_000_000),
		"d_supply": benchI128Val(500_000_000),
	})
	return benchStateChange(b, poolID, key, value)
}

// benchFoldedState folds a one-reserve pool plus totalUsers Positions writes
// through a real DecodeState pass (in the given mode), so an incremental
// strategy's position cache (state_incremental.go's s.index) is genuinely
// populated — the only way ProjectPositions' O(1)-per-pair lookup path is
// exercised. The one-time fold cost is O(totalUsers) but happens once, before
// the timed loop; it is not what either benchmark below measures.
func benchFoldedState(b *testing.B, mode StateMode, poolID string, assetSeed byte, totalUsers int) (*Adapter, *bindings.LedgerState) {
	b.Helper()
	adapter, err := New(Config{AllowUnknownV2: true, StateMode: mode})
	if err != nil {
		b.Fatalf("new adapter: %v", err)
	}
	changes := make([]bindings.ContractDataChange, 0, totalUsers+3)
	changes = append(changes,
		benchResListChange(b, poolID, assetSeed),
		benchResConfigChange(b, poolID, assetSeed),
		benchResDataChange(b, poolID, assetSeed),
	)
	for i := 0; i < totalUsers; i++ {
		changes = append(changes, benchPositionsChange(b, poolID, benchAddress(b, i), 1_000_000))
	}
	state, err := adapter.DecodeState(nil, changes, 1000)
	if err != nil {
		b.Fatalf("seed fold: %v", err)
	}
	return adapter, state
}

func BenchmarkProjectPositionsSingleUser(b *testing.B) {
	poolID := benchContract(b, 60)
	for _, totalUsers := range []int{1_000, 10_000, 50_000} {
		b.Run(fmt.Sprintf("users_%d", totalUsers), func(b *testing.B) {
			adapter, state := benchFoldedState(b, StateModeIncremental, poolID, 61, totalUsers)
			dirty := []bindings.DirtyPosition{{
				Address:        benchAddress(b, totalUsers/2),
				PoolContractID: poolID,
				Kind:           bindings.DirtyUpsert,
			}}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out, err := adapter.ProjectPositions(state, dirty, int64(1000+i), time.Time{})
				if err != nil {
					b.Fatalf("project: %v", err)
				}
				if len(out.Positions) == 0 {
					b.Fatalf("expected at least one projected position")
				}
			}
		})
	}
}

func BenchmarkTransformAllUsers(b *testing.B) {
	poolID := benchContract(b, 62)
	for _, totalUsers := range []int{1_000, 10_000, 50_000} {
		b.Run(fmt.Sprintf("users_%d", totalUsers), func(b *testing.B) {
			adapter, state := benchFoldedState(b, StateModeParanoid, poolID, 63, totalUsers)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out, err := adapter.Transform(bindings.TransformInput{LedgerSeq: int64(1000 + i), State: state})
				if err != nil {
					b.Fatalf("transform: %v", err)
				}
				if len(out.Positions) == 0 {
					b.Fatalf("expected projected positions")
				}
			}
		})
	}
}
