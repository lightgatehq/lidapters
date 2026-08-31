package blend

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lightgatehq/lidapters/bindings"
	"github.com/lightgatehq/lidapters/blend/contracts"
	"github.com/shopspring/decimal"
)

type normalizedPool struct {
	rateScalar            decimal.Decimal
	rateModifierScalar    decimal.Decimal
	scalarVersion         string
	version               string
	backstopTakeRaw       decimal.Decimal
	backstopTakeAvailable bool
}

type normalizedReserve struct {
	poolContract             string
	assetID                  string
	assetDecimals            int32
	bRateRaw                 decimal.Decimal
	dRateRaw                 decimal.Decimal
	cFactorRaw               decimal.Decimal
	lFactorRaw               decimal.Decimal
	utilTargetRaw            decimal.Decimal
	maxUtilRaw               decimal.Decimal
	rBaseRaw                 decimal.Decimal
	rOneRaw                  decimal.Decimal
	rTwoRaw                  decimal.Decimal
	rThreeRaw                decimal.Decimal
	rateModifierRaw          decimal.Decimal
	reactivityRaw            decimal.Decimal
	usdPrice                 decimal.Decimal
	priceAvailable           bool
	totalSuppliedRaw         decimal.Decimal
	totalBorrowedRaw         decimal.Decimal
	utilizationRaw           decimal.Decimal
	borrowAPRRaw             *decimal.Decimal
	supplyAPRRaw             *decimal.Decimal
	aprPartial               bool
	supplyCapRaw             string
	borrowCapRaw             decimal.Decimal
	remainingBorrowableRaw   decimal.Decimal
	utilizationSource        string
	rateModifierNormalized   decimal.Decimal
	cFactorNormalized        decimal.Decimal
	lFactorNormalized        decimal.Decimal
	utilTargetNormalized     decimal.Decimal
	maxUtilNormalized        decimal.Decimal
	rBaseNormalized          decimal.Decimal
	rOneNormalized           decimal.Decimal
	rTwoNormalized           decimal.Decimal
	rThreeNormalized         decimal.Decimal
	reactivityNormalized     decimal.Decimal
	borrowAPRNormalized      decimal.Decimal
	supplyAPRNormalized      decimal.Decimal
	borrowAPRNormalizedValid bool
	supplyAPRNormalizedValid bool
	raw                      contracts.ReserveState
}

type poolSummaryAccumulator struct {
	poolContract           string
	depositedUSD           decimal.Decimal
	borrowedUSD            decimal.Decimal
	effectiveCollateralUSD decimal.Decimal
	effectiveLiabilityUSD  decimal.Decimal
	netAPYWeightUSD        decimal.Decimal
	netAPYNumeratorUSD     decimal.Decimal
	lFactorZeroLiability   bool
	hasLiability           bool
	hasEffectiveCollateral bool
	pricePartial           bool
	// dataPartial / reservePricePartial are the two cold-start staleness axes. A
	// held reserve leg is dropped from valuation when its ResData has not folded yet
	// (dataPartial) or when its oracle price is unavailable after reload — map
	// present but price missing, e.g. an evicted temporary price entry or a price not
	// yet re-set (reservePricePartial). Either makes the account's health factor
	// incomplete, so its summary is suppressed rather than emitted over good gold; it
	// self-heals once the missing data/price re-folds from bronze. reservePricePartial
	// is scoped to reserve legs only — the backstop pricePartial (LP-token pricing,
	// always unavailable in the current decode) must NOT suppress an account.
	dataPartial               bool
	reservePricePartial       bool
	aprPartial                bool
	netAPYPartial             bool
	liquidationCollaterals    []liquidationCollateral
	liquidationPriceScenarios map[string]string
}

type liquidationCollateral struct {
	assetID                string
	units                  decimal.Decimal
	cFactor                decimal.Decimal
	effectiveCollateralUSD decimal.Decimal
}

type protocolAccumulator struct {
	depositedUSD       decimal.Decimal
	borrowedUSD        decimal.Decimal
	netAPYWeightUSD    decimal.Decimal
	netAPYNumeratorUSD decimal.Decimal
	netAPYPartial      bool
}

type poolBreakdownEntry struct {
	DepositedUSD              string            `json:"deposited_usd"`
	BorrowedUSD               string            `json:"borrowed_usd"`
	EffectiveCollateralUSD    string            `json:"effective_collateral_usd"`
	EffectiveLiabilityUSD     string            `json:"effective_liability_usd"`
	HealthFactor              string            `json:"health_factor,omitempty"`
	BorrowLimitPct            string            `json:"borrow_limit_pct,omitempty"`
	BorrowCapUSD              string            `json:"borrow_cap_usd,omitempty"`
	ShortfallUSD              string            `json:"shortfall_usd"`
	PricePartial              bool              `json:"price_partial"`
	APRPartial                bool              `json:"apr_partial"`
	LiquidationPriceScenarios map[string]string `json:"liquidation_price_scenarios,omitempty"`
}

