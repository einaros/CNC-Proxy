package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store is the durable catalog + job queue. It keeps everything in memory and
// persists the whole model atomically to a JSON file on every mutation. The
// dataset (a few hundred gcode files) is small enough that whole-file
// persistence is simpler and safe; we can shard later if needed.
type Store struct {
	mu      sync.RWMutex
	path    string
	now     func() time.Time
	entries map[string]*Entry // keyed by Path
	jobs    []*Job
	nextJob int64
	ui      UISettings
	active  string
	subs    map[int]chan Event
	nextSub int
}

// Event notifies subscribers of a change, for pushing to the web UI.
type Event struct {
	Kind            string `json:"kind"` // "entry" | "job" | "active_gcode"
	Entry           *Entry `json:"entry,omitempty"`
	Job             *Job   `json:"job,omitempty"`
	ActiveGcodePath string `json:"active_gcode_path,omitempty"`
}

type persisted struct {
	Entries         map[string]*Entry `json:"entries"`
	Jobs            []*Job            `json:"jobs"`
	NextJob         int64             `json:"next_job"`
	UI              *UISettings       `json:"ui,omitempty"`
	ActiveGcodePath string            `json:"active_gcode_path,omitempty"`
}

type modelSnapshot struct {
	entries map[string]*Entry
	jobs    []*Job
	nextJob int64
	ui      UISettings
	active  string
}

// Batch groups catalog and queue mutations into one durable store flush.
type Batch struct {
	s      *Store
	now    time.Time
	dirty  bool
	events []Event
}

// Snapshot is an exportable copy of the durable state.json model.
type Snapshot struct {
	Entries         map[string]Entry `json:"entries"`
	Jobs            []Job            `json:"jobs"`
	NextJob         int64            `json:"next_job"`
	UI              UISettings       `json:"ui"`
	ActiveGcodePath string           `json:"active_gcode_path,omitempty"`
}

// Open loads a store from path, creating an empty one if the file is absent.
func Open(path string) (*Store, error) {
	s := &Store{
		path:    path,
		now:     time.Now,
		entries: map[string]*Entry{},
		subs:    map[int]chan Event{},
		nextJob: 1,
		ui:      defaultUISettings(),
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("store: corrupt file %s: %w", path, err)
	}
	if p.Entries != nil {
		s.entries = p.Entries
	}
	for _, e := range s.entries {
		normalizeLoadedEntry(e)
	}
	s.jobs = p.Jobs
	if p.NextJob > 0 {
		s.nextJob = p.NextJob
	}
	if p.UI != nil {
		s.ui = normalizeUISettings(*p.UI, s.now())
	}
	s.active = strings.TrimSpace(p.ActiveGcodePath)
	return s, nil
}

// Snapshot returns a deep copy of the durable model for backup/export.
func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := Snapshot{
		Entries:         make(map[string]Entry, len(s.entries)),
		Jobs:            make([]Job, 0, len(s.jobs)),
		NextJob:         s.nextJob,
		UI:              copyUISettings(s.ui),
		ActiveGcodePath: s.active,
	}
	for k, e := range s.entries {
		out.Entries[k] = *e
	}
	for _, j := range s.jobs {
		cp := *j
		clearJobDiagnostics(&cp)
		out.Jobs = append(out.Jobs, cp)
	}
	return out
}

// Restore replaces the durable model with a backup snapshot.
func (s *Store) Restore(in Snapshot) error {
	entries := make(map[string]*Entry, len(in.Entries))
	for k, e := range in.Entries {
		cp := e
		normalizeLoadedEntry(&cp)
		entries[k] = &cp
	}
	jobs := make([]*Job, 0, len(in.Jobs))
	nextJob := in.NextJob
	for _, j := range in.Jobs {
		clearJobDiagnostics(&j)
		cp := j
		jobs = append(jobs, &cp)
		if cp.ID >= nextJob {
			nextJob = cp.ID + 1
		}
	}
	if nextJob <= 0 {
		nextJob = 1
	}
	ui := normalizeUISettings(in.UI, s.now())
	active := strings.TrimSpace(in.ActiveGcodePath)
	return s.Batch(func(b *Batch) error {
		b.replaceModel(entries, jobs, nextJob, ui, active)
		return nil
	})
}

