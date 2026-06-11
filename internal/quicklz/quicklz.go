// Package quicklz is a Go port of QuickLZ 1.5.0 at compression level 3 with
// streaming disabled (QLZ_STREAMING_BUFFER = 0), matching the settings the
// Carvera firmware was built with (see vendor quicklz.h). The firmware
// decompresses uploaded `.lz` files with qlz_decompress, so our Compress output
// must be a valid level-3 stream it accepts; Decompress is the inverse, used
// when downloading `.lz` sidecars.
//
// Only level 3 is implemented, because that is the only level the firmware uses.
// The port follows vendor/.../quicklz.c closely, using integer indices in place
// of pointers.
package quicklz

import "errors"

const (
	hashValues = 4096
	pointers   = 16 // QLZ_POINTERS for level 3
	minOffset  = 2
	uncondMatch = 6 // UNCONDITIONAL_MATCHLEN
	uncompEnd   = 4 // UNCOMPRESSED_END
	cwordLen    = 4
	level       = 3
)

// ErrCorrupt is returned when decompression fails on malformed input.
var ErrCorrupt = errors.New("quicklz: corrupt or unsupported stream")

func hashFunc(i uint32) uint32 { return ((i >> 12) ^ i) & (hashValues - 1) }

// readLE reads n (1..4) bytes little-endian from b at i, zero-padding if the
// read would run past the end (matches fast_read on a guarded buffer).
func readLE(b []byte, i int, n int) uint32 {
	var v uint32
	for k := 0; k < n; k++ {
		if i+k < len(b) {
			v |= uint32(b[i+k]) << (8 * k)
		}
	}
	return v
}

func read3(b []byte, i int) uint32 { return readLE(b, i, 3) }

func hashAt(b []byte, i int) uint32 { return hashFunc(read3(b, i)) }

// writeLE writes the low n bytes of f little-endian into dst starting at p,
// growing dst as needed, and returns the new length.
func appendLE(dst []byte, f uint32, n int) []byte {
	for k := 0; k < n; k++ {
		dst = append(dst, byte(f>>(8*k)))
	}
	return dst
}

// state holds the level-3 compressor hash table.
type state struct {
	offset      [hashValues][pointers]int
	hashCounter [hashValues]byte
}

// CompressBlock compresses src (a single block, e.g. up to 4096 bytes) into a
// standalone QuickLZ level-3 frame. Each call uses fresh state, matching the
// controller's per-block compression (streaming disabled).
func CompressBlock(src []byte) []byte {
	size := len(src)
	if size == 0 {
		return nil
	}
	base := 9
	if size < 216 {
		base = 3
	}

	body, compressed := compressCore(src)
	var out []byte
	if !compressed {
		// Incompressible: store raw after the header.
		out = make([]byte, base, base+size)
		out = append(out, src...)
	} else {
		out = make([]byte, base, base+len(body))
		out = append(out, body...)
	}
	r := len(out)

	// Header. Byte 0 layout: 01SSLLHC (C=compressed, H=header-size bit via the
	// "2" flag, LL=level, SS=streaming).
	cflag := byte(0)
	if compressed {
		cflag = 1
	}
	if base == 3 {
		out[0] = cflag
		out[1] = byte(r)
		out[2] = byte(size)
	} else {
		out[0] = 2 | cflag
		putLE(out[1:], uint32(r), 4)
		putLE(out[5:], uint32(size), 4)
	}
	out[0] |= level << 2
	out[0] |= 1 << 6
	// streaming setting bits (SS) are 0 for QLZ_STREAMING_BUFFER == 0.
	return out
}

func putLE(dst []byte, f uint32, n int) {
	for k := 0; k < n; k++ {
		dst[k] = byte(f >> (8 * k))
	}
}

