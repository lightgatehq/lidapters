package soroswap

// Mainnet decode tests over testdata/*.xdr — REAL pubnet ScVal bytes for the
// Soroswap factory and the three golden pairs (see testdata/REGISTRY.md for
// provenance). Every expected constant below was hand-derived from the raw
// XDR with an independent stdlib byte walk (no stellar SDK), never read back
// from this package.
//
// Hand-derived anchors (registry "Hand derivation" section, re-verified):
//
//	cdlm instance @63750862: Token0 = XLM SAC, Token1 = LIBRE,
//	  Reserve0 = 6_911_037_650, Reserve1 = 1_174_050_007_304,
//	  TotalSupply = 87_628_791_895, factory = fact, KLast ABSENT,
//	  METADATA symbol "native-LIBRE-SOROSWAP-LP" decimal 7
//	cam7 instance @63751402: Reserve0 = 3_379_689_401_562,
//	  Reserve1 = 578_116_831_925, TotalSupply = 1_219_283_143_019
//	cccd instance @63750872: Reserve0 = 257_958_218_761,
//	  Reserve1 = 6_238_333_411_519, TotalSupply = 1_209_342_926_092
//	Balance(gb77) on cdlm @63415332 = 67_512_400_545
//	Balance(gacx) on cam7 @63491372 = 1_192_466_439_370
//	Balance(gc7i) on cccd @58465535 = 1_191_193_505_547
//	Balance(cccd) on cccd @55467506 = 1000 (the pair's own MINIMUM_LIQUIDITY
//	  lock — a CONTRACT address as a legitimate LP holder)
//	factory instance @63675481: TotalPairs = 209, FeeTo = FeeToSetter =
//	  GAYPUMZF…, FeesEnabled ABSENT; PairAddressesNIndexed(0) @62344317 =
//	  CB46LMGJ…

import (
	"bufio"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lightgatehq/lidapters/bindings"
)

const (
	factoryID = "CA4HEQTL2WPEUYKYKCDOHCDNIV4QHNJ7EL4J4NQ6VADP7SYHVRYZ7AW2"
	cdlmPair  = "CDLMAKG5TSJA6FGP7LLC2FKJRQW6DQYMEPP6FURFVULDEQMP3PRZ4ISI"
	cam7Pair  = "CAM7DY53G63XA4AJRS24Z6VFYAFSSF76C3RZ45BE5YU3FQS5255OOABP"
	cccdPair  = "CCCDU62TWI744KFK6COAW2PARPVPXKKE3DBVBUZCFWZOGGD7HZ5YEY3X"

	xlmSAC    = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	libreToken = "CBEM2CAIYLM3HBOPU5HLQL7V5BUAKM3N77DYQKX4FNHTQLQUUD2ZFBOX"
	usdcToken = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	blndToken = "CD25MNVTZDL4Y3XBCPCJXGXATV5WUHHOWMYFF4YBEGU5FCPGMYTVG5JY"

	gb77 = "GB77C7CHJQGWNMDPWRXXN5KMS55K5SSERBGND4GGCYDDBLC52FEKHUOR"
	gacx = "GACXANRYOSGYKYI3CRTSIOFLTCCO3AWIN5IFJEF4VO4OU4IWR4WAQ4ON"
	gc7i = "GC7IUIQ7R6NOIFNB4PYFNVYVNHSLJIULSWQTXG7UK33UTIC6NSZIW2BC"

	pairWasm = "18051456816b66f12e773a56f77c5794fac1b1fb7ab6e22d4fad5a412770f73e"
)

