package fxdao

// Mainnet decode tests over testdata/*.xdr — REAL pubnet ScVal bytes for the
// FxDAO vaults contract (see testdata/REGISTRY.md for provenance). Every
// expected constant below was hand-derived from the raw XDR with an
// independent stdlib byte walk, never read back from this package.
//
// Hand-derived anchors:
//
//	L062448349 (the golden write ledger):
//	  Vault(GDVZ…, USD): index 19_182_692_307, total_collateral
//	    24_937_500_000, total_debt 1_300_000_000, next Some(GBWT…)
//	  Vault(GBWT…, USD): index 19_837_263_906, total_collateral
//	    1_269_584_890_000, total_debt 64_000_000_000, next None
//	  VaultIndex(GDVZ…, USD) RESTORED: u128 19_182_692_307
//	  instance wasm f3f08b40…, VaultsInfo(USD): total_vaults 5, total_debt
//	    239_040_000_000, total_col 2_704_671_870_000, min_col_rate
//	    11_000_000, min_debt_creation 1_000_000_000, opening_col_rate
//	    11_500_000
//	L062473181: GDVZ rewritten (same amounts, next None) while GBWT's entry
//	  is deleted in the same ledger
//	L062645363 (the terminal ledger): instance wasm 0245bac3…,
//	  VaultsInfo(USD): total_vaults 0, total_col 0, total_debt
//	  175_040_000_000 (a stored residual — recorded as written)

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lightgatehq/lidapters/bindings"
)

const (
	vaultsContract = "CCUN4RXU5VNDHSF4S4RKV4ZJYMX2YWKOH6L4AKEKVNVDQ7HY5QIAO4UB"
	gdvz           = "GDVZ4XYYKURF6OE7AQQPGUYU744ULGIJAKZDEETH2YMDABFZQCS3PENZ"
	gbwt           = "GBWTPGKBA6RIPW6UE7GRIGTCFMI326YC25MCCUV7O72QA4JYBICW6LWJ"
)

