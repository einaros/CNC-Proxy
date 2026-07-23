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
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/uwin/cnc-proxy/internal/client"
	"github.com/uwin/cnc-proxy/internal/filepolicy"
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
	maxUIMacros          = 48
	maxMacroLines        = 40
	maxMacroLineLen      = 240
	maxMacroNameLen      = 80
	maxMacroDescLen      = 240
	maxMacroButtons      = 96
	maxMacroColorLen     = 32
	maxGamepadAxis       = 31
	maxGamepadButton     = 63
	maxGamepadMacros     = 32
	maxMachineSpanMM     = 5000
	maxTapFeedMMMin      = 10000
	maxSavedOrigins      = 48
	maxOriginLabelLen    = 80
	maxProbeDepthMM      = 200
	maxProbeFeedMM       = 1000
	maxTracePoints       = 4000
	maxFailedJobsPerPath = 20
	defaultSafeZMM       = -3.0
	safeZLimitMarginMM   = 3.0
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
	// the controller's selected_remote_filename concept, with one firmware-backed
	// recovery path: while a file is actually running, the firmware's read-only
	// progress command exposes the current player filename.
	activeMu    sync.Mutex
	activeGcode activeGcodeState

	activeProbeMu       sync.Mutex
	activeProbeInFlight bool
	activeProbeLast     time.Time
	activeProbeLoaded   bool

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
		s.maybeLoadActiveGcodeFromMachine(st)
	}
	statusCh, _ := s.arb.Tracker().Subscribe()
	go func() {
		for st := range statusCh {
			s.runHistory.ObserveStatus(st)
			s.maybeLoadActiveGcodeFromMachine(st)
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

// ProbeZRequest describes one serialized Z probe at the current XY or at a
// supplied machine-coordinate XY location.
type ProbeZRequest struct {
	MachineX       float64  `json:"machine_x"`
	MachineY       float64  `json:"machine_y"`
	MoveXY         bool     `json:"move_xy"`
	SafeZMM        float64  `json:"safe_z_mm"`
	RetractZMM     *float64 `json:"retract_z_mm,omitempty"`
	RetractAboveMM *float64 `json:"retract_above_mm,omitempty"`
	ProbeDepthMM   float64  `json:"probe_depth_mm"`
	ProbeFeedMM    float64  `json:"probe_feed_mm_min"`
}

// ProbeZResult is the parsed firmware probe report.
type ProbeZResult struct {
	Machine    machine.AxisValues `json:"machine"`
	RetractZMM float64            `json:"retract_z_mm"`
	Output     string             `json:"output,omitempty"`
}

// TracePoint is one machine-coordinate XY point for a probe-laser outline trace.
type TracePoint struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// TraceOutlineRequest describes a serialized probe-laser outline trace.
type TraceOutlineRequest struct {
	MachinePoints []TracePoint `json:"machine_points"`
	MachineZ      float64      `json:"machine_z"`
	SafeZMM       float64      `json:"safe_z_mm"`
	FeedMM        float64      `json:"feed_mm_min"`
	Closed        bool         `json:"closed"`
}

// TraceOutlineResult is returned after the probe-laser trace transaction.
type TraceOutlineResult struct {
	Action       string `json:"action"`
	Points       int    `json:"points"`
	CommandCount int    `json:"command_count"`
	Verified     bool   `json:"verified"`
	Message      string `json:"message"`
}

// MachineLearnResult is returned after refreshing read-only firmware
// parameters into the proxy's local UI settings.
type MachineLearnResult struct {
	Action  string               `json:"action"`
	UI      store.UISettings     `json:"ui"`
	Learned store.MachineLearned `json:"learned"`
	Message string               `json:"message"`
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
		return fmt.Errorf("%w: unsupported backup version %d", ErrInvalidArgument, b.Version)
	}
	if err := validateBackupState(b.State); err != nil {
		return err
	}
	for i := range b.State.Jobs {
		if b.State.Jobs[i].State == store.Running {
			b.State.Jobs[i].State = store.Failed
			b.State.Jobs[i].LastError = "backup was captured while this job was running; inspect the machine state and retry manually"
		}
	}
	if err := s.store.Restore(b.State); err != nil {
		return err
	}
	s.gcodeLog.Replace(b.GcodeLog)
	s.runHistory.Replace(b.RunHistory)
	return nil
}

func validateBackupState(state store.Snapshot) error {
	for key, entry := range state.Entries {
		if key != entry.Path || !filepolicy.IsGcodePath(entry.Path) {
			return fmt.Errorf("%w: invalid catalog path %q", ErrInvalidArgument, entry.Path)
		}
		switch entry.Sync {
		case store.Synced, store.LocalOnly, store.PendingUpload, store.Uploading,
			store.PendingDelete, store.Deleting, store.PendingRename, store.RemoteOnly, store.Error:
		default:
			return fmt.Errorf("%w: invalid catalog sync state %q", ErrInvalidArgument, entry.Sync)
		}
		switch entry.CacheState {
		case "", store.CacheNone, store.CacheReady, store.CacheValidating:
		default:
			return fmt.Errorf("%w: invalid cache state %q", ErrInvalidArgument, entry.CacheState)
		}
		if entry.IsDir && entry.CachePath != "" {
			return fmt.Errorf("%w: directory %q has cached file content", ErrInvalidArgument, entry.Path)
		}
	}
	if state.ActiveGcodePath != "" && !filepolicy.IsGcodePath(state.ActiveGcodePath) {
		return fmt.Errorf("%w: invalid active gcode path %q", ErrInvalidArgument, state.ActiveGcodePath)
	}
	seenIDs := make(map[int64]bool, len(state.Jobs))
	for _, job := range state.Jobs {
		if job.ID <= 0 || seenIDs[job.ID] {
			return fmt.Errorf("%w: invalid or duplicate job id %d", ErrInvalidArgument, job.ID)
		}
		seenIDs[job.ID] = true
		if !filepolicy.IsGcodePath(job.Path) {
			return fmt.Errorf("%w: invalid job path %q", ErrInvalidArgument, job.Path)
		}
		switch job.Kind {
		case store.JobUpload, store.JobDelete, store.JobMkdir:
			if job.DestPath != "" {
				return fmt.Errorf("%w: unexpected destination for %s job", ErrInvalidArgument, job.Kind)
			}
		case store.JobRename:
			if !filepolicy.IsGcodePath(job.DestPath) {
				return fmt.Errorf("%w: invalid rename destination %q", ErrInvalidArgument, job.DestPath)
			}
		default:
			return fmt.Errorf("%w: invalid job kind %q", ErrInvalidArgument, job.Kind)
		}
		switch job.State {
		case store.Queued, store.Running, store.Done, store.Failed:
		default:
			return fmt.Errorf("%w: invalid job state %q", ErrInvalidArgument, job.State)
		}
	}
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
			_, _ = s.store.PruneFailedJobs(maxFailedJobsPerPath)
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
		return store.UISettings{}, fmt.Errorf("%w: at most %d macros are allowed", ErrInvalidArgument, maxUIMacros)
	}
	if len(ui.MacroButtons) > maxMacroButtons {
		return store.UISettings{}, fmt.Errorf("%w: at most %d macro buttons are allowed", ErrInvalidArgument, maxMacroButtons)
	}
	for i, m := range ui.Macros {
		if strings.TrimSpace(m.Name) == "" {
			return store.UISettings{}, fmt.Errorf("%w: macro %d requires a name", ErrInvalidArgument, i+1)
		}
		if len(m.Name) > maxMacroNameLen {
			return store.UISettings{}, fmt.Errorf("%w: macro %q name is too long", ErrInvalidArgument, m.Name)
		}
		if len(m.Description) > maxMacroDescLen {
			return store.UISettings{}, fmt.Errorf("%w: macro %q description is too long", ErrInvalidArgument, m.Name)
		}
		if len(m.Color) > maxMacroColorLen {
			return store.UISettings{}, fmt.Errorf("%w: macro %q color is too long", ErrInvalidArgument, m.Name)
		}
		if len(m.Lines) > maxMacroLines {
			return store.UISettings{}, fmt.Errorf("%w: macro %q has too many lines", ErrInvalidArgument, m.Name)
		}
		nonBlank := 0
		for _, line := range m.Lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			nonBlank++
			if len(line) > maxMacroLineLen {
				return store.UISettings{}, fmt.Errorf("%w: macro %q has a line longer than %d bytes", ErrInvalidArgument, m.Name, maxMacroLineLen)
			}
		}
		if nonBlank == 0 {
			return store.UISettings{}, fmt.Errorf("%w: macro %q requires at least one line", ErrInvalidArgument, m.Name)
		}
	}
	if err := validateGamepadSettings(ui.Gamepad); err != nil {
		return store.UISettings{}, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if err := validateMachineUI(ui.Machine); err != nil {
		return store.UISettings{}, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	ui.Machine.SafeZMM = clampSafeZMM(ui.Machine.SafeZMM, ui.Machine.Learned)
	if ui.Log.Filter == "" {
		ui.Log.Filter = "all"
		current := s.store.UISettings()
		ui.Log.Autoscroll = current.Log.Autoscroll
	}
	return s.store.SetUISettings(ui)
}

// LearnMachineParameters refreshes read-only firmware-reported parameters into
// local UI metadata. It mirrors the original controller's connection/config
// queries where they expose durable machine parameters, and also asks diagnose
// for the compact state tuple the controller parses for peripheral/endstop
// status.
func (s *Service) LearnMachineParameters() (MachineLearnResult, error) {
	var modelOut, versionOut, ftypeOut, diagnoseOut, configOut string
	err := s.arb.WithMachine(true, func(c *client.Conn) error {
		var e error
		if modelOut, e = s.sendLearnLine(c, "model"); e != nil {
			return e
		}
		if versionOut, e = s.sendLearnLine(c, "version"); e != nil {
			return e
		}
		if ftypeOut, e = s.sendLearnLine(c, "ftype"); e != nil {
			return e
		}
		if diagnoseOut, e = s.sendLearnLine(c, "diagnose"); e != nil {
			return e
		}
		if configOut, e = s.sendLearnLine(c, "config-get-all -e"); e != nil {
			return e
		}
		return nil
	})
	if err != nil {
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, "error: "+err.Error())
		return MachineLearnResult{}, err
	}

	learned, err := buildMachineLearned(modelOut, versionOut, ftypeOut, diagnoseOut, configOut, time.Now())
	if err != nil {
		return MachineLearnResult{}, err
	}
	ui := s.store.UISettings()
	ui.Machine.Learned = learned
	applyLearnedMachineDefaults(&ui.Machine, learned)
	ui, err = s.SetUISettings(ui)
	if err != nil {
		return MachineLearnResult{}, err
	}
	return MachineLearnResult{
		Action:  "learn_machine_parameters",
		UI:      ui,
		Learned: ui.Machine.Learned,
		Message: "Learned machine parameters from firmware.",
	}, nil
}

