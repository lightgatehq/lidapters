package aquarius

import (
	"math/big"
	"testing"
	"time"

	"github.com/lightgatehq/lidapters/bindings"
)

func TestProRataFloors(t *testing.T) {
	got, err := proRata("2", "10", "3")
	if err != nil || got != "6" {
		t.Fatalf("got %q err %v", got, err)
	}
	if _, err = proRata("1", "1", "0"); err == nil {
		t.Fatal("zero total shares accepted")
	}
}
// Frozen mainnet reward checkpoints from run-2026-07-18T15-10-48-353Z:
// deposit ledger 63535435 (close 1784387458), step-4 ledger 63535437 (close
// 1784387469), withdraw ledger 63535438 (close 1784387475). Expected pending
// rewards come from the harness's RPC get_user_reward reads.
func TestPendingRewardFrozenCheckpoints(t *testing.T) {
	pool := bindings.AMMPoolState{
		RewardTpsRaw:         "3619584",
		RewardExpiredAtRaw:   "1784391020",
		RewardAccumulatedRaw: "2223695363846271",
		RewardLastTimeRaw:    "1784387458",
		WorkingSupplyRaw:     "1127868973968343",
	}
	pos := bindings.AMMPositionState{
		PendingRewardRaw:         "205",
		WorkingBalanceRaw:        "514194607",
		RewardPoolAccumulatedRaw: "2223695363846271",
	}
	if got := pendingReward(pool, pos, time.Unix(1784387458, 0).UTC()); got != "205" {
		t.Fatalf("checkpoint instant: got %s want 205", got)
	}
	if got := pendingReward(pool, pos, time.Unix(1784387469, 0).UTC()); got != "223" {
		t.Fatalf("step-4 live accrual: got %s want 223", got)
	}
	// Zero working balance: no accrual regardless of elapsed time.
	closed := pos
	closed.PendingRewardRaw = "233"
	closed.WorkingBalanceRaw = "0"
	closed.RewardPoolAccumulatedRaw = "2223695425379199"
	if got := pendingReward(pool, closed, time.Unix(1784387489, 0).UTC()); got != "233" {
		t.Fatalf("post-withdraw: got %s want 233", got)
	}
	// Expired emission caps accrual: nothing accrues past expired_at.
	expiredPool := pool
	expiredPool.RewardExpiredAtRaw = "1784387460"
	if got := pendingReward(expiredPool, pos, time.Unix(1784387469, 0).UTC()); got == "223" {
		t.Fatal("accrual continued past emission expiry")
	}
	// Missing checkpoint inputs: checkpointed to_claim only, no fabrication.
	bare := pos
	bare.RewardPoolAccumulatedRaw = ""
	if got := pendingReward(pool, bare, time.Unix(1784387469, 0).UTC()); got != "205" {
		t.Fatalf("missing checkpoint acc: got %s want 205", got)
	}
}

func TestRangePrincipalBranches(t *testing.T) {
	q := q96.String()
	two := new(big.Int).Lsh(big.NewInt(1), 97).String()
	three := new(big.Int).Mul(big.NewInt(3), q96).String()
	a0, a1, err := rangePrincipal("100", q, q, two)
	if err != nil || a0 != "50" || a1 != "0" {
		t.Fatalf("below: %s %s %v", a0, a1, err)
	}
	a0, a1, err = rangePrincipal("100", three, q, two)
	if err != nil || a0 != "0" || a1 != "100" {
		t.Fatalf("above: %s %s %v", a0, a1, err)
	}
}
