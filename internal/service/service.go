// Package service is the application core shared by the HTTP API and the WebDAV
// filesystem driver. It turns user intentions (write this file, delete that
// one) into local cache writes plus durable jobs, and exposes read views of the
// catalog, queue, and machine state. Write-side operations never block on the
// machine — they enqueue work the sync engine performs later.
//
// The one synchronous machine operation here is download-on-demand
// (FetchToCache): reading a file that exists only on the machine must fetch its
// bytes, which inherently waits for the machine to be reachable and idle. It
// goes through the same arbiter as the sync engine, so it still respects the
// single-conversation rule.
package service

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/uwin/cnc-proxy/internal/client"
	"github.com/uwin/cnc-proxy/internal/gcodelog"
	"github.com/uwin/cnc-proxy/internal/machine"
	"github.com/uwin/cnc-proxy/internal/protocol"
	"github.com/uwin/cnc-proxy/internal/quicklz"
	"github.com/uwin/cnc-proxy/internal/runhistory"
	"github.com/uwin/cnc-proxy/internal/session"
	"github.com/uwin/cnc-proxy/internal/store"
)

// GcodeRoot is the machine directory the filesystem exposes. All API paths are
// relative to it.
const GcodeRoot = "/sd/gcodes"

const (
	maxUIMacros      = 48
	maxMacroLines    = 40
	maxMacroLineLen  = 240
	maxMacroNameLen  = 80
	maxMacroDescLen  = 240
	maxMacroButtons  = 96
	maxMacroColorLen = 32
	maxGamepadAxis   = 31
	maxGamepadButton = 63
	maxGamepadMacros = 32
)

// Service wires the store, arbiter (for machine state), and local cache.
type Service struct {
	store    *store.Store
	arb      *session.Arbiter
	cacheDir string

	// gcodeLog records all gcode/console I/O with the machine — lines injected
	// via SendGcode here, plus controller traffic the relay sniffs into the same
	// log — for streaming to web clients.
	gcodeLog *gcodelog.Log

	// runHistory derives recent run summaries from the gcode log and observed
	// status stream. It never performs machine I/O.
	runHistory *runhistory.History

	// activeGcode is the web/API-selected file and cached preview. This mirrors
	// the controller's selected_remote_filename concept and is intentionally
	// proxy-local state; the machine is only touched when RunActiveGcode sends
	// the controller-compatible play command.
	activeMu    sync.Mutex
	activeGcode activeGcodeState

	// commitMu makes a mutation's "publish to cache + update catalog + enqueue
	// job" sequence atomic across concurrent callers, so the cache file, the
	// catalog entry's MD5/size, and the queued job always describe the same
	// content even when the same path is written concurrently.
	commitMu sync.Mutex
}

// New creates a Service, ensuring the cache directory exists.
func New(st *store.Store, arb *session.Arbiter) (*Service, error) {
	cacheDir := st.CacheDir()
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, err
	}
	s := &Service{
		store:      st,
		arb:        arb,
		cacheDir:   cacheDir,
		gcodeLog:   gcodelog.New(500),
		runHistory: runhistory.New(100),
	}
	s.startRunHistoryObservers()
	return s, nil
}

// GcodeLog exposes the shared gcode I/O log so the relay can record controller
// traffic into it and the API can stream it.
func (s *Service) GcodeLog() *gcodelog.Log { return s.gcodeLog }

// RunHistory returns recent observed runs, newest first.
func (s *Service) RunHistory() []runhistory.Run { return s.runHistory.Recent() }

// ClearRunHistory removes retained local run history. It never touches the machine.
func (s *Service) ClearRunHistory() { s.runHistory.Clear() }

func (s *Service) startRunHistoryObservers() {
	if st, _ := s.arb.Tracker().Current(); !st.ObservedAt.IsZero() {
		s.runHistory.ObserveStatus(st)
	}
	statusCh, _ := s.arb.Tracker().Subscribe()
	go func() {
		for st := range statusCh {
			s.runHistory.ObserveStatus(st)
		}
	}()
	gcodeCh, _ := s.gcodeLog.Subscribe()
	go func() {
		for ln := range gcodeCh {
			s.runHistory.ObserveLine(ln)
		}
	}()
}

// MachineStatus is the snapshot returned to clients.
type MachineStatus struct {
	State        machine.State       `json:"state"`
	Mode         string              `json:"mode"`
	Connected    bool                `json:"connected"`
	Reconnecting bool                `json:"reconnecting"`
	PendingJobs  int                 `json:"pending_jobs"`
	AgeMs        int64               `json:"age_ms"`
	ObservedAt   time.Time           `json:"observed_at,omitempty"`
	Stale        bool                `json:"stale"`
	Raw          string              `json:"raw,omitempty"`
	Fields       map[string]string   `json:"fields,omitempty"`
	MPos         machine.AxisValues  `json:"mpos,omitempty"`
	WPos         machine.AxisValues  `json:"wpos,omitempty"`
	Feed         *machine.Triple     `json:"feed,omitempty"`
	Spindle      *machine.Spindle    `json:"spindle,omitempty"`
	Tool         *machine.ToolStatus `json:"tool,omitempty"`
	HaltReason   *machine.HaltReason `json:"halt_reason,omitempty"`
	Progress     []float64           `json:"progress,omitempty"`
	Machine      []float64           `json:"machine,omitempty"`
}

// Status returns the current machine state and proxy mode.
func (s *Service) Status() MachineStatus {
	st, age := s.arb.Tracker().Current()
	observed := !st.ObservedAt.IsZero()
	mode := s.arb.Mode().String()
	stale := !s.arb.Tracker().Fresh(s.arb.StateMaxAge())
	return MachineStatus{
		State:        st.State,
		Mode:         mode,
		Connected:    observed && st.State != machine.Unknown,
		Reconnecting: mode == session.ModeOwner.String() && stale,
		PendingJobs:  s.pendingJobCount(),
		AgeMs:        age.Milliseconds(),
		ObservedAt:   st.ObservedAt,
		Stale:        stale,
		Raw:          st.Raw,
		Fields:       st.Fields,
		MPos:         st.MPos,
		WPos:         st.WPos,
		Feed:         st.Feed,
		Spindle:      st.Spindle,
		Tool:         st.Tool,
		HaltReason:   st.HaltReason,
		Progress:     st.Progress,
		Machine:      st.Machine,
	}
}

func (s *Service) pendingJobCount() int {
	n := 0
	for _, j := range s.store.ListJobs() {
		if j.State == store.Queued || j.State == store.Running {
			n++
		}
	}
	return n
}

// Files returns the catalog.
func (s *Service) Files() []store.Entry { return s.store.ListEntries() }

// Lookup returns the catalog entry for a path (relative or absolute under the
// root), if present.
func (s *Service) Lookup(remotePath string) (store.Entry, bool) {
	remote, err := normalizeRemote(remotePath)
	if err != nil {
		return store.Entry{}, false
	}
	return s.store.GetEntry(remote)
}

