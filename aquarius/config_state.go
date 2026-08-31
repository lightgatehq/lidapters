package aquarius

import (
	"encoding/json"
	"fmt"

	"github.com/lightgatehq/lidapters/bindings"
)

var _ bindings.ConfigStateful = (*Adapter)(nil)

func (a *Adapter) ConfigSchema() []bindings.ConfigTableSchema {
	return []bindings.ConfigTableSchema{{Kind: "aquarius.pool", Table: "aquarius_pool_config", Generated: []bindings.ConfigGeneratedColumn{{Name: "pool_type", SQLType: "text", Expr: "payload->>'PoolType'"}, {Name: "wasm_hash", SQLType: "text", Expr: "payload->>'WasmHash'"}}}}
}
func (a *Adapter) ConfigRecords(next *bindings.LedgerState, changes []bindings.ContractDataChange, ledgerSeq int64) []bindings.ConfigRecord {
	if next == nil {
		return nil
	}
	touched := map[string]struct{}{}
	for _, c := range changes {
		touched[c.ContractID] = struct{}{}
	}
	var out []bindings.ConfigRecord
	for _, p := range next.AMMPools {
		if p.Protocol != a.cfg.Protocol {
			continue
		}
		if _, ok := touched[p.ContractID]; !ok {
			continue
		}
		b, _ := json.Marshal(p)
		out = append(out, bindings.ConfigRecord{Kind: "aquarius.pool", EntityKey: p.ContractID, Ledger: uint32(ledgerSeq), Payload: b})
	}
	return out
}
func (a *Adapter) HydrateConfig(records []bindings.ConfigRecord) (*bindings.LedgerState, error) {
	s := &bindings.LedgerState{}
	for _, r := range records {
		if r.Kind != "aquarius.pool" || r.Removed {
			continue
		}
		var p bindings.AMMPoolState
		if err := json.Unmarshal(r.Payload, &p); err != nil {
			return nil, fmt.Errorf("aquarius: hydrate pool %s: %w", r.EntityKey, err)
		}
		s.AMMPools = append(s.AMMPools, p)
		a.RegisterContracts(p.ContractID)
	}
	return s, nil
}
