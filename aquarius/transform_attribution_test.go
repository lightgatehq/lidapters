package aquarius

// Per-name activity attribution tests, pinned against the shapes the chain
// emits (one shape per name, sampled from the mainnet event stream — the
// same observations the cross-engine reference encodes). Wallet and asset
// slots are fixed topic/data positions; amounts are fixed data-vector
// indices. A name with no acting wallet attributes to the emitting contract
// itself — never an empty address, never a same-transaction bystander.

import (
	"testing"
	"time"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

func contractVal(t *testing.T, seed byte) (xdr.ScVal, string) {
	t.Helper()
	var raw xdr.ContractId
	raw[31] = seed
	address, err := xdr.NewScAddress(xdr.ScAddressTypeScAddressTypeContract, raw)
	if err != nil {
		t.Fatalf("contract address: %v", err)
	}
	v, err := xdr.NewScVal(xdr.ScValTypeScvAddress, address)
	if err != nil {
		t.Fatalf("address scval: %v", err)
	}
	return v, strkey.MustEncode(strkey.VersionByteContract, raw[:])
}

func accountStrkey(seed byte) string {
	var raw [32]byte
	raw[31] = seed
	return strkey.MustEncode(strkey.VersionByteAccountID, raw[:])
}

func rawEventOn(t *testing.T, contractID string, ledgerSeq int64, topics []xdr.ScVal, data xdr.ScVal) bindings.RawEventEnvelope {
	t.Helper()
	return bindings.RawEventEnvelope{
		LedgerSeq:  ledgerSeq,
		TxHash:     "tx1",
		EventIndex: 0,
		ContractID: contractID,
		CloseTime:  time.Unix(1785596600, 0).UTC(),
		RawEvent:   aquariusEventRaw(t, topics, data),
	}
}

func singleActivity(t *testing.T, in bindings.TransformInput, a *Adapter) bindings.Activity {
	t.Helper()
	out, err := a.Transform(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Activities) != 1 || len(out.Quarantine) != 0 {
		t.Fatalf("activities %#v quarantine %#v", out.Activities, out.Quarantine)
	}
	return out.Activities[0]
}

// TestPoolTradeAttribution: topics [trade, token_in, token_out, initiator];
// data [amount_in, amount_out, fee]. The initiator is recorded as written —
// a router-initiated trade attributes to the router contract, never to a
// wallet scavenged from elsewhere in the transaction.
func TestPoolTradeAttribution(t *testing.T) {
	a, err := NewWithConfig(Config{})
	if err != nil {
		t.Fatal(err)
	}
	tokenIn, tokenInID := contractVal(t, 1)
	tokenOut, _ := contractVal(t, 2)
	data := vecVal(t, i128Data(t, 514545659), i128Data(t, 3000000005), i128Data(t, 514546))

	user := rawEventOn(t, "pool", 63415290, []xdr.ScVal{symVal(t, "trade"), tokenIn, tokenOut, accountVal(t, 9)}, data)
	act := singleActivity(t, vocabTransformInput(t, "constant_product", user), a)
	if act.Address != accountStrkey(9) || act.AssetID != tokenInID || act.AmountRaw != "514545659" {
		t.Fatalf("user trade %#v", act)
	}

	router, routerID := contractVal(t, 7)
	viaRouter := rawEventOn(t, "pool", 63415290, []xdr.ScVal{symVal(t, "trade"), tokenIn, tokenOut, router}, data)
	act = singleActivity(t, vocabTransformInput(t, "constant_product", viaRouter), a)
	if act.Address != routerID || act.AssetID != tokenInID || act.AmountRaw != "514545659" {
		t.Fatalf("router-initiated trade %#v", act)
	}
}

// TestPoolLifecycleEventsAttributeToTheContract: update_reserves, pool_state
// and the pool LP legs carry no acting wallet in their observed shapes. They
// land under the emitting contract with no asset and no amount — the data
// payload's scalars are reserve/state values, not a token amount.
func TestPoolLifecycleEventsAttributeToTheContract(t *testing.T) {
	a, err := NewWithConfig(Config{})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		poolType, name string
	}{
		{"constant_product", "update_reserves"},
		{"concentrated", "pool_state"},
		{"constant_product", "deposit_liquidity"},
		{"constant_product", "withdraw_liquidity"},
	}
	for _, c := range cases {
		// A user topic is present on the LP legs' transactions elsewhere; the
		// event itself carries reserve scalars only.
		evt := rawEventOn(t, "pool", 63415290, []xdr.ScVal{symVal(t, c.name)}, vecVal(t, u128ScVal(t, 43048952672876), u128ScVal(t, 21837045747626409)))
		act := singleActivity(t, vocabTransformInput(t, c.poolType, evt), a)
		if act.Address != "pool" || act.AssetID != "" || act.AmountRaw != "" {
			t.Fatalf("%s: %#v", c.name, act)
		}
	}
}

