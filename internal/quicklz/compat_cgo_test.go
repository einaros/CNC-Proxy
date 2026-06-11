//go:build cgo_compat

package quicklz

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestGoCompressFirmwareDecompress(t *testing.T) {
	for _, data := range compatCorpus() {
		comp := CompressBlock(data)
		got := firmwareDecompress(comp, len(data))
		if !bytes.Equal(got, data) {
			t.Fatalf("Go->C mismatch for %d bytes", len(data))
		}
	}
}

func TestFirmwareCompressGoDecompress(t *testing.T) {
	for _, data := range compatCorpus() {
		if len(data) == 0 {
			continue
		}
		comp := firmwareCompress(data)
		got, err := DecompressBlock(comp)
		if err != nil {
			t.Fatalf("Go decompress of C output failed (%d bytes): %v", len(data), err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("C->Go mismatch for %d bytes", len(data))
		}
	}
}

func compatCorpus() [][]byte {
	var corpus [][]byte
	rng := rand.New(rand.NewSource(1))
	for _, n := range []int{1, 2, 3, 16, 215, 216, 1000, 4096, 8000} {
		a := make([]byte, n)
		rng.Read(a)
		corpus = append(corpus, a)
		b := bytes.Repeat([]byte("QuickLZ!"), n/8+1)[:n]
		corpus = append(corpus, b)
	}
	return corpus
}
