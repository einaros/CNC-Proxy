package synceng

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/uwin/cnc-proxy/internal/carveratest"
	"github.com/uwin/cnc-proxy/internal/client"
	"github.com/uwin/cnc-proxy/internal/machine"
	"github.com/uwin/cnc-proxy/internal/session"
	"github.com/uwin/cnc-proxy/internal/store"
)

func setup(t *testing.T) (*carveratest.FakeMachine, *store.Store, *session.Arbiter, *machine.Tracker) {
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
		Dial:    func() (*client.Conn, error) { return client.Dial(m.Addr(), 2*time.Second) },
	})
	return m, st, arb, tr
}

func newEngine(st *store.Store, arb *session.Arbiter) *Engine {
	return New(Config{Store: st, Arbiter: arb, OpTimeout: 3 * time.Second, BaseBackoff: time.Millisecond})
}

// writeCache creates a cache file with random content and returns path+md5+size.
func writeCache(t *testing.T, dir string, size int) (string, string, int64) {
	t.Helper()
	content := make([]byte, size)
	rand.Read(content)
	p := filepath.Join(dir, "cache.bin")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := md5.Sum(content)
	return p, hex.EncodeToString(sum[:]), int64(size)
}

func TestUploadJobSyncsWhenIdle(t *testing.T) {
	m, st, arb, tr := setup(t)
	eng := newEngine(st, arb)

	cachePath, md5hex, size := writeCache(t, t.TempDir(), 9000)
	remote := "/sd/gcodes/part.nc"
	st.PutEntry(store.Entry{Path: remote, Size: size, MD5: md5hex, CachePath: cachePath, Sync: store.PendingUpload})
	st.Enqueue(store.Job{Kind: store.JobUpload, Path: remote, CachePath: cachePath, MD5: md5hex, Size: size})

	// Machine busy → drain should leave the job queued.
	tr.Observe(machine.Run)
	eng.drain()
	if e, _ := st.GetEntry(remote); e.Sync == store.Synced {
		t.Fatal("should not have synced while machine is Run")
	}
	if j, ok := st.NextQueued(); !ok || j.Kind != store.JobUpload {
		t.Fatal("upload job should still be queued while busy")
	}

	// Machine idle → drain should upload and sync.
	tr.Observe(machine.Idle)
	eng.drain()

	e, _ := st.GetEntry(remote)
	if e.Sync != store.Synced {
		t.Fatalf("entry sync = %q, want synced", e.Sync)
	}
	got, ok := m.File(remote)
	if !ok || int64(len(got)) != size {
		t.Fatalf("machine file = %d bytes ok=%v, want %d", len(got), ok, size)
	}
	if _, ok := st.NextQueued(); ok {
		t.Error("queue should be empty after successful drain")
	}
}

func TestUploadCompressedWhenSupported(t *testing.T) {
	m, st, arb, tr := setup(t)
	eng := newEngine(st, arb)
	m.SetFtype("lz") // firmware advertises compression support
	tr.Observe(machine.Idle)

	// A large, highly compressible payload (> quicklz.BlockSize).
	content := bytes.Repeat([]byte("G1 X10 Y10 Z0.5 F1000\n"), 2000)
	dir := t.TempDir()
	p := filepath.Join(dir, "big.nc")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := md5.Sum(content)
	hexsum := hex.EncodeToString(sum[:])
	remote := "/sd/gcodes/big.nc"
	st.PutEntry(store.Entry{Path: remote, Size: int64(len(content)), MD5: hexsum, CachePath: p, Sync: store.PendingUpload})
	st.Enqueue(store.Job{Kind: store.JobUpload, Path: remote, CachePath: p, MD5: hexsum, Size: int64(len(content))})

	eng.drain()

	// The machine should hold the file under its ORIGINAL name (the .lz suffix
	// is stripped) with the DECOMPRESSED content intact.
	got, ok := m.File(remote)
	if !ok {
		t.Fatal("machine missing decompressed file under original name")
	}
	if !bytes.Equal(got, content) {
		t.Errorf("decompressed content mismatch: got %d bytes, want %d", len(got), len(content))
	}
	if _, hasLz := m.File(remote + ".lz"); hasLz {
		t.Error("machine should not retain the .lz name after decompression")
	}
	if e, _ := st.GetEntry(remote); e.Sync != store.Synced {
		t.Errorf("entry sync = %q, want synced", e.Sync)
	}
}

