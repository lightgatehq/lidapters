// Package fxdao decodes and folds the FxDAO vaults contract into the neutral
// vault state carrier and gold shapes. FXDAO is a SNAPSHOT-ONLY surface: the
// vaults contract emits zero contract events by design (no publish call
// anywhere in its source), so this adapter registers NO activity vocabulary —
// there is no activity surface to emit. Vault state folds from contract_data
// deltas only, and ANY event arriving on an owned contract is quarantined as
// an anomaly (something the source says cannot happen), never classified,
// never dropped.
//
// Storage layout, derived from the pinned protocol sources (FxDAO/FxDAO-SC
// @ b73d8b6, contracts/vaults/src/storage/vaults.rs unless noted):
//
//   - persistent Vault((Address, Symbol)) — the enum-variant-with-TUPLE-
//     payload encoding makes the KEY the nested-Vec shape
//     Vec[Symbol("Vault"), Vec[Address(owner), Symbol(denomination)]]; a flat
//     Vec[Symbol, Address, Symbol] decode matches nothing. Value: the Vault
//     struct (index/total_debt/total_collateral u128, next_key an
//     OptionalVaultKey linked-list pointer).
//   - persistent VaultIndex(VaultIndexKey{user, denomination}) → u128, the
//     owner's current vault index.
//   - instance VaultsInfo(Symbol) → per-denomination totals and rate
//     parameters (total_vaults u64, total_debt/total_col/min_col_rate/
//     min_debt_creation/opening_col_rate u128, lowest_key).
//   - instance CoreState / Currency(Symbol): recognized-not-carried.
//
// Persistent entries live under 28-day TTL bumps with a 14-day threshold
// (vaults.rs:3-5), so archival and restore are the COMMON case, not an edge:
// a LedgerEntryRestored change is a LIVE write (the raw change variant is
// preserved on VaultState.Restored), and only a genuine Removed change closes
// a vault.
package fxdao

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lightgatehq/lidapters/bindings"
)

type Config struct {
	AdapterID string
	Protocol  string
	// VaultsContracts is the owned vaults-contract set.
	VaultsContracts map[string]struct{}
}

func DefaultConfig() Config { return Config{AdapterID: "fxdao", Protocol: "fxdao"} }

type Adapter struct {
	cfg       Config
	contracts map[string]struct{}
	dirty     []DirtyVault
}

var _ bindings.ProtocolAdapter = (*Adapter)(nil)
var _ bindings.StateReporter = (*Adapter)(nil)

// DirtyVault is one (account, denomination) vault the most recent DecodeState
// call touched — the vault analog of bindings.DirtyPosition, with the
// protocol's own dirty-key shape.
type DirtyVault struct {
	ContractID   string
	Account      string
	Denomination string
	Kind         bindings.DirtyKind
}

func New() *Adapter { a, _ := NewWithConfig(Config{}); return a }

func NewWithConfig(config Config) (*Adapter, error) {
	cfg := DefaultConfig()
	if config.AdapterID != "" {
		cfg.AdapterID = config.AdapterID
	}
	if config.Protocol != "" {
		cfg.Protocol = config.Protocol
	}
	cfg.VaultsContracts = map[string]struct{}{}
	for id := range config.VaultsContracts {
		if strings.TrimSpace(id) != "" {
			cfg.VaultsContracts[id] = struct{}{}
		}
	}
	if cfg.AdapterID == "" || cfg.Protocol == "" {
		return nil, fmt.Errorf("fxdao: adapter id and protocol are required")
	}
	a := &Adapter{cfg: cfg, contracts: map[string]struct{}{}}
	for id := range cfg.VaultsContracts {
		a.contracts[id] = struct{}{}
	}
	return a, nil
}

func (a *Adapter) ID() string       { return a.cfg.AdapterID }
func (a *Adapter) Protocol() string { return a.cfg.Protocol }

func (a *Adapter) OwnsContract(id string) bool {
	_, ok := a.contracts[id]
	return ok
}

func (a *Adapter) RegisterContracts(ids ...string) {
	for _, id := range ids {
		if strings.TrimSpace(id) != "" {
			a.contracts[id] = struct{}{}
		}
	}
}

// LastDirtyVaults reports the (account, denomination) vaults the most recent
// DecodeState call touched, and whether each was an upsert or a genuine
// on-chain removal. Same single-fold-at-a-time contract as
// bindings.DirtyPositionsProvider: read immediately after folding.
func (a *Adapter) LastDirtyVaults() []DirtyVault {
	return a.dirty
}

func (a *Adapter) StateStats(s *bindings.LedgerState) bindings.StateStats {
	var x bindings.StateStats
	if s == nil {
		return x
	}
	seenContracts := map[string]struct{}{}
	seenAccounts := map[string]struct{}{}
	for _, v := range s.Vaults {
		if v.Protocol != a.cfg.Protocol {
			continue
		}
		seenContracts[v.ContractID] = struct{}{}
		if !v.Closed {
			seenAccounts[v.Account] = struct{}{}
		}
	}
	x.Pools = len(seenContracts)
	x.Users = len(seenAccounts)
	return x
}

func sortDirty(d []DirtyVault) {
	sort.Slice(d, func(i, j int) bool {
		if d[i].ContractID != d[j].ContractID {
			return d[i].ContractID < d[j].ContractID
		}
		if d[i].Account != d[j].Account {
			return d[i].Account < d[j].Account
		}
		return d[i].Denomination < d[j].Denomination
	})
}
