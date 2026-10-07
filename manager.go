package nearby

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/guggero/nearby-pay-req/hce"
	"github.com/guggero/nearby-pay-req/radio"
	"github.com/guggero/nearby-pay-req/session"
	"github.com/lightningnetwork/lnd/clock"
)

var (
	// ErrReplaced ends a share or find that a newer call of the same
	// kind superseded.
	ErrReplaced = errors.New("replaced by a newer call")

	// ErrPaused ends every share and find when the Manager is paused.
	// Callers usually retry once the app is in the foreground again.
	ErrPaused = errors.New("nearby sharing paused")

	// ErrInvalidPaymentRequest is returned for a payment request no
	// session could deliver: empty, longer than
	// session.MaxPaymentRequestLen or not valid UTF-8.
	ErrInvalidPaymentRequest = errors.New(
		"payment request must be 1 to 8192 bytes of UTF-8 text",
	)

	// ErrUnavailable is returned when a share or find can never work
	// while it runs, for example because the OS does not support the
	// role or a permission is denied. UnavailableError carries the
	// reason.
	ErrUnavailable = errors.New("nearby role unavailable")
)

// UnavailableError is the ErrUnavailable a share or find returns, with the
// availability that rules it out.
type UnavailableError struct {
	// Role is "share" or "find".
	Role string

	// Availability is why the role cannot run.
	Availability Availability
}

// Error implements error.
func (e *UnavailableError) Error() string {
	return fmt.Sprintf("nearby %s is unavailable: %v", e.Role,
		e.Availability)
}

// Unwrap makes errors.Is(err, ErrUnavailable) hold.
func (e *UnavailableError) Unwrap() error {
	return ErrUnavailable
}

// Config holds the Manager's dependencies.
type Config struct {
	// Radio is the platform radio, or nil where there is none. Without
	// one every role is unsupported.
	Radio radio.Radio

	// Clock drives every timeout; nil means the wall clock.
	Clock clock.Clock

	// Rand supplies the session randomness; nil means crypto/rand.
	Rand io.Reader

	// Params tunes timeouts, peer selection and rate limits. Unset
	// fields take their defaults (see DefaultParams).
	Params Params
}

// ShareOptions configures one share.
type ShareOptions struct {
	// NFC also serves the request as an emulated NFC tag where the
	// device supports host card emulation, whenever NFC is on: the
	// share starts and stops the tag as the user switches NFC on and
	// off (see StatusChanged), reporting each change as ShareStarted.
	NFC bool

	// ContinueAfterChosen keeps sharing after a payer chose the request
	// (see Chosen), for a request several people may pay, such as a
	// donation address. By default a chosen request stops being shared
	// and Share returns nil.
	ContinueAfterChosen bool
}

// FindOptions configures one find.
type FindOptions struct {
	// Exclude lists sharer peer ids to skip, typically the PeerID of a
	// Received whose code the user said does not match.
	Exclude []string

	// Validate checks a received payment request before the payer
	// acknowledges it, typically by running the wallet's payment
	// parser. A rejected request is reported to the sharer, the session
	// fails with FailurePayloadInvalid and the find goes on. Nil
	// accepts every request. The returned error is only logged at trace
	// level, since parser errors often quote their input.
	Validate func(paymentRequest string) error

	// Collect keeps finding after a Received: the find visits every
	// sharer in range one after another and emits a Received for each,
	// so the user can pick the request whose code matches the screen of
	// the person they are paying. It runs until ctx ends. Without it,
	// Find returns after the first Received.
	Collect bool
}

// Manager runs nearby shares and finds on one radio. It owns the radio
// wrapper and the emulated NFC tag, and runs at most one share and one find
// at a time; starting another of the same kind replaces the running one.
type Manager struct {
	cfg   Config
	radio *radioMux
	tag   *hce.Tag

	mu         sync.Mutex
	share      *activeCall
	find       *activeCall
	statusSubs map[chan struct{}]struct{}
}

// activeCall is the running share or find: how to stop it and when it has
// finished cleaning up the radio.
type activeCall struct {
	cancel context.CancelCauseFunc
	done   chan struct{}
}

