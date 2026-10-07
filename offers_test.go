package nearby

import (
	"testing"
	"time"

	"github.com/guggero/nearby-pay-req/nearbytest"
	"github.com/stretchr/testify/require"
)

// TestOfferBudget checks that a payer holds at most MaxOffers requests for
// confirmation within one ComparisonTimeout: a collecting find that used up
// its budget connects to nobody else until its offers expire, reports each
// expiry, and may then visit the same sharers again.
func TestOfferBudget(t *testing.T) {
	t.Parallel()

	// Candidates outlive the wait, so the find needs no fresh sighting
	// once it has room again.
	world := nearbytest.NewWorld()
	payer := newNodeWithParams(t, world, "payer", Params{
		MaxOffers:    2,
		CandidateTTL: time.Minute,
	})
	for i, id := range []string{"alice", "bob", "carol"} {
		payee := newNode(t, world, id)
		world.SetRSSI("payer", id, -40-10*i)
		expectStarted(
			t, payee.share(t, testRequest, false), true, false,
		)
	}

	find := startFindWith(t, world, payer, FindOptions{Collect: true})
	nextAs[Connecting](t, find)
	alice := nextAs[Received](t, find)
	require.Equal(t, "alice", alice.PeerID)
	require.Equal(
		t, payer.clock.Now().Add(payer.params.ComparisonTimeout),
		alice.ExpiresAt,
	)
	payer.nextWindow(t)
	nextAs[Connecting](t, find)
	require.Equal(t, "bob", nextAs[Received](t, find).PeerID)

	// Carol is in range, but the budget is used up: the find waits
	// for Alice's offer to expire instead of connecting.
	world.Advertise()
	payer.nextWindow(t)
	payer.waitTick(t, alice.ExpiresAt.Sub(payer.clock.Now()))

	// Both offers expire, and the find goes on with Carol. The expiries
	// and the connection may come in either order.
	payer.advance(payer.params.ComparisonTimeout)
	expired := make(map[string]bool)
	for {
		ev := find.next(t)
		if offer, ok := ev.(OfferExpired); ok {
			expired[offer.PeerID] = true
			continue
		}
		if received, ok := ev.(Received); ok {
			require.Equal(t, "carol", received.PeerID)
			break
		}
		require.IsType(t, Connecting{}, ev)
	}
	require.Equal(t, map[string]bool{"alice": true, "bob": true}, expired)
}

// TestOfferBudgetAcrossFinds checks that restarting the search does not
// reset the budget: it belongs to the Manager, not to one find.
func TestOfferBudgetAcrossFinds(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	payer := newNodeWithParams(t, world, "payer", Params{
		MaxOffers:       1,
		CandidateTTL:    time.Minute,
		FindGiveUpAfter: time.Hour,
	})
	alice := newNode(t, world, "alice")
	bob := newNode(t, world, "bob")
	world.SetRSSI("payer", "bob", -60)
	expectStarted(t, alice.share(t, testRequest, false), true, false)
	expectStarted(t, bob.share(t, testRequest, false), true, false)

	first := receiveFrom(t, world, payer)
	require.Equal(t, "alice", first.PeerID)

	// The user says the codes differ and searches again, skipping
	// Alice: Bob is only tried once the first offer expired.
	find := startFind(t, world, payer, "alice")
	payer.waitTick(t, first.ExpiresAt.Sub(payer.clock.Now()))
	payer.advance(payer.params.ComparisonTimeout)
	nextAs[Connecting](t, find)
	require.Equal(t, "bob", nextAs[Received](t, find).PeerID)
}
