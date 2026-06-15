// Package jog implements the low-latency, server-side gamepad jogging engine.
package jog

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/uwin/cnc-proxy/internal/gcodelog"
	"github.com/uwin/cnc-proxy/internal/machine"
	"github.com/uwin/cnc-proxy/internal/protocol"
	"github.com/uwin/cnc-proxy/internal/session"
)

const (
	CodeDisabled          = "disabled"
	CodeNotIdle           = "not_idle"
	CodeBusy              = "busy"
	CodeStaleStatus       = "stale_status"
	CodeBadInput          = "bad_input"
	CodeControllerWaiting = "controller_waiting"
	CodeMachineError      = "machine_error"
	CodeUnauthorized      = "unauthorized"

	deadzone      = 0.12
	slowScale     = 0.2
	maxXYLeadMM   = 2.5
	maxZLeadMM    = 1.0
	statusTimeout = 2 * time.Second
	motionLogGap  = time.Second
)

// Config controls the jog engine.
type Config struct {
	Enabled         bool
	MaxXYMMMin      float64
	MaxZMMMin       float64
	Tick            time.Duration
	StatusInterval  time.Duration
	DeadmanTimeout  time.Duration
	MotionPrimitive MotionPrimitive
	Log             Logger
}

// MotionPrimitive selects the wire command used for one jog segment.
type MotionPrimitive string

const (
	// MotionPrimitiveInstant uses the firmware SimpleShell `$J` path. It plans a
	// relative XYZ delta without touching modal G90/G91 state and force-starts
	// the conveyor queue, which makes gamepad jogs lower latency than normal G0.
	MotionPrimitiveInstant MotionPrimitive = "instant"
	// MotionPrimitiveG53 keeps the older absolute machine-coordinate G53 G0
	// segment path as a fallback.
	MotionPrimitiveG53 MotionPrimitive = "g53"
)

// Logger records operational jog activity without coupling the jog engine to
// the API/service layer.
type Logger interface {
	Append(dir, source, text string)
}

// DefaultConfig returns production-safe defaults.
func DefaultConfig() Config {
	return Config{
		Enabled:         true,
		MaxXYMMMin:      1200,
		MaxZMMMin:       300,
		Tick:            50 * time.Millisecond,
		StatusInterval:  100 * time.Millisecond,
		DeadmanTimeout:  150 * time.Millisecond,
		MotionPrimitive: MotionPrimitiveInstant,
	}
}

func (c Config) normalize() Config {
	d := DefaultConfig()
	if c.MaxXYMMMin <= 0 {
		c.MaxXYMMMin = d.MaxXYMMMin
	}
	if c.MaxZMMMin <= 0 {
		c.MaxZMMMin = d.MaxZMMMin
	}
	if c.Tick <= 0 {
		c.Tick = d.Tick
	}
	if c.StatusInterval <= 0 {
		c.StatusInterval = d.StatusInterval
	}
	if c.DeadmanTimeout <= 0 {
		c.DeadmanTimeout = d.DeadmanTimeout
	}
	switch c.MotionPrimitive {
	case "", MotionPrimitiveInstant:
		c.MotionPrimitive = MotionPrimitiveInstant
	case MotionPrimitiveG53:
	default:
		c.MotionPrimitive = d.MotionPrimitive
	}
	return c
}

// Axes is a normalized XYZ gamepad vector.
type Axes struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

// Input is the latest operator intent from the gamepad client.
type Input struct {
	Seq     int64
	Axes    Axes
	Deadman bool
	Slow    bool
	At      time.Time
}

// Availability explains whether a new jog session can arm right now.
type Availability struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	Message   string `json:"message,omitempty"`
}

// Capabilities is returned by /api/jog/capabilities.
type Capabilities struct {
	Enabled          bool         `json:"enabled"`
	Axes             []string     `json:"axes"`
	MaxXYMMMin       float64      `json:"max_xy_mm_min"`
	MaxZMMMin        float64      `json:"max_z_mm_min"`
	TickMs           int64        `json:"tick_ms"`
	StatusIntervalMs int64        `json:"status_interval_ms"`
	DeadmanTimeoutMs int64        `json:"deadman_timeout_ms"`
	Availability     Availability `json:"availability"`
}

