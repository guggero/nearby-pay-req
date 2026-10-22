package nearby

import (
	"crypto/rand"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/guggero/nearby-transfer/nearbytest"
	"github.com/guggero/nearby-transfer/radio"
	"github.com/guggero/nearby-transfer/session"
	"github.com/guggero/nearby-transfer/wire"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// firstChunks returns a fresh payer's opening message split into chunks of
// the smallest size, so it takes several writes.
func firstChunks(t *testing.T) [][]byte {
	t.Helper()

	sess, err := session.NewPayerSession(session.Config{
		Prologue: session.Prologue(serviceUUIDBytes()),
		Rand:     rand.Reader,
	})
	require.NoError(t, err)
	out, err := sess.Start()
	require.NoError(t, err)
	chunks, err := wire.Chunk(out.Send[0], wire.MinChunkSize)
	require.NoError(t, err)
	require.Greater(t, len(chunks), 1)

	return chunks
}

// expectDropped waits for the rogue's link to the payee to go down.
func (r *rogue) expectDropped(t *testing.T) {
	t.Helper()

	ev := r.next(t)
	require.Equalf(t, evDisconnected, ev.kind, "got %+v", ev)
}

// TestFirstMessageDeadline checks that a payer has FirstMessageTimeout from
// its subscription to send its first complete message: a silent payer and
// one that sent only part of it are dropped, what arrives afterwards
// starts nothing, and fragments do not extend the deadline.
func TestFirstMessageDeadline(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	payee := newNode(t, world, "payee")
	r := newRogue(t, world, "rogue")
	r.radio.SetMaxChunk(wire.MinChunkSize)
	timeout := payee.params.FirstMessageTimeout

	share := payee.share(t, testRequest, false)
	expectStarted(t, share, true, false)

	// Silent.
	r.connect(t, "payee")
	payee.waitTick(t, timeout)
	payee.advance(timeout)
	r.expectDropped(t)

	// One fragment, then the deadline; the rest arrives too late and
	// is never answered.
	r.connect(t, "payee")
	chunks := firstChunks(t)
	require.NoError(t, r.radio.Write("payee", r.connID, chunks[0], 1000))
	payee.waitTick(t, timeout)
	payee.advance(timeout)
	r.expectDropped(t)
	for _, c := range chunks[1:] {
		require.ErrorIs(
			t, r.radio.Write("payee", r.connID, c, 1000),
			nearbytest.ErrNotConnected,
		)
	}

	// Fragments trickling in up to just before the deadline still
	// make it: the session starts and the payee answers.
	r.connect(t, "payee")
	payee.waitTick(t, timeout)
	for _, c := range chunks[:len(chunks)-1] {
		require.NoError(t, r.radio.Write("payee", r.connID, c, 1000))
	}
	payee.advance(timeout - 1)
	require.NoError(t, r.radio.Write(
		"payee", r.connID, chunks[len(chunks)-1], 1000,
	))
	msg, err := wire.DecodeMessage(r.receive(t))
	require.NoError(t, err)
	require.Equal(t, wire.TypeNoiseEEE, msg.Type)
}

// TestMaxCentrals checks that a payee keeps at most MaxCentrals payers
// waiting without a session, dropping any further one as it subscribes.
func TestMaxCentrals(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	payee := newNode(t, world, "payee")
	share := payee.share(t, testRequest, false)
	expectStarted(t, share, true, false)

	for i := range payee.params.MaxCentrals {
		r := newRogue(t, world, fmt.Sprintf("rogue-%d", i))
		r.connect(t, "payee")
	}

	// The fake reports the subscription synchronously, but the payee
	// handles it in its own loop: the one over the cap is dropped.
	extra := newRogue(t, world, "extra")
	extra.connect(t, "payee")
	extra.expectDropped(t)
}

// subscriptionRadio hands a test the peripheral callback the Manager
// registers, so it can report a central subscribing a second time.
type subscriptionRadio struct {
	*nearbytest.Radio

	mu sync.Mutex
	cb radio.PeripheralCallback
}

// StartAdvertising implements radio.Radio.
func (r *subscriptionRadio) StartAdvertising(service, rx, tx string,
	cb radio.PeripheralCallback) error {

	r.mu.Lock()
	r.cb = cb
	r.mu.Unlock()

	return r.Radio.StartAdvertising(service, rx, tx, cb)
}

// resubscribe reports the central as ready once more, with maxChunk.
func (r *subscriptionRadio) resubscribe(central string, maxChunk int) {
	r.mu.Lock()
	cb := r.cb
	r.mu.Unlock()

	cb.OnCentralReady(central, maxChunk)
}

// TestResubscribeKeepsDeadline checks that a payer enabling notifications
// again just before its first-message deadline is dropped at that deadline
// all the same, and that a payer mid-session doing so keeps its session.
func TestResubscribeKeepsDeadline(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	var native *subscriptionRadio
	payee := newNodeWithRadio(
		t, world, "payee", Params{},
		func(r *nearbytest.Radio, _ *clock.TestClock) radio.Radio {
			native = &subscriptionRadio{Radio: r}
			return native
		},
	)
	r := newRogue(t, world, "rogue")
	timeout := payee.params.FirstMessageTimeout

	share := payee.share(t, testRequest, false)
	expectStarted(t, share, true, false)

	// Silent, re-subscribing just before the deadline: dropped on it.
	r.connect(t, "payee")
	payee.waitTick(t, timeout)
	payee.advance(timeout - time.Millisecond)
	native.resubscribe("rogue", nearbytest.DefaultMaxChunk)
	payee.advance(time.Millisecond)
	r.expectDropped(t)

	// Mid-session, with a message half received and a smaller chunk
	// size: the session goes on, with the new chunk size.
	r.connect(t, "payee")
	sess, reply := r.openSession(t, "payee")
	out, err := sess.Handle(reply)
	require.NoError(t, err)
	r.radio.SetMaxChunk(wire.MinChunkSize)
	chunks, err := wire.Chunk(out.Send[0], wire.MinChunkSize)
	require.NoError(t, err)
	require.NoError(t, r.radio.Write("payee", r.connID, chunks[0], 1000))
	native.resubscribe("rogue", wire.MinChunkSize)
	for _, c := range chunks[1:] {
		require.NoError(t, r.radio.Write("payee", r.connID, c, 1000))
	}
	r.chunk = wire.MinChunkSize
	out, err = sess.Handle(r.receive(t))
	require.NoError(t, err)
	require.NotEmpty(t, out.Code)
}
