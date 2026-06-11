package session

import "errors"

var (
	// ErrRelayActive means a controller currently owns the machine connection,
	// so owner-mode operations (the sync engine) must wait.
	ErrRelayActive = errors.New("session: controller is connected (relay mode)")

	// ErrNotIdle means the machine is not in a fresh Idle state, so file
	// operations cannot run yet.
	ErrNotIdle = errors.New("session: machine not idle")

	// ErrBusy means the controller is mid file-transfer, so an injected
	// operation cannot safely begin right now. Like ErrRelayActive, it is a
	// "try again later" condition, not a failure.
	ErrBusy = errors.New("session: controller transaction in progress")
)

// Retryable reports whether an error from WithMachine means "blocked, try
// later" rather than a genuine operation failure. The sync engine uses this to
// keep jobs queued instead of marking them failed.
func Retryable(err error) bool {
	return errors.Is(err, ErrRelayActive) || errors.Is(err, ErrNotIdle) || errors.Is(err, ErrBusy)
}
