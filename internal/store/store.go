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
	subs    map[int]chan Event
	nextSub int
}

// Event notifies subscribers of a change, for pushing to the web UI.
type Event struct {
	Kind  string `json:"kind"` // "entry" | "job"
	Entry *Entry `json:"entry,omitempty"`
	Job   *Job   `json:"job,omitempty"`
}

type persisted struct {
	Entries map[string]*Entry `json:"entries"`
	Jobs    []*Job            `json:"jobs"`
	NextJob int64             `json:"next_job"`
	UI      *UISettings       `json:"ui,omitempty"`
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
	s.jobs = p.Jobs
	if p.NextJob > 0 {
		s.nextJob = p.NextJob
	}
	if p.UI != nil {
		s.ui = normalizeUISettings(*p.UI, s.now())
	}
	return s, nil
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
	p := persisted{Entries: s.entries, Jobs: s.jobs, NextJob: s.nextJob, UI: &s.ui}
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
	s.mu.Lock()
	defer s.mu.Unlock()
	e.UpdatedAt = s.now()
	cp := e
	s.entries[e.Path] = &cp
	if err := s.flushLocked(); err != nil {
		return err
	}
	s.publishLocked(Event{Kind: "entry", Entry: &cp})
	return nil
}

// SetEntrySync updates just the sync state (and error) of an entry if present.
func (s *Store) SetEntrySync(path string, state SyncState, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[path]
	if !ok {
		return nil
	}
	e.Sync = state
	e.Error = errMsg
	e.UpdatedAt = s.now()
	if err := s.flushLocked(); err != nil {
		return err
	}
	cp := *e
	s.publishLocked(Event{Kind: "entry", Entry: &cp})
	return nil
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

// DeleteEntry removes an entry from the catalog.
func (s *Store) DeleteEntry(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[path]; !ok {
		return nil
	}
	delete(s.entries, path)
	if err := s.flushLocked(); err != nil {
		return err
	}
	s.publishLocked(Event{Kind: "entry", Entry: &Entry{Path: path, Sync: ""}})
	return nil
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
	s.mu.Lock()
	defer s.mu.Unlock()
	j.ID = s.nextJob
	s.nextJob++
	j.State = Queued
	now := s.now()
	j.CreatedAt = now
	j.UpdatedAt = now
	cp := j
	s.jobs = append(s.jobs, &cp)
	if err := s.flushLocked(); err != nil {
		return Job{}, err
	}
	s.publishLocked(Event{Kind: "job", Job: &cp})
	return cp, nil
}

// SupersedeQueuedUploads marks any still-queued upload job for path as Done
// without running it. A newer upload of the same path has replaced its content,
// so the older queued transfer is obsolete (it would upload superseded bytes
// under a stale MD5). Running jobs are not touched — they hold an open fd to the
// cache file and complete consistently. Returns the count superseded.
func (s *Store) SupersedeQueuedUploads(path string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, j := range s.jobs {
		if j.Kind == JobUpload && j.Path == path && j.State == Queued {
			j.State = Done
			j.UpdatedAt = s.now()
			n++
			cp := *j
			s.publishLocked(Event{Kind: "job", Job: &cp})
		}
	}
	if n > 0 {
		if err := s.flushLocked(); err != nil {
			return n, err
		}
	}
	return n, nil
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
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == id {
			mutate(j)
			j.UpdatedAt = s.now()
			if err := s.flushLocked(); err != nil {
				return err
			}
			cp := *j
			s.publishLocked(Event{Kind: "job", Job: &cp})
			return nil
		}
	}
	return fmt.Errorf("store: job %d not found", id)
}

// ListJobs returns all jobs in queue order.
func (s *Store) ListJobs() []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, *j)
	}
	return out
}

// PruneDoneJobs removes completed jobs older than the given age, keeping the
// queue from growing without bound. Failed jobs are retained for visibility.
func (s *Store) PruneDoneJobs(olderThan time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.now().Add(-olderThan)
	kept := s.jobs[:0]
	for _, j := range s.jobs {
		if j.State == Done && j.UpdatedAt.Before(cutoff) {
			continue
		}
		kept = append(kept, j)
	}
	s.jobs = kept
	return s.flushLocked()
}

// UISettings returns a copy of the durable operator UI settings.
func (s *Store) UISettings() UISettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return copyUISettings(s.ui)
}

// SetUISettings replaces the durable operator UI settings.
func (s *Store) SetUISettings(ui UISettings) (UISettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ui = normalizeUISettings(ui, s.now())
	if err := s.flushLocked(); err != nil {
		return UISettings{}, err
	}
	return copyUISettings(s.ui), nil
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
