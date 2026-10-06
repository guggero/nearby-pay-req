package ndef

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestEncodeURIMessage pins the exact wire bytes for the record shapes an
// emulated tag serves: a short record with no URI abbreviation, a long
// record, and an abbreviated https:// prefix.
func TestEncodeURIMessage(t *testing.T) {
	t.Parallel()

	longURI := "lightning:" + strings.Repeat("q", 300)
	tests := []struct {
		name    string
		uri     string
		wantHex string
		wantErr string
	}{{
		name: "short record without abbreviation",
		uri:  "bitcoin:bc1q",
		// MB|ME|SR|TNF=1, type length 1, payload length 13, 'U',
		// code 0, "bitcoin:bc1q".
		wantHex: "d1010d5500" + hex.EncodeToString(
			[]byte("bitcoin:bc1q"),
		),
	}, {
		name: "abbreviated https prefix",
		uri:  "https://example.com",
		wantHex: "d1010c5504" + hex.EncodeToString(
			[]byte("example.com"),
		),
	}, {
		name: "longest prefix wins",
		uri:  "https://www.example.com",
		wantHex: "d1010c5502" + hex.EncodeToString(
			[]byte("example.com"),
		),
	}, {
		name: "long record",
		uri:  longURI,
		// MB|ME|TNF=1, type length 1, 4-byte payload length 311.
		wantHex: "c101000001375500" + hex.EncodeToString(
			[]byte(longURI),
		),
	}, {
		name:    "empty",
		uri:     "",
		wantErr: "empty URI",
	}, {
		name:    "NUL byte",
		uri:     "bitcoin:\x00",
		wantErr: "invalid UTF-8",
	}, {
		name:    "invalid UTF-8",
		uri:     "bitcoin:\xff",
		wantErr: "invalid UTF-8",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := EncodeURIMessage(tc.uri)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantHex, hex.EncodeToString(got))
		})
	}
}

// TestEncodeURIMessageRoundTrip checks that every encodable URI decodes back
// to itself, so an emulated tag is read as exactly the string the QR code
// shows.
func TestEncodeURIMessageRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Mix known prefixes in so the abbreviation path is exercised
		// as often as the plain one.
		prefix := rapid.SampledFrom(append(
			uriPrefixes[:], "lightning:", "bitcoin:",
		)).Draw(t, "prefix")

		// NUL is the one valid code point a reader refuses; the
		// encoder refuses it too, which the table test covers.
		body := strings.ReplaceAll(
			rapid.StringN(0, 600, -1).Draw(t, "body"), "\x00", "",
		)
		uri := prefix + body
		if uri == "" {
			uri = "x"
		}

		msg, err := EncodeURIMessage(uri)
		require.NoError(t, err)

		got, err := DecodeURIMessage(msg)
		require.NoError(t, err)
		require.Equal(t, uri, got)
	})
}
