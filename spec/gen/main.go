// Command nearbypayreqvectors generates the test vectors of the Nearby
// Payment Request Exchange bLIP. It implements the exchange from the
// specification text alone, using flynn/noise only for the Noise NN
// handshake; TLV, framing, commitment, code and chunking are written out
// by hand so they can be checked against the document.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/flynn/noise"
)

var serviceUUID = mustHex("7be78411b15148bfa570252b55847970")

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// detBytes is a deterministic byte source for the vector inputs:
// SHA-256(seed || BE-u64 counter), concatenated.
func detBytes(seed string, n int) []byte {
	var out []byte
	for ctr := uint64(0); len(out) < n; ctr++ {
		var c [8]byte
		binary.BigEndian.PutUint64(c[:], ctr)
		h := sha256.Sum256(append([]byte(seed), c[:]...))
		out = append(out, h[:]...)
	}
	return out[:n]
}

func bigSize(v uint64) []byte {
	switch {
	case v < 0xfd:
		return []byte{byte(v)}
	case v <= 0xffff:
		return binary.BigEndian.AppendUint16([]byte{0xfd}, uint16(v))
	case v <= 0xffffffff:
		return binary.BigEndian.AppendUint32([]byte{0xfe}, uint32(v))
	default:
		return binary.BigEndian.AppendUint64([]byte{0xff}, v)
	}
}

type rec struct {
	t uint64
	v []byte
}

func tlvStream(recs ...rec) []byte {
	var b []byte
	for _, r := range recs {
		b = append(b, bigSize(r.t)...)
		b = append(b, bigSize(uint64(len(r.v)))...)
		b = append(b, r.v...)
	}
	return b
}

func msg(typ byte, body []byte) []byte {
	return append([]byte{0x01, typ}, body...)
}

func chunk(m []byte, size int) [][]byte {
	var out [][]byte
	seq := byte(0)
	for off := 0; off < len(m); seq++ {
		hdr := 2
		if off == 0 {
			hdr = 4
		}
		n := min(size-hdr, len(m)-off)
		c := []byte{0, seq}
		if off == 0 {
			c[0] |= 0x80
			c = binary.BigEndian.AppendUint16(c, uint16(len(m)))
		}
		if off+n == len(m) {
			c[0] |= 0x40
		}
		c = append(c, m[off:off+n]...)
		out = append(out, c)
		off += n
	}
	return out
}

type tags struct{ prologue, commit, code string }

type vector struct {
	Name           string `json:"name"`
	PaymentRequest string `json:"payment_request"`

	PayerEphemeralPriv string `json:"payer_ephemeral_privkey"`
	PayeeEphemeralPriv string `json:"payee_ephemeral_privkey"`
	PayerNonce         string `json:"payer_nonce"`
	PayeeNonce         string `json:"payee_nonce"`

	Prologue      string `json:"prologue"`
	Commitment    string `json:"commitment"`
	HandshakeHash string `json:"handshake_hash"`
	Code          string `json:"code"`

	Msg1 string `json:"msg1_noise_e"`
	Msg2 string `json:"msg2_noise_e_ee"`
	Msg3 string `json:"msg3_payer_nonce"`
	Msg4 string `json:"msg4_reveal"`
	Msg5 string `json:"msg5_ack"`

	RevealChunks map[int][]string `json:"msg4_chunks"`
}

