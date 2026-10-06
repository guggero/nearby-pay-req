package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/guggero/nearby-pay-req/wire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

var (
	// testServiceUUID is the service the sessions in these tests bind
	// their prologue to.
	testServiceUUID = [16]byte{
		0x7b, 0xe7, 0x84, 0x11, 0xb1, 0x51, 0x48, 0xbf,
		0xa5, 0x70, 0x25, 0x2b, 0x55, 0x84, 0x79, 0x70,
	}

	// testRequest is a stand-in payment request; the session never
	// interprets it.
	testRequest = "lightning:lnbcrt1pexample"
)

// detRand is a deterministic reader (SHA-256 in counter mode over a seed),
// so a transcript is reproducible byte for byte.
type detRand struct {
	seed    []byte
	counter uint64
	buf     []byte
}

// Read implements io.Reader.
func (d *detRand) Read(p []byte) (int, error) {
	for len(d.buf) < len(p) {
		var ctr [8]byte
		binary.BigEndian.PutUint64(ctr[:], d.counter)
		d.counter++
		block := sha256.Sum256(append(
			append([]byte{}, d.seed...), ctr[:]...,
		))
		d.buf = append(d.buf, block[:]...)
	}
	n := copy(p, d.buf)
	d.buf = d.buf[n:]

	return n, nil
}

// newPair builds a payer and payee session with the given randomness.
func newPair(t *testing.T, payerRand, payeeRand *detRand,
	request string) (*PayerSession, *PayeeSession) {

	t.Helper()

	prologue := Prologue(testServiceUUID)
	payer, err := NewPayerSession(Config{
		Prologue: prologue,
		Rand:     payerRand,
	})
	require.NoError(t, err)
	payee, err := NewPayeeSession(Config{
		Prologue: prologue,
		Rand:     payeeRand,
	}, request)
	require.NoError(t, err)

	return payer, payee
}

// onlyMessage returns the single message an Output sends.
func onlyMessage(t *testing.T, out Output) []byte {
	t.Helper()

	require.Len(t, out.Send, 1)
	return out.Send[0]
}

// transcript is every message of one complete exchange plus what each side
// concluded.
type transcript struct {
	noiseE, noiseEEE, payerNonce, reveal, ack []byte

	payerCode, payeeCode   string
	payerToken, payeeToken *[wire.ChosenTokenLen]byte
	received               string
}

// runExchange drives a complete, successful exchange.
func runExchange(t *testing.T, payer *PayerSession,
	payee *PayeeSession) transcript {

	t.Helper()

	var tr transcript
	out, err := payer.Start()
	require.NoError(t, err)
	tr.noiseE = onlyMessage(t, out)

	out, err = payee.Handle(tr.noiseE)
	require.NoError(t, err)
	tr.noiseEEE = onlyMessage(t, out)
	require.Empty(t, out.Code)

	out, err = payer.Handle(tr.noiseEEE)
	require.NoError(t, err)
	tr.payerNonce = onlyMessage(t, out)

	// The payee knows the code as soon as it has Na.
	out, err = payee.Handle(tr.payerNonce)
	require.NoError(t, err)
	tr.reveal = onlyMessage(t, out)
	tr.payeeCode = out.Code
	tr.payeeToken = out.ChosenToken

	out, err = payer.Handle(tr.reveal)
	require.NoError(t, err)
	require.Empty(t, out.Send)
	tr.payerCode = out.Code
	tr.payerToken = out.ChosenToken
	tr.received = out.PaymentRequest

	out, err = payer.Accept()
	require.NoError(t, err)
	require.True(t, out.Done)
	tr.ack = onlyMessage(t, out)

	out, err = payee.Handle(tr.ack)
	require.NoError(t, err)
	require.True(t, out.Delivered)
	require.True(t, out.Done)
	require.Empty(t, out.Send)

	return tr
}

// TestExchange runs complete exchanges with real randomness: both sides
// derive the same six-digit code and the payer receives the request
// unchanged.
func TestExchange(t *testing.T) {
	t.Parallel()

	for _, request := range []string{
		testRequest, "x", strings.Repeat("q", MaxPaymentRequestLen),
	} {

		prologue := Prologue(testServiceUUID)
		payer, err := NewPayerSession(Config{
			Prologue: prologue,
			Rand:     rand.Reader,
		})
		require.NoError(t, err)
		payee, err := NewPayeeSession(Config{
			Prologue: prologue,
			Rand:     rand.Reader,
		}, request)
		require.NoError(t, err)

		tr := runExchange(t, payer, payee)
		require.Len(t, tr.payerCode, 6)
		require.Equal(t, tr.payeeCode, tr.payerCode)
		require.Equal(t, request, tr.received)

		// Both ends derive the same chosen token.
		require.NotNil(t, tr.payerToken)
		require.Equal(t, tr.payeeToken, tr.payerToken)

		// The largest request still fits one message.
		require.LessOrEqual(t, len(tr.reveal), wire.MaxMessageLen)
	}
}

