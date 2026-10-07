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

	// errNotConnected refuses a write before the connection is up.
	errNotConnected = errors.New("not connected")
)

// candidate is one sharer seen while scanning.
type candidate struct {
	rssi     float64
	lastSeen time.Time
}

// payerRun is the one session the payer runs at a time.
type payerRun struct {
	peer      string
	connID    int
	sess      *session.PayerSession
	maxChunk  int
	reasm     wire.Reassembler
	connected bool

	// deadline fires at deadlineAt, when connecting or the session has
	// to be over. Writing checks deadlineAt before every chunk, so a
	// slow sharer cannot stretch the session past it.
	deadline   <-chan time.Time
	deadlineAt time.Time
}

// finder searches for one payment request until it receives one or its
// context ends.
type finder struct {
	// ctx is the find's context, checked between the chunks of a
	// message so a cancelled find stops writing at once.
	ctx context.Context

	// owner identifies the find's blocking radio calls (see
	// radioMux.newOwner).
	owner uint64

	radio  *radioMux
	clock  clock.Clock
	rand   io.Reader
	params Params

	exclude  map[string]bool
	validate func(string) error
	collect  bool

	availability  func() Availability
	statusChanged <-chan struct{}

	send func(FindEvent) error

	events sink

	lastAvailability Availability
	scanning         bool
	candidates       map[string]*candidate

	// known holds every sharer this find tracks anything about. It is
	// capped at MaxPeersPerFind, which bounds candidates and every
	// per-sharer map below whatever number of identities sharers in
	// range present.
	known       map[string]struct{}
	failed      map[string]bool
	received    map[string]bool
	busyRetries map[string]int
	retryAt     map[string]time.Time
	window      <-chan time.Time
	giveUp      <-chan time.Time
	tooFarSent  bool
	active      *payerRun
}

