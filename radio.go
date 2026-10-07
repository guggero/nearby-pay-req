package nearby

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/guggero/nearby-pay-req/radio"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
)

const (
	// controlSendTimeout bounds sending a message that belongs to no
	// session deadline: a BUSY turning a payer away, the answer to a
	// CHOSEN, or an ABORT for malformed input. Each is one chunk.
	controlSendTimeout = time.Second

	// abortSendTimeout bounds the best-effort ABORT sent once a session
	// deadline passed, which is all a session may overrun its deadline
	// by.
	abortSendTimeout = 250 * time.Millisecond
)

var (
	// errDeadline ends sending a message whose deadline passed before
	// all of its chunks went out.
	errDeadline = errors.New("deadline passed")
)

// eventKind is what a radio event reports.
type eventKind uint8

const (
	// Peripheral (payee) events.
	evCentralReady eventKind = iota + 1
	evWrite
	evCentralGone
	evAdvertisingFailed

	// Central (payer) events.
	evAdvertisement
	evConnected
	evNotify
	evDisconnected
	evScanFailed
)

// event is one radio callback, copied out of the native call.
type event struct {
	kind eventKind

	// id is the central (payee side) or peer (payer side) id.
	id string

	// connID is the payer-side connection an event belongs to.
	connID int

	// chunk is a copy of the value written or notified.
	chunk []byte

	// rssi is set on advertisements, maxChunk on readiness events.
	rssi     int
	maxChunk int

	// scanGen is the native scan an advertisement or scan failure came
	// from (see radioMux.scanGen).
	scanGen uint64

	// reason is the native failure description, for logs only.
	reason string
}

// sink is a queue of radio events with a non-blocking send. Native threads
// must never wait for Go, so a full queue drops the event (see
// Params.EventQueueLen).
type sink chan event

// push enqueues ev or drops it.
func (s sink) push(ev event) {
	select {
	case s <- ev:

	default:
		log.Warnf("Radio event queue full, dropping event %d", ev.kind)
	}
}

// radioMux wraps the platform radio. It owns the single callback object of
// each role, so the native side always reports to the same Go value, and
// routes events to whichever share or find is currently subscribed. It also
// debounces stopping a scan (see Params.ScanStopDebounce).
type radioMux struct {
	native       radio.Radio
	clock        clock.Clock
	stopDebounce time.Duration

	mu sync.Mutex

	// shareSink and findSink receive the events of the current share
	// and find; nil when none runs.
	shareSink sink
	findSink  sink

	// advGen numbers advertisements like scanGen numbers scans: each
	// native StartAdvertising gets a callback carrying the next number,
	// and stopping advertising moves past it, so a closed GATT server's
	// late callbacks never reach a share.
	advGen uint64

	// scanning is whether the native scan runs; stopGen invalidates a
	// pending debounced stop when a new scan starts.
	scanning bool
	stopGen  uint64

	// scanGen numbers native scans. Each native StartScan gets a
	// callback carrying the next number, so an advertisement or failure
	// a scan reports after it was stopped or replaced is told apart
	// from one of the scan running now and dropped.
	scanGen uint64

	// lastConnID is the last connection id handed out by connect.
	lastConnID atomic.Int64

	// shareOp is the Notify a share blocks on, and findOp the Write a
	// find or Choose blocks on, so cancelling the call can close that
	// link and make the native call return at once (see interruptShare
	// and interruptFind). Each records the call it belongs to, numbered
	// by lastOwner: a cancelled call's interrupt can run after the call
	// returned, and must then leave its successor's operation alone.
	shareOp   *inflight
	findOp    *inflight
	lastOwner atomic.Uint64

	*fn.ContextGuard
}

// newRadioMux wraps native, which must not be nil.
func newRadioMux(native radio.Radio, clk clock.Clock,
	stopDebounce time.Duration) *radioMux {

	return &radioMux{
		native:       native,
		clock:        clk,
		stopDebounce: stopDebounce,
		ContextGuard: fn.NewContextGuard(),
	}
}

// status returns the native status bitmask.
func (r *radioMux) status() int {
	return r.native.Status()
}

// startAdvertising subscribes s to peripheral events and starts the GATT
// server and advertisement.
func (r *radioMux) startAdvertising(s sink) error {
	r.mu.Lock()
	r.shareSink = s
	r.advGen++
	gen := r.advGen
	r.mu.Unlock()

	err := r.native.StartAdvertising(
		ServiceUUID, RxUUID, TxUUID,
		&peripheralCallback{r: r, advGen: gen},
	)
	if err != nil {
		r.mu.Lock()
		r.shareSink = nil
		r.advGen++
		r.mu.Unlock()
	}

	return err
}

// stopAdvertising stops advertising and unsubscribes the share.
func (r *radioMux) stopAdvertising() {
	r.native.StopAdvertising()

	r.mu.Lock()
	r.shareSink = nil
	r.advGen++
	r.mu.Unlock()
}

