package trayicon

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"testing"
)

func TestBytesReturnsPNGForNonWindows(t *testing.T) {
	b, err := Bytes("darwin")
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if !bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatalf("got non-PNG header % x", b[:8])
	}
	if _, err := png.Decode(bytes.NewReader(b)); err != nil {
		t.Fatalf("decode PNG: %v", err)
	}
}

func TestBytesReturnsICOForWindows(t *testing.T) {
	b, err := Bytes("windows")
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if len(b) < 6 {
		t.Fatalf("ICO too short: %d", len(b))
	}
	if got := binary.LittleEndian.Uint16(b[0:2]); got != 0 {
		t.Fatalf("reserved = %d", got)
	}
	if got := binary.LittleEndian.Uint16(b[2:4]); got != 1 {
		t.Fatalf("type = %d", got)
	}
	count := int(binary.LittleEndian.Uint16(b[4:6]))
	if count != 3 {
		t.Fatalf("image count = %d", count)
	}
	for i, wantSize := range []byte{16, 32, 48} {
		entry := b[6+i*16:]
		if entry[0] != wantSize || entry[1] != wantSize {
			t.Fatalf("entry %d size = %dx%d", i, entry[0], entry[1])
		}
		if got := binary.LittleEndian.Uint16(entry[6:]); got != 32 {
			t.Fatalf("entry %d bit depth = %d", i, got)
		}
		size := int(binary.LittleEndian.Uint32(entry[8:]))
		offset := int(binary.LittleEndian.Uint32(entry[12:]))
		if size <= 40 || offset < 6+count*16 || offset+size > len(b) {
			t.Fatalf("entry %d invalid payload offset=%d size=%d len=%d", i, offset, size, len(b))
		}
		if got := binary.LittleEndian.Uint32(b[offset:]); got != 40 {
			t.Fatalf("entry %d DIB header size = %d", i, got)
		}
	}
}
