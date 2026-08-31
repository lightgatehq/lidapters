// Structured event-field tests (lidapters#9 section 7): auction events decode
// their two parties (auctioned user vs filler), full lot/bid maps, percents
// and block into structured fields instead of the generic first-address /
// first-numeric scrape; claim's amount is the trailing scalar, not a reserve
// token id. Adversarial cases pin fail-safe behavior on truncated or garbage
// event data.
package blend

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/lightgatehq/lidapters/blend/contracts"
	"github.com/stellar/go-stellar-sdk/xdr"
)

func transformOneEvent(t *testing.T, raw []byte, topicJSON string) *bindings.TransformOutput {
	t.Helper()
	adapter, err := New(DefaultConfig())
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	out, err := adapter.Transform(bindings.TransformInput{
		LedgerSeq: 62986500,
		CloseTime: time.Unix(100, 0).UTC(),
		Events: []bindings.RawEventEnvelope{{
			LedgerSeq:  62986500,
			TxHash:     "tx-structured",
			EventIndex: 1,
			ContractID: validContractString(t, 60),
			Topic:      topicJSON,
			RawEvent:   raw,
			CloseTime:  time.Unix(100, 0).UTC(),
			Metadata:   map[string]string{"protocol_id": "blend"},
		}},
	})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	return out
}

func auctionDataVal(t *testing.T, lot, bid map[string]xdr.ScVal, block uint32) xdr.ScVal {
	t.Helper()
	lotEntries := make(xdr.ScMap, 0, len(lot))
	for _, val := range lot {
		lotEntries = append(lotEntries, xdr.ScMapEntry{Key: contractAddressVal(t, 8), Val: val})
	}
	bidEntries := make(xdr.ScMap, 0, len(bid))
	for _, val := range bid {
		bidEntries = append(bidEntries, xdr.ScMapEntry{Key: contractAddressVal(t, 9), Val: val})
	}
	lotPtr, bidPtr := &lotEntries, &bidEntries
	return mapVal(t, map[string]xdr.ScVal{
		"lot":   {Type: xdr.ScValTypeScvMap, Map: &lotPtr},
		"bid":   {Type: xdr.ScValTypeScvMap, Map: &bidPtr},
		"block": u32Val(block),
	})
}

// TestFillAuctionEventStructuredFields is the section-7 counterparty pin: a
// fill_auction event carries BOTH parties — the auctioned user (topics) as
// the address and the filler (data) as the counterparty — plus fill_percent,
// auction_type, block and the full lot/bid maps in metadata. Before this, the
// two parties collapsed to whichever address the generic scan found first.
func TestFillAuctionEventStructuredFields(t *testing.T) {
	t.Parallel()

	user := validAccountString(t, 63)
	filler := validAccountString(t, 64)
	lotAsset := validContractString(t, 8)
	bidAsset := validContractString(t, 9)

	raw := contractEventRaw(t,
		// Real v2 topic order: [symbol, auction_type: u32, user: Address].
		[]xdr.ScVal{symbolVal(t, "fill_auction"), u32Val(0), accountAddressVal(t, 63)},
		vecVal(
			accountAddressVal(t, 64), // filler
			i128Val(50),              // fill_percent
			auctionDataVal(t,
				map[string]xdr.ScVal{"a": i128Val(1_000_000)},
				map[string]xdr.ScVal{"b": i128MaxVal()},
				62986400,
			),
		),
	)
	out := transformOneEvent(t, raw, `{"topics":["fill_auction"]}`)
	if len(out.Quarantine) != 0 || len(out.Activities) != 1 {
		t.Fatalf("activities=%+v quarantine=%+v", out.Activities, out.Quarantine)
	}
	got := out.Activities[0]
	if got.ActivityType != contracts.ActivityTypeFillAuction {
		t.Fatalf("type = %s", got.ActivityType)
	}
	if got.Address != user {
		t.Fatalf("address = %s, want auctioned user %s", got.Address, user)
	}
	if got.Counterparty != filler {
		t.Fatalf("counterparty = %q, want filler %s", got.Counterparty, filler)
	}
	// Multi-asset event: no single AssetID/AmountRaw may be fabricated.
	if got.AssetID != "" || got.AmountRaw != "" || got.ShareAmount != "" {
		t.Fatalf("fabricated scalar fields: asset=%q amount=%q share=%q", got.AssetID, got.AmountRaw, got.ShareAmount)
	}
	wantChecks := map[string]string{
		"auction_type":  "user_liquidation",
		"fill_percent":  "50",
		"auction_block": "62986400",
		"event_name":    "fill_auction",
	}
	for key, want := range wantChecks {
		if got.Metadata[key] != want {
			t.Errorf("metadata[%s] = %q, want %q", key, got.Metadata[key], want)
		}
	}
	var lot map[string]string
	if err := json.Unmarshal([]byte(got.Metadata["auction_lot"]), &lot); err != nil {
		t.Fatalf("auction_lot not JSON: %q", got.Metadata["auction_lot"])
	}
	if lot[lotAsset] != "1000000" {
		t.Fatalf("auction_lot = %v", lot)
	}
	var bid map[string]string
	if err := json.Unmarshal([]byte(got.Metadata["auction_bid"]), &bid); err != nil {
		t.Fatalf("auction_bid not JSON: %q", got.Metadata["auction_bid"])
	}
	if bid[bidAsset] != i128MaxString {
		t.Fatalf("auction_bid = %v (extreme i128 must round-trip)", bid)
	}
}

