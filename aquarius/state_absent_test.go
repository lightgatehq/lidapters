package aquarius

// Absent-is-not-zero assertions for the aquarius share/tick keying — the
// same defect class as blend's reserve-index zero-default collision
// (lidapters#33): a Go zero value must never masquerade as an observed
// on-chain zero. Shares: "" (never observed) is distinct from "0" (observed
// close). Ticks: a concentrated Position key without decodable bounds must
// refuse to fold rather than collide onto a guessed (0, 0) range, and the
// refusal must be loud (a decode diagnostic), never a silent drop.

import (
	"bytes"
	"encoding/base64"
	"testing"
	"time"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/stellar/go-stellar-sdk/xdr"
)

func b64ScVal(t *testing.T, v xdr.ScVal) string {
	t.Helper()
	var raw bytes.Buffer
	if _, err := xdr.Marshal(&raw, v); err != nil {
		t.Fatalf("marshal scval: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw.Bytes())
}

func vecVal(t *testing.T, items ...xdr.ScVal) xdr.ScVal {
	t.Helper()
	vec := xdr.ScVec(items)
	v, err := xdr.NewScVal(xdr.ScValTypeScvVec, &vec)
	if err != nil {
		t.Fatalf("vec scval: %v", err)
	}
	return v
}

func mapEntriesVal(t *testing.T, pairs ...[2]xdr.ScVal) xdr.ScVal {
	t.Helper()
	entries := make(xdr.ScMap, 0, len(pairs))
	for _, p := range pairs {
		entries = append(entries, xdr.ScMapEntry{Key: p[0], Val: p[1]})
	}
	v, err := xdr.NewScVal(xdr.ScValTypeScvMap, &entries)
	if err != nil {
		t.Fatalf("map scval: %v", err)
	}
	return v
}

func i32ScVal(t *testing.T, n int32) xdr.ScVal {
	t.Helper()
	v, err := xdr.NewScVal(xdr.ScValTypeScvI32, xdr.Int32(n))
	if err != nil {
		t.Fatalf("i32 scval: %v", err)
	}
	return v
}

func u128ScVal(t *testing.T, n uint64) xdr.ScVal {
	t.Helper()
	v, err := xdr.NewScVal(xdr.ScValTypeScvU128, xdr.UInt128Parts{Hi: 0, Lo: xdr.Uint64(n)})
	if err != nil {
		t.Fatalf("u128 scval: %v", err)
	}
	return v
}

func entryWrite(t *testing.T, contractID string, key, val xdr.ScVal) bindings.ContractDataChange {
	t.Helper()
	v := b64ScVal(t, val)
	return bindings.ContractDataChange{ContractID: contractID, KeyXDR: b64ScVal(t, key), ValueXDR: &v, Live: true}
}

// TestConcentratedPositionWithoutTickBoundsRefusesLoudly is the aquarius
// analogue of the #33 rule: refuse, don't guess. A Position entry whose key
// carries no (tick_lower, tick_upper) never folds — a guessed (0, 0) range
// would collide distinct ranges of one owner — and the refusal surfaces as a
// decode diagnostic.
func TestConcentratedPositionWithoutTickBoundsRefusesLoudly(t *testing.T) {
	a, err := NewWithConfig(Config{})
	if err != nil {
		t.Fatal(err)
	}
	a.RegisterContracts("pool")
	owner := accountVal(t, 3)
	goodKey := vecVal(t, symVal(t, "Position"), owner, i32ScVal(t, -40), i32ScVal(t, 40))
	tickless := vecVal(t, symVal(t, "Position"), owner)
	positionVal := mapEntriesVal(t, [2]xdr.ScVal{symVal(t, "liquidity"), u128ScVal(t, 777)})

	state, err := a.DecodeState(nil, []bindings.ContractDataChange{
		entryWrite(t, "pool", goodKey, positionVal),
		entryWrite(t, "pool", tickless, positionVal),
	}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.AMMPositions) != 1 {
		t.Fatalf("want only the fully-keyed range, got %#v", state.AMMPositions)
	}
	pos := state.AMMPositions[0]
	if pos.TickLower != -40 || pos.TickUpper != 40 || pos.LiquidityRaw != "777" {
		t.Fatalf("keyed range corrupted by the refused entry: %#v", pos)
	}
	diags := a.LastDecodeDiagnostics()
	if len(diags) != 1 || diags[0].Code != diagMissingTickBounds {
		t.Fatalf("refusal must be loud: %#v", diags)
	}
	if diags[0].PoolContractID != "pool" || diags[0].LedgerSeq != 100 {
		t.Fatalf("diagnostic coordinates %#v", diags[0])
	}
	// The next fold overwrites the diagnostics, mirroring the provider
	// contract.
	if _, err := a.DecodeState(state, nil, 101); err != nil {
		t.Fatal(err)
	}
	if len(a.LastDecodeDiagnostics()) != 0 {
		t.Fatal("diagnostics must reflect the most recent fold only")
	}
}

func instanceWrite(t *testing.T, contractID string, storage xdr.ScMap) bindings.ContractDataChange {
	t.Helper()
	exec, err := xdr.NewContractExecutable(xdr.ContractExecutableTypeContractExecutableStellarAsset, nil)
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	val, err := xdr.NewScVal(xdr.ScValTypeScvContractInstance, xdr.ScContractInstance{Executable: exec, Storage: &storage})
	if err != nil {
		t.Fatalf("instance scval: %v", err)
	}
	key := xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance}
	return entryWrite(t, contractID, key, val)
}

// TestMidRampAmplificationIsAbsent pins the settled-ramp rule for stable
// amplification: the pool has ONE amplification only while InitialA equals
// FutureA. Mid-ramp there is no single value, so the field must empty —
// carrying either endpoint (or a stale settled value) would state an
// amplification the chain never settled on.
func TestMidRampAmplificationIsAbsent(t *testing.T) {
	a, err := NewWithConfig(Config{})
	if err != nil {
		t.Fatal(err)
	}
	a.RegisterContracts("pool")
	settled := xdr.ScMap{
		{Key: vecVal(t, symVal(t, "pool_type")), Val: symVal(t, "stable")},
		{Key: vecVal(t, symVal(t, "InitialA")), Val: u128ScVal(t, 1500)},
		{Key: vecVal(t, symVal(t, "FutureA")), Val: u128ScVal(t, 1500)},
	}
	state, err := a.DecodeState(nil, []bindings.ContractDataChange{instanceWrite(t, "pool", settled)}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.AMMPools) != 1 || state.AMMPools[0].AmplificationRaw != "1500" {
		t.Fatalf("settled ramp %#v", state.AMMPools)
	}
	ramping := xdr.ScMap{
		{Key: vecVal(t, symVal(t, "pool_type")), Val: symVal(t, "stable")},
		{Key: vecVal(t, symVal(t, "InitialA")), Val: u128ScVal(t, 1500)},
		{Key: vecVal(t, symVal(t, "FutureA")), Val: u128ScVal(t, 3000)},
	}
	state, err = a.DecodeState(state, []bindings.ContractDataChange{instanceWrite(t, "pool", ramping)}, 101)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.AMMPools[0].AmplificationRaw; got != "" {
		t.Fatalf("mid-ramp amplification %q, want absent (the prior settled value must not survive)", got)
	}
}

// TestSharesAbsentIsNotZero pins the transform's share semantics: "" (never
// observed in the folded window) emits nothing regardless of lifecycle,
// while an observed "0" emits close tombstones only for a position that
// actually held shares.
func TestSharesAbsentIsNotZero(t *testing.T) {
	a, err := NewWithConfig(Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool := bindings.AMMPoolState{Protocol: "aquarius", ContractID: "pool", PoolType: "constant_product", TotalSharesRaw: "300", Tokens: []bindings.AMMTokenReserve{{AssetID: "a", ReserveRaw: "100"}, {AssetID: "b", ReserveRaw: "200"}}}
	cases := []struct {
		name       string
		pos        bindings.AMMPositionState
		components int
	}{
		{"absent shares, no lifecycle", bindings.AMMPositionState{Address: "u", PoolContractID: "pool"}, 0},
		{"absent shares, HadShares", bindings.AMMPositionState{Address: "u", PoolContractID: "pool", HadShares: true}, 0},
		{"observed zero, HadShares", bindings.AMMPositionState{Address: "u", PoolContractID: "pool", SharesRaw: "0", HadShares: true}, 2},
		{"observed zero, never held", bindings.AMMPositionState{Address: "u", PoolContractID: "pool", SharesRaw: "0"}, 0},
	}
	for _, c := range cases {
		out, err := a.Transform(bindings.TransformInput{LedgerSeq: 9, CloseTime: time.Unix(9, 0).UTC(), State: &bindings.LedgerState{
			AMMPools:     []bindings.AMMPoolState{pool},
			AMMPositions: []bindings.AMMPositionState{c.pos},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.AMMComponents) != c.components {
			t.Errorf("%s: %d components, want %d (%#v)", c.name, len(out.AMMComponents), c.components, out.AMMComponents)
		}
	}
}

// TestRangeLiquidityAbsentIsNotZero pins the same distinction for
// concentrated positions: absent liquidity stays silent even for a position
// that once held some; only an observed zero write tombstones.
func TestRangeLiquidityAbsentIsNotZero(t *testing.T) {
	a, err := NewWithConfig(Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool := bindings.AMMPoolState{Protocol: "aquarius", ContractID: "pool", PoolType: "concentrated", SqrtPriceX96: "79228162514264337593543950336", Tokens: []bindings.AMMTokenReserve{{AssetID: "a"}, {AssetID: "b"}}}
	base := bindings.AMMPositionState{Address: "u", PoolContractID: "pool", TickLower: -40, TickUpper: 40, HadShares: true}
	absent := base
	observedZero := base
	observedZero.LiquidityRaw = "0"

	out, err := a.Transform(bindings.TransformInput{LedgerSeq: 9, CloseTime: time.Unix(9, 0).UTC(), State: &bindings.LedgerState{
		AMMPools:     []bindings.AMMPoolState{pool},
		AMMPositions: []bindings.AMMPositionState{absent},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.AMMComponents) != 0 {
		t.Fatalf("absent liquidity emitted components: %#v", out.AMMComponents)
	}
	out, err = a.Transform(bindings.TransformInput{LedgerSeq: 10, CloseTime: time.Unix(10, 0).UTC(), State: &bindings.LedgerState{
		AMMPools:     []bindings.AMMPoolState{pool},
		AMMPositions: []bindings.AMMPositionState{observedZero},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.AMMComponents) != 2 {
		t.Fatalf("observed zero with lifecycle must tombstone both legs: %#v", out.AMMComponents)
	}
	for _, c := range out.AMMComponents {
		if c.AmountRaw != "0" || c.Metadata["closed"] != "true" || c.TickLower == nil {
			t.Fatalf("bad range tombstone %#v", c)
		}
	}
}
