package soroswap

// Event classification and lifecycle tests. Synthetic events are built to the
// exact topic/payload shapes read out of the pinned protocol sources
// (soroswap/core @ bb90a65): protocol events carry an ScString contract label
// as the first topic and the Symbol event name second; payloads are
// symbol-keyed maps; the pair's SEP-41 LP token echoes use the standard
// Symbol-first-topic shape.

import (
	"testing"
	"time"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// goldActivityTypeCheck is the frozen gold vocabulary — the CHECK constraint
// soroswap_activity_type_vocabulary in docs/gold-ddl/020_soroswap_gold.sql
// (relay.rs), copied name-for-name. The enumerating tests below pin the
// adapter's vocabulary to exactly this set, both directions.
var goldActivityTypeCheck = []string{"deposit", "swap", "withdraw", "sync", "skim"}

func scSym(s string) xdr.ScVal {
	sym := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
}

func scStr(s string) xdr.ScVal {
	str := xdr.ScString(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &str}
}

func scAddr(t *testing.T, id string) xdr.ScVal {
	t.Helper()
	var sa xdr.ScAddress
	switch id[0] {
	case 'C':
		raw := strkey.MustDecode(strkey.VersionByteContract, id)
		var cid xdr.ContractId
		copy(cid[:], raw)
		sa = xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
	case 'G':
		raw := strkey.MustDecode(strkey.VersionByteAccountID, id)
		var key xdr.Uint256
		copy(key[:], raw)
		acc := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &key}
		sa = xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &acc}
	default:
		t.Fatalf("bad address %q", id)
	}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &sa}
}

func scI128(lo int64) xdr.ScVal {
	p := xdr.Int128Parts{Hi: 0, Lo: xdr.Uint64(lo)}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &p}
}

func scMap(t *testing.T, kv ...any) xdr.ScVal {
	t.Helper()
	m := xdr.ScMap{}
	for i := 0; i+1 < len(kv); i += 2 {
		m = append(m, xdr.ScMapEntry{Key: scSym(kv[i].(string)), Val: kv[i+1].(xdr.ScVal)})
	}
	mp := &m
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp}
}

func syntheticEvent(t *testing.T, contractID string, topics []xdr.ScVal, data xdr.ScVal) bindings.RawEventEnvelope {
	t.Helper()
	raw := strkey.MustDecode(strkey.VersionByteContract, contractID)
	var cid xdr.ContractId
	copy(cid[:], raw)
	ce := xdr.ContractEvent{
		ContractId: &cid,
		Type:       xdr.ContractEventTypeContract,
		Body:       xdr.ContractEventBody{V: 0, V0: &xdr.ContractEventV0{Topics: topics, Data: data}},
	}
	b, err := ce.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return bindings.RawEventEnvelope{
		LedgerSeq:  63415332,
		TxHash:     "deadbeef",
		EventIndex: 0,
		ContractID: contractID,
		RawEvent:   b,
		CloseTime:  time.Unix(1753500000, 0).UTC(),
	}
}

func eventAdapter(t *testing.T) *Adapter {
	t.Helper()
	a, err := NewWithConfig(Config{
		Factories: map[string]struct{}{factoryID: {}},
		Routers:   map[string]struct{}{"CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH": {}},
	})
	if err != nil {
		t.Fatal(err)
	}
	a.RegisterPairContracts(cdlmPair)
	return a
}

