package aquarius

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lightgatehq/lidapters/bindings"
)

type Config struct {
	AdapterID        string
	Protocol         string
	Routers          map[string]struct{}
	PoolWasmHashes   map[string]string // hash -> pool type
	AllowUnknownWasm bool
	PoolSeeds        []PoolSeed
}

// PoolSeedPosition is one wallet's known position at the seed ledger.
type PoolSeedPosition struct {
	Address          string
	SharesRaw        string
	PendingRewardRaw string
}

// PoolSeed is a curated, operator-supplied snapshot of one pool's state at a
// known ledger (e.g. the harness's frozen baseline-state.json). Raw-meta
// folding can only reconstruct pool state for entries WRITTEN inside the
// folded window; a pool whose instance entry predates the window would
// otherwise fold to nothing. The seed acts as a gap-fill floor in DecodeState:
// it only fills fields the fold has not observed, and never overrides values
// decoded from the chain. Seeds are config (fingerprinted), so changing them
// invalidates fold checkpoints by construction.
type PoolSeed struct {
	ContractID             string
	RouterContract         string
	PoolHash               string
	PoolType               string
	Tokens                 []string // asset IDs, token-index order
	ReservesRaw            []string // same order as Tokens
	TotalSharesRaw         string
	FeeFractionRaw         string
	ProtocolFeeFractionRaw string
	Positions              []PoolSeedPosition
}

func DefaultConfig() Config { return Config{AdapterID: "aquarius", Protocol: "aquarius"} }

type Adapter struct {
	cfg       Config
	contracts map[string]struct{}
	assets    map[string]struct{}
	// shareTokens maps a pool's LP share-token contract to its pool. The
	// mapping is discovered from the pool instance's TokenShare entry; LP
	// position state rides Balance writes on the share token, so those
	// contracts are owned and their balances fold as positions of the pool.
	shareTokens map[string]string
	// diagnostics records entries the fold refused to guess about (e.g. a
	// concentrated Position key without decodable tick bounds), overwritten
	// by each DecodeState call. See bindings.DecodeDiagnosticsProvider.
	diagnostics []bindings.DecodeDiagnostic
}

var _ bindings.ProtocolAdapter = (*Adapter)(nil)
var _ bindings.StateReporter = (*Adapter)(nil)
var _ bindings.AssetRegistrar = (*Adapter)(nil)
var _ bindings.DecodeDiagnosticsProvider = (*Adapter)(nil)

// LastDecodeDiagnostics reports the entries the most recent DecodeState call
// refused to fold rather than guess about. Same single-fold-at-a-time
// contract as the interface documents: read immediately after folding.
func (a *Adapter) LastDecodeDiagnostics() []bindings.DecodeDiagnostic { return a.diagnostics }

// New preserves the original scaffold constructor for downstream callers.
func New() *Adapter { a, _ := NewWithConfig(Config{}); return a }

func NewWithConfig(config Config) (*Adapter, error) {
	cfg := DefaultConfig()
	{
		c := config
		if c.AdapterID != "" {
			cfg.AdapterID = c.AdapterID
		}
		if c.Protocol != "" {
			cfg.Protocol = c.Protocol
		}
		cfg.AllowUnknownWasm = c.AllowUnknownWasm
		cfg.Routers = copySet(c.Routers)
		cfg.PoolWasmHashes = map[string]string{}
		for k, v := range c.PoolWasmHashes {
			cfg.PoolWasmHashes[strings.ToLower(k)] = v
		}
		cfg.PoolSeeds = append(cfg.PoolSeeds, c.PoolSeeds...)
	}
	if cfg.AdapterID == "" || cfg.Protocol == "" {
		return nil, fmt.Errorf("aquarius: adapter id and protocol are required")
	}
	a := &Adapter{cfg: cfg, contracts: map[string]struct{}{}, assets: map[string]struct{}{}, shareTokens: map[string]string{}}
	for id := range cfg.Routers {
		a.contracts[id] = struct{}{}
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
	if _, ok := a.contracts[id]; ok {
		return true
	}
	if _, ok := a.shareTokens[id]; ok {
		return true
	}
	_, ok := a.assets[id]
	return ok
}
func (a *Adapter) RegisterContracts(ids ...string) {
	for _, id := range ids {
		if strings.TrimSpace(id) != "" {
			a.contracts[id] = struct{}{}
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
		seen[p.Address] = struct{}{}
	}
	x.Users = len(seen)
	return x
}
