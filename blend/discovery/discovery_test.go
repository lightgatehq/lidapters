package discovery

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// factoryContract builds a contract id hash + its strkey form from a seed, so a
// test can both stamp an event's ContractId and register the factory.
func factoryContract(t *testing.T, seed byte) (xdr.ContractId, string) {
	t.Helper()
	var hash xdr.Hash
	hash[31] = seed
	id := xdr.ContractId(hash)
	str, err := strkey.Encode(strkey.VersionByteContract, id[:])
	if err != nil {
		t.Fatalf("encode factory contract id: %v", err)
	}
	return id, str
}

// deployEvent builds a PoolFactory `deploy` contract event emitted by factoryID
// whose value is value, using the given topic and event type so tests can flip
// each gate independently.
func deployEvent(t *testing.T, factoryID xdr.ContractId, topic, value xdr.ScVal, evtType xdr.ContractEventType) xdr.ContractEvent {
	t.Helper()
	body, err := xdr.NewContractEventBody(0, xdr.ContractEventV0{
		Topics: []xdr.ScVal{topic},
		Data:   value,
	})
	if err != nil {
		t.Fatalf("contract event body: %v", err)
	}
	fid := factoryID
	return xdr.ContractEvent{Type: evtType, ContractId: &fid, Body: body}
}

func TestDecodePoolFactoryDeploy(t *testing.T) {
	factoryID, factoryStr := factoryContract(t, 0x11)
	_, otherFactoryStr := factoryContract(t, 0x22)
	poolVal := contractAddressVal(t, 0x33)
	poolStr := scValAddress(poolVal)
	factories := map[string]struct{}{factoryStr: {}}
	deploySym := symbolVal(t, PoolFactoryDeployTopic)

	t.Run("known factory deploy yields pool", func(t *testing.T) {
		evt := deployEvent(t, factoryID, deploySym, poolVal, xdr.ContractEventTypeContract)
		got, ok := decodePoolFactoryDeploy(evt, factories)
		if !ok {
			t.Fatal("expected deploy to decode")
		}
		if got.PoolID != poolStr {
			t.Fatalf("pool id: got %s want %s", got.PoolID, poolStr)
		}
		if got.FactoryID != factoryStr {
			t.Fatalf("factory id: got %s want %s", got.FactoryID, factoryStr)
		}
	})

	t.Run("unknown factory is skipped", func(t *testing.T) {
		evt := deployEvent(t, factoryID, deploySym, poolVal, xdr.ContractEventTypeContract)
		if _, ok := decodePoolFactoryDeploy(evt, map[string]struct{}{otherFactoryStr: {}}); ok {
			t.Fatal("deploy from unregistered factory should be skipped")
		}
	})

	t.Run("non-deploy topic is skipped", func(t *testing.T) {
		evt := deployEvent(t, factoryID, symbolVal(t, "supply"), poolVal, xdr.ContractEventTypeContract)
		if _, ok := decodePoolFactoryDeploy(evt, factories); ok {
			t.Fatal("non-deploy topic should be skipped")
		}
	})

	t.Run("system event type is skipped", func(t *testing.T) {
		evt := deployEvent(t, factoryID, deploySym, poolVal, xdr.ContractEventTypeSystem)
		if _, ok := decodePoolFactoryDeploy(evt, factories); ok {
			t.Fatal("non-contract event type should be skipped")
		}
	})

	t.Run("account-address value is skipped", func(t *testing.T) {
		// The deploy value must be a contract address; an account address is not a
		// pool and must not be enumerated.
		evt := deployEvent(t, factoryID, deploySym, accountAddressVal(t, 0x44), xdr.ContractEventTypeContract)
		if _, ok := decodePoolFactoryDeploy(evt, factories); ok {
			t.Fatal("account-address value should be skipped")
		}
	})

	t.Run("multiple topics is skipped", func(t *testing.T) {
		body, err := xdr.NewContractEventBody(0, xdr.ContractEventV0{
			Topics: []xdr.ScVal{deploySym, symbolVal(t, "extra")},
			Data:   poolVal,
		})
		if err != nil {
			t.Fatalf("event body: %v", err)
		}
		fid := factoryID
		evt := xdr.ContractEvent{Type: xdr.ContractEventTypeContract, ContractId: &fid, Body: body}
		if _, ok := decodePoolFactoryDeploy(evt, factories); ok {
			t.Fatal("deploy event carries exactly one topic; extra topics should be skipped")
		}
	})

	t.Run("nil factories yields nothing", func(t *testing.T) {
		evt := deployEvent(t, factoryID, deploySym, poolVal, xdr.ContractEventTypeContract)
		if _, ok := decodePoolFactoryDeploy(evt, nil); ok {
			t.Fatal("no registered factories should decode nothing")
		}
	})
}

