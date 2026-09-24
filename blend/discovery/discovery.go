// Package discovery is event-based Blend pool discovery. It lives in the
// protocol adapter's tree rather than in the caller, so a protocol stays
// self-contained: event decode, state decode, transform, and pool enumeration
// all ship with the adapter. It is free functions with no Adapter coupling.
//
// Blend deploys each pool through a PoolFactory contract. Enumeration cannot
// read the factory's *instance storage*: on mainnet the PoolFactoryV2 records
// each deployed pool as a SEPARATE persistent ledger entry keyed
// PoolFactoryDataKey::Contracts(<pool address>) — the instance entry holds none
// of them, so an instance-storage read finds nothing on mainnet.
// The one signal emitted on every deployment, on both testnet and mainnet, is
// the factory's `deploy` CONTRACT EVENT, whose value is the new pool address.
// So enumeration is event-based: scan a ledger's raw close-meta for
// PoolFactory `deploy` events and collect their pool addresses.
package discovery

import (
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// PoolFactoryDeployTopic is the single-symbol topic the Blend PoolFactory (both
// V1 and V2) publishes when it deploys a pool. The event carries exactly this
// one topic; the deployed pool address is the event value. The deploy EVENT
// shape is identical across factory versions (only the deploy FUNCTION's
// arguments differ), so one decoder enumerates testnet and mainnet alike.
const PoolFactoryDeployTopic = "deploy"

// DiscoveredPool is one pool enumerated from a PoolFactory `deploy` event, with
// the provenance a caller needs to register and record it: the factory that
// emitted the deploy, and the ledger + transaction it was deployed in.
type DiscoveredPool struct {
	PoolID    string
	FactoryID string
	LedgerSeq int64
	TxHash    string
}

// DiscoverPoolsFromMeta scans one ledger's raw close-meta for PoolFactory
// `deploy` events emitted by any factory in factoryIDs and returns the deployed
// pool addresses. It is the event-based enumeration that replaces reading the
// factory's instance storage, so it works on BOTH testnet and mainnet (see the
// package comment for why instance-storage reads miss mainnet pools).
//
// rawMeta is one ledger's XDR-encoded LedgerCloseMeta. It reads contract events
// straight out of the per-transaction apply meta — no network passphrase, no
// RPC, no transaction-envelope pairing — so it stays a pure function of its
// inputs. The result is deduplicated and sorted by pool address so repeated
// scans of the same meta are byte-identical. An empty factory set returns no
// pools (there is nothing to attribute a deploy to).
func DiscoverPoolsFromMeta(rawMeta []byte, factoryIDs map[string]struct{}) ([]DiscoveredPool, error) {
	if len(factoryIDs) == 0 {
		return nil, nil
	}

	var lcm xdr.LedgerCloseMeta
	if err := xdr.SafeUnmarshal(rawMeta, &lcm); err != nil {
		return nil, fmt.Errorf("discover pools: decode LedgerCloseMeta: %w", err)
	}
	return DiscoverPoolsFromLedgerCloseMeta(lcm, factoryIDs)
}

// DiscoverPoolsFromLedgerCloseMeta is the decoded-input form, for a caller that
// has already decoded the LedgerCloseMeta. It keeps discovery pure while
// avoiding a second XDR decode.
func DiscoverPoolsFromLedgerCloseMeta(lcm xdr.LedgerCloseMeta, factoryIDs map[string]struct{}) ([]DiscoveredPool, error) {
	if len(factoryIDs) == 0 {
		return nil, nil
	}
	ledgerSeq := int64(lcm.LedgerSequence())

	seen := map[string]DiscoveredPool{}
	for i := 0; i < lcm.CountTransactions(); i++ {
		txHash := lcm.TransactionHash(i)
		hexHash := hex.EncodeToString(txHash[:])
		for _, evt := range contractEventsFromMeta(lcm.TxApplyProcessing(i)) {
			pool, ok := decodePoolFactoryDeploy(evt, factoryIDs)
			if !ok {
				continue
			}
			pool.LedgerSeq = ledgerSeq
			pool.TxHash = hexHash
			// First deploy of a given pool address wins; a re-emit in a later tx of
			// the same ledger cannot change the address it already recorded.
			if _, dup := seen[pool.PoolID]; !dup {
				seen[pool.PoolID] = pool
			}
		}
	}

	out := make([]DiscoveredPool, 0, len(seen))
	for _, pool := range seen {
		out = append(out, pool)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PoolID < out[j].PoolID })
	return out, nil
}

// contractEventsFromMeta returns the contract events a transaction emitted,
// across the two meta layouts a Blend ledger scan encounters: TransactionMetaV3
// (protocol 20–22) keeps them under SorobanMeta.Events, while TransactionMetaV4
// (protocol 23+ unified events, CAP-67) moves them into per-operation Events. A
// scan from a mainnet deploy floor to the present spans both, so both are
// handled; classic pre-Soroban meta (V1/V2) carries no contract events.
func contractEventsFromMeta(meta xdr.TransactionMeta) []xdr.ContractEvent {
	switch meta.V {
	case 3:
		if meta.V3 != nil && meta.V3.SorobanMeta != nil {
			return meta.V3.SorobanMeta.Events
		}
	case 4:
		if meta.V4 == nil {
			return nil
		}
		var out []xdr.ContractEvent
		for _, op := range meta.V4.Operations {
			out = append(out, op.Events...)
		}
		return out
	}
	return nil
}

// decodePoolFactoryDeploy decodes a single contract event as a Blend PoolFactory
// `deploy`. It returns the deployed pool address only when every gate holds: the
// event is a CONTRACT event (not a system/diagnostic event), it was emitted by a
// contract in factoryIDs, its single topic is the `deploy` symbol, and its value
// is a contract address. Anything else yields ok=false, so unrelated events
// (token transfers, pool supply/borrow, other contracts' events) are skipped.
func decodePoolFactoryDeploy(evt xdr.ContractEvent, factoryIDs map[string]struct{}) (DiscoveredPool, bool) {
	if evt.Type != xdr.ContractEventTypeContract || evt.ContractId == nil {
		return DiscoveredPool{}, false
	}
	factoryID, err := strkey.Encode(strkey.VersionByteContract, evt.ContractId[:])
	if err != nil {
		return DiscoveredPool{}, false
	}
	if _, ok := factoryIDs[factoryID]; !ok {
		return DiscoveredPool{}, false
	}

	v0, ok := evt.Body.GetV0()
	if !ok || len(v0.Topics) != 1 {
		return DiscoveredPool{}, false
	}
	if scValSymbol(v0.Topics[0]) != PoolFactoryDeployTopic {
		return DiscoveredPool{}, false
	}

	poolID := scValAddress(v0.Data)
	if !validContractAddress(poolID) {
		return DiscoveredPool{}, false
	}
	return DiscoveredPool{PoolID: poolID, FactoryID: factoryID}, true
}
