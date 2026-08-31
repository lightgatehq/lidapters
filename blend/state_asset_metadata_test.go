package blend

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/lightgatehq/lidapters/blend/contracts"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// --- scval builders specific to asset-metadata fixtures ---------------------

func stringVal(t *testing.T, raw string) xdr.ScVal {
	t.Helper()
	value, err := xdr.NewScVal(xdr.ScValTypeScvString, xdr.ScString(raw))
	if err != nil {
		t.Fatalf("string: %v", err)
	}
	return value
}

func bytesVal(t *testing.T, raw []byte) xdr.ScVal {
	t.Helper()
	value, err := xdr.NewScVal(xdr.ScValTypeScvBytes, xdr.ScBytes(raw))
	if err != nil {
		t.Fatalf("bytes: %v", err)
	}
	return value
}

// issuerBytesVal returns the raw 32-byte Ed25519 public key ScVal for a G...
// account address, matching a SAC's AlphaNum4/12 issuer field (BytesN<32>, not
// an ScAddress).
func issuerBytesVal(t *testing.T, accountID string) xdr.ScVal {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteAccountID, accountID)
	if err != nil {
		t.Fatalf("decode account id %s: %v", accountID, err)
	}
	return bytesVal(t, raw)
}

// sacAssetInfoVal builds a Stellar Asset Contract's instance ScVal (no wasm
// executable — a SAC has none) whose storage carries the AssetInfo entry, the
// same shape applyPoolInstanceStorage reads a pool's Config/Backstop from.
func sacAssetInfoVal(t *testing.T, assetInfo xdr.ScVal) xdr.ScVal {
	t.Helper()
	storage := xdr.ScMap{
		{Key: variantVal(t, "AssetInfo"), Val: assetInfo},
	}
	return xdr.ScVal{
		Type: xdr.ScValTypeScvContractInstance,
		Instance: &xdr.ScContractInstance{
			Executable: xdr.ContractExecutable{Type: xdr.ContractExecutableTypeContractExecutableStellarAsset},
			Storage:    &storage,
		},
	}
}

// sep41MetadataInstanceVal builds a wasm-backed SEP-41 token's instance ScVal
// whose storage carries the METADATA entry (soroban-token-sdk TokenMetadata).
func sep41MetadataInstanceVal(t *testing.T, metadata xdr.ScVal) xdr.ScVal {
	t.Helper()
	var wasm xdr.Hash
	wasm[31] = 9
	storage := xdr.ScMap{
		{Key: symbolVal(t, "METADATA"), Val: metadata},
	}
	return xdr.ScVal{
		Type: xdr.ScValTypeScvContractInstance,
		Instance: &xdr.ScContractInstance{
			Executable: xdr.ContractExecutable{
				Type:     xdr.ContractExecutableTypeContractExecutableWasm,
				WasmHash: &wasm,
			},
			Storage: &storage,
		},
	}
}

func newAssetTestAdapter(t *testing.T, assetID string) *Adapter {
	t.Helper()
	adapter, err := New(Config{AllowUnknownV2: true})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	adapter.RegisterAssetContracts(assetID)
	return adapter
}

func findAsset(state *bindings.LedgerState, contractID string) (contracts.AssetMetadata, bool) {
	for _, a := range state.Assets {
		if a.ContractID == contractID {
			return a, true
		}
	}
	return contracts.AssetMetadata{}, false
}

func instanceKeyVal() xdr.ScVal {
	return xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance}
}

// TestDecodeState_SACNativeAssetInfo covers the XLM-shaped SAC: Native carries
// no asset_code/issuer, and both symbol and name are the literal "native" per
// the contract's own symbol()/name() behavior; decimals is always 7.
func TestDecodeState_SACNativeAssetInfo(t *testing.T) {
	t.Parallel()

	assetID := validContractString(t, 100)
	adapter := newAssetTestAdapter(t, assetID)

	state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		stateChange(t, assetID, instanceKeyVal(), sacAssetInfoVal(t, variantVal(t, "Native"))),
	}, 100)
	if err != nil {
		t.Fatalf("decode state: %v", err)
	}
	meta, ok := findAsset(state, assetID)
	if !ok {
		t.Fatalf("expected asset %s decoded, got none (state.Assets=%+v)", assetID, state.Assets)
	}
	if meta.Symbol != "native" || meta.Name != "native" || meta.Decimals != 7 {
		t.Fatalf("native asset info: got %+v", meta)
	}
}

