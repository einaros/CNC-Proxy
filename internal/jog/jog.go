// Package jog implements the low-latency, server-side gamepad jogging engine.
package jog

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"strings"
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
	CodeStatusWaiting     = "status_waiting"
	CodeBadInput          = "bad_input"
	CodeControllerWaiting = "controller_waiting"
	CodeMachineError      = "machine_error"
	CodeUnauthorized      = "unauthorized"

	deadzone        = 0.12
	slowScale       = 0.2
	baseMaxXYLeadMM = 2.5
	baseMaxZLeadMM  = 1.0
	// The firmware's `$J ... F` argument is a scale of the slowest selected
	// actuator max rate, not a feedrate. These match CarveraFirmware
	// config.default for XYZ. Firmware accepts scales above 1; the firmware
	// planner may still cap real hardware at configured machine limits.
	firmwareMaxXYMMMin = 3000.0
	firmwareMaxZMMMin  = 2000.0
	minJogTick         = 5 * time.Millisecond
	minStatusEvery     = 100 * time.Millisecond
	minDeadmanTimeout  = 300 * time.Millisecond
	minJogSegment      = 60 * time.Millisecond
	maxJogSegment      = 100 * time.Millisecond
	minJogLookahead    = 120 * time.Millisecond
	maxJogLookahead    = 180 * time.Millisecond
	minActiveStatusGap = time.Second
	maxActiveStatusGap = 2 * time.Second
	maxSegmentsPerTick = 2
	motionLogGap       = time.Second
	motionEventGap     = 33 * time.Millisecond
	statusWaitGap      = 500 * time.Millisecond
	maxManualStepMM    = 50.0
	maxTargetFeedMMMin = 10000.0
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
		MaxXYMMMin:      firmwareMaxXYMMMin,
		MaxZMMMin:       300,
		Tick:            20 * time.Millisecond,
		StatusInterval:  100 * time.Millisecond,
		DeadmanTimeout:  minDeadmanTimeout,
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
	if c.Tick < minJogTick {
		c.Tick = minJogTick
	}
	if c.StatusInterval <= 0 {
		c.StatusInterval = d.StatusInterval
	}
	if c.StatusInterval < minStatusEvery {
		c.StatusInterval = minStatusEvery
	}
	if c.DeadmanTimeout <= 0 {
		c.DeadmanTimeout = d.DeadmanTimeout
	}
	if c.DeadmanTimeout < minDeadmanTimeout {
		c.DeadmanTimeout = minDeadmanTimeout
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
	Target        machine.AxisValues `json:"target,omitempty"`
	Observed      machine.AxisValues `json:"observed,omitempty"`
	Estimated     machine.AxisValues `json:"estimated,omitempty"`
	EstimatedWPos machine.AxisValues `json:"estimated_wpos,omitempty"`
	Delta         Axes               `json:"delta"`
	Lead          Axes               `json:"lead"`
	QueueLeadMs   int64              `json:"queue_lead_ms,omitempty"`
	Command       string             `json:"command,omitempty"`
}

type command struct {
	typ      string
	seq      int64
	action   string
	axis     string
	distance float64
	value    float64
	target   machine.AxisValues
	feed     float64
	safeZOn  bool
	safeZ    float64
}

type statusResult struct {
	leaseID int64
	payload string
	err     error
}

