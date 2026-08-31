package aquarius

import (
	"fmt"
	"math/big"
	"time"

	"github.com/lightgatehq/lidapters/bindings"
)

var q96 = new(big.Int).Lsh(big.NewInt(1), 96)

// tickMagicLadder holds the Uniswap-v3 getSqrtRatioAtTick Q128.128 constants:
// entry i is sqrt(1.0001)^-(2^i) in Q128.128. The concentrated pool wasm's
// source is unfetchable (repo 404; SE-verified @ a22139a), so the ladder is
// pinned empirically: summing the per-position decomposition over all live
// positions of the pinned XLM/USDC pool reproduces the pool's own instance
// reserves to within unattributed global fee growth (see testdata/REGISTRY.md).
var tickMagicLadder = func() []*big.Int {
	hexes := []string{
		"fff97272373d413259a46990580e213a",
		"fff2e50f5f656932ef12357cf3c7fdcc",
		"ffe5caca7e10e4e61c3624eaa0941cd0",
		"ffcb9843d60f6159c9db58835c926644",
		"ff973b41fa98c081472e6896dfb254c0",
		"ff2ea16466c96a3843ec78b326b52861",
		"fe5dee046a99a2a811c461f1969c3053",
		"fcbe86c7900a88aedcffc83b479aa3a4",
		"f987a7253ac413176f2b074cf7815e54",
		"f3392b0822b70005940c7a398e4b70f3",
		"e7159475a2c29b7443b29c7fa6e889d9",
		"d097f3bdfd2022b8845ad8f792aa5825",
		"a9f746462d870fdf8a65dc1f90e061e5",
		"70d869a156d2a1b890bb3df62baf32f7",
		"31be135f97d08fd981231505542fcfa6",
		"9aa508b5b7a84e1c677de54f3e99bc9",
		"5d6af8dedb81196699c329225ee604",
		"2216e584f5fa1ea926041bedfe98",
		"48a170391f7dc42444e8fa2",
	}
	out := make([]*big.Int, len(hexes))
	for i, h := range hexes {
		n, ok := new(big.Int).SetString(h, 16)
		if !ok {
			panic("aquarius: bad tick ladder constant")
		}
		out[i] = n
	}
	return out
}()

var (
	tickOddMagic = mustHexBig("fffcb933bd6fad37aa2d162d1a594001")
	q128         = new(big.Int).Lsh(big.NewInt(1), 128)
	q256         = new(big.Int).Lsh(big.NewInt(1), 256)
)

func mustHexBig(h string) *big.Int {
	n, ok := new(big.Int).SetString(h, 16)
	if !ok {
		panic("aquarius: bad hex constant")
	}
	return n
}

const maxTick = 887272

// tickSqrtPriceX96 converts a tick to its Q64.96 square-root price — the
// bounds rangePrincipal consumes. Concentrated Position entries key on
// (owner, tick_lower, tick_upper); their sqrt-price bounds are never stored
// on-chain, so the fold derives them here. Anchor: tick -17652 yields
// 32778602836627082880087502758, which brackets the pinned pool's observed
// Slot0.sqrt_price_x96 = 32779403528916036142219842285 from below.
func tickSqrtPriceX96(tick int32) (string, error) {
	abs := uint32(tick)
	if tick < 0 {
		abs = uint32(-int64(tick))
	}
	if abs > maxTick {
		return "", fmt.Errorf("tick %d out of range", tick)
	}
	ratio := new(big.Int).Set(q128)
	if abs&1 != 0 {
		ratio.Set(tickOddMagic)
	}
	for i, magic := range tickMagicLadder {
		if abs&(1<<(uint(i)+1)) != 0 {
			ratio.Mul(ratio, magic)
			ratio.Rsh(ratio, 128)
		}
	}
	if tick > 0 {
		ratio.Div(q256, ratio)
	}
	// Q128.128 -> Q64.96, rounding up like the reference implementation.
	rem := new(big.Int)
	ratio.DivMod(ratio, new(big.Int).Lsh(big.NewInt(1), 32), rem)
	if rem.Sign() != 0 {
		ratio.Add(ratio, big.NewInt(1))
	}
	return ratio.String(), nil
}

func parseUint(s string) (*big.Int, error) {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok || n.Sign() < 0 {
		return nil, fmt.Errorf("invalid unsigned integer %q", s)
	}
	return n, nil
}
func mulDivFloor(a, b, d *big.Int) *big.Int { return new(big.Int).Quo(new(big.Int).Mul(a, b), d) }

