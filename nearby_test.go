package nearby

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/guggero/nearby-pay-req/nearbytest"
	"github.com/guggero/nearby-pay-req/radio"
	"github.com/guggero/nearby-pay-req/session"
	"github.com/guggero/nearby-pay-req/wire"
	"github.com/stretchr/testify/require"
)

// nextAs reads the call's next event and requires it to be a T.
func nextAs[T any, E any](t *testing.T, c *call[E]) T {
	t.Helper()

	ev := c.next(t)
	got, ok := any(ev).(T)
	require.Truef(t, ok, "want %T, got %#v", *new(T), ev)

	return got
}

// expectStarted reads the share's started event.
func expectStarted(t *testing.T, c *call[ShareEvent], ble, nfc bool) {
	t.Helper()

	started := nextAs[ShareStarted](t, c)
	require.Equal(t, ble, started.BLE)
	require.Equal(t, nfc, started.NFC)
}

// startFind starts a find on payer and drives it to the point where it
// connects: one round of advertisements and the collection window.
func startFind(t *testing.T, world *nearbytest.World, payer *node,
	exclude ...string) *call[FindEvent] {

	t.Helper()

	return startFindWith(t, world, payer, FindOptions{Exclude: exclude})
}

// startFindWith is startFind with options.
func startFindWith(t *testing.T, world *nearbytest.World, payer *node,
	opts FindOptions) *call[FindEvent] {

	t.Helper()

	f := payer.findWith(t, opts)
	nextAs[Scanning](t, f)
	payer.waitTick(t, payer.params.FindGiveUpAfter)

	world.Advertise()
	payer.nextWindow(t)

	return f
}

// nextWindow lets the payer's pending collection window elapse.
func (n *node) nextWindow(t *testing.T) {
	t.Helper()

	n.waitTick(t, n.params.CollectWindow)
	n.advance(n.params.CollectWindow)
}

// TestShareFind runs the whole exchange between two phones: the payer
// receives exactly the shared string and both sides show the same code.
func TestShareFind(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	payee := newNode(t, world, "payee")
	payer := newNode(t, world, "payer")

	request := testRequest + "?amount=0.001"
	share := payee.share(t, request, false)
	expectStarted(t, share, true, false)
	require.True(t, payee.radio.Advertising())

	find := startFind(t, world, payer)
	nextAs[Connecting](t, find)
	received := nextAs[Received](t, find)
	require.NoError(t, find.end(t))

	require.Equal(t, request, received.PaymentRequest)
	require.Equal(t, "payee", received.PeerID)
	require.Len(t, received.Code, 6)

	// The payee showed the same code, then saw the acknowledgement.
	require.Equal(
		t, received.Code,
		nextAs[PeerConnected](t, share).Code,
	)
	require.Equal(
		t, received.Code, nextAs[Delivered](t, share).Code,
	)

	// The share keeps running for the next payer; cancelling stops the
	// radio.
	share.cancel()
	require.Error(t, share.end(t))
	require.False(t, payee.radio.Advertising())
}

// TestFindPicksStrongest checks peer selection: the strongest sharer wins,
// an excluded one is skipped, and sharers below the RSSI floor are reported
// as too far instead of picked.
func TestFindPicksStrongest(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	near := newNode(t, world, "near")
	far := newNode(t, world, "far")
	payer := newNode(t, world, "payer")
	world.SetRSSI("payer", "near", -40)
	world.SetRSSI("payer", "far", -65)

	request := testRequest
	expectStarted(t, near.share(t, request, false), true, false)
	expectStarted(t, far.share(t, request, false), true, false)

	find := startFind(t, world, payer)
	nextAs[Connecting](t, find)
	require.Equal(t, "near", nextAs[Received](t, find).PeerID)
	require.NoError(t, find.end(t))

	// "Doesn't match" on the nearest one: the next find skips it.
	find = startFind(t, world, payer, "near")
	nextAs[Connecting](t, find)
	require.Equal(t, "far", nextAs[Received](t, find).PeerID)
	require.NoError(t, find.end(t))

	// Everyone too far: the payer says so and keeps waiting.
	world.SetRSSI("payer", "near", -90)
	world.SetRSSI("payer", "far", -95)
	find = startFind(t, world, payer)
	nextAs[TooFar](t, find)
}

