package synceng

import (
	"context"
	"log"
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
	var remote map[string]protocol.DirEntry
	err := e.arb.WithMachine(true, func(c *client.Conn) error {
		var lerr error
		remote, lerr = listTree(c, service.GcodeRoot, maxDepth, e.opTimeout)
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
		// A settled file whose size changed on the machine: re-fetch on next read.
		if isSettled(existing.Sync) && existing.Size != de.Size {
			existing.Size = de.Size
			existing.MTime = de.MTime
			existing.Sync = store.RemoteOnly
			existing.CachePath = "" // force re-download
			existing.MD5 = ""
			_ = e.store.PutEntry(existing)
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

// isBlocked reports whether an error is the expected "can't run right now" kind
// (a controller transaction in progress, or the machine isn't idle yet).
func isBlocked(err error) bool {
	return session.Retryable(err)
}