func (a *Adapter) computeState(input bindings.TransformInput, output *bindings.TransformOutput) error {
	if input.State == nil {
		return nil
	}

	// assetMeta looks up each registered token contract's decoded human-readable
	// identity by contract ID, built once per ledger from the carried state. A
	// miss (unregistered or not-yet-decoded asset) leaves Reserve.Metadata's
	// asset_symbol/asset_name and Activity.AssetSymbol at their zero value —
	// absent, never guessed.
	assetMeta := map[string]contracts.AssetMetadata{}
	for _, meta := range input.State.Assets {
		assetMeta[meta.ContractID] = meta
	}

	// Oracle freshness/cadence by oracle contract, from the carried oracle
	// state: the mock oracle's top-level `timestamp` entry (last price update)
	// and its `res` resolution. Used to annotate each reserve so a consumer
	// can tell a stale price from a fresh one — absent stays absent.
	oracleFreshness := map[string]contracts.OracleState{}
	for _, oracle := range input.State.Oracles {
		oracleFreshness[oracle.ContractID] = oracle
	}

	pools := map[string]normalizedPool{}
	reserves := map[string]normalizedReserve{}
	for _, pool := range input.State.Pools {
		pool, wasmHashSource := a.enrichPoolIdentity(pool)
		nPool, ok := a.resolvePool(pool)
		if !ok {
			output.Quarantine = append(output.Quarantine, bindings.QuarantineEvent{
				ID:         stableID(a.cfg.AdapterID, "pool", pool.ContractID, "unknown_wasm_hash"),
				AdapterID:  a.cfg.AdapterID,
				LedgerSeq:  input.LedgerSeq,
				ContractID: pool.ContractID,
				Reason:     "unknown_wasm_hash",
				Metadata: map[string]string{
					"wasm_hash": pool.WasmHash,
				},
			})
			continue
		}
		pools[pool.ContractID] = nPool
		poolContractMeta := map[string]string{
			"scalar_version":   nPool.scalarVersion,
			"wasm_hash_source": wasmHashSource,
		}
		// Pool instance/config facets (audit section 3), added only when
		// present on-chain so pre-existing rows stay byte-identical.
		for key, value := range map[string]string{
			"pool_name":      pool.Name,
			"pool_admin":     pool.Admin,
			"blnd_token":     pool.BLNDToken,
			"max_positions":  pool.MaxPositionsRaw,
			"min_collateral": pool.MinCollateralRaw,
		} {
			if value != "" {
				poolContractMeta[key] = value
			}
		}
		if len(pool.PoolEmissions) > 0 {
			// The per-reserve-token BLND emission split, canonical sorted-key
			// JSON ({res_token_id: 7-dp share}).
			split := make(map[string]string, len(pool.PoolEmissions))
			for _, entry := range pool.PoolEmissions {
				split[strconv.FormatInt(int64(entry.ReserveTokenID), 10)] = entry.ShareRaw
			}
			if raw, err := json.Marshal(split); err == nil {
				poolContractMeta["pool_emissions"] = string(raw)
			}
		}
		output.Contracts = append(output.Contracts, bindings.Contract{
			ID:              stableID(a.cfg.Protocol, pool.ContractID),
			Address:         pool.ContractID,
			Protocol:        a.cfg.Protocol,
			ContractType:    "pool",
			Status:          pool.PoolStatus,
			WasmHash:        pool.WasmHash,
			FirstSeenLedger: input.LedgerSeq,
			LastSeenLedger:  input.LedgerSeq,
			Metadata:        poolContractMeta,
		})

		// Pool-level backstop total: the aggregate capital protecting this pool
		// (every depositor's shares/tokens summed), not a single user's deposit —
		// that is the per-user bindings.Position with PositionType=backstop built
		// below. Emitted whenever the pool declares a backstop ref, even before its
		// PoolBalance entry has been observed (raw fields empty -> NULL in gold).
		// q4w_pct is a fraction (shares queued / total shares), not a percentage.
		if pool.BackstopContract != "" {
			totalShares := parseDecimalOrZero(pool.BackstopSharesRaw)
			q4wShares := parseDecimalOrZero(pool.BackstopQ4WSharesRaw)
			q4wPct := ""
			if !totalShares.IsZero() {
				q4wPct = numString(q4wShares.Div(totalShares))
			}
			backstopTotal := bindings.Backstop{
				ID:               stableID(a.cfg.Protocol, pool.ContractID, "backstop_total"),
				Protocol:         a.cfg.Protocol,
				ContractID:       pool.ContractID,
				BackstopContract: pool.BackstopContract,
				SharesRaw:        pool.BackstopSharesRaw,
				LPTokensRaw:      pool.BackstopTokensRaw,
				Q4WSharesRaw:     pool.BackstopQ4WSharesRaw,
				Q4WPct:           q4wPct,
				LedgerSeq:        input.LedgerSeq,
				Timestamp:        input.CloseTime,
			}
			// Pool-level backstop emission accrual (BEmisData), surfaced only
			// when the entry exists on-chain — Metadata stays nil otherwise so
			// pre-emission output is byte-identical.
			if pool.BackstopEmisEPSRaw != "" || pool.BackstopEmisExpirationRaw != "" || pool.BackstopEmisIndexRaw != "" || pool.BackstopEmisLastTimeRaw != "" {
				backstopTotal.Metadata = map[string]string{
					"emission_eps":        pool.BackstopEmisEPSRaw,
					"emission_expiration": pool.BackstopEmisExpirationRaw,
					"emission_index":      pool.BackstopEmisIndexRaw,
					"emission_last_time":  pool.BackstopEmisLastTimeRaw,
				}
			}
			output.Backstops = append(output.Backstops, backstopTotal)
		}

		for _, reserve := range pool.Reserves {
			// A reserve whose ResData half has not been folded yet — config present
			// but no b/d rate or supply — is a cold-start artifact: config-only reload
			// seeds a pool's reserve config (from persisted config) before the bronze
			// re-fold restores its data. Emitting it here would value it at zero
			// (mustParseDecimal("") == 0) and overwrite the reserve's good gold. It is
			// also absent from the valuation map so a position that references it is
			// left stale-but-safe rather than valued against zeros. Once the reserve's
			// ResData re-folds from bronze it is emitted normally. A genuinely-zero but
			// folded reserve keeps its ResData strings ("0"), so it is not skipped.
			if !reserveHasFoldedData(reserve) {
				continue
			}
			nReserve, err := normalizeReserve(pool.ContractID, nPool, reserve)
			if err != nil {
				return err
			}
			key := reserveKey(pool.ContractID, reserve.AssetID)
			reserves[key] = nReserve

			borrowAPY := ""
			if nReserve.borrowAPRNormalizedValid {
				borrowAPY = numString(nReserve.borrowAPRNormalized)
			}
			supplyAPY := ""
			if nReserve.supplyAPRNormalizedValid {
				supplyAPY = numString(nReserve.supplyAPRNormalized)
			}

			reserveMeta := map[string]string{
				"scalar_version":           nPool.scalarVersion,
				"asset_symbol":             assetMeta[reserve.AssetID].Symbol,
				"asset_name":               assetMeta[reserve.AssetID].Name,
				"asset_decimals":           parseDecimalsInt(nReserve.assetDecimals),
				"oracle_price_usd":         numString(nReserve.usdPrice),
				"oracle_price":             numString(nReserve.usdPrice),
				"b_rate":                   numString(nReserve.bRateRaw.Div(nPool.rateScalar)),
				"d_rate":                   numString(nReserve.dRateRaw.Div(nPool.rateScalar)),
				"util_target":              numString(nReserve.utilTargetNormalized),
				"max_util":                 numString(nReserve.maxUtilNormalized),
				"r_base":                   numString(nReserve.rBaseNormalized),
				"r_one":                    numString(nReserve.rOneNormalized),
				"r_two":                    numString(nReserve.rTwoNormalized),
				"r_three":                  numString(nReserve.rThreeNormalized),
				"rate_modifier":            numString(nReserve.rateModifierNormalized),
				"reactivity":               numString(nReserve.reactivityNormalized),
				"enabled":                  boolString(reserve.Enabled),
				"apr_partial":              boolString(nReserve.aprPartial),
				"pool_balance_raw":         nReserve.raw.PoolBalanceRaw,
				"backstop_credit_raw":      reserve.BackstopCreditRaw,
				"accrual_last_time":        reserve.LastTimeRaw,
				"remaining_borrowable_raw": numString(nReserve.remainingBorrowableRaw),
				"rate_scalar":              numString(nPool.rateScalar),
				"rate_modifier_scalar":     numString(nPool.rateModifierScalar),
				"utilization_source":       nReserve.utilizationSource,
			}
			// Provenance is producer-owned (V1-05 D-02): this adapter emits a
			// reserve price only when the pool's decoded oracle supplied one
			// (normalizeReserve sets priceAvailable), so a priced reserve
			// truthfully claims pool_oracle. An unavailable price claims no
			// source — relay must persist that as null, not guess a label.
			// A constant-base branch of the pool oracle (e.g. a USDC-base
			// aggregator returning exactly 1.0) is still pool_oracle.
			if nReserve.priceAvailable {
				reserveMeta["price_source"] = "pool_oracle"
			}
			// Price freshness (audit section 4): the pool oracle's last price
			// update time and cadence, only when decoded — a price with no
			// timestamp stays visibly timestamp-less rather than guessed fresh.
			if oracle, ok := oracleFreshness[pool.OracleContract]; ok {
				if oracle.LastTimestampRaw != "" {
					reserveMeta["oracle_timestamp"] = oracle.LastTimestampRaw
				}
				if oracle.ResolutionRaw != "" {
					reserveMeta["oracle_resolution"] = oracle.ResolutionRaw
				}
			}
			output.Reserves = append(output.Reserves, bindings.Reserve{
				ID:             stableID(a.cfg.Protocol, pool.ContractID, reserve.AssetID),
				Protocol:       a.cfg.Protocol,
				ContractID:     pool.ContractID,
				AssetID:        reserve.AssetID,
				TotalSupplied:  numString(nReserve.totalSuppliedRaw),
				TotalBorrowed:  numString(nReserve.totalBorrowedRaw),
				Utilization:    numString(nReserve.utilizationRaw.Div(factorScaleDecimal)),
				SupplyAPY:      supplyAPY,
				BorrowAPY:      borrowAPY,
				SupplyCap:      reserve.SupplyCapRaw,
				BorrowCap:      numString(nReserve.borrowCapRaw),
				CFactor:        numString(nReserve.cFactorNormalized),
				LFactor:        numString(nReserve.lFactorNormalized),
				OracleContract: pool.OracleContract,
				LedgerSeq:      input.LedgerSeq,
				Timestamp:      input.CloseTime,
				Metadata:       reserveMeta,
			})

			// Per-side emission rows: only for a side with real on-chain emission
			// state — config (EPSRaw != "") or accrual (IndexRaw != "", the v2
			// merged ReserveEmissionData) — absent emissions stay absent, never a
			// fabricated eps=0 row. APY stays "" (no emitted-token price feed
			// exists yet to derive it); every raw field is decoded chain state,
			// so a row here is never fabricated.
			for _, side := range []struct {
				name     string
				eps      string
				exp      string
				index    string
				lastTime string
			}{
				{"supply", reserve.SupplyEmisEPSRaw, reserve.SupplyEmisExpirationRaw, reserve.SupplyEmisIndexRaw, reserve.SupplyEmisLastTimeRaw},
				{"borrow", reserve.BorrowEmisEPSRaw, reserve.BorrowEmisExpirationRaw, reserve.BorrowEmisIndexRaw, reserve.BorrowEmisLastTimeRaw},
			} {
				if side.eps == "" && side.index == "" {
					continue
				}
				var expiration time.Time
				if unix, ok := parseUnixSeconds(side.exp); ok {
					expiration = unix
				}
				output.ReserveEmissions = append(output.ReserveEmissions, bindings.ReserveEmission{
					ID:          stableID(a.cfg.Protocol, pool.ContractID, reserve.AssetID, side.name),
					Protocol:    a.cfg.Protocol,
					ContractID:  pool.ContractID,
					AssetID:     reserve.AssetID,
					Side:        side.name,
					EPSRaw:      side.eps,
					Expiration:  expiration,
					APY:         "",
					IndexRaw:    side.index,
					LastTimeRaw: side.lastTime,
					LedgerSeq:   input.LedgerSeq,
					Timestamp:   input.CloseTime,
					Metadata:    map[string]string{"apy_unavailable": "true"},
				})
			}
		}
	}

	// Structured auction state: each live Auction(AuctionKey) entry surfaced
	// verbatim (per-asset lot/bid maps, start block, typed label). The slice in
	// state is already deterministically sorted by the fold.
	for _, auction := range input.State.Auctions {
		output.Auctions = append(output.Auctions, a.auctionRow(auction, input.LedgerSeq, input.CloseTime))
	}

	// Pending, time-locked reserve-parameter changes (ResInit): the "params
	// about to change" signal, surfaced verbatim. NewConfig carries only the
	// fields present on-chain. The slice in state is deterministically sorted.
	for _, queued := range input.State.QueuedReserves {
		output.QueuedReserves = append(output.QueuedReserves, a.queuedReserveRow(queued, input.LedgerSeq, input.CloseTime))
	}

	// Pool-level backstop LP valuation (V1-09): with every pool's reserves
	// normalized, value each aggregate Backstop row against the folded Comet
	// state — component amounts by exact token ID, USD from the same
	// ledger-pinned reserve prices the per-user path uses. Absent-not-zero
	// rules are identical to the per-user path: unknown supply, a zero
	// denominator, a missing reserve, or a missing price leg leaves the fields
	// absent, never a fabricated zero.
	a.valuePoolBackstops(input, output, reserves)

	// The backstop contract's decoded identity: a Contract row (gold's
	// contract_type 'backstop') carrying the instance addresses — BToken is
	// the Comet LP anchoring share valuation — plus reward-zone membership and
	// drop list as canonical JSON. Only fields present on-chain are emitted.
	for _, instance := range input.State.BackstopInstances {
		meta := map[string]string{}
		for key, value := range map[string]string{
			"backstop_token": instance.BackstopToken,
			"blnd_token":     instance.BLNDToken,
			"usdc_token":     instance.USDCToken,
			"emitter":        instance.Emitter,
			"pool_factory":   instance.PoolFactory,
		} {
			if value != "" {
				meta[key] = value
			}
		}
		if len(instance.RewardZone) > 0 {
			if raw, err := json.Marshal(instance.RewardZone); err == nil {
				meta["reward_zone"] = string(raw)
			}
		}
		if len(instance.DropList) > 0 {
			drop := make(map[string]string, len(instance.DropList))
			for _, entry := range instance.DropList {
				drop[entry.Address] = entry.AmountRaw
			}
			if raw, err := json.Marshal(drop); err == nil {
				meta["drop_list"] = string(raw)
			}
		}
		output.Contracts = append(output.Contracts, bindings.Contract{
			ID:              stableID(a.cfg.Protocol, instance.ContractID),
			Address:         instance.ContractID,
			Protocol:        a.cfg.Protocol,
			ContractType:    "backstop",
			Status:          "active",
			FirstSeenLedger: input.LedgerSeq,
			LastSeenLedger:  input.LedgerSeq,
			Metadata:        meta,
		})
	}

	// Per-user reserve emission accrual: each UserEmis(UserReserveKey) entry
	// surfaced with its side and — when the pool's reserve list resolves the
	// index — its asset. AssetID stays "" (never guessed) when unresolvable;
	// the raw ReserveTokenID always rides along. The relay#26 consumer derives
	// claimable BLND from (index, accrued) against the reserve's own emission
	// index. The slice in state is already deterministically sorted.
	for _, emission := range input.State.UserEmissions {
		side := "borrow"
		if emission.ReserveTokenID%2 == 1 {
			side = "supply"
		}
		assetID := ""
		for _, pool := range input.State.Pools {
			if pool.ContractID != emission.PoolContractID {
				continue
			}
			// The same known-unique rule as the fold's reserveByIndex: an
			// unknown or duplicate index resolves to nothing — the raw
			// ReserveTokenID still rides along, the asset is never guessed.
			if reserve, ok := reserveByIndex(pool, emission.ReserveTokenID/2); ok {
				assetID = reserve.AssetID
			}
			break
		}
		output.UserEmissions = append(output.UserEmissions, bindings.UserEmission{
			ID:             stableID(a.cfg.Protocol, emission.Address, emission.PoolContractID, strconv.FormatInt(int64(emission.ReserveTokenID), 10), "user_emission"),
			Protocol:       a.cfg.Protocol,
			ContractID:     emission.PoolContractID,
			Address:        emission.Address,
			AssetID:        assetID,
			ReserveTokenID: emission.ReserveTokenID,
			Side:           side,
			IndexRaw:       emission.IndexRaw,
			AccruedRaw:     emission.AccruedRaw,
			LedgerSeq:      input.LedgerSeq,
			Timestamp:      input.CloseTime,
		})
	}

	// Activities are valued at the same folded oracle price the reserve itself
	// is valued with at this ledger — the reserves map above is the single price
	// source, resolved purely from in-state data (pool → oracle fold). An
	// activity whose asset has no folded reserve price (non-reserve asset such
	// as a reward token, or a cold-start reserve whose data has not re-folded)
	// keeps a NULL usd_value with the explicit unavailability marker — never a
	// fabricated zero.
	for i := range output.Activities {
		activity := &output.Activities[i]
		if meta, ok := assetMeta[activity.AssetID]; ok {
			activity.AssetSymbol = meta.Symbol
		}
		if activity.AssetID == "" || activity.AmountRaw == "" {
			continue
		}
		if activity.Metadata == nil {
			activity.Metadata = map[string]string{}
		}
		reserve, ok := reserves[reserveKey(activity.ContractID, activity.AssetID)]
		if !ok || !reserve.priceAvailable {
			activity.Metadata["event_price_unavailable"] = "true"
			continue
		}
		amountRaw := parseDecimalOrZero(activity.AmountRaw)
		units := amountRaw.Div(decimal.New(1, reserve.assetDecimals))
		activity.USDValue = numString(units.Mul(reserve.usdPrice))
		activity.Metadata["usd_value_source"] = "reserve_ledger_price"
	}

	poolSummaries := map[string]map[string]*poolSummaryAccumulator{}
	protocolSummaries := map[string]*protocolAccumulator{}

	ensurePoolSummary := func(address, poolContract string) *poolSummaryAccumulator {
		byPool, ok := poolSummaries[address]
		if !ok {
			byPool = map[string]*poolSummaryAccumulator{}
			poolSummaries[address] = byPool
		}
		s, ok := byPool[poolContract]
		if !ok {
			s = &poolSummaryAccumulator{poolContract: poolContract, liquidationPriceScenarios: map[string]string{}}
			byPool[poolContract] = s
		}
		return s
	}

	ensureProtocolSummary := func(address string) *protocolAccumulator {
		s, ok := protocolSummaries[address]
		if !ok {
			s = &protocolAccumulator{}
			protocolSummaries[address] = s
		}
		return s
	}

	for _, userPos := range input.State.Users {
		pool, ok := pools[userPos.PoolContractID]
		if !ok {
			continue
		}
		reserve, ok := reserves[reserveKey(userPos.PoolContractID, userPos.AssetID)]
		if !ok {
			// The reserve is not in the valuation map — its ResData has not folded yet
			// (config-only cold-start reload). Drop this leg AND mark the account's pool
			// summary data-incomplete so an incomplete health factor is not emitted over
			// the account's good gold. Self-heals when the reserve's data re-folds.
			ensurePoolSummary(userPos.Address, userPos.PoolContractID).dataPartial = true
			continue
		}

		share := parseDecimalOrZero(userPos.BTokensRaw)
		assetAmountRaw := fixedMulFloor(share, reserve.bRateRaw, pool.rateScalar)
		if userPos.PositionType == contracts.PositionTypeLiability {
			share = parseDecimalOrZero(userPos.DTokensRaw)
			assetAmountRaw = fixedMulCeil(share, reserve.dRateRaw, pool.rateScalar)
		}

		usdValue := decZero
		usdValueStr := ""
		if reserve.priceAvailable {
			divisor := decimal.New(1, reserve.assetDecimals)
			usdValue = assetAmountRaw.Div(divisor).Mul(reserve.usdPrice)
			usdValueStr = numString(usdValue)
		}

		positionMeta := map[string]string{
			"scalar_version":   pool.scalarVersion,
			"c_factor":         numString(reserve.cFactorNormalized),
			"l_factor":         numString(reserve.lFactorNormalized),
			"oracle_price_usd": numString(reserve.usdPrice),
			"oracle_price":     numString(reserve.usdPrice),
			"b_rate":           numString(reserve.bRateRaw.Div(pool.rateScalar)),
			"d_rate":           numString(reserve.dRateRaw.Div(pool.rateScalar)),
		}

		apy := ""
		aprPartial := false
		signedContribution := decZero
		if userPos.PositionType == contracts.PositionTypeSupply || userPos.PositionType == contracts.PositionTypeCollateral {
			if reserve.supplyAPRNormalizedValid {
				positionMeta["supply_apr"] = numString(reserve.supplyAPRNormalized)
			}
			if emissionsAPR, ok := normalizedAPRInput(reserve.raw.SupplyEmissionsAPR); ok && reserve.supplyAPRNormalizedValid {
				apy = numString(reserve.supplyAPRNormalized.Add(emissionsAPR))
				positionMeta["supply_emissions_apr"] = numString(emissionsAPR)
				positionMeta["net_supply_apr"] = apy
			} else {
				if emissionsAPR, ok := normalizedAPRInput(reserve.raw.SupplyEmissionsAPR); ok {
					// Base APR is invalid, so we can't compute a net APR, but the raw
					// emissions APR parsed and is still useful — surface it on its own
					// in metadata instead of dropping it.
					positionMeta["supply_emissions_apr"] = numString(emissionsAPR)
				} else {
					positionMeta["emissions_apr_unavailable"] = "true"
				}
				positionMeta["apr_partial"] = "true"
				aprPartial = true
			}
		} else if userPos.PositionType == contracts.PositionTypeLiability {
			if reserve.borrowAPRNormalizedValid {
				positionMeta["borrow_apr"] = numString(reserve.borrowAPRNormalized)
			}
			if emissionsAPR, ok := normalizedAPRInput(reserve.raw.BorrowEmissionsAPR); ok && reserve.borrowAPRNormalizedValid {
				apy = numString(reserve.borrowAPRNormalized.Sub(emissionsAPR))
				positionMeta["borrow_emissions_apr"] = numString(emissionsAPR)
				positionMeta["net_borrow_apr"] = apy
			} else {
				if emissionsAPR, ok := normalizedAPRInput(reserve.raw.BorrowEmissionsAPR); ok {
					// Base APR is invalid, so we can't compute a net APR, but the raw
					// emissions APR parsed and is still useful — surface it on its own
					// in metadata instead of dropping it.
					positionMeta["borrow_emissions_apr"] = numString(emissionsAPR)
				} else {
					positionMeta["emissions_apr_unavailable"] = "true"
				}
				positionMeta["apr_partial"] = "true"
				aprPartial = true
			}
		}
		if !reserve.priceAvailable {
			positionMeta["price_unavailable"] = "true"
		}
		if apy != "" && reserve.priceAvailable {
			r, _ := decimal.NewFromString(apy)
			signedContribution = usdValue.Mul(r)
			if userPos.PositionType == contracts.PositionTypeLiability {
				signedContribution = signedContribution.Neg()
			}
		}

		pos := bindings.Position{
			ID:           stableID(a.cfg.Protocol, userPos.Address, userPos.PoolContractID, userPos.AssetID, string(userPos.PositionType)),
			Address:      userPos.Address,
			Protocol:     a.cfg.Protocol,
			ContractID:   userPos.PoolContractID,
			AssetID:      userPos.AssetID,
			PositionType: userPos.PositionType,
			ShareAmount:  numString(share),
			AssetAmount:  numString(assetAmountRaw),
			USDValue:     usdValueStr,
			APY:          apy,
			LedgerSeq:    input.LedgerSeq,
			Timestamp:    input.CloseTime,
			Metadata:     positionMeta,
		}
		output.Positions = append(output.Positions, pos)

		poolSummary := ensurePoolSummary(userPos.Address, userPos.PoolContractID)
		protocolSummary := ensureProtocolSummary(userPos.Address)
		if !reserve.priceAvailable {
			// The reserve is in the oracle map but has no usable price (map-present /
			// price-missing after reload, or an evicted/rejected price). Drop the leg
			// AND mark the account data-incomplete on the price axis so its summary is
			// suppressed below — a health factor computed without this leg's USD value
			// must not overwrite the account's good gold. Self-heals on the next price.
			poolSummary.pricePartial = true
			poolSummary.reservePricePartial = true
			continue
		}

		switch userPos.PositionType {
		case contracts.PositionTypeSupply:
			poolSummary.depositedUSD = poolSummary.depositedUSD.Add(usdValue)
			protocolSummary.depositedUSD = protocolSummary.depositedUSD.Add(usdValue)
		case contracts.PositionTypeCollateral:
			poolSummary.depositedUSD = poolSummary.depositedUSD.Add(usdValue)
			protocolSummary.depositedUSD = protocolSummary.depositedUSD.Add(usdValue)
			effectiveCollateralUSD := usdValue.Mul(reserve.cFactorNormalized).Truncate(18)
			poolSummary.effectiveCollateralUSD = poolSummary.effectiveCollateralUSD.Add(effectiveCollateralUSD)
			poolSummary.hasEffectiveCollateral = true
			if !assetAmountRaw.IsZero() && !reserve.cFactorRaw.IsZero() {
				poolSummary.liquidationCollaterals = append(poolSummary.liquidationCollaterals, liquidationCollateral{
					assetID:                userPos.AssetID,
					units:                  assetAmountRaw.Div(decimal.New(1, reserve.assetDecimals)),
					cFactor:                reserve.cFactorNormalized,
					effectiveCollateralUSD: effectiveCollateralUSD,
				})
			}
		case contracts.PositionTypeLiability:
			poolSummary.borrowedUSD = poolSummary.borrowedUSD.Add(usdValue)
			protocolSummary.borrowedUSD = protocolSummary.borrowedUSD.Add(usdValue)
			poolSummary.hasLiability = true
			if reserve.lFactorRaw.IsZero() && !usdValue.IsZero() {
				poolSummary.lFactorZeroLiability = true
			} else if !reserve.lFactorRaw.IsZero() {
				poolSummary.effectiveLiabilityUSD = poolSummary.effectiveLiabilityUSD.Add(usdValue.Div(reserve.lFactorNormalized).RoundCeil(18))
			}
		}

		poolSummary.netAPYWeightUSD = poolSummary.netAPYWeightUSD.Add(usdValue.Abs())
		protocolSummary.netAPYWeightUSD = protocolSummary.netAPYWeightUSD.Add(usdValue.Abs())
		if aprPartial {
			poolSummary.aprPartial = true
			poolSummary.netAPYPartial = true
			protocolSummary.netAPYPartial = true
		} else {
			poolSummary.netAPYNumeratorUSD = poolSummary.netAPYNumeratorUSD.Add(signedContribution)
			protocolSummary.netAPYNumeratorUSD = protocolSummary.netAPYNumeratorUSD.Add(signedContribution)
		}
	}

	for _, backstop := range input.State.Backstops {
		_, ok := pools[backstop.PoolContractID]
		if !ok {
			continue
		}

		activeShares := parseDecimalOrZero(backstop.UserSharesRaw)
		queuedShares := decZero
		q4wEntries := make([]bindings.BackstopQueueEntry, 0, len(backstop.Q4W))
		var q4wUnlockAt *time.Time
		for _, q := range backstop.Q4W {
			share := parseDecimalOrZero(q.SharesRaw)
			queuedShares = queuedShares.Add(share)
			q4wEntries = append(q4wEntries, bindings.BackstopQueueEntry{
				Amount:   numString(share),
				UnlockAt: q.UnlockAt.UTC(),
			})
			if q4wUnlockAt == nil || q.UnlockAt.Before(*q4wUnlockAt) {
				u := q.UnlockAt.UTC()
				q4wUnlockAt = &u
			}
		}
		totalShares := activeShares.Add(queuedShares)

		poolShares := parseDecimalOrZero(backstop.PoolSharesRaw)
		poolTokens := parseDecimalOrZero(backstop.PoolTokensRaw)
		activeTokens := convertBackstopSharesToTokens(activeShares, poolShares, poolTokens)
		queuedTokens := convertBackstopSharesToTokens(queuedShares, poolShares, poolTokens)
		totalTokens := convertBackstopSharesToTokens(totalShares, poolShares, poolTokens)

		// Comet LP decomposition, absent-not-zero (D-09): every input must be a
		// folded observation, never a defaulted one. Missing supply or either
		// reserve leaves the components absent; a real stored-zero supply is a
		// zero DENOMINATOR — the quotient is undefined, so the components and
		// USD are absent with a diagnostic reason, not silently zero.
		lpSupplyKnown := strings.TrimSpace(backstop.LPTokenSupplyRaw) != ""
		blndReserveKnown := strings.TrimSpace(backstop.LPBLNDReserveRaw) != ""
		usdcReserveKnown := strings.TrimSpace(backstop.LPUSDCReserveRaw) != ""
		lpSupply := parseDecimalOrZero(backstop.LPTokenSupplyRaw)
		blndReserve := parseDecimalOrZero(backstop.LPBLNDReserveRaw)
		usdcReserve := parseDecimalOrZero(backstop.LPUSDCReserveRaw)
		lpDenominatorZero := lpSupplyKnown && lpSupply.IsZero()
		lpComponentsKnown := lpSupplyKnown && !lpSupply.IsZero() && blndReserveKnown && usdcReserveKnown
		blndComponentRaw := decZero
		usdcComponentRaw := decZero
		if lpComponentsKnown {
			blndComponentRaw = fixedMulFloor(totalTokens, blndReserve, lpSupply)
			usdcComponentRaw = fixedMulFloor(totalTokens, usdcReserve, lpSupply)
		}

		blndPriceKnown := strings.TrimSpace(backstop.BLNDPriceUSD) != ""
		usdcPriceKnown := strings.TrimSpace(backstop.USDCPriceUSD) != ""
		backstopUSD := decZero
		backstopUSDStr := ""
		if lpComponentsKnown && blndPriceKnown && usdcPriceKnown {
			blndPrice := parseDecimalOrZero(backstop.BLNDPriceUSD)
			usdcPrice := parseDecimalOrZero(backstop.USDCPriceUSD)
			blndUnits := blndComponentRaw.Div(decimal.New(1, backstop.BLNDDecimals))
			usdcUnits := usdcComponentRaw.Div(decimal.New(1, backstop.USDCDecimals))
			backstopUSD = blndUnits.Mul(blndPrice).Add(usdcUnits.Mul(usdcPrice))
			backstopUSDStr = numString(backstopUSD)
		}

		interestKnown := strings.TrimSpace(backstop.BackstopInterestAPY) != ""
		emissionsKnown := strings.TrimSpace(backstop.BackstopEmissionsAPY) != ""
		apy := ""
		metadata := map[string]string{
			"active_shares":       numString(activeShares),
			"queued_shares":       numString(queuedShares),
			"total_shares":        numString(totalShares),
			"active_lp_tokens":    numString(activeTokens),
			"queued_lp_tokens":    numString(queuedTokens),
			"total_lp_tokens":     numString(totalTokens),
			"unclaimed_emissions": backstop.UnclaimedEmissionsRaw,
		}
		// Component amounts only exist when every LP input was observed and the
		// denominator is nonzero; the reason keys make the absence explicit
		// instead of letting a missing input masquerade as a zero component.
		if lpComponentsKnown {
			metadata["blnd_component"] = numString(blndComponentRaw)
			metadata["usdc_component"] = numString(usdcComponentRaw)
			metadata["blnd_component_raw"] = numString(blndComponentRaw)
			metadata["usdc_component_raw"] = numString(usdcComponentRaw)
		} else if lpDenominatorZero {
			metadata["lp_denominator_zero"] = "true"
		} else {
			metadata["lp_state_unavailable"] = "true"
		}
		if !interestKnown || !emissionsKnown {
			metadata["apr_partial"] = "true"
			if emissionsKnown {
				// Net APR stays partial (interest missing), but the raw emissions
				// APY parsed, so surface it on its own in metadata.
				metadata["backstop_emissions_apr"] = numString(parseDecimalOrZero(backstop.BackstopEmissionsAPY))
			} else {
				metadata["emissions_apr_unavailable"] = "true"
			}
		} else {
			interestAPY := parseDecimalOrZero(backstop.BackstopInterestAPY)
			emissionsAPY := parseDecimalOrZero(backstop.BackstopEmissionsAPY)
			apy = numString(interestAPY.Add(emissionsAPY))
			metadata["backstop_interest_apr"] = numString(interestAPY)
			metadata["backstop_emissions_apr"] = numString(emissionsAPY)
			metadata["net_backstop_apr"] = apy
		}
		if backstopUSDStr == "" {
			metadata["price_unavailable"] = "true"
		}

		pos := bindings.Position{
			ID:           stableID(a.cfg.Protocol, backstop.Address, backstop.PoolContractID, "backstop"),
			Address:      backstop.Address,
			Protocol:     a.cfg.Protocol,
			ContractID:   backstop.PoolContractID,
			AssetID:      "blend_backstop_lp",
			PositionType: contracts.PositionTypeBackstop,
			ShareAmount:  numString(totalShares),
			AssetAmount:  numString(totalTokens),
			USDValue:     backstopUSDStr,
			APY:          apy,
			LedgerSeq:    input.LedgerSeq,
			Timestamp:    input.CloseTime,
			Metadata:     metadata,
			Q4WEntries:   q4wEntries,
		}
		if q4wUnlockAt != nil {
			pos.Metadata["q4w_unlock_at"] = q4wUnlockAt.Format(time.RFC3339)
		}
		if backstop.EmisIndexRaw != "" {
			// The UEmisData checkpoint index behind unclaimed_emissions; only
			// present when the accrual entry exists on-chain.
			pos.Metadata["emission_index"] = backstop.EmisIndexRaw
		}
		output.Positions = append(output.Positions, pos)

		poolSummary := ensurePoolSummary(backstop.Address, backstop.PoolContractID)
		protocolSummary := ensureProtocolSummary(backstop.Address)
		if backstopUSDStr == "" {
			poolSummary.pricePartial = true
			continue
		}
		poolSummary.depositedUSD = poolSummary.depositedUSD.Add(backstopUSD)
		protocolSummary.depositedUSD = protocolSummary.depositedUSD.Add(backstopUSD)
		poolSummary.netAPYWeightUSD = poolSummary.netAPYWeightUSD.Add(backstopUSD.Abs())
		protocolSummary.netAPYWeightUSD = protocolSummary.netAPYWeightUSD.Add(backstopUSD.Abs())
		if apy == "" {
			poolSummary.aprPartial = true
			poolSummary.netAPYPartial = true
			protocolSummary.netAPYPartial = true
		} else {
			r, _ := decimal.NewFromString(apy)
			contribution := backstopUSD.Mul(r)
			poolSummary.netAPYNumeratorUSD = poolSummary.netAPYNumeratorUSD.Add(contribution)
			protocolSummary.netAPYNumeratorUSD = protocolSummary.netAPYNumeratorUSD.Add(contribution)
		}
	}

	addresses := make([]string, 0, len(poolSummaries))
	for address := range poolSummaries {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)

	for _, address := range addresses {
		poolsForAddress := poolSummaries[address]

		// Stale-but-safe: if any of the account's pools is incomplete on either
		// cold-start axis — a held reserve's ResData has not folded, or its oracle
		// price is unavailable after a config reload — suppress the whole summary so an
		// incomplete/null health factor never overwrites the account's good gold. It
		// re-emits once the missing data/price re-folds. Backstop LP-price partiality
		// (reservePricePartial is reserve-scoped) deliberately does not suppress.
		incomplete := false
		for _, pool := range poolsForAddress {
			if pool.dataPartial || pool.reservePricePartial {
				incomplete = true
				break
			}
		}
		if incomplete {
			continue
		}

		protocol := ensureProtocolSummary(address)

		healthFactor := ""
		borrowLimitPct := ""
		borrowCapUSD := ""
		effectiveCollateralUSD := decZero
		effectiveLiabilityUSD := decZero
		hasCollateralInAnyPool := false
		hasLiabilityInAnyPool := false

		var minHealth *decimal.Decimal
		var maxBorrowLimit *decimal.Decimal
		borrowCapTotal := decZero
		hasBorrowCap := false
		var worstPool *poolSummaryAccumulator

		poolBreakdown := map[string]poolBreakdownEntry{}
		poolContracts := make([]string, 0, len(poolsForAddress))
		for poolContract := range poolsForAddress {
			poolContracts = append(poolContracts, poolContract)
		}
		sort.Strings(poolContracts)

		for _, poolContract := range poolContracts {
			pool := poolsForAddress[poolContract]
			if pool.hasEffectiveCollateral {
				hasCollateralInAnyPool = true
			}
			if pool.hasLiability {
				hasLiabilityInAnyPool = true
			}

			poolHealth := ""
			poolBorrowLimit := ""
			poolBorrowCap := ""
			poolEffectiveLiability := pool.effectiveLiabilityUSD
			shortfallUSD := decZero
			if pool.lFactorZeroLiability {
				poolEffectiveLiability = pool.borrowedUSD
				if poolEffectiveLiability.IsZero() {
					poolEffectiveLiability = decimal.New(1, 18)
				}
				poolHealth = "0"
				poolBorrowLimit = "1"
				poolBorrowCap = "0"
				shortfallUSD = maxDecimal(poolEffectiveLiability.Sub(pool.effectiveCollateralUSD), decZero)
			} else {
				if !pool.effectiveLiabilityUSD.IsZero() {
					h := pool.effectiveCollateralUSD.Div(pool.effectiveLiabilityUSD).Truncate(18)
					poolHealth = numString(h)
				}
				if pool.effectiveCollateralUSD.IsZero() {
					poolBorrowLimit = ""
					poolBorrowCap = ""
				} else {
					limit := pool.effectiveLiabilityUSD.Div(pool.effectiveCollateralUSD).RoundCeil(18)
					cap := maxDecimal(pool.effectiveCollateralUSD.Sub(pool.effectiveLiabilityUSD), decZero)
					poolBorrowLimit = numString(limit)
					poolBorrowCap = numString(cap)
					shortfallUSD = maxDecimal(pool.effectiveLiabilityUSD.Sub(pool.effectiveCollateralUSD), decZero)
				}
			}
			pool.liquidationPriceScenarios = liquidationScenarios(pool, poolEffectiveLiability)

			if poolHealth != "" {
				h, _ := decimal.NewFromString(poolHealth)
				if minHealth == nil || h.LessThan(*minHealth) {
					minHealth = &h
					worstPool = pool
				}
			}
			if poolBorrowLimit != "" {
				l, _ := decimal.NewFromString(poolBorrowLimit)
				if maxBorrowLimit == nil || l.GreaterThan(*maxBorrowLimit) {
					maxBorrowLimit = &l
				}
			}
			if poolBorrowCap != "" {
				hasBorrowCap = true
				c, _ := decimal.NewFromString(poolBorrowCap)
				borrowCapTotal = borrowCapTotal.Add(c)
			}

			poolBreakdown[poolContract] = poolBreakdownEntry{
				DepositedUSD:              numString(pool.depositedUSD),
				BorrowedUSD:               numString(pool.borrowedUSD),
				EffectiveCollateralUSD:    numString(pool.effectiveCollateralUSD),
				EffectiveLiabilityUSD:     numString(poolEffectiveLiability),
				HealthFactor:              poolHealth,
				BorrowLimitPct:            poolBorrowLimit,
				BorrowCapUSD:              poolBorrowCap,
				ShortfallUSD:              numString(shortfallUSD),
				PricePartial:              pool.pricePartial,
				APRPartial:                pool.aprPartial,
				LiquidationPriceScenarios: pool.liquidationPriceScenarios,
			}
		}

		if minHealth != nil {
			healthFactor = numString(*minHealth)
		}
		if maxBorrowLimit != nil {
			borrowLimitPct = numString(*maxBorrowLimit)
		}
		if hasBorrowCap {
			borrowCapUSD = numString(borrowCapTotal)
		}
		if worstPool != nil {
			effectiveCollateralUSD = worstPool.effectiveCollateralUSD
			if worstPool.lFactorZeroLiability {
				effectiveLiabilityUSD = maxDecimal(worstPool.borrowedUSD, decimal.New(1, 18))
			} else {
				effectiveLiabilityUSD = worstPool.effectiveLiabilityUSD
			}
		}

		liquidationPrice := singleCollateralLiquidationPrice(worstPool, minHealth, reserves)

		netAPY := decZero
		structuredMeta := map[string]any{
			"risk_semantics":                  "blend_pool_isolated",
			"summary_health_factor_semantics": "worst_pool",
			"liquidation_price_unavailable":   "pool_or_scenario_required",
			"pool_breakdown":                  poolBreakdown,
		}
		meta := map[string]string{
			"risk_semantics":                  "blend_pool_isolated",
			"summary_health_factor_semantics": "worst_pool",
			"liquidation_price_unavailable":   "pool_or_scenario_required",
			"pool_breakdown":                  marshalPoolBreakdown(poolBreakdown),
		}
		if !hasLiabilityInAnyPool {
			healthFactor = ""
		}
		if !hasCollateralInAnyPool {
			borrowLimitPct = ""
			borrowCapUSD = ""
		}

		if protocol.netAPYPartial {
			meta["net_apy_partial"] = "true"
			meta["net_apy_unavailable_reason"] = "missing_row_apr"
			structuredMeta["net_apy_partial"] = true
			structuredMeta["net_apy_unavailable_reason"] = "missing_row_apr"
		} else if !protocol.netAPYWeightUSD.IsZero() {
			netAPY = protocol.netAPYNumeratorUSD.Div(protocol.netAPYWeightUSD)
		}

		output.Summaries = append(output.Summaries, bindings.PositionSummary{
			ID:                     stableID(a.cfg.Protocol, address, "summary"),
			Address:                address,
			Protocol:               a.cfg.Protocol,
			HealthFactor:           healthFactor,
			BorrowLimitPct:         borrowLimitPct,
			BorrowCapUSD:           borrowCapUSD,
			DepositedUSD:           numString(protocol.depositedUSD),
			BorrowedUSD:            numString(protocol.borrowedUSD),
			EffectiveCollateralUSD: numString(effectiveCollateralUSD),
			EffectiveLiabilityUSD:  numString(effectiveLiabilityUSD),
			NetAPY:                 numString(netAPY),
			NetAPYWeightUSD:        numString(protocol.netAPYWeightUSD),
			LiquidationPrice:       liquidationPrice,
			LedgerSeq:              input.LedgerSeq,
			Timestamp:              input.CloseTime,
			Metadata:               meta,
			StructuredMetadata:     structuredMeta,
		})
	}

	return nil
}

