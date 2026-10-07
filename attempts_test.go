package nearby

import (
	"fmt"
	"testing"

	"github.com/guggero/nearby-pay-req/wire"
	"github.com/stretchr/testify/require"
)

// choose sends the CHOSEN of a session's token from central.
func (d *directSharer) choose(t *testing.T, central string,
	token *[wire.ChosenTokenLen]byte) {

	t.Helper()

	conn := &centralConn{maxChunk: wire.MaxChunkSize}
	d.conns[central] = conn
	require.NoError(t, d.handleChosen(central, conn, wire.Message{
		Version: wire.Version1,
		Type:    wire.TypeChosen,
		Body:    token[:],
	}))
	d.radio.chunks = nil
}

// countEvents counts the events of type T the sharer emitted.
func countEvents[T ShareEvent](d *directSharer) int {
	n := 0
	for _, ev := range d.events {
		if _, ok := ev.(T); ok {
			n++
		}
	}

	return n
}

// TestAcknowledgedAttemptsCount checks that sessions whose payer
// acknowledged the request count as code attempts like failed ones do: a
// man in the middle that acknowledges, and even chooses, every attempt to
// release the hold still raises the warning.
func TestAcknowledgedAttemptsCount(t *testing.T) {
	t.Parallel()

	d := newDirectSharer(t, Params{})
	d.continueAfterChosen = true
	for i := range defaultSuspiciousThreshold {
		central := fmt.Sprintf("attacker-%d", i)
		received, ok := d.tryExchange(t, central)
		require.True(t, ok)
		d.choose(t, central, received.ChosenToken)
		d.advance(d.params.MinSessionInterval)
		require.NoError(t, d.reviewAttempts())
	}

	require.Equal(t, 1, countEvents[SuspiciousActivity](d))
	require.Equal(t, SuspiciousActivity{
		Sessions: defaultSuspiciousThreshold,
		Window:   d.params.SuspiciousWindow,
	}, d.events[len(d.events)-1])
}

// TestCodeAttemptLimits checks the two caps on sessions that reach their
// code: MaxCodeAttemptsPerWindow turns further payers away as busy within
// the window, and MaxCodeAttempts ends BLE serving for the whole share.
func TestCodeAttemptLimits(t *testing.T) {
	t.Parallel()

	// Rejected requests are not held, so only the caps get in the way.
	d := newDirectSharer(t, Params{})
	for i := range d.params.MaxCodeAttemptsPerWindow {
		d.exchange(t, fmt.Sprintf("payer-%d", i), false)
		d.advance(d.params.MinSessionInterval)
	}
	_, ok := d.tryExchange(t, "one-too-many")
	require.False(t, ok)
	d.advance(d.params.SessionRateWindow)
	_, ok = d.tryExchange(t, "next-window")
	require.True(t, ok)

	// A share whose budget is three attempts stops serving over BLE
	// after the third, for good.
	d = newDirectSharer(t, Params{MaxCodeAttempts: 3})
	for i := range 3 {
		d.exchange(t, fmt.Sprintf("payer-%d", i), false)
		d.advance(d.params.SessionRateWindow)
		require.NoError(t, d.reviewAttempts())
	}
	require.True(t, d.exhausted)
	require.Equal(t, []ShareEvent{
		AttemptLimitReached{Attempts: 3},
		ShareStarted{},
	}, d.events[len(d.events)-2:])
	_, ok = d.tryExchange(t, "after")
	require.False(t, ok)
}