// TestPayloadInvalid checks that a request the parser rejects fails the
// session on both sides and is never delivered.
func TestPayloadInvalid(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	payee := newNode(t, world, "payee")
	payer := newNode(t, world, "payer")

	share := payee.share(t, "definitely not a payment request", false)
	expectStarted(t, share, true, false)

	find := startFind(t, world, payer)
	nextAs[Connecting](t, find)
	require.Equal(
		t, FailurePayloadInvalid,
		nextAs[SessionFailed](t, find).Reason,
	)

	nextAs[PeerConnected](t, share)
	require.Equal(
		t, FailurePayloadInvalid,
		nextAs[SessionFailed](t, share).Reason,
	)
}

// TestPayeeBusy checks that a second payer is turned away while the payee
// serves another one.
func TestPayeeBusy(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	payee := newNode(t, world, "payee")
	payer := newNode(t, world, "payer")
	other := newRogue(t, world, "other")

	expectStarted(
		t, payee.share(t, testRequest, false), true,
		false,
	)

	// The other payer opens a session and stalls.
	other.connect(t, "payee")
	other.openSession(t, "payee")

	find := startFind(t, world, payer, "other")
	nextAs[Connecting](t, find)
	require.Equal(
		t, FailurePeerBusy,
		nextAs[SessionFailed](t, find).Reason,
	)
}

// TestRateLimit checks that a payee refuses a second session within
// defaultMinSessionInterval.
func TestRateLimit(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	payee := newNode(t, world, "payee")
	r := newRogue(t, world, "rogue")

	share := payee.share(t, testRequest, false)
	expectStarted(t, share, true, false)

	// First session: abandoned after the code.
	r.connect(t, "payee")
	r.runUntilCode(t, "payee")
	nextAs[PeerConnected](t, share)
	r.disconnect("payee")
	nextAs[SessionFailed](t, share)

	// Immediately again: busy.
	r.connect(t, "payee")
	_, reply := r.openSession(t, "payee")

	msg, err := wire.DecodeMessage(reply)
	require.NoError(t, err)
	require.Equal(t, wire.TypeAbort, msg.Type)
	reason, _, err := wire.DecodeAbort(msg.Body)
	require.NoError(t, err)
	require.Equal(t, wire.AbortBusy, reason)
}

// TestSuspiciousActivity checks the warning for sessions that fail after a
// code was shown, the signature of someone grinding for a matching code.
func TestSuspiciousActivity(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	payee := newNode(t, world, "payee")
	r := newRogue(t, world, "rogue")

	share := payee.share(t, testRequest, false)
	expectStarted(t, share, true, false)

	for i := 0; i < defaultSuspiciousThreshold; i++ {
		payee.advance(defaultMinSessionInterval)
		r.connect(t, "payee")
		r.runUntilCode(t, "payee")
		nextAs[PeerConnected](t, share)
		r.disconnect("payee")
		require.Equal(
			t,
			FailureConnectFailed,
			nextAs[SessionFailed](t, share).Reason,
		)
	}

	warning := nextAs[SuspiciousActivity](t, share)
	require.Equal(t, defaultSuspiciousThreshold, warning.FailedSessions)
	require.Equal(t, time.Minute, warning.Window)
}

// TestSessionTimeout checks that a stalled payer is dropped after
// defaultSessionTimeout.
func TestSessionTimeout(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	payee := newNode(t, world, "payee")
	r := newRogue(t, world, "rogue")

	share := payee.share(t, testRequest, false)
	expectStarted(t, share, true, false)

	r.connect(t, "payee")
	r.openSession(t, "payee")

	payee.waitTick(t, defaultSessionTimeout)
	payee.advance(defaultSessionTimeout)
	require.Equal(
		t, FailureTimeout,
		nextAs[SessionFailed](t, share).Reason,
	)
}

// TestNFCShare checks the HCE side of a share: the tag serves the request
// while the share runs, reports a complete read, and is cleared after.
func TestNFCShare(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	payee := newNode(t, world, "payee")

	share := payee.share(t, testRequest, true)
	expectStarted(t, share, true, true)
	require.True(t, payee.radio.HceActive())

	readNDEF(t, payee.mgr)
	nextAs[NFCRead](t, share)

	share.cancel()
	require.Error(t, share.end(t))
	require.False(t, payee.radio.HceActive())

	// No share, no tag.
	resp := payee.mgr.ProcessAPDU([]byte{
		0x00, 0xA4, 0x04, 0x00, 0x07, 0xD2, 0x76, 0x00, 0x00, 0x85,
		0x01, 0x01, 0x00,
	})
	require.Equal(t, []byte{0x6A, 0x82}, resp)
}

