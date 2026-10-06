package nearby

import (
	"context"
	"errors"
	"fmt"

	"github.com/guggero/nearby-pay-req/wire"
	"github.com/lightningnetwork/lnd/clock"
)

var (
	// ErrChooseFailed is returned when Choose could not reach the payee
	// within Params.ChooseAttempts. The payee keeps sharing; the payment
	// itself is not affected.
	ErrChooseFailed = errors.New(
		"could not tell the payee the request was chosen",
	)

	// ErrUnknownSession is returned when the payee does not recognise
	// the session: it stopped or replaced its share, or more than
	// Params.ChosenRetention passed since the delivery.
	ErrUnknownSession = errors.New("payee does not know the session")

	// ErrInvalidChosenToken is returned for a token that is not one
	// Received.ChosenToken carries.
	ErrInvalidChosenToken = errors.New("invalid chosen token")

	// errChooseTimeout ends one Choose attempt that took too long.
	errChooseTimeout = errors.New("timed out")

	// errChooseDisconnected ends one Choose attempt whose link dropped.
	errChooseDisconnected = errors.New("disconnected")
)

// Choose tells the payee of a Received that its user picked that request,
// so the payee stops sharing it (see Chosen). Call it once the user
// confirmed the codes match. It connects to peerID again, sends the
// session's token and waits for the payee's confirmation, retrying
// Params.ChooseAttempts times. Like Find it uses the central role, so it
// replaces a running find. It returns nil once the payee confirmed,
// ErrUnknownSession if the payee no longer knows the session, or
// ErrChooseFailed if it could not be reached. Telling the payee is a
// courtesy: the payment can go ahead whatever Choose returns.
func (m *Manager) Choose(ctx context.Context, peerID string,
	token []byte) error {

	if len(token) != wire.ChosenTokenLen {
		return ErrInvalidChosenToken
	}
	if avail := m.findAvailability(); avail != Available {
		return &UnavailableError{Role: "find", Availability: avail}
	}

	ctx, stop := m.claim(ctx, &m.find)
	defer stop()

	c := &chooser{
		radio:  m.radio,
		clock:  m.cfg.Clock,
		params: m.cfg.Params,
		peer:   peerID,
		events: make(sink, m.cfg.Params.EventQueueLen),
	}
	copy(c.token[:], token)

	return result(ctx, c.run(ctx))
}

// chooser runs the attempts of one Choose.
type chooser struct {
	radio  *radioMux
	clock  clock.Clock
	params Params
	peer   string
	token  [wire.ChosenTokenLen]byte
	events sink
}

// run tries to deliver the CHOSEN message until the payee answered, the
// attempts ran out or ctx ended.
func (c *chooser) run(ctx context.Context) error {
	// The scan subscribes us to central events; it also gives the
	// native side a fresh handle on the peer to connect to.
	if err := c.radio.startScan(c.events); err != nil {
		return fmt.Errorf("%w: %v", ErrChooseFailed, err)
	}
	defer c.radio.stopScan()

	var lastErr error
	for attempt := 0; attempt < c.params.ChooseAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-c.clock.TickAfter(c.params.ChooseRetryDelay):

			case <-ctx.Done():
				return ctx.Err()
			}
		}

		err := c.attempt(ctx)
		switch {
		// Confirmed, or an answer that won't change on retrying.
		case err == nil, errors.Is(err, ErrUnknownSession):
			return err

		case ctx.Err() != nil:
			return ctx.Err()
		}
		log.Debugf("Choose attempt %d with %s failed: %v", attempt+1,
			c.peer, err)
		lastErr = err
	}

	return fmt.Errorf("%w: %v", ErrChooseFailed, lastErr)
}

// attempt connects once, sends the token and waits for the answer.
func (c *chooser) attempt(ctx context.Context) error {
	defer c.radio.native.Disconnect(c.peer)

	err := c.radio.native.Connect(c.peer, ServiceUUID, RxUUID, TxUUID)
	if err != nil {
		return err
	}

	var reasm wire.Reassembler
	deadline := c.clock.TickAfter(c.params.ConnectTimeout)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-deadline:
			return errChooseTimeout

		case ev := <-c.events:
			if ev.id != c.peer {
				continue
			}

			switch ev.kind {
			// Linked: send the token, then wait for the answer.
			case evConnected:
				if err := c.write(ev.maxChunk); err != nil {
					return err
				}
				deadline = c.clock.TickAfter(
					c.params.SessionTimeout,
				)

			// The answer, possibly in chunks.
			case evNotify:
				msg, err := reasm.Add(ev.chunk)
				if err != nil {
					return err
				}
				if msg != nil {
					return answer(msg)
				}

			case evDisconnected:
				return errChooseDisconnected
			}
		}
	}
}

// write sends the CHOSEN message in chunks of at most maxChunk.
func (c *chooser) write(maxChunk int) error {
	chunks, err := wire.Chunk(wire.EncodeChosen(c.token), maxChunk)
	if err != nil {
		return err
	}
	for _, chunk := range chunks {
		if err := c.radio.native.Write(c.peer, chunk); err != nil {
			return err
		}
	}

	return nil
}

// answer interprets the payee's reply to a CHOSEN message.
func answer(msg []byte) error {
	parsed, err := wire.DecodeMessage(msg)
	if err != nil {
		return err
	}

	switch parsed.Type {
	// Confirmed.
	case wire.TypeChosen:
		return nil

	// Refused: an unknown session is final, anything else is worth
	// another attempt.
	case wire.TypeAbort:
		reason, _, err := wire.DecodeAbort(parsed.Body)
		if err != nil {
			return err
		}
		if reason == wire.AbortUnknownSession {
			return ErrUnknownSession
		}

		return fmt.Errorf("payee aborted: %v", reason)

	default:
		return fmt.Errorf("%w: unexpected %v", wire.ErrProtocol,
			parsed.Type)
	}
}
