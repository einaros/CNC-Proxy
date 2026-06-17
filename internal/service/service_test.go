package service

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uwin/cnc-proxy/internal/carveratest"
	"github.com/uwin/cnc-proxy/internal/client"
	"github.com/uwin/cnc-proxy/internal/machine"
	"github.com/uwin/cnc-proxy/internal/protocol"
	"github.com/uwin/cnc-proxy/internal/relay"
	"github.com/uwin/cnc-proxy/internal/runhistory"
	"github.com/uwin/cnc-proxy/internal/session"
	"github.com/uwin/cnc-proxy/internal/store"
)

// seedOnMachine uploads content directly to the fake machine via a client.
func seedOnMachine(t *testing.T, addr, remote string, content []byte) {
	t.Helper()
	conn, err := client.Dial(addr, 2*time.Second, client.WithUploadStartDelay(0))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sum := md5.Sum(content)
	if err := conn.Upload(remote, bytes.NewReader(content), int64(len(content)), hex.EncodeToString(sum[:]), 2*time.Second, nil); err != nil {
		t.Fatalf("seed %s: %v", remote, err)
	}
}

func newService(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	arb := session.New(session.Config{
		Dial: func() (*client.Conn, error) { return nil, io.EOF }, // never dialed in these tests
	})
	svc, err := New(st, arb)
	if err != nil {
		t.Fatal(err)
	}
	return svc, st
}

func TestUploadCreatesEntryAndJob(t *testing.T) {
	svc, st := newService(t)
	content := []byte("G0 X0 Y0\nG1 X10\n")
	entry, err := svc.Upload("part.nc", bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if entry.Path != "/sd/gcodes/part.nc" {
		t.Errorf("path = %q", entry.Path)
	}
	if entry.Sync != store.PendingUpload {
		t.Errorf("sync = %q, want pending_upload", entry.Sync)
	}
	sum := md5.Sum(content)
	if entry.MD5 != hex.EncodeToString(sum[:]) {
		t.Errorf("md5 = %q", entry.MD5)
	}
	jobs := st.ListJobs()
	if len(jobs) != 1 || jobs[0].Kind != store.JobUpload || jobs[0].Path != entry.Path {
		t.Errorf("jobs = %+v", jobs)
	}

	// Content is readable from cache immediately (Drive behavior).
	rc, _, err := svc.ReadCache("part.nc")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, content) {
		t.Errorf("cached content mismatch")
	}
}

func TestPathTraversalRejected(t *testing.T) {
	svc, _ := newService(t)
	if _, err := svc.Upload("../../etc/passwd", bytes.NewReader([]byte("x"))); err == nil {
		t.Error("expected traversal to be rejected")
	}
	if _, err := svc.Upload("/etc/passwd", bytes.NewReader([]byte("x"))); err == nil {
		t.Error("expected absolute path outside root to be rejected")
	}
}

func TestDeleteAndRenameRequireExisting(t *testing.T) {
	svc, _ := newService(t)
	if err := svc.Delete("nope.nc"); err != ErrNotFound {
		t.Errorf("delete missing = %v, want ErrNotFound", err)
	}
	if err := svc.Rename("nope.nc", "x.nc"); err != ErrNotFound {
		t.Errorf("rename missing = %v, want ErrNotFound", err)
	}
}

