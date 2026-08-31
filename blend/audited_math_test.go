package blend

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/lightgatehq/lidapters/blend/contracts"
)

func TestV2RateModifierScaleAndUtilizationClamp(t *testing.T) {
	t.Parallel()

	adapter, err := New(Config{V2WasmHashes: map[string]struct{}{"wasm-v2": {}}})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	out, err := adapter.Transform(bindings.TransformInput{
		LedgerSeq: 1,
		CloseTime: time.Unix(0, 0).UTC(),
		State: &bindings.LedgerState{
			Pools: []contracts.PoolState{{
				ContractID:       "CPOOL",
				WasmHash:         "wasm-v2",
				BackstopTakeRate: "0",
				Reserves: []contracts.ReserveState{{
					AssetID:         "CASSET",
					AssetDecimals:   7,
					BRateRaw:        "1000000000000",
					DRateRaw:        "1000000000000",
					BSupplyRaw:      "1000000000",
					DSupplyRaw:      "2000000000",
					CFactorRaw:      "8000000",
					LFactorRaw:      "9000000",
					UtilTargetRaw:   "5000000",
					MaxUtilRaw:      "9500000",
					RBaseRaw:        "0",
					ROneRaw:         "1000000",
					RTwoRaw:         "0",
					RThreeRaw:       "0",
					RateModifierRaw: "10000000",
					SupplyCapRaw:    "100000000000",
					OraclePriceRaw:  "100000000",
					OracleDecimals:  8,
				}},
			}},
		},
	})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if len(out.Reserves) != 1 {
		t.Fatalf("expected 1 reserve, got %d", len(out.Reserves))
	}
	reserve := out.Reserves[0]
	if reserve.Utilization != "1" {
		t.Fatalf("expected utilization clamp at 1, got %s", reserve.Utilization)
	}
	if reserve.BorrowAPY != "0.1" {
		t.Fatalf("expected borrow APY 0.1 with v2 rate modifier scalar, got %s", reserve.BorrowAPY)
	}
}

// TestTransform_ReserveEmissions is the relay#26 fold gate at the transform
// layer: a reserve with an active supply-side emission config produces exactly
// one ReserveEmission row (never a fabricated borrow-side row for the absent
// config), carrying the raw eps/expiration and an unavailable ("") APY since no
// emitted-token price feed exists yet.
func TestTransform_ReserveEmissions(t *testing.T) {
	t.Parallel()

	adapter, err := New(Config{V2WasmHashes: map[string]struct{}{"wasm-v2": {}}})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	out, err := adapter.Transform(bindings.TransformInput{
		LedgerSeq: 1,
		CloseTime: time.Unix(0, 0).UTC(),
		State: &bindings.LedgerState{
			Pools: []contracts.PoolState{{
				ContractID:       "CPOOL",
				WasmHash:         "wasm-v2",
				BackstopTakeRate: "0",
				Reserves: []contracts.ReserveState{{
					AssetID:                 "CASSET",
					AssetDecimals:           7,
					BRateRaw:                "1000000000000",
					DRateRaw:                "1000000000000",
					BSupplyRaw:              "1000000000",
					DSupplyRaw:              "2000000000",
					CFactorRaw:              "8000000",
					LFactorRaw:              "9000000",
					UtilTargetRaw:           "5000000",
					MaxUtilRaw:              "9500000",
					RateModifierRaw:         "10000000",
					SupplyCapRaw:            "100000000000",
					OraclePriceRaw:          "100000000",
					OracleDecimals:          8,
					SupplyEmisEPSRaw:        "1000000",
					SupplyEmisExpirationRaw: "1800000000",
					// BorrowEmis* left empty: no active borrow-side config.
				}},
			}},
		},
	})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if len(out.ReserveEmissions) != 1 {
		t.Fatalf("expected exactly 1 reserve emission row (supply only), got %d", len(out.ReserveEmissions))
	}
	emission := out.ReserveEmissions[0]
	if emission.Side != "supply" {
		t.Fatalf("expected side=supply, got %q", emission.Side)
	}
	if emission.EPSRaw != "1000000" {
		t.Fatalf("expected EPSRaw=1000000, got %q", emission.EPSRaw)
	}
	if emission.Expiration.Unix() != 1_800_000_000 {
		t.Fatalf("expected expiration unix=1800000000, got %d", emission.Expiration.Unix())
	}
	if emission.APY != "" {
		t.Fatalf("expected unavailable (empty) APY with no price feed, got %q", emission.APY)
	}
}

