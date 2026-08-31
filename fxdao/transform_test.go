package fxdao

// Snapshot-only surface tests: this adapter registers no activity vocabulary,
// and any event on the vaults contract — whatever its shape — quarantines as
// an anomaly. There is no activity code path to exercise, only to prove
// absent.

import (
	"testing"
	"time"

	"github.com/lightgatehq/lidapters/bindings"
)

func TestAnyEventQuarantinesAsAnomaly(t *testing.T) {
	a := testAdapter(t)
	events := []bindings.RawEventEnvelope{
		// Arbitrary bytes: the reason for quarantine is the event's existence,
		// not its decodability — the contract's source has no publish call.
		{LedgerSeq: 62448349, TxHash: "aa", EventIndex: 0, ContractID: vaultsContract, RawEvent: []byte{0x00}},
		{LedgerSeq: 62448349, TxHash: "aa", EventIndex: 1, ContractID: vaultsContract, RawEvent: []byte("anything")},
	}
	out, err := a.Transform(bindings.TransformInput{LedgerSeq: 62448349, CloseTime: time.Unix(1778093393, 0).UTC(), Events: events})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Activities) != 0 {
		t.Fatalf("snapshot-only surface produced activities: %#v", out.Activities)
	}
	if len(out.Quarantine) != 2 {
		t.Fatalf("quarantine %#v", out.Quarantine)
	}
	for _, q := range out.Quarantine {
		if q.Reason != "fxdao_unexpected_event" || len(q.RawEvent) == 0 {
			t.Fatalf("anomaly record %#v", q)
		}
	}
	// Events on contracts this adapter does not own are not its anomalies.
	out, err = a.Transform(bindings.TransformInput{LedgerSeq: 62448349, Events: []bindings.RawEventEnvelope{
		{ContractID: "CA4HEQTL2WPEUYKYKCDOHCDNIV4QHNJ7EL4J4NQ6VADP7SYHVRYZ7AW2", RawEvent: []byte{0x01}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Quarantine) != 0 || len(out.Activities) != 0 {
		t.Fatalf("unowned event judged %#v %#v", out.Quarantine, out.Activities)
	}
}

func TestNeverSeenVaultStaysSilent(t *testing.T) {
	a := testAdapter(t)
	s := &bindings.LedgerState{Vaults: []bindings.VaultState{{
		Protocol: "fxdao", ContractID: vaultsContract,
		Account: gdvz, Denomination: "USD",
		// No HadVault: nothing proved this vault ever existed live on-chain.
	}}}
	out, err := a.Transform(bindings.TransformInput{LedgerSeq: 1, CloseTime: time.Unix(1, 0).UTC(), State: s})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Vaults) != 0 {
		t.Fatalf("never-seen vault emitted %#v", out.Vaults)
	}
}