func normalizeLoadedEntry(e *Entry) {
	if e == nil {
		return
	}
	if e.IsDir || e.CachePath == "" {
		e.CacheState = CacheNone
		e.CacheCheckedAt = time.Time{}
	}
}

func normalizeNewEntry(e *Entry) {
	if e == nil {
		return
	}
	if e.IsDir || e.CachePath == "" {
		e.CacheState = CacheNone
		e.CacheCheckedAt = time.Time{}
		return
	}
	if e.CacheState == "" {
		e.CacheState = CacheReady
	}
	if e.CacheState == CacheNone {
		e.CacheCheckedAt = time.Time{}
	}
}

// flushLocked writes the whole model atomically and durably. Caller holds s.mu.
// It fsyncs the temp file before the rename and the parent directory after, so
// a crash or power loss cannot leave the catalog/queue partially written or the
// rename unrecorded — which would otherwise resurrect completed jobs (e.g.
// re-running a delete) on restart.
func (s *Store) flushLocked() error {
	if s.path == "" {
		return nil // in-memory only (tests)
	}
	p := persisted{Entries: s.entries, Jobs: s.jobs, NextJob: s.nextJob, UI: &s.ui, ActiveGcodePath: s.active}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return err
	}
	// fsync the directory so the rename itself is durable.
	if dir, derr := os.Open(filepath.Dir(s.path)); derr == nil {
		dir.Sync()
		dir.Close()
	}
	return nil
}

func (s *Store) publishLocked(ev Event) {
	for _, ch := range s.subs {
		select {
		case ch <- ev:
		default:
			// Drop if a subscriber is slow; the UI can re-fetch the full state.
		}
	}
}

func (s *Store) snapshotLocked() modelSnapshot {
	snap := modelSnapshot{
		entries: make(map[string]*Entry, len(s.entries)),
		jobs:    make([]*Job, 0, len(s.jobs)),
		nextJob: s.nextJob,
		ui:      copyUISettings(s.ui),
		active:  s.active,
	}
	for k, e := range s.entries {
		cp := *e
		snap.entries[k] = &cp
	}
	for _, j := range s.jobs {
		cp := *j
		snap.jobs = append(snap.jobs, &cp)
	}
	return snap
}

func (s *Store) restoreLocked(snap modelSnapshot) {
	s.entries = snap.entries
	s.jobs = snap.jobs
	s.nextJob = snap.nextJob
	s.ui = snap.ui
	s.active = snap.active
}

// Batch runs fn while holding the store lock, flushing the full model once at
// the end. If fn or the flush fails, all in-memory changes are rolled back and
// no subscriber events are published.
func (s *Store) Batch(fn func(*Batch) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.batchLocked(fn)
}

func (s *Store) batchLocked(fn func(*Batch) error) error {
	snap := s.snapshotLocked()
	b := &Batch{s: s, now: s.now()}
	if err := fn(b); err != nil {
		s.restoreLocked(snap)
		return err
	}
	if !b.dirty {
		return nil
	}
	if err := s.flushLocked(); err != nil {
		s.restoreLocked(snap)
		return err
	}
	for _, ev := range b.events {
		s.publishLocked(ev)
	}
	return nil
}

func (b *Batch) markDirty() {
	b.dirty = true
}

func (b *Batch) publishEntry(e Entry) {
	cp := e
	b.events = append(b.events, Event{Kind: "entry", Entry: &cp})
}

func (b *Batch) publishJob(j Job) {
	cp := j
	b.events = append(b.events, Event{Kind: "job", Job: &cp})
}