// TestActivityVocabularyEnumeratesGoldCheck pins both directions: every name
// in the frozen gold CHECK classifies to an activity under exactly that name,
// and the adapter's vocabulary contains nothing else.
func TestActivityVocabularyEnumeratesGoldCheck(t *testing.T) {
	if len(pairActivityVocabulary) != len(goldActivityTypeCheck) {
		t.Fatalf("adapter vocabulary %v has %d names, gold CHECK has %d",
			pairActivityVocabulary, len(pairActivityVocabulary), len(goldActivityTypeCheck))
	}
	for _, name := range goldActivityTypeCheck {
		if _, ok := pairActivityVocabulary[name]; !ok {
			t.Fatalf("gold CHECK name %q missing from adapter vocabulary", name)
		}
	}
	a := eventAdapter(t)
	for _, name := range goldActivityTypeCheck {
		data := scMap(t, "new_reserve_0", scI128(1), "new_reserve_1", scI128(2))
		if name == "deposit" || name == "swap" || name == "withdraw" {
			data = scMap(t, "to", scAddr(t, gb77), "liquidity", scI128(7))
		}
		evt := syntheticEvent(t, cdlmPair, []xdr.ScVal{scStr("SoroswapPair"), scSym(name)}, data)
		out, err := a.Transform(bindings.TransformInput{LedgerSeq: 63415332, Events: []bindings.RawEventEnvelope{evt}})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Activities) != 1 || len(out.Quarantine) != 0 {
			t.Fatalf("%s: activities %d quarantine %d", name, len(out.Activities), len(out.Quarantine))
		}
		act := out.Activities[0]
		if string(act.ActivityType) != name {
			t.Fatalf("%s stored as %q — the gold CHECK requires the exact on-chain name", name, act.ActivityType)
		}
		// Multi-legged payloads: no fabricated single-asset aggregate.
		if act.AssetID != "" || act.AmountRaw != "" {
			t.Fatalf("%s fabricated asset/amount %#v", name, act)
		}
		if act.Address == "" {
			t.Fatalf("%s empty address", name)
		}
		if name == "sync" || name == "skim" {
			if act.Address != cdlmPair {
				t.Fatalf("%s address %q, want the pair itself", name, act.Address)
			}
		} else if act.Address != gb77 {
			t.Fatalf("%s address %q, want the payload `to`", name, act.Address)
		}
	}
}