// readNDEF reads the emulated tag completely through ProcessAPDU.
func readNDEF(t *testing.T, mgr *Manager) {
	t.Helper()

	cmds := [][]byte{
		{0x00, 0xA4, 0x04, 0x00, 0x07, 0xD2, 0x76, 0x00, 0x00, 0x85,
			0x01, 0x01, 0x00},
		{0x00, 0xA4, 0x00, 0x0C, 0x02, 0xE1, 0x04},
	}
	for _, c := range cmds {
		require.Equal(t, []byte{0x90, 0x00}, mgr.ProcessAPDU(c))
	}

	// Le = 0 reads up to 255 bytes, which covers this short request.
	resp := mgr.ProcessAPDU([]byte{0x00, 0xB0, 0x00, 0x00, 0x00})
	require.Equal(t, []byte{0x90, 0x00}, resp[len(resp)-2:])
}

// TestAvailability covers the mapping from radio status bits to the
// availability of each role.
func TestAvailability(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	n := newNode(t, world, "phone")
	full := nearbytest.DefaultStatus

	tests := []struct {
		name   string
		status int
		share  Availability
		find   Availability
		nfc    bool
	}{{
		name:   "everything",
		status: full,
		share:  Available,
		find:   Available,
		nfc:    true,
	}, {
		name:   "old android",
		status: full &^ radio.StatusOSSupported,
		share:  UnsupportedOS,
		find:   UnsupportedOS,
		nfc:    true,
	}, {
		name:   "no advertiser",
		status: full &^ radio.StatusPeripheralSupported,
		share:  PeripheralUnsupported,
		find:   Available,
		nfc:    true,
	}, {
		name:   "permission denied",
		status: full &^ radio.StatusPermissionGranted,
		share:  PermissionDenied,
		find:   PermissionDenied,
		nfc:    true,
	}, {
		name: "bluetooth off, nfc off",
		status: full &^ (radio.StatusPoweredOn |
			radio.StatusNfcEnabled),
		share: BluetoothOff,
		find:  BluetoothOff,
	}}
	for _, tc := range tests {
		n.radio.SetStatus(tc.status)
		require.Equal(t, Status{
			Share:    tc.share,
			Find:     tc.find,
			NFCShare: tc.nfc,
		}, n.mgr.Status(), tc.name)
	}

	// No radio at all.
	none := New(Config{Clock: n.clock})
	require.Equal(t, Status{
		Share: UnsupportedOS,
		Find:  UnsupportedOS,
	}, none.Status())
}

// TestShareWaitsForBluetooth checks that a share started with Bluetooth off
// reports it and starts advertising once Bluetooth comes on, and that a
// denied permission without NFC is refused outright.
func TestShareWaitsForBluetooth(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	n := newNode(t, world, "phone")
	request := testRequest

	n.radio.SetStatus(
		nearbytest.DefaultStatus &^
			radio.StatusPoweredOn,
	)
	share := n.share(t, request, false)
	require.Equal(
		t, BluetoothOff,
		nextAs[AvailabilityChanged](t, share).Availability,
	)
	require.False(t, n.radio.Advertising())

	// The status nudge can race the share's subscription; repeat it
	// until the share reacts.
	n.radio.SetStatus(nearbytest.DefaultStatus)
	require.Eventually(t, func() bool {
		n.mgr.StatusChanged()
		return n.radio.Advertising()
	}, testTimeout, 10*time.Millisecond)
	expectStarted(t, share, true, false)

	// Bluetooth off again mid-share.
	n.radio.SetStatus(
		nearbytest.DefaultStatus &^
			radio.StatusPoweredOn,
	)
	n.mgr.StatusChanged()
	require.Equal(
		t, BluetoothOff,
		nextAs[AvailabilityChanged](t, share).Availability,
	)
	require.False(t, n.radio.Advertising())
	share.cancel()

	// Denied permission and no NFC: nothing can work.
	n.radio.SetStatus(
		nearbytest.DefaultStatus &^
			radio.StatusPermissionGranted,
	)
	denied := n.share(t, request, false)
	err := denied.end(t)
	require.ErrorIs(t, err, ErrUnavailable)
	var unavailable *UnavailableError
	require.ErrorAs(t, err, &unavailable)
	require.Equal(t, PermissionDenied, unavailable.Availability)

	// With NFC the share still runs, NFC only.
	nfcOnly := n.share(t, request, true)
	require.Equal(
		t, PermissionDenied,
		nextAs[AvailabilityChanged](t, nfcOnly).Availability,
	)
	expectStarted(t, nfcOnly, false, true)
}

