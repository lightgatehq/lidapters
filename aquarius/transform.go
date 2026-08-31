package aquarius

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/lightgatehq/lidapters/blend/contracts"
	"github.com/stellar/go-stellar-sdk/xdr"
)

func stableID(parts ...any) string {
	var b strings.Builder
	for _, p := range parts {
		if b.Len() > 0 {
			b.WriteByte('|')
		}
		b.WriteString(fmt.Sprint(p))
	}
	s := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(s[:])
}

func (a *Adapter) Transform(in bindings.TransformInput) (*bindings.TransformOutput, error) {
	out := &bindings.TransformOutput{LedgerSeq: in.LedgerSeq}
	if in.State == nil {
		return out, nil
	}
	pools := map[string]bindings.AMMPoolState{}
	for _, p := range in.State.AMMPools {
		if p.Protocol == a.cfg.Protocol {
			pools[p.ContractID] = p
			out.AMMPools = append(out.AMMPools, bindings.AMMPool{Protocol: a.cfg.Protocol, RouterContract: p.RouterContract, PoolHash: p.PoolHash, ContractID: p.ContractID, PoolType: p.PoolType, WasmHash: p.WasmHash, Tokens: p.Tokens, TotalSharesRaw: p.TotalSharesRaw, FeeFractionRaw: p.FeeFractionRaw, ProtocolFeeFractionRaw: p.ProtocolFeeFractionRaw, AmplificationRaw: p.AmplificationRaw, TickSpacing: p.TickSpacing, SqrtPriceX96: p.SqrtPriceX96, CurrentTick: p.CurrentTick, ActiveLiquidityRaw: p.ActiveLiquidityRaw, LedgerSeq: in.LedgerSeq, Timestamp: in.CloseTime})
		}
	}
	for _, pos := range in.State.AMMPositions {
		pool, ok := pools[pos.PoolContractID]
		if !ok {
			// A position whose pool never folded (bounded replay without the
			// pool's instance write or seed) cannot be decomposed — but it
			// must not vanish silently. Quarantine, don't drop.
			out.Quarantine = append(out.Quarantine, bindings.QuarantineEvent{ID: stableID(a.ID(), pos.PoolContractID, pos.Address, in.LedgerSeq), AdapterID: a.ID(), LedgerSeq: in.LedgerSeq, ContractID: pos.PoolContractID, Reason: "aquarius_position_unknown_pool"})
			continue
		}
		group := stableID(a.cfg.Protocol, pos.Address, pos.PoolContractID, pos.TickLower, pos.TickUpper)
		if pool.PoolType == "concentrated" {
			appendRangeComponents(out, group, pos, pool, in)
		} else if pos.SharesRaw != "" && pos.SharesRaw != "0" {
			for _, t := range pool.Tokens {
				amount, e := proRata(pos.SharesRaw, t.ReserveRaw, pool.TotalSharesRaw)
				if e != nil {
					out.Quarantine = append(out.Quarantine, bindings.QuarantineEvent{ID: stableID(group, t.AssetID, in.LedgerSeq), AdapterID: a.ID(), LedgerSeq: in.LedgerSeq, ContractID: pool.ContractID, Reason: "invalid_lp_share_state"})
					continue
				}
				out.AMMComponents = append(out.AMMComponents, component(group, pos, a.cfg.Protocol, t.AssetID, "lp_principal", amount, pos.SharesRaw, nil, nil, in, false))
			}
		} else if pos.SharesRaw == "0" && pos.HadShares {
			// Closed classic LP position: write explicit zero tombstones so the
			// latest-per-id current view stops surfacing the pre-close rows.
			// HadShares distinguishes a real close from a never-held position.
			for _, t := range pool.Tokens {
				out.AMMComponents = append(out.AMMComponents, component(group, pos, a.cfg.Protocol, t.AssetID, "lp_principal", "0", "0", nil, nil, in, true))
			}
		}
		if pending := pendingReward(pool, pos, in.CloseTime); pending != "" && pending != "0" {
			out.AMMRewards = append(out.AMMRewards, bindings.AMMReward{ID: stableID(group, "aqua", pos.RewardTokenID), PositionGroupID: group, Address: pos.Address, Protocol: a.cfg.Protocol, PoolContractID: pos.PoolContractID, RewardTokenID: pos.RewardTokenID, RewardKind: "aqua", AmountRaw: pending, LedgerSeq: in.LedgerSeq, Timestamp: in.CloseTime, Metadata: map[string]string{"price_unavailable": "true"}})
		}
	}
	for _, evt := range in.Events {
		acts, qs := a.decodeActivities(evt, a.eventClass(evt.ContractID, pools))
		out.Activities = append(out.Activities, acts...)
		out.Quarantine = append(out.Quarantine, qs...)
	}
	sort.Slice(out.AMMPools, func(i, j int) bool { return out.AMMPools[i].ContractID < out.AMMPools[j].ContractID })
	sort.Slice(out.AMMComponents, func(i, j int) bool { return out.AMMComponents[i].ID < out.AMMComponents[j].ID })
	return out, nil
}

