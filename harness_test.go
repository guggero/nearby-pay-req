package nearby

import (
	"context"
	"crypto/rand"
	"errors"
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

const (
	// testTimeout bounds every wait for an event; the fake radio is
	// synchronous, so anything slower is a hang.
	testTimeout = 5 * time.Second

	// testRequest is a payment request validateTestRequest accepts.
	testRequest = "bitcoin:bcrt1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq"
)

// node is one phone: a fake radio, its own test clock and a Manager.
type node struct {
	id    string
	radio *nearbytest.Radio
	clock *clock.TestClock
	ticks chan time.Duration
	mgr   *Manager
}

// newNode adds a phone to world.
func newNode(t *testing.T, world *nearbytest.World, id string) *node {
	t.Helper()

	// A large buffer keeps TickAfter from blocking when a test isn't
	// waiting for the tick.
	ticks := make(chan time.Duration, 1024)
	clk := clock.NewTestClockWithTickSignal(
		time.Unix(1_700_000_000, 0), ticks,
	)
	fake := world.NewRadio(id)
	mgr := New(Config{Radio: fake, Clock: clk})
	t.Cleanup(mgr.Stop)

	return &node{id: id, radio: fake, clock: clk, ticks: ticks, mgr: mgr}
}

// waitTick waits until a ticker of duration d was registered.
func (n *node) waitTick(t *testing.T, d time.Duration) {
	t.Helper()

	timeout := time.After(testTimeout)
	for {
		select {
		case got := <-n.ticks:
			if got == d {
				return
			}

		case <-timeout:
			t.Fatalf("no %v ticker registered on %s", d, n.id)
		}
	}
}

// advance moves the node's clock forward.
func (n *node) advance(d time.Duration) {
	n.clock.SetTime(n.clock.Now().Add(d))
}

// call is one running share or find.
type call[E any] struct {
	events chan E
	cancel context.CancelFunc
	result chan error
}

// next returns the next event of the call.
func (c *call[E]) next(t *testing.T) E {
	t.Helper()

	select {
	case ev := <-c.events:
		return ev

	case err := <-c.result:
		t.Fatalf("call ended early: %v", err)

	case <-time.After(testTimeout):
		t.Fatalf("no event within %v", testTimeout)
	}

	var zero E
	return zero
}

// end waits for the call to return.
func (c *call[E]) end(t *testing.T) error {
	t.Helper()

	select {
	case err := <-c.result:
		return err

	case <-time.After(testTimeout):
		t.Fatalf("call did not end within %v", testTimeout)
	}

	return nil
}

// startCall runs a share or find in the background, collecting what it
// emits.
func startCall[E any](t *testing.T,
	run func(context.Context, func(E) error) error) *call[E] {

	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	c := &call[E]{
		events: make(chan E, 64),
		cancel: cancel,
		result: make(chan error, 1),
	}
	emit := func(ev E) error {
		select {
		case c.events <- ev:
			return nil

		case <-ctx.Done():
			return ctx.Err()
		}
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.result <- run(ctx, emit)
	}()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	return c
}

// share starts a share on n.
func (n *node) share(t *testing.T, request string,
	nfc bool) *call[ShareEvent] {

	t.Helper()

	return startCall(
		t,
		func(ctx context.Context, emit func(ShareEvent) error) error {
			return n.mgr.Share(
				ctx, request, ShareOptions{NFC: nfc}, emit,
			)
		},
	)
}

// find starts a find on n.
func (n *node) find(t *testing.T, exclude ...string) *call[FindEvent] {
	t.Helper()

	return startCall(
		t, func(ctx context.Context, emit func(FindEvent) error) error {
			return n.mgr.Find(ctx, FindOptions{
				Exclude:  exclude,
				Validate: validateTestRequest,
			}, emit)
		},
	)
}

// validateTestRequest stands in for a wallet's payment parser: it accepts
// what looks like a BIP 21 URI and rejects everything else.
func validateTestRequest(request string) error {
	if !strings.HasPrefix(request, "bitcoin:") {
		return errors.New("not a bitcoin URI")
	}

	return nil
}

// rogue is a central driven by hand, for playing a misbehaving or
// concurrent payer against a sharer.
type rogue struct {
	radio  *nearbytest.Radio
	events chan event
	reasm  wire.Reassembler
	chunk  int
}

// rogueCallback forwards central callbacks to the rogue's channel.
type rogueCallback struct {
	events chan event
}

// OnAdvertisement implements radio.CentralCallback.
func (r *rogueCallback) OnAdvertisement(string, int) {}

// OnConnected implements radio.CentralCallback.
func (r *rogueCallback) OnConnected(id string, maxChunk int) {
	r.events <- event{kind: evConnected, id: id, maxChunk: maxChunk}
}

// OnNotify implements radio.CentralCallback.
func (r *rogueCallback) OnNotify(id string, chunk []byte) {
	r.events <- event{kind: evNotify, id: id, chunk: chunk}
}

// OnDisconnected implements radio.CentralCallback.
func (r *rogueCallback) OnDisconnected(id string, reason string) {
	r.events <- event{kind: evDisconnected, id: id, reason: reason}
}

// OnScanFailed implements radio.CentralCallback.
func (r *rogueCallback) OnScanFailed(string) {}

var _ radio.CentralCallback = (*rogueCallback)(nil)

// newRogue adds a hand-driven central to world.
func newRogue(t *testing.T, world *nearbytest.World, id string) *rogue {
	t.Helper()

	r := &rogue{radio: world.NewRadio(id), events: make(chan event, 64)}
	require.NoError(t, r.radio.StartScan(
		ServiceUUID, &rogueCallback{events: r.events},
	))

	return r
}

// connect links the rogue to a sharer.
func (r *rogue) connect(t *testing.T, peer string) {
	t.Helper()

	require.NoError(t, r.radio.Connect(peer, "", "", ""))

	// Skip the disconnect of a previous link the rogue dropped itself.
	ev := r.next(t)
	for ev.kind == evDisconnected {
		ev = r.next(t)
	}
	require.Equal(t, evConnected, ev.kind)
	r.chunk = ev.maxChunk
	r.reasm = wire.Reassembler{}
}

// next returns the rogue's next radio event.
func (r *rogue) next(t *testing.T) event {
	t.Helper()

	select {
	case ev := <-r.events:
		return ev

	case <-time.After(testTimeout):
		t.Fatal("rogue got no event")
	}

	return event{}
}

// send writes one message to the sharer.
func (r *rogue) send(t *testing.T, peer string, msg []byte) {
	t.Helper()

	chunks, err := wire.Chunk(msg, r.chunk)
	require.NoError(t, err)
	for _, c := range chunks {
		require.NoError(t, r.radio.Write(peer, c))
	}
}

// receive reads one complete message from the sharer.
func (r *rogue) receive(t *testing.T) []byte {
	t.Helper()

	for {
		ev := r.next(t)
		require.Equalf(t, evNotify, ev.kind, "got %+v", ev)
		msg, err := r.reasm.Add(ev.chunk)
		require.NoError(t, err)
		if msg != nil {
			return msg
		}
	}
}

// openSession starts a payer session against peer and returns it together
// with the sharer's first reply: its commitment, or an ABORT if it turned
// the rogue away. Neither side knows a code at this point.
func (r *rogue) openSession(t *testing.T, peer string) (*session.PayerSession,
	[]byte) {

	t.Helper()

	sess, err := session.NewPayerSession(session.Config{
		Prologue: session.Prologue(serviceUUIDBytes()),
		Rand:     rand.Reader,
	})
	require.NoError(t, err)
	out, err := sess.Start()
	require.NoError(t, err)
	r.send(t, peer, out.Send[0])

	return sess, r.receive(t)
}

// runUntilCode runs a payer session against peer up to the point where
// both sides know the code, and returns the payer session.
func (r *rogue) runUntilCode(t *testing.T, peer string) *session.PayerSession {
	t.Helper()

	sess, reply := r.openSession(t, peer)
	out, err := sess.Handle(reply)
	require.NoError(t, err)
	r.send(t, peer, out.Send[0])

	out, err = sess.Handle(r.receive(t))
	require.NoError(t, err)
	require.NotEmpty(t, out.Code)

	return sess
}
