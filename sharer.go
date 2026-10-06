package nearby

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/guggero/nearby-pay-req/hce"
	"github.com/guggero/nearby-pay-req/ndef"
	"github.com/guggero/nearby-pay-req/session"
	"github.com/guggero/nearby-pay-req/wire"
	"github.com/lightningnetwork/lnd/clock"
)

var (
	// errChosen ends a share whose request a payer chose, unless the share
	// was told to continue.
	errChosen = errors.New("payment request chosen")
)

// centralConn is what the payee knows about one connected payer.
type centralConn struct {
	maxChunk int
	reasm    wire.Reassembler
}

// payeeRun is the one session the payee serves at a time.
type payeeRun struct {
	central   string
	sess      *session.PayeeSession
	deadline  <-chan time.Time
	codeShown bool
	lastCode  string
	token     *[wire.ChosenTokenLen]byte
}

// deliveredSession is a session whose payer acknowledged the request,
// remembered so that payer can still say its user chose it.
type deliveredSession struct {
	code string
	at   time.Time
}

// sharer serves one payment request until its context ends. It runs on the
// caller's goroutine; radio callbacks reach it through events.
type sharer struct {
	radio  *radioMux
	tag    *hce.Tag
	clock  clock.Clock
	rand   io.Reader
	params Params

	paymentRequest      string
	nfc                 bool
	continueAfterChosen bool

	// availability reports the current BLE sharing availability;
	// statusChanged nudges a re-check.
	availability  func() Availability
	statusChanged <-chan struct{}

	send func(ShareEvent) error

	events   sink
	nfcReads chan struct{}

	lastAvailability Availability
	advertising      bool
	conns            map[string]*centralConn
	active           *payeeRun

	// sessionStarts and codeFailures are the rolling windows for rate
	// limiting and the suspicious-activity warning.
	sessionStarts []time.Time
	codeFailures  []time.Time
	warnedAt      time.Time

	// delivered maps the chosen token of every recently delivered
	// session to what is needed to report a CHOSEN for it.
	delivered map[[wire.ChosenTokenLen]byte]deliveredSession
}

// run shares until ctx ends or the stream breaks.
func (s *sharer) run(ctx context.Context) error {
	s.events = make(sink, s.params.EventQueueLen)
	s.nfcReads = make(chan struct{}, 1)
	s.conns = make(map[string]*centralConn)
	s.delivered = make(map[[wire.ChosenTokenLen]byte]deliveredSession)

	// The NFC tag only needs the message and the preferred-service
	// registration; it runs independently of Bluetooth.
	if s.nfc {
		message, err := ndef.EncodeURIMessage(s.paymentRequest)
		if err != nil {
			return err
		}
		err = s.tag.SetMessage(message, func() {
			select {
			case s.nfcReads <- struct{}{}:

			default:
			}
		})
		if err != nil {
			return err
		}
		s.radio.native.SetHceActive(true)
		defer func() {
			s.radio.native.SetHceActive(false)
			s.tag.Clear()
		}()
	}
	defer s.stopBLE()

	// The first refresh starts advertising if possible and always
	// reports where the share stands.
	if err := s.refreshBLE(true); err != nil {
		return err
	}

	for {
		var deadline <-chan time.Time
		if s.active != nil {
			deadline = s.active.deadline
		}

		select {
		case <-ctx.Done():
			return ctx.Err()

		// A radio callback.
		case ev := <-s.events:
			if err := s.handleEvent(ev); err != nil {
				return err
			}

		// A reader finished reading the NFC tag.
		case <-s.nfcReads:
			if err := s.send(NFCRead{}); err != nil {
				return err
			}

		// Bluetooth was switched on or off, or a permission changed.
		case <-s.statusChanged:
			if err := s.refreshBLE(false); err != nil {
				return err
			}

		// The active session took too long.
		case <-deadline:
			s.abortActive(wire.AbortTimeout)
			err := s.failActive(
				FailureTimeout,
			)
			if err != nil {
				return err
			}
		}
	}
}

