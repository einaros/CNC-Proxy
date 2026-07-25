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
	"net"
	"os"
	"path/filepath"
	"slices"
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
	svc, st, _ := newServiceWithStatePath(t)
	return svc, st
}

func newServiceWithStatePath(t *testing.T) (*Service, *store.Store, string) {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "state.json")
	st, _ := store.Open(statePath)
	arb := session.New(session.Config{
		Dial: func() (*client.Conn, error) { return nil, io.EOF }, // never dialed in these tests
	})
	svc, err := New(st, arb)
	if err != nil {
		t.Fatal(err)
	}
	return svc, st, statePath
}

func forceStoreFlushFailure(t *testing.T, statePath string) {
	t.Helper()
	if err := os.Remove(statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath, 0o755); err != nil {
		t.Fatal(err)
	}
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

func TestUploadStoreFailureRollsBackCatalogJobAndCache(t *testing.T) {
	svc, st, statePath := newServiceWithStatePath(t)
	oldContent := []byte("G0 X0\n")
	oldEntry := putCachedEntry(t, svc, "part.nc", oldContent, store.Synced)
	forceStoreFlushFailure(t, statePath)

	if _, err := svc.Upload("part.nc", bytes.NewReader([]byte("G1 X1\n"))); err == nil {
		t.Fatal("Upload succeeded despite store flush failure")
	}
	entry, ok := st.GetEntry(oldEntry.Path)
	if !ok || entry.Sync != store.Synced || entry.MD5 != oldEntry.MD5 || entry.CachePath != oldEntry.CachePath {
		t.Fatalf("entry after failed upload = %+v ok=%v, want original %+v", entry, ok, oldEntry)
	}
	got, err := os.ReadFile(oldEntry.CachePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, oldContent) {
		t.Fatalf("cache after failed upload = %q, want %q", string(got), string(oldContent))
	}
	if jobs := st.ListJobs(); len(jobs) != 0 {
		t.Fatalf("failed upload queued jobs: %+v", jobs)
	}
}

func TestUploadRangeStoreFailureRollsBackCache(t *testing.T) {
	svc, st, statePath := newServiceWithStatePath(t)
	oldContent := []byte("old\n")
	oldEntry := putCachedEntry(t, svc, "range.nc", oldContent, store.LocalOnly)
	forceStoreFlushFailure(t, statePath)

	newContent := []byte("new content\n")
	if _, _, err := svc.UploadRange("range.nc", 0, int64(len(newContent)-1), int64(len(newContent)), bytes.NewReader(newContent)); err == nil {
		t.Fatal("UploadRange succeeded despite store flush failure")
	}
	entry, ok := st.GetEntry(oldEntry.Path)
	if !ok || entry.Size != oldEntry.Size || entry.MD5 != oldEntry.MD5 || entry.CachePath != oldEntry.CachePath {
		t.Fatalf("entry after failed range upload = %+v ok=%v, want original %+v", entry, ok, oldEntry)
	}
	got, err := os.ReadFile(oldEntry.CachePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, oldContent) {
		t.Fatalf("cache after failed range upload = %q, want %q", string(got), string(oldContent))
	}
	if jobs := st.ListJobs(); len(jobs) != 0 {
		t.Fatalf("failed range upload queued jobs: %+v", jobs)
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

func TestUploadRejectsJunkFilename(t *testing.T) {
	svc, st := newService(t)
	for _, name := range []string{"._part.nc", ".DS_Store", "sub/Thumbs.db"} {
		if _, err := svc.Upload(name, bytes.NewReader([]byte("junk"))); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("Upload(%q) = %v, want ErrInvalidArgument", name, err)
		}
	}
	if entries := st.ListEntries(); len(entries) != 0 {
		t.Fatalf("junk leaked into catalog: %+v", entries)
	}
	if jobs := st.ListJobs(); len(jobs) != 0 {
		t.Fatalf("junk leaked into queue: %+v", jobs)
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

func TestDeleteSyncedStoreFailureLeavesNoPartialJob(t *testing.T) {
	svc, st, statePath := newServiceWithStatePath(t)
	entry := putCachedEntry(t, svc, "delete.nc", []byte("G0 X0\n"), store.Synced)
	forceStoreFlushFailure(t, statePath)

	if err := svc.Delete("delete.nc"); err == nil {
		t.Fatal("Delete succeeded despite store flush failure")
	}
	got, ok := st.GetEntry(entry.Path)
	if !ok || got.Sync != store.Synced {
		t.Fatalf("entry after failed delete = %+v ok=%v, want synced", got, ok)
	}
	if jobs := st.ListJobs(); len(jobs) != 0 {
		t.Fatalf("failed delete queued jobs: %+v", jobs)
	}
}

func TestRemoteRenameStoreFailureLeavesNoPartialJob(t *testing.T) {
	svc, st, statePath := newServiceWithStatePath(t)
	entry := putCachedEntry(t, svc, "remote.nc", []byte("G0 X0\n"), store.Synced)
	forceStoreFlushFailure(t, statePath)

	if err := svc.Rename("remote.nc", "renamed.nc"); err == nil {
		t.Fatal("Rename succeeded despite store flush failure")
	}
	got, ok := st.GetEntry(entry.Path)
	if !ok || got.Sync != store.Synced {
		t.Fatalf("entry after failed remote rename = %+v ok=%v, want synced", got, ok)
	}
	if _, ok := st.GetEntry("/sd/gcodes/renamed.nc"); ok {
		t.Fatal("destination entry leaked after failed remote rename")
	}
	if jobs := st.ListJobs(); len(jobs) != 0 {
		t.Fatalf("failed remote rename queued jobs: %+v", jobs)
	}
}

func TestMkdirStoreFailureLeavesNoPartialEntryOrJob(t *testing.T) {
	svc, st, statePath := newServiceWithStatePath(t)
	forceStoreFlushFailure(t, statePath)

	if _, err := svc.Mkdir("newdir"); err == nil {
		t.Fatal("Mkdir succeeded despite store flush failure")
	}
	if _, ok := st.GetEntry("/sd/gcodes/newdir"); ok {
		t.Fatal("mkdir entry leaked after failed store commit")
	}
	if jobs := st.ListJobs(); len(jobs) != 0 {
		t.Fatalf("failed mkdir queued jobs: %+v", jobs)
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

func TestRenamePendingUploadStoreFailureRollsBackCacheMove(t *testing.T) {
	svc, st, statePath := newServiceWithStatePath(t)
	content := []byte("G0 X0\nG1 X1\n")
	entry, err := svc.Upload("upload.tmp", bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	destCache := svc.cacheNameFor("/sd/gcodes/final.nc")
	forceStoreFlushFailure(t, statePath)

	if err := svc.Rename("upload.tmp", "final.nc"); err == nil {
		t.Fatal("Rename succeeded despite store flush failure")
	}
	gotEntry, ok := st.GetEntry(entry.Path)
	if !ok || gotEntry.CachePath != entry.CachePath || gotEntry.Sync != store.PendingUpload {
		t.Fatalf("source entry after failed rename = %+v ok=%v, want original %+v", gotEntry, ok, entry)
	}
	if _, ok := st.GetEntry("/sd/gcodes/final.nc"); ok {
		t.Fatal("destination entry leaked after failed rename")
	}
	got, err := os.ReadFile(entry.CachePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("source cache after failed rename = %q, want %q", string(got), string(content))
	}
	if _, err := os.Stat(destCache); err == nil {
		t.Fatal("destination cache file leaked after failed rename")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
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

func TestRetryStoreFailureLeavesJobAndEntryFailed(t *testing.T) {
	svc, st, statePath := newServiceWithStatePath(t)
	entry, err := svc.Upload("retry-fail.nc", bytes.NewReader([]byte("x")))
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
	if err := st.SetEntrySync(entry.Path, store.Error, "upload failed"); err != nil {
		t.Fatal(err)
	}
	forceStoreFlushFailure(t, statePath)

	if _, err := svc.RetryJob(1); err == nil {
		t.Fatal("RetryJob succeeded despite store flush failure")
	}
	job := st.ListJobs()[0]
	if job.State != store.Failed || job.Attempts != 8 || job.LastError != "upload failed" {
		t.Fatalf("job after failed retry = %+v, want original failed job", job)
	}
	got, ok := st.GetEntry(entry.Path)
	if !ok || got.Sync != store.Error || got.Error != "upload failed" {
		t.Fatalf("entry after failed retry = %+v ok=%v, want original error entry", got, ok)
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

// TestFetchToCacheDoesNotClobberConcurrentUpload guards the FetchToCache commit
// against a local write that lands while the (slow) download is in flight. The
// newer local write must win: its cache bytes, catalog entry, and queued upload
// survive; the downloaded machine content is discarded.
func TestFetchToCacheDoesNotClobberConcurrentUpload(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	tr.Observe(machine.Idle)

	oldContent := bytes.Repeat([]byte("G0 X0 ; machine copy\n"), 400) // ~8.4 KB
	seedOnMachine(t, m.Addr(), "/sd/gcodes/race-fetch.nc", oldContent)
	if err := svc.PutRemoteOnly("race-fetch.nc", int64(len(oldContent)), time.Unix(0, 0), ""); err != nil {
		t.Fatal(err)
	}

	// Make the download slow enough that the concurrent write lands mid-fetch.
	m.SetDownloadPacketSize(1024)
	m.SetDownloadPacketDelay(60 * time.Millisecond) // ~9 packets => ~540ms

	fetchDone := make(chan error, 1)
	go func() { fetchDone <- svc.FetchToCache("race-fetch.nc") }()

	// Let the fetch get in flight, then write new content to the same path.
	time.Sleep(150 * time.Millisecond)
	newContent := []byte("G1 X99 ; operator's newer write\n")
	if _, err := svc.Upload("race-fetch.nc", bytes.NewReader(newContent)); err != nil {
		t.Fatalf("concurrent upload: %v", err)
	}

	select {
	case err := <-fetchDone:
		if err != nil {
			t.Fatalf("FetchToCache: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("FetchToCache did not finish")
	}

	entry, ok := svc.Lookup("race-fetch.nc")
	if !ok {
		t.Fatal("entry missing after fetch+upload")
	}
	if entry.Sync != store.PendingUpload {
		t.Fatalf("entry sync = %q, want pending_upload (fetch must not revert the newer write)", entry.Sync)
	}
	if entry.MD5 != md5hex(newContent) {
		t.Fatalf("entry md5 = %s, want the newer write's %s", entry.MD5, md5hex(newContent))
	}
	got, err := os.ReadFile(entry.CachePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, newContent) {
		t.Fatalf("cache = %d bytes (md5 %s), want the newer write's content", len(got), md5hex(got))
	}
	var queuedUploads int
	for _, j := range svc.store.ListJobs() {
		if j.Kind == store.JobUpload && j.Path == entry.Path && j.State == store.Queued && j.MD5 == md5hex(newContent) {
			queuedUploads++
		}
	}
	if queuedUploads != 1 {
		t.Fatalf("jobs = %+v, want one queued upload of the newer content", svc.store.ListJobs())
	}
}

// TestRetryStaleFailedUploadDoesNotStrandSyncedEntry guards RetryJob against a
// Failed upload job whose content the catalog no longer holds: retrying it must
// not drag a Synced entry back to pending_upload (where the requeued job's
// IfMatch no-op would strand it forever).
func TestRetryStaleFailedUploadDoesNotStrandSyncedEntry(t *testing.T) {
	svc, st := newService(t)
	oldContent := []byte("old content\n")
	if _, err := svc.Upload("stale-retry.nc", bytes.NewReader(oldContent)); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateJob(1, func(j *store.Job) {
		j.State = store.Failed
		j.Attempts = 8
		j.LastError = "upload failed"
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEntrySync("/sd/gcodes/stale-retry.nc", store.Error, "upload failed"); err != nil {
		t.Fatal(err)
	}

	// A newer upload of the same path replaces the content...
	newContent := []byte("new content wins\n")
	if _, err := svc.Upload("stale-retry.nc", bytes.NewReader(newContent)); err != nil {
		t.Fatal(err)
	}
	// ...and the old failed job can never usefully run again: it must be
	// superseded, not left Failed.
	if job, ok := st.GetJob(1); !ok || job.State != store.Done {
		t.Fatalf("old failed upload after replacement upload = %+v ok=%v, want superseded (done)", job, ok)
	}
	// Simulate the engine syncing the new upload.
	if err := st.UpdateJob(2, func(j *store.Job) { j.State = store.Done }); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEntrySync("/sd/gcodes/stale-retry.nc", store.Synced, ""); err != nil {
		t.Fatal(err)
	}

	// Retrying the stale job must not drag the entry backward. (Post-supersede
	// it reports retry-unavailable; the entry must stay synced either way.)
	if _, err := svc.RetryJob(1); err != nil && !errors.Is(err, ErrRetryUnavailable) {
		t.Fatalf("RetryJob stale = %v, want nil or ErrRetryUnavailable", err)
	}
	entry, ok := st.GetEntry("/sd/gcodes/stale-retry.nc")
	if !ok || entry.Sync != store.Synced || entry.MD5 != md5hex(newContent) {
		t.Fatalf("entry after stale retry = %+v ok=%v, want synced with the new content", entry, ok)
	}

	// Same guard for a Failed job that supersede never saw (e.g. created before
	// the entry was replaced through another path): retry may requeue it, but
	// the mismatched entry must be left synced for the engine's IfMatch no-op.
	staleJob, err := st.Enqueue(store.Job{
		Kind:      store.JobUpload,
		Path:      "/sd/gcodes/stale-retry.nc",
		CachePath: filepath.Join(st.CacheDir(), "gone-cache"),
		MD5:       "0123456789abcdef0123456789abcdef",
		Size:      3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateJob(staleJob.ID, func(j *store.Job) {
		j.State = store.Failed
		j.Attempts = 8
		j.LastError = "upload failed"
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RetryJob(staleJob.ID); err != nil {
		t.Fatalf("RetryJob mismatched stale job: %v", err)
	}
	entry, ok = st.GetEntry("/sd/gcodes/stale-retry.nc")
	if !ok || entry.Sync != store.Synced || entry.MD5 != md5hex(newContent) {
		t.Fatalf("entry after mismatched-job retry = %+v ok=%v, want untouched synced entry", entry, ok)
	}
}

// TestDeleteNonEmptyDirRejected: deleting a directory that still has catalog
// entries under it is rejected with a specific error and no queue/catalog
// mutation (the chosen F4 strategy is rejection, consistent across API+davfs
// because both go through the service boundary).
func TestDeleteNonEmptyDirRejected(t *testing.T) {
	svc, st := newService(t)
	if err := st.PutEntry(store.Entry{Path: "/sd/gcodes/full", IsDir: true, Sync: store.Synced}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutEntry(store.Entry{Path: "/sd/gcodes/full/child.nc", Sync: store.Synced}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete("full"); !errors.Is(err, ErrDirectoryNotEmpty) {
		t.Fatalf("delete non-empty dir = %v, want ErrDirectoryNotEmpty", err)
	}
	if e, ok := st.GetEntry("/sd/gcodes/full"); !ok || e.Sync != store.Synced {
		t.Fatalf("dir entry after rejected delete = %+v ok=%v, want unchanged synced", e, ok)
	}
	if jobs := st.ListJobs(); len(jobs) != 0 {
		t.Fatalf("rejected delete queued jobs: %+v", jobs)
	}
}

// TestRenameNonEmptyDirRejected mirrors TestDeleteNonEmptyDirRejected for the
// rename surface.
func TestRenameNonEmptyDirRejected(t *testing.T) {
	svc, st := newService(t)
	if err := st.PutEntry(store.Entry{Path: "/sd/gcodes/full", IsDir: true, Sync: store.Synced}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutEntry(store.Entry{Path: "/sd/gcodes/full/child.nc", Sync: store.Synced}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Rename("full", "renamed"); !errors.Is(err, ErrDirectoryNotEmpty) {
		t.Fatalf("rename non-empty dir = %v, want ErrDirectoryNotEmpty", err)
	}
	if e, ok := st.GetEntry("/sd/gcodes/full"); !ok || e.Sync != store.Synced {
		t.Fatalf("dir entry after rejected rename = %+v ok=%v, want unchanged synced", e, ok)
	}
	if _, ok := st.GetEntry("/sd/gcodes/renamed"); ok {
		t.Fatal("destination entry leaked after rejected rename")
	}
	if jobs := st.ListJobs(); len(jobs) != 0 {
		t.Fatalf("rejected rename queued jobs: %+v", jobs)
	}
}

func TestReadCacheBlocksValidationPending(t *testing.T) {
	svc, st := newService(t)
	content := []byte("G0 X0\n")
	cachePath := filepath.Join(st.CacheDir(), "validating-cache")
	if err := os.WriteFile(cachePath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.PutEntry(store.Entry{
		Path:       "/sd/gcodes/validating.nc",
		Size:       int64(len(content)),
		MD5:        md5hex(content),
		CachePath:  cachePath,
		CacheState: store.CacheValidating,
		Sync:       store.Synced,
	}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := svc.ReadCache("validating.nc"); !errors.Is(err, ErrCacheValidationPending) {
		t.Fatalf("ReadCache validating = %v, want ErrCacheValidationPending", err)
	}
	if _, _, err := svc.Open("validating.nc"); !errors.Is(err, ErrCacheValidationPending) {
		t.Fatalf("Open validating = %v, want ErrCacheValidationPending", err)
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

func TestLearnMachineParametersPersistsConfigDrivenProfile(t *testing.T) {
	svc, m, _ := serviceWithMachine(t)
	m.SetFtype("lz")
	m.SetGcodeReply("model", "model = CarveraAir,FACTORY,PROBE")
	m.SetGcodeReply("version", "version = 1.2.3")
	m.PutFile("/sd/config.txt", []byte(strings.Join([]string{
		"soft_endstop.enable=false",
		"soft_endstop.x_min=-302.0",
		"soft_endstop.y_min=-212.0",
		"soft_endstop.z_min=-121.0",
		"alpha_max=0",
		"beta_max=0",
		"gamma_max=0",
		"delta_min=0",
		"delta_max=360",
		"default_feed_rate=1000",
		"default_seek_rate=3000",
		"alpha_max_rate=3000.0",
		"beta_max_rate=2800.0",
		"gamma_max_rate=2000.0",
		"delta_max_rate=1800.0",
		"epsilon_max_rate=100.0",
		"coordinate.clearance_x=-5.0",
		"coordinate.clearance_y=-21.0",
		"coordinate.clearance_z=-3.0",
		"coordinate.anchor1_x=-287.51",
		"coordinate.anchor1_y=-202.11",
		"coordinate.anchor2_offset_x=88.5",
		"coordinate.anchor2_offset_y=45.0",
		"atc.probe.fast_rate_mm_m=300",
		"atc.probe.slow_rate_mm_m=60",
		"atc.probe.retract_mm=2",
	}, "\n")))

	res, err := svc.LearnMachineParameters()
	if err != nil {
		t.Fatal(err)
	}
	if res.Learned.Identity.Model != "CarveraAir,FACTORY,PROBE" || res.Learned.Identity.Version != "1.2.3" || res.Learned.Identity.FileType != "lz" {
		t.Fatalf("identity = %+v", res.Learned.Identity)
	}
	if res.UI.Machine.WorkArea != (store.WorkArea{XMin: -302, XMax: -1, YMin: -212, YMax: -1}) {
		t.Fatalf("learned work area = %+v", res.UI.Machine.WorkArea)
	}
	if res.UI.Machine.FeedMaxMMMin != 2800 || res.Learned.Feed.MaxXYMMMin != 2800 {
		t.Fatalf("feed max = ui %v learned %+v", res.UI.Machine.FeedMaxMMMin, res.Learned.Feed)
	}
	if res.UI.Machine.SafeZMM != -3 || res.Learned.ZMinMM != -121 || res.Learned.ZMaxMM != 0 {
		t.Fatalf("Z profile = ui safe %v learned %+v", res.UI.Machine.SafeZMM, res.Learned)
	}
	if res.Learned.SoftEndstop.Enabled || res.Learned.Clearance.X != -5 || res.Learned.Probe.RetractMM != 2 {
		t.Fatalf("known config profile = %+v", res.Learned)
	}
	if res.Learned.SoftEndstop.XMax != -1 || res.Learned.SoftEndstop.YMax != -1 {
		t.Fatalf("firmware travel maxima = %+v", res.Learned.SoftEndstop)
	}
	if !res.Learned.Anchors.Available || res.Learned.Anchors.Anchor1 != (store.XYPoint{X: -287.51, Y: -202.11}) || res.Learned.Anchors.Anchor2 != (store.XYPoint{X: -199.01, Y: -157.11}) {
		t.Fatalf("learned anchors = %+v", res.Learned.Anchors)
	}
	if got := res.Learned.Config["soft_endstop.x_min"]; got != "-302.0" {
		t.Fatalf("raw config soft_endstop.x_min = %q", got)
	}
	gcodes := m.Gcodes()
	for _, want := range []string{"model", "version"} {
		if !slices.Contains(gcodes, want) {
			t.Fatalf("gcodes %v missing %q", gcodes, want)
		}
	}
	for _, got := range gcodes {
		if strings.HasPrefix(got, "config-") {
			t.Fatalf("learning sent a console config command %q instead of downloading /sd/config.txt", got)
		}
	}
}

func TestParseMachineConfigAcceptsVendorConfigFileSyntax(t *testing.T) {
	config := parseMachineConfig("# vendor config\ncoordinate.anchor1_x -287.51\ncoordinate.anchor1_y\t-202.11\nsoft_endstop.enable true\n")
	if config["coordinate.anchor1_x"] != "-287.51" || config["coordinate.anchor1_y"] != "-202.11" || config["soft_endstop.enable"] != "true" {
		t.Fatalf("parsed config = %+v", config)
	}
}

func TestBackgroundMachineLearningDoesNotRetryFailedGeneration(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	m.PutFile("/sd/config.txt", []byte("coordinate.anchor1_x -287.51\n"))
	m.SetGcodeReply("model", "error:Unsupported command - model")
	if err := svc.arb.WithMachine(false, func(*client.Conn) error { return nil }); err != nil {
		t.Fatal(err)
	}
	tr.Observe(machine.Idle)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.RunMachineLearning(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && countString(m.Gcodes(), "model") == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := countString(m.Gcodes(), "model"); got != 1 {
		t.Fatalf("initial learning attempts = %d, want 1", got)
	}
	for i := 0; i < 5; i++ {
		tr.Observe(machine.Idle)
	}
	time.Sleep(100 * time.Millisecond)
	if got := countString(m.Gcodes(), "model"); got != 1 {
		t.Fatalf("failed learning retried %d times in one connection generation", got)
	}
}

func countString(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}

func TestRunMachineLearningRefreshesEachNewConnectionAndPersistsProfile(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	m.SetFtype("lz")
	m.SetGcodeReply("model", "model = CarveraAir,FACTORY,PROBE")
	m.SetGcodeReply("version", "version = 1.2.3")
	m.PutFile("/sd/config.txt", []byte(strings.Join([]string{
		"soft_endstop.x_min=-302.0",
		"soft_endstop.y_min=-212.0",
		"coordinate.anchor1_x=-287.51",
		"coordinate.anchor1_y=-202.11",
		"coordinate.anchor2_offset_x=88.5",
		"coordinate.anchor2_offset_y=45.0",
	}, "\n")))

	if err := svc.arb.WithMachine(false, func(*client.Conn) error { return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr.Observe(machine.Idle)
	go svc.RunMachineLearning(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		learned := svc.UISettings().Machine.Learned
		if learned.Anchors.Available {
			key := "CarveraAir,FACTORY,PROBE | 1.2.3 | lz"
			profile, ok := svc.UISettings().Machine.LearnedProfiles[key]
			if !ok || !profile.Anchors.Available {
				t.Fatalf("persisted machine profile = %+v", svc.UISettings().Machine.LearnedProfiles)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("background learning did not persist anchor parameters: %+v", svc.UISettings().Machine.Learned)
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
	if active.Preview == nil || active.Preview.LineCount != 5 || active.Preview.MoveCount != 3 || active.Preview.PlottedSegments != 2 {
		t.Fatalf("preview = %+v", active.Preview)
	}
	if len(active.Preview.Tools) != 1 || active.Preview.Tools[0] != 2 {
		t.Fatalf("tools = %v, want [2]", active.Preview.Tools)
	}
	if active.Preview.Bounds == nil || active.Preview.Bounds.Max[0] != 10 || active.Preview.Bounds.Max[1] != 5 || active.Preview.Bounds.Min[2] != -1 {
		t.Fatalf("bounds = %+v", active.Preview.Bounds)
	}
}

func TestParseGcodePreviewSkipsLeadInRapidsFromUnknownStart(t *testing.T) {
	gcode := strings.Join([]string{
		"G21 G90",
		"G0 Z15",      // Z anchored; X/Y still unknown -> not plotted
		"G0 X40 Y-20", // X/Y anchored; start X/Y unknown -> not plotted
		"G0 Z1",       // fully anchored rapid -> plotted
		"G1 Z-1 F200", // cut -> plotted
		"G1 X50 Y-20", // cut -> plotted
	}, "\n")
	preview, err := ParseGcodePreview(strings.NewReader(gcode))
	if err != nil {
		t.Fatalf("ParseGcodePreview: %v", err)
	}
	if preview.MoveCount != 5 || preview.PlottedSegments != 3 {
		t.Fatalf("moves=%d plotted=%d, want 5 moves with 3 plotted", preview.MoveCount, preview.PlottedSegments)
	}
	first := preview.Segments[0]
	if first.Kind != "rapid" || first.From != [4]float64{40, -20, 15, 0} || first.To != [4]float64{40, -20, 1, 0} {
		t.Fatalf("first plotted segment = %+v, want anchored Z rapid", first)
	}
	b := preview.Bounds
	if b == nil || b.Min[0] != 40 || b.Max[0] != 50 || b.Min[1] != -20 || b.Max[1] != -20 || b.Min[2] != -1 || b.Max[2] != 15 {
		t.Fatalf("bounds = %+v, want bounds excluding the assumed origin", b)
	}

	// Cut moves from an unanchored start keep the historical assumed-origin
	// behavior so files that never position all axes still render.
	preview, err = ParseGcodePreview(strings.NewReader("G21 G90\nG1 X10 F100\nG1 Y10\n"))
	if err != nil {
		t.Fatalf("ParseGcodePreview: %v", err)
	}
	if preview.PlottedSegments != 2 || preview.Segments[0].From != [4]float64{} {
		t.Fatalf("cut-only preview = %+v, want cuts plotted from assumed origin", preview)
	}
}

func TestActiveGcodePreviewIsBounded(t *testing.T) {
	var gcode strings.Builder
	for i := 0; i < maxPreviewSegments+100; i++ {
		fmt.Fprintf(&gcode, "G1 X%d\n", i&1)
	}
	preview, err := ParseGcodePreview(strings.NewReader(gcode.String()))
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Truncated || len(preview.Segments) != maxPreviewSegments {
		t.Fatalf("preview truncated=%v segments=%d, want true/%d", preview.Truncated, len(preview.Segments), maxPreviewSegments)
	}
}

func TestActiveGcodeSelectionPersistsAcrossServiceRestart(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	st, err := store.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	arb := session.New(session.Config{
		Dial: func() (*client.Conn, error) { return nil, io.EOF },
	})
	svc, err := New(st, arb)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("G90\nG0 X0 Y0 Z2\nG1 X3 Y4 Z-1\n")
	putCachedEntry(t, svc, "restart.nc", content, store.Synced)

	selected, err := svc.SelectActiveGcode("restart.nc")
	if err != nil {
		t.Fatalf("SelectActiveGcode: %v", err)
	}
	if got := st.ActiveGcodePath(); got != selected.Path {
		t.Fatalf("stored active gcode path = %q, want %q", got, selected.Path)
	}

	reopened, err := store.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(reopened, arb)
	if err != nil {
		t.Fatal(err)
	}
	active := restarted.ActiveGcode()
	if active.Path != selected.Path || !active.Runnable {
		t.Fatalf("restored active = %+v, want runnable %s", active, selected.Path)
	}
	if active.Preview == nil || active.Preview.MoveCount != 2 {
		t.Fatalf("restored preview = %+v", active.Preview)
	}
}

func TestParseMachineProgressGcodePath(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string
		ok   bool
	}{
		{
			name: "running file",
			out:  "file: /sd/gcodes/my part.nc, 42 % complete, elapsed time: 00:01:02",
			want: "/sd/gcodes/my part.nc",
			ok:   true,
		},
		{
			name: "escaped filename",
			out:  "file: /sd/gcodes/my\x01part.nc, 1 % complete, elapsed time: 00:00:01",
			want: "/sd/gcodes/my part.nc",
			ok:   true,
		},
		{
			name: "not playing",
			out:  "Not currently playing",
			ok:   false,
		},
		{
			name: "outside gcode root",
			out:  "file: /sd/config.txt, 50 % complete, elapsed time: 00:00:10",
			ok:   false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseMachineProgressGcodePath(c.out)
			if ok != c.ok || got != c.want {
				t.Fatalf("parseMachineProgressGcodePath(%q) = %q, %v; want %q, %v", c.out, got, ok, c.want, c.ok)
			}
		})
	}
}

func TestActiveGcodeLoadsRunningFileFromFirmwareProgress(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	const runningPath = "/sd/gcodes/running part.nc"
	m.SetGcodeReply("progress", "file: "+runningPath+", 42 % complete, elapsed time: 00:01:02")

	if !tr.ObserveStatusPayload("<Run|MPos:0,0,0|WPos:0,0,0|P:1,42,62>") {
		t.Fatal("status payload was not accepted")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := svc.store.ActiveGcodePath(); got == runningPath {
			active := svc.ActiveGcode()
			if active.Path != runningPath || active.Entry == nil || active.Entry.Sync != store.RemoteOnly || !active.Runnable {
				t.Fatalf("active = %+v, want remote-only running path", active)
			}
			if active.Entry.CachePath != "" || active.Preview != nil {
				t.Fatalf("active unexpectedly loaded cache/preview: %+v", active)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("active gcode path = %q, want %q", svc.store.ActiveGcodePath(), runningPath)
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
	m.PutFile("/sd/gcodes/my part.nc", []byte("G1 X1\n"))
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
	if res.Verified || !strings.Contains(res.Message, "machine confirmation was not available") {
		t.Fatalf("result = %+v, want unverified neutral message", res)
	}
	if g := m.Gcodes(); len(g) != 1 || g[0] != "play /sd/gcodes/my part.nc" {
		t.Fatalf("machine gcodes = %v, want play command", g)
	}
	lines := svc.GcodeLog().Recent()
	if got := lines[len(lines)-1].Text; got != "sent: no reply observed" {
		t.Fatalf("last gcode log line = %q, want neutral sent line", got)
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
	} else if res.Verified || !strings.Contains(res.Message, "machine confirmation was not available") {
		t.Fatalf("SetCurrentToolID result=%+v, want unverified neutral message", res)
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
	if res, err := svc.ContinueToolChange(); err != nil || res.Command != "M490.2" {
		t.Fatalf("ContinueToolChange result=%+v err=%v", res, err)
	}
	if res, err := svc.DropCurrentTool(); err != nil || res.Command != "M6T-1" {
		t.Fatalf("DropCurrentTool result=%+v err=%v", res, err)
	}
	if res, err := svc.CalibrateCurrentTool(); err != nil || res.Command != "M491" {
		t.Fatalf("CalibrateCurrentTool result=%+v err=%v", res, err)
	}
	if g := m.Gcodes(); len(g) != 8 ||
		g[0] != "M493.2T3" ||
		g[1] != "M493.2T0" ||
		g[2] != "M493.2T-1" ||
		g[3] != "M493.2T8888" ||
		g[4] != "M6T4" ||
		g[5] != "M490.2" ||
		g[6] != "M6T-1" ||
		g[7] != "M491" {
		t.Fatalf("machine gcodes = %v, want vendor tool commands", g)
	}
}

func TestContinueToolChangeRunsWhileAwaitingToolAndRefreshesTLO(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	if !tr.ObserveStatusPayload(m.Snapshot().Status.Raw) {
		t.Fatal("failed to seed tracker status")
	}

	if res, err := svc.ChangeTool(2); err != nil || res.Command != "M6T2" {
		t.Fatalf("ChangeTool result=%+v err=%v", res, err)
	}
	st, _ := tr.Current()
	if st.State != machine.Tool || st.Tool == nil || st.Tool.Target == nil || *st.Tool.Target != 2 {
		t.Fatalf("tracker after change = %+v, want Tool target 2", st)
	}
	if _, err := m.InsertTool("tool_6"); err != nil {
		t.Fatal(err)
	}
	if snap := m.Snapshot(); snap.Status.State != machine.Tool || snap.Status.Tool == nil || snap.Status.Tool.Target == nil || *snap.Status.Tool.Target != 2 {
		t.Fatalf("fake status after physical insert = %+v, want target preserved", snap.Status.Tool)
	}

	if res, err := svc.ContinueToolChange(); err != nil || res.Command != "M490.2" {
		t.Fatalf("ContinueToolChange result=%+v err=%v", res, err)
	}
	st, _ = tr.Current()
	if st.State != machine.Idle || st.Tool == nil || st.Tool.Active != 2 || st.Tool.Target != nil || math.Abs(st.Tool.Offset) < 0.001 {
		t.Fatalf("tracker after continue = %+v, want Idle active tool 2 with non-zero TLO", st)
	}
	if snap := m.Snapshot(); snap.InsertedTool == nil || !snap.InsertedTool.Calibrated || math.Abs(snap.InsertedTool.CalibratedOffsetMM-st.Tool.Offset) > 0.001 {
		t.Fatalf("inserted tool after continue = %+v, want calibrated to tracker TLO %.3f", snap.InsertedTool, st.Tool.Offset)
	}
}

func TestContinueToolChangeRejectsNonToolState(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	status := "<Run|MPos:0,0,0|WPos:0,0,0|T:0,0>"
	m.SetStatus(status)
	if !tr.ObserveStatusPayload(status) {
		t.Fatal("failed to seed tracker status")
	}

	if _, err := svc.ContinueToolChange(); !errors.Is(err, ErrToolChangeUnavailable) {
		t.Fatalf("ContinueToolChange err = %v, want ErrToolChangeUnavailable", err)
	}
	if g := m.Gcodes(); len(g) != 0 {
		t.Fatalf("continue command reached machine outside Tool state: %v", g)
	}
}

func TestProbeZFieldRetractsAboveFirstContactWithoutCrossingSafeCeiling(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	status := "<Idle|MPos:0,0,-10|WPos:0,0,-10|T:0,0>"
	m.SetStatus(status)
	if !tr.ObserveStatusPayload(status) {
		t.Fatal("failed to seed tracker status")
	}
	m.SetGcodeReply("G38.2 Z-20.0000 F50.0000", "[PRB:10.0000,-5.0000,-10.0000:1]")
	clearance := 5.0

	res, err := svc.ProbeZ(ProbeZRequest{
		MachineX:       10,
		MachineY:       -5,
		MoveXY:         true,
		SafeZMM:        -1,
		RetractAboveMM: &clearance,
		ProbeDepthMM:   20,
		ProbeFeedMM:    50,
	})
	if err != nil {
		t.Fatalf("ProbeZ: %v", err)
	}
	if res.RetractZMM != -5 {
		t.Fatalf("retract Z = %v, want -5", res.RetractZMM)
	}
	if st, _ := tr.Current(); st.State != machine.Idle {
		t.Fatalf("probe returned before retract completed: %s", st.State)
	}
	want := []string{
		"G53 G0 Z-3.0000",
		"G53 G0 X10.0000 Y-5.0000",
		"G38.2 Z-20.0000 F50.0000",
		"G53 G0 Z-5.0000",
	}
	if got := m.Gcodes(); !stringSlicesEqual(got, want) {
		t.Fatalf("probe gcodes = %v, want %v", got, want)
	}
}

func TestProbeFloorUsesConfiguredSafeZ(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	ui := svc.UISettings()
	ui.Machine.SafeZMM = -6
	if _, err := svc.SetUISettings(ui); err != nil {
		t.Fatal(err)
	}
	status := "<Idle|MPos:20,30,-10|WPos:12.5,-3.25,1|T:0,0>"
	m.SetStatus(status)
	if !tr.ObserveStatusPayload(status) {
		t.Fatal("failed to seed tracker status")
	}
	m.SetGcodeReply("G38.2 Z-20.0000 F50.0000", "[PRB:20.0000,30.0000,-14.0000:1]")

	res, err := svc.ProbeFloor()
	if err != nil {
		t.Fatalf("ProbeFloor: %v", err)
	}
	if !res.Verified || res.Machine["z"] != -14 {
		t.Fatalf("floor probe result = %+v", res)
	}
	want := []string{"G53 G0 Z-10.0000", "G38.2 Z-20.0000 F50.0000", "G10 L20 P0 Z0", "G53 G0 Z-6.0000"}
	if got := m.Gcodes(); !stringSlicesEqual(got, want) {
		t.Fatalf("floor probe gcodes = %v, want %v", got, want)
	}
}

func TestSafeZTargetUsesFirmwareClearanceAsTheSharedCeiling(t *testing.T) {
	svc, _, _ := serviceWithMachine(t)
	ui := svc.UISettings()
	ui.Machine.SafeZMM = 0
	ui.Machine.Learned = store.MachineLearned{
		ZMinMM: -121,
		ZMaxMM: 0,
		ConfigNumbers: map[string]float64{
			"coordinate.clearance_z": -5,
		},
	}
	ui, err := svc.SetUISettings(ui)
	if err != nil {
		t.Fatal(err)
	}
	if ui.Machine.SafeZMM != -5 {
		t.Fatalf("persisted safe Z = %v, want firmware clearance -5", ui.Machine.SafeZMM)
	}
	if got := svc.SafeZTargetMM(-1); got != -5 {
		t.Fatalf("effective safe Z = %v, want firmware clearance -5", got)
	}
}

func TestSetUISettingsKeepsNewerLearnedAnchorsFromStaleBrowserSave(t *testing.T) {
	svc, _ := newService(t)
	now := time.Now().UTC()
	ui := svc.UISettings()
	ui.Machine.Learned = store.MachineLearned{
		LearnedAt: now,
		Identity:  store.MachineIdentity{Model: "CarveraAir", Version: "1.2.3", FileType: "lz"},
		Anchors: store.MachineAnchorProfile{
			Available: true,
			Anchor1:   store.XYPoint{X: -287.51, Y: -202.11},
			Anchor2:   store.XYPoint{X: -199.01, Y: -157.11},
		},
	}
	ui.Machine.LearnedProfiles = map[string]store.MachineLearned{
		"CarveraAir | 1.2.3 | lz": ui.Machine.Learned,
	}
	if _, err := svc.SetUISettings(ui); err != nil {
		t.Fatal(err)
	}

	stale := svc.UISettings()
	stale.Machine.Learned = store.MachineLearned{}
	stale.Machine.LearnedProfiles = nil
	stale.Machine.TapFeedMMMin = 700
	got, err := svc.SetUISettings(stale)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Machine.Learned.Anchors.Available || got.Machine.Learned.Anchors.Anchor1 != (store.XYPoint{X: -287.51, Y: -202.11}) {
		t.Fatalf("stale settings save erased learned anchors: %+v", got.Machine.Learned.Anchors)
	}
	if profile := got.Machine.LearnedProfiles["CarveraAir | 1.2.3 | lz"]; !profile.Anchors.Available {
		t.Fatalf("stale settings save erased learned profile: %+v", got.Machine.LearnedProfiles)
	}
}

func TestTraceOutlineUsesVendorMarginLaserModeAndVerifies(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	status := "<Idle|MPos:-100,-50,-3|WPos:10,20,0|T:0,0>"
	m.SetStatus(status)
	if !tr.ObserveStatusPayload(status) {
		t.Fatal("failed to seed tracker status")
	}

	res, err := svc.TraceOutline(TraceOutlineRequest{
		MachinePoints: []TracePoint{{X: -100, Y: -50}, {X: -90, Y: -50}, {X: -90, Y: -45}},
		SafeZMM:       5,
		FeedMM:        600,
		Closed:        true,
	})
	if err != nil {
		t.Fatalf("TraceOutline: %v", err)
	}
	if res.Verified || res.Points != 4 || res.CommandCount != 7 {
		t.Fatalf("trace result = %+v", res)
	}
	want := []string{
		"M497.4",
		"M494.0",
		"G53 G0 Z-3.0000",
		"G90 G0 X10.0000 Y20.0000",
		"G90 G1 X20.0000 Y20.0000 F600.0000",
		"G90 G1 X20.0000 Y25.0000 F600.0000",
		"G90 G1 X10.0000 Y20.0000 F600.0000",
	}
	if got := m.Gcodes(); !stringSlicesEqual(got, want) {
		t.Fatalf("trace gcodes = %v, want %v", got, want)
	}
	if !m.Snapshot().ProbeLaserActive {
		t.Fatal("trace turned the probe laser off")
	}
}

func TestTraceOutlineRejectsInvalidStateAndInput(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	status := "<Idle|MPos:0,0,0|WPos:0,0,0|T:3,0>"
	m.SetStatus(status)
	if !tr.ObserveStatusPayload(status) {
		t.Fatal("failed to seed tracker status")
	}
	req := TraceOutlineRequest{
		MachinePoints: []TracePoint{{X: 0, Y: 0}, {X: 10, Y: 0}},
		SafeZMM:       5,
		FeedMM:        600,
	}
	if _, err := svc.TraceOutline(req); !errors.Is(err, ErrProbeUnavailable) {
		t.Fatalf("TraceOutline non-probe err = %v, want ErrProbeUnavailable", err)
	}
	if g := m.Gcodes(); len(g) != 0 {
		t.Fatalf("non-probe trace leaked to machine: %v", g)
	}

	status = "<Run|MPos:0,0,0|WPos:0,0,0|T:0,0>"
	m.SetStatus(status)
	if !tr.ObserveStatusPayload(status) {
		t.Fatal("failed to seed tracker status")
	}
	if _, err := svc.TraceOutline(req); !session.Retryable(err) {
		t.Fatalf("TraceOutline non-idle err = %v, want retryable", err)
	}

	if _, err := svc.TraceOutline(TraceOutlineRequest{MachinePoints: []TracePoint{{X: 0, Y: 0}}, FeedMM: 600}); err == nil {
		t.Fatal("expected too few trace points to be rejected")
	}
	if _, err := svc.TraceOutline(TraceOutlineRequest{
		MachinePoints: []TracePoint{{X: 0, Y: 0}, {X: math.NaN(), Y: 1}},
		SafeZMM:       5,
		FeedMM:        600,
	}); err == nil {
		t.Fatal("expected non-finite trace point to be rejected")
	}
	points := make([]TracePoint, maxTracePoints+1)
	for i := range points {
		points[i] = TracePoint{X: float64(i), Y: 0}
	}
	if _, err := svc.TraceOutline(TraceOutlineRequest{MachinePoints: points, SafeZMM: 5, FeedMM: 600}); err == nil {
		t.Fatal("expected excessive trace points to be rejected")
	}
}

func TestTraceOutlineKeepsProbeLaserEnabledAfterFailure(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	status := "<Idle|MPos:0,0,0|WPos:0,0,0|T:0,0>"
	m.SetStatus(status)
	if !tr.ObserveStatusPayload(status) {
		t.Fatal("failed to seed tracker status")
	}
	m.SetGcodeReply("G53 G0 Z-3.0000", "error: soft limit")

	_, err := svc.TraceOutline(TraceOutlineRequest{
		MachinePoints: []TracePoint{{X: 0, Y: 0}, {X: 10, Y: 0}},
		SafeZMM:       5,
		FeedMM:        600,
	})
	if err == nil {
		t.Fatal("expected trace move failure")
	}
	want := []string{
		"M497.4",
		"M494.0",
		"G53 G0 Z-3.0000",
	}
	if got := m.Gcodes(); !stringSlicesEqual(got, want) {
		t.Fatalf("trace failure gcodes = %v, want %v", got, want)
	}
	if !m.Snapshot().ProbeLaserActive {
		t.Fatal("failed trace turned the probe laser off")
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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

func TestRelayControllerDownloadUsesUploadedCache(t *testing.T) {
	m, err := carveratest.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)

	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	arb := session.New(session.Config{
		Dial: func() (*client.Conn, error) {
			return client.Dial(m.Addr(), 2*time.Second, client.WithUploadStartDelay(0))
		},
	})
	svc, err := New(st, arb)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("G0 X0\nG1 X12.5 Y4\n")
	if _, err := svc.Upload("web cached.nc", bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}

	relaySrv := &relay.Server{
		Dial:          func() (string, error) { return m.Addr(), nil },
		Observer:      arb,
		DownloadCache: svc,
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

	var got bytes.Buffer
	gotMD5, written, err := client.New(controller).Download("/sd/gcodes/web cached.nc", &got, 2*time.Second, nil)
	if err != nil {
		t.Fatalf("controller download: %v", err)
	}
	if !bytes.Equal(got.Bytes(), content) || written != int64(len(content)) {
		t.Fatalf("downloaded content = %q (%d bytes), want %q", got.Bytes(), written, content)
	}
	sum := md5.Sum(content)
	if gotMD5 != hex.EncodeToString(sum[:]) {
		t.Fatalf("download md5 = %q, want %q", gotMD5, hex.EncodeToString(sum[:]))
	}
	if _, ok := m.File("/sd/gcodes/web cached.nc"); ok {
		t.Fatal("fake machine unexpectedly has the web-uploaded file")
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
		Machine:      store.MachineUI{WorkArea: store.WorkArea{XMin: -300, XMax: 5, YMin: -210, YMax: 10}, SavedOrigins: []store.SavedOrigin{{ID: "fixture", Label: "Fixture", Origin: store.XYPoint{X: -10, Y: -20}}}, FeedMinMMMin: 100, FeedMaxMMMin: 1800, TapFeedMMMin: 700},
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
	if got := restored.UISettings(); len(got.Macros) != 1 || got.Macros[0].Name != "Position" || got.Gamepad.DeadmanButton != 2 || got.Machine.FeedMinMMMin != 100 || got.Machine.FeedMaxMMMin != 1800 || got.Machine.TapFeedMMMin != 700 || len(got.Machine.SavedOrigins) != 1 {
		t.Fatalf("restored UI = %+v", got)
	}
	if lines := restored.GcodeLog().Recent(); len(lines) != 1 || lines[0].Text != "M114" {
		t.Fatalf("restored log = %+v", lines)
	}
	if runs := restored.RunHistory(); len(runs) != 1 || runs[0].File != "part.nc" {
		t.Fatalf("restored runs = %+v", runs)
	}
}

func TestImportBackupRejectsUnsafeState(t *testing.T) {
	svc, _ := newService(t)
	for _, state := range []store.Snapshot{
		{Entries: map[string]store.Entry{"/etc/passwd": {Path: "/etc/passwd", Sync: store.RemoteOnly}}},
		{Jobs: []store.Job{{ID: 1, Kind: store.JobDelete, Path: "/sd/config", State: store.Queued}}},
		{Jobs: []store.Job{{ID: 1, Kind: store.JobRename, Path: "/sd/gcodes/a.nc", DestPath: "/sd/config", State: store.Queued}}},
		{Jobs: []store.Job{{ID: 1, Kind: "execute", Path: "/sd/gcodes/a.nc", State: store.Queued}}},
		{Entries: map[string]store.Entry{"/sd/gcodes/a.nc": {Path: "/sd/gcodes/a.nc", Sync: "impossible"}}},
	} {
		err := svc.ImportBackup(Backup{Version: 1, State: state})
		if !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("ImportBackup(%+v) = %v, want ErrInvalidArgument", state, err)
		}
	}
}

func TestImportBackupConvertsRunningJobsToFailed(t *testing.T) {
	svc, st := newService(t)
	backup := Backup{Version: 1, State: store.Snapshot{
		Jobs: []store.Job{{ID: 1, Kind: store.JobDelete, Path: "/sd/gcodes/a.nc", State: store.Running}},
	}}
	if err := svc.ImportBackup(backup); err != nil {
		t.Fatal(err)
	}
	job, ok := st.GetJob(1)
	if !ok || job.State != store.Failed || job.LastError == "" {
		t.Fatalf("restored running job = %+v ok=%v, want visible failed job", job, ok)
	}
}

func TestReadCacheRejectsPathOutsideCacheDirectory(t *testing.T) {
	svc, st := newService(t)
	if err := st.PutEntry(store.Entry{
		Path:       "/sd/gcodes/leak.nc",
		CachePath:  "/etc/hosts",
		CacheState: store.CacheReady,
		Sync:       store.Synced,
	}); err != nil {
		t.Fatal(err)
	}
	if rc, _, err := svc.ReadCache("leak.nc"); !errors.Is(err, ErrNotCached) {
		if rc != nil {
			rc.Close()
		}
		t.Fatalf("ReadCache outside cache = %v, want ErrNotCached", err)
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
