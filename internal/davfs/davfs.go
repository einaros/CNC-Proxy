// Package davfs adapts the service to a WebDAV filesystem, so the machine's
// gcode directory can be mounted natively on macOS/Windows/Linux with no driver
// install and nothing to sign — the OS's built-in WebDAV client connects to the
// server we run here.
//
// Semantics are Google-Drive-like, inherited from the service:
//   - A write buffers locally and, on close, lands in the cache immediately and
//     enqueues an upload. The file is visible and readable at once; it reaches
//     the machine later, when the machine is idle and no controller is attached.
//   - Reads from the mount are served from the local cache only. File managers
//     probe file content while browsing folders, and a probe must not turn into
//     a blocking firmware download of a multi-megabyte remote_only file.
//   - Directory listings come from the catalog.
//
// The status of in-flight files is surfaced through the web UI / tray app, not
// through native file badges (which would require signing on macOS).
package davfs

import (
	"context"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/net/webdav"

	"github.com/uwin/cnc-proxy/internal/service"
	"github.com/uwin/cnc-proxy/internal/store"
)

// FS implements webdav.FileSystem backed by the service.
type FS struct {
	svc *service.Service
}

// New returns a WebDAV filesystem over the service.
func New(svc *service.Service) *FS { return &FS{svc: svc} }

// Handler returns a ready-to-serve WebDAV HTTP handler.
func (fs *FS) Handler(prefix string) http.Handler {
	h := &webdav.Handler{
		Prefix:     prefix,
		FileSystem: fs,
		LockSystem: webdav.NewMemLS(),
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), requestMethodKey{}, r.Method)
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (fs *FS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	if isRoot(name) {
		return os.ErrExist
	}
	if isJunk(name) {
		return nil // pretend success; never create OS-metadata dirs on the CNC
	}
	_, err := fs.svc.Mkdir(svcPath(name))
	return err
}

func (fs *FS) RemoveAll(ctx context.Context, name string) error {
	if isRoot(name) {
		return os.ErrPermission
	}
	if isJunk(name) {
		return nil // junk was never synced; deleting it is a no-op success
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
	if isJunk(oldName) || isJunk(newName) {
		return nil // junk paths aren't tracked; renaming them is a no-op
	}
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
	if isJunk(name) {
		return nil, os.ErrNotExist // junk doesn't exist as far as the mount is concerned
	}
	entry, ok := fs.svc.Lookup(svcPath(name))
	if !ok {
		return nil, os.ErrNotExist
	}
	return fileInfoFromEntry(entry), nil
}

func (fs *FS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	writable := flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC) != 0
	if isJunk(name) {
		// Accept writes to OS-metadata files but discard them; reads/opens 404.
		if writable {
			return newDiscardFile(name), nil
		}
		return nil, os.ErrNotExist
	}
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
	// WebDAV clients can issue GET/HEAD/range probes while simply opening a
	// folder. Keep mounted reads cache-only so Explorer/Finder never blocks on
	// an implicit firmware download of a remote_only file.
	rc, e, err := fs.svc.ReadCache(svcPath(name))
	if err != nil {
		switch {
		case err == service.ErrNotCached:
			if requestMethod(ctx) == "PROPFIND" {
				return newMetadataFile(e), nil
			}
			// On the machine but not cached locally. Keep it visible in listings,
			// but fail content reads quickly instead of downloading implicitly.
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

// isJunk reports whether a path's final component is OS-generated metadata that
// must never be synced to the CNC (it would create dead files on the SD card
// and, worse, a failed cleanup of one could block the sync queue). macOS Finder
// alone writes AppleDouble "._*" files, ".DS_Store", and a handful of dot-dirs;
// Windows writes "Thumbs.db"/"desktop.ini". We treat these as nonexistent so
// the OS believes its bookkeeping succeeded while nothing reaches the machine.
func isJunk(name string) bool {
	base := path.Base(strings.Trim(path.Clean("/"+name), "/"))
	switch base {
	case ".DS_Store", ".localized", "Thumbs.db", "desktop.ini", ".fseventsd",
		".Spotlight-V100", ".Trashes", ".TemporaryItems", ".apdisk":
		return true
	}
	return strings.HasPrefix(base, "._")
}

type requestMethodKey struct{}

func requestMethod(ctx context.Context) string {
	if method, ok := ctx.Value(requestMethodKey{}).(string); ok {
		return method
	}
	return ""
}

// svcPath converts a WebDAV mount-relative path (rooted at "/") into the
// relative path the service expects (which it joins under GcodeRoot). The mount
// root "/" maps to "" so the service resolves it to GcodeRoot itself.
func svcPath(name string) string {
	return strings.Trim(path.Clean("/"+name), "/")
}

// --- FileInfo ---

type fileInfo struct {
	name  string
	size  int64
	mode  os.FileMode
	mtime time.Time
	isDir bool
}

func (fi *fileInfo) Name() string       { return fi.name }
func (fi *fileInfo) Size() int64        { return fi.size }
func (fi *fileInfo) Mode() os.FileMode  { return fi.mode }
func (fi *fileInfo) ModTime() time.Time { return fi.mtime }
func (fi *fileInfo) IsDir() bool        { return fi.isDir }
func (fi *fileInfo) Sys() any           { return nil }
func (fi *fileInfo) ContentType(context.Context) (string, error) {
	if fi.isDir {
		return "", webdav.ErrNotImplemented
	}
	switch strings.ToLower(path.Ext(fi.name)) {
	case ".cnc", ".gc", ".gcode", ".nc", ".ngc", ".tap":
		return "text/plain; charset=utf-8", nil
	}
	if ctype := mime.TypeByExtension(path.Ext(fi.name)); ctype != "" {
		return ctype, nil
	}
	return "application/octet-stream", nil
}

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

// notCachedError reports a file present on the machine but not cached locally.
// The WebDAV mount deliberately does not fetch remote_only content on open
// because OS file managers probe files during folder browsing.
type notCachedError struct{ name string }

func (e *notCachedError) Error() string {
	return "davfs: " + e.name + " is on the machine but not cached locally yet"
}