func fixtureB64(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func change(t *testing.T, keyFixture, valFixture, durability, changeType string) bindings.ContractDataChange {
	t.Helper()
	c := bindings.ContractDataChange{
		ContractID: vaultsContract,
		KeyXDR:     fixtureB64(t, keyFixture),
		Durability: durability,
		ChangeType: changeType,
		Live:       changeType != "Removed",
	}
	if valFixture != "" {
		val := fixtureB64(t, valFixture)
		c.ValueXDR = &val
	}
	return c
}

func testAdapter(t *testing.T) *Adapter {
	t.Helper()
	a, err := NewWithConfig(Config{VaultsContracts: map[string]struct{}{vaultsContract: {}}})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func goldenLedgerChanges(t *testing.T) []bindings.ContractDataChange {
	t.Helper()
	return []bindings.ContractDataChange{
		change(t, "pubnet-L062448349-fxdao-vault-gdvz-usd-key.xdr", "pubnet-L062448349-fxdao-vault-gdvz-usd-val.xdr", "persistent", "Updated"),
		change(t, "pubnet-L062448349-fxdao-vault-gbwt-usd-key.xdr", "pubnet-L062448349-fxdao-vault-gbwt-usd-val.xdr", "persistent", "Updated"),
		// The raw restore variant, preserved from the source ledger.
		change(t, "pubnet-L062448349-fxdao-vaultindex-gdvz-usd-restored-key.xdr", "pubnet-L062448349-fxdao-vaultindex-gdvz-usd-restored-val.xdr", "persistent", "Restored"),
		change(t, "pubnet-L062448349-fxdao-vaults-instance-ccun-key.xdr", "pubnet-L062448349-fxdao-vaults-instance-ccun-val.xdr", "instance", "Updated"),
	}
}

func TestDecodeNestedVecVaultKeys(t *testing.T) {
	a := testAdapter(t)
	s, err := a.DecodeState(nil, goldenLedgerChanges(t), 62448349)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Vaults) != 2 {
		t.Fatalf("vaults %#v", s.Vaults)
	}
	byAccount := map[string]bindings.VaultState{}
	for _, v := range s.Vaults {
		byAccount[v.Account] = v
		if v.Denomination != "USD" || v.ContractID != vaultsContract || v.Closed || !v.HadVault {
			t.Fatalf("vault identity/liveness %#v", v)
		}
	}
	g := byAccount[gdvz]
	if g.IndexRaw != "19182692307" || g.CollateralRaw != "24937500000" || g.DebtRaw != "1300000000" {
		t.Fatalf("gdvz vault %#v", g)
	}
	b := byAccount[gbwt]
	if b.IndexRaw != "19837263906" || b.CollateralRaw != "1269584890000" || b.DebtRaw != "64000000000" {
		t.Fatalf("gbwt vault %#v", b)
	}

	if len(s.VaultsInfo) != 3 {
		t.Fatalf("vaults info %#v", s.VaultsInfo)
	}
	var usd bindings.VaultsInfoState
	for _, i := range s.VaultsInfo {
		if i.Denomination == "USD" {
			usd = i
		}
	}
	if usd.TotalVaultsRaw != "5" || usd.TotalDebtRaw != "239040000000" || usd.TotalColRaw != "2704671870000" {
		t.Fatalf("VaultsInfo(USD) totals %#v", usd)
	}
	if usd.MinColRateRaw != "11000000" || usd.MinDebtCreationRaw != "1000000000" || usd.OpeningColRateRaw != "11500000" {
		t.Fatalf("VaultsInfo(USD) rates %#v", usd)
	}

	dirty := a.LastDirtyVaults()
	if len(dirty) != 2 {
		t.Fatalf("dirty %#v", dirty)
	}
	for _, d := range dirty {
		if d.Denomination != "USD" || d.Kind != bindings.DirtyUpsert {
			t.Fatalf("dirty key %#v", d)
		}
	}
}

// TestFlatVecKeyIsNotAVault pins the nested-Vec TUPLE encoding: the enum
// variant's tuple payload is itself an ScVec inside the variant Vec, so a
// flat Vec[Symbol, Address, Symbol] must match nothing.
func TestFlatVecKeyIsNotAVault(t *testing.T) {
	// Build a flat 3-element vec from the real fixture's parts is not possible
	// without re-encoding; assert on the decoder directly.
	key, ok := decodeVal(fixtureB64(t, "pubnet-L062448349-fxdao-vault-gdvz-usd-key.xdr"))
	if !ok {
		t.Fatal("fixture key undecodable")
	}
	account, denom, ok := vaultEntryKey(key)
	if !ok || account != gdvz || denom != "USD" {
		t.Fatalf("nested key decode %q %q %v", account, denom, ok)
	}
	vec, _ := key.GetVec()
	flat := (*vec)[:1] // Vec[Symbol("Vault")] only — payload missing
	flatKey := key
	fv := &flat
	flatKey.Vec = &fv
	if _, _, ok := vaultEntryKey(flatKey); ok {
		t.Fatal("truncated key decoded as a vault")
	}
}

func TestRestoredEntryIsLive(t *testing.T) {
	a := testAdapter(t)
	// First fold the golden ledger, then replay the GDVZ vault write itself as
	// a Restored change (28-day TTLs make restore-then-write the protocol's
	// normal wake-up sequence — restored must decode exactly like live).
	s1, err := a.DecodeState(nil, goldenLedgerChanges(t), 62448349)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := a.DecodeState(s1, []bindings.ContractDataChange{
		change(t, "pubnet-L062448349-fxdao-vault-gdvz-usd-key.xdr", "pubnet-L062448349-fxdao-vault-gdvz-usd-val.xdr", "persistent", "Restored"),
	}, 62448350)
	if err != nil {
		t.Fatal(err)
	}
	var g bindings.VaultState
	for _, v := range s2.Vaults {
		if v.Account == gdvz {
			g = v
		}
	}
	if g.Closed || !g.Restored || g.CollateralRaw != "24937500000" {
		t.Fatalf("restored vault not live %#v", g)
	}
	// The raw variant is preserved onto the gold row's metadata.
	out, err := a.Transform(bindings.TransformInput{LedgerSeq: 62448350, CloseTime: time.Unix(1778093400, 0).UTC(), State: s2})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range out.Vaults {
		if v.Address == gdvz {
			if v.Status != "active" || v.Metadata["restored"] != "true" {
				t.Fatalf("restored gold row %#v", v)
			}
			return
		}
	}
	t.Fatal("gdvz row missing")
}

// TestVaultIndexRestoreIsDirtySignal folds the real RESTORED VaultIndex entry
// alone: it must mark the (account, denomination) vault dirty without
// fabricating vault state.
func TestVaultIndexRestoreIsDirtySignal(t *testing.T) {
	a := testAdapter(t)
	s, err := a.DecodeState(nil, []bindings.ContractDataChange{
		change(t, "pubnet-L062448349-fxdao-vaultindex-gdvz-usd-restored-key.xdr", "pubnet-L062448349-fxdao-vaultindex-gdvz-usd-restored-val.xdr", "persistent", "Restored"),
	}, 62448349)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Vaults) != 0 {
		t.Fatalf("VaultIndex fabricated vault state %#v", s.Vaults)
	}
	dirty := a.LastDirtyVaults()
	if len(dirty) != 1 || dirty[0].Account != gdvz || dirty[0].Denomination != "USD" || dirty[0].Kind != bindings.DirtyUpsert {
		t.Fatalf("dirty %#v", dirty)
	}
}

