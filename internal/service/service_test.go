package service

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/uwin/cnc-proxy/internal/carveratest"
	"github.com/uwin/cnc-proxy/internal/client"
	"github.com/uwin/cnc-proxy/internal/machine"
	"github.com/uwin/cnc-proxy/internal/session"
	"github.com/uwin/cnc-proxy/internal/store"
)

// seedOnMachine uploads content directly to the fake machine via a client.
func seedOnMachine(t *testing.T, addr, remote string, content []byte) {
	t.Helper()
	conn, err := client.Dial(addr, 2*time.Second)
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

	svc.Upload("a.nc", bytes.NewReader([]byte("x")))
	if err := svc.Delete("a.nc"); err != nil {
		t.Errorf("delete existing: %v", err)
	}
	e, _ := svc.store.GetEntry("/sd/gcodes/a.nc")
	if e.Sync != store.PendingDelete {
		t.Errorf("sync after delete = %q, want pending_delete", e.Sync)
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
		Dial:    func() (*client.Conn, error) { return client.Dial(m.Addr(), 2*time.Second) },
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
		Dial:    func() (*client.Conn, error) { return client.Dial(m.Addr(), 2*time.Second) },
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
	svc.arb.EnterRelay()
	if svc.Status().Mode != "relay" {
		t.Error("expected relay mode after EnterRelay")
	}
	_ = time.Now
}