// TestRouterSwapAttribution: topics [swap, tokens-vec, user]; data
// [pool, token_in, token_out, in_amount, out_amount]. The traded asset is
// data[1] and the amount data[3] — data[0] is the POOL address and must
// never leak into asset_id.
func TestRouterSwapAttribution(t *testing.T) {
	a, err := NewWithConfig(Config{Routers: map[string]struct{}{"router": {}}})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := contractVal(t, 3)
	tokenIn, tokenInID := contractVal(t, 1)
	tokenOut, _ := contractVal(t, 2)
	tokensTopic := vecVal(t, tokenIn, tokenOut)
	data := vecVal(t, pool, tokenIn, tokenOut, u128ScVal(t, 81039263), u128ScVal(t, 80430452))
	evt := rawEventOn(t, "router", 63415290, []xdr.ScVal{symVal(t, "swap"), tokensTopic, accountVal(t, 9)}, data)
	in := bindings.TransformInput{LedgerSeq: evt.LedgerSeq, CloseTime: evt.CloseTime, Events: []bindings.RawEventEnvelope{evt}, State: &bindings.LedgerState{}}
	act := singleActivity(t, in, a)
	if act.Address != accountStrkey(9) || act.AssetID != tokenInID || act.AmountRaw != "81039263" {
		t.Fatalf("router swap %#v", act)
	}
}

// TestRouterClaimAttribution: topics [claim, tokens-vec, user]; data
// [pool, reward_token, amount].
func TestRouterClaimAttribution(t *testing.T) {
	a, err := NewWithConfig(Config{Routers: map[string]struct{}{"router": {}}})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := contractVal(t, 3)
	aqua, aquaID := contractVal(t, 4)
	evt := rawEventOn(t, "router", 63554300, []xdr.ScVal{symVal(t, "claim"), vecVal(t), accountVal(t, 9)}, vecVal(t, pool, aqua, i128Data(t, 15797042587)))
	in := bindings.TransformInput{LedgerSeq: evt.LedgerSeq, CloseTime: evt.CloseTime, Events: []bindings.RawEventEnvelope{evt}, State: &bindings.LedgerState{}}
	act := singleActivity(t, in, a)
	if act.Address != accountStrkey(9) || act.AssetID != aquaID || act.AmountRaw != "15797042587" {
		t.Fatalf("router claim %#v", act)
	}
}

// TestPoolClaimRewardAttribution: topics [claim_reward, reward_token, owner];
// data [amount].
func TestPoolClaimRewardAttribution(t *testing.T) {
	a, err := NewWithConfig(Config{})
	if err != nil {
		t.Fatal(err)
	}
	aqua, aquaID := contractVal(t, 4)
	evt := rawEventOn(t, "pool", 63415290, []xdr.ScVal{symVal(t, "claim_reward"), aqua, accountVal(t, 9)}, vecVal(t, i128Data(t, 15797042587)))
	act := singleActivity(t, vocabTransformInput(t, "constant_product", evt), a)
	if act.Address != accountStrkey(9) || act.AssetID != aquaID || act.AmountRaw != "15797042587" {
		t.Fatalf("claim_reward %#v", act)
	}
}