func fixtureB64(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func fixtureChange(t *testing.T, contractID, base, durability, changeType string) bindings.ContractDataChange {
	t.Helper()
	val := fixtureB64(t, base+"-val.xdr")
	return bindings.ContractDataChange{
		ContractID: contractID,
		KeyXDR:     fixtureB64(t, base+"-key.xdr"),
		ValueXDR:   &val,
		Durability: durability,
		ChangeType: changeType,
		Live:       true,
	}
}

func mainnetAdapter(t *testing.T) *Adapter {
	t.Helper()
	a, err := NewWithConfig(Config{Factories: map[string]struct{}{factoryID: {}}})
	if err != nil {
		t.Fatal(err)
	}
	a.RegisterPairContracts(cdlmPair, cam7Pair, cccdPair)
	return a
}

func TestDecodePairInstancesU32Layout(t *testing.T) {
	a := mainnetAdapter(t)
	s, err := a.DecodeState(nil, []bindings.ContractDataChange{
		fixtureChange(t, cdlmPair, "pubnet-L063750862-soroswap-pair-instance-cdlm-layoutu32", "instance", "Updated"),
		fixtureChange(t, cam7Pair, "pubnet-L063751402-soroswap-pair-instance-cam7-layoutu32", "instance", "Updated"),
		fixtureChange(t, cccdPair, "pubnet-L063750872-soroswap-pair-instance-cccd-layoutu32", "instance", "Updated"),
	}, 63751402)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.AMMPools) != 3 {
		t.Fatalf("pools %#v", s.AMMPools)
	}
	want := map[string]struct {
		token0, token1, r0, r1, total string
	}{
		cdlmPair: {xlmSAC, libreToken, "6911037650", "1174050007304", "87628791895"},
		cam7Pair: {xlmSAC, usdcToken, "3379689401562", "578116831925", "1219283143019"},
		cccdPair: {usdcToken, blndToken, "257958218761", "6238333411519", "1209342926092"},
	}
	for _, p := range s.AMMPools {
		w := want[p.ContractID]
		if p.PoolType != "constant_product" {
			t.Fatalf("%s pool type %q", p.ContractID, p.PoolType)
		}
		if p.WasmHash != pairWasm {
			t.Fatalf("%s wasm %q", p.ContractID, p.WasmHash)
		}
		if p.RouterContract != factoryID {
			t.Fatalf("%s discovery authority %q", p.ContractID, p.RouterContract)
		}
		if len(p.Tokens) != 2 || p.Tokens[0].AssetID != w.token0 || p.Tokens[1].AssetID != w.token1 {
			t.Fatalf("%s tokens %#v", p.ContractID, p.Tokens)
		}
		if p.Tokens[0].ReserveRaw != w.r0 || p.Tokens[1].ReserveRaw != w.r1 {
			t.Fatalf("%s reserves %#v", p.ContractID, p.Tokens)
		}
		if p.TotalSharesRaw != w.total {
			t.Fatalf("%s total shares %q", p.ContractID, p.TotalSharesRaw)
		}
		// Absent-not-zero: no fee fraction is stored on-chain (the 0.3% rate is
		// code, not storage) and KLast is absent from these instances.
		if p.FeeFractionRaw != "" || p.ProtocolFeeFractionRaw != "" {
			t.Fatalf("%s fabricated fee fields %#v", p.ContractID, p)
		}
	}
	// The pair's own LP-token METADATA folds as asset identity.
	syms := map[string]string{}
	for _, m := range s.AMMAssets {
		syms[m.ContractID] = m.Symbol
		if m.Decimals != 7 {
			t.Fatalf("LP decimals %#v", m)
		}
	}
	if syms[cdlmPair] != "native-LIBRE-SOROSWAP-LP" || syms[cam7Pair] != "native-USDC-SOROSWAP-LP" || syms[cccdPair] != "USDC-BLND-SOROSWAP-LP" {
		t.Fatalf("LP metadata %#v", syms)
	}
}

func TestDecodeBalancesAndProRataAnchors(t *testing.T) {
	a := mainnetAdapter(t)
	s, err := a.DecodeState(nil, []bindings.ContractDataChange{
		fixtureChange(t, cdlmPair, "pubnet-L063750862-soroswap-pair-instance-cdlm-layoutu32", "instance", "Updated"),
		fixtureChange(t, cam7Pair, "pubnet-L063751402-soroswap-pair-instance-cam7-layoutu32", "instance", "Updated"),
		fixtureChange(t, cccdPair, "pubnet-L063750872-soroswap-pair-instance-cccd-layoutu32", "instance", "Updated"),
		fixtureChange(t, cdlmPair, "pubnet-L063415332-soroswap-pair-balance-cdlm-gb77", "persistent", "Updated"),
		fixtureChange(t, cam7Pair, "pubnet-L063491372-soroswap-pair-balance-cam7-gacx", "persistent", "Updated"),
		fixtureChange(t, cccdPair, "pubnet-L058465535-soroswap-pair-balance-cccd-gc7i", "persistent", "Updated"),
		fixtureChange(t, cccdPair, "pubnet-L055467506-soroswap-pair-balance-cccd-cccd-minlock", "persistent", "Updated"),
	}, 63751402)
	if err != nil {
		t.Fatal(err)
	}
	shares := map[string]string{}
	for _, p := range s.AMMPositions {
		shares[p.PoolContractID+"|"+p.Address] = p.SharesRaw
		if !p.HadShares {
			t.Fatalf("nonzero balance without HadShares %#v", p)
		}
	}
	if shares[cdlmPair+"|"+gb77] != "67512400545" ||
		shares[cam7Pair+"|"+gacx] != "1192466439370" ||
		shares[cccdPair+"|"+gc7i] != "1191193505547" {
		t.Fatalf("wallet balances %#v", shares)
	}
	// The MINIMUM_LIQUIDITY first-mint lock: the pair contract holds 1000 of
	// its own shares — a contract address the census enrolls like any wallet.
	if shares[cccdPair+"|"+cccdPair] != "1000" {
		t.Fatalf("min-liquidity lock %#v", shares)
	}

	dirty := a.LastDirtyPositions()
	if len(dirty) != 4 {
		t.Fatalf("dirty %#v", dirty)
	}
	for _, d := range dirty {
		if d.Kind != bindings.DirtyUpsert {
			t.Fatalf("dirty kind %#v", d)
		}
	}

	out, err := a.Transform(bindings.TransformInput{LedgerSeq: 63751402, CloseTime: time.Unix(1754000000, 0).UTC(), State: s})
	if err != nil {
		t.Fatal(err)
	}
	amounts := map[string]string{}
	for _, c := range out.AMMComponents {
		if c.ComponentKind != "lp_principal" {
			t.Fatalf("component kind %#v", c)
		}
		amounts[c.Address+"|"+c.AssetID] = c.AmountRaw
	}
	// Pro-rata anchors: shares * reserve_i / TotalSupply, floor division, all
	// inputs hand-read from the raw instance/balance bytes.
	anchors := map[string]string{
		gb77 + "|" + xlmSAC:     "5324514145",
		gb77 + "|" + libreToken: "904530721454",
		gacx + "|" + xlmSAC:     "3305357094397",
		gacx + "|" + usdcToken:  "565401829798",
		gc7i + "|" + usdcToken:  "254086866728",
		gc7i + "|" + blndToken:  "6144710557204",
	}
	for k, want := range anchors {
		if amounts[k] != want {
			t.Fatalf("pro-rata %s: got %q want %q", k, amounts[k], want)
		}
	}
	if len(out.Quarantine) != 0 {
		t.Fatalf("unexpected quarantine %#v", out.Quarantine)
	}
}

