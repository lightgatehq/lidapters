package aquarius

// Enumerating tests for the exact per-wasm event vocabulary. The era tables
// in transform.go mirror relay.rs deployments/aquarius.pubnet.toml; the rows
// here are an independent transcription of the same file, so any drift
// between code and the deployment data fails one-to-one. The activity subset
// must additionally match the frozen aquarius_activities CHECK constraint
// (relay.rs docs/gold-ddl/019_aquarius_gold.sql) exactly — the blend lesson:
// substring classification silently quarantined real events; exact names or
// nothing.

import (
	"bytes"
	"testing"
	"time"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// goldActivityVocabulary is the aquarius_activities CHECK constraint from
// 019_aquarius_gold.sql, transcribed character-exactly.
var goldActivityVocabulary = []string{
	// pool-level LP ops (liquidity_pool_events vocabulary)
	"deposit_liquidity", "withdraw_liquidity", "trade",
	"update_reserves", "pool_state",
	// router-level ops (liquidity_pool_router events vocabulary)
	"deposit", "withdraw", "swap", "add_pool", "claim",
	// reward / fee ops observed across wasm eras
	"claim_reward", "claim_fees", "config_rewards",
	// concentrated range mutation
	"position_update",
}

// tomlEraRows transcribes every [[era]] row of aquarius.pubnet.toml:
// (class, event, activity, from_ledger).
var tomlEraRows = []struct {
	class    string
	event    string
	activity bool
	from     int64
}{
	{"router", "add_pool", true, 52728530},
	{"router", "deposit", true, 52728548},
	{"router", "swap", true, 52728694},
	{"router", "withdraw", true, 52728753},
	{"router", "claim", true, 53554212},
	{"router", "config_rewards", true, 53788765},
	{"router", "commit_transfer_ownership", false, 55363698},
	{"router", "apply_transfer_ownership", false, 55363729},
	{"router", "set_privileged_addrs", false, 55363632},
	{"router", "commit_upgrade", false, 56429464},
	{"router", "apply_upgrade", false, 56505099},
	{"router", "set_protocol_fee", false, 57711843},
	{"router", "pool_gauge_switch_token", false, 58787667},
	{"pool_cp", "deposit_liquidity", true, 52728548},
	{"pool_cp", "trade", true, 52728694},
	{"pool_cp", "withdraw_liquidity", true, 52728753},
	{"pool_cp", "claim_reward", true, 55364200},
	{"pool_cp", "update_reserves", true, 57724992},
	{"pool_cp", "kill_claim", false, 53446148},
	{"pool_cp", "unkill_claim", false, 53553379},
	{"pool_cp", "set_privileged_addrs", false, 55363632},
	{"pool_cp", "commit_transfer_ownership", false, 55363698},
	{"pool_cp", "apply_transfer_ownership", false, 55363729},
	{"pool_cp", "set_rewards_config", false, 55363850},
	{"pool_cp", "commit_upgrade", false, 56429489},
	{"pool_cp", "apply_upgrade", false, 56505152},
	{"pool_cp", "set_protocol_fee", false, 57725741},
	{"pool_cp", "claim_protocol_fee", false, 57812875},
	{"pool_cp", "rewards_gauge_add", false, 58961340},
	{"pool_cp", "rewards_gauge_schedule_reward", false, 58961340},
	{"pool_cp", "rewards_gauge_claim", false, 58961606},
	{"pool_cp", "reserves_sync", false, 62338454},
	{"pool_cp", "set_rewards_state", false, 62460053},
	{"pool_stable", "deposit_liquidity", true, 53721288},
	{"pool_stable", "trade", true, 53752457},
	{"pool_stable", "withdraw_liquidity", true, 54357611},
	{"pool_stable", "claim_reward", true, 55364553},
	{"pool_stable", "update_reserves", true, 57711655},
	{"pool_stable", "set_privileged_addrs", false, 55363632},
	{"pool_stable", "commit_transfer_ownership", false, 55363698},
	{"pool_stable", "apply_transfer_ownership", false, 55363729},
	{"pool_stable", "set_rewards_config", false, 55363874},
	{"pool_stable", "commit_upgrade", false, 56429464},
	{"pool_stable", "apply_upgrade", false, 56505116},
	{"pool_stable", "set_protocol_fee", false, 57711843},
	{"pool_stable", "claim_protocol_fee", false, 58023965},
	{"pool_concentrated", "deposit_liquidity", true, 62341770},
	{"pool_concentrated", "pool_state", true, 62341770},
	{"pool_concentrated", "position_update", true, 62341770},
	{"pool_concentrated", "update_reserves", true, 62341770},
	{"pool_concentrated", "trade", true, 62341787},
	{"pool_concentrated", "withdraw_liquidity", true, 62342165},
	{"pool_concentrated", "claim_fees", true, 62343467},
	{"pool_concentrated", "claim_reward", true, 62350500},
	{"pool_concentrated", "commit_upgrade", false, 62429301},
	{"pool_concentrated", "claim_protocol_fee", false, 62440492},
	{"pool_concentrated", "apply_upgrade", false, 62475394},
	{"pool_concentrated", "enable_emergency_mode", false, 62877434},
	{"pool_concentrated", "disable_emergency_mode", false, 62877804},
	{"pool_concentrated", "set_rewards_state", false, 63082253},
	{"share_token", "mint", false, 53553264},
	{"share_token", "burn", false, 53555897},
	{"share_token", "transfer", false, 53738883},
	{"share_token", "approve", false, 60433173},
}

func TestEventEraTablesMatchDeploymentDataOneToOne(t *testing.T) {
	total := 0
	for _, row := range tomlEraRows {
		era, ok := eventErasByClass[row.class][row.event]
		if !ok {
			t.Errorf("(%s, %s) in deployment data but not in the era tables", row.class, row.event)
			continue
		}
		if era.activity != row.activity || era.fromLedger != row.from {
			t.Errorf("(%s, %s) = {activity %v, from %d}, deployment data says {activity %v, from %d}",
				row.class, row.event, era.activity, era.fromLedger, row.activity, row.from)
		}
		total++
	}
	inCode := 0
	for _, eras := range eventErasByClass {
		inCode += len(eras)
	}
	if inCode != len(tomlEraRows) {
		t.Errorf("era tables carry %d rows, deployment data has %d — a row exists in code only", inCode, len(tomlEraRows))
	}
}

func TestActivityVocabularyMatchesGoldCheckExactly(t *testing.T) {
	gold := map[string]struct{}{}
	for _, name := range goldActivityVocabulary {
		gold[name] = struct{}{}
	}
	if len(gold) != len(goldActivityVocabulary) {
		t.Fatal("duplicate name in the transcribed CHECK vocabulary")
	}
	served := map[string]struct{}{}
	for class, eras := range eventErasByClass {
		for name, era := range eras {
			if !era.activity {
				continue
			}
			served[name] = struct{}{}
			if _, ok := gold[name]; !ok {
				t.Errorf("(%s, %s) is served as an activity but is outside the frozen CHECK vocabulary", class, name)
			}
		}
	}
	for name := range gold {
		if _, ok := served[name]; !ok {
			t.Errorf("CHECK vocabulary name %q is served by no era table row", name)
		}
	}
}

func aquariusEventRaw(t *testing.T, topics []xdr.ScVal, data xdr.ScVal) []byte {
	t.Helper()
	body, err := xdr.NewContractEventBody(0, xdr.ContractEventV0{Topics: topics, Data: data})
	if err != nil {
		t.Fatalf("contract event body: %v", err)
	}
	var raw bytes.Buffer
	if _, err := xdr.Marshal(&raw, xdr.ContractEvent{Type: xdr.ContractEventTypeContract, Body: body}); err != nil {
		t.Fatalf("marshal contract event: %v", err)
	}
	return raw.Bytes()
}

func symVal(t *testing.T, s string) xdr.ScVal {
	t.Helper()
	sym := xdr.ScSymbol(s)
	v, err := xdr.NewScVal(xdr.ScValTypeScvSymbol, sym)
	if err != nil {
		t.Fatalf("symbol scval: %v", err)
	}
	return v
}

func accountVal(t *testing.T, seed byte) xdr.ScVal {
	t.Helper()
	var raw xdr.Uint256
	raw[31] = seed
	account, err := xdr.NewAccountId(xdr.PublicKeyTypePublicKeyTypeEd25519, raw)
	if err != nil {
		t.Fatalf("account id: %v", err)
	}
	address, err := xdr.NewScAddress(xdr.ScAddressTypeScAddressTypeAccount, account)
	if err != nil {
		t.Fatalf("account address: %v", err)
	}
	v, err := xdr.NewScVal(xdr.ScValTypeScvAddress, address)
	if err != nil {
		t.Fatalf("address scval: %v", err)
	}
	return v
}

func i128Data(t *testing.T, n int64) xdr.ScVal {
	t.Helper()
	v, err := xdr.NewScVal(xdr.ScValTypeScvI128, xdr.Int128Parts{Hi: 0, Lo: xdr.Uint64(n)})
	if err != nil {
		t.Fatalf("i128 scval: %v", err)
	}
	return v
}

func vocabTransformInput(t *testing.T, poolType string, evt bindings.RawEventEnvelope) bindings.TransformInput {
	t.Helper()
	return bindings.TransformInput{
		LedgerSeq: evt.LedgerSeq,
		CloseTime: evt.CloseTime,
		Events:    []bindings.RawEventEnvelope{evt},
		State: &bindings.LedgerState{AMMPools: []bindings.AMMPoolState{{
			Protocol: "aquarius", ContractID: evt.ContractID, PoolType: poolType,
			Tokens: []bindings.AMMTokenReserve{{AssetID: "a", ReserveRaw: "10"}, {AssetID: "b", ReserveRaw: "20"}},
		}}},
	}
}

func vocabEvent(t *testing.T, contractID, name string, ledgerSeq int64, withUser bool) bindings.RawEventEnvelope {
	t.Helper()
	topics := []xdr.ScVal{symVal(t, name)}
	if withUser {
		topics = append(topics, accountVal(t, 9))
	}
	return bindings.RawEventEnvelope{
		LedgerSeq:  ledgerSeq,
		TxHash:     "tx1",
		EventIndex: 0,
		ContractID: contractID,
		CloseTime:  time.Unix(1785596600, 0).UTC(),
		RawEvent:   aquariusEventRaw(t, topics, i128Data(t, 41)),
	}
}

// TestExactNameEmission pins that an in-vocabulary event emits its EXACT
// on-chain name as the activity type — no translation layer (the old matcher
// rewrote deposit_liquidity to add_liquidity, which gold's CHECK rejects).
func TestExactNameEmission(t *testing.T) {
	a, err := NewWithConfig(Config{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Transform(vocabTransformInput(t, "constant_product", vocabEvent(t, "pool", "deposit_liquidity", 62000000, true)))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Activities) != 1 || len(out.Quarantine) != 0 {
		t.Fatalf("activities %#v quarantine %#v", out.Activities, out.Quarantine)
	}
	act := out.Activities[0]
	if string(act.ActivityType) != "deposit_liquidity" {
		t.Fatalf("activity type %q, want the exact on-chain name", act.ActivityType)
	}
	// deposit_liquidity has no per-user attribution shape: the activity lands
	// under the emitting pool itself, with no scavenged asset or amount.
	if act.Address != "pool" || act.AssetID != "" || act.AmountRaw != "" {
		t.Fatalf("activity payload %#v", act)
	}
}

func TestOutOfVocabularyEventQuarantines(t *testing.T) {
	a, err := NewWithConfig(Config{})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bindings.RawEventEnvelope{
		// Unknown name for the class.
		"aquarius_event_unknown_name": vocabEvent(t, "pool", "yeet_liquidity", 62000000, true),
		// Known name, but observed before its era floor (claim_reward on a
		// CP pool predating ledger 55364200).
		"aquarius_event_before_era_floor": vocabEvent(t, "pool", "claim_reward", 55000000, true),
		// Exact means case-sensitive: near-miss spellings never classify.
		"aquarius_event_unknown_name/case": vocabEvent(t, "pool", "Deposit_Liquidity", 62000000, true),
	}
	for label, evt := range cases {
		out, err := a.Transform(vocabTransformInput(t, "constant_product", evt))
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Activities) != 0 {
			t.Errorf("%s: emitted %#v", label, out.Activities)
		}
		if len(out.Quarantine) != 1 {
			t.Errorf("%s: quarantine %#v", label, out.Quarantine)
			continue
		}
		want := label
		if i := len("aquarius_event_unknown_name"); len(label) > i && label[:i] == "aquarius_event_unknown_name" {
			want = "aquarius_event_unknown_name"
		}
		if out.Quarantine[0].Reason != want {
			t.Errorf("%s: reason %q", label, out.Quarantine[0].Reason)
		}
	}
}

func TestUnclassifiableContractEventQuarantines(t *testing.T) {
	a, err := NewWithConfig(Config{})
	if err != nil {
		t.Fatal(err)
	}
	// A pool whose type never affirmatively decoded has no era table; its
	// events must not classify by guesswork.
	out, err := a.Transform(vocabTransformInput(t, "volatile", vocabEvent(t, "pool", "deposit_liquidity", 62000000, true)))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Activities) != 0 || len(out.Quarantine) != 1 || out.Quarantine[0].Reason != "aquarius_event_unclassified_contract" {
		t.Fatalf("activities %#v quarantine %#v", out.Activities, out.Quarantine)
	}
}

