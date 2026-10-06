// Package nearbytest provides in-memory radios that implement radio.Radio
// and are linked to each other through a shared World, so the Manager — and
// whole apps embedding it — can be tested end to end without Bluetooth. It
// is meant to be imported from tests only.
package nearbytest

import (
	"bytes"
	"errors"
	"fmt"
	"sync"

	"github.com/guggero/nearby-pay-req/radio"
)

const (
	// DefaultMaxChunk is the ATT value size the fake reports unless a
	// test overrides it: the iOS-central figure.
	DefaultMaxChunk = 512

	// DefaultStatus is a fully capable, permitted, powered radio with
	// NFC host card emulation.
	DefaultStatus = radio.StatusPoweredOn |
		radio.StatusPermissionGranted |
		radio.StatusPeripheralSupported |
		radio.StatusCentralSupported |
		radio.StatusOSSupported |
		radio.StatusHceSupported |
		radio.StatusNfcEnabled

	// DefaultRSSI is the signal strength between two radios unless a
	// test sets one: close enough to be picked.
	DefaultRSSI = -50
)

var (
	// ErrNotConnected is returned for writes or notifies on a link that
	// does not exist.
	ErrNotConnected = errors.New("not connected")

	// ErrWriteFailed is returned for a write once FailWritesAfter armed
	// the failure.
	ErrWriteFailed = errors.New("write failed")
)

// World links radios. A central in the world sees every advertising
// peripheral.
type World struct {
	mu     sync.Mutex
	radios map[string]*Radio
	rssi   map[[2]string]int
}

// NewWorld returns an empty world.
func NewWorld() *World {
	return &World{
		radios: make(map[string]*Radio),
		rssi:   make(map[[2]string]int),
	}
}

// NewRadio adds a radio with the given id, which is what peers see as its
// central or peer id.
func (w *World) NewRadio(id string) *Radio {
	r := &Radio{
		world:          w,
		id:             id,
		status:         DefaultStatus,
		maxChunk:       DefaultMaxChunk,
		writeFailAfter: -1,
		links:          make(map[string]bool),
	}

	w.mu.Lock()
	w.radios[id] = r
	w.mu.Unlock()

	return r
}

// SetRSSI sets the signal strength the central sees for the peripheral.
func (w *World) SetRSSI(central, peripheral string, rssi int) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.rssi[[2]string{central, peripheral}] = rssi
}

// rssiOf returns the signal strength between two radios. Callers hold mu.
func (w *World) rssiOf(central, peripheral string) int {
	if rssi, ok := w.rssi[[2]string{central, peripheral}]; ok {
		return rssi
	}

	return DefaultRSSI
}

// radio looks a radio up.
func (w *World) radio(id string) *Radio {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.radios[id]
}

// Advertise makes every scanning central see every advertising peripheral
// once more, as a real scan reports repeated sightings with fresh RSSI.
func (w *World) Advertise() {
	w.mu.Lock()
	type sighting struct {
		cb   radio.CentralCallback
		id   string
		rssi int
	}
	var sightings []sighting
	for _, c := range w.radios {
		cb := c.scanCallback()
		if cb == nil {
			continue
		}
		for _, p := range w.radios {
			if p != c && p.advertisingCallback() != nil {
				sightings = append(sightings, sighting{
					cb:   cb,
					id:   p.id,
					rssi: w.rssiOf(c.id, p.id),
				})
			}
		}
	}
	w.mu.Unlock()

	// Callbacks run without any lock, like native ones do.
	for _, s := range sightings {
		s.cb.OnAdvertisement(s.id, s.rssi)
	}
}

// Radio is one fake phone. Its knobs may be changed at any time.
type Radio struct {
	world *World
	id    string

	mu        sync.Mutex
	status    int
	maxChunk  int
	failNext  bool
	periphCB  radio.PeripheralCallback
	centralCB radio.CentralCallback

	// writeFailAfter is how many more writes succeed before every
	// further one fails; negative means writes never fail.
	writeFailAfter int

	// links are the peers this radio is connected to as a central.
	links map[string]bool

	// hceActive and the call counts are observable state for tests.
	hceActive  bool
	scanStarts int
	scanStops  int
}

// SetStatus changes what Status reports.
func (r *Radio) SetStatus(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.status = status
}

// SetMaxChunk changes the ATT value size reported for this radio's links.
func (r *Radio) SetMaxChunk(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.maxChunk = n
}

// FailNextConnect makes the next Connect from this radio fail.
func (r *Radio) FailNextConnect() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.failNext = true
}

// FailWritesAfter makes every Write from this radio fail once n more have
// succeeded, like a link that drops mid-session.
func (r *Radio) FailWritesAfter(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.writeFailAfter = n
}

// writeFails spends one write of the budget FailWritesAfter set and reports
// whether this write has to fail.
func (r *Radio) writeFails() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch {
	case r.writeFailAfter < 0:
		return false

	case r.writeFailAfter == 0:
		return true

	default:
		r.writeFailAfter--
		return false
	}
}

// HceActive reports the last SetHceActive value.
func (r *Radio) HceActive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.hceActive
}

// ScanCalls returns how often the native scan was started and stopped.
func (r *Radio) ScanCalls() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.scanStarts, r.scanStops
}

// Advertising reports whether the radio advertises.
func (r *Radio) Advertising() bool {
	return r.advertisingCallback() != nil
}

// Scanning reports whether the radio scans.
func (r *Radio) Scanning() bool {
	return r.scanCallback() != nil
}

