package fxdao

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/lightgatehq/lidapters/bindings"
)

func stableID(parts ...any) string {
	var b strings.Builder
	for _, p := range parts {
		if b.Len() > 0 {
			b.WriteByte('|')
		}
		b.WriteString(fmt.Sprint(p))
	}
	s := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(s[:])
}

// Transform folds vault state into gold snapshot rows. State only: this
// adapter has no activity path — the vaults contract emits zero events by
// design, so any event that DOES arrive on an owned contract is an anomaly
// and is quarantined with its raw bytes (never classified, never dropped).
//
// A live vault emits a status='active' row; a genuinely deleted vault emits
// its terminal status='closed' row (the tombstone) with amounts absent —
// current-state views project the latest row per (address, contract,
// denomination). RatioRaw / USD valuations stay honest-null: the protocol
// oracle's price layout is unconfirmed, so the raw collateral, debt and
// index are the served facts.
func (a *Adapter) Transform(in bindings.TransformInput) (*bindings.TransformOutput, error) {
	out := &bindings.TransformOutput{LedgerSeq: in.LedgerSeq}
	if in.State != nil {
		for _, v := range in.State.Vaults {
			if v.Protocol != a.cfg.Protocol || !v.HadVault {
				continue
			}
			status := "active"
			if v.Closed {
				status = "closed"
			}
			metadata := map[string]string{}
			if v.Restored {
				// The raw change variant, preserved end to end: this snapshot's
				// backing write was a LedgerEntryRestored (restored = live).
				metadata["restored"] = "true"
			}
			out.Vaults = append(out.Vaults, bindings.Vault{
				ID:            stableID(a.cfg.Protocol, v.ContractID, v.Account, v.Denomination),
				Address:       v.Account,
				Protocol:      a.cfg.Protocol,
				ContractID:    v.ContractID,
				Denomination:  v.Denomination,
				VaultIndexRaw: v.IndexRaw,
				CollateralRaw: v.CollateralRaw,
				DebtRaw:       v.DebtRaw,
				Status:        status,
				LedgerSeq:     in.LedgerSeq,
				Timestamp:     in.CloseTime,
				Metadata:      metadata,
			})
		}
	}
	for _, evt := range in.Events {
		if !a.OwnsContract(evt.ContractID) {
			continue
		}
		out.Quarantine = append(out.Quarantine, bindings.QuarantineEvent{
			ID:         stableID(a.ID(), evt.LedgerSeq, evt.TxHash, evt.EventIndex),
			AdapterID:  a.ID(),
			LedgerSeq:  evt.LedgerSeq,
			TxHash:     evt.TxHash,
			EventIndex: evt.EventIndex,
			ContractID: evt.ContractID,
			Reason:     "fxdao_unexpected_event",
			RawEvent:   evt.RawEvent,
		})
	}
	sort.Slice(out.Vaults, func(i, j int) bool {
		if out.Vaults[i].ContractID != out.Vaults[j].ContractID {
			return out.Vaults[i].ContractID < out.Vaults[j].ContractID
		}
		if out.Vaults[i].Address != out.Vaults[j].Address {
			return out.Vaults[i].Address < out.Vaults[j].Address
		}
		return out.Vaults[i].Denomination < out.Vaults[j].Denomination
	})
	return out, nil
}