func (a *Adapter) enrichPoolIdentity(pool contracts.PoolState) (contracts.PoolState, string) {
	if strings.TrimSpace(pool.WasmHash) != "" {
		return pool, "contract_instance"
	}
	if !poolHasBlendState(pool) || len(a.cfg.V2WasmHashes) != 1 {
		return pool, ""
	}
	for wasmHash := range a.cfg.V2WasmHashes {
		pool.WasmHash = wasmHash
		return pool, "configured_single_v2_hash"
	}
	return pool, ""
}

func poolHasBlendState(pool contracts.PoolState) bool {
	if pool.ContractID == "" || len(pool.Reserves) == 0 {
		return false
	}
	for _, reserve := range pool.Reserves {
		if reserve.AssetID != "" && reserve.BRateRaw != "" && reserve.DRateRaw != "" {
			return true
		}
	}
	return false
}

func (a *Adapter) resolvePool(pool contracts.PoolState) (normalizedPool, bool) {
	if pool.WasmHash == "" {
		if a.cfg.AllowUnknownV2 {
			return newV2Pool(a.cfg.V2Scalar, pool.BackstopTakeRate), true
		}
		return normalizedPool{}, false
	}
	if _, ok := a.cfg.V2WasmHashes[pool.WasmHash]; ok {
		return newV2Pool(a.cfg.V2Scalar, pool.BackstopTakeRate), true
	}
	if a.cfg.AllowUnknownV2 {
		return newV2Pool(a.cfg.V2Scalar, pool.BackstopTakeRate), true
	}
	return normalizedPool{}, false
}

