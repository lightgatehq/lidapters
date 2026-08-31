package blend

import (
	"encoding/json"
	"math/big"
	"regexp"
	"strings"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/lightgatehq/lidapters/blend/contracts"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

var (
	addressRE = regexp.MustCompile(`[GC][A-Z2-7]{55}`)
)

type decodedEvent struct {
	isBlend      bool
	eventName    string
	activityType contracts.ActivityType
	address      string
	assetID      string
	amountRaw    string
	shareRaw     string
	direction    string
	reason       string
	counterparty string
	metadata     map[string]string
}

func decodeEvent(evt bindings.RawEventEnvelope) decodedEvent {
	out := decodedEvent{
		isBlend:  looksBlend(evt),
		metadata: map[string]string{"topic": evt.Topic},
	}
	for k, v := range evt.Metadata {
		out.metadata[k] = v
	}

	// Fixture and synthetic events may store JSON directly in raw_event.
	if parsed := decodeFixturePayload(evt.RawEvent); parsed.activityType != "" {
		mergeDecoded(&out, parsed)
		return out
	}

	// For ingest-produced events, topic is a JSON blob with human and XDR forms.
	if parsed := decodeTopicJSON(evt.Topic); parsed.activityType != "" {
		mergeDecoded(&out, parsed)
	}

	// Source of truth for chain events is raw Soroban ContractEvent XDR.
	if parsed := decodeContractEventXDR(evt.RawEvent); parsed.activityType != "" {
		mergeDecoded(&out, parsed)
	}

	if out.activityType == "" {
		if evt.Metadata["protocol_id"] == "blend" {
			out.reason = "unsupported_blend_v2_event"
		} else {
			out.reason = "unknown_blend_event_shape"
		}
	}
	if out.direction == "" {
		out.direction = directionForActivity(out.activityType)
	}
	if out.address == "" || out.assetID == "" {
		wallet, asset := extractAddresses(evt.Topic)
		if out.address == "" {
			out.address = wallet
		}
		// Auction events span multiple assets (per-asset lot/bid maps carried in
		// metadata), so a single AssetID would misattribute them — and the
		// auctioned user may itself be a contract (interest auctions), which this
		// heuristic would misread as the asset.
		if out.assetID == "" && !isAuctionActivity(out.activityType) {
			out.assetID = asset
		}
	}
	// The exact contract event symbol, preserved even for legacy names whose
	// activity type differs from it (deposit-era fixtures, reserve_config).
	if out.eventName != "" {
		out.metadata["event_name"] = out.eventName
	}
	return out
}

func isAuctionActivity(a contracts.ActivityType) bool {
	switch a {
	case contracts.ActivityTypeNewAuction, contracts.ActivityTypeFillAuction, contracts.ActivityTypeDeleteAuction:
		return true
	default:
		return false
	}
}

func decodeFixturePayload(raw []byte) decodedEvent {
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		return decodedEvent{}
	}
	evType, _ := v["type"].(string)
	if evType == "" {
		return decodedEvent{}
	}
	amount := jsonString(v["amount"])
	wallet := jsonString(v["wallet"])
	asset := jsonString(v["asset"])
	act := classifyEventName(evType)
	return decodedEvent{
		eventName:    strings.ToLower(strings.TrimSpace(evType)),
		activityType: act,
		address:      wallet,
		assetID:      asset,
		amountRaw:    amount,
		direction:    directionForActivity(act),
		metadata:     map[string]string{"fixture_type": evType},
	}
}

