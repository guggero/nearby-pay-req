package session

import (
	"bytes"
	"crypto/subtle"

	"github.com/flynn/noise"
	"github.com/guggero/nearby-pay-req/wire"
	"github.com/lightningnetwork/lnd/tlv"
)

// payerState is where a PayerSession is in the exchange.
type payerState uint8

const (
	payerNew payerState = iota
	payerAwaitEEE
	payerAwaitReveal
	payerAwaitDecision
	payerDone
)

// PayerSession fetches one payment request from one payee. It is the Noise
// initiator and the GATT central side. Not safe for concurrent use.
type PayerSession struct {
	cfg Config

	state payerState
	hs    *noise.HandshakeState

	// send encrypts payer → payee, recv decrypts payee → payer.
	send, recv *noise.CipherState

	commitment [32]byte
	na         [nonceLen]byte
}

// NewPayerSession prepares a session; Start produces its first message.
func NewPayerSession(cfg Config) (*PayerSession, error) {
	hs, err := newHandshake(cfg, true)
	if err != nil {
		return nil, err
	}

	return &PayerSession{cfg: cfg, hs: hs}, nil
}

// Start opens the exchange with Noise message 1.
func (s *PayerSession) Start() (Output, error) {
	if s.state != payerNew {
		return Output{}, failure(
			ReasonProtocol, "session already started",
		)
	}

	msg, _, _, err := s.hs.WriteMessage(nil, nil)
	if err != nil {
		return Output{}, err
	}
	s.state = payerAwaitEEE

	return Output{
		Send: [][]byte{wire.EncodeMessage(wire.TypeNoiseE, msg)},
	}, nil
}

// Handle consumes one reassembled message from the payee. On failure the
// returned Output may still carry an ABORT to send before disconnecting.
// Once a payment request arrived, the caller decides whether it can use it
// and calls Accept or Reject.
func (s *PayerSession) Handle(raw []byte) (Output, error) {
	out, err := s.handle(raw)
	if err != nil {
		s.state = payerDone
	}

	return out, err
}

// handle is Handle without the terminal-state bookkeeping.
func (s *PayerSession) handle(raw []byte) (Output, error) {
	msg, err := wire.DecodeMessage(raw)
	if err != nil {
		return abortOutput(wire.AbortProtocolError), protocolFailure(
			err,
		)
	}
	if msg.Type == wire.TypeAbort {
		return Output{}, abortFailure(msg.Body)
	}
	if msg.Version != wire.Version1 {
		return abortOutput(wire.AbortVersionUnsupported),
			failure(
				ReasonVersion, "payee speaks version %d",
				msg.Version,
			)
	}

	switch s.state {
	// The payee answers our ephemeral with its own and a commitment.
	case payerAwaitEEE:
		return s.handleNoiseEEE(msg)

	// The payee reveals its nonce and the payment request.
	case payerAwaitReveal:
		return s.handleReveal(msg)

	// Nothing else may arrive: before Start, while the caller decides
	// on the request, and after the end.
	default:
		return abortOutput(wire.AbortProtocolError),
			failure(ReasonProtocol, "unexpected %v", msg.Type)
	}
}

// handleNoiseEEE completes the handshake, keeps the payee's commitment and
// only then reveals our nonce. Sending Na strictly after receiving the
// commitment is what makes the code non-grindable for the payee side.
func (s *PayerSession) handleNoiseEEE(msg wire.Message) (Output, error) {
	if msg.Type != wire.TypeNoiseEEE {
		return abortOutput(wire.AbortProtocolError),
			failure(
				ReasonProtocol, "expected %v, got %v",
				wire.TypeNoiseEEE, msg.Type,
			)
	}

	payload, send, recv, err := s.hs.ReadMessage(nil, msg.Body)
	if err != nil {
		return abortOutput(wire.AbortProtocolError),
			failure(ReasonHandshake, "reading noise e, ee: %w", err)
	}
	err = decodeExpected(
		payload, tlv.MakePrimitiveRecord(typeCommitment, &s.commitment),
	)
	if err != nil {
		return abortOutput(wire.AbortProtocolError), protocolFailure(
			err,
		)
	}
	s.send, s.recv = send, recv

	s.na, err = readNonce(s.cfg.Rand)
	if err != nil {
		return Output{}, err
	}
	nonce, err := seal(
		s.send, tlv.MakePrimitiveRecord(typePayerNonce, &s.na),
	)
	if err != nil {
		return Output{}, err
	}
	s.state = payerAwaitReveal

	return Output{Send: [][]byte{nonce}}, nil
}

// handleReveal checks the revealed nonce against the commitment and derives
// the code. The request is handed to the caller, who must Accept or Reject.
func (s *PayerSession) handleReveal(msg wire.Message) (Output, error) {
	if msg.Type != wire.TypeSealed {
		return abortOutput(wire.AbortProtocolError),
			failure(
				ReasonProtocol, "expected %v, got %v",
				wire.TypeSealed, msg.Type,
			)
	}
	plaintext, err := open(s.recv, msg.Body)
	if err != nil {
		return abortOutput(wire.AbortProtocolError), err
	}

	var (
		nb      [nonceLen]byte
		request []byte
	)
	err = decodeExpected(
		plaintext, tlv.MakePrimitiveRecord(typePayeeNonce, &nb),
		tlv.MakePrimitiveRecord(typePaymentRequest, &request),
	)
	if err != nil {
		return abortOutput(wire.AbortProtocolError), protocolFailure(
			err,
		)
	}

	// The commitment check is the authentication step: a payee that
	// picked Nb after seeing Na cannot have committed to it.
	c := commitment(nb)
	if subtle.ConstantTimeCompare(c[:], s.commitment[:]) != 1 {
		return abortOutput(wire.AbortProtocolError),
			failure(
				ReasonHandshake,
				"revealed nonce does not match commitment",
			)
	}

	// Copy out of the decryption buffer: the string outlives it.
	paymentRequest := string(bytes.Clone(request))
	if err := ValidatePaymentRequest(paymentRequest); err != nil {
		return abortOutput(wire.AbortPayloadRejected),
			&Error{Reason: ReasonPayloadRejected, Err: err}
	}
	s.state = payerAwaitDecision

	return Output{
		Code:           comparisonCode(s.hs.ChannelBinding(), s.na, nb),
		PaymentRequest: paymentRequest,
	}, nil
}

// Accept acknowledges a payment request the caller could use and ends the
// session.
func (s *PayerSession) Accept() (Output, error) {
	return s.ack(ackReceived)
}

// Reject tells the payee its request was unusable and ends the session.
func (s *PayerSession) Reject() (Output, error) {
	return s.ack(ackRejected)
}

// ack sends the final acknowledgement.
func (s *PayerSession) ack(status uint8) (Output, error) {
	if s.state != payerAwaitDecision {
		return Output{}, failure(
			ReasonProtocol, "no payment request to acknowledge",
		)
	}
	s.state = payerDone

	msg, err := seal(s.send, tlv.MakePrimitiveRecord(typeAck, &status))
	if err != nil {
		return Output{}, err
	}

	return Output{Send: [][]byte{msg}, Done: true}, nil
}
