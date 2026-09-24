package blend

import (
	"fmt"

	"github.com/shopspring/decimal"
)

const (
	factorScale         = "10000000"
	rateModifierScaleV1 = "1000000000"
	rateModifierScaleV2 = "10000000"
)

var (
	decZero            = decimal.Zero
	decOne             = decimal.NewFromInt(1)
	factorScaleDecimal = decimal.RequireFromString(factorScale)
)

func init() {
	// Keep enough precision for chained USD and risk-ratio divisions.
	decimal.DivisionPrecision = 48
}

func mustParseDecimal(v string) (decimal.Decimal, error) {
	if v == "" {
		return decZero, nil
	}
	d, err := decimal.NewFromString(v)
	if err != nil {
		return decZero, fmt.Errorf("parse decimal %q: %w", v, err)
	}
	return d, nil
}

func parseDecimalOrZero(v string) decimal.Decimal {
	d, err := decimal.NewFromString(v)
	if err != nil {
		return decZero
	}
	return d
}

func normalizedRateModifier(raw string, scalar decimal.Decimal) (decimal.Decimal, error) {
	v, err := mustParseDecimal(raw)
	if err != nil {
		return decZero, err
	}
	if scalar.IsZero() {
		return decZero, nil
	}
	return v.Div(scalar), nil
}

func numString(d decimal.Decimal) string {
	return d.String()
}

func maxDecimal(a, b decimal.Decimal) decimal.Decimal {
	if a.GreaterThan(b) {
		return a
	}
	return b
}

func minDecimal(a, b decimal.Decimal) decimal.Decimal {
	if a.LessThan(b) {
		return a
	}
	return b
}

func floorDiv(n, d decimal.Decimal) decimal.Decimal {
	if d.IsZero() {
		return decZero
	}
	return n.Div(d).Floor()
}

func ceilDiv(n, d decimal.Decimal) decimal.Decimal {
	if d.IsZero() {
		return decZero
	}
	return n.Div(d).Ceil()
}

func fixedMulFloor(x, y, scalar decimal.Decimal) decimal.Decimal {
	return floorDiv(x.Mul(y), scalar)
}

func fixedMulCeil(x, y, scalar decimal.Decimal) decimal.Decimal {
	return ceilDiv(x.Mul(y), scalar)
}

func fixedDivCeil(x, y, scalar decimal.Decimal) decimal.Decimal {
	return ceilDiv(x.Mul(scalar), y)
}