func (s *Service) sendLearnLine(c *client.Conn, line string) (string, error) {
	s.gcodeLog.Append(gcodelog.DirSend, gcodelog.SourceAPI, "learn "+line)
	out, err := c.SendConsoleCommand(ensureWireLine(line), client.GcodeOpts{ExpectReply: true, Cap: gcodeReplyCap})
	if out != "" {
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, out)
	}
	if err != nil {
		return out, err
	}
	if out == "" {
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, "ok")
	}
	return out, nil
}

func applyLearnedMachineDefaults(m *store.MachineUI, learned store.MachineLearned) {
	if validWorkArea(learned.WorkArea) {
		m.WorkArea = learned.WorkArea
	}
	m.SafeZMM = safeZCeilingMM(learned)
	if learned.Feed.MaxXYMMMin > 0 {
		m.FeedMaxMMMin = learned.Feed.MaxXYMMMin
	} else if learned.Feed.SeekMMMin > 0 {
		m.FeedMaxMMMin = learned.Feed.SeekMMMin
	}
	if m.FeedMinMMMin <= 0 {
		m.FeedMinMMMin = 1
	}
	if m.FeedMaxMMMin > 0 {
		if m.TapFeedMMMin <= 0 {
			m.TapFeedMMMin = m.FeedMaxMMMin
		}
		if m.TapFeedMMMin > m.FeedMaxMMMin {
			m.TapFeedMMMin = m.FeedMaxMMMin
		}
	}
}