func newV2Pool(v2Scalar, backstopTakeRate string) normalizedPool {
	backstopTakeRaw, available := parseFactorRaw(backstopTakeRate)
	backstopTakeRaw = minDecimal(maxDecimal(backstopTakeRaw, decZero), factorScaleDecimal)
	return normalizedPool{
		rateScalar:            parseDecimalOrZero(v2Scalar),
		rateModifierScalar:    parseDecimalOrZero(rateModifierScaleV2),
		scalarVersion:         "v2",
		version:               "v2",
		backstopTakeRaw:       backstopTakeRaw,
		backstopTakeAvailable: available,
	}
}

// reserveHasFoldedData reports whether a reserve's ResData half has been folded
// from bronze. ResData sets the b/d rate accumulators and the b/d supplies
// together, so any one being non-empty means data is present. A reserve rebuilt
// from persisted config alone (cold-start reload) has all four empty until its
// bronze re-fold; it must not be valued or emitted until then.
func reserveHasFoldedData(r contracts.ReserveState) bool {
	return r.BRateRaw != "" || r.DRateRaw != "" || r.BSupplyRaw != "" || r.DSupplyRaw != ""
}

func normalizeReserve(poolContract string, pool normalizedPool, reserve contracts.ReserveState) (normalizedReserve, error) {
	var out normalizedReserve
	out.poolContract = poolContract
	out.assetID = reserve.AssetID
	out.assetDecimals = reserve.AssetDecimals
	out.raw = reserve
	out.utilizationSource = "contract_parity"

	if pool.rateScalar.IsZero() {
		return out, fmt.Errorf("scalar cannot be zero for pool %s", poolContract)
	}

	var err error
	if out.bRateRaw, err = mustParseDecimal(reserve.BRateRaw); err != nil {
		return out, err
	}
	if out.dRateRaw, err = mustParseDecimal(reserve.DRateRaw); err != nil {
		return out, err
	}
	if out.cFactorRaw, err = mustParseDecimal(reserve.CFactorRaw); err != nil {
		return out, err
	}
	if out.lFactorRaw, err = mustParseDecimal(reserve.LFactorRaw); err != nil {
		return out, err
	}
	if out.utilTargetRaw, err = mustParseDecimal(reserve.UtilTargetRaw); err != nil {
		return out, err
	}
	if out.maxUtilRaw, err = mustParseDecimal(reserve.MaxUtilRaw); err != nil {
		return out, err
	}
	if out.rBaseRaw, err = mustParseDecimal(reserve.RBaseRaw); err != nil {
		return out, err
	}
	if out.rOneRaw, err = mustParseDecimal(reserve.ROneRaw); err != nil {
		return out, err
	}
	if out.rTwoRaw, err = mustParseDecimal(reserve.RTwoRaw); err != nil {
		return out, err
	}
	if out.rThreeRaw, err = mustParseDecimal(reserve.RThreeRaw); err != nil {
		return out, err
	}
	if out.rateModifierRaw, err = mustParseDecimal(reserve.RateModifierRaw); err != nil {
		return out, err
	}
	if out.reactivityRaw, err = mustParseDecimal(reserve.ReactivityRaw); err != nil {
		return out, err
	}

	out.cFactorNormalized = out.cFactorRaw.Div(factorScaleDecimal)
	out.lFactorNormalized = out.lFactorRaw.Div(factorScaleDecimal)
	out.utilTargetNormalized = out.utilTargetRaw.Div(factorScaleDecimal)
	out.maxUtilNormalized = out.maxUtilRaw.Div(factorScaleDecimal)
	out.rBaseNormalized = out.rBaseRaw.Div(factorScaleDecimal)
	out.rOneNormalized = out.rOneRaw.Div(factorScaleDecimal)
	out.rTwoNormalized = out.rTwoRaw.Div(factorScaleDecimal)
	out.rThreeNormalized = out.rThreeRaw.Div(factorScaleDecimal)
	out.reactivityNormalized = out.reactivityRaw.Div(factorScaleDecimal)
	out.rateModifierNormalized, err = normalizedRateModifier(reserve.RateModifierRaw, pool.rateModifierScalar)
	if err != nil {
		return out, err
	}

	priceRaw := parseDecimalOrZero(reserve.OraclePriceRaw)
	if !priceRaw.IsZero() {
		divisor := decimal.New(1, reserve.OracleDecimals)
		out.usdPrice = priceRaw.Div(divisor)
		out.priceAvailable = true
	}

	bSupplyRaw := parseDecimalOrZero(reserve.BSupplyRaw)
	dSupplyRaw := parseDecimalOrZero(reserve.DSupplyRaw)
	out.totalSuppliedRaw = fixedMulFloor(bSupplyRaw, out.bRateRaw, pool.rateScalar)
	out.totalBorrowedRaw = fixedMulCeil(dSupplyRaw, out.dRateRaw, pool.rateScalar)

	switch {
	case out.totalBorrowedRaw.IsZero():
		out.utilizationRaw = decZero
	case out.totalSuppliedRaw.IsZero():
		out.utilizationRaw = factorScaleDecimal
	default:
		out.utilizationRaw = fixedDivCeil(out.totalBorrowedRaw, out.totalSuppliedRaw, factorScaleDecimal)
	}
	if pool.version == "v2" && out.utilizationRaw.GreaterThan(factorScaleDecimal) {
		out.utilizationRaw = factorScaleDecimal
	}

	borrowAPRRaw, borrowAPRValid, aprPartial := computeBorrowAPRRaw(out.utilizationRaw, out.utilTargetRaw, out.rateModifierRaw, pool.rateModifierScalar, out.rBaseRaw, out.rOneRaw, out.rTwoRaw, out.rThreeRaw, dSupplyRaw)
	out.aprPartial = aprPartial
	if borrowAPRValid {
		v := borrowAPRRaw
		out.borrowAPRRaw = &v
		out.borrowAPRNormalized = borrowAPRRaw.Div(factorScaleDecimal)
		out.borrowAPRNormalizedValid = true
	}

	if !pool.backstopTakeAvailable || !borrowAPRValid {
		out.supplyAPRRaw = nil
		out.supplyAPRNormalizedValid = false
		out.aprPartial = true
	} else {
		tmp := fixedMulFloor(borrowAPRRaw, out.utilizationRaw, factorScaleDecimal)
		supplyAPRRaw := fixedMulFloor(tmp, factorScaleDecimal.Sub(pool.backstopTakeRaw), factorScaleDecimal)
		v := supplyAPRRaw
		out.supplyAPRRaw = &v
		out.supplyAPRNormalized = supplyAPRRaw.Div(factorScaleDecimal)
		out.supplyAPRNormalizedValid = true
	}

	out.supplyCapRaw = reserve.SupplyCapRaw
	out.borrowCapRaw = fixedMulFloor(out.totalSuppliedRaw, out.maxUtilRaw, factorScaleDecimal)
	out.remainingBorrowableRaw = maxDecimal(out.borrowCapRaw.Sub(out.totalBorrowedRaw), decZero)

	return out, nil
}

