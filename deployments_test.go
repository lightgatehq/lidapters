package lidapters

import "testing"

func TestDeployPinsParse(t *testing.T) {
	// init() panics if the embedded TOML fails to parse, so reaching here with a
	// populated table proves the canonical file parsed.
	if len(DeployPins()) == 0 {
		t.Fatal("deployments.toml parsed to zero rows")
	}
}

func TestDeployPinTestnetResolves(t *testing.T) {
	// Exact create_contract ledgers (replacing the earlier 3294000 approximation).
	cases := map[string]int64{
		"CANNO5MLDIANFGZBIFUAYLY2GZLMJOVQKIGBEGWYWQRCRR3QYKEKJAXU": 3289010, // pool
		"CCSXSQDUZLCRIGRWLCYK3F547PLBGSFTUVJRQCIXRO2ZDI3VIZKN3NH2": 3288990, // oracle
	}
	for id, want := range cases {
		got, ok := DeployLedger("testnet", id)
		if !ok {
			t.Fatalf("testnet %s: expected curated pin, got ok=false", id)
		}
		if got != want {
			t.Fatalf("testnet %s: got deploy ledger %d, want %d", id, got, want)
		}
	}
}

func TestDeployPinMainnetResolves(t *testing.T) {
	// All four mainnet pins are now curated (exact create_contract ledgers).
	cases := map[string]int64{
		"CD74A3C54EKUVEGUC6WNTUPOTHB624WFKXN3IYTFJGX3EHXDXHCYMXXR": 56647972, // YieldBloxV2 oracle
		"CCVTVW2CVA7JLH4ROQGP3CU4T3EXVCK66AZGSM4MUQPXAI4QHCZPOATS": 56569289, // FixedV2 oracle
		"CCCCIQSDILITHMM7PBSLVDT5MISSY7R26MNZXCX4H7J5JQ5FPIYOGYFS": 56658268, // YieldBloxV2 pool
		"CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD": 56615475, // FixedV2 pool
	}
	for id, want := range cases {
		got, ok := DeployLedger("public", id)
		if !ok {
			t.Fatalf("public %s: expected curated pin, got ok=false", id)
		}
		if got != want {
			t.Fatalf("public %s: got deploy ledger %d, want %d", id, got, want)
		}
	}
}

func TestDeployPinUnknownContract(t *testing.T) {
	if got, ok := DeployLedger("testnet", "CNOTACONTRACT"); ok || got != 0 {
		t.Fatalf("unknown contract: got (%d, %v), want (0, false)", got, ok)
	}
}
