package synceng

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/uwin/cnc-proxy/internal/client"
	"github.com/uwin/cnc-proxy/internal/machine"
	"github.com/uwin/cnc-proxy/internal/store"
)

func testTimeout() time.Duration { return 3 * time.Second }

// uploadRaw uploads content to remote via the client, computing its md5.
func uploadRaw(t *testing.T, conn *client.Conn, remote string, content []byte) {
	t.Helper()
	sum := md5.Sum(content)
	err := conn.Upload(remote, bytes.NewReader(content), int64(len(content)), hex.EncodeToString(sum[:]), testTimeout(), nil)
	if err != nil {
		t.Fatalf("uploadRaw %s: %v", remote, err)
	}
}

// seedMachineFile uploads content directly to the fake machine via a client,
// so it exists on the machine but is unknown to our catalog (the out-of-band
// case reconcile must discover).
func seedMachineFile(t *testing.T, addr, remote string, content []byte) {
	t.Helper()
	conn, err := client.Dial(addr, testTimeout(), client.WithUploadStartDelay(0))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	uploadRaw(t, conn, remote, content)
}

func TestReconcileDiscoversAndPrunes(t *testing.T) {
	m, st, arb, tr := setup(t)
	eng := newEngine(st, arb)
	tr.Observe(machine.Idle)

	// Two files appear on the machine out-of-band (e.g. via the controller).
	seedMachineFile(t, m.Addr(), "/sd/gcodes/a.nc", []byte("hello"))
	seedMachineFile(t, m.Addr(), "/sd/gcodes/b.nc", []byte("world!!"))

	if err := eng.Reconcile(4); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	a, ok := st.GetEntry("/sd/gcodes/a.nc")
	if !ok || a.Sync != store.RemoteOnly || a.Size != 5 {
		t.Errorf("a.nc after reconcile = %+v ok=%v", a, ok)
	}
	if b, ok := st.GetEntry("/sd/gcodes/b.nc"); !ok || b.Size != 7 {
		t.Errorf("b.nc after reconcile = %+v ok=%v", b, ok)
	}

	// Remove one on the machine; reconcile should drop the settled entry.
	if err := removeOnMachine(t, m.Addr(), "/sd/gcodes/a.nc"); err != nil {
		t.Fatal(err)
	}
	if err := eng.Reconcile(4); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.GetEntry("/sd/gcodes/a.nc"); ok {
		t.Error("a.nc should have been pruned after machine-side delete")
	}
	if _, ok := st.GetEntry("/sd/gcodes/b.nc"); !ok {
		t.Error("b.nc should still be present")
	}
}

func TestReconcileLeavesInflightAlone(t *testing.T) {
	m, st, arb, tr := setup(t)
	eng := newEngine(st, arb)
	tr.Observe(machine.Idle)

	// A locally-queued upload that hasn't synced yet — not on the machine.
	st.PutEntry(store.Entry{Path: "/sd/gcodes/pending.nc", Size: 10, Sync: store.PendingUpload})

	if err := eng.Reconcile(4); err != nil {
		t.Fatal(err)
	}
	// Reconcile must NOT prune a pending (in-flight) entry just because it isn't
	// on the machine yet.
	e, ok := st.GetEntry("/sd/gcodes/pending.nc")
	if !ok || e.Sync != store.PendingUpload {
		t.Errorf("pending entry = %+v ok=%v, want untouched pending_upload", e, ok)
	}
	_ = m
}

func TestReconcileRemoteOnlyMTimeChangeInvalidates(t *testing.T) {
	m, st, arb, tr := setup(t)
	eng := newEngine(st, arb)
	tr.Observe(machine.Idle)

	seedMachineFile(t, m.Addr(), "/sd/gcodes/remote.nc", []byte("same"))
	old := time.Date(2020, 1, 1, 12, 0, 0, 0, time.Local)
	if err := st.PutEntry(store.Entry{
		Path:  "/sd/gcodes/remote.nc",
		Size:  4,
		MTime: old,
		Sync:  store.RemoteOnly,
	}); err != nil {
		t.Fatal(err)
	}

	if err := eng.Reconcile(4); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetEntry("/sd/gcodes/remote.nc")
	if got.MTime.Equal(old) {
		t.Fatalf("mtime was not refreshed: %+v", got)
	}
}