// TestDecodeState_SACAlphaNum4AssetInfo covers a 4-char classic asset code
// (USDC): symbol is the code, name is "CODE:GISSUER…", decimals is 7.
func TestDecodeState_SACAlphaNum4AssetInfo(t *testing.T) {
	t.Parallel()

	assetID := validContractString(t, 101)
	issuer := validAccountString(t, 102)
	adapter := newAssetTestAdapter(t, assetID)

	assetInfo := variantVal(t, "AlphaNum4", mapVal(t, map[string]xdr.ScVal{
		"asset_code": stringVal(t, "USDC"),
		"issuer":     issuerBytesVal(t, issuer),
	}))
	state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		stateChange(t, assetID, instanceKeyVal(), sacAssetInfoVal(t, assetInfo)),
	}, 100)
	if err != nil {
		t.Fatalf("decode state: %v", err)
	}
	meta, ok := findAsset(state, assetID)
	if !ok {
		t.Fatalf("expected asset %s decoded", assetID)
	}
	if meta.Symbol != "USDC" {
		t.Fatalf("symbol: got %q want %q", meta.Symbol, "USDC")
	}
	if want := "USDC:" + issuer; meta.Name != want {
		t.Fatalf("name: got %q want %q", meta.Name, want)
	}
	if meta.Decimals != 7 {
		t.Fatalf("decimals: got %d want 7", meta.Decimals)
	}
}

// TestDecodeState_SACAlphaNum12AssetInfo covers a >4-char classic asset code.
func TestDecodeState_SACAlphaNum12AssetInfo(t *testing.T) {
	t.Parallel()

	assetID := validContractString(t, 103)
	issuer := validAccountString(t, 104)
	adapter := newAssetTestAdapter(t, assetID)

	assetInfo := variantVal(t, "AlphaNum12", mapVal(t, map[string]xdr.ScVal{
		"asset_code": stringVal(t, "LONGASSETCOD"),
		"issuer":     issuerBytesVal(t, issuer),
	}))
	state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		stateChange(t, assetID, instanceKeyVal(), sacAssetInfoVal(t, assetInfo)),
	}, 100)
	if err != nil {
		t.Fatalf("decode state: %v", err)
	}
	meta, ok := findAsset(state, assetID)
	if !ok {
		t.Fatalf("expected asset %s decoded", assetID)
	}
	if meta.Symbol != "LONGASSETCOD" {
		t.Fatalf("symbol: got %q want %q", meta.Symbol, "LONGASSETCOD")
	}
	if want := "LONGASSETCOD:" + issuer; meta.Name != want {
		t.Fatalf("name: got %q want %q", meta.Name, want)
	}
}

// TestDecodeState_SEP41Metadata covers a custom SEP-41 token's METADATA entry
// (soroban-token-sdk TokenMetadata), including a non-7 decimals value — proving
// decimals is read off the entry rather than hardcoded like the SAC path.
func TestDecodeState_SEP41Metadata(t *testing.T) {
	t.Parallel()

	assetID := validContractString(t, 105)
	adapter := newAssetTestAdapter(t, assetID)

	metadata := mapVal(t, map[string]xdr.ScVal{
		"decimal": u32Val(18),
		"name":    stringVal(t, "Wrapped Ether"),
		"symbol":  stringVal(t, "wETH"),
	})
	state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		stateChange(t, assetID, instanceKeyVal(), sep41MetadataInstanceVal(t, metadata)),
	}, 100)
	if err != nil {
		t.Fatalf("decode state: %v", err)
	}
	meta, ok := findAsset(state, assetID)
	if !ok {
		t.Fatalf("expected asset %s decoded", assetID)
	}
	if meta.Symbol != "wETH" || meta.Name != "Wrapped Ether" || meta.Decimals != 18 {
		t.Fatalf("sep-41 metadata: got %+v", meta)
	}
}

