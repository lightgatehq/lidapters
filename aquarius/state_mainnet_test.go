package aquarius

// Mainnet decode tests, driven by the raw ScVal fixtures in testdata/ — REAL
// mainnet ContractDataEntry key/val slices for the pinned Aquarius pools (the
// XLM/AQUA constant-product, EURx/EURC stableswap and XLM/USDC concentrated
// pools), captured 2026-08-01 from Hubble's contract_data_xdr column (see
// testdata/REGISTRY.md for the exact entry inventory and provenance). All XDR
// is untouched; every expected constant below was hand-derived from the
// on-chain bytes with an independent decoder (a stdlib Python byte walk), not
// read back from this package.
//
// Hand-derived anchors (full derivation table in testdata/REGISTRY.md):
//
//	ccy2 CP instance @63750870: ReserveA 43048952672876, ReserveB
//	  21837045747626409, TotalShares 920330396116230, FeeFraction 30,
//	  tps 1832777, accumulated 2226439666559120, last_time 1785596598,
//	  WorkingSupply 1107920893109695, TokenShare cboh
//	gc7i share balance @57586946: 324877614546414
//	  -> CP legs 15196326354214 XLM / 7708500513693587 AQUA
//	gc7i WorkingBalance @58422172: 405464567278873; UserRewardData
//	  {to_claim 0, pool_accumulated 903562252135547}
//	  -> pending AQUA at t = last_time: 484131964419179
//	cce5 stable instance @63718535: Reserves [146555080, 652942],
//	  TotalShares 135495039, InitialA 1500 == FutureA 1500 (settled ramp
//	  -> amplification 1500) ; gaca balance @60462775: 7672285
//	  -> stable legs 8298549 EURx / 36972 EURC
//	cbbm concentrated instance @63751196: Slot0 {sqrt_price_x96
//	  32779403528916036142219842285, tick -17652}, Liquidity 177763135579314,
//	  TickSpacing 20, FeeGrowthGlobal0X128 4924211664902106266449896255854753808,
//	  FeeGrowthGlobal1X128 996792946982382866094802424747882142
//	Position(gaoa, -21880, -7980) L=23911740265087, owed 78216730/369599
//	  -> in-range amounts (22159167791450, 1885240058815)
//	Position(ga5k, -16560, -16480) L=5782465360699, owed 0/14649
//	  -> out-of-range amounts (52827604008, 0)
//	Position(gbya, -887260, 887260) L=14612333, owed 0/0
//	  -> full-range amounts (35318162, 6045622)

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lightgatehq/lidapters/bindings"
)

const (
	cpPoolID    = "CCY2PXGMKNQHO7WNYXEWX76L2C5BH3JUW3RCATGUYKY7QQTRILBZIFWV"
	stablePool  = "CCE5SYJ4EJDVN2ZNB5A3DE7UOLLHI2I3J5FOFO6U6BFSXRGMYQ6GOTH7"
	concPoolID  = "CBBMQBNHB2FYVZYV7VNHOJHUMTFJLR4PUMRVQYNW6RHIKZO2NQMIBUCV"
	cpShareID   = "CBOHAVUYKQD4C7FIVXEDJCVLUZYUO6RN3VIKEDOTIJGDDV3QN33Y4T4D"
	stbShareID  = "CC4QKBXXYJSGTZKE2VQCHORCOW2TD5EOHQROMF5T7FZOKATAVCXCVHTZ"
	xlmSAC      = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	aquaSAC     = "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK"
	usdcSAC     = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	cpLP        = "GC7IUIQ7R6NOIFNB4PYFNVYVNHSLJIULSWQTXG7UK33UTIC6NSZIW2BC"
	stableLP    = "GACAI36BCRAIFTXW7NTH7BQD3UQ7SLFCENNJGICPETTRE7VPOQ6GUWPM"
	inRangeLP   = "GAOAZ7K5ZCG4Y2FIPTELRE5WVR2NGI6KG6OTE2LZEJI2PBZ6NT3JDCMC"
	outRangeLP  = "GA5KKLGWKXTOVSF7SI3J3FYFFNK35MIVCW4SSUK5GURJTCJDAMXM63YB"
	fullRangeLP = "GBYA2DXFVWHC6ITSXC6IZ2Q65DATOAQ6ZRWJTUUDXGSWTRUPKD5FJDUH"

	cpWasm     = "ae0da5a84b15805c5c7931ac567a8d1b34be3f26b483993d9ff80cb2c3de9852"
	stableWasm = "f1077e0b77da5e62d596e13aeae4160104cad99e7ef7f3183a6c9b6ec9e747cd"
	concWasm   = "12fca5a7a96577273b6d4184cf9c984036cda0e8f0594747e7b2933dced37ee6"
)