// Children returns the entries that are direct children of a directory path.
// The root (GcodeRoot) is implicit and always considered a directory.
func (s *Service) Children(dirPath string) ([]store.Entry, error) {
	dir, err := normalizeRemote(dirPath)
	if err != nil {
		return nil, err
	}
	prefix := dir + "/"
	if dir == GcodeRoot {
		prefix = GcodeRoot + "/"
	}
	var out []store.Entry
	for _, e := range s.store.ListEntries() {
		rest, ok := strings.CutPrefix(e.Path, prefix)
		if !ok || rest == "" {
			continue
		}
		// Direct children only: no further slash in the remainder.
		if strings.Contains(rest, "/") {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// Root returns the machine directory the filesystem is rooted at.
func (s *Service) Root() string { return GcodeRoot }

// PutRemoteOnly records an entry known to exist on the machine but not cached
// locally. Reconciliation (ls/md5sum sweeps) uses this to surface files added
// out-of-band, e.g. by the controller. Reads of such files fetch through the
// arbiter on demand when the machine is reachable and idle.
func (s *Service) PutRemoteOnly(remotePath string, size int64, mtime time.Time, md5hex string) error {
	remote, err := normalizeRemote(remotePath)
	if err != nil {
		return err
	}
	return s.store.PutEntry(store.Entry{
		Path:       remote,
		Size:       size,
		MTime:      mtime,
		MD5:        md5hex,
		CacheState: store.CacheNone,
		Sync:       store.RemoteOnly,
	})
}

const (
	jobDiagBaseBackoff = 2 * time.Second
	jobDiagMaxBackoff  = 60 * time.Second
)

// Jobs returns the job queue with transient operator diagnostics populated.
func (s *Service) Jobs() []store.Job {
	jobs := s.store.ListJobs()
	for i := range jobs {
		s.enrichJob(&jobs[i])
	}
	return jobs
}

// EnrichEventJob returns a copy of a store event with job diagnostics populated.
func (s *Service) EnrichEventJob(ev store.Event) store.Event {
	if ev.Job == nil {
		return ev
	}
	cp := *ev.Job
	s.enrichJob(&cp)
	ev.Job = &cp
	return ev
}

func (s *Service) enrichJob(j *store.Job) {
	switch j.State {
	case store.Done:
		return
	case store.Failed:
		j.BlockedReason = "failed"
		if j.LastError != "" {
			j.BlockedMessage = "Failed: " + j.LastError
		} else {
			j.BlockedMessage = "Failed. Inspect the job error and queue the operation again."
		}
		return
	case store.Running:
		j.BlockedReason = "running"
		j.BlockedMessage = "Running on the machine."
		return
	}

	now := time.Now()
	if j.Attempts > 0 {
		wait := jobDiagBaseBackoff << (j.Attempts - 1)
		if wait > jobDiagMaxBackoff || wait <= 0 {
			wait = jobDiagMaxBackoff
		}
		until := j.UpdatedAt.Add(wait)
		if now.Before(until) {
			j.BlockedReason = "backoff"
			j.BlockedUntil = &until
			j.BlockedMessage = fmt.Sprintf("Backing off after error; retry after %s.", until.Format(time.RFC3339))
			return
		}
	}

	st, _ := s.arb.Tracker().Current()
	if !s.arb.Tracker().Fresh(s.arb.StateMaxAge()) {
		j.BlockedReason = "stale_status"
		j.BlockedMessage = "Refreshing machine status before syncing."
		return
	}
	if !st.State.CanRunFileOps() {
		j.BlockedReason = "not_idle"
		j.BlockedMessage = "Waiting for the machine to be Idle; current state is " + stateLabel(st.State) + "."
		return
	}
	if s.arb.Mode() == session.ModeRelay {
		j.BlockedReason = "relay_active"
		j.BlockedMessage = "Controller is connected; sync will use an injection window between controller transactions."
		return
	}
	j.BlockedReason = "ready"
	j.BlockedMessage = "Ready to sync on the next queue pass."
}

// Subscribe proxies the store's event subscription for SSE.
func (s *Service) Subscribe() (<-chan store.Event, func()) { return s.store.Subscribe() }

// Backup is the portable JSON export for the proxy's local state.
type Backup struct {
	Version    int              `json:"version"`
	ExportedAt time.Time        `json:"exported_at"`
	State      store.Snapshot   `json:"state"`
	GcodeLog   []gcodelog.Line  `json:"gcode_log"`
	RunHistory []runhistory.Run `json:"run_history"`
}

// ExportBackup returns a JSON-serializable copy of state.json, UI settings,
// retained gcode log lines, and observed run history.
func (s *Service) ExportBackup() Backup {
	return Backup{
		Version:    1,
		ExportedAt: time.Now(),
		State:      s.store.Snapshot(),
		GcodeLog:   s.gcodeLog.Recent(),
		RunHistory: s.runHistory.Snapshot(),
	}
}

// ImportBackup replaces local proxy state from a backup export. It only mutates
// local proxy state; machine I/O remains governed by the sync queue and arbiter.
func (s *Service) ImportBackup(b Backup) error {
	if b.Version != 1 {
		return fmt.Errorf("service: unsupported backup version %d", b.Version)
	}
	if err := s.store.Restore(b.State); err != nil {
		return err
	}
	s.gcodeLog.Replace(b.GcodeLog)
	s.runHistory.Replace(b.RunHistory)
	return nil
}

// CachePruneReport summarizes one cache maintenance pass.
type CachePruneReport struct {
	FilesRemoved int   `json:"files_removed"`
	BytesRemoved int64 `json:"bytes_removed"`
}

// RunMaintenance periodically prunes completed jobs and unreferenced cache
// files. It does not touch machine state.
func (s *Service) RunMaintenance(ctx context.Context, interval, doneJobAge, cacheFileAge time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = s.store.PruneDoneJobs(doneJobAge)
			_, _ = s.PruneCache(cacheFileAge)
		}
	}
}

// PruneCache removes old temp files and cache files no catalog entry or active
// job references anymore.
func (s *Service) PruneCache(olderThan time.Duration) (CachePruneReport, error) {
	if olderThan <= 0 {
		olderThan = time.Hour
	}
	cutoff := time.Now().Add(-olderThan)
	referenced := map[string]bool{}
	cacheDir, err := filepath.Abs(s.cacheDir)
	if err != nil {
		return CachePruneReport{}, err
	}
	for _, e := range s.store.ListEntries() {
		s.markReferencedCache(referenced, cacheDir, e.CachePath)
	}
	for _, j := range s.store.ListJobs() {
		if j.State == store.Done || j.State == store.Failed {
			continue
		}
		s.markReferencedCache(referenced, cacheDir, j.CachePath)
	}

	entries, err := os.ReadDir(s.cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return CachePruneReport{}, nil
		}
		return CachePruneReport{}, err
	}
	var report CachePruneReport
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		full := filepath.Join(s.cacheDir, ent.Name())
		abs, err := filepath.Abs(full)
		if err != nil || !strings.HasPrefix(abs, cacheDir+string(os.PathSeparator)) {
			continue
		}
		if referenced[abs] {
			continue
		}
		if err := os.Remove(full); err == nil {
			report.FilesRemoved++
			report.BytesRemoved += info.Size()
		}
	}
	return report, nil
}

