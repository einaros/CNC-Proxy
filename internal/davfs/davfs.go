// Package davfs adapts the service to a WebDAV filesystem, so the machine's
// gcode directory can be mounted natively on macOS/Windows/Linux with no driver
// install and nothing to sign — the OS's built-in WebDAV client connects to the
// server we run here.
//
// Semantics are Google-Drive-like, inherited from the service:
//   - A write buffers locally and, on close, lands in the cache immediately and
//     enqueues an upload. The file is visible and readable at once; it reaches
//     the machine later, when the machine is idle and no controller is attached.
//   - Reads are served from the local cache. Files known only to be on the
//     machine but not cached return a not-yet-available error (download-on-demand
//     is a future enhancement).
//   - Directory listings come from the catalog.
//
// The status of in-flight files is surfaced through the web UI / tray app, not
// through native file badges (which would require signing on macOS).
package davfs

import (
	"context"
	"errors"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/net/webdav"

	"github.com/uwin/cnc-proxy/internal/service"
	"github.com/uwin/cnc-proxy/internal/session"
	"github.com/uwin/cnc-proxy/internal/store"
)

// FS implements webdav.FileSystem backed by the service.
type FS struct {
	svc *service.Service
}

// New returns a WebDAV filesystem over the service.
func New(svc *service.Service) *FS { return &FS{svc: svc} }

// Handler returns a ready-to-serve WebDAV HTTP handler.
func (fs *FS) Handler(prefix string) *webdav.Handler {
	return &webdav.Handler{
		Prefix:     prefix,
		FileSystem: fs,
		LockSystem: webdav.NewMemLS(),
	}
}

func (fs *FS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	if isRoot(name) {
		return os.ErrExist
	}
	_, err := fs.svc.Mkdir(svcPath(name))
	return err
}

func (fs *FS) RemoveAll(ctx context.Context, name string) error {
	if isRoot(name) {
		return os.ErrPermission
	}
	if err := fs.svc.Delete(svcPath(name)); err != nil {
		if err == service.ErrNotFound {
			return os.ErrNotExist
		}
		return err
	}
	return nil
}

func (fs *FS) Rename(ctx context.Context, oldName, newName string) error {
	if err := fs.svc.Rename(svcPath(oldName), svcPath(newName)); err != nil {
		if err == service.ErrNotFound {
			return os.ErrNotExist
		}
		return err
	}
	return nil
}

func (fs *FS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	if isRoot(name) {
		return rootInfo(), nil
	}
	entry, ok := fs.svc.Lookup(svcPath(name))
	if !ok {
		return nil, os.ErrNotExist
	}
	return fileInfoFromEntry(entry), nil
}

func (fs *FS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	writable := flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC) != 0
	if writable {
		return newWriteFile(fs.svc, svcPath(name))
	}
	// Read or directory open.
	if isRoot(name) {
		return fs.openDir(name)
	}
	entry, ok := fs.svc.Lookup(svcPath(name))
	if !ok {
		return nil, os.ErrNotExist
	}
	if entry.IsDir {
		return fs.openDir(name)
	}
	// Open serves from cache, fetching from the machine on demand for files that
	// are known but not yet cached (remote_only).
	rc, e, err := fs.svc.Open(svcPath(name))
	if err != nil {
		switch {
		case err == service.ErrNotCached:
			// On the machine but not fetchable right now (relay active / not idle).
			return nil, &notCachedError{name: name}
		case errors.Is(err, session.ErrRelayActive), errors.Is(err, session.ErrNotIdle):
			return nil, &notCachedError{name: name}
		default:
			return nil, os.ErrNotExist
		}
	}
	return newReadFile(rc, e), nil
}

func (fs *FS) openDir(name string) (webdav.File, error) {
	children, err := fs.svc.Children(svcPath(name))
	if err != nil {
		return nil, err
	}
	infos := make([]os.FileInfo, 0, len(children))
	for _, c := range children {
		infos = append(infos, fileInfoFromEntry(c))
	}
	var info os.FileInfo = rootInfo()
	if !isRoot(name) {
		if e, ok := fs.svc.Lookup(svcPath(name)); ok {
			info = fileInfoFromEntry(e)
		}
	}
	return newDirFile(info, infos), nil
}

// isRoot reports whether a WebDAV path refers to the mount root.
func isRoot(name string) bool {
	return svcPath(name) == ""
}

// svcPath converts a WebDAV mount-relative path (rooted at "/") into the
// relative path the service expects (which it joins under GcodeRoot). The mount
// root "/" maps to "" so the service resolves it to GcodeRoot itself.
func svcPath(name string) string {
	return strings.Trim(path.Clean("/"+name), "/")
}

// --- FileInfo ---

type fileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	mtime   time.Time
	isDir   bool
}

func (fi *fileInfo) Name() string       { return fi.name }
func (fi *fileInfo) Size() int64        { return fi.size }
func (fi *fileInfo) Mode() os.FileMode  { return fi.mode }
func (fi *fileInfo) ModTime() time.Time { return fi.mtime }
func (fi *fileInfo) IsDir() bool        { return fi.isDir }
func (fi *fileInfo) Sys() any           { return nil }

func fileInfoFromEntry(e store.Entry) *fileInfo {
	mode := os.FileMode(0o644)
	if e.IsDir {
		mode = os.ModeDir | 0o755
	}
	return &fileInfo{
		name:  path.Base(e.Path),
		size:  e.Size,
		mode:  mode,
		mtime: e.MTime,
		isDir: e.IsDir,
	}
}

func rootInfo() *fileInfo {
	return &fileInfo{name: "gcodes", mode: os.ModeDir | 0o755, isDir: true, mtime: time.Unix(0, 0)}
}

// notCachedError reports a file present on the machine but not yet cached
// locally. It maps to a 404-ish condition for the WebDAV client until
// download-on-demand is implemented.
type notCachedError struct{ name string }

func (e *notCachedError) Error() string {
	return "davfs: " + e.name + " is on the machine but not cached locally yet"
}
