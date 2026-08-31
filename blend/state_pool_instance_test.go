package blend

import (
	"testing"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/lightgatehq/lidapters/blend/contracts"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// poolInstanceVal builds a pool contract-instance ScVal whose storage map holds
// the "Config" (PoolConfig) and "Backstop" entries, matching how a Blend pool
// stores them on-chain: inside the instance's storage, NOT as top-level
// contract_data entries.
func poolInstanceVal(t *testing.T, oracleID, backstopID string) xdr.ScVal {
	t.Helper()
	var wasm xdr.Hash
	wasm[31] = 7
	storage := xdr.ScMap{
		{Key: symbolVal(t, "Config"), Val: mapVal(t, map[string]xdr.ScVal{
			"oracle":     addressVal(t, oracleID),
			"bstop_rate": u32Val(1_000_000),
			"status":     u32Val(1),
		})},
		{Key: symbolVal(t, "Backstop"), Val: addressVal(t, backstopID)},
	}
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

// TestDecodeState_PoolConfigFromInstanceStorage is the null-HF regression: a
// Blend pool keeps its PoolConfig (oracle, bstop_rate, status) and backstop
// address inside its contract-instance storage map, not as top-level "Config"/
// "Backstop" contract_data entries. Before the fix the fold read only the wasm
// hash off the instance and dropped the storage map, so OracleContract stayed
// empty, resolveOraclePrices found no oracle, and every reserve's USD value and
// health factor surfaced null. This folds the instance and asserts the oracle
// link, backstop, and status land.
func TestDecodeState_PoolConfigFromInstanceStorage(t *testing.T) {
	t.Parallel()

	adapter, err := New(Config{AllowUnknownV2: true})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	poolID := validContractString(t, 1)
	oracleID := validContractString(t, 3)
	backstopID := validContractString(t, 4)

	instanceKey := xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance}
	state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		stateChange(t, poolID, instanceKey, poolInstanceVal(t, oracleID, backstopID)),
	}, 100)
	if err != nil {
		t.Fatalf("decode state: %v", err)
	}

	var pool *contracts.PoolState
	for i := range state.Pools {
		if state.Pools[i].ContractID == poolID {
			pool = &state.Pools[i]
		}
	}
	if pool == nil {
		t.Fatalf("pool %s not folded", poolID)
	}
	if pool.OracleContract != oracleID {
		t.Fatalf("OracleContract from instance storage: got %q want %q", pool.OracleContract, oracleID)
	}
	if pool.BackstopContract != backstopID {
		t.Fatalf("BackstopContract from instance storage: got %q want %q", pool.BackstopContract, backstopID)
	}
	if pool.PoolStatus != "active" {
		t.Fatalf("PoolStatus from instance storage: got %q want %q", pool.PoolStatus, "active")
	}
}
