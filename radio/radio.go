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
	// while already advertising replaces cb.
	StartAdvertising(serviceUUID, rxUUID, txUUID string,
		cb PeripheralCallback) error

	// StopAdvertising stops advertising, disconnects every central and
	// closes the GATT server. Idempotent.
	StopAdvertising()

	// Notify sends one chunk to the given central on txUUID. It blocks
	// until the stack accepted the value (Android onNotificationSent,
	// iOS updateValue returning true or peripheralManagerIsReady).
	Notify(centralID string, chunk []byte) error

	// DisconnectCentral drops one central. CoreBluetooth cannot force a
	// disconnect, so on iOS the radio stops serving that central and
	// ignores its further writes.
	DisconnectCentral(centralID string)

	// StartScan scans for serviceUUID and reports every matching
	// advertisement, repeats included, so RSSI stays fresh.
	StartScan(serviceUUID string, cb CentralCallback) error

	// StopScan stops scanning. Idempotent.
	StopScan()

	// Connect connects to peerID, requests the largest MTU (Android),
	// discovers the service, enables notifications on txUUID and then
	// reports OnConnected. It returns immediately; failures arrive as
	// OnDisconnected.
	Connect(peerID, serviceUUID, rxUUID, txUUID string) error

	// Write writes one chunk to rxUUID with response. It blocks until
	// the write response arrived.
	Write(peerID string, chunk []byte) error

	// Disconnect drops the connection to peerID. Idempotent.
	Disconnect(peerID string)

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
	// subscription. maxChunk is the largest value Write may send in a
	// single ATT Write Request, which is the negotiated MTU − 3 (iOS:
	// maximumWriteValueLength(.withoutResponse)). Never a larger value
	// the platform would send as a prepared write: payees refuse those.
	OnConnected(peerID string, maxChunk int)

	// OnNotify delivers one txUUID notification, in order.
	OnNotify(peerID string, chunk []byte)

	// OnDisconnected fires when a connection attempt failed or a
	// connection dropped.
	OnDisconnected(peerID string, reason string)

	// OnScanFailed reports that scanning could not start.
	OnScanFailed(reason string)
}