func (b *Batch) publishReset() {
	b.events = append(b.events, Event{Kind: "reset"})
}

func (b *Batch) replaceModel(entries map[string]*Entry, jobs []*Job, nextJob int64, ui UISettings, active string) {
	b.s.entries = entries
	b.s.jobs = jobs
	b.s.nextJob = nextJob
	b.s.ui = ui
	b.s.active = active
	b.markDirty()
	b.publishReset()
}

// GetEntry returns a copy of an entry visible inside this batch.
func (b *Batch) GetEntry(path string) (Entry, bool) {
	e, ok := b.s.entries[path]
	if !ok {
		return Entry{}, false
	}
	return *e, true
}

// GetJob returns a copy of a job visible inside this batch.
func (b *Batch) GetJob(id int64) (Job, bool) {
	for _, j := range b.s.jobs {
		if j.ID == id {
			return *j, true
		}
	}
	return Job{}, false
}

// ListJobs returns copies of all jobs visible inside this batch.
func (b *Batch) ListJobs() []Job {
	out := make([]Job, 0, len(b.s.jobs))
	for _, j := range b.s.jobs {
		out = append(out, *j)
	}
	return out
}

// PutEntry inserts or replaces an entry in this batch.
func (b *Batch) PutEntry(e Entry) Entry {
	normalizeNewEntry(&e)
	e.UpdatedAt = b.now
	cp := e
	b.s.entries[e.Path] = &cp
	b.markDirty()
	b.publishEntry(cp)
	return cp
}

// SetEntrySync updates just the sync state and error of an entry if present.
func (b *Batch) SetEntrySync(path string, state SyncState, errMsg string) (Entry, bool) {
	e, ok := b.s.entries[path]
	if !ok {
		return Entry{}, false
	}
	e.Sync = state
	e.Error = errMsg
	e.UpdatedAt = b.now
	cp := *e
	b.markDirty()
	b.publishEntry(cp)
	return cp, true
}

// SetEntrySyncIf updates an entry only when its current sync state is allowed.
func (b *Batch) SetEntrySyncIf(path string, state SyncState, errMsg string, allowed ...SyncState) (Entry, bool) {
	e, ok := b.s.entries[path]
	if !ok || !syncStateIn(e.Sync, allowed) {
		return Entry{}, false
	}
	e.Sync = state
	e.Error = errMsg
	e.UpdatedAt = b.now
	cp := *e
	b.markDirty()
	b.publishEntry(cp)
	return cp, true
}

// SetEntrySyncIfMatch updates an entry only when match accepts the current entry.
func (b *Batch) SetEntrySyncIfMatch(path string, state SyncState, errMsg string, match func(Entry) bool) (Entry, bool) {
	e, ok := b.s.entries[path]
	if !ok {
		return Entry{}, false
	}
	current := *e
	if !match(current) {
		return Entry{}, false
	}
	e.Sync = state
	e.Error = errMsg
	e.UpdatedAt = b.now
	cp := *e
	b.markDirty()
	b.publishEntry(cp)
	return cp, true
}

// DeleteEntry removes an entry from the catalog.
func (b *Batch) DeleteEntry(path string) bool {
	if _, ok := b.s.entries[path]; !ok {
		return false
	}
	delete(b.s.entries, path)
	b.markDirty()
	b.publishEntry(Entry{Path: path, Sync: ""})
	return true
}

// DeleteEntryIfSync removes an entry only when its current sync state is allowed.
func (b *Batch) DeleteEntryIfSync(path string, allowed ...SyncState) (Entry, bool) {
	e, ok := b.s.entries[path]
	if !ok || !syncStateIn(e.Sync, allowed) {
		return Entry{}, false
	}
	entry := *e
	delete(b.s.entries, path)
	b.markDirty()
	b.publishEntry(Entry{Path: path, Sync: ""})
	return entry, true
}