// TestInvalidShare checks argument validation.
func TestInvalidShare(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	n := newNode(t, world, "phone")

	for _, request := range []string{"", "bitcoin:\xff"} {
		c := n.share(t, request, true)
		require.ErrorIs(t, c.end(t), ErrInvalidPaymentRequest)
	}
}

// TestReplaceAndPause checks that a new share supersedes the running one
// and that pausing ends every share and find.
func TestReplaceAndPause(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	n := newNode(t, world, "phone")
	request := testRequest

	first := n.share(t, request, true)
	expectStarted(t, first, true, true)

	second := n.share(t, request+"?amount=1", true)
	require.ErrorIs(t, first.end(t), ErrReplaced)

	// The replaced share's cleanup did not undo the new one.
	expectStarted(t, second, true, true)
	require.True(t, n.radio.Advertising())
	require.True(t, n.radio.HceActive())

	find := n.find(t)
	nextAs[Scanning](t, find)

	n.mgr.Pause()
	require.ErrorIs(t, second.end(t), ErrPaused)
	require.ErrorIs(t, find.end(t), ErrPaused)
	require.False(t, n.radio.Advertising())
	require.False(t, n.radio.HceActive())
}

// TestScanDebounce checks that a find restarted within defaultScanStopDebounce
// reuses the running native scan, and that the scan stops once the debounce
// elapses.
func TestScanDebounce(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	n := newNode(t, world, "phone")

	find := n.find(t)
	nextAs[Scanning](t, find)
	find.cancel()
	require.Error(t, find.end(t))
	n.waitTick(t, defaultScanStopDebounce)

	find = n.find(t)
	nextAs[Scanning](t, find)
	starts, stops := n.radio.ScanCalls()
	require.Equal(t, 1, starts)
	require.Equal(t, 0, stops)

	// The pending stop from the first find is void.
	n.advance(defaultScanStopDebounce)
	find.cancel()
	require.Error(t, find.end(t))
	n.waitTick(t, defaultScanStopDebounce)
	n.advance(defaultScanStopDebounce)
	require.Eventually(t, func() bool {
		_, stops := n.radio.ScanCalls()
		return stops == 1
	}, testTimeout, 10*time.Millisecond)
}

// TestFailureReason pins the mapping from session errors to reasons.
func TestFailureReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		err  error
		want FailureReason
	}{{
		err:  &session.Error{Reason: session.ReasonProtocol},
		want: FailureProtocolError,
	}, {
		err:  &session.Error{Reason: session.ReasonVersion},
		want: FailureVersionUnsupported,
	}, {
		err:  &session.Error{Reason: session.ReasonHandshake},
		want: FailureHandshakeFailed,
	}, {
		err:  &session.Error{Reason: session.ReasonBusy},
		want: FailurePeerBusy,
	}, {
		err:  &session.Error{Reason: session.ReasonPayloadRejected},
		want: FailurePayloadInvalid,
	}, {
		err:  &session.Error{Reason: session.ReasonPeerAborted},
		want: FailureConnectFailed,
	}, {
		err:  fmt.Errorf("x: %w", wire.ErrProtocol),
		want: FailureProtocolError,
	}, {
		err:  errors.New("other"),
		want: FailureUnspecified,
	}}
	for _, tc := range tests {
		require.Equalf(t, tc.want, failureReason(tc.err), "%v", tc.err)
	}
}

// TestDisconnectBeforeCodeIsNotSuspicious checks that payers dropping the
// link before they sent their nonce, so before any code was shown, do not
// count towards the suspicious-activity warning: it is meant for sessions
// that showed a code and then went nowhere, the shape of a grinding man in
// the middle, and a payer that never saw a code cannot have been grinding.
func TestDisconnectBeforeCodeIsNotSuspicious(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	payee := newNode(t, world, "payee")
	r := newRogue(t, world, "rogue")

	share := payee.share(t, testRequest, false)
	expectStarted(t, share, true, false)

	// Up to the commitment and gone: no code was shown yet.
	for i := 0; i < defaultSuspiciousThreshold; i++ {
		payee.advance(defaultMinSessionInterval)
		r.connect(t, "payee")
		r.openSession(t, "payee")
		r.disconnect("payee")
		require.Equal(
			t,
			FailureConnectFailed,
			nextAs[SessionFailed](t, share).Reason,
		)
	}

	// The warning still needs defaultSuspiciousThreshold sessions that
	// showed a code; the drops above were not among them.
	for i := 0; i < defaultSuspiciousThreshold; i++ {
		payee.advance(defaultMinSessionInterval)
		r.connect(t, "payee")
		r.runUntilCode(t, "payee")
		nextAs[PeerConnected](t, share)
		r.disconnect("payee")
		nextAs[SessionFailed](t, share)
	}
	warning := nextAs[SuspiciousActivity](t, share)
	require.Equal(t, defaultSuspiciousThreshold, warning.FailedSessions)
}

