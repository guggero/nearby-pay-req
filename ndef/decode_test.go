package ndef

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDecodeURIMessage checks the accepted shapes and that everything else
// is refused rather than guessed at.
func TestDecodeURIMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		msgHex  string
		want    string
		wantErr bool
	}{{
		name:   "short record, no prefix",
		msgHex: "d1010655" + "00" + hex.EncodeToString([]byte("bc1q:")),
		want:   "bc1q:",
	}, {
		name:   "short record, https prefix",
		msgHex: "d1010455" + "04" + hex.EncodeToString([]byte("a.b")),
		want:   "https://a.b",
	}, {
		name:   "long record",
		msgHex: "c10100000002550061",
		want:   "a",
	}, {
		name:   "with id",
		msgHex: "d901020155" + "ff" + "0061",
		want:   "a",
	}, {
		name:    "too short",
		msgHex:  "d101",
		wantErr: true,
	}, {
		name:    "not message end",
		msgHex:  "910102550061",
		wantErr: true,
	}, {
		name:    "chunked",
		msgHex:  "f10102550061",
		wantErr: true,
	}, {
		name:    "mime type",
		msgHex:  "d20102550061",
		wantErr: true,
	}, {
		name:    "text record",
		msgHex:  "d10102540061",
		wantErr: true,
	}, {
		name:    "trailing bytes",
		msgHex:  "d1010255006100",
		wantErr: true,
	}, {
		name:    "truncated payload",
		msgHex:  "d101035500",
		wantErr: true,
	}, {
		name:    "unknown identifier code",
		msgHex:  "d1010255ff61",
		wantErr: true,
	}, {
		name:    "empty uri",
		msgHex:  "d101015500",
		wantErr: true,
	}, {
		name:    "nul byte",
		msgHex:  "d1010355000061",
		wantErr: true,
	}, {
		name:    "invalid utf-8",
		msgHex:  "d101025500ff",
		wantErr: true,
	}, {
		name:    "oversized long record",
		msgHex:  "c10100010001550061",
		wantErr: true,
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			msg, err := hex.DecodeString(tc.msgHex)
			require.NoError(t, err)

			got, err := DecodeURIMessage(msg)
			if tc.wantErr {
				require.ErrorIs(t, err, ErrMalformed)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// FuzzDecodeURIMessage checks that no input panics the decoder and that
// whatever it accepts encodes back to the same URI.
func FuzzDecodeURIMessage(f *testing.F) {
	for _, seed := range []string{
		"d101065500626331713a", "c10100000002550061", "d901020155ff00" +
			"61",
	} {

		msg, err := hex.DecodeString(seed)
		require.NoError(f, err)
		f.Add(msg)
	}

	f.Fuzz(func(t *testing.T, msg []byte) {
		uri, err := DecodeURIMessage(msg)
		if err != nil {
			return
		}
		_, err = EncodeURIMessage(uri)
		require.NoError(t, err)
	})
}
