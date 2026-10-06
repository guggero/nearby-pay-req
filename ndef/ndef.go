// Package ndef encodes and decodes the one NDEF shape the nearby protocol
// uses: a message of a single NFC Forum URI record carrying the payment
// request, exactly as a QR code would show it. It is not a general NDEF
// library.
package ndef

import (
	"errors"
)

const (
	// MaxBytes bounds the payload of a record.
	MaxBytes = 65536

	// WellKnown is the type name format of NFC Forum record types,
	// including URI.
	WellKnown = 1
)

var (
	// ErrMalformed means a message is not a single well-formed URI
	// record.
	ErrMalformed = errors.New("ndef: not a single URI record")

	// uriPrefixes is the NFC Forum URI RTD identifier table (0x00..0x23).
	uriPrefixes = [...]string{
		"", "http://www.", "https://www.", "http://", "https://",
		"tel:", "mailto:", "ftp://anonymous:anonymous@", "ftp://ftp.",
		"ftps://", "sftp://", "smb://", "nfs://", "ftp://", "dav://",
		"news:", "telnet://", "imap:", "rtsp://", "urn:", "pop:",
		"sip:", "sips:", "tftp:", "btspp://", "btl2cap://", "btgoep://",
		"tcpobex://", "irdaobex://", "file://", "urn:epc:id:",
		"urn:epc:tag:", "urn:epc:pat:", "urn:epc:raw:", "urn:epc:",
		"urn:nfc:",
	}
)