// TestTransform_ReserveEmissions_AbsentWhenNoConfig guards the "never
// fabricated" acceptance criterion: a reserve with no EmisConfig on either
// side must produce zero ReserveEmission rows, not rows with a zero eps.
func TestTransform_ReserveEmissions_AbsentWhenNoConfig(t *testing.T) {
	t.Parallel()

	adapter, err := New(Config{V2WasmHashes: map[string]struct{}{"wasm-v2": {}}})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	out, err := adapter.Transform(bindings.TransformInput{
		LedgerSeq: 1,
		CloseTime: time.Unix(0, 0).UTC(),
		State: &bindings.LedgerState{
			Pools: []contracts.PoolState{{
				ContractID:       "CPOOL",
				WasmHash:         "wasm-v2",
				BackstopTakeRate: "0",
				Reserves: []contracts.ReserveState{{
					AssetID:         "CASSET",
					AssetDecimals:   7,
					BRateRaw:        "1000000000000",
					DRateRaw:        "1000000000000",
					BSupplyRaw:      "1000000000",
					DSupplyRaw:      "2000000000",
					CFactorRaw:      "8000000",
					LFactorRaw:      "9000000",
					UtilTargetRaw:   "5000000",
					MaxUtilRaw:      "9500000",
					RateModifierRaw: "10000000",
					SupplyCapRaw:    "100000000000",
					OraclePriceRaw:  "100000000",
					OracleDecimals:  8,
				}},
			}},
		},
	})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if len(out.ReserveEmissions) != 0 {
		t.Fatalf("expected 0 reserve emission rows when no EmisConfig exists, got %d", len(out.ReserveEmissions))
	}
}

func TestPoolIsolatedWorstSummarySemantics(t *testing.T) {
	t.Parallel()

	adapter, err := New(Config{V2WasmHashes: map[string]struct{}{"wasm-v2": {}}})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	address := "GUSER"
	out, err := adapter.Transform(bindings.TransformInput{
		LedgerSeq: 2,
		CloseTime: time.Unix(10, 0).UTC(),
		State: &bindings.LedgerState{
			Pools: []contracts.PoolState{
				{
					ContractID:       "CPOOLA",
					WasmHash:         "wasm-v2",
					BackstopTakeRate: "0",
					Reserves: []contracts.ReserveState{{
						AssetID:         "CASSETA",
						AssetDecimals:   7,
						BRateRaw:        "1000000000000",
						DRateRaw:        "1000000000000",
						BSupplyRaw:      "10000000000",
						DSupplyRaw:      "0",
						CFactorRaw:      "8000000",
						LFactorRaw:      "9000000",
						UtilTargetRaw:   "8000000",
						MaxUtilRaw:      "9500000",
						RBaseRaw:        "0",
						ROneRaw:         "0",
						RTwoRaw:         "0",
						RThreeRaw:       "0",
						RateModifierRaw: "10000000",
						SupplyCapRaw:    "100000000000",
						OraclePriceRaw:  "100000000",
						OracleDecimals:  8,
					}},
				},
				{
					ContractID:       "CPOOLB",
					WasmHash:         "wasm-v2",
					BackstopTakeRate: "0",
					Reserves: []contracts.ReserveState{{
						AssetID:         "CASSETB",
						AssetDecimals:   7,
						BRateRaw:        "1000000000000",
						DRateRaw:        "1000000000000",
						BSupplyRaw:      "10000000000",
						DSupplyRaw:      "1000000000",
						CFactorRaw:      "8000000",
						LFactorRaw:      "9000000",
						UtilTargetRaw:   "8000000",
						MaxUtilRaw:      "9500000",
						RBaseRaw:        "0",
						ROneRaw:         "0",
						RTwoRaw:         "0",
						RThreeRaw:       "0",
						RateModifierRaw: "10000000",
						SupplyCapRaw:    "100000000000",
						OraclePriceRaw:  "100000000",
						OracleDecimals:  8,
					}},
				},
			},
			Users: []contracts.UserReservePosition{
				{Address: address, PoolContractID: "CPOOLA", AssetID: "CASSETA", PositionType: contracts.PositionTypeCollateral, BTokensRaw: "10000000000"},
				{Address: address, PoolContractID: "CPOOLB", AssetID: "CASSETB", PositionType: contracts.PositionTypeLiability, DTokensRaw: "1000000000"},
			},
		},
	})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	summary := findSummary(out, address)
	if summary == nil {
		t.Fatalf("summary missing")
	}
	if summary.HealthFactor != "0" {
		t.Fatalf("expected worst-pool health factor 0, got %s", summary.HealthFactor)
	}
	if summary.EffectiveCollateralUSD != "0" {
		t.Fatalf("expected effective_collateral_usd from worst pool, got %s", summary.EffectiveCollateralUSD)
	}
	if summary.Metadata["risk_semantics"] != "blend_pool_isolated" {
		t.Fatalf("expected blend_pool_isolated semantics")
	}
	if summary.Metadata["summary_health_factor_semantics"] != "worst_pool" {
		t.Fatalf("expected worst_pool semantics")
	}
	if summary.Metadata["pool_breakdown"] == "" {
		t.Fatalf("expected pool_breakdown metadata")
	}
	var breakdown map[string]any
	if err := json.Unmarshal([]byte(summary.Metadata["pool_breakdown"]), &breakdown); err != nil {
		t.Fatalf("pool_breakdown should be valid JSON: %v", err)
	}
	if _, ok := breakdown["CPOOLA"]; !ok {
		t.Fatalf("expected pool breakdown for CPOOLA")
	}
	if _, ok := breakdown["CPOOLB"]; !ok {
		t.Fatalf("expected pool breakdown for CPOOLB")
	}
}