func TestUploadUncompressedWhenUnsupported(t *testing.T) {
	m, st, arb, tr := setup(t)
	eng := newEngine(st, arb)
	m.SetFtype("nc") // no compression support
	tr.Observe(machine.Idle)

	content := bytes.Repeat([]byte("data\n"), 2000)
	dir := t.TempDir()
	p := filepath.Join(dir, "plain.nc")
	os.WriteFile(p, content, 0o644)
	sum := md5.Sum(content)
	remote := "/sd/gcodes/plain.nc"
	st.PutEntry(store.Entry{Path: remote, Size: int64(len(content)), MD5: hex.EncodeToString(sum[:]), CachePath: p, Sync: store.PendingUpload})
	st.Enqueue(store.Job{Kind: store.JobUpload, Path: remote, CachePath: p, MD5: hex.EncodeToString(sum[:]), Size: int64(len(content))})

	eng.drain()

	got, ok := m.File(remote)
	if !ok || !bytes.Equal(got, content) {
		t.Errorf("uncompressed upload mismatch: ok=%v len=%d want=%d", ok, len(got), len(content))
	}
}

func TestBlockedWhileRelayActive(t *testing.T) {
	_, st, arb, tr := setup(t)
	eng := newEngine(st, arb)
	tr.Observe(machine.Idle)
	arb.EnterRelay() // controller connected

	cachePath, md5hex, size := writeCache(t, t.TempDir(), 100)
	remote := "/sd/gcodes/x.nc"
	st.PutEntry(store.Entry{Path: remote, Size: size, MD5: md5hex, CachePath: cachePath, Sync: store.PendingUpload})
	st.Enqueue(store.Job{Kind: store.JobUpload, Path: remote, CachePath: cachePath, MD5: md5hex, Size: size})

	eng.drain()
	if _, ok := st.NextQueued(); !ok {
		t.Error("job should remain queued while relay is active")
	}

	arb.ExitRelay()
	eng.drain()
	if e, _ := st.GetEntry(remote); e.Sync != store.Synced {
		t.Errorf("after exit relay, sync = %q, want synced", e.Sync)
	}
}

func TestDeleteJob(t *testing.T) {
	m, st, arb, tr := setup(t)
	eng := newEngine(st, arb)
	tr.Observe(machine.Idle)

	// Seed a file on the machine via an upload job first.
	cachePath, md5hex, size := writeCache(t, t.TempDir(), 50)
	remote := "/sd/gcodes/del.nc"
	st.PutEntry(store.Entry{Path: remote, Size: size, MD5: md5hex, CachePath: cachePath, Sync: store.PendingUpload})
	st.Enqueue(store.Job{Kind: store.JobUpload, Path: remote, CachePath: cachePath, MD5: md5hex, Size: size})
	eng.drain()
	if _, ok := m.File(remote); !ok {
		t.Fatal("precondition: file should exist on machine")
	}

	// Now delete it.
	st.SetEntrySync(remote, store.PendingDelete, "")
	st.Enqueue(store.Job{Kind: store.JobDelete, Path: remote})
	eng.drain()

	if _, ok := m.File(remote); ok {
		t.Error("file should be gone from machine")
	}
	if _, ok := st.GetEntry(remote); ok {
		t.Error("catalog entry should be removed after delete")
	}
}

