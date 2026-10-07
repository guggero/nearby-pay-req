package wire

import (
	"encoding/binary"
	"fmt"
)

const (
	// Version1 is the only protocol version defined so far.
	Version1 byte = 1

	// messageHeaderLen is the version + type prefix of every message.
	messageHeaderLen = 2

	// ChosenTokenLen is the length of the token a CHOSEN message carries.
	ChosenTokenLen = 32
)

// MessageType identifies what a message body carries.
type MessageType byte

const (
	// TypeNoiseE is Noise NN message 1 (payer → payee): the payer's
	// ephemeral key.
	TypeNoiseE MessageType = 0x01

	// TypeNoiseEEE is Noise NN message 2 (payee → payer): the payee's
	// ephemeral key plus the encrypted commitment payload.
	TypeNoiseEEE MessageType = 0x02

	// TypeSealed is a Noise transport message carrying a TLV stream.
	TypeSealed MessageType = 0x03

	// TypeChosen opens a connection of its own, after a delivered
	// session, to tell the payee its request was picked: the body is
	// the 32-byte chosen token of that session. The payee answers with
	// an empty TypeChosen to confirm, or ABORT(UNKNOWN_SESSION).
	TypeChosen MessageType = 0x04

	// TypeAbort is a plaintext, unauthenticated session abort. It can
	// only ever end a session, never change what a session delivered.
	TypeAbort MessageType = 0x7f
)

// String returns a short name for logs.
func (t MessageType) String() string {
	switch t {
	case TypeNoiseE:
		return "NOISE_E"

	case TypeNoiseEEE:
		return "NOISE_E_EE"

	case TypeSealed:
		return "SEALED"

	case TypeChosen:
		return "CHOSEN"

	case TypeAbort:
		return "ABORT"

	default:
		return fmt.Sprintf("UNKNOWN(0x%02x)", byte(t))
	}
}

// AbortReason is the code carried by an ABORT message.
type AbortReason uint16

const (
	// AbortProtocolError means the peer sent something malformed.
	AbortProtocolError AbortReason = 1

	// AbortVersionUnsupported means the message version is not spoken;
	// the body additionally names the highest version that is.
	AbortVersionUnsupported AbortReason = 2

	// AbortBusy means the payee is already serving another payer or
	// is rate limiting new sessions.
	AbortBusy AbortReason = 3

	// AbortTimeout means a step took too long.
	AbortTimeout AbortReason = 4

	// AbortPayloadRejected means the payer could not use the payment
	// request it received.
	AbortPayloadRejected AbortReason = 5

	// AbortCancelled means the sender stopped the session on purpose.
	AbortCancelled AbortReason = 6

	// AbortUnknownSession means a CHOSEN token matches no session the
	// payee delivered recently, or the payee no longer shares.
	AbortUnknownSession AbortReason = 7
)

// String returns a short name for logs.
func (r AbortReason) String() string {
	switch r {
	case AbortProtocolError:
		return "protocol error"

	case AbortVersionUnsupported:
		return "version unsupported"

	case AbortBusy:
		return "busy"

	case AbortTimeout:
		return "timeout"

	case AbortPayloadRejected:
		return "payload rejected"

	case AbortCancelled:
		return "cancelled"

	case AbortUnknownSession:
		return "unknown session"

	default:
		return fmt.Sprintf("unknown(%d)", uint16(r))
	}
}

// Message is one decoded protocol message.
type Message struct {
	Version byte
	Type    MessageType
	Body    []byte
}

// EncodeMessage prefixes body with the v1 header.
func EncodeMessage(t MessageType, body []byte) []byte {
	msg := make([]byte, 0, messageHeaderLen+len(body))
	msg = append(msg, Version1, byte(t))

	return append(msg, body...)
}

// DecodeMessage splits a reassembled message into its header and body. It
// does not judge the version or type: the session decides what it accepts,
// because an unsupported version must be answered rather than dropped.
func DecodeMessage(msg []byte) (Message, error) {
	if len(msg) < messageHeaderLen {
		return Message{}, fmt.Errorf("%w: message too short",
			ErrProtocol)
	}

	return Message{
		Version: msg[0],
		Type:    MessageType(msg[1]),
		Body:    msg[messageHeaderLen:],
	}, nil
}

// EncodeAbort builds an ABORT message. For AbortVersionUnsupported the body
// also carries the highest version this side speaks, so the peer can retry.
func EncodeAbort(reason AbortReason) []byte {
	body := binary.BigEndian.AppendUint16(nil, uint16(reason))
	if reason == AbortVersionUnsupported {
		body = append(body, Version1)
	}

	return EncodeMessage(TypeAbort, body)
}

// DecodeAbort parses an ABORT body. maxVersion is only meaningful for
// AbortVersionUnsupported and is zero otherwise.
func DecodeAbort(body []byte) (AbortReason, byte, error) {
	if len(body) < 2 {
		return 0, 0, fmt.Errorf("%w: abort too short", ErrProtocol)
	}
	reason := AbortReason(binary.BigEndian.Uint16(body))

	var maxVersion byte
	if reason == AbortVersionUnsupported && len(body) >= 3 {
		maxVersion = body[2]
	}

	return reason, maxVersion, nil
}

// EncodeChosen builds the payer's CHOSEN message for a delivered session's
// token.
func EncodeChosen(token [ChosenTokenLen]byte) []byte {
	return EncodeMessage(TypeChosen, token[:])
}

// EncodeChosenAck builds the payee's confirmation of a CHOSEN message.
func EncodeChosenAck() []byte {
	return EncodeMessage(TypeChosen, nil)
}

// DecodeChosenAck checks the body of the payee's CHOSEN confirmation, which
// is empty.
func DecodeChosenAck(body []byte) error {
	if len(body) != 0 {
		return fmt.Errorf("%w: chosen confirmation with %d body bytes",
			ErrProtocol, len(body))
	}

	return nil
}

// DecodeChosen parses the body of the payer's CHOSEN message.
func DecodeChosen(body []byte) ([ChosenTokenLen]byte, error) {
	var token [ChosenTokenLen]byte
	if len(body) != ChosenTokenLen {
		return token, fmt.Errorf("%w: chosen token of %d bytes",
			ErrProtocol, len(body))
	}
	copy(token[:], body)

	return token, nil
}
