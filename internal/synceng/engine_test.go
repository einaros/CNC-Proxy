package synceng

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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
	"github.com/uwin/cnc-proxy/internal/service"
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
		Dial: func() (*client.Conn, error) {
			return client.Dial(m.Addr(), 2*time.Second, client.WithUploadStartDelay(0))
		},
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

func readScriptFrame(c net.Conn, scan *protocol.Scanner) (protocol.Frame, error) {
	buf := make([]byte, 1024)
	for {
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := c.Read(buf)
		if n > 0 {
			frames := scan.Push(buf[:n])
			if len(frames) > 0 {
				return frames[0], nil
			}
		}
		if err != nil {
			return protocol.Frame{}, err
		}
	}
}

func forceSyncStoreFlushFailure(t *testing.T, statePath string) {
	t.Helper()
	if err := os.Remove(statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestUploadJobSyncsWhenIdle(t *testing.T) {
	m, st, arb, tr := setup(t)
	eng := newEngine(st, arb)

	cachePath, md5hex, size := writeCache(t, t.TempDir(), 9000)
	remote := "/sd/gcodes/part.nc"
	st.PutEntry(store.Entry{Path: remote, Size: size, MD5: md5hex, CachePath: cachePath, Sync: store.PendingUpload})
	st.Enqueue(store.Job{Kind: store.JobUpload, Path: remote, CachePath: cachePath, MD5: md5hex, Size: size})

	// Machine busy -> drain should refresh status, see Run, and leave the job queued.
	m.SetStatus("<Run|MPos:0,0,0|WPos:0,0,0>")
	tr.Observe(machine.Run)
	eng.drain()
	if e, _ := st.GetEntry(remote); e.Sync == store.Synced {
		t.Fatal("should not have synced while machine is Run")
	}
	if j, ok := st.NextQueued(); !ok || j.Kind != store.JobUpload {
		t.Fatal("upload job should still be queued while busy")
	}

	// Machine idle -> the next pass should refresh status again, upload, and sync.
	m.SetStatus("<Idle|MPos:0,0,0|WPos:0,0,0>")
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

func TestUploadJobRefreshesStaleStatusBeforeSyncing(t *testing.T) {
	m, err := carveratest.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	tr := machine.NewTracker()
	arb := session.New(session.Config{
		Tracker:      tr,
		PollInterval: 10 * time.Millisecond,
		Dial: func() (*client.Conn, error) {
			return client.Dial(m.Addr(), 2*time.Second, client.WithFilePacketSize(client.USBPacketSize), client.WithUploadStartDelay(0))
		},
		FilePacketSize:            client.USBPacketSize,
		PreserveConnOnPollTimeout: true,
	})
	eng := newEngine(st, arb)

	cachePath, md5hex, size := writeCache(t, t.TempDir(), client.USBPacketSize*2+17)
	remote := "/sd/gcodes/usb.nc"
	st.PutEntry(store.Entry{Path: remote, Size: size, MD5: md5hex, CachePath: cachePath, Sync: store.PendingUpload})
	st.Enqueue(store.Job{Kind: store.JobUpload, Path: remote, CachePath: cachePath, MD5: md5hex, Size: size})

	eng.drain()
	e, _ := st.GetEntry(remote)
	if e.Sync != store.Synced {
		t.Fatalf("entry sync = %q, want synced", e.Sync)
	}
	if !tr.Fresh(time.Second) {
		t.Fatal("upload pass should refresh stale machine status itself")
	}
	got, ok := m.File(remote)
	if !ok || int64(len(got)) != size {
		t.Fatalf("machine file = %d bytes ok=%v, want %d", len(got), ok, size)
	}
	sizes := m.UploadPacketSizes()
	if len(sizes) != 1 || sizes[0] != client.USBPacketSize {
		t.Fatalf("upload packet sizes = %v, want [%d]", sizes, client.USBPacketSize)
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

func TestDeleteJobDoesNotRemoveReplacementUploadEntry(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	eng := New(Config{Store: st, OpTimeout: time.Second})
	remote := "/sd/gcodes/race.nc"
	cachePath := filepath.Join(t.TempDir(), "cache.bin")
	oldContent := []byte("old\n")
	if err := os.WriteFile(cachePath, oldContent, 0o644); err != nil {
		t.Fatal(err)
	}
	oldSum := md5.Sum(oldContent)
	if err := st.PutEntry(store.Entry{
		Path:      remote,
		Size:      int64(len(oldContent)),
		MD5:       hex.EncodeToString(oldSum[:]),
		CachePath: cachePath,
		Sync:      store.PendingDelete,
	}); err != nil {
		t.Fatal(err)
	}

	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	conn := client.New(clientSide)
	done := make(chan error, 1)
	go func() {
		defer clientSide.Close()
		done <- eng.execute(conn, store.Job{Kind: store.JobDelete, Path: remote})
	}()

	var scan protocol.Scanner
	frame, err := readScriptFrame(serverSide, &scan)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Cmd != protocol.CmdCtrlMulti {
		t.Fatalf("frame command = 0x%x, want CTRL_MULTI", frame.Cmd)
	}
	if got, want := protocol.Unescape(string(frame.Data)), "rm "+remote+" -e\n"; got != want {
		t.Fatalf("command = %q, want %q", got, want)
	}

	newContent := []byte("new content\n")
	if err := os.WriteFile(cachePath, newContent, 0o644); err != nil {
		t.Fatal(err)
	}
	newSum := md5.Sum(newContent)
	newMD5 := hex.EncodeToString(newSum[:])
	if err := st.PutEntry(store.Entry{
		Path:      remote,
		Size:      int64(len(newContent)),
		MD5:       newMD5,
		CachePath: cachePath,
		Sync:      store.PendingUpload,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Enqueue(store.Job{Kind: store.JobUpload, Path: remote, CachePath: cachePath, MD5: newMD5, Size: int64(len(newContent))}); err != nil {
		t.Fatal(err)
	}
	if _, err := serverSide.Write(protocol.Encode(protocol.CmdLoadFinish, []byte("ok\r\n"))); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execute delete: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("delete execute did not finish")
	}

	entry, ok := st.GetEntry(remote)
	if !ok {
		t.Fatal("replacement entry was removed by stale delete completion")
	}
	if entry.Sync != store.PendingUpload || entry.CachePath != cachePath || entry.MD5 != newMD5 || entry.Size != int64(len(newContent)) {
		t.Fatalf("entry after stale delete completion = %+v, want replacement upload", entry)
	}
	got, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, newContent) {
		t.Fatalf("cache = %q, want %q", string(got), string(newContent))
	}
	if queued, ok := st.NextQueued(); !ok || queued.Kind != store.JobUpload || queued.Path != remote {
		t.Fatalf("next queued job = %+v ok=%v, want replacement upload", queued, ok)
	}
}

func TestMachineCompletedStoreFailureDoesNotLookSuccessful(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	st, err := store.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	eng := New(Config{Store: st, OpTimeout: time.Second})
	remote := "/sd/gcodes/done-but-unrecorded.nc"
	if err := st.PutEntry(store.Entry{Path: remote, Sync: store.PendingDelete}); err != nil {
		t.Fatal(err)
	}
	job, err := st.Enqueue(store.Job{Kind: store.JobDelete, Path: remote})
	if err != nil {
		t.Fatal(err)
	}

	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	conn := client.New(clientSide)
	done := make(chan error, 1)
	go func() {
		defer clientSide.Close()
		done <- eng.execute(conn, job)
	}()

	var scan protocol.Scanner
	frame, err := readScriptFrame(serverSide, &scan)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Cmd != protocol.CmdCtrlMulti {
		t.Fatalf("frame command = 0x%x, want CTRL_MULTI", frame.Cmd)
	}
	if got, want := protocol.Unescape(string(frame.Data)), "rm "+remote+" -e\n"; got != want {
		t.Fatalf("command = %q, want %q", got, want)
	}

	forceSyncStoreFlushFailure(t, statePath)
	if _, err := serverSide.Write(protocol.Encode(protocol.CmdLoadFinish, []byte("ok\r\n"))); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		var completed machineCompletedError
		if !errors.As(err, &completed) {
			t.Fatalf("execute err = %v, want machineCompletedError", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("delete execute did not finish")
	}
	if entry, ok := st.GetEntry(remote); !ok || entry.Sync != store.Deleting {
		t.Fatalf("entry after failed completion record = %+v ok=%v, want deleting", entry, ok)
	}
	if job := st.ListJobs()[0]; job.State == store.Done {
		t.Fatalf("job after failed completion record = %+v, must not be done", job)
	}
}

func TestRecordMachineCompletedFailureMarksJobFailed(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	remote := "/sd/gcodes/unrecorded.nc"
	if err := st.PutEntry(store.Entry{Path: remote, Sync: store.Deleting}); err != nil {
		t.Fatal(err)
	}
	job, err := st.Enqueue(store.Job{Kind: store.JobDelete, Path: remote})
	if err != nil {
		t.Fatal(err)
	}
	eng := New(Config{Store: st})
	if err := eng.recordMachineCompletedFailure(job, errors.New("flush failed")); err != nil {
		t.Fatal(err)
	}
	got := st.ListJobs()[0]
	if got.State != store.Failed || !strings.Contains(got.LastError, "machine operation completed") {
		t.Fatalf("job after completed-machine failure = %+v", got)
	}
	entry, _ := st.GetEntry(remote)
	if entry.Sync != store.Error || !strings.Contains(entry.Error, "machine operation completed") {
		t.Fatalf("entry after completed-machine failure = %+v", entry)
	}
}

func TestUploadJobSyncUpdateRequiresCurrentContent(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	eng := New(Config{Store: st, OpTimeout: time.Second})
	remote := "/sd/gcodes/overwrite.nc"
	cachePath := filepath.Join(t.TempDir(), "cache.bin")
	oldContent := []byte("old\n")
	oldSum := md5.Sum(oldContent)
	oldMD5 := hex.EncodeToString(oldSum[:])
	job := store.Job{Kind: store.JobUpload, Path: remote, CachePath: cachePath, MD5: oldMD5, Size: int64(len(oldContent))}

	newContent := []byte("new content\n")
	if err := os.WriteFile(cachePath, newContent, 0o644); err != nil {
		t.Fatal(err)
	}
	newSum := md5.Sum(newContent)
	newMD5 := hex.EncodeToString(newSum[:])
	if err := st.PutEntry(store.Entry{
		Path:      remote,
		Size:      int64(len(newContent)),
		MD5:       newMD5,
		CachePath: cachePath,
		Sync:      store.PendingUpload,
	}); err != nil {
		t.Fatal(err)
	}

	ok, err := eng.setUploadJobSync(job, store.Synced, "")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("stale upload job updated replacement entry")
	}
	entry, _ := st.GetEntry(remote)
	if entry.Sync != store.PendingUpload || entry.MD5 != newMD5 || entry.Size != int64(len(newContent)) {
		t.Fatalf("entry = %+v, want replacement pending upload", entry)
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

func TestTransientUploadFailureKeepsEntryPendingUntilAttemptsExhausted(t *testing.T) {
	_, st, arb, tr := setup(t)
	eng := newEngine(st, arb)
	eng.maxAttempts = 2
	eng.baseBackoff = time.Millisecond
	tr.Observe(machine.Idle)

	remote := "/sd/gcodes/missing-cache.nc"
	missingCachePath := filepath.Join(t.TempDir(), "missing-cache.bin")
	st.PutEntry(store.Entry{Path: remote, Size: 123, MD5: "bad-md5", CachePath: missingCachePath, Sync: store.PendingUpload})
	st.Enqueue(store.Job{Kind: store.JobUpload, Path: remote, CachePath: missingCachePath, MD5: "bad-md5", Size: 123})

	eng.drain()
	job := st.ListJobs()[0]
	if job.State != store.Queued {
		t.Fatalf("after first failure job state = %q, want queued", job.State)
	}
	if job.Attempts != 1 || job.LastError == "" {
		t.Fatalf("after first failure job = %+v, want one failed attempt with error", job)
	}
	entry, _ := st.GetEntry(remote)
	if entry.Sync != store.PendingUpload {
		t.Fatalf("after first failure entry sync = %q, want pending_upload", entry.Sync)
	}
	if entry.Error == "" {
		t.Fatal("after first failure entry should retain the last error text")
	}

	time.Sleep(5 * time.Millisecond)
	eng.drain()
	job = st.ListJobs()[0]
	if job.State != store.Failed {
		t.Fatalf("after final failure job state = %q, want failed", job.State)
	}
	entry, _ = st.GetEntry(remote)
	if entry.Sync != store.Error {
		t.Fatalf("after final failure entry sync = %q, want error", entry.Sync)
	}
}

func TestPostUploadMD5TimeoutDoesNotHoldQueueForOperationTimeout(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()
	defer serverSide.Close()

	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	tr := machine.NewTracker()
	tr.Observe(machine.Idle)
	arb := session.New(session.Config{
		Tracker: tr,
		Dial: func() (*client.Conn, error) {
			return client.New(clientSide, client.WithUploadStartDelay(0)), nil
		},
	})
	compress := false
	eng := New(Config{
		Store:                  st,
		Arbiter:                arb,
		OpTimeout:              time.Second,
		PostUploadCheckTimeout: 25 * time.Millisecond,
		Compress:               &compress,
	})

	cachePath, md5hex, size := writeCache(t, t.TempDir(), 5*1024)
	remote := "/sd/gcodes/slow-md5.nc"
	st.PutEntry(store.Entry{Path: remote, Size: size, MD5: md5hex, CachePath: cachePath, Sync: store.PendingUpload})
	st.Enqueue(store.Job{Kind: store.JobUpload, Path: remote, CachePath: cachePath, MD5: md5hex, Size: size})

	done := make(chan struct{})
	serverErr := make(chan error, 1)
	go func() {
		defer serverSide.Close()
		var scan protocol.Scanner
		expect := func(cmd byte) (protocol.Frame, error) {
			f, err := readScriptFrame(serverSide, &scan)
			if err != nil {
				return protocol.Frame{}, err
			}
			if f.Cmd != cmd {
				return protocol.Frame{}, fmt.Errorf("frame = %s, want %s", protocol.CmdName(f.Cmd), protocol.CmdName(cmd))
			}
			return f, nil
		}
		if _, err := expect(protocol.CmdFileStart); err != nil {
			serverErr <- err
			return
		}
		if _, err := expect(protocol.CmdFileMD5); err != nil {
			serverErr <- err
			return
		}
		if _, err := serverSide.Write(protocol.Encode(protocol.CmdFileView, nil)); err != nil {
			serverErr <- err
			return
		}
		if _, err := expect(protocol.CmdFileView); err != nil {
			serverErr <- err
			return
		}
		if _, err := serverSide.Write(protocol.Encode(protocol.CmdFileData, []byte{0, 0, 0, 1})); err != nil {
			serverErr <- err
			return
		}
		if _, err := expect(protocol.CmdFileData); err != nil {
			serverErr <- err
			return
		}
		if _, err := serverSide.Write(protocol.Encode(protocol.CmdFileEnd, nil)); err != nil {
			serverErr <- err
			return
		}
		f, err := expect(protocol.CmdCtrlMulti)
		if err != nil {
			serverErr <- err
			return
		}
		if !bytes.Contains(f.Data, []byte("md5sum ")) {
			serverErr <- fmt.Errorf("post-upload command = %q, want md5sum", string(f.Data))
			return
		}
		<-done
		serverErr <- nil
	}()

	start := time.Now()
	eng.drain()
	elapsed := time.Since(start)
	close(done)
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("drain elapsed = %s, post-upload md5 used operation timeout instead of short check", elapsed)
	}
	if entry, _ := st.GetEntry(remote); entry.Sync != store.Synced {
		t.Fatalf("entry sync = %q, want synced", entry.Sync)
	}
	if job := st.ListJobs()[0]; job.State != store.Done {
		t.Fatalf("job state = %q, want done", job.State)
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
	job, _ := st.Enqueue(store.Job{Kind: store.JobUpload, Path: remote, CachePath: p, MD5: hex.EncodeToString(sum[:]), Size: 0})
	eng.now = func() time.Time { return job.CreatedAt.Add(zeroByteUploadSettle + time.Second) }

	eng.drain()
	if e, _ := st.GetEntry(remote); e.Sync != store.Synced {
		t.Errorf("empty upload sync = %q, want synced", e.Sync)
	}
	if b, ok := m.File(remote); !ok || len(b) != 0 {
		t.Errorf("machine empty file = %v ok=%v", b, ok)
	}
}

func TestZeroByteUploadWaitsForSettle(t *testing.T) {
	m, st, arb, tr := setup(t)
	eng := newEngine(st, arb)
	tr.Observe(machine.Idle)
	now := time.Now()
	eng.now = func() time.Time { return now }

	dir := t.TempDir()
	p := filepath.Join(dir, "empty.nc")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := md5.Sum(nil)
	remote := "/sd/gcodes/empty-placeholder.nc"
	st.PutEntry(store.Entry{Path: remote, Size: 0, MD5: hex.EncodeToString(sum[:]), CachePath: p, Sync: store.PendingUpload})
	job, _ := st.Enqueue(store.Job{Kind: store.JobUpload, Path: remote, CachePath: p, MD5: hex.EncodeToString(sum[:]), Size: 0})
	now = job.CreatedAt

	eng.drain()
	if _, ok := m.File(remote); ok {
		t.Fatal("zero-byte upload ran before settle window")
	}
	if j, ok := st.NextQueued(); !ok || j.Path != remote {
		t.Fatalf("zero-byte job = %+v ok=%v, want queued", j, ok)
	}

	now = now.Add(zeroByteUploadSettle + time.Second)
	eng.drain()
	if b, ok := m.File(remote); !ok || len(b) != 0 {
		t.Fatalf("settled zero-byte file = %v ok=%v, want empty upload", b, ok)
	}
}

func TestZeroByteUploadSupersededBeforeSettleUploadsContent(t *testing.T) {
	m, st, arb, tr := setup(t)
	eng := newEngine(st, arb)
	tr.Observe(machine.Idle)
	now := time.Now()
	eng.now = func() time.Time { return now }

	dir := t.TempDir()
	emptyPath := filepath.Join(dir, "empty.nc")
	if err := os.WriteFile(emptyPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	emptySum := md5.Sum(nil)
	remote := "/sd/gcodes/placeholder.nc"
	st.PutEntry(store.Entry{Path: remote, Size: 0, MD5: hex.EncodeToString(emptySum[:]), CachePath: emptyPath, Sync: store.PendingUpload})
	st.Enqueue(store.Job{Kind: store.JobUpload, Path: remote, CachePath: emptyPath, MD5: hex.EncodeToString(emptySum[:]), Size: 0})

	content := []byte("G0 X0\nG1 X1\n")
	contentPath := filepath.Join(dir, "content.nc")
	if err := os.WriteFile(contentPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	contentSum := md5.Sum(content)
	md5hex := hex.EncodeToString(contentSum[:])
	if _, err := st.SupersedeQueuedUploads(remote); err != nil {
		t.Fatal(err)
	}
	st.PutEntry(store.Entry{Path: remote, Size: int64(len(content)), MD5: md5hex, CachePath: contentPath, Sync: store.PendingUpload})
	st.Enqueue(store.Job{Kind: store.JobUpload, Path: remote, CachePath: contentPath, MD5: md5hex, Size: int64(len(content))})

	eng.drain()
	got, ok := m.File(remote)
	if !ok || !bytes.Equal(got, content) {
		t.Fatalf("machine content = %q ok=%v, want %q", string(got), ok, string(content))
	}
	for _, j := range st.ListJobs() {
		if j.Size == 0 && j.State != store.Done {
			t.Fatalf("zero placeholder job = %+v, want done", j)
		}
	}
}

func contentMD5(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// TestRenameThenWriteToSourceKeepsNewerContent: a write to the rename's source
// path that lands after the rename is queued must not be lost. The machine mv
// still happens first (per-path FIFO), but the completion must not move the
// NEWER source entry to the destination or silently drop its queued upload.
func TestRenameThenWriteToSourceKeepsNewerContent(t *testing.T) {
	m, st, arb, tr := setup(t)
	eng := newEngine(st, arb)
	tr.Observe(machine.Idle)

	src := "/sd/gcodes/src.nc"
	dst := "/sd/gcodes/dst.nc"
	oldContent := []byte("old source content\n")
	m.PutFile(src, oldContent)
	st.PutEntry(store.Entry{Path: src, Size: int64(len(oldContent)), MD5: contentMD5(oldContent), Sync: store.PendingRename})
	st.Enqueue(store.Job{Kind: store.JobRename, Path: src, DestPath: dst, MD5: contentMD5(oldContent), Size: int64(len(oldContent))})

	// A newer write to the source path arrives before the queue drains.
	newCache, newMD5, newSize := writeCache(t, t.TempDir(), 300)
	st.PutEntry(store.Entry{Path: src, Size: newSize, MD5: newMD5, CachePath: newCache, Sync: store.PendingUpload})
	st.Enqueue(store.Job{Kind: store.JobUpload, Path: src, CachePath: newCache, MD5: newMD5, Size: newSize})

	eng.drain()

	// The rename moved the OLD content to the destination on the machine...
	if got, ok := m.File(dst); !ok || !bytes.Equal(got, oldContent) {
		t.Fatalf("machine %s = %d bytes ok=%v, want the pre-rename content", dst, len(got), ok)
	}
	// ...and the NEWER write to the source was uploaded, not silently dropped.
	gotSrc, ok := m.File(src)
	if !ok {
		t.Fatalf("machine %s missing: the newer write was lost", src)
	}
	if contentMD5(gotSrc) != newMD5 {
		t.Fatalf("machine %s md5 = %s, want the newer write's %s", src, contentMD5(gotSrc), newMD5)
	}
	srcEntry, ok := st.GetEntry(src)
	if !ok || srcEntry.Sync != store.Synced || srcEntry.MD5 != newMD5 {
		t.Fatalf("source entry = %+v ok=%v, want synced newer content", srcEntry, ok)
	}
	// No job may be left queued, and no entry may claim an MD5 the machine
	// does not hold.
	if j, ok := st.NextQueued(); ok {
		t.Fatalf("job left queued after drain: %+v", j)
	}
	if dstEntry, ok := st.GetEntry(dst); ok {
		if got, _ := m.File(dst); dstEntry.MD5 != "" && dstEntry.MD5 != contentMD5(got) {
			t.Fatalf("destination entry md5 %s does not match machine content %s", dstEntry.MD5, contentMD5(got))
		}
	}
}

// TestDeferredRenameDoesNotClobberDestinationUpload: while a rename into B is
// backing off, a fresh upload to B must not run ahead of it (deferral must key
// on DestPath too) — otherwise the later mv would overwrite the fresh content.
func TestDeferredRenameDoesNotClobberDestinationUpload(t *testing.T) {
	m, st, arb, tr := setup(t)
	eng := newEngine(st, arb)
	eng.baseBackoff = time.Hour // keep the failed rename backing off
	tr.Observe(machine.Idle)

	src := "/sd/gcodes/move-src.nc"
	dst := "/sd/gcodes/move-dst.nc"
	oldContent := []byte("content moved by rename\n")
	m.PutFile(src, oldContent)
	st.PutEntry(store.Entry{Path: src, Size: int64(len(oldContent)), MD5: contentMD5(oldContent), Sync: store.PendingRename})
	renameJob, err := st.Enqueue(store.Job{Kind: store.JobRename, Path: src, DestPath: dst, MD5: contentMD5(oldContent), Size: int64(len(oldContent))})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate one failed attempt so the rename is backing off.
	if err := st.UpdateJob(renameJob.ID, func(j *store.Job) { j.Attempts = 1; j.LastError = "transient mv failure" }); err != nil {
		t.Fatal(err)
	}

	// A fresh upload to the rename's DESTINATION arrives while it backs off.
	freshCache, freshMD5, freshSize := writeCache(t, t.TempDir(), 250)
	st.PutEntry(store.Entry{Path: dst, Size: freshSize, MD5: freshMD5, CachePath: freshCache, Sync: store.PendingUpload})
	st.Enqueue(store.Job{Kind: store.JobUpload, Path: dst, CachePath: freshCache, MD5: freshMD5, Size: freshSize})

	eng.drain()
	if _, ok := m.File(dst); ok {
		t.Fatal("upload to the destination ran ahead of the deferred rename into it")
	}

	// Let the backoff elapse; the rename then the fresh upload run in order.
	eng.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	eng.drain()

	got, ok := m.File(dst)
	if !ok {
		t.Fatalf("machine %s missing after drains", dst)
	}
	if contentMD5(got) != freshMD5 {
		t.Fatalf("machine %s md5 = %s, want the fresh upload's %s (rename clobbered it)", dst, contentMD5(got), freshMD5)
	}
	dstEntry, ok := st.GetEntry(dst)
	if !ok || dstEntry.Sync != store.Synced || dstEntry.MD5 != freshMD5 {
		t.Fatalf("destination entry = %+v ok=%v, want synced fresh content", dstEntry, ok)
	}
	if _, ok := m.File(src); ok {
		t.Fatalf("machine still has %s after rename drained", src)
	}
	if j, ok := st.NextQueued(); ok {
		t.Fatalf("job left queued after drains: %+v", j)
	}
}

// TestMkdirSettlesEntryToSynced: a successful JobMkdir must settle the catalog
// entry to synced; leaving it pending_upload makes a later delete local-discard
// (no machine rm) and reconcile then resurrects the directory.
func TestMkdirSettlesEntryToSynced(t *testing.T) {
	m, st, arb, tr := setup(t)
	eng := newEngine(st, arb)
	tr.Observe(machine.Idle)

	dir := "/sd/gcodes/newdir"
	st.PutEntry(store.Entry{Path: dir, IsDir: true, Sync: store.PendingUpload})
	st.Enqueue(store.Job{Kind: store.JobMkdir, Path: dir})

	eng.drain()

	if !m.HasDir(dir) {
		t.Fatal("mkdir never reached the machine")
	}
	entry, ok := st.GetEntry(dir)
	if !ok || entry.Sync != store.Synced {
		t.Fatalf("dir entry after mkdir = %+v ok=%v, want synced", entry, ok)
	}
	if job := st.ListJobs()[0]; job.State != store.Done {
		t.Fatalf("mkdir job = %+v, want done", job)
	}
}

// TestDeleteProxyCreatedDirIsNotResurrected drives the full mkdir->sync->
// delete->reconcile cycle through the service: deleting a proxy-created
// directory must issue a real machine rm, and a reconcile sweep afterwards must
// not resurrect it. (Lives here rather than service_test.go because it needs
// the engine's drain/reconcile, and synceng already imports service.)
func TestDeleteProxyCreatedDirIsNotResurrected(t *testing.T) {
	m, st, arb, tr := setup(t)
	eng := newEngine(st, arb)
	tr.Observe(machine.Idle)
	svc, err := service.New(st, arb)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Mkdir("newdir"); err != nil {
		t.Fatal(err)
	}
	eng.drain()
	if !m.HasDir("/sd/gcodes/newdir") {
		t.Fatal("mkdir never reached the machine")
	}
	if entry, ok := st.GetEntry("/sd/gcodes/newdir"); !ok || entry.Sync != store.Synced {
		t.Fatalf("dir entry after mkdir drain = %+v ok=%v, want synced", entry, ok)
	}

	if err := svc.Delete("newdir"); err != nil {
		t.Fatalf("delete proxy-created dir: %v", err)
	}
	eng.drain()
	if m.HasDir("/sd/gcodes/newdir") {
		t.Fatal("no machine rm was issued: the directory still exists on the machine")
	}

	if err := eng.Reconcile(3); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if entry, ok := st.GetEntry("/sd/gcodes/newdir"); ok {
		t.Fatalf("deleted directory resurrected by reconcile: %+v", entry)
	}
}
