// Package soroswap decodes and folds Soroswap (Uniswap-V2-style) factory and
// pair contracts into the neutral AMM state carrier and gold shapes.
//
// Soroswap is deliberately its own package, not a configuration of the
// aquarius package: pair instance storage uses RAW u32 ScVal keys (a
// #[repr(u32)] enum, Token0=0..KLast=5), the pair contract IS its own SEP-41
// LP token (persistent Balance(Address) entries plus an instance TotalSupply
// under the Vec[Symbol("TotalSupply")] key shape), and there is no reward
// machinery. Storage and event layouts were derived from the pinned protocol
// sources (soroswap/core @ bb90a65): contracts/pair/src/storage.rs (u32 keys,
// i128 reserves, KLast read with a default and possibly ABSENT),
// contracts/pair/src/soroswap_pair_token/ (Balance/TotalSupply/METADATA and
// the SEP-41 token events), contracts/factory/src/storage.rs (FeeTo,
// FeeToSetter, FeesEnabled, TotalPairs instance keys; PairWasmHash and the
// PairAddressesNIndexed(u32) discovery registry, persistent).
//
// Decode posture: absent-not-zero (an absent KLast or FeesEnabled decodes to
// nothing, never a fabricated zero/false), quarantine-don't-drop (an
// unrecognized event on an owned contract is quarantined with its raw bytes,
// never silently skipped). Recognized-but-not-carried storage: KLast (pair
// fee-accounting internal, no gold column), Allowance entries (temporary
// SEP-41 allowances, not positions), and the factory's own instance fields —
// they decode cleanly and are intentionally not folded into state.
package soroswap

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lightgatehq/lidapters/bindings"
)

type Config struct {
	AdapterID string
	Protocol  string
	// Factories is the pair-discovery authority set: contract ids whose
	// PairAddressesNIndexed(u32) writes register pairs into the fold.
	Factories map[string]struct{}
	// Routers emit convenience-op events only (init/add/remove/swap); they are
	// owned so their events are recognized, never quarantined, never activities.
	Routers map[string]struct{}
	// PairWasmHashes is an optional allow-list (lowercase hex). When non-empty,
	// a pair instance whose executable is not listed does not fold.
	PairWasmHashes map[string]struct{}
}

func DefaultConfig() Config { return Config{AdapterID: "soroswap", Protocol: "soroswap"} }

type Adapter struct {
	cfg       Config
	factories map[string]struct{}
	routers   map[string]struct{}
	pairs     map[string]struct{}
	assets    map[string]struct{}
	dirty     []bindings.DirtyPosition
}

var _ bindings.ProtocolAdapter = (*Adapter)(nil)
var _ bindings.DirtyPositionsProvider = (*Adapter)(nil)
var _ bindings.StateReporter = (*Adapter)(nil)
var _ bindings.AssetRegistrar = (*Adapter)(nil)

func New() *Adapter { a, _ := NewWithConfig(Config{}); return a }

func NewWithConfig(config Config) (*Adapter, error) {
	cfg := DefaultConfig()
	if config.AdapterID != "" {
		cfg.AdapterID = config.AdapterID
	}
	if config.Protocol != "" {
		cfg.Protocol = config.Protocol
	}
	cfg.Factories = copySet(config.Factories)
	cfg.Routers = copySet(config.Routers)
	cfg.PairWasmHashes = map[string]struct{}{}
	for h := range config.PairWasmHashes {
		if strings.TrimSpace(h) != "" {
			cfg.PairWasmHashes[strings.ToLower(h)] = struct{}{}
		}
	}
	if cfg.AdapterID == "" || cfg.Protocol == "" {
		return nil, fmt.Errorf("soroswap: adapter id and protocol are required")
	}
	a := &Adapter{
		cfg:       cfg,
		factories: copySet(cfg.Factories),
		routers:   copySet(cfg.Routers),
		pairs:     map[string]struct{}{},
		assets:    map[string]struct{}{},
	}
	return a, nil
}

func copySet(in map[string]struct{}) map[string]struct{} {
	out := map[string]struct{}{}
	for k := range in {
		if strings.TrimSpace(k) != "" {
			out[k] = struct{}{}
		}
	}
	return out
}

func (a *Adapter) ID() string       { return a.cfg.AdapterID }
func (a *Adapter) Protocol() string { return a.cfg.Protocol }

func (a *Adapter) OwnsContract(id string) bool {
	if _, ok := a.factories[id]; ok {
		return true
	}
	if _, ok := a.routers[id]; ok {
		return true
	}
	if _, ok := a.pairs[id]; ok {
		return true
	}
	_, ok := a.assets[id]
	return ok
}

// RegisterPairContracts seeds pairs known ahead of the fold (e.g. a curated
// verification set); in-fold discovery from the factory registry adds the rest.
func (a *Adapter) RegisterPairContracts(ids ...string) {
	for _, id := range ids {
		if strings.TrimSpace(id) != "" {
			a.pairs[id] = struct{}{}
		}
	}
}

func (a *Adapter) RegisterAssetContracts(ids ...string) {
	for _, id := range ids {
		if strings.TrimSpace(id) != "" {
			a.assets[id] = struct{}{}
		}
	}
}

func (a *Adapter) StateAssetContracts(s *bindings.LedgerState) []string {
	set := map[string]struct{}{}
	if s != nil {
		for _, p := range s.AMMPools {
			if p.Protocol == a.cfg.Protocol {
				for _, t := range p.Tokens {
					if t.AssetID != "" {
						set[t.AssetID] = struct{}{}
					}
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (a *Adapter) StateStats(s *bindings.LedgerState) bindings.StateStats {
	var x bindings.StateStats
	if s == nil {
		return x
	}
	for _, p := range s.AMMPools {
		if p.Protocol == a.cfg.Protocol {
			x.Pools++
		}
	}
	seen := map[string]struct{}{}
	for _, p := range s.AMMPositions {
		if _, ok := a.pairs[p.PoolContractID]; ok {
			seen[p.Address] = struct{}{}
		}
	}
	x.Users = len(seen)
	return x
}

// LastDirtyPositions reports the (address, pair) LP balances the most recent
// DecodeState call touched. Same single-fold-at-a-time contract as the
// interface documents: read immediately after folding.
func (a *Adapter) LastDirtyPositions() []bindings.DirtyPosition {
	return a.dirty
}
