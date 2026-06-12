package davfs

import (
	"errors"
	"io"
	"os"
	"path"
	"runtime"

	"github.com/uwin/cnc-proxy/internal/service"
	"github.com/uwin/cnc-proxy/internal/store"
)

// readFile serves a cached file's contents to a WebDAV reader.
type readFile struct {
	rc   io.ReadCloser
	rs   io.ReadSeeker // set when rc is also seekable (it is: *os.File)
	info os.FileInfo
}

func newReadFile(rc io.ReadCloser, e store.Entry) *readFile {
	rf := &readFile{rc: rc, info: fileInfoFromEntry(e)}
	if s, ok := rc.(io.ReadSeeker); ok {
		rf.rs = s
	}
	return rf
}

func (f *readFile) Read(p []byte) (int, error) { return f.rc.Read(p) }
func (f *readFile) Close() error               { return f.rc.Close() }
func (f *readFile) Write([]byte) (int, error)  { return 0, os.ErrPermission }

func (f *readFile) Seek(offset int64, whence int) (int64, error) {
	if f.rs == nil {
		return 0, errors.New("davfs: file not seekable")
	}
	return f.rs.Seek(offset, whence)
}

func (f *readFile) Readdir(int) ([]os.FileInfo, error) {
	return nil, errors.New("davfs: not a directory")
}
func (f *readFile) Stat() (os.FileInfo, error) { return f.info, nil }

// dirFile presents a directory's children to the WebDAV server's PROPFIND.
type dirFile struct {
	info     os.FileInfo
	children []os.FileInfo
	pos      int
}

func newDirFile(info os.FileInfo, children []os.FileInfo) *dirFile {
	return &dirFile{info: info, children: children}
}

func (d *dirFile) Read([]byte) (int, error)  { return 0, os.ErrInvalid }
func (d *dirFile) Write([]byte) (int, error) { return 0, os.ErrPermission }
func (d *dirFile) Close() error              { return nil }
func (d *dirFile) Seek(int64, int) (int64, error) {
	return 0, errors.New("davfs: cannot seek a directory")
}
func (d *dirFile) Stat() (os.FileInfo, error) { return d.info, nil }

// Readdir mirrors os.File.Readdir: count<=0 returns all remaining entries; a
// positive count returns up to that many, with io.EOF when exhausted.
func (d *dirFile) Readdir(count int) ([]os.FileInfo, error) {
	if count <= 0 {
		rest := d.children[d.pos:]
		d.pos = len(d.children)
		return rest, nil
	}
	if d.pos >= len(d.children) {
		return nil, io.EOF
	}
	end := d.pos + count
	if end > len(d.children) {
		end = len(d.children)
	}
	batch := d.children[d.pos:end]
	d.pos = end
	return batch, nil
}

// writeFile buffers writes to a temp file and, on Close, hands the contents to
// the service (cache + enqueue upload). This is what gives the mount its
// deferred-sync behavior: the write completes locally and syncs to the machine
// later. The WebDAV client sees an immediate success.
type writeFile struct {
	svc     *service.Service
	name    string
	tmp     *os.File
	tmpPath string
	closed  bool
}

func newWriteFile(svc *service.Service, name string) (*writeFile, error) {
	tmp, err := os.CreateTemp("", "davfs-upload-*")
	if err != nil {
		return nil, err
	}
	wf := &writeFile{svc: svc, name: name, tmp: tmp, tmpPath: tmp.Name()}
	// A WebDAV client may abandon a write without calling Close (crash, dropped
	// connection). A finalizer reclaims the temp file so they can't accumulate.
	runtime.SetFinalizer(wf, func(f *writeFile) {
		if !f.closed && f.tmpPath != "" {
			f.tmp.Close()
			os.Remove(f.tmpPath)
		}
	})
	return wf, nil
}

func (f *writeFile) Write(p []byte) (int, error) { return f.tmp.Write(p) }

func (f *writeFile) Read([]byte) (int, error) { return 0, os.ErrPermission }
func (f *writeFile) Readdir(int) ([]os.FileInfo, error) {
	return nil, errors.New("davfs: not a directory")
}

// Seek supports the WebDAV server rewinding before/after writes; we allow
// seeking within the temp file.
func (f *writeFile) Seek(offset int64, whence int) (int64, error) {
	return f.tmp.Seek(offset, whence)
}

func (f *writeFile) Stat() (os.FileInfo, error) {
	// Report the in-progress temp file's stat (size grows as bytes are written).
	return f.tmp.Stat()
}

// discardFile accepts and silently drops writes. It backs OS-metadata files
// (AppleDouble "._*", ".DS_Store", etc.) that a desktop file manager insists on
// writing into the mount: the client sees success, but nothing is staged or
// synced to the CNC.
type discardFile struct {
	name string
	n    int64
}

func newDiscardFile(name string) *discardFile { return &discardFile{name: path.Base(name)} }

func (f *discardFile) Write(p []byte) (int, error) { f.n += int64(len(p)); return len(p), nil }
func (f *discardFile) Read([]byte) (int, error)    { return 0, io.EOF }
func (f *discardFile) Close() error                { return nil }
func (f *discardFile) Seek(int64, int) (int64, error) {
	return 0, nil
}
func (f *discardFile) Readdir(int) ([]os.FileInfo, error) {
	return nil, errors.New("davfs: not a directory")
}
func (f *discardFile) Stat() (os.FileInfo, error) {
	return &fileInfo{name: f.name, size: f.n, mode: 0o644}, nil
}

// Close flushes the buffered content into the service and cleans up the temp
// file. Errors flushing propagate so the WebDAV client learns the write failed.
func (f *writeFile) Close() error {
	if f.closed {
		return nil
	}
	f.closed = true
	runtime.SetFinalizer(f, nil) // normal close handles cleanup; drop the finalizer
	defer os.Remove(f.tmpPath)

	if _, err := f.tmp.Seek(0, io.SeekStart); err != nil {
		f.tmp.Close()
		return err
	}
	_, upErr := f.svc.Upload(f.name, f.tmp)
	closeErr := f.tmp.Close()
	if upErr != nil {
		return upErr
	}
	return closeErr
}