func component(group string, p bindings.AMMPositionState, protocol, asset, kind, amount, shares string, lower, upper *int32, in bindings.TransformInput, closed bool) bindings.AMMPositionComponent {
	metadata := map[string]string{"price_unavailable": "true", "apr_partial": "true"}
	if closed {
		metadata["closed"] = "true"
	}
	return bindings.AMMPositionComponent{ID: stableID(group, kind, asset), PositionGroupID: group, Address: p.Address, Protocol: protocol, PoolContractID: p.PoolContractID, ComponentKind: kind, AssetID: asset, AmountRaw: amount, ShareAmountRaw: shares, TickLower: lower, TickUpper: upper, LedgerSeq: in.LedgerSeq, Timestamp: in.CloseTime, Metadata: metadata}
}
func appendRangeComponents(out *bindings.TransformOutput, group string, p bindings.AMMPositionState, pool bindings.AMMPoolState, in bindings.TransformInput) {
	if len(pool.Tokens) < 2 {
		return
	}
	lo, hi := p.TickLower, p.TickUpper
	if p.LiquidityRaw == "0" && p.HadShares {
		// Closed range position (liquidity written or removed to zero):
		// explicit zero tombstones, mirroring the classic-LP close path.
		// HadShares distinguishes a real close from a never-held range;
		// absent liquidity ("") stays silent — absent is not zero.
		for i := 0; i < 2; i++ {
			out.AMMComponents = append(out.AMMComponents, component(group, p, pool.Protocol, pool.Tokens[i].AssetID, "range_principal", "0", "0", &lo, &hi, in, true))
		}
		return
	}
	if p.Principal0Raw == "" && p.Principal1Raw == "" && p.LiquidityRaw != "" && pool.SqrtPriceX96 != "" && p.SqrtPriceLowerX96 != "" && p.SqrtPriceUpperX96 != "" {
		if x, y, err := rangePrincipal(p.LiquidityRaw, pool.SqrtPriceX96, p.SqrtPriceLowerX96, p.SqrtPriceUpperX96); err == nil {
			p.Principal0Raw, p.Principal1Raw = x, y
		}
	}
	for i, x := range []string{p.Principal0Raw, p.Principal1Raw} {
		if x != "" && x != "0" {
			out.AMMComponents = append(out.AMMComponents, component(group, p, pool.Protocol, pool.Tokens[i].AssetID, "range_principal", x, p.LiquidityRaw, &lo, &hi, in, false))
		}
	}
	for i, x := range []string{p.PendingFee0Raw, p.PendingFee1Raw} {
		if x != "" && x != "0" {
			out.AMMComponents = append(out.AMMComponents, component(group, p, pool.Protocol, pool.Tokens[i].AssetID, "unclaimed_fee", x, "", &lo, &hi, in, false))
		}
	}
}

// eventEra is one row of the per-wasm event-era table: whether the exact
// event name is a served activity, and the first ledger the name was observed
// on-chain for its contract class. The tables below mirror the deployment
// data in relay.rs deployments/aquarius.pubnet.toml character-exactly — the
// single source of truth shared by the relay wing, this package and the
// serving layer. The vocabulary only ever GREW across wasm upgrades, so one
// floor per (class, name) is a faithful era encoding. A name outside its
// class's table, or seen before its floor, quarantines loudly: the fix is a
// new table row (data), never a keyword match (code).
type eventEra struct {
	activity   bool
	fromLedger int64
}

