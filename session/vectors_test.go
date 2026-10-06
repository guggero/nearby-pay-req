package session

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/guggero/nearby-pay-req/wire"
	"github.com/stretchr/testify/require"
)

const (
	// vectorFile is the path of the committed vectors.
	vectorFile = "testdata/vectors.json"
)

var (
	// updateVectors regenerates testdata/vectors.json instead of
	// comparing against it. Only ever needed when the protocol changes,
	// which also means a new protocol version.
	updateVectors = flag.Bool("update", false, "rewrite test vectors")

	// vectorChunkSizes are the chunk sizes each vector is split at: the
	// default-MTU minimum, a typical iPhone-peripheral size, and the ATT
	// maximum.
	vectorChunkSizes = []int{wire.MinChunkSize, 182, wire.MaxChunkSize}
)

// vector is one recorded exchange. Byte fields are hex.
type vector struct {
	Name           string `json:"name"`
	PayerSeed      string `json:"payer_seed"`
	PayeeSeed      string `json:"payee_seed"`
	PaymentRequest string `json:"payment_request"`

	NoiseE     string `json:"noise_e"`
	NoiseEEE   string `json:"noise_e_ee"`
	PayerNonce string `json:"payer_nonce_msg"`
	Reveal     string `json:"reveal_msg"`
	Ack        string `json:"ack_msg"`
	Code       string `json:"code"`

	// Chunks maps chunk size to the reveal message's chunks, the only
	// message long enough to span several.
	Chunks map[int][]string `json:"reveal_chunks"`
}

// vectorInputs are the payloads the vectors cover: every payment request
// shape the transport must carry without interpreting it.
func vectorInputs() []vector {
	return []vector{{
		Name:           "bolt11-short",
		PayerSeed:      "payer-1",
		PayeeSeed:      "payee-1",
		PaymentRequest: "lightning:lnbc1" + strings.Repeat("q", 290),
	}, {
		Name:           "bolt11-route-hints",
		PayerSeed:      "payer-2",
		PayeeSeed:      "payee-2",
		PaymentRequest: "lightning:lnbc1" + strings.Repeat("p", 2990),
	}, {
		Name:      "bip21-unified",
		PayerSeed: "payer-3",
		PayeeSeed: "payee-3",
		PaymentRequest: "bitcoin:bc1qar0srrr7xfkvy5l643lydnw9re59gtzz" +
			"wf5mdq?amount=0.0001&lightning=lnbc1" +
			strings.Repeat("z", 300),
	}, {
		Name:           "ark-bare",
		PayerSeed:      "payer-4",
		PayeeSeed:      "payee-4",
		PaymentRequest: "tark1" + strings.Repeat("x", 120),
	}, {
		Name:           "bolt12-offer",
		PayerSeed:      "payer-5",
		PayeeSeed:      "payee-5",
		PaymentRequest: "lno1" + strings.Repeat("r", 200),
	}}
}

// recordVector runs one exchange with the vector's seeds and fills in its
// outputs.
func recordVector(t *testing.T, v vector) vector {
	t.Helper()

	payer, payee := newPair(
		t, &detRand{seed: []byte(v.PayerSeed)},
		&detRand{seed: []byte(v.PayeeSeed)}, v.PaymentRequest,
	)
	tr := runExchange(t, payer, payee)
	require.Equal(t, tr.payeeCode, tr.payerCode)
	require.Equal(t, v.PaymentRequest, tr.received)

	v.NoiseE = hex.EncodeToString(tr.noiseE)
	v.NoiseEEE = hex.EncodeToString(tr.noiseEEE)
	v.PayerNonce = hex.EncodeToString(tr.payerNonce)
	v.Reveal = hex.EncodeToString(tr.reveal)
	v.Ack = hex.EncodeToString(tr.ack)
	v.Code = tr.payerCode

	v.Chunks = make(map[int][]string)
	for _, size := range vectorChunkSizes {
		chunks, err := wire.Chunk(tr.reveal, size)
		require.NoError(t, err)
		for _, c := range chunks {
			v.Chunks[size] = append(
				v.Chunks[size], hex.EncodeToString(c),
			)
		}
	}

	return v
}

// TestVectors reproduces every committed exchange byte for byte, so any
// change to the handshake, the TLV layout, the code derivation or the
// chunking shows up as a vector mismatch rather than silently breaking
// interoperability with already-shipped apps.
func TestVectors(t *testing.T) {
	var got []vector
	for _, v := range vectorInputs() {
		got = append(got, recordVector(t, v))
	}

	if *updateVectors {
		data, err := json.MarshalIndent(got, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(
			vectorFile, append(data, '\n'), 0o644,
		))
		return
	}

	data, err := os.ReadFile(vectorFile)
	require.NoError(t, err)
	var want []vector
	require.NoError(t, json.Unmarshal(data, &want))
	require.Equal(t, want, got)
}
