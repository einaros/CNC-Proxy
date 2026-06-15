package relay

import (
	"bytes"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uwin/cnc-proxy/internal/gcodelog"
	"github.com/uwin/cnc-proxy/internal/machinetransport"
	"github.com/uwin/cnc-proxy/internal/protocol"
)

// frameMachine is a minimal in-test machine that reads whole frames and lets the
// test script its replies. It records the controller frames it received.
type frameMachine struct {
	ln       net.Listener
	mu       sync.Mutex
	received []protocol.Frame
	onFrame  func(c net.Conn, f protocol.Frame)
}

func newFrameMachine(t *testing.T) *frameMachine {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &frameMachine{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go m.serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return m
}

func (m *frameMachine) serve(c net.Conn) {
	defer c.Close()
	var sc protocol.Scanner
	buf := make([]byte, 4096)
	for {
		n, err := c.Read(buf)
		for _, f := range sc.Push(buf[:n]) {
			m.mu.Lock()
			m.received = append(m.received, f)
			fn := m.onFrame
			m.mu.Unlock()
			if fn != nil {
				fn(c, f)
			}
		}
		if err != nil {
			return
		}
	}
}

func (m *frameMachine) recvFrames() []protocol.Frame {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]protocol.Frame(nil), m.received...)
}

func startRelay(t *testing.T, m *frameMachine) (*Server, string) {
	t.Helper()
	srv := &Server{Dial: func() (string, error) { return m.ln.Addr().String(), nil }}
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { proxyLn.Close() })
	go srv.Serve(proxyLn)
	return srv, proxyLn.Addr().String()
}

// TestRelayForwardsFramesVerbatim checks a controller frame reaches the machine
// byte-for-byte and a machine reply reaches the controller byte-for-byte.
func TestRelayForwardsFramesVerbatim(t *testing.T) {
	m := newFrameMachine(t)
	// Echo an "ok" NORMAL_INFO reply for each gcode the machine receives.
	m.onFrame = func(c net.Conn, f protocol.Frame) {
		if f.Cmd == protocol.CmdCtrlMulti {
			c.Write(protocol.Encode(protocol.CmdNormalInfo, []byte("ok\r\n")))
		}
	}
	_, addr := startRelay(t, m)

	client, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	gcode := protocol.Encode(protocol.CmdCtrlMulti, []byte("G0 X10\n"))
	if _, err := client.Write(gcode); err != nil {
		t.Fatal(err)
	}

	// Read the reply frame back.
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	var sc protocol.Scanner
	buf := make([]byte, 256)
	n, _ := client.Read(buf)
	frames := sc.Push(buf[:n])
	if len(frames) == 0 || frames[0].Cmd != protocol.CmdNormalInfo || string(frames[0].Data) != "ok\r\n" {
		t.Fatalf("controller did not get the ok reply: %+v", frames)
	}

	// Machine should have received exactly the gcode frame, unaltered.
	time.Sleep(50 * time.Millisecond)
	rf := m.recvFrames()
	if len(rf) != 1 || !bytes.Equal(rf[0].Raw, gcode) {
		t.Fatalf("machine received %d frames; first matches gcode=%v", len(rf), len(rf) == 1 && bytes.Equal(rf[0].Raw, gcode))
	}
}

func TestRelayUsesGenericMachineTransport(t *testing.T) {
	m := newFrameMachine(t)
	m.onFrame = func(c net.Conn, f protocol.Frame) {
		if f.Cmd == protocol.CmdCtrlSingle && len(f.Data) == 1 && f.Data[0] == '?' {
			c.Write(protocol.Encode(protocol.CmdStatusRes, []byte("<Idle|MPos:0,0,0>")))
		}
	}
	var dials atomic.Int32
	srv := &Server{
		MachineDial: func() (*machinetransport.Opened, error) {
			dials.Add(1)
			c, err := net.Dial("tcp", m.ln.Addr().String())
			if err != nil {
				return nil, err
			}
			return &machinetransport.Opened{
				Conn:       c,
				Label:      "fake-usb",
				Kind:       machinetransport.KindUSB,
				PacketSize: machinetransport.USBPacketSize,
			}, nil
		},
	}
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { proxyLn.Close() })
	go srv.Serve(proxyLn)

	controller, err := net.Dial("tcp", proxyLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	if _, err := controller.Write(protocol.QueryStatus()); err != nil {
		t.Fatal(err)
	}
	readControllerFrame(t, controller, protocol.CmdStatusRes)
	if got := dials.Load(); got != 1 {
		t.Fatalf("MachineDial calls = %d, want 1", got)
	}
}

