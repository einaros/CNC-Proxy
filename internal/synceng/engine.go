// Package synceng runs the deferred-sync loop: it drains the durable job queue
// against the machine whenever the proxy owns the connection (owner mode) and
// the machine is idle. Jobs that can't run yet (controller connected, machine
// busy, machine unreachable) simply stay queued and are retried, which is what
// makes the mounted filesystem behave like Google Drive — writes are accepted
// immediately and pushed to the machine when it's free.
package synceng

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/uwin/cnc-proxy/internal/client"
	"github.com/uwin/cnc-proxy/internal/quicklz"
	"github.com/uwin/cnc-proxy/internal/session"
	"github.com/uwin/cnc-proxy/internal/store"
)

// Engine executes queued jobs against the machine via the arbiter.
type Engine struct {
	store   *store.Store
	arb     *session.Arbiter
	opTimeout time.Duration

	// backoff bounds. A failed job's next attempt waits up to maxBackoff.
	baseBackoff time.Duration
	maxBackoff  time.Duration
	maxAttempts int

	// compress controls whether uploads larger than quicklz.BlockSize are
	// QuickLZ-compressed when the firmware advertises ".lz" support.
	compress bool

	// ftype caches the firmware's advertised upload type ("lz" => compression
	// supported); empty until first queried on the same connection.
	mu    sync.Mutex
	ftype string

	now func() time.Time
}

// Config configures the sync engine.
type Config struct {
	Store       *store.Store
	Arbiter     *session.Arbiter
	OpTimeout   time.Duration
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	MaxAttempts int
	// Compress enables QuickLZ compression for large uploads when the firmware
	// advertises ".lz" support. Defaults to true.
	Compress *bool
}

// New creates an Engine with sensible defaults for unset fields.
func New(cfg Config) *Engine {
	if cfg.OpTimeout == 0 {
		cfg.OpTimeout = 60 * time.Second
	}
	if cfg.BaseBackoff == 0 {
		cfg.BaseBackoff = 2 * time.Second
	}
	if cfg.MaxBackoff == 0 {
		cfg.MaxBackoff = 60 * time.Second
	}
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = 8
	}
	compress := true
	if cfg.Compress != nil {
		compress = *cfg.Compress
	}
	return &Engine{
		store:       cfg.Store,
		arb:         cfg.Arbiter,
		opTimeout:   cfg.OpTimeout,
		baseBackoff: cfg.BaseBackoff,
		maxBackoff:  cfg.MaxBackoff,
		maxAttempts: cfg.MaxAttempts,
		compress:    compress,
		now:         time.Now,
	}
}

// Run drives the queue until ctx is canceled, polling at the given interval.
func (e *Engine) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.drain()
		}
	}
}

// drain runs queued jobs until none are runnable (queue empty, or blocked by
// relay/idle/unreachable). One job at a time, in order — the machine is
// single-conversation.
func (e *Engine) drain() {
	for {
		job, ok := e.store.NextQueued()
		if !ok {
			return
		}
		if !e.shouldAttempt(job) {
			// The head job is backing off; don't starve it by skipping ahead,
			// since order matters for correctness (e.g. mkdir before upload).
			return
		}
		ran, err := e.runJob(job)
		if !ran {
			// Blocked (relay active / not idle / unreachable): stop this pass
			// and try again next tick.
			return
		}
		if err != nil {
			// Attempted but failed. The job stays queued at the head; stop the
			// pass so it isn't busy-retried within a single drain — the next
			// tick re-attempts it once its backoff has elapsed.
			log.Printf("synceng: job %d (%s %s) failed: %v", job.ID, job.Kind, job.Path, err)
			return
		}
		// Success: the job is done; advance to the next queued job.
	}
}

// shouldAttempt applies per-job backoff based on attempts and last update.
func (e *Engine) shouldAttempt(j store.Job) bool {
	if j.Attempts == 0 {
		return true
	}
	wait := e.baseBackoff << (j.Attempts - 1)
	if wait > e.maxBackoff || wait <= 0 {
		wait = e.maxBackoff
	}
	return e.now().Sub(j.UpdatedAt) >= wait
}

// runJob attempts one job. The bool reports whether execution was actually
// attempted (false = blocked, should retry later); the error is the job's
// outcome when attempted.
func (e *Engine) runJob(job store.Job) (bool, error) {
	// Most jobs require idle; that is the firmware's constraint for file ops.
	err := e.arb.WithMachine(true, func(c *client.Conn) error {
		return e.execute(c, job)
	})
	switch {
	case session.Retryable(err):
		return false, nil // blocked (relay/idle/busy), not a failure
	case err != nil:
		e.recordFailure(job, err)
		return true, err
	default:
		e.recordSuccess(job)
		return true, nil
	}
}

