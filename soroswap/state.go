package soroswap

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

// Pair instance u32 storage keys (contracts/pair/src/storage.rs:7-15
// @ bb90a65). Reserves and KLast are i128; KLast is read with a default in
// the contract and may be ABSENT on-chain — absence decodes to absence here.
const (
	pairKeyToken0   = 0
	pairKeyToken1   = 1
	pairKeyReserve0 = 2
	pairKeyReserve1 = 3
	pairKeyFactory  = 4
	pairKeyKLast    = 5
)

// DecodeState is a pure reducer: (prior, changes, ledgerSeq) -> next. Two
// passes per ledger — a discovery pass registers pairs from the factory's
// PairAddressesNIndexed(u32) persistent writes, then the fold pass decodes
// pair instances (raw u32 keys), LP Balance entries and TotalSupply.
// A Restored change is a live write (the entry came back from archival with
// its bytes intact); only a Removed change tombstones.
func (a *Adapter) DecodeState(prior *bindings.LedgerState, changes []bindings.ContractDataChange, ledgerSeq int64) (*bindings.LedgerState, error) {
	next := cloneState(prior)
	a.dirty = nil

	for _, c := range changes {
		if _, isFactory := a.factories[c.ContractID]; !isFactory || c.ValueXDR == nil {
			continue
		}
		key, ok := decodeVal(c.KeyXDR)
		if !ok {
			continue
		}
		if symbolOrFirst(key) == "PairAddressesNIndexed" {
			val, ok := decodeVal(*c.ValueXDR)
			if !ok {
				continue
			}
			if pair := addr(val); pair != "" {
				a.pairs[pair] = struct{}{}
			}
		}
	}

	pools := map[string]bindings.AMMPoolState{}
	for _, p := range next.AMMPools {
		pools[p.ContractID] = p
	}
	positions := map[string]bindings.AMMPositionState{}
	for _, p := range next.AMMPositions {
		positions[positionKey(p)] = p
	}

	for _, c := range changes {
		if !a.OwnsContract(c.ContractID) {
			continue
		}
		if _, isAsset := a.assets[c.ContractID]; isAsset {
			if c.ValueXDR == nil {
				continue
			}
			key, kok := decodeVal(c.KeyXDR)
			val, vok := decodeVal(*c.ValueXDR)
			if kok && vok {
				if m, ok := assetMetadata(c.ContractID, key, val); ok {
					upsertAsset(&next.AMMAssets, m)
				}
			}
			continue
		}
		if _, isPair := a.pairs[c.ContractID]; !isPair {
			// Factory changes were consumed by the discovery pass; the factory's
			// remaining entries (instance fields, PairWasmHash,
			// PairAddressesByTokens) are recognized-not-carried.
			continue
		}
		key, ok := decodeVal(c.KeyXDR)
		if !ok {
			continue
		}
		if c.ValueXDR == nil || c.ChangeType == "Removed" {
			// Only a genuine LedgerEntryRemoved change closes a position. A TTL
			// lapse / network eviction (nil value without Removed) archives: the
			// last folded state is kept, matching the blend Change-1 doctrine.
			if c.ChangeType == "Removed" {
				if holder := balanceHolder(key); holder != "" {
					a.tombstonePosition(positions, c.ContractID, holder)
				}
			}
			continue
		}
		val, ok := decodeVal(*c.ValueXDR)
		if !ok {
			continue
		}
		p := pools[c.ContractID]
		p.Protocol = a.cfg.Protocol
		p.ContractID = c.ContractID
		p.PoolType = "constant_product"
		touched := false
		if instance, isInstance := val.GetInstance(); isInstance {
			if hash, ok := instance.Executable.GetWasmHash(); ok {
				p.WasmHash = xdr.Hash(hash).HexString()
			}
			if len(a.cfg.PairWasmHashes) > 0 && p.WasmHash != "" {
				if _, known := a.cfg.PairWasmHashes[strings.ToLower(p.WasmHash)]; !known {
					continue
				}
			}
			if instance.Storage != nil {
				for _, entry := range *instance.Storage {
					if u, ok := entry.Key.GetU32(); ok {
						switch uint32(u) {
						case pairKeyToken0:
							upsertToken(&p.Tokens, 0, addr(entry.Val), "")
						case pairKeyToken1:
							upsertToken(&p.Tokens, 1, addr(entry.Val), "")
						case pairKeyReserve0:
							upsertToken(&p.Tokens, 0, "", i128String(entry.Val))
						case pairKeyReserve1:
							upsertToken(&p.Tokens, 1, "", i128String(entry.Val))
						case pairKeyFactory:
							// The factory is the pair-discovery authority; the gold
							// router_contract column carries it.
							p.RouterContract = addr(entry.Val)
						case pairKeyKLast:
							// Recognized-not-carried: mint_fee accounting internal,
							// no gold column consumes it.
						}
						touched = true
						continue
					}
					switch symbolOrFirst(entry.Key) {
					case "TotalSupply":
						p.TotalSharesRaw = i128String(entry.Val)
						touched = true
					case "METADATA":
						if m, ok := assetMetadata(c.ContractID, entry.Key, entry.Val); ok {
							upsertAsset(&next.AMMAssets, m)
						}
					}
				}
			}
		}
		if holder := balanceHolder(key); holder != "" {
			if shares := i128String(val); shares != "" {
				a.upsertPosition(positions, c.ContractID, holder, shares)
			}
		}
		if touched {
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
	sort.Slice(a.dirty, func(i, j int) bool {
		if a.dirty[i].PoolContractID != a.dirty[j].PoolContractID {
			return a.dirty[i].PoolContractID < a.dirty[j].PoolContractID
		}
		return a.dirty[i].Address < a.dirty[j].Address
	})
	return next, nil
}

func (a *Adapter) upsertPosition(positions map[string]bindings.AMMPositionState, pair, holder, shares string) {
	pos := bindings.AMMPositionState{Address: holder, PoolContractID: pair, SharesRaw: shares}
	k := positionKey(pos)
	if current, ok := positions[k]; ok {
		pos.HadShares = current.HadShares
	}
	if shares != "0" {
		pos.HadShares = true
	}
	positions[k] = pos
	a.dirty = append(a.dirty, bindings.DirtyPosition{Address: holder, PoolContractID: pair, Kind: bindings.DirtyUpsert})
}

// tombstonePosition records a genuine on-chain Balance removal: shares go to
// zero (HadShares preserved so the transform writes explicit tombstones) and
// the dirty set reports a Removal.
func (a *Adapter) tombstonePosition(positions map[string]bindings.AMMPositionState, pair, holder string) {
	pos := bindings.AMMPositionState{Address: holder, PoolContractID: pair, SharesRaw: "0"}
	k := positionKey(pos)
	if current, ok := positions[k]; ok {
		pos.HadShares = current.HadShares
	}
	positions[k] = pos
	a.dirty = append(a.dirty, bindings.DirtyPosition{Address: holder, PoolContractID: pair, Kind: bindings.DirtyRemoval})
}

// balanceHolder returns the holder address of a persistent LP Balance entry
// key — Vec[Symbol("Balance"), Address] (soroswap_pair_token/storage_types.rs
// :25-29 @ bb90a65). The holder may be a CONTRACT address: the pair mints the
// MINIMUM_LIQUIDITY first-deposit lock to itself (pair/src/lib.rs:31,193).
func balanceHolder(key xdr.ScVal) string {
	vec, ok := key.GetVec()
	if !ok || vec == nil || len(*vec) != 2 {
		return ""
	}
	if s, ok := (*vec)[0].GetSym(); !ok || string(s) != "Balance" {
		return ""
	}
	return addr((*vec)[1])
}

func positionKey(p bindings.AMMPositionState) string {
	return p.PoolContractID + "|" + p.Address
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

// i128String renders an i128/u128/u32/u64 ScVal as a decimal string; "" for
// anything else (absent-not-zero: a missing value never becomes "0").
func i128String(v xdr.ScVal) string {
	if i, ok := v.GetI128(); ok {
		return int128String(i)
	}
	if u, ok := v.GetU128(); ok {
		n := new(big.Int).SetUint64(uint64(u.Hi))
		n.Lsh(n, 64)
		n.Or(n, new(big.Int).SetUint64(uint64(u.Lo)))
		return n.String()
	}
	if u, ok := v.GetU64(); ok {
		return fmt.Sprint(uint64(u))
	}
	if u, ok := v.GetU32(); ok {
		return fmt.Sprint(uint32(u))
	}
	return ""
}

func int128String(v xdr.Int128Parts) string {
	n := new(big.Int).SetUint64(uint64(v.Hi))
	n.Lsh(n, 64)
	n.Or(n, new(big.Int).SetUint64(uint64(v.Lo)))
	if int64(v.Hi) < 0 {
		n.Sub(n, new(big.Int).Lsh(big.NewInt(1), 128))
	}
	return n.String()
}

func fields(v xdr.ScVal) map[string]xdr.ScVal {
	out := map[string]xdr.ScVal{}
	if m, ok := v.GetMap(); ok && m != nil {
		for _, e := range *m {
			out[symbolOrFirst(e.Key)] = e.Val
		}
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

// assetMetadata decodes a token METADATA instance entry (SEP-41 shape: name,
// symbol, decimal). On a pair contract this is the pair's own LP-token
// identity (e.g. "native-USDC-SOROSWAP-LP").
func assetMetadata(id string, key, val xdr.ScVal) (bindings.AMMAssetMetadata, bool) {
	if symbolOrFirst(key) != "METADATA" {
		return bindings.AMMAssetMetadata{}, false
	}
	f := fields(val)
	m := bindings.AMMAssetMetadata{ContractID: id, Name: stringOrSymbol(f["name"]), Symbol: stringOrSymbol(f["symbol"])}
	if d, ok := f["decimal"].GetU32(); ok {
		m.Decimals = int32(d)
	}
	return m, m.Symbol != ""
}

func stringOrSymbol(v xdr.ScVal) string {
	if s, ok := v.GetStr(); ok {
		return string(s)
	}
	if s, ok := v.GetSym(); ok {
		return string(s)
	}
	return ""
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