func (s *Service) markReferencedCache(ref map[string]bool, cacheDir, p string) {
	if p == "" {
		return
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return
	}
	if abs == cacheDir || !strings.HasPrefix(abs, cacheDir+string(os.PathSeparator)) {
		return
	}
	ref[abs] = true
}

// UISettings returns durable web UI preferences. It is cache-only and never
// touches the machine.
func (s *Service) UISettings() store.UISettings { return s.store.UISettings() }

// SetUISettings validates and persists durable web UI preferences.
func (s *Service) SetUISettings(ui store.UISettings) (store.UISettings, error) {
	if len(ui.Macros) > maxUIMacros {
		return store.UISettings{}, fmt.Errorf("service: at most %d macros are allowed", maxUIMacros)
	}
	if len(ui.MacroButtons) > maxMacroButtons {
		return store.UISettings{}, fmt.Errorf("service: at most %d macro buttons are allowed", maxMacroButtons)
	}
	for i, m := range ui.Macros {
		if strings.TrimSpace(m.Name) == "" {
			return store.UISettings{}, fmt.Errorf("service: macro %d requires a name", i+1)
		}
		if len(m.Name) > maxMacroNameLen {
			return store.UISettings{}, fmt.Errorf("service: macro %q name is too long", m.Name)
		}
		if len(m.Description) > maxMacroDescLen {
			return store.UISettings{}, fmt.Errorf("service: macro %q description is too long", m.Name)
		}
		if len(m.Color) > maxMacroColorLen {
			return store.UISettings{}, fmt.Errorf("service: macro %q color is too long", m.Name)
		}
		if len(m.Lines) > maxMacroLines {
			return store.UISettings{}, fmt.Errorf("service: macro %q has too many lines", m.Name)
		}
		nonBlank := 0
		for _, line := range m.Lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			nonBlank++
			if len(line) > maxMacroLineLen {
				return store.UISettings{}, fmt.Errorf("service: macro %q has a line longer than %d bytes", m.Name, maxMacroLineLen)
			}
		}
		if nonBlank == 0 {
			return store.UISettings{}, fmt.Errorf("service: macro %q requires at least one line", m.Name)
		}
	}
	if err := validateGamepadSettings(ui.Gamepad); err != nil {
		return store.UISettings{}, err
	}
	if ui.Log.Filter == "" {
		ui.Log.Filter = "all"
		current := s.store.UISettings()
		ui.Log.Autoscroll = current.Log.Autoscroll
	}
	return s.store.SetUISettings(ui)
}

func validateGamepadSettings(g store.Gamepad) error {
	for name, axis := range map[string]store.GamepadAxis{"x": g.Axes.X, "y": g.Axes.Y, "z": g.Axes.Z} {
		// Scale 0 means "not supplied" and is normalized to the default by the
		// store. Any explicit non-zero value must stay within the client-side
		// multiplier range.
		if axis.Scale < 0 || axis.Scale > 1 {
			return fmt.Errorf("service: gamepad axis %s scale must be between 0 and 1", name)
		}
		if axis.Axis < 0 || axis.Axis > maxGamepadAxis {
			return fmt.Errorf("service: gamepad axis %s index must be between 0 and %d", name, maxGamepadAxis)
		}
	}
	if g.DeadmanButton < 0 || g.DeadmanButton > maxGamepadButton {
		return fmt.Errorf("service: gamepad deadman button must be between 0 and %d", maxGamepadButton)
	}
	for _, btn := range g.SlowButtons {
		if btn < 0 || btn > maxGamepadButton {
			return fmt.Errorf("service: gamepad slow button must be between 0 and %d", maxGamepadButton)
		}
	}
	if len(g.MacroButtons) > maxGamepadMacros {
		return fmt.Errorf("service: at most %d gamepad macro buttons are allowed", maxGamepadMacros)
	}
	for _, binding := range g.MacroButtons {
		if binding.Button < 0 || binding.Button > maxGamepadButton {
			return fmt.Errorf("service: gamepad macro button must be between 0 and %d", maxGamepadButton)
		}
	}
	return nil
}

// normalizeRemote converts a user-supplied relative or absolute path into a
// machine-absolute path under GcodeRoot, rejecting traversal outside the root.
func normalizeRemote(p string) (string, error) {
	p = strings.ReplaceAll(p, "\\", "/")
	if !strings.HasPrefix(p, "/") {
		p = path.Join(GcodeRoot, p)
	}
	clean := path.Clean(p)
	if clean != GcodeRoot && !strings.HasPrefix(clean, GcodeRoot+"/") {
		return "", fmt.Errorf("service: path %q escapes %s", p, GcodeRoot)
	}
	return clean, nil
}

// cacheNameFor derives a stable cache filename for a remote path.
func (s *Service) cacheNameFor(remote string) string {
	sum := md5.Sum([]byte(remote))
	return filepath.Join(s.cacheDir, hex.EncodeToString(sum[:]))
}

type cacheReplacement struct {
	target        string
	backup        string
	restoreSource string
	hadBackup     bool
	committed     bool
}

func (s *Service) backupCacheTarget(target string) (string, bool, error) {
	if _, err := os.Stat(target); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	backup, err := os.CreateTemp(s.cacheDir, "cache-backup-*.tmp")
	if err != nil {
		return "", false, err
	}
	backupPath := backup.Name()
	if err := backup.Close(); err != nil {
		os.Remove(backupPath)
		return "", false, err
	}
	os.Remove(backupPath)
	if err := os.Rename(target, backupPath); err != nil {
		return "", false, err
	}
	return backupPath, true, nil
}

func (s *Service) replaceCacheFile(staged, target string) (*cacheReplacement, error) {
	backup, hadBackup, err := s.backupCacheTarget(target)
	if err != nil {
		return nil, err
	}
	if err := os.Rename(staged, target); err != nil {
		if hadBackup {
			_ = os.Rename(backup, target)
		}
		return nil, err
	}
	return &cacheReplacement{target: target, backup: backup, hadBackup: hadBackup}, nil
}

func (s *Service) moveCacheFile(source, target string) (*cacheReplacement, error) {
	if source == target {
		return &cacheReplacement{committed: true}, nil
	}
	backup, hadBackup, err := s.backupCacheTarget(target)
	if err != nil {
		return nil, err
	}
	if err := os.Rename(source, target); err != nil {
		if hadBackup {
			_ = os.Rename(backup, target)
		}
		return nil, err
	}
	return &cacheReplacement{target: target, backup: backup, restoreSource: source, hadBackup: hadBackup}, nil
}

func (r *cacheReplacement) Commit() {
	if r == nil || r.committed {
		return
	}
	if r.hadBackup {
		_ = os.Remove(r.backup)
	}
	r.committed = true
}

func (r *cacheReplacement) Rollback() {
	if r == nil || r.committed {
		return
	}
	if r.restoreSource != "" {
		_ = os.Rename(r.target, r.restoreSource)
	} else {
		_ = os.Remove(r.target)
	}
	if r.hadBackup {
		_ = os.Rename(r.backup, r.target)
	}
	r.committed = true
}

