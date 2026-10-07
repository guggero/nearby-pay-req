package nearby

import (
	"crypto/rand"
	"testing"

	"github.com/guggero/nearby-pay-req/nearbytest"
	"github.com/guggero/nearby-pay-req/session"
	"github.com/guggero/nearby-pay-req/wire"
	"github.com/stretchr/testify/require"
)

// tryExchange runs a whole session for a payer on central like exchange,
// but reports false instead of failing when the payee turns the payer away
// as busy.
func (d *directSharer) tryExchange(t *testing.T,
	central string) (session.Output, bool) {

	t.Helper()

	p, err := session.NewPayerSession(session.Config{
		Prologue: session.Prologue(serviceUUIDBytes()),
		Rand:     rand.Reader,
	})
	require.NoError(t, err)

	d.connect(t, central)
	out, err := p.Start()
	require.NoError(t, err)
	d.deliver(t, central, out.Send[0])
	out, err = p.Handle(d.reply(t))
	if session.ReasonOf(err) == session.ReasonBusy {
		return session.Output{}, false
	}
	require.NoError(t, err)
	d.deliver(t, central, out.Send[0])
	received, err := p.Handle(d.reply(t))
	require.NoError(t, err)
	ack, err := p.Accept()
	require.NoError(t, err)
	d.deliver(t, central, ack.Send[0])

	return received, true
}

// shownCode returns the code the payee shows now: that of the last session
// that reached its code.
func (d *directSharer) shownCode(t *testing.T) string {
	t.Helper()

	for i := len(d.events) - 1; i >= 0; i-- {
		if ev, ok := d.events[i].(PeerConnected); ok {
			return ev.Code
		}
	}
	require.FailNow(t, "no code shown")

	return ""
}

// permutations returns every ordering of steps.
func permutations[T any](steps []T) [][]T {
	if len(steps) <= 1 {
		return [][]T{append([]T(nil), steps...)}
	}

	var out [][]T
	for i := range steps {
		rest := append(append([]T(nil), steps[:i]...), steps[i+1:]...)
		for _, p := range permutations(rest) {
			out = append(out, append([]T{steps[i]}, p...))
		}
	}

	return out
}

// TestTwoPayersTwoPayees plays two payers each trying to get the requests
// of the same two payees, in every order, within one comparison timeout.
// Whatever the order, every request a payer received still carries the
// code its payee shows: a later payer is turned away as busy instead of
// silently replacing the code the first one is comparing.
func TestTwoPayersTwoPayees(t *testing.T) {
	t.Parallel()

	type step struct{ payer, payee string }
	steps := []step{
		{"one", "alice"}, {"one", "bob"},
		{"two", "alice"}, {"two", "bob"},
	}
	for _, order := range permutations(steps) {
		payees := map[string]*directSharer{
			"alice": newDirectSharer(t, Params{}),
			"bob":   newDirectSharer(t, Params{}),
		}
		offers := make(map[step]string)
		for _, s := range order {
			for _, d := range payees {
				d.advance(d.params.MinSessionInterval)
			}
			received, ok := payees[s.payee].tryExchange(t, s.payer)
			if ok {
				offers[s] = received.Code
			}
		}

		// Each payee served exactly one payer, and that payer's
		// code is what it shows.
		require.Len(t, offers, 2, "%v", order)
		for s, code := range offers {
			require.Equal(
				t, payees[s.payee].shownCode(t), code,
				"%v", order,
			)
		}
	}
}

// TestComparisonHold checks how the hold on a delivered code ends: a
// CHOSEN for that session ends it at once, otherwise it expires with a
// ComparisonExpired after ComparisonTimeout, and either way the next payer
// is served again.
func TestComparisonHold(t *testing.T) {
	t.Parallel()

	d := newDirectSharer(t, Params{})
	d.continueAfterChosen = true

	first, ok := d.tryExchange(t, "one")
	require.True(t, ok)
	d.advance(d.params.MinSessionInterval)
	_, ok = d.tryExchange(t, "two")
	require.False(t, ok)

	// The first payer chose: the second one is served at once.
	conn := &centralConn{maxChunk: wire.MaxChunkSize}
	d.conns["one"] = conn
	require.NoError(t, d.handleChosen("one", conn, wire.Message{
		Version: wire.Version1,
		Type:    wire.TypeChosen,
		Body:    first.ChosenToken[:],
	}))
	require.Equal(t, wire.EncodeChosenAck(), d.reply(t))
	second, ok := d.tryExchange(t, "two")
	require.True(t, ok)

	// Nobody chooses the second one: the hold expires.
	d.advance(d.params.MinSessionInterval)
	_, ok = d.tryExchange(t, "three")
	require.False(t, ok)
	d.advance(d.params.ComparisonTimeout)
	require.NoError(t, d.housekeep())
	require.Equal(
		t, ComparisonExpired{Code: second.Code},
		d.events[len(d.events)-1],
	)
	_, ok = d.tryExchange(t, "three")
	require.True(t, ok)
}

// TestHoldThenRetry checks the payer's side of a hold: a second payer is
// turned away as busy while the first one's code is held, and gets the
// request by retrying once the hold expired.
func TestHoldThenRetry(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	payee := newNode(t, world, "payee")
	first := newNode(t, world, "first")
	second := newNode(t, world, "second")

	share := payee.share(t, testRequest, false)
	expectStarted(t, share, true, false)
	receiveFrom(t, world, first)
	nextAs[PeerConnected](t, share)
	nextAs[Delivered](t, share)

	payee.advance(payee.params.MinSessionInterval)
	find := startFind(t, world, second)
	nextAs[Connecting](t, find)
	require.Equal(
		t, FailurePeerBusy, nextAs[SessionFailed](t, find).Reason,
	)

	payee.waitTick(t, payee.params.ComparisonTimeout)
	payee.advance(payee.params.ComparisonTimeout)
	nextAs[ComparisonExpired](t, share)
	second.advance(second.params.BusyRetryDelay)
	world.Advertise()
	second.nextWindow(t)
	nextAs[Connecting](t, find)
	nextAs[Received](t, find)
}
