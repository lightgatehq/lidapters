package fxdao

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// DecodeState is a pure reducer: (prior, changes, ledgerSeq) -> next. It folds
// persistent Vault entries, VaultIndex writes (dirty-signal only — the value
// duplicates the vault's own index field) and the instance VaultsInfo maps.
// Restored changes are LIVE writes with the raw variant preserved; only a
// genuine Removed change closes a vault.
func (a *Adapter) DecodeState(prior *bindings.LedgerState, changes []bindings.ContractDataChange, ledgerSeq int64) (*bindings.LedgerState, error) {
	next := cloneState(prior)
	a.dirty = nil

	vaults := map[string]bindings.VaultState{}
	for _, v := range next.Vaults {
		vaults[vaultKey(v.ContractID, v.Account, v.Denomination)] = v
	}
	infos := map[string]bindings.VaultsInfoState{}
	for _, v := range next.VaultsInfo {
		infos[v.ContractID+"|"+v.Denomination] = v
	}

	for _, c := range changes {
		if !a.OwnsContract(c.ContractID) {
			continue
		}
		key, ok := decodeVal(c.KeyXDR)
		if !ok {
			continue
		}
		if c.ValueXDR == nil || c.ChangeType == "Removed" {
			// Only a genuine LedgerEntryRemoved closes a vault. A TTL lapse /
			// eviction (nil value without Removed) archives — the entry restores
			// later with its bytes intact, so the last folded state is kept.
			if c.ChangeType != "Removed" {
				continue
			}
			if account, denom, ok := vaultEntryKey(key); ok {
				k := vaultKey(c.ContractID, account, denom)
				v := vaults[k]
				v.Protocol = a.cfg.Protocol
				v.ContractID = c.ContractID
				v.Account = account
				v.Denomination = denom
				// The removal itself proves the vault existed on-chain, even when
				// its live write predates the folded window.
				v.HadVault = true
				v.Closed = true
				v.Restored = false
				// Deleted on-chain: the amounts are gone — absent, not zero.
				v.IndexRaw, v.CollateralRaw, v.DebtRaw = "", "", ""
				vaults[k] = v
				a.dirty = append(a.dirty, DirtyVault{ContractID: c.ContractID, Account: account, Denomination: denom, Kind: bindings.DirtyRemoval})
			}
			continue
		}
		val, ok := decodeVal(*c.ValueXDR)
		if !ok {
			continue
		}
		if instance, isInstance := val.GetInstance(); isInstance {
			if instance.Storage == nil {
				continue
			}
			for _, entry := range *instance.Storage {
				denom, ok := vaultsInfoEntryKey(entry.Key)
				if !ok {
					// CoreState, Currency(Symbol) and any other instance keys are
					// recognized-not-carried.
					continue
				}
				f := fields(entry.Val)
				infos[c.ContractID+"|"+denom] = bindings.VaultsInfoState{
					Protocol:           a.cfg.Protocol,
					ContractID:         c.ContractID,
					Denomination:       denom,
					TotalVaultsRaw:     uintString(f["total_vaults"]),
					TotalDebtRaw:       uintString(f["total_debt"]),
					TotalColRaw:        uintString(f["total_col"]),
					MinColRateRaw:      uintString(f["min_col_rate"]),
					MinDebtCreationRaw: uintString(f["min_debt_creation"]),
					OpeningColRateRaw:  uintString(f["opening_col_rate"]),
				}
			}
			continue
		}
		if account, denom, ok := vaultEntryKey(key); ok {
			f := fields(val)
			k := vaultKey(c.ContractID, account, denom)
			v := vaults[k]
			v.Protocol = a.cfg.Protocol
			v.ContractID = c.ContractID
			v.Account = account
			v.Denomination = denom
			v.IndexRaw = uintString(f["index"])
			v.CollateralRaw = uintString(f["total_collateral"])
			v.DebtRaw = uintString(f["total_debt"])
			v.HadVault = true
			v.Closed = false
			// Restored = live, with the raw change variant preserved.
			v.Restored = c.ChangeType == "Restored"
			vaults[k] = v
			a.dirty = append(a.dirty, DirtyVault{ContractID: c.ContractID, Account: account, Denomination: denom, Kind: bindings.DirtyUpsert})
			continue
		}
		if account, denom, ok := vaultIndexEntryKey(key); ok {
			// The VaultIndex value duplicates the vault's own index field; its
			// write (including a restore-from-archival) is a dirty signal for the
			// (account, denomination) vault, not carried state.
			a.dirty = append(a.dirty, DirtyVault{ContractID: c.ContractID, Account: account, Denomination: denom, Kind: bindings.DirtyUpsert})
			continue
		}
		// Other persistent keys on the vaults contract: recognized-not-carried.
	}

	next.Vaults = next.Vaults[:0]
	for _, v := range vaults {
		next.Vaults = append(next.Vaults, v)
	}
	sort.Slice(next.Vaults, func(i, j int) bool {
		return vaultKey(next.Vaults[i].ContractID, next.Vaults[i].Account, next.Vaults[i].Denomination) <
			vaultKey(next.Vaults[j].ContractID, next.Vaults[j].Account, next.Vaults[j].Denomination)
	})
	next.VaultsInfo = next.VaultsInfo[:0]
	for _, v := range infos {
		next.VaultsInfo = append(next.VaultsInfo, v)
	}
	sort.Slice(next.VaultsInfo, func(i, j int) bool {
		return next.VaultsInfo[i].ContractID+"|"+next.VaultsInfo[i].Denomination <
			next.VaultsInfo[j].ContractID+"|"+next.VaultsInfo[j].Denomination
	})
	sortDirty(a.dirty)
	a.dirty = dedupeDirty(a.dirty)
	return next, nil
}

