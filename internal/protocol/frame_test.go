package protocol

import (
	"bytes"
	"testing"
)

// buildFrame constructs a wire frame with a deliberately bogus CRC, since the
// scanner does not validate CRC (neither does the firmware).
func buildFrame(cmd byte, data []byte) []byte {
	dataLen := 1 + len(data) + 2
	b := []byte{0x86, 0x68, byte(dataLen >> 8), byte(dataLen), cmd}
	b = append(b, data...)
	b = append(b, 0x00, 0x00)       // CRC placeholder (unchecked)
	b = append(b, 0x55, 0xAA)       // footer
	return b
}

func TestScannerSingleFrame(t *testing.T) {
	var s Scanner
	frames := s.Push(buildFrame(CmdCtrlMulti, []byte("ls -e -s /sd/gcodes\n")))
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	if frames[0].Cmd != CmdCtrlMulti {
		t.Errorf("cmd = %#x, want CTRL_MULTI", frames[0].Cmd)
	}
	if string(frames[0].Data) != "ls -e -s /sd/gcodes\n" {
		t.Errorf("data = %q", frames[0].Data)
	}
}

func TestScannerByteAtATime(t *testing.T) {
	// A frame split across single-byte reads must still parse exactly once.
	full := buildFrame(CmdFileData, bytes.Repeat([]byte{0x42}, 100))
	var s Scanner
	var got []Frame
	for _, c := range full {
		got = append(got, s.Push([]byte{c})...)
	}
	if len(got) != 1 {
		t.Fatalf("got %d frames, want 1", len(got))
	}
	if len(got[0].Data) != 100 {
		t.Errorf("data len = %d, want 100", len(got[0].Data))
	}
}

func TestScannerMultipleAndGarbage(t *testing.T) {
	var buf []byte
	buf = append(buf, 0xDE, 0xAD, 0xBE, 0xEF) // leading garbage
	buf = append(buf, buildFrame(CmdLoadInfo, []byte("file.nc 1234 20260101120000\r\n"))...)
	buf = append(buf, 0x01, 0x02) // inter-frame garbage
	buf = append(buf, buildFrame(CmdLoadFinish, []byte("ok\r\n"))...)

	var s Scanner
	frames := s.Push(buf)
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}
	if frames[0].Cmd != CmdLoadInfo || frames[1].Cmd != CmdLoadFinish {
		t.Errorf("cmds = %#x, %#x", frames[0].Cmd, frames[1].Cmd)
	}
}

func TestScannerResyncOnBadFooter(t *testing.T) {
	// A header with plausible length but wrong footer must not wedge the
	// scanner: it should skip and find the real frame that follows.
	bad := []byte{0x86, 0x68, 0x00, 0x04, CmdCtrlSingle, '?', 0x00, 0x00, 0x99, 0x99}
	good := buildFrame(CmdNormalInfo, []byte("hi"))
	var s Scanner
	frames := s.Push(append(bad, good...))
	if len(frames) == 0 {
		t.Fatal("expected to recover the good frame after bad footer")
	}
	last := frames[len(frames)-1]
	if last.Cmd != CmdNormalInfo || string(last.Data) != "hi" {
		t.Errorf("recovered frame = %#x %q", last.Cmd, last.Data)
	}
}
