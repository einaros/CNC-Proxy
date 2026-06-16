package davfs

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uwin/cnc-proxy/internal/carveratest"
	"github.com/uwin/cnc-proxy/internal/client"
	"github.com/uwin/cnc-proxy/internal/httpauth"
	"github.com/uwin/cnc-proxy/internal/machine"
	"github.com/uwin/cnc-proxy/internal/service"
	"github.com/uwin/cnc-proxy/internal/session"
	"github.com/uwin/cnc-proxy/internal/store"
)

func newFS(t *testing.T) (*FS, *service.Service) {
	t.Helper()
	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	arb := session.New(session.Config{Dial: func() (*client.Conn, error) { return nil, io.EOF }})
	svc, err := service.New(st, arb)
	if err != nil {
		t.Fatal(err)
	}
	return New(svc), svc
}

// TestJunkFilesNotSynced ensures OS-metadata files written into the mount are
// accepted (so the file manager is happy) but never reach the catalog/queue and
// thus never the CNC.
func TestJunkFilesNotSynced(t *testing.T) {
	fs, svc := newFS(t)
	ctx := context.Background()

	for _, junk := range []string{"/._part.nc", "/.DS_Store", "/sub/._x", "/Thumbs.db"} {
		wf, err := fs.OpenFile(ctx, junk, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatalf("open junk %q: %v", junk, err)
		}
		if _, err := wf.Write([]byte("garbage")); err != nil {
			t.Errorf("write junk %q: %v", junk, err)
		}
		if err := wf.Close(); err != nil {
			t.Errorf("close junk %q: %v", junk, err)
		}
		// Stat must report it as nonexistent, and nothing should be enqueued.
		if _, err := fs.Stat(ctx, junk); !os.IsNotExist(err) {
			t.Errorf("stat junk %q = %v, want NotExist", junk, err)
		}
	}
	if files := svc.Files(); len(files) != 0 {
		t.Errorf("junk leaked into catalog: %+v", files)
	}
	// Mkdir/Remove/Rename of junk are no-op successes.
	if err := fs.Mkdir(ctx, "/.Spotlight-V100", 0o755); err != nil {
		t.Errorf("mkdir junk: %v", err)
	}
	if err := fs.RemoveAll(ctx, "/._gone"); err != nil {
		t.Errorf("remove junk: %v", err)
	}
}

