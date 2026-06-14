package relay

import (
	"errors"
	"net"
	"sync"
	"time"

	"github.com/uwin/cnc-proxy/internal/protocol"
)

// ErrBusy is returned by AcquireInjection when the controller is mid-transaction
// (e.g. a file transfer) and injection cannot safely begin.
var ErrBusy = errors.New("relay: machine busy with controller transaction")

// ErrNoSession is returned when no controller session (and thus no machine
// connection) is currently active.
var ErrNoSession = errors.New("relay: no active controller session")

// mux owns the single machine socket during a controller session and
// multiplexes two producers onto it: the controller (default) and an injected
// proxy operation (upload, gcode) that briefly takes over.
//
// Only one injection runs at a time, and only while the controller is between
// transactions. While an injection holds the mux:
//   - controller->machine frames are buffered and replayed when it releases;
//   - machine->controller frames are diverted to the injector (so the stateful
//     LOAD_*/FILE_*/DIAG responses of the injected op never reach the
//     controller and corrupt its state);
//   - the controller's status `?` polls are answered from the cached last
//     status report, keeping its heartbeat alive with no time pressure.
type mux struct {
	machine net.Conn

	mu sync.Mutex
	// injecting is true while an injected operation owns the reply stream.
	injecting bool
	// interactive is true for a long-lived jog/control lease. A controller
	// non-status frame aborts the lease so the controller can proceed.
	interactive bool
	// injectCh delivers machine frames to the active injector.
	injectCh chan protocol.Frame
	// abortCh is closed when an interactive lease must release promptly.
	abortCh chan struct{}
	// heldController accumulates controller->machine frames during injection,
	// to be flushed verbatim when injection ends.
	heldController [][]byte
	// lastStatus is the most recent STATUS_RES payload seen from the machine,
	// used to answer controller polls during injection.
	lastStatus []byte
	// controllerMidXfer is true while the controller is inside a file transfer
	// (FILE_START..FILE_END/CAN), during which injection must not begin.
	controllerMidXfer bool
}

func newMux(machine net.Conn) *mux {
	return &mux{machine: machine}
}

// noteControllerFrame updates controller-transaction tracking from an
// observed controller->machine frame, returning whether the frame should be
// forwarded now (true) or held because an injection is active (false).
func (m *mux) noteControllerFrame(f protocol.Frame, raw []byte) (forward bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Track whether the controller is mid file-transfer, so AcquireInjection
	// can refuse to interrupt it. The controller starts a transfer with
	// FILE_START and ends it at FILE_END/CAN.
	switch f.Cmd {
	case protocol.CmdFileStart:
		m.controllerMidXfer = true
	case protocol.CmdFileEnd, protocol.CmdFileCancel:
		m.controllerMidXfer = false
	}

	if !m.injecting {
		return true
	}
	// During injection, hold controller frames for later replay — EXCEPT a
	// bare status poll, which we answer ourselves (handled by the caller).
	if isStatusPoll(f) {
		return false
	}
	if m.interactive {
		m.abortInteractiveLocked()
	}
	cp := append([]byte(nil), raw...)
	m.heldController = append(m.heldController, cp)
	return false
}

// isStatusPoll reports whether a controller frame is a single-char '?' query.
func isStatusPoll(f protocol.Frame) bool {
	return f.Cmd == protocol.CmdCtrlSingle && len(f.Data) == 1 && f.Data[0] == '?'
}

// cachedStatusFrame returns a STATUS_RES frame built from the last seen status,
// or nil if none has been observed yet.
func (m *mux) cachedStatusFrame() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.lastStatus) == 0 {
		return nil
	}
	return protocol.Encode(protocol.CmdStatusRes, m.lastStatus)
}

// writeControl writes a single realtime control character to the machine socket
// out-of-band, without taking the injection window. net.Conn writes are
// concurrency-safe, and a CTRL_SINGLE frame is one atomic Write, so this is safe
// to call concurrently with an active injection, controller file transfer, or
// the relay's own pumps.
func (m *mux) writeControl(c byte) error {
	_, err := m.machine.Write(protocol.Encode(protocol.CmdCtrlSingle, []byte{c}))
	return err
}

// AcquireInjection begins an injection window, returning a transport the client
// package can drive. It fails with ErrBusy if the controller is mid file
// transfer. The caller MUST call the returned release func when done.
func (m *mux) AcquireInjection() (*injectTransport, func(), error) {
	m.mu.Lock()
	if m.injecting {
		m.mu.Unlock()
		return nil, nil, ErrBusy
	}
	if m.controllerMidXfer {
		m.mu.Unlock()
		return nil, nil, ErrBusy
	}
	m.injecting = true
	m.interactive = false
	m.injectCh = make(chan protocol.Frame, 64)
	m.abortCh = nil
	m.mu.Unlock()

	it := &injectTransport{m: m}
	release := func() { m.releaseInjection() }
	return it, release, nil
}

