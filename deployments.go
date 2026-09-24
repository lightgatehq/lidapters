package lidapters

import (
	_ "embed"
	"fmt"

	"github.com/BurntSushi/toml"
)

//go:embed deployments.toml
var deployPinsTOML []byte

// DeployPin is one row of the canonical deploy-ledger table. DeployLedger is a
// pointer so an omitted value (uncurated row) is distinguishable from ledger 0.
type DeployPin struct {
	Network      string `toml:"network"`
	ContractID   string `toml:"contract_id"`
	Role         string `toml:"role"`
	DeployLedger *int64 `toml:"deploy_ledger"`
	Source       string `toml:"source"`
	Notes        string `toml:"notes"`
}

var deployPins []DeployPin

func init() {
	var file struct {
		Pin []DeployPin `toml:"pin"`
	}
	if err := toml.Unmarshal(deployPinsTOML, &file); err != nil {
		// Embedded canonical data must always parse; a failure is a build-time
		// data bug, not a runtime condition to recover from.
		panic(fmt.Sprintf("lidapters: parse deployments.toml: %v", err))
	}
	deployPins = file.Pin
}

// DeployPins returns every row of the canonical table.
func DeployPins() []DeployPin {
	return deployPins
}

// DeployLedger returns the curated deploy ledger for a contract on a network.
// The second result is false when the contract is unknown or its row is
// uncurated. (Named DeployLedger, not DeployPin, to avoid colliding with the
// DeployPin row type in this package.)
func DeployLedger(network, contractID string) (int64, bool) {
	for _, p := range deployPins {
		if p.Network == network && p.ContractID == contractID {
			if p.DeployLedger == nil {
				return 0, false
			}
			return *p.DeployLedger, true
		}
	}
	return 0, false
}