// SafeZTargetMM returns the highest machine-coordinate Z that proxy-managed
// safe-travel sequences may use. The firmware's clearance Z is authoritative
// when learned; the upper soft limit is an additional hard ceiling. The
// fallback remains three millimetres below the usual Carvera Z-zero boundary.
// Explicit raw G-code is intentionally outside this policy.
func (s *Service) SafeZTargetMM(requested float64) float64 {
	return clampSafeZMM(requested, s.store.UISettings().Machine.Learned)
}

func clampSafeZMM(requested float64, learned store.MachineLearned) float64 {
	if !finite(requested) {
		return requested
	}
	return math.Min(requested, safeZCeilingMM(learned))
}

func safeZCeilingMM(learned store.MachineLearned) float64 {
	ceiling := defaultSafeZMM
	if clearance, ok := learned.ConfigNumbers["coordinate.clearance_z"]; ok && finite(clearance) {
		ceiling = math.Min(ceiling, clearance)
	}
	if finite(learned.ZMinMM) && finite(learned.ZMaxMM) && learned.ZMaxMM-learned.ZMinMM > 2*safeZLimitMarginMM {
		ceiling = math.Min(ceiling, learned.ZMaxMM-safeZLimitMarginMM)
	}
	return ceiling
}

func buildMachineLearned(modelOut, versionOut, ftypeOut, diagnoseOut, configOut string, now time.Time) (store.MachineLearned, error) {
	config := parseMachineConfig(configOut)
	numbers := machineConfigNumbers(config)
	bools := machineConfigBools(config)
	diagnostics := parseMachineDiagnostics(diagnoseOut)
	learned := store.MachineLearned{
		LearnedAt:     now,
		Source:        "firmware",
		Identity:      parseMachineIdentity(modelOut, versionOut, ftypeOut),
		Config:        config,
		ConfigNumbers: numbers,
		ConfigBools:   bools,
		Diagnostics:   diagnostics,
		RawDiagnose:   strings.TrimSpace(diagnoseOut),
	}
	applyKnownMachineConfig(&learned)
	if len(config) == 0 && len(diagnostics) == 0 && learned.Identity == (store.MachineIdentity{}) {
		return store.MachineLearned{}, errors.New("service: machine did not return learnable parameters")
	}
	return learned, nil
}

func parseMachineIdentity(modelOut, versionOut, ftypeOut string) store.MachineIdentity {
	return store.MachineIdentity{
		Model:    firstReplyValue(modelOut, "model", "del"),
		Version:  firstReplyValue(versionOut, "version"),
		FileType: firstReplyValue(ftypeOut, "ftype"),
	}
}

func firstReplyValue(out string, keys ...string) string {
	want := map[string]bool{}
	for _, key := range keys {
		want[strings.ToLower(strings.TrimSpace(key))] = true
	}
	for _, line := range strings.Split(stripFirmwareEOT(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		left, right, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(left))
		if !want[key] {
			continue
		}
		return strings.TrimSpace(right)
	}
	return ""
}

func parseMachineConfig(out string) map[string]string {
	out = stripFirmwareEOT(out)
	config := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			continue
		}
		config[key] = value
	}
	if len(config) == 0 {
		return nil
	}
	return config
}

func stripFirmwareEOT(s string) string {
	return strings.ReplaceAll(s, "\x04", "")
}

