package aquarius

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// diagMissingTickBounds marks a concentrated Position entry whose key lacked
// decodable (tick_lower, tick_upper) bounds: the fold refuses to key it as a
// guessed (0, 0) range and surfaces the refusal instead (absent is not zero).
const diagMissingTickBounds = "aquarius_position_missing_tick_bounds"

// DecodeState is a pure delta fold. JSON values are supported for audited
// fixtures; production entries are decoded from ScVal XDR maps/vectors.
func (a *Adapter) DecodeState(prior *bindings.LedgerState, changes []bindings.ContractDataChange, ledgerSeq int64) (*bindings.LedgerState, error) {
	next := cloneState(prior)
	a.diagnostics = nil
	pools := map[string]bindings.AMMPoolState{}
	for _, p := range next.AMMPools {
		pools[p.ContractID] = p
	}
	positions := map[string]bindings.AMMPositionState{}
	for _, p := range next.AMMPositions {
		positions[positionKey(p)] = p
	}
	a.applyPoolSeeds(pools, positions)
	for _, c := range changes {
		if !a.OwnsContract(c.ContractID) {
			continue
		}
		if c.ValueXDR == nil || !c.Live || (c.LiveUntilLedgerSeq != nil && *c.LiveUntilLedgerSeq < uint32(ledgerSeq)) {
			a.applyEntryRemoval(c, pools, positions)
			continue
		}
		key, _ := decodeVal(c.KeyXDR)
		val, ok := decodeVal(*c.ValueXDR)
		if !ok {
			continue
		}
		name := strings.ToLower(symbolOrFirst(key))
		if poolID, isShare := a.shareTokens[c.ContractID]; isShare {
			// LP position state rides Balance writes on the pool's share
			// token; the balance folds as a position of the owning pool.
			if name == "balance" {
				if owner := addrInKey(key); owner != "" {
					if shares := firstUint(val); shares != "" {
						upsertClassicShares(positions, poolID, owner, shares)
					}
				}
			}
			continue
		}
		if _, isAsset := a.assets[c.ContractID]; isAsset {
			if m, ok := assetMetadata(c.ContractID, key, val); ok {
				upsertAsset(&next.AMMAssets, m)
			}
			continue
		}
		p := pools[c.ContractID]
		p.Protocol = a.cfg.Protocol
		p.ContractID = c.ContractID
		if instance, isInstance := val.GetInstance(); isInstance {
			if hash, ok := instance.Executable.GetWasmHash(); ok {
				p.WasmHash = xdr.Hash(hash).HexString()
			}
			var initialA, futureA string
			if instance.Storage != nil {
				for _, entry := range *instance.Storage {
					switch strings.ToLower(symbolOrFirst(entry.Key)) {
					case "pool_type", "type":
						p.PoolType = strings.ToLower(symbolOrFirst(entry.Val))
					case "router":
						p.RouterContract = addr(entry.Val)
					case "tokena", "token0":
						upsertToken(&p.Tokens, 0, addr(entry.Val), "")
					case "tokenb", "token1":
						upsertToken(&p.Tokens, 1, addr(entry.Val), "")
					case "reservea", "reserve0":
						upsertToken(&p.Tokens, 0, "", firstUint(entry.Val))
					case "reserveb", "reserve1":
						upsertToken(&p.Tokens, 1, "", firstUint(entry.Val))
					case "tickspacing":
						fmt.Sscan(firstUint(entry.Val), &p.TickSpacing)
					case "liquidity":
						// Concentrated instance key: the pool's active
						// (in-range) liquidity.
						p.ActiveLiquidityRaw = firstUint(entry.Val)
					case "tokenshare":
						if id := addr(entry.Val); id != "" {
							a.shareTokens[id] = c.ContractID
						}
					case "tokens":
						p.Tokens = decodeReserves(p.Tokens, entry.Val)
					case "reserves":
						p.Tokens = decodeReserves(p.Tokens, entry.Val)
					case "totalshares", "total_shares":
						p.TotalSharesRaw = firstUint(entry.Val)
					case "feefraction", "fee":
						p.FeeFractionRaw = firstUint(entry.Val)
					case "protocolfeefraction":
						p.ProtocolFeeFractionRaw = firstUint(entry.Val)
					case "poolrewardconfig":
						f := fields(entry.Val)
						p.RewardTpsRaw = firstUint(f["tps"])
						p.RewardExpiredAtRaw = firstUint(f["expired_at"])
					case "poolrewarddata":
						f := fields(entry.Val)
						p.RewardAccumulatedRaw = firstUint(f["accumulated"])
						p.RewardLastTimeRaw = firstUint(f["last_time"])
					case "workingsupply":
						p.WorkingSupplyRaw = firstUint(entry.Val)
					case "rewardtoken":
						p.RewardTokenID = addr(entry.Val)
					case "a":
						p.AmplificationRaw = firstUint(entry.Val)
					case "initiala":
						initialA = firstUint(entry.Val)
					case "futurea":
						futureA = firstUint(entry.Val)
					case "feegrowthglobal0x128":
						// Concentrated instance key: the pool-lifetime fee
						// accumulator per unit of liquidity, u256 in X128
						// fixed point.
						p.FeeGrowthGlobal0X128 = firstUint(entry.Val)
					case "feegrowthglobal1x128":
						p.FeeGrowthGlobal1X128 = firstUint(entry.Val)
					case "slot0":
						decodeSlot0(&p, entry.Val)
					}
				}
			}
			if initialA != "" && futureA != "" {
				// Stable amplification rides an InitialA/FutureA ramp. The
				// pool has ONE settled amplification only when the ramp
				// endpoints agree; a mid-ramp pool has no single value —
				// absent is not zero, so the field empties.
				if initialA == futureA {
					p.AmplificationRaw = initialA
				} else {
					p.AmplificationRaw = ""
				}
			}
		}
		switch name {
		case "instance", "pool", "config":
			decodePoolInstance(&p, val)
		case "reserves", "reserve":
			p.Tokens = decodeReserves(p.Tokens, val)
		case "totalshares", "total_shares", "shares":
			p.TotalSharesRaw = firstUint(val)
		case "slot0", "pool_state":
			decodeSlot0(&p, val)
		case "active_liquidity":
			p.ActiveLiquidityRaw = firstUint(val)
		case "workingbalance":
			// The WorkingBalance entry value is the checkpointed reward
			// weight (ICE boost scales it within [0.4x, 1.0x] of the
			// deposit) — it is NOT the share balance and must never
			// overwrite SharesRaw.
			if owner := addrInKey(key); owner != "" {
				if weight := firstUint(val); weight != "" {
					k := positionKey(bindings.AMMPositionState{PoolContractID: c.ContractID, Address: owner})
					current, exists := positions[k]
					if !exists {
						current = bindings.AMMPositionState{Address: owner, PoolContractID: c.ContractID}
					}
					current.WorkingBalanceRaw = weight
					positions[k] = current
				}
			}
		case "position", "positions", "balance":
			pos, ok := decodePosition(c.ContractID, key, val)
			if !ok {
				if symbolOrFirst(key) == "Position" && pos.Address != "" {
					// A range Position key whose (tick_lower, tick_upper)
					// bounds did not decode must never fold as a guessed
					// (0, 0) range — absent is not zero. Refuse the entry
					// and surface the refusal.
					a.diagnostics = append(a.diagnostics, bindings.DecodeDiagnostic{
						Code:           diagMissingTickBounds,
						LedgerSeq:      ledgerSeq,
						PoolContractID: c.ContractID,
						Address:        pos.Address,
					})
				}
				break
			}
			current := positions[positionKey(pos)]
			if current.Address == "" {
				current = pos
			} else {
				current.SharesRaw = pos.SharesRaw
				current.LiquidityRaw = pos.LiquidityRaw
				current.SqrtPriceLowerX96 = pos.SqrtPriceLowerX96
				current.SqrtPriceUpperX96 = pos.SqrtPriceUpperX96
				current.TickLower = pos.TickLower
				current.TickUpper = pos.TickUpper
				current.PendingFee0Raw = pos.PendingFee0Raw
				current.PendingFee1Raw = pos.PendingFee1Raw
			}
			if (current.SharesRaw != "" && current.SharesRaw != "0") ||
				(current.LiquidityRaw != "" && current.LiquidityRaw != "0") {
				current.HadShares = true
			}
			positions[positionKey(current)] = current
		case "userrewarddata":
			if pos, ok := decodeRewardPosition(c.ContractID, key, val); ok {
				current := positions[positionKey(pos)]
				if current.Address == "" {
					current = pos
				}
				current.PendingRewardRaw = pos.PendingRewardRaw
				current.RewardPoolAccumulatedRaw = pos.RewardPoolAccumulatedRaw
				positions[positionKey(current)] = current
			}
		default:
			if user := addr(key); user != "" {
				if shares := firstUint(val); shares != "" {
					upsertClassicShares(positions, c.ContractID, user, shares)
				}
			}
		}
		if p.WasmHash != "" {
			typ, known := a.cfg.PoolWasmHashes[strings.ToLower(p.WasmHash)]
			if known {
				if p.PoolType == "" {
					p.PoolType = typ
				}
			} else if !a.cfg.AllowUnknownWasm {
				continue
			}
		}
		if p.PoolType == "" && len(p.Tokens) == 2 && p.Tokens[0].ReserveRaw != "" && p.Tokens[1].ReserveRaw != "" {
			p.PoolType = "volatile"
		}
		if p.PoolType != "" || len(p.Tokens) > 0 {
			pools[c.ContractID] = p
		}
	}
	next.AMMPools = next.AMMPools[:0]
	for _, p := range pools {
		next.AMMPools = append(next.AMMPools, p)
	}
	sort.Slice(next.AMMPools, func(i, j int) bool { return next.AMMPools[i].ContractID < next.AMMPools[j].ContractID })
	next.AMMPositions = next.AMMPositions[:0]
	for _, p := range positions {
		next.AMMPositions = append(next.AMMPositions, p)
	}
	sort.Slice(next.AMMPositions, func(i, j int) bool { return positionKey(next.AMMPositions[i]) < positionKey(next.AMMPositions[j]) })
	return next, nil
}