func computeBorrowAPRRaw(utilizationRaw, utilTargetRaw, rateModifierRaw, rateModifierScalar, rBaseRaw, rOneRaw, rTwoRaw, rThreeRaw, dSupplyRaw decimal.Decimal) (decimal.Decimal, bool, bool) {
	if utilizationRaw.IsZero() || dSupplyRaw.IsZero() {
		return decZero, true, false
	}
	if utilTargetRaw.IsZero() {
		return decZero, false, true
	}

	u95 := decimal.RequireFromString("9500000")
	if utilizationRaw.LessThanOrEqual(utilTargetRaw) {
		utilScalar := fixedDivCeil(utilizationRaw, utilTargetRaw, factorScaleDecimal)
		baseRate := rBaseRaw.Add(fixedMulCeil(utilScalar, rOneRaw, factorScaleDecimal))
		return fixedMulCeil(baseRate, rateModifierRaw, rateModifierScalar), true, false
	}
	if utilizationRaw.LessThanOrEqual(u95) {
		den := u95.Sub(utilTargetRaw)
		if den.IsZero() {
			baseRate := rBaseRaw.Add(rOneRaw).Add(rTwoRaw)
			return fixedMulCeil(baseRate, rateModifierRaw, rateModifierScalar), true, false
		}
		utilScalar := fixedDivCeil(utilizationRaw.Sub(utilTargetRaw), den, factorScaleDecimal)
		baseRate := rBaseRaw.Add(rOneRaw).Add(fixedMulCeil(utilScalar, rTwoRaw, factorScaleDecimal))
		return fixedMulCeil(baseRate, rateModifierRaw, rateModifierScalar), true, false
	}

	utilScalar := fixedDivCeil(utilizationRaw.Sub(u95), decimal.RequireFromString("500000"), factorScaleDecimal)
	extraRate := fixedMulCeil(utilScalar, rThreeRaw, factorScaleDecimal)
	intersection := fixedMulCeil(rBaseRaw.Add(rOneRaw).Add(rTwoRaw), rateModifierRaw, rateModifierScalar)
	return intersection.Add(extraRate), true, false
}