// refreshBLE starts or stops advertising to match the current availability
// and reports changes. initial forces a report, so the client always learns
// the starting state.
func (s *sharer) refreshBLE(initial bool) error {
	avail := s.availability()
	changed := initial || avail != s.lastAvailability
	s.lastAvailability = avail

	switch {
	// Available and not yet advertising: start. A failure to start
	// leaves BLE off but the share (and NFC) running.
	case avail == Available &&
		!s.advertising:

		if err := s.radio.startAdvertising(s.events); err != nil {
			log.Warnf("Starting nearby advertising failed: %v", err)
		} else {
			s.advertising = true
		}

		return s.sendStarted()

	// Gone unavailable while advertising: stop and say why.
	case avail != Available &&
		s.advertising:

		s.stopBLE()
		return s.sendAvailability(avail)

	// Unavailable from the start (Bluetooth off, …): report it, plus
	// the NFC-only start if NFC runs.
	case initial && avail != Available:
		if err := s.sendAvailability(avail); err != nil {
			return err
		}
		if s.nfc {
			return s.sendStarted()
		}

		return nil

	// Still unavailable, for a different reason.
	case changed && !s.advertising:
		return s.sendAvailability(avail)

	default:
		return nil
	}
}

// stopBLE stops advertising and forgets every connection.
func (s *sharer) stopBLE() {
	if !s.advertising {
		return
	}
	s.radio.stopAdvertising()
	s.advertising = false
	s.conns = make(map[string]*centralConn)
	s.active = nil
}

// handleEvent processes one peripheral callback.
func (s *sharer) handleEvent(ev event) error {
	switch ev.kind {
	// A payer subscribed; we can now notify it.
	case evCentralReady:
		s.conns[ev.id] = &centralConn{maxChunk: ev.maxChunk}
		return nil

	// One chunk from a payer.
	case evWrite:
		return s.handleWrite(ev.id, ev.chunk)

	// A payer left; mid-session that ends the session. Only a session
	// that already showed its code counts towards the suspicious-activity
	// warning: a payer that drops before sending its nonce never learned
	// a code, so it cannot have been grinding for one.
	case evCentralGone:
		delete(s.conns, ev.id)
		if s.active == nil || s.active.central != ev.id {
			return nil
		}
		codeShown := s.active.codeShown
		s.active = nil

		return s.sessionFailed(
			FailureConnectFailed,
			codeShown,
		)

	// The OS stopped our advertisement. Keep the share (NFC may still
	// work) but report BLE as down.
	case evAdvertisingFailed:
		log.Warnf("Nearby advertising failed: %s", ev.reason)
		s.stopBLE()
		err := s.sessionFailed(
			FailureRadioError,
			false,
		)
		if err != nil {
			return err
		}

		return s.sendStarted()

	default:
		return nil
	}
}

// handleWrite reassembles a payer's chunk and feeds complete messages to its
// session, starting one if this is the payer's first message.
func (s *sharer) handleWrite(central string, chunk []byte) error {
	conn, ok := s.conns[central]
	if !ok {
		// A write before the payer subscribed to notifications: we
		// could never answer it.
		log.Debugf("Write from unsubscribed central %s", central)
		s.radio.native.DisconnectCentral(central)
		return nil
	}

	msg, err := conn.reasm.Add(chunk)
	if err != nil {
		return s.dropCentral(central, err)
	}
	if msg == nil {
		return nil
	}

	// A CHOSEN opens a connection of its own and needs no session, so
	// it is answered even while another payer is mid-session.
	if s.active == nil || s.active.central != central {
		parsed, err := wire.DecodeMessage(msg)
		if err == nil && parsed.Type == wire.TypeChosen {
			return s.handleChosen(central, conn, parsed)
		}
	}

	// Another payer is mid-session: turn this one away.
	if s.active != nil && s.active.central != central {
		s.notify(central, conn, wire.EncodeAbort(wire.AbortBusy))
		s.radio.native.DisconnectCentral(central)
		delete(s.conns, central)
		return nil
	}

	if s.active == nil {
		started, err := s.startSession(central, conn)
		if err != nil || !started {
			return err
		}
	}

	out, err := s.active.sess.Handle(msg)
	if !s.notifyAll(central, conn, out.Send) {
		s.active = nil
		s.radio.native.DisconnectCentral(central)
		return s.sessionFailed(
			FailureRadioError,
			true,
		)
	}
	if err != nil {
		log.Debugf("Nearby payee session with %s failed: %v",
			central, err)
		return s.failActive(failureReason(err))
	}

	// The payer has our code once it has sent its nonce.
	if out.Code != "" {
		s.active.codeShown = true
		log.Tracef("Nearby payee code %s for %s", out.Code, central)
		if err := s.send(PeerConnected{Code: out.Code}); err != nil {
			return err
		}
		s.active.lastCode = out.Code
		s.active.token = out.ChosenToken
	}

	if out.Delivered {
		code, token := s.active.lastCode, s.active.token
		s.active = nil
		s.radio.native.DisconnectCentral(central)
		delete(s.conns, central)

		if token != nil {
			s.delivered[*token] = deliveredSession{
				code: code,
				at:   s.clock.Now(),
			}
		}

		return s.send(Delivered{Code: code})
	}

	return nil
}

