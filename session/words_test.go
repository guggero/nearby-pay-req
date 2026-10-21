package session

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	// bip39EnglishSHA256 is the SHA-256 of english.txt as published with
	// BIP 39.
	bip39EnglishSHA256 = "2f5eed53a4727b4bf8880d8f3f199efc90e58503" +
		"646d9ff8eff3a2ed3b24dbda"
)

// TestWordList checks that the embedded list is the published BIP 39
// English list, so every wallet's code maps to the same words.
func TestWordList(t *testing.T) {
	t.Parallel()

	sum := sha256.Sum256([]byte(bip39English))
	require.Equal(t, bip39EnglishSHA256, hex.EncodeToString(sum[:]))

	require.Len(t, wordList, 1<<codeWordBits)
	require.Equal(t, "abandon", wordList[0])
	require.Equal(t, "zoo", wordList[len(wordList)-1])
}

// TestCodeFromDigest pins which bits of the digest pick which word: the
// first 11 bits the first word, the next 11 the second, the rest none.
func TestCodeFromDigest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		digest string
		code   string
	}{{
		name:   "all zero",
		digest: "00000000",
		code:   "abandon abandon",
	}, {
		name:   "first word index 1",
		digest: "00200000",
		code:   "ability abandon",
	}, {
		name:   "second word index 1",
		digest: "00000400",
		code:   "abandon ability",
	}, {
		name:   "bits after the 22nd ignored",
		digest: "000003ff" + strings.Repeat("ff", 28),
		code:   "abandon abandon",
	}, {
		name:   "all one",
		digest: "ffffffff",
		code:   "zoo zoo",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			digest, err := hex.DecodeString(tc.digest)
			require.NoError(t, err)
			require.Equal(t, tc.code, codeFromDigest(digest))
		})
	}
}

// requireCode checks that code is codeWordCount words of the word list,
// joined by single spaces.
func requireCode(t *testing.T, code string) {
	t.Helper()

	words := strings.Split(code, " ")
	require.Len(t, words, codeWordCount)
	for _, w := range words {
		require.Contains(t, wordList, w)
	}
}