func reserveKey(poolContract, assetID string) string {
	return poolContract + "|" + assetID
}

func parseDecimalsInt(v int32) string {
	return strconv.FormatInt(int64(v), 10)
}

// auctionRow builds the gold-facing bindings.Auction for one live auction
// state. Shared by computeState's full-state loop and
// ProjectTemporaryStateChanges so both emit the identical row shape.
func (a *Adapter) auctionRow(auction contracts.AuctionState, ledgerSeq int64, closeTime time.Time) bindings.Auction {
	typeLabel := auctionTypeLabel(auction.AuctionType)
	lot := make([]bindings.AuctionAmount, 0, len(auction.Lot))
	for _, entry := range auction.Lot {
		lot = append(lot, bindings.AuctionAmount{AssetID: entry.AssetID, AmountRaw: entry.AmountRaw})
	}
	bid := make([]bindings.AuctionAmount, 0, len(auction.Bid))
	for _, entry := range auction.Bid {
		bid = append(bid, bindings.AuctionAmount{AssetID: entry.AssetID, AmountRaw: entry.AmountRaw})
	}
	return bindings.Auction{
		ID:          stableID(a.cfg.Protocol, auction.PoolContractID, auction.UserAddress, typeLabel, "auction"),
		Protocol:    a.cfg.Protocol,
		ContractID:  auction.PoolContractID,
		UserAddress: auction.UserAddress,
		AuctionType: typeLabel,
		Block:       auction.Block,
		Lot:         lot,
		Bid:         bid,
		LedgerSeq:   ledgerSeq,
		Timestamp:   closeTime,
	}
}

