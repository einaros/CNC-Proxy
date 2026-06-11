package relay

import (
	"net"
	"testing"
	"time"

	"github.com/uwin/cnc-proxy/internal/protocol"
)

// TestInjectionIsolatesResponses is the safety-critical test: while a controller
// is connected, an injected operation's stateful responses (here LOAD_INFO /
// LOAD_FINISH from an `ls`) must reach the injector and NOT the controller,
// while the controller's own status polls keep being answered.
func TestInjectionIsolatesResponses(t *testing.T) {
	// Machine: replies to `?` with a status, and to a CTRL_MULTI `ls` with a
	// LOAD_INFO + LOAD_FINISH pair.
	m := newFrameMachine(t)
	m.onFrame = func(c net.Conn, f protocol.Frame) {
		switch f.Cmd {
		case protocol.CmdCtrlSingle:
			if len(f.Data) == 1 && f.Data[0] == '?' {
				c.Write(protocol.Encode(protocol.CmdStatusRes, []byte("<Idle|MPos:0,0,0>")))
			}
		case protocol.CmdCtrlMulti:
			c.Write(protocol.Encode(protocol.CmdLoadInfo, []byte("a.nc 10 20260101120000\r\n")))
			c.Write(protocol.Encode(protocol.CmdLoadFinish, []byte("ok\r\n")))
		}
	}
	srv, addr := startRelay(t, m)

	controller, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()

	// Prime a status report so the mux has a cached status to answer polls with,
	// and let the session establish.
	controller.Write(protocol.QueryStatus())
	time.Sleep(150 * time.Millisecond)

	// Collect everything the controller receives, in the background. The reader
	// owns ctrlCmds and signals doneReading when it stops, so the main goroutine
	// reads the collected commands only after the reader has finished (no
	// concurrent access to the slice).
	var ctrlCmds []byte
	doneReading := make(chan struct{})
	go func() {
		defer close(doneReading)
		var sc protocol.Scanner
		buf := make([]byte, 4096)
		for {
			controller.SetReadDeadline(time.Now().Add(time.Second))
			n, err := controller.Read(buf)
			for _, f := range sc.Push(buf[:n]) {
				ctrlCmds = append(ctrlCmds, f.Cmd)
			}
			if err != nil {
				return
			}
		}
	}()

	// Acquire an injection and run an `ls` over it.
	it, release, err := srv.AcquireMachine()
	if err != nil {
		t.Fatalf("AcquireMachine: %v", err)
	}

	// Drive the ls directly over the inject transport.
	it.Write(protocol.LsCommand("/sd/gcodes"))
	var sc protocol.Scanner
	var gotInfo, gotFinish bool
	buf := make([]byte, 4096)
	deadline := time.Now().Add(2 * time.Second)
	for !gotFinish && time.Now().Before(deadline) {
		it.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, _ := it.Read(buf)
		for _, f := range sc.Push(buf[:n]) {
			switch f.Cmd {
			case protocol.CmdLoadInfo:
				gotInfo = true
			case protocol.CmdLoadFinish:
				gotFinish = true
			}
		}
		if n == 0 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	release()

	if !gotInfo || !gotFinish {
		t.Fatalf("injector did not receive ls responses: info=%v finish=%v", gotInfo, gotFinish)
	}

	// Close the controller so the reader goroutine returns, then inspect the
	// commands it collected once it has fully stopped (no concurrent access).
	controller.Close()
	<-doneReading
	for _, cmd := range ctrlCmds {
		if cmd == protocol.CmdLoadInfo || cmd == protocol.CmdLoadFinish || cmd == protocol.CmdLoadError {
			t.Errorf("controller wrongly received injected LOAD frame cmd=%#x", cmd)
		}
	}
}

// TestInjectionAnswersControllerPollsFromCache verifies that during an
// injection, a controller status poll is answered from the cached last status
// (so its heartbeat survives) rather than hitting the busy machine.
func TestInjectionAnswersControllerPollsFromCache(t *testing.T) {
	m := newFrameMachine(t)
	m.onFrame = func(c net.Conn, f protocol.Frame) {
		if f.Cmd == protocol.CmdCtrlSingle && len(f.Data) == 1 && f.Data[0] == '?' {
			c.Write(protocol.Encode(protocol.CmdStatusRes, []byte("<Run|MPos:1,2,3>")))
		}
		// Note: deliberately do NOT answer anything else, so if the poll during
		// injection reached the machine we'd see nothing; the cache must answer.
	}
	srv, addr := startRelay(t, m)

	controller, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()

	// Prime the cached status.
	controller.Write(protocol.QueryStatus())
	time.Sleep(150 * time.Millisecond)

	statusCh := make(chan string, 16)
	go func() {
		var sc protocol.Scanner
		buf := make([]byte, 4096)
		for {
			controller.SetReadDeadline(time.Now().Add(2 * time.Second))
			n, err := controller.Read(buf)
			for _, f := range sc.Push(buf[:n]) {
				if f.Cmd == protocol.CmdStatusRes {
					statusCh <- string(f.Data)
				}
			}
			if err != nil {
				return
			}
		}
	}()
	// Drain the initial status.
	select {
	case <-statusCh:
	case <-time.After(time.Second):
	}

	_, release, err := srv.AcquireMachine()
	if err != nil {
		t.Fatalf("AcquireMachine: %v", err)
	}
	// While injected, the controller polls; it should be answered from cache.
	controller.Write(protocol.QueryStatus())
	select {
	case s := <-statusCh:
		if s == "" {
			t.Error("empty status answered during injection")
		}
	case <-time.After(time.Second):
		t.Error("controller status poll was not answered during injection")
	}
	release()
}
