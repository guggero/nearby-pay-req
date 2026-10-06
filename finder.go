package nearby

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/guggero/nearby-pay-req/session"
	"github.com/guggero/nearby-pay-req/wire"
	"github.com/lightningnetwork/lnd/clock"
)

var (
	// ErrScanFailed ends a find whose native scan stopped. Restarting
	// the find usually works.
	ErrScanFailed = errors.New("nearby scan failed")
)

// candidate is one sharer seen while scanning.
type candidate struct {
	rssi float64
}

// payerRun is the one session the payer runs at a time.
type payerRun struct {
	peer      string
	sess      *session.PayerSession
	maxChunk  int
	reasm     wire.Reassembler
	deadline  <-chan time.Time
	connected bool
}

// finder searches for one payment request until it receives one or its
// context ends.
type finder struct {
	radio *radioMux
	clock clock.Clock
	rand  io.Reader

	exclude  map[string]bool
	validate func(string) error

	availability  func() Availability
	statusChanged <-chan struct{}

	send func(FindEvent) error

	events sink

	lastAvailability Availability
	scanning         bool
	candidates       map[string]*candidate
	failed           map[string]bool
	window           <-chan time.Time
	giveUp           <-chan time.Time
	tooFarSent       bool
	active           *payerRun
}

// run searches until a payment request was received (nil), ctx ends, the
// stream breaks or the scan fails.
func (f *finder) run(ctx context.Context) error {
	f.events = make(sink, eventQueueLen)
	f.candidates = make(map[string]*candidate)
	f.failed = make(map[string]bool)
	defer f.stopScan(false)

	if err := f.refresh(true); err != nil {
		return err
	}

	for {
		var deadline <-chan time.Time
		if f.active != nil {
			deadline = f.active.deadline
		}

		select {
		case <-ctx.Done():
			return ctx.Err()

		// A radio callback; done reports a received request.
		case ev := <-f.events:
			done, err := f.handleEvent(ev)
			if err != nil || done {
				return err
			}

		// The collection window closed: pick a sharer.
		case <-f.window:
			f.window = nil
			if err := f.pick(); err != nil {
				return err
			}

		// Nothing worked for a while. Say so, but keep looking.
		case <-f.giveUp:
			f.giveUp = nil
			err := f.sendFailed(
				FailureTimeout,
			)
			if err != nil {
				return err
			}

		// Connecting or the session took too long.
		case <-deadline:
			f.write(wire.EncodeAbort(wire.AbortTimeout))
			err := f.failActive(
				FailureTimeout,
			)
			if err != nil {
				return err
			}

		case <-f.statusChanged:
			if err := f.refresh(false); err != nil {
				return err
			}
		}
	}
}

// refresh starts or stops scanning to match the current availability.
func (f *finder) refresh(initial bool) error {
	avail := f.availability()
	changed := avail != f.lastAvailability
	f.lastAvailability = avail

	switch {
	// Available: start scanning and the give-up timer.
	case avail == Available &&
		!f.scanning:

		if err := f.radio.startScan(f.events); err != nil {
			log.Warnf("Starting nearby scan failed: %v", err)
			return ErrScanFailed
		}
		f.scanning = true
		f.giveUp = f.clock.TickAfter(findGiveUpAfter)

		return f.send(Scanning{})

	// No longer available: stop and report. The OS already ended the
	// scan along with the radio.
	case avail != Available &&
		f.scanning:

		f.stopScan(true)
		return f.sendAvailability(avail)

	// Unavailable from the start, or for a new reason.
	case avail != Available &&
		(initial || changed):

		return f.sendAvailability(avail)

	default:
		return nil
	}
}

