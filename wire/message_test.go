package wire

import (
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestMessageRoundTrip checks the header codec and that DecodeMessage leaves
// version and type judgement to the caller.
func TestMessageRoundTrip(t *testing.T) {
	t.Parallel()

	msg := EncodeMessage(TypeSealed, []byte{1, 2, 3})
	require.Equal(t, []byte{1, 3, 1, 2, 3}, msg)

	got, err := DecodeMessage(msg)
	require.NoError(t, err)
	require.Equal(t, Message{
		Version: Version1,
		Type:    TypeSealed,
		Body:    []byte{1, 2, 3},
	}, got)

	// A future version and an unknown type still decode.
	got, err = DecodeMessage([]byte{9, 0x55})
	require.NoError(t, err)
	require.Equal(t, byte(9), got.Version)
	require.Equal(t, MessageType(0x55), got.Type)
	require.Empty(t, got.Body)

	_, err = DecodeMessage([]byte{1})
	require.ErrorIs(t, err, ErrProtocol)
}

// TestAbortRoundTrip checks that only a version abort carries the highest
// supported version.
func TestAbortRoundTrip(t *testing.T) {
	t.Parallel()

	msg, err := DecodeMessage(EncodeAbort(AbortBusy))
	require.NoError(t, err)
	require.Equal(t, TypeAbort, msg.Type)
	reason, maxVersion, err := DecodeAbort(msg.Body)
	require.NoError(t, err)
	require.Equal(t, AbortBusy, reason)
	require.Zero(t, maxVersion)

	msg, err = DecodeMessage(EncodeAbort(AbortVersionUnsupported))
	require.NoError(t, err)
	reason, maxVersion, err = DecodeAbort(msg.Body)
	require.NoError(t, err)
	require.Equal(t, AbortVersionUnsupported, reason)
	require.Equal(t, Version1, maxVersion)

	_, _, err = DecodeAbort([]byte{1})
	require.ErrorIs(t, err, ErrProtocol)
}

// TestTLVOddEven checks the "it's ok to be odd" rule on top of lnd's stream:
// unknown odd types are skipped, unknown even types fail, and non-canonical
// streams fail.
func TestTLVOddEven(t *testing.T) {
	t.Parallel()

	var known [32]byte
	known[0] = 0x42
	encode := func(records ...tlv.Record) []byte {
		b, err := EncodeTLV(records...)
		require.NoError(t, err)
		return b
	}

	odd := []byte{9, 9}
	even := []byte{8, 8}
	withOdd := encode(
		tlv.MakePrimitiveRecord(2, &known),
		tlv.MakePrimitiveRecord(5, &odd),
	)
	withEven := encode(
		tlv.MakePrimitiveRecord(2, &known),
		tlv.MakePrimitiveRecord(4, &even),
	)

	var got [32]byte
	parsed, err := DecodeTLV(withOdd, tlv.MakePrimitiveRecord(2, &got))
	require.NoError(t, err)
	require.Equal(t, known, got)
	require.Contains(t, parsed, tlv.Type(2))
	require.Contains(t, parsed, tlv.Type(5))

	_, err = DecodeTLV(withEven, tlv.MakePrimitiveRecord(2, &got))
	require.ErrorIs(t, err, ErrProtocol)

	// Type 2 after type 5 is not canonical.
	unsorted := append(encode(tlv.MakePrimitiveRecord(5, &odd)), encode(
		tlv.MakePrimitiveRecord(2, &known),
	)...)
	_, err = DecodeTLV(unsorted, tlv.MakePrimitiveRecord(2, &got))
	require.ErrorIs(t, err, ErrProtocol)

	// Truncated value.
	_, err = DecodeTLV(withOdd[:10], tlv.MakePrimitiveRecord(2, &got))
	require.ErrorIs(t, err, ErrProtocol)
}
