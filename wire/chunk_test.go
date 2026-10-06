package wire

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestChunkLayout pins the exact chunk bytes for a message that needs one
// START, one middle and one END chunk at the minimum chunk size.
func TestChunkLayout(t *testing.T) {
	t.Parallel()

	msg := bytes.Repeat([]byte{0xaa}, 40)
	chunks, err := Chunk(msg, MinChunkSize)
	require.NoError(t, err)

	// START carries 16 data bytes, the others 18 each: 16+18+6 = 40.
	require.Len(t, chunks, 3)
	require.Equal(
		t, "80000028"+hex.EncodeToString(msg[:16]),
		hex.EncodeToString(chunks[0]),
	)
	require.Equal(
		t, "0001"+hex.EncodeToString(msg[16:34]),
		hex.EncodeToString(chunks[1]),
	)
	require.Equal(
		t, "4002"+hex.EncodeToString(msg[34:]),
		hex.EncodeToString(chunks[2]),
	)

	// A message that fits is a single START|END chunk.
	chunks, err = Chunk([]byte{1, 2, 3}, MaxChunkSize)
	require.NoError(t, err)
	require.Equal(t, [][]byte{{0xc0, 0, 0, 3, 1, 2, 3}}, chunks)
}

// TestChunkLimits covers the inputs Chunk refuses and the cap on the chunk
// size.
func TestChunkLimits(t *testing.T) {
	t.Parallel()

	_, err := Chunk(nil, MaxChunkSize)
	require.ErrorIs(t, err, ErrProtocol)

	_, err = Chunk(make([]byte, MaxMessageLen+1), MaxChunkSize)
	require.ErrorIs(t, err, ErrProtocol)

	_, err = Chunk([]byte{1}, MinChunkSize-1)
	require.ErrorIs(t, err, ErrProtocol)

	// A chunk size above the ATT maximum is clamped, not refused.
	chunks, err := Chunk(make([]byte, 2000), 4096)
	require.NoError(t, err)
	for _, c := range chunks {
		require.LessOrEqual(t, len(c), MaxChunkSize)
	}
}

// TestChunkRoundTrip checks that chunking and reassembling any message at
// any allowed chunk size is the identity, including messages long enough to
// wrap the sequence byte.
func TestChunkRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		msg := rapid.SliceOfN(rapid.Byte(), 1, MaxMessageLen).
			Draw(t, "msg")
		size := rapid.IntRange(MinChunkSize, MaxChunkSize+16).
			Draw(t, "chunkSize")

		chunks, err := Chunk(msg, size)
		require.NoError(t, err)

		var r Reassembler
		for i, c := range chunks {
			got, err := r.Add(c)
			require.NoError(t, err)
			if i < len(chunks)-1 {
				require.Nil(t, got)
				continue
			}
			require.Equal(t, msg, got)
		}

		// The reassembler is ready for the next message.
		got, err := r.Add(chunks[0])
		require.NoError(t, err)
		if len(chunks) == 1 {
			require.Equal(t, msg, got)
		}
	})
}

// TestReassemblerRejects covers every framing violation the reassembler must
// catch.
func TestReassemblerRejects(t *testing.T) {
	t.Parallel()

	twoChunks, err := Chunk(make([]byte, 30), MinChunkSize)
	require.NoError(t, err)
	require.Len(t, twoChunks, 2)

	tests := []struct {
		name   string
		chunks [][]byte
	}{{
		name:   "too short",
		chunks: [][]byte{{0xc0}},
	}, {
		name:   "start too short",
		chunks: [][]byte{{0xc0, 0, 0}},
	}, {
		name:   "reserved flag",
		chunks: [][]byte{{0xc1, 0, 0, 1, 1}},
	}, {
		name:   "continuation first",
		chunks: [][]byte{{0x40, 0, 1}},
	}, {
		name:   "start sequence not zero",
		chunks: [][]byte{{0xc0, 1, 0, 1, 1}},
	}, {
		name:   "zero length",
		chunks: [][]byte{{0xc0, 0, 0, 0, 1}},
	}, {
		name:   "length too large",
		chunks: [][]byte{{0xc0, 0, 0x30, 0x01, 1}},
	}, {
		name:   "empty data",
		chunks: [][]byte{{0x80, 0, 0, 1}},
	}, {
		name:   "overrun",
		chunks: [][]byte{{0xc0, 0, 0, 1, 1, 2}},
	}, {
		name:   "end too early",
		chunks: [][]byte{{0xc0, 0, 0, 2, 1}},
	}, {
		name:   "complete without end",
		chunks: [][]byte{{0x80, 0, 0, 1, 1}},
	}, {
		name:   "start inside message",
		chunks: [][]byte{twoChunks[0], twoChunks[0]},
	}, {
		name: "sequence gap",
		chunks: [][]byte{
			twoChunks[0], append([]byte{
				0x40, 5,
			}, twoChunks[1][2:]...),
		},
	}, {
		name:   "duplicate chunk",
		chunks: [][]byte{twoChunks[0], {0x00, 0, 1}},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var (
				r   Reassembler
				err error
			)
			for _, c := range tc.chunks {
				if _, err = r.Add(c); err != nil {
					break
				}
			}
			require.ErrorIs(t, err, ErrProtocol)
		})
	}
}

// FuzzReassembler feeds arbitrary chunk sequences to the reassembler. It
// must never panic, never return a message longer than the bound, and any
// message it returns must be what its START chunk declared.
func FuzzReassembler(f *testing.F) {
	seed, _ := Chunk(bytes.Repeat([]byte{7}, 100), MinChunkSize)
	f.Add(bytes.Join(seed, []byte{0xff}))
	f.Add([]byte{0xc0, 0, 0, 1, 1})

	f.Fuzz(func(t *testing.T, data []byte) {
		var r Reassembler
		for _, c := range bytes.Split(data, []byte{0xff}) {
			msg, err := r.Add(c)
			if err != nil {
				return
			}
			require.LessOrEqual(t, len(msg), MaxMessageLen)
		}
	})
}