// stopScan stops scanning and drops any session in flight. radioLost says
// the OS ended the native scan on its own (Bluetooth off, permission
// revoked), so it must not be reused by a find that follows within the stop
// debounce.
func (f *finder) stopScan(radioLost bool) {
	if !f.scanning {
		return
	}
	if f.active != nil {
		f.radio.native.Disconnect(f.active.peer)
		f.active = nil
	}
	if radioLost {
		f.radio.scanLost()
	} else {
		f.radio.stopScan()
	}
	f.scanning = false
	f.window, f.giveUp = nil, nil
	f.candidates = make(map[string]*candidate)
}

// handleEvent processes one central callback. It reports true once a
// payment request was received and delivered to the client.
func (f *finder) handleEvent(ev event) (bool, error) {
	switch ev.kind {
	// A sighting: update the peer's smoothed RSSI and open the
	// collection window on the first one.
	case evAdvertisement:
		if !f.scanning || f.exclude[ev.id] || f.failed[ev.id] {
			return false, nil
		}
		c, ok := f.candidates[ev.id]
		if !ok {
			c = &candidate{rssi: float64(ev.rssi)}
			f.candidates[ev.id] = c
		} else {
			c.rssi += rssiSmoothing * (float64(ev.rssi) - c.rssi)
		}
		if f.window == nil && f.active == nil {
			f.window = f.clock.TickAfter(collectWindow)
		}

		return false, nil

	// Connected and subscribed: open the session.
	case evConnected:
		if f.active == nil || f.active.peer != ev.id ||
			f.active.connected {

			return false, nil
		}

		return false, f.startSession(ev.maxChunk)

	// A chunk from the sharer.
	case evNotify:
		if f.active == nil || f.active.peer != ev.id {
			return false, nil
		}

		return f.handleNotify(ev.chunk)

	// The link went away before we were done.
	case evDisconnected:
		if f.active == nil || f.active.peer != ev.id {
			return false, nil
		}
		log.Debugf("Nearby sharer %s disconnected: %s", ev.id,
			ev.reason)

		return false, f.failActive(
			FailureConnectFailed,
		)

	// The OS stopped our scan; the client restarts the find.
	case evScanFailed:
		log.Warnf("Nearby scan failed: %s", ev.reason)
		err := f.sendFailed(
			FailureRadioError,
		)
		if err != nil {
			return false, err
		}

		return false, ErrScanFailed

	default:
		return false, nil
	}
}

// pick connects to the strongest sharer, or reports that all are too far.
func (f *finder) pick() error {
	if f.active != nil {
		return nil
	}

	var (
		best   string
		bestDB float64
	)
	for id, c := range f.candidates {
		if best == "" || c.rssi > bestDB {
			best, bestDB = id, c.rssi
		}
	}
	if best == "" {
		return nil
	}

	// Keep re-evaluating while sharers are visible but too far away;
	// someone walking up should be picked without restarting.
	if bestDB < rssiFloor {
		f.window = f.clock.TickAfter(collectWindow)
		if f.tooFarSent {
			return nil
		}
		f.tooFarSent = true

		return f.send(TooFar{})
	}
	f.tooFarSent = false

	log.Debugf("Connecting to nearby sharer %s (rssi %.0f)", best, bestDB)
	f.active = &payerRun{
		peer:     best,
		deadline: f.clock.TickAfter(connectTimeout),
	}
	if err := f.send(Connecting{}); err != nil {
		return err
	}

	err := f.radio.native.Connect(best, ServiceUUID, RxUUID, TxUUID)
	if err != nil {
		log.Debugf("Connecting to %s failed: %v", best, err)
		return f.failActive(
			FailureConnectFailed,
		)
	}

	return nil
}

// startSession opens the payer session on a fresh connection.
func (f *finder) startSession(maxChunk int) error {
	sess, err := session.NewPayerSession(session.Config{
		Prologue: session.Prologue(serviceUUIDBytes()),
		Rand:     f.rand,
	})
	if err != nil {
		return err
	}
	out, err := sess.Start()
	if err != nil {
		return err
	}

	f.active.sess = sess
	f.active.maxChunk = maxChunk
	f.active.connected = true
	f.active.deadline = f.clock.TickAfter(sessionTimeout)

	if !f.writeAll(out.Send) {
		return f.failActive(
			FailureRadioError,
		)
	}

	return nil
}

