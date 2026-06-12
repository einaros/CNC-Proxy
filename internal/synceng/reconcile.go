package synceng

import (
	"context"
	"log"
	"os"
	"path"
	"time"

	"github.com/uwin/cnc-proxy/internal/client"
	"github.com/uwin/cnc-proxy/internal/protocol"
	"github.com/uwin/cnc-proxy/internal/service"
	"github.com/uwin/cnc-proxy/internal/session"
	"github.com/uwin/cnc-proxy/internal/store"
)

// settledStates are the catalog states a reconcile sweep is allowed to touch.
// Anything else (pending_upload, uploading, pending_delete, deleting,
// pending_rename, error) represents a local intention still in flight, which
// reconcile must not clobber.
func isSettled(s store.SyncState) bool {
	return s == store.Synced || s == store.RemoteOnly
}

// Reconcile walks the machine's gcode tree and folds the result into the
// catalog, so files added, changed, or removed out-of-band (e.g. by the
// official controller) become visible. It runs through the arbiter and so only
// proceeds in owner mode with an idle machine; otherwise it returns the
// arbiter's error and changes nothing.
//
// It is conservative: it only adds previously-unknown files (as remote_only),
// flips settled entries whose size changed back to remote_only (so the next
// read re-fetches), and drops settled entries that vanished from the machine.
// In-flight entries are left untouched.
func (e *Engine) Reconcile(maxDepth int) error {
	return e.reconcile(maxDepth, false)
}

// DeepReconcile performs the metadata reconcile plus MD5 checks for cached
// synced files. It is intentionally separate from the frequent metadata sweep:
// md5sum is slower and firmware-shaped, but it catches same-size out-of-band
// edits that listing metadata alone cannot distinguish.
func (e *Engine) DeepReconcile(maxDepth int) error {
	return e.reconcile(maxDepth, true)
}

func (e *Engine) reconcile(maxDepth int, deep bool) error {
	var remote map[string]protocol.DirEntry
	err := e.arb.WithMachine(true, func(c *client.Conn) error {
		var lerr error
		remote, lerr = listTree(c, service.GcodeRoot, maxDepth, e.opTimeout)
		if lerr == nil && deep {
			e.deepCheck(c, remote)
		}
		return lerr
	})
	if err != nil {
		return err
	}

	// Index existing catalog entries.
	known := map[string]store.Entry{}
	for _, en := range e.store.ListEntries() {
		known[en.Path] = en
	}

	// Add or update from the remote view.
	for p, de := range remote {
		existing, ok := known[p]
		if !ok {
			// Newly discovered on the machine.
			_ = e.store.PutEntry(store.Entry{
				Path:  p,
				IsDir: de.IsDir,
				Size:  de.Size,
				MTime: de.MTime,
				Sync:  syncStateFor(de),
			})
			continue
		}
		if de.IsDir || existing.IsDir {
			continue
		}
		// A settled file whose machine metadata changed: re-fetch on next read.
		// For freshly uploaded synced files, local mtime and machine mtime are
		// often different even when content is identical, so mtime alone only
		// invalidates remote_only entries. DeepReconcile uses md5sum for synced
		// cached files to catch same-size content changes without that false hit.
		if isSettled(existing.Sync) && (existing.Size != de.Size || remoteOnlyMTimeChanged(existing, de)) {
			e.markRemoteOnly(existing, de, "")
		}
	}

	// Drop settled entries that disappeared from the machine.
	for p, en := range known {
		if _, stillThere := remote[p]; stillThere {
			continue
		}
		if isSettled(en.Sync) {
			_ = e.store.DeleteEntry(p)
		}
	}
	return nil
}

func remoteOnlyMTimeChanged(existing store.Entry, de protocol.DirEntry) bool {
	return existing.Sync == store.RemoteOnly && !existing.MTime.IsZero() && !de.MTime.IsZero() && !existing.MTime.Equal(de.MTime)
}

func (e *Engine) deepCheck(c *client.Conn, remote map[string]protocol.DirEntry) {
	for _, existing := range e.store.ListEntries() {
		if existing.Sync != store.Synced || existing.IsDir || existing.MD5 == "" || existing.CachePath == "" {
			continue
		}
		de, ok := remote[existing.Path]
		if !ok || de.IsDir {
			continue
		}
		remoteMD5, err := c.Md5(existing.Path, e.opTimeout)
		if err != nil {
			log.Printf("synceng: deep reconcile md5 skipped for %s: %v", existing.Path, err)
			continue
		}
		if remoteMD5 != existing.MD5 {
			e.markRemoteOnly(existing, de, remoteMD5)
		}
	}
}

func (e *Engine) markRemoteOnly(existing store.Entry, de protocol.DirEntry, remoteMD5 string) {
	if existing.CachePath != "" {
		_ = os.Remove(existing.CachePath)
	}
	existing.Size = de.Size
	existing.MTime = de.MTime
	existing.Sync = store.RemoteOnly
	existing.CachePath = ""
	existing.MD5 = remoteMD5
	existing.Error = ""
	_ = e.store.PutEntry(existing)
}

func syncStateFor(de protocol.DirEntry) store.SyncState {
	if de.IsDir {
		return store.Synced // directories have no content to fetch
	}
	return store.RemoteOnly
}

// listTree lists dir and its subdirectories up to maxDepth levels deep,
// returning a map of machine-absolute path -> entry (both files and dirs).
func listTree(c *client.Conn, root string, maxDepth int, timeout time.Duration) (map[string]protocol.DirEntry, error) {
	out := map[string]protocol.DirEntry{}
	type item struct {
		dir   string
		depth int
	}
	queue := []item{{dir: root, depth: 0}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		entries, err := c.List(cur.dir, timeout)
		if err != nil {
			return nil, err
		}
		for _, de := range entries {
			full := path.Join(cur.dir, de.Name)
			out[full] = de
			if de.IsDir && cur.depth < maxDepth {
				queue = append(queue, item{dir: full, depth: cur.depth + 1})
			}
		}
	}
	return out, nil
}

// RunReconcile periodically reconciles, until ctx is canceled. The first sweep
// happens after one interval (the engine's job drain handles immediate needs).
func (e *Engine) RunReconcile(ctx context.Context, interval time.Duration, maxDepth int) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.Reconcile(maxDepth); err != nil {
				// Blocked (relay/idle) or transient: not worth logging loudly.
				if !isBlocked(err) {
					log.Printf("synceng: reconcile error: %v", err)
				}
			}
		}
	}
}

// RunDeepReconcile periodically runs the slower MD5-based reconcile until ctx is
// canceled. It is meant to run less frequently than RunReconcile.
func (e *Engine) RunDeepReconcile(ctx context.Context, interval time.Duration, maxDepth int) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.DeepReconcile(maxDepth); err != nil {
				if !isBlocked(err) {
					log.Printf("synceng: deep reconcile error: %v", err)
				}
			}
		}
	}
}

// isBlocked reports whether an error is the expected "can't run right now" kind
// (a controller transaction in progress, or the machine isn't idle yet).
func isBlocked(err error) bool {
	return session.Retryable(err)
}