// queuedReserveRow builds the gold-facing bindings.QueuedReserve for one
// pending ResInit entry. NewConfig carries only the fields present on-chain.
// Shared by computeState's full-state loop and ProjectTemporaryStateChanges.
func (a *Adapter) queuedReserveRow(queued contracts.QueuedReserveState, ledgerSeq int64, closeTime time.Time) bindings.QueuedReserve {
	var unlockTime time.Time
	if unix, ok := parseUnixSeconds(queued.UnlockTimeRaw); ok {
		unlockTime = unix
	}
	newConfig := map[string]string{}
	for key, value := range map[string]string{
		"index":      queued.NewConfig.IndexRaw,
		"decimals":   queued.NewConfig.DecimalsRaw,
		"c_factor":   queued.NewConfig.CFactorRaw,
		"l_factor":   queued.NewConfig.LFactorRaw,
		"util":       queued.NewConfig.UtilRaw,
		"max_util":   queued.NewConfig.MaxUtilRaw,
		"r_base":     queued.NewConfig.RBaseRaw,
		"r_one":      queued.NewConfig.ROneRaw,
		"r_two":      queued.NewConfig.RTwoRaw,
		"r_three":    queued.NewConfig.RThreeRaw,
		"reactivity": queued.NewConfig.ReactivityRaw,
		"supply_cap": queued.NewConfig.SupplyCapRaw,
		"enabled":    queued.NewConfig.Enabled,
	} {
		if value != "" {
			newConfig[key] = value
		}
	}
	return bindings.QueuedReserve{
		ID:            stableID(a.cfg.Protocol, queued.PoolContractID, queued.AssetID, "queued_reserve"),
		Protocol:      a.cfg.Protocol,
		ContractID:    queued.PoolContractID,
		AssetID:       queued.AssetID,
		UnlockTimeRaw: queued.UnlockTimeRaw,
		UnlockTime:    unlockTime,
		NewConfig:     newConfig,
		LedgerSeq:     ledgerSeq,
		Timestamp:     closeTime,
	}
}

// ProjectTemporaryStateChanges projects one ledger's auction/queued-reserve
// transition set (Adapter.LastTemporaryStateChanges) into gold-facing
// lifecycle rows: a DirtyUpsert change is resolved against the freshly folded
// state and emitted as an Active=true row with the full payload (the same row
// computeState emits for that entry); a DirtyRemoval change needs identity
// only and is emitted as an Active=false row whose payload fields stay zero —
// never a fabricated outcome (a lone removed temporary entry cannot tell a
// filled auction from a deleted one, nor an executed queued change from a
// cancelled one). The result carries ONLY AuctionLifecycle and
// QueuedReserveLifecycle; every other TransformOutput slice stays empty.
//
// An upsert whose identity is absent from state (a malformed value the fold
// skipped, or a create-then-remove inside one ledger — which finalizes as a
// removal anyway) emits nothing: projecting a payload for an entry the fold
// does not hold would fabricate state.
func (a *Adapter) ProjectTemporaryStateChanges(state *bindings.LedgerState, changes []bindings.TemporaryStateChange, ledgerSeq int64, closeTime time.Time) *bindings.TransformOutput {
	out := &bindings.TransformOutput{LedgerSeq: ledgerSeq}
	if state == nil || len(changes) == 0 {
		return out
	}

	auctions := make(map[string]contracts.AuctionState, len(state.Auctions))
	for _, auction := range state.Auctions {
		auctions[typedAuctionEntityKey(auction.PoolContractID, auction.UserAddress, auction.AuctionType)] = auction
	}
	queued := make(map[string]contracts.QueuedReserveState, len(state.QueuedReserves))
	for _, entry := range state.QueuedReserves {
		queued[typedReserveEntityKey(entry.PoolContractID, entry.AssetID)] = entry
	}

	for _, change := range changes {
		switch change.Kind {
		case bindings.TemporaryAuction:
			if change.Action == bindings.DirtyRemoval {
				out.AuctionLifecycle = append(out.AuctionLifecycle, bindings.AuctionLifecycle{
					Auction: bindings.Auction{
						ID:          stableID(a.cfg.Protocol, change.PoolContractID, change.UserAddress, auctionTypeLabel(change.AuctionType), "auction"),
						Protocol:    a.cfg.Protocol,
						ContractID:  change.PoolContractID,
						UserAddress: change.UserAddress,
						AuctionType: auctionTypeLabel(change.AuctionType),
						LedgerSeq:   ledgerSeq,
						Timestamp:   closeTime,
					},
					Active: false,
				})
				continue
			}
			auction, ok := auctions[typedAuctionEntityKey(change.PoolContractID, change.UserAddress, change.AuctionType)]
			if !ok {
				continue
			}
			out.AuctionLifecycle = append(out.AuctionLifecycle, bindings.AuctionLifecycle{
				Auction: a.auctionRow(auction, ledgerSeq, closeTime),
				Active:  true,
			})
		case bindings.TemporaryQueuedReserve:
			if change.Action == bindings.DirtyRemoval {
				out.QueuedReserveLifecycle = append(out.QueuedReserveLifecycle, bindings.QueuedReserveLifecycle{
					QueuedReserve: bindings.QueuedReserve{
						ID:         stableID(a.cfg.Protocol, change.PoolContractID, change.AssetID, "queued_reserve"),
						Protocol:   a.cfg.Protocol,
						ContractID: change.PoolContractID,
						AssetID:    change.AssetID,
						LedgerSeq:  ledgerSeq,
						Timestamp:  closeTime,
					},
					Active: false,
				})
				continue
			}
			entry, ok := queued[typedReserveEntityKey(change.PoolContractID, change.AssetID)]
			if !ok {
				continue
			}
			out.QueuedReserveLifecycle = append(out.QueuedReserveLifecycle, bindings.QueuedReserveLifecycle{
				QueuedReserve: a.queuedReserveRow(entry, ledgerSeq, closeTime),
				Active:        true,
			})
		}
	}
	return out
}

