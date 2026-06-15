package jog

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/uwin/cnc-proxy/internal/carveratest"
	"github.com/uwin/cnc-proxy/internal/client"
	"github.com/uwin/cnc-proxy/internal/gcodelog"
	"github.com/uwin/cnc-proxy/internal/machine"
	"github.com/uwin/cnc-proxy/internal/protocol"
	"github.com/uwin/cnc-proxy/internal/relay"
	"github.com/uwin/cnc-proxy/internal/session"
)

func TestNormalizeDeadzoneClampAndSlow(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Tick = 50 * time.Millisecond
	got := Normalize(Axes{X: 0.10, Y: 2, Z: -1}, true, cfg)
	if got.X != 0 {
		t.Fatalf("deadzone X = %v, want 0", got.X)
	}
	if got.Y <= 0 || got.Z >= 0 {
		t.Fatalf("unexpected directions: %+v", got)
	}
	if got.Y > cfg.MaxXYMMMin*cfg.Tick.Minutes()*slowScale {
		t.Fatalf("Y exceeds slow max: %+v", got)
	}
	if -got.Z > cfg.MaxZMMMin*cfg.Tick.Minutes()*slowScale {
		t.Fatalf("Z exceeds slow max: %+v", got)
	}
}

func TestManagerSingleActiveSession(t *testing.T) {
	mgr, _, cleanup := newJogManager(t)
	defer cleanup()

	s1, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	if _, err := mgr.Start(context.Background()); err == nil {
		t.Fatal("second session should be rejected")
	}
}

func TestActiveSessionAvailabilityIgnoresItself(t *testing.T) {
	mgr, _, cleanup := newJogManager(t)
	defer cleanup()

	s, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	hello := drainUntil(t, s, "hello")
	if hello.Capabilities == nil || !hello.Capabilities.Availability.Available {
		t.Fatalf("active session hello availability = %+v, want available", hello.Capabilities)
	}
	if ext := mgr.Availability(); ext.Available || ext.Reason != CodeBusy {
		t.Fatalf("external availability during active session = %+v, want busy", ext)
	}

	s.emitState(1)
	ev := readEvent(t, s, "state")
	if ev.Availability == nil || !ev.Availability.Available || ev.Availability.Reason == CodeBusy {
		t.Fatalf("active session state availability = %+v, want available and not busy", ev.Availability)
	}
}

