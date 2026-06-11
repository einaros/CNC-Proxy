package client

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net"
	"testing"
	"time"

	"github.com/uwin/cnc-proxy/internal/carveratest"
	"github.com/uwin/cnc-proxy/internal/protocol"
)

const testTimeout = 3 * time.Second

func dialFake(t *testing.T, m *carveratest.FakeMachine) *Conn {
	t.Helper()
	conn, err := Dial(m.Addr(), testTimeout)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// uploadFixture uploads content to remote via the client so the fake machine
// holds it, returning the content for later comparison.
func uploadFixture(t *testing.T, conn *Conn, remote string, size int) []byte {
	t.Helper()
	content := make([]byte, size)
	rand.Read(content)
	sum := md5.Sum(content)
	err := conn.Upload(remote, bytes.NewReader(content), int64(size), hex.EncodeToString(sum[:]), testTimeout, nil)
	if err != nil {
		t.Fatalf("upload fixture: %v", err)
	}
	return content
}

func TestListReflectsUploads(t *testing.T) {
	m, err := carveratest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	conn := dialFake(t, m)

	uploadFixture(t, conn, "/sd/gcodes/a.nc", 100)
	conn.Mkdir("/sd/gcodes/sub", testTimeout)

	entries, err := conn.List("/sd/gcodes", testTimeout)
	if err != nil {
		t.Fatal(err)
	}
	var sawFile, sawDir bool
	for _, e := range entries {
		if e.Name == "a.nc" && !e.IsDir && e.Size == 100 {
			sawFile = true
		}
		if e.Name == "sub" && e.IsDir {
			sawDir = true
		}
	}
	if !sawFile || !sawDir {
		t.Errorf("listing missing entries: %+v", entries)
	}
}

func TestRemoveRenameMkdir(t *testing.T) {
	m, _ := carveratest.New()
	defer m.Close()
	conn := dialFake(t, m)

	if err := conn.Remove("/sd/gcodes/x.nc", testTimeout); err != nil {
		t.Errorf("Remove: %v", err)
	}
	if err := conn.Rename("/sd/gcodes/a.nc", "/sd/gcodes/b.nc", testTimeout); err != nil {
		t.Errorf("Rename: %v", err)
	}
	if err := conn.Mkdir("/sd/gcodes/new", testTimeout); err != nil {
		t.Errorf("Mkdir: %v", err)
	}
	if !m.HasDir("/sd/gcodes/new") {
		t.Error("mkdir not recorded on machine")
	}
}

func TestMd5MatchesUploadedContent(t *testing.T) {
	m, _ := carveratest.New()
	defer m.Close()
	conn := dialFake(t, m)

	content := uploadFixture(t, conn, "/sd/gcodes/x.nc", 256)
	want := md5.Sum(content)

	got, err := conn.Md5("/sd/gcodes/x.nc", testTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if got != hex.EncodeToString(want[:]) {
		t.Errorf("md5 = %q, want %q", got, hex.EncodeToString(want[:]))
	}
}

func TestQueryState(t *testing.T) {
	m, _ := carveratest.New()
	defer m.Close()
	m.SetStatus("<Idle|MPos:0,0,0|WPos:0,0,0>")
	conn := dialFake(t, m)

	payload, err := conn.QueryState(testTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if payload != "<Idle|MPos:0,0,0|WPos:0,0,0>" {
		t.Errorf("status = %q", payload)
	}
}

func TestUploadRoundTrip(t *testing.T) {
	m, _ := carveratest.New()
	defer m.Close()
	conn := dialFake(t, m)

	content := make([]byte, WifiPacketSize*2+1234)
	rand.Read(content)
	sum := md5.Sum(content)

	var lastSent, lastTotal uint32
	err := conn.Upload("/sd/gcodes/big.nc", bytes.NewReader(content), int64(len(content)), hex.EncodeToString(sum[:]), testTimeout,
		func(sent, total uint32) { lastSent, lastTotal = sent, total })
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	got, ok := m.File("/sd/gcodes/big.nc")
	if !ok || !bytes.Equal(got, content) {
		t.Errorf("uploaded bytes differ: got %d bytes ok=%v, want %d", len(got), ok, len(content))
	}
	if lastSent != lastTotal || lastTotal != 3 {
		t.Errorf("progress final = %d/%d, want 3/3", lastSent, lastTotal)
	}
}

func TestDownloadRoundTrip(t *testing.T) {
	m, _ := carveratest.New()
	defer m.Close()
	conn := dialFake(t, m)

	// Seed a multi-chunk file on the machine via upload, then download it.
	content := uploadFixture(t, conn, "/sd/gcodes/dl.nc", WifiPacketSize*2+777)

	var buf bytes.Buffer
	var lastRecv, lastTotal uint32
	gotMD5, written, err := conn.Download("/sd/gcodes/dl.nc", &buf, testTimeout,
		func(recv, total uint32) { lastRecv, lastTotal = recv, total })
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if written != int64(len(content)) || !bytes.Equal(buf.Bytes(), content) {
		t.Errorf("downloaded %d bytes, want %d (equal=%v)", written, len(content), bytes.Equal(buf.Bytes(), content))
	}
	sum := md5.Sum(content)
	if gotMD5 != hex.EncodeToString(sum[:]) {
		t.Errorf("download md5 = %q, want %q", gotMD5, hex.EncodeToString(sum[:]))
	}
	if lastRecv != lastTotal || lastTotal != 3 {
		t.Errorf("progress final = %d/%d, want 3/3", lastRecv, lastTotal)
	}
}

func TestDownloadMissing(t *testing.T) {
	m, _ := carveratest.New()
	defer m.Close()
	conn := dialFake(t, m)

	_, _, err := conn.Download("/sd/gcodes/nope.nc", io.Discard, testTimeout, nil)
	if err != ErrDownloadCanceled {
		t.Errorf("download missing = %v, want ErrDownloadCanceled", err)
	}
}

// gcodeServer scripts a single response to one CTRL_MULTI gcode line: it sends
// the given frames, then optionally closes the connection. Used to exercise
// SendGcode's termination logic without the full fake machine.
func gcodeServer(t *testing.T, reply func(c net.Conn)) *Conn {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		// Read the one inbound gcode frame, then reply.
		buf := make([]byte, 512)
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		c.Read(buf)
		reply(c)
	}()
	conn, err := Dial(ln.Addr().String(), testTimeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestSendGcodeOkTerminates(t *testing.T) {
	conn := gcodeServer(t, func(c net.Conn) {
		c.Write(protocol.Encode(protocol.CmdNormalInfo, []byte("ok\r\n")))
	})
	out, err := conn.SendGcode("G0 X0", testTimeout)
	if err != nil || out != "" {
		t.Errorf("ok-only: out=%q err=%v", out, err)
	}
}

func TestSendGcodeOkWithPayload(t *testing.T) {
	conn := gcodeServer(t, func(c net.Conn) {
		c.Write(protocol.Encode(protocol.CmdNormalInfo, []byte("ok C: X:1.0 Y:2.0\r\n")))
	})
	out, err := conn.SendGcode("M114", testTimeout)
	if err != nil || out != "C: X:1.0 Y:2.0" {
		t.Errorf("ok-payload: out=%q err=%v", out, err)
	}
}

func TestSendGcodeOutputThenClose(t *testing.T) {
	// A console command (e.g. version) that emits output with NO "ok", then the
	// connection closes. The output must be returned successfully, not as EOF.
	conn := gcodeServer(t, func(c net.Conn) {
		c.Write(protocol.Encode(protocol.CmdNormalInfo, []byte("version = 1.0.5\n")))
		c.Close()
	})
	out, err := conn.SendGcode("version", testTimeout)
	if err != nil {
		t.Errorf("output-then-close: unexpected err=%v (out=%q)", err, out)
	}
	if out != "version = 1.0.5\n" {
		t.Errorf("output-then-close: out=%q", out)
	}
}

func TestSendGcodeNoOutputClose(t *testing.T) {
	// Connection closes with no output at all → a genuine error.
	conn := gcodeServer(t, func(c net.Conn) { c.Close() })
	_, err := conn.SendGcode("G0 X0", testTimeout)
	if err == nil {
		t.Error("no-output close should return an error")
	}
}

func TestSendGcodeError(t *testing.T) {
	conn := gcodeServer(t, func(c net.Conn) {
		c.Write(protocol.Encode(protocol.CmdNormalInfo, []byte("error:Bad command\r\n")))
	})
	_, err := conn.SendGcode("G999", testTimeout)
	if err == nil {
		t.Error("expected error for error: response")
	}
}

func TestUploadEmptyFile(t *testing.T) {
	m, _ := carveratest.New()
	defer m.Close()
	conn := dialFake(t, m)

	sum := md5.Sum(nil)
	err := conn.Upload("/sd/gcodes/empty.nc", bytes.NewReader(nil), 0, hex.EncodeToString(sum[:]), testTimeout, nil)
	if err != nil {
		t.Fatalf("Upload empty: %v", err)
	}
	got, ok := m.File("/sd/gcodes/empty.nc")
	if !ok || len(got) != 0 {
		t.Errorf("empty upload = %q ok=%v", got, ok)
	}
}