// advertisingCallback returns the peripheral callback while advertising.
func (r *Radio) advertisingCallback() radio.PeripheralCallback {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.periphCB
}

// scanCallback returns the central callback while scanning.
func (r *Radio) scanCallback() radio.CentralCallback {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.centralCB
}

// Status implements radio.Radio.
func (r *Radio) Status() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.status
}

// StartAdvertising implements radio.Radio.
func (r *Radio) StartAdvertising(_, _, _ string,
	cb radio.PeripheralCallback) error {

	r.mu.Lock()
	r.periphCB = cb
	r.mu.Unlock()

	return nil
}

// StopAdvertising implements radio.Radio. Every central
// connected to this radio sees a disconnect.
func (r *Radio) StopAdvertising() {
	r.mu.Lock()
	r.periphCB = nil
	r.mu.Unlock()

	for _, c := range r.world.centralsOf(r.id) {
		c.dropLink(r.id, "peripheral stopped")
	}
}

// Notify implements radio.Radio.
func (r *Radio) Notify(centralID string, chunk []byte) error {
	c := r.world.radio(centralID)
	if c == nil || !c.linked(r.id) {
		return ErrNotConnected
	}
	if err := checkChunk(chunk, c.chunkLimit(r)); err != nil {
		return err
	}
	if cb := c.scanCallback(); cb != nil {
		cb.OnNotify(r.id, bytes.Clone(chunk))
	}

	return nil
}

// DisconnectCentral implements radio.Radio.
func (r *Radio) DisconnectCentral(centralID string) {
	if c := r.world.radio(centralID); c != nil {
		c.dropLink(r.id, "peripheral disconnected")
	}
}

// StartScan implements radio.Radio. Advertisements are
// reported on World.Advertise.
func (r *Radio) StartScan(_ string, cb radio.CentralCallback) error {
	r.mu.Lock()
	r.centralCB = cb
	r.scanStarts++
	r.mu.Unlock()

	return nil
}

// StopScan implements radio.Radio. The callback is kept
// for connection events, like a native central keeps its delegate.
func (r *Radio) StopScan() {
	r.mu.Lock()
	r.scanStops++
	r.mu.Unlock()
}

// Connect implements radio.Radio: the link is up at
// once and both sides learn the chunk size.
func (r *Radio) Connect(peerID, _, _, _ string) error {
	r.mu.Lock()
	cb := r.centralCB
	fail := r.failNext
	r.failNext = false
	r.mu.Unlock()

	p := r.world.radio(peerID)
	var periphCB radio.PeripheralCallback
	if p != nil {
		periphCB = p.advertisingCallback()
	}
	if fail || periphCB == nil {
		if cb != nil {
			cb.OnDisconnected(peerID, "connect failed")
		}
		return nil
	}

	r.mu.Lock()
	r.links[peerID] = true
	r.mu.Unlock()

	limit := r.chunkLimit(p)
	periphCB.OnCentralReady(r.id, limit)
	if cb != nil {
		cb.OnConnected(peerID, limit)
	}

	return nil
}

// Write implements radio.Radio.
func (r *Radio) Write(peerID string, chunk []byte) error {
	if !r.linked(peerID) {
		return ErrNotConnected
	}
	if r.writeFails() {
		return ErrWriteFailed
	}
	p := r.world.radio(peerID)
	if err := checkChunk(chunk, r.chunkLimit(p)); err != nil {
		return err
	}
	if cb := p.advertisingCallback(); cb != nil {
		cb.OnWrite(r.id, bytes.Clone(chunk))
	}

	return nil
}

// Disconnect implements radio.Radio.
func (r *Radio) Disconnect(peerID string) {
	r.dropLink(peerID, "central disconnected")
}

// SetHceActive implements radio.Radio.
func (r *Radio) SetHceActive(active bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.hceActive = active
}

// linked reports whether this radio, as a central, is connected to peerID.
func (r *Radio) linked(peerID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.links[peerID]
}

// dropLink tears down the link from this central to peerID and tells both
// sides, once.
func (r *Radio) dropLink(peerID, reason string) {
	r.mu.Lock()
	wasLinked := r.links[peerID]
	delete(r.links, peerID)
	cb := r.centralCB
	r.mu.Unlock()

	if !wasLinked {
		return
	}
	if cb != nil {
		cb.OnDisconnected(peerID, reason)
	}
	if p := r.world.radio(peerID); p != nil {
		if pcb := p.advertisingCallback(); pcb != nil {
			pcb.OnCentralGone(r.id)
		}
	}
}

// chunkLimit is the ATT value size of a link: the smaller of both ends.
func (r *Radio) chunkLimit(other *Radio) int {
	// Read each side under its own lock only: holding both would
	// deadlock two radios computing the limit of their link at once.
	return min(r.ownMaxChunk(), other.ownMaxChunk())
}

// ownMaxChunk returns this radio's configured ATT value size.
func (r *Radio) ownMaxChunk() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.maxChunk
}

// centralsOf returns every radio connected to the peripheral id.
func (w *World) centralsOf(id string) []*Radio {
	w.mu.Lock()
	defer w.mu.Unlock()

	var out []*Radio
	for _, r := range w.radios {
		if r.linked(id) {
			out = append(out, r)
		}
	}

	return out
}

// checkChunk enforces the ATT value size, as a real stack would.
func checkChunk(chunk []byte, limit int) error {
	if len(chunk) > limit {
		return fmt.Errorf("chunk of %d bytes exceeds link limit %d",
			len(chunk), limit)
	}

	return nil
}

var _ radio.Radio = (*Radio)(nil)
