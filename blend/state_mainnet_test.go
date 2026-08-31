package blend

// Mainnet decode tests, driven by testdata/blend_mainnet.json and
// testdata/blend_mainnet_live.json — REAL mainnet contract_data changes,
// contract events and live config entries for the Blend V2 pools (Fixed,
// YieldBlox) and the V2 backstop, captured 2026-07-25 from
// https://mainnet.sorobanrpc.com (see each fixture's _comment for the exact
// ledger inventory). All XDR is untouched; every expected constant below was
// hand-derived from the on-chain XDR with an independent decoder
// (scratchpad/fixture-capture/scval.py), not read back from this package.
//
// Hand-derived anchors:
//
//	63627277 user liquidation created (auct_type 0, GCH6THZV):
//	  block 63627278, lot {XLM SAC: 20032300369}, bid {USDC: 2551564716}
//	63627456 the same auction filled -> entry Removed
//	63637204 interest auction created (auct_type 2, user = backstop):
//	  block 63637205, lot {USDC: 2010460825, EURC: 144500276},
//	  bid {Comet LP CAS3FL6T...: 10167463475}
//	63637440 the interest auction filled -> entry Removed
//	63563168 first-touch borrow: UserEmis(1) and UserEmis(2) Created for
//	  GDCKNNJB with accrued 0 and index 27872613430832 / 22398427417475978;
//	  EmisData(1) {eps 5311514346213, exp 1785134425, last 1784543088},
//	  EmisData(2) {eps 10623028692476, exp 1785134425, last 1784543088}
//	63622056 gulp_emissions: EmisData(1) {eps 5318072863260, exp 1785479267,
//	  index 27875229836977, last 1784874467}, EmisData(4) {eps 10636145726603,
//	  index 22149965795425790}
//	63518638 backstop deposit+claim: BEmisData(Fixed) {eps 61868469844163,
//	  exp 1784788866, index 400338638704506, last 1784293063}; UEmisData for
//	  GDJSH2NU {accrued 0, index 400338638704506}
//	63636721 full exit: GDMZDE4U Positions prior {collateral {1: 66684}} ->
//	  all maps empty
//	live snapshot @63637827: Fixed instance {Name "Fixed", Admin GAX2VVWV...,
//	  BLNDTkn CD25MNVT..., Backstop CAQQR5SW..., Config {oracle CCVTVW2C...,
//	  bstop_rate 2000000, status 1, max_positions 6, min_collateral 50000000}},
//	  wasm a41fc53d...; PoolEmis {1: 2000000, 2: 4000000, 4: 4000000}

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/lightgatehq/lidapters/blend/contracts"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

const (
	mainnetBackstopID = "CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7"
	cometLPTokenID    = "CAS3FL6TLZKDGGSISDBWGGPXT3NRR4DYTZD7YOD3HMYO6LTJUVGRVEAM"

	liquidatedUser = "GCH6THZV4BAFGII4JB6GXMRQZHLQB7VBRAJM7YK4RXBQGRWCQ3SCTXAM"
	firstTouchUser = "GDCKNNJBFHVZTXGVRT5OGZWS2RMQG2RAB3LARGBELNPUAOFWXVLGTWR3"
	backstopUser   = "GDJSH2NU2WF6J4P5DL4522DUCABWSTZOKFQ7BHBCFYQ3QKC6FRYWP6OL"
	fullExitUser   = "GDMZDE4UCYGMXZS5FBKZ4DHYCCSCFM4DH3ZVCTLVXMUOEIN2CCCMYYPG"
)

// YieldBlox reserve list as of the live snapshot (8 assets, on-chain order).
var ybxReserveAssets = []string{
	xlmSACID,
	usdcTokenID,
	eurcTokenID,
	"CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK",
	"CB226ZOEYXTBPD3QEGABTJYSKZVBP2PASEISLG3SBMTN5CE4QZUVZ3CE",
	"CBLV4ATSIWU67CFSQU2NVRKINQIKUZ2ODSZBUJTJ43VJVRSBTZYOPNUR",
	"CAL6ER2TI6CTRAY6BFXWNWA7WTYXUXTQCHUBCIBU5O6KM3HJFG6Z6VXV",
	"CCCRWH6Q3FNP3I2I57BDLM5AFAT7O6OF6GKQOC6SSJNDAVRZ57SPHGU2",
}

type mainnetFixtureEvent struct {
	TxHash      string `json:"tx_hash"`
	EventIndex  int    `json:"event_index"`
	ContractID  string `json:"contract_id"`
	Topic       string `json:"topic"`
	RawEventB64 string `json:"raw_event_b64"`
}

type mainnetFixtureChange struct {
	ContractID         string  `json:"contract_id"`
	KeyXDR             string  `json:"key_xdr"`
	ValueXDR           *string `json:"value_xdr"`
	Durability         string  `json:"durability"`
	ChangeType         string  `json:"change_type"`
	Live               bool    `json:"live"`
	LiveUntilLedgerSeq *uint32 `json:"live_until_ledger_seq"`
	LastModifiedLedger uint32  `json:"last_modified_ledger"`
	PriorValueXDR      *string `json:"prior_value_xdr"`
}

type mainnetFixtureLedger struct {
	LedgerSeq     int64                  `json:"ledger_seq"`
	CloseTimeUnix int64                  `json:"close_time_unix"`
	RawChanges    []mainnetFixtureChange `json:"changes"`
	Events        []mainnetFixtureEvent  `json:"events"`
	Changes       []bindings.ContractDataChange
}