// inflight is one blocking native call: of which share, find or Choose
// (owner), on which link.
type inflight struct {
	owner  uint64
	peer   string
	connID int
}

// newOwner numbers a share, find or Choose for its blocking calls.
func (r *radioMux) newOwner() uint64 {
	return r.lastOwner.Add(1)
}

// notify sends one chunk to a central for owner, blocking for at most
// timeout.
func (r *radioMux) notify(owner uint64, central string, chunk []byte,
	timeout time.Duration) error {

	op := &inflight{owner: owner, peer: central}
	r.mu.Lock()
	r.shareOp = op
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		if r.shareOp == op {
			r.shareOp = nil
		}
		r.mu.Unlock()
	}()

	return r.native.Notify(central, chunk, millis(timeout))
}

// interruptShare makes owner's blocked Notify return at once by
// disconnecting the central it sends to. It runs when that share is
// cancelled, which disconnects every central right after anyway, and does
// nothing once a later share sends.
func (r *radioMux) interruptShare(owner uint64) {
	r.mu.Lock()
	op := r.shareOp
	r.mu.Unlock()

	if op != nil && op.owner == owner {
		r.native.DisconnectCentral(op.peer)
	}
}

// disconnectCentral drops one central.
func (r *radioMux) disconnectCentral(central string) {
	r.native.DisconnectCentral(central)
}

// connect opens a new connection to peer and returns the id its events
// carry.
func (r *radioMux) connect(peer string) (int, error) {
	connID := int(r.lastConnID.Add(1))
	err := r.native.Connect(peer, connID, ServiceUUID, RxUUID, TxUUID)

	return connID, err
}

// write writes one chunk on connection connID for owner, blocking for at
// most timeout.
func (r *radioMux) write(owner uint64, peer string, connID int,
	chunk []byte, timeout time.Duration) error {

	op := &inflight{owner: owner, peer: peer, connID: connID}
	r.mu.Lock()
	r.findOp = op
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		if r.findOp == op {
			r.findOp = nil
		}
		r.mu.Unlock()
	}()

	return r.native.Write(peer, connID, chunk, millis(timeout))
}

// interruptFind makes owner's blocked Write return at once by closing the
// connection it writes on, which the cancelled find or Choose closes right
// after anyway. It does nothing once a later find or Choose writes.
func (r *radioMux) interruptFind(owner uint64) {
	r.mu.Lock()
	op := r.findOp
	r.mu.Unlock()

	if op != nil && op.owner == owner {
		r.native.Disconnect(op.peer, op.connID)
	}
}

// disconnect closes connection connID to peer.
func (r *radioMux) disconnect(peer string, connID int) {
	r.native.Disconnect(peer, connID)
}

// millis converts a timeout for the native radio, which takes whole
// milliseconds. A positive timeout never rounds down to zero, which could
// read as "no time at all" or "no limit".
func millis(d time.Duration) int {
	return int(max(d.Milliseconds(), 1))
}

// startScan subscribes s to central events and makes sure the native scan
// runs, reusing a scan whose stop is still pending. The native call runs
// without the lock: it may block (iOS waits for the central manager to
// power on), and the callbacks it needs delivered meanwhile take the lock.
func (r *radioMux) startScan(s sink) error {
	r.mu.Lock()
	r.findSink = s
	r.stopGen++
	scanning := r.scanning
	if !scanning {
		r.scanGen++
	}
	gen := r.scanGen
	r.mu.Unlock()

	if scanning {
		return nil
	}

	err := r.native.StartScan(
		ServiceUUID, &centralCallback{r: r, scanGen: gen},
	)

	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.findSink = nil
		return err
	}
	r.scanning = true

	return nil
}

// scanLost unsubscribes the find and records that the OS ended the native
// scan on its own, as it does when Bluetooth is switched off, so the next
// startScan starts a fresh one instead of reusing a scan that no longer
// runs. The native stop is still issued so both sides agree.
func (r *radioMux) scanLost() {
	r.mu.Lock()
	r.findSink = nil
	r.stopGen++
	scanning := r.scanning
	r.scanning = false
	r.scanGen++
	r.mu.Unlock()

	if scanning {
		r.native.StopScan()
	}
}

// scanFailed records that the native scan numbered gen failed, so the next
// startScan starts a fresh scan instead of reusing the dead one, whose
// debounced stop would otherwise keep it marked as running. Unlike scanLost
// it keeps the subscription: a Choose that saw the failure still waits for
// its connection's events. A failure of an older scan changes nothing.
func (r *radioMux) scanFailed(gen uint64) {
	r.mu.Lock()
	current := r.scanning && gen == r.scanGen
	if current {
		r.stopGen++
		r.scanning = false
		r.scanGen++
	}
	r.mu.Unlock()

	if current {
		r.native.StopScan()
	}
}