func TestBackstopShareAndTokenAccounting(t *testing.T) {
	t.Parallel()

	adapter, err := New(Config{V2WasmHashes: map[string]struct{}{"wasm-v2": {}}})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	unlockAt := time.Unix(2000, 0).UTC()
	out, err := adapter.Transform(bindings.TransformInput{
		LedgerSeq: 3,
		CloseTime: time.Unix(20, 0).UTC(),
		State: &bindings.LedgerState{
			Pools: []contracts.PoolState{{
				ContractID: "CPOOL",
				WasmHash:   "wasm-v2",
			}},
			Backstops: []contracts.BackstopPosition{{
				Address:              "GBACKSTOP",
				PoolContractID:       "CPOOL",
				UserSharesRaw:        "300",
				PoolSharesRaw:        "8000",
				PoolTokensRaw:        "10000",
				Q4W:                  []contracts.Q4WEntry{{SharesRaw: "100", UnlockAt: unlockAt}},
				LPTokenSupplyRaw:     "10000",
				LPBLNDReserveRaw:     "20000",
				LPUSDCReserveRaw:     "30000",
				BLNDDecimals:         7,
				USDCDecimals:         7,
				BLNDPriceUSD:         "2",
				USDCPriceUSD:         "1",
				BackstopInterestAPY:  "0.1",
				BackstopEmissionsAPY: "",
			}},
		},
	})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	var backstop *bindings.Position
	for i := range out.Positions {
		if out.Positions[i].PositionType == contracts.PositionTypeBackstop {
			backstop = &out.Positions[i]
			break
		}
	}
	if backstop == nil {
		t.Fatalf("expected backstop position")
	}
	if backstop.ShareAmount != "400" {
		t.Fatalf("expected total shares 400, got %s", backstop.ShareAmount)
	}
	if backstop.AssetAmount != "500" {
		t.Fatalf("expected total LP tokens 500, got %s", backstop.AssetAmount)
	}
	if backstop.Metadata["active_lp_tokens"] != "375" {
		t.Fatalf("expected active LP tokens 375, got %s", backstop.Metadata["active_lp_tokens"])
	}
	if backstop.Metadata["queued_lp_tokens"] != "125" {
		t.Fatalf("expected queued LP tokens 125, got %s", backstop.Metadata["queued_lp_tokens"])
	}
	if backstop.APY != "" {
		t.Fatalf("expected NULL APY when emissions APR is missing, got %s", backstop.APY)
	}
	if backstop.Metadata["apr_partial"] != "true" {
		t.Fatalf("expected apr_partial metadata")
	}
	if len(backstop.Q4WEntries) != 1 || !backstop.Q4WEntries[0].UnlockAt.Equal(unlockAt) {
		t.Fatalf("expected q4w entries preserved")
	}
}