// Event is a WebSocket-ready server message.
type Event struct {
	Type         string        `json:"type"`
	Seq          int64         `json:"seq,omitempty"`
	Code         string        `json:"code,omitempty"`
	Message      string        `json:"message,omitempty"`
	Armed        *bool         `json:"armed,omitempty"`
	Mode         string        `json:"mode,omitempty"`
	Availability *Availability `json:"availability,omitempty"`
	Capabilities *Capabilities `json:"capabilities,omitempty"`
	Status       *StatusEvent  `json:"status,omitempty"`
	Motion       *MotionEvent  `json:"motion,omitempty"`
	LatencyMs    int64         `json:"latency_ms,omitempty"`
}

// StatusEvent is the status subset streamed to jog clients.
type StatusEvent struct {
	State      machine.State      `json:"state"`
	AgeMs      int64              `json:"age_ms"`
	ObservedAt time.Time          `json:"observed_at,omitempty"`
	Raw        string             `json:"raw,omitempty"`
	MPos       machine.AxisValues `json:"mpos,omitempty"`
	WPos       machine.AxisValues `json:"wpos,omitempty"`
}

// MotionEvent reports one server-generated jog segment for visualization and
// low-rate telemetry. It is best-effort; slow clients may miss motion events.
type MotionEvent struct {
	Target   machine.AxisValues `json:"target,omitempty"`
	Observed machine.AxisValues `json:"observed,omitempty"`
	Delta    Axes               `json:"delta"`
	Lead     Axes               `json:"lead"`
	Command  string             `json:"command,omitempty"`
}

type command struct {
	typ    string
	seq    int64
	action string
}

// Manager owns the single active jog session.
type Manager struct {
	cfg Config
	arb *session.Arbiter

	mu     sync.Mutex
	active *Session
	now    func() time.Time
}

// New creates a Manager.
func New(arb *session.Arbiter, cfg Config) *Manager {
	cfg = cfg.normalize()
	return &Manager{cfg: cfg, arb: arb, now: time.Now}
}

// Config returns the normalized config.
func (m *Manager) Config() Config { return m.cfg }

// Capabilities reports static limits plus current availability.
func (m *Manager) Capabilities() Capabilities {
	return m.capabilities(nil)
}

func (m *Manager) capabilities(ignore *Session) Capabilities {
	return Capabilities{
		Enabled:          m.cfg.Enabled,
		Axes:             []string{"x", "y", "z"},
		MaxXYMMMin:       m.cfg.MaxXYMMMin,
		MaxZMMMin:        m.cfg.MaxZMMMin,
		TickMs:           m.cfg.Tick.Milliseconds(),
		StatusIntervalMs: m.cfg.StatusInterval.Milliseconds(),
		DeadmanTimeoutMs: m.cfg.DeadmanTimeout.Milliseconds(),
		Availability:     m.availability(ignore),
	}
}

// Availability reports whether a new session can arm right now.
func (m *Manager) Availability() Availability {
	return m.availability(nil)
}

func (m *Manager) availability(ignore *Session) Availability {
	if !m.cfg.Enabled {
		return Availability{Available: false, Reason: CodeDisabled, Message: "Jogging is disabled."}
	}
	m.mu.Lock()
	active := m.active
	m.mu.Unlock()
	if active != nil && active != ignore {
		return Availability{Available: false, Reason: CodeBusy, Message: "Another jog session is active. Close the other CNC Proxy tab/client or wait for it to disconnect."}
	}
	st, _ := m.arb.Tracker().Current()
	if !m.arb.Tracker().Fresh(m.arb.StateMaxAge()) {
		return Availability{Available: false, Reason: CodeStaleStatus, Message: "Machine status is stale. Wait for a fresh Idle status before jogging."}
	}
	if st.State != machine.Idle {
		return Availability{Available: false, Reason: CodeNotIdle, Message: "Machine is " + stateLabel(st.State) + ". Jogging requires fresh Idle status."}
	}
	if len(st.MPos) == 0 {
		return Availability{Available: false, Reason: CodeStaleStatus, Message: "Machine position is unavailable. Wait for a status report with MPos before jogging."}
	}
	return Availability{Available: true, Message: "Ready to arm jog."}
}

