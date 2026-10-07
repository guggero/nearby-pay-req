package hce

import (
	"encoding/binary"
	"encoding/hex"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/guggero/nearby-pay-req/ndef"
	"github.com/stretchr/testify/require"
)

// apdu decodes a hex command.
func apdu(t *testing.T, s string) []byte {
	t.Helper()

	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	require.NoError(t, err)
	return b
}

// readTag reads the NDEF message the way an NFC Forum Type 4 reader does:
// select the application, read the CC to learn MLe and the NDEF file, then
// read NLEN and the message in MLe-sized pieces.
func readTag(t *testing.T, tag *Tag) []byte {
	t.Helper()

	ok := func(resp []byte) []byte {
		require.GreaterOrEqual(t, len(resp), 2)
		require.Equalf(t, swOK, resp[len(resp)-2:], "%x", resp)
		return resp[:len(resp)-2]
	}

	ok(tag.Process(apdu(t, "00A4040007D276000085010100")))
	ok(tag.Process(apdu(t, "00A4000C02E103")))
	cc := ok(tag.Process(apdu(t, "00B000000F")))
	require.Len(t, cc, 15)
	mle := int(binary.BigEndian.Uint16(cc[3:5]))
	require.Equal(t, []byte{0x04, 0x06, 0xE1, 0x04}, cc[7:11])
	fileSize := int(binary.BigEndian.Uint16(cc[11:13]))

	ok(tag.Process(apdu(t, "00A4000C02E104")))
	nlen := ok(tag.Process(apdu(t, "00B0000002")))
	msgLen := int(binary.BigEndian.Uint16(nlen))
	require.Equal(t, fileSize, msgLen+2)

	var msg []byte
	for offset := 2; offset < 2+msgLen; {
		le := min(mle, 2+msgLen-offset)
		cmd := []byte{0x00, 0xB0, byte(offset >> 8), byte(offset),
			byte(le)}
		chunk := ok(tag.Process(cmd))
		require.Len(t, chunk, le)
		msg = append(msg, chunk...)
		offset += le
	}

	return msg
}

// TestReadRoundTrip reads a payment request through the full APDU flow and
// decodes it with the same NDEF code the payer side uses: the tag serves
// exactly the QR string.
func TestReadRoundTrip(t *testing.T) {
	t.Parallel()

	for _, uri := range []string{
		"bitcoin:bc1q", "lightning:lnbc1" + strings.Repeat("q", 3000),
		"tark1" + strings.Repeat("x", 120),
	} {

		message, err := ndef.EncodeURIMessage(uri)
		require.NoError(t, err)

		var reads atomic.Int32
		tag := New()
		require.NoError(t, tag.SetMessage(message, func() {
			reads.Add(1)
		}))

		got := readTag(t, tag)
		require.Equal(t, message, got)
		text, err := ndef.DecodeURIMessage(got)
		require.NoError(t, err)
		require.Equal(t, uri, text)
		require.EqualValues(t, 1, reads.Load())

		// Re-reading within the same selection doesn't notify
		// again; a new selection does.
		tag.Process(apdu(t, "00B0000002"))
		require.EqualValues(t, 1, reads.Load())
		readTag(t, tag)
		require.EqualValues(t, 2, reads.Load())
	}
}

// TestInactiveTag checks that a tag without a message, or after Clear,
// refuses the application so the reader moves on to another service.
func TestInactiveTag(t *testing.T) {
	t.Parallel()

	tag := New()
	selectApp := apdu(t, "00A4040007D276000085010100")
	require.Equal(t, swFileNotFound, tag.Process(selectApp))
	require.Equal(t, swFileNotFound, tag.Process(apdu(t, "00A4000C02E103")))
	require.Equal(t, swNotAllowed, tag.Process(apdu(t, "00B0000002")))

	message, err := ndef.EncodeURIMessage("bitcoin:bc1q")
	require.NoError(t, err)
	require.NoError(t, tag.SetMessage(message, nil))
	require.Equal(t, swOK, tag.Process(selectApp))
	require.Equal(t, swOK, tag.Process(apdu(t, "00A4000C02E104")))

	// Clear drops the selection mid-read, too.
	tag.Clear()
	require.Equal(t, swNotAllowed, tag.Process(apdu(t, "00B0000002")))
	require.Equal(t, swFileNotFound, tag.Process(selectApp))
}