// TestBackstopPoolTotalEmittedOnReserves covers the pool-level backstop total
// (relay.lightgate.xyz#25 / orion.lightgate.xyz#37): a per-pool aggregate,
// distinct from the per-user bindings.Position rows TestBackstopShareAndTokenAccounting
// covers above. It must emit from PoolState alone, with zero backstop users —
// the whole point of carrying these totals on PoolState instead of only on
// BackstopPosition.
func TestBackstopPoolTotalEmittedOnReserves(t *testing.T) {
	t.Parallel()

	adapter, err := New(Config{V2WasmHashes: map[string]struct{}{"wasm-v2": {}}})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	out, err := adapter.Transform(bindings.TransformInput{
		LedgerSeq: 5,
		CloseTime: time.Unix(40, 0).UTC(),
		State: &bindings.LedgerState{
			Pools: []contracts.PoolState{{
				ContractID:           "CPOOL",
				WasmHash:             "wasm-v2",
				BackstopContract:     "CBACKSTOP",
				BackstopSharesRaw:    "8000",
				BackstopTokensRaw:    "10000",
				BackstopQ4WSharesRaw: "800",
			}},
			// Deliberately no Backstops (no individual depositors decoded yet) —
			// the pool-level total must not depend on user rows existing.
		},
	})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	if len(out.Backstops) != 1 {
		t.Fatalf("expected one pool-level backstop total, got %d", len(out.Backstops))
	}
	bt := out.Backstops[0]
	if bt.ContractID != "CPOOL" {
		t.Fatalf("expected pool contract CPOOL, got %s", bt.ContractID)
	}
	if bt.BackstopContract != "CBACKSTOP" {
		t.Fatalf("expected backstop contract CBACKSTOP, got %s", bt.BackstopContract)
	}
	if bt.SharesRaw != "8000" {
		t.Fatalf("expected total shares 8000, got %s", bt.SharesRaw)
	}
	if bt.LPTokensRaw != "10000" {
		t.Fatalf("expected total LP tokens 10000, got %s", bt.LPTokensRaw)
	}
	if bt.Q4WSharesRaw != "800" {
		t.Fatalf("expected q4w shares 800, got %s", bt.Q4WSharesRaw)
	}
	if bt.Q4WPct != "0.1" {
		t.Fatalf("expected q4w_pct fraction 0.1, got %s", bt.Q4WPct)
	}
	if bt.USDValue != "" {
		t.Fatalf("expected NULL usd_value (LP pricing unavailable), got %s", bt.USDValue)
	}
}

// TestBackstopPoolTotalAbsentWithoutBackstopRef asserts a pool that has not
// wired a backstop contract yet emits no backstop total row (never a
// fabricated all-empty row).
func TestBackstopPoolTotalAbsentWithoutBackstopRef(t *testing.T) {
	t.Parallel()

	adapter, err := New(Config{V2WasmHashes: map[string]struct{}{"wasm-v2": {}}})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	out, err := adapter.Transform(bindings.TransformInput{
		LedgerSeq: 6,
		CloseTime: time.Unix(50, 0).UTC(),
		State: &bindings.LedgerState{
			Pools: []contracts.PoolState{{
				ContractID: "CPOOL",
				WasmHash:   "wasm-v2",
			}},
		},
	})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if len(out.Backstops) != 0 {
		t.Fatalf("expected no backstop total without a backstop ref, got %d", len(out.Backstops))
	}
}

