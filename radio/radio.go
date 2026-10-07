// Package radio defines the platform radio the nearby Manager drives: a
// Bluetooth LE peripheral and central plus, on Android, NFC host card
// emulation. It is a leaf package so that platform bindings and test fakes
// can implement it without importing the Manager.
package radio

// Status flags returned by Radio.Status. They are separate bits rather than
// one enum because the conditions are independent: an Android phone can be
// powered and permitted but lack an advertiser.
const (
	// StatusPoweredOn means the Bluetooth adapter is on.
	StatusPoweredOn = 1 << 0

	// StatusPermissionGranted means the app holds every runtime
	// permission the radio needs (Android: BLUETOOTH_SCAN, _CONNECT and
	// _ADVERTISE; iOS: Bluetooth authorization).
	StatusPermissionGranted = 1 << 1

	// StatusPeripheralSupported means the chipset can advertise and host
	// a GATT server.
	StatusPeripheralSupported = 1 << 2

	// StatusCentralSupported means the device can scan and connect.
	StatusCentralSupported = 1 << 3

	// StatusOSSupported means the OS version is one the feature runs on
	// (for example Android 12 or later).
	StatusOSSupported = 1 << 4

	// StatusHceSupported means NFC host card emulation exists (Android
	// only).
	StatusHceSupported = 1 << 5

	// StatusNfcEnabled means NFC is switched on.
	StatusNfcEnabled = 1 << 6
)

// Radio is the platform's Bluetooth LE (and, on Android, NFC host card
// emulation) stack. It is a byte pump: it never inspects chunk contents, and
// the UUIDs, chunking and protocol all stay in Go. Every method
// must be safe to call from any goroutine. Methods documented as blocking
// are only ever called from one session goroutine per connection.
//
// Connections the radio opens as a central are identified by a connID the
// Manager picks for every Connect. The radio stores it with the connection
// and reports it in every callback about that connection, so a callback the
// OS delivers late for a connection that was already closed or replaced
// cannot be mistaken for one of its successor. Blocking calls take the time
// they may block for, and closing the link they run on makes them fail at
// once, so a session's deadline and cancellation reach into the native
// stack.
//
// The interface only uses types gomobile can bind, so a mobile binding
// package can declare an identical interface for Kotlin and Swift to
// implement and hand that implementation to this package through a small
// adapter (the callback parameters differ only in their named type).
type Radio interface {
	// Status returns a bitmask of Status* flags.
	Status() int

	// StartAdvertising opens a GATT server exposing serviceUUID with a
	// write characteristic rxUUID (central → peripheral, write with
	// response, no encryption or authentication permissions, so no OS
	// ever offers to pair) and a notify characteristic txUUID, then
	// advertises serviceUUID: connectable, no local name, no
	// manufacturer data. Prepared (long) writes to rxUUID must be
	// refused: every chunk arrives as one ATT Write Request. Calling it
	// while already advertising replaces cb. Setting up is all or
	// nothing: when it returns an error, no part of the GATT server or
	// advertisement may stay behind, and a stack answer arriving after a
	// timeout must not complete a later setup.
	StartAdvertising(serviceUUID, rxUUID, txUUID string,
		cb PeripheralCallback) error

	// StopAdvertising stops advertising, disconnects every central and
	// closes the GATT server. Idempotent. A Notify blocked meanwhile
	// fails at once, and callbacks of the closed server are never
	// delivered through the cb of a later StartAdvertising.
	StopAdvertising()

	// Notify sends one chunk to the given central on txUUID. It blocks
	// until the stack accepted the value (Android onNotificationSent,
	// iOS updateValue returning true or peripheralManagerIsReady), and
	// fails once timeoutMillis passed without that.
	Notify(centralID string, chunk []byte, timeoutMillis int) error

	// DisconnectCentral drops one central. A Notify to it blocked
	// meanwhile fails at once. CoreBluetooth cannot force a disconnect,
	// so on iOS the radio stops serving that central and ignores its
	// further writes.
	DisconnectCentral(centralID string)

	// StartScan scans for serviceUUID and reports every matching
	// advertisement, repeats included, so RSSI stays fresh.
	StartScan(serviceUUID string, cb CentralCallback) error

	// StopScan stops scanning. Idempotent.
	StopScan()

	// Connect connects to peerID, requests the largest MTU (Android),
	// discovers the service, enables notifications on txUUID and then
	// reports OnConnected. It returns immediately; failures arrive as
	// OnDisconnected. connID identifies this connection attempt: every
	// callback about it carries connID. A Connect to a peer that still
	// has a connection replaces that one, whose callbacks, including any
	// the OS delivers after the replacement, are dropped or keep
	// carrying the old connID. It must also work for a peer reported
	// earlier in the app's lifetime whose scan has since stopped,
	// because the Manager reconnects to tell a payee its request was
	// chosen (on iOS, keep the CBPeripheral or use
	// retrievePeripherals(withIdentifiers:)).
	Connect(peerID string, connID int, serviceUUID, rxUUID,
		txUUID string) error

	// Write writes one chunk to rxUUID with response on connection
	// connID. It blocks until the write response arrived and fails once
	// timeoutMillis passed without one, or at once if connID is not the
	// peer's current connection. Only a response to this very write
	// completes it: a late response to an earlier write that timed out
	// does not.
	Write(peerID string, connID int, chunk []byte,
		timeoutMillis int) error

	// Disconnect drops connection connID to peerID; a Write on it
	// blocked meanwhile fails at once. It does nothing if connID is not
	// the peer's current connection, so closing an old connection never
	// tears down its successor. Idempotent.
	Disconnect(peerID string, connID int)

	// SetHceActive makes the app's HCE service the preferred one while
	// active (Android CardEmulation.setPreferredService on the
	// foreground activity) and releases it on false. A no-op where there
	// is no HCE. The tag content itself is served by the Manager through
	// ProcessAPDU.
	SetHceActive(active bool)
}

