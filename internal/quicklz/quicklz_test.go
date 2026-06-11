package quicklz

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"
)

func roundTrip(t *testing.T, data []byte) {
	t.Helper()
	comp := CompressBlock(data)
	if len(data) > 0 && len(comp) == 0 {
		t.Fatal("empty compression output for non-empty input")
	}
	got, err := DecompressBlock(comp)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("round trip mismatch: got %d bytes, want %d", len(got), len(data))
	}
}

func TestRoundTripCompressible(t *testing.T) {
	// Highly repetitive data exercises the match encoder paths.
	roundTrip(t, bytes.Repeat([]byte("ABCDABCDABCD"), 300))
	roundTrip(t, []byte(strings.Repeat("G1 X10 Y10 Z0.5\n", 200)))
}

func TestRoundTripIncompressible(t *testing.T) {
	data := make([]byte, 4096)
	rand.Read(data)
	roundTrip(t, data) // likely stored raw; must still round-trip
}

func TestRoundTripSizes(t *testing.T) {
	for _, n := range []int{1, 2, 3, 10, 100, 215, 216, 1000, 4095, 4096} {
		data := make([]byte, n)
		for i := range data {
			data[i] = byte(i%7) + 'a' // semi-repetitive
		}
		roundTrip(t, data)
	}
}

func TestRoundTripMixed(t *testing.T) {
	var b bytes.Buffer
	b.WriteString(strings.Repeat("hello world ", 50))
	r := make([]byte, 500)
	rand.Read(r)
	b.Write(r)
	b.WriteString(strings.Repeat("trailing data trailing data ", 30))
	roundTrip(t, b.Bytes())
}

func TestStreamRoundTrip(t *testing.T) {
	orig := []byte(strings.Repeat("The quick brown fox. ", 2000)) // multi-block
	var compressed bytes.Buffer
	n, err := CompressStream(&compressed, bytes.NewReader(orig))
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(orig)) {
		t.Errorf("consumed %d, want %d", n, len(orig))
	}
	var out bytes.Buffer
	if err := DecompressStream(&out, compressed.Bytes()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), orig) {
		t.Fatal("stream round trip mismatch")
	}
	// It should actually be smaller than the original for repetitive data.
	if compressed.Len() >= len(orig) {
		t.Errorf("compressed (%d) not smaller than original (%d)", compressed.Len(), len(orig))
	}
}

func TestStreamChecksumDetection(t *testing.T) {
	orig := []byte(strings.Repeat("data", 2000))
	var compressed bytes.Buffer
	CompressStream(&compressed, bytes.NewReader(orig))
	corrupt := compressed.Bytes()
	corrupt[len(corrupt)-1] ^= 0xff // break the trailing checksum
	if err := DecompressStream(&bytes.Buffer{}, corrupt); err != ErrChecksum {
		t.Errorf("expected ErrChecksum, got %v", err)
	}
}
