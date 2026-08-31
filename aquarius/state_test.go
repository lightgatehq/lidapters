package aquarius

import (
	"testing"

	"github.com/lightgatehq/lidapters/bindings"
)

func testSeedConfig() Config {
	return Config{PoolSeeds: []PoolSeed{{
		ContractID:     "pool",
		RouterContract: "router",
		PoolHash:       "deadbeef",
		PoolType:       "volatile",
		Tokens:         []string{"a", "b"},
		ReservesRaw:    []string{"100", "200"},
		TotalSharesRaw: "300",
		FeeFractionRaw: "30",
		Positions:      []PoolSeedPosition{{Address: "user", SharesRaw: "0", PendingRewardRaw: "205"}},
	}}}
}

func TestPoolSeedInsertsAbsentState(t *testing.T) {
	a, err := NewWithConfig(testSeedConfig())
	if err != nil {
		t.Fatal(err)
	}
	a.RegisterContracts("pool")
	s, err := a.DecodeState(nil, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.AMMPools) != 1 {
		t.Fatalf("pools %#v", s.AMMPools)
	}
	p := s.AMMPools[0]
	if p.PoolHash != "deadbeef" || p.PoolType != "volatile" || p.TotalSharesRaw != "300" || p.FeeFractionRaw != "30" || p.RouterContract != "router" {
		t.Fatalf("seed pool %#v", p)
	}
	if len(p.Tokens) != 2 || p.Tokens[0].AssetID != "a" || p.Tokens[0].ReserveRaw != "100" || p.Tokens[1].AssetID != "b" || p.Tokens[1].ReserveRaw != "200" {
		t.Fatalf("seed tokens %#v", p.Tokens)
	}
	if len(s.AMMPositions) != 1 {
		t.Fatalf("positions %#v", s.AMMPositions)
	}
	pos := s.AMMPositions[0]
	if pos.Address != "user" || pos.SharesRaw != "0" || pos.PendingRewardRaw != "205" {
		t.Fatalf("seed position %#v", pos)
	}
}

func TestPoolSeedNeverOverridesObservedState(t *testing.T) {
	a, err := NewWithConfig(testSeedConfig())
	if err != nil {
		t.Fatal(err)
	}
	a.RegisterContracts("pool")
	prior := &bindings.LedgerState{
		AMMPools: []bindings.AMMPoolState{{
			Protocol:       "aquarius",
			ContractID:     "pool",
			PoolType:       "constant_product",
			TotalSharesRaw: "999",
			Tokens:         []bindings.AMMTokenReserve{{AssetID: "a", ReserveRaw: "1"}, {AssetID: "b", ReserveRaw: "2"}},
		}},
		AMMPositions: []bindings.AMMPositionState{{Address: "user", PoolContractID: "pool", SharesRaw: "7", PendingRewardRaw: "9"}},
	}
	s, err := a.DecodeState(prior, nil, 11)
	if err != nil {
		t.Fatal(err)
	}
	p := s.AMMPools[0]
	if p.TotalSharesRaw != "999" || p.Tokens[0].ReserveRaw != "1" || p.Tokens[1].ReserveRaw != "2" || p.PoolType != "constant_product" {
		t.Fatalf("observed state clobbered by seed: %#v", p)
	}
	if p.PoolHash != "deadbeef" || p.RouterContract != "router" || p.FeeFractionRaw != "30" {
		t.Fatalf("empty fields not gap-filled from seed: %#v", p)
	}
	pos := s.AMMPositions[0]
	if pos.SharesRaw != "7" || pos.PendingRewardRaw != "9" {
		t.Fatalf("observed position clobbered by seed: %#v", pos)
	}
}
