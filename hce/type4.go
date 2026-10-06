// Package hce emulates a read-only NFC Forum Type 4 tag for Android host card
// emulation. The Android HostApduService forwards every command APDU here
// unchanged and returns whatever this answers, so the whole tag lives in Go
// and is testable from APDU transcripts alone. Only the NDEF Tag Application
// (AID D2760000850101) is served, and only while a share is active: with no
// message loaded every SELECT answers "file not found", so the app never
// captures a tap meant for another app.
package hce

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sync"
)

const (
	// maxNdefMessageLen bounds the message an emulated tag serves. READ
	// BINARY addresses at most offset 0x7FFF with a short P1P2, so the
	// whole file (NLEN + message) has to fit below that.
	maxNdefMessageLen = 0x7FFF - nlenSize

	// nlenSize is the NDEF file's two-byte length prefix.
	nlenSize = 2

	// maxResponseData is the largest data field we return per READ
	// BINARY, advertised as MLe in the capability container. 255 keeps
	// every response a short APDU, which every reader supports.
	maxResponseData = 0xFF

	// claISO is the only class byte we answer.
	claISO = 0x00

	// insSelect and insReadBinary are the two instructions a Type 4 tag
	// reader uses for a read-only tag.
	insSelect     = 0xA4
	insReadBinary = 0xB0

	// selectByName and selectByFileID are the SELECT P1 values for an
	// application and for an elementary file.
	selectByName   = 0x04
	selectByFileID = 0x00
)

var (
	// ndefAID is the NFC Forum NDEF Tag Application identifier.
	ndefAID = []byte{0xD2, 0x76, 0x00, 0x00, 0x85, 0x01, 0x01}

	// ccFileID and ndefFileID are the elementary files of the tag.
	ccFileID   = []byte{0xE1, 0x03}
	ndefFileID = []byte{0xE1, 0x04}

	// Status words.
	swOK               = []byte{0x90, 0x00}
	swFileNotFound     = []byte{0x6A, 0x82}
	swWrongLength      = []byte{0x67, 0x00}
	swWrongParams      = []byte{0x6B, 0x00}
	swNotAllowed       = []byte{0x69, 0x86}
	swInsNotSupported  = []byte{0x6D, 0x00}
	swClaNotSupported  = []byte{0x6E, 0x00}
	swFuncNotSupported = []byte{0x6A, 0x81}
)

// selectedFile is the elementary file READ BINARY reads from.
type selectedFile uint8

const (
	fileNone selectedFile = iota
	fileCC
	fileNDEF
)

// Tag is the emulated tag. Process is called from the Android main thread
// while SetMessage/Clear are called from the share's goroutine, so all state
// is behind one mutex.
type Tag struct {
	mu sync.Mutex

	// cc and ndef are the two file images; nil when no share is active.
	cc, ndef []byte

	// onRead fires once per application selection when the reader has
	// read the whole NDEF file.
	onRead func()

	appSelected bool
	file        selectedFile

	// servedUpTo is the highest NDEF file offset returned since the
	// application was last selected; notified latches onRead.
	servedUpTo int
	notified   bool
}

// New returns a tag with no message loaded.
func New() *Tag {
	return &Tag{}
}

// SetMessage loads an NDEF message to serve. onRead, which may be nil, is
// called — without the tag's lock held, so it may call back into the tag —
// each time a reader finishes reading the whole message. It must not block:
// it runs on the Android main thread.
func (t *Tag) SetMessage(message []byte, onRead func()) error {
	if len(message) == 0 || len(message) > maxNdefMessageLen {
		return fmt.Errorf("hce: NDEF message length %d out of range",
			len(message))
	}

	file := make([]byte, nlenSize, nlenSize+len(message))
	binary.BigEndian.PutUint16(file, uint16(len(message)))
	file = append(file, message...)

	t.mu.Lock()
	defer t.mu.Unlock()

	t.cc = capabilityContainer(len(file))
	t.ndef = file
	t.onRead = onRead
	t.resetSelection()

	return nil
}

// Clear stops serving: every SELECT answers "file not found" again.
func (t *Tag) Clear() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.cc, t.ndef, t.onRead = nil, nil, nil
	t.resetSelection()
}

// resetSelection forgets the application and file selection and the read
// progress. Callers hold mu.
func (t *Tag) resetSelection() {
	t.appSelected = false
	t.file = fileNone
	t.servedUpTo = 0
	t.notified = false
}