// handleChosen answers a payer saying its user chose the request of one of
// our delivered sessions. A match is confirmed and reported, and ends the
// share unless it should continue; anything else is refused without
// telling the payer more than that.
func (s *sharer) handleChosen(central string, conn *centralConn,
	msg wire.Message) error {

	defer func() {
		s.radio.native.DisconnectCentral(central)
		delete(s.conns, central)
	}()

	s.pruneDelivered()
	token, err := wire.DecodeChosen(msg.Body)
	if err != nil || msg.Version != wire.Version1 {
		s.notify(central, conn, wire.EncodeAbort(
			wire.AbortProtocolError,
		))
		return nil
	}
	ds, ok := s.delivered[token]
	if !ok {
		log.Debugf("Unknown chosen token from %s", central)
		s.notify(central, conn, wire.EncodeAbort(
			wire.AbortUnknownSession,
		))
		return nil
	}
	delete(s.delivered, token)
	s.notify(central, conn, wire.EncodeChosenAck())

	if err := s.send(Chosen{Code: ds.code}); err != nil {
		return err
	}
	if s.continueAfterChosen {
		return nil
	}

	return errChosen
}

// pruneDelivered forgets delivered sessions older than ChosenRetention.
func (s *sharer) pruneDelivered() {
	cutoff := s.clock.Now().Add(-s.params.ChosenRetention)
	for token, d := range s.delivered {
		if d.at.Before(cutoff) {
			delete(s.delivered, token)
		}
	}
}

// startSession admits a new payer unless the rate limit says otherwise. It
// reports whether a session started.
func (s *sharer) startSession(central string, conn *centralConn) (bool, error) {
	now := s.clock.Now()
	s.sessionStarts = pruneBefore(
		s.sessionStarts, now.Add(-s.params.SessionRateWindow),
	)
	limited := len(
		s.sessionStarts,
	) >= s.params.MaxSessionsPerWindow || len(
		s.sessionStarts,
	) > 0 &&
		now.Sub(s.sessionStarts[len(s.sessionStarts)-1]) <
			s.params.MinSessionInterval
	if limited {
		log.Debugf("Rate limiting nearby session from %s", central)
		s.notify(central, conn, wire.EncodeAbort(wire.AbortBusy))
		s.radio.native.DisconnectCentral(central)
		delete(s.conns, central)

		return false, nil
	}

	sess, err := session.NewPayeeSession(session.Config{
		Prologue: session.Prologue(serviceUUIDBytes()),
		Rand:     s.rand,
	}, s.paymentRequest)
	if err != nil {
		return false, err
	}
	s.sessionStarts = append(s.sessionStarts, now)
	s.active = &payeeRun{
		central:  central,
		sess:     sess,
		deadline: s.clock.TickAfter(s.params.SessionTimeout),
	}

	return true, nil
}

