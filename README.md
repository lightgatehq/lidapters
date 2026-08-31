# Lidapters

Public adapter module for Lightgate protocol adapters.

The module is nested per protocol. Shared, protocol-agnostic seam types live in
`bindings`; each protocol owns its own package tree. The adapters must not
import relay, ingest, database, queue, or runtime packages.

```
lidapters/
├── deployments.toml    canonical cross-protocol deploy-ledger pins
├── deployments.go      pin loader (root package lidapters)
├── bindings/           ProtocolAdapter seam, envelopes, gold rows, config IoC
├── blend/              Blend adapter (Transform, DecodeState, config state)
│   ├── contracts/      Blend-domain types: pool/reserve/oracle state, enums
│   └── discovery/      event-based pool enumeration from raw close-meta
└── aquarius/           Aquarius adapter scaffold (not implemented yet)
```

## Imports

```go
import (
	"github.com/lightgatehq/lidapters/bindings"
	"github.com/lightgatehq/lidapters/blend"
	"github.com/lightgatehq/lidapters/blend/contracts"
	"github.com/lightgatehq/lidapters/blend/discovery"
)
```

The module follows semantic versioning. Consumers should pin a release such as:

```sh
go get github.com/lightgatehq/lidapters@v0.4.0
```

## Commands

```sh
make test
make tidy
```