func loadMainnetFixture(t *testing.T) []mainnetFixtureLedger {
	t.Helper()
	raw, err := os.ReadFile("testdata/blend_mainnet.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var doc struct {
		Ledgers []mainnetFixtureLedger `json:"ledgers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	for i := range doc.Ledgers {
		for _, ch := range doc.Ledgers[i].RawChanges {
			doc.Ledgers[i].Changes = append(doc.Ledgers[i].Changes, bindings.ContractDataChange{
				ContractID:         ch.ContractID,
				KeyXDR:             ch.KeyXDR,
				ValueXDR:           ch.ValueXDR,
				Durability:         ch.Durability,
				ChangeType:         ch.ChangeType,
				Live:               ch.Live,
				LiveUntilLedgerSeq: ch.LiveUntilLedgerSeq,
				LastModifiedLedger: ch.LastModifiedLedger,
			})
		}
	}
	return doc.Ledgers
}

type mainnetLiveEntry struct {
	ContractID         string  `json:"contract_id"`
	Label              string  `json:"label"`
	KeyXDR             string  `json:"key_xdr"`
	ValueXDR           *string `json:"value_xdr"`
	LastModifiedLedger uint32  `json:"last_modified_ledger"`
	Found              bool    `json:"found"`
}

func loadMainnetLiveSnapshot(t *testing.T) (uint32, []mainnetLiveEntry) {
	t.Helper()
	raw, err := os.ReadFile("testdata/blend_mainnet_live.json")
	if err != nil {
		t.Fatalf("read live snapshot: %v", err)
	}
	var doc struct {
		LatestLedger uint32             `json:"latest_ledger"`
		Entries      []mainnetLiveEntry `json:"entries"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse live snapshot: %v", err)
	}
	return doc.LatestLedger, doc.Entries
}

func newMainnetAdapter(t *testing.T) *Adapter {
	t.Helper()
	adapter, err := New(Config{AllowUnknownV2: true})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	adapter.RegisterContracts(fixedPoolID, yieldBloxPoolID, mainnetBackstopID)
	return adapter
}

// mainnetPrior seeds both pools with their real reserve lists (per the live
// ResList entries) — the shape the pool fold produces long before the fixture
// window opens. The reserve LISTS are what EmisData/UserEmis resolution keys
// off; every reserve value asserted below comes from fixture XDR.
func mainnetPrior() *bindings.LedgerState {
	fixedReserves := make([]contracts.ReserveState, 0, 3)
	for i, asset := range []string{xlmSACID, usdcTokenID, eurcTokenID} {
		fixedReserves = append(fixedReserves, contracts.ReserveState{AssetID: asset, ReserveIndex: int32(i), ReserveIndexKnown: true})
	}
	ybxReserves := make([]contracts.ReserveState, 0, len(ybxReserveAssets))
	for i, asset := range ybxReserveAssets {
		ybxReserves = append(ybxReserves, contracts.ReserveState{AssetID: asset, ReserveIndex: int32(i), ReserveIndexKnown: true})
	}
	return &bindings.LedgerState{
		Pools: []contracts.PoolState{
			{
				ContractID:       fixedPoolID,
				BackstopContract: mainnetBackstopID,
				PoolStatus:       "active",
				Reserves:         fixedReserves,
			},
			{
				ContractID:       yieldBloxPoolID,
				BackstopContract: mainnetBackstopID,
				PoolStatus:       "admin_active",
				Reserves:         ybxReserves,
			},
		},
	}
}

func foldMainnetLedgers(t *testing.T, adapter *Adapter, prior *bindings.LedgerState, ledgers []mainnetFixtureLedger) *bindings.LedgerState {
	t.Helper()
	state := prior
	for _, ledger := range ledgers {
		next, err := adapter.DecodeStateAt(state, ledger.Changes, ledger.LedgerSeq, time.Unix(ledger.CloseTimeUnix, 0).UTC())
		if err != nil {
			t.Fatalf("decode ledger %d: %v", ledger.LedgerSeq, err)
		}
		state = next
	}
	return state
}

func mainnetLedgersThrough(ledgers []mainnetFixtureLedger, seq int64) []mainnetFixtureLedger {
	out := make([]mainnetFixtureLedger, 0, len(ledgers))
	for _, l := range ledgers {
		if l.LedgerSeq <= seq {
			out = append(out, l)
		}
	}
	return out
}

func findAuction(state *bindings.LedgerState, pool, user string, auctType int32) *contracts.AuctionState {
	for i := range state.Auctions {
		a := &state.Auctions[i]
		if a.PoolContractID == pool && a.UserAddress == user && a.AuctionType == auctType {
			return a
		}
	}
	return nil
}