// Upload writes content to the local cache immediately and enqueues an upload
// job. The returned entry is available at once (Sync = pending_upload) — the
// Google-Drive behavior. The machine transfer happens later via the engine.
func (s *Service) Upload(remotePath string, r io.Reader) (store.Entry, error) {
	remote, err := normalizeRemote(remotePath)
	if err != nil {
		return store.Entry{}, err
	}

	cachePath := s.cacheNameFor(remote)
	// Stage into a unique temp file in the cache dir so concurrent writes to the
	// same path can't corrupt a shared temp or race on the rename. CreateTemp in
	// the cache dir keeps the final rename on the same filesystem (atomic).
	f, err := os.CreateTemp(s.cacheDir, "upload-*.tmp")
	if err != nil {
		return store.Entry{}, err
	}
	tmp := f.Name()
	h := md5.New()
	size, err := io.Copy(io.MultiWriter(f, h), r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return store.Entry{}, err
	}
	md5hex := hex.EncodeToString(h.Sum(nil))

	// Commit atomically: rename the staged temp over the cache file, record the
	// catalog entry, supersede any older still-queued upload of this path (its
	// content is now stale), and enqueue this upload. Serialized so concurrent
	// writers to the same path can't leave the cache file, entry MD5, and job
	// describing different content.
	s.commitMu.Lock()
	defer s.commitMu.Unlock()

	replacement, err := s.replaceCacheFile(tmp, cachePath)
	if err != nil {
		os.Remove(tmp)
		return store.Entry{}, err
	}
	defer replacement.Rollback()

	entry := store.Entry{
		Path:       remote,
		Size:       size,
		MTime:      time.Now(),
		MD5:        md5hex,
		CachePath:  cachePath,
		CacheState: store.CacheReady,
		Sync:       store.PendingUpload,
	}
	if err := s.store.Batch(func(b *store.Batch) error {
		b.PutEntry(entry)
		b.DiscardJobs(remote, store.JobDelete)
		b.SupersedeQueuedUploads(remote)
		b.Enqueue(store.Job{
			Kind:      store.JobUpload,
			Path:      remote,
			CachePath: cachePath,
			MD5:       md5hex,
			Size:      size,
		})
		return nil
	}); err != nil {
		return store.Entry{}, err
	}
	replacement.Commit()
	return entry, nil
}

// UploadRange stages one Content-Range PUT from WebDAV. Incomplete contiguous
// ranges are kept local_only and are not queued for the machine; the upload job
// is queued only after the final byte has arrived.
func (s *Service) UploadRange(remotePath string, start, end, total int64, r io.Reader) (store.Entry, bool, error) {
	if start < 0 || end < start || total <= 0 || end >= total {
		return store.Entry{}, false, fmt.Errorf("service: invalid upload range")
	}
	remote, err := normalizeRemote(remotePath)
	if err != nil {
		return store.Entry{}, false, err
	}

	expected := end - start + 1
	part, err := os.CreateTemp(s.cacheDir, "upload-range-part-*.tmp")
	if err != nil {
		return store.Entry{}, false, err
	}
	partPath := part.Name()
	n, err := io.Copy(part, r)
	if cerr := part.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(partPath)
		return store.Entry{}, false, err
	}
	if n != expected {
		os.Remove(partPath)
		return store.Entry{}, false, fmt.Errorf("service: upload range length %d does not match Content-Range length %d", n, expected)
	}
	defer os.Remove(partPath)

	s.commitMu.Lock()
	defer s.commitMu.Unlock()

	cachePath := s.cacheNameFor(remote)
	entry, complete, err := s.mergeUploadRange(remote, cachePath, partPath, start, end, total)
	if err != nil {
		return store.Entry{}, false, err
	}
	return entry, complete, nil
}

func (s *Service) mergeUploadRange(remote, cachePath, partPath string, start, end, total int64) (store.Entry, bool, error) {
	merge, err := os.CreateTemp(s.cacheDir, "upload-range-*.tmp")
	if err != nil {
		return store.Entry{}, false, err
	}
	mergePath := merge.Name()
	cleanup := true
	defer func() {
		merge.Close()
		if cleanup {
			os.Remove(mergePath)
		}
	}()

	currentSize := int64(0)
	if existing, ok := s.store.GetEntry(remote); ok && existing.CachePath != "" {
		old, err := os.Open(existing.CachePath)
		if err != nil {
			return store.Entry{}, false, err
		}
		currentSize, err = io.Copy(merge, old)
		closeErr := old.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return store.Entry{}, false, err
		}
	}
	if start > currentSize {
		return store.Entry{}, false, fmt.Errorf("service: upload range starts at %d with only %d contiguous bytes staged", start, currentSize)
	}

	part, err := os.Open(partPath)
	if err != nil {
		return store.Entry{}, false, err
	}
	if _, err := merge.Seek(start, io.SeekStart); err != nil {
		part.Close()
		return store.Entry{}, false, err
	}
	written, err := io.Copy(merge, part)
	closeErr := part.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return store.Entry{}, false, err
	}
	if written != end-start+1 {
		return store.Entry{}, false, fmt.Errorf("service: staged upload range changed size")
	}

	contiguous := currentSize
	if end+1 > contiguous {
		contiguous = end + 1
	}
	complete := contiguous >= total
	stagedSize := contiguous
	if complete {
		stagedSize = total
	}
	if err := merge.Truncate(stagedSize); err != nil {
		return store.Entry{}, false, err
	}
	if err := merge.Close(); err != nil {
		return store.Entry{}, false, err
	}
	replacement, err := s.replaceCacheFile(mergePath, cachePath)
	if err != nil {
		return store.Entry{}, false, err
	}
	cleanup = false
	defer replacement.Rollback()

	entry := store.Entry{
		Path:       remote,
		Size:       stagedSize,
		MTime:      time.Now(),
		CachePath:  cachePath,
		CacheState: store.CacheReady,
		Sync:       store.LocalOnly,
	}
	if complete {
		md5hex, err := fileMD5(cachePath, total)
		if err != nil {
			return store.Entry{}, false, err
		}
		entry.MD5 = md5hex
		entry.Sync = store.PendingUpload
	}
	if err := s.store.Batch(func(b *store.Batch) error {
		b.PutEntry(entry)
		b.DiscardJobs(remote, store.JobDelete)
		b.SupersedeQueuedUploads(remote)
		if complete {
			b.Enqueue(store.Job{
				Kind:      store.JobUpload,
				Path:      remote,
				CachePath: cachePath,
				MD5:       entry.MD5,
				Size:      total,
			})
		}
		return nil
	}); err != nil {
		return store.Entry{}, false, err
	}
	replacement.Commit()
	return entry, complete, nil
}

func fileMD5(path string, wantSize int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", err
	}
	if n != wantSize {
		return "", fmt.Errorf("service: staged upload size %d does not match Content-Range total %d", n, wantSize)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Service) discardQueuedDeletesForWrite(remote string) error {
	_, _, err := s.store.DiscardJobs(remote, store.JobDelete)
	return err
}