// Event classes. Pools split by pool type because each wasm lineage carries
// its own vocabulary; share tokens (and the pools' underlying token
// contracts) speak SEP-41, whose events are recognized non-activities — LP
// position state rides Balance WRITES, not token events.
const (
	classRouter           = "router"
	classPoolCP           = "pool_cp"
	classPoolStable       = "pool_stable"
	classPoolConcentrated = "pool_concentrated"
	classShareToken       = "share_token"
)

var eventErasByClass = map[string]map[string]eventEra{
	classRouter: {
		"add_pool":                  {activity: true, fromLedger: 52728530},
		"deposit":                   {activity: true, fromLedger: 52728548},
		"swap":                      {activity: true, fromLedger: 52728694},
		"withdraw":                  {activity: true, fromLedger: 52728753},
		"claim":                     {activity: true, fromLedger: 53554212},
		"config_rewards":            {activity: true, fromLedger: 53788765},
		"commit_transfer_ownership": {activity: false, fromLedger: 55363698},
		"apply_transfer_ownership":  {activity: false, fromLedger: 55363729},
		"set_privileged_addrs":      {activity: false, fromLedger: 55363632},
		"commit_upgrade":            {activity: false, fromLedger: 56429464},
		"apply_upgrade":             {activity: false, fromLedger: 56505099},
		"set_protocol_fee":          {activity: false, fromLedger: 57711843},
		"pool_gauge_switch_token":   {activity: false, fromLedger: 58787667},
	},
	classPoolCP: {
		"deposit_liquidity":             {activity: true, fromLedger: 52728548},
		"trade":                         {activity: true, fromLedger: 52728694},
		"withdraw_liquidity":            {activity: true, fromLedger: 52728753},
		"claim_reward":                  {activity: true, fromLedger: 55364200},
		"update_reserves":               {activity: true, fromLedger: 57724992},
		"kill_claim":                    {activity: false, fromLedger: 53446148},
		"unkill_claim":                  {activity: false, fromLedger: 53553379},
		"set_privileged_addrs":          {activity: false, fromLedger: 55363632},
		"commit_transfer_ownership":     {activity: false, fromLedger: 55363698},
		"apply_transfer_ownership":      {activity: false, fromLedger: 55363729},
		"set_rewards_config":            {activity: false, fromLedger: 55363850},
		"commit_upgrade":                {activity: false, fromLedger: 56429489},
		"apply_upgrade":                 {activity: false, fromLedger: 56505152},
		"set_protocol_fee":              {activity: false, fromLedger: 57725741},
		"claim_protocol_fee":            {activity: false, fromLedger: 57812875},
		"rewards_gauge_add":             {activity: false, fromLedger: 58961340},
		"rewards_gauge_schedule_reward": {activity: false, fromLedger: 58961340},
		"rewards_gauge_claim":           {activity: false, fromLedger: 58961606},
		"reserves_sync":                 {activity: false, fromLedger: 62338454},
		"set_rewards_state":             {activity: false, fromLedger: 62460053},
	},
	classPoolStable: {
		"deposit_liquidity":         {activity: true, fromLedger: 53721288},
		"trade":                     {activity: true, fromLedger: 53752457},
		"withdraw_liquidity":        {activity: true, fromLedger: 54357611},
		"claim_reward":              {activity: true, fromLedger: 55364553},
		"update_reserves":           {activity: true, fromLedger: 57711655},
		"set_privileged_addrs":      {activity: false, fromLedger: 55363632},
		"commit_transfer_ownership": {activity: false, fromLedger: 55363698},
		"apply_transfer_ownership":  {activity: false, fromLedger: 55363729},
		"set_rewards_config":        {activity: false, fromLedger: 55363874},
		"commit_upgrade":            {activity: false, fromLedger: 56429464},
		"apply_upgrade":             {activity: false, fromLedger: 56505116},
		"set_protocol_fee":          {activity: false, fromLedger: 57711843},
		"claim_protocol_fee":        {activity: false, fromLedger: 58023965},
	},
	classPoolConcentrated: {
		"deposit_liquidity":      {activity: true, fromLedger: 62341770},
		"pool_state":             {activity: true, fromLedger: 62341770},
		"position_update":        {activity: true, fromLedger: 62341770},
		"update_reserves":        {activity: true, fromLedger: 62341770},
		"trade":                  {activity: true, fromLedger: 62341787},
		"withdraw_liquidity":     {activity: true, fromLedger: 62342165},
		"claim_fees":             {activity: true, fromLedger: 62343467},
		"claim_reward":           {activity: true, fromLedger: 62350500},
		"commit_upgrade":         {activity: false, fromLedger: 62429301},
		"claim_protocol_fee":     {activity: false, fromLedger: 62440492},
		"apply_upgrade":          {activity: false, fromLedger: 62475394},
		"enable_emergency_mode":  {activity: false, fromLedger: 62877434},
		"disable_emergency_mode": {activity: false, fromLedger: 62877804},
		"set_rewards_state":      {activity: false, fromLedger: 63082253},
	},
	classShareToken: {
		"mint":     {activity: false, fromLedger: 53553264},
		"burn":     {activity: false, fromLedger: 53555897},
		"transfer": {activity: false, fromLedger: 53738883},
		"approve":  {activity: false, fromLedger: 60433173},
	},
}

