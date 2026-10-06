// Package session implements the two sides of one nearby payment-request
// exchange as sans-IO state machines: a PayeeSession that serves a payment
// request and a PayerSession that fetches one. They take reassembled
// messages in and hand messages to send out, with no goroutines, radios or
// clocks of their own, so the whole protocol — Noise handshake, commitment,
// comparison code and TLV payloads — is testable from bytes alone. The
// Manager in the root package owns I/O and timeouts.
//
// The exchange is Noise_NN_25519_ChaChaPoly_SHA256 followed by a
// commit/reveal of two nonces, from which both sides derive a six-digit
// code. NN alone authenticates nobody (neither phone knows a key of the
// other in advance); the code, compared by a human, is the authentication,
// and the commitment ordering is what stops a man in the middle from
// grinding for a matching code. See spec/ for the full rationale.
package session

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/flynn/noise"
	"github.com/guggero/nearby-pay-req/wire"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// MaxPaymentRequestLen bounds the shared string. It matches the input
	// cap of the payment parser, which is what consumes it on the payer.
	MaxPaymentRequestLen = 8192

	// nonceLen is the size of the commit/reveal nonces Na and Nb.
	nonceLen = 32

	// codeModulus turns the code hash into six decimal digits.
	codeModulus = 1_000_000

	// ackReceived is the ack status for a request the payer could parse.
	ackReceived uint8 = 0

	// ackRejected is the ack status for a request the payer could not
	// use.
	ackRejected uint8 = 1
)

// TLV types of the records carried inside the handshake payload and the
// sealed messages. Even types are mandatory; odd types are reserved for
// optional extensions and skipped by older peers.
const (
	typeCommitment     tlv.Type = 0
	typePayerNonce     tlv.Type = 2
	typePayeeNonce     tlv.Type = 4
	typePaymentRequest tlv.Type = 6
	typeAck            tlv.Type = 8
)

// The domain-separation strings are those of the Nearby Payment Request
// Exchange bLIP draft and carry no wallet name, so any compliant wallet
// derives the same transcript and code. Changing one is a new protocol.
var (
	// prologueTag starts the Noise prologue, binding every transcript to
	// this protocol and version.
	prologueTag = []byte("nearby-payreq/1")

	// commitTag domain-separates the payee's nonce commitment.
	commitTag = []byte("nearby-payreq/1/commit")

	// codeTag domain-separates the comparison code.
	codeTag = []byte("nearby-payreq/1/code")

	// cipherSuite is the Noise suite both sides use.
	cipherSuite = noise.NewCipherSuite(
		noise.DH25519, noise.CipherChaChaPoly, noise.HashSHA256,
	)
)

// Reason classifies why a session failed, so the orchestration can map it to
// what the UI shows without matching error strings.
type Reason uint8

const (
	// ReasonProtocol means the peer sent something malformed or out of
	// order.
	ReasonProtocol Reason = iota + 1

	// ReasonVersion means the peer speaks no version we do.
	ReasonVersion

	// ReasonHandshake means authentication failed: a bad AEAD tag or a
	// commitment that does not match the revealed nonce.
	ReasonHandshake

	// ReasonBusy means the payee is serving someone else or rate
	// limiting.
	ReasonBusy

	// ReasonPayloadRejected means the payer could not use the payment
	// request (on the payee: the payer acked with "rejected").
	ReasonPayloadRejected

	// ReasonPeerAborted means the peer aborted for another reason.
	ReasonPeerAborted
)

// String returns a short name for logs.
func (r Reason) String() string {
	switch r {
	case ReasonProtocol:
		return "protocol error"

	case ReasonVersion:
		return "version unsupported"

	case ReasonHandshake:
		return "handshake failed"

	case ReasonBusy:
		return "peer busy"

	case ReasonPayloadRejected:
		return "payload rejected"

	case ReasonPeerAborted:
		return "peer aborted"

	default:
		return fmt.Sprintf("unknown(%d)", uint8(r))
	}
}

// Error is a failed session. Reason is what callers branch on; the wrapped
// error carries the detail for logs.
type Error struct {
	Reason Reason
	Err    error
}

// Error implements error.
func (e *Error) Error() string {
	return fmt.Sprintf("nearby session: %v: %v", e.Reason, e.Err)
}

// Unwrap returns the underlying cause.
func (e *Error) Unwrap() error {
	return e.Err
}

// failure builds an *Error.
func failure(reason Reason, format string, args ...any) *Error {
	return &Error{Reason: reason, Err: fmt.Errorf(format, args...)}
}

// ReasonOf extracts the Reason from err, or 0 if err is not a session error.
func ReasonOf(err error) Reason {
	var sErr *Error
	if errors.As(err, &sErr) {
		return sErr.Reason
	}

	return 0
}

// Output is what one step of a session produced.
type Output struct {
	// Send are complete messages to deliver to the peer, in order.
	Send [][]byte

	// Code is set once the comparison code is known.
	Code string

	// PaymentRequest is set on the payer once the request arrived.
	PaymentRequest string

	// Delivered is set on the payee once the payer acknowledged receipt.
	Delivered bool

	// Done means the session ended successfully; nothing more will be
	// sent or accepted.
	Done bool
}