// DiscardEntry removes a catalog entry and marks matching queued/failed jobs done.
func (b *Batch) DiscardEntry(path string, kinds ...JobKind) (Entry, bool) {
	e, ok := b.s.entries[path]
	if !ok {
		return Entry{}, false
	}
	entry := *e
	delete(b.s.entries, path)
	b.markDirty()

	for _, j := range b.matchingQueuedOrFailedJobs(path, kinds...) {
		j.State = Done
		j.LastError = ""
		j.UpdatedAt = b.now
		b.publishJob(*j)
	}
	b.publishEntry(Entry{Path: path, Sync: ""})
	return entry, true
}

// DiscardJobs marks matching queued/failed jobs done without requiring an entry.
func (b *Batch) DiscardJobs(path string, kinds ...JobKind) ([]Job, bool) {
	var out []Job
	for _, j := range b.matchingQueuedOrFailedJobs(path, kinds...) {
		j.State = Done
		j.LastError = ""
		j.UpdatedAt = b.now
		cp := *j
		out = append(out, cp)
		b.markDirty()
		b.publishJob(cp)
	}
	return out, len(out) > 0
}

func (b *Batch) matchingQueuedOrFailedJobs(path string, kinds ...JobKind) []*Job {
	kindSet := map[JobKind]bool{}
	for _, kind := range kinds {
		kindSet[kind] = true
	}
	var out []*Job
	for _, j := range b.s.jobs {
		if j.Path != path || !kindSet[j.Kind] || (j.State != Queued && j.State != Failed) {
			continue
		}
		out = append(out, j)
	}
	return out
}

// Enqueue appends a job, assigning an ID and timestamps.
func (b *Batch) Enqueue(j Job) Job {
	j.ID = b.s.nextJob
	b.s.nextJob++
	j.State = Queued
	j.CreatedAt = b.now
	j.UpdatedAt = b.now
	cp := j
	b.s.jobs = append(b.s.jobs, &cp)
	b.markDirty()
	b.publishJob(cp)
	return cp
}

// SupersedeQueuedUploads marks still-queued upload jobs for path as done.
func (b *Batch) SupersedeQueuedUploads(path string) int {
	n := 0
	for _, j := range b.s.jobs {
		if j.Kind == JobUpload && j.Path == path && j.State == Queued {
			j.State = Done
			j.UpdatedAt = b.now
			n++
			b.markDirty()
			b.publishJob(*j)
		}
	}
	return n
}

// UpdateJob applies a mutation to a job by ID.
func (b *Batch) UpdateJob(id int64, mutate func(*Job)) (Job, bool) {
	for _, j := range b.s.jobs {
		if j.ID == id {
			mutate(j)
			j.UpdatedAt = b.now
			cp := *j
			b.markDirty()
			b.publishJob(cp)
			return cp, true
		}
	}
	return Job{}, false
}

// StartJob moves a queued job to running.
func (b *Batch) StartJob(id int64) (Job, bool) {
	for _, j := range b.s.jobs {
		if j.ID != id {
			continue
		}
		if j.State != Queued {
			return *j, false
		}
		j.State = Running
		j.UpdatedAt = b.now
		cp := *j
		b.markDirty()
		b.publishJob(cp)
		return cp, true
	}
	return Job{}, false
}

// RetryJob resets a failed job to queued.
func (b *Batch) RetryJob(id int64) (Job, bool) {
	for _, j := range b.s.jobs {
		if j.ID != id {
			continue
		}
		if j.State != Failed {
			return *j, false
		}
		j.State = Queued
		j.Attempts = 0
		j.LastError = ""
		j.UpdatedAt = b.now
		cp := *j
		b.markDirty()
		b.publishJob(cp)
		return cp, true
	}
	return Job{}, false
}

