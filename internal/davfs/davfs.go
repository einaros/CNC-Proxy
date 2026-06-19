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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
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
		LockSystem: newPermissiveLockSystem(),
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lw := &webDAVLogResponseWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			logWebDAVRequest(r, lw.status)
		}()
		w = lw
		setNoCacheHeaders(w)
		if r.Method == http.MethodOptions {
			fs.serveOptions(w, r, prefix)
			return
		}
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && fs.cacheValidationPending(w, r, prefix) {
			return
		}
		ctx := context.WithValue(r.Context(), requestMethodKey{}, r.Method)
		if r.Method == http.MethodPut {
			cr, ok, err := parseContentRange(r.Header.Get("Content-Range"))
			if err != nil {
				http.Error(w, "invalid Content-Range", http.StatusBadRequest)
				return
			}
			if ok {
				ctx = context.WithValue(ctx, requestContentRangeKey{}, cr)
			}
		}
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (fs *FS) cacheValidationPending(w http.ResponseWriter, r *http.Request, prefix string) bool {
	reqPath, ok := stripHandlerPrefix(prefix, r.URL.Path)
	if !ok || isRoot(reqPath) || isJunk(reqPath) {
		return false
	}
	entry, ok := fs.svc.Lookup(svcPath(reqPath))
	if !ok || entry.IsDir || !entryNeedsCacheValidation(entry) {
		return false
	}
	w.Header().Set("Retry-After", "5")
	http.Error(w, service.ErrCacheValidationPending.Error(), http.StatusServiceUnavailable)
	return true
}

type webDAVLogResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *webDAVLogResponseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func logWebDAVRequest(r *http.Request, status int) {
	switch r.Method {
	case http.MethodOptions, http.MethodGet, http.MethodHead, http.MethodDelete, http.MethodPut, "MKCOL", "COPY", "MOVE", "LOCK", "UNLOCK", "PROPFIND", "PROPPATCH":
	default:
		return
	}
	log.Printf("webdav: %s %s -> %d depth=%q overwrite=%q if=%q ua=%q", r.Method, r.URL.Path, status, r.Header.Get("Depth"), r.Header.Get("Overwrite"), r.Header.Get("If"), r.UserAgent())
}

func setNoCacheHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	h.Set("Pragma", "no-cache")
	h.Set("Expires", "0")
}

func (fs *FS) serveOptions(w http.ResponseWriter, r *http.Request, prefix string) {
	reqPath, ok := stripHandlerPrefix(prefix, r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	allow := "OPTIONS, PUT, MKCOL"
	if fi, err := fs.Stat(r.Context(), reqPath); err == nil {
		if fi.IsDir() {
			allow = "OPTIONS, DELETE, PROPPATCH, COPY, MOVE, PROPFIND"
		} else {
			allow = "OPTIONS, GET, HEAD, POST, DELETE, PROPPATCH, COPY, MOVE, PROPFIND, PUT"
		}
	}
	w.Header().Set("Allow", allow)
	w.Header().Set("DAV", "1")
	w.Header().Set("MS-Author-Via", "DAV")
}

func stripHandlerPrefix(prefix, p string) (string, bool) {
	if prefix == "" {
		return p, true
	}
	if r := strings.TrimPrefix(p, prefix); len(r) < len(p) {
		return r, true
	}
	return p, false
}

type permissiveLockSystem struct {
	mu      sync.Mutex
	next    uint64
	details map[string]webdav.LockDetails
	expires map[string]time.Time
}

func newPermissiveLockSystem() *permissiveLockSystem {
	return &permissiveLockSystem{
		details: map[string]webdav.LockDetails{},
		expires: map[string]time.Time{},
	}
}

func (l *permissiveLockSystem) Confirm(now time.Time, name0, name1 string, conditions ...webdav.Condition) (func(), error) {
	l.mu.Lock()
	l.cleanupLocked(now)
	l.mu.Unlock()
	return func() {}, nil
}

func (l *permissiveLockSystem) Create(now time.Time, details webdav.LockDetails) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cleanupLocked(now)
	l.next++
	token := strconv.FormatUint(l.next, 10)
	l.details[token] = details
	l.expires[token] = lockExpiry(now, details.Duration)
	return token, nil
}

