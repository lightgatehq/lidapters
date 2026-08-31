package blend

// State-laden fold benchmarks — the defect this seam exists for, in miniature.
// A fold's cost in paranoid mode is O(total accumulated state) per ledger (the
// prior mirror is rebuilt and the whole typed state re-sorted every ledger),
// so per-ledger throughput decays as users accumulate. Incremental mode folds
// the same ledgers at O(changes), plus the output materialization.
//
//	go test ./blend/ -run '^$' -bench StateLadenFold -benchtime 2s
//
// The REAL measurement of record is the EC2 harness run against a production
// checkpoint (600k ledgers of accumulated users); this benchmark just keeps
// the shape of the problem visible in-repo.

import (
	"fmt"
	"testing"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/lightgatehq/lidapters/blend/contracts"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

func benchAddress(b *testing.B, i int) string {
	b.Helper()
	var raw [32]byte
	raw[0] = byte(i >> 16)
	raw[1] = byte(i >> 8)
	raw[2] = byte(i)
	addr, err := strkey.Encode(strkey.VersionByteAccountID, raw[:])
	if err != nil {
		b.Fatalf("encode address: %v", err)
	}
	return addr
}

func benchContract(b *testing.B, seed byte) string {
	b.Helper()
	var raw [32]byte
	raw[0] = seed
	addr, err := strkey.Encode(strkey.VersionByteContract, raw[:])
	if err != nil {
		b.Fatalf("encode contract: %v", err)
	}
	return addr
}

func benchPositionsChange(b *testing.B, poolID, user string, supply int64) bindings.ContractDataChange {
	b.Helper()
	accountID := xdr.MustAddress(user)
	address := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &accountID}
	userVal := xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &address}
	sym := xdr.ScSymbol("Positions")
	keyVec := xdr.ScVec{{Type: xdr.ScValTypeScvSymbol, Sym: &sym}, userVal}
	keyVecPtr := &keyVec
	key := xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &keyVecPtr}

	index := xdr.Uint32(1)
	amount := xdr.Int128Parts{Hi: 0, Lo: xdr.Uint64(supply)}
	inner := xdr.ScMap{{
		Key: xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &index},
		Val: xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &amount},
	}}
	innerPtr := &inner
	supplySym := xdr.ScSymbol("supply")
	outer := xdr.ScMap{{
		Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &supplySym},
		Val: xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &innerPtr},
	}}
	outerPtr := &outer
	value := xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &outerPtr}

	keyXDR, err := xdr.MarshalBase64(key)
	if err != nil {
		b.Fatalf("marshal key: %v", err)
	}
	valueXDR, err := xdr.MarshalBase64(value)
	if err != nil {
		b.Fatalf("marshal value: %v", err)
	}
	return bindings.ContractDataChange{
		ContractID: poolID,
		KeyXDR:     keyXDR,
		ValueXDR:   &valueXDR,
		Durability: "persistent",
		Live:       true,
	}
}

// benchPrior builds a prior LedgerState carrying nUsers accumulated user
// positions against one pool — the "600k ledgers into the sprint" shape.
func benchPrior(b *testing.B, poolID string, nUsers int) *bindings.LedgerState {
	b.Helper()
	assetID := benchContract(b, 2)
	prior := &bindings.LedgerState{
		Pools: []contracts.PoolState{{
			ContractID: poolID,
			PoolStatus: "active",
			Reserves:   []contracts.ReserveState{{ReserveIndex: 1, ReserveIndexKnown: true, AssetID: assetID, AssetDecimals: 7}},
		}},
	}
	blob := benchPositionsChange(b, poolID, benchAddress(b, 0), 1000)
	for i := 0; i < nUsers; i++ {
		prior.PendingUserPositions = append(prior.PendingUserPositions, contracts.PendingUserPosition{
			Address:        benchAddress(b, i),
			PoolContractID: poolID,
			PositionsXDR:   *blob.ValueXDR,
		})
	}
	return prior
}

func benchmarkStateLadenFold(b *testing.B, mode StateMode, nUsers int) {
	poolID := benchContract(b, 1)
	adapter, err := New(Config{StateMode: mode})
	if err != nil {
		b.Fatalf("new adapter: %v", err)
	}
	state := benchPrior(b, poolID, nUsers)
	// One touched user per ledger — a quiet mainnet ledger's shape.
	change := benchPositionsChange(b, poolID, benchAddress(b, 42), 2000)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		next, err := adapter.DecodeState(state, []bindings.ContractDataChange{change}, int64(1000+i))
		if err != nil {
			b.Fatalf("decode: %v", err)
		}
		state = next
	}
}

func BenchmarkStateLadenFold(b *testing.B) {
	for _, mode := range []StateMode{StateModeParanoid, StateModeIncremental} {
		for _, nUsers := range []int{1_000, 10_000, 50_000} {
			b.Run(fmt.Sprintf("%s/users_%d", mode, nUsers), func(b *testing.B) {
				benchmarkStateLadenFold(b, mode, nUsers)
			})
		}
	}
}
