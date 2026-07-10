package relay

import (
	"errors"
	"sync"
	"time"

	"github.com/uwin/cnc-proxy/internal/machinetransport"
	"github.com/uwin/cnc-proxy/internal/protocol"
)

// ErrBusy is returned by AcquireInjection when the controller is mid-transaction
// (e.g. a file transfer) and injection cannot safely begin.
var ErrBusy = errors.New("relay: machine busy with controller transaction")

// ErrNoSession is returned when no controller session (and thus no machine
// connection) is currently active.
var ErrNoSession = errors.New("relay: no active controller session")

// mux owns the single machine-side transport during a controller session and
// multiplexes two producers onto it: the controller (default) and an injected
// proxy operation (upload, gcode) that briefly takes over.
//
// Only one injection runs at a time, and only while the controller is between
// transactions. While an injection holds the mux:
//   - controller->machine frames are buffered and replayed when it releases;
//   - machine->controller frames produced by the injected op are diverted to the
//     injector (so stateful LOAD_*/FILE_*/DIAG responses, plus STATUS_RES
//     replies to injected `?` polls, never reach the controller and corrupt its
//     state);
//   - the controller's status `?` polls are answered from the cached last
//     status report, keeping its heartbeat alive with no time pressure.
type mux struct {
	machine machinetransport.Conn

	mu sync.Mutex
	// injecting is true while an injected operation owns the reply stream.
	injecting bool
	// interactive is true for a long-lived jog/control lease. A controller
	// non-status frame aborts the lease so the controller can proceed.
	interactive bool
	// injectCh delivers machine frames to the active injector.
	injectCh chan protocol.Frame
	// injectOverflow is set when a non-interactive injection's reply frame
	// could not be buffered. The transport is then errored so the managed op
	// fails loudly instead of reporting success over a silently truncated
	// reply stream (e.g. a partial `ls` that reconcile would act on).
	injectOverflow bool
	// injectStatusPolls counts STATUS_RES replies owed to non-interactive
	// injected `?` polls. Controller polls are answered from cache and never
	// increment this.
	injectStatusPolls int
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

	// testHookBeforeHeldFlush, when set (tests only), runs once per release just
	// before the first batch of held controller frames is written to the
	// machine. It lets ordering tests deterministically interleave a fresh
	// controller frame with the held-frame flush.
	testHookBeforeHeldFlush func()
}

func newMux(machine machinetransport.Conn) *mux {
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

func (m *mux) beginLocalControllerTransfer() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.controllerMidXfer {
		return false
	}
	if m.interactive {
		m.abortInteractiveLocked()
	}
	m.controllerMidXfer = true
	return true
}

func (m *mux) finishLocalControllerTransfer() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.controllerMidXfer = false
}

// writeControl writes a single realtime control character to the machine
// transport out-of-band, without taking the injection window. Transports must
// serialize Write calls so a CTRL_SINGLE frame cannot byte-interleave with an
// active injection, controller file transfer, or the relay's own pumps.
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
	// One 32 KB machine read can decode hundreds of LOAD_INFO frames; size the
	// buffer for a full burst. If it still overflows, the transport is errored
	// (see deliverInjectLocked) rather than silently truncated.
	m.injectCh = make(chan protocol.Frame, 256)
	m.injectOverflow = false
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
	m.injectOverflow = false
	m.abortCh = make(chan struct{})
	abortCh := m.abortCh
	m.mu.Unlock()

	it := &injectTransport{m: m}
	release := func() { m.releaseInjection() }
	return it, abortCh, release, nil
}