// dedupeDirty collapses duplicate (contract, account, denomination) entries
// (a vault write plus its VaultIndex write in the same ledger), keeping a
// Removal over an Upsert. Input must be sorted.
func dedupeDirty(in []DirtyVault) []DirtyVault {
	out := in[:0]
	for _, d := range in {
		if n := len(out); n > 0 &&
			out[n-1].ContractID == d.ContractID &&
			out[n-1].Account == d.Account &&
			out[n-1].Denomination == d.Denomination {
			if d.Kind == bindings.DirtyRemoval {
				out[n-1].Kind = bindings.DirtyRemoval
			}
			continue
		}
		out = append(out, d)
	}
	return out
}

func vaultKey(contractID, account, denom string) string {
	return contractID + "|" + account + "|" + denom
}

// vaultEntryKey matches the persistent Vault((Address, Symbol)) key — the
// nested-Vec shape Vec[Symbol("Vault"), Vec[Address(owner), Symbol(denom)]].
// The tuple payload is itself an ScVec INSIDE the variant Vec; a flat
// three-element Vec is NOT a vault key and matches nothing.
func vaultEntryKey(key xdr.ScVal) (account, denom string, ok bool) {
	vec, has := key.GetVec()
	if !has || vec == nil || len(*vec) != 2 {
		return "", "", false
	}
	if s, has := (*vec)[0].GetSym(); !has || string(s) != "Vault" {
		return "", "", false
	}
	tuple, has := (*vec)[1].GetVec()
	if !has || tuple == nil || len(*tuple) != 2 {
		return "", "", false
	}
	account = addr((*tuple)[0])
	s, has := (*tuple)[1].GetSym()
	if account == "" || !has {
		return "", "", false
	}
	return account, string(s), true
}

// vaultIndexEntryKey matches VaultIndex(VaultIndexKey{user, denomination}) —
// Vec[Symbol("VaultIndex"), Map{denomination, user}].
func vaultIndexEntryKey(key xdr.ScVal) (account, denom string, ok bool) {
	vec, has := key.GetVec()
	if !has || vec == nil || len(*vec) != 2 {
		return "", "", false
	}
	if s, has := (*vec)[0].GetSym(); !has || string(s) != "VaultIndex" {
		return "", "", false
	}
	f := fields((*vec)[1])
	account = addr(f["user"])
	s, has := f["denomination"].GetSym()
	if account == "" || !has {
		return "", "", false
	}
	return account, string(s), true
}

// vaultsInfoEntryKey matches the instance VaultsInfo(Symbol) key —
// Vec[Symbol("VaultsInfo"), Symbol(denomination)].
func vaultsInfoEntryKey(key xdr.ScVal) (denom string, ok bool) {
	vec, has := key.GetVec()
	if !has || vec == nil || len(*vec) != 2 {
		return "", false
	}
	if s, has := (*vec)[0].GetSym(); !has || string(s) != "VaultsInfo" {
		return "", false
	}
	s, has := (*vec)[1].GetSym()
	if !has {
		return "", false
	}
	return string(s), true
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

// uintString renders a u128/u64/u32 ScVal as a decimal string; "" for
// anything else (absent-not-zero: a missing field never becomes "0").
func uintString(v xdr.ScVal) string {
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

func fields(v xdr.ScVal) map[string]xdr.ScVal {
	out := map[string]xdr.ScVal{}
	if m, ok := v.GetMap(); ok && m != nil {
		for _, e := range *m {
			if s, ok := e.Key.GetSym(); ok {
				out[string(s)] = e.Val
			}
		}
	}
	return out
}
