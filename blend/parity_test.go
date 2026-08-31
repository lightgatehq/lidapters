package blend

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/lightgatehq/lidapters/blend/contracts"
	"github.com/shopspring/decimal"
)

type paritySnapshot struct {
	LedgerSeq                   int64  `json:"ledger_seq"`
	CloseTime                   string `json:"close_time"`
	WasmHash                    string `json:"wasm_hash"`
	Address                     string `json:"address"`
	PoolContract                string `json:"pool_contract"`
	AssetID                     string `json:"asset_id"`
	AssetDecimals               int32  `json:"asset_decimals"`
	OracleDecimals              int32  `json:"oracle_decimals"`
	BRateRaw                    string `json:"b_rate_raw"`
	DRateRaw                    string `json:"d_rate_raw"`
	BTokensRaw                  string `json:"b_tokens_raw"`
	DTokensRaw                  string `json:"d_tokens_raw"`
	OraclePriceRaw              string `json:"oracle_price_raw"`
	CFactorRaw                  string `json:"c_factor_raw"`
	LFactorRaw                  string `json:"l_factor_raw"`
	ExpectedSupplyAssetRaw      string `json:"expected_supply_asset_raw"`
	ExpectedBorrowAssetRaw      string `json:"expected_borrow_asset_raw"`
	ExpectedSupplyUSD           string `json:"expected_supply_usd"`
	ExpectedBorrowUSD           string `json:"expected_borrow_usd"`
	ExpectedEffectiveCollateral string `json:"expected_effective_collateral_usd"`
	ExpectedEffectiveLiability  string `json:"expected_effective_liability_usd"`
	ExpectedHealthFactor        string `json:"expected_health_factor"`
	ExpectedBorrowLimitPct      string `json:"expected_borrow_limit_pct"`
}

func TestRecordedV2ParitySnapshot(t *testing.T) {
	t.Parallel()

	snap := loadParitySnapshot(t, "testdata/v2_parity_snapshot.json")
	adapter, err := New(Config{
		V2WasmHashes: map[string]struct{}{
			snap.WasmHash: {},
		},
	})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	input := bindings.TransformInput{
		LedgerSeq: snap.LedgerSeq,
		State: &bindings.LedgerState{
			Pools: []contracts.PoolState{
				{
					ContractID: snap.PoolContract,
					WasmHash:   snap.WasmHash,
					Reserves: []contracts.ReserveState{
						{
							AssetID:         snap.AssetID,
							AssetDecimals:   snap.AssetDecimals,
							BRateRaw:        snap.BRateRaw,
							DRateRaw:        snap.DRateRaw,
							BSupplyRaw:      snap.BTokensRaw,
							DSupplyRaw:      snap.DTokensRaw,
							CFactorRaw:      snap.CFactorRaw,
							LFactorRaw:      snap.LFactorRaw,
							UtilTargetRaw:   "8000000",
							MaxUtilRaw:      "9500000",
							RBaseRaw:        "0",
							ROneRaw:         "0",
							RTwoRaw:         "0",
							RThreeRaw:       "0",
							RateModifierRaw: "1000000000",
							SupplyCapRaw:    "100000000000",
							OraclePriceRaw:  snap.OraclePriceRaw,
							OracleDecimals:  snap.OracleDecimals,
						},
					},
				},
			},
			Users: []contracts.UserReservePosition{
				{
					Address:        snap.Address,
					PoolContractID: snap.PoolContract,
					AssetID:        snap.AssetID,
					PositionType:   contracts.PositionTypeCollateral,
					BTokensRaw:     snap.BTokensRaw,
				},
				{
					Address:        snap.Address,
					PoolContractID: snap.PoolContract,
					AssetID:        snap.AssetID,
					PositionType:   contracts.PositionTypeLiability,
					DTokensRaw:     snap.DTokensRaw,
				},
			},
		},
	}

	out, err := adapter.Transform(input)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if len(out.Positions) < 2 {
		t.Fatalf("expected positions to be computed, got %d", len(out.Positions))
	}
	summary := findSummary(out, snap.Address)
	if summary == nil {
		t.Fatalf("summary missing for %s", snap.Address)
	}

	assertWithinTolerance(t, snap.ExpectedEffectiveCollateral, summary.EffectiveCollateralUSD, "0.000000000000000001", "effective_collateral_usd")
	assertWithinTolerance(t, snap.ExpectedEffectiveLiability, summary.EffectiveLiabilityUSD, "0.000000000000000001", "effective_liability_usd")
	assertWithinTolerance(t, snap.ExpectedHealthFactor, summary.HealthFactor, "0.000000000000000001", "health_factor")
	assertWithinTolerance(t, snap.ExpectedBorrowLimitPct, summary.BorrowLimitPct, "0.000000000000000001", "borrow_limit_pct")
}

func loadParitySnapshot(t *testing.T, relPath string) paritySnapshot {
	t.Helper()
	path := filepath.Join(".", relPath)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read parity file %s: %v", path, err)
	}
	var snap paritySnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatalf("decode parity file %s: %v", path, err)
	}
	return snap
}

func assertWithinTolerance(t *testing.T, expected, actual, tol, field string) {
	t.Helper()
	exp, err := decimal.NewFromString(expected)
	if err != nil {
		t.Fatalf("invalid expected decimal for %s: %v", field, err)
	}
	got, err := decimal.NewFromString(actual)
	if err != nil {
		t.Fatalf("invalid actual decimal for %s: %v", field, err)
	}
	tolerance, err := decimal.NewFromString(tol)
	if err != nil {
		t.Fatalf("invalid tolerance for %s: %v", field, err)
	}
	diff := exp.Sub(got).Abs()
	if diff.GreaterThan(tolerance) {
		t.Fatalf("%s mismatch: expected %s got %s diff %s > tol %s", field, exp, got, diff, tolerance)
	}
}