func mainnetWasmMap() map[string]string {
	return map[string]string{
		cpWasm:     "constant_product",
		stableWasm: "stable",
		concWasm:   "concentrated",
	}
}

// fixtureChange loads testdata/<base>-{key,val}.xdr as one live entry write.
func fixtureChange(t *testing.T, contractID, base string) bindings.ContractDataChange {
	t.Helper()
	read := func(suffix string) string {
		raw, err := os.ReadFile(filepath.Join("testdata", base+suffix))
		if err != nil {
			t.Fatalf("fixture %s%s: %v", base, suffix, err)
		}
		return base64.StdEncoding.EncodeToString(raw)
	}
	val := read("-val.xdr")
	return bindings.ContractDataChange{ContractID: contractID, KeyXDR: read("-key.xdr"), ValueXDR: &val, Live: true}
}

func mainnetAdapter(t *testing.T, poolIDs ...string) *Adapter {
	t.Helper()
	a, err := NewWithConfig(Config{PoolWasmHashes: mainnetWasmMap()})
	if err != nil {
		t.Fatal(err)
	}
	a.RegisterContracts(poolIDs...)
	return a
}

func componentsByKindAsset(out *bindings.TransformOutput) map[string]bindings.AMMPositionComponent {
	m := map[string]bindings.AMMPositionComponent{}
	for _, c := range out.AMMComponents {
		m[c.Address+"|"+c.ComponentKind+"|"+c.AssetID] = c
	}
	return m
}

func TestMainnetCPInstanceDecodeAndProRataAnchors(t *testing.T) {
	a := mainnetAdapter(t, cpPoolID)
	state, err := a.DecodeState(nil, []bindings.ContractDataChange{
		fixtureChange(t, cpPoolID, "pubnet-L063750870-aquarius-pool-instance-ccy2-layoutcp"),
		fixtureChange(t, cpShareID, "pubnet-L057586946-aquarius-sharetoken-balance-cboh-gc7i"),
		fixtureChange(t, cpPoolID, "pubnet-L058422172-aquarius-pool-workingbalance-ccy2-gc7i"),
		fixtureChange(t, cpPoolID, "pubnet-L058422172-aquarius-pool-userrewarddata-ccy2-gc7i"),
	}, 63750870)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.AMMPools) != 1 {
		t.Fatalf("pools %#v", state.AMMPools)
	}
	p := state.AMMPools[0]
	if p.PoolType != "constant_product" || p.WasmHash != cpWasm {
		t.Fatalf("pool identity %q %q", p.PoolType, p.WasmHash)
	}
	if len(p.Tokens) != 2 ||
		p.Tokens[0].AssetID != xlmSAC || p.Tokens[0].ReserveRaw != "43048952672876" ||
		p.Tokens[1].AssetID != aquaSAC || p.Tokens[1].ReserveRaw != "21837045747626409" {
		t.Fatalf("tokens %#v", p.Tokens)
	}
	if p.TotalSharesRaw != "920330396116230" || p.FeeFractionRaw != "30" || p.ProtocolFeeFractionRaw != "5000" {
		t.Fatalf("shares/fees %q %q %q", p.TotalSharesRaw, p.FeeFractionRaw, p.ProtocolFeeFractionRaw)
	}
	if p.RewardTpsRaw != "1832777" || p.RewardExpiredAtRaw != "1785600622" ||
		p.RewardAccumulatedRaw != "2226439666559120" || p.RewardLastTimeRaw != "1785596598" ||
		p.WorkingSupplyRaw != "1107920893109695" || p.RewardTokenID != aquaSAC {
		t.Fatalf("reward config %#v", p)
	}
	if len(state.AMMPositions) != 1 {
		t.Fatalf("positions %#v", state.AMMPositions)
	}
	pos := state.AMMPositions[0]
	if pos.Address != cpLP || pos.PoolContractID != cpPoolID {
		t.Fatalf("position identity %#v", pos)
	}
	if pos.SharesRaw != "324877614546414" {
		t.Fatalf("shares %q (WorkingBalance must never clobber the share balance)", pos.SharesRaw)
	}
	if pos.WorkingBalanceRaw != "405464567278873" {
		t.Fatalf("working balance %q", pos.WorkingBalanceRaw)
	}
	if pos.PendingRewardRaw != "0" || pos.RewardPoolAccumulatedRaw != "903562252135547" {
		t.Fatalf("reward checkpoint %q %q", pos.PendingRewardRaw, pos.RewardPoolAccumulatedRaw)
	}
	if !pos.HadShares {
		t.Fatal("nonzero share balance must set HadShares")
	}

	out, err := a.Transform(bindings.TransformInput{LedgerSeq: 63750870, CloseTime: time.Unix(1785596598, 0).UTC(), State: state})
	if err != nil {
		t.Fatal(err)
	}
	legs := componentsByKindAsset(out)
	if got := legs[cpLP+"|lp_principal|"+xlmSAC].AmountRaw; got != "15196326354214" {
		t.Fatalf("XLM leg %q, want 15196326354214", got)
	}
	if got := legs[cpLP+"|lp_principal|"+aquaSAC].AmountRaw; got != "7708500513693587" {
		t.Fatalf("AQUA leg %q, want 7708500513693587", got)
	}
	if len(out.AMMRewards) != 1 || out.AMMRewards[0].AmountRaw != "484131964419179" {
		t.Fatalf("pending AQUA %#v, want 484131964419179", out.AMMRewards)
	}
}

