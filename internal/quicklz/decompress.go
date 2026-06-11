package quicklz

// Header helpers, mirroring qlz_size_* in quicklz.c. Byte 0 bit 1 ("& 2")
// selects a 9-byte header (n=4) vs 3-byte (n=1).

func sizeDecompressed(src []byte) uint32 {
	n := 1
	if src[0]&2 == 2 {
		n = 4
	}
	r := readLE(src, 1+n, n)
	if n != 4 {
		r &= 0xffffffff >> uint((4-n)*8)
	}
	return r
}

func sizeCompressed(src []byte) uint32 {
	n := 1
	if src[0]&2 == 2 {
		n = 4
	}
	r := readLE(src, 1, n)
	if n != 4 {
		r &= 0xffffffff >> uint((4-n)*8)
	}
	return r
}

func sizeHeader(src []byte) int {
	if src[0]&2 == 2 {
		return 9
	}
	return 3
}

// DecompressBlock decompresses a single QuickLZ level-3 frame produced by
// CompressBlock (or by the firmware/controller). It returns the original bytes.
func DecompressBlock(src []byte) ([]byte, error) {
	if len(src) < 3 {
		return nil, ErrCorrupt
	}
	dsize := int(sizeDecompressed(src))
	if dsize < 0 {
		return nil, ErrCorrupt
	}
	dst := make([]byte, 0, dsize)

	if src[0]&1 == 0 {
		// Stored uncompressed: copy the bytes after the header.
		h := sizeHeader(src)
		if h+dsize > len(src) {
			return nil, ErrCorrupt
		}
		dst = append(dst, src[h:h+dsize]...)
		return dst, nil
	}

	d, err := decompressCore(src, dsize)
	if err != nil {
		return nil, err
	}
	return d, nil
}

// decompressCore ports qlz_decompress_core for level 3. dst grows to dsize.
func decompressCore(source []byte, dsize int) ([]byte, error) {
	bitlut := [16]uint32{4, 0, 1, 0, 2, 0, 1, 0, 3, 0, 1, 0, 2, 0, 1, 0}

	src := sizeHeader(source)
	lastSourceByte := int(sizeCompressed(source)) - 1
	dst := make([]byte, 0, dsize)
	lastDestByte := dsize - 1
	lastMatchstart := lastDestByte - uncondMatch - uncompEnd
	cwordVal := uint32(1)

	readSrc4 := func(i int) uint32 { return readLE(source, i, 4) }

	for {
		if cwordVal == 1 {
			if src+cwordLen-1 > lastSourceByte {
				return nil, ErrCorrupt
			}
			cwordVal = readSrc4(src)
			src += cwordLen
		}
		if src+4-1 > lastSourceByte {
			return nil, ErrCorrupt
		}
		fetch := readSrc4(src)

		if (cwordVal & 1) == 1 {
			var matchlen int
			var offset uint32
			cwordVal >>= 1

			switch {
			case fetch&3 == 0:
				offset = (fetch & 0xff) >> 2
				matchlen = 3
				src++
			case fetch&2 == 0:
				offset = (fetch & 0xffff) >> 2
				matchlen = 3
				src += 2
			case fetch&1 == 0:
				offset = (fetch & 0xffff) >> 6
				matchlen = int((fetch>>2)&15) + 3
				src += 2
			case fetch&127 != 3:
				offset = (fetch >> 7) & 0x1ffff
				matchlen = int((fetch>>2)&0x1f) + 2
				src += 3
			default:
				offset = fetch >> 15
				matchlen = int((fetch>>7)&255) + 3
				src += 4
			}

			offset2 := len(dst) - int(offset)
			if offset2 < 0 || offset2 > len(dst)-minOffset-1 {
				return nil, ErrCorrupt
			}
			if matchlen > lastDestByte-len(dst)-uncompEnd+1 {
				return nil, ErrCorrupt
			}
			// Copy with forward overlap semantics (memcpy_up).
			for k := 0; k < matchlen; k++ {
				dst = append(dst, dst[offset2+k])
			}
		} else {
			if len(dst) < lastMatchstart {
				n := bitlut[cwordVal&0xf]
				// Copy 4 bytes from src, advance dst/src by n.
				if src+4 > len(source) {
					// pad-read guard
				}
				for k := 0; k < 4; k++ {
					var b byte
					if src+k < len(source) {
						b = source[src+k]
					}
					if k < int(n) {
						dst = append(dst, b)
					}
				}
				cwordVal >>= n
				src += int(n)
			} else {
				for len(dst) <= lastDestByte {
					if cwordVal == 1 {
						src += cwordLen
						cwordVal = 1 << 31
					}
					if src >= lastSourceByte+1 {
						return nil, ErrCorrupt
					}
					dst = append(dst, source[src])
					src++
					cwordVal >>= 1
				}
				return dst, nil
			}
		}
	}
}