func TestFailureRecordedAndRetried(t *testing.T) {
	m, st, arb, tr := setup(t)
	eng := newEngine(st, arb)
	eng.maxAttempts = 3
	eng.baseBackoff = time.Millisecond // short, but we wait past it between drains
	tr.Observe(machine.Idle)

	m.FailCommand("mkdir")
	st.Enqueue(store.Job{Kind: store.JobMkdir, Path: "/sd/gcodes/newdir"})

	// Each drain pass must attempt the failing job AT MOST once (no busy-retry
	// within a single pass). With backoff elapsed between passes, attempts climb
	// by exactly one per drain until maxAttempts marks the job Failed.
	for i := 1; i <= 3; i++ {
		eng.drain()
		jobs := st.ListJobs()
		if len(jobs) != 1 {
			t.Fatalf("pass %d: job count = %d", i, len(jobs))
		}
		if jobs[0].Attempts != i {
			t.Fatalf("pass %d: attempts = %d, want %d (one attempt per drain)", i, jobs[0].Attempts, i)
		}
		if jobs[0].LastError == "" {
			t.Errorf("pass %d: expected LastError set", i)
		}
		time.Sleep(5 * time.Millisecond) // let backoff elapse before next pass
	}
	if final := st.ListJobs()[0]; final.State != store.Failed {
		t.Errorf("final state = %q, want failed after %d attempts", final.State, eng.maxAttempts)
	}
}

// TestFailingJobDoesNotBlockOthers ensures one persistently-failing job (e.g. a
// delete of a file that doesn't exist on the machine) doesn't stall unrelated
// jobs behind it in the queue — the head-of-line problem seen on a real mount
// when macOS wrote AppleDouble metadata.
func TestFailingJobDoesNotBlockOthers(t *testing.T) {
	m, st, arb, tr := setup(t)
	eng := newEngine(st, arb)
	tr.Observe(machine.Idle)

	// A delete that the machine always fails (file not present).
	m.FailCommand("rm")
	st.Enqueue(store.Job{Kind: store.JobDelete, Path: "/sd/gcodes/ghost.nc"})

	// A legitimate upload queued AFTER the failing delete, different path.
	cachePath, md5hex, size := writeCache(t, t.TempDir(), 100)
	remote := "/sd/gcodes/real.nc"
	st.PutEntry(store.Entry{Path: remote, Size: size, MD5: md5hex, CachePath: cachePath, Sync: store.PendingUpload})
	st.Enqueue(store.Job{Kind: store.JobUpload, Path: remote, CachePath: cachePath, MD5: md5hex, Size: size})

	eng.drain()

	// The upload must have synced despite the failing delete ahead of it.
	if e, _ := st.GetEntry(remote); e.Sync != store.Synced {
		t.Errorf("real.nc sync = %q, want synced (failing delete blocked the queue)", e.Sync)
	}
	if _, ok := m.File(remote); !ok {
		t.Error("real.nc never reached the machine")
	}
}

func TestEmptyFileUploadSyncs(t *testing.T) {
	m, st, arb, tr := setup(t)
	eng := newEngine(st, arb)
	tr.Observe(machine.Idle)

	dir := t.TempDir()
	p := filepath.Join(dir, "empty.bin")
	os.WriteFile(p, nil, 0o644)
	sum := md5.Sum(nil)
	remote := "/sd/gcodes/empty.nc"
	st.PutEntry(store.Entry{Path: remote, Size: 0, MD5: hex.EncodeToString(sum[:]), CachePath: p, Sync: store.PendingUpload})
	st.Enqueue(store.Job{Kind: store.JobUpload, Path: remote, CachePath: p, MD5: hex.EncodeToString(sum[:]), Size: 0})

	eng.drain()
	if e, _ := st.GetEntry(remote); e.Sync != store.Synced {
		t.Errorf("empty upload sync = %q, want synced", e.Sync)
	}
	if b, ok := m.File(remote); !ok || len(b) != 0 {
		t.Errorf("machine empty file = %v ok=%v", b, ok)
	}
}