// PruneDoneJobs removes completed jobs older than cutoff.
func (b *Batch) PruneDoneJobs(cutoff time.Time) int {
	kept := b.s.jobs[:0]
	removed := 0
	for _, j := range b.s.jobs {
		if j.State == Done && j.UpdatedAt.Before(cutoff) {
			removed++
			continue
		}
		kept = append(kept, j)
	}
	if removed == 0 {
		return 0
	}
	b.s.jobs = kept
	b.markDirty()
	b.publishReset()
	return removed
}

// SetUISettings replaces durable operator UI settings.
func (b *Batch) SetUISettings(ui UISettings) UISettings {
	b.s.ui = normalizeUISettings(ui, b.now)
	b.markDirty()
	return copyUISettings(b.s.ui)
}

// SetActiveGcodePath persists the proxy-side selected gcode path.
func (b *Batch) SetActiveGcodePath(path string) bool {
	path = strings.TrimSpace(path)
	if b.s.active == path {
		return false
	}
	b.s.active = path
	b.markDirty()
	b.events = append(b.events, Event{Kind: "active_gcode", ActiveGcodePath: path})
	return true
}

// Subscribe returns a channel of change events and an unsubscribe func.
func (s *Store) Subscribe() (<-chan Event, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextSub
	s.nextSub++
	ch := make(chan Event, 64)
	s.subs[id] = ch
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if c, ok := s.subs[id]; ok {
			delete(s.subs, id)
			close(c)
		}
	}
}

// --- Catalog ---

// PutEntry inserts or replaces an entry, stamping UpdatedAt and persisting.
func (s *Store) PutEntry(e Entry) error {
	return s.Batch(func(b *Batch) error {
		b.PutEntry(e)
		return nil
	})
}

// SetEntrySync updates just the sync state (and error) of an entry if present.
func (s *Store) SetEntrySync(path string, state SyncState, errMsg string) error {
	return s.Batch(func(b *Batch) error {
		b.SetEntrySync(path, state, errMsg)
		return nil
	})
}

// SetEntrySyncIf updates an entry only when its current sync state is one of
// allowed. It is used by job completion paths so stale jobs cannot clobber a
// newer desired state for the same path.
func (s *Store) SetEntrySyncIf(path string, state SyncState, errMsg string, allowed ...SyncState) (Entry, bool, error) {
	var entry Entry
	var ok bool
	err := s.Batch(func(b *Batch) error {
		entry, ok = b.SetEntrySyncIf(path, state, errMsg, allowed...)
		return nil
	})
	return entry, ok, err
}

// SetEntrySyncIfMatch updates an entry only when match accepts the current
// entry. The predicate runs while the store lock is held and should stay cheap.
func (s *Store) SetEntrySyncIfMatch(path string, state SyncState, errMsg string, match func(Entry) bool) (Entry, bool, error) {
	var entry Entry
	var ok bool
	err := s.Batch(func(b *Batch) error {
		entry, ok = b.SetEntrySyncIfMatch(path, state, errMsg, match)
		return nil
	})
	return entry, ok, err
}

// GetEntry returns a copy of an entry.
func (s *Store) GetEntry(path string) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[path]
	if !ok {
		return Entry{}, false
	}
	return *e, true
}

// GetJob returns a copy of a queued job by ID.
func (s *Store) GetJob(id int64) (Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, j := range s.jobs {
		if j.ID == id {
			cp := *j
			clearJobDiagnostics(&cp)
			return cp, true
		}
	}
	return Job{}, false
}

// DeleteEntry removes an entry from the catalog.
func (s *Store) DeleteEntry(path string) error {
	return s.Batch(func(b *Batch) error {
		b.DeleteEntry(path)
		return nil
	})
}

// DeleteEntryIfSync removes an entry only when its current sync state is one of
// allowed. This keeps stale delete jobs from removing a replacement upload that
// arrived while the delete was running.
func (s *Store) DeleteEntryIfSync(path string, allowed ...SyncState) (Entry, bool, error) {
	var entry Entry
	var ok bool
	err := s.Batch(func(b *Batch) error {
		entry, ok = b.DeleteEntryIfSync(path, allowed...)
		return nil
	})
	return entry, ok, err
}

