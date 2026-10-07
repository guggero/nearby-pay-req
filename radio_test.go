package nearby

import (
	"testing"
	"time"

	"github.com/guggero/nearby-pay-req/nearbytest"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// TestStaleCallbacks checks that callbacks of a native scan or
// advertisement that no longer runs never reach the current find or share,
// while connection events pass whatever scan they arrive through: their
// connID tells their owner apart.
func TestStaleCallbacks(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	mux := newRadioMux(
		world.NewRadio("phone"), clock.NewDefaultClock(), time.Second,
	)
	t.Cleanup(mux.stop)

	share := make(sink, 8)
	require.NoError(t, mux.startAdvertising(share))
	old := &peripheralCallback{r: mux, advGen: mux.advGen}
	mux.stopAdvertising()
	require.NoError(t, mux.startAdvertising(share))
	current := &peripheralCallback{r: mux, advGen: mux.advGen}

	old.OnCentralReady("central", 100)
	old.OnWrite("central", []byte{1})
	old.OnCentralGone("central")
	old.OnAdvertisingFailed("late")
	require.Empty(t, share)
	current.OnWrite("central", []byte{1})
	require.Len(t, share, 1)

	find := make(sink, 8)
	require.NoError(t, mux.startScan(find))
	oldScan := &centralCallback{r: mux, scanGen: mux.scanGen}
	mux.scanLost()
	require.NoError(t, mux.startScan(find))

	oldScan.OnAdvertisement("peer", -40)
	oldScan.OnScanFailed("late")
	require.Empty(t, find)
	oldScan.OnConnected("peer", 7, 100)
	oldScan.OnNotify("peer", 7, []byte{1})
	oldScan.OnDisconnected("peer", 7, "gone")
	require.Len(t, find, 3)
	for range 3 {
		require.Equal(t, 7, (<-find).connID)
	}
}

// TestLateConnectionEvents checks that events of an earlier connection to
// the sharer a find talks to now, which the OS may deliver after the new
// connection opened, leave the current session alone.
func TestLateConnectionEvents(t *testing.T) {
	t.Parallel()

	world := nearbytest.NewWorld()
	payer := newNode(t, world, "payer")
	var sent []FindEvent
	f := &finder{
		radio:  payer.mgr.radio,
		clock:  payer.clock,
		params: payer.params,
		send: func(ev FindEvent) error {
			sent = append(sent, ev)
			return nil
		},
		candidates:  make(map[string]*candidate),
		failed:      make(map[string]bool),
		received:    make(map[string]bool),
		busyRetries: make(map[string]int),
		retryAt:     make(map[string]time.Time),
		active: &payerRun{
			peer:      "payee",
			connID:    2,
			connected: true,
		},
	}

	for _, ev := range []event{
		{kind: evConnected, id: "payee", connID: 1, maxChunk: 20},
		{kind: evNotify, id: "payee", connID: 1, chunk: []byte{1}},
		{kind: evDisconnected, id: "payee", connID: 1},
	} {

		done, err := f.handleEvent(ev)
		require.NoError(t, err)
		require.False(t, done)
	}
	require.NotNil(t, f.active)
	require.Empty(t, sent)

	// The current connection dropping does end the session.
	_, err := f.handleEvent(event{
		kind:   evDisconnected,
		id:     "payee",
		connID: 2,
	})
	require.NoError(t, err)
	require.Nil(t, f.active)
	require.Equal(t, []FindEvent{
		SessionFailed{Reason: FailureConnectFailed},
	}, sent)
}