// applyPoolSeeds folds each configured pool seed into the working maps as a
// gap-fill floor: a pool/position absent from state (its instance entry
// predates the folded window) is inserted from the seed, and individual empty
// fields on a present pool are filled from the seed. Values the fold already
// observed from the chain are never overridden. Runs before each ledger's
// change loop, so chain writes in-window always supersede the seed.
func (a *Adapter) applyPoolSeeds(pools map[string]bindings.AMMPoolState, positions map[string]bindings.AMMPositionState) {
	for _, seed := range a.cfg.PoolSeeds {
		if strings.TrimSpace(seed.ContractID) == "" {
			continue
		}
		p, ok := pools[seed.ContractID]
		if !ok {
			p = bindings.AMMPoolState{Protocol: a.cfg.Protocol, ContractID: seed.ContractID}
		}
		if p.Protocol == "" {
			p.Protocol = a.cfg.Protocol
		}
		if p.RouterContract == "" {
			p.RouterContract = seed.RouterContract
		}
		if p.PoolHash == "" {
			p.PoolHash = seed.PoolHash
		}
		if p.PoolType == "" {
			p.PoolType = seed.PoolType
		}
		for i, id := range seed.Tokens {
			if id == "" {
				continue
			}
			upsertToken(&p.Tokens, i, id, "")
		}
		for i, r := range seed.ReservesRaw {
			if r == "" {
				continue
			}
			if i < len(p.Tokens) && p.Tokens[i].ReserveRaw == "" {
				upsertToken(&p.Tokens, i, "", r)
			}
		}
		if p.TotalSharesRaw == "" {
			p.TotalSharesRaw = seed.TotalSharesRaw
		}
		if p.FeeFractionRaw == "" {
			p.FeeFractionRaw = seed.FeeFractionRaw
		}
		if p.ProtocolFeeFractionRaw == "" {
			p.ProtocolFeeFractionRaw = seed.ProtocolFeeFractionRaw
		}
		pools[seed.ContractID] = p
		for _, sp := range seed.Positions {
			if strings.TrimSpace(sp.Address) == "" {
				continue
			}
			pos := bindings.AMMPositionState{Address: sp.Address, PoolContractID: seed.ContractID}
			key := positionKey(pos)
			current, ok := positions[key]
			if !ok {
				current = pos
			}
			if current.SharesRaw == "" {
				current.SharesRaw = sp.SharesRaw
			}
			if current.PendingRewardRaw == "" {
				current.PendingRewardRaw = sp.PendingRewardRaw
			}
			positions[key] = current
		}
	}
}