// TestBlendMainnet_AuctionLifecycle folds the real liquidation
// (63627277 create -> 63627456 fill) and the real interest auction
// (63637204 create -> 63637440 fill) and pins the decoded AuctionState
// against the hand-derived on-chain values.
func TestBlendMainnet_AuctionLifecycle(t *testing.T) {
	t.Parallel()
	ledgers := loadMainnetFixture(t)
	adapter := newMainnetAdapter(t)

	// Liquidation created.
	state := foldMainnetLedgers(t, adapter, mainnetPrior(), mainnetLedgersThrough(ledgers, 63627277))
	auction := findAuction(state, fixedPoolID, liquidatedUser, 0)
	if auction == nil {
		t.Fatalf("liquidation auction for %s not decoded; auctions = %+v", liquidatedUser, state.Auctions)
	}
	if auction.Block != 63627278 {
		t.Errorf("liquidation block = %d, want 63627278", auction.Block)
	}
	wantLot := []contracts.AuctionEntry{{AssetID: xlmSACID, AmountRaw: "20032300369"}}
	wantBid := []contracts.AuctionEntry{{AssetID: usdcTokenID, AmountRaw: "2551564716"}}
	assertAuctionEntries(t, "liquidation lot", auction.Lot, wantLot)
	assertAuctionEntries(t, "liquidation bid", auction.Bid, wantBid)

	// Filled -> the temporary entry is removed and the auction is gone.
	state = foldMainnetLedgers(t, adapter, mainnetPrior(), mainnetLedgersThrough(ledgers, 63627456))
	if a := findAuction(state, fixedPoolID, liquidatedUser, 0); a != nil {
		t.Errorf("liquidation auction still carried after its fill_auction removal: %+v", a)
	}

	// Interest auction created (auctioned "user" is the backstop contract).
	state = foldMainnetLedgers(t, adapter, mainnetPrior(), mainnetLedgersThrough(ledgers, 63637204))
	interest := findAuction(state, fixedPoolID, mainnetBackstopID, 2)
	if interest == nil {
		t.Fatalf("interest auction not decoded; auctions = %+v", state.Auctions)
	}
	if interest.Block != 63637205 {
		t.Errorf("interest block = %d, want 63637205", interest.Block)
	}
	assertAuctionEntries(t, "interest lot", interest.Lot, []contracts.AuctionEntry{
		{AssetID: usdcTokenID, AmountRaw: "2010460825"},
		{AssetID: eurcTokenID, AmountRaw: "144500276"},
	})
	assertAuctionEntries(t, "interest bid", interest.Bid, []contracts.AuctionEntry{
		{AssetID: cometLPTokenID, AmountRaw: "10167463475"},
	})

	// Interest auction filled -> gone; no auctions remain carried.
	state = foldMainnetLedgers(t, adapter, mainnetPrior(), ledgers)
	if len(state.Auctions) != 0 {
		t.Errorf("auctions still carried after both fills: %+v", state.Auctions)
	}
}

func assertAuctionEntries(t *testing.T, label string, got, want []contracts.AuctionEntry) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %+v, want %+v", label, got, want)
		return
	}
	byAsset := map[string]string{}
	for _, e := range got {
		byAsset[e.AssetID] = e.AmountRaw
	}
	for _, w := range want {
		if byAsset[w.AssetID] != w.AmountRaw {
			t.Errorf("%s[%s] = %q, want %q", label, w.AssetID, byAsset[w.AssetID], w.AmountRaw)
		}
	}
}

// TestBlendMainnet_EmissionAccrual pins the emission-accrual decode paths
// against real writes: UserEmis first-touch creation, the v2 merged
// ReserveEmissionData (EmisData alone carries eps/expiration), the
// gulp_emissions refresh, and the backstop's BEmisData/UEmisData entries.
func TestBlendMainnet_EmissionAccrual(t *testing.T) {
	t.Parallel()
	ledgers := loadMainnetFixture(t)
	adapter := newMainnetAdapter(t)

	// Through the first-touch borrow ledger.
	state := foldMainnetLedgers(t, adapter, mainnetPrior(), mainnetLedgersThrough(ledgers, 63563168))

	wantUserEmis := map[int32]string{
		1: "27872613430832",    // res 0 (XLM) b-token side
		2: "22398427417475978", // res 1 (USDC) d-token side
	}
	found := 0
	for _, ue := range state.UserEmissions {
		if ue.Address != firstTouchUser || ue.PoolContractID != fixedPoolID {
			continue
		}
		wantIndex, ok := wantUserEmis[ue.ReserveTokenID]
		if !ok {
			t.Errorf("unexpected UserEmis token id %d for %s", ue.ReserveTokenID, firstTouchUser)
			continue
		}
		found++
		if ue.IndexRaw != wantIndex {
			t.Errorf("UserEmis(%d).IndexRaw = %q, want %q", ue.ReserveTokenID, ue.IndexRaw, wantIndex)
		}
		if ue.AccruedRaw != "0" {
			t.Errorf("UserEmis(%d).AccruedRaw = %q, want 0 (a real first-touch checkpoint)", ue.ReserveTokenID, ue.AccruedRaw)
		}
	}
	if found != 2 {
		t.Errorf("decoded %d UserEmis entries for %s, want 2", found, firstTouchUser)
	}

	// EmisData(1): XLM supply side. EmisData(2): USDC borrow side.
	xlm := mainnetReserve(t, state, fixedPoolID, xlmSACID)
	if xlm.SupplyEmisEPSRaw != "5311514346213" || xlm.SupplyEmisExpirationRaw != "1785134425" ||
		xlm.SupplyEmisIndexRaw != "27872613430832" || xlm.SupplyEmisLastTimeRaw != "1784543088" {
		t.Errorf("XLM supply emission = {eps %q, exp %q, index %q, last %q}, want the 63563168 EmisData(1) values",
			xlm.SupplyEmisEPSRaw, xlm.SupplyEmisExpirationRaw, xlm.SupplyEmisIndexRaw, xlm.SupplyEmisLastTimeRaw)
	}
	usdc := mainnetReserve(t, state, fixedPoolID, usdcTokenID)
	if usdc.BorrowEmisEPSRaw != "10623028692476" || usdc.BorrowEmisIndexRaw != "22398427417475978" {
		t.Errorf("USDC borrow emission = {eps %q, index %q}, want the 63563168 EmisData(2) values",
			usdc.BorrowEmisEPSRaw, usdc.BorrowEmisIndexRaw)
	}

	// After the gulp_emissions refresh the same sides carry the new checkpoint.
	state = foldMainnetLedgers(t, adapter, mainnetPrior(), mainnetLedgersThrough(ledgers, 63622056))
	xlm = mainnetReserve(t, state, fixedPoolID, xlmSACID)
	if xlm.SupplyEmisEPSRaw != "5318072863260" || xlm.SupplyEmisExpirationRaw != "1785479267" ||
		xlm.SupplyEmisIndexRaw != "27875229836977" || xlm.SupplyEmisLastTimeRaw != "1784874467" {
		t.Errorf("XLM supply emission after gulp = {eps %q, exp %q, index %q, last %q}, want the 63622056 EmisData(1) values",
			xlm.SupplyEmisEPSRaw, xlm.SupplyEmisExpirationRaw, xlm.SupplyEmisIndexRaw, xlm.SupplyEmisLastTimeRaw)
	}
	eurc := mainnetReserve(t, state, fixedPoolID, eurcTokenID)
	if eurc.BorrowEmisEPSRaw != "10636145726603" || eurc.BorrowEmisIndexRaw != "22149965795425790" {
		t.Errorf("EURC borrow emission after gulp = {eps %q, index %q}, want the 63622056 EmisData(4) values",
			eurc.BorrowEmisEPSRaw, eurc.BorrowEmisIndexRaw)
	}

	// Backstop pool-level accrual (BEmisData) and per-user accrual (UEmisData)
	// from the 63518638 backstop deposit+claim.
	state = foldMainnetLedgers(t, adapter, mainnetPrior(), mainnetLedgersThrough(ledgers, 63518638))
	pool := mainnetPool(t, state, fixedPoolID)
	if pool.BackstopEmisEPSRaw != "61868469844163" || pool.BackstopEmisExpirationRaw != "1784788866" ||
		pool.BackstopEmisIndexRaw != "400338638704506" || pool.BackstopEmisLastTimeRaw != "1784293063" {
		t.Errorf("backstop emission = {eps %q, exp %q, index %q, last %q}, want the 63518638 BEmisData values",
			pool.BackstopEmisEPSRaw, pool.BackstopEmisExpirationRaw, pool.BackstopEmisIndexRaw, pool.BackstopEmisLastTimeRaw)
	}
	var backstopPos *contracts.BackstopPosition
	for i := range state.Backstops {
		b := &state.Backstops[i]
		if b.Address == backstopUser && b.PoolContractID == fixedPoolID {
			backstopPos = b
		}
	}
	if backstopPos == nil {
		t.Fatalf("backstop position for %s not decoded", backstopUser)
	}
	if backstopPos.UnclaimedEmissionsRaw != "0" || backstopPos.EmisIndexRaw != "400338638704506" {
		t.Errorf("UEmisData = {accrued %q, index %q}, want {0, 400338638704506} (claim zeroed the accrual)",
			backstopPos.UnclaimedEmissionsRaw, backstopPos.EmisIndexRaw)
	}
}