type plannedSegment struct {
	start time.Time
	end   time.Time
	from  machine.AxisValues
	to    machine.AxisValues
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
	activeJog := ignore != nil && active == ignore && ignore.hasLease()
	if !m.arb.Tracker().Fresh(m.arb.StateMaxAge()) {
		return Availability{Available: false, Reason: CodeStaleStatus, Message: "Machine status is stale. Wait for a fresh Idle status before jogging."}
	}
	if activeJog && canContinueJog(st.State) && len(st.MPos) > 0 {
		return Availability{Available: true, Message: "Jog session active."}
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
		mgr:      m,
		ctx:      ctx,
		cancel:   cancel,
		cmds:     make(chan command, 16),
		statusCh: make(chan statusResult, 4),
		events:   make(chan Event, 128),
		done:     make(chan struct{}),
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

	cmds     chan command
	statusCh chan statusResult
	events   chan Event
	done     chan struct{}
	once     sync.Once
	emitMu   sync.Mutex
	closed   bool

	mu              sync.Mutex
	latest          Input
	haveInput       bool
	armed           bool
	lease           *session.JogLease
	leaseID         int64
	statusInFlight  bool
	statusWG        sync.WaitGroup
	lastStatus      machine.Status
	lastStatusAt    time.Time
	planned         machine.AxisValues
	queuedUntil     time.Time
	segments        []plannedSegment
	lastMotionCmd   string
	lastMotionEvent time.Time
	lastStatusWait  time.Time
	lastAlarmRaw    string
	lastMotionLog   time.Time
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

// Step requests one explicit axis jog from an armed session.
func (s *Session) Step(seq int64, axis string, distance float64) {
	s.enqueue(command{typ: "step", seq: seq, axis: axis, distance: distance})
}

// SetOrigin sets the current work-coordinate axis value.
func (s *Session) SetOrigin(seq int64, axis string, value float64) {
	s.enqueue(command{typ: "origin", seq: seq, axis: axis, value: value})
}

// Target requests one explicit XY move from an armed session.
func (s *Session) Target(seq int64, target machine.AxisValues, feedMMMin float64, safeZEnabled bool, safeZMM float64) {
	s.enqueue(command{typ: "target", seq: seq, target: copyAxes(target), feed: feedMMMin, safeZOn: safeZEnabled, safeZ: safeZMM})
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
		case res := <-s.statusCh:
			s.handleStatusResult(res)
		case <-statusTick.C:
			if s.shouldRequestStatus(s.mgr.now()) {
				s.requestStatus()
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
	case "step":
		s.handleStep(cmd.seq, cmd.axis, cmd.distance)
	case "origin":
		s.handleOrigin(cmd.seq, cmd.axis, cmd.value)
	case "target":
		s.handleTarget(cmd.seq, cmd.target, cmd.feed, cmd.safeZOn, cmd.safeZ)
	}
}

func (s *Session) handleStep(seq int64, axis string, distance float64) {
	delta, err := stepDelta(axis, distance)
	if err != nil {
		s.emit(Event{Type: "error", Seq: seq, Code: CodeBadInput, Message: err.Error()})
		return
	}
	now := s.mgr.now()
	s.mu.Lock()
	lease := s.lease
	st := s.lastStatus
	lastStatusAt := s.lastStatusAt
	planned := copyAxes(s.planned)
	queuedUntil := s.queuedUntil
	statusInFlight := s.statusInFlight
	s.mu.Unlock()
	if lease == nil {
		s.emit(Event{Type: "error", Seq: seq, Code: CodeBadInput, Message: "arm jog before using step buttons"})
		s.emitState(seq)
		return
	}
	if !canContinueJog(st.State) {
		if st.State == machine.Alarm {
			s.logAlarmStatus(st)
		}
		s.release(nil)
		s.emit(Event{Type: "error", Seq: seq, Code: CodeNotIdle, Message: "machine left joggable state: " + stateLabel(st.State)})
		s.emitState(seq)
		return
	}
	queuedLead := queueLead(now, queuedUntil)
	if (len(st.MPos) == 0 || now.Sub(lastStatusAt) > activeStatusMaxAge(s.mgr.cfg)) && queuedLead == 0 {
		if statusInFlight {
			s.emit(Event{Type: "error", Seq: seq, Code: CodeStatusWaiting, Message: "Waiting for fresh machine status before step jog."})
			return
		}
		s.emit(Event{Type: "error", Seq: seq, Code: CodeStatusWaiting, Message: "Waiting for fresh machine status before step jog."})
		s.requestStatus()
		return
	}
	if planned == nil || queuedLead == 0 {
		planned = copyAxes(st.MPos)
	}
	if planned == nil {
		s.emit(Event{Type: "error", Seq: seq, Code: CodeStaleStatus, Message: "machine position is unavailable"})
		return
	}

	target := copyAxes(planned)
	target["x"] += delta.X
	target["y"] += delta.Y
	target["z"] += delta.Z
	dur := stepJogDuration(delta, s.mgr.cfg)
	cmd := jogCommandForDuration(target, delta, s.mgr.cfg, dur)
	if err := lease.Conn.WriteGcodeLine(cmd); err != nil {
		s.failLease(CodeMachineError, err)
		return
	}
	segStart := queuedUntil
	if segStart.Before(now) {
		segStart = now
	}
	segEnd := segStart.Add(dur)
	queuedUntil = segEnd
	queuedLead = queueLead(now, queuedUntil)
	s.mu.Lock()
	s.planned = target
	s.queuedUntil = queuedUntil
	s.segments = appendPlannedSegment(s.segments, plannedSegment{
		start: segStart,
		end:   segEnd,
		from:  copyAxes(planned),
		to:    copyAxes(target),
	}, now)
	s.lastMotionCmd = cmd
	s.mu.Unlock()
	s.logMotion(now, cmd)
	s.emitMotionEstimate(now, st, target, delta, cmd, queuedLead)
	s.emit(Event{Type: "ack", Seq: seq})
}

func (s *Session) handleOrigin(seq int64, axis string, value float64) {
	cmd, err := originCommand(axis, value)
	if err != nil {
		s.emit(Event{Type: "error", Seq: seq, Code: CodeBadInput, Message: err.Error()})
		return
	}
	now := s.mgr.now()
	s.mu.Lock()
	lease := s.lease
	st := s.lastStatus
	lastStatusAt := s.lastStatusAt
	queuedUntil := s.queuedUntil
	statusInFlight := s.statusInFlight
	s.mu.Unlock()
	if lease == nil {
		s.emit(Event{Type: "error", Seq: seq, Code: CodeBadInput, Message: "arm tap move before setting origin"})
		s.emitState(seq)
		return
	}
	if st.State != machine.Idle {
		if st.State == machine.Alarm {
			s.logAlarmStatus(st)
			s.release(nil)
		} else if !canContinueJog(st.State) {
			s.release(nil)
		}
		s.emit(Event{Type: "error", Seq: seq, Code: CodeNotIdle, Message: "machine must be Idle to set origin: " + stateLabel(st.State)})
		s.emitState(seq)
		return
	}
	queuedLead := queueLead(now, queuedUntil)
	if queuedLead > 0 {
		s.emit(Event{Type: "error", Seq: seq, Code: CodeBusy, Message: "wait for queued jog motion to finish before setting origin"})
		return
	}
	if len(st.MPos) == 0 || now.Sub(lastStatusAt) > activeStatusMaxAge(s.mgr.cfg) {
		if statusInFlight {
			s.emit(Event{Type: "error", Seq: seq, Code: CodeStatusWaiting, Message: "Waiting for fresh machine status before setting origin."})
			return
		}
		s.emit(Event{Type: "error", Seq: seq, Code: CodeStatusWaiting, Message: "Waiting for fresh machine status before setting origin."})
		s.requestStatus()
		return
	}
	if err := lease.Conn.WriteGcodeLine(cmd); err != nil {
		s.failLease(CodeMachineError, err)
		return
	}
	s.log(gcodelog.DirSend, cmd)
	s.log(gcodelog.DirRecv, "ok")
	s.emit(Event{Type: "ack", Seq: seq})
	s.requestStatus()
}

func (s *Session) handleTarget(seq int64, targetAxes machine.AxisValues, feedMMMin float64, safeZEnabled bool, safeZMM float64) {
	if len(targetAxes) == 0 {
		s.emit(Event{Type: "error", Seq: seq, Code: CodeBadInput, Message: "target requires at least one axis"})
		return
	}
	hasXYTarget := false
	for axis, value := range targetAxes {
		if axis != "x" && axis != "y" && axis != "z" {
			s.emit(Event{Type: "error", Seq: seq, Code: CodeBadInput, Message: "target axis must be one of: x, y, z"})
			return
		}
		if math.IsNaN(value) || math.IsInf(value, 0) {
			s.emit(Event{Type: "error", Seq: seq, Code: CodeBadInput, Message: "target requires finite coordinates"})
			return
		}
		if axis == "x" || axis == "y" {
			hasXYTarget = true
		}
	}
	if safeZEnabled && (math.IsNaN(safeZMM) || math.IsInf(safeZMM, 0)) {
		s.emit(Event{Type: "error", Seq: seq, Code: CodeBadInput, Message: "safe Z must be finite"})
		return
	}
	cfg := s.mgr.cfg.normalize()
	if math.IsNaN(feedMMMin) || math.IsInf(feedMMMin, 0) || feedMMMin <= 0 || feedMMMin > maxTargetFeedMMMin {
		s.emit(Event{Type: "error", Seq: seq, Code: CodeBadInput, Message: fmt.Sprintf("feed must be between 1 and %.0f mm/min", maxTargetFeedMMMin)})
		return
	}
	now := s.mgr.now()
	s.mu.Lock()
	lease := s.lease
	st := s.lastStatus
	lastStatusAt := s.lastStatusAt
	planned := copyAxes(s.planned)
	queuedUntil := s.queuedUntil
	statusInFlight := s.statusInFlight
	s.mu.Unlock()
	if lease == nil {
		s.emit(Event{Type: "error", Seq: seq, Code: CodeBadInput, Message: "arm tap move before selecting a target"})
		s.emitState(seq)
		return
	}
	if !canContinueJog(st.State) {
		if st.State == machine.Alarm {
			s.logAlarmStatus(st)
		}
		s.release(nil)
		s.emit(Event{Type: "error", Seq: seq, Code: CodeNotIdle, Message: "machine left joggable state: " + stateLabel(st.State)})
		s.emitState(seq)
		return
	}
	queuedLead := queueLead(now, queuedUntil)
	if queuedLead > jogLookahead(cfg) {
		s.emit(Event{Type: "error", Seq: seq, Code: CodeBusy, Message: "tap move is already queued"})
		return
	}
	if (len(st.MPos) == 0 || now.Sub(lastStatusAt) > activeStatusMaxAge(cfg)) && queuedLead == 0 {
		if statusInFlight {
			s.emit(Event{Type: "error", Seq: seq, Code: CodeStatusWaiting, Message: "Waiting for fresh machine status before tap move."})
			return
		}
		s.emit(Event{Type: "error", Seq: seq, Code: CodeStatusWaiting, Message: "Waiting for fresh machine status before tap move."})
		s.requestStatus()
		return
	}
	if planned == nil || queuedLead == 0 {
		planned = copyAxes(st.MPos)
	}
	if planned == nil {
		s.emit(Event{Type: "error", Seq: seq, Code: CodeStaleStatus, Message: "machine position is unavailable"})
		return
	}
	for axis := range targetAxes {
		if _, ok := planned[axis]; !ok {
			s.emit(Event{Type: "error", Seq: seq, Code: CodeStaleStatus, Message: "machine " + strings.ToUpper(axis) + " position is unavailable"})
			return
		}
	}

	finalTarget := copyAxes(planned)
	for axis, value := range targetAxes {
		finalTarget[axis] = value
	}
	fullDelta := axesDelta(planned, finalTarget)
	if fullDelta.X == 0 && fullDelta.Y == 0 && fullDelta.Z == 0 {
		s.emitMotionEstimate(now, st, finalTarget, fullDelta, "", queuedLead)
		s.emit(Event{Type: "ack", Seq: seq})
		return
	}

	lastCmd := ""
	if safeZEnabled && hasXYTarget {
		plannedZ, ok := planned["z"]
		if !ok || math.IsNaN(plannedZ) || math.IsInf(plannedZ, 0) {
			s.emit(Event{Type: "error", Seq: seq, Code: CodeStaleStatus, Message: "machine Z position is unavailable for safe tap move"})
			return
		}
		if plannedZ < safeZMM {
			safeTarget := copyAxes(planned)
			safeTarget["z"] = safeZMM
			safeDelta := Axes{Z: safeZMM - plannedZ}
			var err error
			planned, queuedUntil, queuedLead, lastCmd, err = s.writePlannedJogSegment(now, lease, planned, safeTarget, safeDelta, stepJogDuration(safeDelta, cfg), queuedUntil, cfg)
			if err != nil {
				s.failLease(CodeMachineError, err)
				return
			}
		}
	}
	if safeZEnabled && hasXYTarget {
		xyTarget := copyAxes(planned)
		if x, ok := targetAxes["x"]; ok {
			xyTarget["x"] = x
		}
		if y, ok := targetAxes["y"]; ok {
			xyTarget["y"] = y
		}
		xyDelta := axesDelta(planned, xyTarget)
		if xyDelta.X != 0 || xyDelta.Y != 0 {
			var err error
			planned, queuedUntil, queuedLead, lastCmd, err = s.writePlannedJogSegment(now, lease, planned, xyTarget, xyDelta, targetMoveDuration(xyDelta, feedMMMin, cfg), queuedUntil, cfg)
			if err != nil {
				s.failLease(CodeMachineError, err)
				return
			}
		}
	}

	target := copyAxes(planned)
	for axis, value := range targetAxes {
		target[axis] = value
	}
	delta := axesDelta(planned, target)
	if delta.X != 0 || delta.Y != 0 || delta.Z != 0 {
		var err error
		_, queuedUntil, queuedLead, lastCmd, err = s.writePlannedJogSegment(now, lease, planned, target, delta, targetMoveDuration(delta, feedMMMin, cfg), queuedUntil, cfg)
		if err != nil {
			s.failLease(CodeMachineError, err)
			return
		}
	}
	s.emitMotionEstimate(now, st, target, delta, lastCmd, queuedLead)
	s.emit(Event{Type: "ack", Seq: seq})
}

func (s *Session) writePlannedJogSegment(now time.Time, lease *session.JogLease, from, target machine.AxisValues, delta Axes, dur time.Duration, queuedUntil time.Time, cfg Config) (machine.AxisValues, time.Time, time.Duration, string, error) {
	cmd := jogCommandForDuration(target, delta, cfg, dur)
	if err := lease.Conn.WriteGcodeLine(cmd); err != nil {
		return from, queuedUntil, queueLead(now, queuedUntil), "", err
	}
	segStart := queuedUntil
	if segStart.Before(now) {
		segStart = now
	}
	segEnd := segStart.Add(dur)
	queuedUntil = segEnd
	queuedLead := queueLead(now, queuedUntil)
	s.mu.Lock()
	s.planned = target
	s.queuedUntil = queuedUntil
	s.segments = appendPlannedSegment(s.segments, plannedSegment{
		start: segStart,
		end:   segEnd,
		from:  copyAxes(from),
		to:    copyAxes(target),
	}, now)
	s.lastMotionCmd = cmd
	s.mu.Unlock()
	s.logMotion(now, cmd)
	return copyAxes(target), queuedUntil, queuedLead, cmd, nil
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
	st, age := s.mgr.arb.Tracker().Current()
	if !s.mgr.arb.Tracker().Fresh(s.mgr.arb.StateMaxAge()) || st.State != machine.Idle || len(st.MPos) == 0 {
		lease.Release(nil)
		code := CodeNotIdle
		if !s.mgr.arb.Tracker().Fresh(s.mgr.arb.StateMaxAge()) || len(st.MPos) == 0 {
			code = CodeStaleStatus
		}
		s.emit(Event{Type: "error", Seq: seq, Code: code, Message: "machine is not ready to jog"})
		s.emitState(seq)
		return
	}
	now := s.mgr.now()
	s.mu.Lock()
	s.lease = lease
	s.leaseID++
	s.statusInFlight = false
	s.armed = true
	s.haveInput = false
	s.lastStatus = st
	s.lastStatusAt = now
	s.planned = copyAxes(st.MPos)
	s.queuedUntil = time.Time{}
	s.segments = nil
	s.mu.Unlock()
	s.emit(Event{Type: "status", Status: statusEvent(st, age)})
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
	payload, err := lease.Conn.QueryState(statusQueryTimeout(s.mgr.cfg))
	if err != nil {
		if isTimeout(err) {
			s.emitStatusWaiting(s.mgr.now())
			return nil
		}
		return err
	}
	return s.applyStatusPayload(payload)
}

func (s *Session) requestStatus() {
	s.mu.Lock()
	lease := s.lease
	if lease == nil || s.statusInFlight {
		s.mu.Unlock()
		return
	}
	leaseID := s.leaseID
	timeout := statusQueryTimeout(s.mgr.cfg)
	s.statusInFlight = true
	s.statusWG.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.statusWG.Done()
		payload, err := lease.Conn.QueryState(timeout)
		res := statusResult{leaseID: leaseID, payload: payload, err: err}
		select {
		case s.statusCh <- res:
		case <-s.ctx.Done():
		}
	}()
}

func (s *Session) shouldRequestStatus(now time.Time) bool {
	s.mu.Lock()
	lease := s.lease
	in := s.latest
	haveInput := s.haveInput
	inFlight := s.statusInFlight
	s.mu.Unlock()
	if lease == nil || inFlight {
		return false
	}
	if motionInputActive(haveInput, in, now, s.mgr.cfg) {
		return false
	}
	return true
}

func (s *Session) handleStatusResult(res statusResult) {
	s.mu.Lock()
	if res.leaseID != s.leaseID {
		s.mu.Unlock()
		return
	}
	s.statusInFlight = false
	if s.lease == nil {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	if res.err != nil {
		if isTimeout(res.err) {
			s.emitStatusWaiting(s.mgr.now())
			return
		}
		s.failLease(CodeMachineError, res.err)
		return
	}
	if err := s.applyStatusPayload(res.payload); err != nil {
		s.failLease(CodeMachineError, err)
	}
}

func (s *Session) applyStatusPayload(payload string) error {
	if !s.mgr.arb.Tracker().ObserveStatusPayload(payload) {
		return Error{Code: CodeStaleStatus, Message: "machine returned malformed status"}
	}
	st, age := s.mgr.arb.Tracker().Current()
	s.mu.Lock()
	s.lastStatus = st
	now := s.mgr.now()
	s.lastStatusAt = now
	if queueLead(now, s.queuedUntil) == 0 {
		s.planned = copyAxes(st.MPos)
		s.segments = nil
	}
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
	queuedUntil := s.queuedUntil
	s.mu.Unlock()

	queuedLead := queueLead(now, queuedUntil)
	activeMotion := motionInputActive(haveInput, in, now, s.mgr.cfg)
	hasMotionPlan := planned != nil && queuedLead > 0
	if !canContinueJog(st.State) {
		if st.State == machine.Alarm {
			s.logAlarmStatus(st)
		}
		s.release(nil)
		s.emit(Event{Type: "error", Code: CodeNotIdle, Message: "machine left joggable state: " + stateLabel(st.State)})
		s.emitState(0)
		return
	}
	if !activeMotion {
		if queuedLead > 0 {
			s.emitMotionEstimate(now, st, planned, Axes{}, "", queuedLead)
		}
		return
	}
	staleCorrectionStatus := len(st.MPos) == 0 || now.Sub(lastStatusAt) > activeStatusMaxAge(s.mgr.cfg)
	if staleCorrectionStatus && !hasMotionPlan {
		if queuedLead > 0 {
			s.emitMotionEstimate(now, st, planned, Axes{}, "", queuedLead)
		}
		s.emitStatusWaiting(now)
		return
	}
	if planned == nil || queuedLead == 0 {
		planned = copyAxes(st.MPos)
	}
	segDur := jogSegmentDuration(s.mgr.cfg)
	if queuedLead >= jogLookahead(s.mgr.cfg) {
		s.emitMotionEstimate(now, st, planned, Axes{}, "", queuedLead)
		return
	}

	delta := MotionDelta(in.Axes, in.Slow, s.mgr.cfg)
	if delta.X == 0 && delta.Y == 0 && delta.Z == 0 {
		if queuedLead > 0 {
			s.emitMotionEstimate(now, st, planned, Axes{}, "", queuedLead)
		}
		return
	}

	s.mu.Lock()
	lease := s.lease
	s.mu.Unlock()
	if lease == nil {
		return
	}
	lastCmd := ""
	for sent := 0; queuedLead < jogLookahead(s.mgr.cfg) && sent < maxSegmentsPerTick; sent++ {
		target := copyAxes(planned)
		target["x"] += delta.X
		target["y"] += delta.Y
		target["z"] += delta.Z
		cmd := jogCommandForDuration(target, delta, s.mgr.cfg, segDur)
		if err := lease.Conn.WriteGcodeLine(cmd); err != nil {
			s.failLease(CodeMachineError, err)
			return
		}
		segStart := queuedUntil
		if queuedUntil.Before(now) {
			segStart = now
		}
		segEnd := segStart.Add(segDur)
		queuedUntil = segEnd
		queuedLead = queueLead(now, queuedUntil)
		s.mu.Lock()
		s.planned = target
		s.queuedUntil = queuedUntil
		s.segments = appendPlannedSegment(s.segments, plannedSegment{
			start: segStart,
			end:   segEnd,
			from:  copyAxes(planned),
			to:    copyAxes(target),
		}, now)
		s.lastMotionCmd = cmd
		s.mu.Unlock()
		planned = target
		lastCmd = cmd
		s.logMotion(now, cmd)
	}
	s.emitMotionEstimate(now, st, planned, delta, lastCmd, queuedLead)
}

// Normalize converts raw axes into one tick of motion in mm.
func Normalize(axes Axes, slow bool, cfg Config) Axes {
	cfg = cfg.normalize()
	return normalizeForDuration(axes, slow, cfg, cfg.Tick)
}

// MotionDelta converts raw axes into one buffered jog segment in mm.
func MotionDelta(axes Axes, slow bool, cfg Config) Axes {
	cfg = cfg.normalize()
	return normalizeForDuration(axes, slow, cfg, jogSegmentDuration(cfg))
}

func normalizeForDuration(axes Axes, slow bool, cfg Config, d time.Duration) Axes {
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
	dtMin := d.Minutes()
	return Axes{
		X: x * cfg.MaxXYMMMin * dtMin * scale,
		Y: y * cfg.MaxXYMMMin * dtMin * scale,
		Z: z * cfg.MaxZMMMin * dtMin * scale,
	}
}

func stepDelta(axis string, distance float64) (Axes, error) {
	axis = strings.ToLower(strings.TrimSpace(axis))
	if axis != "x" && axis != "y" && axis != "z" {
		return Axes{}, fmt.Errorf("axis must be one of: x, y, z")
	}
	if math.IsNaN(distance) || math.IsInf(distance, 0) || distance == 0 {
		return Axes{}, fmt.Errorf("distance must be non-zero")
	}
	if math.Abs(distance) > maxManualStepMM {
		return Axes{}, fmt.Errorf("distance must be between %.1f and %.1f mm", -maxManualStepMM, maxManualStepMM)
	}
	switch axis {
	case "x":
		return Axes{X: distance}, nil
	case "y":
		return Axes{Y: distance}, nil
	default:
		return Axes{Z: distance}, nil
	}
}

func originCommand(axis string, value float64) (string, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "", fmt.Errorf("origin value must be finite")
	}
	axis = strings.ToLower(strings.TrimSpace(axis))
	switch axis {
	case "x", "y", "z":
		return fmt.Sprintf("G10L20P0%s%.4f", strings.ToUpper(axis), value), nil
	default:
		return "", fmt.Errorf("axis must be one of: x, y, z")
	}
}

func stepJogDuration(delta Axes, cfg Config) time.Duration {
	cfg = cfg.normalize()
	xyDist := math.Hypot(delta.X, delta.Y)
	zDist := math.Abs(delta.Z)
	mins := 0.0
	if xyDist > 0 && cfg.MaxXYMMMin > 0 {
		mins = xyDist / cfg.MaxXYMMMin
	}
	if zDist > 0 && cfg.MaxZMMMin > 0 {
		zMins := zDist / cfg.MaxZMMMin
		if zMins > mins {
			mins = zMins
		}
	}
	d := time.Duration(mins * float64(time.Minute))
	if d < minJogSegment {
		return minJogSegment
	}
	return d
}

func axesDelta(from, target machine.AxisValues) Axes {
	return Axes{
		X: target["x"] - from["x"],
		Y: target["y"] - from["y"],
		Z: target["z"] - from["z"],
	}
}

func targetMoveDuration(delta Axes, feedMMMin float64, cfg Config) time.Duration {
	cfg = cfg.normalize()
	xyDist := math.Hypot(delta.X, delta.Y)
	zDist := math.Abs(delta.Z)
	mins := 0.0
	if xyDist > 0 && feedMMMin > 0 {
		mins = xyDist / feedMMMin
	}
	if zDist > 0 && cfg.MaxZMMMin > 0 {
		zMins := zDist / cfg.MaxZMMMin
		if zMins > mins {
			mins = zMins
		}
	}
	if mins <= 0 {
		return minJogSegment
	}
	d := time.Duration(mins * float64(time.Minute))
	if d < minJogSegment {
		return minJogSegment
	}
	return d
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

func jogCommand(target machine.AxisValues, delta Axes, cfg Config) string {
	return jogCommandForDuration(target, delta, cfg, cfg.normalize().Tick)
}

func jogCommandForDuration(target machine.AxisValues, delta Axes, cfg Config, d time.Duration) string {
	cfg = cfg.normalize()
	if cfg.MotionPrimitive == MotionPrimitiveG53 {
		return g53JogCommand(target, delta)
	}
	return instantJogCommand(delta, cfg, d)
}

func instantJogCommand(delta Axes, cfg Config, d time.Duration) string {
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
	parts += fmt.Sprintf(" F%.4f", jogFeedScale(delta, cfg, d))
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

func jogFeedScale(delta Axes, cfg Config, d time.Duration) float64 {
	cfg = cfg.normalize()
	dist := math.Sqrt(delta.X*delta.X + delta.Y*delta.Y + delta.Z*delta.Z)
	if dist == 0 || d <= 0 {
		return 1
	}
	desiredMMMin := dist / d.Minutes()
	machineMax := selectedJogMachineMax(delta)
	if machineMax <= 0 {
		return 1
	}
	scale := desiredMMMin / machineMax
	if scale < 0.001 {
		return 0.001
	}
	return scale
}

func selectedJogMachineMax(delta Axes) float64 {
	maxRate := 0.0
	if delta.X != 0 || delta.Y != 0 {
		maxRate = firmwareMaxXYMMMin
	}
	if delta.Z != 0 && (maxRate == 0 || firmwareMaxZMMMin < maxRate) {
		maxRate = firmwareMaxZMMMin
	}
	return maxRate
}

func safetyLeadTooLarge(planned, observed machine.AxisValues, cfg Config) bool {
	xy, z := safetyLead(cfg)
	return math.Abs(planned["x"]-observed["x"]) > xy ||
		math.Abs(planned["y"]-observed["y"]) > xy ||
		math.Abs(planned["z"]-observed["z"]) > z
}

func safetyLead(cfg Config) (float64, float64) {
	cfg = cfg.normalize()
	budget := activeStatusMaxAge(cfg)
	if budget > 750*time.Millisecond {
		budget = 750 * time.Millisecond
	}
	xy := cfg.MaxXYMMMin * budget.Minutes()
	if xy < baseMaxXYLeadMM {
		xy = baseMaxXYLeadMM
	}
	z := cfg.MaxZMMMin * budget.Minutes()
	if z < baseMaxZLeadMM {
		z = baseMaxZLeadMM
	}
	return xy, z
}

func jogSegmentDuration(cfg Config) time.Duration {
	cfg = cfg.normalize()
	d := 4 * cfg.Tick
	if d < minJogSegment {
		d = minJogSegment
	}
	if d > maxJogSegment {
		d = maxJogSegment
	}
	return d
}

func jogLookahead(cfg Config) time.Duration {
	d := 2 * jogSegmentDuration(cfg.normalize())
	if d < minJogLookahead {
		d = minJogLookahead
	}
	if d > maxJogLookahead {
		d = maxJogLookahead
	}
	return d
}

func queueLead(now, queuedUntil time.Time) time.Duration {
	if queuedUntil.IsZero() || !queuedUntil.After(now) {
		return 0
	}
	return queuedUntil.Sub(now)
}

func motionInputActive(haveInput bool, in Input, now time.Time, cfg Config) bool {
	cfg = cfg.normalize()
	if !haveInput || !in.Deadman || now.Sub(in.At) > cfg.DeadmanTimeout {
		return false
	}
	return response(in.Axes.X) != 0 || response(in.Axes.Y) != 0 || response(in.Axes.Z) != 0
}

func activeStatusPollInterval(cfg Config) time.Duration {
	cfg = cfg.normalize()
	d := 8 * cfg.StatusInterval
	if d < minActiveStatusGap {
		d = minActiveStatusGap
	}
	if d > maxActiveStatusGap {
		d = maxActiveStatusGap
	}
	return d
}

func activeStatusMaxAge(cfg Config) time.Duration {
	cfg = cfg.normalize()
	maxAge := activeStatusPollInterval(cfg) + statusQueryTimeout(cfg) + 500*time.Millisecond
	if maxAge < cfg.DeadmanTimeout {
		maxAge = cfg.DeadmanTimeout
	}
	return maxAge
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

func appendPlannedSegment(segments []plannedSegment, seg plannedSegment, now time.Time) []plannedSegment {
	segments = trimPlannedSegments(segments, now)
	return append(segments, seg)
}

func trimPlannedSegments(segments []plannedSegment, now time.Time) []plannedSegment {
	keep := 0
	for keep+1 < len(segments) && !segments[keep].end.After(now) {
		keep++
	}
	if keep == 0 {
		return segments
	}
	out := append([]plannedSegment(nil), segments[keep:]...)
	return out
}

func estimateFromSegments(segments []plannedSegment, now time.Time, fallback machine.AxisValues) machine.AxisValues {
	if len(segments) == 0 {
		return copyAxes(fallback)
	}
	for _, seg := range segments {
		if now.Before(seg.start) || seg.end.Equal(seg.start) {
			return copyAxes(seg.from)
		}
		if now.Before(seg.end) || now.Equal(seg.end) {
			t := now.Sub(seg.start).Seconds() / seg.end.Sub(seg.start).Seconds()
			return interpolateAxes(seg.from, seg.to, t)
		}
	}
	return copyAxes(segments[len(segments)-1].to)
}

func interpolateAxes(from, to machine.AxisValues, t float64) machine.AxisValues {
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	out := copyAxes(from)
	if out == nil {
		out = machine.AxisValues{}
	}
	for axis, target := range to {
		out[axis] = out[axis] + (target-out[axis])*t
	}
	return out
}

func estimatedWorkPosition(estimated machine.AxisValues, st machine.Status) machine.AxisValues {
	if len(estimated) == 0 || len(st.MPos) == 0 || len(st.WPos) == 0 {
		return nil
	}
	out := copyAxes(st.WPos)
	if out == nil {
		out = machine.AxisValues{}
	}
	for _, axis := range []string{"x", "y", "z"} {
		m, mok := st.MPos[axis]
		w, wok := st.WPos[axis]
		e, eok := estimated[axis]
		if mok && wok && eok {
			out[axis] = e - (m - w)
		}
	}
	return out
}

func motionEvent(target, observed, estimated machine.AxisValues, delta Axes, cmd string, queuedLead time.Duration, st machine.Status) *MotionEvent {
	if estimated == nil {
		estimated = observed
	}
	leadFrom := estimated
	if leadFrom == nil {
		leadFrom = observed
	}
	return &MotionEvent{
		Target:        copyAxes(target),
		Observed:      copyAxes(observed),
		Estimated:     copyAxes(estimated),
		EstimatedWPos: estimatedWorkPosition(estimated, st),
		Delta:         delta,
		Lead: Axes{
			X: target["x"] - leadFrom["x"],
			Y: target["y"] - leadFrom["y"],
			Z: target["z"] - leadFrom["z"],
		},
		QueueLeadMs: queuedLead.Milliseconds(),
		Command:     cmd,
	}
}

func statusQueryTimeout(cfg Config) time.Duration {
	cfg = cfg.normalize()
	timeout := 2 * cfg.StatusInterval
	if timeout < 100*time.Millisecond {
		timeout = 100 * time.Millisecond
	}
	if timeout > 250*time.Millisecond {
		timeout = 250 * time.Millisecond
	}
	return timeout
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func (s *Session) emitStatusWaiting(now time.Time) {
	s.mu.Lock()
	last := s.lastStatusWait
	if !last.IsZero() && now.Sub(last) < statusWaitGap {
		s.mu.Unlock()
		return
	}
	s.lastStatusWait = now
	s.mu.Unlock()
	s.emit(Event{Type: "error", Code: CodeStatusWaiting, Message: "Waiting for fresh machine status before continuing jog."})
}

func (s *Session) emitMotionEstimate(now time.Time, st machine.Status, target machine.AxisValues, delta Axes, cmd string, queuedLead time.Duration) {
	s.mu.Lock()
	s.segments = trimPlannedSegments(s.segments, now)
	estimated := estimateFromSegments(s.segments, now, target)
	s.mu.Unlock()
	if target == nil {
		target = estimated
	}
	if target == nil && estimated == nil {
		return
	}
	s.emitMotion(now, motionEvent(target, st.MPos, estimated, delta, cmd, queuedLead, st))
}

func (s *Session) emitMotion(now time.Time, ev *MotionEvent) {
	s.mu.Lock()
	last := s.lastMotionEvent
	if !last.IsZero() && now.Sub(last) < motionEventGap {
		s.mu.Unlock()
		return
	}
	s.lastMotionEvent = now
	s.mu.Unlock()
	s.emit(Event{Type: "motion", Motion: ev})
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
	if lease != nil {
		s.leaseID++
	}
	s.armed = false
	s.statusInFlight = false
	s.planned = nil
	s.queuedUntil = time.Time{}
	s.segments = nil
	s.lastMotionCmd = ""
	s.lastMotionEvent = time.Time{}
	s.lastStatusWait = time.Time{}
	s.lastAlarmRaw = ""
	s.lastMotionLog = time.Time{}
	s.mu.Unlock()
	if lease != nil {
		s.statusWG.Wait()
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