func TestReservePositionAPRMaterializesOnlyWhenEmissionsKnown(t *testing.T) {
	t.Parallel()

	adapter, err := New(Config{V2WasmHashes: map[string]struct{}{"wasm-v2": {}}})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	out, err := adapter.Transform(bindings.TransformInput{
		LedgerSeq: 4,
		CloseTime: time.Unix(30, 0).UTC(),
		State: &bindings.LedgerState{
			Pools: []contracts.PoolState{{
				ContractID:       "CPOOL",
				WasmHash:         "wasm-v2",
				BackstopTakeRate: "0",
				Reserves: []contracts.ReserveState{{
					AssetID:            "CASSET",
					AssetDecimals:      7,
					BRateRaw:           "1000000000000",
					DRateRaw:           "1000000000000",
					BSupplyRaw:         "100000000",
					DSupplyRaw:         "10000000",
					CFactorRaw:         "8000000",
					LFactorRaw:         "10000000",
					UtilTargetRaw:      "5000000",
					MaxUtilRaw:         "9500000",
					RBaseRaw:           "0",
					ROneRaw:            "1000000",
					RTwoRaw:            "0",
					RThreeRaw:          "0",
					RateModifierRaw:    "10000000",
					SupplyCapRaw:       "100000000000",
					OraclePriceRaw:     "100000000",
					OracleDecimals:     8,
					SupplyEmissionsAPR: "0.003",
					BorrowEmissionsAPR: "0.005",
				}},
			}},
			Users: []contracts.UserReservePosition{
				{Address: "GAPR", PoolContractID: "CPOOL", AssetID: "CASSET", PositionType: contracts.PositionTypeSupply, BTokensRaw: "10000000"},
				{Address: "GAPR", PoolContractID: "CPOOL", AssetID: "CASSET", PositionType: contracts.PositionTypeLiability, DTokensRaw: "10000000"},
			},
		},
	})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	supply := findPosition(out, contracts.PositionTypeSupply)
	if supply == nil {
		t.Fatalf("expected supply position")
	}
	if supply.APY != "0.005" {
		t.Fatalf("expected supply net APR 0.005, got %s", supply.APY)
	}
	if supply.Metadata["net_supply_apr"] != "0.005" {
		t.Fatalf("expected net_supply_apr metadata")
	}

	liability := findPosition(out, contracts.PositionTypeLiability)
	if liability == nil {
		t.Fatalf("expected liability position")
	}
	if liability.APY != "0.015" {
		t.Fatalf("expected liability net APR 0.015, got %s", liability.APY)
	}
	if liability.Metadata["net_borrow_apr"] != "0.015" {
		t.Fatalf("expected net_borrow_apr metadata")
	}
}

func TestReservePositionAPRMissingEmissionsStaysPartial(t *testing.T) {
	t.Parallel()

	adapter, err := New(Config{V2WasmHashes: map[string]struct{}{"wasm-v2": {}}})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	out, err := adapter.Transform(bindings.TransformInput{
		LedgerSeq: 5,
		CloseTime: time.Unix(40, 0).UTC(),
		State: &bindings.LedgerState{
			Pools: []contracts.PoolState{{
				ContractID:       "CPOOL",
				WasmHash:         "wasm-v2",
				BackstopTakeRate: "0",
				Reserves: []contracts.ReserveState{{
					AssetID:         "CASSET",
					AssetDecimals:   7,
					BRateRaw:        "1000000000000",
					DRateRaw:        "1000000000000",
					BSupplyRaw:      "100000000",
					DSupplyRaw:      "10000000",
					CFactorRaw:      "8000000",
					LFactorRaw:      "10000000",
					UtilTargetRaw:   "5000000",
					MaxUtilRaw:      "9500000",
					RBaseRaw:        "0",
					ROneRaw:         "1000000",
					RTwoRaw:         "0",
					RThreeRaw:       "0",
					RateModifierRaw: "10000000",
					SupplyCapRaw:    "100000000000",
					OraclePriceRaw:  "100000000",
					OracleDecimals:  8,
				}},
			}},
			Users: []contracts.UserReservePosition{
				{Address: "GPARTIAL", PoolContractID: "CPOOL", AssetID: "CASSET", PositionType: contracts.PositionTypeSupply, BTokensRaw: "10000000"},
			},
		},
	})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	supply := findPosition(out, contracts.PositionTypeSupply)
	if supply == nil {
		t.Fatalf("expected supply position")
	}
	if supply.APY != "" {
		t.Fatalf("expected NULL APY when emissions APR is missing, got %s", supply.APY)
	}
	if supply.Metadata["apr_partial"] != "true" || supply.Metadata["emissions_apr_unavailable"] != "true" {
		t.Fatalf("expected partial APR metadata")
	}
}