func syncStateIn(state SyncState, allowed []SyncState) bool {
	for _, candidate := range allowed {
		if state == candidate {
			return true
		}
	}
	return false
}

// DiscardEntry removes a local catalog entry and marks matching queued/failed
// jobs done in one persisted update. It is used when a desired local create
// never reached the machine, so deletion is a local discard rather than a
// machine-side rm.
func (s *Store) DiscardEntry(path string, kinds ...JobKind) (Entry, bool, error) {
	var entry Entry
	var ok bool
	err := s.Batch(func(b *Batch) error {
		entry, ok = b.DiscardEntry(path, kinds...)
		return nil
	})
	return entry, ok, err
}

// DiscardJobs marks matching queued/failed jobs done without requiring a
// catalog entry. It is used to clear orphaned failed jobs from the activity
// panel after the underlying catalog entry has already disappeared.
func (s *Store) DiscardJobs(path string, kinds ...JobKind) ([]Job, bool, error) {
	var jobs []Job
	var ok bool
	err := s.Batch(func(b *Batch) error {
		jobs, ok = b.DiscardJobs(path, kinds...)
		return nil
	})
	return jobs, ok, err
}

// ListEntries returns all entries sorted by path.
func (s *Store) ListEntries() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// --- Job queue ---

// Enqueue appends a job, assigning an ID and stamping timestamps.
func (s *Store) Enqueue(j Job) (Job, error) {
	var out Job
	err := s.Batch(func(b *Batch) error {
		out = b.Enqueue(j)
		return nil
	})
	return out, err
}

// SupersedeQueuedUploads marks any still-queued upload job for path as Done
// without running it. A newer upload of the same path has replaced its content,
// so the older queued transfer is obsolete (it would upload superseded bytes
// under a stale MD5). Running jobs are not touched — they hold an open fd to the
// cache file and complete consistently. Returns the count superseded.
func (s *Store) SupersedeQueuedUploads(path string) (int, error) {
	n := 0
	err := s.Batch(func(b *Batch) error {
		n = b.SupersedeQueuedUploads(path)
		return nil
	})
	return n, err
}

// NextQueued returns a copy of the oldest queued job, or false if none.
func (s *Store) NextQueued() (Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, j := range s.jobs {
		if j.State == Queued {
			return *j, true
		}
	}
	return Job{}, false
}

// QueuedJobs returns copies of all queued jobs in queue (FIFO) order.
func (s *Store) QueuedJobs() []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Job
	for _, j := range s.jobs {
		if j.State == Queued {
			out = append(out, *j)
		}
	}
	return out
}

// UpdateJob applies a mutation to a job by ID, persisting and publishing.
func (s *Store) UpdateJob(id int64, mutate func(*Job)) error {
	return s.Batch(func(b *Batch) error {
		if _, ok := b.UpdateJob(id, mutate); !ok {
			return fmt.Errorf("store: job %d not found", id)
		}
		return nil
	})
}

// StartJob moves a queued job to running, returning false if the job no longer
// exists or was already completed/failed/discarded by another operation.
func (s *Store) StartJob(id int64) (Job, bool, error) {
	var job Job
	var ok bool
	err := s.Batch(func(b *Batch) error {
		job, ok = b.StartJob(id)
		return nil
	})
	return job, ok, err
}

// RetryJob resets a failed job to queued so the sync engine will attempt it
// again on the next drain pass.
func (s *Store) RetryJob(id int64) (Job, bool, error) {
	var job Job
	var ok bool
	err := s.Batch(func(b *Batch) error {
		job, ok = b.RetryJob(id)
		if !ok && job.ID == 0 {
			return fmt.Errorf("store: job %d not found", id)
		}
		return nil
	})
	return job, ok, err
}

// ListJobs returns all jobs in queue order.
func (s *Store) ListJobs() []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		cp := *j
		clearJobDiagnostics(&cp)
		out = append(out, cp)
	}
	return out
}