// applyEntryRemoval folds an entry deletion/eviction/expiry. Only an
// instance-level removal tears the whole pool down; a removed per-user entry
// closes exactly that user's leg. A Balance/Position entry is deleted on full
// exit, so its position zeroes (HadShares survives, letting Transform emit
// the close tombstones), and a removed reward checkpoint zeroes only the
// pending claim. Before this refinement any removed entry deleted the pool
// and every position in it.
func (a *Adapter) applyEntryRemoval(c bindings.ContractDataChange, pools map[string]bindings.AMMPoolState, positions map[string]bindings.AMMPositionState) {
	poolID := c.ContractID
	if mapped, isShare := a.shareTokens[c.ContractID]; isShare {
		poolID = mapped
	}
	if key, ok := decodeVal(c.KeyXDR); ok {
		switch strings.ToLower(symbolOrFirst(key)) {
		case "balance", "position", "positions":
			probe := bindings.AMMPositionState{PoolContractID: poolID, Address: addrInKey(key)}
			if probe.Address == "" {
				return
			}
			if lo, hi, ok := ticksInKey(key); ok {
				probe.TickLower, probe.TickUpper = lo, hi
			}
			k := positionKey(probe)
			if current, exists := positions[k]; exists {
				current.SharesRaw = "0"
				current.LiquidityRaw = "0"
				current.Principal0Raw = ""
				current.Principal1Raw = ""
				current.PendingFee0Raw = ""
				current.PendingFee1Raw = ""
				positions[k] = current
			}
			return
		case "userrewarddata":
			probe := bindings.AMMPositionState{PoolContractID: poolID, Address: addrInKey(key)}
			if probe.Address == "" {
				return
			}
			k := positionKey(probe)
			if current, exists := positions[k]; exists {
				current.PendingRewardRaw = "0"
				current.RewardPoolAccumulatedRaw = ""
				positions[k] = current
			}
			return
		case "workingbalance", "user":
			return
		}
	}
	// Instance-level (or undecodable-key) removal: the pool itself is gone.
	delete(pools, c.ContractID)
	for k, p := range positions {
		if p.PoolContractID == c.ContractID {
			delete(positions, k)
		}
	}
}