func (l *permissiveLockSystem) Refresh(now time.Time, token string, duration time.Duration) (webdav.LockDetails, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cleanupLocked(now)
	details, ok := l.details[token]
	if !ok {
		return webdav.LockDetails{}, webdav.ErrNoSuchLock
	}
	details.Duration = duration
	l.details[token] = details
	l.expires[token] = lockExpiry(now, duration)
	return details, nil
}

func (l *permissiveLockSystem) Unlock(now time.Time, token string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cleanupLocked(now)
	delete(l.details, token)
	delete(l.expires, token)
	return nil
}

func (l *permissiveLockSystem) cleanupLocked(now time.Time) {
	for token, exp := range l.expires {
		if !exp.IsZero() && !now.Before(exp) {
			delete(l.details, token)
			delete(l.expires, token)
		}
	}
}

func lockExpiry(now time.Time, duration time.Duration) time.Time {
	if duration <= 0 {
		duration = 30 * time.Minute
	}
	return now.Add(duration)
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
		children, err := fs.svc.Children(svcPath(name))
		if err != nil {
			return nil, err
		}
		return fs.dirInfo(name, rootInfo(), children), nil
	}
	if isJunk(name) {
		return nil, os.ErrNotExist // junk doesn't exist as far as the mount is concerned
	}
	entry, ok := fs.svc.Lookup(svcPath(name))
	if !ok || !visibleInDAV(entry) {
		return nil, os.ErrNotExist
	}
	info := fileInfoFromEntry(entry)
	if entry.IsDir {
		children, err := fs.svc.Children(svcPath(name))
		if err != nil {
			return nil, err
		}
		info = fs.dirInfo(name, info, children)
	}
	return info, nil
}

func (fs *FS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	writable := flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC) != 0
	method := requestMethod(ctx)
	if method == "PROPPATCH" {
		// x/net/webdav opens the target O_RDWR during PROPPATCH so optional
		// dead properties can be updated. That is metadata access, not a content
		// write; treating it as writable staged an empty upload on close.
		writable = false
	}
	if isJunk(name) {
		// Accept writes to OS-metadata files but discard them; reads/opens 404.
		if writable {
			return newDiscardFile(name), nil
		}
		return nil, os.ErrNotExist
	}
	if writable {
		// x/net/webdav creates lock-null resources by opening a missing path
		// during LOCK. That must not enqueue a real CNC upload.
		if method == "LOCK" {
			return newDiscardFile(name), nil
		}
		return newWriteFile(fs.svc, svcPath(name), contentRangeFromContext(ctx))
	}
	// Read or directory open.
	if isRoot(name) {
		return fs.openDir(name)
	}
	entry, ok := fs.svc.Lookup(svcPath(name))
	if !ok || !visibleInDAV(entry) {
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
		case errors.Is(err, service.ErrNotCached):
			if requestMethod(ctx) == "PROPFIND" {
				return newMetadataFile(e), nil
			}
			// On the machine but not cached locally. Keep it visible in listings,
			// but fail content reads quickly instead of downloading implicitly.
			return nil, &notCachedError{name: name}
		case errors.Is(err, service.ErrCacheValidationPending):
			if requestMethod(ctx) == "PROPFIND" {
				return newMetadataFile(e), nil
			}
			return nil, err
		default:
			return nil, os.ErrNotExist
		}
	}
	return newReadFile(rc, e), nil
}

func entryNeedsCacheValidation(e store.Entry) bool {
	return e.CachePath != "" && (e.CacheState == store.CacheValidating || (e.CacheState == "" && e.Sync == store.Synced))
}

func (fs *FS) openDir(name string) (webdav.File, error) {
	children, err := fs.svc.Children(svcPath(name))
	if err != nil {
		return nil, err
	}
	infos := make([]os.FileInfo, 0, len(children))
	for _, c := range children {
		if !visibleInDAV(c) {
			continue
		}
		infos = append(infos, fileInfoFromEntry(c))
	}
	var info os.FileInfo = fs.dirInfo(name, rootInfo(), children)
	if !isRoot(name) {
		if e, ok := fs.svc.Lookup(svcPath(name)); ok {
			info = fs.dirInfo(name, fileInfoFromEntry(e), children)
		}
	}
	return newDirFile(info, infos), nil
}

