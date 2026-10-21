package session

import (
	_ "embed"
	"encoding/binary"
	"strings"
)

const (
	// codeWordCount is how many words a comparison code has.
	codeWordCount = 2

	// codeWordBits is how many bits of the code digest pick one word:
	// the word list has 2^11 words, as in a BIP 39 mnemonic.
	codeWordBits = 11
)

var (
	// bip39English is the English word list of BIP 39, byte for byte as
	// published: 2048 lowercase words, sorted, one per line.
	//
	//go:embed bip39-english.txt
	bip39English string

	// wordList is bip39English split into its words, indexed from 0.
	wordList = strings.Fields(bip39English)
)

// codeFromDigest turns the code digest into the comparison code: its first
// 22 bits, read as two 11-bit indices into the word list, exactly as a
// BIP 39 mnemonic maps entropy to words, with the words joined by one space.
// Whole bits keep every one of the 2^22 codes equally likely.
func codeFromDigest(digest []byte) string {
	bits := binary.BigEndian.Uint32(digest)

	words := make([]string, codeWordCount)
	for i := range words {
		shift := 32 - (i+1)*codeWordBits
		words[i] = wordList[bits>>shift&(1<<codeWordBits-1)]
	}

	return strings.Join(words, " ")
}