func TestJogOwnerEmitsInstantSegments(t *testing.T) {
	mgr, fm, cleanup := newJogManager(t)
	defer cleanup()

	s, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	drainUntil(t, s, "hello")

	s.Arm(1)
	drainUntil(t, s, "ack")
	s.SetInput(Input{Seq: 2, Deadman: true, Axes: Axes{X: 1}, At: time.Now()})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, line := range fm.Gcodes() {
			if strings.HasPrefix(line, "$J X") && !strings.Contains(line, "G91") {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no instant jog command observed; gcodes=%v", fm.Gcodes())
}

func TestJogG53FallbackCommand(t *testing.T) {
	target := machine.AxisValues{"x": 1.25, "y": -2.5, "z": 3.75}
	delta := Axes{X: 0.5, Z: -0.25}
	got := jogCommand(target, delta, MotionPrimitiveG53)
	if got != "G53 G0 X1.2500 Z3.7500" {
		t.Fatalf("G53 fallback command = %q", got)
	}
}

func TestJogEmitsMotionEventAndLog(t *testing.T) {
	mgr, _, cleanup := newJogManager(t)
	defer cleanup()
	log := gcodelog.New(20)
	mgr.cfg.Log = log

	s, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	drainUntil(t, s, "hello")

	s.Arm(1)
	drainUntil(t, s, "ack")
	s.SetInput(Input{Seq: 2, Deadman: true, Axes: Axes{X: 1, Y: -0.5}, At: time.Now()})
	ev := drainUntil(t, s, "motion")
	if ev.Motion == nil || !strings.HasPrefix(ev.Motion.Command, "$J") || ev.Motion.Target["x"] == 0 {
		t.Fatalf("motion event = %+v", ev.Motion)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, ln := range log.Recent() {
			if ln.Source == gcodelog.SourceJog && ln.Dir == gcodelog.DirSend && strings.HasPrefix(ln.Text, "$J") {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("jog motion was not logged: %+v", log.Recent())
}

func TestJogLogsAlarmWithLastMotion(t *testing.T) {
	mgr, fm, cleanup := newJogManager(t)
	defer cleanup()
	log := gcodelog.New(20)
	mgr.cfg.Log = log

	s, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	drainUntil(t, s, "hello")

	s.Arm(1)
	drainUntil(t, s, "ack")
	s.SetInput(Input{Seq: 2, Deadman: true, Axes: Axes{X: 1}, At: time.Now()})
	drainUntil(t, s, "motion")

	fm.SetStatus("<Alarm|MPos:0.1,0,0|WPos:0.1,0,0|H:10>")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, ln := range log.Recent() {
			if ln.Source == gcodelog.SourceJog && ln.Dir == gcodelog.DirRecv &&
				strings.Contains(ln.Text, "alarm status:") &&
				strings.Contains(ln.Text, "H:10 Soft limit triggered") &&
				strings.Contains(ln.Text, "after $J") {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("jog alarm diagnostic was not logged: %+v", log.Recent())
}

func TestJogRequiresDeadman(t *testing.T) {
	mgr, fm, cleanup := newJogManager(t)
	defer cleanup()

	s, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	drainUntil(t, s, "hello")
	s.Arm(1)
	drainUntil(t, s, "ack")
	s.SetInput(Input{Seq: 2, Deadman: false, Axes: Axes{X: 1}, At: time.Now()})
	time.Sleep(150 * time.Millisecond)
	if got := fm.Gcodes(); len(got) != 0 {
		t.Fatalf("motion emitted without deadman: %v", got)
	}
}

func TestJogControlBypassesLease(t *testing.T) {
	mgr, fm, cleanup := newJogManager(t)
	defer cleanup()

	s, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	drainUntil(t, s, "hello")
	s.Arm(1)
	drainUntil(t, s, "ack")
	s.Control(2, "halt")
	drainUntil(t, s, "ack")

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := fm.Controls(); len(got) == 1 && got[0] == 0x18 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("halt did not reach fake machine; controls=%v", fm.Controls())
}

func TestJogReleasesLeaseOnStaleStatus(t *testing.T) {
	mgr, _, cleanup := newJogManager(t)
	defer cleanup()
	base := time.Unix(100, 0)
	mgr.cfg.Tick = time.Hour
	mgr.cfg.StatusInterval = 100 * time.Millisecond
	mgr.cfg.DeadmanTimeout = time.Second
	mgr.now = func() time.Time { return base }

	s, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	drainUntil(t, s, "hello")
	s.Arm(1)
	drainUntil(t, s, "ack")
	if !s.hasLease() {
		t.Fatal("expected armed lease")
	}

	now := base.Add(2 * time.Second)
	mgr.now = func() time.Time { return now }
	s.SetInput(Input{Seq: 2, Deadman: true, Axes: Axes{X: 1}, At: now})
	s.motionTick()

	ev := readEvent(t, s, "error")
	if ev.Code != CodeStaleStatus {
		t.Fatalf("error event = %+v, want stale status", ev)
	}
	if s.hasLease() {
		t.Fatal("stale status should release the jog lease")
	}
}

func TestJogReleasesLeaseOnAlarmStatusWithoutDeadman(t *testing.T) {
	mgr, fm, cleanup := newJogManager(t)
	defer cleanup()
	mgr.cfg.Tick = time.Hour
	mgr.cfg.StatusInterval = time.Hour
	mgr.cfg.DeadmanTimeout = time.Second

	s, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	drainUntil(t, s, "hello")
	s.Arm(1)
	drainUntil(t, s, "ack")
	if !s.hasLease() {
		t.Fatal("expected armed lease")
	}

	fm.SetStatus("<Alarm|MPos:0,0,0|WPos:0,0,0|H:10>")
	if err := s.refreshStatus(); err != nil {
		t.Fatalf("refresh alarm status: %v", err)
	}
	ev := readEvent(t, s, "error")
	if ev.Code != CodeNotIdle {
		t.Fatalf("error event = %+v, want not_idle", ev)
	}
	if s.hasLease() {
		t.Fatal("alarm status without deadman should release the jog lease")
	}
}

func TestJogRelayIdleController(t *testing.T) {
	fm, err := carveratest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer fm.Close()
	status := "<Idle|MPos:0,0,0|WPos:0,0,0>"
	fm.SetStatus(status)
	tr := machine.NewTracker()
	if !tr.ObserveStatusPayload(status) {
		t.Fatal("status precondition failed")
	}
	arb := session.New(session.Config{
		Tracker:     tr,
		StateMaxAge: time.Second,
		Dial:        func() (*client.Conn, error) { return client.Dial(fm.Addr(), 2*time.Second) },
	})
	rs := &relay.Server{
		Dial:     func() (string, error) { return fm.Addr(), nil },
		Observer: arb,
	}
	arb.SetInjector(relayAdapter{rs})
	arb.SetControlWriter(rs)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go rs.Serve(ln)

	controller, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	waitMode(t, arb, session.ModeRelay)
	controller.Write(protocol.QueryStatus())
	readStatusFrame(t, controller)

	cfg := DefaultConfig()
	cfg.Tick = 20 * time.Millisecond
	cfg.StatusInterval = 40 * time.Millisecond
	cfg.DeadmanTimeout = 120 * time.Millisecond
	mgr := New(arb, cfg)
	s, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	drainUntil(t, s, "hello")
	s.Arm(1)
	drainUntil(t, s, "ack")

	controller.Write(protocol.QueryStatus())
	readStatusFrame(t, controller)

	s.SetInput(Input{Seq: 2, Deadman: true, Axes: Axes{X: 1}, At: time.Now()})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, line := range fm.Gcodes() {
			if strings.HasPrefix(line, "$J X") {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no relay instant jog command observed; gcodes=%v", fm.Gcodes())
}

func newJogManager(t *testing.T) (*Manager, *carveratest.FakeMachine, func()) {
	t.Helper()
	fm, err := carveratest.New()
	if err != nil {
		t.Fatal(err)
	}
	status := "<Idle|MPos:0,0,0|WPos:0,0,0|F:0,0,100|S:0,0,100>"
	fm.SetStatus(status)
	tr := machine.NewTracker()
	if !tr.ObserveStatusPayload(status) {
		t.Fatal("status precondition failed")
	}
	arb := session.New(session.Config{
		Tracker:     tr,
		StateMaxAge: time.Second,
		Dial:        func() (*client.Conn, error) { return client.Dial(fm.Addr(), 2*time.Second) },
	})
	cfg := DefaultConfig()
	cfg.Tick = 20 * time.Millisecond
	cfg.StatusInterval = 40 * time.Millisecond
	cfg.DeadmanTimeout = 120 * time.Millisecond
	mgr := New(arb, cfg)
	return mgr, fm, fm.Close
}

func drainUntil(t *testing.T, s *Session, typ string) Event {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatalf("events closed before %q", typ)
			}
			if ev.Type == "error" {
				t.Fatalf("unexpected jog error waiting for %q: %+v", typ, ev)
			}
			if ev.Type == typ {
				return ev
			}
		case <-deadline:
			t.Fatalf("timeout waiting for event %q", typ)
		}
	}
}

func readEvent(t *testing.T, s *Session, typ string) Event {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatalf("events closed before %q", typ)
			}
			if ev.Type == typ {
				return ev
			}
		case <-deadline:
			t.Fatalf("timeout waiting for event %q", typ)
		}
	}
}

type relayAdapter struct{ srv *relay.Server }

func (a relayAdapter) AcquireMachine() (session.InjectTransport, func(), error) {
	it, release, err := a.srv.AcquireMachine()
	if err != nil {
		return nil, nil, err
	}
	return it, release, nil
}

func (a relayAdapter) AcquireInteractive() (session.InjectTransport, <-chan struct{}, func(), error) {
	it, abort, release, err := a.srv.AcquireInteractive()
	if err != nil {
		return nil, nil, nil, err
	}
	return it, abort, release, nil
}

func waitMode(t *testing.T, arb *session.Arbiter, want session.Mode) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if arb.Mode() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("mode = %s, want %s", arb.Mode(), want)
}

func readStatusFrame(t *testing.T, c net.Conn) protocol.Frame {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	var scan protocol.Scanner
	buf := make([]byte, 1024)
	for {
		n, err := c.Read(buf)
		for _, f := range scan.Push(buf[:n]) {
			if f.Cmd == protocol.CmdStatusRes {
				return f
			}
		}
		if err != nil {
			t.Fatalf("read status frame: %v", err)
		}
	}
}