func run(tg tags, name, req string, ePayer, ePayee, na, nb []byte,
	chunkSizes []int) vector {

	prologue := append([]byte(tg.prologue), serviceUUID...)
	cs := noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly,
		noise.HashSHA256)
	mk := func(init bool, e []byte) *noise.HandshakeState {
		hs, err := noise.NewHandshakeState(noise.Config{
			CipherSuite: cs, Random: bytes.NewReader(e),
			Pattern: noise.HandshakeNN, Initiator: init,
			Prologue: prologue,
		})
		if err != nil {
			panic(err)
		}
		return hs
	}
	payer, payee := mk(true, ePayer), mk(false, ePayee)

	m1, _, _, err := payer.WriteMessage(nil, nil)
	check(err)
	_, _, _, err = payee.ReadMessage(nil, m1)
	check(err)

	ch := sha256.Sum256(append([]byte(tg.commit), nb...))
	m2, pyeRecv, pyeSend, err := payee.WriteMessage(nil,
		tlvStream(rec{0, ch[:]}))
	check(err)
	p2, pyrSend, pyrRecv, err := payer.ReadMessage(nil, m2)
	check(err)
	if !bytes.Equal(p2, tlvStream(rec{0, ch[:]})) {
		panic("commitment payload")
	}

	c3, err := pyrSend.Encrypt(nil, nil, tlvStream(rec{2, na}))
	check(err)
	_, err = pyeRecv.Decrypt(nil, nil, c3)
	check(err)
	c4, err := pyeSend.Encrypt(nil, nil,
		tlvStream(rec{4, nb}, rec{6, []byte(req)}))
	check(err)
	_, err = pyrRecv.Decrypt(nil, nil, c4)
	check(err)
	c5, err := pyrSend.Encrypt(nil, nil, tlvStream(rec{8, []byte{0}}))
	check(err)

	h := payer.ChannelBinding()
	if !bytes.Equal(h, payee.ChannelBinding()) {
		panic("h mismatch")
	}
	hc := sha256.New()
	hc.Write([]byte(tg.code))
	hc.Write(h)
	hc.Write(na)
	hc.Write(nb)
	code := fmt.Sprintf("%06d", binary.BigEndian.Uint32(hc.Sum(nil))%1_000_000)

	v := vector{
		Name: name, PaymentRequest: req,
		PayerEphemeralPriv: hex.EncodeToString(ePayer),
		PayeeEphemeralPriv: hex.EncodeToString(ePayee),
		PayerNonce:         hex.EncodeToString(na),
		PayeeNonce:         hex.EncodeToString(nb),
		Prologue:           hex.EncodeToString(prologue),
		Commitment:         hex.EncodeToString(ch[:]),
		HandshakeHash:      hex.EncodeToString(h),
		Code:               code,
		Msg1:               hex.EncodeToString(msg(0x01, m1)),
		Msg2:               hex.EncodeToString(msg(0x02, m2)),
		Msg3:               hex.EncodeToString(msg(0x03, c3)),
		Msg4:               hex.EncodeToString(msg(0x03, c4)),
		Msg5:               hex.EncodeToString(msg(0x03, c5)),
		RevealChunks:       map[int][]string{},
	}
	for _, s := range chunkSizes {
		for _, c := range chunk(msg(0x03, c4), s) {
			v.RevealChunks[s] = append(v.RevealChunks[s],
				hex.EncodeToString(c))
		}
	}
	return v
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}

type input struct{ name, payerSeed, payeeSeed, req string }

func inputs() []input {
	return []input{
		{"bolt11-short", "payer-1", "payee-1",
			"lightning:lnbc1" + strings.Repeat("q", 290)},
		{"bolt11-route-hints", "payer-2", "payee-2",
			"lightning:lnbc1" + strings.Repeat("p", 2990)},
		{"bip21-unified", "payer-3", "payee-3",
			"bitcoin:bc1qar0srrr7xfkvy5l643lydnw9re59gtzz" +
				"wf5mdq?amount=0.0001&lightning=lnbc1" +
				strings.Repeat("z", 300)},
		{"ark-bare", "payer-4", "payee-4", "tark1" + strings.Repeat("x", 120)},
		{"bolt12-offer", "payer-5", "payee-5", "lno1" + strings.Repeat("r", 200)},
	}
}

// keys derives a vector's secret inputs from its seeds: the payer's
// ephemeral key and Na, and the payee's Nb and ephemeral key.
func keys(in input) (ePayer, na, nb, ePayee []byte) {
	p := detBytes(in.payerSeed, 64)
	q := detBytes(in.payeeSeed, 64)
	return p[:32], p[32:], q[:32], q[32:]
}

func main() {
	t := tags{"nearby-payreq/1", "nearby-payreq/1/commit", "nearby-payreq/1/code"}
	var out []vector
	all := append([]input{{"inline-short", "payer-0", "payee-0",
		"lightning:lnbc1" + strings.Repeat("q", 60)}}, inputs()...)
	for _, in := range all {
		ePayer, na, nb, ePayee := keys(in)
		out = append(out, run(t, in.name, in.req, ePayer, ePayee,
			na, nb, []int{20, 182, 512}))
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	check(enc.Encode(out))
}