// gcodeReplyCap bounds a reply-expected query (M114, version, $G, M503, …). The
// firmware answers these promptly, so this is only a safety net; the read
// actually terminates on the reply's quiescence well before this.
const gcodeReplyCap = 30 * time.Second

const recoveryStatusTimeout = 2 * time.Second

// RecoveryResult is returned by explicit alarm-recovery actions so operators
// can see what was sent and what the machine reported afterward.
type RecoveryResult struct {
	Action     string              `json:"action"`
	Commands   []string            `json:"commands,omitempty"`
	Output     []string            `json:"output,omitempty"`
	State      machine.State       `json:"state"`
	HaltReason *machine.HaltReason `json:"halt_reason,omitempty"`
	Recovered  bool                `json:"recovered"`
	NeedsHome  bool                `json:"needs_home,omitempty"`
	Message    string              `json:"message"`
}

// SendGcode runs a single gcode line on the machine and returns the machine's
// output (the payload of an "ok <payload>" reply, or output lines for a no-"ok"
// reply; empty for fire-and-forget commands). It works in both owner mode and
// relay mode (injected between the controller's transactions).
//
// protocol.ClassifyGcode is the single source of truth for two independent
// decisions, both grounded in hardware-verified firmware behavior:
//
//   - requiresIdle: read-only queries (M114, version, $G, bare M220, …) run
//     regardless of machine state — observing state while a program runs is
//     legitimate. Motion, modal/state changes, dual-nature SETs, and SD I/O
//     require a fresh Idle machine and return session.ErrNotIdle otherwise
//     (HTTP 503, retryable), so the proxy can never disturb a running program.
//   - resp: whether the firmware will reply at all. Reply-expected commands are
//     read to quiescence; fire-and-forget commands (which the firmware never
//     acks over WiFi) are written and only briefly drained for a late error.
//     This is why an injected move no longer blocks waiting for an ack that
//     never comes (the original "second move hangs" bug).
//
// In relay mode it additionally returns session.ErrBusy if the controller is
// mid file-transfer; that too is retryable, not a failure.
func (s *Service) SendGcode(line string) (string, error) {
	s.gcodeLog.Append(gcodelog.DirSend, gcodelog.SourceAPI, line)
	resp, requireIdle := protocol.ClassifyGcode(line)
	var out string
	err := s.arb.WithMachine(requireIdle, func(c *client.Conn) error {
		o, e := c.SendGcodeLine(line, client.GcodeOpts{
			ExpectReply: resp == protocol.ReplyExpected,
			Cap:         gcodeReplyCap,
		})
		out = o
		return e
	})
	if out != "" {
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, out)
	}
	if err != nil {
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, "error: "+err.Error())
	} else if out == "" {
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, "ok")
	}
	return out, err
}

// Control characters accepted by SendControl (mirrors the protocol constants).
const (
	ControlFeedHold = protocol.CtrlFeedHold
	ControlResume   = protocol.CtrlResume
	ControlHalt     = protocol.CtrlHalt
)

// SendControl injects a realtime control character. These are out-of-band on
// the firmware (acted upon immediately from its receive path, independent of
// the gcode stream), so unlike SendGcode they are NOT idle-gated and — crucially
// — they do NOT take the arbiter's transaction lock: feed-hold, resume, and
// emergency-halt must work precisely WHILE the machine is moving, including
// preempting a blocking move or a program a connected controller started. The
// same policy intentionally applies during controller file transfers: the
// firmware's file parser still accepts standalone CTRL_SINGLE realtime frames,
// and the relay writes those frames without entering the injection window.
// Errors here mean an unsupported control or no live path to the machine.
func (s *Service) SendControl(c byte) error {
	label, ok := protocol.ControlLabel(c)
	if !ok {
		return fmt.Errorf("service: unsupported control character %#x", c)
	}
	s.gcodeLog.Append(gcodelog.DirSend, gcodelog.SourceAPI, label)
	err := s.arb.SendControl(c)
	if err != nil {
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, "error: "+err.Error())
	}
	return err
}

// RecoverAlarm sends one of the firmware's explicit alarm recovery commands and
// verifies the observed state afterward when the command is expected to clear an
// alarm. This is separate from generic gcode because recovery must be possible
// while the state is Alarm, where normal state-changing gcode is intentionally
// blocked by idle gating. It still goes through the arbiter, so it never
// interleaves with another normal machine conversation.
func (s *Service) RecoverAlarm(action string) (RecoveryResult, error) {
	action = strings.ToLower(strings.TrimSpace(action))
	st, err := s.recoveryStatus()
	if err != nil {
		return RecoveryResult{Action: action, Message: err.Error()}, err
	}
	res := recoveryResult(action, st)
	if st.State != machine.Alarm && !(action == "home" && st.State == machine.Idle) {
		err := fmt.Errorf("%w: machine is %s, not Alarm", ErrRecoveryUnavailable, stateLabel(st.State))
		res.Message = err.Error()
		return res, err
	}

	switch action {
	case "recover":
		return s.recoverAlarmGuided(st, res)
	case "unlock":
		if st.HaltReason != nil && st.HaltReason.Recovery != "unlock" {
			err := fmt.Errorf("%w: H:%d %s requires %s", ErrRecoveryUnavailable, st.HaltReason.Code, st.HaltReason.Message, st.HaltReason.Recovery)
			res.Message = err.Error()
			return res, err
		}
		return s.recoverAlarmUnlock(st, res)
	case "home":
		if st.State == machine.Alarm && st.HaltReason != nil && st.HaltReason.Recovery != "unlock" {
			err := fmt.Errorf("%w: H:%d %s requires %s before homing", ErrRecoveryUnavailable, st.HaltReason.Code, st.HaltReason.Message, st.HaltReason.Recovery)
			res.Message = err.Error()
			return res, err
		}
		return s.recoverAlarmHome(res)
	case "reset":
		if st.HaltReason != nil && st.HaltReason.Recovery == "power_cycle" {
			err := fmt.Errorf("%w: H:%d %s requires power cycle", ErrRecoveryUnavailable, st.HaltReason.Code, st.HaltReason.Message)
			res.Message = err.Error()
			return res, err
		}
		return s.recoverAlarmReset(res)
	default:
		err := fmt.Errorf("service: recovery action must be one of: recover, unlock, home, reset")
		res.Message = err.Error()
		return res, err
	}
}

func (s *Service) recoverAlarmGuided(st machine.Status, res RecoveryResult) (RecoveryResult, error) {
	if st.HaltReason == nil {
		err := fmt.Errorf("%w: alarm has no H: reason; inspect the machine before recovery", ErrRecoveryUnavailable)
		res.Message = err.Error()
		return res, err
	}
	switch st.HaltReason.Recovery {
	case "unlock":
		return s.recoverAlarmUnlock(st, res)
	case "reset":
		return s.recoverAlarmReset(res)
	case "power_cycle":
		err := fmt.Errorf("%w: H:%d %s requires power cycle", ErrRecoveryUnavailable, st.HaltReason.Code, st.HaltReason.Message)
		res.Message = err.Error()
		return res, err
	default:
		err := fmt.Errorf("%w: H:%d %s requires inspection before recovery", ErrRecoveryUnavailable, st.HaltReason.Code, st.HaltReason.Message)
		res.Message = err.Error()
		return res, err
	}
}

