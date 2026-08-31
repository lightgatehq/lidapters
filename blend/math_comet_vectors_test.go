package blend

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/lightgatehq/lidapters/blend/contracts"
)

// The neutral V1-09 Comet valuation vectors (testdata/v1_09_comet_vectors.json,
// also embedded — see fixtures.go). Expected values come from a standalone
// integer oracle, not from this package; a floor->ceil, collapsed-stage,
// missing->zero, or hardcoded-price mutation in the valuation path must fail
// this test.
type cometVectorFile struct {
	Schema  string        `json:"schema"`
	Vectors []cometVector `json:"vectors"`
}

type cometVector struct {
	Name  string `json:"name"`
	Input struct {
		UserShares   string  `json:"user_shares"`
		PoolShares   string  `json:"pool_shares"`
		PoolTokens   string  `json:"pool_tokens"`
		LPSupply     *string `json:"lp_supply"`
		BLNDReserve  *string `json:"blnd_reserve"`
		USDCReserve  *string `json:"usdc_reserve"`
		BLNDPriceUSD *string `json:"blnd_price_usd"`
		USDCPriceUSD *string `json:"usdc_price_usd"`
	} `json:"input"`
	Expect struct {
		LPTokens      string  `json:"lp_tokens"`
		BLNDComponent *string `json:"blnd_component"`
		USDCComponent *string `json:"usdc_component"`
		USDValue      *string `json:"usd_value"`
		AbsentReason  string  `json:"absent_reason"`
	} `json:"expect"`
}

func loadCometVectors(t *testing.T) cometVectorFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "v1_09_comet_vectors.json"))
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var file cometVectorFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("decode vectors: %v", err)
	}
	if len(file.Vectors) == 0 {
		t.Fatal("no vectors")
	}
	return file
}

func orEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func TestCometValuationVectors(t *testing.T) {
	t.Parallel()

	file := loadCometVectors(t)
	for _, vector := range file.Vectors {
		vector := vector
		t.Run(vector.Name, func(t *testing.T) {
			t.Parallel()

			adapter, err := New(Config{V2WasmHashes: map[string]struct{}{"wasm-v2": {}}})
			if err != nil {
				t.Fatalf("new adapter: %v", err)
			}
			out, err := adapter.Transform(bindings.TransformInput{
				LedgerSeq: 7,
				CloseTime: time.Unix(70, 0).UTC(),
				State: &bindings.LedgerState{
					Pools: []contracts.PoolState{{
						ContractID: "CPOOL",
						WasmHash:   "wasm-v2",
					}},
					Backstops: []contracts.BackstopPosition{{
						Address:          "GWALLET",
						PoolContractID:   "CPOOL",
						UserSharesRaw:    vector.Input.UserShares,
						PoolSharesRaw:    vector.Input.PoolShares,
						PoolTokensRaw:    vector.Input.PoolTokens,
						LPTokenSupplyRaw: orEmpty(vector.Input.LPSupply),
						LPBLNDReserveRaw: orEmpty(vector.Input.BLNDReserve),
						LPUSDCReserveRaw: orEmpty(vector.Input.USDCReserve),
						BLNDDecimals:     7,
						USDCDecimals:     7,
						BLNDPriceUSD:     orEmpty(vector.Input.BLNDPriceUSD),
						USDCPriceUSD:     orEmpty(vector.Input.USDCPriceUSD),
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
				t.Fatal("expected a backstop position row")
			}

			if backstop.AssetAmount != vector.Expect.LPTokens {
				t.Fatalf("lp tokens: expected %s, got %s", vector.Expect.LPTokens, backstop.AssetAmount)
			}
			if got := backstop.USDValue; got != orEmpty(vector.Expect.USDValue) {
				t.Fatalf("usd value: expected %q, got %q", orEmpty(vector.Expect.USDValue), got)
			}
			if vector.Expect.BLNDComponent == nil {
				if _, ok := backstop.Metadata["blnd_component"]; ok {
					t.Fatalf("components must be absent, got blnd_component=%q", backstop.Metadata["blnd_component"])
				}
			} else {
				if got := backstop.Metadata["blnd_component"]; got != *vector.Expect.BLNDComponent {
					t.Fatalf("blnd component: expected %s, got %s", *vector.Expect.BLNDComponent, got)
				}
				if got := backstop.Metadata["usdc_component"]; got != *vector.Expect.USDCComponent {
					t.Fatalf("usdc component: expected %s, got %s", *vector.Expect.USDCComponent, got)
				}
			}

			switch vector.Expect.AbsentReason {
			case "":
				if backstop.Metadata["lp_denominator_zero"] == "true" || backstop.Metadata["lp_state_unavailable"] == "true" {
					t.Fatalf("unexpected absence marker: %+v", backstop.Metadata)
				}
			case "lp_denominator_zero":
				if backstop.Metadata["lp_denominator_zero"] != "true" {
					t.Fatalf("expected lp_denominator_zero marker, metadata %+v", backstop.Metadata)
				}
			case "lp_state_unavailable":
				if backstop.Metadata["lp_state_unavailable"] != "true" {
					t.Fatalf("expected lp_state_unavailable marker, metadata %+v", backstop.Metadata)
				}
			case "price_unavailable":
				if backstop.Metadata["price_unavailable"] != "true" {
					t.Fatalf("expected price_unavailable marker, metadata %+v", backstop.Metadata)
				}
			default:
				t.Fatalf("unknown absent_reason %q", vector.Expect.AbsentReason)
			}
		})
	}
}