// TestWriteFailureFailsFast checks that a payer whose write to the sharer
// fails mid-handshake gives up on that sharer at once, instead of leaving
// the sharer waiting until the session timeout.
func TestWriteFailureFailsFast(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	payee := newNode(t, world, "payee")
	payer := newNode(t, world, "payer")

	share := payee.share(t, testRequest, false)
	expectStarted(t, share, true, false)

	// The opening message goes out; the nonce answering the commitment
	// does not.
	payer.radio.FailWritesAfter(1)
	find := startFind(t, world, payer)
	nextAs[Connecting](t, find)
	require.Equal(
		t, FailureRadioError,
		nextAs[SessionFailed](t, find).Reason,
	)
}

// TestScanRestartsAfterBluetoothOff checks that a find whose Bluetooth was
// switched off and on again starts a new native scan: the OS ends a scan
// together with the radio, so the one from before must not be reused.
func TestScanRestartsAfterBluetoothOff(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	n := newNode(t, world, "phone")

	find := n.find(t)
	nextAs[Scanning](t, find)

	n.radio.SetStatus(
		nearbytest.DefaultStatus &^ radio.StatusPoweredOn,
	)
	n.mgr.StatusChanged()
	require.Equal(
		t, BluetoothOff,
		nextAs[AvailabilityChanged](t, find).Availability,
	)

	n.radio.SetStatus(nearbytest.DefaultStatus)
	n.mgr.StatusChanged()
	nextAs[Scanning](t, find)

	starts, stops := n.radio.ScanCalls()
	require.Equal(t, 2, starts)
	require.Equal(t, 1, stops)
}

// TestScanFailureRestartsScan checks that a find started right after a scan
// failure starts a new native scan instead of reusing the dead one within
// the stop debounce, and that a failure reported late by the dead scan
// cannot end the find that replaced it.
func TestScanFailureRestartsScan(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	payee := newNode(t, world, "payee")
	payer := newNode(t, world, "payer")
	expectStarted(t, payee.share(t, testRequest, false), true, false)

	first := payer.find(t)
	nextAs[Scanning](t, first)
	payer.radio.FailScan("stack error")
	require.Equal(
		t, FailureRadioError, nextAs[SessionFailed](t, first).Reason,
	)
	require.ErrorIs(t, first.end(t), ErrScanFailed)

	// Within the debounce: a fresh native scan.
	second := startFind(t, world, payer)
	starts, _ := payer.radio.ScanCalls()
	require.Equal(t, 2, starts)

	// The first scan reports its failure once more, late. The find on
	// the second scan does not notice and delivers.
	stale := &centralCallback{r: payer.mgr.radio, scanGen: 1}
	stale.OnScanFailed("late")
	nextAs[Connecting](t, second)
	nextAs[Received](t, second)
	require.NoError(t, second.end(t))

	// A healthy scan is still reused by a quick restart.
	third := payer.find(t)
	nextAs[Scanning](t, third)
	starts, _ = payer.radio.ScanCalls()
	require.Equal(t, 2, starts)
}

// TestAckWriteFailure checks that a payer whose acknowledgement could not
// be written fails the session instead of handing on a request the payee
// never counts as delivered, and that the payee keeps no token for it.
func TestAckWriteFailure(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	payee := newNode(t, world, "payee")
	payer := newNode(t, world, "payer")

	share := payee.share(t, testRequest, false)
	expectStarted(t, share, true, false)

	// The opening message and the nonce go out, each one chunk; the
	// acknowledgement does not.
	payer.radio.FailWritesAfter(2)
	find := startFind(t, world, payer)
	nextAs[Connecting](t, find)
	require.Equal(
		t, FailureRadioError, nextAs[SessionFailed](t, find).Reason,
	)

	// The payee showed its code, then lost the payer without an
	// acknowledgement.
	nextAs[PeerConnected](t, share)
	require.Equal(
		t, FailureConnectFailed, nextAs[SessionFailed](t, share).Reason,
	)
}
