package blend

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/lightgatehq/lidapters/blend/contracts"
)

// reserveScenarioState builds a minimal pool+reserve LedgerState, optionally
// carrying decoded asset metadata for the reserve's asset — the shared fixture
// for the two tests below.
func reserveScenarioState(poolID, assetID string, assets []contracts.AssetMetadata) *bindings.LedgerState {
	return &bindings.LedgerState{
		Pools: []contracts.PoolState{{
			ContractID: poolID,
			Reserves: []contracts.ReserveState{{
				AssetID:         assetID,
				AssetDecimals:   7,
				BRateRaw:        "1000000000000",
				DRateRaw:        "1000000000000",
				BSupplyRaw:      "1000",
				DSupplyRaw:      "0",
				CFactorRaw:      "8000000",
				LFactorRaw:      "10000000",
				UtilTargetRaw:   "5000000",
				MaxUtilRaw:      "9500000",
				RBaseRaw:        "0",
				ROneRaw:         "0",
				RTwoRaw:         "0",
				RThreeRaw:       "0",
				RateModifierRaw: "10000000",
			}},
		}},
		Assets: assets,
	}
}

func depositEvent(t *testing.T, poolID, assetID, wallet string) bindings.RawEventEnvelope {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"type":   "deposit",
		"amount": "500",
		"wallet": wallet,
		"asset":  assetID,
	})
	if err != nil {
		t.Fatalf("marshal fixture event: %v", err)
	}
	return bindings.RawEventEnvelope{
		LedgerSeq:  200,
		TxHash:     "tx-asset-metadata",
		EventIndex: 0,
		ContractID: poolID,
		Topic:      "blend deposit",
		RawEvent:   raw,
		CloseTime:  time.Unix(2000, 0).UTC(),
	}
}

// TestTransform_ReserveAndActivityCarryDecodedAssetSymbol proves a decoded
// asset's symbol/name reach Reserve.Metadata and a decoded asset's symbol
// reaches Activity.AssetSymbol — the threading lidapters#8 adds on top of the
// SAC/SEP-41 decode.
func TestTransform_ReserveAndActivityCarryDecodedAssetSymbol(t *testing.T) {
	t.Parallel()

	adapter, err := New(Config{AllowUnknownV2: true})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	poolID := validContractString(t, 120)
	assetID := validContractString(t, 121)
	wallet := validAccountString(t, 122)

	out, err := adapter.Transform(bindings.TransformInput{
		LedgerSeq: 200,
		CloseTime: time.Unix(2000, 0).UTC(),
		Events:    []bindings.RawEventEnvelope{depositEvent(t, poolID, assetID, wallet)},
		State: reserveScenarioState(poolID, assetID, []contracts.AssetMetadata{
			{ContractID: assetID, Symbol: "USDC", Name: "USDC:GISSUER", Decimals: 7},
		}),
	})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if len(out.Quarantine) != 0 {
		t.Fatalf("expected no quarantine, got %+v", out.Quarantine)
	}
	if len(out.Reserves) != 1 {
		t.Fatalf("expected one reserve, got %d", len(out.Reserves))
	}
	if got := out.Reserves[0].Metadata["asset_symbol"]; got != "USDC" {
		t.Fatalf("reserve asset_symbol: got %q want %q", got, "USDC")
	}
	if got := out.Reserves[0].Metadata["asset_name"]; got != "USDC:GISSUER" {
		t.Fatalf("reserve asset_name: got %q want %q", got, "USDC:GISSUER")
	}
	if len(out.Activities) != 1 {
		t.Fatalf("expected one activity, got %d", len(out.Activities))
	}
	if got := out.Activities[0].AssetSymbol; got != "USDC" {
		t.Fatalf("activity AssetSymbol: got %q want %q", got, "USDC")
	}
}

// TestTransform_UndecodedAssetLeavesSymbolEmpty proves the absence of decoded
// metadata leaves Reserve.Metadata's asset_symbol/asset_name and
// Activity.AssetSymbol empty rather than falling back to a guess — the relay
// store's firstNonEmpty(asset_symbol, contract_id) fallback is what turns this
// into the address display, not lidapters.
func TestTransform_UndecodedAssetLeavesSymbolEmpty(t *testing.T) {
	t.Parallel()

	adapter, err := New(Config{AllowUnknownV2: true})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	poolID := validContractString(t, 123)
	assetID := validContractString(t, 124)
	wallet := validAccountString(t, 125)

	out, err := adapter.Transform(bindings.TransformInput{
		LedgerSeq: 200,
		CloseTime: time.Unix(2000, 0).UTC(),
		Events:    []bindings.RawEventEnvelope{depositEvent(t, poolID, assetID, wallet)},
		State:     reserveScenarioState(poolID, assetID, nil),
	})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if len(out.Reserves) != 1 {
		t.Fatalf("expected one reserve, got %d", len(out.Reserves))
	}
	if got := out.Reserves[0].Metadata["asset_symbol"]; got != "" {
		t.Fatalf("reserve asset_symbol: got %q want empty", got)
	}
	if got := out.Reserves[0].Metadata["asset_name"]; got != "" {
		t.Fatalf("reserve asset_name: got %q want empty", got)
	}
	if len(out.Activities) != 1 {
		t.Fatalf("expected one activity, got %d", len(out.Activities))
	}
	if got := out.Activities[0].AssetSymbol; got != "" {
		t.Fatalf("activity AssetSymbol: got %q want empty", got)
	}
}
