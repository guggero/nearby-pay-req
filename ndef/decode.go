package ndef

import (
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// recordHeaderCF and recordHeaderIL are the chunk and ID-length
	// flags of an NDEF record header; recordHeaderTNF masks the type
	// name format.
	recordHeaderCF  = 0x20
	recordHeaderIL  = 0x08
	recordHeaderTNF = 0x07
)

// DecodeURIMessage returns the URI of a message holding exactly one NFC
// Forum URI record, the shape EncodeURIMessage produces and a payee's tag
// serves. A payer reading the tag through the platform's NFC API can hand
// the raw message here. Anything else — several records, chunked records,
// another record type, an unknown identifier code, invalid UTF-8 or NUL —
// is refused with ErrMalformed rather than guessed at.
func DecodeURIMessage(msg []byte) (string, error) {
	if len(msg) < 3 {
		return "", fmt.Errorf("%w: %d bytes", ErrMalformed, len(msg))
	}

	// One record that begins and ends the message, unchunked, of a
	// well-known type.
	header := msg[0]
	if header&(recordHeaderMB|recordHeaderME) !=
		recordHeaderMB|recordHeaderME ||
		header&recordHeaderCF != 0 ||
		header&recordHeaderTNF != WellKnown {

		return "", fmt.Errorf("%w: header %#02x", ErrMalformed, header)
	}
	typeLen := int(msg[1])
	rest := msg[2:]

	// The payload length is one byte for a short record, four
	// otherwise.
	var payloadLen int
	if header&recordHeaderSR != 0 {
		payloadLen = int(rest[0])
		rest = rest[1:]
	} else {
		if len(rest) < 4 {
			return "", fmt.Errorf("%w: truncated length",
				ErrMalformed)
		}
		n := binary.BigEndian.Uint32(rest)
		if n > MaxBytes {
			return "", fmt.Errorf("%w: payload of %d bytes",
				ErrMalformed, n)
		}
		payloadLen = int(n)
		rest = rest[4:]
	}

	// An ID would be harmless, but nothing produces one; skip it.
	idLen := 0
	if header&recordHeaderIL != 0 {
		if len(rest) < 1 {
			return "", fmt.Errorf("%w: truncated id length",
				ErrMalformed)
		}
		idLen = int(rest[0])
		rest = rest[1:]
	}

	// Exactly type, id and payload must remain: trailing bytes would be
	// a second, unflagged record.
	if len(rest) != typeLen+idLen+payloadLen {
		return "", fmt.Errorf("%w: record length mismatch",
			ErrMalformed)
	}
	if typeLen != 1 || rest[0] != 'U' {
		return "", fmt.Errorf("%w: not a URI record", ErrMalformed)
	}
	payload := rest[typeLen+idLen:]
	if len(payload) < 1 || int(payload[0]) >= len(uriPrefixes) {
		return "", fmt.Errorf("%w: bad identifier code", ErrMalformed)
	}

	uri := uriPrefixes[payload[0]] + string(payload[1:])
	if uri == "" || !utf8.ValidString(uri) ||
		strings.IndexByte(uri, 0) >= 0 {

		return "", fmt.Errorf("%w: invalid URI text", ErrMalformed)
	}

	return uri, nil
}