// poolTypeEventClass maps a decoded pool type to its event class. Only exact,
// affirmatively decoded pool types classify; anything else quarantines.
var poolTypeEventClass = map[string]string{
	"constant_product": classPoolCP,
	"stable":           classPoolStable,
	"concentrated":     classPoolConcentrated,
}

// eventClass resolves the era table an event's contract speaks. The pools'
// underlying token contracts (registered as asset contracts) speak the same
// SEP-41 vocabulary as share tokens.
func (a *Adapter) eventClass(contractID string, pools map[string]bindings.AMMPoolState) string {
	if _, ok := a.cfg.Routers[contractID]; ok {
		return classRouter
	}
	if _, ok := a.shareTokens[contractID]; ok {
		return classShareToken
	}
	if _, ok := a.assets[contractID]; ok {
		return classShareToken
	}
	if p, ok := pools[contractID]; ok {
		if class, ok := poolTypeEventClass[p.PoolType]; ok {
			return class
		}
	}
	return ""
}

func (a *Adapter) quarantineEvent(evt bindings.RawEventEnvelope, reason string, metadata map[string]string) []bindings.QuarantineEvent {
	return []bindings.QuarantineEvent{{ID: stableID(a.ID(), evt.LedgerSeq, evt.TxHash, evt.EventIndex), AdapterID: a.ID(), LedgerSeq: evt.LedgerSeq, TxHash: evt.TxHash, EventIndex: evt.EventIndex, ContractID: evt.ContractID, Reason: reason, RawEvent: evt.RawEvent, Metadata: metadata}}
}

// decodeActivities classifies one contract event by exact name against its
// contract class's era table and emits the activity under that exact
// on-chain name — the frozen aquarius_activities vocabulary. No keyword or
// substring matching: an unknown name, an unclassifiable contract, or a name
// observed before its era floor quarantines with a reason.
func (a *Adapter) decodeActivities(evt bindings.RawEventEnvelope, class string) ([]bindings.Activity, []bindings.QuarantineEvent) {
	var ce xdr.ContractEvent
	if xdr.SafeUnmarshal(evt.RawEvent, &ce) != nil {
		return nil, nil
	}
	v, ok := ce.Body.GetV0()
	if !ok || len(v.Topics) == 0 {
		return nil, nil
	}
	name := symbolOrFirst(v.Topics[0])
	eras, classified := eventErasByClass[class]
	if !classified {
		return nil, a.quarantineEvent(evt, "aquarius_event_unclassified_contract", map[string]string{"aquarius_event": name})
	}
	era, known := eras[name]
	if !known {
		return nil, a.quarantineEvent(evt, "aquarius_event_unknown_name", map[string]string{"aquarius_event": name, "event_class": class})
	}
	if evt.LedgerSeq < era.fromLedger {
		return nil, a.quarantineEvent(evt, "aquarius_event_before_era_floor", map[string]string{"aquarius_event": name, "event_class": class})
	}
	if !era.activity {
		// Recognized non-activity (admin/lifecycle/SEP-41): excluded by the
		// table's decision, never silently.
		return nil, nil
	}
	wallet, asset, amount := attributeActivity(class, name, v)
	if wallet == "" {
		// A name with no acting wallet in its observed shape (pool-lifecycle
		// and admin ops) is attributed to the emitting contract itself —
		// never an empty address, never a same-transaction bystander.
		wallet = evt.ContractID
	}
	return []bindings.Activity{{ID: stableID(a.cfg.Protocol, evt.LedgerSeq, evt.TxHash, evt.EventIndex, name), LedgerSeq: evt.LedgerSeq, TxHash: evt.TxHash, EventIndex: evt.EventIndex, ContractID: evt.ContractID, Address: wallet, Protocol: a.cfg.Protocol, ActivityType: contracts.ActivityType(name), AssetID: asset, AmountRaw: amount, Timestamp: evt.CloseTime, Metadata: map[string]string{"aquarius_event": name, "event_class": class}}}, nil
}