// TestRejectedRequest checks that a payer's Reject ends the payee session
// with ReasonPayloadRejected instead of a delivery.
func TestRejectedRequest(t *testing.T) {
	t.Parallel()

	payer, payee := newPair(
		t, &detRand{seed: []byte("a")}, &detRand{seed: []byte("b")},
		testRequest,
	)

	out, err := payer.Start()
	require.NoError(t, err)
	out, err = payee.Handle(onlyMessage(t, out))
	require.NoError(t, err)
	out, err = payer.Handle(onlyMessage(t, out))
	require.NoError(t, err)
	out, err = payee.Handle(onlyMessage(t, out))
	require.NoError(t, err)
	_, err = payer.Handle(onlyMessage(t, out))
	require.NoError(t, err)

	out, err = payer.Reject()
	require.NoError(t, err)
	require.True(t, out.Done)

	_, err = payee.Handle(onlyMessage(t, out))
	require.Equal(t, ReasonPayloadRejected, ReasonOf(err))

	// Neither side accepts anything after the end.
	_, err = payer.Accept()
	require.Equal(t, ReasonProtocol, ReasonOf(err))
}

// TestPayeeRejectsInvalidRequest checks the share-side validation of the
// string handed in by the caller.
func TestPayeeRejectsInvalidRequest(t *testing.T) {
	t.Parallel()

	cfg := Config{Prologue: Prologue(testServiceUUID), Rand: rand.Reader}
	for _, request := range []string{
		"", strings.Repeat("q", MaxPaymentRequestLen+1), "bitcoin:\x00",
		"bitcoin:\xff",
	} {

		_, err := NewPayeeSession(cfg, request)
		require.Error(t, err)
	}
}

// TestCommitmentMismatch is the man-in-the-middle check: a payee (or an
// attacker posing as one) that reveals a nonce other than the one it
// committed to is caught by the payer as a handshake failure, so it cannot
// pick Nb after learning Na.
func TestCommitmentMismatch(t *testing.T) {
	t.Parallel()

	payer, payee := newPair(
		t, &detRand{seed: []byte("a")}, &detRand{seed: []byte("b")},
		testRequest,
	)

	out, err := payer.Start()
	require.NoError(t, err)
	out, err = payee.Handle(onlyMessage(t, out))
	require.NoError(t, err)
	out, err = payer.Handle(onlyMessage(t, out))
	require.NoError(t, err)
	_, err = payee.Handle(onlyMessage(t, out))
	require.NoError(t, err)

	// Re-seal a reveal with a different Nb under the payee's real send
	// key: the AEAD is valid, only the commitment is wrong.
	var forged [nonceLen]byte
	forged[0] = 1
	request := []byte(testRequest)
	reveal, err := seal(
		payee.send, tlv.MakePrimitiveRecord(typePayeeNonce, &forged),
		tlv.MakePrimitiveRecord(typePaymentRequest, &request),
	)
	require.NoError(t, err)

	out, err = payer.Handle(reveal)
	require.Equal(t, ReasonHandshake, ReasonOf(err))
	require.Empty(t, out.PaymentRequest)
	require.Empty(t, out.Code)
}

// TestOrdering pins that each side only accepts the next message of the
// exchange: a reveal can never precede the commitment, and nothing is
// accepted before Start or twice.
func TestOrdering(t *testing.T) {
	t.Parallel()

	newSessions := func() (*PayerSession, *PayeeSession) {
		return newPair(
			t, &detRand{seed: []byte("a")},
			&detRand{seed: []byte("b")}, testRequest,
		)
	}
	payer, payee := newSessions()
	tr := runExchange(t, payer, payee)

	tests := []struct {
		name string
		run  func() error
	}{{
		name: "payee gets sealed first",
		run: func() error {
			_, payee := newSessions()
			_, err := payee.Handle(tr.payerNonce)
			return err
		},
	}, {
		name: "payer gets reveal before commitment",
		run: func() error {
			payer, _ := newSessions()
			if _, err := payer.Start(); err != nil {
				return err
			}
			_, err := payer.Handle(tr.reveal)
			return err
		},
	}, {
		name: "payer handles before start",
		run: func() error {
			payer, _ := newSessions()
			_, err := payer.Handle(tr.noiseEEE)
			return err
		},
	}, {
		name: "payer starts twice",
		run: func() error {
			payer, _ := newSessions()
			if _, err := payer.Start(); err != nil {
				return err
			}
			_, err := payer.Start()
			return err
		},
	}, {
		name: "payee gets noise e twice",
		run: func() error {
			_, payee := newSessions()
			if _, err := payee.Handle(tr.noiseE); err != nil {
				return err
			}
			_, err := payee.Handle(tr.noiseE)
			return err
		},
	}, {
		name: "payee after done",
		run: func() error {
			_, err := payee.Handle(tr.ack)
			return err
		},
	}, {
		name: "accept before reveal",
		run: func() error {
			payer, _ := newSessions()
			_, err := payer.Accept()
			return err
		},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			require.Error(t, err)
			require.NotZero(t, ReasonOf(err))
		})
	}
}