// auctionTypeLabel maps the contract's AuctionType enum to its label form.
// An out-of-range value stays visible as its number — never silently coerced
// to a known type.
func auctionTypeLabel(auctType int32) string {
	switch auctType {
	case 0:
		return "user_liquidation"
	case 1:
		return "bad_debt"
	case 2:
		return "interest"
	default:
		return strconv.FormatInt(int64(auctType), 10)
	}
}

func boolString(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// parseUnixSeconds parses a raw unix-seconds string (as decoded from a u64
// contract field) into a UTC time.Time. Empty or unparseable input reports
// false rather than defaulting to the zero time being mistaken for a real
// expiration.
func parseUnixSeconds(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0).UTC(), true
}

func normalizedAPRInput(raw string) (decimal.Decimal, bool) {
	return normalizedDecimalInput(raw)
}

func normalizedDecimalInput(raw string) (decimal.Decimal, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return decZero, false
	}
	d, err := decimal.NewFromString(trimmed)
	if err != nil {
		return decZero, false
	}
	return d, true
}

func liquidationScenarios(pool *poolSummaryAccumulator, effectiveLiabilityUSD decimal.Decimal) map[string]string {
	if len(pool.liquidationCollaterals) == 0 || effectiveLiabilityUSD.LessThanOrEqual(decZero) {
		return map[string]string{}
	}
	scenarios := map[string]string{}
	for _, collateral := range pool.liquidationCollaterals {
		denominator := collateral.units.Mul(collateral.cFactor)
		if denominator.LessThanOrEqual(decZero) {
			continue
		}
		otherEffectiveCollateral := pool.effectiveCollateralUSD.Sub(collateral.effectiveCollateralUSD)
		numerator := effectiveLiabilityUSD.Sub(otherEffectiveCollateral)
		if numerator.LessThanOrEqual(decZero) {
			continue
		}
		price := numerator.Div(denominator)
		if price.GreaterThan(decZero) {
			scenarios[collateral.assetID] = numString(price)
		}
	}
	return scenarios
}

// singleCollateralLiquidationPrice returns the collateral price at which the
// worst pool's health factor would fall to 1, but only when that pool has a
// single collateral asset. With one collateral, effective collateral scales
// linearly with the asset's price while effective liability does not, so the
// price that brings health to 1 is simply the current price divided by the
// current health factor.
//
// With zero or several collateral assets the liquidation point depends on which
// asset's price moves, which is under-determined, so the scalar is left empty;
// the per-pool liquidation_price_scenarios map carries those cases instead.
// Collateral and liability rounding are untouched here — this reads the already
// rounded oracle price and health factor and only divides.
func singleCollateralLiquidationPrice(worstPool *poolSummaryAccumulator, healthFactor *decimal.Decimal, reserves map[string]normalizedReserve) string {
	if worstPool == nil || healthFactor == nil || len(worstPool.liquidationCollaterals) != 1 {
		return ""
	}
	if healthFactor.LessThanOrEqual(decZero) {
		return ""
	}
	collateral := worstPool.liquidationCollaterals[0]
	reserve, ok := reserves[reserveKey(worstPool.poolContract, collateral.assetID)]
	if !ok || !reserve.priceAvailable {
		return ""
	}
	return numString(reserve.usdPrice.Div(*healthFactor))
}

func parseFactorRaw(raw string) (decimal.Decimal, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return decZero, false
	}
	d := parseDecimalOrZero(trimmed)
	if d.IsZero() {
		return decZero, true
	}
	if d.Abs().LessThanOrEqual(decOne) {
		return d.Mul(factorScaleDecimal).Floor(), true
	}
	return d, true
}

// valuePoolBackstops fills each emitted pool-level Backstop row's LP component
// amounts (BLNDAmountRaw / USDCAmountRaw) and USDValue from the folded Comet
// state behind the row's backstop contract (V1-09, lidapters#31). The join is
// by exact identity: pool's backstop contract -> decoded instance -> BToken ->
// folded Comet pool -> Record.balance of the exact BLND/USDC token IDs. The
// price legs reuse the normalized reserves' folded oracle prices (the same
// ledger-pinned source as reserve valuation), bound deterministically in
// ascending pool-contract order. Absent-not-zero (D-09): unknown LP supply, a
// stored-zero denominator, a missing record, or a missing price leg leaves the
// row's fields absent — no fabricated zero, no hardcoded $1.
func (a *Adapter) valuePoolBackstops(input bindings.TransformInput, output *bindings.TransformOutput, reserves map[string]normalizedReserve) {
	if len(output.Backstops) == 0 {
		return
	}
	instances := make(map[string]contracts.BackstopInstanceState, len(input.State.BackstopInstances))
	for _, instance := range input.State.BackstopInstances {
		instances[instance.ContractID] = instance
	}
	comets := map[string]bindings.AMMPoolState{}
	for _, pool := range input.State.AMMPools {
		if pool.PoolType == cometPoolType {
			comets[pool.ContractID] = pool
		}
	}
	reservePrice := func(assetID string) (decimal.Decimal, bool) {
		if assetID == "" {
			return decZero, false
		}
		poolIDs := make([]string, 0, len(reserves))
		byPool := map[string]normalizedReserve{}
		for _, reserve := range reserves {
			if reserve.assetID == assetID && reserve.priceAvailable {
				poolIDs = append(poolIDs, reserve.poolContract)
				byPool[reserve.poolContract] = reserve
			}
		}
		if len(poolIDs) == 0 {
			return decZero, false
		}
		sort.Strings(poolIDs)
		return byPool[poolIDs[0]].usdPrice, true
	}
	for i := range output.Backstops {
		row := &output.Backstops[i]
		instance, ok := instances[row.BackstopContract]
		if !ok {
			continue
		}
		comet, ok := comets[instance.BackstopToken]
		if !ok || strings.TrimSpace(comet.TotalSharesRaw) == "" {
			continue
		}
		lpSupply := parseDecimalOrZero(comet.TotalSharesRaw)
		if lpSupply.IsZero() {
			// Stored-zero denominator: the quotient is undefined — absent, not
			// silently zero (D-09).
			continue
		}
		blndReserveRaw, usdcReserveRaw := "", ""
		for _, token := range comet.Tokens {
			switch token.AssetID {
			case instance.BLNDToken:
				blndReserveRaw = token.ReserveRaw
			case instance.USDCToken:
				usdcReserveRaw = token.ReserveRaw
			}
		}
		if strings.TrimSpace(blndReserveRaw) == "" || strings.TrimSpace(usdcReserveRaw) == "" {
			continue
		}
		blndPrice, ok := reservePrice(instance.BLNDToken)
		if !ok {
			continue
		}
		usdcPrice, ok := reservePrice(instance.USDCToken)
		if !ok {
			continue
		}
		lpTokens := parseDecimalOrZero(row.LPTokensRaw)
		blndComponent := fixedMulFloor(lpTokens, parseDecimalOrZero(blndReserveRaw), lpSupply)
		usdcComponent := fixedMulFloor(lpTokens, parseDecimalOrZero(usdcReserveRaw), lpSupply)
		row.BLNDAmountRaw = numString(blndComponent)
		row.USDCAmountRaw = numString(usdcComponent)
		// Component decimals match the fold's stamped BLNDDecimals/USDCDecimals
		// (both 7 on the pinned deployment).
		usd := blndComponent.Div(decimal.New(1, 7)).Mul(blndPrice).
			Add(usdcComponent.Div(decimal.New(1, 7)).Mul(usdcPrice))
		row.USDValue = numString(usd)
	}
}

func convertBackstopSharesToTokens(shares, poolShares, poolTokens decimal.Decimal) decimal.Decimal {
	if poolShares.IsZero() {
		return decZero
	}
	if shares.Equal(poolShares) {
		return poolTokens
	}
	return fixedMulFloor(shares, poolTokens, poolShares)
}

func marshalPoolBreakdown(breakdown map[string]poolBreakdownEntry) string {
	raw, err := json.Marshal(breakdown)
	if err != nil {
		return "{}"
	}
	return string(raw)
}