// upsertClassicShares folds a share-balance write into the classic (untick'd)
// position for (pool, owner), preserving previously folded reward state.
func upsertClassicShares(positions map[string]bindings.AMMPositionState, poolID, owner, shares string) {
	pos := bindings.AMMPositionState{Address: owner, PoolContractID: poolID}
	k := positionKey(pos)
	if current, ok := positions[k]; ok {
		pos = current
	}
	pos.SharesRaw = shares
	if shares != "0" {
		pos.HadShares = true
	}
	positions[k] = pos
}

func addrInKey(key xdr.ScVal) string {
	if a := addr(key); a != "" {
		return a
	}
	if vec, ok := key.GetVec(); ok && vec != nil {
		for _, x := range *vec {
			if a := addr(x); a != "" {
				return a
			}
		}
	}
	return ""
}

// ticksInKey extracts the (tick_lower, tick_upper) bounds a concentrated
// Position entry carries in its KEY vector — `Position(owner, i32, i32)`.
func ticksInKey(key xdr.ScVal) (int32, int32, bool) {
	vec, ok := key.GetVec()
	if !ok || vec == nil {
		return 0, 0, false
	}
	ticks := make([]int32, 0, 2)
	for _, x := range *vec {
		if i, ok := x.GetI32(); ok {
			ticks = append(ticks, int32(i))
		}
	}
	if len(ticks) != 2 {
		return 0, 0, false
	}
	return ticks[0], ticks[1], true
}