// handleNotify reassembles a sharer's chunk and runs the session on complete
// messages. Once the request arrived it is parsed, acknowledged and handed
// to the client.
func (f *finder) handleNotify(chunk []byte) (bool, error) {
	if !f.active.connected {
		return false, nil
	}
	msg, err := f.active.reasm.Add(chunk)
	if err != nil {
		f.write(wire.EncodeAbort(wire.AbortProtocolError))
		return false, f.failActive(failureReason(err))
	}
	if msg == nil {
		return false, nil
	}

	out, err := f.active.sess.Handle(msg)
	sent := f.writeAll(out.Send)
	if err != nil {
		log.Debugf("Nearby payer session with %s failed: %v",
			f.active.peer, err)
		return false, f.failActive(failureReason(err))
	}

	// A reply that didn't reach the sharer leaves it waiting for us;
	// give up on it now rather than at the session timeout.
	if !sent {
		return false, f.failActive(
			FailureRadioError,
		)
	}
	if out.PaymentRequest == "" {
		return false, nil
	}

	// Let the wallet check the request before acknowledging it; an
	// unusable request is rejected so the sharer learns about it, and
	// the search goes on.
	log.Tracef("Nearby payment request from %s: %q", f.active.peer,
		out.PaymentRequest)
	if err := f.validate(out.PaymentRequest); err != nil {
		log.Debugf("Nearby payment request from %s rejected: %v",
			f.active.peer, err)
		if reject, rErr := f.active.sess.Reject(); rErr == nil {
			f.writeAll(reject.Send)
		}

		return false, f.failActive(
			FailurePayloadInvalid,
		)
	}

	accept, err := f.active.sess.Accept()
	if err != nil {
		return false, err
	}
	f.writeAll(accept.Send)

	peer := f.active.peer
	f.radio.native.Disconnect(peer)
	f.active = nil

	return true, f.send(Received{
		PaymentRequest: out.PaymentRequest,
		Code:           out.Code,
		PeerID:         peer,
	})
}

// failActive ends the active session, excludes its sharer for the rest of
// this find and goes back to picking.
func (f *finder) failActive(reason FailureReason) error {
	if f.active == nil {
		return nil
	}
	peer := f.active.peer
	f.active = nil
	f.failed[peer] = true
	delete(f.candidates, peer)
	f.radio.native.Disconnect(peer)

	if len(f.candidates) > 0 {
		f.window = f.clock.TickAfter(collectWindow)
	}

	return f.sendFailed(reason)
}

// writeAll sends messages to the active sharer and reports whether every
// chunk went out.
func (f *finder) writeAll(messages [][]byte) bool {
	for _, msg := range messages {
		if !f.write(msg) {
			return false
		}
	}

	return true
}

// write chunks one message and writes it to the active sharer.
func (f *finder) write(msg []byte) bool {
	if f.active == nil || !f.active.connected {
		return false
	}
	chunks, err := wire.Chunk(msg, f.active.maxChunk)
	if err != nil {
		log.Debugf("Chunking for %s failed: %v", f.active.peer, err)
		return false
	}
	for _, c := range chunks {
		if err := f.radio.native.Write(f.active.peer, c); err != nil {
			log.Debugf("Write to %s failed: %v", f.active.peer, err)
			return false
		}
	}

	return true
}

// sendFailed reports one failed session.
func (f *finder) sendFailed(reason FailureReason) error {
	return f.send(SessionFailed{Reason: reason})
}

// sendAvailability reports a changed availability.
func (f *finder) sendAvailability(avail Availability) error {
	return f.send(AvailabilityChanged{Availability: avail})
}