func mainnetPool(t *testing.T, state *bindings.LedgerState, poolID string) contracts.PoolState {
	t.Helper()
	for _, pool := range state.Pools {
		if pool.ContractID == poolID {
			return pool
		}
	}
	t.Fatalf("pool %s not in state", poolID)
	return contracts.PoolState{}
}

func mainnetReserve(t *testing.T, state *bindings.LedgerState, poolID, assetID string) contracts.ReserveState {
	t.Helper()
	pool := mainnetPool(t, state, poolID)
	for _, reserve := range pool.Reserves {
		if reserve.AssetID == assetID {
			return reserve
		}
	}
	t.Fatalf("reserve %s not in pool %s", assetID, poolID)
	return contracts.ReserveState{}
}

// TestBlendMainnet_EventClassification runs every captured event through
// decodeEvent and pins the exact activity type. Expected values are
// hand-derived from each event's on-chain topic symbol: the pool vocabulary
// classifies to itself; the backstop-only vocabulary (queue_withdrawal,
// dequeue_withdrawal, donate, distribute) is deliberately outside the enum
// and must classify to NOTHING — never a guessed type.
func TestBlendMainnet_EventClassification(t *testing.T) {
	t.Parallel()
	ledgers := loadMainnetFixture(t)

	wantByName := map[string]contracts.ActivityType{
		"supply":                  contracts.ActivityTypeSupply,
		"withdraw":                contracts.ActivityTypeWithdraw,
		"supply_collateral":       contracts.ActivityTypeSupplyCollateral,
		"withdraw_collateral":     contracts.ActivityTypeWithdrawCollateral,
		"borrow":                  contracts.ActivityTypeBorrow,
		"repay":                   contracts.ActivityTypeRepay,
		"claim":                   contracts.ActivityTypeClaim,
		"flash_loan":              contracts.ActivityTypeFlashLoan,
		"new_auction":             contracts.ActivityTypeNewAuction,
		"fill_auction":            contracts.ActivityTypeFillAuction,
		"gulp_emissions":          contracts.ActivityTypeGulpEmissions,
		"reserve_emission_update": contracts.ActivityTypeReserveEmissionUpdate,
		"deposit":                 contracts.ActivityTypeDeposit,
		// Backstop-only vocabulary: no activity type today, decode refuses to guess.
		"queue_withdrawal":   "",
		"dequeue_withdrawal": "",
		"donate":             "",
		"distribute":         "",
	}

	total := 0
	for _, ledger := range ledgers {
		for _, evt := range ledger.Events {
			total++
			var payload struct {
				Topics []string `json:"topics"`
			}
			if err := json.Unmarshal([]byte(evt.Topic), &payload); err != nil || len(payload.Topics) == 0 {
				t.Fatalf("ledger %d event %d: bad topic payload", ledger.LedgerSeq, evt.EventIndex)
			}
			name := payload.Topics[0]
			want, known := wantByName[name]
			if !known {
				t.Fatalf("ledger %d event %d: fixture carries unexpected event %q — extend the expectation table from the on-chain derivations", ledger.LedgerSeq, evt.EventIndex, name)
			}

			decoded := decodeEvent(bindings.RawEventEnvelope{
				LedgerSeq:  ledger.LedgerSeq,
				TxHash:     evt.TxHash,
				EventIndex: evt.EventIndex,
				ContractID: evt.ContractID,
				Topic:      evt.Topic,
				RawEvent:   mustBase64(t, evt.RawEventB64),
			})
			if decoded.activityType != want {
				t.Errorf("ledger %d %s[%d] %q: activity = %q, want %q",
					ledger.LedgerSeq, evt.TxHash[:8], evt.EventIndex, name, decoded.activityType, want)
			}
			if want != "" && decoded.metadata["event_name"] != name {
				t.Errorf("ledger %d %s[%d]: metadata event_name = %q, want the exact on-chain symbol %q",
					ledger.LedgerSeq, evt.TxHash[:8], evt.EventIndex, decoded.metadata["event_name"], name)
			}
		}
	}
	if total != 33 {
		t.Errorf("fixture carries %d events, want 33 — inventory drifted", total)
	}
}