// Start opens a single active session. The session does not acquire the machine
// lease until the client arms it.
func (m *Manager) Start(ctx context.Context) (*Session, error) {
	if !m.cfg.Enabled {
		return nil, Error{Code: CodeDisabled, Message: "jogging is disabled"}
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &Session{
		mgr:    m,
		ctx:    ctx,
		cancel: cancel,
		cmds:   make(chan command, 16),
		events: make(chan Event, 128),
		done:   make(chan struct{}),
	}
	m.mu.Lock()
	if m.active != nil {
		m.mu.Unlock()
		cancel()
		return nil, Error{Code: CodeBusy, Message: "another jog session is active"}
	}
	m.active = s
	m.mu.Unlock()
	go s.run()
	return s, nil
}

func (m *Manager) clearActive(s *Session) {
	m.mu.Lock()
	if m.active == s {
		m.active = nil
	}
	m.mu.Unlock()
}

// Error is a stable jog error with an API code.
type Error struct {
	Code    string
	Message string
}

func (e Error) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Message
}

// Session is one active WebSocket jog session.
type Session struct {
	mgr    *Manager
	ctx    context.Context
	cancel context.CancelFunc

	cmds   chan command
	events chan Event
	done   chan struct{}
	once   sync.Once
	emitMu sync.Mutex
	closed bool

	mu            sync.Mutex
	latest        Input
	haveInput     bool
	armed         bool
	lease         *session.JogLease
	lastStatus    machine.Status
	lastStatusAt  time.Time
	planned       machine.AxisValues
	lastMotionCmd string
	lastAlarmRaw  string
	lastMotionLog time.Time
}

// Events streams server messages until the session closes.
func (s *Session) Events() <-chan Event { return s.events }

// Close ends the session.
func (s *Session) Close() {
	s.once.Do(func() {
		s.cancel()
		<-s.done
	})
}

// Arm requests the machine lease.
func (s *Session) Arm(seq int64) { s.enqueue(command{typ: "arm", seq: seq}) }

// Disarm releases the machine lease.
func (s *Session) Disarm(seq int64) { s.enqueue(command{typ: "disarm", seq: seq}) }

// Control sends an out-of-band realtime control.
func (s *Session) Control(seq int64, action string) {
	s.enqueue(command{typ: "control", seq: seq, action: action})
}

// ReportError emits a client-facing validation error.
func (s *Session) ReportError(seq int64, code, msg string) {
	s.emit(Event{Type: "error", Seq: seq, Code: code, Message: msg})
}

// SetInput replaces the latest gamepad intent. Inputs are never queued.
func (s *Session) SetInput(in Input) {
	if in.At.IsZero() {
		in.At = s.mgr.now()
	}
	s.mu.Lock()
	s.latest = in
	s.haveInput = true
	s.mu.Unlock()
}

func (s *Session) enqueue(cmd command) {
	select {
	case s.cmds <- cmd:
	case <-s.ctx.Done():
	default:
		s.emit(Event{Type: "error", Seq: cmd.seq, Code: CodeBusy, Message: "jog command queue is full"})
	}
}

func (s *Session) run() {
	defer func() {
		s.release(nil)
		s.mgr.clearActive(s)
		s.closeEvents()
		close(s.done)
	}()
	s.emit(Event{Type: "hello", Capabilities: ptr(s.mgr.capabilities(s))})

	tick := time.NewTicker(s.mgr.cfg.Tick)
	defer tick.Stop()
	statusTick := time.NewTicker(s.mgr.cfg.StatusInterval)
	defer statusTick.Stop()

	for {
		var abort <-chan struct{}
		s.mu.Lock()
		if s.lease != nil {
			abort = s.lease.Abort
		}
		s.mu.Unlock()

		select {
		case <-s.ctx.Done():
			return
		case <-abort:
			s.release(nil)
			s.emit(Event{Type: "error", Code: CodeControllerWaiting, Message: "controller requested the machine connection"})
			s.emitState(0)
		case cmd := <-s.cmds:
			s.handleCommand(cmd)
		case <-statusTick.C:
			if s.hasLease() {
				if err := s.refreshStatus(); err != nil {
					s.failLease(CodeMachineError, err)
				}
			}
		case <-tick.C:
			if s.hasLease() {
				s.motionTick()
			}
		}
	}
}