// Process answers one command APDU. It never panics on malformed input and
// always returns at least a status word.
func (t *Tag) Process(apdu []byte) []byte {
	resp, onRead := t.process(apdu)
	if onRead != nil {
		onRead()
	}

	return resp
}

// process is Process under the lock; it returns the callback to run once
// the lock is released.
func (t *Tag) process(apdu []byte) ([]byte, func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// CLA INS P1 P2 is the shortest valid command (case 1).
	if len(apdu) < 4 {
		return swWrongLength, nil
	}
	if apdu[0] != claISO {
		return swClaNotSupported, nil
	}

	switch apdu[1] {
	// SELECT picks the application, then a file within it.
	case insSelect:
		return t.handleSelect(apdu), nil

	// READ BINARY reads from the selected file.
	case insReadBinary:
		return t.handleReadBinary(apdu)

	// UPDATE BINARY and everything else: a read-only tag.
	default:
		return swInsNotSupported, nil
	}
}

// handleSelect implements SELECT by name (the NDEF application) and SELECT
// by file identifier (CC or NDEF file). Callers hold mu.
func (t *Tag) handleSelect(apdu []byte) []byte {
	if len(apdu) < 5 {
		return swWrongLength
	}
	lc := int(apdu[4])
	if len(apdu) < 5+lc {
		return swWrongLength
	}
	data := apdu[5 : 5+lc]

	switch apdu[2] {
	// Selecting the application starts a fresh read; when no share is
	// active we pretend not to exist so the reader moves on.
	case selectByName:
		if t.ndef == nil || !bytes.Equal(data, ndefAID) {
			t.resetSelection()
			return swFileNotFound
		}
		t.resetSelection()
		t.appSelected = true

		return swOK

	// A file can only be selected inside the application.
	case selectByFileID:
		if !t.appSelected {
			return swFileNotFound
		}
		switch {
		case bytes.Equal(data, ccFileID):
			t.file = fileCC

		case bytes.Equal(data, ndefFileID):
			t.file = fileNDEF

		default:
			t.file = fileNone
			return swFileNotFound
		}

		return swOK

	default:
		return swFuncNotSupported
	}
}

// handleReadBinary returns up to Le bytes of the selected file at the P1P2
// offset, and reports a completed NDEF read. Callers hold mu.
func (t *Tag) handleReadBinary(apdu []byte) ([]byte, func()) {
	var image []byte
	switch t.file {
	case fileCC:
		image = t.cc

	case fileNDEF:
		image = t.ndef

	default:
		return swNotAllowed, nil
	}

	// Short APDU case 2: CLA INS P1 P2 Le, with Le = 0 meaning 256.
	if len(apdu) != 5 {
		return swWrongLength, nil
	}
	offset := int(binary.BigEndian.Uint16(apdu[2:4]))
	if offset&0x8000 != 0 || offset > len(image) {
		return swWrongParams, nil
	}
	le := int(apdu[4])
	if le == 0 {
		le = 256
	}
	n := min(le, maxResponseData, len(image)-offset)

	resp := make([]byte, 0, n+len(swOK))
	resp = append(resp, image[offset:offset+n]...)
	resp = append(resp, swOK...)

	// A complete read is reported once per application selection, so a
	// reader re-reading the same tag in one session doesn't spam events.
	if t.file != fileNDEF {
		return resp, nil
	}
	t.servedUpTo = max(t.servedUpTo, offset+n)
	if t.notified || t.servedUpTo < len(t.ndef) || t.onRead == nil {
		return resp, nil
	}
	t.notified = true

	return resp, t.onRead
}

// capabilityContainer builds the 15-byte CC file for an NDEF file of the
// given size: mapping version 2.0, our MLe/MLc, and one NDEF file control
// TLV granting free read and no write access.
func capabilityContainer(ndefFileSize int) []byte {
	cc := make([]byte, 0, 15)
	cc = binary.BigEndian.AppendUint16(cc, 15)
	cc = append(cc, 0x20)
	cc = binary.BigEndian.AppendUint16(cc, maxResponseData)
	cc = binary.BigEndian.AppendUint16(cc, maxResponseData)
	cc = append(cc, 0x04, 0x06)
	cc = append(cc, ndefFileID...)
	cc = binary.BigEndian.AppendUint16(cc, uint16(ndefFileSize))

	return append(cc, 0x00, 0xFF)
}