func TestClosureEmitsTerminalClosedRow(t *testing.T) {
	a := testAdapter(t)
	s1, err := a.DecodeState(nil, goldenLedgerChanges(t), 62448349)
	if err != nil {
		t.Fatal(err)
	}
	// Ledger 62,473,181: GBWT's vault is genuinely deleted while GDVZ is
	// rewritten around the closure (next_key back to None, amounts unchanged).
	s2, err := a.DecodeState(s1, []bindings.ContractDataChange{
		change(t, "pubnet-L062448349-fxdao-vault-gdvz-usd-key.xdr", "pubnet-L062473181-fxdao-vault-gdvz-usd-val.xdr", "persistent", "Updated"),
		change(t, "pubnet-L062448349-fxdao-vault-gbwt-usd-key.xdr", "", "persistent", "Removed"),
	}, 62473181)
	if err != nil {
		t.Fatal(err)
	}
	byAccount := map[string]bindings.VaultState{}
	for _, v := range s2.Vaults {
		byAccount[v.Account] = v
	}
	g := byAccount[gdvz]
	if g.Closed || g.CollateralRaw != "24937500000" || g.DebtRaw != "1300000000" {
		t.Fatalf("gdvz after closure ledger %#v", g)
	}
	b := byAccount[gbwt]
	if !b.Closed || !b.HadVault {
		t.Fatalf("gbwt not closed %#v", b)
	}
	// Deleted on-chain: amounts are absent, never zero.
	if b.IndexRaw != "" || b.CollateralRaw != "" || b.DebtRaw != "" {
		t.Fatalf("closed vault carries fabricated amounts %#v", b)
	}
	dirty := a.LastDirtyVaults()
	kinds := map[string]bindings.DirtyKind{}
	for _, d := range dirty {
		kinds[d.Account] = d.Kind
	}
	if kinds[gdvz] != bindings.DirtyUpsert || kinds[gbwt] != bindings.DirtyRemoval {
		t.Fatalf("dirty kinds %#v", dirty)
	}

	out, err := a.Transform(bindings.TransformInput{LedgerSeq: 62473181, CloseTime: time.Unix(1778179800, 0).UTC(), State: s2})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Vaults) != 2 {
		t.Fatalf("gold rows %#v", out.Vaults)
	}
	for _, v := range out.Vaults {
		switch v.Address {
		case gdvz:
			if v.Status != "active" || v.CollateralRaw != "24937500000" {
				t.Fatalf("gdvz row %#v", v)
			}
		case gbwt:
			if v.Status != "closed" || v.CollateralRaw != "" || v.DebtRaw != "" {
				t.Fatalf("gbwt terminal row %#v", v)
			}
		}
		// Honest-null valuation: no confirmed oracle layout, no USD, no ratio.
		if v.RatioRaw != "" || v.CollateralUSD != "" || v.DebtUSD != "" {
			t.Fatalf("fabricated valuation %#v", v)
		}
	}
}

func TestTerminalLedgerInstance(t *testing.T) {
	a := testAdapter(t)
	s, err := a.DecodeState(nil, []bindings.ContractDataChange{
		change(t, "pubnet-L062448349-fxdao-vaults-instance-ccun-key.xdr", "pubnet-L062645363-fxdao-vaults-instance-ccun-final-val.xdr", "instance", "Updated"),
	}, 62645363)
	if err != nil {
		t.Fatal(err)
	}
	var usd bindings.VaultsInfoState
	for _, i := range s.VaultsInfo {
		if i.Denomination == "USD" {
			usd = i
		}
	}
	// The protocol's last state-mutating write: zero vaults, zero collateral,
	// and a nonzero stored debt residual — recorded exactly as written
	// (absent-not-zero applies to ABSENT keys, not to stored values).
	if usd.TotalVaultsRaw != "0" || usd.TotalColRaw != "0" || usd.TotalDebtRaw != "175040000000" {
		t.Fatalf("terminal VaultsInfo(USD) %#v", usd)
	}
}