func (fs *FS) dirInfo(name string, info *fileInfo, children []store.Entry) *fileInfo {
	info.mtime = collectionModTime(info.mtime, children)
	info.etag = collectionETag(svcPath(name), info.mtime, children)
	return info
}

func collectionModTime(base time.Time, children []store.Entry) time.Time {
	mt := base
	for _, child := range children {
		if child.MTime.After(mt) {
			mt = child.MTime
		}
		if child.UpdatedAt.After(mt) {
			mt = child.UpdatedAt
		}
	}
	if mt.IsZero() || mt.Equal(time.Unix(0, 0)) {
		return time.Now()
	}
	return mt
}

func collectionETag(name string, mt time.Time, children []store.Entry) string {
	parts := []string{name, strconv.FormatInt(mt.UnixNano(), 10), strconv.Itoa(len(children))}
	for _, child := range children {
		parts = append(parts,
			child.Path,
			strconv.FormatInt(child.Size, 10),
			child.MD5,
			string(child.Sync),
			strconv.FormatInt(child.MTime.UnixNano(), 10),
			strconv.FormatInt(child.UpdatedAt.UnixNano(), 10),
		)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

func visibleInDAV(e store.Entry) bool {
	return e.Sync != store.PendingDelete && e.Sync != store.Deleting
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
type requestContentRangeKey struct{}

type contentRange struct {
	start int64
	end   int64
	total int64
}

func requestMethod(ctx context.Context) string {
	if method, ok := ctx.Value(requestMethodKey{}).(string); ok {
		return method
	}
	return ""
}

func contentRangeFromContext(ctx context.Context) *contentRange {
	if cr, ok := ctx.Value(requestContentRangeKey{}).(contentRange); ok {
		return &cr
	}
	return nil
}

func parseContentRange(raw string) (contentRange, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return contentRange{}, false, nil
	}
	const prefix = "bytes "
	if !strings.HasPrefix(strings.ToLower(raw), prefix) {
		return contentRange{}, false, os.ErrInvalid
	}
	spec := strings.TrimSpace(raw[len(prefix):])
	rangePart, totalPart, ok := strings.Cut(spec, "/")
	if !ok || totalPart == "*" {
		return contentRange{}, false, os.ErrInvalid
	}
	startPart, endPart, ok := strings.Cut(rangePart, "-")
	if !ok {
		return contentRange{}, false, os.ErrInvalid
	}
	start, err := strconv.ParseInt(strings.TrimSpace(startPart), 10, 64)
	if err != nil {
		return contentRange{}, false, os.ErrInvalid
	}
	end, err := strconv.ParseInt(strings.TrimSpace(endPart), 10, 64)
	if err != nil {
		return contentRange{}, false, os.ErrInvalid
	}
	total, err := strconv.ParseInt(strings.TrimSpace(totalPart), 10, 64)
	if err != nil {
		return contentRange{}, false, os.ErrInvalid
	}
	if start < 0 || end < start || total <= 0 || end >= total {
		return contentRange{}, false, os.ErrInvalid
	}
	return contentRange{start: start, end: end, total: total}, true, nil
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
	etag  string
}

func (fi *fileInfo) Name() string       { return fi.name }
func (fi *fileInfo) Size() int64        { return fi.size }
func (fi *fileInfo) Mode() os.FileMode  { return fi.mode }
func (fi *fileInfo) ModTime() time.Time { return fi.mtime }
func (fi *fileInfo) IsDir() bool        { return fi.isDir }
func (fi *fileInfo) Sys() any           { return nil }
func (fi *fileInfo) ETag(context.Context) (string, error) {
	if fi.etag == "" {
		return "", webdav.ErrNotImplemented
	}
	return fi.etag, nil
}
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
		etag:  entryETag(e),
	}
}

func entryETag(e store.Entry) string {
	if e.IsDir {
		return ""
	}
	identity := e.MD5
	if identity == "" {
		identity = strconv.FormatInt(e.MTime.UnixNano(), 10)
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		e.Path,
		strconv.FormatInt(e.Size, 10),
		identity,
	}, "\x00")))
	return `"` + hex.EncodeToString(sum[:16]) + `"`
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