// TestConcentratedOwnerOnlyShapes: position_update and claim_fees carry the
// owner in topic 1; their data legs (tick bounds / two per-token fee
// amounts) have no single token amount, so both stay empty.
func TestConcentratedOwnerOnlyShapes(t *testing.T) {
	a, err := NewWithConfig(Config{})
	if err != nil {
		t.Fatal(err)
	}
	update := rawEventOn(t, "pool", 63415290, []xdr.ScVal{symVal(t, "position_update"), accountVal(t, 9)}, vecVal(t, i32ScVal(t, -21880), i32ScVal(t, -7980), i128Data(t, 23911740265087)))
	act := singleActivity(t, vocabTransformInput(t, "concentrated", update), a)
	if act.Address != accountStrkey(9) || act.AssetID != "" || act.AmountRaw != "" {
		t.Fatalf("position_update %#v", act)
	}
	token0, _ := contractVal(t, 1)
	token1, _ := contractVal(t, 2)
	claim := rawEventOn(t, "pool", 63415290, []xdr.ScVal{symVal(t, "claim_fees"), accountVal(t, 9), token0, token1}, vecVal(t, i128Data(t, 78216730), i128Data(t, 369599)))
	act = singleActivity(t, vocabTransformInput(t, "concentrated", claim), a)
	if act.Address != accountStrkey(9) || act.AssetID != "" || act.AmountRaw != "" {
		t.Fatalf("claim_fees %#v", act)
	}
}

// TestRouterDepositWithdrawAttribution: topics [name, tokens-vec, user] —
// the user attributes, the multi-leg amounts stay unattributed.
func TestRouterDepositWithdrawAttribution(t *testing.T) {
	a, err := NewWithConfig(Config{Routers: map[string]struct{}{"router": {}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"deposit", "withdraw"} {
		evt := rawEventOn(t, "router", 63415290, []xdr.ScVal{symVal(t, name), vecVal(t), accountVal(t, 9)}, vecVal(t, u128ScVal(t, 5), u128ScVal(t, 6)))
		in := bindings.TransformInput{LedgerSeq: evt.LedgerSeq, CloseTime: evt.CloseTime, Events: []bindings.RawEventEnvelope{evt}, State: &bindings.LedgerState{}}
		act := singleActivity(t, in, a)
		if act.Address != accountStrkey(9) || act.AssetID != "" || act.AmountRaw != "" {
			t.Fatalf("router %s %#v", name, act)
		}
	}
}

// TestAmountShapeGuard: only i128, and u128 within i128 range, qualify as an
// amount. A u128 beyond i128 (or a non-numeric value at the amount slot) is
// a shape change and yields an honest empty, never a truncated number.
func TestAmountShapeGuard(t *testing.T) {
	a, err := NewWithConfig(Config{Routers: map[string]struct{}{"router": {}}})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := contractVal(t, 3)
	tokenIn, _ := contractVal(t, 1)
	tokenOut, _ := contractVal(t, 2)
	huge, err := xdr.NewScVal(xdr.ScValTypeScvU128, xdr.UInt128Parts{Hi: xdr.Uint64(1) << 63, Lo: 0})
	if err != nil {
		t.Fatal(err)
	}
	data := vecVal(t, pool, tokenIn, tokenOut, huge, u128ScVal(t, 80430452))
	evt := rawEventOn(t, "router", 63415290, []xdr.ScVal{symVal(t, "swap"), vecVal(t), accountVal(t, 9)}, data)
	in := bindings.TransformInput{LedgerSeq: evt.LedgerSeq, CloseTime: evt.CloseTime, Events: []bindings.RawEventEnvelope{evt}, State: &bindings.LedgerState{}}
	act := singleActivity(t, in, a)
	if act.AmountRaw != "" {
		t.Fatalf("u128 beyond i128 range must not attribute an amount: %#v", act)
	}
}
