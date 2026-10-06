// Package nearby shares payment requests phone to phone, as specified
// by the Nearby Payment Request Exchange draft in spec/. A payee on a
// receive screen serves the string its QR code shows over Bluetooth LE and,
// on Android, as an emulated NFC tag; a payer on the send screen finds it,
// both users compare a six-digit code, and the payer's wallet takes the
// request as if it had scanned the QR code.
//
// The Manager is the entry point. It drives the platform's radio through the
// Radio interface, applies timeouts, rate limits and peer selection, and
// reports progress as ShareEvent and FindEvent values. The protocol itself
// lives in the sans-IO subpackages session (handshake, commitment, code)
// and wire (chunking, framing), the NFC tag in hce and ndef.
package nearby

import (
	"github.com/google/uuid"
)

const (
	// ServiceUUID is the "Nearby Payment Request v1" GATT service. A
	// non-negotiable change to the v1 framing would get a new UUID;
	// anything else is negotiated inside the protocol.
	ServiceUUID = "7be78411-b151-48bf-a570-252b55847970"

	// RxUUID is the payer → payee write characteristic.
	RxUUID = "35effe11-bcc0-4db2-8ef4-c7271011c5e6"

	// TxUUID is the payee → payer notify characteristic.
	TxUUID = "13cccaba-d2e9-47d5-ab11-2b3730d3c97e"
)

// serviceUUIDBytes returns ServiceUUID in its 16-byte form, which the Noise
// prologue binds.
func serviceUUIDBytes() [16]byte {
	return uuid.MustParse(ServiceUUID)
}
