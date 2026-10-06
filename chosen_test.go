package nearby

import (
	"testing"
	"time"

	"github.com/guggero/nearby-pay-req/nearbytest"
	"github.com/guggero/nearby-pay-req/wire"
	"github.com/stretchr/testify/require"
)

// receiveFrom runs a find on payer up to the Received from the one sharer
// in range.
func receiveFrom(t *testing.T, world *nearbytest.World, payer *node) Received {
	t.Helper()

	find := startFind(t, world, payer)
	nextAs[Connecting](t, find)
	received := nextAs[Received](t, find)
	require.NoError(t, find.end(t))

	return received
}

// TestChosen checks that a payer choosing a delivered request makes the
// payee report Chosen and stop sharing.
func TestChosen(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	payee := newNode(t, world, "payee")
	payer := newNode(t, world, "payer")

	share := payee.share(t, testRequest, false)
	expectStarted(t, share, true, false)

	received := receiveFrom(t, world, payer)
	require.Len(t, received.ChosenToken, wire.ChosenTokenLen)
	nextAs[PeerConnected](t, share)
	nextAs[Delivered](t, share)
	require.True(t, payee.radio.Advertising())

	err := payer.mgr.Choose(
		t.Context(), received.PeerID, received.ChosenToken,
	)
	require.NoError(t, err)

	chosen := nextAs[Chosen](t, share)
	require.Equal(t, received.Code, chosen.Code)
	require.NoError(t, share.end(t))
	require.False(t, payee.radio.Advertising())
}

// TestChosenContinue checks that a share told to continue after a choice
// reports it and keeps serving, and that a token is only good once.
func TestChosenContinue(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	payee := newNode(t, world, "payee")
	payer := newNode(t, world, "payer")

	share := payee.shareWith(t, testRequest, ShareOptions{
		ContinueAfterChosen: true,
	})
	expectStarted(t, share, true, false)

	received := receiveFrom(t, world, payer)
	nextAs[PeerConnected](t, share)
	nextAs[Delivered](t, share)

	require.NoError(t, payer.mgr.Choose(
		t.Context(), received.PeerID, received.ChosenToken,
	))
	nextAs[Chosen](t, share)
	require.True(t, payee.radio.Advertising())

	err := payer.mgr.Choose(
		t.Context(), received.PeerID, received.ChosenToken,
	)
	require.ErrorIs(t, err, ErrUnknownSession)
}

// TestChooseRejected checks the tokens a payee refuses: one it never
// handed out, one that expired, and one that is not a token at all.
func TestChooseRejected(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	payee := newNode(t, world, "payee")
	payer := newNode(t, world, "payer")

	share := payee.share(t, testRequest, false)
	expectStarted(t, share, true, false)

	// Never handed out.
	err := payer.mgr.Choose(
		t.Context(), "payee", make([]byte, wire.ChosenTokenLen),
	)
	require.ErrorIs(t, err, ErrUnknownSession)

	// Not a token.
	err = payer.mgr.Choose(t.Context(), "payee", []byte{1, 2, 3})
	require.ErrorIs(t, err, ErrInvalidChosenToken)

	// Expired: the payee forgets deliveries after ChosenRetention.
	received := receiveFrom(t, world, payer)
	nextAs[PeerConnected](t, share)
	nextAs[Delivered](t, share)
	payee.advance(payee.params.ChosenRetention + time.Second)
	err = payer.mgr.Choose(
		t.Context(), received.PeerID, received.ChosenToken,
	)
	require.ErrorIs(t, err, ErrUnknownSession)
	require.True(t, payee.radio.Advertising())
}