func TestRecognizedNonActivitiesClassifyToNothing(t *testing.T) {
	a := eventAdapter(t)
	routerID := "CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH"
	var events []bindings.RawEventEnvelope
	for _, name := range []string{"init", "new_pair", "fee_to", "setter", "fees"} {
		events = append(events, syntheticEvent(t, factoryID, []xdr.ScVal{scStr("SoroswapFactory"), scSym(name)}, scMap(t)))
	}
	for _, name := range []string{"init", "add", "remove", "swap"} {
		events = append(events, syntheticEvent(t, routerID, []xdr.ScVal{scStr("SoroswapRouter"), scSym(name)}, scMap(t)))
	}
	// The pair's own SEP-41 LP token echoes: recognized, never activities,
	// never quarantine-spam (the AMM events carry the feed, the Balance writes
	// carry the positions).
	for _, name := range []string{"mint", "burn", "transfer", "approve"} {
		events = append(events, syntheticEvent(t, cdlmPair, []xdr.ScVal{scSym(name), scAddr(t, gb77)}, scI128(5)))
	}
	out, err := a.Transform(bindings.TransformInput{LedgerSeq: 63415332, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Activities) != 0 {
		t.Fatalf("recognized non-activities classified: %#v", out.Activities)
	}
	if len(out.Quarantine) != 0 {
		t.Fatalf("recognized non-activities quarantined: %#v", out.Quarantine)
	}
}

func TestUnknownEventsQuarantineNotDrop(t *testing.T) {
	a := eventAdapter(t)
	events := []bindings.RawEventEnvelope{
		// Unknown name under a known label.
		syntheticEvent(t, cdlmPair, []xdr.ScVal{scStr("SoroswapPair"), scSym("flashloan")}, scMap(t)),
		// Unknown label entirely.
		syntheticEvent(t, cdlmPair, []xdr.ScVal{scStr("NotSoroswap"), scSym("swap")}, scMap(t)),
		// Unknown bare-symbol event (not a token echo).
		syntheticEvent(t, cdlmPair, []xdr.ScVal{scSym("upgrade")}, scMap(t)),
	}
	out, err := a.Transform(bindings.TransformInput{LedgerSeq: 63415332, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Activities) != 0 {
		t.Fatalf("unknown events classified %#v", out.Activities)
	}
	if len(out.Quarantine) != 3 {
		t.Fatalf("quarantine %#v", out.Quarantine)
	}
	for i, q := range out.Quarantine {
		if len(q.RawEvent) == 0 {
			t.Fatalf("quarantine %d dropped the raw bytes", i)
		}
	}
	// Events on contracts this adapter does not own are not this adapter's to
	// judge — no activity, no quarantine.
	other := syntheticEvent(t, "CB46LMGJC7SYSH4C7SBNLV635OX5BSNQDGRR32NRXAV7N2AVNZMQUJ3A", []xdr.ScVal{scSym("anything")}, scMap(t))
	out, err = a.Transform(bindings.TransformInput{LedgerSeq: 63415332, Events: []bindings.RawEventEnvelope{other}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Activities) != 0 || len(out.Quarantine) != 0 {
		t.Fatalf("unowned contract event judged: %#v %#v", out.Activities, out.Quarantine)
	}
}

// TestHadSharesLifecycle walks deposit -> full withdraw -> tombstone: a
// position that closes emits explicit zero components exactly once-per-write,
// and a never-held zero position stays silent.
func TestHadSharesLifecycle(t *testing.T) {
	a := mainnetAdapter(t)
	instance := fixtureChange(t, cccdPair, "pubnet-L063750872-soroswap-pair-instance-cccd-layoutu32", "instance", "Updated")

	// Deposit: the golden LP's real Balance write.
	s1, err := a.DecodeState(nil, []bindings.ContractDataChange{
		instance,
		fixtureChange(t, cccdPair, "pubnet-L058465535-soroswap-pair-balance-cccd-gc7i", "persistent", "Created"),
	}, 58465535)
	if err != nil {
		t.Fatal(err)
	}
	if len(s1.AMMPositions) != 1 || !s1.AMMPositions[0].HadShares || s1.AMMPositions[0].SharesRaw != "1191193505547" {
		t.Fatalf("deposit state %#v", s1.AMMPositions)
	}

	// Full withdraw: the Balance entry is genuinely Removed on-chain.
	key := fixtureB64(t, "pubnet-L058465535-soroswap-pair-balance-cccd-gc7i-key.xdr")
	s2, err := a.DecodeState(s1, []bindings.ContractDataChange{{
		ContractID: cccdPair, KeyXDR: key, ValueXDR: nil,
		Durability: "persistent", ChangeType: "Removed", Live: false,
	}}, 58465600)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.AMMPositions) != 1 || s2.AMMPositions[0].SharesRaw != "0" || !s2.AMMPositions[0].HadShares {
		t.Fatalf("withdraw state %#v", s2.AMMPositions)
	}
	dirty := a.LastDirtyPositions()
	if len(dirty) != 1 || dirty[0].Kind != bindings.DirtyRemoval || dirty[0].Address != gc7i {
		t.Fatalf("removal dirty %#v", dirty)
	}

	// Tombstone: the transform writes explicit zero components for the close.
	out, err := a.Transform(bindings.TransformInput{LedgerSeq: 58465600, CloseTime: time.Unix(1753500000, 0).UTC(), State: s2})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.AMMComponents) != 2 {
		t.Fatalf("tombstones %#v", out.AMMComponents)
	}
	for _, c := range out.AMMComponents {
		if c.AmountRaw != "0" || c.ShareAmountRaw != "0" || c.Metadata["closed"] != "true" {
			t.Fatalf("bad tombstone %#v", c)
		}
	}

	// Never-held: a zero balance with no HadShares history stays silent.
	s2.AMMPositions[0].HadShares = false
	out, err = a.Transform(bindings.TransformInput{LedgerSeq: 58465601, CloseTime: time.Unix(1753500001, 0).UTC(), State: s2})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.AMMComponents) != 0 {
		t.Fatalf("never-held position emitted %#v", out.AMMComponents)
	}
}

func TestZeroTotalSharesQuarantinesNotGuesses(t *testing.T) {
	a := mainnetAdapter(t)
	s := &bindings.LedgerState{
		AMMPools: []bindings.AMMPoolState{{
			Protocol: "soroswap", ContractID: cdlmPair, PoolType: "constant_product",
			TotalSharesRaw: "0",
			Tokens:         []bindings.AMMTokenReserve{{AssetID: xlmSAC, ReserveRaw: "10"}, {AssetID: libreToken, ReserveRaw: "20"}},
		}},
		AMMPositions: []bindings.AMMPositionState{{Address: gb77, PoolContractID: cdlmPair, SharesRaw: "5", HadShares: true}},
	}
	out, err := a.Transform(bindings.TransformInput{LedgerSeq: 1, CloseTime: time.Unix(1, 0).UTC(), State: s})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.AMMComponents) != 0 {
		t.Fatalf("guessed components %#v", out.AMMComponents)
	}
	if len(out.Quarantine) != 2 {
		t.Fatalf("quarantine %#v", out.Quarantine)
	}
	for _, q := range out.Quarantine {
		if q.Reason != "invalid_lp_share_state" {
			t.Fatalf("reason %q", q.Reason)
		}
	}
}
