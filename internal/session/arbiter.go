// Package session arbitrates the single machine connection between the official
// controller (relay mode) and the proxy's own sync engine (owner mode).
//
// The firmware is single-conversation, so at most one of {controller, sync
// engine} may talk to the machine at a time. The arbiter enforces this:
//
//   - When a controller connects, the relay takes over: the arbiter enters
//     relay mode, the sync engine is told to stand down, and machine state is
//     observed passively by sniffing STATUS_RES frames the controller solicits.
//   - When no controller is connected, the arbiter is in owner mode: it holds
//     its own connection to the machine, polls "?" to track state, and lets the
//     sync engine borrow the connection to run file operations while Idle.
package session

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/uwin/cnc-proxy/internal/client"
	"github.com/uwin/cnc-proxy/internal/machine"
	"github.com/uwin/cnc-proxy/internal/relay"
)

// Arbiter implements relay.Observer so the relay can drive mode transitions and
// feed sniffed machine state into the tracker.
var _ relay.Observer = (*Arbiter)(nil)

// Mode is the arbiter's current ownership of the machine connection.
type Mode int

const (
	// ModeOwner: no controller connected; the proxy owns the connection and the
	// sync engine may run.
	ModeOwner Mode = iota
	// ModeRelay: a controller is connected; the proxy only observes.
	ModeRelay
)

func (m Mode) String() string {
	if m == ModeRelay {
		return "relay"
	}
	return "owner"
}

// Injector borrows the controller's shared machine connection for one injected
// operation during relay mode. The relay Server implements it.
type Injector interface {
	AcquireMachine() (it InjectTransport, release func(), err error)
}

// InjectTransport is the byte channel an injected operation drives.
type InjectTransport interface {
	Write(p []byte) (int, error)
	Read(p []byte) (int, error)
	SetReadDeadline(t time.Time) error
	Close() error
}

// Arbiter tracks mode and machine state, and gates access to the machine
// connection from the proxy's own operations (sync engine, API), whether the
// proxy owns the connection (owner mode) or is sharing it with a connected
// controller (relay mode, via the injector).
type Arbiter struct {
	tracker *machine.Tracker

	// dial opens a fresh owner-mode connection to the machine. Injectable for
	// tests; in production it dials the real machine address.
	dial func() (*client.Conn, error)

	// injector, if set, lets owner operations run during relay mode by borrowing
	// the controller's shared connection between its transactions.
	injector Injector

	mu        sync.Mutex
	mode      Mode
	ownerConn *client.Conn

	// opMu serializes actual machine I/O for the full duration of a WithMachine
	// callback. The machine is single-conversation and a client.Conn is not safe
	// for concurrent use, so the status poll, sync engine, and gcode injection
	// must never run two operations on it at once. Held across fn, separate from
	// mu (which only guards the fields above and must not be held during I/O).
	opMu sync.Mutex

	// pollInterval controls how often owner mode queries "?" for state.
	pollInterval time.Duration
	// stateMaxAge bounds how old an observed state may be and still gate ops.
	stateMaxAge time.Duration
}

// Config configures an Arbiter.
type Config struct {
	Dial         func() (*client.Conn, error)
	Tracker      *machine.Tracker
	Injector     Injector
	PollInterval time.Duration
	StateMaxAge  time.Duration
}

// New creates an Arbiter in owner mode.
func New(cfg Config) *Arbiter {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.StateMaxAge == 0 {
		cfg.StateMaxAge = 10 * time.Second
	}
	if cfg.Tracker == nil {
		cfg.Tracker = machine.NewTracker()
	}
	return &Arbiter{
		tracker:      cfg.Tracker,
		dial:         cfg.Dial,
		injector:     cfg.Injector,
		mode:         ModeOwner,
		pollInterval: cfg.PollInterval,
		stateMaxAge:  cfg.StateMaxAge,
	}
}

// SetInjector wires the relay injector after construction (the relay and
// arbiter are mutually referential at startup).
func (a *Arbiter) SetInjector(inj Injector) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.injector = inj
}

// Mode returns the current mode.
func (a *Arbiter) Mode() Mode {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mode
}

// Tracker exposes the shared machine-state tracker.
func (a *Arbiter) Tracker() *machine.Tracker { return a.tracker }