func decodeTopicJSON(topic string) decodedEvent {
	var payload map[string]any
	if err := json.Unmarshal([]byte(topic), &payload); err != nil {
		return decodedEvent{}
	}
	rawTopics, ok := payload["topics"].([]any)
	if !ok || len(rawTopics) == 0 {
		return decodedEvent{}
	}
	eventName := strings.ToLower(jsonString(rawTopics[0]))
	act := classifyEventName(eventName)
	if act == "" {
		return decodedEvent{}
	}
	if isAuctionActivity(act) {
		// Auction topics are [event, auction_type: u32, user]; the only address
		// there is the auctioned user. Amounts and assets are multi-asset
		// (per-asset lot/bid) and come from the XDR path's structured decode —
		// the generic scans below would misfile the auctioned user as the asset
		// (interest auctions auction the backstop, a contract) and the percent
		// as the amount.
		wallet := ""
		for _, val := range rawTopics[1:] {
			if s := jsonString(val); validActorAddress(s) {
				wallet = s
				break
			}
		}
		return decodedEvent{
			eventName:    eventName,
			activityType: act,
			address:      wallet,
			direction:    directionForActivity(act),
		}
	}
	wallet := ""
	asset := ""
	// Canonical Blend reserve events are [event, asset, actor]. The actor may
	// itself be a Soroban contract, so type-based scanning alone cannot tell the
	// two contract addresses apart.
	if len(rawTopics) > 1 {
		candidate := jsonString(rawTopics[1])
		if validContractAddress(candidate) {
			asset = candidate
		}
	}
	if len(rawTopics) > 2 {
		candidate := jsonString(rawTopics[2])
		if validActorAddress(candidate) {
			wallet = candidate
		}
	}
	for _, val := range rawTopics {
		s := jsonString(val)
		if wallet == "" && validAccountAddress(s) {
			wallet = s
		}
		if asset == "" && validContractAddress(s) {
			asset = s
		}
	}
	if wallet == "" || asset == "" {
		dataWallet, dataAsset := extractJSONAddresses(payload["data"])
		if wallet == "" {
			wallet = dataWallet
		}
		if asset == "" {
			asset = dataAsset
		}
	}
	amount := jsonString(payload["amount"])
	if amount == "" {
		amount = firstJSONNumeric(payload["data"])
	}
	if dataXDR := jsonString(payload["data_xdr"]); dataXDR != "" {
		if data, ok := decodeScValBase64(dataXDR); ok {
			dataWallet, dataAsset := collectScValAddresses(data)
			if wallet == "" {
				wallet = dataWallet
			}
			if asset == "" {
				asset = dataAsset
			}
			numbers := scValNumerics(data)
			if amount == "" && len(numbers) > 0 {
				amount = numbers[0]
			}
			share := ""
			if len(numbers) > 1 {
				share = numbers[1]
			}
			return decodedEvent{
				eventName:    eventName,
				activityType: act,
				address:      wallet,
				assetID:      asset,
				amountRaw:    amount,
				shareRaw:     share,
				direction:    directionForActivity(act),
			}
		}
	}
	return decodedEvent{
		eventName:    eventName,
		activityType: act,
		address:      wallet,
		assetID:      asset,
		amountRaw:    amount,
		direction:    directionForActivity(act),
	}
}

