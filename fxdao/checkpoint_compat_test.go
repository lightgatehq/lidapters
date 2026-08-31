package fxdao

// Checkpoint JSON compatibility for the LedgerState vault extension. The
// testdata checkpoint was serialized by the PRE-extension bindings.LedgerState
// (before the Vaults/VaultsInfo slices existed — see testdata/REGISTRY.md for
// the exact provenance), so these tests prove the extension is additive: old
// checkpoints decode unchanged, and the new slices round-trip beside the
// Blend and AMM families without disturbing them.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/lightgatehq/lidapters/bindings"
)

func TestPreExtensionCheckpointRoundTrips(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "ledgerstate-checkpoint-pre-vault-extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s bindings.LedgerState
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("pre-extension checkpoint no longer decodes: %v", err)
	}
	// The pre-change families arrive intact...
	if len(s.Pools) != 1 || s.Pools[0].Name != "Fixed" {
		t.Fatalf("blend pools %#v", s.Pools)
	}
	if len(s.Users) != 1 || len(s.AMMPools) != 1 || len(s.AMMPositions) != 1 || len(s.AMMAssets) != 1 {
		t.Fatalf("pre-change slices %#v", s)
	}
	if s.AMMPools[0].TotalSharesRaw != "300" || !s.AMMPositions[0].HadShares {
		t.Fatalf("AMM payload %#v %#v", s.AMMPools, s.AMMPositions)
	}
	// ...and the new families read as absent, never fabricated.
	if len(s.Vaults) != 0 || len(s.VaultsInfo) != 0 {
		t.Fatalf("vault slices fabricated from old checkpoint %#v %#v", s.Vaults, s.VaultsInfo)
	}
	// Full round-trip through the extended struct is lossless.
	re, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var s2 bindings.LedgerState
	if err := json.Unmarshal(re, &s2); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s, s2) {
		t.Fatal("re-marshaled checkpoint diverged")
	}
}

func TestExtendedCheckpointRoundTripsWithVaults(t *testing.T) {
	s := bindings.LedgerState{
		AMMPools: []bindings.AMMPoolState{{Protocol: "soroswap", ContractID: "pair", PoolType: "constant_product"}},
		Vaults: []bindings.VaultState{{
			Protocol: "fxdao", ContractID: vaultsContract, Account: gdvz,
			Denomination: "USD", IndexRaw: "19182692307",
			CollateralRaw: "24937500000", DebtRaw: "1300000000",
			HadVault: true, Restored: true,
		}},
		VaultsInfo: []bindings.VaultsInfoState{{
			Protocol: "fxdao", ContractID: vaultsContract, Denomination: "USD",
			TotalVaultsRaw: "5", TotalDebtRaw: "239040000000", TotalColRaw: "2704671870000",
			MinColRateRaw: "11000000", MinDebtCreationRaw: "1000000000", OpeningColRateRaw: "11500000",
		}},
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var s2 bindings.LedgerState
	if err := json.Unmarshal(b, &s2); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s, s2) {
		t.Fatal("vault checkpoint diverged across round-trip")
	}
}
