// Package store holds the proxy's durable view of the machine's file space: the
// catalog of files and their sync state, and an ordered queue of pending
// operations. Both survive restarts and offline periods, which is what gives
// the filesystem its Google-Drive-like behavior: a write is accepted locally
// immediately and reconciled with the machine later, when it is reachable and
// idle.
package store

import "time"

// SyncState describes where a catalog entry stands relative to the machine.
type SyncState string

const (
	// Synced: local cache and machine agree (MD5 match).
	Synced SyncState = "synced"
	// LocalOnly: exists locally, no upload queued yet (transient).
	LocalOnly SyncState = "local_only"
	// PendingUpload: an upload job is queued but not started.
	PendingUpload SyncState = "pending_upload"
	// Uploading: an upload job is actively transferring.
	Uploading SyncState = "uploading"
	// PendingDelete: a delete job is queued.
	PendingDelete SyncState = "pending_delete"
	// Deleting: a delete job is running.
	Deleting SyncState = "deleting"
	// PendingRename: a rename job is queued.
	PendingRename SyncState = "pending_rename"
	// RemoteOnly: known to exist on the machine but not cached locally.
	RemoteOnly SyncState = "remote_only"
	// Error: the last operation on this entry failed.
	Error SyncState = "error"
)

// Entry is one file (or directory) in the catalog. Path is the machine-absolute
// path (e.g. "/sd/gcodes/part.nc"). For directories, Size is 0 and MD5 empty.
type Entry struct {
	Path      string    `json:"path"`
	IsDir     bool      `json:"is_dir"`
	Size      int64     `json:"size"`
	MTime     time.Time `json:"mtime"`
	MD5       string    `json:"md5,omitempty"`        // content MD5 (uncompressed)
	CachePath string    `json:"cache_path,omitempty"` // local cache file, if held
	Sync      SyncState `json:"sync"`
	Error     string    `json:"error,omitempty"` // detail when Sync == Error
	UpdatedAt time.Time `json:"updated_at"`
}

// JobKind is the type of a queued operation.
type JobKind string

const (
	JobUpload JobKind = "upload"
	JobDelete JobKind = "delete"
	JobRename JobKind = "rename"
	JobMkdir  JobKind = "mkdir"
)

// JobState is the lifecycle state of a job.
type JobState string

const (
	Queued  JobState = "queued"
	Running JobState = "running"
	Done    JobState = "done"
	Failed  JobState = "failed"
)

// Job is one durable operation against the machine. Jobs are executed in order
// (FIFO) and are idempotent so a retry after a crash is safe.
type Job struct {
	ID        int64     `json:"id"`
	Kind      JobKind   `json:"kind"`
	Path      string    `json:"path"`                 // primary target (machine-absolute)
	DestPath  string    `json:"dest_path,omitempty"`  // for rename
	CachePath string    `json:"cache_path,omitempty"` // local source for upload
	MD5       string    `json:"md5,omitempty"`        // expected content MD5 for upload
	Size      int64     `json:"size,omitempty"`
	State     JobState  `json:"state"`
	Attempts  int       `json:"attempts"`
	LastError string    `json:"last_error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// UISettings is durable operator UI configuration. It is intentionally kept in
// the store file with the catalog/queue so a deployment restart preserves the
// control console layout without requiring browser-local state.
type UISettings struct {
	Macros       []Macro     `json:"macros"`
	MacroButtons []MacroSlot `json:"macro_buttons"`
	Log          LogSettings `json:"log"`
	Gamepad      Gamepad     `json:"gamepad"`
}

// Macro is a named sequence of gcode/console lines that can be assigned to a
// button in the web console.
type Macro struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Lines       []string  `json:"lines"`
	Color       string    `json:"color,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
}

// MacroSlot places a macro button in a named UI region.
type MacroSlot struct {
	ID      string `json:"id"`
	MacroID string `json:"macro_id"`
	Region  string `json:"region"` // "toolbar" | "panel"
	Order   int    `json:"order"`
}

// LogSettings stores operator preferences for the log console.
type LogSettings struct {
	Filter     string `json:"filter,omitempty"`
	Autoscroll bool   `json:"autoscroll"`
}

// Gamepad stores browser gamepad mapping and speed preferences for the jog UI.
// The server still enforces the configured jog speed limits; these settings only
// scale and map the normalized client input before it is sent to the jog
// WebSocket.
type Gamepad struct {
	Axes          GamepadAxes          `json:"axes"`
	DeadmanButton int                  `json:"deadman_button"`
	SlowButtons   []int                `json:"slow_buttons"`
	MacroButtons  []GamepadMacroButton `json:"macro_buttons"`
}

// GamepadAxes maps physical browser axes into machine axes.
type GamepadAxes struct {
	X GamepadAxis `json:"x"`
	Y GamepadAxis `json:"y"`
	Z GamepadAxis `json:"z"`
}

// GamepadAxis configures one physical axis. Scale is 0..1, applied client-side
// before the normalized axis value is sent to the jog engine.
type GamepadAxis struct {
	Axis   int     `json:"axis"`
	Invert bool    `json:"invert"`
	Scale  float64 `json:"scale"`
}

// GamepadMacroButton maps one physical gamepad button to a saved gcode macro.
// The web UI fires these edge-triggered while jog is armed and the deadman is
// held; execution still goes through the normal /api/gcode path.
type GamepadMacroButton struct {
	ID      string `json:"id"`
	Button  int    `json:"button"`
	MacroID string `json:"macro_id"`
}
