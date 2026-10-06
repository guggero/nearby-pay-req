package nearby

import (
	"bytes"
	"context"
	"sync"

	"github.com/guggero/nearby-pay-req/radio"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
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

	// chunk is a copy of the value written or notified.
	chunk []byte

	// rssi is set on advertisements, maxChunk on readiness events.
	rssi     int
	maxChunk int

	// reason is the native failure description, for logs only.
	reason string
}

// sink is a queue of radio events with a non-blocking send. Native threads
// must never wait for Go, so a full queue drops the event (see
// eventQueueLen).
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
// debounces stopping a scan (see scanStopDebounce).
type radioMux struct {
	native radio.Radio
	clock  clock.Clock

	mu sync.Mutex

	// shareSink and findSink receive the events of the current share
	// and find; nil when none runs.
	shareSink sink
	findSink  sink

	// scanning is whether the native scan runs; stopGen invalidates a
	// pending debounced stop when a new scan starts.
	scanning bool
	stopGen  uint64

	*fn.ContextGuard
}

// newRadioMux wraps native, which must not be nil.
func newRadioMux(native radio.Radio, clk clock.Clock) *radioMux {
	return &radioMux{
		native:       native,
		clock:        clk,
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
	r.mu.Unlock()

	err := r.native.StartAdvertising(
		ServiceUUID, RxUUID, TxUUID, &peripheralCallback{r: r},
	)
	if err != nil {
		r.mu.Lock()
		r.shareSink = nil
		r.mu.Unlock()
	}

	return err
}

// stopAdvertising stops advertising and unsubscribes the share.
func (r *radioMux) stopAdvertising() {
	r.native.StopAdvertising()

	r.mu.Lock()
	r.shareSink = nil
	r.mu.Unlock()
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
	r.mu.Unlock()

	if scanning {
		return nil
	}

	err := r.native.StartScan(ServiceUUID, &centralCallback{r: r})

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
	r.mu.Unlock()

	if scanning {
		r.native.StopScan()
	}
}

// stopScan unsubscribes the find and stops the native scan after
// scanStopDebounce, unless another scan starts first.
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
		case <-r.clock.TickAfter(scanStopDebounce):

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
	}()
}

// stop tears everything down when the Manager stops.
func (r *radioMux) stop() {
	r.Quit()
	r.WgWait()

	r.mu.Lock()
	scanning := r.scanning
	r.scanning = false
	r.shareSink, r.findSink = nil, nil
	r.mu.Unlock()

	if scanning {
		r.native.StopScan()
	}
	r.native.StopAdvertising()
}

// dispatchShare routes a peripheral event to the current share.
func (r *radioMux) dispatchShare(ev event) {
	r.mu.Lock()
	s := r.shareSink
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

// peripheralCallback receives the native payee-side callbacks. Every method
// copies what it needs and enqueues; none blocks.
type peripheralCallback struct {
	r *radioMux
}

// OnCentralReady implements radio.PeripheralCallback.
func (c *peripheralCallback) OnCentralReady(centralID string, maxChunk int) {
	c.r.dispatchShare(event{
		kind:     evCentralReady,
		id:       centralID,
		maxChunk: maxChunk,
	})
}

// OnWrite implements radio.PeripheralCallback.
func (c *peripheralCallback) OnWrite(centralID string, chunk []byte) {
	c.r.dispatchShare(event{
		kind:  evWrite,
		id:    centralID,
		chunk: bytes.Clone(chunk),
	})
}

// OnCentralGone implements radio.PeripheralCallback.
func (c *peripheralCallback) OnCentralGone(centralID string) {
	c.r.dispatchShare(event{kind: evCentralGone, id: centralID})
}

// OnAdvertisingFailed implements radio.PeripheralCallback.
func (c *peripheralCallback) OnAdvertisingFailed(reason string) {
	c.r.dispatchShare(event{kind: evAdvertisingFailed, reason: reason})
}

// centralCallback receives the native payer-side callbacks.
type centralCallback struct {
	r *radioMux
}

// OnAdvertisement implements radio.CentralCallback.
func (c *centralCallback) OnAdvertisement(peerID string, rssi int) {
	c.r.dispatchFind(event{kind: evAdvertisement, id: peerID, rssi: rssi})
}

// OnConnected implements radio.CentralCallback.
func (c *centralCallback) OnConnected(peerID string, maxChunk int) {
	c.r.dispatchFind(event{
		kind:     evConnected,
		id:       peerID,
		maxChunk: maxChunk,
	})
}

// OnNotify implements radio.CentralCallback.
func (c *centralCallback) OnNotify(peerID string, chunk []byte) {
	c.r.dispatchFind(event{
		kind:  evNotify,
		id:    peerID,
		chunk: bytes.Clone(chunk),
	})
}

// OnDisconnected implements radio.CentralCallback.
func (c *centralCallback) OnDisconnected(peerID string, reason string) {
	c.r.dispatchFind(event{
		kind:   evDisconnected,
		id:     peerID,
		reason: reason,
	})
}

// OnScanFailed implements radio.CentralCallback.
func (c *centralCallback) OnScanFailed(reason string) {
	c.r.dispatchFind(event{kind: evScanFailed, reason: reason})
}

var (
	_ radio.PeripheralCallback = (*peripheralCallback)(nil)
	_ radio.CentralCallback    = (*centralCallback)(nil)
)