func (s *Service) recoverAlarmUnlock(start machine.Status, res RecoveryResult) (RecoveryResult, error) {
	err := s.arb.WithMachine(false, func(c *client.Conn) error {
		st, err := s.runRecoveryCommand(c, &res, "$X")
		if err != nil {
			return err
		}
		res = updateRecoveryResult(res, st)
		if st.State != machine.Alarm {
			return nil
		}
		if start.HaltReason == nil || start.HaltReason.Code != 10 {
			return fmt.Errorf("%w: unlock command sent, but machine still reports %s", ErrRecoveryUnavailable, statusSummary(st))
		}
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, "H:10 still Alarm after $X; trying firmware M999 fallback")
		st, err = s.runRecoveryCommand(c, &res, "M999")
		if err != nil {
			return err
		}
		res = updateRecoveryResult(res, st)
		return nil
	})
	if err != nil {
		res.Message = err.Error()
		return res, err
	}
	if res.State == machine.Alarm {
		err := fmt.Errorf("%w: recovery commands sent, but machine still reports %s", ErrRecoveryUnavailable, statusSummary(machine.Status{State: res.State, HaltReason: res.HaltReason}))
		res.Message = err.Error()
		return res, err
	}
	res.Recovered = true
	res.NeedsHome = true
	res.Message = "Alarm cleared. Home the machine before moving or cutting."
	return res, nil
}

func (s *Service) recoverAlarmHome(res RecoveryResult) (RecoveryResult, error) {
	err := s.arb.WithMachine(false, func(c *client.Conn) error {
		st, err := s.runRecoveryCommand(c, &res, "$H")
		if err != nil {
			return err
		}
		res = updateRecoveryResult(res, st)
		return nil
	})
	if err != nil {
		res.Message = err.Error()
		return res, err
	}
	if res.State == machine.Alarm {
		err := fmt.Errorf("%w: home command sent, but machine still reports %s", ErrRecoveryUnavailable, statusSummary(machine.Status{State: res.State, HaltReason: res.HaltReason}))
		res.Message = err.Error()
		return res, err
	}
	res.Recovered = true
	res.Message = "Home command sent and the machine is no longer in Alarm."
	return res, nil
}

func (s *Service) recoverAlarmReset(res RecoveryResult) (RecoveryResult, error) {
	err := s.arb.WithMachine(false, func(c *client.Conn) error {
		s.recordRecoverySend(&res, "reset")
		if err := c.WriteConsoleCommand("reset"); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		res.Message = err.Error()
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, "error: "+err.Error())
		return res, err
	}
	s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, "reset sent")
	res.Message = "Reset command sent. Wait for reconnect, then home the machine."
	return res, nil
}

func (s *Service) runRecoveryCommand(c *client.Conn, res *RecoveryResult, line string) (machine.Status, error) {
	s.recordRecoverySend(res, line)
	out, err := c.SendConsoleCommand(line, client.GcodeOpts{Cap: recoveryStatusTimeout})
	if out != "" {
		res.Output = append(res.Output, out)
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, out)
	}
	if err != nil {
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, "error: "+err.Error())
		return machine.Status{}, err
	}
	if out == "" {
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, "ok")
	}
	return s.queryRecoveryStatus(c)
}

func (s *Service) recordRecoverySend(res *RecoveryResult, line string) {
	res.Commands = append(res.Commands, line)
	s.gcodeLog.Append(gcodelog.DirSend, gcodelog.SourceAPI, "recovery "+res.Action+" ("+line+")")
}

func (s *Service) queryRecoveryStatus(c *client.Conn) (machine.Status, error) {
	payload, err := c.QueryState(recoveryStatusTimeout)
	if err != nil {
		return machine.Status{}, err
	}
	if !s.arb.Tracker().ObserveStatusPayload(payload) {
		return machine.Status{}, fmt.Errorf("%w: machine returned malformed status", ErrMachineStatusStale)
	}
	st, _ := s.arb.Tracker().Current()
	return st, nil
}

func (s *Service) recoveryStatus() (machine.Status, error) {
	st, _ := s.arb.Tracker().Current()
	if s.arb.Tracker().Fresh(s.arb.StateMaxAge()) {
		return st, nil
	}
	err := s.arb.WithMachine(false, func(c *client.Conn) error {
		var queryErr error
		st, queryErr = s.queryRecoveryStatus(c)
		return queryErr
	})
	if err != nil {
		return machine.Status{}, err
	}
	st, _ = s.arb.Tracker().Current()
	if !s.arb.Tracker().Fresh(s.arb.StateMaxAge()) {
		return machine.Status{}, ErrMachineStatusStale
	}
	return st, nil
}

func recoveryResult(action string, st machine.Status) RecoveryResult {
	return updateRecoveryResult(RecoveryResult{Action: action}, st)
}

func updateRecoveryResult(res RecoveryResult, st machine.Status) RecoveryResult {
	res.State = st.State
	if st.HaltReason != nil {
		reason := *st.HaltReason
		res.HaltReason = &reason
	} else {
		res.HaltReason = nil
	}
	return res
}

func statusSummary(st machine.Status) string {
	if st.State == machine.Alarm && st.HaltReason != nil {
		return fmt.Sprintf("Alarm H:%d: %s", st.HaltReason.Code, st.HaltReason.Message)
	}
	return stateLabel(st.State)
}

func stateLabel(st machine.State) string {
	if st == machine.Unknown {
		return "Unknown"
	}
	return string(st)
}

// Delete removes local-only desired state immediately, or enqueues a machine
// delete for entries that may exist remotely.
func (s *Service) Delete(remotePath string) error {
	remote, err := normalizeRemote(remotePath)
	if err != nil {
		return err
	}
	entry, ok := s.store.GetEntry(remote)
	if !ok {
		return ErrNotFound
	}
	if s.shouldDiscardLocalEntry(remote, entry) {
		discarded, ok, err := s.store.DiscardEntry(remote, store.JobUpload, store.JobMkdir, store.JobDelete, store.JobRename)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}
		if discarded.CachePath != "" {
			os.Remove(discarded.CachePath)
		}
		return nil
	}
	return s.store.Batch(func(b *store.Batch) error {
		if _, ok := b.SetEntrySync(remote, store.PendingDelete, ""); !ok {
			return ErrNotFound
		}
		b.Enqueue(store.Job{Kind: store.JobDelete, Path: remote})
		return nil
	})
}

// RetryJob requeues a failed sync job and restores the catalog state to the
// corresponding in-flight state so the UI no longer shows the stale error.
func (s *Service) RetryJob(id int64) (store.Job, error) {
	job, ok := s.store.GetJob(id)
	if !ok {
		return store.Job{}, ErrNotFound
	}
	if job.State != store.Failed {
		return store.Job{}, ErrRetryUnavailable
	}
	var retried store.Job
	err := s.store.Batch(func(b *store.Batch) error {
		current, ok := b.GetJob(id)
		if !ok {
			return ErrNotFound
		}
		if current.State != store.Failed {
			return ErrRetryUnavailable
		}
		if err := s.restoreEntryStateForRetryBatch(b, current); err != nil {
			return err
		}
		var retryOK bool
		retried, retryOK = b.RetryJob(id)
		if !retryOK {
			return ErrRetryUnavailable
		}
		return nil
	})
	if err != nil {
		return store.Job{}, err
	}
	return retried, nil
}

