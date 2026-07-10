package relay

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/uwin/cnc-proxy/internal/client"
	"github.com/uwin/cnc-proxy/internal/machinetransport"
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

// TestInjectionMotionDoesNotLeakToController is the safety test for injecting a
// motion command while a controller is connected. Motion is fire-and-forget:
// the firmware sends no reply, so the injected command produces no NORMAL_INFO
// frame — and even if it did (e.g. an error line), it must be diverted to the
// injector and never leak to the controller, whose ok-accounting would corrupt.
// The injected send must also return promptly (not block), so the injection
// window releases and the controller's traffic resumes.
func TestInjectionMotionDoesNotLeakToController(t *testing.T) {
	m := newFrameMachine(t)
	m.onFrame = func(c net.Conn, f protocol.Frame) {
		if f.Cmd == protocol.CmdCtrlSingle && len(f.Data) == 1 && f.Data[0] == '?' {
			c.Write(protocol.Encode(protocol.CmdStatusRes, []byte("<Run|MPos:0,0,0>")))
		}
		// CTRL_MULTI motion: the firmware is silent, like real hardware.
	}
	srv, addr := startRelay(t, m)

	controller, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	drainController(controller)
	controller.Write(protocol.QueryStatus())
	time.Sleep(150 * time.Millisecond)

	it, release, err := srv.AcquireMachine()
	if err != nil {
		t.Fatalf("AcquireMachine: %v", err)
	}
	defer release()

	// Watch the controller for any leaked injected output.
	go func() {
		var sc protocol.Scanner
		buf := make([]byte, 4096)
		for {
			controller.SetReadDeadline(time.Now().Add(2 * time.Second))
			n, err := controller.Read(buf)
			for _, f := range sc.Push(buf[:n]) {
				if f.Cmd == protocol.CmdNormalInfo {
					t.Errorf("controller wrongly received injected NORMAL_INFO: %q", f.Data)
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Drive a motion command over the injector via the real client. It must
	// return promptly (fire-and-forget), not block waiting for an ack.
	conn := client.NewTransport(it)
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn.SendGcodeLine("G91 G0 X-10", client.GcodeOpts{
			ExpectReply: false,
			Settle:      200 * time.Millisecond,
			Cap:         2 * time.Second,
		})
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("injected motion command did not return promptly")
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

// TestInjectionStatusPollDeliveredToInjector verifies that a proxy status poll
// sent inside a normal injection window receives its STATUS_RES. Controller
// polls are still answered from cache by TestInjectionAnswersControllerPollsFromCache.
func TestInjectionStatusPollDeliveredToInjector(t *testing.T) {
	m := newFrameMachine(t)
	m.onFrame = func(c net.Conn, f protocol.Frame) {
		if f.Cmd == protocol.CmdCtrlSingle && len(f.Data) == 1 && f.Data[0] == '?' {
			c.Write(protocol.Encode(protocol.CmdStatusRes, []byte("<Idle|MPos:4,5,6>")))
		}
	}
	srv, addr := startRelay(t, m)

	controller, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()

	controller.Write(protocol.QueryStatus())
	readControllerFrame(t, controller, protocol.CmdStatusRes)

	it, release, err := srv.AcquireMachine()
	if err != nil {
		t.Fatalf("AcquireMachine: %v", err)
	}
	defer release()

	if _, err := it.Write(protocol.QueryStatus()); err != nil {
		t.Fatal(err)
	}
	f := readTransportFrame(t, it, protocol.CmdStatusRes)
	if string(f.Data) != "<Idle|MPos:4,5,6>" {
		t.Fatalf("injector status = %q", f.Data)
	}

	controller.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	buf := make([]byte, 256)
	if n, err := controller.Read(buf); err == nil && n > 0 {
		t.Fatalf("controller received proxy-injected status frame: %x", buf[:n])
	}
}

func TestRelayControlAllowedDuringControllerFileTransfer(t *testing.T) {
	m := newFrameMachine(t)
	srv, addr := startRelay(t, m)

	controller, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()

	if _, err := controller.Write(protocol.UploadCommand("/sd/gcodes/job.nc")); err != nil {
		t.Fatal(err)
	}
	waitForMachineFrame(t, m, protocol.CmdFileStart)

	controls := []byte{protocol.CtrlFeedHold, protocol.CtrlResume, protocol.CtrlHalt}
	for _, c := range controls {
		if err := srv.SendControl(c); err != nil {
			t.Fatalf("SendControl(%#x) during file transfer: %v", c, err)
		}
	}

	dataPayload := append([]byte{0, 0, 0, 1}, []byte("chunk")...)
	if _, err := controller.Write(protocol.Encode(protocol.CmdFileData, dataPayload)); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Write(protocol.Encode(protocol.CmdFileEnd, nil)); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		frames := m.recvFrames()
		if hasControlFrames(frames, controls) &&
			hasFrame(frames, protocol.CmdFileData) &&
			hasFrame(frames, protocol.CmdFileEnd) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("machine frames did not include controls and file frames: %s", frameSummary(m.recvFrames()))
}

func TestInteractiveLeaseDivertsStatusFromController(t *testing.T) {
	m := newFrameMachine(t)
	m.onFrame = func(c net.Conn, f protocol.Frame) {
		if f.Cmd == protocol.CmdCtrlSingle && len(f.Data) == 1 && f.Data[0] == '?' {
			c.Write(protocol.Encode(protocol.CmdStatusRes, []byte("<Idle|MPos:1,2,3>")))
		}
	}
	srv, addr := startRelay(t, m)

	controller, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()

	controller.Write(protocol.QueryStatus())
	readControllerFrame(t, controller, protocol.CmdStatusRes)

	it, _, release, err := srv.AcquireInteractive()
	if err != nil {
		t.Fatalf("AcquireInteractive: %v", err)
	}
	defer release()

	if _, err := it.Write(protocol.QueryStatus()); err != nil {
		t.Fatal(err)
	}
	readTransportFrame(t, it, protocol.CmdStatusRes)

	controller.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	buf := make([]byte, 256)
	if n, err := controller.Read(buf); err == nil && n > 0 {
		t.Fatalf("controller received proxy interactive status frame: %x", buf[:n])
	}
}

func TestInteractiveLeaseAbortsAndFlushesControllerTraffic(t *testing.T) {
	m := newFrameMachine(t)
	srv, addr := startRelay(t, m)

	controller, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	time.Sleep(100 * time.Millisecond)

	it, abort, release, err := srv.AcquireInteractive()
	if err != nil {
		t.Fatalf("AcquireInteractive: %v", err)
	}
	readDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 128)
		_, err := it.Read(buf)
		readDone <- err
	}()

	gcode := protocol.Encode(protocol.CmdCtrlMulti, []byte("M114\n"))
	if _, err := controller.Write(gcode); err != nil {
		t.Fatal(err)
	}
	select {
	case <-abort:
	case <-time.After(time.Second):
		t.Fatal("interactive lease was not aborted by controller traffic")
	}
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("interactive transport read unexpectedly succeeded after abort")
		}
	case <-time.After(150 * time.Millisecond):
		t.Fatal("interactive transport read did not unblock promptly after abort")
	}

	release()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, f := range m.recvFrames() {
			if f.Cmd == protocol.CmdCtrlMulti && string(f.Data) == "M114\n" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("controller frame was not flushed after release; frames: %s", frameSummary(m.recvFrames()))
}

// TestReleaseFlushesHeldFramesBeforeResumingController verifies the F8
// ordering invariant: when an injection releases, controller frames that were
// held during the injection must reach the machine BEFORE any controller frame
// forwarded after the release. If release clears `injecting` before flushing,
// a freshly forwarded FILE_DATA can overtake the held FILE_START and corrupt
// the controller's upload handshake. The mux test hook makes the race
// deterministic: at the moment release begins flushing, the controller sends
// its next frame and the hook waits until the pump has processed it.
func TestReleaseFlushesHeldFramesBeforeResumingController(t *testing.T) {
	m := newFrameMachine(t)
	srv, addr := startRelay(t, m)

	controller, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	drainController(controller)
	time.Sleep(100 * time.Millisecond)

	_, release, err := srv.AcquireMachine()
	if err != nil {
		t.Fatalf("AcquireMachine: %v", err)
	}

	// FILE_START sent while the injection holds the mux: it must be held.
	if _, err := controller.Write(protocol.UploadCommand("/sd/gcodes/job.nc")); err != nil {
		t.Fatal(err)
	}
	mx := currentMux(t, srv)
	waitForHeldFrame(t, mx, protocol.CmdFileStart)

	// At the exact moment release starts flushing held frames, the controller
	// sends the transfer's next frame. The hook waits until the pump has
	// processed it — pre-fix it is forwarded straight to the machine (racing
	// ahead of the held FILE_START); post-fix it is held and flushed in order.
	dataPayload := append([]byte{0, 0, 0, 1}, []byte("chunk")...)
	dataFrame := protocol.Encode(protocol.CmdFileData, dataPayload)
	mx.testHookBeforeHeldFlush = func() {
		if _, err := controller.Write(dataFrame); err != nil {
			return
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if heldContains(mx, protocol.CmdFileData) || hasFrame(m.recvFrames(), protocol.CmdFileData) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	release()

	waitForMachineFrame(t, m, protocol.CmdFileStart)
	waitForMachineFrame(t, m, protocol.CmdFileData)
	frames := m.recvFrames()
	start, data := -1, -1
	for i, f := range frames {
		switch f.Cmd {
		case protocol.CmdFileStart:
			if start == -1 {
				start = i
			}
		case protocol.CmdFileData:
			if data == -1 {
				data = i
			}
		}
	}
	if start == -1 || data == -1 {
		t.Fatalf("machine missing transfer frames: %s", frameSummary(frames))
	}
	if start > data {
		t.Fatalf("held FILE_START reached the machine AFTER the later FILE_DATA; frames: %s", frameSummary(frames))
	}
}

// TestManagedInjectionDoesNotDropListingFrames verifies the F9 contract: a
// managed injection (relay-mode reconcile `ls`) whose reply frames overflow
// the inject channel while the consumer is briefly not draining must never
// surface as a LOAD_FINISH "success" with a silently truncated listing — it
// must either deliver the complete listing or fail the operation.
func TestManagedInjectionDoesNotDropListingFrames(t *testing.T) {
	const total = 400
	m := newFrameMachine(t)
	m.onFrame = func(c net.Conn, f protocol.Frame) {
		if f.Cmd != protocol.CmdCtrlMulti {
			return
		}
		// Burst far more LOAD_INFO frames than the inject channel buffers,
		// then send LOAD_FINISH only after the consumer has had time to drain
		// the buffered prefix — the exact pattern in which dropped middle
		// frames still end in a "successful" LOAD_FINISH.
		for i := 0; i < total; i++ {
			row := fmt.Sprintf("file%04d.nc 10 20260101120000\r\n", i)
			c.Write(protocol.Encode(protocol.CmdLoadInfo, []byte(row)))
		}
		time.Sleep(600 * time.Millisecond)
		c.Write(protocol.Encode(protocol.CmdLoadFinish, []byte("ok\r\n")))
	}
	srv, addr := startRelay(t, m)

	controller, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	drainController(controller)
	time.Sleep(100 * time.Millisecond)

	it, release, err := srv.AcquireMachine()
	if err != nil {
		t.Fatalf("AcquireMachine: %v", err)
	}
	defer release()

	// Slow consumer: delay the first read so the whole burst lands while
	// nobody is draining the inject channel (a briefly descheduled reconcile).
	conn := client.NewTransport(&slowFirstReadTransport{Conn: it, delay: 300 * time.Millisecond})
	entries, err := conn.List("/sd/gcodes", 5*time.Second)
	if err != nil {
		t.Logf("managed ls failed instead of silently truncating (acceptable): %v", err)
		return
	}
	if len(entries) != total {
		t.Fatalf("managed ls reported SUCCESS with a truncated listing: %d of %d entries", len(entries), total)
	}
}

// slowFirstReadTransport delays the first Read to simulate a consumer that is
// descheduled while the machine's reply burst arrives.
type slowFirstReadTransport struct {
	machinetransport.Conn
	once  sync.Once
	delay time.Duration
}

func (s *slowFirstReadTransport) Read(p []byte) (int, error) {
	s.once.Do(func() { time.Sleep(s.delay) })
	return s.Conn.Read(p)
}

// currentMux returns the active session's mux, waiting for the relay session
// to establish.
func currentMux(t *testing.T, srv *Server) *mux {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		srv.mu.Lock()
		mx := srv.curMux
		srv.mu.Unlock()
		if mx != nil {
			return mx
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no active relay session")
	return nil
}

// waitForHeldFrame waits until the injection window is holding a controller
// frame with the given command.
func waitForHeldFrame(t *testing.T, mx *mux, cmd byte) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if heldContains(mx, cmd) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("frame %s was not held by the injection window", protocol.CmdName(cmd))
}

func heldContains(mx *mux, cmd byte) bool {
	mx.mu.Lock()
	held := make([][]byte, len(mx.heldController))
	copy(held, mx.heldController)
	mx.mu.Unlock()
	for _, raw := range held {
		var sc protocol.Scanner
		for _, f := range sc.Push(raw) {
			if f.Cmd == cmd {
				return true
			}
		}
	}
	return false
}

func waitForMachineFrame(t *testing.T, m *frameMachine, want byte) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hasFrame(m.recvFrames(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("machine did not receive %s; frames: %s", protocol.CmdName(want), frameSummary(m.recvFrames()))
}

func readControllerFrame(t *testing.T, c net.Conn, want byte) protocol.Frame {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	var sc protocol.Scanner
	buf := make([]byte, 1024)
	for {
		n, err := c.Read(buf)
		for _, f := range sc.Push(buf[:n]) {
			if f.Cmd == want {
				return f
			}
		}
		if err != nil {
			t.Fatalf("read controller frame %s: %v", protocol.CmdName(want), err)
		}
	}
}

func readTransportFrame(t *testing.T, tr InjectTransport, want byte) protocol.Frame {
	t.Helper()
	tr.SetReadDeadline(time.Now().Add(2 * time.Second))
	var sc protocol.Scanner
	buf := make([]byte, 1024)
	for {
		n, err := tr.Read(buf)
		for _, f := range sc.Push(buf[:n]) {
			if f.Cmd == want {
				return f
			}
		}
		if err != nil {
			t.Fatalf("read transport frame %s: %v", protocol.CmdName(want), err)
		}
	}
}

func hasFrame(frames []protocol.Frame, want byte) bool {
	for _, f := range frames {
		if f.Cmd == want {
			return true
		}
	}
	return false
}

func hasControlFrames(frames []protocol.Frame, controls []byte) bool {
	counts := make(map[byte]int, len(controls))
	for _, c := range controls {
		counts[c]++
	}
	for _, f := range frames {
		if f.Cmd == protocol.CmdCtrlSingle && len(f.Data) == 1 {
			if counts[f.Data[0]] > 0 {
				counts[f.Data[0]]--
			}
		}
	}
	for _, n := range counts {
		if n != 0 {
			return false
		}
	}
	return true
}

func frameSummary(frames []protocol.Frame) string {
	if len(frames) == 0 {
		return "[]"
	}
	out := "["
	for i, f := range frames {
		if i > 0 {
			out += " "
		}
		out += protocol.CmdName(f.Cmd)
		if f.Cmd == protocol.CmdCtrlSingle && len(f.Data) == 1 {
			out += "(" + protocol.Unescape(string(f.Data)) + ")"
		}
	}
	return out + "]"
}