func cloneState(s *bindings.LedgerState) *bindings.LedgerState {
	if s == nil {
		return &bindings.LedgerState{}
	}
	b, _ := json.Marshal(s)
	var out bindings.LedgerState
	_ = json.Unmarshal(b, &out)
	return &out
}
func positionKey(p bindings.AMMPositionState) string {
	return fmt.Sprintf("%s|%s|%d|%d", p.PoolContractID, p.Address, p.TickLower, p.TickUpper)
}
func decodeVal(s string) (xdr.ScVal, bool) {
	b, e := base64.StdEncoding.DecodeString(s)
	if e != nil {
		return xdr.ScVal{}, false
	}
	var v xdr.ScVal
	if xdr.SafeUnmarshal(b, &v) != nil {
		return xdr.ScVal{}, false
	}
	return v, true
}
func symbolOrFirst(v xdr.ScVal) string {
	if s, ok := v.GetSym(); ok {
		return string(s)
	}
	if vec, ok := v.GetVec(); ok && vec != nil && len(*vec) > 0 {
		return symbolOrFirst((*vec)[0])
	}
	return ""
}
func addr(v xdr.ScVal) string {
	a, ok := v.GetAddress()
	if !ok {
		return ""
	}
	switch a.Type {
	case xdr.ScAddressTypeScAddressTypeContract:
		return strkey.MustEncode(strkey.VersionByteContract, a.ContractId[:])
	case xdr.ScAddressTypeScAddressTypeAccount:
		return strkey.MustEncode(strkey.VersionByteAccountID, a.AccountId.Ed25519[:])
	}
	return ""
}
func firstUint(v xdr.ScVal) string {
	if u, ok := v.GetU128(); ok {
		return new(bigInt).u128(u)
	}
	if i, ok := v.GetI128(); ok {
		return new(bigInt).i128(i)
	}
	if u, ok := v.GetU256(); ok {
		return new(bigInt).u256(u)
	}
	if u, ok := v.GetU64(); ok {
		return fmt.Sprint(uint64(u))
	}
	if u, ok := v.GetU32(); ok {
		return fmt.Sprint(uint32(u))
	}
	if i, ok := v.GetI64(); ok {
		return fmt.Sprint(int64(i))
	}
	if i, ok := v.GetI32(); ok {
		return fmt.Sprint(int32(i))
	}
	if vec, ok := v.GetVec(); ok && vec != nil {
		for _, x := range *vec {
			if n := firstUint(x); n != "" {
				return n
			}
		}
	}
	return ""
}

type bigInt struct{}

