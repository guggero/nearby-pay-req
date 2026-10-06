package wire

import (
	"bytes"
	"fmt"

	"github.com/lightningnetwork/lnd/tlv"
)

// EncodeTLV serialises records as a canonical BOLT 1 TLV stream. Records are
// sorted by type first, so callers can list them in any order.
func EncodeTLV(records ...tlv.Record) ([]byte, error) {
	tlv.SortRecords(records)
	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := stream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// DecodeTLV parses data into the given records and returns the set of types
// that were present. It applies the BOLT 1 "it's ok to be odd" rule that
// lnd's stream leaves to the caller: an unknown odd type is skipped, an
// unknown even type is a protocol violation. Streams must be canonical
// (strictly increasing types), which the underlying decoder enforces.
func DecodeTLV(data []byte, records ...tlv.Record) (tlv.TypeMap, error) {
	tlv.SortRecords(records)
	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	// The P2P variant caps every record at 64 KiB before allocating,
	// which matters even though our messages are already bounded:
	// a length field is attacker-controlled.
	parsed, err := stream.DecodeWithParsedTypesP2P(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: invalid TLV stream: %v", ErrProtocol,
			err)
	}

	// Known records are reported with a nil value; anything else is a
	// type we don't understand.
	for typ, value := range parsed {
		if value != nil && typ%2 == 0 {
			return nil, fmt.Errorf("%w: unknown required TLV type "+
				"%d", ErrProtocol, typ)
		}
	}

	return parsed, nil
}