// TestCollectAndChoose plays two payments in one room: a payer collects the
// requests of both payees, its user picks one by its code, and only that
// payee stops sharing, so the other one is still there for its own payer.
func TestCollectAndChoose(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	alice := newNode(t, world, "alice")
	bob := newNode(t, world, "bob")
	payer := newNode(t, world, "payer")
	world.SetRSSI("payer", "alice", -40)
	world.SetRSSI("payer", "bob", -60)

	aliceShare := alice.share(t, testRequest+"?amount=1", false)
	expectStarted(t, aliceShare, true, false)
	bobShare := bob.share(t, testRequest+"?amount=2", false)
	expectStarted(t, bobShare, true, false)

	// Both requests arrive, the closer one first, and the find keeps
	// going.
	find := startFindWith(t, world, payer, FindOptions{Collect: true})
	nextAs[Connecting](t, find)
	first := nextAs[Received](t, find)
	require.Equal(t, "alice", first.PeerID)

	payer.nextWindow(t)
	nextAs[Connecting](t, find)
	second := nextAs[Received](t, find)
	require.Equal(t, "bob", second.PeerID)
	require.NotEqual(t, first.ChosenToken, second.ChosenToken)

	// Nobody new in range: further advertisements change nothing.
	world.Advertise()
	find.cancel()
	require.Error(t, find.end(t))

	// The user's payee is Bob. Waiting for his Delivered first means
	// he finished with the first link before the payer comes back.
	nextAs[PeerConnected](t, bobShare)
	nextAs[Delivered](t, bobShare)
	require.NoError(t, payer.mgr.Choose(
		t.Context(), second.PeerID, second.ChosenToken,
	))
	require.Equal(t, second.Code, nextAs[Chosen](t, bobShare).Code)
	require.NoError(t, bobShare.end(t))
	require.False(t, bob.radio.Advertising())

	// Alice delivered too, but nobody chose her request: she keeps
	// sharing for her own payer.
	nextAs[PeerConnected](t, aliceShare)
	nextAs[Delivered](t, aliceShare)
	require.True(t, alice.radio.Advertising())
}

// TestBusyRetry checks that a payer turned away because the payee serves
// someone else tries again after BusyRetryDelay, instead of skipping the
// payee for the rest of the find.
func TestBusyRetry(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	payee := newNode(t, world, "payee")
	payer := newNode(t, world, "payer")
	other := newRogue(t, world, "other")

	share := payee.share(t, testRequest, false)
	expectStarted(t, share, true, false)

	// Another payer holds the payee.
	other.connect(t, "payee")
	other.openSession(t, "payee")

	find := startFind(t, world, payer)
	nextAs[Connecting](t, find)
	require.Equal(
		t, FailurePeerBusy, nextAs[SessionFailed](t, find).Reason,
	)

	// Too early: the payee is not picked while the retry delay runs.
	world.Advertise()

	// The other payer leaves; once the delay passed the payee is
	// picked again and delivers.
	other.radio.Disconnect("payee")
	payee.advance(payee.params.MinSessionInterval)
	payer.advance(payer.params.BusyRetryDelay)
	world.Advertise()
	payer.nextWindow(t)
	nextAs[Connecting](t, find)
	require.Equal(t, testRequest, nextAs[Received](t, find).PaymentRequest)
}

// TestCustomParams checks that a Manager uses the parameters it was given:
// a longer collection window and a floor that lets far sharers be picked.
func TestCustomParams(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	payee := newNode(t, world, "payee")
	payer := newNodeWithParams(t, world, "payer", Params{
		CollectWindow: 3 * time.Second,
		RSSIFloor:     -127,
	})
	world.SetRSSI("payer", "payee", -95)
	require.Equal(t, 3*time.Second, payer.params.CollectWindow)

	expectStarted(t, payee.share(t, testRequest, false), true, false)

	// startFind waits for a 3 s window; with the default floor the
	// sharer would be reported as too far.
	received := receiveFrom(t, world, payer)
	require.Equal(t, "payee", received.PeerID)
}

// TestParamsDefaults checks which values fall back to the defaults.
func TestParamsDefaults(t *testing.T) {
	t.Parallel()

	require.Equal(t, DefaultParams(), Params{}.withDefaults())

	p := Params{
		SessionTimeout:       -time.Second,
		MaxSessionsPerWindow: -1,
		RSSIFloor:            -90,
		RSSISmoothing:        1.5,
		BusyRetryDelay:       250 * time.Millisecond,
	}.withDefaults()
	require.Equal(t, defaultSessionTimeout, p.SessionTimeout)
	require.Equal(t, defaultMaxSessionsPerWindow, p.MaxSessionsPerWindow)
	require.Equal(t, -90, p.RSSIFloor)
	require.InDelta(t, defaultRSSISmoothing, p.RSSISmoothing, 0)
	require.Equal(t, 250*time.Millisecond, p.BusyRetryDelay)
}