// TestBlendMainnet_AuctionEventStructure pins the structured decode of the
// real liquidation events: both parties, the auction type label, and the
// event-embedded AuctionData metadata.
func TestBlendMainnet_AuctionEventStructure(t *testing.T) {
	t.Parallel()
	ledgers := loadMainnetFixture(t)

	var newAuction, fillAuction *mainnetFixtureEvent
	var newSeq, fillSeq int64
	for i := range ledgers {
		for j := range ledgers[i].Events {
			evt := &ledgers[i].Events[j]
			switch {
			case ledgers[i].LedgerSeq == 63627277 && evt.EventIndex == 0:
				newAuction, newSeq = evt, ledgers[i].LedgerSeq
			case ledgers[i].LedgerSeq == 63627456 && evt.EventIndex == 1:
				fillAuction, fillSeq = evt, ledgers[i].LedgerSeq
			}
		}
	}
	if newAuction == nil || fillAuction == nil {
		t.Fatal("liquidation events not found in fixture")
	}

	decoded := decodeEvent(bindings.RawEventEnvelope{
		LedgerSeq: newSeq, TxHash: newAuction.TxHash, EventIndex: newAuction.EventIndex,
		ContractID: newAuction.ContractID, Topic: newAuction.Topic, RawEvent: mustBase64(t, newAuction.RawEventB64),
	})
	if decoded.activityType != contracts.ActivityTypeNewAuction {
		t.Fatalf("new_auction activity = %q", decoded.activityType)
	}
	if decoded.address != liquidatedUser {
		t.Errorf("new_auction address = %q, want the auctioned user %q", decoded.address, liquidatedUser)
	}
	if decoded.metadata["auction_type"] != "user_liquidation" {
		t.Errorf("auction_type = %q, want user_liquidation", decoded.metadata["auction_type"])
	}
	if got := decoded.metadata["auction_block"]; got != "63627278" {
		t.Errorf("auction_block = %q, want 63627278", got)
	}
	if got := decoded.metadata["auction_lot"]; got != `{"`+xlmSACID+`":"20032300369"}` {
		t.Errorf("auction_lot = %q, want the real XLM lot", got)
	}
	if got := decoded.metadata["auction_bid"]; got != `{"`+usdcTokenID+`":"2551564716"}` {
		t.Errorf("auction_bid = %q, want the real USDC bid", got)
	}

	decoded = decodeEvent(bindings.RawEventEnvelope{
		LedgerSeq: fillSeq, TxHash: fillAuction.TxHash, EventIndex: fillAuction.EventIndex,
		ContractID: fillAuction.ContractID, Topic: fillAuction.Topic, RawEvent: mustBase64(t, fillAuction.RawEventB64),
	})
	if decoded.activityType != contracts.ActivityTypeFillAuction {
		t.Fatalf("fill_auction activity = %q", decoded.activityType)
	}
	if decoded.address != liquidatedUser {
		t.Errorf("fill_auction address = %q, want the auctioned user %q", decoded.address, liquidatedUser)
	}
	if decoded.metadata["auction_type"] != "user_liquidation" {
		t.Errorf("fill auction_type = %q, want user_liquidation", decoded.metadata["auction_type"])
	}
}

