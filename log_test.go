package nearby

import (
	"bytes"
	"sync"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/guggero/nearby-pay-req/nearbytest"
	"github.com/stretchr/testify/require"
)

// syncBuffer is a bytes.Buffer safe for the concurrent writes of the
// share's and the find's goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write implements io.Writer.
func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.buf.Write(p)
}

// String returns everything written so far.
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.buf.String()
}

// logExchanges runs a successful and a rejected exchange with the library
// logging at trace level if trace is set and at debug level otherwise, and
// returns what it logged and the successful exchange's code.
func logExchanges(t *testing.T, trace bool) (string, string) {
	t.Helper()

	var out syncBuffer
	handler := btclog.NewDefaultHandler(&out, btclog.WithNoTimestamp())
	handler.SetLevel(btclog.LevelDebug)
	if trace {
		handler.SetLevel(btclog.LevelTrace)
	}
	UseLogger(btclog.NewSLogger(handler))
	defer DisableLog()

	world := nearbytest.NewWorld()
	payee := newNode(t, world, "payee")
	payer := newNode(t, world, "payer")

	request := testRequest + "?amount=0.001&label=secret"
	share := payee.share(t, request, false)
	expectStarted(t, share, true, false)
	find := startFind(t, world, payer)
	nextAs[Connecting](t, find)
	received := nextAs[Received](t, find)
	require.NoError(t, find.end(t))
	nextAs[PeerConnected](t, share)
	nextAs[Delivered](t, share)
	share.cancel()
	require.Error(t, share.end(t))

	rejected := "not-a-bitcoin-uri:secret"
	other := newNode(t, world, "other")
	share = other.share(t, rejected, false)
	expectStarted(t, share, true, false)
	find = startFind(t, world, payer, "payee")
	nextAs[Connecting](t, find)
	nextAs[SessionFailed](t, find)
	nextAs[PeerConnected](t, share)
	nextAs[SessionFailed](t, share)
	find.cancel()
	require.Error(t, find.end(t))
	share.cancel()
	require.Error(t, share.end(t))

	return out.String(), received.Code
}

// TestLogsOmitSecrets runs a successful and a rejected exchange and checks
// that neither the payment requests, the validator's error nor the
// comparison codes reach a log at debug level, which production builds
// may run at, while the trace level, kept for development, carries them.
// It is not parallel: the logger is package state.
func TestLogsOmitSecrets(t *testing.T) {
	logged, code := logExchanges(t, false)
	require.Contains(t, logged, "Nearby payment request from payee")
	require.NotContains(t, logged, "secret")
	require.NotContains(t, logged, "not a bitcoin URI")
	require.NotContains(t, logged, code)

	logged, code = logExchanges(t, true)
	require.Contains(t, logged, "label=secret")
	require.Contains(t, logged, "not a bitcoin URI")
	require.Contains(t, logged, code)
}
