package blend

// Reflector decode tests, driven by testdata/reflector_mainnet.json — REAL
// mainnet contract_data changes (both feed storage protocols and the Fixed
// aggregator's whole config assembly) extracted from the bronze archive with
// untouched XDR. Every expected price constant below was hand-derived from the
// on-chain values independently of the decoder:
//
//	CEX feed (backs the Fixed aggregator; base fiat USD; 14 decimals):
//	  round 1744386000000: XLM 23319734333487  USDC 100049014360832  EURC 113434046199375
//	  round 1744386300000: XLM 23350127418476  USDC 100045020259116  EURC 113322710193460
//	  round 1782472800000: (instance-cache round riding round 1782473100000's rewrite)
//	  round 1782473100000: XLM 17445678924432  USDC 100111152528926  EURC 114201759459499
//	  round 1782473400000: XLM 17432999981295  USDC 100111136721026  EURC 114207531624160
//	  round 1782473700000: XLM 17447100518867  USDC 100112817578160  EURC 114196337841055
//	DEX feed (backs the YieldBlox aggregator; base USDC token; 14 decimals):
//	  round 1782473100000: XLM(idx 7) 17478364261058
//	  round 1782473400000: XLM(idx 7) 17473840523802
//
// The aggregator reports 7 decimals, so an expected reserve price is the
// 14-decimal round value floor-divided by 10^7.

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/lightgatehq/lidapters/blend/contracts"
	"github.com/stellar/go-stellar-sdk/xdr"
)

const (
	fixedAggregatorID = "CCVTVW2CVA7JLH4ROQGP3CU4T3EXVCK66AZGSM4MUQPXAI4QHCZPOATS"
	cexFeedID         = "CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN"
	dexFeedID         = "CALI2BYU2JE6WVRUFYTS6MSBNEHGJ35P4AVCZYF3B6QOE3QKOB2PLE6M"
	fixedPoolID       = "CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD"
	yieldBloxPoolID   = "CCCCIQSDILITHMM7PBSLVDT5MISSY7R26MNZXCX4H7J5JQ5FPIYOGYFS"

	xlmSACID    = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	usdcTokenID = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	eurcTokenID = "CDTKPWPLOURQA2SGTKTUQOWRCBZEORB4BWBOMJ3D3ZTQQSGE5F6JBQLV"

	// YieldBlox aggregator BaseAssets (hardcoded price 1, no feed), per the
	// aggregator's live instance storage.
	ybxBaseAsset1 = "CB226ZOEYXTBPD3QEGABTJYSKZVBP2PASEISLG3SBMTN5CE4QZUVZ3CE"
	ybxBaseAsset2 = "CCCRWH6Q3FNP3I2I57BDLM5AFAT7O6OF6GKQOC6SSJNDAVRZ57SPHGU2"
)

type reflectorFixtureLedger struct {
	LedgerSeq     int64                         `json:"ledger_seq"`
	CloseTimeUnix int64                         `json:"close_time_unix"`
	Changes       []bindings.ContractDataChange `json:"-"`
	RawChanges    []reflectorFixtureChange      `json:"changes"`
}

type reflectorFixtureChange struct {
	ContractID         string  `json:"contract_id"`
	KeyXDR             string  `json:"key_xdr"`
	ValueXDR           *string `json:"value_xdr"`
	Durability         string  `json:"durability"`
	ChangeType         string  `json:"change_type"`
	Live               bool    `json:"live"`
	LiveUntilLedgerSeq *uint32 `json:"live_until_ledger_seq"`
	LastModifiedLedger uint32  `json:"last_modified_ledger"`
}