func TestMainnetStableInstanceDecodeAndProRataAnchors(t *testing.T) {
	a := mainnetAdapter(t, stablePool)
	state, err := a.DecodeState(nil, []bindings.ContractDataChange{
		fixtureChange(t, stablePool, "pubnet-L063718535-aquarius-pool-instance-cce5-layoutstable"),
		fixtureChange(t, stbShareID, "pubnet-L060462775-aquarius-sharetoken-balance-cc4q-gaca"),
	}, 63718535)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.AMMPools) != 1 {
		t.Fatalf("pools %#v", state.AMMPools)
	}
	p := state.AMMPools[0]
	if p.PoolType != "stable" || p.TotalSharesRaw != "135495039" || p.FeeFractionRaw != "10" {
		t.Fatalf("stable pool %#v", p)
	}
	// InitialA == FutureA == 1500 (u128) in the raw instance: the ramp is
	// settled, so the pool has exactly one amplification.
	if p.AmplificationRaw != "1500" {
		t.Fatalf("amplification %q, want the settled ramp value 1500", p.AmplificationRaw)
	}
	if len(p.Tokens) != 2 || p.Tokens[0].ReserveRaw != "146555080" || p.Tokens[1].ReserveRaw != "652942" {
		t.Fatalf("stable reserves %#v", p.Tokens)
	}
	out, err := a.Transform(bindings.TransformInput{LedgerSeq: 63718535, CloseTime: time.Unix(1785595000, 0).UTC(), State: state})
	if err != nil {
		t.Fatal(err)
	}
	legs := componentsByKindAsset(out)
	if got := legs[stableLP+"|lp_principal|"+p.Tokens[0].AssetID].AmountRaw; got != "8298549" {
		t.Fatalf("EURx leg %q, want 8298549", got)
	}
	if got := legs[stableLP+"|lp_principal|"+p.Tokens[1].AssetID].AmountRaw; got != "36972" {
		t.Fatalf("EURC leg %q, want 36972", got)
	}
}