// New builds a Manager. A nil cfg.Radio makes every role unsupported.
func New(cfg Config) *Manager {
	if cfg.Rand == nil {
		cfg.Rand = rand.Reader
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.NewDefaultClock()
	}
	cfg.Params = cfg.Params.withDefaults()
	m := &Manager{
		cfg:        cfg,
		tag:        hce.New(),
		statusSubs: make(map[chan struct{}]struct{}),
	}
	if cfg.Radio != nil {
		m.radio = newRadioMux(
			cfg.Radio, cfg.Clock, cfg.Params.ScanStopDebounce,
		)
	}

	return m
}

// ProcessAPDU answers one command APDU for the emulated NFC tag. The
// platform's HCE service calls it, usually on the main thread; it never
// blocks.
func (m *Manager) ProcessAPDU(apdu []byte) []byte {
	return m.tag.Process(apdu)
}

// StatusChanged tells running shares and finds that the radio status may
// have changed (Bluetooth toggled, permission granted), so they re-check
// instead of polling. The platform calls it from its state listeners.
func (m *Manager) StatusChanged() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for ch := range m.statusSubs {
		select {
		case ch <- struct{}{}:

		default:
		}
	}
}

// Pause ends every running share and find with ErrPaused, for example when
// the app goes to the background.
func (m *Manager) Pause() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, call := range []*activeCall{m.share, m.find} {
		if call != nil {
			call.cancel(ErrPaused)
		}
	}
}

// Stop releases the radio for good. It waits for the running share and find
// to finish their own cleanup first, so the radio is not torn down
// underneath a session still talking to it.
func (m *Manager) Stop() {
	m.Pause()

	m.mu.Lock()
	calls := []*activeCall{m.share, m.find}
	m.mu.Unlock()
	for _, call := range calls {
		if call != nil {
			<-call.done
		}
	}

	if m.radio != nil {
		m.radio.stop()
	}
	m.tag.Clear()
}

// Status reports the availability of every role.
func (m *Manager) Status() Status {
	return Status{
		Share:    m.shareAvailability(),
		Find:     m.findAvailability(),
		NFCShare: m.nfcAvailable(),
	}
}

// Share serves paymentRequest until ctx ends or a payer chose it, reporting
// progress through emit, which runs on the calling goroutine. It keeps
// serving after a delivery, for the next payer, and waits for Bluetooth to
// be switched on if it is off. It returns nil after emitting Chosen (unless
// opts.ContinueAfterChosen is set), ErrInvalidPaymentRequest or an
// *UnavailableError before starting, and otherwise ErrReplaced, ErrPaused,
// the context's error, or the error emit returned.
func (m *Manager) Share(ctx context.Context, paymentRequest string,
	opts ShareOptions, emit func(ShareEvent) error) error {

	err := session.ValidatePaymentRequest(paymentRequest)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidPaymentRequest, err)
	}

	// Refuse only what can never work during this share. Bluetooth or
	// NFC being off is temporary: the share waits for it, reporting
	// why.
	ble := m.shareAvailability()
	nfc := opts.NFC && m.hceSupported()
	if !nfc && !ble.recoverable() {
		return &UnavailableError{Role: "share", Availability: ble}
	}

	ctx, stop := m.claim(ctx, &m.share)
	defer stop()

	statusCh, unsubscribe := m.subscribeStatus()
	defer unsubscribe()

	sh := &sharer{
		radio:               m.radio,
		tag:                 m.tag,
		params:              m.cfg.Params,
		continueAfterChosen: opts.ContinueAfterChosen,
		clock:               m.cfg.Clock,
		rand:                m.cfg.Rand,
		paymentRequest:      paymentRequest,
		nfcRequested:        opts.NFC,
		nfcAvailable:        m.nfcAvailable,
		availability:        m.shareAvailability,
		statusChanged:       statusCh,
		send:                emit,
	}

	return result(ctx, sh.run(ctx))
}

