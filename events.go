package nearby

import (
	"time"
)

// Availability is whether one BLE role can run on this device right now, and
// if not, what the user could do about it.
type Availability uint8

const (
	// AvailabilityUnknown is the zero value; the Manager never reports
	// it.
	AvailabilityUnknown Availability = iota

	// Available means the role can run.
	Available

	// BluetoothOff means the adapter is off; switching it on in place
	// makes the role available without restarting a share or find.
	BluetoothOff

	// PermissionDenied means the app lacks a runtime permission; the
	// user has to grant it in the system settings.
	PermissionDenied

	// UnsupportedOS means the OS (or the absence of any radio) rules the
	// role out for good.
	UnsupportedOS

	// PeripheralUnsupported means the chipset cannot advertise or host
	// a GATT server. It only applies to sharing.
	PeripheralUnsupported
)

// String returns a readable name for the availability.
func (a Availability) String() string {
	switch a {
	case Available:
		return "available"

	case BluetoothOff:
		return "bluetooth off"

	case PermissionDenied:
		return "permission denied"

	case UnsupportedOS:
		return "unsupported os"

	case PeripheralUnsupported:
		return "peripheral unsupported"

	default:
		return "unknown"
	}
}

// recoverable reports whether the availability can turn into Available
// while a share or find keeps running: only Bluetooth being off qualifies.
func (a Availability) recoverable() bool {
	return a == Available || a == BluetoothOff
}

// FailureReason is why one session failed. A failed session never ends a
// share or find: the payee keeps serving and the payer moves on to the next
// sharer.
type FailureReason uint8

const (
	// FailureUnspecified is an error none of the other reasons covers.
	FailureUnspecified FailureReason = iota

	// FailureConnectFailed means the link could not be established or
	// dropped mid-session, or the peer gave up on its own.
	FailureConnectFailed

	// FailureTimeout means connecting or the session took too long.
	FailureTimeout

	// FailureHandshakeFailed means authentication failed: a bad AEAD
	// tag or a broken commitment.
	FailureHandshakeFailed

	// FailureProtocolError means malformed or out-of-order data.
	FailureProtocolError

	// FailureVersionUnsupported means the peer speaks no protocol
	// version this implementation knows.
	FailureVersionUnsupported

	// FailurePayloadInvalid means the payer could not use the payment
	// request (on the payee: the payer said so).
	FailurePayloadInvalid

	// FailurePeerBusy means the payee is serving someone else or is
	// rate limiting.
	FailurePeerBusy

	// FailureRadioError means the radio failed a write, a notification
	// or the advertisement.
	FailureRadioError
)

// String returns a readable name for the reason.
func (r FailureReason) String() string {
	switch r {
	case FailureConnectFailed:
		return "connect failed"

	case FailureTimeout:
		return "timeout"

	case FailureHandshakeFailed:
		return "handshake failed"

	case FailureProtocolError:
		return "protocol error"

	case FailureVersionUnsupported:
		return "version unsupported"

	case FailurePayloadInvalid:
		return "payload invalid"

	case FailurePeerBusy:
		return "peer busy"

	case FailureRadioError:
		return "radio error"

	default:
		return "unspecified"
	}
}

// Status is the availability of every role, for deciding which controls to
// show before starting anything.
type Status struct {
	// Share is the availability of serving over BLE.
	Share Availability

	// Find is the availability of finding over BLE.
	Find Availability

	// NFCShare is whether a share can also be read as an NFC tag.
	NFCShare bool
}

// ShareEvent is one event of a running share. It is one of ShareStarted,
// AvailabilityChanged, PeerConnected, Delivered, Chosen, ComparisonExpired,
// SessionFailed, SuspiciousActivity or NFCRead.
type ShareEvent interface {
	shareEvent()
}

// FindEvent is one event of a running find. It is one of Scanning,
// AvailabilityChanged, TooFar, Connecting, SessionFailed or Received.
type FindEvent interface {
	findEvent()
}

// ShareStarted reports which transports serve the payment request. It is
// sent again whenever that changes.
type ShareStarted struct {
	// BLE is whether the request is advertised over Bluetooth.
	BLE bool

	// NFC is whether the request is served as an emulated NFC tag.
	NFC bool
}

