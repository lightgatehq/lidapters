package blend

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/shopspring/decimal"
)

type mathFixture struct {
	Name   string                  `json:"name"`
	Input  bindings.TransformInput `json:"input"`
	Expect struct {
		Address                string `json:"address"`
		DepositedUSD           string `json:"deposited_usd"`
		BorrowedUSD            string `json:"borrowed_usd"`
		EffectiveCollateralUSD string `json:"effective_collateral_usd"`
		EffectiveLiabilityUSD  string `json:"effective_liability_usd"`
		HealthFactor           string `json:"health_factor"`
		BorrowLimitPct         string `json:"borrow_limit_pct"`
		BorrowCapUSD           string `json:"borrow_cap_usd"`
	} `json:"expect"`
}

func TestV2MathFixtures(t *testing.T) {
	t.Parallel()

	adapter, err := New(Config{
		V2WasmHashes: map[string]struct{}{
			"wasm-v2-main": {},
		},
	})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	fixtures := loadMathFixtures(t, "testdata/v2_math_fixtures.json")
	for _, fixture := range fixtures {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			output, err := adapter.Transform(fixture.Input)
			if err != nil {
				t.Fatalf("transform: %v", err)
			}
			summary := findSummary(output, fixture.Expect.Address)
			if summary == nil {
				t.Fatalf("summary not found for %s", fixture.Expect.Address)
			}
			assertDecimalEquals(t, fixture.Expect.DepositedUSD, summary.DepositedUSD, "deposited_usd")
			assertDecimalEquals(t, fixture.Expect.BorrowedUSD, summary.BorrowedUSD, "borrowed_usd")
			assertDecimalEquals(t, fixture.Expect.EffectiveCollateralUSD, summary.EffectiveCollateralUSD, "effective_collateral_usd")
			assertDecimalEquals(t, fixture.Expect.EffectiveLiabilityUSD, summary.EffectiveLiabilityUSD, "effective_liability_usd")
			assertDecimalEquals(t, fixture.Expect.HealthFactor, summary.HealthFactor, "health_factor")
			assertDecimalEquals(t, fixture.Expect.BorrowLimitPct, summary.BorrowLimitPct, "borrow_limit_pct")
			assertDecimalEquals(t, fixture.Expect.BorrowCapUSD, summary.BorrowCapUSD, "borrow_cap_usd")
		})
	}
}

func loadMathFixtures(t *testing.T, relPath string) []mathFixture {
	t.Helper()
	path := filepath.Join(".", relPath)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture file %s: %v", path, err)
	}
	fixtures := make([]mathFixture, 0)
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatalf("unmarshal fixture file %s: %v", path, err)
	}
	// Explicit close time fallback so tests are stable if fixture omits it.
	for i := range fixtures {
		if fixtures[i].Input.CloseTime.IsZero() {
			fixtures[i].Input.CloseTime = time.Unix(0, 0).UTC()
		}
	}
	return fixtures
}

func findSummary(output *bindings.TransformOutput, address string) *bindings.PositionSummary {
	for i := range output.Summaries {
		if output.Summaries[i].Address == address {
			return &output.Summaries[i]
		}
	}
	return nil
}

func assertDecimalEquals(t *testing.T, expected, actual, field string) {
	t.Helper()
	if expected == "" {
		if actual != "" {
			t.Fatalf("%s mismatch: expected empty, got %q", field, actual)
		}
		return
	}
	exp, err := decimal.NewFromString(expected)
	if err != nil {
		t.Fatalf("invalid expected decimal for %s: %v", field, err)
	}
	got, err := decimal.NewFromString(actual)
	if err != nil {
		t.Fatalf("invalid actual decimal for %s: %v", field, err)
	}
	if !exp.Equal(got) {
		t.Fatalf("%s mismatch: expected %s, got %s", field, exp.String(), got.String())
	}
}
