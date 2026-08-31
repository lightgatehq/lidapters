package blend

import (
	"testing"
	"time"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/lightgatehq/lidapters/blend/contracts"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// relayExactEventNames are the 17 values relay migration 017 added to gold's
// activity_type enum (relay.lightgate.xyz#65/#75). classifyEventName must map
// each one to itself — any drift here and relay's normalizeActivityType
// coerces the row to contract_status_change, which then fails gold's
// lifecycle_synthetic_identity CHECK.
var relayExactEventNames = []string{
	"supply", "supply_collateral", "withdraw_collateral", "claim",
	"new_auction", "fill_auction", "delete_auction",
	"set_status", "set_reserve", "queue_set_reserve", "cancel_set_reserve",
	"update_pool", "set_admin", "gulp", "gulp_emissions",
	"reserve_emission_update", "defaulted_debt",
}

func TestClassifyEventNameExactV2Vocabulary(t *testing.T) {
	t.Parallel()

	// Every migration-017 name classifies to itself.
	for _, name := range relayExactEventNames {
		if got := classifyEventName(name); string(got) != name {
			t.Errorf("classifyEventName(%q) = %q, want exact name", name, got)
		}
	}

	// The four v2 events that share their spelling with legacy enum values.
	for _, name := range []string{"withdraw", "borrow", "repay", "flash_loan"} {
		if got := classifyEventName(name); string(got) != name {
			t.Errorf("classifyEventName(%q) = %q, want exact name", name, got)
		}
	}

	// Case and whitespace are normalized, never part of the identity.
	if got := classifyEventName("  Fill_Auction "); got != contracts.ActivityTypeFillAuction {
		t.Errorf("classifyEventName normalized = %q, want %q", got, contracts.ActivityTypeFillAuction)
	}
}

func TestClassifyEventNameLegacyVocabularyPreserved(t *testing.T) {
	t.Parallel()

	cases := map[string]contracts.ActivityType{
		"deposit":                contracts.ActivityTypeDeposit,
		"liquidation":            contracts.ActivityTypeLiquidation,
		"claim_rewards":          contracts.ActivityTypeClaimRewards,
		"bad_debt":               contracts.ActivityTypeBadDebt,
		"baddebt":                contracts.ActivityTypeBadDebt,
		"reserve_config":         contracts.ActivityTypeStatusChange,
		"contract_status_change": contracts.ActivityTypeStatusChange,
	}
	for name, want := range cases {
		if got := classifyEventName(name); got != want {
			t.Errorf("classifyEventName(%q) = %q, want legacy %q", name, got, want)
		}
	}
}

// TestClassifyEventNameUnknownFallsBackEmpty is the regression guard for the
// fallback path: an unrecognized name returns "" so the adapter quarantines it
// with a reason instead of guessing a type. The old substring matcher made
// exactly that mistake ("supply_xyz" → deposit); these near-miss spellings must
// no longer classify.
func TestClassifyEventNameUnknownFallsBackEmpty(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"", "auction", "supply_xyz", "withdrawal", "new_liquidation_auction",
		"totally_made_up_event", "fill", "emission",
	} {
		if got := classifyEventName(name); got != "" {
			t.Errorf("classifyEventName(%q) = %q, want empty fallback", name, got)
		}
	}
}

// TestFillAuctionEventProducesLiquidationActivity is the #65 acceptance pin at
// the adapter level: a fill_auction contract event — the on-chain shape of a
// liquidation, previously dropped by the substring classifier — decodes into an
// activity under its exact name with the auctioned user as address and the raw
// tx identity intact.
func TestFillAuctionEventProducesLiquidationActivity(t *testing.T) {
	t.Parallel()

	adapter, err := New(DefaultConfig())
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	user := accountAddressVal(t, 63)
	raw := contractEventRaw(t,
		[]xdr.ScVal{symbolVal(t, "fill_auction"), user, u32Val(0)},
		vecVal(accountAddressVal(t, 64), i128Val(50)),
	)

	out, err := adapter.Transform(bindings.TransformInput{
		LedgerSeq: 62986500,
		CloseTime: time.Unix(100, 0).UTC(),
		Events: []bindings.RawEventEnvelope{{
			LedgerSeq:  62986500,
			TxHash:     "tx-fill-auction",
			EventIndex: 2,
			ContractID: validContractString(t, 65),
			Topic:      `{"topics":["fill_auction"]}`,
			RawEvent:   raw,
			CloseTime:  time.Unix(100, 0).UTC(),
			Metadata:   map[string]string{"protocol_id": "blend"},
		}},
	})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if len(out.Quarantine) != 0 {
		t.Fatalf("fill_auction quarantined: %+v", out.Quarantine)
	}
	if len(out.Activities) != 1 {
		t.Fatalf("expected one fill_auction activity, got %d", len(out.Activities))
	}
	got := out.Activities[0]
	if got.ActivityType != contracts.ActivityTypeFillAuction {
		t.Fatalf("expected %s, got %s", contracts.ActivityTypeFillAuction, got.ActivityType)
	}
	if got.TxHash != "tx-fill-auction" || got.EventIndex != 2 {
		t.Fatalf("expected raw identity, got %s/%d", got.TxHash, got.EventIndex)
	}
	if got.Address == "" {
		t.Fatalf("expected auctioned user address on fill_auction activity")
	}
}

// TestEmissionEventsClassifyExactWithContractFallback pins the emission-side
// half of #65: gulp_emissions and reserve_emission_update carry no address at
// all on-chain, so the adapter must fall back to the emitting contract as the
// activity address — exact type, raw identity, no quarantine.
func TestEmissionEventsClassifyExactWithContractFallback(t *testing.T) {
	t.Parallel()

	adapter, err := New(DefaultConfig())
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	cases := []struct {
		event string
		want  contracts.ActivityType
	}{
		{"gulp_emissions", contracts.ActivityTypeGulpEmissions},
		{"reserve_emission_update", contracts.ActivityTypeReserveEmissionUpdate},
	}
	for _, tc := range cases {
		t.Run(tc.event, func(t *testing.T) {
			contractID := validContractString(t, 66)
			raw := contractEventRaw(t, []xdr.ScVal{symbolVal(t, tc.event)}, i128Val(777))
			out, err := adapter.Transform(bindings.TransformInput{
				LedgerSeq: 200,
				CloseTime: time.Unix(100, 0).UTC(),
				Events: []bindings.RawEventEnvelope{{
					LedgerSeq:  200,
					TxHash:     "tx-" + tc.event,
					EventIndex: 1,
					ContractID: contractID,
					Topic:      `{"topics":["` + tc.event + `"]}`,
					RawEvent:   raw,
					CloseTime:  time.Unix(100, 0).UTC(),
					Metadata:   map[string]string{"protocol_id": "blend"},
				}},
			})
			if err != nil {
				t.Fatalf("transform: %v", err)
			}
			if len(out.Quarantine) != 0 {
				t.Fatalf("%s quarantined: %+v", tc.event, out.Quarantine)
			}
			if len(out.Activities) != 1 {
				t.Fatalf("expected one %s activity, got %d", tc.event, len(out.Activities))
			}
			got := out.Activities[0]
			if got.ActivityType != tc.want {
				t.Fatalf("expected %s, got %s", tc.want, got.ActivityType)
			}
			if got.Address != contractID {
				t.Fatalf("expected contract fallback address %s, got %s", contractID, got.Address)
			}
			if got.TxHash != "tx-"+tc.event || got.EventIndex != 1 {
				t.Fatalf("expected raw identity, got %s/%d", got.TxHash, got.EventIndex)
			}
		})
	}
}