func TestReservePositionEmissionsSurfaceWhenBaseAPRInvalid(t *testing.T) {
	t.Parallel()

	adapter, err := New(Config{V2WasmHashes: map[string]struct{}{"wasm-v2": {}}})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	// UtilTargetRaw == 0 with non-zero utilization makes the borrow (and hence
	// supply) base APR invalid, so apr stays partial; but the raw emissions APRs
	// parse and must be surfaced independently into metadata.
	out, err := adapter.Transform(bindings.TransformInput{
		LedgerSeq: 6,
		CloseTime: time.Unix(50, 0).UTC(),
		State: &bindings.LedgerState{
			Pools: []contracts.PoolState{{
				ContractID:       "CPOOL",
				WasmHash:         "wasm-v2",
				BackstopTakeRate: "0",
				Reserves: []contracts.ReserveState{{
					AssetID:            "CASSET",
					AssetDecimals:      7,
					BRateRaw:           "1000000000000",
					DRateRaw:           "1000000000000",
					BSupplyRaw:         "100000000",
					DSupplyRaw:         "10000000",
					CFactorRaw:         "8000000",
					LFactorRaw:         "10000000",
					UtilTargetRaw:      "0",
					MaxUtilRaw:         "9500000",
					RBaseRaw:           "0",
					ROneRaw:            "1000000",
					RTwoRaw:            "0",
					RThreeRaw:          "0",
					RateModifierRaw:    "10000000",
					SupplyCapRaw:       "100000000000",
					OraclePriceRaw:     "100000000",
					OracleDecimals:     8,
					SupplyEmissionsAPR: "0.003",
					BorrowEmissionsAPR: "0.005",
				}},
			}},
			Users: []contracts.UserReservePosition{
				{Address: "GEMIT", PoolContractID: "CPOOL", AssetID: "CASSET", PositionType: contracts.PositionTypeSupply, BTokensRaw: "10000000"},
				{Address: "GEMIT", PoolContractID: "CPOOL", AssetID: "CASSET", PositionType: contracts.PositionTypeLiability, DTokensRaw: "10000000"},
			},
		},
	})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	supply := findPosition(out, contracts.PositionTypeSupply)
	if supply == nil {
		t.Fatalf("expected supply position")
	}
	if supply.APY != "" {
		t.Fatalf("expected NULL APY when base APR invalid, got %s", supply.APY)
	}
	if supply.Metadata["apr_partial"] != "true" {
		t.Fatalf("expected apr_partial=true on supply, got %q", supply.Metadata["apr_partial"])
	}
	if supply.Metadata["supply_emissions_apr"] != "0.003" {
		t.Fatalf("expected supply_emissions_apr surfaced as 0.003, got %q", supply.Metadata["supply_emissions_apr"])
	}
	if supply.Metadata["net_supply_apr"] != "" {
		t.Fatalf("expected no net_supply_apr when base APR invalid, got %q", supply.Metadata["net_supply_apr"])
	}

	liability := findPosition(out, contracts.PositionTypeLiability)
	if liability == nil {
		t.Fatalf("expected liability position")
	}
	if liability.APY != "" {
		t.Fatalf("expected NULL APY when base APR invalid, got %s", liability.APY)
	}
	if liability.Metadata["apr_partial"] != "true" {
		t.Fatalf("expected apr_partial=true on liability, got %q", liability.Metadata["apr_partial"])
	}
	if liability.Metadata["borrow_emissions_apr"] != "0.005" {
		t.Fatalf("expected borrow_emissions_apr surfaced as 0.005, got %q", liability.Metadata["borrow_emissions_apr"])
	}
	if liability.Metadata["net_borrow_apr"] != "" {
		t.Fatalf("expected no net_borrow_apr when base APR invalid, got %q", liability.Metadata["net_borrow_apr"])
	}
}