func TestNewAuctionEventStructuredFields(t *testing.T) {
	t.Parallel()

	user := validAccountString(t, 63)
	raw := contractEventRaw(t,
		[]xdr.ScVal{symbolVal(t, "new_auction"), u32Val(1), accountAddressVal(t, 63)},
		vecVal(
			u32Val(100), // percent
			auctionDataVal(t, map[string]xdr.ScVal{"a": i128Val(5)}, map[string]xdr.ScVal{"b": i128Val(6)}, 10),
		),
	)
	out := transformOneEvent(t, raw, `{"topics":["new_auction"]}`)
	if len(out.Quarantine) != 0 || len(out.Activities) != 1 {
		t.Fatalf("activities=%+v quarantine=%+v", out.Activities, out.Quarantine)
	}
	got := out.Activities[0]
	if got.Address != user || got.Counterparty != "" {
		t.Fatalf("new_auction parties = address %q counterparty %q", got.Address, got.Counterparty)
	}
	if got.Metadata["auction_type"] != "bad_debt" || got.Metadata["auction_percent"] != "100" {
		t.Fatalf("metadata = %+v", got.Metadata)
	}
	if got.AssetID != "" || got.AmountRaw != "" {
		t.Fatalf("fabricated scalars: asset=%q amount=%q", got.AssetID, got.AmountRaw)
	}
}

// TestInterestAuctionUserIsContract pins the misfile fix: an interest
// auction's "user" is the backstop — a contract — which the generic address
// scan used to classify as the event's ASSET. It must land as the address.
func TestInterestAuctionUserIsContract(t *testing.T) {
	t.Parallel()

	backstop := validContractString(t, 70)
	raw := contractEventRaw(t,
		[]xdr.ScVal{symbolVal(t, "new_auction"), u32Val(2), contractAddressVal(t, 70)},
		vecVal(u32Val(100), auctionDataVal(t, map[string]xdr.ScVal{"a": i128Val(5)}, map[string]xdr.ScVal{}, 11)),
	)
	out := transformOneEvent(t, raw, `{"topics":["new_auction"]}`)
	if len(out.Quarantine) != 0 || len(out.Activities) != 1 {
		t.Fatalf("activities=%+v quarantine=%+v", out.Activities, out.Quarantine)
	}
	got := out.Activities[0]
	if got.Address != backstop {
		t.Fatalf("address = %q, want backstop contract %s", got.Address, backstop)
	}
	if got.AssetID != "" {
		t.Fatalf("auctioned contract misfiled as asset: %q", got.AssetID)
	}
	if got.Metadata["auction_type"] != "interest" {
		t.Fatalf("auction_type = %q", got.Metadata["auction_type"])
	}
}