func decodeContractEventXDR(raw []byte) decodedEvent {
	var evt xdr.ContractEvent
	if err := xdr.SafeUnmarshal(raw, &evt); err != nil {
		return decodedEvent{}
	}
	v0, ok := evt.Body.GetV0()
	if !ok {
		return decodedEvent{}
	}
	eventName := ""
	wallet := ""
	asset := ""
	addresses := make([]string, 0, len(v0.Topics))
	for _, topic := range v0.Topics {
		if eventName == "" {
			if symbol := scValSymbol(topic); symbol != "" {
				eventName = symbol
				continue
			}
		}
		if addr := scValAddress(topic); addr != "" {
			addresses = append(addresses, addr)
		}
	}
	act := classifyEventName(eventName)
	if act == "" {
		return decodedEvent{}
	}
	if isAuctionActivity(act) {
		return decodeAuctionEventXDR(eventName, act, v0)
	}
	if len(addresses) > 0 && validContractAddress(addresses[0]) {
		asset = addresses[0]
	}
	if len(addresses) > 1 && validActorAddress(addresses[1]) {
		wallet = addresses[1]
	}
	if wallet == "" && len(addresses) > 0 && !validContractAddress(addresses[0]) && validAccountAddress(addresses[0]) {
		// Events like claim carry only the actor in their topics ([claim,
		// from]): an account in the first address slot can never be the asset
		// (assets are contracts), so it is the wallet.
		wallet = addresses[0]
	}
	dataWallet, dataAsset := collectScValAddresses(v0.Data)
	if wallet == "" {
		wallet = dataWallet
	}
	if asset == "" {
		asset = dataAsset
	}
	numbers := scValNumerics(v0.Data)
	amount := ""
	share := ""
	if len(numbers) > 0 {
		amount = numbers[0]
	}
	if len(numbers) > 1 {
		share = numbers[1]
	}
	if act == contracts.ActivityTypeClaim && len(numbers) > 0 {
		// claim data is (reserve_token_ids: Vec<u32>, amount_claimed: i128), so
		// the first numeric is a reserve token ID, not the amount — the claimed
		// amount is the trailing scalar. There is no share quantity here.
		amount = numbers[len(numbers)-1]
		share = ""
	}
	return decodedEvent{
		eventName:    strings.ToLower(strings.TrimSpace(eventName)),
		activityType: act,
		address:      wallet,
		assetID:      asset,
		amountRaw:    amount,
		shareRaw:     share,
		direction:    directionForActivity(act),
	}
}

// decodeAuctionEventXDR is the structured decode for new_auction /
// fill_auction / delete_auction contract events. Topics are [symbol,
// auction_type: u32, user: Address] — the auctioned user is the activity
// address. Data varies by event: fill carries (filler, fill_percent,
// AuctionData) so the filler lands as the counterparty (the liquidation's
// second party, previously collapsed away); new carries (percent,
// AuctionData); delete carries unit. The AuctionData lot/bid maps are
// per-asset, so no single AssetID/AmountRaw is fabricated — the full maps
// ride in metadata (auction_lot / auction_bid, canonical sorted-key JSON)
// alongside auction_type, auction_block and the percent. Any piece that
// fails to decode is simply absent from metadata, never guessed.
func decodeAuctionEventXDR(eventName string, act contracts.ActivityType, v0 xdr.ContractEventV0) decodedEvent {
	out := decodedEvent{
		eventName:    strings.ToLower(strings.TrimSpace(eventName)),
		activityType: act,
		direction:    directionForActivity(act),
		metadata:     map[string]string{},
	}
	for _, topic := range v0.Topics[1:] {
		if _, tagged := out.metadata["auction_type"]; !tagged && topic.Type == xdr.ScValTypeScvU32 {
			if auctType, ok := scInt32(topic); ok {
				out.metadata["auction_type"] = auctionTypeLabel(auctType)
				continue
			}
		}
		if out.address == "" {
			if addr := scValAddress(topic); addr != "" {
				out.address = addr
			}
		}
	}
	elems := []xdr.ScVal{v0.Data}
	if vec, ok := scVec(v0.Data); ok {
		elems = vec
	}
	for _, elem := range elems {
		if addr := scValAddress(elem); addr != "" {
			if out.counterparty == "" {
				out.counterparty = addr
			}
			continue
		}
		if fields := scMapFields(elem); fields != nil {
			mergeAuctionDataMetadata(out.metadata, fields)
			continue
		}
		if n, ok := scIntString(elem); ok {
			key := "auction_percent"
			if act == contracts.ActivityTypeFillAuction {
				key = "fill_percent"
			}
			if _, exists := out.metadata[key]; !exists {
				out.metadata[key] = n
			}
		}
	}
	return out
}