// dropCentral handles a framing violation from a payer.
func (s *sharer) dropCentral(central string, err error) error {
	log.Debugf("Dropping nearby central %s: %v", central, err)
	if conn, ok := s.conns[central]; ok {
		s.notify(central, conn, wire.EncodeAbort(
			wire.AbortProtocolError,
		))
	}
	s.radio.native.DisconnectCentral(central)
	delete(s.conns, central)

	if s.active == nil || s.active.central != central {
		return nil
	}
	codeShown := s.active.codeShown
	s.active = nil

	return s.sessionFailed(
		FailureProtocolError,
		codeShown,
	)
}

// abortActive tells the active payer the session ends, best effort.
func (s *sharer) abortActive(reason wire.AbortReason) {
	if s.active == nil {
		return
	}
	if conn, ok := s.conns[s.active.central]; ok {
		s.notify(s.active.central, conn, wire.EncodeAbort(reason))
	}
}

// failActive ends the active session with reason and disconnects its payer.
func (s *sharer) failActive(reason FailureReason) error {
	if s.active == nil {
		return nil
	}
	central, codeShown := s.active.central, s.active.codeShown
	s.active = nil
	s.radio.native.DisconnectCentral(central)
	delete(s.conns, central)

	return s.sessionFailed(reason, codeShown)
}

// sessionFailed reports a failed session and, if the session had already
// shown a code, counts it towards the suspicious-activity warning: a man in
// the middle grinding for a matching code produces exactly such failures.
func (s *sharer) sessionFailed(reason FailureReason,
	codeShown bool) error {

	err := s.send(SessionFailed{Reason: reason})
	if err != nil || !codeShown {
		return err
	}

	now := s.clock.Now()
	s.codeFailures = pruneBefore(
		s.codeFailures, now.Add(-s.params.SuspiciousWindow),
	)
	s.codeFailures = append(s.codeFailures, now)

	// Warn once per window, not on every further failure.
	if len(s.codeFailures) < s.params.SuspiciousThreshold ||
		now.Sub(s.warnedAt) < s.params.SuspiciousWindow {

		return nil
	}
	s.warnedAt = now

	return s.send(SuspiciousActivity{
		FailedSessions: len(s.codeFailures),
		Window:         s.params.SuspiciousWindow,
	})
}

// notifyAll sends messages to a payer and reports whether every chunk went
// out.
func (s *sharer) notifyAll(central string, conn *centralConn,
	messages [][]byte) bool {

	for _, msg := range messages {
		if !s.notify(central, conn, msg) {
			return false
		}
	}

	return true
}

// notify chunks one message and sends it to a payer.
func (s *sharer) notify(central string, conn *centralConn, msg []byte) bool {
	chunks, err := wire.Chunk(msg, conn.maxChunk)
	if err != nil {
		log.Debugf("Chunking for central %s failed: %v", central, err)
		return false
	}
	for _, c := range chunks {
		if err := s.radio.native.Notify(central, c); err != nil {
			log.Debugf("Notify to central %s failed: %v", central,
				err)
			return false
		}
	}

	return true
}

// sendStarted reports which transports are live.
func (s *sharer) sendStarted() error {
	return s.send(ShareStarted{BLE: s.advertising, NFC: s.nfc})
}

// sendAvailability reports a changed BLE availability.
func (s *sharer) sendAvailability(avail Availability) error {
	return s.send(AvailabilityChanged{Availability: avail})
}

// failureReason maps a session or framing error onto a FailureReason.
func failureReason(err error) FailureReason {
	switch session.ReasonOf(err) {
	case session.ReasonProtocol:
		return FailureProtocolError

	case session.ReasonVersion:
		return FailureVersionUnsupported

	case session.ReasonHandshake:
		return FailureHandshakeFailed

	case session.ReasonBusy:
		return FailurePeerBusy

	case session.ReasonPayloadRejected:
		return FailurePayloadInvalid

	// The peer gave up on its own; from here that is a lost
	// connection.
	case session.ReasonPeerAborted:
		return FailureConnectFailed
	}

	// Framing errors come straight from the reassembler.
	if errors.Is(err, wire.ErrProtocol) {
		return FailureProtocolError
	}

	return FailureUnspecified
}

// pruneBefore drops the timestamps older than cutoff from a sorted slice.
func pruneBefore(times []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(times) && times[i].Before(cutoff) {
		i++
	}

	return times[i:]
}