// AcquireInteractive begins a long-lived interactive lease for jog/control
// traffic. It uses the same reply diversion as injection, but STATUS_RES frames
// produced by the interactive side are delivered to the lease rather than
// forwarded to the controller. A controller non-status frame aborts the lease.
func (m *mux) AcquireInteractive() (*injectTransport, <-chan struct{}, func(), error) {
	m.mu.Lock()
	if m.injecting {
		m.mu.Unlock()
		return nil, nil, nil, ErrBusy
	}
	if m.controllerMidXfer {
		m.mu.Unlock()
		return nil, nil, nil, ErrBusy
	}
	m.injecting = true
	m.interactive = true
	m.injectCh = make(chan protocol.Frame, 128)
	m.abortCh = make(chan struct{})
	abortCh := m.abortCh
	m.mu.Unlock()

	it := &injectTransport{m: m}
	release := func() { m.releaseInjection() }
	return it, abortCh, release, nil
}

// releaseInjection ends the injection window and flushes held controller frames
// to the machine so the controller's pending traffic proceeds.
func (m *mux) releaseInjection() {
	m.mu.Lock()
	held := m.heldController
	m.heldController = nil
	m.injecting = false
	m.interactive = false
	ch := m.injectCh
	m.injectCh = nil
	abortCh := m.abortCh
	m.abortCh = nil
	m.mu.Unlock()

	if abortCh != nil {
		m.closeAbort(abortCh)
	}
	if ch != nil {
		close(ch)
	}
	for _, raw := range held {
		_, _ = m.machine.Write(raw)
	}
}

// routeMachineFrame is called by the reader for each machine->controller frame.
// During injection it diverts frames to the injector and returns false (do not
// forward to controller); otherwise returns true.
func (m *mux) routeMachineFrame(f protocol.Frame) (forward bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if f.Cmd == protocol.CmdStatusRes {
		m.lastStatus = append(m.lastStatus[:0], f.Data...)
	}
	if !m.injecting {
		return true
	}
	if m.interactive && f.Cmd == protocol.CmdStatusRes {
		if m.injectCh != nil {
			select {
			case m.injectCh <- f:
			default:
			}
		}
		return false
	}
	// Status reports during injection still go to the controller too, so its
	// heartbeat stays fed; everything else is the injected op's response.
	if f.Cmd == protocol.CmdStatusRes {
		return true
	}
	if m.injectCh != nil {
		select {
		case m.injectCh <- f:
		default:
		}
	}
	return false
}

func (m *mux) abortInteractiveLocked() {
	if m.abortCh == nil {
		return
	}
	m.closeAbort(m.abortCh)
	m.abortCh = nil
	if m.injectCh != nil {
		close(m.injectCh)
		m.injectCh = nil
	}
}

func (m *mux) closeAbort(ch chan struct{}) {
	defer func() { _ = recover() }()
	close(ch)
}

// injectTransport adapts the mux into a client.transport for the duration of an
// injection: writes go straight to the machine socket, reads come from the
// diverted machine frames.
type injectTransport struct {
	m        *mux
	scanBuf  []byte // leftover bytes from a frame not fully consumed by Read
	deadline time.Time
}

func (t *injectTransport) Write(p []byte) (int, error) {
	t.m.mu.Lock()
	closed := !t.m.injecting || (t.m.interactive && t.m.injectCh == nil)
	t.m.mu.Unlock()
	if closed {
		return 0, errInjectionClosed
	}
	return t.m.machine.Write(p)
}

func (t *injectTransport) Read(p []byte) (int, error) {
	if len(t.scanBuf) > 0 {
		n := copy(p, t.scanBuf)
		t.scanBuf = t.scanBuf[n:]
		return n, nil
	}
	t.m.mu.Lock()
	ch := t.m.injectCh
	t.m.mu.Unlock()
	if ch == nil {
		return 0, errInjectionClosed
	}

	// Honor the read deadline so client commands that expect a quiescence
	// timeout (e.g. console commands with no "ok") don't block until release.
	var timer *time.Timer
	var timeoutCh <-chan time.Time
	if !t.deadline.IsZero() {
		d := time.Until(t.deadline)
		if d <= 0 {
			return 0, timeoutError{}
		}
		timer = time.NewTimer(d)
		timeoutCh = timer.C
	}
	if timer != nil {
		defer timer.Stop()
	}

	select {
	case f, ok := <-ch:
		if !ok {
			return 0, errInjectionClosed
		}
		raw := f.Raw // exact wire bytes, preserving the original CRC
		n := copy(p, raw)
		if n < len(raw) {
			t.scanBuf = append(t.scanBuf, raw[n:]...)
		}
		return n, nil
	case <-timeoutCh:
		return 0, timeoutError{}
	}
}

func (t *injectTransport) SetReadDeadline(d time.Time) error { t.deadline = d; return nil }
func (t *injectTransport) Close() error                      { return nil }

var errInjectionClosed = errors.New("relay: injection window closed")

// timeoutError is a net.Error timeout, so a read deadline on the injection
// transport surfaces the same way a socket read timeout would.
type timeoutError struct{}

func (timeoutError) Error() string   { return "relay: injection read timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