// attributeActivity resolves one vocabulary-approved event's (wallet, asset,
// amount) from its observed on-chain shape. Wallet and asset slots are fixed
// topic/data positions per name — recorded as written, even when the slot
// holds a contract (a router-initiated trade attributes to the router).
// Amounts read a fixed index of the data VECTOR; a shape that does not match
// its observation yields honest empties, never a scavenged nearby scalar.
func attributeActivity(class, name string, v xdr.ContractEventV0) (wallet, asset, amount string) {
	topicAddr := func(i int) string {
		if i < len(v.Topics) {
			return addr(v.Topics[i])
		}
		return ""
	}
	var data []xdr.ScVal
	if vec, ok := v.Data.GetVec(); ok && vec != nil {
		data = *vec
	}
	dataAddr := func(i int) string {
		if i < len(data) {
			return addr(data[i])
		}
		return ""
	}
	dataAmount := func(i int) string {
		if i < len(data) {
			return eventAmount(data[i])
		}
		return ""
	}
	switch {
	// Pool trade: topics [name, token_in, token_out, initiator];
	// data [amount_in, amount_out, fee].
	case name == "trade":
		return topicAddr(3), topicAddr(1), dataAmount(0)
	// Reward claim: topics [name, reward_token, owner]; data [amount].
	case name == "claim_reward":
		return topicAddr(2), topicAddr(1), dataAmount(0)
	// Concentrated range mutation and fee collection carry the owner in
	// topic 1; their data legs (tick bounds / two fee amounts) have no
	// single token amount.
	case name == "position_update" || name == "claim_fees":
		return topicAddr(1), "", ""
	// Router user ops: topics [name, tokens-vec, user] — multi-leg amounts
	// stay unattributed.
	case class == classRouter && (name == "deposit" || name == "withdraw"):
		return topicAddr(2), "", ""
	// Router swap: data [pool, token_in, token_out, in_amount, out_amount].
	case class == classRouter && name == "swap":
		return topicAddr(2), dataAddr(1), dataAmount(3)
	// Router claim: data [pool, reward_token, amount].
	case class == classRouter && name == "claim":
		return topicAddr(2), dataAddr(1), dataAmount(2)
	}
	// Pool LP legs (deposit_liquidity/withdraw_liquidity), update_reserves,
	// pool_state, add_pool, config_rewards, and any future vocabulary name
	// without an attribution shape: no wallet, no asset, no amount.
	return "", "", ""
}

// eventAmount renders one event-payload token amount. Only the two amount
// shapes observed on-chain qualify (i128, and u128 within i128 range) — any
// other value at an amount slot is a shape change and yields empty.
func eventAmount(v xdr.ScVal) string {
	if p, ok := v.GetI128(); ok {
		return new(bigInt).i128(p)
	}
	if p, ok := v.GetU128(); ok {
		if uint64(p.Hi)>>63 != 0 {
			return ""
		}
		return new(bigInt).u128(p)
	}
	return ""
}