// TestMainnetConcentratedRangeDecodeAnchors pins the (owner, tick_lower,
// tick_upper) position keying and the in/out/full-range principal
// decomposition against the pinned XLM/USDC pool.
func TestMainnetConcentratedRangeDecodeAnchors(t *testing.T) {
	a := mainnetAdapter(t, concPoolID)
	state, err := a.DecodeState(nil, []bindings.ContractDataChange{
		fixtureChange(t, concPoolID, "pubnet-L063751196-aquarius-pool-instance-cbbm-layoutconc"),
		fixtureChange(t, concPoolID, "pubnet-L063066122-aquarius-pool-position-cbbm-gaoa-inrange"),
		fixtureChange(t, concPoolID, "pubnet-L063422562-aquarius-pool-position-cbbm-ga5k-outofrange"),
		fixtureChange(t, concPoolID, "pubnet-L063685926-aquarius-pool-position-cbbm-gbya-fullrange"),
	}, 63751196)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.AMMPools) != 1 {
		t.Fatalf("pools %#v", state.AMMPools)
	}
	p := state.AMMPools[0]
	if p.PoolType != "concentrated" || p.TickSpacing != 20 || p.CurrentTick != -17652 {
		t.Fatalf("concentrated pool frame %#v", p)
	}
	if p.SqrtPriceX96 != "32779403528916036142219842285" || p.ActiveLiquidityRaw != "177763135579314" {
		t.Fatalf("slot0/liquidity %q %q", p.SqrtPriceX96, p.ActiveLiquidityRaw)
	}
	// FeeGrowthGlobal{0,1}X128 are u256 X128 fixed-point accumulators; the
	// expected decimals are byte walks of the raw instance value
	// (0x03b45e6124c27d0ef023fc86b6283810 / 0x00bff9afc8996cd33c325f82ebf9769e).
	if p.FeeGrowthGlobal0X128 != "4924211664902106266449896255854753808" {
		t.Fatalf("fee growth 0 %q", p.FeeGrowthGlobal0X128)
	}
	if p.FeeGrowthGlobal1X128 != "996792946982382866094802424747882142" {
		t.Fatalf("fee growth 1 %q", p.FeeGrowthGlobal1X128)
	}
	if p.AmplificationRaw != "" {
		t.Fatalf("a concentrated pool has no amplification, got %q", p.AmplificationRaw)
	}
	if len(p.Tokens) != 2 || p.Tokens[0].AssetID != xlmSAC || p.Tokens[1].AssetID != usdcSAC ||
		p.Tokens[0].ReserveRaw != "51409225147481" || p.Tokens[1].ReserveRaw != "4541883346199" {
		t.Fatalf("token0/token1 %#v", p.Tokens)
	}
	if len(state.AMMPositions) != 3 {
		t.Fatalf("want 3 distinct range positions, got %#v", state.AMMPositions)
	}
	byOwner := map[string]bindings.AMMPositionState{}
	for _, pos := range state.AMMPositions {
		byOwner[pos.Address] = pos
	}
	if pos := byOwner[inRangeLP]; pos.TickLower != -21880 || pos.TickUpper != -7980 || pos.LiquidityRaw != "23911740265087" || !pos.HadShares {
		t.Fatalf("gaoa position %#v", pos)
	}
	if pos := byOwner[outRangeLP]; pos.TickLower != -16560 || pos.TickUpper != -16480 || pos.LiquidityRaw != "5782465360699" {
		t.Fatalf("ga5k position %#v", pos)
	}
	if pos := byOwner[fullRangeLP]; pos.TickLower != -887260 || pos.TickUpper != 887260 || pos.LiquidityRaw != "14612333" {
		t.Fatalf("gbya position %#v", pos)
	}

	out, err := a.Transform(bindings.TransformInput{LedgerSeq: 63751196, CloseTime: time.Unix(1785596600, 0).UTC(), State: state})
	if err != nil {
		t.Fatal(err)
	}
	legs := componentsByKindAsset(out)
	checks := []struct{ key, want string }{
		{inRangeLP + "|range_principal|" + xlmSAC, "22159167791450"},
		{inRangeLP + "|range_principal|" + usdcSAC, "1885240058815"},
		{inRangeLP + "|unclaimed_fee|" + xlmSAC, "78216730"},
		{inRangeLP + "|unclaimed_fee|" + usdcSAC, "369599"},
		{outRangeLP + "|range_principal|" + xlmSAC, "52827604008"},
		{outRangeLP + "|unclaimed_fee|" + usdcSAC, "14649"},
		{fullRangeLP + "|range_principal|" + xlmSAC, "35318162"},
		{fullRangeLP + "|range_principal|" + usdcSAC, "6045622"},
	}
	for _, c := range checks {
		leg, ok := legs[c.key]
		if !ok || leg.AmountRaw != c.want {
			t.Errorf("component %s = %q, want %q", c.key, leg.AmountRaw, c.want)
		}
	}
	if len(out.AMMComponents) != len(checks) {
		t.Fatalf("component count %d, want %d: %#v", len(out.AMMComponents), len(checks), out.AMMComponents)
	}
	// D-05 keying: the same owner at different tick bounds must never share a
	// position group.
	groups := map[string]struct{}{}
	for _, c := range out.AMMComponents {
		groups[c.PositionGroupID] = struct{}{}
		if c.TickLower == nil || c.TickUpper == nil {
			t.Fatalf("range component without tick bounds: %#v", c)
		}
	}
	if len(groups) != 3 {
		t.Fatalf("want 3 distinct position groups, got %d", len(groups))
	}
}

