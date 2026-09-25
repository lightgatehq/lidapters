# lidapters

Protocol adapters for Stellar and Soroban DeFi protocols, as a Go module.

Each adapter takes what a ledger close produces for a protocol's contracts
(contract-data changes and contract events) and turns it into typed protocol
state and output rows: positions, per-account summaries with health factor,
reserves, pools, vaults, and activity. It is written for indexers and services
that need protocol-aware decoding but bring their own ledger source, storage and
runtime. The adapters do no I/O: no database, network, clock or randomness.

Requires Go 1.25 or later.

## Install

```sh
go get github.com/lightgatehq/lidapters@v0.16.1
```

## Packages

| Package | What it does |
|---|---|
| `bindings` | The shared contract every adapter implements: `ProtocolAdapter`, the input types (`ContractDataChange`, `RawEventEnvelope`, `TransformInput`), the state carrier (`LedgerState`) and the output rows (`TransformOutput`). |
| `blend` | Blend lending pools: reserves, supply/collateral/liability positions, per-account summaries with health factor, backstops (including the Comet LP behind them), auctions, emissions, and oracle prices from Reflector feeds and oracle aggregators. |
| `blend/contracts` | Blend-domain state types (pool, reserve, backstop, auction, oracle) and the position and activity enums. |
| `blend/discovery` | Finds Blend pools by scanning a ledger's close meta for pool-factory `deploy` events. |
| `aquarius` | Aquarius AMM pools (constant-product, stable and concentrated): pool state, LP positions broken into per-token components, concentrated-range positions with unclaimed fees, pending AQUA rewards, and activity. |
| `aquarius/discovery` | Finds Aquarius pools by scanning a ledger's close meta for pool-creation events from known routers. |
| `soroswap` | Soroswap factory and pairs: pair reserves and supply, LP positions as per-token components, and pair activity. |
| `fxdao` | FxDAO vaults: one row per owner and denomination with its debt, collateral and vault index (the contract's collateralisation ordering); collateral ratio and USD values are left null. The vaults contract emits no events, so state comes from contract data only. |

Every protocol package exposes `DefaultConfig` and a constructor — `New(cfg)`
in `blend`; `New()` and `NewWithConfig(cfg)` in the others — and its `Adapter`
satisfies `bindings.ProtocolAdapter`.

## The adapter contract

`bindings.ProtocolAdapter` is the interface a consumer programs against:

| Method | Purpose |
|---|---|
| `ID() string` | The adapter instance's identifier. |
| `Protocol() string` | The protocol name the adapter serves. |
| `OwnsContract(contractID string) bool` | Whether a contract's changes and events belong to this adapter. |
| `DecodeState(prior, changes, ledgerSeq)` | Applies one ledger's contract-data changes to the prior state and returns the next state. Deterministic, no I/O. |
| `Transform(input TransformInput)` | Turns one ledger's events plus the decoded state into a `TransformOutput`. Deterministic, no I/O. An `Adapter` value keeps per-call results and, in some modes, state between ledgers, so it is not safe for concurrent use. |

Adapters may also implement optional capabilities, which a consumer discovers
with a type assertion: `ConfigStateful`, `CloseTimeStateDecoder`,
`DirtyPositionsProvider`, `DirtyBackstopsProvider`, `DecodeDiagnosticsProvider`,
`TemporaryStateChangesProvider`, `StateReporter` and `AssetRegistrar`. Each is
documented where it is declared in `bindings`.

## Deployment pins

`deployments.toml` is a hand-curated table of protocol contracts, one `[[pin]]`
per contract, with its network (`testnet` or `public`), contract id, role (for
example `pool`, `oracle`, `price_feed`, `soroswap_factory`, `fxdao_vaults`) and
the ledger the contract was deployed in, plus where that value came from. A
deploy ledger is immutable, so it tells a consumer the earliest ledger it must
read from to see a contract's full history. A row without `deploy_ledger` is
not yet curated.

The table is embedded in the root package at build time:

```go
import "github.com/lightgatehq/lidapters"

pins := lidapters.DeployPins() // []lidapters.DeployPin, every row

ledger, ok := lidapters.DeployLedger("public", contractID)
// ok is false when the contract is unknown or its row is not curated
```

## Development

```sh
make test   # go test ./...
make lint   # go vet ./..., plus golangci-lint when installed
make run    # go build ./... (the module has no binary; this is a compile check)
make tidy   # go mod tidy
```

CI runs the build, `go test -race ./...`, `go vet`, a `go mod tidy` diff check
and `govulncheck` on every pull request.

## Versioning

The module follows semantic versioning and is pre-1.0:

- A minor release (`v0.x.0`) may change exported types and signatures. Pin a
  tag and read the release notes before moving to a new minor version.
- A patch release (`v0.x.y`) does not change exported signatures or the meaning
  of existing rows in `deployments.toml`.

Releases are cut by pushing a `v*` tag; the release workflow builds the module,
runs the tests and publishes a GitHub release with generated notes.

v0.16.1 is the first tag that carries the LICENSE file; earlier tags fetch the
same code without it.

## Contributing

- Run `make test` and `make lint` before opening a pull request.
- Run `make tidy` and commit any change to `go.mod` and `go.sum`; CI fails if
  they are out of date.
- Keep adapters free of I/O. The module's only direct dependencies are the Stellar Go
  SDK, `shopspring/decimal` and `BurntSushi/toml`; adapters do not import
  database, network, message-queue or service-runtime packages. This is a review
  rule, not an automated check.
- New decode behaviour comes with a test, ideally against a captured mainnet
  ledger entry in the package's `testdata/`.

## License

Apache License 2.0. See [LICENSE](LICENSE).