// AvailabilityChanged reports that the BLE role of a share or find became
// unavailable, or unavailable for a different reason. The share or find
// keeps running and resumes once the role is available again.
type AvailabilityChanged struct {
	Availability Availability
}

// PeerConnected reports that a payer finished the handshake. Code is the
// comparison code the payer shows too; the payee's user compares them.
type PeerConnected struct {
	Code string
}

// Delivered reports that a payer received the payment request and can use
// it. Code is the code of that session. It does not mean the payer's user
// picked this request: in a room with several payees the payer may collect
// several and its user picks one by its code, which arrives as Chosen.
// The payee keeps showing Code and turns other payers away as busy until
// that payer chose the request or Params.ComparisonTimeout passed (see
// ComparisonExpired), so another payer cannot replace the code its user is
// comparing.
type Delivered struct {
	Code string
}

// Chosen reports that the payer of an earlier delivered session matched the
// codes and picked this request, so other payers should no longer get it.
// Code is the code of that session. Unless ShareOptions.ContinueAfterChosen
// is set, the share ends right after this event.
type Chosen struct {
	Code string
}

// ComparisonExpired reports that the hold on the code of the last delivered
// session ended without its payer choosing the request: from now on another
// payer's session may replace that code. The code stays valid for its
// payer's comparison until then (see Params.ComparisonTimeout).
type ComparisonExpired struct {
	Code string
}

// SessionFailed reports one failed session. The share or find goes on.
type SessionFailed struct {
	Reason FailureReason
}

// SuspiciousActivity warns that unusually many sessions failed after
// showing a code, which is what a man in the middle grinding for a matching
// code looks like. It is sent at most once per Window.
type SuspiciousActivity struct {
	// FailedSessions is how many sessions failed after their code was
	// shown within Window.
	FailedSessions int

	// Window is the rolling window FailedSessions was counted in.
	Window time.Duration
}

// NFCRead reports that a reader read the emulated tag to the end.
type NFCRead struct{}

// Scanning reports that the find scans for sharers.
type Scanning struct{}

// TooFar reports that sharers are visible but none is close enough to be
// picked. The find keeps re-evaluating.
type TooFar struct{}

// Connecting reports that the find picked a sharer and connects to it.
type Connecting struct{}

// Received is the end of a successful find: the payment request, which the
// FindOptions.Validate hook accepted, and the code the payer's user compares
// with the payee's screen. It is only emitted once the radio took the
// acknowledgement; a find whose acknowledgement could not be sent fails
// that session instead. The radio taking it does not prove the payee
// processed it: a payee that lost the link at that instant reports no
// Delivered and later answers Choose with ErrUnknownSession.
type Received struct {
	// PaymentRequest is the shared string, exactly as the payee's QR
	// code would carry it.
	PaymentRequest string

	// Code is the six-digit comparison code.
	Code string

	// PeerID is the radio's id of the sharer. Passing it in
	// FindOptions.Exclude skips this sharer on the next find, for when
	// the user says the codes do not match.
	PeerID string

	// ChosenToken identifies this session to the payee. Once the user
	// confirmed the codes match and picked this request, pass it with
	// PeerID to Manager.Choose so the payee stops sharing. It is opaque
	// and only meaningful to that payee, for Params.ChosenRetention.
	ChosenToken []byte
}

func (ShareStarted) shareEvent()        {}
func (AvailabilityChanged) shareEvent() {}
func (AvailabilityChanged) findEvent()  {}
func (PeerConnected) shareEvent()       {}
func (Delivered) shareEvent()           {}
func (Chosen) shareEvent()              {}
func (ComparisonExpired) shareEvent()   {}
func (SessionFailed) shareEvent()       {}
func (SessionFailed) findEvent()        {}
func (SuspiciousActivity) shareEvent()  {}
func (NFCRead) shareEvent()             {}
func (Scanning) findEvent()             {}
func (TooFar) findEvent()               {}
func (Connecting) findEvent()           {}
func (Received) findEvent()             {}