func (*bigInt) u128(v xdr.UInt128Parts) string { return newBig(uint64(v.Hi), uint64(v.Lo), false) }
func (*bigInt) i128(v xdr.Int128Parts) string {
	return newBig(uint64(v.Hi), uint64(v.Lo), int64(v.Hi) < 0)
}
func (*bigInt) u256(v xdr.UInt256Parts) string {
	n := new(big.Int).SetUint64(uint64(v.HiHi))
	for _, part := range []uint64{uint64(v.HiLo), uint64(v.LoHi), uint64(v.LoLo)} {
		n.Lsh(n, 64)
		n.Or(n, new(big.Int).SetUint64(part))
	}
	return n.String()
}
func newBig(hi, lo uint64, neg bool) string {
	n := new(big.Int).SetUint64(hi)
	n.Lsh(n, 64)
	n.Or(n, new(big.Int).SetUint64(lo))
	if neg {
		limit := new(big.Int).Lsh(big.NewInt(1), 128)
		n.Sub(n, limit)
	}
	return n.String()
}
func fields(v xdr.ScVal) map[string]xdr.ScVal {
	out := map[string]xdr.ScVal{}
	if m, ok := v.GetMap(); ok && m != nil {
		for _, e := range *m {
			out[strings.ToLower(symbolOrFirst(e.Key))] = e.Val
		}
	}
	return out
}
func decodePoolInstance(p *bindings.AMMPoolState, v xdr.ScVal) {
	f := fields(v)
	if x := f["pool_type"]; x.Type != 0 {
		p.PoolType = strings.ToLower(symbolOrFirst(x))
	}
	if x := f["fee"]; x.Type != 0 {
		p.FeeFractionRaw = firstUint(x)
	}
	if x := f["a"]; x.Type != 0 {
		p.AmplificationRaw = firstUint(x)
	}
	if x := f["tick_spacing"]; x.Type != 0 {
		fmt.Sscan(firstUint(x), &p.TickSpacing)
	}
	if x := f["pool_hash"]; x.Type != 0 {
		if b, ok := x.GetBytes(); ok {
			p.PoolHash = fmt.Sprintf("%x", b)
		}
	}
	if x := f["tokens"]; x.Type != 0 {
		p.Tokens = decodeReserves(p.Tokens, x)
	}
}
func decodeReserves(old []bindings.AMMTokenReserve, v xdr.ScVal) []bindings.AMMTokenReserve {
	vec, ok := v.GetVec()
	if !ok || vec == nil {
		return old
	}
	out := make([]bindings.AMMTokenReserve, 0, len(*vec))
	for i, x := range *vec {
		// Tokens (address vec) and Reserves (uint vec) are separate storage
		// entries folded in either order; each pass fills its own column and
		// preserves the other's.
		r := bindings.AMMTokenReserve{}
		if i < len(old) {
			r = old[i]
		}
		if id := addr(x); id != "" {
			r.AssetID = id
		} else {
			r.ReserveRaw = firstUint(x)
		}
		out = append(out, r)
	}
	return out
}
func upsertToken(tokens *[]bindings.AMMTokenReserve, idx int, assetID, reserve string) {
	for len(*tokens) <= idx {
		*tokens = append(*tokens, bindings.AMMTokenReserve{})
	}
	if assetID != "" {
		(*tokens)[idx].AssetID = assetID
	}
	if reserve != "" {
		(*tokens)[idx].ReserveRaw = reserve
	}
}
func decodeSlot0(p *bindings.AMMPoolState, v xdr.ScVal) {
	f := fields(v)
	p.SqrtPriceX96 = firstUint(f["sqrt_price_x96"])
	fmt.Sscan(firstUint(f["tick"]), &p.CurrentTick)
	if x := firstUint(f["active_liquidity"]); x != "" {
		// The concentrated instance stores active liquidity under its own
		// Liquidity key; only overwrite it when Slot0 actually carries one.
		p.ActiveLiquidityRaw = x
	}
}
func decodePosition(pool string, key, val xdr.ScVal) (bindings.AMMPositionState, bool) {
	p := bindings.AMMPositionState{PoolContractID: pool, Address: addrInKey(key)}
	f := fields(val)
	p.SharesRaw = firstUint(f["shares"])
	if p.SharesRaw == "" {
		p.SharesRaw = firstUint(val)
	}
	p.LiquidityRaw = firstUint(f["liquidity"])
	p.SqrtPriceLowerX96 = firstUint(f["sqrt_price_lower_x96"])
	p.SqrtPriceUpperX96 = firstUint(f["sqrt_price_upper_x96"])
	fmt.Sscan(firstUint(f["tick_lower"]), &p.TickLower)
	fmt.Sscan(firstUint(f["tick_upper"]), &p.TickUpper)
	p.PendingFee0Raw = firstUint(f["tokens_owed_0"])
	p.PendingFee1Raw = firstUint(f["tokens_owed_1"])
	if symbolOrFirst(key) == "Position" {
		// Concentrated ranges key on (owner, tick_lower, tick_upper) — the
		// bounds live in the entry KEY, not the value (D-05 keying). A
		// Position key without both bounds must not fold: a guessed (0, 0)
		// range would collide distinct ranges of one owner.
		lo, hi, ok := ticksInKey(key)
		if !ok {
			return p, false
		}
		p.TickLower, p.TickUpper = lo, hi
		if p.SqrtPriceLowerX96 == "" {
			if x, err := tickSqrtPriceX96(lo); err == nil {
				p.SqrtPriceLowerX96 = x
			}
		}
		if p.SqrtPriceUpperX96 == "" {
			if x, err := tickSqrtPriceX96(hi); err == nil {
				p.SqrtPriceUpperX96 = x
			}
		}
	}
	return p, p.Address != ""
}
func decodeRewardPosition(pool string, key, val xdr.ScVal) (bindings.AMMPositionState, bool) {
	p := bindings.AMMPositionState{PoolContractID: pool}
	if vec, ok := key.GetVec(); ok && vec != nil {
		for _, x := range *vec {
			if a := addr(x); strings.HasPrefix(a, "G") {
				p.Address = a
			}
		}
	}
	f := fields(val)
	p.PendingRewardRaw = firstUint(f["to_claim"])
	p.RewardPoolAccumulatedRaw = firstUint(f["pool_accumulated"])
	return p, p.Address != ""
}
func assetMetadata(id string, key, val xdr.ScVal) (bindings.AMMAssetMetadata, bool) {
	if strings.ToLower(symbolOrFirst(key)) != "metadata" {
		return bindings.AMMAssetMetadata{}, false
	}
	f := fields(val)
	m := bindings.AMMAssetMetadata{ContractID: id, Name: symbolOrFirst(f["name"]), Symbol: symbolOrFirst(f["symbol"])}
	fmt.Sscan(firstUint(f["decimal"]), &m.Decimals)
	if m.Decimals == 0 {
		fmt.Sscan(firstUint(f["decimals"]), &m.Decimals)
	}
	return m, m.Symbol != ""
}
func upsertAsset(xs *[]bindings.AMMAssetMetadata, m bindings.AMMAssetMetadata) {
	for i := range *xs {
		if (*xs)[i].ContractID == m.ContractID {
			(*xs)[i] = m
			return
		}
	}
	*xs = append(*xs, m)
}
