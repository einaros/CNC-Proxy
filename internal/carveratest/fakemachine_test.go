package carveratest

import (
	"math"
	"net"
	"testing"
	"time"

	"github.com/uwin/cnc-proxy/internal/machine"
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

func TestFakeMachineInstantJogInterpolatesStatusPosition(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	c := dialFakeMachine(t, m)
	defer c.Close()

	writeCtrlMulti(t, c, "$J X30 F0.1\n")

	st := queryFakeStatus(t, c)
	if st.MPos["x"] >= 30 {
		t.Fatalf("instant jog status jumped to target immediately: %+v", st.MPos)
	}
}

func TestFakeMachineInstantJogFinishesAtTarget(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	c := dialFakeMachine(t, m)
	defer c.Close()

	writeCtrlMulti(t, c, "$J X1.25 Y-2 Z0.5 F1\n")
	time.Sleep(100 * time.Millisecond)

	st := queryFakeStatus(t, c)
	assertAxis(t, st.MPos, "x", 1.25)
	assertAxis(t, st.MPos, "y", -2)
	assertAxis(t, st.MPos, "z", 0.5)
	assertAxis(t, st.WPos, "x", 1.25)
	assertAxis(t, st.WPos, "y", -2)
	assertAxis(t, st.WPos, "z", 0.5)
}

func TestFakeMachineG53MoveUpdatesMachineAndWorkPosition(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.SetStatus("<Idle|MPos:1,2,3|WPos:0,1,2|F:0,0,100>")
	c := dialFakeMachine(t, m)
	defer c.Close()

	writeCtrlMulti(t, c, "G53 G0 X5 Z4\n")
	time.Sleep(180 * time.Millisecond)

	st := queryFakeStatus(t, c)
	assertAxis(t, st.MPos, "x", 5)
	assertAxis(t, st.MPos, "y", 2)
	assertAxis(t, st.MPos, "z", 4)
	assertAxis(t, st.WPos, "x", 4)
	assertAxis(t, st.WPos, "y", 1)
	assertAxis(t, st.WPos, "z", 3)
	if st.Feed == nil {
		t.Fatal("status mutation dropped F field")
	}
}

func TestFakeMachineG0IgnoresFeedRate(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	c := dialFakeMachine(t, m)
	defer c.Close()

	writeCtrlMulti(t, c, "G0 X1 F1\n")
	time.Sleep(100 * time.Millisecond)

	st := queryFakeStatus(t, c)
	assertAxis(t, st.MPos, "x", 1)
	assertAxis(t, st.WPos, "x", 1)
}

func TestFakeMachineG1UsesModalFeedRate(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	c := dialFakeMachine(t, m)
	defer c.Close()

	writeCtrlMulti(t, c, "G1 F60\n")
	writeCtrlMulti(t, c, "X10\n")
	time.Sleep(120 * time.Millisecond)

	st := queryFakeStatus(t, c)
	got := st.MPos["x"]
	if got <= 0 || got >= 5 {
		t.Fatalf("modal feed move X = %v, want slow in-progress move below 5mm (all axes %+v)", got, st.MPos)
	}
}

func TestFakeMachineOriginCommandUpdatesWorkPosition(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.SetStatus("<Idle|MPos:5,2,-1|WPos:4,1,-2>")
	c := dialFakeMachine(t, m)
	defer c.Close()

	writeCtrlMulti(t, c, "G10L20P0Z0\n")

	st := queryFakeStatus(t, c)
	assertAxis(t, st.MPos, "x", 5)
	assertAxis(t, st.MPos, "y", 2)
	assertAxis(t, st.MPos, "z", -1)
	assertAxis(t, st.WPos, "x", 4)
	assertAxis(t, st.WPos, "y", 1)
	assertAxis(t, st.WPos, "z", 0)
}

func dialFakeMachine(t *testing.T, m *FakeMachine) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", m.Addr())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func writeCtrlMulti(t *testing.T, c net.Conn, line string) {
	t.Helper()
	if _, err := c.Write(protocol.Encode(protocol.CmdCtrlMulti, []byte(line))); err != nil {
		t.Fatal(err)
	}
}

func queryFakeStatus(t *testing.T, c net.Conn) machine.Status {
	t.Helper()
	if _, err := c.Write(protocol.QueryStatus()); err != nil {
		t.Fatal(err)
	}
	f := readFakeFrame(t, c)
	if f.Cmd != protocol.CmdStatusRes {
		t.Fatalf("query response cmd = %s, want STATUS_RES", protocol.CmdName(f.Cmd))
	}
	st, ok := machine.ParseStatusPayload(string(f.Data))
	if !ok {
		t.Fatalf("malformed status payload %q", string(f.Data))
	}
	return st
}

func readFakeFrame(t *testing.T, c net.Conn) protocol.Frame {
	t.Helper()
	var scan protocol.Scanner
	buf := make([]byte, 1024)
	deadline := time.Now().Add(time.Second)
	for {
		if err := c.SetReadDeadline(deadline); err != nil {
			t.Fatal(err)
		}
		n, err := c.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			continue
		}
		frames := scan.Push(buf[:n])
		if len(frames) > 0 {
			return frames[0]
		}
	}
}

func assertAxis(t *testing.T, vals machine.AxisValues, axis string, want float64) {
	t.Helper()
	got, ok := vals[axis]
	if !ok {
		t.Fatalf("axis %s missing from %+v", axis, vals)
	}
	if math.Abs(got-want) > 0.0001 {
		t.Fatalf("axis %s = %v, want %v (all axes %+v)", axis, got, want, vals)
	}
}
