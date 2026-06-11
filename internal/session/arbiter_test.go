package session

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/uwin/cnc-proxy/internal/client"
	"github.com/uwin/cnc-proxy/internal/machine"
	"github.com/uwin/cnc-proxy/internal/protocol"
	"github.com/uwin/cnc-proxy/internal/relay"
)

// relayBusy returns the relay's busy error, which the arbiter maps to ErrBusy.
func relayBusy() error { return relay.ErrBusy }

// miniMachine answers "?" with a configurable status so the arbiter's idle
// gating can be exercised without the full fake machine.
func miniMachine(t *testing.T, status func() string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var scan protocol.Scanner
				buf := make([]byte, 4096)
				for {
					c.SetReadDeadline(time.Now().Add(2 * time.Second))
					n, err := c.Read(buf)
					for _, f := range scan.Push(buf[:n]) {
						if f.Cmd == protocol.CmdCtrlSingle && len(f.Data) == 1 && f.Data[0] == '?' {
							c.Write(protocol.Encode(protocol.CmdStatusRes, []byte(status())))
						}
					}
					if err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String()
}

func newArbiter(t *testing.T, addr string, tr *machine.Tracker) *Arbiter {
	return New(Config{
		Tracker: tr,
		Dial:    func() (*client.Conn, error) { return client.Dial(addr, time.Second) },
	})
}

func TestWithMachineBlockedInRelay(t *testing.T) {
	addr := miniMachine(t, func() string { return "<Idle>" })
	a := newArbiter(t, addr, machine.NewTracker())
	a.EnterRelay()

	err := a.WithMachine(false, func(*client.Conn) error { return nil })
	if !errors.Is(err, ErrRelayActive) {
		t.Errorf("got %v, want ErrRelayActive", err)
	}

	a.ExitRelay()
	if err := a.WithMachine(false, func(*client.Conn) error { return nil }); err != nil {
		t.Errorf("owner mode should allow access: %v", err)
	}
}

// fakeInjector implements Injector with a TCP pipe to a mini machine, plus an
// optional forced error to exercise busy/no-session mapping.
type fakeInjector struct {
	addr     string
	forceErr error
	acquired int
}

func (f *fakeInjector) AcquireMachine() (InjectTransport, func(), error) {
	if f.forceErr != nil {
		return nil, nil, f.forceErr
	}
	f.acquired++
	c, err := net.Dial("tcp", f.addr)
	if err != nil {
		return nil, nil, err
	}
	return c, func() { c.Close() }, nil
}

func TestWithMachineInjectsDuringRelay(t *testing.T) {
	addr := miniMachine(t, func() string { return "<Idle>" })
	tr := machine.NewTracker()
	tr.Observe(machine.Idle)
	inj := &fakeInjector{addr: addr}
	a := New(Config{
		Tracker:  tr,
		Dial:     func() (*client.Conn, error) { return client.Dial(addr, time.Second) },
		Injector: inj,
	})
	a.EnterRelay()

	// In relay mode with an injector, WithMachine should route through it.
	ran := false
	err := a.WithMachine(false, func(c *client.Conn) error {
		ran = true
		// Exercise the borrowed connection: a status query round-trips.
		_, e := c.QueryState(time.Second)
		return e
	})
	if err != nil {
		t.Fatalf("WithMachine via injector: %v", err)
	}
	if !ran || inj.acquired != 1 {
		t.Errorf("injector not used: ran=%v acquired=%d", ran, inj.acquired)
	}
}

func TestWithMachineMapsBusyError(t *testing.T) {
	addr := miniMachine(t, func() string { return "<Idle>" })
	tr := machine.NewTracker()
	tr.Observe(machine.Idle)
	inj := &fakeInjector{addr: addr, forceErr: relayBusy()}
	a := New(Config{Tracker: tr, Injector: inj,
		Dial: func() (*client.Conn, error) { return client.Dial(addr, time.Second) }})
	a.EnterRelay()

	err := a.WithMachine(false, func(*client.Conn) error { return nil })
	if !errors.Is(err, ErrBusy) {
		t.Errorf("got %v, want ErrBusy", err)
	}
	if !Retryable(err) {
		t.Error("ErrBusy should be Retryable")
	}
}

func TestWithMachineIdleGating(t *testing.T) {
	tr := machine.NewTracker()
	addr := miniMachine(t, func() string { return "<Idle>" })
	a := newArbiter(t, addr, tr)

	// No state observed yet → not fresh → blocked when idle required.
	if err := a.WithMachine(true, func(*client.Conn) error { return nil }); !errors.Is(err, ErrNotIdle) {
		t.Errorf("stale state: got %v, want ErrNotIdle", err)
	}

	tr.Observe(machine.Run)
	if err := a.WithMachine(true, func(*client.Conn) error { return nil }); !errors.Is(err, ErrNotIdle) {
		t.Errorf("Run state: got %v, want ErrNotIdle", err)
	}

	tr.Observe(machine.Idle)
	ran := false
	if err := a.WithMachine(true, func(*client.Conn) error { ran = true; return nil }); err != nil {
		t.Errorf("Idle state should allow: %v", err)
	}
	if !ran {
		t.Error("fn did not run under Idle")
	}
}

func TestEnterRelayObservesStatus(t *testing.T) {
	tr := machine.NewTracker()
	addr := miniMachine(t, func() string { return "<Idle>" })
	a := newArbiter(t, addr, tr)

	// A sniffed status frame (relay mode) should update the tracker.
	a.EnterRelay()
	if !a.ObserveStatusPayload("<Run|MPos:1,2,3>") {
		t.Fatal("expected valid status payload")
	}
	if st, _ := tr.Snapshot(); st != machine.Run {
		t.Errorf("observed state = %q, want Run", st)
	}
}

func TestConnReusedAcrossCalls(t *testing.T) {
	addr := miniMachine(t, func() string { return "<Idle>" })
	a := newArbiter(t, addr, machine.NewTracker())

	var c1, c2 *client.Conn
	a.WithMachine(false, func(c *client.Conn) error { c1 = c; return nil })
	a.WithMachine(false, func(c *client.Conn) error { c2 = c; return nil })
	if c1 == nil || c1 != c2 {
		t.Error("expected the owner connection to be reused across calls")
	}
}

func TestConnDroppedOnError(t *testing.T) {
	addr := miniMachine(t, func() string { return "<Idle>" })
	a := newArbiter(t, addr, machine.NewTracker())

	var c1 *client.Conn
	a.WithMachine(false, func(c *client.Conn) error { c1 = c; return nil })
	// Force an error; the arbiter should drop the connection.
	a.WithMachine(false, func(c *client.Conn) error { return errors.New("boom") })
	var c2 *client.Conn
	a.WithMachine(false, func(c *client.Conn) error { c2 = c; return nil })
	if c1 == c2 {
		t.Error("expected a fresh connection after an error")
	}
}