// TestRelaySniffsGcodeIntoLog verifies that a controller's command lines and
// the machine's textual replies land in the gcode log, while `?` status polls
// and their STATUS_RES replies stay out of it.
func TestRelaySniffsGcodeIntoLog(t *testing.T) {
	m := newFrameMachine(t)
	m.onFrame = func(c net.Conn, f protocol.Frame) {
		switch f.Cmd {
		case protocol.CmdCtrlMulti:
			c.Write(protocol.Encode(protocol.CmdNormalInfo, []byte("ok\r\n")))
		case protocol.CmdCtrlSingle:
			if len(f.Data) == 1 && f.Data[0] == '?' {
				c.Write(protocol.Encode(protocol.CmdStatusRes, []byte("<Idle|MPos:0,0,0>")))
			}
		}
	}
	glog := gcodelog.New(50)
	srv, addr := startRelay(t, m)
	srv.GcodeLog = glog

	controller, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	drainController(controller)

	// A status poll (must NOT be logged), then a gcode line (must be logged,
	// with its escaped space decoded), then the machine's "ok" reply.
	controller.Write(protocol.QueryStatus())
	controller.Write(protocol.Encode(protocol.CmdCtrlMulti, []byte("G0\x01X10\n")))

	deadline := time.Now().Add(2 * time.Second)
	var lines []gcodelog.Line
	for time.Now().Before(deadline) {
		lines = glog.Recent()
		if len(lines) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(lines) != 2 {
		t.Fatalf("log = %+v, want exactly [send G0 X10, recv ok]", lines)
	}
	if lines[0].Dir != gcodelog.DirSend || lines[0].Source != gcodelog.SourceController || lines[0].Text != "G0 X10" {
		t.Errorf("send line = %+v", lines[0])
	}
	if lines[1].Dir != gcodelog.DirRecv || lines[1].Text != "ok" {
		t.Errorf("recv line = %+v", lines[1])
	}
}

// TestInjectionResponsesNotLoggedAsController verifies frames diverted to an
// injected operation do not appear in the log as controller traffic (the
// injecting side logs its own conversation under the "api" source).
func TestInjectionResponsesNotLoggedAsController(t *testing.T) {
	m := newFrameMachine(t)
	m.onFrame = func(c net.Conn, f protocol.Frame) {
		if f.Cmd == protocol.CmdCtrlMulti {
			c.Write(protocol.Encode(protocol.CmdNormalInfo, []byte("injected-reply\r\n")))
		}
	}
	glog := gcodelog.New(50)
	srv, addr := startRelay(t, m)
	srv.GcodeLog = glog

	controller, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	drainController(controller)
	time.Sleep(100 * time.Millisecond) // let the session establish

	it, release, err := srv.AcquireMachine()
	if err != nil {
		t.Fatalf("AcquireMachine: %v", err)
	}
	it.Write(protocol.Encode(protocol.CmdCtrlMulti, []byte("version\n")))
	it.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 4096)
	it.Read(buf) // consume the diverted reply
	release()

	for _, ln := range glog.Recent() {
		if ln.Source == gcodelog.SourceController && ln.Text == "injected-reply" {
			t.Errorf("diverted injection reply was logged as controller traffic: %+v", ln)
		}
	}
}

// TestRelaySingleSession verifies a second controller is refused while one is
// active.
func TestRelaySingleSession(t *testing.T) {
	m := newFrameMachine(t)
	_, addr := startRelay(t, m)

	first, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	time.Sleep(100 * time.Millisecond)

	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	second.SetReadDeadline(time.Now().Add(time.Second))
	b := make([]byte, 1)
	if _, err := second.Read(b); err == nil {
		t.Error("second session was not refused")
	}
}

// TestInjectionNoSession confirms AcquireMachine fails when no controller is
// connected (callers fall back to owner mode).
func TestInjectionNoSession(t *testing.T) {
	srv := &Server{Dial: func() (string, error) { return "", nil }}
	if _, _, err := srv.AcquireMachine(); err != ErrNoSession {
		t.Errorf("AcquireMachine with no session = %v, want ErrNoSession", err)
	}
}

// drainController reads and discards from a connection until closed, so the
// relay's controller->machine pump isn't blocked on a full socket.
func drainController(c net.Conn) {
	go io.Copy(io.Discard, c)
}