// compressCore is the port of qlz_compress_core for level 3. It returns the
// compressed body (excluding the header) and whether compression was kept
// (false means the data was incompressible and should be stored raw).
func compressCore(source []byte) (body []byte, compressed bool) {
	size := len(source)
	st := &state{}

	// destination buffer; we track the control-word slot by index.
	dst := make([]byte, 0, size+cwordLen+16)
	cwordIdx := 0
	dst = append(dst, 0, 0, 0, 0) // reserve first control word
	cwordVal := uint32(1) << 31

	src := 0
	lastByte := size - 1
	lastMatchstart := lastByte - uncondMatch - uncompEnd

	flushCword := func() {
		putLE(dst[cwordIdx:], (cwordVal>>1)|(1<<31), cwordLen)
		cwordIdx = len(dst)
		dst = append(dst, 0, 0, 0, 0)
		cwordVal = uint32(1) << 31
	}

	for src <= lastMatchstart {
		if (cwordVal & 1) == 1 {
			// Bail out to "store uncompressed" if the ratio is too poor. In the
			// C, dst-destination is bytes emitted so far (== len(dst)) and
			// src-source == src.
			if src > size>>1 && len(dst) > src-(src>>5) {
				return nil, false
			}
			flushCword()
		}

		f := read3(source, src)
		hash := hashFunc(f)
		c := st.hashCounter[hash]

		remaining := lastByte - uncompEnd - src + 1
		if remaining > 255 {
			remaining = 255
		}

		// Find the best match among stored offsets for this hash.
		var matchlen int
		offset2 := 0
		if c > 0 {
			o0 := st.offset[hash][0]
			if o0 < src-minOffset && (read3(source, o0)^f)&0xffffff == 0 {
				matchlen = 3
				if at(source, o0+matchlen) == at(source, src+matchlen) {
					matchlen = 4
					for at(source, o0+matchlen) == at(source, src+matchlen) && matchlen < remaining {
						matchlen++
					}
				}
				offset2 = o0
			}
		}
		for k := 1; k < pointers && int(c) > k; k++ {
			o := st.offset[hash][k]
			if (read3(source, o)^f)&0xffffff == 0 && o < src-minOffset {
				m := 3
				for at(source, o+m) == at(source, src+m) && m < remaining {
					m++
				}
				if m > matchlen || (m == matchlen && o > offset2) {
					offset2 = o
					matchlen = m
				}
			}
		}

		o := offset2
		st.offset[hash][int(c)&(pointers-1)] = src
		c++
		st.hashCounter[hash] = c

		if matchlen > 2 && src-o < 131071 {
			offset := uint32(src - o)
			for u := 1; u < matchlen; u++ {
				h2 := hashAt(source, src+u)
				cc := st.hashCounter[h2]
				st.offset[h2][int(cc)&(pointers-1)] = src + u
				st.hashCounter[h2] = cc + 1
			}
			cwordVal = (cwordVal >> 1) | (1 << 31)
			src += matchlen

			switch {
			case matchlen == 3 && offset <= 63:
				dst = append(dst, byte(offset<<2))
			case matchlen == 3 && offset <= 16383:
				dst = appendLE(dst, (offset<<2)|1, 2)
			case matchlen <= 18 && offset <= 1023:
				dst = appendLE(dst, (uint32(matchlen-3)<<2)|(offset<<6)|2, 2)
			case matchlen <= 33:
				dst = appendLE(dst, (uint32(matchlen-2)<<2)|(offset<<7)|3, 3)
			default:
				dst = appendLE(dst, (uint32(matchlen-3)<<7)|(offset<<15)|3, 4)
			}
		} else {
			dst = append(dst, source[src])
			src++
			cwordVal >>= 1
		}
	}

	// Tail: remaining literals.
	for src <= lastByte {
		if (cwordVal & 1) == 1 {
			flushCword()
		}
		dst = append(dst, source[src])
		src++
		cwordVal >>= 1
	}

	// Flush the final control word into its slot.
	for (cwordVal & 1) != 1 {
		cwordVal >>= 1
	}
	putLE(dst[cwordIdx:], (cwordVal>>1)|(1<<31), cwordLen)

	if len(dst) < 9 {
		// pad so qlz_size_* can read 9 bytes; firmware enforces min 9.
		for len(dst) < 9 {
			dst = append(dst, 0)
		}
	}
	return dst, true
}

// at returns source[i] or 0 if out of range (guarded reads during matching).
func at(b []byte, i int) byte {
	if i < 0 || i >= len(b) {
		return 0
	}
	return b[i]
}