// PruneDoneJobs removes completed jobs older than the given age, keeping the
// queue from growing without bound. Failed jobs are retained for visibility.
func (s *Store) PruneDoneJobs(olderThan time.Duration) (int, error) {
	cutoff := s.now().Add(-olderThan)
	removed := 0
	err := s.Batch(func(b *Batch) error {
		removed = b.PruneDoneJobs(cutoff)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// UISettings returns a copy of the durable operator UI settings.
func (s *Store) UISettings() UISettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return copyUISettings(s.ui)
}

// SetUISettings replaces the durable operator UI settings.
func (s *Store) SetUISettings(ui UISettings) (UISettings, error) {
	var out UISettings
	err := s.Batch(func(b *Batch) error {
		out = b.SetUISettings(ui)
		return nil
	})
	return out, err
}

// ActiveGcodePath returns the durable proxy-side selected gcode path.
func (s *Store) ActiveGcodePath() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.active
}

// SetActiveGcodePath persists the proxy-side selected gcode path.
func (s *Store) SetActiveGcodePath(path string) error {
	return s.Batch(func(b *Batch) error {
		b.SetActiveGcodePath(path)
		return nil
	})
}

// CacheDir returns the directory where cached file contents should live,
// alongside the store file.
func (s *Store) CacheDir() string {
	return filepath.Join(filepath.Dir(s.path), "cache")
}

func normalizeUISettings(in UISettings, now time.Time) UISettings {
	out := UISettings{
		Macros:       []Macro{},
		MacroButtons: []MacroSlot{},
		Log: LogSettings{
			Filter:     in.Log.Filter,
			Autoscroll: in.Log.Autoscroll,
		},
	}
	if out.Log.Filter == "" {
		out.Log.Filter = "all"
	}
	seenMacros := map[string]bool{}
	for i, m := range in.Macros {
		if m.ID == "" {
			m.ID = fmt.Sprintf("macro-%d", i+1)
		}
		if seenMacros[m.ID] {
			continue
		}
		seenMacros[m.ID] = true
		m.Lines = compactLines(m.Lines)
		if m.CreatedAt.IsZero() {
			m.CreatedAt = now
		}
		if m.UpdatedAt.IsZero() {
			m.UpdatedAt = now
		}
		out.Macros = append(out.Macros, m)
	}
	seenSlots := map[string]bool{}
	seenSlotMacros := map[string]bool{}
	for i, slot := range in.MacroButtons {
		if slot.MacroID == "" || !seenMacros[slot.MacroID] {
			continue
		}
		if seenSlotMacros[slot.MacroID] {
			continue
		}
		if slot.ID == "" {
			slot.ID = fmt.Sprintf("slot-%d", i+1)
		}
		if seenSlots[slot.ID] {
			continue
		}
		seenSlots[slot.ID] = true
		seenSlotMacros[slot.MacroID] = true
		if slot.Region != "toolbar" && slot.Region != "panel" {
			slot.Region = "panel"
		}
		out.MacroButtons = append(out.MacroButtons, slot)
	}
	sort.SliceStable(out.MacroButtons, func(i, j int) bool {
		if out.MacroButtons[i].Region != out.MacroButtons[j].Region {
			return out.MacroButtons[i].Region < out.MacroButtons[j].Region
		}
		return out.MacroButtons[i].Order < out.MacroButtons[j].Order
	})
	out.Gamepad = normalizeGamepadSettings(in.Gamepad, seenMacros)
	return out
}

func defaultUISettings() UISettings {
	return UISettings{
		Macros:       []Macro{},
		MacroButtons: []MacroSlot{},
		Log:          LogSettings{Filter: "all", Autoscroll: true},
		Gamepad:      defaultGamepadSettings(),
	}
}

func defaultGamepadSettings() Gamepad {
	return Gamepad{
		Axes: GamepadAxes{
			X: GamepadAxis{Axis: 0, Scale: 1},
			Y: GamepadAxis{Axis: 1, Invert: true, Scale: 1},
			Z: GamepadAxis{Axis: 3, Invert: true, Scale: 1},
		},
		DeadmanButton: 0,
		SlowButtons:   []int{4, 5},
		MacroButtons:  []GamepadMacroButton{},
	}
}

func normalizeGamepadSettings(in Gamepad, macros map[string]bool) Gamepad {
	d := defaultGamepadSettings()
	out := Gamepad{
		Axes: GamepadAxes{
			X: normalizeGamepadAxis(in.Axes.X, d.Axes.X),
			Y: normalizeGamepadAxis(in.Axes.Y, d.Axes.Y),
			Z: normalizeGamepadAxis(in.Axes.Z, d.Axes.Z),
		},
		DeadmanButton: normalizeGamepadButton(in.DeadmanButton, d.DeadmanButton),
		MacroButtons:  []GamepadMacroButton{},
	}
	if in.SlowButtons == nil {
		out.SlowButtons = append([]int(nil), d.SlowButtons...)
	} else {
		seen := map[int]bool{}
		for _, btn := range in.SlowButtons {
			if btn < 0 || btn > 63 || seen[btn] {
				continue
			}
			seen[btn] = true
			out.SlowButtons = append(out.SlowButtons, btn)
		}
	}
	seenMacroButtons := map[string]bool{}
	seenButtons := map[int]bool{}
	for i, binding := range in.MacroButtons {
		if binding.MacroID == "" || !macros[binding.MacroID] || binding.Button < 0 || binding.Button > 63 || seenButtons[binding.Button] {
			continue
		}
		if binding.ID == "" {
			binding.ID = fmt.Sprintf("gamepad-macro-%d", i+1)
		}
		if seenMacroButtons[binding.ID] {
			continue
		}
		seenMacroButtons[binding.ID] = true
		seenButtons[binding.Button] = true
		out.MacroButtons = append(out.MacroButtons, binding)
	}
	sort.SliceStable(out.MacroButtons, func(i, j int) bool {
		return out.MacroButtons[i].Button < out.MacroButtons[j].Button
	})
	return out
}

func normalizeGamepadAxis(in, def GamepadAxis) GamepadAxis {
	if in.Axis == 0 && !in.Invert && in.Scale == 0 {
		return def
	}
	if in.Scale <= 0 {
		in.Scale = def.Scale
	}
	if in.Scale > 1 {
		in.Scale = 1
	}
	if in.Axis < 0 {
		in.Axis = def.Axis
	}
	return in
}

func normalizeGamepadButton(in, def int) int {
	if in < 0 || in > 63 {
		return def
	}
	return in
}

func compactLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			out = append(out, ln)
		}
	}
	return out
}

func copyUISettings(in UISettings) UISettings {
	out := in
	out.Macros = append([]Macro(nil), in.Macros...)
	if out.Macros == nil {
		out.Macros = []Macro{}
	}
	for i := range out.Macros {
		out.Macros[i].Lines = append([]string(nil), in.Macros[i].Lines...)
	}
	out.MacroButtons = append([]MacroSlot(nil), in.MacroButtons...)
	if out.MacroButtons == nil {
		out.MacroButtons = []MacroSlot{}
	}
	out.Gamepad.SlowButtons = append([]int(nil), in.Gamepad.SlowButtons...)
	if out.Gamepad.SlowButtons == nil {
		out.Gamepad.SlowButtons = []int{}
	}
	out.Gamepad.MacroButtons = append([]GamepadMacroButton(nil), in.Gamepad.MacroButtons...)
	if out.Gamepad.MacroButtons == nil {
		out.Gamepad.MacroButtons = []GamepadMacroButton{}
	}
	return out
}

func clearJobDiagnostics(j *Job) {
	j.BlockedReason = ""
	j.BlockedMessage = ""
	j.BlockedUntil = nil
}