// TestActivityUSDValuedAtReserveLedgerPrice pins the activity valuation seam:
// an asset-bearing activity is valued at the folded oracle price its reserve
// carries at that ledger (units × price), from in-state data only. Stale
// event-metadata price stamps (the never-produced event_ledger_usd_price
// contract this replaced) are ignored.
func TestActivityUSDValuedAtReserveLedgerPrice(t *testing.T) {
	t.Parallel()

	adapter, err := New(Config{V2WasmHashes: map[string]struct{}{"wasm-v2": {}}})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	walletID := validAccountString(t, 60)
	poolID := validContractString(t, 61)
	assetID := validContractString(t, 62)
	raw, _ := json.Marshal(map[string]any{
		"type":   "supply",
		"amount": "10000000",
		"wallet": walletID,
		"asset":  assetID,
	})

	out, err := adapter.Transform(bindings.TransformInput{
		LedgerSeq: 6,
		CloseTime: time.Unix(50, 0).UTC(),
		Events: []bindings.RawEventEnvelope{{
			LedgerSeq:  6,
			TxHash:     "tx-activity-price",
			EventIndex: 0,
			ContractID: poolID,
			Topic:      "blend supply",
			RawEvent:   raw,
			CloseTime:  time.Unix(50, 0).UTC(),
			Metadata: map[string]string{
				// Dead stamp contract: must not override the reserve price.
				"event_ledger_usd_price": "2",
				"asset_decimals":         "7",
			},
		}},
		State: &bindings.LedgerState{
			Pools: []contracts.PoolState{{
				ContractID: poolID,
				WasmHash:   "wasm-v2",
				Reserves: []contracts.ReserveState{{
					AssetID:         assetID,
					AssetDecimals:   7,
					BRateRaw:        "1000000000000",
					DRateRaw:        "1000000000000",
					BSupplyRaw:      "0",
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
					OraclePriceRaw:  "500000000",
					OracleDecimals:  8,
				}},
			}},
		},
	})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if len(out.Activities) != 1 {
		t.Fatalf("expected one activity, got %d", len(out.Activities))
	}
	// 10000000 raw / 10^7 decimals = 1 unit; 500000000 / 10^8 = 5 USD.
	if out.Activities[0].USDValue != "5" {
		t.Fatalf("expected reserve-priced USD value 5, got %s", out.Activities[0].USDValue)
	}
	if out.Activities[0].Metadata["usd_value_source"] != "reserve_ledger_price" {
		t.Fatalf("expected reserve ledger price source metadata, got %q", out.Activities[0].Metadata["usd_value_source"])
	}
	if out.Activities[0].Metadata["event_price_unavailable"] != "" {
		t.Fatalf("unexpected event_price_unavailable on priced activity")
	}
}

// TestActivityUSDWithoutFoldedPriceStaysNull pins the unavailability contract:
// an activity whose asset has no folded oracle price — reserve present but
// price missing, or no reserve at all (e.g. a reward token) — keeps a NULL
// usd_value with the explicit marker, never a fabricated zero.
func TestActivityUSDWithoutFoldedPriceStaysNull(t *testing.T) {
	t.Parallel()

	adapter, err := New(Config{V2WasmHashes: map[string]struct{}{"wasm-v2": {}}})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	walletID := validAccountString(t, 63)
	poolID := validContractString(t, 64)
	reserveAssetID := validContractString(t, 65)
	nonReserveAssetID := validContractString(t, 66)
	rawReserveAsset, _ := json.Marshal(map[string]any{
		"type":   "supply",
		"amount": "10000000",
		"wallet": walletID,
		"asset":  reserveAssetID,
	})
	rawNonReserveAsset, _ := json.Marshal(map[string]any{
		"type":   "supply",
		"amount": "10000000",
		"wallet": walletID,
		"asset":  nonReserveAssetID,
	})

	out, err := adapter.Transform(bindings.TransformInput{
		LedgerSeq: 7,
		CloseTime: time.Unix(60, 0).UTC(),
		Events: []bindings.RawEventEnvelope{
			{
				LedgerSeq:  7,
				TxHash:     "tx-activity-no-price",
				EventIndex: 0,
				ContractID: poolID,
				Topic:      "blend supply",
				RawEvent:   rawReserveAsset,
				CloseTime:  time.Unix(60, 0).UTC(),
			},
			{
				LedgerSeq:  7,
				TxHash:     "tx-activity-no-reserve",
				EventIndex: 1,
				ContractID: poolID,
				Topic:      "blend supply",
				RawEvent:   rawNonReserveAsset,
				CloseTime:  time.Unix(60, 0).UTC(),
			},
		},
		State: &bindings.LedgerState{
			Pools: []contracts.PoolState{{
				ContractID: poolID,
				WasmHash:   "wasm-v2",
				Reserves: []contracts.ReserveState{{
					AssetID:         reserveAssetID,
					AssetDecimals:   7,
					BRateRaw:        "1000000000000",
					DRateRaw:        "1000000000000",
					BSupplyRaw:      "0",
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
					// No OraclePriceRaw: the reserve has folded data but no price.
					OracleDecimals: 8,
				}},
			}},
		},
	})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if len(out.Activities) != 2 {
		t.Fatalf("expected two activities, got %d", len(out.Activities))
	}
	for _, activity := range out.Activities {
		if activity.USDValue == "0" {
			t.Fatalf("fabricated zero USD value for %s", activity.TxHash)
		}
		if activity.USDValue != "" {
			t.Fatalf("expected NULL USD value without folded price for %s, got %s", activity.TxHash, activity.USDValue)
		}
		if activity.Metadata["event_price_unavailable"] != "true" {
			t.Fatalf("expected event_price_unavailable marker for %s", activity.TxHash)
		}
		if activity.Metadata["usd_value_source"] != "" {
			t.Fatalf("unexpected usd_value_source without price for %s", activity.TxHash)
		}
	}
}

