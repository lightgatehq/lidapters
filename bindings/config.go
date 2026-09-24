package bindings

// ConfigRecord is an opaque, adapter-owned unit of low-frequency configuration
// that a storage host persists verbatim and returns unchanged on cold start.
//
// This is the inversion-of-control seam between an adapter (the authority on
// its own state) and a generic storage host. The host treats Kind and EntityKey
// as opaque namespacing strings and Payload as opaque bytes: it never decodes
// Payload and never interprets what a record means. The adapter serializes its
// own config struct into Payload as canonical JSON — it owns the shape. The
// host stores records append-on-change keyed by (Kind, EntityKey, Ledger),
// tombstones via Removed, and hands the latest-per-entity set back to the
// adapter's HydrateConfig on restart.
//
// Persist only LOW-FREQUENCY config (written on a config-key change, not every
// ledger): an oracle's asset->index map + decimals (NOT its prices), a pool's
// oracle/backstop/status/take-rate (NOT its reserves' economic data), a
// reserve's rate-curve/factor/cap config (NOT its b/d rate accumulators or
// supplies). The high-frequency data half is deliberately not persisted; it is
// rebuilt naturally from the ledger history after a restart.
type ConfigRecord struct {
	// Kind is the adapter-namespaced config kind, e.g. "blend.oracle". It scopes a
	// record to one shape the adapter knows how to (de)serialize.
	Kind string
	// EntityKey is the adapter-chosen opaque identity of the config entity within
	// its kind, e.g. a pool contract id or a pool_id|asset_id composite. The host
	// treats it as an opaque string and never parses it.
	EntityKey string
	// Ledger is the ledger sequence at which this config change was observed. The
	// host stamps the persisted row with it and reloads the latest per entity.
	Ledger uint32
	// Payload is the adapter-serialized canonical config body (JSON). It is empty
	// when Removed is true.
	Payload []byte
	// Removed tombstones the entity: its config was removed on-chain this ledger
	// (pool decommission, reserve drop, oracle removal). The host records the
	// tombstone and excludes the entity from the reload set.
	Removed bool
}

// ConfigTableSchema is one config kind's storage declaration: the physical
// table the host stores the kind's records in, plus the analytics surface
// derived from the jsonb payload. It is how the adapter keeps ALL kind-specific
// knowledge (table name, decimal scaling, which fields are query-hot) on its
// own side while the host stays a generic store that imports no adapter types.
//
// The host's write path is generic and identical for every kind: it INSERTs only
// (entity_key, ledger, payload, removed). The Generated columns populate
// themselves from payload — Postgres STORED generated columns — so the write never
// extracts a value and the read side is still SQL-queryable for behavioural
// analytics. The host renders the table DDL mechanically from this declaration.
type ConfigTableSchema struct {
	// Kind matches ConfigRecord.Kind, e.g. "blend.oracle".
	Kind string
	// Table is the physical table name, e.g. "blend_oracle_config". Every config
	// table shares the generic shape: entity_key TEXT, ledger BIGINT, removed BOOL,
	// payload JSONB, observed_at TIMESTAMPTZ, PRIMARY KEY (entity_key, ledger).
	Table string
	// Generated declares the STORED generated columns derived from payload — the
	// analytics surface. They are additive and never read by the reload path.
	Generated []ConfigGeneratedColumn
	// Indexes declares btree indexes over the generated columns for hot analytics
	// queries. The (entity_key, ledger DESC) reload index is always created and need
	// not be declared here.
	Indexes []ConfigIndex
	// Views declares read-only analytics views over the table (e.g. a per-asset
	// unnest of a jsonb array, or a decimals-scaled price join). They are additive
	// convenience surfaces the reload path never reads.
	Views []ConfigView
}

// ConfigView is an adapter-declared analytics view over one or more config tables.
// Body is the full SELECT the host wraps in CREATE OR REPLACE VIEW. The adapter
// owns the SQL (including any jsonb unnest or cross-table decimal scaling) so the
// host renders it without knowing the shape.
type ConfigView struct {
	Name string
	Body string
}

// ConfigGeneratedColumn is one STORED generated column derived from the jsonb
// payload. Expr is a Postgres expression over `payload` (e.g.
// `NULLIF(payload->>'c_factor',”)::numeric / 1e7`). The adapter owns the
// expression, including any protocol-specific decimal scaling, so the scaling
// knowledge stays out of the host.
type ConfigGeneratedColumn struct {
	Name    string
	SQLType string // e.g. "numeric", "text", "int"
	Expr    string // Postgres expression over `payload`
}

// ConfigIndex declares a btree index over generated columns of a config table.
type ConfigIndex struct {
	Name    string
	Columns []string
}

// ConfigStateful is the optional capability a ProtocolAdapter implements when it
// manages low-frequency config that a storage host should persist across process
// restarts. The host type-asserts a ProtocolAdapter for it; adapters with no
// cross-restart config need not implement it.
//
// All methods are PURE (no DB/network/clock/random) so they preserve the
// run-twice determinism of the state decode: the adapter DECLARES config
// (schema + records) and the host PERSISTS it — the reducer itself does no I/O.
type ConfigStateful interface {
	// ConfigSchema declares one table per kind: the physical table plus the
	// generated-column analytics surface. The host derives the kinds it loads on
	// cold start from the declared kinds, and renders the table DDL from this
	// declaration — it never hard-codes a kind's shape.
	ConfigSchema() []ConfigTableSchema

	// ConfigRecords derives this ledger's config changes as opaque records from
	// the owned contract-data changes and the freshly decoded next state. It is
	// emit-on-change: a record is produced only for an entity whose config key
	// appeared in this ledger's meta (an upsert carrying the entity's current
	// config payload) or was removed (a tombstone). It is a pure function of its
	// inputs.
	ConfigRecords(next *LedgerState, ownedChanges []ContractDataChange, ledgerSeq int64) []ConfigRecord

	// HydrateConfig reconstructs the seed LedgerState from previously persisted
	// records, which the host has already reduced to the latest record per (Kind,
	// EntityKey) with tombstoned entities excluded. The result is passed as
	// priorState to the first live decode after a restart, so a restart resumes
	// with its config (oracle map, pool/reserve config) already in place. Pure.
	HydrateConfig(records []ConfigRecord) (*LedgerState, error)
}