// TestBlendMainnet_FullExitReplay replays the real full exit at 63636721:
// prior state is seeded from the entry's own pre-change STATE image (real
// on-chain prior value, captured from the same close meta), then the exit
// ledger folds an all-empty Positions write on top. The user's position must
// clear, and the dirty-positions seam must report the pair as an upsert (the
// entry still exists on-chain, holding empty maps — Blend never deletes it).
func TestBlendMainnet_FullExitReplay(t *testing.T) {
	t.Parallel()
	ledgers := loadMainnetFixture(t)
	adapter := newMainnetAdapter(t)

	var exitLedger *mainnetFixtureLedger
	for i := range ledgers {
		if ledgers[i].LedgerSeq == 63636721 {
			exitLedger = &ledgers[i]
		}
	}
	if exitLedger == nil {
		t.Fatal("exit ledger 63636721 not in fixture")
	}

	// Find the exiting user's Positions change and its real prior image.
	var exitChange *mainnetFixtureChange
	for i := range exitLedger.RawChanges {
		ch := &exitLedger.RawChanges[i]
		if ch.PriorValueXDR == nil || ch.ValueXDR == nil {
			continue
		}
		if keyIsPositionsFor(t, ch.KeyXDR, fullExitUser) {
			exitChange = ch
		}
	}
	if exitChange == nil {
		t.Fatal("full-exit Positions change with prior image not in fixture")
	}

	// Seed the prior world: the user's real pre-exit Positions entry, folded
	// one ledger before the exit.
	seed := bindings.ContractDataChange{
		ContractID:         exitChange.ContractID,
		KeyXDR:             exitChange.KeyXDR,
		ValueXDR:           exitChange.PriorValueXDR,
		Durability:         exitChange.Durability,
		ChangeType:         "LedgerEntryChangeTypeLedgerEntryCreated",
		Live:               true,
		LastModifiedLedger: 63636720,
	}
	prior, err := adapter.DecodeStateAt(mainnetPrior(), []bindings.ContractDataChange{seed}, 63636720, time.Unix(exitLedger.CloseTimeUnix-5, 0).UTC())
	if err != nil {
		t.Fatalf("seed prior: %v", err)
	}
	if got := nonZeroPositions(prior, fullExitUser); len(got) != 1 ||
		got[0].AssetID != usdcTokenID || got[0].PositionType != contracts.PositionTypeCollateral || got[0].BTokensRaw != "66684" {
		t.Fatalf("seeded prior positions = %+v, want the real pre-exit collateral {USDC: 66684 bTokens}", got)
	}

	// Replay the real exit ledger.
	after, err := adapter.DecodeStateAt(prior, exitLedger.Changes, exitLedger.LedgerSeq, time.Unix(exitLedger.CloseTimeUnix, 0).UTC())
	if err != nil {
		t.Fatalf("decode exit ledger: %v", err)
	}
	if got := nonZeroPositions(after, fullExitUser); len(got) != 0 {
		t.Errorf("positions after full exit = %+v, want none", got)
	}

	dirty := adapter.LastDirtyPositions()
	var pair *bindings.DirtyPosition
	for i := range dirty {
		if dirty[i].Address == fullExitUser && dirty[i].PoolContractID == fixedPoolID {
			pair = &dirty[i]
		}
	}
	if pair == nil {
		t.Fatalf("full exit not reported dirty; dirty = %+v", dirty)
	}
	if pair.Kind != bindings.DirtyUpsert {
		t.Errorf("full exit dirty kind = %q, want %q (empty write, not an on-chain delete)", pair.Kind, bindings.DirtyUpsert)
	}
}

func nonZeroPositions(state *bindings.LedgerState, address string) []contracts.UserReservePosition {
	var out []contracts.UserReservePosition
	for _, u := range state.Users {
		if u.Address != address {
			continue
		}
		if (u.BTokensRaw != "" && u.BTokensRaw != "0") || (u.DTokensRaw != "" && u.DTokensRaw != "0") {
			out = append(out, u)
		}
	}
	return out
}

