package ndef

import (
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// shortRecordMaxPayload is the largest payload a short record (SR
	// flag, one-byte payload length) can carry.
	shortRecordMaxPayload = 255

	// recordHeaderMB, recordHeaderME and recordHeaderSR are the message
	// begin, message end and short record flags of an NDEF record header.
	recordHeaderMB = 0x80
	recordHeaderME = 0x40
	recordHeaderSR = 0x10
)

// EncodeURIMessage builds a single-record NDEF message holding uri as an NFC
// Forum URI record. It is what an emulated tag serves, so any NDEF reader
// sees the same string a QR code would carry. The
// longest matching URI identifier code abbreviates the prefix; payment
// schemes such as "lightning:" and "bitcoin:" have none and are stored in
// full behind code 0x00.
func EncodeURIMessage(uri string) ([]byte, error) {
	// Refuse what a reader would refuse on the way back in, so an
	// encoded message always decodes to exactly the string we were
	// given.
	if uri == "" {
		return nil, fmt.Errorf("ndef: empty URI")
	}
	if !utf8.ValidString(uri) || strings.IndexByte(uri, 0) >= 0 {
		return nil, fmt.Errorf("ndef: invalid UTF-8 URI")
	}

	code, suffix := uriPrefixCode(uri)
	payloadLen := 1 + len(suffix)
	if payloadLen > MaxBytes {
		return nil, fmt.Errorf("ndef: URI exceeds maximum size")
	}

	// A short record saves three bytes of length field for the common
	// case of an address or a small invoice; anything longer needs the
	// four-byte payload length.
	header := byte(recordHeaderMB | recordHeaderME | WellKnown)
	short := payloadLen <= shortRecordMaxPayload
	if short {
		header |= recordHeaderSR
	}

	msg := make([]byte, 0, 7+payloadLen)
	msg = append(msg, header, 1)
	if short {
		msg = append(msg, byte(payloadLen))
	} else {
		msg = binary.BigEndian.AppendUint32(msg, uint32(payloadLen))
	}
	msg = append(msg, 'U', code)
	msg = append(msg, suffix...)

	return msg, nil
}

// uriPrefixCode returns the URI identifier code with the longest prefix of
// uri, and the remainder of uri after it. Code 0x00 (no abbreviation) always
// matches.
func uriPrefixCode(uri string) (byte, string) {
	best := 0
	for i := 1; i < len(uriPrefixes); i++ {
		prefix := uriPrefixes[i]
		if len(prefix) > len(uriPrefixes[best]) &&
			strings.HasPrefix(uri, prefix) {

			best = i
		}
	}

	return byte(best), uri[len(uriPrefixes[best]):]
}
