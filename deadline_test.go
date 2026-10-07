package nearby

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guggero/nearby-pay-req/nearbytest"
	"github.com/guggero/nearby-pay-req/radio"
	"github.com/guggero/nearby-pay-req/session"
	"github.com/guggero/nearby-pay-req/wire"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// slowRadio is a fake radio whose every Notify and Write the stack accepts
// only after step has passed on the phone's clock: each call succeeds well
// within its own timeout, yet together they take far longer than a
// session may.
type slowRadio struct {
	*nearbytest.Radio

	clock *clock.TestClock
	step  time.Duration

	mu       sync.Mutex
	timeouts []int
}

// Notify implements radio.Radio.
func (s *slowRadio) Notify(central string, chunk []byte, timeout int) error {
	s.record(timeout)
	return s.Radio.Notify(central, chunk, timeout)
}

// Write implements radio.Radio.
func (s *slowRadio) Write(peer string, connID int, chunk []byte,
	timeout int) error {

	s.record(timeout)
	return s.Radio.Write(peer, connID, chunk, timeout)
}

// record notes the timeout of one call and lets step pass.
func (s *slowRadio) record(timeout int) {
	s.mu.Lock()
	s.timeouts = append(s.timeouts, timeout)
	s.mu.Unlock()

	s.clock.SetTime(s.clock.Now().Add(s.step))
}

// calls returns the timeouts of every call so far.
func (s *slowRadio) calls() []int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]int(nil), s.timeouts...)
}

// TestSlowNotificationsStopAtDeadline sends the largest request in the
// smallest chunks to a payer whose every notification takes a second. The
// payee gives up at the session timeout instead of sending all of the
// several hundred chunks, and never lets one native call wait past it.
func TestSlowNotificationsStopAtDeadline(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	var slow *slowRadio
	payee := newNodeWithRadio(
		t, world, "payee", Params{},
		func(r *nearbytest.Radio, clk *clock.TestClock) radio.Radio {
			slow = &slowRadio{
				Radio: r,
				clock: clk,
				step:  time.Second,
			}
			return slow
		},
	)
	r := newRogue(t, world, "rogue")
	r.radio.SetMaxChunk(wire.MinChunkSize)

	request := "bitcoin:" + strings.Repeat(
		"x", session.MaxPaymentRequestLen-len("bitcoin:"),
	)
	share := payee.share(t, request, false)
	expectStarted(t, share, true, false)

	r.connect(t, "payee")
	sess, reply := r.openSession(t, "payee")
	out, err := sess.Handle(reply)
	require.NoError(t, err)
	r.send(t, "payee", out.Send[0])

	require.Equal(
		t, FailureTimeout, nextAs[SessionFailed](t, share).Reason,
	)

	// Message 4 alone is over 450 chunks of 20 bytes. The payee sent
	// as many as fit in the ten seconds, plus the one-chunk ABORT, and
	// every call was given no more than what was left of the session.
	calls := slow.calls()
	limit := int(defaultSessionTimeout / time.Second)
	require.LessOrEqual(t, len(calls), limit+1)
	for i, timeout := range calls[:len(calls)-1] {
		require.LessOrEqual(
			t, timeout, int(defaultSessionTimeout.Milliseconds()),
		)
		if i > 0 {
			require.Less(t, timeout, calls[i-1])
		}
	}
	require.LessOrEqual(
		t, calls[len(calls)-1], int(abortSendTimeout.Milliseconds()),
	)
}

// TestSlowWritesStopAtDeadline is the payer's side: writes that each take
// four seconds stop at the session timeout, and the find moves on.
func TestSlowWritesStopAtDeadline(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	payee := newNode(t, world, "payee")
	payer := newNodeWithRadio(
		t, world, "payer", Params{},
		func(r *nearbytest.Radio, clk *clock.TestClock) radio.Radio {
			return &slowRadio{
				Radio: r,
				clock: clk,
				step:  4 * time.Second,
			}
		},
	)
	payer.radio.SetMaxChunk(wire.MinChunkSize)
	expectStarted(t, payee.share(t, testRequest, false), true, false)

	find := startFind(t, world, payer)
	nextAs[Connecting](t, find)
	require.Equal(
		t, FailureTimeout, nextAs[SessionFailed](t, find).Reason,
	)
}

// blockingRadio is a fake radio whose Notify blocks until the central it
// sends to is disconnected or its timeout passed in real time.
type blockingRadio struct {
	*nearbytest.Radio

	entered chan struct{}

	mu      sync.Mutex
	pending map[string]chan struct{}
}

// Notify implements radio.Radio.
func (b *blockingRadio) Notify(central string, _ []byte, timeout int) error {
	released := make(chan struct{})
	b.mu.Lock()
	b.pending[central] = released
	b.mu.Unlock()
	b.entered <- struct{}{}

	select {
	case <-released:
		return nearbytest.ErrNotConnected

	case <-time.After(time.Duration(timeout) * time.Millisecond):
		return nearbytest.ErrNotConnected
	}
}

// DisconnectCentral implements radio.Radio: it releases a blocked Notify to
// the central, as the contract requires.
func (b *blockingRadio) DisconnectCentral(central string) {
	b.mu.Lock()
	if released, ok := b.pending[central]; ok {
		close(released)
		delete(b.pending, central)
	}
	b.mu.Unlock()

	b.Radio.DisconnectCentral(central)
}

// TestCancelInterruptsNotify checks that a share cancelled while the native
// stack holds one of its notifications ends at once: the Manager closes the
// link the notification waits on instead of waiting out its timeout, which
// here is longer than the test allows.
func TestCancelInterruptsNotify(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	var blocking *blockingRadio
	payee := newNodeWithRadio(
		t, world, "payee", Params{},
		func(r *nearbytest.Radio, _ *clock.TestClock) radio.Radio {
			blocking = &blockingRadio{
				Radio:   r,
				entered: make(chan struct{}, 1),
				pending: make(map[string]chan struct{}),
			}
			return blocking
		},
	)
	r := newRogue(t, world, "rogue")

	share := payee.share(t, testRequest, false)
	expectStarted(t, share, true, false)

	// The commitment the payee answers with never leaves the stack.
	r.connect(t, "payee")
	sess, err := session.NewPayerSession(session.Config{
		Prologue: session.Prologue(serviceUUIDBytes()),
	})
	require.NoError(t, err)
	out, err := sess.Start()
	require.NoError(t, err)
	r.send(t, "payee", out.Send[0])
	select {
	case <-blocking.entered:

	case <-time.After(testTimeout):
		t.Fatal("payee never notified")
	}

	share.cancel()
	start := time.Now()
	require.ErrorIs(t, share.end(t), context.Canceled)
	require.Less(t, time.Since(start), time.Second)
}
