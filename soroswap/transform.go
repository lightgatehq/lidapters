package soroswap

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/lightgatehq/lidapters/blend/contracts"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// The activity vocabulary is the complete pair event set from the pinned
// protocol sources (contracts/pair/src/event.rs:33,64,98,115,133 @ bb90a65),
// stored under the EXACT on-chain event names. It must stay equal to the
// frozen gold CHECK constraint (soroswap_activities.activity_type in
// docs/gold-ddl/020_soroswap_gold.sql); additions there are a numbered
// follow-up migration, never an edit.
var pairActivityVocabulary = map[string]struct{}{
	"deposit":  {},
	"swap":     {},
	"withdraw": {},
	"sync":     {},
	"skim":     {},
}

// Recognized non-activities. Factory events (contracts/factory/src/event.rs
// :16,42,67,88,107) and router events (contracts/router/src/event.rs
// :16,64,115,150) are protocol plumbing / convenience-op echoes: the pair
// events carry the activity feed. The pair also emits SEP-41 token events for
// its own LP shares — mint, burn, transfer, approve
// (soroswap_pair_token/contract.rs @ bb90a65, soroban-token-sdk) — which are
// recognized-not-activities: the AMM events carry the feed and the Balance
// writes carry the positions; classifying the token echo would double-book
// both. Recognized events classify to nothing and are never quarantined.
var factoryEventNames = map[string]struct{}{
	"init": {}, "new_pair": {}, "fee_to": {}, "setter": {}, "fees": {},
}
var routerEventNames = map[string]struct{}{
	"init": {}, "add": {}, "remove": {}, "swap": {},
}
var tokenEchoNames = map[string]struct{}{
	"mint": {}, "burn": {}, "transfer": {}, "approve": {},
}

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

func (a *Adapter) Transform(in bindings.TransformInput) (*bindings.TransformOutput, error) {
	out := &bindings.TransformOutput{LedgerSeq: in.LedgerSeq}
	if in.State != nil {
		pools := map[string]bindings.AMMPoolState{}
		for _, p := range in.State.AMMPools {
			if p.Protocol != a.cfg.Protocol {
				continue
			}
			pools[p.ContractID] = p
			out.AMMPools = append(out.AMMPools, bindings.AMMPool{
				Protocol:       a.cfg.Protocol,
				RouterContract: p.RouterContract,
				ContractID:     p.ContractID,
				PoolType:       p.PoolType,
				WasmHash:       p.WasmHash,
				Tokens:         p.Tokens,
				TotalSharesRaw: p.TotalSharesRaw,
				LedgerSeq:      in.LedgerSeq,
				Timestamp:      in.CloseTime,
			})
		}
		for _, pos := range in.State.AMMPositions {
			pool, ok := pools[pos.PoolContractID]
			if !ok {
				continue
			}
			group := stableID(a.cfg.Protocol, pos.Address, pos.PoolContractID)
			if pos.SharesRaw != "" && pos.SharesRaw != "0" {
				for _, t := range pool.Tokens {
					amount, err := proRata(pos.SharesRaw, t.ReserveRaw, pool.TotalSharesRaw)
					if err != nil {
						out.Quarantine = append(out.Quarantine, bindings.QuarantineEvent{
							ID: stableID(group, t.AssetID, in.LedgerSeq), AdapterID: a.ID(),
							LedgerSeq: in.LedgerSeq, ContractID: pool.ContractID,
							Reason: "invalid_lp_share_state",
						})
						continue
					}
					out.AMMComponents = append(out.AMMComponents, a.component(group, pos, t.AssetID, amount, pos.SharesRaw, in, false))
				}
			} else if pos.SharesRaw == "0" && pos.HadShares {
				// Closed LP position: explicit zero tombstones so the
				// latest-per-identity current view stops surfacing the pre-close
				// rows. HadShares distinguishes a real close from a never-held
				// position, which stays silent.
				for _, t := range pool.Tokens {
					out.AMMComponents = append(out.AMMComponents, a.component(group, pos, t.AssetID, "0", "0", in, true))
				}
			}
		}
	}
	for _, evt := range in.Events {
		act, q := a.classifyEvent(evt)
		if act != nil {
			out.Activities = append(out.Activities, *act)
		}
		if q != nil {
			out.Quarantine = append(out.Quarantine, *q)
		}
	}
	sort.Slice(out.AMMPools, func(i, j int) bool { return out.AMMPools[i].ContractID < out.AMMPools[j].ContractID })
	sort.Slice(out.AMMComponents, func(i, j int) bool { return out.AMMComponents[i].ID < out.AMMComponents[j].ID })
	return out, nil
}

func (a *Adapter) component(group string, p bindings.AMMPositionState, asset, amount, shares string, in bindings.TransformInput, closed bool) bindings.AMMPositionComponent {
	metadata := map[string]string{"price_unavailable": "true"}
	if closed {
		metadata["closed"] = "true"
	}
	return bindings.AMMPositionComponent{
		ID: stableID(group, "lp_principal", asset), PositionGroupID: group,
		Address: p.Address, Protocol: a.cfg.Protocol, PoolContractID: p.PoolContractID,
		ComponentKind: "lp_principal", AssetID: asset, AmountRaw: amount,
		ShareAmountRaw: shares, LedgerSeq: in.LedgerSeq, Timestamp: in.CloseTime,
		Metadata: metadata,
	}
}