// releaseInjection ends the injection window, flushing held controller frames
// to the machine so the controller's pending traffic proceeds.
//
// Ordering invariant (F8): held frames must reach the machine before any
// controller frame forwarded after the release. `injecting` therefore stays
// true until the held queue has fully drained — while it is set the controller
// pump keeps holding new frames instead of forwarding them, so a freshly
// arrived frame can never overtake a held one. The machine writes themselves
// happen outside m.mu (never hold the mode lock across I/O); serialization is
// by state: the pump forwards only when injecting is false, which this
// function only makes true after the last held frame was written.
func (m *mux) releaseInjection() {
	firstFlush := true
	for {
		m.mu.Lock()
		held := m.heldController
		m.heldController = nil
		if len(held) == 0 {
			// Nothing (left) held: end the window atomically with the empty
			// queue so the pump resumes forwarding only after every held
			// frame reached the machine.
			m.injecting = false
			m.interactive = false
			m.injectStatusPolls = 0
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
			return
		}
		m.mu.Unlock()

		if firstFlush && m.testHookBeforeHeldFlush != nil {
			m.testHookBeforeHeldFlush()
		}
		firstFlush = false
		for _, raw := range held {
			_, _ = m.machine.Write(raw)
		}
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
	if f.Cmd == protocol.CmdStatusRes {
		if m.interactive {
			m.deliverInjectLocked(f)
			return false
		}
		if m.injectStatusPolls > 0 {
			m.injectStatusPolls--
			m.deliverInjectLocked(f)
			return false
		}
		// Stray status reports during injection still go to the controller; its
		// own polls are answered from cache and never hit the machine.
		return true
	}
	m.deliverInjectLocked(f)
	return false
}

func (m *mux) deliverInjectLocked(f protocol.Frame) {
	if m.injectCh == nil {
		return
	}
	select {
	case m.injectCh <- f:
	default:
		if m.interactive {
			// Interactive (jog) delivery is latest-wins status traffic;
			// dropping under pressure is acceptable there.
			return
		}
		// Managed injection: a dropped frame would let runManaged see a later
		// LOAD_FINISH and report success over a truncated reply stream. Error
		// the transport instead so the operation fails and is retried. We must
		// not block here (m.mu is held; the controller pump and heartbeat
		// answering path both need it), so backpressure is not an option.
		m.injectOverflow = true
		close(m.injectCh)
		m.injectCh = nil
	}
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
// injection: writes go straight to the machine transport, reads come from the
// diverted machine frames.
type injectTransport struct {
	m        *mux
	scanBuf  []byte // leftover bytes from a frame not fully consumed by Read
	writeSc  protocol.Scanner
	deadline time.Time
}

func (t *injectTransport) Write(p []byte) (int, error) {
	t.m.mu.Lock()
	overflow := t.m.injectOverflow
	closed := !t.m.injecting || (t.m.interactive && t.m.injectCh == nil)
	t.m.mu.Unlock()
	if overflow {
		return 0, errInjectOverflow
	}
	if closed {
		return 0, errInjectionClosed
	}
	t.noteInjectedFrames(p)
	return t.m.machine.Write(p)
}

func (t *injectTransport) noteInjectedFrames(p []byte) {
	polls := 0
	for _, f := range t.writeSc.Push(p) {
		if isStatusPoll(f) {
			polls++
		}
	}
	if polls == 0 {
		return
	}
	t.m.mu.Lock()
	if t.m.injecting && !t.m.interactive && t.m.injectCh != nil {
		t.m.injectStatusPolls += polls
	}
	t.m.mu.Unlock()
}

func (t *injectTransport) Read(p []byte) (int, error) {
	if len(t.scanBuf) > 0 {
		n := copy(p, t.scanBuf)
		t.scanBuf = t.scanBuf[n:]
		return n, nil
	}
	t.m.mu.Lock()
	ch := t.m.injectCh
	overflow := t.m.injectOverflow
	t.m.mu.Unlock()
	if ch == nil {
		if overflow {
			return 0, errInjectOverflow
		}
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
			t.m.mu.Lock()
			overflow := t.m.injectOverflow
			t.m.mu.Unlock()
			if overflow {
				return 0, errInjectOverflow
			}
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

// errInjectOverflow errors a managed injection whose reply frames arrived
// faster than the injector consumed them. The operation fails (and can be
// retried) instead of acting on a silently truncated reply stream.
var errInjectOverflow = errors.New("relay: injection reply overflow, frames arrived faster than the injector consumed them")

// timeoutError is a net.Error timeout, so a read deadline on the injection
// transport surfaces the same way a socket read timeout would.
type timeoutError struct{}

func (timeoutError) Error() string   { return "relay: injection read timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
