package blend

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/lightgatehq/lidapters/bindings"
)

// newOracleCarryAdapter builds an adapter that owns the frozen layout's oracle so
// its price contract_data reaches the decoder, matching what the registry does
// once a pool's oracle is registered.
func newOracleCarryAdapter(t *testing.T, l oracleLayout) *Adapter {
	t.Helper()
	adapter, err := New(Config{AllowUnknownV2: true})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	adapter.RegisterContracts(l.OracleContract)
	return adapter
}

// oraclePriceOnlyChange builds a single oracle price contract_data change for one
// asset: the asset's stored price key (a u128 keyed by index) with a fresh raw
// i128 value. It is a price-only entry — no instance, no pool config — exactly
// the shape a set_price ledger writes after the oracle was deployed.
func oraclePriceOnlyChange(t *testing.T, l oracleLayout, code string, rawValue int64) bindings.ContractDataChange {
	t.Helper()
	return stateChange(t, l.OracleContract, mustDecodeScVal(t, codeKeyXDR(t, l, code)), i128Val(rawValue))
}

func reservePrice(t *testing.T, state *bindings.LedgerState, l oracleLayout, code string) string {
	t.Helper()
	return findReserve(t, state, l.PoolContract, assetIDByCode(l, code)).OraclePriceRaw
}

// TestDecodeState_OraclePriceCarriesAcrossPriceOnlyLedgers is the Cause-B
// regression: the oracle instance (asset->index map + decimals) is written once
// at deploy, and a price entry only appears in the ledger it changes. Before the
// fix the index map was rebuilt empty on every ledger after the deploy, so a
// price-only ledger mapped nothing — repriced assets were dropped (stale) and the
// rest surfaced empty. This folds three ledgers where the instance is seen ONLY
// at the deploy ledger (the "floor after deploy / instance never re-seen" case)
// and asserts each price-only ledger still resolves: the repriced asset takes its
// new price and the silent assets carry their last price forward.
func TestDecodeState_OraclePriceCarriesAcrossPriceOnlyLedgers(t *testing.T) {
	t.Parallel()

	layout := loadOracleLayout(t)
	adapter := newOracleCarryAdapter(t, layout)

	// Ledger N — the deploy ledger: oracle instance + the initial price map. Every
	// reserve resolves to its frozen price.
	priorN, err := adapter.DecodeState(nil, oracleSceneChanges(t, layout), layout.LedgerSeq)
	if err != nil {
		t.Fatalf("decode ledger N: %v", err)
	}
	for _, asset := range layout.Assets {
		if got := reservePrice(t, priorN, layout, asset.Code); got != asset.OraclePriceRaw {
			t.Fatalf("ledger N %s price: got %q want %q", asset.Code, got, asset.OraclePriceRaw)
		}
	}

	// Ledger N+1 — price-only, NO instance: wBTC reprices, the others are silent.
	const newWBTC = int64(900_000_000_000)
	stateN1, err := adapter.DecodeState(priorN, []bindings.ContractDataChange{
		oraclePriceOnlyChange(t, layout, "wBTC", newWBTC),
	}, layout.LedgerSeq+1)
	if err != nil {
		t.Fatalf("decode ledger N+1: %v", err)
	}
	// The repriced asset maps on a price-only ledger — the core Cause-B fix. Before
	// the fix this stayed at the ledger-N value (the new price was dropped).
	if got, want := reservePrice(t, stateN1, layout, "wBTC"), "900000000000"; got != want {
		t.Fatalf("ledger N+1 wBTC reprice: got %q want %q (price-only ledger did not map the new price)", got, want)
	}
	if got := reservePrice(t, stateN1, layout, "wBTC"); got == valueOf(t, layout, "wBTC") {
		t.Fatalf("ledger N+1 wBTC still at its ledger-N price %q — the reprice was dropped", got)
	}
	// The silent assets carry their last price forward (not lost to empty).
	for _, code := range []string{"USDC", "XLM", "wETH"} {
		if got, want := reservePrice(t, stateN1, layout, code), valueOf(t, layout, code); got != want {
			t.Fatalf("ledger N+1 %s carry-forward: got %q want %q", code, got, want)
		}
	}

	// Ledger N+2 — price-only again, instance STILL never re-seen: XLM reprices.
	const newXLM = int64(5_000_000)
	stateN2, err := adapter.DecodeState(stateN1, []bindings.ContractDataChange{
		oraclePriceOnlyChange(t, layout, "XLM", newXLM),
	}, layout.LedgerSeq+2)
	if err != nil {
		t.Fatalf("decode ledger N+2: %v", err)
	}
	if got, want := reservePrice(t, stateN2, layout, "XLM"), "5000000"; got != want {
		t.Fatalf("ledger N+2 XLM reprice: got %q want %q", got, want)
	}
	// wBTC's N+1 reprice survives a further price-only ledger; USDC/wETH unchanged.
	if got, want := reservePrice(t, stateN2, layout, "wBTC"), "900000000000"; got != want {
		t.Fatalf("ledger N+2 wBTC carry-forward: got %q want %q", got, want)
	}
	for _, code := range []string{"USDC", "wETH"} {
		if got, want := reservePrice(t, stateN2, layout, code), valueOf(t, layout, code); got != want {
			t.Fatalf("ledger N+2 %s carry-forward: got %q want %q", code, got, want)
		}
	}

	// The carried oracle state must not leak map-iteration order: folding the same
	// price-only ledger twice off the same prior is byte-identical.
	again, err := adapter.DecodeState(priorN, []bindings.ContractDataChange{
		oraclePriceOnlyChange(t, layout, "wBTC", newWBTC),
	}, layout.LedgerSeq+1)
	if err != nil {
		t.Fatalf("decode ledger N+1 again: %v", err)
	}
	b1, _ := json.Marshal(stateN1)
	b2, _ := json.Marshal(again)
	if !bytes.Equal(b1, b2) {
		t.Fatalf("price-only fold not byte-identical across two runs")
	}
}