// TestDeleteAuctionEventUnitData: delete_auction publishes unit data — the
// activity carries the user and type, and nothing fabricated.
func TestDeleteAuctionEventUnitData(t *testing.T) {
	t.Parallel()

	user := validAccountString(t, 63)
	raw := contractEventRaw(t,
		[]xdr.ScVal{symbolVal(t, "delete_auction"), u32Val(0), accountAddressVal(t, 63)},
		xdr.ScVal{Type: xdr.ScValTypeScvVoid},
	)
	out := transformOneEvent(t, raw, `{"topics":["delete_auction"]}`)
	if len(out.Quarantine) != 0 || len(out.Activities) != 1 {
		t.Fatalf("activities=%+v quarantine=%+v", out.Activities, out.Quarantine)
	}
	got := out.Activities[0]
	if got.ActivityType != contracts.ActivityTypeDeleteAuction || got.Address != user {
		t.Fatalf("delete_auction activity = %+v", got)
	}
	if got.AmountRaw != "" || got.ShareAmount != "" || got.Counterparty != "" {
		t.Fatalf("fabricated fields on unit-data event: %+v", got)
	}
	if got.Metadata["auction_type"] != "user_liquidation" {
		t.Fatalf("auction_type = %q", got.Metadata["auction_type"])
	}
}

// TestFillAuctionTruncatedDataFailsSafe: a fill event whose data lost the
// percent and auction data still produces the activity with both parties it
// can prove, and simply omits the rest — no panic, no invented values.
func TestFillAuctionTruncatedDataFailsSafe(t *testing.T) {
	t.Parallel()

	user := validAccountString(t, 63)
	raw := contractEventRaw(t,
		[]xdr.ScVal{symbolVal(t, "fill_auction"), u32Val(0), accountAddressVal(t, 63)},
		vecVal(accountAddressVal(t, 64)),
	)
	out := transformOneEvent(t, raw, `{"topics":["fill_auction"]}`)
	if len(out.Quarantine) != 0 || len(out.Activities) != 1 {
		t.Fatalf("activities=%+v quarantine=%+v", out.Activities, out.Quarantine)
	}
	got := out.Activities[0]
	if got.Address != user || got.Counterparty == "" {
		t.Fatalf("truncated fill parties = %+v", got)
	}
	for _, key := range []string{"fill_percent", "auction_lot", "auction_bid", "auction_block"} {
		if _, present := got.Metadata[key]; present {
			t.Fatalf("metadata[%s] fabricated from truncated data: %+v", key, got.Metadata)
		}
	}
}

// TestFillAuctionGarbageAuctionDataFailsSafe: a well-formed fill whose
// embedded AuctionData has a garbage lot map keeps block/bid and drops only
// auction_lot.
func TestFillAuctionGarbageAuctionDataFailsSafe(t *testing.T) {
	t.Parallel()

	badLot := xdr.ScMap{{Key: u32Val(1), Val: boolVal(true)}}
	badLotPtr := &badLot
	goodBid := xdr.ScMap{{Key: contractAddressVal(t, 9), Val: i128Val(7)}}
	goodBidPtr := &goodBid
	raw := contractEventRaw(t,
		[]xdr.ScVal{symbolVal(t, "fill_auction"), u32Val(0), accountAddressVal(t, 63)},
		vecVal(
			accountAddressVal(t, 64),
			i128Val(25),
			mapVal(t, map[string]xdr.ScVal{
				"lot":   {Type: xdr.ScValTypeScvMap, Map: &badLotPtr},
				"bid":   {Type: xdr.ScValTypeScvMap, Map: &goodBidPtr},
				"block": u32Val(3),
			}),
		),
	)
	out := transformOneEvent(t, raw, `{"topics":["fill_auction"]}`)
	if len(out.Activities) != 1 {
		t.Fatalf("expected activity, got %+v / %+v", out.Activities, out.Quarantine)
	}
	got := out.Activities[0]
	if _, present := got.Metadata["auction_lot"]; present {
		t.Fatalf("garbage lot surfaced: %q", got.Metadata["auction_lot"])
	}
	if got.Metadata["auction_block"] != "3" || got.Metadata["auction_bid"] == "" {
		t.Fatalf("independent pieces dropped with the garbage lot: %+v", got.Metadata)
	}
	if got.Metadata["fill_percent"] != "25" {
		t.Fatalf("fill_percent = %q", got.Metadata["fill_percent"])
	}
}

