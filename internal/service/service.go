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
	"github.com/uwin/cnc-proxy/internal/session"
	"github.com/uwin/cnc-proxy/internal/store"
)

// GcodeRoot is the machine directory the filesystem exposes. All API paths are
// relative to it.
const GcodeRoot = "/sd/gcodes"

// Service wires the store, arbiter (for machine state), and local cache.
type Service struct {
	store    *store.Store
	arb      *session.Arbiter
	cacheDir string

	// gcodeLog records all gcode/console I/O with the machine — lines injected
	// via SendGcode here, plus controller traffic the relay sniffs into the same
	// log — for streaming to web clients.
	gcodeLog *gcodelog.Log

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
	return &Service{store: st, arb: arb, cacheDir: cacheDir, gcodeLog: gcodelog.New(500)}, nil
}

// GcodeLog exposes the shared gcode I/O log so the relay can record controller
// traffic into it and the API can stream it.
func (s *Service) GcodeLog() *gcodelog.Log { return s.gcodeLog }

// MachineStatus is the snapshot returned to clients.
type MachineStatus struct {
	State      machine.State       `json:"state"`
	Mode       string              `json:"mode"`
	Connected  bool                `json:"connected"`
	AgeMs      int64               `json:"age_ms"`
	ObservedAt time.Time           `json:"observed_at,omitempty"`
	Stale      bool                `json:"stale"`
	Raw        string              `json:"raw,omitempty"`
	Fields     map[string]string   `json:"fields,omitempty"`
	MPos       machine.AxisValues  `json:"mpos,omitempty"`
	WPos       machine.AxisValues  `json:"wpos,omitempty"`
	Feed       *machine.Triple     `json:"feed,omitempty"`
	Spindle    *machine.Spindle    `json:"spindle,omitempty"`
	Tool       *machine.ToolStatus `json:"tool,omitempty"`
	Progress   []float64           `json:"progress,omitempty"`
	Machine    []float64           `json:"machine,omitempty"`
}

// Status returns the current machine state and proxy mode.
func (s *Service) Status() MachineStatus {
	st, age := s.arb.Tracker().Current()
	observed := !st.ObservedAt.IsZero()
	return MachineStatus{
		State:      st.State,
		Mode:       s.arb.Mode().String(),
		Connected:  observed && st.State != machine.Unknown,
		AgeMs:      age.Milliseconds(),
		ObservedAt: st.ObservedAt,
		Stale:      !s.arb.Tracker().Fresh(s.arb.StateMaxAge()),
		Raw:        st.Raw,
		Fields:     st.Fields,
		MPos:       st.MPos,
		WPos:       st.WPos,
		Feed:       st.Feed,
		Spindle:    st.Spindle,
		Tool:       st.Tool,
		Progress:   st.Progress,
		Machine:    st.Machine,
	}
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
		Path:  remote,
		Size:  size,
		MTime: mtime,
		MD5:   md5hex,
		Sync:  store.RemoteOnly,
	})
}

// Jobs returns the job queue.
func (s *Service) Jobs() []store.Job { return s.store.ListJobs() }

// Subscribe proxies the store's event subscription for SSE.
func (s *Service) Subscribe() (<-chan store.Event, func()) { return s.store.Subscribe() }

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

	if err := os.Rename(tmp, cachePath); err != nil {
		os.Remove(tmp)
		return store.Entry{}, err
	}

	entry := store.Entry{
		Path:      remote,
		Size:      size,
		MTime:     time.Now(),
		MD5:       md5hex,
		CachePath: cachePath,
		Sync:      store.PendingUpload,
	}
	if err := s.store.PutEntry(entry); err != nil {
		return store.Entry{}, err
	}
	if _, err := s.store.SupersedeQueuedUploads(remote); err != nil {
		return store.Entry{}, err
	}
	if _, err := s.store.Enqueue(store.Job{
		Kind:      store.JobUpload,
		Path:      remote,
		CachePath: cachePath,
		MD5:       md5hex,
		Size:      size,
	}); err != nil {
		return store.Entry{}, err
	}
	return entry, nil
}

// gcodeReplyCap bounds a reply-expected query (M114, version, $G, M503, …). The
// firmware answers these promptly, so this is only a safety net; the read
// actually terminates on the reply's quiescence well before this.
const gcodeReplyCap = 30 * time.Second

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

// Delete enqueues a delete and marks the entry pending_delete.
func (s *Service) Delete(remotePath string) error {
	remote, err := normalizeRemote(remotePath)
	if err != nil {
		return err
	}
	if _, ok := s.store.GetEntry(remote); !ok {
		return ErrNotFound
	}
	s.store.SetEntrySync(remote, store.PendingDelete, "")
	_, err = s.store.Enqueue(store.Job{Kind: store.JobDelete, Path: remote})
	return err
}

// Rename enqueues a rename and marks the entry pending_rename.
func (s *Service) Rename(fromPath, toPath string) error {
	from, err := normalizeRemote(fromPath)
	if err != nil {
		return err
	}
	to, err := normalizeRemote(toPath)
	if err != nil {
		return err
	}
	if _, ok := s.store.GetEntry(from); !ok {
		return ErrNotFound
	}
	s.store.SetEntrySync(from, store.PendingRename, "")
	_, err = s.store.Enqueue(store.Job{Kind: store.JobRename, Path: from, DestPath: to})
	return err
}

// Mkdir enqueues a directory creation and records a directory entry.
func (s *Service) Mkdir(remotePath string) (store.Entry, error) {
	remote, err := normalizeRemote(remotePath)
	if err != nil {
		return store.Entry{}, err
	}
	entry := store.Entry{Path: remote, IsDir: true, MTime: time.Now(), Sync: store.PendingUpload}
	if err := s.store.PutEntry(entry); err != nil {
		return store.Entry{}, err
	}
	if _, err := s.store.Enqueue(store.Job{Kind: store.JobMkdir, Path: remote}); err != nil {
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
	if entry.CachePath == "" {
		return nil, entry, ErrNotCached
	}
	f, err := os.Open(entry.CachePath)
	if err != nil {
		return nil, entry, ErrNotCached
	}
	return f, entry, nil
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
	if err != ErrNotCached {
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
	if err := os.Rename(finalTmp, cachePath); err != nil {
		os.Remove(finalTmp)
		return err
	}

	entry.CachePath = cachePath
	entry.Size = int64(len(content))
	entry.MD5 = md5hex(content)
	entry.Sync = store.Synced
	return s.store.PutEntry(entry)
}

// md5hex returns the lowercase hex MD5 of b.
func md5hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// Errors returned by the service.
var (
	ErrNotFound  = errors.New("service: not found")
	ErrNotCached = errors.New("service: content not cached locally")
)