func TestFactoryRegistryDiscovery(t *testing.T) {
	// The .jsonl registry carries the raw key/val bytes of every live
	// PairAddressesNIndexed(u32) entry (209 at capture). Folding them must
	// register every pair for ownership without fabricating pool rows.
	f, err := os.Open(filepath.Join("testdata", "pubnet-L063675481-soroswap-factory-pairindex-registry-fact.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var changes []bindings.ContractDataChange
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var row struct {
			Index              int    `json:"index"`
			KeyXDRHex          string `json:"key_xdr_hex"`
			ValXDRHex          string `json:"val_xdr_hex"`
			LastModifiedLedger uint32 `json:"last_modified_ledger"`
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		kb, err := hex.DecodeString(row.KeyXDRHex)
		if err != nil {
			t.Fatal(err)
		}
		vb, err := hex.DecodeString(row.ValXDRHex)
		if err != nil {
			t.Fatal(err)
		}
		val := base64.StdEncoding.EncodeToString(vb)
		changes = append(changes, bindings.ContractDataChange{
			ContractID: factoryID,
			KeyXDR:     base64.StdEncoding.EncodeToString(kb),
			ValueXDR:   &val,
			Durability: "persistent",
			ChangeType: "Created",
			Live:       true,
			LastModifiedLedger: row.LastModifiedLedger,
		})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(changes) != 209 {
		t.Fatalf("registry rows %d", len(changes))
	}
	a, err := NewWithConfig(Config{Factories: map[string]struct{}{factoryID: {}}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.DecodeState(nil, changes, 63675481)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.pairs) < 200 {
		t.Fatalf("discovered %d pairs, want >= 200", len(a.pairs))
	}
	for _, id := range []string{"CB46LMGJC7SYSH4C7SBNLV635OX5BSNQDGRR32NRXAV7N2AVNZMQUJ3A", cdlmPair, cam7Pair, cccdPair} {
		if !a.OwnsContract(id) {
			t.Fatalf("pair %s not discovered", id)
		}
	}
	// Discovery registers ownership; pool rows materialize only when a pair's
	// own instance folds.
	if len(s.AMMPools) != 0 {
		t.Fatalf("registry fold fabricated pools %#v", s.AMMPools)
	}
}

func TestFactoryInstanceRecognizedNotCarried(t *testing.T) {
	a := mainnetAdapter(t)
	s, err := a.DecodeState(nil, []bindings.ContractDataChange{
		fixtureChange(t, factoryID, "pubnet-L063675481-soroswap-factory-instance-fact", "instance", "Updated"),
		fixtureChange(t, factoryID, "pubnet-L062344317-soroswap-factory-pairindex-fact-n0", "persistent", "Updated"),
	}, 63675481)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.AMMPools) != 0 || len(s.AMMPositions) != 0 {
		t.Fatalf("factory entries leaked into pool/position state %#v %#v", s.AMMPools, s.AMMPositions)
	}
	if !a.OwnsContract("CB46LMGJC7SYSH4C7SBNLV635OX5BSNQDGRR32NRXAV7N2AVNZMQUJ3A") {
		t.Fatal("PairAddressesNIndexed(0) did not register its pair")
	}
}
