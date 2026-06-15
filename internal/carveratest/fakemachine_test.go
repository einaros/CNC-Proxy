package carveratest

import (
	"net"
	"testing"
	"time"

	"github.com/uwin/cnc-proxy/internal/protocol"
)

func TestFakeMachineDiagnoseUsesDiagRes(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	c, err := net.Dial("tcp", m.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := c.Write(protocol.Encode(protocol.CmdCtrlMulti, []byte("diagnose\n"))); err != nil {
		t.Fatal(err)
	}

	c.SetReadDeadline(time.Now().Add(time.Second))
	var scan protocol.Scanner
	buf := make([]byte, 1024)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	frames := scan.Push(buf[:n])
	if len(frames) == 0 {
		t.Fatal("no response frame")
	}
	if frames[0].Cmd != protocol.CmdDiagRes {
		t.Fatalf("diagnose response cmd = %s, want DIAG_RES", protocol.CmdName(frames[0].Cmd))
	}
}