// TestVersionAndAbort covers the version gate and peer aborts.
func TestVersionAndAbort(t *testing.T) {
	t.Parallel()

	payer, payee := newPair(
		t, &detRand{seed: []byte("a")}, &detRand{seed: []byte("b")},
		testRequest,
	)

	// A v2 payer is told which version we speak.
	out, err := payee.Handle([]byte{2, byte(wire.TypeNoiseE)})
	require.Equal(t, ReasonVersion, ReasonOf(err))
	msg, err := wire.DecodeMessage(onlyMessage(t, out))
	require.NoError(t, err)
	reason, maxVersion, err := wire.DecodeAbort(msg.Body)
	require.NoError(t, err)
	require.Equal(t, wire.AbortVersionUnsupported, reason)
	require.Equal(t, wire.Version1, maxVersion)

	// The payer maps that abort, and a busy one, to reasons.
	_, err = payer.Start()
	require.NoError(t, err)
	_, err = payer.Handle(onlyMessage(t, out))
	require.Equal(t, ReasonVersion, ReasonOf(err))

	payer, _ = newPair(
		t, &detRand{seed: []byte("a")}, &detRand{seed: []byte("b")},
		testRequest,
	)
	_, err = payer.Start()
	require.NoError(t, err)
	_, err = payer.Handle(wire.EncodeAbort(wire.AbortBusy))
	require.Equal(t, ReasonBusy, ReasonOf(err))
}

// TestTamperedMessages flips every byte of every message of a transcript in
// turn and checks the receiving side fails with a typed reason instead of
// accepting it or panicking. Everything after the header is authenticated,
// and a flipped header byte is a version or type error.
func TestTamperedMessages(t *testing.T) {
	t.Parallel()

	payer, payee := newPair(
		t, &detRand{seed: []byte("a")}, &detRand{seed: []byte("b")},
		testRequest,
	)
	tr := runExchange(t, payer, payee)

	// replayTo brings fresh sessions to the point where the message at
	// a step would be consumed, and returns its consumer.
	type step int
	const (
		stepNoiseE step = iota
		stepNoiseEEE
		stepPayerNonce
		stepReveal
		stepAck
	)
	replayTo := func(s step) func([]byte) error {
		payer, payee := newPair(
			t, &detRand{seed: []byte("a")},
			&detRand{seed: []byte("b")}, testRequest,
		)
		_, err := payer.Start()
		require.NoError(t, err)
		if s == stepNoiseE {
			return func(m []byte) error {
				_, err := payee.Handle(m)
				return err
			}
		}
		_, err = payee.Handle(tr.noiseE)
		require.NoError(t, err)
		if s == stepNoiseEEE {
			return func(m []byte) error {
				_, err := payer.Handle(m)
				return err
			}
		}
		_, err = payer.Handle(tr.noiseEEE)
		require.NoError(t, err)
		if s == stepPayerNonce {
			return func(m []byte) error {
				_, err := payee.Handle(m)
				return err
			}
		}
		_, err = payee.Handle(tr.payerNonce)
		require.NoError(t, err)
		if s == stepReveal {
			return func(m []byte) error {
				_, err := payer.Handle(m)
				return err
			}
		}
		_, err = payer.Handle(tr.reveal)
		require.NoError(t, err)
		_, err = payer.Accept()
		require.NoError(t, err)
		return func(m []byte) error {
			_, err := payee.Handle(m)
			return err
		}
	}

	messages := []struct {
		step step
		msg  []byte
	}{
		{stepNoiseEEE, tr.noiseEEE},
		{stepPayerNonce, tr.payerNonce},
		{stepReveal, tr.reveal},
		{stepAck, tr.ack},
	}
	for _, m := range messages {
		for i := range m.msg {
			tampered := append([]byte{}, m.msg...)
			tampered[i] ^= 0x01
			err := replayTo(m.step)(tampered)
			require.Errorf(t, err, "step %d byte %d", m.step, i)
			require.NotZero(t, ReasonOf(err))
		}

		// Truncation fails too.
		err := replayTo(m.step)(m.msg[:len(m.msg)-1])
		require.Error(t, err)
	}

	// Noise message 1 has no MAC; only its framing can be broken.
	err := replayTo(stepNoiseE)(tr.noiseE[:10])
	require.Equal(t, ReasonProtocol, ReasonOf(err))
}

// TestCodeDistinctPerSession checks that the code depends on the session:
// two exchanges with different randomness yield different handshake hashes
// and, with overwhelming probability, different codes.
func TestCodeDistinctPerSession(t *testing.T) {
	t.Parallel()

	codes := make(map[string]struct{})
	for i := 0; i < 20; i++ {
		payer, payee := newPair(
			t, &detRand{seed: []byte{byte(i), 'a'}},
			&detRand{seed: []byte{byte(i), 'b'}}, testRequest,
		)
		tr := runExchange(t, payer, payee)
		codes[tr.payerCode] = struct{}{}
	}
	require.Greater(t, len(codes), 15)
}