func TestWriteThenReadAndStat(t *testing.T) {
	fs, _ := newFS(t)
	ctx := context.Background()

	// Open for write, write content, close → flushes into the service.
	wf, err := fs.OpenFile(ctx, "/part.nc", os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("G0 X0 Y0\nG1 X10 Y10\n")
	if _, err := wf.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := wf.Close(); err != nil {
		t.Fatal(err)
	}

	// Stat should now find it.
	fi, err := fs.Stat(ctx, "/part.nc")
	if err != nil {
		t.Fatalf("stat after write: %v", err)
	}
	if fi.Name() != "part.nc" || fi.Size() != int64(len(content)) || fi.IsDir() {
		t.Errorf("fileinfo = name=%q size=%d dir=%v", fi.Name(), fi.Size(), fi.IsDir())
	}

	// Read it back from cache.
	rf, err := fs.OpenFile(ctx, "/part.nc", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	got, _ := io.ReadAll(rf)
	if string(got) != string(content) {
		t.Errorf("read back = %q", got)
	}
}

func TestStatMissing(t *testing.T) {
	fs, _ := newFS(t)
	if _, err := fs.Stat(context.Background(), "/nope.nc"); !os.IsNotExist(err) {
		t.Errorf("stat missing = %v, want NotExist", err)
	}
}

func TestRootIsDir(t *testing.T) {
	fs, _ := newFS(t)
	fi, err := fs.Stat(context.Background(), "/")
	if err != nil || !fi.IsDir() {
		t.Errorf("root stat = %+v err=%v", fi, err)
	}
}

func TestReaddir(t *testing.T) {
	fs, svc := newFS(t)
	ctx := context.Background()
	svc.Upload("a.nc", strings.NewReader("x"))
	svc.Upload("b.nc", strings.NewReader("yy"))
	svc.Mkdir("sub")

	dir, err := fs.OpenFile(ctx, "/", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	infos, err := dir.Readdir(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 3 {
		t.Fatalf("readdir got %d entries, want 3", len(infos))
	}
	names := map[string]bool{}
	for _, fi := range infos {
		names[fi.Name()] = true
	}
	for _, want := range []string{"a.nc", "b.nc", "sub"} {
		if !names[want] {
			t.Errorf("missing %q in listing", want)
		}
	}
}

func TestReaddirOnlyDirectChildren(t *testing.T) {
	fs, svc := newFS(t)
	ctx := context.Background()
	svc.Mkdir("sub")
	svc.Upload("sub/nested.nc", strings.NewReader("x"))
	svc.Upload("top.nc", strings.NewReader("y"))

	dir, _ := fs.OpenFile(ctx, "/", os.O_RDONLY, 0)
	defer dir.Close()
	infos, _ := dir.Readdir(0)
	// Root should list "sub" and "top.nc" but NOT "nested.nc".
	for _, fi := range infos {
		if fi.Name() == "nested.nc" {
			t.Error("nested file should not appear in root listing")
		}
	}

	subdir, err := fs.OpenFile(ctx, "/sub", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer subdir.Close()
	subInfos, _ := subdir.Readdir(0)
	if len(subInfos) != 1 || subInfos[0].Name() != "nested.nc" {
		t.Errorf("sub listing = %+v, want [nested.nc]", subInfos)
	}
}

func TestRemoveAndRename(t *testing.T) {
	fs, svc := newFS(t)
	ctx := context.Background()
	svc.Upload("a.nc", strings.NewReader("x"))

	if err := fs.Rename(ctx, "/a.nc", "/b.nc"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	// The entry is marked pending_rename; a delete of a missing file 404s.
	if err := fs.RemoveAll(ctx, "/missing.nc"); !os.IsNotExist(err) {
		t.Errorf("remove missing = %v, want NotExist", err)
	}
	if err := fs.RemoveAll(ctx, "/a.nc"); err != nil {
		t.Errorf("remove existing: %v", err)
	}
}

func TestReadNotCached(t *testing.T) {
	m, err := carveratest.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	m.SetStatus("<Run|MPos:0,0,0|WPos:0,0,0>")
	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	tr := machine.NewTracker()
	tr.Observe(machine.Run)
	arb := session.New(session.Config{
		Tracker: tr,
		Dial:    func() (*client.Conn, error) { return client.Dial(m.Addr(), time.Second) },
	})
	svc, err := service.New(st, arb)
	if err != nil {
		t.Fatal(err)
	}
	fs := New(svc)
	ctx := context.Background()
	// A file known only on the machine (remote_only, no cache path).
	if err := svc.PutRemoteOnly("remote.nc", 1234, time.Unix(0, 0), ""); err != nil {
		t.Fatal(err)
	}

	_, err = fs.OpenFile(ctx, "/remote.nc", os.O_RDONLY, 0)
	if err == nil {
		t.Fatal("expected error reading a non-cached remote file")
	}
	var nce *notCachedError
	if !errors.As(err, &nce) {
		t.Errorf("error = %v, want notCachedError", err)
	}
}

func TestWebDAVAuthMiddleware(t *testing.T) {
	fs, _ := newFS(t)
	srv := httptest.NewServer(httpauth.Middleware(
		httpauth.Config{User: "operator", Token: "secret"},
		fs.Handler(""),
	))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated WebDAV status = %d, want 401", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodOptions, srv.URL+"/", nil)
	req.SetBasicAuth("operator", "secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatal("authenticated WebDAV request was rejected")
	}
}