func (s *Service) restoreEntryStateForRetry(job store.Job) error {
	return s.store.Batch(func(b *store.Batch) error {
		return s.restoreEntryStateForRetryBatch(b, job)
	})
}

func (s *Service) restoreEntryStateForRetryBatch(b *store.Batch, job store.Job) error {
	switch job.Kind {
	case store.JobUpload:
		if _, ok := b.GetEntry(job.Path); !ok {
			b.PutEntry(store.Entry{
				Path:       job.Path,
				Size:       job.Size,
				MD5:        job.MD5,
				CachePath:  job.CachePath,
				CacheState: store.CacheReady,
				MTime:      time.Now(),
				Sync:       store.PendingUpload,
			})
			return nil
		}
		b.SetEntrySync(job.Path, store.PendingUpload, "")
		return nil
	case store.JobMkdir:
		if _, ok := b.GetEntry(job.Path); !ok {
			b.PutEntry(store.Entry{Path: job.Path, IsDir: true, MTime: time.Now(), Sync: store.PendingUpload})
			return nil
		}
		b.SetEntrySync(job.Path, store.PendingUpload, "")
		return nil
	case store.JobDelete:
		b.SetEntrySync(job.Path, store.PendingDelete, "")
		return nil
	case store.JobRename:
		b.SetEntrySync(job.Path, store.PendingRename, "")
		return nil
	default:
		return ErrRetryUnavailable
	}
}

// DiscardLocal removes unsettled local catalog state and any queued/failed jobs
// for the path without touching the machine. If the file actually exists on the
// machine, the reconcile pass will rediscover it as remote_only.
func (s *Service) DiscardLocal(remotePath string) error {
	remote, err := normalizeRemote(remotePath)
	if err != nil {
		return err
	}
	entry, ok := s.store.GetEntry(remote)
	if !ok {
		return s.discardJobsWithoutEntry(remote)
	}
	if !canDiscardLocal(entry.Sync) || s.hasRunningJob(remote) {
		return ErrDiscardUnavailable
	}
	discarded, ok, err := s.store.DiscardEntry(remote, store.JobUpload, store.JobMkdir, store.JobDelete, store.JobRename)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	if discarded.CachePath != "" {
		os.Remove(discarded.CachePath)
	}
	return nil
}

func (s *Service) discardJobsWithoutEntry(remote string) error {
	if s.hasRunningJob(remote) {
		return ErrDiscardUnavailable
	}
	discarded, ok, err := s.store.DiscardJobs(remote, store.JobUpload, store.JobMkdir, store.JobDelete, store.JobRename)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	for _, job := range discarded {
		if job.CachePath != "" {
			os.Remove(job.CachePath)
		}
	}
	return nil
}

func canDiscardLocal(sync store.SyncState) bool {
	switch sync {
	case store.LocalOnly, store.PendingUpload, store.Uploading, store.PendingDelete, store.Deleting, store.PendingRename, store.Error:
		return true
	default:
		return false
	}
}

func (s *Service) shouldDiscardLocalEntry(remote string, entry store.Entry) bool {
	switch entry.Sync {
	case store.LocalOnly, store.PendingUpload:
		return true
	case store.Error:
		return s.hasLocalCreateJob(remote)
	default:
		return false
	}
}

func (s *Service) hasLocalCreateJob(remote string) bool {
	for _, j := range s.store.ListJobs() {
		if j.Path != remote || (j.Kind != store.JobUpload && j.Kind != store.JobMkdir) {
			continue
		}
		if j.State == store.Queued || j.State == store.Failed {
			return true
		}
	}
	return false
}

func (s *Service) hasRunningJob(remote string) bool {
	for _, j := range s.store.ListJobs() {
		if j.Path == remote && j.State == store.Running {
			return true
		}
	}
	return false
}

// Rename enqueues a machine rename for synced/remote files. If the source is a
// not-yet-synced local upload, move the cached content locally and enqueue an
// upload for the destination instead; the machine has nothing to rename yet.
func (s *Service) Rename(fromPath, toPath string) error {
	from, err := normalizeRemote(fromPath)
	if err != nil {
		return err
	}
	to, err := normalizeRemote(toPath)
	if err != nil {
		return err
	}
	entry, ok := s.store.GetEntry(from)
	if !ok {
		return ErrNotFound
	}
	if s.canRenameLocalUpload(from, entry) {
		return s.renameLocalUpload(from, to, entry)
	}
	return s.enqueueRemoteRename(from, to)
}

func (s *Service) canRenameLocalUpload(from string, entry store.Entry) bool {
	return !entry.IsDir && entry.CachePath != "" && s.hasLocalCreateJob(from) && !s.hasRunningJob(from)
}

func (s *Service) renameLocalUpload(from, to string, entry store.Entry) error {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()

	// Re-read under commitMu in case a concurrent upload/delete changed it while
	// Rename was normalizing paths.
	current, ok := s.store.GetEntry(from)
	if !ok {
		return ErrNotFound
	}
	if !s.canRenameLocalUpload(from, current) {
		return s.enqueueRemoteRename(from, to)
	}
	entry = current

	cachePath := s.cacheNameFor(to)
	var replacement *cacheReplacement
	if entry.CachePath != cachePath {
		var err error
		replacement, err = s.moveCacheFile(entry.CachePath, cachePath)
		if err != nil {
			return err
		}
		defer replacement.Rollback()
	}

	entry.Path = to
	entry.CachePath = cachePath
	entry.CacheState = store.CacheReady
	entry.CacheCheckedAt = time.Time{}
	entry.Sync = store.PendingUpload
	entry.Error = ""
	entry.MTime = time.Now()
	if err := s.store.Batch(func(b *store.Batch) error {
		b.DiscardJobs(from, store.JobUpload, store.JobMkdir)
		b.DeleteEntry(from)
		b.PutEntry(entry)
		b.SupersedeQueuedUploads(to)
		b.Enqueue(store.Job{
			Kind:      store.JobUpload,
			Path:      to,
			CachePath: cachePath,
			MD5:       entry.MD5,
			Size:      entry.Size,
		})
		return nil
	}); err != nil {
		return err
	}
	if replacement != nil {
		replacement.Commit()
	}
	return nil
}

func (s *Service) enqueueRemoteRename(from, to string) error {
	return s.store.Batch(func(b *store.Batch) error {
		if _, ok := b.SetEntrySync(from, store.PendingRename, ""); !ok {
			return ErrNotFound
		}
		b.Enqueue(store.Job{Kind: store.JobRename, Path: from, DestPath: to})
		return nil
	})
}