func machineConfigNumbers(config map[string]string) map[string]float64 {
	out := map[string]float64{}
	for key, value := range config {
		n, ok := parseMachineNumber(value)
		if ok {
			out[key] = n
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func machineConfigBools(config map[string]string) map[string]bool {
	out := map[string]bool{}
	for key, value := range config {
		if b, ok := parseMachineBool(value); ok {
			out[key] = b
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseMachineNumber(value string) (float64, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	n, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, false
	}
	return n, true
}

func parseMachineBool(value string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "on", "yes", "1":
		return true, true
	case "false", "off", "no", "0":
		return false, true
	default:
		return false, false
	}
}

func parseMachineDiagnostics(out string) map[string][]float64 {
	record := extractMachineDiagnosticRecord(out)
	if record == "" {
		return nil
	}
	fields := map[string][]float64{}
	for _, part := range strings.Split(record, "|") {
		key, raw, ok := strings.Cut(part, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		values := []float64{}
		for _, item := range strings.Split(raw, ",") {
			n, ok := parseMachineNumber(item)
			if ok {
				values = append(values, n)
			}
		}
		if len(values) > 0 {
			fields[key] = values
		}
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

func extractMachineDiagnosticRecord(out string) string {
	out = strings.TrimSpace(stripFirmwareEOT(out))
	start := strings.IndexByte(out, '{')
	end := strings.LastIndexByte(out, '}')
	if start < 0 || end <= start {
		return ""
	}
	return out[start+1 : end]
}

func applyKnownMachineConfig(learned *store.MachineLearned) {
	num := func(keys ...string) (float64, bool) {
		for _, key := range keys {
			if v, ok := learned.ConfigNumbers[key]; ok {
				return v, true
			}
		}
		return 0, false
	}
	boolValue := func(key string) (bool, bool) {
		v, ok := learned.ConfigBools[key]
		return v, ok
	}

	if v, ok := num("default_feed_rate"); ok {
		learned.Feed.DefaultMMMin = v
	}
	if v, ok := num("default_seek_rate"); ok {
		learned.Feed.SeekMMMin = v
	}
	if v, ok := num("x_axis_max_speed", "alpha_max_rate"); ok {
		learned.Feed.XMaxMMMin = v
	}
	if v, ok := num("y_axis_max_speed", "beta_max_rate"); ok {
		learned.Feed.YMaxMMMin = v
	}
	if v, ok := num("z_axis_max_speed", "gamma_max_rate"); ok {
		learned.Feed.ZMaxMMMin = v
	}
	if v, ok := num("delta_max_rate"); ok {
		learned.Feed.AMax = v
	}
	if v, ok := num("epsilon_max_rate"); ok {
		learned.Feed.ATCMaxMMMin = v
	}
	if learned.Feed.XMaxMMMin > 0 && learned.Feed.YMaxMMMin > 0 {
		learned.Feed.MaxXYMMMin = math.Min(learned.Feed.XMaxMMMin, learned.Feed.YMaxMMMin)
	}

	if v, ok := boolValue("soft_endstop.enable"); ok {
		learned.SoftEndstop.Enabled = v
	}
	if v, ok := num("soft_endstop.x_min"); ok {
		learned.SoftEndstop.XMin = v
	}
	if v, ok := num("soft_endstop.y_min"); ok {
		learned.SoftEndstop.YMin = v
	}
	if v, ok := num("soft_endstop.z_min"); ok {
		learned.SoftEndstop.ZMin = v
		learned.ZMinMM = v
	}

	xMin, xMinOK := num("soft_endstop.x_min")
	xMax, xMaxOK := num("soft_endstop.x_max", "alpha_max")
	yMin, yMinOK := num("soft_endstop.y_min")
	yMax, yMaxOK := num("soft_endstop.y_max", "beta_max")
	if !xMaxOK {
		xMax = 0
		xMaxOK = xMinOK
	}
	if !yMaxOK {
		yMax = 0
		yMaxOK = yMinOK
	}
	if xMinOK && xMaxOK && yMinOK && yMaxOK {
		area := store.WorkArea{XMin: xMin, XMax: xMax, YMin: yMin, YMax: yMax}
		if validWorkArea(area) {
			learned.WorkArea = area
		}
	}

	if zMax, ok := num("soft_endstop.z_max", "gamma_max"); ok {
		learned.ZMaxMM = zMax
	}
	if v, ok := num("delta_min"); ok {
		learned.AMin = v
	}
	if v, ok := num("delta_max"); ok {
		learned.AMax = v
	}
	if v, ok := num("zeta_min"); ok {
		learned.CMin = v
	}
	if v, ok := num("zeta_max"); ok {
		learned.CMax = v
	}
	if v, ok := num("coordinate.clearance_x"); ok {
		learned.Clearance.X = v
	}
	if v, ok := num("coordinate.clearance_y"); ok {
		learned.Clearance.Y = v
	}
	if v, ok := num("coordinate.clearance_z"); ok {
		learned.Clearance.Z = v
	}
	anchor1X, anchor1XOK := num("coordinate.anchor1_x")
	anchor1Y, anchor1YOK := num("coordinate.anchor1_y")
	anchor2OffsetX, anchor2OffsetXOK := num("coordinate.anchor2_offset_x")
	anchor2OffsetY, anchor2OffsetYOK := num("coordinate.anchor2_offset_y")
	if anchor1XOK && anchor1YOK && anchor2OffsetXOK && anchor2OffsetYOK {
		learned.Anchors = store.MachineAnchorProfile{
			Available: true,
			Anchor1:   store.XYPoint{X: anchor1X, Y: anchor1Y},
			Anchor2:   store.XYPoint{X: anchor1X + anchor2OffsetX, Y: anchor1Y + anchor2OffsetY},
		}
	}
	if v, ok := num("atc.probe.fast_rate_mm_m"); ok {
		learned.Probe.FastRateMMMin = v
	}
	if v, ok := num("atc.probe.slow_rate_mm_m"); ok {
		learned.Probe.SlowRateMMMin = v
	}
	if v, ok := num("atc.probe.retract_mm"); ok {
		learned.Probe.RetractMM = v
	}
}

func validWorkArea(area store.WorkArea) bool {
	return finite(area.XMin) && finite(area.XMax) && finite(area.YMin) && finite(area.YMax) &&
		area.XMin < area.XMax && area.YMin < area.YMax &&
		area.XMax-area.XMin <= maxMachineSpanMM && area.YMax-area.YMin <= maxMachineSpanMM
}

func finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func validateMachineUI(m store.MachineUI) error {
	values := map[string]float64{
		"work_area.x_min": m.WorkArea.XMin,
		"work_area.x_max": m.WorkArea.XMax,
		"work_area.y_min": m.WorkArea.YMin,
		"work_area.y_max": m.WorkArea.YMax,
		"origin.x":        m.Origin.X,
		"origin.y":        m.Origin.Y,
		"feed_min_mm_min": m.FeedMinMMMin,
		"feed_max_mm_min": m.FeedMaxMMMin,
		"tap_feed_mm_min": m.TapFeedMMMin,
		"safe_z_mm":       m.SafeZMM,
	}
	allZero := true
	for _, v := range values {
		if v != 0 {
			allZero = false
			break
		}
	}
	if allZero && len(m.SavedOrigins) == 0 {
		return nil
	}
	for name, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("service: machine %s must be finite", name)
		}
	}
	workAreaMissing := m.WorkArea.XMin == 0 && m.WorkArea.XMax == 0 && m.WorkArea.YMin == 0 && m.WorkArea.YMax == 0
	if !workAreaMissing {
		if m.WorkArea.XMin >= m.WorkArea.XMax || m.WorkArea.YMin >= m.WorkArea.YMax {
			return fmt.Errorf("service: machine work area min values must be less than max values")
		}
		if m.WorkArea.XMax-m.WorkArea.XMin > maxMachineSpanMM || m.WorkArea.YMax-m.WorkArea.YMin > maxMachineSpanMM {
			return fmt.Errorf("service: machine work area span must be at most %.0f mm", float64(maxMachineSpanMM))
		}
	}
	if m.TapFeedMMMin != 0 && (m.TapFeedMMMin < 1 || m.TapFeedMMMin > maxTapFeedMMMin) {
		return fmt.Errorf("service: tap move feed must be between 1 and %.0f mm/min", float64(maxTapFeedMMMin))
	}
	if m.FeedMinMMMin != 0 && (m.FeedMinMMMin < 1 || m.FeedMinMMMin > maxTapFeedMMMin) {
		return fmt.Errorf("service: machine minimum feed must be between 1 and %.0f mm/min", float64(maxTapFeedMMMin))
	}
	if m.FeedMaxMMMin != 0 && (m.FeedMaxMMMin < 1 || m.FeedMaxMMMin > maxTapFeedMMMin) {
		return fmt.Errorf("service: machine maximum feed must be between 1 and %.0f mm/min", float64(maxTapFeedMMMin))
	}
	if m.FeedMinMMMin != 0 && m.FeedMaxMMMin != 0 && m.FeedMinMMMin > m.FeedMaxMMMin {
		return fmt.Errorf("service: machine minimum feed must be less than or equal to maximum feed")
	}
	if len(m.SavedOrigins) > maxSavedOrigins {
		return fmt.Errorf("service: saved origins must contain at most %d entries", maxSavedOrigins)
	}
	for _, saved := range m.SavedOrigins {
		if strings.TrimSpace(saved.Label) == "" {
			return fmt.Errorf("service: saved origin label is required")
		}
		if len(saved.Label) > maxOriginLabelLen {
			return fmt.Errorf("service: saved origin label must be at most %d bytes", maxOriginLabelLen)
		}
		if math.IsNaN(saved.Origin.X) || math.IsInf(saved.Origin.X, 0) || math.IsNaN(saved.Origin.Y) || math.IsInf(saved.Origin.Y, 0) {
			return fmt.Errorf("service: saved origin coordinates must be finite")
		}
	}
	return nil
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
		return "", fmt.Errorf("%w: path %q escapes %s", ErrInvalidArgument, p, GcodeRoot)
	}
	if filepolicy.IsJunk(clean) {
		return "", fmt.Errorf("%w: OS metadata path %q is not accepted", ErrInvalidArgument, p)
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
		return store.Entry{}, false, fmt.Errorf("%w: invalid upload range", ErrInvalidArgument)
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

const (
	traceStatusPollInterval = 250 * time.Millisecond
	traceStatusMinTimeout   = 5 * time.Second
	traceStatusMaxTimeout   = 2 * time.Minute
)

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

// ProbeZ performs a single serialized Z probe transaction. It holds the arbiter
// operation lock across the safe-Z move, optional XY move, probe command, and
// final safe-Z lift so no other proxy operation can interleave with the probe.
func (s *Service) ProbeZ(req ProbeZRequest) (ProbeZResult, error) {
	req.SafeZMM = s.SafeZTargetMM(req.SafeZMM)
	if err := validateProbeZRequest(req); err != nil {
		return ProbeZResult{}, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if err := s.arb.WithMachine(true, func(*client.Conn) error { return nil }); err != nil {
		return ProbeZResult{}, err
	}
	st, _ := s.arb.Tracker().Current()
	if st.Tool == nil || st.Tool.Active != 0 {
		return ProbeZResult{}, fmt.Errorf("%w: active tool is %s", ErrProbeUnavailable, toolStatusLabel(st.Tool))
	}
	var res ProbeZResult
	err := s.arb.WithMachine(true, func(c *client.Conn) error {
		if _, err := s.sendProbeLine(c, fmt.Sprintf("G53 G0 Z%.4f", req.SafeZMM), false); err != nil {
			return err
		}
		if req.MoveXY {
			if _, err := s.sendProbeLine(c, fmt.Sprintf("G53 G0 X%.4f Y%.4f", req.MachineX, req.MachineY), false); err != nil {
				return err
			}
		}
		line := fmt.Sprintf("G38.2 Z-%.4f F%.4f", req.ProbeDepthMM, req.ProbeFeedMM)
		out, err := s.sendProbeLine(c, line, true)
		if err != nil {
			return err
		}
		pos, ok, err := parseProbeResult(out)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: probe did not report contact", ErrProbeUnavailable)
		}
		retractZ, err := probeRetractZ(req, pos)
		if err != nil {
			return err
		}
		res = ProbeZResult{Machine: pos, RetractZMM: retractZ, Output: out}
		if _, err := s.sendProbeLine(c, fmt.Sprintf("G53 G0 Z%.4f", retractZ), false); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return ProbeZResult{}, err
	}
	return res, nil
}

// TraceOutline runs a serialized probe-laser trace around a machine-coordinate
// outline. The probe laser is switched off again if any command after activation
// fails, so the operator is not left with an active laser after a partial trace.
func (s *Service) TraceOutline(req TraceOutlineRequest) (TraceOutlineResult, error) {
	req.SafeZMM = s.SafeZTargetMM(req.SafeZMM)
	if err := validateTraceOutlineRequest(req); err != nil {
		err = fmt.Errorf("%w: %v", ErrInvalidArgument, err)
		return TraceOutlineResult{Action: "trace_outline", Message: err.Error()}, err
	}
	var res TraceOutlineResult
	err := s.arb.WithMachine(true, func(c *client.Conn) error {
		st, _ := s.arb.Tracker().Current()
		if st.Tool == nil || st.Tool.Active != 0 {
			return fmt.Errorf("%w: active tool is %s", ErrProbeUnavailable, toolStatusLabel(st.Tool))
		}
		points := traceOutlinePoints(req)
		res = TraceOutlineResult{
			Action:       "trace_outline",
			Points:       len(points),
			CommandCount: 0,
			Verified:     false,
		}
		laserOn := false
		run := func(line string) error {
			res.CommandCount++
			_, err := s.sendTraceLine(c, line)
			return err
		}
		if err := run("M494.1"); err != nil {
			return err
		}
		laserOn = true
		var err error
		first := points[0]
		if err = run(fmt.Sprintf("G53 G0 Z%.4f", req.SafeZMM)); err == nil {
			err = run(fmt.Sprintf("G53 G0 X%.4f Y%.4f", first.X, first.Y))
		}
		if err == nil {
			err = run(fmt.Sprintf("G53 G0 Z%.4f", req.MachineZ))
		}
		for i := 1; err == nil && i < len(points); i++ {
			p := points[i]
			err = run(fmt.Sprintf("G53 G1 X%.4f Y%.4f F%.4f", p.X, p.Y, req.FeedMM))
		}
		if err == nil {
			err = run("M400")
		}
		if laserOn {
			offErr := run("M494.2")
			laserOn = false
			if err == nil {
				err = offErr
			}
		}
		if err != nil {
			res.Message = "Trace outline failed: " + err.Error()
			return err
		}
		st, err = s.waitTraceIdle(c, traceIdleTimeout(points, req.FeedMM))
		if err != nil {
			res.Message = "Trace outline could not verify final machine status: " + err.Error()
			return err
		}
		if st.State != machine.Idle {
			err := fmt.Errorf("%w: trace commands sent, but machine reports %s", ErrMachineStatusStale, statusSummary(st))
			res.Message = err.Error()
			return err
		}
		res.Verified = true
		res.Message = fmt.Sprintf("Trace outline completed with %d points.", res.Points)
		return nil
	})
	if err != nil {
		if res.Action == "" {
			res.Action = "trace_outline"
		}
		if res.Message == "" {
			res.Message = err.Error()
		}
		return res, err
	}
	return res, nil
}

func validateProbeZRequest(req ProbeZRequest) error {
	values := map[string]float64{
		"machine_x":         req.MachineX,
		"machine_y":         req.MachineY,
		"safe_z_mm":         req.SafeZMM,
		"probe_depth_mm":    req.ProbeDepthMM,
		"probe_feed_mm_min": req.ProbeFeedMM,
	}
	for name, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("service: probe %s must be finite", name)
		}
	}
	if req.ProbeDepthMM <= 0 || req.ProbeDepthMM > maxProbeDepthMM {
		return fmt.Errorf("service: probe depth must be between 0 and %.0f mm", float64(maxProbeDepthMM))
	}
	if req.ProbeFeedMM <= 0 || req.ProbeFeedMM > maxProbeFeedMM {
		return fmt.Errorf("service: probe feed must be between 0 and %.0f mm/min", float64(maxProbeFeedMM))
	}
	if req.RetractZMM != nil && req.RetractAboveMM != nil {
		return fmt.Errorf("service: probe retract_z_mm and retract_above_mm are mutually exclusive")
	}
	if req.RetractZMM != nil {
		if !finite(*req.RetractZMM) {
			return fmt.Errorf("service: probe retract_z_mm must be finite")
		}
		if *req.RetractZMM > req.SafeZMM {
			return fmt.Errorf("service: probe retract_z_mm must not exceed safe_z_mm")
		}
	}
	if req.RetractAboveMM != nil {
		if !finite(*req.RetractAboveMM) {
			return fmt.Errorf("service: probe retract_above_mm must be finite")
		}
		if *req.RetractAboveMM < 0 || *req.RetractAboveMM > maxProbeDepthMM {
			return fmt.Errorf("service: probe retract_above_mm must be between 0 and %.0f mm", float64(maxProbeDepthMM))
		}
	}
	return nil
}

func probeRetractZ(req ProbeZRequest, pos machine.AxisValues) (float64, error) {
	if req.RetractZMM != nil {
		return *req.RetractZMM, nil
	}
	if req.RetractAboveMM == nil {
		return req.SafeZMM, nil
	}
	z, ok := finiteAxisValue(pos, "z")
	if !ok {
		return 0, fmt.Errorf("%w: probe result does not include a finite Z position", ErrProbeUnavailable)
	}
	// SafeZMM is the independently bounded machine-coordinate ceiling used to
	// reach the first point. Never let the relative field-probe clearance rise
	// above it, even if the first sample is close to the machine's soft limit.
	return math.Min(z+*req.RetractAboveMM, req.SafeZMM), nil
}

func validateTraceOutlineRequest(req TraceOutlineRequest) error {
	if len(req.MachinePoints) < 2 {
		return fmt.Errorf("service: trace outline needs at least two points")
	}
	if len(req.MachinePoints) > maxTracePoints {
		return fmt.Errorf("service: trace outline supports at most %d points", maxTracePoints)
	}
	values := map[string]float64{
		"machine_z":   req.MachineZ,
		"safe_z_mm":   req.SafeZMM,
		"feed_mm_min": req.FeedMM,
	}
	for name, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("service: trace %s must be finite", name)
		}
	}
	if req.FeedMM <= 0 || req.FeedMM > maxTapFeedMMMin {
		return fmt.Errorf("service: trace feed must be between 0 and %.0f mm/min", float64(maxTapFeedMMMin))
	}
	for i, p := range req.MachinePoints {
		if math.IsNaN(p.X) || math.IsInf(p.X, 0) || math.IsNaN(p.Y) || math.IsInf(p.Y, 0) {
			return fmt.Errorf("service: trace point %d must be finite", i+1)
		}
	}
	points := traceOutlinePoints(req)
	if len(points) > maxTracePoints {
		return fmt.Errorf("service: trace outline supports at most %d points", maxTracePoints)
	}
	return nil
}

func traceOutlinePoints(req TraceOutlineRequest) []TracePoint {
	points := append([]TracePoint(nil), req.MachinePoints...)
	if req.Closed && len(points) > 1 {
		first := points[0]
		last := points[len(points)-1]
		if math.Hypot(last.X-first.X, last.Y-first.Y) > 0.00005 {
			points = append(points, first)
		}
	}
	return points
}

func traceIdleTimeout(points []TracePoint, feedMMMin float64) time.Duration {
	if len(points) < 2 || feedMMMin <= 0 || math.IsNaN(feedMMMin) || math.IsInf(feedMMMin, 0) {
		return traceStatusMinTimeout
	}
	var length float64
	for i := 1; i < len(points); i++ {
		length += math.Hypot(points[i].X-points[i-1].X, points[i].Y-points[i-1].Y)
	}
	estimated := time.Duration((length/feedMMMin)*float64(time.Minute)) + 10*time.Second
	if estimated < traceStatusMinTimeout {
		return traceStatusMinTimeout
	}
	if estimated > traceStatusMaxTimeout {
		return traceStatusMaxTimeout
	}
	return estimated
}

func (s *Service) waitTraceIdle(c *client.Conn, timeout time.Duration) (machine.Status, error) {
	deadline := time.Now().Add(timeout)
	var last machine.Status
	for {
		st, err := s.queryRecoveryStatus(c)
		if err != nil {
			return st, err
		}
		last = st
		if st.State == machine.Idle {
			return st, nil
		}
		if time.Now().After(deadline) {
			return last, nil
		}
		time.Sleep(traceStatusPollInterval)
	}
}

func (s *Service) sendProbeLine(c *client.Conn, line string, expectReply bool) (string, error) {
	s.gcodeLog.Append(gcodelog.DirSend, gcodelog.SourceAPI, "probe "+line)
	cap := gcodeReplyCap
	firstReplyTimeout := time.Duration(0)
	if expectReply {
		cap = 2 * time.Minute
		firstReplyTimeout = cap
	}
	out, err := c.SendGcodeLine(line, client.GcodeOpts{
		ExpectReply:       expectReply,
		Cap:               cap,
		FirstReplyTimeout: firstReplyTimeout,
	})
	if out != "" {
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, out)
	}
	if err != nil {
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, "error: "+err.Error())
		return out, err
	}
	if out == "" {
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, "ok")
	}
	return out, nil
}

