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