// Mkdir enqueues a directory creation and records a directory entry.
func (s *Service) Mkdir(remotePath string) (store.Entry, error) {
	remote, err := normalizeRemote(remotePath)
	if err != nil {
		return store.Entry{}, err
	}
	entry := store.Entry{Path: remote, IsDir: true, MTime: time.Now(), Sync: store.PendingUpload}
	if err := s.store.Batch(func(b *store.Batch) error {
		entry = b.PutEntry(entry)
		b.Enqueue(store.Job{Kind: store.JobMkdir, Path: remote})
		return nil
	}); err != nil {
		return store.Entry{}, err
	}
	return entry, nil
}

// ReadCache opens the cached content of a file for reading, if present.
func (s *Service) ReadCache(remotePath string) (io.ReadCloser, store.Entry, error) {
	remote, err := normalizeRemote(remotePath)
	if err != nil {
		return nil, store.Entry{}, err
	}
	entry, ok := s.store.GetEntry(remote)
	if !ok {
		return nil, store.Entry{}, ErrNotFound
	}
	if entry.CachePath == "" || entry.CacheState == store.CacheNone {
		return nil, entry, ErrNotCached
	}
	if entry.CacheState == store.CacheValidating || (entry.CacheState == "" && entry.Sync == store.Synced) {
		return nil, entry, ErrCacheValidationPending
	}
	f, err := os.Open(entry.CachePath)
	if err != nil {
		return nil, entry, ErrNotCached
	}
	return f, entry, nil
}

// OpenDownloadCache exposes complete cache-ready file content for the relay's
// controller download path. It deliberately refuses incomplete range uploads,
// validation-pending files, directories, and entries being deleted or renamed.
func (s *Service) OpenDownloadCache(remotePath string) (io.ReaderAt, io.Closer, int64, string, error) {
	rc, entry, err := s.ReadCache(remotePath)
	if err != nil {
		return nil, nil, 0, "", err
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = rc.Close()
		}
	}()
	if entry.IsDir || entry.MD5 == "" {
		return nil, nil, 0, "", ErrNotCached
	}
	switch entry.Sync {
	case store.PendingDelete, store.Deleting, store.PendingRename, store.RemoteOnly:
		return nil, nil, 0, "", ErrNotCached
	}
	f, ok := rc.(*os.File)
	if !ok {
		return nil, nil, 0, "", ErrNotCached
	}
	info, err := f.Stat()
	if err != nil {
		return nil, nil, 0, "", err
	}
	if info.Size() != entry.Size {
		return nil, nil, 0, "", ErrNotCached
	}
	closeOnError = false
	return f, f, entry.Size, entry.MD5, nil
}

// downloadTimeout bounds a single download-on-demand transfer.
const downloadTimeout = 5 * time.Minute

// Open returns a reader for a file's content, fetching it from the machine on
// demand if it is known but not yet cached (remote_only). Unlike ReadCache it
// may block while the machine sends the file, but only when the cache misses.
func (s *Service) Open(remotePath string) (io.ReadCloser, store.Entry, error) {
	rc, entry, err := s.ReadCache(remotePath)
	if err == nil {
		return rc, entry, nil
	}
	if !errors.Is(err, ErrNotCached) {
		return nil, entry, err
	}
	// Cache miss for a known file: fetch it, then serve from cache.
	if err := s.FetchToCache(entry.Path); err != nil {
		return nil, entry, err
	}
	return s.ReadCache(entry.Path)
}

// FetchToCache downloads a file from the machine into the local cache and marks
// it synced. It is used for download-on-demand reads of remote_only files. It
// goes through the arbiter, so it waits for owner mode and an idle machine and
// returns session.ErrRelayActive / session.ErrNotIdle when those aren't met.
func (s *Service) FetchToCache(remotePath string) error {
	remote, err := normalizeRemote(remotePath)
	if err != nil {
		return err
	}
	entry, ok := s.store.GetEntry(remote)
	if !ok {
		return ErrNotFound
	}
	if entry.IsDir {
		return errors.New("service: cannot download a directory")
	}

	cachePath := s.cacheNameFor(remote)
	// Stage into a unique temp file so a concurrent fetch/upload of the same
	// path can't collide on a shared name.
	f, err := os.CreateTemp(s.cacheDir, "download-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()

	var remoteMD5 string
	derr := s.arb.WithMachine(true, func(c *client.Conn) error {
		md5hex, _, dErr := c.Download(remote, f, downloadTimeout, nil)
		remoteMD5 = md5hex
		return dErr
	})
	f.Close()
	if derr != nil {
		os.Remove(tmp)
		return derr
	}

	// The machine reports the MD5 of the UNCOMPRESSED content. If a .lz sidecar
	// existed it sent compressed bytes, so the raw download won't match — in
	// that case decompress in place. We detect this by comparing MD5s rather
	// than guessing from magic bytes.
	raw, err := os.ReadFile(tmp)
	if err != nil {
		os.Remove(tmp)
		return err
	}
	content := raw
	if remoteMD5 != "" && md5hex(raw) != remoteMD5 {
		var dec bytes.Buffer
		if derr := quicklz.DecompressStream(&dec, raw); derr == nil && md5hex(dec.Bytes()) == remoteMD5 {
			content = dec.Bytes()
		}
		// If decompression didn't help, fall through and store what we got;
		// the size/MD5 will still be recorded from the actual content.
	}
	// Write the final content atomically: stage to a sibling temp then rename,
	// so a concurrent reader of cachePath never sees a partial file.
	final, err := os.CreateTemp(s.cacheDir, "fetched-*.tmp")
	if err != nil {
		os.Remove(tmp)
		return err
	}
	finalTmp := final.Name()
	_, werr := final.Write(content)
	if cerr := final.Close(); werr == nil {
		werr = cerr
	}
	os.Remove(tmp)
	if werr != nil {
		os.Remove(finalTmp)
		return werr
	}

	entry.CachePath = cachePath
	entry.Size = int64(len(content))
	entry.MD5 = md5hex(content)
	entry.CacheState = store.CacheReady
	entry.CacheCheckedAt = time.Now()
	entry.Sync = store.Synced

	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	replacement, err := s.replaceCacheFile(finalTmp, cachePath)
	if err != nil {
		os.Remove(finalTmp)
		return err
	}
	defer replacement.Rollback()
	if err := s.store.Batch(func(b *store.Batch) error {
		b.PutEntry(entry)
		return nil
	}); err != nil {
		return err
	}
	replacement.Commit()
	return nil
}

// md5hex returns the lowercase hex MD5 of b.
func md5hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// Errors returned by the service.
var (
	ErrNotFound               = errors.New("service: not found")
	ErrNotCached              = errors.New("service: content not cached locally")
	ErrCacheValidationPending = errors.New("service: cache validation pending")
	ErrMachineStatusStale     = errors.New("service: machine status is stale")
	ErrRecoveryUnavailable    = errors.New("service: recovery unavailable")
	ErrRetryUnavailable       = errors.New("service: retry unavailable")
	ErrDiscardUnavailable     = errors.New("service: discard unavailable")
	ErrNoActiveGcode          = errors.New("service: no active gcode selected")
	ErrActiveGcodeUnavailable = errors.New("service: active gcode is not runnable")
)