func (s *Service) sendTraceLine(c *client.Conn, line string) (string, error) {
	s.gcodeLog.Append(gcodelog.DirSend, gcodelog.SourceAPI, "trace "+line)
	out, err := c.SendGcodeLine(line, client.GcodeOpts{ExpectReply: false, Cap: gcodeReplyCap})
	if out != "" {
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, out)
	}
	if err != nil {
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, "error: "+err.Error())
		return out, err
	}
	if out == "" {
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, "ok")
	}
	return out, nil
}

func parseProbeResult(out string) (machine.AxisValues, bool, error) {
	start := strings.Index(out, "[PRB:")
	if start < 0 {
		return nil, false, fmt.Errorf("%w: probe response did not include PRB", ErrProbeUnavailable)
	}
	rest := out[start+len("[PRB:"):]
	end := strings.IndexByte(rest, ']')
	if end < 0 {
		return nil, false, fmt.Errorf("%w: malformed probe response", ErrProbeUnavailable)
	}
	payload := rest[:end]
	coordText, statusText, ok := strings.Cut(payload, ":")
	if !ok {
		return nil, false, fmt.Errorf("%w: malformed probe response", ErrProbeUnavailable)
	}
	coords := strings.Split(coordText, ",")
	if len(coords) < 3 {
		return nil, false, fmt.Errorf("%w: malformed probe coordinates", ErrProbeUnavailable)
	}
	x, err := parseProbeFloat(coords[0])
	if err != nil {
		return nil, false, err
	}
	y, err := parseProbeFloat(coords[1])
	if err != nil {
		return nil, false, err
	}
	z, err := parseProbeFloat(coords[2])
	if err != nil {
		return nil, false, err
	}
	statusText = strings.TrimSpace(statusText)
	success := statusText == "1" || strings.EqualFold(statusText, "true")
	return machine.AxisValues{"x": x, "y": y, "z": z}, success, nil
}

