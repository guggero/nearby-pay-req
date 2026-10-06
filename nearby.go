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
	"time"

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

// Timing and selection parameters. They are initial values, to be tuned from
// measurements on real devices.
const (
	// sessionTimeout bounds one whole exchange, from the payer's first
	// message to its acknowledgement. The protocol is five small
	// messages, so this is generous even over a slow link.
	sessionTimeout = 10 * time.Second

	// connectTimeout bounds connecting, service discovery and
	// subscription on the payer.
	connectTimeout = 5 * time.Second

	// collectWindow is how long the payer keeps collecting
	// advertisements after the first sighting before it picks the
	// closest sharer.
	collectWindow = 800 * time.Millisecond

	// findGiveUpAfter is when a search without any completed session
	// reports a timeout. It keeps scanning afterwards; the UI only
	// changes its wording.
	findGiveUpAfter = 20 * time.Second

	// rssiFloor is the weakest smoothed RSSI (dBm) a sharer may have to
	// be picked. Below it the payer reports "too far".
	rssiFloor = -75

	// rssiSmoothing is the weight of a new RSSI sample in the moving
	// average, damping the large swings phone RSSI shows.
	rssiSmoothing = 0.3

	// minSessionInterval is the shortest gap between two sessions a
	// payee accepts; together with maxSessionsPerMinute it throttles
	// anyone trying sessions until a code matches.
	minSessionInterval = time.Second

	// maxSessionsPerMinute caps payee sessions per rolling minute.
	maxSessionsPerMinute = 30

	// suspiciousThreshold is how many sessions that failed after
	// showing a code, within suspiciousWindow, trigger a warning.
	suspiciousThreshold = 3

	// suspiciousWindow is the rolling window for suspiciousThreshold.
	suspiciousWindow = time.Minute

	// scanStopDebounce delays stopping a scan, so leaving and quickly
	// re-entering the send screen reuses the running scan instead of
	// spending one of the five scan starts Android allows per 30 s.
	scanStopDebounce = 2 * time.Second

	// eventQueueLen buffers radio callbacks. Callbacks never block, so
	// an overflowing queue drops events; the affected session then
	// fails on a broken chunk sequence and the peer can retry.
	eventQueueLen = 256
)

// serviceUUIDBytes returns ServiceUUID in its 16-byte form, which the Noise
// prologue binds.
func serviceUUIDBytes() [16]byte {
	return uuid.MustParse(ServiceUUID)
}