// TestClaimEventAmountIsTrailingScalar pins the claim misdecode fix: claim
// data is (reserve_token_ids: Vec<u32>, amount_claimed: i128), so the generic
// first-numeric scrape used to report a reserve token ID as the claimed
// amount.
func TestClaimEventAmountIsTrailingScalar(t *testing.T) {
	t.Parallel()

	raw := contractEventRaw(t,
		[]xdr.ScVal{symbolVal(t, "claim"), accountAddressVal(t, 63)},
		vecVal(vecVal(u32Val(2), u32Val(3)), i128Val(777_000_000)),
	)
	out := transformOneEvent(t, raw, `{"topics":["claim"]}`)
	if len(out.Quarantine) != 0 || len(out.Activities) != 1 {
		t.Fatalf("activities=%+v quarantine=%+v", out.Activities, out.Quarantine)
	}
	got := out.Activities[0]
	if got.ActivityType != contracts.ActivityTypeClaim {
		t.Fatalf("type = %s", got.ActivityType)
	}
	if got.AmountRaw != "777000000" {
		t.Fatalf("claim amount = %q, want 777000000 (the trailing scalar, not a token id)", got.AmountRaw)
	}
	if got.ShareAmount != "" {
		t.Fatalf("claim fabricated a share amount: %q", got.ShareAmount)
	}
}

// TestEventNameMetadataAlwaysPresent: the exact contract symbol rides in
// metadata for every decoded event — including legacy names whose activity
// type differs from the symbol.
func TestEventNameMetadataAlwaysPresent(t *testing.T) {
	t.Parallel()

	wallet := accountAddressVal(t, 7)
	asset := contractAddressVal(t, 8)
	raw := contractEventRaw(t, []xdr.ScVal{symbolVal(t, "supply"), asset, wallet},
		vecVal(i128Val(100), i128Val(90)))
	out := transformOneEvent(t, raw, `{"topics":["supply"]}`)
	if len(out.Activities) != 1 {
		t.Fatalf("expected activity, got %+v", out.Activities)
	}
	if out.Activities[0].Metadata["event_name"] != "supply" {
		t.Fatalf("event_name = %q", out.Activities[0].Metadata["event_name"])
	}

	legacy := contractEventRaw(t, []xdr.ScVal{symbolVal(t, "reserve_config")}, i128Val(1))
	out = transformOneEvent(t, legacy, `{"topics":["reserve_config"]}`)
	if len(out.Activities) != 1 {
		t.Fatalf("expected legacy activity, got %+v", out.Activities)
	}
	got := out.Activities[0]
	if got.ActivityType != contracts.ActivityTypeStatusChange || got.Metadata["event_name"] != "reserve_config" {
		t.Fatalf("legacy event_name pin: type=%s event_name=%q", got.ActivityType, got.Metadata["event_name"])
	}
}

// TestSupplyShareDecodeRegression re-pins the pre-existing two-amount decode
// (underlying + reserve shares) byte for byte after the structured-event
// changes — the generic path must be untouched for non-auction events.
func TestSupplyShareDecodeRegression(t *testing.T) {
	t.Parallel()

	raw := contractEventRaw(t,
		[]xdr.ScVal{symbolVal(t, "supply_collateral"), contractAddressVal(t, 8), accountAddressVal(t, 7)},
		vecVal(i128Val(1_000_000_000), i128Val(2_102_830_215)),
	)
	out := transformOneEvent(t, raw, `{"topics":["supply_collateral"]}`)
	if len(out.Activities) != 1 {
		t.Fatalf("expected activity, got %+v", out.Activities)
	}
	got := out.Activities[0]
	if got.AmountRaw != "1000000000" || got.ShareAmount != "2102830215" || got.ShareType != "collateral" {
		t.Fatalf("share decode drifted: %+v", got)
	}
	if got.Counterparty != "" {
		t.Fatalf("non-auction event fabricated a counterparty: %q", got.Counterparty)
	}
}