// classifyEvent maps one contract event to (activity, quarantine). Exactly one
// of three outcomes: an activity under its exact on-chain name (pair AMM
// events only), recognized-to-nothing (factory / router / SEP-41 LP token
// echoes), or quarantine (anything else on an owned contract — never dropped).
//
// Topic shape, from source: the protocol's own events carry an ScString
// contract label as the FIRST topic ("SoroswapPair" / "SoroswapFactory" /
// "SoroswapRouter") and the event name Symbol as the SECOND; the SEP-41 token
// echoes use the standard Symbol-first-topic shape.
func (a *Adapter) classifyEvent(evt bindings.RawEventEnvelope) (*bindings.Activity, *bindings.QuarantineEvent) {
	if !a.OwnsContract(evt.ContractID) {
		return nil, nil
	}
	var ce xdr.ContractEvent
	if xdr.SafeUnmarshal(evt.RawEvent, &ce) != nil {
		return nil, a.quarantine(evt, "soroswap_event_undecodable")
	}
	v, ok := ce.Body.GetV0()
	if !ok || len(v.Topics) == 0 {
		return nil, a.quarantine(evt, "soroswap_event_empty_topics")
	}
	if label, ok := v.Topics[0].GetStr(); ok {
		name := ""
		if len(v.Topics) > 1 {
			if s, ok := v.Topics[1].GetSym(); ok {
				name = string(s)
			}
		}
		switch string(label) {
		case "SoroswapPair":
			if _, isActivity := pairActivityVocabulary[name]; isActivity {
				return a.pairActivity(evt, name, v.Data), nil
			}
			return nil, a.quarantine(evt, "soroswap_unknown_pair_event")
		case "SoroswapFactory":
			if _, known := factoryEventNames[name]; known {
				return nil, nil
			}
			return nil, a.quarantine(evt, "soroswap_unknown_factory_event")
		case "SoroswapRouter":
			if _, known := routerEventNames[name]; known {
				return nil, nil
			}
			return nil, a.quarantine(evt, "soroswap_unknown_router_event")
		}
		return nil, a.quarantine(evt, "soroswap_unknown_event_label")
	}
	if s, ok := v.Topics[0].GetSym(); ok {
		if _, echo := tokenEchoNames[string(s)]; echo {
			return nil, nil
		}
	}
	return nil, a.quarantine(evt, "soroswap_unknown_event")
}

// pairActivity builds the gold activity row for one pair event. Payloads are
// #[contracttype] structs (symbol-keyed maps). Every pair event is
// multi-legged (per-token amounts), so asset_id/amount stay absent and the
// ordered legs ride in metadata — never a fabricated aggregate. deposit /
// swap / withdraw carry the acting address in their `to` field; sync and skim
// have no actor by construction, so the pair contract itself is the address.
func (a *Adapter) pairActivity(evt bindings.RawEventEnvelope, name string, data xdr.ScVal) *bindings.Activity {
	f := fields(data)
	metadata := map[string]string{}
	for _, k := range []string{
		"amount_0", "amount_1", "liquidity",
		"amount_0_in", "amount_1_in", "amount_0_out", "amount_1_out",
		"new_reserve_0", "new_reserve_1", "skimmed_0", "skimmed_1",
	} {
		if v, ok := f[k]; ok {
			if n := i128String(v); n != "" {
				metadata[k] = n
			}
		}
	}
	address := ""
	if v, ok := f["to"]; ok {
		address = addr(v)
	}
	if address == "" {
		address = evt.ContractID
	}
	share := ""
	if v, ok := f["liquidity"]; ok {
		share = i128String(v)
	}
	return &bindings.Activity{
		ID:           stableID(a.cfg.Protocol, evt.LedgerSeq, evt.TxHash, evt.EventIndex, name),
		LedgerSeq:    evt.LedgerSeq,
		TxHash:       evt.TxHash,
		EventIndex:   evt.EventIndex,
		ContractID:   evt.ContractID,
		Address:      address,
		Protocol:     a.cfg.Protocol,
		ActivityType: contracts.ActivityType(name),
		ShareAmount:  share,
		Timestamp:    evt.CloseTime,
		Metadata:     metadata,
	}
}

func (a *Adapter) quarantine(evt bindings.RawEventEnvelope, reason string) *bindings.QuarantineEvent {
	return &bindings.QuarantineEvent{
		ID:         stableID(a.ID(), evt.LedgerSeq, evt.TxHash, evt.EventIndex, reason),
		AdapterID:  a.ID(),
		LedgerSeq:  evt.LedgerSeq,
		TxHash:     evt.TxHash,
		EventIndex: evt.EventIndex,
		ContractID: evt.ContractID,
		Reason:     reason,
		RawEvent:   evt.RawEvent,
	}
}
