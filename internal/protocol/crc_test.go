package protocol

import "testing"

// These reference values come from the official controller's crctable
// (XMODEM.py) — the generated table must match it byte-for-byte.
func TestCRCTableMatchesReference(t *testing.T) {
	cases := map[int]uint16{
		0:   0x0000,
		1:   0x1021,
		2:   0x2042,
		255: 0x1ef0,
		128: 0x9188,
	}
	for idx, want := range cases {
		if crcTable[idx] != want {
			t.Errorf("crcTable[%d] = %#04x, want %#04x", idx, crcTable[idx], want)
		}
	}
}

// TestEncodeMatchesController pins a full encoded frame to the exact bytes the
// official Python controller produces for the same command, including CRC.
func TestEncodeMatchesController(t *testing.T) {
	got := Encode(CmdCtrlMulti, []byte("ls -e -s /sd/gcodes\n"))
	want := []byte{
		0x86, 0x68, 0x00, 0x17, 0xa2,
		'l', 's', ' ', '-', 'e', ' ', '-', 's', ' ', '/', 's', 'd', '/', 'g', 'c', 'o', 'd', 'e', 's', '\n',
		0x4c, 0xd4, 0x55, 0xaa,
	}
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d len(want)=%d\ngot=%x", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("byte %d: got %#02x want %#02x\nfull got=%x", i, got[i], want[i], got)
		}
	}
}

// TestEncodeDecodeRoundTrip ensures a frame we encode scans back to the same
// command and data.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	var s Scanner
	frames := s.Push(Encode(CmdFileMD5, []byte("d41d8cd98f00b204e9800998ecf8427e")))
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	if frames[0].Cmd != CmdFileMD5 || string(frames[0].Data) != "d41d8cd98f00b204e9800998ecf8427e" {
		t.Errorf("round trip mismatch: %#x %q", frames[0].Cmd, frames[0].Data)
	}
}