func TestDeepReconcileDetectsSameSizeRemoteChange(t *testing.T) {
	m, st, arb, tr := setup(t)
	eng := newEngine(st, arb)
	tr.Observe(machine.Idle)

	remote := "/sd/gcodes/same-size.nc"
	local := []byte("AAAA")
	changed := []byte("BBBB")
	seedMachineFile(t, m.Addr(), remote, local)

	cachePath := filepath.Join(t.TempDir(), "cache")
	if err := os.WriteFile(cachePath, local, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := md5.Sum(local)
	if err := st.PutEntry(store.Entry{
		Path:      remote,
		Size:      int64(len(local)),
		MTime:     time.Now(),
		MD5:       hex.EncodeToString(sum[:]),
		CachePath: cachePath,
		Sync:      store.Synced,
	}); err != nil {
		t.Fatal(err)
	}

	seedMachineFile(t, m.Addr(), remote, changed)
	if err := eng.DeepReconcile(4); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetEntry(remote)
	if got.Sync != store.RemoteOnly || got.CachePath != "" {
		t.Fatalf("entry after deep reconcile = %+v, want remote_only without cache", got)
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatalf("stale cache stat = %v, want removed", err)
	}
}

func TestDeepReconcileLeavesUnchangedSyncedFileCached(t *testing.T) {
	m, st, arb, tr := setup(t)
	eng := newEngine(st, arb)
	tr.Observe(machine.Idle)

	remote := "/sd/gcodes/unchanged.nc"
	content := []byte("AAAA")
	seedMachineFile(t, m.Addr(), remote, content)
	cachePath := filepath.Join(t.TempDir(), "cache")
	if err := os.WriteFile(cachePath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := md5.Sum(content)
	if err := st.PutEntry(store.Entry{
		Path:      remote,
		Size:      int64(len(content)),
		MD5:       hex.EncodeToString(sum[:]),
		CachePath: cachePath,
		Sync:      store.Synced,
	}); err != nil {
		t.Fatal(err)
	}

	if err := eng.DeepReconcile(4); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetEntry(remote)
	if got.Sync != store.Synced || got.CachePath != cachePath {
		t.Fatalf("unchanged entry = %+v, want synced with cache", got)
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("cache should remain: %v", err)
	}
}

func TestDeepReconcileMd5FailureIsNonFatal(t *testing.T) {
	m, st, arb, tr := setup(t)
	eng := newEngine(st, arb)
	tr.Observe(machine.Idle)

	failPath := "/sd/gcodes/fail.nc"
	changePath := "/sd/gcodes/change.nc"
	seedMachineFile(t, m.Addr(), failPath, []byte("AAAA"))
	seedMachineFile(t, m.Addr(), changePath, []byte("1111"))
	m.FailCommand("md5sum " + failPath)

	for _, p := range []string{failPath, changePath} {
		content := []byte("AAAA")
		if p == changePath {
			content = []byte("0000")
		}
		cachePath := filepath.Join(t.TempDir(), filepath.Base(p))
		if err := os.WriteFile(cachePath, content, 0o644); err != nil {
			t.Fatal(err)
		}
		sum := md5.Sum(content)
		if err := st.PutEntry(store.Entry{
			Path:      p,
			Size:      int64(len(content)),
			MD5:       hex.EncodeToString(sum[:]),
			CachePath: cachePath,
			Sync:      store.Synced,
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := eng.DeepReconcile(4); err != nil {
		t.Fatal(err)
	}
	failEntry, _ := st.GetEntry(failPath)
	if failEntry.Sync != store.Synced {
		t.Fatalf("md5 failure entry changed: %+v", failEntry)
	}
	changeEntry, _ := st.GetEntry(changePath)
	if changeEntry.Sync != store.RemoteOnly {
		t.Fatalf("independent changed entry not reconciled: %+v", changeEntry)
	}
}

func TestPrepareStartupCacheValidationMarksSyncedOnly(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	eng := New(Config{Store: st})

	syncedContent := []byte("G0 X0\n")
	syncedCache := writeCacheContent(t, syncedContent)
	if err := st.PutEntry(store.Entry{
		Path:       "/sd/gcodes/synced.nc",
		Size:       int64(len(syncedContent)),
		MD5:        md5HexBytes(syncedContent),
		CachePath:  syncedCache,
		CacheState: store.CacheReady,
		Sync:       store.Synced,
	}); err != nil {
		t.Fatal(err)
	}

	pendingContent := []byte("G0 X1\n")
	pendingCache := writeCacheContent(t, pendingContent)
	if err := st.PutEntry(store.Entry{
		Path:       "/sd/gcodes/pending.nc",
		Size:       int64(len(pendingContent)),
		MD5:        md5HexBytes(pendingContent),
		CachePath:  pendingCache,
		CacheState: store.CacheReady,
		Sync:       store.PendingUpload,
	}); err != nil {
		t.Fatal(err)
	}

	missingCache := filepath.Join(t.TempDir(), "missing-cache")
	if err := st.PutEntry(store.Entry{
		Path:       "/sd/gcodes/missing.nc",
		Size:       12,
		MD5:        md5HexBytes([]byte("missing")),
		CachePath:  missingCache,
		CacheState: store.CacheReady,
		Sync:       store.Synced,
	}); err != nil {
		t.Fatal(err)
	}

	if err := eng.PrepareStartupCacheValidation(); err != nil {
		t.Fatal(err)
	}

	synced, _ := st.GetEntry("/sd/gcodes/synced.nc")
	if synced.CacheState != store.CacheValidating || synced.CachePath != syncedCache || !synced.CacheCheckedAt.IsZero() {
		t.Fatalf("synced entry after prepare = %+v, want validating with cache", synced)
	}
	pending, _ := st.GetEntry("/sd/gcodes/pending.nc")
	if pending.CacheState != store.CacheReady || pending.Sync != store.PendingUpload || pending.CachePath != pendingCache {
		t.Fatalf("pending entry after prepare = %+v, want ready pending upload", pending)
	}
	missing, _ := st.GetEntry("/sd/gcodes/missing.nc")
	if missing.Sync != store.RemoteOnly || missing.CacheState != store.CacheNone || missing.CachePath != "" {
		t.Fatalf("missing cache entry after prepare = %+v, want remote_only without cache", missing)
	}
}

func TestPrepareStartupCacheValidationMarksRunningJobsFailed(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	eng := New(Config{Store: st})
	job, err := st.Enqueue(store.Job{Kind: store.JobUpload, Path: "/sd/gcodes/interrupted.nc"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateJob(job.ID, func(j *store.Job) {
		j.State = store.Running
	}); err != nil {
		t.Fatal(err)
	}

	if err := eng.PrepareStartupCacheValidation(); err != nil {
		t.Fatal(err)
	}
	got := st.ListJobs()[0]
	if got.State != store.Failed || got.LastError == "" {
		t.Fatalf("running job after startup prepare = %+v, want failed with message", got)
	}
}

func TestValidateStartupCacheOutcomes(t *testing.T) {
	m, st, arb, tr := setup(t)
	eng := newEngine(st, arb)
	tr.Observe(machine.Idle)

	matchContent := []byte("AAAA")
	changedLocal := []byte("BBBB")
	changedRemote := []byte("CCCC")
	missingContent := []byte("DDDD")
	failContent := []byte("EEEE")

	seedMachineFile(t, m.Addr(), "/sd/gcodes/match.nc", matchContent)
	seedMachineFile(t, m.Addr(), "/sd/gcodes/changed.nc", changedRemote)
	seedMachineFile(t, m.Addr(), "/sd/gcodes/fail-md5.nc", failContent)
	m.FailCommand("md5sum /sd/gcodes/fail-md5.nc")

	matchCache := writeCacheContent(t, matchContent)
	changedCache := writeCacheContent(t, changedLocal)
	missingCache := writeCacheContent(t, missingContent)
	failCache := writeCacheContent(t, failContent)

	for _, tc := range []struct {
		path    string
		content []byte
		cache   string
	}{
		{"/sd/gcodes/match.nc", matchContent, matchCache},
		{"/sd/gcodes/changed.nc", changedLocal, changedCache},
		{"/sd/gcodes/missing.nc", missingContent, missingCache},
		{"/sd/gcodes/fail-md5.nc", failContent, failCache},
	} {
		if err := st.PutEntry(store.Entry{
			Path:       tc.path,
			Size:       int64(len(tc.content)),
			MD5:        md5HexBytes(tc.content),
			CachePath:  tc.cache,
			CacheState: store.CacheValidating,
			Sync:       store.Synced,
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := eng.ValidateStartupCache(4); err != nil {
		t.Fatal(err)
	}

	match, _ := st.GetEntry("/sd/gcodes/match.nc")
	if match.CacheState != store.CacheReady || match.CachePath != matchCache || match.CacheCheckedAt.IsZero() {
		t.Fatalf("matching entry after validation = %+v, want ready cache", match)
	}
	changed, _ := st.GetEntry("/sd/gcodes/changed.nc")
	if changed.Sync != store.RemoteOnly || changed.CacheState != store.CacheNone || changed.CachePath != "" || changed.MD5 == md5HexBytes(changedLocal) {
		t.Fatalf("changed entry after validation = %+v, want remote_only with remote md5", changed)
	}
	if _, err := os.Stat(changedCache); !os.IsNotExist(err) {
		t.Fatalf("changed cache stat = %v, want removed", err)
	}
	if _, ok := st.GetEntry("/sd/gcodes/missing.nc"); ok {
		t.Fatal("missing remote entry should be removed from catalog")
	}
	if _, err := os.Stat(missingCache); !os.IsNotExist(err) {
		t.Fatalf("missing cache stat = %v, want removed", err)
	}
	failed, _ := st.GetEntry("/sd/gcodes/fail-md5.nc")
	if failed.CacheState != store.CacheValidating || failed.CachePath != failCache {
		t.Fatalf("md5 failure entry after validation = %+v, want still validating", failed)
	}
	if _, err := os.Stat(failCache); err != nil {
		t.Fatalf("md5 failure cache should remain: %v", err)
	}
}

func TestReconcileLeavesErrorStateAlone(t *testing.T) {
	_, st, arb, tr := setup(t)
	eng := newEngine(st, arb)
	tr.Observe(machine.Idle)

	if err := st.PutEntry(store.Entry{
		Path:  "/sd/gcodes/error.nc",
		Size:  4,
		Sync:  store.Error,
		Error: "previous failure",
	}); err != nil {
		t.Fatal(err)
	}
	if err := eng.DeepReconcile(4); err != nil {
		t.Fatal(err)
	}
	got, ok := st.GetEntry("/sd/gcodes/error.nc")
	if !ok || got.Sync != store.Error || got.Error != "previous failure" {
		t.Fatalf("error entry = %+v ok=%v, want untouched", got, ok)
	}
}

func writeCacheContent(t *testing.T, content []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cache.bin")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func md5HexBytes(content []byte) string {
	sum := md5.Sum(content)
	return hex.EncodeToString(sum[:])
}

func TestReconcileBlockedWhenNotIdle(t *testing.T) {
	m, st, arb, tr := setup(t)
	eng := newEngine(st, arb)
	m.SetStatus("<Run|MPos:0,0,0|WPos:0,0,0>")
	tr.Observe(machine.Run) // busy

	err := eng.Reconcile(4)
	if !isBlocked(err) {
		t.Errorf("reconcile while busy = %v, want a blocked error", err)
	}
}

// removeOnMachine deletes a file directly on the fake machine via the protocol.
func removeOnMachine(t *testing.T, addr, remote string) error {
	t.Helper()
	conn, err := client.Dial(addr, testTimeout())
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Remove(remote, testTimeout())
}

func TestListTreeRecurses(t *testing.T) {
	m, _, _, _ := setup(t)
	conn, err := client.Dial(m.Addr(), testTimeout(), client.WithUploadStartDelay(0))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	conn.Mkdir("/sd/gcodes/sub", testTimeout())
	uploadRaw(t, conn, "/sd/gcodes/top.nc", []byte("x"))
	uploadRaw(t, conn, "/sd/gcodes/sub/nested.nc", []byte("yy"))

	tree, err := listTree(conn, "/sd/gcodes", 4, testTimeout())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/sd/gcodes/sub", "/sd/gcodes/top.nc", "/sd/gcodes/sub/nested.nc"} {
		if _, ok := tree[want]; !ok {
			t.Errorf("listTree missing %q (got %v)", want, keys(tree))
		}
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