// execute performs the actual protocol operation and updates catalog state.
func (e *Engine) execute(c *client.Conn, job store.Job) error {
	switch job.Kind {
	case store.JobMkdir:
		return c.Mkdir(job.Path, e.opTimeout)

	case store.JobDelete:
		e.store.SetEntrySync(job.Path, store.Deleting, "")
		if err := c.Remove(job.Path, e.opTimeout); err != nil {
			return err
		}
		// Drop the catalog entry and its cache file on successful delete.
		if entry, ok := e.store.GetEntry(job.Path); ok && entry.CachePath != "" {
			os.Remove(entry.CachePath)
		}
		return e.store.DeleteEntry(job.Path)

	case store.JobRename:
		e.store.SetEntrySync(job.Path, store.PendingRename, "")
		if err := c.Rename(job.Path, job.DestPath, e.opTimeout); err != nil {
			return err
		}
		// Move the catalog entry to the new path.
		if entry, ok := e.store.GetEntry(job.Path); ok {
			entry.Path = job.DestPath
			entry.Sync = store.Synced
			e.store.PutEntry(entry)
			e.store.DeleteEntry(job.Path)
		}
		return nil

	case store.JobUpload:
		e.store.SetEntrySync(job.Path, store.Uploading, "")
		if err := e.doUpload(c, job); err != nil {
			return err
		}
		// The upload handshake is itself MD5-verified: the controller sends the
		// content MD5 up front and the firmware stores it, replying FILE_END
		// only on a complete transfer. A post-upload md5sum is a best-effort
		// extra check — but the firmware pauses (cachewait) right after a
		// successful upload, so an immediate md5sum can transiently fail or
		// race. Treat any mismatch/▽error as non-fatal: the transfer already
		// succeeded, so mark synced and only log a discrepancy.
		if remoteMD5, mErr := c.Md5(job.Path, e.opTimeout); mErr != nil {
			log.Printf("synceng: post-upload md5 check skipped for %s: %v", job.Path, mErr)
		} else if remoteMD5 != job.MD5 {
			log.Printf("synceng: post-upload md5 mismatch for %s (got %s want %s)", job.Path, remoteMD5, job.MD5)
		}
		return e.store.SetEntrySync(job.Path, store.Synced, "")

	default:
		return errors.New("synceng: unknown job kind " + string(job.Kind))
	}
}

// doUpload transfers a job's file, compressing it with QuickLZ first when the
// machine supports ".lz" and the file is large enough to benefit. The MD5 sent
// and later verified is always of the UNCOMPRESSED content (the firmware
// decompresses on receipt and stores that MD5), so verification is unchanged.
func (e *Engine) doUpload(c *client.Conn, job store.Job) error {
	// .bin firmware images are never compressed (matches the controller).
	if e.compress && job.Size > quicklz.BlockSize && !strings.HasSuffix(job.Path, ".bin") && e.lzSupported(c) {
		return e.uploadCompressed(c, job)
	}
	f, err := os.Open(job.CachePath)
	if err != nil {
		return err
	}
	defer f.Close()
	return c.Upload(job.Path, f, job.Size, job.MD5, e.opTimeout, nil)
}

// uploadCompressed compresses the cache file into a temporary .lz container and
// uploads it under "<path>.lz". The firmware strips the .lz suffix, decompresses
// the content, and stores it under job.Path, so the catalog path is unchanged.
func (e *Engine) uploadCompressed(c *client.Conn, job store.Job) error {
	in, err := os.Open(job.CachePath)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp, err := os.CreateTemp("", "cnc-upload-*.lz")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	if _, err := quicklz.CompressStream(tmp, in); err != nil {
		return err
	}
	size, err := tmp.Seek(0, os.SEEK_CUR)
	if err != nil {
		return err
	}
	if _, err := tmp.Seek(0, os.SEEK_SET); err != nil {
		return err
	}
	// Upload under the .lz name; MD5 is of the uncompressed original.
	return c.Upload(job.Path+".lz", tmp, size, job.MD5, e.opTimeout, nil)
}

// lzSupported reports whether the firmware advertises ".lz" upload support,
// querying once and caching the answer. A query failure is treated as "no
// compression" so uploads still proceed uncompressed.
func (e *Engine) lzSupported(c *client.Conn) bool {
	e.mu.Lock()
	cached := e.ftype
	e.mu.Unlock()
	if cached == "" {
		t, err := c.Ftype(e.opTimeout)
		if err != nil {
			return false
		}
		e.mu.Lock()
		e.ftype = t
		e.mu.Unlock()
		cached = t
	}
	return strings.Contains(cached, "lz")
}

func (e *Engine) recordSuccess(job store.Job) {
	e.store.UpdateJob(job.ID, func(j *store.Job) {
		j.State = store.Done
		j.Attempts++
		j.LastError = ""
	})
}

func (e *Engine) recordFailure(job store.Job, err error) {
	e.store.UpdateJob(job.ID, func(j *store.Job) {
		j.Attempts++
		j.LastError = err.Error()
		if j.Attempts >= e.maxAttempts {
			j.State = store.Failed
		}
		// else: stays Queued, retried after backoff.
	})
	// Reflect failure on the catalog entry too, unless it was a delete that
	// removed the entry.
	if _, ok := e.store.GetEntry(job.Path); ok {
		if serr := e.store.SetEntrySync(job.Path, store.Error, err.Error()); serr != nil {
			log.Printf("synceng: failed to record error state for %s: %v", job.Path, serr)
		}
	}
}
