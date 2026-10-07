package nearby

import (
	"crypto/rand"
	"fmt"
	"testing"
	"time"

	"github.com/guggero/nearby-pay-req/radio"
	"github.com/guggero/nearby-pay-req/session"
	"github.com/guggero/nearby-pay-req/wire"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// recordingRadio records the notifications and disconnects of a sharer
// driven directly by a test, without any event loop. Methods a sharer does
// not use in these tests stay unimplemented.
type recordingRadio struct {
	radio.Radio

	chunks      [][]byte
	disconnects []string
}

// Notify implements radio.Radio.
func (r *recordingRadio) Notify(_ string, chunk []byte, _ int) error {
	r.chunks = append(r.chunks, chunk)
	return nil
}

// DisconnectCentral implements radio.Radio.
func (r *recordingRadio) DisconnectCentral(id string) {
	r.disconnects = append(r.disconnects, id)
}

// directSharer is a sharer whose events a test feeds by hand, for exact
// traces of what a payee does.
type directSharer struct {
	*sharer

	radio  *recordingRadio
	clock  *clock.TestClock
	events []ShareEvent
}

// newDirectSharer prepares a sharer of testRequest with the given
// parameters.
func newDirectSharer(t *testing.T, params Params) *directSharer {
	t.Helper()

	clk := clock.NewTestClock(time.Unix(1_700_000_000, 0))
	native := &recordingRadio{}
	d := &directSharer{radio: native, clock: clk}
	d.sharer = &sharer{
		ctx:            t.Context(),
		radio:          &radioMux{native: native},
		clock:          clk,
		rand:           rand.Reader,
		params:         params.withDefaults(),
		paymentRequest: testRequest,
		conns:          make(map[string]*centralConn),
		delivered: make(
			map[[wire.ChosenTokenLen]byte]deliveredSession,
		),
		send: func(ev ShareEvent) error {
			d.events = append(d.events, ev)
			return nil
		},
	}

	return d
}

// advance moves the sharer's clock forward.
func (d *directSharer) advance(dur time.Duration) {
	d.clock.SetTime(d.clock.Now().Add(dur))
}

// deliver feeds one whole payer message to the sharer.
func (d *directSharer) deliver(t *testing.T, central string, msg []byte) {
	t.Helper()

	chunks, err := wire.Chunk(msg, wire.MaxChunkSize)
	require.NoError(t, err)
	for _, chunk := range chunks {
		require.NoError(t, d.handleWrite(central, chunk))
	}
}

// reply reassembles the one message the sharer notified since the last
// call.
func (d *directSharer) reply(t *testing.T) []byte {
	t.Helper()

	var (
		reasm    wire.Reassembler
		messages [][]byte
	)
	for _, chunk := range d.radio.chunks {
		msg, err := reasm.Add(chunk)
		require.NoError(t, err)
		if msg != nil {
			messages = append(messages, msg)
		}
	}
	d.radio.chunks = nil
	require.Len(t, messages, 1)

	return messages[0]
}

// connect subscribes a payer.
func (d *directSharer) connect(t *testing.T, central string) {
	t.Helper()

	require.NoError(t, d.handleEvent(event{
		kind:     evCentralReady,
		id:       central,
		maxChunk: wire.MaxChunkSize,
	}))
}

// exchange runs a whole session for a payer on central, accepting the
// request if accept is set and rejecting it otherwise, and returns what the
// payer received. It requires the session to be admitted.
func (d *directSharer) exchange(t *testing.T, central string,
	accept bool) session.Output {

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
	require.NoError(t, err)
	d.deliver(t, central, out.Send[0])
	received, err := p.Handle(d.reply(t))
	require.NoError(t, err)

	ackFn := p.Reject
	if accept {
		ackFn = p.Accept
	}
	ack, err := ackFn()
	require.NoError(t, err)
	d.deliver(t, central, ack.Send[0])

	return received
}

// TestDeliveredTokensBounded checks that a share nobody sends CHOSEN to
// forgets its delivered sessions' tokens at their deadline, and never keeps
// more than maxDeliveredSessions of them.
func TestDeliveredTokensBounded(t *testing.T) {
	t.Parallel()

	d := newDirectSharer(t, Params{})
	for i := range 5 {
		d.exchange(t, fmt.Sprintf("payer-%d", i), true)
		d.advance(d.params.ChosenRetention + time.Second)
	}

	// Inserting prunes, and the housekeeping at a token's deadline
	// prunes without any insertion at all.
	require.Len(t, d.delivered, 1)
	d.housekeep()
	require.Empty(t, d.delivered)

	// A burst within the retention keeps only the newest tokens.
	d = newDirectSharer(t, Params{})
	for i := range maxDeliveredSessions + 5 {
		d.rememberDelivered([wire.ChosenTokenLen]byte{byte(i)}, "x")
		d.advance(time.Millisecond)
	}
	require.Len(t, d.delivered, maxDeliveredSessions)
	_, ok := d.delivered[[wire.ChosenTokenLen]byte{0}]
	require.False(t, ok)
}

// TestFindStateBounded checks that a find tracks at most MaxPeersPerFind
// sharers however many identities show up, and that a sharer not seen for
// CandidateTTL is no longer picked.
func TestFindStateBounded(t *testing.T) {
	t.Parallel()

	clk := clock.NewTestClock(time.Unix(1_700_000_000, 0))
	var sent []FindEvent
	f := &finder{
		ctx:        t.Context(),
		clock:      clk,
		params:     DefaultParams(),
		scanning:   true,
		candidates: make(map[string]*candidate),
		known:      make(map[string]struct{}),
		failed:     make(map[string]bool),
		received:   make(map[string]bool),
		send: func(ev FindEvent) error {
			sent = append(sent, ev)
			return nil
		},
	}

	for i := range 5000 {
		_, err := f.handleEvent(event{
			kind: evAdvertisement,
			id:   fmt.Sprintf("peer-%d", i),
			rssi: -90,
		})
		require.NoError(t, err)
	}
	require.Len(t, f.known, f.params.MaxPeersPerFind)
	require.Len(t, f.candidates, f.params.MaxPeersPerFind)

	// All far away: TooFar. Once none was seen for CandidateTTL,
	// nothing is left to pick or report.
	require.NoError(t, f.pick())
	require.Equal(t, []FindEvent{TooFar{}}, sent)
	clk.SetTime(clk.Now().Add(f.params.CandidateTTL + time.Second))
	require.NoError(t, f.pick())
	require.Empty(t, f.candidates)
}