// TestDecodeState_UnknownAssetLayoutDecodesToNothing proves a malformed or
// unrecognized layout on a registered asset contract decodes to nothing —
// never a guessed value. Covers a missing issuer field and an unrecognized
// AssetInfo variant name.
func TestDecodeState_UnknownAssetLayoutDecodesToNothing(t *testing.T) {
	t.Parallel()

	cases := map[string]xdr.ScVal{
		"missing_issuer": sacAssetInfoVal(t, variantVal(t, "AlphaNum4", mapVal(t, map[string]xdr.ScVal{
			"asset_code": stringVal(t, "USDC"),
		}))),
		"unrecognized_variant": sacAssetInfoVal(t, variantVal(t, "Bogus")),
	}
	for name, instanceVal := range cases {
		t.Run(name, func(t *testing.T) {
			assetID := validContractString(t, 106)
			adapter := newAssetTestAdapter(t, assetID)
			state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
				stateChange(t, assetID, instanceKeyVal(), instanceVal),
			}, 100)
			if err != nil {
				t.Fatalf("decode state: %v", err)
			}
			if _, ok := findAsset(state, assetID); ok {
				t.Fatalf("expected no decoded asset for %s, got one", name)
			}
		})
	}
}

// TestDecodeState_RegisteredAssetNeverBecomesPhantomPool is the hazard the
// issue calls out: a wasm-backed SEP-41 token's instance carries a wasm hash
// and would pass the generic pool branch's wasm-hash sniff. A contract
// registered as an asset must never reach that branch, decode success or not.
func TestDecodeState_RegisteredAssetNeverBecomesPhantomPool(t *testing.T) {
	t.Parallel()

	assetID := validContractString(t, 107)
	adapter := newAssetTestAdapter(t, assetID)

	// Storage carries an unrelated key (not AssetInfo/METADATA) so decode misses,
	// but the instance still has a wasm executable — exactly the hazard case.
	var wasm xdr.Hash
	wasm[31] = 11
	storage := xdr.ScMap{{Key: symbolVal(t, "Admin"), Val: addressVal(t, validContractString(t, 108))}}
	instanceVal := xdr.ScVal{
		Type: xdr.ScValTypeScvContractInstance,
		Instance: &xdr.ScContractInstance{
			Executable: xdr.ContractExecutable{Type: xdr.ContractExecutableTypeContractExecutableWasm, WasmHash: &wasm},
			Storage:    &storage,
		},
	}

	state, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		stateChange(t, assetID, instanceKeyVal(), instanceVal),
	}, 100)
	if err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if hasPool(state, assetID) {
		t.Fatalf("registered asset contract %s was folded as a phantom pool", assetID)
	}
	if _, ok := findAsset(state, assetID); ok {
		t.Fatalf("expected no decoded asset metadata (unrecognized storage), got one")
	}
}

// TestDecodeState_AssetMetadataCarriesAcrossLedgers proves the decoded
// identity survives a later ledger where the instance is not re-seen — it is
// written once at deploy, so this is the asset analog of the oracle carry
// regression.
func TestDecodeState_AssetMetadataCarriesAcrossLedgers(t *testing.T) {
	t.Parallel()

	assetID := validContractString(t, 109)
	adapter := newAssetTestAdapter(t, assetID)

	priorN, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		stateChange(t, assetID, instanceKeyVal(), sacAssetInfoVal(t, variantVal(t, "Native"))),
	}, 100)
	if err != nil {
		t.Fatalf("decode ledger N: %v", err)
	}
	if _, ok := findAsset(priorN, assetID); !ok {
		t.Fatalf("expected asset decoded at ledger N")
	}

	// Ledger N+1 — no changes at all for this contract. The carried metadata must
	// still be present in the returned state.
	stateN1, err := adapter.DecodeState(priorN, nil, 101)
	if err != nil {
		t.Fatalf("decode ledger N+1: %v", err)
	}
	meta, ok := findAsset(stateN1, assetID)
	if !ok || meta.Symbol != "native" {
		t.Fatalf("expected asset metadata to carry to ledger N+1, got %+v (ok=%v)", meta, ok)
	}
}