// run searches until a payment request was received (nil), ctx ends, the
// stream breaks or the scan fails.
func (f *finder) run(ctx context.Context) error {
	f.ctx = ctx
	f.events = make(sink, f.params.EventQueueLen)
	f.candidates = make(map[string]*candidate)
	f.known = make(map[string]struct{})
	f.failed = make(map[string]bool)
	f.received = make(map[string]bool)
	f.busyRetries = make(map[string]int)
	f.retryAt = make(map[string]time.Time)
	defer f.stopScan(false)

	// A write blocked in the native stack when the find ends is cut
	// short instead of running into its timeout.
	f.owner = f.radio.newOwner()
	stopInterrupt := context.AfterFunc(ctx, func() {
		f.radio.interruptFind(f.owner)
	})
	defer stopInterrupt()

	if err := f.refresh(true); err != nil {
		return err
	}

	for {
		// An expired session ends before anything else is handled,
		// so a steady stream of events cannot keep it alive.
		if f.active != nil &&
			!f.clock.Now().Before(f.active.deadlineAt) {

			if err := f.timeoutActive(); err != nil {
				return err
			}
			continue
		}

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
			if err := f.timeoutActive(); err != nil {
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
		f.giveUp = f.clock.TickAfter(f.params.FindGiveUpAfter)

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
		f.radio.disconnect(f.active.peer, f.active.connID)
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
		if !f.eligible(ev.id) || !f.track(ev.id) {
			return false, nil
		}
		c, ok := f.candidates[ev.id]
		if !ok {
			c = &candidate{rssi: float64(ev.rssi)}
			f.candidates[ev.id] = c
		} else {
			c.rssi += f.params.RSSISmoothing *
				(float64(ev.rssi) - c.rssi)
		}
		c.lastSeen = f.clock.Now()
		if f.window == nil && f.active == nil {
			f.window = f.clock.TickAfter(f.params.CollectWindow)
		}

		return false, nil

	// Connected and subscribed: open the session.
	case evConnected:
		if !f.ownConnection(ev) || f.active.connected {
			return false, nil
		}

		return false, f.startSession(ev.maxChunk)

	// A chunk from the sharer.
	case evNotify:
		if !f.ownConnection(ev) {
			return false, nil
		}

		return f.handleNotify(ev.chunk)

	// The link went away before we were done.
	case evDisconnected:
		if !f.ownConnection(ev) {
			return false, nil
		}
		log.Debugf("Nearby sharer %s disconnected: %s", ev.id,
			ev.reason)

		return false, f.failActive(
			FailureConnectFailed,
		)

	// The OS stopped our scan; the client restarts the find. The dead
	// scan must not be reused by that next find.
	case evScanFailed:
		log.Warnf("Nearby scan failed: %s", ev.reason)
		f.radio.scanFailed(ev.scanGen)
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

// ownConnection reports whether a connection event belongs to the active
// session's connection, rather than to an earlier connection to the same
// sharer whose callbacks arrive late.
func (f *finder) ownConnection(ev event) bool {
	return f.active != nil && f.active.peer == ev.id &&
		f.active.connID == ev.connID
}

// track starts tracking a sharer, or reports false once the find tracks
// MaxPeersPerFind sharers already. Beyond that the find admits no new
// sharer: forgetting an old one instead could let a sharer that failed, or
// was excluded for a mismatch, come back under the code-attempt budget.
func (f *finder) track(peer string) bool {
	if _, ok := f.known[peer]; ok {
		return true
	}
	if len(f.known) >= f.params.MaxPeersPerFind {
		return false
	}
	f.known[peer] = struct{}{}

	return true
}

// pick connects to the strongest sharer, or reports that all are too far.
// A sharer not seen for CandidateTTL is no candidate any more: it left, or
// its RSSI average is too old to compare.
func (f *finder) pick() error {
	if f.active != nil {
		return nil
	}

	var (
		best   string
		bestDB float64
		now    = f.clock.Now()
	)
	for id, c := range f.candidates {
		if now.Sub(c.lastSeen) > f.params.CandidateTTL {
			delete(f.candidates, id)
			continue
		}
		if best == "" || c.rssi > bestDB {
			best, bestDB = id, c.rssi
		}
	}
	if best == "" {
		return nil
	}

	// Keep re-evaluating while sharers are visible but too far away;
	// someone walking up should be picked without restarting.
	if bestDB < float64(f.params.RSSIFloor) {
		f.window = f.clock.TickAfter(f.params.CollectWindow)
		if f.tooFarSent {
			return nil
		}
		f.tooFarSent = true

		return f.send(TooFar{})
	}
	f.tooFarSent = false

	log.Debugf("Connecting to nearby sharer %s (rssi %.0f)", best, bestDB)
	f.active = &payerRun{
		peer:       best,
		deadline:   f.clock.TickAfter(f.params.ConnectTimeout),
		deadlineAt: f.clock.Now().Add(f.params.ConnectTimeout),
	}
	if err := f.send(Connecting{}); err != nil {
		return err
	}

	connID, err := f.radio.connect(best)
	f.active.connID = connID
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
	f.active.deadline = f.clock.TickAfter(f.params.SessionTimeout)
	f.active.deadlineAt = f.clock.Now().Add(f.params.SessionTimeout)

	if err := f.writeAll(out.Send, f.active.deadlineAt); err != nil {
		return f.failSend(err)
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
		f.sendControl(wire.EncodeAbort(wire.AbortProtocolError))
		return false, f.failActive(failureReason(err))
	}
	if msg == nil {
		return false, nil
	}

	// A failed session may still have an ABORT to send, best effort.
	out, err := f.active.sess.Handle(msg)
	if err != nil {
		log.Debugf("Nearby payer session with %s failed: %v",
			f.active.peer, err)
		for _, msg := range out.Send {
			f.sendControl(msg)
		}

		return false, f.failActive(failureReason(err))
	}

	// A reply that didn't reach the sharer leaves it waiting for us;
	// give up on it now rather than at the session timeout.
	if err := f.writeAll(out.Send, f.active.deadlineAt); err != nil {
		return false, f.failSend(err)
	}
	if out.PaymentRequest == "" {
		return false, nil
	}

	// Let the wallet check the request before acknowledging it; an
	// unusable request is rejected so the sharer learns about it, and
	// the search goes on. The request and the validator's error, which
	// may quote it, only go to the trace level, which production builds
	// keep off: amounts, addresses and node ids have no place in a
	// production log, but a developer chasing a rejected request needs
	// them.
	log.Debugf("Nearby payment request from %s: %d bytes", f.active.peer,
		len(out.PaymentRequest))
	log.Tracef("Nearby payment request from %s: %q", f.active.peer,
		out.PaymentRequest)
	if err := f.validate(out.PaymentRequest); err != nil {
		log.Debugf("Nearby payment request from %s rejected",
			f.active.peer)
		log.Tracef("Nearby payment request from %s rejected: %v",
			f.active.peer, err)
		if reject, rErr := f.active.sess.Reject(); rErr == nil {
			for _, msg := range reject.Send {
				f.sendControl(msg)
			}
		}

		return false, f.failActive(
			FailurePayloadInvalid,
		)
	}

	accept, err := f.active.sess.Accept()
	if err != nil {
		return false, err
	}
	err = f.writeAll(accept.Send, f.active.deadlineAt)
	if err != nil {
		log.Debugf("Acknowledgement to %s not sent: %v", f.active.peer,
			err)
	}

	peer := f.active.peer
	f.radio.disconnect(peer, f.active.connID)
	f.active = nil

	var token []byte
	if out.ChosenToken != nil {
		token = out.ChosenToken[:]
	}
	err = f.send(Received{
		PaymentRequest: out.PaymentRequest,
		Code:           out.Code,
		PeerID:         peer,
		ChosenToken:    token,
	})
	if err != nil || !f.collect {
		return err == nil, err
	}

	// Collecting: this sharer is done, the others still get their
	// turn, and the find no longer counts as fruitless.
	f.received[peer] = true
	delete(f.candidates, peer)
	f.giveUp = nil
	if len(f.candidates) > 0 {
		f.window = f.clock.TickAfter(f.params.CollectWindow)
	}

	return false, nil
}

// eligible reports whether a sighted sharer may become a candidate: not
// excluded by the caller, not failed or already received from in this find,
// and not waiting out a busy retry delay.
func (f *finder) eligible(peer string) bool {
	if !f.scanning || f.exclude[peer] || f.failed[peer] ||
		f.received[peer] {

		return false
	}

	return !f.clock.Now().Before(f.retryAt[peer])
}

// failActive ends the active session and goes back to picking. A sharer
// that was busy serving someone else is tried again after BusyRetryDelay,
// up to MaxBusyRetries times; any other failure excludes it for the rest of
// this find.
func (f *finder) failActive(reason FailureReason) error {
	if f.active == nil {
		return nil
	}
	peer, connID := f.active.peer, f.active.connID
	f.active = nil
	delete(f.candidates, peer)
	f.radio.disconnect(peer, connID)

	if reason == FailurePeerBusy &&
		f.busyRetries[peer] < f.params.MaxBusyRetries {

		f.busyRetries[peer]++
		f.retryAt[peer] = f.clock.Now().Add(f.params.BusyRetryDelay)
	} else {
		f.failed[peer] = true
	}

	if len(f.candidates) > 0 {
		f.window = f.clock.TickAfter(f.params.CollectWindow)
	}

	return f.sendFailed(reason)
}

// writeAll sends messages to the active sharer and reports whether every
// chunk went out.
func (f *finder) writeAll(messages [][]byte, until time.Time) error {
	for _, msg := range messages {
		if err := f.write(msg, until); err != nil {
			return err
		}
	}

	return nil
}

// sendControl writes a one-chunk message to the active sharer outside the
// session deadline, best effort: a sharer that misses it learns the outcome
// from the disconnect that follows.
func (f *finder) sendControl(msg []byte) {
	err := f.write(msg, f.clock.Now().Add(controlSendTimeout))
	if err != nil {
		log.Debugf("Message to %s not sent: %v", f.active.peer, err)
	}
}

// timeoutActive ends the active session whose deadline passed, telling the
// sharer within abortSendTimeout, so an expired session is not stretched by
// telling the sharer it expired.
func (f *finder) timeoutActive() error {
	if f.active != nil && f.active.connected {
		err := f.write(
			wire.EncodeAbort(wire.AbortTimeout),
			f.clock.Now().Add(abortSendTimeout),
		)
		if err != nil {
			log.Debugf("Abort to %s not sent: %v", f.active.peer,
				err)
		}
	}

	return f.failActive(FailureTimeout)
}

// failSend ends the active session after a write failed: a cancelled find
// ends, a passed deadline is a timeout and anything else a radio error.
func (f *finder) failSend(err error) error {
	switch {
	case f.ctx.Err() != nil:
		return f.ctx.Err()

	case errors.Is(err, errDeadline):
		return f.timeoutActive()

	default:
		return f.failActive(FailureRadioError)
	}
}

// write chunks one message and writes it to the active sharer. It stops
// with errDeadline once until passed and with the context's error once the
// find ended, checking both before every chunk, and never lets the native
// stack block beyond until.
func (f *finder) write(msg []byte, until time.Time) error {
	if f.active == nil || !f.active.connected {
		return errNotConnected
	}
	chunks, err := wire.Chunk(msg, f.active.maxChunk)
	if err != nil {
		return err
	}
	for _, c := range chunks {
		if err := f.ctx.Err(); err != nil {
			return err
		}
		remaining := until.Sub(f.clock.Now())
		if remaining <= 0 {
			return errDeadline
		}
		err := f.radio.write(
			f.owner, f.active.peer, f.active.connID, c, remaining,
		)
		if err != nil {
			log.Debugf("Write to %s failed: %v", f.active.peer, err)
			return err
		}
	}

	return nil
}

// sendFailed reports one failed session.
func (f *finder) sendFailed(reason FailureReason) error {
	return f.send(SessionFailed{Reason: reason})
}

// sendAvailability reports a changed availability.
func (f *finder) sendAvailability(avail Availability) error {
	return f.send(AvailabilityChanged{Availability: avail})
}