func (s *Session) handleCommand(cmd command) {
	switch cmd.typ {
	case "arm":
		s.handleArm(cmd.seq)
	case "disarm":
		s.release(nil)
		s.emit(Event{Type: "ack", Seq: cmd.seq})
		s.emitState(cmd.seq)
	case "control":
		if err := s.sendControl(cmd.action); err != nil {
			s.emit(Event{Type: "error", Seq: cmd.seq, Code: CodeBadInput, Message: err.Error()})
			return
		}
		if cmd.action == "hold" || cmd.action == "feedhold" || cmd.action == "pause" || cmd.action == "halt" || cmd.action == "stop" || cmd.action == "estop" {
			s.release(nil)
		}
		s.emit(Event{Type: "ack", Seq: cmd.seq})
		s.emitState(cmd.seq)
	}
}

func (s *Session) handleArm(seq int64) {
	if !s.mgr.cfg.Enabled {
		s.emit(Event{Type: "error", Seq: seq, Code: CodeDisabled, Message: "jogging is disabled"})
		return
	}
	if s.hasLease() {
		s.emit(Event{Type: "ack", Seq: seq})
		s.emitState(seq)
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
	defer cancel()
	lease, err := s.mgr.arb.AcquireJog(ctx)
	if err != nil {
		s.emit(Event{Type: "error", Seq: seq, Code: codeForErr(err), Message: err.Error()})
		s.emitState(seq)
		return
	}
	s.mu.Lock()
	s.lease = lease
	s.armed = true
	s.haveInput = false
	s.planned = nil
	s.mu.Unlock()

	if err := s.refreshStatus(); err != nil {
		s.failLease(CodeMachineError, err)
		return
	}
	s.mu.Lock()
	st := s.lastStatus
	s.mu.Unlock()
	if st.State != machine.Idle || len(st.MPos) == 0 {
		s.release(nil)
		code := CodeNotIdle
		if len(st.MPos) == 0 {
			code = CodeStaleStatus
		}
		s.emit(Event{Type: "error", Seq: seq, Code: code, Message: "machine is not ready to jog"})
		s.emitState(seq)
		return
	}
	s.mu.Lock()
	s.planned = copyAxes(st.MPos)
	s.mu.Unlock()
	s.emit(Event{Type: "ack", Seq: seq})
	s.emitState(seq)
}

func (s *Session) sendControl(action string) error {
	var c byte
	switch action {
	case "hold", "feedhold", "pause":
		c = protocol.CtrlFeedHold
	case "resume":
		c = protocol.CtrlResume
	case "halt", "stop", "estop":
		c = protocol.CtrlHalt
	default:
		return fmt.Errorf("action must be one of: hold, resume, halt")
	}
	if err := s.mgr.arb.SendControl(c); err != nil {
		return err
	}
	s.log(gcodelog.DirSend, "control "+action)
	return nil
}

func (s *Session) refreshStatus() error {
	s.mu.Lock()
	lease := s.lease
	s.mu.Unlock()
	if lease == nil {
		return nil
	}
	payload, err := lease.Conn.QueryState(statusTimeout)
	if err != nil {
		return err
	}
	if !s.mgr.arb.Tracker().ObserveStatusPayload(payload) {
		return Error{Code: CodeStaleStatus, Message: "machine returned malformed status"}
	}
	st, age := s.mgr.arb.Tracker().Current()
	s.mu.Lock()
	s.lastStatus = st
	s.lastStatusAt = s.mgr.now()
	s.mu.Unlock()
	if st.State == machine.Alarm {
		s.logAlarmStatus(st)
	}
	s.emit(Event{Type: "status", Status: statusEvent(st, age)})
	if !canContinueJog(st.State) {
		s.release(nil)
		s.emit(Event{Type: "error", Code: CodeNotIdle, Message: "machine left joggable state: " + stateLabel(st.State)})
		s.emitState(0)
	}
	return nil
}

func (s *Session) motionTick() {
	now := s.mgr.now()
	s.mu.Lock()
	in := s.latest
	haveInput := s.haveInput
	st := s.lastStatus
	planned := copyAxes(s.planned)
	lastStatusAt := s.lastStatusAt
	s.mu.Unlock()

	if !haveInput || !in.Deadman || now.Sub(in.At) > s.mgr.cfg.DeadmanTimeout {
		return
	}
	if !canContinueJog(st.State) {
		if st.State == machine.Alarm {
			s.logAlarmStatus(st)
		}
		s.release(nil)
		s.emit(Event{Type: "error", Code: CodeNotIdle, Message: "machine left joggable state: " + stateLabel(st.State)})
		s.emitState(0)
		return
	}
	if len(st.MPos) == 0 || now.Sub(lastStatusAt) > max(3*s.mgr.cfg.StatusInterval, s.mgr.cfg.DeadmanTimeout) {
		s.release(nil)
		s.emit(Event{Type: "error", Code: CodeStaleStatus, Message: "status is too stale to jog"})
		s.emitState(0)
		return
	}
	if planned == nil {
		planned = copyAxes(st.MPos)
	}
	if leadTooLarge(planned, st.MPos) {
		return
	}

	delta := Normalize(in.Axes, in.Slow, s.mgr.cfg)
	if delta.X == 0 && delta.Y == 0 && delta.Z == 0 {
		return
	}
	target := copyAxes(planned)
	target["x"] += delta.X
	target["y"] += delta.Y
	target["z"] += delta.Z
	cmd := jogCommand(target, delta, s.mgr.cfg.MotionPrimitive)

	s.mu.Lock()
	lease := s.lease
	s.mu.Unlock()
	if lease == nil {
		return
	}
	if err := lease.Conn.WriteGcodeLine(cmd); err != nil {
		s.failLease(CodeMachineError, err)
		return
	}
	s.mu.Lock()
	s.planned = target
	s.lastMotionCmd = cmd
	s.mu.Unlock()
	s.emit(Event{Type: "motion", Motion: motionEvent(target, st.MPos, delta, cmd)})
	s.logMotion(now, cmd)
}

// Normalize converts raw axes into one tick of motion in mm.
func Normalize(axes Axes, slow bool, cfg Config) Axes {
	cfg = cfg.normalize()
	x := response(axes.X)
	y := response(axes.Y)
	z := response(axes.Z)
	if mag := math.Hypot(x, y); mag > 1 {
		x /= mag
		y /= mag
	}
	scale := 1.0
	if slow {
		scale = slowScale
	}
	dtMin := cfg.Tick.Minutes()
	return Axes{
		X: x * cfg.MaxXYMMMin * dtMin * scale,
		Y: y * cfg.MaxXYMMMin * dtMin * scale,
		Z: z * cfg.MaxZMMMin * dtMin * scale,
	}
}

func response(v float64) float64 {
	if v > 1 {
		v = 1
	} else if v < -1 {
		v = -1
	}
	sign := 1.0
	if v < 0 {
		sign = -1
		v = -v
	}
	if v < deadzone {
		return 0
	}
	n := (v - deadzone) / (1 - deadzone)
	return sign * n * n * n
}

func jogCommand(target machine.AxisValues, delta Axes, primitive MotionPrimitive) string {
	if primitive == MotionPrimitiveG53 {
		return g53JogCommand(target, delta)
	}
	return instantJogCommand(delta)
}

func instantJogCommand(delta Axes) string {
	parts := "$J"
	if delta.X != 0 {
		parts += fmt.Sprintf(" X%.4f", delta.X)
	}
	if delta.Y != 0 {
		parts += fmt.Sprintf(" Y%.4f", delta.Y)
	}
	if delta.Z != 0 {
		parts += fmt.Sprintf(" Z%.4f", delta.Z)
	}
	return parts
}

func g53JogCommand(target machine.AxisValues, delta Axes) string {
	parts := "G53 G0"
	if delta.X != 0 {
		parts += fmt.Sprintf(" X%.4f", target["x"])
	}
	if delta.Y != 0 {
		parts += fmt.Sprintf(" Y%.4f", target["y"])
	}
	if delta.Z != 0 {
		parts += fmt.Sprintf(" Z%.4f", target["z"])
	}
	return parts
}

func leadTooLarge(planned, observed machine.AxisValues) bool {
	return math.Abs(planned["x"]-observed["x"]) > maxXYLeadMM ||
		math.Abs(planned["y"]-observed["y"]) > maxXYLeadMM ||
		math.Abs(planned["z"]-observed["z"]) > maxZLeadMM
}

func canContinueJog(st machine.State) bool {
	return st == machine.Idle || st == machine.Run
}

func stateLabel(st machine.State) string {
	if st == machine.Unknown {
		return "Unknown"
	}
	return string(st)
}

func motionEvent(target, observed machine.AxisValues, delta Axes, cmd string) *MotionEvent {
	return &MotionEvent{
		Target:   copyAxes(target),
		Observed: copyAxes(observed),
		Delta:    delta,
		Lead: Axes{
			X: target["x"] - observed["x"],
			Y: target["y"] - observed["y"],
			Z: target["z"] - observed["z"],
		},
		Command: cmd,
	}
}

func (s *Session) logMotion(now time.Time, cmd string) {
	s.mu.Lock()
	last := s.lastMotionLog
	if !last.IsZero() && now.Sub(last) < motionLogGap {
		s.mu.Unlock()
		return
	}
	s.lastMotionLog = now
	s.mu.Unlock()
	s.log(gcodelog.DirSend, cmd)
}

func (s *Session) logAlarmStatus(st machine.Status) {
	raw := st.Raw
	if raw == "" {
		raw = string(st.State)
	}
	s.mu.Lock()
	if s.lastAlarmRaw == raw {
		s.mu.Unlock()
		return
	}
	s.lastAlarmRaw = raw
	lastCmd := s.lastMotionCmd
	s.mu.Unlock()

	text := "alarm status: " + raw
	if st.HaltReason != nil {
		text += fmt.Sprintf(" (H:%d %s; recovery: %s)", st.HaltReason.Code, st.HaltReason.Message, st.HaltReason.Recovery)
	}
	if lastCmd != "" {
		text += " after " + lastCmd
	}
	s.log(gcodelog.DirRecv, text)
}

func (s *Session) log(dir, text string) {
	if s.mgr.cfg.Log == nil {
		return
	}
	s.mgr.cfg.Log.Append(dir, gcodelog.SourceJog, text)
}

func (s *Session) release(err error) {
	s.mu.Lock()
	lease := s.lease
	s.lease = nil
	s.armed = false
	s.planned = nil
	s.lastMotionCmd = ""
	s.lastAlarmRaw = ""
	s.lastMotionLog = time.Time{}
	s.mu.Unlock()
	if lease != nil {
		lease.Release(err)
	}
}

func (s *Session) failLease(code string, err error) {
	s.release(err)
	s.emit(Event{Type: "error", Code: code, Message: err.Error()})
	s.emitState(0)
}

func (s *Session) hasLease() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lease != nil
}

