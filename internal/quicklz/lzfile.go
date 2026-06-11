package quicklz

import (
	"encoding/binary"
	"errors"
	"io"
)

// BlockSize is the uncompressed input block size the controller uses (makera.py
// BLOCK_SIZE). Each block is QuickLZ-compressed independently.
const BlockSize = 4096

// CompressStream writes the controller's `.lz` container for r into w:
//
//	repeat: [compressed-size: 4 bytes BE][QuickLZ block]
//	then:   [checksum: 2 bytes BE]   (sum of all input bytes, & 0xffff)
//
// The firmware's decompress() consumes exactly this framing. Returns the number
// of input bytes consumed.
func CompressStream(w io.Writer, r io.Reader) (int64, error) {
	buf := make([]byte, BlockSize)
	var total int64
	var sum uint32
	hdr := make([]byte, 4)
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			block := buf[:n]
			for _, b := range block {
				sum += uint32(b)
			}
			total += int64(n)
			comp := CompressBlock(block)
			binary.BigEndian.PutUint32(hdr, uint32(len(comp)))
			if _, werr := w.Write(hdr); werr != nil {
				return total, werr
			}
			if _, werr := w.Write(comp); werr != nil {
				return total, werr
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return total, err
		}
	}
	var trailer [2]byte
	binary.BigEndian.PutUint16(trailer[:], uint16(sum&0xffff))
	if _, err := w.Write(trailer[:]); err != nil {
		return total, err
	}
	return total, nil
}

// ErrChecksum indicates the trailing checksum did not match the decompressed
// data.
var ErrChecksum = errors.New("quicklz: lz checksum mismatch")

// DecompressStream reads the controller's `.lz` container from src bytes and
// writes the original data to w, verifying the trailing checksum.
func DecompressStream(w io.Writer, src []byte) error {
	if len(src) < 2 {
		return errors.New("quicklz: lz stream too short")
	}
	body := src[:len(src)-2]
	wantSum := binary.BigEndian.Uint16(src[len(src)-2:])

	var sum uint32
	i := 0
	for i < len(body) {
		if i+4 > len(body) {
			return errors.New("quicklz: truncated block header")
		}
		blkLen := int(binary.BigEndian.Uint32(body[i : i+4]))
		i += 4
		if blkLen <= 0 || i+blkLen > len(body) {
			return errors.New("quicklz: truncated block")
		}
		out, err := DecompressBlock(body[i : i+blkLen])
		if err != nil {
			return err
		}
		i += blkLen
		for _, b := range out {
			sum += uint32(b)
		}
		if _, err := w.Write(out); err != nil {
			return err
		}
	}
	if uint16(sum&0xffff) != wantSum {
		return ErrChecksum
	}
	return nil
}