func TestRenamePendingUploadMovesLocalContentToDestination(t *testing.T) {
	svc, st := newService(t)
	content := []byte("G0 X0\nG1 X1\n")
	entry, err := svc.Upload("upload.tmp", bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Rename("upload.tmp", "final.nc"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, ok := svc.Lookup("upload.tmp"); ok {
		t.Fatal("source entry still exists after local pending rename")
	}
	got, ok := svc.Lookup("final.nc")
	if !ok {
		t.Fatal("destination entry missing after local pending rename")
	}
	if got.Size != int64(len(content)) || got.MD5 != md5hex(content) || got.CachePath == entry.CachePath {
		t.Fatalf("destination entry = %+v, want moved cached content", got)
	}
	rc, _, err := svc.ReadCache("final.nc")
	if err != nil {
		t.Fatalf("ReadCache final: %v", err)
	}
	read, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(read, content) {
		t.Fatalf("final cache = %q, want %q", string(read), string(content))
	}
	if _, err := os.Stat(entry.CachePath); !os.IsNotExist(err) {
		t.Fatalf("source cache still exists: %v", err)
	}

	var finalUploads, remoteRenames int
	for _, j := range st.ListJobs() {
		if j.Kind == store.JobUpload && j.Path == "/sd/gcodes/final.nc" && j.State == store.Queued {
			finalUploads++
		}
		if j.Kind == store.JobRename {
			remoteRenames++
		}
	}
	if finalUploads != 1 || remoteRenames != 0 {
		t.Fatalf("jobs = %+v, want one destination upload and no remote rename", st.ListJobs())
	}
}

func TestDeletePendingUploadDiscardsLocalEntry(t *testing.T) {
	svc, st := newService(t)
	entry, err := svc.Upload("a.nc", bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete("a.nc"); err != nil {
		t.Fatalf("delete pending upload: %v", err)
	}
	if _, ok := st.GetEntry("/sd/gcodes/a.nc"); ok {
		t.Fatal("pending upload entry should be removed locally")
	}
	jobs := st.ListJobs()
	if len(jobs) != 1 || jobs[0].State != store.Done {
		t.Fatalf("upload job = %+v, want done", jobs)
	}
	if _, err := os.Stat(entry.CachePath); !os.IsNotExist(err) {
		t.Fatalf("cache stat = %v, want removed", err)
	}
}

func TestDeleteFailedUploadDiscardsLocalEntry(t *testing.T) {
	svc, st := newService(t)
	entry, err := svc.Upload("bad.nc", bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateJob(1, func(j *store.Job) {
		j.State = store.Failed
		j.Attempts = 8
		j.LastError = "upload failed"
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEntrySync("/sd/gcodes/bad.nc", store.Error, "upload failed"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete("bad.nc"); err != nil {
		t.Fatalf("delete failed upload: %v", err)
	}
	if _, ok := st.GetEntry("/sd/gcodes/bad.nc"); ok {
		t.Fatal("failed upload entry should be removed locally")
	}
	jobs := st.ListJobs()
	if len(jobs) != 1 || jobs[0].State != store.Done || jobs[0].LastError != "" {
		t.Fatalf("failed upload job = %+v, want done without error", jobs)
	}
	if _, err := os.Stat(entry.CachePath); !os.IsNotExist(err) {
		t.Fatalf("cache stat = %v, want removed", err)
	}
}

func TestRetryFailedUploadRestoresPendingState(t *testing.T) {
	svc, st := newService(t)
	entry, err := svc.Upload("retry.nc", bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateJob(1, func(j *store.Job) {
		j.State = store.Failed
		j.Attempts = 8
		j.LastError = "upload failed"
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEntrySync("/sd/gcodes/retry.nc", store.Error, "upload failed"); err != nil {
		t.Fatal(err)
	}
	job, err := svc.RetryJob(1)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if job.State != store.Queued || job.Attempts != 0 || job.LastError != "" {
		t.Fatalf("retried job = %+v", job)
	}
	got, ok := st.GetEntry("/sd/gcodes/retry.nc")
	if !ok || got.Sync != store.PendingUpload || got.Error != "" || got.CachePath != entry.CachePath {
		t.Fatalf("entry after retry = %+v ok=%v", got, ok)
	}
}

func TestDiscardLocalErrorClearsStaleFailedDelete(t *testing.T) {
	svc, st := newService(t)
	if err := st.PutEntry(store.Entry{Path: "/sd/gcodes/stale.nc", Sync: store.Error, Error: "delete failed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Enqueue(store.Job{Kind: store.JobDelete, Path: "/sd/gcodes/stale.nc"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateJob(1, func(j *store.Job) {
		j.State = store.Failed
		j.Attempts = 8
		j.LastError = "delete failed"
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DiscardLocal("stale.nc"); err != nil {
		t.Fatalf("discard local: %v", err)
	}
	if _, ok := st.GetEntry("/sd/gcodes/stale.nc"); ok {
		t.Fatal("entry should be removed")
	}
	if got := st.ListJobs()[0]; got.State != store.Done || got.LastError != "" {
		t.Fatalf("job after discard = %+v", got)
	}
}

func TestDiscardLocalClearsFailedJobWithoutEntry(t *testing.T) {
	svc, st := newService(t)
	cachePath := filepath.Join(st.CacheDir(), "orphan-cache")
	if err := os.MkdirAll(st.CacheDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Enqueue(store.Job{Kind: store.JobUpload, Path: "/sd/gcodes/orphan.nc", CachePath: cachePath}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateJob(1, func(j *store.Job) {
		j.State = store.Failed
		j.Attempts = 8
		j.LastError = "upload failed"
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DiscardLocal("orphan.nc"); err != nil {
		t.Fatalf("discard local: %v", err)
	}
	if got := st.ListJobs()[0]; got.State != store.Done || got.LastError != "" {
		t.Fatalf("job after discard = %+v", got)
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatalf("cache stat = %v, want removed", err)
	}
}

func TestDeleteSyncedEntryQueuesMachineDelete(t *testing.T) {
	svc, st := newService(t)
	if err := st.PutEntry(store.Entry{Path: "/sd/gcodes/a.nc", Sync: store.Synced}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete("a.nc"); err != nil {
		t.Fatalf("delete synced: %v", err)
	}
	e, _ := st.GetEntry("/sd/gcodes/a.nc")
	if e.Sync != store.PendingDelete {
		t.Errorf("sync after delete = %q, want pending_delete", e.Sync)
	}
	jobs := st.ListJobs()
	if len(jobs) != 1 || jobs[0].Kind != store.JobDelete || jobs[0].State != store.Queued {
		t.Fatalf("delete job = %+v", jobs)
	}
}

func TestUploadAfterQueuedDeleteReplacesDeleteIntent(t *testing.T) {
	svc, st := newService(t)
	if _, err := svc.Upload("a.nc", bytes.NewReader([]byte("old\n"))); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEntrySync("/sd/gcodes/a.nc", store.Synced, ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete("a.nc"); err != nil {
		t.Fatalf("delete synced: %v", err)
	}

	content := []byte("new content\n")
	entry, err := svc.Upload("a.nc", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("upload replacement: %v", err)
	}
	if entry.Sync != store.PendingUpload || entry.Size != int64(len(content)) || entry.MD5 != md5hex(content) {
		t.Fatalf("replacement entry = %+v, want pending upload with new content", entry)
	}
	rc, _, err := svc.ReadCache("a.nc")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("cache = %q, want %q", string(got), string(content))
	}

	var queuedDeletes, doneDeletes, queuedUploads int
	for _, job := range st.ListJobs() {
		if job.Path != "/sd/gcodes/a.nc" {
			continue
		}
		switch {
		case job.Kind == store.JobDelete && job.State == store.Queued:
			queuedDeletes++
		case job.Kind == store.JobDelete && job.State == store.Done:
			doneDeletes++
		case job.Kind == store.JobUpload && job.State == store.Queued:
			queuedUploads++
		}
	}
	if queuedDeletes != 0 || doneDeletes != 1 || queuedUploads != 1 {
		t.Fatalf("jobs = %+v, want no queued delete, one discarded delete, one replacement upload", st.ListJobs())
	}
}

func TestDownloadOnDemand(t *testing.T) {
	// A service wired to a real arbiter + fake machine, so Open() can fetch.
	m, err := carveratest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	tr := machine.NewTracker()
	tr.Observe(machine.Idle)
	arb := session.New(session.Config{
		Tracker: tr,
		Dial: func() (*client.Conn, error) {
			return client.Dial(m.Addr(), 2*time.Second, client.WithUploadStartDelay(0))
		},
	})
	svc, err := New(st, arb)
	if err != nil {
		t.Fatal(err)
	}

	// Seed a file on the machine and record it as remote_only (as a reconcile
	// sweep would).
	content := []byte("G0 X1 Y1 ; on the machine only\n")
	seedOnMachine(t, m.Addr(), "/sd/gcodes/remote.nc", content)
	svc.PutRemoteOnly("remote.nc", int64(len(content)), time.Unix(0, 0), "")

	// Opening it should fetch from the machine into the cache, then serve it.
	rc, entry, err := svc.Open("remote.nc")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != string(content) {
		t.Errorf("downloaded content = %q, want %q", got, content)
	}
	if entry.Sync != store.Synced && entry.Sync != store.RemoteOnly {
		t.Errorf("entry sync = %q", entry.Sync)
	}
	// After the fetch the catalog entry should be cached + synced.
	if e, _ := svc.Lookup("remote.nc"); e.Sync != store.Synced || e.CachePath == "" {
		t.Errorf("after fetch entry = %+v, want synced+cached", e)
	}
}

func TestDownloadCompressedSidecar(t *testing.T) {
	m, err := carveratest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.SetCompressDownloads(true) // machine sends .lz, reports uncompressed MD5

	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	tr := machine.NewTracker()
	tr.Observe(machine.Idle)
	arb := session.New(session.Config{
		Tracker: tr,
		Dial: func() (*client.Conn, error) {
			return client.Dial(m.Addr(), 2*time.Second, client.WithUploadStartDelay(0))
		},
	})
	svc, err := New(st, arb)
	if err != nil {
		t.Fatal(err)
	}

	content := bytes.Repeat([]byte("compress me please\n"), 1000)
	seedOnMachine(t, m.Addr(), "/sd/gcodes/z.nc", content)
	svc.PutRemoteOnly("z.nc", int64(len(content)), time.Unix(0, 0), "")

	rc, _, err := svc.Open("z.nc")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, content) {
		t.Errorf("decompressed download mismatch: got %d bytes, want %d", len(got), len(content))
	}
}

// TestConcurrentUploadsSamePath ensures simultaneous uploads of the same path
// don't corrupt each other's cache file (they used to share one ".tmp"). Each
// upload must end with a coherent cache file matching its own content's MD5.
func TestConcurrentUploadsSamePath(t *testing.T) {
	svc, st := newService(t)

	const n = 8
	done := make(chan []byte, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			// Distinct content per goroutine, all to the SAME remote path.
			content := bytes.Repeat([]byte{byte('A' + i)}, 4096)
			if _, err := svc.Upload("race.nc", bytes.NewReader(content)); err != nil {
				t.Errorf("upload %d: %v", i, err)
				done <- nil
				return
			}
			done <- content
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}

	// The winning entry's cache file must exactly match its recorded MD5 — i.e.
	// no interleaved/corrupted write survived.
	entry, ok := st.GetEntry("/sd/gcodes/race.nc")
	if !ok {
		t.Fatal("no entry after concurrent uploads")
	}
	rc, _, err := svc.ReadCache("race.nc")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	sum := md5.Sum(got)
	if hex.EncodeToString(sum[:]) != entry.MD5 {
		t.Errorf("cache file MD5 %x does not match entry MD5 %s — corrupted write",
			sum, entry.MD5)
	}
	// And no leftover temp files in the cache dir.
	leftovers, _ := filepath.Glob(filepath.Join(st.CacheDir(), "*.tmp"))
	if len(leftovers) != 0 {
		t.Errorf("leftover temp files: %v", leftovers)
	}
}

func TestStatusReflectsArbiter(t *testing.T) {
	svc, _ := newService(t)
	st := svc.Status()
	if st.Mode != "owner" {
		t.Errorf("mode = %q, want owner", st.Mode)
	}
	if !st.Reconnecting {
		t.Error("owner mode with no fresh status should report reconnecting")
	}
	if !svc.arb.Tracker().ObserveStatusPayload("<Idle|MPos:0,0,0|WPos:0,0,0>") {
		t.Fatal("status should parse")
	}
	if st := svc.Status(); st.Reconnecting || st.Stale {
		t.Errorf("fresh owner status should not reconnect/stale: %+v", st)
	}
	svc.arb.EnterRelay()
	if st := svc.Status(); st.Mode != "relay" || st.Reconnecting {
		t.Error("expected relay mode after EnterRelay")
	}
	_ = time.Now
}

// serviceWithMachine wires a service to a real arbiter + fake machine with an
// explicit tracker so tests can drive machine state for idle gating.
func serviceWithMachine(t *testing.T) (*Service, *carveratest.FakeMachine, *machine.Tracker) {
	t.Helper()
	m, err := carveratest.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	tr := machine.NewTracker()
	arb := session.New(session.Config{
		Tracker: tr,
		Dial: func() (*client.Conn, error) {
			return client.Dial(m.Addr(), 2*time.Second, client.WithUploadStartDelay(0))
		},
	})
	svc, err := New(st, arb)
	if err != nil {
		t.Fatal(err)
	}
	return svc, m, tr
}

func putCachedEntry(t *testing.T, svc *Service, remotePath string, content []byte, sync store.SyncState) store.Entry {
	t.Helper()
	remote, err := normalizeRemote(remotePath)
	if err != nil {
		t.Fatal(err)
	}
	cachePath := svc.cacheNameFor(remote)
	if err := os.WriteFile(cachePath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	entry := store.Entry{
		Path:      remote,
		Size:      int64(len(content)),
		MTime:     time.Now(),
		MD5:       md5hex(content),
		CachePath: cachePath,
		Sync:      sync,
	}
	if err := svc.store.PutEntry(entry); err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestSelectActiveGcodeParsesPreview(t *testing.T) {
	svc, _ := newService(t)
	content := []byte("T2 M6\nG90\nG0 X0 Y0 Z5\nG1 X10 Y0 Z-1\nG1 X10 Y5\n")
	putCachedEntry(t, svc, "part.nc", content, store.Synced)

	active, err := svc.SelectActiveGcode("part.nc")
	if err != nil {
		t.Fatalf("SelectActiveGcode: %v", err)
	}
	if active.Path != "/sd/gcodes/part.nc" || !active.Runnable {
		t.Fatalf("active = %+v, want runnable part.nc", active)
	}
	if active.Preview == nil || active.Preview.LineCount != 5 || active.Preview.MoveCount != 3 || active.Preview.PlottedSegments != 3 {
		t.Fatalf("preview = %+v", active.Preview)
	}
	if len(active.Preview.Tools) != 1 || active.Preview.Tools[0] != 2 {
		t.Fatalf("tools = %v, want [2]", active.Preview.Tools)
	}
	if active.Preview.Bounds == nil || active.Preview.Bounds.Max[0] != 10 || active.Preview.Bounds.Max[1] != 5 || active.Preview.Bounds.Min[2] != -1 {
		t.Fatalf("bounds = %+v", active.Preview.Bounds)
	}
}

func TestParseGcodePreviewCoversCarveraMotionModes(t *testing.T) {
	gcode := strings.Join([]string{
		"G21 G90 G17",
		"G0 X0 Y0 Z5",
		"G1 X10 Y0 Z0",
		"G2 X10 Y10 I0 J5",
		"G18 G3 X0 Z0 I-5 K0",
		"G38.2 Z-2 F50",
		"G1 A90",
		"G92.4 A0 S0",
		"G98 G81 X5 Y5 Z-3 R1 F80",
		"G99 G83 X6 Y5 Z-6 R1 Q2 F80",
		"G80",
		"G17 G2 I5 J0",
	}, "\n")

	preview, err := ParseGcodePreview(strings.NewReader(gcode))
	if err != nil {
		t.Fatalf("ParseGcodePreview: %v", err)
	}
	if !preview.Has4Axis {
		t.Fatalf("Has4Axis = false, want true")
	}
	if preview.MoveCount < 12 || preview.PlottedSegments <= preview.MoveCount || preview.TotalDistance <= 0 {
		t.Fatalf("preview counters = moves %d plotted %d distance %.3f", preview.MoveCount, preview.PlottedSegments, preview.TotalDistance)
	}
	kinds := map[string]int{}
	for _, seg := range preview.Segments {
		kinds[seg.Kind]++
		if len(seg.From) != 4 || len(seg.To) != 4 {
			t.Fatalf("segment is not 4-axis aware: %+v", seg)
		}
		if seg.DistanceEnd < seg.DistanceStart {
			t.Fatalf("segment distance regressed: %+v", seg)
		}
	}
	for _, kind := range []string{"rapid", "cut", "arc", "probe"} {
		if kinds[kind] == 0 {
			t.Fatalf("kind %q missing from preview, counts=%v", kind, kinds)
		}
	}
	if preview.Bounds == nil || preview.Bounds.MinA > -89.9 || preview.Bounds.Max[0] < 10 || preview.Bounds.Min[2] > -6 {
		t.Fatalf("bounds = %+v", preview.Bounds)
	}
}

func TestRunActiveGcodeSendsPlayCommand(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	tr.Observe(machine.Idle)
	putCachedEntry(t, svc, "my part.nc", []byte("G1 X1\n"), store.Synced)
	if _, err := svc.SelectActiveGcode("my part.nc"); err != nil {
		t.Fatalf("select: %v", err)
	}

	res, err := svc.RunActiveGcode()
	if err != nil {
		t.Fatalf("run active: %v", err)
	}
	if res.Command != "play /sd/gcodes/my part.nc" {
		t.Fatalf("result = %+v", res)
	}
	if g := m.Gcodes(); len(g) != 1 || g[0] != "play /sd/gcodes/my part.nc" {
		t.Fatalf("machine gcodes = %v, want play command", g)
	}
}

func TestRunActiveGcodeRejectsUnsyncedSelection(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	tr.Observe(machine.Idle)
	putCachedEntry(t, svc, "queued.nc", []byte("G1 X1\n"), store.PendingUpload)
	active, err := svc.SelectActiveGcode("queued.nc")
	if err != nil {
		t.Fatalf("select pending upload: %v", err)
	}
	if active.Runnable {
		t.Fatalf("pending upload should not be runnable: %+v", active)
	}
	if _, err := svc.RunActiveGcode(); !errors.Is(err, ErrActiveGcodeUnavailable) {
		t.Fatalf("RunActiveGcode err = %v, want ErrActiveGcodeUnavailable", err)
	}
	if g := m.Gcodes(); len(g) != 0 {
		t.Fatalf("unsynced run leaked to machine: %v", g)
	}
}

func TestToolActionsSendControllerCommands(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	tr.Observe(machine.Idle)

	if res, err := svc.SetCurrentToolID(3); err != nil || res.Command != "M493.2T3" {
		t.Fatalf("SetCurrentToolID result=%+v err=%v", res, err)
	}
	if res, err := svc.SetCurrentToolID(0); err != nil || res.Command != "M493.2T0" {
		t.Fatalf("SetCurrentToolID probe result=%+v err=%v", res, err)
	}
	if res, err := svc.SetCurrentToolID(-1); err != nil || res.Command != "M493.2T-1" {
		t.Fatalf("SetCurrentToolID empty result=%+v err=%v", res, err)
	}
	if res, err := svc.SetCurrentToolID(8888); err != nil || res.Command != "M493.2T8888" {
		t.Fatalf("SetCurrentToolID laser result=%+v err=%v", res, err)
	}
	if res, err := svc.ChangeTool(4); err != nil || res.Command != "M6T4" {
		t.Fatalf("ChangeTool result=%+v err=%v", res, err)
	}
	if res, err := svc.DropCurrentTool(); err != nil || res.Command != "M6T-1" {
		t.Fatalf("DropCurrentTool result=%+v err=%v", res, err)
	}
	if res, err := svc.CalibrateCurrentTool(); err != nil || res.Command != "M491" {
		t.Fatalf("CalibrateCurrentTool result=%+v err=%v", res, err)
	}
	if g := m.Gcodes(); len(g) != 7 ||
		g[0] != "M493.2T3" ||
		g[1] != "M493.2T0" ||
		g[2] != "M493.2T-1" ||
		g[3] != "M493.2T8888" ||
		g[4] != "M6T4" ||
		g[5] != "M6T-1" ||
		g[6] != "M491" {
		t.Fatalf("machine gcodes = %v, want vendor tool commands", g)
	}
}

func TestSetCurrentToolIDValidation(t *testing.T) {
	svc, _, _ := serviceWithMachine(t)
	if _, err := svc.SetCurrentToolID(1000); err == nil {
		t.Fatal("expected tool_id 1000 to be rejected")
	}
	if _, err := svc.SetCurrentToolID(-2); err == nil {
		t.Fatal("expected tool_id -2 to be rejected")
	}
	if _, err := svc.ChangeTool(-1); err == nil {
		t.Fatal("expected change tool_id -1 to be rejected")
	}
}

// TestSendGcodeQueryRunsRegardlessOfState confirms a read-only query (M114)
// runs even while the machine is in Run state (e.g. a controller program), and
// returns the machine's payload.
func TestSendGcodeQueryRunsRegardlessOfState(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	m.SetGcodeReply("M114", "ok C: X:1.000 Y:2.000 Z:3.000")
	tr.Observe(machine.Run) // a program is running

	out, err := svc.SendGcode("M114")
	if err != nil {
		t.Fatalf("M114 during Run should be allowed: %v", err)
	}
	if out != "C: X:1.000 Y:2.000 Z:3.000" {
		t.Errorf("M114 out = %q", out)
	}
	if g := m.Gcodes(); len(g) != 1 || g[0] != "M114" {
		t.Errorf("machine gcodes = %v", g)
	}
}

// TestSendGcodeMotionRequiresIdle confirms a motion command is rejected (and
// never reaches the machine) while a program runs, but succeeds once Idle.
func TestSendGcodeMotionRequiresIdle(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)

	m.SetStatus("<Run|MPos:0,0,0|WPos:0,0,0>")
	tr.Observe(machine.Run)
	_, err := svc.SendGcode("G91 G0 X-10")
	if !session.Retryable(err) {
		t.Fatalf("motion during Run = %v, want retryable ErrNotIdle", err)
	}
	if g := m.Gcodes(); len(g) != 0 {
		t.Fatalf("motion leaked to machine during Run: %v", g)
	}

	// Now Idle: the move is accepted and reaches the machine.
	m.SetStatus("<Idle|MPos:0,0,0|WPos:0,0,0>")
	tr.Observe(machine.Idle)
	if _, err := svc.SendGcode("G91 G0 X-10"); err != nil {
		t.Fatalf("motion during Idle: %v", err)
	}
	if g := m.Gcodes(); len(g) != 1 || g[0] != "G91 G0 X-10" {
		t.Errorf("machine gcodes = %v, want the move", g)
	}
}

// TestSendGcodeMotionDoesNotWaitForOk is the regression for the "second move
// spins forever" bug. The firmware sends NO terminating "ok" for motion gcode
// over WiFi (verified on hardware), so SendGcode must NOT block waiting for one
// — if it did, the first move would hold opMu until the command timeout and
// every later command would queue behind it. This sends several motion commands
// back-to-back; each must return promptly and all must reach the machine in
// order. A regression (waiting for ok) makes this hang well past the deadline.
func TestSendGcodeMotionDoesNotWaitForOk(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	tr.Observe(machine.Idle)

	moves := []string{"G91 G0 X-10", "G91 G0 X10", "G91 G0 X-10", "G91 G0 X10"}
	done := make(chan error, 1)
	go func() {
		for _, mv := range moves {
			if _, err := svc.SendGcode(mv); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("sequential motion: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sequential motion commands hung — SendGcode is waiting for an ok the firmware never sends")
	}

	if g := m.Gcodes(); len(g) != len(moves) {
		t.Fatalf("machine received %d gcodes, want %d: %v", len(g), len(moves), g)
	}
}

// TestSendControlNotIdleGated confirms feed-hold/resume/halt reach the machine
// even while it is running — that is the whole point of realtime control.
func TestSendControlNotIdleGated(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	tr.Observe(machine.Run)

	if err := svc.SendControl(ControlFeedHold); err != nil {
		t.Fatalf("feed-hold during Run: %v", err)
	}
	if err := svc.SendControl(ControlHalt); err != nil {
		t.Fatalf("halt during Run: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(m.Controls()) < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := m.Controls(); len(got) != 2 || got[0] != '!' || got[1] != 0x18 {
		t.Errorf("controls = %v, want [! 0x18]", got)
	}
}

// TestSendControlRejectsUnknown guards the action-mapping.
func TestSendControlRejectsUnknown(t *testing.T) {
	svc, _, _ := serviceWithMachine(t)
	if err := svc.SendControl('Q'); err == nil {
		t.Error("expected error for unsupported control char")
	}
}

func TestRecoverAlarmSoftLimitUnlocksAndVerifies(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	if !tr.ObserveStatusPayload("<Alarm|MPos:0,0,0|WPos:0,0,0|H:10>") {
		t.Fatal("alarm status should parse")
	}

	start := time.Now()
	res, err := svc.RecoverAlarm("recover")
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("recovery waited too long: %s", elapsed)
	}
	if !res.Recovered || res.State != machine.Idle || !res.NeedsHome {
		t.Fatalf("recovery result = %+v, want recovered Idle with needs_home", res)
	}
	if g := m.Gcodes(); len(g) != 1 || g[0] != "$X" {
		t.Fatalf("recovery gcodes = %v, want [$X]", g)
	}
}

func TestRecoverAlarmViaRelayInjectionVerifiesStatus(t *testing.T) {
	m, err := carveratest.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	status := "<Alarm|MPos:0,0,0|WPos:0,0,0|H:10>"
	m.SetStatus(status)

	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	tr := machine.NewTracker()
	arb := session.New(session.Config{
		Tracker: tr,
		Dial: func() (*client.Conn, error) {
			return client.Dial(m.Addr(), 2*time.Second, client.WithUploadStartDelay(0))
		},
	})
	relaySrv := &relay.Server{
		Dial:     func() (string, error) { return m.Addr(), nil },
		Observer: arb,
	}
	arb.SetInjector(relayAdapter{relaySrv})
	arb.SetControlWriter(relaySrv)
	svc, err := New(st, arb)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go relaySrv.Serve(ln)

	controller, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close() })
	if _, err := controller.Write(protocol.QueryStatus()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		st, _ := tr.Current()
		if arb.Mode() == session.ModeRelay && st.State == machine.Alarm {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("relay did not observe initial alarm status; mode=%s status=%+v", arb.Mode(), st)
		}
		time.Sleep(10 * time.Millisecond)
	}

	res, err := svc.RecoverAlarm("recover")
	if err != nil {
		t.Fatalf("recover via relay: %v", err)
	}
	if !res.Recovered || res.State != machine.Idle || !res.NeedsHome {
		t.Fatalf("recovery result = %+v, want recovered Idle with needs_home", res)
	}
	if g := m.Gcodes(); len(g) != 1 || g[0] != "$X" {
		t.Fatalf("recovery gcodes = %v, want [$X]", g)
	}
}

type relayAdapter struct{ srv *relay.Server }

func (a relayAdapter) AcquireMachine() (session.InjectTransport, func(), error) {
	it, release, err := a.srv.AcquireMachine()
	if err != nil {
		return nil, nil, err
	}
	return it, release, nil
}

func TestRecoverAlarmSoftLimitFallsBackToM999(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	m.SetUnlockDoesNotClear(true)
	if !tr.ObserveStatusPayload("<Alarm|MPos:0,0,0|WPos:0,0,0|H:10>") {
		t.Fatal("alarm status should parse")
	}
	m.SetStatus("<Alarm|MPos:0,0,0|WPos:0,0,0|H:10>")

	res, err := svc.RecoverAlarm("recover")
	if err != nil {
		t.Fatalf("recover with M999 fallback: %v", err)
	}
	if !res.Recovered || res.State != machine.Idle {
		t.Fatalf("recovery result = %+v, want recovered Idle", res)
	}
	if g := m.Gcodes(); len(g) != 2 || g[0] != "$X" || g[1] != "M999" {
		t.Fatalf("recovery gcodes = %v, want [$X M999]", g)
	}
}

func TestRecoverAlarmStillAlarmReturnsUnavailable(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	m.SetUnlockDoesNotClear(true)
	m.SetM999DoesNotClear(true)
	status := "<Alarm|MPos:0,0,0|WPos:0,0,0|H:10>"
	m.SetStatus(status)
	if !tr.ObserveStatusPayload(status) {
		t.Fatal("alarm status should parse")
	}

	res, err := svc.RecoverAlarm("recover")
	if err == nil {
		t.Fatal("expected recovery failure")
	}
	if !errors.Is(err, ErrRecoveryUnavailable) {
		t.Fatalf("recover err = %v, want ErrRecoveryUnavailable", err)
	}
	if res.Recovered || res.State != machine.Alarm {
		t.Fatalf("recovery result = %+v, want unrecovered Alarm", res)
	}
	if g := m.Gcodes(); len(g) != 2 || g[0] != "$X" || g[1] != "M999" {
		t.Fatalf("recovery gcodes = %v, want [$X M999]", g)
	}
}

func TestRecoverAlarmHomeBypassesAlarmIdleGate(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	if !tr.ObserveStatusPayload("<Alarm|MPos:0,0,0|WPos:0,0,0|H:10>") {
		t.Fatal("alarm status should parse")
	}

	res, err := svc.RecoverAlarm("home")
	if err != nil {
		t.Fatalf("home: %v", err)
	}
	if !res.Recovered || res.State != machine.Idle {
		t.Fatalf("home result = %+v, want recovered Idle", res)
	}
	if g := m.Gcodes(); len(g) != 1 || g[0] != "$H" {
		t.Fatalf("recovery gcodes = %v, want [$H]", g)
	}
}

func TestRecoverAlarmRefreshesStaleStatus(t *testing.T) {
	m, err := carveratest.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	status := "<Alarm|MPos:0,0,0|WPos:0,0,0|H:10>"
	m.SetStatus(status)
	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	tr := machine.NewTracker()
	if !tr.ObserveStatusPayload(status) {
		t.Fatal("alarm status should parse")
	}
	arb := session.New(session.Config{
		Tracker:     tr,
		StateMaxAge: 50 * time.Millisecond,
		Dial: func() (*client.Conn, error) {
			return client.Dial(m.Addr(), 2*time.Second, client.WithUploadStartDelay(0))
		},
	})
	svc, err := New(st, arb)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(75 * time.Millisecond)

	res, err := svc.RecoverAlarm("unlock")
	if err != nil {
		t.Fatalf("unlock with stale cached status should refresh first: %v", err)
	}
	if !res.Recovered || res.State != machine.Idle {
		t.Fatalf("recovery result = %+v, want recovered Idle", res)
	}
	if g := m.Gcodes(); len(g) != 1 || g[0] != "$X" {
		t.Fatalf("recovery gcodes = %v, want [$X]", g)
	}
}

func TestBackupExportImportRoundTrip(t *testing.T) {
	svc, _ := newService(t)
	if _, err := svc.Upload("part.nc", strings.NewReader("G0 X0\n")); err != nil {
		t.Fatal(err)
	}
	ui, err := svc.SetUISettings(store.UISettings{
		Macros:       []store.Macro{{ID: "m1", Name: "Position", Lines: []string{"M114"}}},
		MacroButtons: []store.MacroSlot{{ID: "s1", MacroID: "m1", Region: "toolbar"}},
		Log:          store.LogSettings{Filter: "all", Autoscroll: true},
		Gamepad:      store.Gamepad{DeadmanButton: 2},
	})
	if err != nil || len(ui.Macros) != 1 {
		t.Fatalf("settings = %+v err=%v", ui, err)
	}
	svc.gcodeLog.Append("send", "api", "M114")
	svc.runHistory.Replace([]runhistory.Run{{ID: 1, File: "part.nc", StartedAt: time.Unix(1000, 0)}})
	backup := svc.ExportBackup()

	restored, _ := newService(t)
	if err := restored.ImportBackup(backup); err != nil {
		t.Fatal(err)
	}
	if _, ok := restored.Lookup("part.nc"); !ok {
		t.Fatal("restored backup missing catalog entry")
	}
	if got := restored.UISettings(); len(got.Macros) != 1 || got.Macros[0].Name != "Position" || got.Gamepad.DeadmanButton != 2 {
		t.Fatalf("restored UI = %+v", got)
	}
	if lines := restored.GcodeLog().Recent(); len(lines) != 1 || lines[0].Text != "M114" {
		t.Fatalf("restored log = %+v", lines)
	}
	if runs := restored.RunHistory(); len(runs) != 1 || runs[0].File != "part.nc" {
		t.Fatalf("restored runs = %+v", runs)
	}
}

func TestJobDiagnostics(t *testing.T) {
	svc, st := newService(t)
	st.Enqueue(store.Job{Kind: store.JobUpload, Path: "/sd/gcodes/a.nc"})
	if got := svc.Jobs()[0]; got.BlockedReason != "stale_status" {
		t.Fatalf("stale diagnostic = %+v", got)
	}
	svc.arb.Tracker().Observe(machine.Run)
	if got := svc.Jobs()[0]; got.BlockedReason != "not_idle" {
		t.Fatalf("run diagnostic = %+v", got)
	}
	svc.arb.Tracker().Observe(machine.Idle)
	if got := svc.Jobs()[0]; got.BlockedReason != "ready" {
		t.Fatalf("ready diagnostic = %+v", got)
	}
	svc.arb.EnterRelay()
	if got := svc.Jobs()[0]; got.BlockedReason != "relay_active" {
		t.Fatalf("relay diagnostic = %+v", got)
	}
	svc.arb.ExitRelay()
	if err := st.UpdateJob(1, func(j *store.Job) {
		j.Attempts = 1
		j.LastError = "temporary"
	}); err != nil {
		t.Fatal(err)
	}
	if got := svc.Jobs()[0]; got.BlockedReason != "backoff" || got.BlockedUntil == nil {
		t.Fatalf("backoff diagnostic = %+v", got)
	}
}

func TestPruneCacheRemovesOnlyUnreferencedOldFiles(t *testing.T) {
	svc, st := newService(t)
	cacheDir := st.CacheDir()
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ref := filepath.Join(cacheDir, "referenced")
	orphan := filepath.Join(cacheDir, "orphan")
	temp := filepath.Join(cacheDir, "upload-old.tmp")
	for _, p := range []string{ref, orphan, temp} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PutEntry(store.Entry{Path: "/sd/gcodes/a.nc", CachePath: ref, Sync: store.Synced}); err != nil {
		t.Fatal(err)
	}
	report, err := svc.PruneCache(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if report.FilesRemoved != 2 {
		t.Fatalf("report = %+v, want two files removed", report)
	}
	if _, err := os.Stat(ref); err != nil {
		t.Fatalf("referenced cache should remain: %v", err)
	}
	for _, p := range []string{orphan, temp} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s stat = %v, want removed", p, err)
		}
	}
}
