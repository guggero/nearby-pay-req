package session

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"testing"

	"github.com/guggero/nearby-pay-req/wire"
	"github.com/stretchr/testify/require"
)

const (
	// specVectorFile holds the test vectors published with the
	// specification. They were produced by an independent
	// implementation, so reproducing them shows this package follows the
	// written spec rather than only itself.
	specVectorFile = "../spec/vectors.json"
)

// specVector is one vector of the specification. Byte fields are hex.
type specVector struct {
	Name           string `json:"name"`
	PaymentRequest string `json:"payment_request"`

	PayerEphemeral string `json:"payer_ephemeral_privkey"`
	PayeeEphemeral string `json:"payee_ephemeral_privkey"`
	PayerNonce     string `json:"payer_nonce"`
	PayeeNonce     string `json:"payee_nonce"`

	Prologue      string `json:"prologue"`
	Commitment    string `json:"commitment"`
	HandshakeHash string `json:"handshake_hash"`
	Code          string `json:"code"`

	Msg1 string `json:"msg1_noise_e"`
	Msg2 string `json:"msg2_noise_e_ee"`
	Msg3 string `json:"msg3_payer_nonce"`
	Msg4 string `json:"msg4_reveal"`
	Msg5 string `json:"msg5_ack"`

	// Msg4Chunks maps a chunk size to the reveal message's chunks.
	Msg4Chunks map[string][]string `json:"msg4_chunks"`
}

// unhex decodes a hex vector field.
func unhex(t *testing.T, s string) []byte {
	t.Helper()

	b, err := hex.DecodeString(s)
	require.NoError(t, err)

	return b
}

// TestSpecVectors replays every specification vector. The sessions draw
// their randomness in a fixed order (the payer its ephemeral key and then
// its nonce, the payee its nonce before the reply that creates its
// ephemeral key), so a reader serving the vector's secrets in that order
// reproduces the exchange exactly.
func TestSpecVectors(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(specVectorFile)
	require.NoError(t, err)
	var vectors []specVector
	require.NoError(t, json.Unmarshal(data, &vectors))
	require.NotEmpty(t, vectors)

	for _, v := range vectors {
		t.Run(v.Name, func(t *testing.T) {
			t.Parallel()

			prologue := Prologue(testServiceUUID)
			require.Equal(t, v.Prologue, hex.EncodeToString(
				prologue,
			))

			payerRand := bytes.NewReader(append(
				unhex(t, v.PayerEphemeral),
				unhex(t, v.PayerNonce)...,
			))
			payeeRand := bytes.NewReader(append(
				unhex(t, v.PayeeNonce),
				unhex(t, v.PayeeEphemeral)...,
			))
			payer, err := NewPayerSession(Config{
				Prologue: prologue,
				Rand:     payerRand,
			})
			require.NoError(t, err)
			payee, err := NewPayeeSession(Config{
				Prologue: prologue,
				Rand:     payeeRand,
			}, v.PaymentRequest)
			require.NoError(t, err)

			tr := runExchange(t, payer, payee)
			require.Equal(t, v.Msg1, hex.EncodeToString(tr.noiseE))
			require.Equal(t, v.Msg2, hex.EncodeToString(
				tr.noiseEEE,
			))
			require.Equal(t, v.Msg3, hex.EncodeToString(
				tr.payerNonce,
			))
			require.Equal(t, v.Msg4, hex.EncodeToString(tr.reveal))
			require.Equal(t, v.Msg5, hex.EncodeToString(tr.ack))
			require.Equal(t, v.Code, tr.payerCode)
			require.Equal(t, v.Code, tr.payeeCode)
			require.Equal(t, v.PaymentRequest, tr.received)

			// The commitment and the code are pure functions of
			// the nonces and the handshake hash.
			var na, nb [nonceLen]byte
			copy(na[:], unhex(t, v.PayerNonce))
			copy(nb[:], unhex(t, v.PayeeNonce))
			c := commitment(nb)
			require.Equal(t, v.Commitment, hex.EncodeToString(c[:]))
			require.Equal(t, v.Code, comparisonCode(
				unhex(t, v.HandshakeHash), na, nb,
			))

			for size, want := range v.Msg4Chunks {
				n, err := strconv.Atoi(size)
				require.NoError(t, err)
				chunks, err := wire.Chunk(tr.reveal, n)
				require.NoError(t, err)

				got := make([]string, len(chunks))
				for i, c := range chunks {
					got[i] = hex.EncodeToString(c)
				}
				require.Equal(t, want, got, "chunk size %d", n)
			}
		})
	}
}