// EnterRelay is called when a controller connects. The proxy drops its owner
// connection (if any) so the controller becomes the machine's sole conversation
// partner, and the sync engine is blocked from acquiring the connection.
func (a *Arbiter) EnterRelay() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mode = ModeRelay
	if a.ownerConn != nil {
		a.ownerConn.Close()
		a.ownerConn = nil
	}
}

// ExitRelay is called when the controller disconnects, returning to owner mode.
func (a *Arbiter) ExitRelay() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mode = ModeOwner
}

// ObserveStatusPayload feeds a status report (from either a sniffed relay frame
// or an owner-mode poll) into the tracker.
func (a *Arbiter) ObserveStatusPayload(payload string) bool {
	return a.tracker.ObserveStatusPayload(payload)
}

// WithMachine runs fn with a connection to the machine. In owner mode it uses
// the proxy's own persistent connection. In relay mode it borrows the
// controller's shared connection via the injector, running fn between the
// controller's transactions; if no injector is configured it returns
// ErrRelayActive. When requireIdle is set, fn runs only if machine state is a
// fresh Idle (ErrNotIdle otherwise). Relay-mode injection additionally returns
// ErrBusy (from the injector) if the controller is mid file-transfer.
func (a *Arbiter) WithMachine(requireIdle bool, fn func(*client.Conn) error) error {
	// Serialize all machine I/O: only one operation may use the single
	// connection at a time. Held for the whole callback.
	a.opMu.Lock()
	defer a.opMu.Unlock()

	a.mu.Lock()
	mode := a.mode
	inj := a.injector
	if requireIdle {
		st, _ := a.tracker.Snapshot()
		if !a.tracker.Fresh(a.stateMaxAge) || !st.CanRunFileOps() {
			a.mu.Unlock()
			return ErrNotIdle
		}
	}

	if mode != ModeOwner {
		a.mu.Unlock()
		if inj == nil {
			return ErrRelayActive
		}
		return a.withInjection(inj, fn)
	}

	conn, err := a.acquireConnLocked()
	a.mu.Unlock()
	if err != nil {
		return err
	}
	if err := fn(conn); err != nil {
		// On any connection-level error, drop it so the next call reconnects.
		a.mu.Lock()
		if a.ownerConn == conn {
			a.ownerConn.Close()
			a.ownerConn = nil
		}
		a.mu.Unlock()
		return err
	}
	return nil
}

// withInjection borrows the controller's shared connection for one operation.
func (a *Arbiter) withInjection(inj Injector, fn func(*client.Conn) error) error {
	it, release, err := inj.AcquireMachine()
	if err != nil {
		// Map the relay's "can't right now" errors to session semantics so the
		// sync engine treats them as retryable rather than failures.
		switch {
		case errors.Is(err, relay.ErrBusy):
			return ErrBusy
		case errors.Is(err, relay.ErrNoSession):
			return ErrRelayActive
		default:
			return err
		}
	}
	defer release()
	conn := client.NewTransport(it)
	return fn(conn)
}

// acquireConnLocked returns the cached owner connection, dialing if needed. The
// caller must hold a.mu.
func (a *Arbiter) acquireConnLocked() (*client.Conn, error) {
	if a.ownerConn != nil {
		return a.ownerConn, nil
	}
	conn, err := a.dial()
	if err != nil {
		return nil, err
	}
	a.ownerConn = conn
	return conn, nil
}

// Poll runs the owner-mode state poll loop until ctx is canceled. It queries
// "?" immediately and then every pollInterval while in owner mode, feeding
// results to the tracker. In relay mode it does nothing (the relay observer
// feeds state instead).
func (a *Arbiter) Poll(ctx context.Context, queryTimeout time.Duration) {
	ticker := time.NewTicker(a.pollInterval)
	defer ticker.Stop()
	a.pollOnce(queryTimeout) // observe state right away, don't wait a full tick
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.pollOnce(queryTimeout)
		}
	}
}

// pollOnce queries machine state if in owner mode.
func (a *Arbiter) pollOnce(queryTimeout time.Duration) {
	if a.Mode() != ModeOwner {
		return
	}
	_ = a.WithMachine(false, func(c *client.Conn) error {
		payload, err := c.QueryState(queryTimeout)
		if err != nil {
			return err
		}
		a.tracker.ObserveStatusPayload(payload)
		return nil
	})
}