func TestRecognizedNonActivityIsExcludedByDecision(t *testing.T) {
	a, err := NewWithConfig(Config{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Transform(vocabTransformInput(t, "constant_product", vocabEvent(t, "pool", "set_rewards_config", 62000000, true)))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Activities) != 0 || len(out.Quarantine) != 0 {
		t.Fatalf("recognized non-activity must be excluded silently by the table's decision: %#v %#v", out.Activities, out.Quarantine)
	}
}

func TestRouterEventsClassifyThroughRouterTable(t *testing.T) {
	a, err := NewWithConfig(Config{Routers: map[string]struct{}{"router": {}}})
	if err != nil {
		t.Fatal(err)
	}
	evt := vocabEvent(t, "router", "swap", 62000000, false)
	out, err := a.Transform(bindings.TransformInput{LedgerSeq: evt.LedgerSeq, CloseTime: evt.CloseTime, Events: []bindings.RawEventEnvelope{evt}, State: &bindings.LedgerState{}})
	if err != nil {
		t.Fatal(err)
	}
	// swap is pool-scope: no wallet topic is fine, and the exact name flows.
	if len(out.Activities) != 1 || string(out.Activities[0].ActivityType) != "swap" || len(out.Quarantine) != 0 {
		t.Fatalf("activities %#v quarantine %#v", out.Activities, out.Quarantine)
	}
}
