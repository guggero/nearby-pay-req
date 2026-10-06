// Package wire holds the byte-level codecs of the nearby payment-request
// protocol: the chunk layer that splits a message into ATT-sized values, the
// message header, and the TLV streams carried inside sealed messages. It
// knows nothing about Noise, sessions or radios, so every rule here is
// testable from bytes alone. See spec/ for the protocol this implements.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	// MaxMessageLen bounds one reassembled message. The largest message
	// the protocol defines carries a payment request of at most 8192
	// bytes plus a few TLV and AEAD bytes, so this leaves headroom for
	// optional fields without letting a peer make us buffer much.
	MaxMessageLen = 12288

	// MinChunkSize is the smallest chunk the protocol runs over: the ATT
	// value size of the default 23-byte MTU. Anything smaller cannot even
	// carry a start header plus useful data.
	MinChunkSize = 20

	// MaxChunkSize caps the chunk size regardless of the negotiated MTU.
	// 512 is the largest attribute value ATT allows.
	MaxChunkSize = 512

	// chunkHeaderLen is the flags + sequence prefix of every chunk.
	chunkHeaderLen = 2

	// startHeaderLen is the prefix of a START chunk: the common header
	// plus the two-byte total message length.
	startHeaderLen = chunkHeaderLen + 2

	// flagStart marks the first chunk of a message.
	flagStart = 0x80

	// flagEnd marks the last chunk of a message.
	flagEnd = 0x40

	// flagsReserved are the flag bits a v1 peer must leave unset.
	flagsReserved = 0x3f
)

var (
	// ErrProtocol is returned for any chunk or message that violates the
	// framing rules. The session treats it as fatal and disconnects.
	ErrProtocol = errors.New("nearby protocol violation")
)

// Chunk splits msg into chunks of at most chunkSize bytes each. The first
// chunk carries the total length; the sequence byte counts up from zero and
// wraps, which is harmless because ATT delivers values in order and the
// reassembler only uses it to detect a lost or duplicated value.
func Chunk(msg []byte, chunkSize int) ([][]byte, error) {
	if len(msg) == 0 || len(msg) > MaxMessageLen {
		return nil, fmt.Errorf("%w: message length %d out of range",
			ErrProtocol, len(msg))
	}
	if chunkSize < MinChunkSize {
		return nil, fmt.Errorf("%w: chunk size %d below minimum %d",
			ErrProtocol, chunkSize, MinChunkSize)
	}
	chunkSize = min(chunkSize, MaxChunkSize)

	var (
		chunks [][]byte
		seq    byte
	)
	for offset := 0; offset < len(msg); seq++ {
		// The START chunk spends two more bytes on the total length,
		// so it has room for slightly less data than the rest.
		header := chunkHeaderLen
		if offset == 0 {
			header = startHeaderLen
		}
		n := min(chunkSize-header, len(msg)-offset)

		chunk := make([]byte, header, header+n)
		chunk[1] = seq
		if offset == 0 {
			chunk[0] |= flagStart
			binary.BigEndian.PutUint16(chunk[2:], uint16(len(msg)))
		}
		if offset+n == len(msg) {
			chunk[0] |= flagEnd
		}
		chunk = append(chunk, msg[offset:offset+n]...)

		chunks = append(chunks, chunk)
		offset += n
	}

	return chunks, nil
}

// Reassembler rebuilds messages from the chunks of one direction of one
// connection. It is not safe for concurrent use; each session owns its own.
type Reassembler struct {
	buf     []byte
	total   int
	nextSeq byte
	active  bool
}

// Add consumes one chunk. It returns the complete message once the END chunk
// arrived, or nil while more chunks are expected. Any framing violation
// returns an error wrapping ErrProtocol and leaves the reassembler unusable
// for the current message; the caller is expected to drop the connection.
func (r *Reassembler) Add(chunk []byte) ([]byte, error) {
	if len(chunk) < chunkHeaderLen {
		return nil, fmt.Errorf("%w: chunk too short", ErrProtocol)
	}
	flags, seq := chunk[0], chunk[1]
	if flags&flagsReserved != 0 {
		return nil, fmt.Errorf("%w: reserved chunk flags set",
			ErrProtocol)
	}

	// A START chunk opens a new message, which is only allowed when none
	// is in progress; anything else must continue the open one in order.
	start := flags&flagStart != 0
	switch {
	case start && r.active:
		return nil, fmt.Errorf("%w: start chunk inside a message",
			ErrProtocol)

	case !start && !r.active:
		return nil, fmt.Errorf("%w: continuation without a message",
			ErrProtocol)

	case !start && seq != r.nextSeq:
		return nil, fmt.Errorf("%w: chunk sequence %d, expected %d",
			ErrProtocol, seq, r.nextSeq)

	case start && seq != 0:
		return nil, fmt.Errorf("%w: start chunk sequence %d",
			ErrProtocol, seq)
	}

	data := chunk[chunkHeaderLen:]
	if start {
		if len(chunk) < startHeaderLen {
			return nil, fmt.Errorf("%w: start chunk too short",
				ErrProtocol)
		}
		total := int(binary.BigEndian.Uint16(chunk[2:]))
		if total == 0 || total > MaxMessageLen {
			return nil, fmt.Errorf("%w: message length %d out of "+
				"range", ErrProtocol, total)
		}
		data = chunk[startHeaderLen:]

		r.buf = make([]byte, 0, total)
		r.total = total
		r.active = true
	}

	// Chunk never emits a chunk without data, and accepting one would
	// let a peer keep a message open forever at no cost.
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: empty chunk", ErrProtocol)
	}

	// The declared total is a hard bound: never buffer past it, and the
	// END flag must land exactly on it.
	if len(r.buf)+len(data) > r.total {
		return nil, fmt.Errorf("%w: chunk overruns message length",
			ErrProtocol)
	}
	r.buf = append(r.buf, data...)
	r.nextSeq = seq + 1

	end := flags&flagEnd != 0
	complete := len(r.buf) == r.total
	switch {
	case end && !complete:
		return nil, fmt.Errorf("%w: message ended early", ErrProtocol)

	case complete && !end:
		return nil, fmt.Errorf("%w: missing end flag", ErrProtocol)

	case !end:
		return nil, nil
	}

	msg := r.buf
	*r = Reassembler{}

	return msg, nil
}