// PeripheralCallback is implemented by the Manager and called by the
// Radio in the payee (peripheral) role. Implementations never block: they
// enqueue and return, so a radio thread is never held up by Go.
type PeripheralCallback interface {
	// OnCentralReady fires once a central subscribed to txUUID. maxChunk
	// is the largest value Notify may send to it (Android: negotiated
	// MTU − 3; iOS: central.maximumUpdateValueLength).
	OnCentralReady(centralID string, maxChunk int)

	// OnWrite delivers one rxUUID write, in order.
	OnWrite(centralID string, chunk []byte)

	// OnCentralGone fires when a central disconnected.
	OnCentralGone(centralID string)

	// OnAdvertisingFailed reports that advertising could not start or
	// stopped unexpectedly.
	OnAdvertisingFailed(reason string)
}

// CentralCallback is implemented by the Manager and called by the Radio in
// the payer (central) role. Implementations never block.
type CentralCallback interface {
	// OnAdvertisement reports one sighting of a peer advertising the
	// service.
	OnAdvertisement(peerID string, rssi int)

	// OnConnected fires after service discovery and notification
	// subscription of connection connID. maxChunk is the largest value
	// Write may send in a single ATT Write Request, which is the
	// negotiated MTU − 3 (iOS: maximumWriteValueLength(.withoutResponse)).
	// Never a larger value the platform would send as a prepared write:
	// payees refuse those.
	OnConnected(peerID string, connID int, maxChunk int)

	// OnNotify delivers one txUUID notification of connection connID,
	// in order.
	OnNotify(peerID string, connID int, chunk []byte)

	// OnDisconnected fires when connection attempt connID failed or the
	// connection dropped.
	OnDisconnected(peerID string, connID int, reason string)

	// OnScanFailed reports that scanning could not start.
	OnScanFailed(reason string)
}