// TestBlendMainnet_LiveConfigIdentity applies the live-snapshot entries (pool
// instances, ResList, PoolEmis, backstop instance + RZ/DropList) as a single
// fold and pins the instance-identity and PoolEmis decode against the real
// on-chain configuration.
func TestBlendMainnet_LiveConfigIdentity(t *testing.T) {
	t.Parallel()
	latestLedger, entries := loadMainnetLiveSnapshot(t)
	adapter := newMainnetAdapter(t)

	var changes []bindings.ContractDataChange
	for _, entry := range entries {
		if !entry.Found {
			continue
		}
		changes = append(changes, bindings.ContractDataChange{
			ContractID:         entry.ContractID,
			KeyXDR:             entry.KeyXDR,
			ValueXDR:           entry.ValueXDR,
			Durability:         "ContractDataDurabilityPersistent",
			ChangeType:         "LedgerEntryChangeTypeLedgerEntryCreated",
			Live:               true,
			LastModifiedLedger: entry.LastModifiedLedger,
		})
	}
	state, err := adapter.DecodeStateAt(&bindings.LedgerState{}, changes, int64(latestLedger), time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatalf("decode live snapshot: %v", err)
	}

	fixed := mainnetPool(t, state, fixedPoolID)
	if fixed.Name != "Fixed" {
		t.Errorf("Fixed pool Name = %q, want the on-chain instance string \"Fixed\"", fixed.Name)
	}
	if fixed.Admin != "GAX2VVWVHU5YQY5J3NJBXKHI3FFKZN54BE6GRJCWSIKSBZTQWJJNJMPC" {
		t.Errorf("Fixed pool Admin = %q", fixed.Admin)
	}
	if fixed.BLNDToken != "CD25MNVTZDL4Y3XBCPCJXGXATV5WUHHOWMYFF4YBEGU5FCPGMYTVG5JY" {
		t.Errorf("Fixed pool BLNDToken = %q", fixed.BLNDToken)
	}
	if fixed.BackstopContract != mainnetBackstopID {
		t.Errorf("Fixed pool BackstopContract = %q", fixed.BackstopContract)
	}
	if fixed.OracleContract != fixedAggregatorID {
		t.Errorf("Fixed pool OracleContract = %q, want %q", fixed.OracleContract, fixedAggregatorID)
	}
	if fixed.PoolStatus != "active" || fixed.BackstopTakeRate != "2000000" ||
		fixed.MaxPositionsRaw != "6" || fixed.MinCollateralRaw != "50000000" {
		t.Errorf("Fixed pool config = {status %q, take %q, maxpos %q, mincol %q}, want {active, 2000000, 6, 50000000}",
			fixed.PoolStatus, fixed.BackstopTakeRate, fixed.MaxPositionsRaw, fixed.MinCollateralRaw)
	}
	if fixed.WasmHash != "a41fc53d6753b6c04eb15b021c55052366a4c8e0e21bc72700f461264ec1350e" {
		t.Errorf("Fixed pool WasmHash = %q, want the shared V2 pool wasm", fixed.WasmHash)
	}
	wantPoolEmis := []contracts.PoolEmissionEntry{
		{ReserveTokenID: 1, ShareRaw: "2000000"},
		{ReserveTokenID: 2, ShareRaw: "4000000"},
		{ReserveTokenID: 4, ShareRaw: "4000000"},
	}
	if len(fixed.PoolEmissions) != len(wantPoolEmis) {
		t.Fatalf("Fixed PoolEmissions = %+v, want %+v", fixed.PoolEmissions, wantPoolEmis)
	}
	for i, want := range wantPoolEmis {
		if fixed.PoolEmissions[i] != want {
			t.Errorf("Fixed PoolEmissions[%d] = %+v, want %+v", i, fixed.PoolEmissions[i], want)
		}
	}
	if len(fixed.Reserves) != 3 {
		t.Errorf("Fixed pool decoded %d reserves from the real ResList, want 3", len(fixed.Reserves))
	}

	ybx := mainnetPool(t, state, yieldBloxPoolID)
	if ybx.Name != "YieldBlox" || ybx.PoolStatus != "admin_active" {
		t.Errorf("YieldBlox = {Name %q, status %q}, want {YieldBlox, admin_active}", ybx.Name, ybx.PoolStatus)
	}
	if ybx.OracleContract != "CD74A3C54EKUVEGUC6WNTUPOTHB624WFKXN3IYTFJGX3EHXDXHCYMXXR" {
		t.Errorf("YieldBlox OracleContract = %q", ybx.OracleContract)
	}
	if len(ybx.Reserves) != 8 {
		t.Errorf("YieldBlox decoded %d reserves from the real ResList, want 8", len(ybx.Reserves))
	}

	var backstop *contracts.BackstopInstanceState
	for i := range state.BackstopInstances {
		if state.BackstopInstances[i].ContractID == mainnetBackstopID {
			backstop = &state.BackstopInstances[i]
		}
	}
	if backstop == nil {
		t.Fatal("backstop instance identity not decoded")
	}
	if backstop.BackstopToken != cometLPTokenID {
		t.Errorf("backstop BToken = %q, want the Comet LP %q", backstop.BackstopToken, cometLPTokenID)
	}
	if backstop.BLNDToken != "CD25MNVTZDL4Y3XBCPCJXGXATV5WUHHOWMYFF4YBEGU5FCPGMYTVG5JY" ||
		backstop.USDCToken != usdcTokenID {
		t.Errorf("backstop tokens = {BLND %q, USDC %q}", backstop.BLNDToken, backstop.USDCToken)
	}
	wantRZ := []string{
		"CDMAVJPFXPADND3YRL4BSM3AKZWCTFMX27GLLXCML3PD62HEQS5FPVAI", // Etherfuse
		"CBYOBT7ZCCLQCBUYYIABZLSEGDPEUWXCUXQTZYOG3YBDR7U357D5ZIRF", // Forex
		"CAE7QVOMBLZ53CDRGK3UNRRHG5EZ5NQA7HHTFASEMYBWHG6MDFZTYHXC", // Orbit
		yieldBloxPoolID,
		fixedPoolID,
	}
	if len(backstop.RewardZone) != len(wantRZ) {
		t.Fatalf("reward zone = %v, want %v", backstop.RewardZone, wantRZ)
	}
	for i, want := range wantRZ {
		if backstop.RewardZone[i] != want {
			t.Errorf("reward zone[%d] = %q, want %q", i, backstop.RewardZone[i], want)
		}
	}
	if len(backstop.DropList) != 2 {
		t.Fatalf("drop list = %+v, want 2 entries", backstop.DropList)
	}
	for i, want := range []contracts.DropListEntry{
		{Address: "CBBUMX75GDGWZDD5D4ADYESUXZVC4CJ6M65LTUNIEUE3NAY2AHQUDDUN", AmountRaw: "10000000000000"},
		{Address: "CBF5LTJ5YH64NY4MZWXXUPFMY5LDU77EDCERBC2MHJNEZGMWWPTGU6O7", AmountRaw: "10000000000000"},
	} {
		if backstop.DropList[i] != want {
			t.Errorf("drop list[%d] = %+v, want %+v", i, backstop.DropList[i], want)
		}
	}
}

// witnessUser33 is the wallet whose YieldBlox positions were misattributed in
// the #33 mainnet repro (bounded replay 62,986,500–62,988,499, ledger
// 62,986,834). The amounts below are hand-derived from that ledger's decoded
// Positions entry XDR: collateral {0: 210,346,315,861 (XLM), 1: 16,523,965,334
// (USDC)}, liabilities {0: 14,746,315,917, 1: 12,665,205,938}.
const witnessUser33 = "GD4EN5NB25YLXKTCUV7XPIPDL6RUQEC7L7T7JMB2QMGPFSKAHNMFWGC6"