func parseProbeFloat(s string) (float64, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("%w: malformed probe coordinates", ErrProbeUnavailable)
	}
	return v, nil
}

func toolStatusLabel(t *machine.ToolStatus) string {
	if t == nil {
		return "unknown"
	}
	if t.Active == 0 {
		return "probe"
	}
	return fmt.Sprintf("tool %d", t.Active)
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
		return fmt.Errorf("%w: unsupported control character %#x", ErrInvalidArgument, c)
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
		err := fmt.Errorf("%w: recovery action must be one of: recover, unlock, home, reset", ErrInvalidArgument)
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
		var discarded store.Entry
		var discardedOK bool
		err := s.store.Batch(func(b *store.Batch) error {
			current, ok := b.GetEntry(remote)
			if !ok {
				return ErrNotFound
			}
			if current.IsDir && b.HasDescendants(remote) {
				return ErrDirectoryNotEmpty
			}
			discarded, discardedOK = b.DiscardEntry(remote, store.JobUpload, store.JobMkdir, store.JobDelete, store.JobRename)
			return nil
		})
		if err != nil {
			return err
		}
		if !discardedOK {
			return ErrNotFound
		}
		if discarded.CachePath != "" {
			os.Remove(discarded.CachePath)
		}
		return nil
	}
	return s.store.Batch(func(b *store.Batch) error {
		current, ok := b.GetEntry(remote)
		if !ok {
			return ErrNotFound
		}
		if current.IsDir && b.HasDescendants(remote) {
			return ErrDirectoryNotEmpty
		}
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

func (s *Service) restoreEntryStateForRetryBatch(b *store.Batch, job store.Job) error {
	switch job.Kind {
	case store.JobUpload:
		entry, ok := b.GetEntry(job.Path)
		if !ok {
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
		if !entry.IsDir && entry.CachePath == job.CachePath && entry.MD5 == job.MD5 && entry.Size == job.Size {
			b.SetEntrySync(job.Path, store.PendingUpload, "")
		}
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
		entry, ok := b.GetEntry(from)
		if !ok {
			return ErrNotFound
		}
		if entry.IsDir && b.HasDescendants(from) {
			return ErrDirectoryNotEmpty
		}
		b.SetEntrySync(from, store.PendingRename, "")
		b.Enqueue(store.Job{
			Kind:      store.JobRename,
			Path:      from,
			DestPath:  to,
			CachePath: entry.CachePath,
			MD5:       entry.MD5,
			Size:      entry.Size,
		})
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
	if !filepolicy.IsWithinDir(s.cacheDir, entry.CachePath) {
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
	committed := false
	if err := s.store.Batch(func(b *store.Batch) error {
		current, ok := b.GetEntry(remote)
		if !ok || current.Sync != store.RemoteOnly || current.UpdatedAt != entry.UpdatedAt {
			return nil
		}
		b.PutEntry(entry)
		committed = true
		return nil
	}); err != nil {
		return err
	}
	if !committed {
		return nil
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
	ErrInvalidArgument        = errors.New("service: invalid argument")
	ErrNotFound               = errors.New("service: not found")
	ErrNotCached              = errors.New("service: content not cached locally")
	ErrCacheValidationPending = errors.New("service: cache validation pending")
	ErrMachineStatusStale     = errors.New("service: machine status is stale")
	ErrRecoveryUnavailable    = errors.New("service: recovery unavailable")
	ErrRetryUnavailable       = errors.New("service: retry unavailable")
	ErrDiscardUnavailable     = errors.New("service: discard unavailable")
	ErrNoActiveGcode          = errors.New("service: no active gcode selected")
	ErrActiveGcodeUnavailable = errors.New("service: active gcode is not runnable")
	ErrProbeUnavailable       = errors.New("service: probe unavailable")
	ErrToolChangeUnavailable  = errors.New("service: tool change is not awaiting confirmation")
	ErrDirectoryNotEmpty      = errors.New("service: directory not empty")
)