// Prologue returns the Noise prologue for a service: the protocol tag
// followed by the 16 service UUID bytes, so a transcript can never be
// replayed against another service or version.
func Prologue(serviceUUID [16]byte) []byte {
	p := make([]byte, 0, len(prologueTag)+len(serviceUUID))
	p = append(p, prologueTag...)

	return append(p, serviceUUID[:]...)
}

// Config is shared by both session kinds.
type Config struct {
	// Prologue binds the handshake to the service; see Prologue.
	Prologue []byte

	// Rand supplies the Noise ephemeral key and the commit/reveal
	// nonce. Production passes crypto/rand.Reader; tests pass a
	// deterministic reader to reproduce the vectors.
	Rand io.Reader
}

// newHandshake builds the NN handshake state for one side.
func newHandshake(cfg Config, initiator bool) (*noise.HandshakeState, error) {
	return noise.NewHandshakeState(noise.Config{
		CipherSuite: cipherSuite,
		Random:      cfg.Rand,
		Pattern:     noise.HandshakeNN,
		Initiator:   initiator,
		Prologue:    cfg.Prologue,
	})
}

// commitment returns SHA-256(commitTag ‖ nb).
func commitment(nb [nonceLen]byte) [sha256.Size]byte {
	h := sha256.New()
	h.Write(commitTag)
	h.Write(nb[:])

	var c [sha256.Size]byte
	copy(c[:], h.Sum(nil))

	return c
}

// comparisonCode derives the six-digit code both screens show from the
// handshake hash and both nonces. The modulo bias over a 32-bit value is
// below 0.03 % and irrelevant at this code length.
func comparisonCode(handshakeHash []byte, na,
	nb [nonceLen]byte) string {

	h := sha256.New()
	h.Write(codeTag)
	h.Write(handshakeHash)
	h.Write(na[:])
	h.Write(nb[:])
	sum := h.Sum(nil)

	return fmt.Sprintf("%06d", binary.BigEndian.Uint32(sum)%codeModulus)
}

// ValidatePaymentRequest checks what the sealed payload may carry:
// non-empty, bounded, valid UTF-8 without NUL. The parser does the real
// validation; this only keeps garbage from crossing the session boundary,
// and lets a share refuse a string no session could ever deliver.
func ValidatePaymentRequest(s string) error {
	if s == "" || len(s) > MaxPaymentRequestLen {
		return fmt.Errorf("payment request length %d out of range",
			len(s))
	}
	if !utf8.ValidString(s) || strings.IndexByte(s, 0) >= 0 {
		return fmt.Errorf("payment request is not valid UTF-8 text")
	}

	return nil
}

// decodeExpected parses a TLV stream into records and requires every one of
// them to be present: in this protocol each message has a fixed set of
// mandatory records.
func decodeExpected(data []byte, records ...tlv.Record) error {
	parsed, err := wire.DecodeTLV(data, records...)
	if err != nil {
		return err
	}
	for _, r := range records {
		if _, ok := parsed[r.Type()]; !ok {
			return fmt.Errorf("%w: missing TLV type %d",
				wire.ErrProtocol, r.Type())
		}
	}

	return nil
}

// abortFailure maps a peer's ABORT body onto a session failure.
func abortFailure(body []byte) *Error {
	reason, _, err := wire.DecodeAbort(body)
	if err != nil {
		return failure(ReasonProtocol, "malformed abort: %w", err)
	}

	switch reason {
	// We only speak v1, so a peer that doesn't is a dead end; there
	// is no lower version to retry with.
	case wire.AbortVersionUnsupported:
		return failure(ReasonVersion, "peer aborted: %v", reason)

	// A busy payee is worth telling apart: the payer moves on to the
	// next candidate rather than reporting a broken peer.
	case wire.AbortBusy:
		return failure(ReasonBusy, "peer aborted: %v", reason)

	// A payer that could not use our request.
	case wire.AbortPayloadRejected:
		return failure(
			ReasonPayloadRejected, "peer aborted: %v", reason,
		)

	default:
		return failure(ReasonPeerAborted, "peer aborted: %v", reason)
	}
}

// protocolFailure wraps a decode error as a protocol failure.
func protocolFailure(err error) *Error {
	return &Error{Reason: ReasonProtocol, Err: err}
}

// readNonce reads one nonce from rand.
func readNonce(rand io.Reader) ([nonceLen]byte, error) {
	var n [nonceLen]byte
	if _, err := io.ReadFull(rand, n[:]); err != nil {
		return n, fmt.Errorf("reading nonce: %w", err)
	}

	return n, nil
}

// seal encrypts a TLV stream as a SEALED message.
func seal(cs *noise.CipherState, records ...tlv.Record) ([]byte, error) {
	plaintext, err := wire.EncodeTLV(records...)
	if err != nil {
		return nil, err
	}
	ciphertext, err := cs.Encrypt(nil, nil, plaintext)
	if err != nil {
		return nil, err
	}

	return wire.EncodeMessage(wire.TypeSealed, ciphertext), nil
}

// open decrypts the body of a SEALED message. A failed tag is an
// authentication failure, not a framing one.
func open(cs *noise.CipherState, body []byte) ([]byte, error) {
	plaintext, err := cs.Decrypt(nil, nil, body)
	if err != nil {
		return nil, failure(
			ReasonHandshake, "decrypting sealed message: %w", err,
		)
	}

	return plaintext, nil
}
