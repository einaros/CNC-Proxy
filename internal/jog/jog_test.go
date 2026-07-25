package jog

import (
	"context"
	"math"
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

func TestNormalizeClampsFastJogCadence(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Tick = 5 * time.Millisecond
	cfg.StatusInterval = 5 * time.Millisecond
	cfg.DeadmanTimeout = 150 * time.Millisecond
	got := cfg.normalize()
	if got.Tick != minJogTick {
		t.Fatalf("tick = %s, want %s", got.Tick, minJogTick)
	}
	if got.StatusInterval != minStatusEvery {
		t.Fatalf("status interval = %s, want %s", got.StatusInterval, minStatusEvery)
	}
	if got.DeadmanTimeout != minDeadmanTimeout {
		t.Fatalf("deadman timeout = %s, want %s", got.DeadmanTimeout, minDeadmanTimeout)
	}
}

func TestStatusQueryTimeoutToleratesTransportLatency(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Tick = 5 * time.Millisecond
	cfg.StatusInterval = 100 * time.Millisecond
	if got := statusQueryTimeout(cfg); got != 200*time.Millisecond {
		t.Fatalf("fast status query timeout = %s, want 200ms", got)
	}

	cfg.Tick = 50 * time.Millisecond
	cfg.StatusInterval = 500 * time.Millisecond
	if got := statusQueryTimeout(cfg); got != 250*time.Millisecond {
		t.Fatalf("capped status query timeout = %s, want 250ms", got)
	}
}

func TestActiveMotionSuppressesStatusPolling(t *testing.T) {
	mgr, _, cleanup := newJogManager(t)
	defer cleanup()
	base := time.Unix(100, 0)
	now := base
	mgr.now = func() time.Time { return now }
	mgr.cfg.Tick = minJogTick
	mgr.cfg.StatusInterval = minStatusEvery
	mgr.cfg.DeadmanTimeout = time.Second

	s, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	drainUntil(t, s, "hello")
	s.Arm(1)
	drainUntil(t, s, "ack")
	s.SetInput(Input{Seq: 2, Deadman: true, Axes: Axes{X: 1}, At: base})

	now = base.Add(mgr.cfg.StatusInterval)
	if s.shouldRequestStatus(now) {
		t.Fatal("active jog should not poll status at the normal status interval")
	}

	now = base.Add(activeStatusPollInterval(mgr.cfg))
	s.mu.Lock()
	s.queuedUntil = now.Add(minJogLookahead / 2)
	s.mu.Unlock()
	if s.shouldRequestStatus(now) {
		t.Fatal("active jog should not poll status while motion input is live")
	}

	s.mu.Lock()
	s.queuedUntil = now.Add(minJogLookahead)
	s.mu.Unlock()
	if s.shouldRequestStatus(now) {
		t.Fatal("active jog should not poll status even when buffered")
	}

	s.SetInput(Input{Seq: 3, Deadman: true, Axes: Axes{}, At: now})
	if !s.shouldRequestStatus(now) {
		t.Fatal("jog should poll status once active axis input stops")
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

func TestActiveJogAvailabilityAllowsOwnRunState(t *testing.T) {
	mgr, _, cleanup := newJogManager(t)
	defer cleanup()

	s, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	drainUntil(t, s, "hello")
	s.Arm(1)
	drainUntil(t, s, "ack")
	if !mgr.arb.Tracker().ObserveStatusPayload("<Run|MPos:0,0,0|WPos:0,0,0>") {
		t.Fatal("status precondition failed")
	}

	s.emitState(2)
	ev := readEvent(t, s, "state")
	if ev.Availability == nil || !ev.Availability.Available {
		t.Fatalf("active jog availability during Run = %+v, want available", ev.Availability)
	}
	if ev.Availability.Message != "Jog session active." {
		t.Fatalf("active jog availability message = %q", ev.Availability.Message)
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
	cfg := DefaultConfig()
	cfg.MotionPrimitive = MotionPrimitiveG53
	got := jogCommand(target, delta, cfg)
	if got != "G53 G0 X1.2500 Z3.7500" {
		t.Fatalf("G53 fallback command = %q", got)
	}
}

func TestInstantJogCommandIncludesVelocityScale(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Tick = 20 * time.Millisecond
	cfg.MaxXYMMMin = 1200
	delta := Normalize(Axes{X: 1}, false, cfg)
	got := jogCommand(machine.AxisValues{"x": 0, "y": 0, "z": 0}, delta, cfg)
	if got != "$J X0.4000 F0.4000" {
		t.Fatalf("instant jog command = %q, want velocity-matched F scale", got)
	}
}

func TestInstantJogCommandAllowsScaleAboveFirmwareDefault(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Tick = 20 * time.Millisecond
	cfg.MaxXYMMMin = 6000
	delta := Normalize(Axes{X: 1}, false, cfg)
	got := jogCommand(machine.AxisValues{"x": 0, "y": 0, "z": 0}, delta, cfg)
	if got != "$J X2.0000 F2.0000" {
		t.Fatalf("instant jog command = %q, want configured speed above firmware default", got)
	}
}

func TestMotionDeltaUsesBufferedSegment(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Tick = 20 * time.Millisecond
	cfg.MaxXYMMMin = 1200
	delta := MotionDelta(Axes{X: 1}, false, cfg)
	if math.Abs(delta.X-1.6) > 0.0001 {
		t.Fatalf("buffered X delta = %.4f, want 1.6000", delta.X)
	}
	got := jogCommandForDuration(machine.AxisValues{"x": delta.X, "y": 0, "z": 0}, delta, cfg, jogSegmentDuration(cfg))
	if got != "$J X1.6000 F0.4000" {
		t.Fatalf("buffered instant jog command = %q, want velocity-matched 80ms segment", got)
	}
}

func TestJogSessionAllowsScaleAboveFirmwareDefault(t *testing.T) {
	mgr, _, cleanup := newJogManager(t)
	defer cleanup()
	mgr.cfg.MaxXYMMMin = 6000

	s, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	drainUntil(t, s, "hello")

	s.Arm(1)
	drainUntil(t, s, "ack")
	s.SetInput(Input{Seq: 2, Deadman: true, Axes: Axes{X: 1}, At: time.Now()})
	ev := drainUntil(t, s, "motion")
	if ev.Motion == nil || !strings.Contains(ev.Motion.Command, " F2.0000") {
		t.Fatalf("motion command = %+v, want F2.0000 scale", ev.Motion)
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

func TestJogTargetUsesFreshStatusWhileStatusPollInFlight(t *testing.T) {
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

	s.mu.Lock()
	s.statusInFlight = true
	s.mu.Unlock()
	s.Target(2, machine.AxisValues{"x": 10, "y": -5}, 600, false, 0)
	ack := drainUntil(t, s, "ack")
	if ack.Seq != 2 {
		t.Fatalf("target ack = %+v, want seq 2", ack)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, line := range fm.Gcodes() {
			if strings.Contains(line, "X10.0000") && strings.Contains(line, "Y-5.0000") {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no target jog command observed: %v", fm.Gcodes())
}

func TestJogTargetRemainsExclusiveUntilObservedAtTarget(t *testing.T) {
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

	s.Target(2, machine.AxisValues{"x": 10, "y": -5}, 600, false, 0)
	ack := drainUntil(t, s, "ack")
	if ack.Seq != 2 {
		t.Fatalf("target ack = %+v, want seq 2", ack)
	}
	deadline := time.Now().Add(time.Second)
	for len(fm.Gcodes()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(fm.Gcodes()) == 0 {
		t.Fatal("first target did not reach fake machine")
	}
	before := len(fm.Gcodes())
	s.SetInput(Input{Seq: 3, Deadman: true, Axes: Axes{X: 1}, At: time.Now()})
	s.motionTick()
	if got := len(fm.Gcodes()); got != before {
		t.Fatalf("continuous input wrote %d commands while target was pending, want %d", got, before)
	}

	s.Target(4, machine.AxisValues{"x": 20, "y": 5}, 600, false, 0)
	busy := readEvent(t, s, "error")
	if busy.Seq != 4 || busy.Code != CodeBusy {
		t.Fatalf("second target event = %+v, want busy error for seq 4", busy)
	}
	if got := len(fm.Gcodes()); got != before {
		t.Fatalf("second target wrote %d commands while first target was pending, want %d", got, before)
	}

	status := "<Idle|MPos:10,-5,0|WPos:10,-5,0>"
	fm.SetStatus(status)
	if err := s.applyStatusPayload(status); err != nil {
		t.Fatal(err)
	}
	complete := drainUntil(t, s, "target_complete")
	if complete.Seq != 2 || complete.Target["x"] != 10 || complete.Target["y"] != -5 {
		t.Fatalf("target completion = %+v, want observed target for seq 2", complete)
	}

	s.Target(5, machine.AxisValues{"x": 20, "y": 5}, 600, false, 0)
	ack = drainUntil(t, s, "ack")
	if ack.Seq != 5 {
		t.Fatalf("target ack after observed completion = %+v, want seq 5", ack)
	}
}

func TestJogTargetThatStopsShortTerminatesAndAllowsNextTarget(t *testing.T) {
	mgr, _, cleanup := newJogManager(t)
	defer cleanup()
	base := time.Now()
	now := base
	mgr.now = func() time.Time { return now }
	mgr.cfg.StatusInterval = time.Hour

	s, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	drainUntil(t, s, "hello")
	s.Arm(1)
	drainUntil(t, s, "ack")

	s.Target(2, machine.AxisValues{"x": 10, "y": -5}, 600, false, 0)
	ack := drainUntil(t, s, "ack")
	if ack.Seq != 2 {
		t.Fatalf("target ack = %+v, want seq 2", ack)
	}
	s.mu.Lock()
	verifyAfter := s.targetPending.verifyAfter
	s.mu.Unlock()

	now = verifyAfter.Add(time.Millisecond)
	status := "<Idle|MPos:9.5,-5,0|WPos:9.5,-5,0>"
	if err := s.applyStatusPayload(status); err != nil {
		t.Fatal(err)
	}
	failed := readEvent(t, s, "error")
	if failed.Seq != 2 || failed.Code != CodeTargetNotReached {
		t.Fatalf("short target result = %+v, want terminal target_not_reached for seq 2", failed)
	}
	s.mu.Lock()
	pending := s.targetPending
	s.mu.Unlock()
	if pending != nil {
		t.Fatalf("short target remained pending: %+v", pending)
	}

	s.Target(3, machine.AxisValues{"x": 8, "y": -4}, 600, false, 0)
	ack = drainUntil(t, s, "ack")
	if ack.Seq != 3 {
		t.Fatalf("next target ack = %+v, want seq 3", ack)
	}
}

func TestJogTargetWithoutFreshCompletionStatusDisarms(t *testing.T) {
	mgr, _, cleanup := newJogManager(t)
	defer cleanup()
	base := time.Now()
	now := base
	mgr.now = func() time.Time { return now }
	mgr.cfg.StatusInterval = time.Hour

	s, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	drainUntil(t, s, "hello")
	s.Arm(1)
	drainUntil(t, s, "ack")

	s.Target(2, machine.AxisValues{"x": 10, "y": -5}, 600, false, 0)
	ack := drainUntil(t, s, "ack")
	if ack.Seq != 2 {
		t.Fatalf("target ack = %+v, want seq 2", ack)
	}
	s.mu.Lock()
	verifyAfter := s.targetPending.verifyAfter
	s.lastStatusAt = s.targetPending.motionDoneAt.Add(-time.Millisecond)
	s.statusInFlight = false
	s.mu.Unlock()

	now = verifyAfter.Add(statusQueryTimeout(mgr.cfg) + time.Millisecond)
	s.motionTick()
	failed := readEvent(t, s, "error")
	if failed.Seq != 2 || failed.Code != CodeStaleStatus {
		t.Fatalf("unverified target result = %+v, want terminal stale_status for seq 2", failed)
	}
	s.mu.Lock()
	pending := s.targetPending
	armed := s.armed
	lease := s.lease
	s.mu.Unlock()
	if pending != nil || armed || lease != nil {
		t.Fatalf("unverified target state pending=%+v armed=%t lease=%v, want fully disarmed", pending, armed, lease)
	}
}

func TestJogTargetWaitsForQueuedManualMotion(t *testing.T) {
	mgr, _, cleanup := newJogManager(t)
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
	drainUntil(t, s, "motion")
	s.Target(3, machine.AxisValues{"x": 10, "y": -5}, 600, false, 0)
	busy := readEvent(t, s, "error")
	if busy.Seq != 3 || busy.Code != CodeBusy {
		t.Fatalf("target during queued manual motion = %+v, want busy error for seq 3", busy)
	}
}

func TestJogTargetMovesToSafeZBeforeXY(t *testing.T) {
	mgr, fm, cleanup := newJogManager(t)
	defer cleanup()
	status := "<Idle|MPos:0,0,-5|WPos:0,0,-5>"
	fm.SetStatus(status)
	if !mgr.arb.Tracker().ObserveStatusPayload(status) {
		t.Fatal("failed to seed tracker status")
	}

	s, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	drainUntil(t, s, "hello")
	s.Arm(1)
	drainUntil(t, s, "ack")

	s.Target(2, machine.AxisValues{"x": 10, "y": -5}, 600, true, 0)
	ack := drainUntil(t, s, "ack")
	if ack.Seq != 2 {
		t.Fatalf("target ack = %+v, want seq 2", ack)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		gcodes := fm.Gcodes()
		if len(gcodes) >= 2 {
			if !strings.Contains(gcodes[0], "Z5.0000") {
				t.Fatalf("first target command = %q, want safe Z lift; all=%v", gcodes[0], gcodes)
			}
			if !strings.Contains(gcodes[1], "X10.0000") || !strings.Contains(gcodes[1], "Y-5.0000") {
				t.Fatalf("second target command = %q, want XY target; all=%v", gcodes[1], gcodes)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("safe Z target did not emit two jog commands: %v", fm.Gcodes())
}

func TestJogTargetWithUnchangedXYDoesNotLiftToSafeZ(t *testing.T) {
	mgr, fm, cleanup := newJogManager(t)
	defer cleanup()
	status := "<Idle|MPos:0,0,-5|WPos:0,0,-5>"
	fm.SetStatus(status)
	if !mgr.arb.Tracker().ObserveStatusPayload(status) {
		t.Fatal("failed to seed tracker status")
	}

	s, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	drainUntil(t, s, "hello")
	s.Arm(1)
	drainUntil(t, s, "ack")

	// Work-coordinate inputs include the live X/Y values. They are targets but
	// not an XY move, so a Z-only request must not take a Safe Z detour.
	s.Target(2, machine.AxisValues{"x": 0, "y": 0, "z": -2}, 600, true, 0)
	ack := drainUntil(t, s, "ack")
	if ack.Seq != 2 {
		t.Fatalf("target ack = %+v, want seq 2", ack)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		gcodes := fm.Gcodes()
		if len(gcodes) == 0 {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if len(gcodes) != 1 || !strings.Contains(gcodes[0], "Z3.0000") || strings.Contains(gcodes[0], "Z5.0000") {
			t.Fatalf("Z-only target gcodes = %v, want one direct Z move", gcodes)
		}
		return
	}
	t.Fatalf("Z-only target did not reach fake machine: %v", fm.Gcodes())
}

func TestJogTargetFeedIsNotCappedByContinuousJogLimit(t *testing.T) {
	mgr, fm, cleanup := newJogManager(t)
	defer cleanup()
	mgr.cfg.MaxXYMMMin = 1200

	s, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	drainUntil(t, s, "hello")
	s.Arm(1)
	drainUntil(t, s, "ack")

	s.Target(2, machine.AxisValues{"x": 10}, 3000, false, 0)
	ack := drainUntil(t, s, "ack")
	if ack.Seq != 2 {
		t.Fatalf("target ack = %+v, want seq 2", ack)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, line := range fm.Gcodes() {
			if strings.Contains(line, "X10.0000") {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("high-feed target did not reach fake machine: %v", fm.Gcodes())
}

func TestJogTargetMovesXYZWithSafeZSequence(t *testing.T) {
	mgr, fm, cleanup := newJogManager(t)
	defer cleanup()
	status := "<Idle|MPos:0,0,-5|WPos:0,0,-5>"
	fm.SetStatus(status)
	if !mgr.arb.Tracker().ObserveStatusPayload(status) {
		t.Fatal("failed to seed tracker status")
	}

	s, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	drainUntil(t, s, "hello")
	s.Arm(1)
	drainUntil(t, s, "ack")

	s.Target(2, machine.AxisValues{"x": 10, "y": -5, "z": -2}, 600, true, 0)
	ack := drainUntil(t, s, "ack")
	if ack.Seq != 2 {
		t.Fatalf("target ack = %+v, want seq 2", ack)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		gcodes := fm.Gcodes()
		if len(gcodes) >= 3 {
			if !strings.Contains(gcodes[0], "Z5.0000") {
				t.Fatalf("first target command = %q, want safe Z lift; all=%v", gcodes[0], gcodes)
			}
			if !strings.Contains(gcodes[1], "X10.0000") || !strings.Contains(gcodes[1], "Y-5.0000") || strings.Contains(gcodes[1], "Z") {
				t.Fatalf("second target command = %q, want XY only; all=%v", gcodes[1], gcodes)
			}
			if !strings.Contains(gcodes[2], "Z-2.0000") {
				t.Fatalf("third target command = %q, want final Z; all=%v", gcodes[2], gcodes)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("safe XYZ target did not emit three jog commands: %v", fm.Gcodes())
}

func TestJogSetOriginSendsControllerCommand(t *testing.T) {
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

	s.SetOrigin(2, "z", 0)
	ack := drainUntil(t, s, "ack")
	if ack.Seq != 2 {
		t.Fatalf("origin ack = %+v, want seq 2", ack)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, line := range fm.Gcodes() {
			if line == "G10L20P0Z0.0000" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no origin command observed: %v", fm.Gcodes())
}

func TestJogSetMachineOriginUsesOneVendorG10L2Command(t *testing.T) {
	mgr, fm, cleanup := newJogManager(t)
	defer cleanup()
	status := "<Idle|MPos:-100,-80,-3|WPos:0,0,0>"
	fm.SetStatus(status)
	if !mgr.arb.Tracker().ObserveStatusPayload(status) {
		t.Fatal("failed to seed tracker status")
	}

	s, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	drainUntil(t, s, "hello")
	s.Arm(1)
	drainUntil(t, s, "ack")

	s.SetMachineOrigin(2, machine.AxisValues{"x": -287.51, "y": -202.11})
	ack := drainUntil(t, s, "ack")
	if ack.Seq != 2 {
		t.Fatalf("origin ack = %+v, want seq 2", ack)
	}
	if math.Abs(ack.Target["x"]-187.51) > 1e-9 || math.Abs(ack.Target["y"]-122.11) > 1e-9 {
		t.Fatalf("origin verification target = %+v", ack.Target)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, line := range fm.Gcodes() {
			if line == "G10L2P0X-287.5100Y-202.1100" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no machine-origin command observed: %v", fm.Gcodes())
}

func TestJogFastTickDoesNotFloodMotionEvents(t *testing.T) {
	mgr, fm, cleanup := newJogManager(t)
	defer cleanup()
	base := time.Unix(100, 0)
	now := base
	mgr.now = func() time.Time { return now }
	mgr.cfg.Tick = minJogTick
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
	s.SetInput(Input{Seq: 2, Deadman: true, Axes: Axes{X: 1}, At: base})

	for i := 0; i < 20; i++ {
		now = base.Add(time.Duration(i) * mgr.cfg.Tick)
		s.motionTick()
	}
	var got int
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		got = len(fm.Gcodes())
		if got >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got < 3 {
		t.Fatalf("buffered jog should maintain queued motion, got %d commands: %v", got, fm.Gcodes())
	}
	s.mu.Lock()
	lead := queueLead(now, s.queuedUntil)
	s.mu.Unlock()
	if lead < minJogLookahead || lead > maxJogLookahead+jogSegmentDuration(mgr.cfg) {
		t.Fatalf("queued lead = %s, want bounded lookahead", lead)
	}
	motions := drainAvailableEvents(s, "motion")
	if motions == 0 {
		t.Fatal("expected at least one motion event")
	}
	if motions > 4 {
		t.Fatalf("motion events should be UI-rate limited, got %d events across 20 fast ticks", motions)
	}
}

func TestContinuousJogDoesNotPauseForCorrectionStatusAge(t *testing.T) {
	mgr, fm, cleanup := newJogManager(t)
	defer cleanup()
	base := time.Unix(100, 0)
	now := base
	mgr.now = func() time.Time { return now }
	mgr.cfg.Tick = time.Hour
	mgr.cfg.StatusInterval = time.Hour
	mgr.cfg.DeadmanTimeout = 5 * time.Second

	s, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	drainUntil(t, s, "hello")
	s.Arm(1)
	drainUntil(t, s, "ack")
	drainAvailableEvents(s, "state")
	s.SetInput(Input{Seq: 2, Deadman: true, Axes: Axes{X: 1}, At: base})

	end := base.Add(activeStatusMaxAge(mgr.cfg) + 250*time.Millisecond)
	for !now.After(end) {
		s.motionTick()
		now = now.Add(minJogTick)
	}
	if got := len(fm.Gcodes()); got < 10 {
		t.Fatalf("continuous jog paused after correction status aged out; got %d commands: %v", got, fm.Gcodes())
	}
	if !s.hasLease() {
		t.Fatal("continuous jog should keep the lease while correction status is stale")
	}
	failOnAvailableErrors(t, s)
}

func TestJogEmitsEstimatedMotionFromQueuedSegments(t *testing.T) {
	mgr, _, cleanup := newJogManager(t)
	defer cleanup()
	base := time.Unix(100, 0)
	now := base
	mgr.now = func() time.Time { return now }
	mgr.cfg.Tick = minJogTick
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
	drainAvailableEvents(s, "state")
	s.SetInput(Input{Seq: 2, Deadman: true, Axes: Axes{X: 1}, At: base})

	s.motionTick()
	drainAvailableEvents(s, "motion")
	now = base.Add(40 * time.Millisecond)
	s.motionTick()
	ev := readEvent(t, s, "motion")
	if ev.Motion == nil {
		t.Fatal("missing motion event")
	}
	est := ev.Motion.Estimated
	if est == nil {
		t.Fatalf("motion event missing estimated position: %+v", ev.Motion)
	}
	if est["x"] <= 0 || est["x"] >= ev.Motion.Target["x"] {
		t.Fatalf("estimated X = %.4f, target X = %.4f; want interpolated position ahead of observed and behind target", est["x"], ev.Motion.Target["x"])
	}
	if ev.Motion.EstimatedWPos == nil || math.Abs(ev.Motion.EstimatedWPos["x"]-est["x"]) > 0.0001 {
		t.Fatalf("estimated WPos = %+v, estimated MPos = %+v", ev.Motion.EstimatedWPos, est)
	}
	if ev.Motion.QueueLeadMs <= 0 {
		t.Fatalf("queue lead = %dms, want positive queued motion", ev.Motion.QueueLeadMs)
	}
}

func TestJogAsyncStatusDoesNotBlockMotionTicks(t *testing.T) {
	mgr, fm, cleanup := newJogManager(t)
	defer cleanup()
	base := time.Unix(100, 0)
	now := base
	mgr.now = func() time.Time { return now }
	mgr.cfg.Tick = minJogTick
	mgr.cfg.StatusInterval = minStatusEvery
	mgr.cfg.DeadmanTimeout = time.Second
	fm.SetStatusReplyDelay(250 * time.Millisecond)

	s, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	drainUntil(t, s, "hello")
	s.Arm(1)
	drainUntil(t, s, "ack")
	s.requestStatus()
	s.SetInput(Input{Seq: 2, Deadman: true, Axes: Axes{X: 1}, At: base})

	for i := 0; i < 8; i++ {
		now = base.Add(time.Duration(i) * mgr.cfg.Tick)
		s.motionTick()
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := len(fm.Gcodes()); got >= 3 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("delayed status should not block motion ticks; gcodes=%v", fm.Gcodes())
}

func TestJogStatusTimeoutPausesWithoutDisarming(t *testing.T) {
	mgr, fm, cleanup := newJogManager(t)
	defer cleanup()
	mgr.cfg.Tick = time.Hour
	mgr.cfg.StatusInterval = 20 * time.Millisecond
	mgr.cfg.DeadmanTimeout = 150 * time.Millisecond

	s, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	drainUntil(t, s, "hello")
	s.Arm(1)
	drainUntil(t, s, "ack")
	drainAvailableEvents(s, "state")
	if !s.hasLease() {
		t.Fatal("expected armed lease")
	}

	fm.SetStatusReplyDelay(500 * time.Millisecond)
	if err := s.refreshStatus(); err != nil {
		t.Fatalf("status timeout during jog should be retryable, got %v", err)
	}
	ev := readEvent(t, s, "error")
	if ev.Code != CodeStatusWaiting {
		t.Fatalf("status timeout event = %+v, want status_waiting", ev)
	}
	if !s.hasLease() {
		t.Fatal("status timeout during active jog should not release the lease")
	}
}

func TestJogStaleStatusPausesMotionWithoutDisarming(t *testing.T) {
	mgr, fm, cleanup := newJogManager(t)
	defer cleanup()
	base := time.Unix(100, 0)
	now := base
	mgr.now = func() time.Time { return now }
	mgr.cfg.Tick = 5 * time.Millisecond
	mgr.cfg.StatusInterval = 20 * time.Millisecond
	mgr.cfg.DeadmanTimeout = 150 * time.Millisecond

	s, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	drainUntil(t, s, "hello")
	s.Arm(1)
	drainUntil(t, s, "ack")
	drainAvailableEvents(s, "state")
	s.SetInput(Input{Seq: 2, Deadman: true, Axes: Axes{X: 1}, At: base})

	now = base.Add(activeStatusMaxAge(mgr.cfg) + time.Millisecond)
	s.SetInput(Input{Seq: 3, Deadman: true, Axes: Axes{X: 1}, At: now})
	s.motionTick()
	ev := readEvent(t, s, "error")
	if ev.Code != CodeStatusWaiting {
		t.Fatalf("stale status event = %+v, want status_waiting", ev)
	}
	if !s.hasLease() {
		t.Fatal("stale status should pause motion, not release the jog lease")
	}
	if got := fm.Gcodes(); len(got) != 0 {
		t.Fatalf("stale status should not emit motion, got %v", got)
	}
}

func TestJogLogsAlarmWithLastMotion(t *testing.T) {
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
	s.SetInput(Input{Seq: 2, Deadman: true, Axes: Axes{X: 1}, At: time.Now()})
	drainUntil(t, s, "motion")

	if err := s.applyStatusPayload("<Alarm|MPos:0.1,0,0|WPos:0.1,0,0|H:10>"); err != nil {
		t.Fatal(err)
	}
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

func TestJogPausesLeaseOnStaleStatus(t *testing.T) {
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
	if ev.Code != CodeStatusWaiting {
		t.Fatalf("error event = %+v, want status waiting", ev)
	}
	if !s.hasLease() {
		t.Fatal("stale status should pause motion without releasing the jog lease")
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

func TestJogKeepsLeaseDuringOwnRunStatus(t *testing.T) {
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

	fm.SetStatus("<Run|MPos:0,0,0|WPos:0,0,0>")
	if err := s.refreshStatus(); err != nil {
		t.Fatalf("refresh Run status: %v", err)
	}
	ev := readEvent(t, s, "status")
	if ev.Status == nil || ev.Status.State != machine.Run {
		t.Fatalf("status event = %+v, want Run", ev)
	}
	if !s.hasLease() {
		t.Fatal("Run during active jog should not release the jog lease")
	}

	now := time.Now()
	s.SetInput(Input{Seq: 2, Deadman: true, Axes: Axes{X: 1}, At: now})
	s.motionTick()
	drainUntil(t, s, "motion")
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

func drainAvailableEvents(s *Session, typ string) int {
	n := 0
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				return n
			}
			if ev.Type == typ {
				n++
			}
		default:
			return n
		}
	}
}

func failOnAvailableErrors(t *testing.T, s *Session) {
	t.Helper()
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				return
			}
			if ev.Type == "error" {
				t.Fatalf("unexpected jog error: %+v", ev)
			}
		default:
			return
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
