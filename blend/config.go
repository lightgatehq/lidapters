package blend

// StateMode selects which state-decode strategy an Adapter is built with. The
// strategy is chosen once, at New, and swapped as a whole — there is no
// per-call flag. See state_strategy.go for the two-mode contract.
type StateMode string

const (
	// StateModeParanoid is the reference decoder: a stateless pure reducer that
	// rebuilds its whole in-memory mirror from the prior LedgerState on every
	// ledger. O(total state) per ledger, but with no cross-call state at all it
	// is the oracle every other strategy is measured against. Default.
	StateModeParanoid StateMode = "paranoid"
	// StateModeIncremental carries a live mirror across ledgers and only
	// re-derives what a ledger's changes touched. Output is byte-identical to
	// paranoid — same slices, same ordering, same checksums — at a per-ledger
	// cost of O(changes) instead of O(total state). The adapter becomes
	// stateful: it must not be shared across concurrent decodes.
	StateModeIncremental StateMode = "incremental"
)

type Config struct {
	AdapterID      string
	Protocol       string
	V2Scalar       string
	V2WasmHashes   map[string]struct{}
	AllowUnknownV2 bool
	// StateMode picks the state-decode strategy. Empty means StateModeParanoid,
	// so existing consumers are untouched; opting into StateModeIncremental is
	// a config decision made once at construction.
	StateMode StateMode
}

func DefaultConfig() Config {
	return Config{
		AdapterID: "blend",
		Protocol:  "blend",
		V2Scalar:  "1000000000000",
	}
}