func TestDiscoverPoolsFromMeta(t *testing.T) {
	factoryID, factoryStr := factoryContract(t, 0x11)
	factories := map[string]struct{}{factoryStr: {}}

	poolA := contractAddressVal(t, 0xA1)
	poolB := contractAddressVal(t, 0xB2)
	poolAStr := scValAddress(poolA)
	poolBStr := scValAddress(poolB)

	events := []xdr.ContractEvent{
		// Two real deploys from the factory.
		deployEvent(t, factoryID, symbolVal(t, PoolFactoryDeployTopic), poolA, xdr.ContractEventTypeContract),
		deployEvent(t, factoryID, symbolVal(t, PoolFactoryDeployTopic), poolB, xdr.ContractEventTypeContract),
		// A duplicate deploy of poolA — must dedupe.
		deployEvent(t, factoryID, symbolVal(t, PoolFactoryDeployTopic), poolA, xdr.ContractEventTypeContract),
		// A non-deploy event from the factory — must be ignored.
		deployEvent(t, factoryID, symbolVal(t, "supply"), poolA, xdr.ContractEventTypeContract),
	}

	want := []string{poolAStr, poolBStr}
	if poolAStr > poolBStr {
		want = []string{poolBStr, poolAStr}
	}

	// The deploy events land in TransactionMetaV3 on protocol 20–22 ledgers and in
	// TransactionMetaV4 on protocol 23+; a scan from a mainnet deploy floor
	// spans both, so both layouts must enumerate identically.
	for _, meta := range []string{"v3", "v4"} {
		t.Run(meta, func(t *testing.T) {
			rawMeta := buildCloseMeta(t, 987654, "deadbeef", events, meta)
			pools, err := DiscoverPoolsFromMeta(rawMeta, factories)
			if err != nil {
				t.Fatalf("discover pools: %v", err)
			}
			if len(pools) != 2 {
				t.Fatalf("expected 2 discovered pools, got %d: %+v", len(pools), pools)
			}
			for i, p := range pools {
				if p.PoolID != want[i] {
					t.Fatalf("pool[%d]: got %s want %s", i, p.PoolID, want[i])
				}
				if p.FactoryID != factoryStr {
					t.Fatalf("pool[%d] factory: got %s want %s", i, p.FactoryID, factoryStr)
				}
				if p.LedgerSeq != 987654 {
					t.Fatalf("pool[%d] ledger: got %d want 987654", i, p.LedgerSeq)
				}
				if p.TxHash == "" {
					t.Fatalf("pool[%d]: expected a tx hash", i)
				}
			}
		})
	}
}

func TestDiscoverPoolsFromMetaEmptyFactorySet(t *testing.T) {
	factoryID, _ := factoryContract(t, 0x11)
	events := []xdr.ContractEvent{
		deployEvent(t, factoryID, symbolVal(t, PoolFactoryDeployTopic), contractAddressVal(t, 0xA1), xdr.ContractEventTypeContract),
	}
	rawMeta := buildCloseMeta(t, 100, "abc123", events, "v4")

	pools, err := DiscoverPoolsFromMeta(rawMeta, nil)
	if err != nil {
		t.Fatalf("discover pools: %v", err)
	}
	if len(pools) != 0 {
		t.Fatalf("empty factory set should discover no pools, got %d", len(pools))
	}
}

// buildCloseMeta assembles a V1 LedgerCloseMeta with a single transaction whose
// apply meta (V3 or V4) carries the given contract events, so the whole bronze
// raw_meta scan path (unmarshal -> per-tx event walk -> decode) is exercised end
// to end. The scan reads events and the tx hash straight from TxProcessing, so
// no TxSet or signed envelope is needed.
func buildCloseMeta(t *testing.T, ledgerSeq uint32, txHashHex string, events []xdr.ContractEvent, metaVersion string) []byte {
	t.Helper()

	var meta xdr.TransactionMeta
	var err error
	switch metaVersion {
	case "v3":
		meta, err = xdr.NewTransactionMeta(3, xdr.TransactionMetaV3{
			SorobanMeta: &xdr.SorobanTransactionMeta{
				Events:      events,
				ReturnValue: xdr.ScVal{Type: xdr.ScValTypeScvVoid},
			},
		})
	case "v4":
		meta, err = xdr.NewTransactionMeta(4, xdr.TransactionMetaV4{
			Operations: []xdr.OperationMetaV2{{Events: events}},
		})
	default:
		t.Fatalf("unknown meta version %q", metaVersion)
	}
	if err != nil {
		t.Fatalf("transaction meta: %v", err)
	}

	var txHash xdr.Hash
	copy(txHash[:], txHashHex)

	resultResult, err := xdr.NewTransactionResultResult(xdr.TransactionResultCodeTxSuccess, []xdr.OperationResult{})
	if err != nil {
		t.Fatalf("transaction result: %v", err)
	}

	lcm := xdr.LedgerCloseMeta{
		V: 1,
		V1: &xdr.LedgerCloseMetaV1{
			LedgerHeader: xdr.LedgerHeaderHistoryEntry{
				Header: xdr.LedgerHeader{LedgerVersion: 23, LedgerSeq: xdr.Uint32(ledgerSeq)},
			},
			TxSet: xdr.GeneralizedTransactionSet{
				V:       1,
				V1TxSet: &xdr.TransactionSetV1{Phases: []xdr.TransactionPhase{}},
			},
			TxProcessing: []xdr.TransactionResultMeta{{
				Result: xdr.TransactionResultPair{
					TransactionHash: txHash,
					Result:          xdr.TransactionResult{Result: resultResult},
				},
				TxApplyProcessing: meta,
			}},
		},
	}

	raw, err := lcm.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal close meta: %v", err)
	}
	return raw
}
