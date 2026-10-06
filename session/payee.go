package session

import (
	"github.com/flynn/noise"
	"github.com/guggero/nearby-pay-req/wire"
	"github.com/lightningnetwork/lnd/tlv"
)

// payeeState is where a PayeeSession is in the exchange.
type payeeState uint8

const (
	payeeAwaitE payeeState = iota
	payeeAwaitPayerNonce
	payeeAwaitAck
	payeeDone
)

// PayeeSession serves one payment request to one payer. It is the Noise
// responder and the GATT peripheral side. Not safe for concurrent use.
type PayeeSession struct {
	cfg            Config
	paymentRequest string

	state payeeState
	hs    *noise.HandshakeState

	// recv decrypts payer → payee, send encrypts payee → payer.
	recv, send *noise.CipherState

	nb [nonceLen]byte
}

// NewPayeeSession prepares a session that will serve paymentRequest. The
// string is validated here so a share that could never be delivered fails
// when the share starts instead of on every connection.
func NewPayeeSession(cfg Config, paymentRequest string) (*PayeeSession, error) {
	if err := ValidatePaymentRequest(paymentRequest); err != nil {
		return nil, err
	}
	hs, err := newHandshake(cfg, false)
	if err != nil {
		return nil, err
	}

	return &PayeeSession{
		cfg:            cfg,
		paymentRequest: paymentRequest,
		hs:             hs,
	}, nil
}

// Handle consumes one reassembled message from the payer. On failure the
// returned Output may still carry an ABORT to send before disconnecting.
func (s *PayeeSession) Handle(raw []byte) (Output, error) {
	out, err := s.handle(raw)
	if err != nil {
		s.state = payeeDone
	}

	return out, err
}

// handle is Handle without the terminal-state bookkeeping.
func (s *PayeeSession) handle(raw []byte) (Output, error) {
	msg, err := wire.DecodeMessage(raw)
	if err != nil {
		return abortOutput(wire.AbortProtocolError), protocolFailure(
			err,
		)
	}

	// The version gate comes first so a future payer learns what we
	// speak instead of being told its message is malformed.
	if msg.Type == wire.TypeAbort {
		return Output{}, abortFailure(msg.Body)
	}
	if msg.Version != wire.Version1 {
		return abortOutput(wire.AbortVersionUnsupported),
			failure(
				ReasonVersion, "payer speaks version %d",
				msg.Version,
			)
	}

	switch s.state {
	// The payer opens with its ephemeral key.
	case payeeAwaitE:
		return s.handleNoiseE(msg)

	// The payer reveals its nonce after seeing our commitment.
	case payeeAwaitPayerNonce:
		return s.handlePayerNonce(msg)

	// The payer acknowledges the request.
	case payeeAwaitAck:
		return s.handleAck(msg)

	default:
		return Output{}, failure(
			ReasonProtocol, "message %v after session end",
			msg.Type,
		)
	}
}

// handleNoiseE answers Noise message 1 with message 2, whose encrypted
// payload commits to our nonce before we have seen the payer's.
func (s *PayeeSession) handleNoiseE(msg wire.Message) (Output, error) {
	if msg.Type != wire.TypeNoiseE {
		return abortOutput(wire.AbortProtocolError),
			failure(
				ReasonProtocol, "expected %v, got %v",
				wire.TypeNoiseE, msg.Type,
			)
	}

	// NN message 1 carries no payload; anything extra is malformed.
	payload, _, _, err := s.hs.ReadMessage(nil, msg.Body)
	if err != nil {
		return abortOutput(wire.AbortProtocolError),
			failure(ReasonProtocol, "reading noise e: %w", err)
	}
	if len(payload) != 0 {
		return abortOutput(wire.AbortProtocolError),
			failure(ReasonProtocol, "unexpected noise e payload")
	}

	s.nb, err = readNonce(s.cfg.Rand)
	if err != nil {
		return Output{}, err
	}
	c := commitment(s.nb)
	commitPayload, err := wire.EncodeTLV(
		tlv.MakePrimitiveRecord(typeCommitment, &c),
	)
	if err != nil {
		return Output{}, err
	}

	reply, recv, send, err := s.hs.WriteMessage(nil, commitPayload)
	if err != nil {
		return Output{}, err
	}
	s.recv, s.send = recv, send
	s.state = payeeAwaitPayerNonce

	return Output{
		Send: [][]byte{wire.EncodeMessage(wire.TypeNoiseEEE, reply)},
	}, nil
}

// handlePayerNonce learns Na, which fixes the comparison code, and reveals
// Nb together with the payment request.
func (s *PayeeSession) handlePayerNonce(msg wire.Message) (Output, error) {
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

	var na [nonceLen]byte
	err = decodeExpected(
		plaintext, tlv.MakePrimitiveRecord(typePayerNonce, &na),
	)
	if err != nil {
		return abortOutput(wire.AbortProtocolError), protocolFailure(
			err,
		)
	}

	request := []byte(s.paymentRequest)
	reveal, err := seal(
		s.send, tlv.MakePrimitiveRecord(typePayeeNonce, &s.nb),
		tlv.MakePrimitiveRecord(typePaymentRequest, &request),
	)
	if err != nil {
		return Output{}, err
	}
	s.state = payeeAwaitAck

	return Output{
		Send: [][]byte{reveal},
		Code: comparisonCode(s.hs.ChannelBinding(), na, s.nb),
	}, nil
}

// handleAck ends the session on the payer's acknowledgement.
func (s *PayeeSession) handleAck(msg wire.Message) (Output, error) {
	if msg.Type != wire.TypeSealed {
		return Output{}, failure(
			ReasonProtocol, "expected %v, got %v", wire.TypeSealed,
			msg.Type,
		)
	}
	plaintext, err := open(s.recv, msg.Body)
	if err != nil {
		return Output{}, err
	}

	var status uint8
	err = decodeExpected(plaintext, tlv.MakePrimitiveRecord(
		typeAck, &status,
	))
	if err != nil {
		return Output{}, protocolFailure(err)
	}
	s.state = payeeDone

	// Only "received" counts as delivered; any other status is the payer
	// telling us it could not use the request.
	if status != ackReceived {
		return Output{}, failure(
			ReasonPayloadRejected,
			"payer rejected the payment request (status %d)",
			status,
		)
	}

	return Output{Delivered: true, Done: true}, nil
}

// abortOutput is an Output that only sends an ABORT.
func abortOutput(reason wire.AbortReason) Output {
	return Output{Send: [][]byte{wire.EncodeAbort(reason)}}
}