// pendingReward reproduces the pool's get_user_reward getter from folded
// checkpoint state (classic pools): the contract checkpoints to_claim and the
// pool's accumulated total into UserRewardData on every user interaction, and
// accrues tps per second to the pool between interactions, capped at the
// emission expiry. Between the user's checkpoint and ledger close time T the
// user is owed working_balance * (accumulated(T) - pool_accumulated_user) /
// working_supply on top of the checkpointed to_claim. Verified against frozen
// mainnet checkpoints (205/223/233 raw AQUA stroops). All arithmetic is exact
// big.Int; when any checkpoint input is missing the checkpointed to_claim is
// returned unchanged (no fabricated accrual).
func pendingReward(pool bindings.AMMPoolState, pos bindings.AMMPositionState, closeTime time.Time) string {
	toClaim := pos.PendingRewardRaw
	if toClaim == "" {
		toClaim = "0"
	}
	wbRaw := pos.WorkingBalanceRaw
	if wbRaw == "" {
		wbRaw = pos.SharesRaw
	}
	if wbRaw == "" || wbRaw == "0" || pos.RewardPoolAccumulatedRaw == "" {
		return toClaim
	}
	if pool.RewardTpsRaw == "" || pool.RewardAccumulatedRaw == "" || pool.RewardLastTimeRaw == "" || pool.WorkingSupplyRaw == "" || pool.WorkingSupplyRaw == "0" {
		return toClaim
	}
	wb, err := parseUint(wbRaw)
	if err != nil {
		return toClaim
	}
	tps, err := parseUint(pool.RewardTpsRaw)
	if err != nil {
		return toClaim
	}
	accStored, err := parseUint(pool.RewardAccumulatedRaw)
	if err != nil {
		return toClaim
	}
	accUser, err := parseUint(pos.RewardPoolAccumulatedRaw)
	if err != nil {
		return toClaim
	}
	ws, err := parseUint(pool.WorkingSupplyRaw)
	if err != nil || ws.Sign() == 0 {
		return toClaim
	}
	lastTime, err := parseUint(pool.RewardLastTimeRaw)
	if err != nil {
		return toClaim
	}
	now := new(big.Int).SetInt64(closeTime.Unix())
	if pool.RewardExpiredAtRaw != "" {
		if exp, e := parseUint(pool.RewardExpiredAtRaw); e == nil && now.Cmp(exp) > 0 {
			now = exp
		}
	}
	dt := new(big.Int).Sub(now, lastTime)
	if dt.Sign() > 0 {
		accStored = new(big.Int).Add(accStored, new(big.Int).Mul(tps, dt))
	}
	deltaAcc := new(big.Int).Sub(accStored, accUser)
	if deltaAcc.Sign() <= 0 {
		return toClaim
	}
	claim, err := parseUint(toClaim)
	if err != nil {
		return toClaim
	}
	return new(big.Int).Add(claim, mulDivFloor(wb, deltaAcc, ws)).String()
}

func proRata(shares, reserve, total string) (string, error) {
	s, e := parseUint(shares)
	if e != nil {
		return "", e
	}
	r, e := parseUint(reserve)
	if e != nil {
		return "", e
	}
	t, e := parseUint(total)
	if e != nil {
		return "", e
	}
	if t.Sign() == 0 {
		return "", fmt.Errorf("zero total shares")
	}
	return mulDivFloor(s, r, t).String(), nil
}

// rangePrincipal applies burn/withdraw rounding (down) to Q96 square-root
// prices. Bounds are supplied by the audited tick-math decoder.
func rangePrincipal(liquidity, p, pa, pb string) (string, string, error) {
	L, e := parseUint(liquidity)
	if e != nil {
		return "", "", e
	}
	P, e := parseUint(p)
	if e != nil {
		return "", "", e
	}
	A, e := parseUint(pa)
	if e != nil {
		return "", "", e
	}
	B, e := parseUint(pb)
	if e != nil {
		return "", "", e
	}
	if A.Sign() <= 0 || A.Cmp(B) >= 0 {
		return "", "", fmt.Errorf("invalid sqrt-price bounds")
	}
	amount0 := func(x, y *big.Int) *big.Int {
		num := new(big.Int).Mul(new(big.Int).Lsh(new(big.Int).Set(L), 96), new(big.Int).Sub(y, x))
		return new(big.Int).Quo(new(big.Int).Quo(num, y), x)
	}
	amount1 := func(x, y *big.Int) *big.Int { return mulDivFloor(L, new(big.Int).Sub(y, x), q96) }
	if P.Cmp(A) <= 0 {
		return amount0(A, B).String(), "0", nil
	}
	if P.Cmp(B) < 0 {
		return amount0(P, B).String(), amount1(A, P).String(), nil
	}
	return "0", amount1(A, B).String(), nil
}