// TestDecodeState_OraclePriceEvictionClearsReserve proves the carried price is
// cleared when the oracle's price entry is evicted/TTL-lapses: the reserve must
// not keep serving the last-seen price. Before the fix the carried oracle was
// absent on the eviction ledger, so the delete no-op'd and the reserve kept its
// stale price.
func TestDecodeState_OraclePriceEvictionClearsReserve(t *testing.T) {
	t.Parallel()

	layout := loadOracleLayout(t)
	adapter := newOracleCarryAdapter(t, layout)

	priorN, err := adapter.DecodeState(nil, oracleSceneChanges(t, layout), layout.LedgerSeq)
	if err != nil {
		t.Fatalf("decode ledger N: %v", err)
	}
	if reservePrice(t, priorN, layout, "wBTC") == "" {
		t.Fatal("expected wBTC priced at ledger N")
	}

	// Ledger N+1 — evict the wBTC price entry (not live). The reserve clears.
	evict := stateChange(t, layout.OracleContract,
		mustDecodeScVal(t, codeKeyXDR(t, layout, "wBTC")), i128Val(1), withLive(false))
	stateN1, err := adapter.DecodeState(priorN, []bindings.ContractDataChange{evict}, layout.LedgerSeq+1)
	if err != nil {
		t.Fatalf("decode ledger N+1: %v", err)
	}
	if got := reservePrice(t, stateN1, layout, "wBTC"); got != "" {
		t.Fatalf("evicted wBTC price not cleared: got %q want empty", got)
	}
	// A sibling whose price was not evicted is unaffected.
	if reservePrice(t, stateN1, layout, "USDC") == "" {
		t.Fatal("USDC price should be unaffected by the wBTC eviction")
	}
}

// valueOf returns an asset's frozen raw price from the layout.
func valueOf(t *testing.T, l oracleLayout, code string) string {
	t.Helper()
	for _, a := range l.Assets {
		if a.Code == code {
			return a.OraclePriceRaw
		}
	}
	t.Fatalf("asset %s not in layout", code)
	return ""
}