// TestCommandErrors covers the status words for malformed or unsupported
// commands.
func TestCommandErrors(t *testing.T) {
	t.Parallel()

	message, err := ndef.EncodeURIMessage("bitcoin:bc1q")
	require.NoError(t, err)
	tag := New()
	require.NoError(t, tag.SetMessage(message, nil))

	tests := []struct {
		name string
		cmd  string
		want []byte
	}{
		{"too short", "00A4", swWrongLength},
		{"class", "80A4040007D276000085010100", swClaNotSupported},
		{"update binary", "00D6000001AA", swInsNotSupported},
		{"other aid", "00A4040007A000000003101000", swFileNotFound},
		{"truncated select", "00A4040007D2760000", swWrongLength},
		{"select without lc", "00A40400", swWrongLength},
		{"select p1", "00A4080002E104", swFuncNotSupported},
		{"file before app", "00A4000C02E104", swFileNotFound},
		{"read before file", "00B0000002", swNotAllowed},
	}
	for _, tc := range tests {
		require.Equal(t, tc.want, tag.Process(apdu(t, tc.cmd)), tc.name)
	}

	require.Equal(t, swOK, tag.Process(
		apdu(t, "00A4040007D276000085010100"),
	))
	require.Equal(t, swFileNotFound, tag.Process(apdu(t, "00A4000C02E105")))
	require.Equal(t, swOK, tag.Process(apdu(t, "00A4000C02E104")))

	// Offset past the end, offset with the high bit, and a case-1
	// READ BINARY without Le.
	require.Equal(t, swWrongParams, tag.Process(apdu(t, "00B0010001")))
	require.Equal(t, swWrongParams, tag.Process(apdu(t, "00B0800001")))
	require.Equal(t, swWrongLength, tag.Process(apdu(t, "00B00000")))

	// Le larger than the file returns what is there; Le = 0 means 256.
	resp := tag.Process(apdu(t, "00B0000000"))
	require.Len(t, resp, len(message)+2+2)

	// Reading at exactly the end returns no data.
	end := byte(len(message) + 2)
	require.Equal(t, swOK, tag.Process([]byte{0, 0xB0, 0, end, 1}))
}

// TestSetMessageBounds checks the message length limits.
func TestSetMessageBounds(t *testing.T) {
	t.Parallel()

	tag := New()
	require.Error(t, tag.SetMessage(nil, nil))
	require.Error(t, tag.SetMessage(make([]byte, maxNdefMessageLen+1), nil))
	require.NoError(t, tag.SetMessage(make([]byte, maxNdefMessageLen), nil))
}

// FuzzProcess sends arbitrary APDUs: the tag must always answer with a
// status word and never panic.
func FuzzProcess(f *testing.F) {
	f.Add([]byte{
		0x00, 0xA4, 0x04, 0x00, 0x07, 0xD2, 0x76, 0x00, 0x00, 0x85,
		0x01, 0x01, 0x00,
	})
	f.Add([]byte{0x00, 0xB0, 0x00, 0x00, 0x00})

	message, _ := ndef.EncodeURIMessage("bitcoin:bc1q")
	f.Fuzz(func(t *testing.T, data []byte) {
		tag := New()
		require.NoError(t, tag.SetMessage(message, nil))
		tag.Process([]byte{
			0x00, 0xA4, 0x04, 0x00, 0x07, 0xD2, 0x76, 0x00, 0x00,
			0x85, 0x01, 0x01, 0x00,
		})
		tag.Process([]byte{0x00, 0xA4, 0x00, 0x0C, 0x02, 0xE1, 0x04})

		resp := tag.Process(data)
		require.GreaterOrEqual(t, len(resp), 2)
	})
}

// TestReadCoverage checks that only a reader that got every byte of the
// NDEF file counts as having read it, whatever order it read in: reading at
// the end of the file, its tail only, or around a gap must not be reported
// as a completed read.
func TestReadCoverage(t *testing.T) {
	t.Parallel()

	message, err := ndef.EncodeURIMessage("bitcoin:bc1q")
	require.NoError(t, err)
	fileLen := 2 + len(message)

	// read builds a READ BINARY of le bytes at offset.
	read := func(offset, le int) []byte {
		return []byte{0x00, 0xB0, byte(offset >> 8), byte(offset),
			byte(le)}
	}

	tests := []struct {
		name  string
		reads [][]byte
		want  int32
	}{{
		name:  "eof only",
		reads: [][]byte{read(fileLen, 1)},
	}, {
		name:  "tail only",
		reads: [][]byte{read(2, 0)},
	}, {
		name:  "gap",
		reads: [][]byte{read(0, 2), read(4, 0)},
	}, {
		name:  "sequential",
		reads: [][]byte{read(0, 2), read(2, len(message))},
		want:  1,
	}, {
		name:  "out of order",
		reads: [][]byte{read(5, 0), read(0, 5)},
		want:  1,
	}, {
		name: "reread",
		reads: [][]byte{
			read(0, 0), read(0, 0), read(3, 2), read(0, 0),
		},
		want: 1,
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var reads atomic.Int32
			tag := New()
			require.NoError(t, tag.SetMessage(message, func() {
				reads.Add(1)
			}))
			require.Equal(t, swOK, tag.Process(
				apdu(t, "00A4040007D276000085010100"),
			))
			require.Equal(t, swOK, tag.Process(
				apdu(t, "00A4000C02E104"),
			))
			for _, cmd := range tc.reads {
				resp := tag.Process(cmd)
				require.Equal(t, swOK, resp[len(resp)-2:])
			}
			require.Equal(t, tc.want, reads.Load())
		})
	}

	// A replaced message starts its coverage from scratch.
	var reads atomic.Int32
	tag := New()
	require.NoError(t, tag.SetMessage(message, func() { reads.Add(1) }))
	tag.Process(apdu(t, "00A4040007D276000085010100"))
	tag.Process(apdu(t, "00A4000C02E104"))
	tag.Process(read(0, 4))
	require.NoError(t, tag.SetMessage(message, func() { reads.Add(1) }))
	tag.Process(apdu(t, "00A4040007D276000085010100"))
	tag.Process(apdu(t, "00A4000C02E104"))
	tag.Process(read(4, 0))
	require.Zero(t, reads.Load())
}