// TestDecodeState_AssetMetadataClearedOnInstanceEviction proves the decoded
// identity is dropped when the instance itself is evicted/TTL-lapses — a
// not-live instance is no longer backed by live on-chain state.
func TestDecodeState_AssetMetadataClearedOnInstanceEviction(t *testing.T) {
	t.Parallel()

	assetID := validContractString(t, 110)
	adapter := newAssetTestAdapter(t, assetID)

	priorN, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		stateChange(t, assetID, instanceKeyVal(), sacAssetInfoVal(t, variantVal(t, "Native"))),
	}, 100)
	if err != nil {
		t.Fatalf("decode ledger N: %v", err)
	}

	evict := stateChange(t, assetID, instanceKeyVal(), boolVal(false), withLive(false))
	stateN1, err := adapter.DecodeState(priorN, []bindings.ContractDataChange{evict}, 101)
	if err != nil {
		t.Fatalf("decode ledger N+1: %v", err)
	}
	if _, ok := findAsset(stateN1, assetID); ok {
		t.Fatalf("expected evicted asset metadata to be cleared")
	}
}

// TestDecodeState_AssetMetadataUnaffectedByBalanceEntryDelete proves a
// registered asset's OTHER persistent-storage deletes (e.g. a user's Balance
// entry going to zero) do not clobber the decoded identity — only the
// instance entry itself going not-live does.
func TestDecodeState_AssetMetadataUnaffectedByBalanceEntryDelete(t *testing.T) {
	t.Parallel()

	assetID := validContractString(t, 111)
	adapter := newAssetTestAdapter(t, assetID)

	priorN, err := adapter.DecodeState(nil, []bindings.ContractDataChange{
		stateChange(t, assetID, instanceKeyVal(), sacAssetInfoVal(t, variantVal(t, "Native"))),
	}, 100)
	if err != nil {
		t.Fatalf("decode ledger N: %v", err)
	}

	balanceKey := variantVal(t, "Balance", accountAddressVal(t, 112))
	evict := stateChange(t, assetID, balanceKey, boolVal(false), withLive(false))
	stateN1, err := adapter.DecodeState(priorN, []bindings.ContractDataChange{evict}, 101)
	if err != nil {
		t.Fatalf("decode ledger N+1: %v", err)
	}
	if _, ok := findAsset(stateN1, assetID); !ok {
		t.Fatalf("expected asset metadata unaffected by an unrelated balance-entry delete")
	}
}

// TestDecodeState_AssetMetadataDeterministic proves folding the same input
// twice yields byte-identical LedgerState.Assets — no map-iteration-order leak.
func TestDecodeState_AssetMetadataDeterministic(t *testing.T) {
	t.Parallel()

	assetA := validContractString(t, 113)
	assetB := validContractString(t, 114)
	adapter, err := New(Config{AllowUnknownV2: true})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	adapter.RegisterAssetContracts(assetA, assetB)

	changes := []bindings.ContractDataChange{
		stateChange(t, assetA, instanceKeyVal(), sacAssetInfoVal(t, variantVal(t, "Native"))),
		stateChange(t, assetB, instanceKeyVal(), sep41MetadataInstanceVal(t, mapVal(t, map[string]xdr.ScVal{
			"decimal": u32Val(7),
			"name":    stringVal(t, "Wrapped Bitcoin"),
			"symbol":  stringVal(t, "wBTC"),
		}))),
	}

	state1, err := adapter.DecodeState(nil, changes, 100)
	if err != nil {
		t.Fatalf("decode 1: %v", err)
	}
	state2, err := adapter.DecodeState(nil, changes, 100)
	if err != nil {
		t.Fatalf("decode 2: %v", err)
	}
	b1, _ := json.Marshal(state1.Assets)
	b2, _ := json.Marshal(state2.Assets)
	if !bytes.Equal(b1, b2) {
		t.Fatalf("asset metadata fold not byte-identical across two runs:\n%s\n%s", b1, b2)
	}
}