func TestLiquidationScenariosArePoolScoped(t *testing.T) {
	t.Parallel()

	adapter, err := New(Config{V2WasmHashes: map[string]struct{}{"wasm-v2": {}}})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	out, err := adapter.Transform(bindings.TransformInput{
		LedgerSeq: 8,
		CloseTime: time.Unix(70, 0).UTC(),
		State: &bindings.LedgerState{
			Pools: []contracts.PoolState{{
				ContractID:       "CPOOL",
				WasmHash:         "wasm-v2",
				BackstopTakeRate: "0",
				Reserves: []contracts.ReserveState{{
					AssetID:         "CASSET",
					AssetDecimals:   7,
					BRateRaw:        "1000000000000",
					DRateRaw:        "1000000000000",
					BSupplyRaw:      "1000000000",
					DSupplyRaw:      "500000000",
					CFactorRaw:      "8000000",
					LFactorRaw:      "10000000",
					UtilTargetRaw:   "5000000",
					MaxUtilRaw:      "9500000",
					RBaseRaw:        "0",
					ROneRaw:         "0",
					RTwoRaw:         "0",
					RThreeRaw:       "0",
					RateModifierRaw: "10000000",
					SupplyCapRaw:    "100000000000",
					OraclePriceRaw:  "100000000",
					OracleDecimals:  8,
				}},
			}},
			Users: []contracts.UserReservePosition{
				{Address: "GLIQ", PoolContractID: "CPOOL", AssetID: "CASSET", PositionType: contracts.PositionTypeCollateral, BTokensRaw: "1000000000"},
				{Address: "GLIQ", PoolContractID: "CPOOL", AssetID: "CASSET", PositionType: contracts.PositionTypeLiability, DTokensRaw: "500000000"},
			},
		},
	})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	summary := findSummary(out, "GLIQ")
	if summary == nil {
		t.Fatalf("expected summary")
	}
	// With a single collateral asset the canonical liquidation_price is the
	// price at which health falls to 1 (current price / health factor =
	// 1 / 1.6), which matches this pool's own CASSET liquidation scenario below.
	if summary.LiquidationPrice != "0.625" {
		t.Fatalf("expected single-collateral liquidation_price 0.625, got %s", summary.LiquidationPrice)
	}
	breakdown, ok := summary.StructuredMetadata["pool_breakdown"].(map[string]poolBreakdownEntry)
	if !ok {
		t.Fatalf("expected structured pool breakdown")
	}
	scenario := breakdown["CPOOL"].LiquidationPriceScenarios["CASSET"]
	if scenario != "0.625" {
		t.Fatalf("expected CASSET liquidation scenario 0.625, got %s", scenario)
	}
}

func findPosition(output *bindings.TransformOutput, positionType contracts.PositionType) *bindings.Position {
	for i := range output.Positions {
		if output.Positions[i].PositionType == positionType {
			return &output.Positions[i]
		}
	}
	return nil
}