// TestMainnetConcentratedEntryRemovalClosesOnlyThatRange exercises the
// HadShares close lifecycle on the real exit path: a full withdraw deletes
// the Position entry, which must tombstone exactly that range — and must not
// tear down the pool or any sibling position.
func TestMainnetConcentratedEntryRemovalClosesOnlyThatRange(t *testing.T) {
	a := mainnetAdapter(t, concPoolID)
	state, err := a.DecodeState(nil, []bindings.ContractDataChange{
		fixtureChange(t, concPoolID, "pubnet-L063751196-aquarius-pool-instance-cbbm-layoutconc"),
		fixtureChange(t, concPoolID, "pubnet-L063066122-aquarius-pool-position-cbbm-gaoa-inrange"),
		fixtureChange(t, concPoolID, "pubnet-L063685926-aquarius-pool-position-cbbm-gbya-fullrange"),
	}, 63751196)
	if err != nil {
		t.Fatal(err)
	}
	removal := fixtureChange(t, concPoolID, "pubnet-L063066122-aquarius-pool-position-cbbm-gaoa-inrange")
	removal.ValueXDR = nil
	removal.Live = false
	removal.ChangeType = "Removed"
	state, err = a.DecodeState(state, []bindings.ContractDataChange{removal}, 63751200)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.AMMPools) != 1 {
		t.Fatal("a per-position entry removal must not tear down the pool")
	}
	if len(state.AMMPositions) != 2 {
		t.Fatalf("positions %#v", state.AMMPositions)
	}
	out, err := a.Transform(bindings.TransformInput{LedgerSeq: 63751200, CloseTime: time.Unix(1785596700, 0).UTC(), State: state})
	if err != nil {
		t.Fatal(err)
	}
	closed := 0
	for _, c := range out.AMMComponents {
		if c.Address == inRangeLP {
			if c.ComponentKind != "range_principal" || c.AmountRaw != "0" || c.Metadata["closed"] != "true" {
				t.Fatalf("bad close tombstone %#v", c)
			}
			if c.TickLower == nil || *c.TickLower != -21880 || c.TickUpper == nil || *c.TickUpper != -7980 {
				t.Fatalf("tombstone lost its tick bounds: %#v", c)
			}
			closed++
		}
	}
	if closed != 2 {
		t.Fatalf("want 2 close tombstones, got %d", closed)
	}
	// The untouched sibling still decomposes.
	if _, ok := componentsByKindAsset(out)[fullRangeLP+"|range_principal|"+xlmSAC]; !ok {
		t.Fatal("sibling position lost by the removal")
	}
}

// TestTransformQuarantinesInsteadOfDropping pins the two decomposition
// failure paths: a position against a never-folded pool, and a share state
// pro-rata cannot decompose. Both must surface as QuarantineEvents, never a
// silent drop.
func TestTransformQuarantinesInsteadOfDropping(t *testing.T) {
	a, err := NewWithConfig(Config{})
	if err != nil {
		t.Fatal(err)
	}
	in := bindings.TransformInput{LedgerSeq: 11, CloseTime: time.Unix(11, 0).UTC(), State: &bindings.LedgerState{
		AMMPools: []bindings.AMMPoolState{{Protocol: "aquarius", ContractID: "pool", PoolType: "constant_product", TotalSharesRaw: "0", Tokens: []bindings.AMMTokenReserve{{AssetID: "a", ReserveRaw: "10"}, {AssetID: "b", ReserveRaw: "20"}}}},
		AMMPositions: []bindings.AMMPositionState{
			{Address: "user", PoolContractID: "pool", SharesRaw: "5"},
			{Address: "user", PoolContractID: "ghost-pool", SharesRaw: "5"},
		},
	}}
	out, err := a.Transform(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.AMMComponents) != 0 {
		t.Fatalf("undecomposable positions still emitted: %#v", out.AMMComponents)
	}
	reasons := map[string]int{}
	for _, q := range out.Quarantine {
		reasons[q.Reason]++
	}
	if reasons["invalid_lp_share_state"] != 2 {
		t.Fatalf("zero-total-shares legs not quarantined: %#v", out.Quarantine)
	}
	if reasons["aquarius_position_unknown_pool"] != 1 {
		t.Fatalf("unknown-pool position not quarantined: %#v", out.Quarantine)
	}
}