// stopScan unsubscribes the find and stops the native scan after
// the stop debounce, unless another scan starts first.
func (r *radioMux) stopScan() {
	r.mu.Lock()
	r.findSink = nil
	r.stopGen++
	gen := r.stopGen
	r.mu.Unlock()

	r.WgAdd(1)
	go func() {
		defer r.WgDone()

		ctx, cancel := r.Create(context.Background())
		defer cancel()

		select {
		case <-r.clock.TickAfter(r.stopDebounce):

		case <-ctx.Done():
			return
		}

		r.mu.Lock()
		defer r.mu.Unlock()
		if r.stopGen != gen || !r.scanning {
			return
		}
		r.native.StopScan()
		r.scanning = false
		r.scanGen++
	}()
}

// stop tears everything down when the Manager stops.
func (r *radioMux) stop() {
	r.Quit()
	r.WgWait()

	r.mu.Lock()
	scanning := r.scanning
	r.scanning = false
	r.scanGen++
	r.advGen++
	r.shareSink, r.findSink = nil, nil
	r.mu.Unlock()

	if scanning {
		r.native.StopScan()
	}
	r.native.StopAdvertising()
}

// dispatchShare routes a peripheral event to the current share, unless it
// came from an advertisement that no longer runs.
func (r *radioMux) dispatchShare(gen uint64, ev event) {
	r.mu.Lock()
	s := r.shareSink
	if gen != r.advGen {
		s = nil
	}
	r.mu.Unlock()

	if s != nil {
		s.push(ev)
	}
}

// dispatchFind routes a central event to the current find.
func (r *radioMux) dispatchFind(ev event) {
	r.mu.Lock()
	s := r.findSink
	r.mu.Unlock()

	if s != nil {
		s.push(ev)
	}
}

// dispatchScan routes an advertisement or scan failure to the current find,
// unless it came from a native scan that no longer runs.
func (r *radioMux) dispatchScan(ev event) {
	r.mu.Lock()
	s := r.findSink
	if ev.scanGen != r.scanGen {
		s = nil
	}
	r.mu.Unlock()

	if s != nil {
		s.push(ev)
	}
}

// peripheralCallback receives the native payee-side callbacks of one
// advertisement. Every method copies what it needs and enqueues; none
// blocks.
type peripheralCallback struct {
	r      *radioMux
	advGen uint64
}

// OnCentralReady implements radio.PeripheralCallback.
func (c *peripheralCallback) OnCentralReady(centralID string, maxChunk int) {
	c.r.dispatchShare(c.advGen, event{
		kind:     evCentralReady,
		id:       centralID,
		maxChunk: maxChunk,
	})
}

// OnWrite implements radio.PeripheralCallback.
func (c *peripheralCallback) OnWrite(centralID string, chunk []byte) {
	c.r.dispatchShare(c.advGen, event{
		kind:  evWrite,
		id:    centralID,
		chunk: bytes.Clone(chunk),
	})
}

// OnCentralGone implements radio.PeripheralCallback.
func (c *peripheralCallback) OnCentralGone(centralID string) {
	c.r.dispatchShare(c.advGen, event{kind: evCentralGone, id: centralID})
}

// OnAdvertisingFailed implements radio.PeripheralCallback.
func (c *peripheralCallback) OnAdvertisingFailed(reason string) {
	c.r.dispatchShare(c.advGen, event{
		kind:   evAdvertisingFailed,
		reason: reason,
	})
}

// centralCallback receives the native payer-side callbacks of one native
// scan.
type centralCallback struct {
	r       *radioMux
	scanGen uint64
}

// OnAdvertisement implements radio.CentralCallback.
func (c *centralCallback) OnAdvertisement(peerID string, rssi int) {
	c.r.dispatchScan(event{
		kind:    evAdvertisement,
		id:      peerID,
		rssi:    rssi,
		scanGen: c.scanGen,
	})
}

// OnConnected implements radio.CentralCallback. Connection events are
// routed whatever scan they arrive through: a connection outlives the scan
// it was found by, and its connID tells its owner apart.
func (c *centralCallback) OnConnected(peerID string, connID int,
	maxChunk int) {

	c.r.dispatchFind(event{
		kind:     evConnected,
		id:       peerID,
		connID:   connID,
		maxChunk: maxChunk,
	})
}

// OnNotify implements radio.CentralCallback.
func (c *centralCallback) OnNotify(peerID string, connID int, chunk []byte) {
	c.r.dispatchFind(event{
		kind:   evNotify,
		id:     peerID,
		connID: connID,
		chunk:  bytes.Clone(chunk),
	})
}

// OnDisconnected implements radio.CentralCallback.
func (c *centralCallback) OnDisconnected(peerID string, connID int,
	reason string) {

	c.r.dispatchFind(event{
		kind:   evDisconnected,
		id:     peerID,
		connID: connID,
		reason: reason,
	})
}

// OnScanFailed implements radio.CentralCallback.
func (c *centralCallback) OnScanFailed(reason string) {
	c.r.dispatchScan(event{
		kind:    evScanFailed,
		reason:  reason,
		scanGen: c.scanGen,
	})
}

var (
	_ radio.PeripheralCallback = (*peripheralCallback)(nil)
	_ radio.CentralCallback    = (*centralCallback)(nil)
)