// Find searches for one payment request, or with opts.Collect for every one
// in range, reporting progress through emit, which runs on the calling
// goroutine. Without Collect it returns nil right after emitting Received;
// otherwise it runs until ctx ends and returns ErrReplaced,
// ErrPaused, ErrScanFailed, the context's error, or the error emit
// returned. It returns an *UnavailableError before starting if finding
// cannot work at all.
func (m *Manager) Find(ctx context.Context, opts FindOptions,
	emit func(FindEvent) error) error {

	avail := m.findAvailability()
	if !avail.recoverable() {
		return &UnavailableError{Role: "find", Availability: avail}
	}

	ctx, stop := m.claim(ctx, &m.find)
	defer stop()

	statusCh, unsubscribe := m.subscribeStatus()
	defer unsubscribe()

	exclude := make(map[string]bool, len(opts.Exclude))
	for _, id := range opts.Exclude {
		exclude[id] = true
	}
	validate := opts.Validate
	if validate == nil {
		validate = func(string) error { return nil }
	}
	f := &finder{
		radio:         m.radio,
		clock:         m.cfg.Clock,
		rand:          m.cfg.Rand,
		params:        m.cfg.Params,
		collect:       opts.Collect,
		exclude:       exclude,
		validate:      validate,
		availability:  m.findAvailability,
		statusChanged: statusCh,
		send:          emit,
	}

	return result(ctx, f.run(ctx))
}

// claim makes the caller the one running share (or find). A previous one is
// cancelled with ErrReplaced and, crucially, waited for: its cleanup stops
// advertising and clears the NFC tag, which must not undo what the new call
// sets up. The returned stop releases the slot and signals completion; it
// must run after the call's own cleanup.
func (m *Manager) claim(parent context.Context,
	slot **activeCall) (context.Context, func()) {

	ctx, cancel := context.WithCancelCause(parent)
	call := &activeCall{cancel: cancel, done: make(chan struct{})}

	m.mu.Lock()
	prev := *slot
	*slot = call
	m.mu.Unlock()

	if prev != nil {
		prev.cancel(ErrReplaced)
		<-prev.done
	}

	return ctx, func() {
		m.mu.Lock()
		if *slot == call {
			*slot = nil
		}
		m.mu.Unlock()

		cancel(context.Canceled)
		close(call.done)
	}
}

// subscribeStatus registers for StatusChanged nudges.
func (m *Manager) subscribeStatus() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)

	m.mu.Lock()
	m.statusSubs[ch] = struct{}{}
	m.mu.Unlock()

	return ch, func() {
		m.mu.Lock()
		delete(m.statusSubs, ch)
		m.mu.Unlock()
	}
}

// result maps how a share or find ended onto the error its caller sees:
// the reason the Manager cancelled it, if it did, and otherwise what the
// run returned.
func result(ctx context.Context, err error) error {
	switch cause := context.Cause(ctx); {
	// A find that delivered its request, a share whose request was
	// chosen, or a Choose the payee confirmed.
	case err == nil, errors.Is(err, errChosen):
		return nil

	// Superseded by a newer call, or paused.
	case errors.Is(cause, ErrReplaced), errors.Is(cause, ErrPaused):
		return cause

	// The caller's context ended.
	case ctx.Err() != nil:
		return ctx.Err()

	default:
		return err
	}
}

// shareAvailability derives BLE sharing availability from the radio status.
func (m *Manager) shareAvailability() Availability {
	return m.availability(
		radio.StatusPeripheralSupported, PeripheralUnsupported,
	)
}

// findAvailability derives BLE finding availability from the radio status.
func (m *Manager) findAvailability() Availability {
	return m.availability(radio.StatusCentralSupported, UnsupportedOS)
}

// availability checks the status bits in the order the user can act on
// them: nothing can be done about the OS or hardware, a denied permission
// needs the system settings, and Bluetooth can be switched on in place.
func (m *Manager) availability(roleFlag int,
	roleMissing Availability) Availability {

	if m.radio == nil {
		return UnsupportedOS
	}
	st := m.radio.status()

	switch {
	case st&radio.StatusOSSupported == 0:
		return UnsupportedOS

	case st&roleFlag == 0:
		return roleMissing

	case st&radio.StatusPermissionGranted == 0:
		return PermissionDenied

	case st&radio.StatusPoweredOn == 0:
		return BluetoothOff

	default:
		return Available
	}
}

// hceSupported reports whether the device can emulate an NFC tag at all,
// NFC being switched on or not.
func (m *Manager) hceSupported() bool {
	return m.radio != nil && m.radio.status()&radio.StatusHceSupported != 0
}

// nfcAvailable reports whether a share can also be read as an NFC tag.
func (m *Manager) nfcAvailable() bool {
	if m.radio == nil {
		return false
	}
	st := m.radio.status()
	need := radio.StatusHceSupported | radio.StatusNfcEnabled

	return st&need == need
}