// mergeAuctionDataMetadata surfaces an event-embedded AuctionData struct
// ({bid, lot, block}) into activity metadata. Each piece is independent: a
// malformed lot map drops only auction_lot, it does not take block or bid
// with it.
func mergeAuctionDataMetadata(metadata map[string]string, fields map[string]xdr.ScVal) {
	if block, ok := fieldIntString(fields, "block"); ok {
		metadata["auction_block"] = block
	}
	if lot, ok := decodeAuctionEntries(fields["lot"]); ok {
		metadata["auction_lot"] = auctionEntriesJSON(lot)
	}
	if bid, ok := decodeAuctionEntries(fields["bid"]); ok {
		metadata["auction_bid"] = auctionEntriesJSON(bid)
	}
}

// auctionEntriesJSON renders lot/bid entries as canonical JSON — a
// {asset: amount} object whose keys json.Marshal sorts, so the same map is
// byte-identical run to run.
func auctionEntriesJSON(entries []contracts.AuctionEntry) string {
	m := make(map[string]string, len(entries))
	for _, entry := range entries {
		m[entry.AssetID] = entry.AmountRaw
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(raw)
}

func looksBlend(evt bindings.RawEventEnvelope) bool {
	if evt.Metadata["protocol_id"] == "blend" {
		return true
	}
	s := strings.ToLower(evt.ContractID + " " + evt.Topic)
	keys := []string{
		"blend", "pool", "backstop", "supply", "borrow", "repay", "withdraw", "liquid", "emission", "flash",
		"auction", "gulp", "defaulted_debt",
	}
	for _, k := range keys {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// exactEventActivities maps every blend-contracts-v2 pool event symbol to an
// activity type carrying the exact on-chain name. This replaced a substring
// keyword matcher that silently dropped the auction and emission events
// (fill_auction — the on-chain shape of a liquidation — new_auction,
// delete_auction, gulp_emissions, reserve_emission_update, gulp, defaulted_debt,
// set_admin, update_pool, ...) and misfiled others (supply → deposit,
// set_reserve → contract_status_change). The names must stay in lock-step with
// gold's activity_type enum (relay migration 017, relay.lightgate.xyz#65/#75).
// The auction subtype (user_liquidation / bad_debt / interest) is a u32 topic
// discriminator in v2, not part of the event symbol.
var exactEventActivities = map[string]contracts.ActivityType{
	"supply":                  contracts.ActivityTypeSupply,
	"withdraw":                contracts.ActivityTypeWithdraw,
	"supply_collateral":       contracts.ActivityTypeSupplyCollateral,
	"withdraw_collateral":     contracts.ActivityTypeWithdrawCollateral,
	"borrow":                  contracts.ActivityTypeBorrow,
	"repay":                   contracts.ActivityTypeRepay,
	"flash_loan":              contracts.ActivityTypeFlashLoan,
	"claim":                   contracts.ActivityTypeClaim,
	"new_auction":             contracts.ActivityTypeNewAuction,
	"fill_auction":            contracts.ActivityTypeFillAuction,
	"delete_auction":          contracts.ActivityTypeDeleteAuction,
	"set_status":              contracts.ActivityTypeSetStatus,
	"set_reserve":             contracts.ActivityTypeSetReserve,
	"queue_set_reserve":       contracts.ActivityTypeQueueSetReserve,
	"cancel_set_reserve":      contracts.ActivityTypeCancelSetReserve,
	"update_pool":             contracts.ActivityTypeUpdatePool,
	"set_admin":               contracts.ActivityTypeSetAdmin,
	"gulp":                    contracts.ActivityTypeGulp,
	"gulp_emissions":          contracts.ActivityTypeGulpEmissions,
	"reserve_emission_update": contracts.ActivityTypeReserveEmissionUpdate,
	"defaulted_debt":          contracts.ActivityTypeDefaultedDebt,
}

// legacyEventActivities keeps the pre-exact vocabulary decoding: fixture and
// synthetic events name activities by legacy type directly, and the v1-era
// reserve_config lifecycle event keeps its contract_status_change identity.
var legacyEventActivities = map[string]contracts.ActivityType{
	"deposit":                contracts.ActivityTypeDeposit,
	"liquidation":            contracts.ActivityTypeLiquidation,
	"claim_rewards":          contracts.ActivityTypeClaimRewards,
	"bad_debt":               contracts.ActivityTypeBadDebt,
	"baddebt":                contracts.ActivityTypeBadDebt,
	"reserve_config":         contracts.ActivityTypeStatusChange,
	"contract_status_change": contracts.ActivityTypeStatusChange,
}

func classifyEventName(name string) contracts.ActivityType {
	s := strings.ToLower(strings.TrimSpace(name))
	if act, ok := exactEventActivities[s]; ok {
		return act
	}
	if act, ok := legacyEventActivities[s]; ok {
		return act
	}
	return ""
}

func directionForActivity(a contracts.ActivityType) string {
	switch a {
	case contracts.ActivityTypeDeposit, contracts.ActivityTypeRepay, contracts.ActivityTypeClaimRewards,
		contracts.ActivityTypeSupply, contracts.ActivityTypeSupplyCollateral, contracts.ActivityTypeClaim:
		return "in"
	case contracts.ActivityTypeWithdraw, contracts.ActivityTypeBorrow, contracts.ActivityTypeWithdrawCollateral:
		return "out"
	default:
		return "neutral"
	}
}

// contractScopedActivity reports whether an activity type is a pool-lifecycle
// fact with no guaranteed wallet actor in the event (set_status from the
// permissionless update_status path, gulp_emissions, reserve_emission_update,
// gulp, defaulted_debt carry no address at all; the admin-triggered config
// events may). For these the adapter falls back to the emitting contract as
// the activity address instead of quarantining on missing_activity_address —
// the same fallback contract_status_change always had. Auction events are
// excluded: they always carry the auctioned user in their topics, so a missing
// address there is a real decode failure.
func contractScopedActivity(a contracts.ActivityType) bool {
	switch a {
	case contracts.ActivityTypeStatusChange,
		contracts.ActivityTypeSetStatus,
		contracts.ActivityTypeSetReserve,
		contracts.ActivityTypeQueueSetReserve,
		contracts.ActivityTypeCancelSetReserve,
		contracts.ActivityTypeUpdatePool,
		contracts.ActivityTypeSetAdmin,
		contracts.ActivityTypeGulp,
		contracts.ActivityTypeGulpEmissions,
		contracts.ActivityTypeReserveEmissionUpdate,
		contracts.ActivityTypeDefaultedDebt:
		return true
	default:
		return false
	}
}

// shareTypeForActivity classifies the position-share an activity moves, using
// the same vocabulary as PositionType (supply / liability). Deposits and
// withdrawals move supply shares; borrows and repays move liability. Activities
// whose share semantics are not determinable from the type alone (liquidation,
// claim_rewards, status changes, etc.) return "" so the store can COALESCE.
func shareTypeForEvent(eventName string, a contracts.ActivityType) string {
	name := strings.ToLower(strings.TrimSpace(eventName))
	if strings.Contains(name, "collateral") {
		return string(contracts.PositionTypeCollateral)
	}
	switch a {
	case contracts.ActivityTypeSupplyCollateral, contracts.ActivityTypeWithdrawCollateral:
		return string(contracts.PositionTypeCollateral)
	case contracts.ActivityTypeDeposit, contracts.ActivityTypeWithdraw, contracts.ActivityTypeSupply:
		return string(contracts.PositionTypeSupply)
	case contracts.ActivityTypeBorrow, contracts.ActivityTypeRepay:
		return string(contracts.PositionTypeLiability)
	default:
		return ""
	}
}

func extractAddresses(s string) (wallet, asset string) {
	matches := addressRE.FindAllString(s, -1)
	for _, m := range matches {
		if wallet == "" && validAccountAddress(m) {
			wallet = m
			continue
		}
		if asset == "" && validContractAddress(m) {
			asset = m
		}
	}
	return wallet, asset
}

func extractJSONAddresses(v any) (wallet, asset string) {
	switch val := v.(type) {
	case string:
		return extractAddresses(val)
	case []any:
		for _, item := range val {
			w, a := extractJSONAddresses(item)
			if wallet == "" {
				wallet = w
			}
			if asset == "" {
				asset = a
			}
		}
	case map[string]any:
		for key, item := range val {
			if strings.HasSuffix(strings.ToLower(key), "_xdr") || strings.EqualFold(key, "raw_event") {
				continue
			}
			w, a := extractJSONAddresses(item)
			if wallet == "" {
				wallet = w
			}
			if asset == "" {
				asset = a
			}
		}
	}
	return wallet, asset
}

func firstJSONNumeric(v any) string {
	switch val := v.(type) {
	case float64, json.Number:
		return jsonString(val)
	case []any:
		for _, item := range val {
			if n := firstJSONNumeric(item); n != "" {
				return n
			}
		}
	case map[string]any:
		for _, key := range []string{"amount", "tokens", "shares", "share_amount"} {
			if n := firstJSONNumeric(val[key]); n != "" {
				return n
			}
		}
	}
	return ""
}

func decodeScValBase64(raw string) (xdr.ScVal, bool) {
	if raw == "" {
		return xdr.ScVal{}, false
	}
	var out xdr.ScVal
	if err := xdr.SafeUnmarshalBase64(raw, &out); err != nil {
		return xdr.ScVal{}, false
	}
	return out, true
}

func collectScValAddresses(val xdr.ScVal) (wallet, asset string) {
	if addr := scValAddress(val); addr != "" {
		if validAccountAddress(addr) {
			return addr, ""
		}
		if validContractAddress(addr) {
			return "", addr
		}
	}
	switch val.Type {
	case xdr.ScValTypeScvVec:
		if val.Vec != nil && *val.Vec != nil {
			for _, item := range **val.Vec {
				w, a := collectScValAddresses(item)
				if wallet == "" {
					wallet = w
				}
				if asset == "" {
					asset = a
				}
			}
		}
	case xdr.ScValTypeScvMap:
		if val.Map != nil && *val.Map != nil {
			for _, entry := range **val.Map {
				w, a := collectScValAddresses(entry.Val)
				if wallet == "" {
					wallet = w
				}
				if asset == "" {
					asset = a
				}
			}
		}
	}
	return wallet, asset
}

func validAccountAddress(address string) bool {
	return strkey.IsValidEd25519PublicKey(address)
}

func validContractAddress(address string) bool {
	return strkey.IsValidContractAddress(address)
}

func validActorAddress(address string) bool {
	return validAccountAddress(address) || validContractAddress(address)
}

func mergeDecoded(target *decodedEvent, src decodedEvent) {
	if src.eventName != "" {
		target.eventName = src.eventName
	}
	if src.activityType != "" {
		target.activityType = src.activityType
	}
	if src.address != "" {
		target.address = src.address
	}
	if src.assetID != "" {
		target.assetID = src.assetID
	}
	if src.amountRaw != "" {
		target.amountRaw = src.amountRaw
	}
	if src.shareRaw != "" {
		target.shareRaw = src.shareRaw
	}
	if src.direction != "" {
		target.direction = src.direction
	}
	if src.counterparty != "" {
		target.counterparty = src.counterparty
	}
	for k, v := range src.metadata {
		target.metadata[k] = v
	}
}

func jsonString(v any) string {
	switch val := v.(type) {
	case string:
		return val
	case float64:
		return strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(big.NewFloat(val).Text('f', -1)), ".0"), ".")
	case json.Number:
		return val.String()
	default:
		return ""
	}
}

func scValSymbol(val xdr.ScVal) string {
	switch val.Type {
	case xdr.ScValTypeScvSymbol:
		if val.Sym != nil {
			return string(*val.Sym)
		}
	case xdr.ScValTypeScvString:
		if val.Str != nil {
			return string(*val.Str)
		}
	}
	return ""
}

func scValAddress(val xdr.ScVal) string {
	if val.Type != xdr.ScValTypeScvAddress || val.Address == nil {
		return ""
	}
	addr := *val.Address
	switch addr.Type {
	case xdr.ScAddressTypeScAddressTypeAccount:
		if addr.AccountId == nil {
			return ""
		}
		encoded, err := strkey.Encode(strkey.VersionByteAccountID, addr.AccountId.Ed25519[:])
		if err != nil {
			return ""
		}
		return encoded
	case xdr.ScAddressTypeScAddressTypeContract:
		if addr.ContractId == nil {
			return ""
		}
		encoded, err := strkey.Encode(strkey.VersionByteContract, addr.ContractId[:])
		if err != nil {
			return ""
		}
		return encoded
	default:
		return ""
	}
}

func scValNumeric(val xdr.ScVal) string {
	switch val.Type {
	case xdr.ScValTypeScvI32:
		if val.I32 != nil {
			return big.NewInt(int64(*val.I32)).String()
		}
	case xdr.ScValTypeScvU32:
		if val.U32 != nil {
			return new(big.Int).SetUint64(uint64(*val.U32)).String()
		}
	case xdr.ScValTypeScvI64:
		if val.I64 != nil {
			return big.NewInt(int64(*val.I64)).String()
		}
	case xdr.ScValTypeScvU64:
		if val.U64 != nil {
			return new(big.Int).SetUint64(uint64(*val.U64)).String()
		}
	case xdr.ScValTypeScvI128:
		if val.I128 != nil {
			return int128ToString(*val.I128)
		}
	case xdr.ScValTypeScvU128:
		if val.U128 != nil {
			return uint128ToString(*val.U128)
		}
	case xdr.ScValTypeScvVec:
		if val.Vec != nil && *val.Vec != nil {
			for _, item := range **val.Vec {
				if n := scValNumeric(item); n != "" {
					return n
				}
			}
		}
	case xdr.ScValTypeScvMap:
		if val.Map != nil && *val.Map != nil {
			for _, entry := range **val.Map {
				if n := scValNumeric(entry.Val); n != "" {
					return n
				}
			}
		}
	}
	return ""
}

// scValNumerics returns scalar numeric values in wire order. Blend V2 activity
// data is commonly [underlying_amount, reserve_share_amount]; preserving both
// values avoids reconstructing shares with lossy, action-dependent rounding.
func scValNumerics(val xdr.ScVal) []string {
	switch val.Type {
	case xdr.ScValTypeScvVec:
		var out []string
		if val.Vec != nil && *val.Vec != nil {
			for _, item := range **val.Vec {
				out = append(out, scValNumerics(item)...)
			}
		}
		return out
	case xdr.ScValTypeScvMap:
		var out []string
		if val.Map != nil && *val.Map != nil {
			for _, entry := range **val.Map {
				out = append(out, scValNumerics(entry.Val)...)
			}
		}
		return out
	default:
		if n := scValNumeric(val); n != "" {
			return []string{n}
		}
		return nil
	}
}

func uint128ToString(val xdr.UInt128Parts) string {
	hi := new(big.Int).SetUint64(uint64(val.Hi))
	lo := new(big.Int).SetUint64(uint64(val.Lo))
	hi.Lsh(hi, 64)
	hi.Add(hi, lo)
	return hi.String()
}

func int128ToString(val xdr.Int128Parts) string {
	hi := new(big.Int).SetUint64(uint64(val.Hi))
	lo := new(big.Int).SetUint64(uint64(val.Lo))

	if uint64(val.Hi)&(uint64(1)<<63) != 0 {
		hi.Sub(hi, new(big.Int).Lsh(big.NewInt(1), 64))
	}
	hi.Lsh(hi, 64)
	hi.Add(hi, lo)
	return hi.String()
}
