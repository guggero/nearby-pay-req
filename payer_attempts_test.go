package nearby

import (
	"fmt"
	"testing"
	"time"

	"github.com/guggero/nearby-transfer/nearbytest"
	"github.com/guggero/nearby-transfer/wire"
	"github.com/stretchr/testify/require"
)

// TestPayerCodeAttemptLimit checks that a payer counts every session in
// which it revealed its nonce as a code attempt, whatever came of it: after
// MaxCodeAttemptsPerWindow of them it warns, then connects to nobody until
// the window passed, however many sharers are in range.
func TestPayerCodeAttemptLimit(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	payer := newNodeWithParams(t, world, "payer", Params{
		CandidateTTL:    time.Hour,
		FindGiveUpAfter: time.Hour,
	})
	limit := payer.params.MaxCodeAttemptsPerWindow

	// One more rogue than the budget allows, each dropping the link
	// once it learned the code, strongest first.
	for i := range limit + 1 {
		id := fmt.Sprintf("rogue-%d", i)
		newRoguePayee(t, world, id, 0)
		world.SetRSSI("payer", id, -40-i)
	}

	find := startFind(t, world, payer)
	for i := range limit {
		nextAs[Connecting](t, find)
		if i == payer.params.SuspiciousThreshold-1 {
			warning := nextAs[SuspiciousActivity](t, find)
			require.Equal(t, i+1, warning.Sessions)
		}
		require.Equal(
			t, FailureConnectFailed,
			nextAs[SessionFailed](t, find).Reason,
		)
		payer.advance(time.Second)
		payer.nextWindow(t)
	}

	// The last rogue is a candidate, but the budget is used up: the
	// find waits for the oldest attempt to age out, then connects.
	now := payer.clock.Now()
	free := payer.mgr.attempts.nextFree(now)
	require.True(t, free.After(now))
	payer.waitTick(t, free.Sub(now))
	payer.advance(free.Sub(now))
	nextAs[Connecting](t, find)
	nextAs[SessionFailed](t, find)
}

// TestBusyAfterNonceIsFailure checks that a sharer answering BUSY once the
// payer revealed its nonce is a failed sharer, not a busy one: it is not
// retried, and the find moves on to the next candidate.
func TestBusyAfterNonceIsFailure(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	payer := newNode(t, world, "payer")
	rogue := newRoguePayee(t, world, "rogue", wire.AbortBusy)
	payee := newNode(t, world, "payee")
	world.SetRSSI("payer", "rogue", -40)
	world.SetRSSI("payer", "payee", -60)
	expectStarted(t, payee.share(t, testRequest, false), true, false)

	find := startFind(t, world, payer)
	nextAs[Connecting](t, find)
	require.Equal(
		t, FailureConnectFailed, nextAs[SessionFailed](t, find).Reason,
	)

	// The genuine payee is next, even though the rogue is closer and
	// would be retried by now had it been busy.
	payer.advance(payer.params.BusyRetryDelay)
	world.Advertise()
	payer.nextWindow(t)
	nextAs[Connecting](t, find)
	require.Equal(t, "payee", nextAs[Received](t, find).PeerID)
	require.Equal(t, 1, rogue.codesSeen())
}