// TestBlendMainnet_BoundedReplayUnknownIndexNeverMisattributes replays the #33
// witness shape: a pinned-start bounded replay with no config seed, where only
// USDC's ResData has appeared in-window before the witness wallet's Positions
// entry folds at 62,986,834. USDC's reserve holds the zero-value index, so the
// uncorrected fold labels the XLM bucket-0 amounts as USDC and drops the true
// USDC bucket-1 legs. The corrected fold emits no position rows at all: both
// indexes are unmapped, and skipped legs surface as diagnostics instead.
func TestBlendMainnet_BoundedReplayUnknownIndexNeverMisattributes(t *testing.T) {
	t.Parallel()
	adapter := newMainnetAdapter(t)

	resData := stateChange(t, yieldBloxPoolID, variantVal(t, "ResData", addressVal(t, usdcTokenID)), mapVal(t, map[string]xdr.ScVal{
		"d_rate":   i128Val(962409238681),
		"b_rate":   i128Val(1_000_000_000),
		"b_supply": i128Val(5000),
		"d_supply": i128Val(1200),
	}))
	prior, err := adapter.DecodeStateAt(nil, []bindings.ContractDataChange{resData}, 62986833, time.Unix(1785300000, 0).UTC())
	if err != nil {
		t.Fatalf("decode ResData ledger: %v", err)
	}

	positions := stateChange(t, yieldBloxPoolID, variantVal(t, "Positions", accountAddressValFromID(t, witnessUser33)), mapVal(t, map[string]xdr.ScVal{
		"collateral":  intMapVal(t, map[uint32]xdr.ScVal{0: i128Val(210346315861), 1: i128Val(16523965334)}),
		"liabilities": intMapVal(t, map[uint32]xdr.ScVal{0: i128Val(14746315917), 1: i128Val(12665205938)}),
	}))
	state, err := adapter.DecodeStateAt(prior, []bindings.ContractDataChange{positions}, 62986834, time.Unix(1785300005, 0).UTC())
	if err != nil {
		t.Fatalf("decode witness ledger: %v", err)
	}

	for _, u := range state.Users {
		if u.Address != witnessUser33 {
			continue
		}
		if u.AssetID == usdcTokenID && u.BTokensRaw == "210346315861" {
			t.Errorf("XLM collateral misattributed to USDC through the zero-value index: %+v", u)
		}
	}
	if got := nonZeroPositions(state, witnessUser33); len(got) != 0 {
		t.Errorf("unmapped witness legs resolved anyway: %+v, want none — both reserve indexes are unknown", got)
	}

	// All four skipped legs (collateral and liabilities, buckets 0 and 1)
	// surface as unmapped_reserve_index diagnostics at the witness ledger, with
	// the ResData-only USDC reserve as the sole candidate — the skip is loud,
	// so a bounded replay that hits this is visibly incomplete.
	diags := adapter.LastDecodeDiagnostics()
	wantAmounts := map[string]string{
		"collateral|0": "210346315861",
		"collateral|1": "16523965334",
		"liability|0":  "14746315917",
		"liability|1":  "12665205938",
	}
	if len(diags) != len(wantAmounts) {
		t.Fatalf("diagnostics = %+v, want %d skipped-leg records", diags, len(wantAmounts))
	}
	for _, d := range diags {
		key := d.PositionType + "|" + strconv.Itoa(int(d.ReserveIndex))
		want, ok := wantAmounts[key]
		if !ok {
			t.Errorf("unexpected diagnostic leg %s", key)
			continue
		}
		delete(wantAmounts, key)
		if d.Code != bindings.DecodeDiagnosticUnmappedReserveIndex {
			t.Errorf("%s code = %q, want %q", key, d.Code, bindings.DecodeDiagnosticUnmappedReserveIndex)
		}
		if d.LedgerSeq != 62986834 || d.PoolContractID != yieldBloxPoolID || d.Address != witnessUser33 {
			t.Errorf("%s identity = {ledger %d, pool %q, address %q}, want the 62,986,834 witness",
				key, d.LedgerSeq, d.PoolContractID, d.Address)
		}
		if d.AmountRaw != want {
			t.Errorf("%s amount = %q, want %q", key, d.AmountRaw, want)
		}
		if len(d.CandidateAssetIDs) != 1 || d.CandidateAssetIDs[0] != usdcTokenID {
			t.Errorf("%s candidates = %v, want [USDC %s]", key, d.CandidateAssetIDs, usdcTokenID)
		}
	}
	if len(wantAmounts) != 0 {
		t.Errorf("diagnostics missing legs: %v", wantAmounts)
	}
}

// accountAddressValFromID builds the account-address ScVal for a real strkey
// account ID — the addressVal twin for G... wallets.
func accountAddressValFromID(t *testing.T, accountID string) xdr.ScVal {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteAccountID, accountID)
	if err != nil {
		t.Fatalf("decode account id %s: %v", accountID, err)
	}
	var pk xdr.Uint256
	copy(pk[:], raw)
	account, err := xdr.NewAccountId(xdr.PublicKeyTypePublicKeyTypeEd25519, pk)
	if err != nil {
		t.Fatalf("account id: %v", err)
	}
	address, err := xdr.NewScAddress(xdr.ScAddressTypeScAddressTypeAccount, account)
	if err != nil {
		t.Fatalf("account address: %v", err)
	}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &address}
}

func mustBase64(t *testing.T, s string) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("bad raw event base64: %v", err)
	}
	return raw
}

// keyIsPositionsFor reports whether keyXDR is the Positions(user) vec key.
func keyIsPositionsFor(t *testing.T, keyXDR, user string) bool {
	t.Helper()
	var key xdr.ScVal
	if err := xdr.SafeUnmarshalBase64(keyXDR, &key); err != nil {
		t.Fatalf("decode key: %v", err)
	}
	vec, ok := key.GetVec()
	if !ok || vec == nil || len(*vec) != 2 {
		return false
	}
	sym, ok := (*vec)[0].GetSym()
	if !ok || string(sym) != "Positions" {
		return false
	}
	addr, ok := (*vec)[1].GetAddress()
	if !ok {
		return false
	}
	got, err := addr.String()
	if err != nil {
		return false
	}
	return got == user
}