func (s *Session) emitState(seq int64) {
	avail := s.mgr.availability(s)
	s.mu.Lock()
	armed := s.armed
	mode := ""
	if s.lease != nil {
		mode = s.lease.Mode.String()
	}
	s.mu.Unlock()
	s.emit(Event{Type: "state", Seq: seq, Armed: &armed, Mode: mode, Availability: &avail})
}

func (s *Session) emit(ev Event) {
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.events <- ev:
	case <-s.ctx.Done():
	default:
		if ev.Type != "status" && ev.Type != "motion" {
			select {
			case s.events <- ev:
			case <-s.ctx.Done():
			}
		}
	}
}

func (s *Session) closeEvents() {
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.events)
}

func statusEvent(st machine.Status, age time.Duration) *StatusEvent {
	return &StatusEvent{
		State:      st.State,
		AgeMs:      age.Milliseconds(),
		ObservedAt: st.ObservedAt,
		Raw:        st.Raw,
		MPos:       copyAxes(st.MPos),
		WPos:       copyAxes(st.WPos),
	}
}

func codeForErr(err error) string {
	switch {
	case errors.Is(err, session.ErrNotIdle):
		return CodeNotIdle
	case errors.Is(err, session.ErrBusy), errors.Is(err, session.ErrRelayActive), errors.Is(err, context.DeadlineExceeded):
		return CodeBusy
	default:
		return CodeMachineError
	}
}

func copyAxes(in machine.AxisValues) machine.AxisValues {
	if len(in) == 0 {
		return nil
	}
	out := make(machine.AxisValues, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func ptr[T any](v T) *T { return &v }