func loadReflectorFixture(t *testing.T) []reflectorFixtureLedger {
	t.Helper()
	raw, err := os.ReadFile("testdata/reflector_mainnet.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var doc struct {
		Ledgers []reflectorFixtureLedger `json:"ledgers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	for i := range doc.Ledgers {
		for _, ch := range doc.Ledgers[i].RawChanges {
			doc.Ledgers[i].Changes = append(doc.Ledgers[i].Changes, bindings.ContractDataChange{
				ContractID:         ch.ContractID,
				KeyXDR:             ch.KeyXDR,
				ValueXDR:           ch.ValueXDR,
				Durability:         ch.Durability,
				ChangeType:         ch.ChangeType,
				Live:               ch.Live,
				LiveUntilLedgerSeq: ch.LiveUntilLedgerSeq,
				LastModifiedLedger: ch.LastModifiedLedger,
			})
		}
	}
	return doc.Ledgers
}

func newReflectorAdapter(t *testing.T) *Adapter {
	t.Helper()
	adapter, err := New(Config{AllowUnknownV2: true})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	adapter.RegisterContracts(fixedPoolID, fixedAggregatorID)
	adapter.RegisterPriceFeeds(cexFeedID, dexFeedID)
	return adapter
}

// fixedPoolPrior seeds a prior state holding the real Fixed pool wired to its
// real aggregator, with its three reserves. The pool's own contract_data is not
// part of the fixture; what is under test is the price path, and this is the
// exact shape the pool fold produces for it.
func fixedPoolPrior() *bindings.LedgerState {
	return &bindings.LedgerState{
		Pools: []contracts.PoolState{{
			ContractID:     fixedPoolID,
			OracleContract: fixedAggregatorID,
			PoolStatus:     "active",
			Reserves: []contracts.ReserveState{
				{AssetID: xlmSACID, ReserveIndex: 0, ReserveIndexKnown: true},
				{AssetID: usdcTokenID, ReserveIndex: 1, ReserveIndexKnown: true},
				{AssetID: eurcTokenID, ReserveIndex: 2, ReserveIndexKnown: true},
			},
		}},
	}
}

func foldFixtureLedgers(t *testing.T, adapter *Adapter, prior *bindings.LedgerState, ledgers []reflectorFixtureLedger) *bindings.LedgerState {
	t.Helper()
	state := prior
	for _, ledger := range ledgers {
		next, err := adapter.DecodeStateAt(state, ledger.Changes, ledger.LedgerSeq, time.Unix(ledger.CloseTimeUnix, 0).UTC())
		if err != nil {
			t.Fatalf("decode ledger %d: %v", ledger.LedgerSeq, err)
		}
		state = next
	}
	return state
}

func reflectorReservePrice(t *testing.T, state *bindings.LedgerState, poolID, assetID string) (string, int32) {
	t.Helper()
	for _, pool := range state.Pools {
		if pool.ContractID != poolID {
			continue
		}
		for _, reserve := range pool.Reserves {
			if reserve.AssetID == assetID {
				return reserve.OraclePriceRaw, reserve.OracleDecimals
			}
		}
	}
	t.Fatalf("reserve %s not found in pool %s", assetID, poolID)
	return "", 0
}

func ledgersThrough(ledgers []reflectorFixtureLedger, seq int64) []reflectorFixtureLedger {
	out := make([]reflectorFixtureLedger, 0, len(ledgers))
	for _, l := range ledgers {
		if l.LedgerSeq <= seq {
			out = append(out, l)
		}
	}
	return out
}

// TestReflector_GoldenFixture_ProtocolTwo folds the whole fixture — aggregator
// config assembly, protocol-1 rounds, then the 2026 protocol-2 batched rounds —
// and asserts the Fixed pool's reserves resolve the hand-derived 7-decimal
// prices of the newest round.
func TestReflector_GoldenFixture_ProtocolTwo(t *testing.T) {
	t.Parallel()
	ledgers := loadReflectorFixture(t)
	adapter := newReflectorAdapter(t)

	state := foldFixtureLedgers(t, adapter, fixedPoolPrior(), ledgers)

	for _, tc := range []struct {
		asset string
		want  string
	}{
		{xlmSACID, "1744710"},     // 17447100518867 / 1e7
		{usdcTokenID, "10011281"}, // 100112817578160 / 1e7
		{eurcTokenID, "11419633"}, // 114196337841055 / 1e7
	} {
		got, decimals := reflectorReservePrice(t, state, fixedPoolID, tc.asset)
		if got != tc.want {
			t.Errorf("asset %s: OraclePriceRaw = %q, want %q", tc.asset, got, tc.want)
		}
		if decimals != 7 {
			t.Errorf("asset %s: OracleDecimals = %d, want 7", tc.asset, decimals)
		}
	}

	// The carried feed state stays bounded and configured.
	for _, feed := range state.PriceFeeds {
		if len(feed.Rounds) > maxCarriedRounds {
			t.Errorf("feed %s carries %d rounds, cap is %d", feed.ContractID, len(feed.Rounds), maxCarriedRounds)
		}
	}
	var cex contracts.PriceFeedState
	for _, feed := range state.PriceFeeds {
		if feed.ContractID == cexFeedID {
			cex = feed
		}
	}
	if len(cex.Assets) != 16 {
		t.Errorf("CEX feed decoded %d assets, want 16", len(cex.Assets))
	}
	if cex.Decimals != 14 {
		t.Errorf("CEX feed decimals = %d, want 14", cex.Decimals)
	}
	if cex.LastRoundMs != 1782473700000 {
		t.Errorf("CEX last round = %d, want 1782473700000", cex.LastRoundMs)
	}
}

// TestReflector_GoldenFixture_CacheSeedsDeviationGuard pins the protocol-2
// instance-cache decode: the first 2026 CEX write (ledger 63205617) carries the
// previous round in its instance cache, so the deviation guard is satisfiable
// from a single observed write.
func TestReflector_GoldenFixture_CacheSeedsDeviationGuard(t *testing.T) {
	t.Parallel()
	ledgers := loadReflectorFixture(t)
	adapter := newReflectorAdapter(t)

	state := foldFixtureLedgers(t, adapter, fixedPoolPrior(), ledgersThrough(ledgers, 63205617))

	got, _ := reflectorReservePrice(t, state, fixedPoolID, xlmSACID)
	if want := "1744567"; got != want { // 17445678924432 / 1e7
		t.Errorf("XLM after first protocol-2 write = %q, want %q", got, want)
	}
}

// TestReflector_GoldenFixture_ProtocolOne folds only the 2025 ledgers. After
// the config lands with a single observed round, the deviation guard has no
// older price and the aggregator refuses to serve — the fold must refuse too.
// The second real round makes the guard satisfiable and prices resolve.
func TestReflector_GoldenFixture_ProtocolOne(t *testing.T) {
	t.Parallel()
	ledgers := loadReflectorFixture(t)
	adapter := newReflectorAdapter(t)

	// Through the last config-assembly ledger: one round observed, no older
	// price to validate max_dev against -> no price, exactly as on-chain.
	state := foldFixtureLedgers(t, adapter, fixedPoolPrior(), ledgersThrough(ledgers, 56569319))
	for _, asset := range []string{xlmSACID, usdcTokenID, eurcTokenID} {
		if got, _ := reflectorReservePrice(t, state, fixedPoolID, asset); got != "" {
			t.Errorf("asset %s priced %q with a single observed round; deviation guard must refuse", asset, got)
		}
	}

	// The second protocol-1 round (ledger 56569347) satisfies the guard.
	state = foldFixtureLedgers(t, adapter, fixedPoolPrior(), ledgersThrough(ledgers, 56569347))
	for _, tc := range []struct {
		asset string
		want  string
	}{
		{xlmSACID, "2335012"},     // 23350127418476 / 1e7
		{usdcTokenID, "10004502"}, // 100045020259116 / 1e7
		{eurcTokenID, "11332271"}, // 113322710193460 / 1e7
	} {
		if got, _ := reflectorReservePrice(t, state, fixedPoolID, tc.asset); got != tc.want {
			t.Errorf("asset %s: OraclePriceRaw = %q, want %q", tc.asset, got, tc.want)
		}
	}

	// The aggregator config decoded from the real instance writes.
	var agg contracts.OracleAggregatorState
	for _, a := range state.OracleAggregators {
		if a.ContractID == fixedAggregatorID {
			agg = a
		}
	}
	if agg.ContractID == "" {
		t.Fatal("Fixed aggregator config not decoded")
	}
	if agg.Decimals != 7 || agg.MaxAgeS != 900 || agg.BaseKey != "other:USD" {
		t.Errorf("aggregator config = {decimals %d, maxAge %d, base %q}, want {7, 900, other:USD}", agg.Decimals, agg.MaxAgeS, agg.BaseKey)
	}
	if len(agg.Feeds) != 1 || agg.Feeds[0].ContractID != cexFeedID || agg.Feeds[0].Decimals != 14 || agg.Feeds[0].ResolutionS != 300 {
		t.Errorf("aggregator feeds = %+v, want the CEX feed at 14 decimals / 300s", agg.Feeds)
	}
	wantAssets := map[string]struct {
		key    string
		maxDev int64
	}{
		xlmSACID:    {"other:XLM", 60},
		usdcTokenID: {"other:USDC", 20},
		eurcTokenID: {"other:EURC", 20},
	}
	if len(agg.Assets) != len(wantAssets) {
		t.Fatalf("aggregator assets = %d entries, want %d", len(agg.Assets), len(wantAssets))
	}
	for _, asset := range agg.Assets {
		want := wantAssets[asset.AssetID]
		if asset.FeedAssetKey != want.key || asset.MaxDev != want.maxDev {
			t.Errorf("asset %s = {%q, dev %d}, want {%q, dev %d}", asset.AssetID, asset.FeedAssetKey, asset.MaxDev, want.key, want.maxDev)
		}
	}
}

// TestReflector_Determinism folds the fixture twice and requires byte-identical
// output — the same run-twice property every other decode path holds.
func TestReflector_Determinism(t *testing.T) {
	t.Parallel()
	ledgers := loadReflectorFixture(t)

	fold := func() *bindings.LedgerState {
		return foldFixtureLedgers(t, newReflectorAdapter(t), fixedPoolPrior(), ledgers)
	}
	first, second := fold(), fold()
	if !reflect.DeepEqual(first, second) {
		t.Fatal("two folds of the same fixture diverged")
	}
	a, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	b, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(a) != string(b) {
		t.Fatal("two folds of the same fixture are not byte-identical")
	}
}

// TestReflector_StalenessMaxAge advances the close time past MaxAge with no new
// feed writes: prices must clear rather than serve stale, and must still serve
// one second inside the window.
func TestReflector_StalenessMaxAge(t *testing.T) {
	t.Parallel()
	ledgers := loadReflectorFixture(t)
	adapter := newReflectorAdapter(t)
	state := foldFixtureLedgers(t, adapter, fixedPoolPrior(), ledgers)

	const lastRoundS = 1782473700 // newest CEX round, seconds

	fresh, err := adapter.DecodeStateAt(state, nil, 63205720, time.Unix(lastRoundS+899, 0).UTC())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, _ := reflectorReservePrice(t, fresh, fixedPoolID, xlmSACID); got != "1744710" {
		t.Errorf("XLM one second inside MaxAge = %q, want 1744710", got)
	}

	stale, err := adapter.DecodeStateAt(state, nil, 63205721, time.Unix(lastRoundS+901, 0).UTC())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, asset := range []string{xlmSACID, usdcTokenID, eurcTokenID} {
		if got, _ := reflectorReservePrice(t, stale, fixedPoolID, asset); got != "" {
			t.Errorf("asset %s still priced %q beyond MaxAge; must clear", asset, got)
		}
	}
}

// TestReflector_MaxDevRejectsPerAsset appends a SYNTHETIC round on top of the
// real fixture (clearly synthetic: XLM jumped ~2x in 300s) and asserts the
// deviation guard clears exactly that asset while assets absent from the
// synthetic round keep resolving from the previous real round.
func TestReflector_MaxDevRejectsPerAsset(t *testing.T) {
	t.Parallel()
	ledgers := loadReflectorFixture(t)
	adapter := newReflectorAdapter(t)
	state := foldFixtureLedgers(t, adapter, fixedPoolPrior(), ledgers)

	// Synthetic protocol-2 round at the next 300s grid point carrying only XLM
	// (mask bit 13), at ~2x the previous real round's value.
	const syntheticRoundMs = 1782474000000
	mask := make([]byte, 32)
	mask[13/8] = 1 << (13 % 8)
	maskBytes := xdr.ScBytes(mask)
	round := stateChange(t, cexFeedID,
		u64Val(syntheticRoundMs),
		mapVal(t, map[string]xdr.ScVal{
			"mask":   {Type: xdr.ScValTypeScvBytes, Bytes: &maskBytes},
			"prices": vecVal(i128Val(34894201037734)), // ~2x XLM: rejected at max_dev 60
		}),
	)

	next, err := adapter.DecodeStateAt(state, []bindings.ContractDataChange{round}, 63205772, time.Unix(syntheticRoundMs/1000+29, 0).UTC())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, _ := reflectorReservePrice(t, next, fixedPoolID, xlmSACID); got != "" {
		t.Errorf("XLM priced %q through a >max_dev jump; guard must clear it", got)
	}
	// USDC/EURC are absent from the synthetic round: the walk steps back to the
	// previous real round and their guards still pass.
	if got, _ := reflectorReservePrice(t, next, fixedPoolID, usdcTokenID); got != "10011281" {
		t.Errorf("USDC = %q, want 10011281 (from the previous real round)", got)
	}
	if got, _ := reflectorReservePrice(t, next, fixedPoolID, eurcTokenID); got != "11419633" {
		t.Errorf("EURC = %q, want 11419633 (from the previous real round)", got)
	}
}

// TestReflector_YieldBloxShape covers the YieldBlox aggregator semantics the
// bronze archive cannot yet witness (its config writes fall in a not-yet-
// ingested stretch): address-keyed asset mapping into the DEX feed, the USDC
// token as base priced 1, and the two hardcoded BaseAssets priced 1. The
// aggregator instance here is SYNTHETIC, built to mirror its live on-chain
// instance storage; the DEX feed rounds it prices from are the real fixture
// entries.
func TestReflector_YieldBloxShape(t *testing.T) {
	t.Parallel()
	const ybxAggregatorID = "CD74A3C54EKUVEGUC6WNTUPOTHB624WFKXN3IYTFJGX3EHXDXHCYMXXR"

	ledgers := loadReflectorFixture(t)
	adapter, err := New(Config{AllowUnknownV2: true})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	adapter.RegisterContracts(yieldBloxPoolID, ybxAggregatorID)
	adapter.RegisterPriceFeeds(cexFeedID, dexFeedID)

	prior := &bindings.LedgerState{
		Pools: []contracts.PoolState{{
			ContractID:     yieldBloxPoolID,
			OracleContract: ybxAggregatorID,
			PoolStatus:     "active",
			Reserves: []contracts.ReserveState{
				{AssetID: xlmSACID, ReserveIndex: 0, ReserveIndexKnown: true},
				{AssetID: usdcTokenID, ReserveIndex: 1, ReserveIndexKnown: true},
				{AssetID: ybxBaseAsset1, ReserveIndex: 2, ReserveIndexKnown: true},
				{AssetID: ybxBaseAsset2, ReserveIndex: 3, ReserveIndexKnown: true},
			},
		}},
	}

	stellarAsset := func(id string) xdr.ScVal { return variantVal(t, "Stellar", addressVal(t, id)) }
	var wasm xdr.Hash
	wasm[31] = 7
	assetsEntries := xdr.ScMap{
		{Key: stellarAsset(xlmSACID), Val: mapVal(t, map[string]xdr.ScVal{
			"asset":        stellarAsset(xlmSACID),
			"max_dev":      u32Val(20),
			"oracle_index": u32Val(0),
		})},
	}
	assetsPtr := &assetsEntries
	storage := xdr.ScMap{
		{Key: symbolVal(t, "Admin"), Val: addressVal(t, ybxAggregatorID)},
		{Key: symbolVal(t, "Assets"), Val: xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &assetsPtr}},
		{Key: symbolVal(t, "Base"), Val: stellarAsset(usdcTokenID)},
		{Key: symbolVal(t, "BaseAssets"), Val: vecVal(stellarAsset(ybxBaseAsset1), stellarAsset(ybxBaseAsset2))},
		{Key: symbolVal(t, "Decimals"), Val: u32Val(7)},
		{Key: symbolVal(t, "MaxAge"), Val: u64Val(900)},
		{Key: symbolVal(t, "Oracles"), Val: vecVal(mapVal(t, map[string]xdr.ScVal{
			"address":    addressVal(t, dexFeedID),
			"decimals":   u32Val(14),
			"index":      u32Val(0),
			"resolution": u32Val(300),
		}))},
	}
	instance := xdr.ScVal{
		Type: xdr.ScValTypeScvContractInstance,
		Instance: &xdr.ScContractInstance{
			Executable: xdr.ContractExecutable{Type: xdr.ContractExecutableTypeContractExecutableWasm, WasmHash: &wasm},
			Storage:    &storage,
		},
	}

	state := prior
	for _, ledger := range ledgers {
		changes := ledger.Changes
		if ledger.LedgerSeq == 63205614 {
			// Inject the synthetic aggregator config alongside the first real
			// protocol-2 DEX write.
			changes = append([]bindings.ContractDataChange{stateChange(t, ybxAggregatorID, instanceKeyVal(), instance)}, changes...)
		}
		next, err := adapter.DecodeStateAt(state, changes, ledger.LedgerSeq, time.Unix(ledger.CloseTimeUnix, 0).UTC())
		if err != nil {
			t.Fatalf("decode ledger %d: %v", ledger.LedgerSeq, err)
		}
		state = next
	}

	// XLM prices in USDC from the real DEX round 1782473400000 (the DEX feed's
	// newest fixture round): 17473840523802 / 1e7. The deviation guard is
	// satisfied by the real previous round 1782473100000.
	if got, decimals := reflectorReservePrice(t, state, yieldBloxPoolID, xlmSACID); got != "1747384" || decimals != 7 {
		t.Errorf("XLM = %q at %d decimals, want 1747384 at 7", got, decimals)
	}
	// The base asset and the hardcoded BaseAssets are 1.0 at 7 decimals by
	// definition, with no feed lookup.
	for _, asset := range []string{usdcTokenID, ybxBaseAsset1, ybxBaseAsset2} {
		if got, _ := reflectorReservePrice(t, state, yieldBloxPoolID, asset); got != "10000000" {
			t.Errorf("asset %s = %q, want the constant 10000000", asset, got)
		}
	}
}

// TestReflector_ConfigRecordsRestartResume drives the ConfigRecords ->
// HydrateConfig seam: fold part of the fixture, persist the emitted records,
// hydrate a fresh state from them, and fold the remaining ledgers on top. The
// restarted fold must price identically to the uninterrupted one.
func TestReflector_ConfigRecordsRestartResume(t *testing.T) {
	t.Parallel()
	ledgers := loadReflectorFixture(t)
	adapter := newReflectorAdapter(t)

	// Leg 1: fold through the second-to-last ledger, collecting records like the
	// host does (latest per entity key wins).
	latest := map[string]bindings.ConfigRecord{}
	state := fixedPoolPrior()
	head := ledgers[:len(ledgers)-1]
	for _, ledger := range head {
		next, err := adapter.DecodeStateAt(state, ledger.Changes, ledger.LedgerSeq, time.Unix(ledger.CloseTimeUnix, 0).UTC())
		if err != nil {
			t.Fatalf("decode ledger %d: %v", ledger.LedgerSeq, err)
		}
		for _, rec := range adapter.ConfigRecords(next, ledger.Changes, ledger.LedgerSeq) {
			latest[rec.Kind+"|"+rec.EntityKey] = rec
		}
		state = next
	}

	// Restart: hydrate from records only.
	records := make([]bindings.ConfigRecord, 0, len(latest))
	for _, rec := range latest {
		records = append(records, rec)
	}
	hydrated, err := adapter.HydrateConfig(records)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if len(hydrated.OracleAggregators) == 0 {
		t.Fatal("hydrated state carries no aggregator config — a restarted mainnet fold would never price again")
	}
	if len(hydrated.PriceFeeds) == 0 {
		t.Fatal("hydrated state carries no feed state")
	}
	// The hydrated seed has no pools (pool config is hydrated from its own
	// records in production); re-seed the pool the same way leg 1 was seeded.
	hydrated.Pools = fixedPoolPrior().Pools

	// Leg 2: fold the final ledger on the hydrated seed.
	last := ledgers[len(ledgers)-1]
	restarted, err := adapter.DecodeStateAt(hydrated, last.Changes, last.LedgerSeq, time.Unix(last.CloseTimeUnix, 0).UTC())
	if err != nil {
		t.Fatalf("decode ledger %d: %v", last.LedgerSeq, err)
	}

	// The uninterrupted fold over the same ledgers.
	straight, err := adapter.DecodeStateAt(state, last.Changes, last.LedgerSeq, time.Unix(last.CloseTimeUnix, 0).UTC())
	if err != nil {
		t.Fatalf("decode ledger %d: %v", last.LedgerSeq, err)
	}

	for _, asset := range []string{xlmSACID, usdcTokenID, eurcTokenID} {
		wantPrice, wantDecimals := reflectorReservePrice(t, straight, fixedPoolID, asset)
		gotPrice, gotDecimals := reflectorReservePrice(t, restarted, fixedPoolID, asset)
		if gotPrice != wantPrice || gotDecimals != wantDecimals {
			t.Errorf("asset %s after restart = %q/%d, uninterrupted = %q/%d", asset, gotPrice, gotDecimals, wantPrice, wantDecimals)
		}
		if wantPrice == "" {
			t.Errorf("asset %s resolved no price in the uninterrupted fold — the comparison is vacuous", asset)
		}
	}
}
